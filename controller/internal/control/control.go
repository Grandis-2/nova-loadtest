// Package control은 시험 한 번을 끝까지 진행한다(docs/implementation-plan.md 2.4).
//
//	계획 다시 계산 → plan.json → 토큰 발급 → k6 N대 → 시작 확인(RUNNING, 이미지) → 감시 → 판정 → 결과 → 토큰 삭제
//
// 시험 결과(합격, 불합격, 중단, 판정 불가, 한계 측정, 취소)는 오류가 아니다. 결과를 남기지 못한 경우만 오류를 돌려준다.
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Grandis-2/nova-loadtest/config"
	"github.com/Grandis-2/nova-loadtest/controller/internal/clock"
	"github.com/Grandis-2/nova-loadtest/controller/internal/infra"
	"github.com/Grandis-2/nova-loadtest/controller/internal/mimir"
	"github.com/Grandis-2/nova-loadtest/controller/internal/plan"
	"github.com/Grandis-2/nova-loadtest/controller/internal/report"
	"github.com/Grandis-2/nova-loadtest/controller/internal/runner"
)

// ErrNotFound는 Store에 키가 없을 때 돌려준다.
var ErrNotFound = errors.New("없다")

// Store는 시험 버킷이다(S3).
type Store interface {
	Put(ctx context.Context, key string, body []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	DeletePrefix(ctx context.Context, prefix string) error
}

// Metrics는 감시와 판정에 쓰는 지표다(mimir.Metrics).
type Metrics interface {
	Progress(ctx context.Context, at time.Time) (mimir.Live, error)
	LastSeen(ctx context.Context, at time.Time) (map[string]float64, error)
	Coverage(ctx context.Context, start, end time.Time) (map[string]float64, error)
	Window(ctx context.Context, start, end time.Time) (mimir.Stat, map[string]mimir.Stat, error)
	Scenarios(ctx context.Context, start, end time.Time) (map[string]float64, error)
	Outcomes(ctx context.Context, start, end time.Time) (map[string]float64, error)
	Dropped(ctx context.Context, start, end time.Time) (float64, error)
}

// 감시 규칙
const (
	monitorEvery  = 15 * time.Second
	progressEvery = time.Minute      // 진행 값을 로그와 결과에 남기는 간격
	monitorAfter  = time.Minute      // START_AT 뒤 이만큼 지나서 감시를 시작한다(첫 지표가 들어올 시간)
	silentLimit   = time.Minute      // 지표가 이보다 오래 없으면 끊김으로 적는다(시험은 멈추지 않는다)
	minPartial    = 30 * time.Second // 멈춘 시험에서 이보다 짧게 돈 구간은 판정하지 않는다
	endTolerance  = 30 * time.Second // 계획한 끝 시각보다 이만큼 일찍 끝난 것까지 정상 종료로 본다
	stopTimeout   = 2 * time.Minute  // 태스크 정의의 stopTimeout. 멈춘 뒤 요약을 올릴 시간
	minterTimeout = 20 * time.Minute
	minLeadTime   = time.Minute // START_AT 이 지금부터 이보다 가까우면 태스크가 제때 뜨지 못한다
	exportEvery   = 10          // k6 OTel 내보내기 간격(초). 구간 누락 판단에 쓴다
	coverageSlack = 5           // 빠져도 되는 표본 수(약 50초. 1분 넘게 비면 판정 불가)
	// k6 가 끝날 때 내보낸 마지막 지표가 수집 게이트웨이와 수집기(각각 1~2초 배치)를 지나 Mimir 에 들어올 시간.
	// queue 의 처리 인원은 끝난 바로 뒤에 세므로 이만큼 기다린다
	ingestDelay = 30 * time.Second
)

// Deps는 컨트롤러가 쓰는 외부 시스템이다. 테스트는 가짜로 바꾼다.
type Deps struct {
	Runner  *runner.Runner
	Store   Store
	Metrics Metrics
	Clock   clock.Clock
	Logf    func(format string, a ...any)
}

// Run은 시험 하나를 돌리는 데 필요한 값이다.
type Run struct {
	Config        *config.Config
	RunTask       *infra.RunTask
	Observability *infra.Observability
	Input         plan.Input // 워크플로의 입력. 계획을 다시 계산한다
	PlanJSON      string     // 러너(입력 확인)가 계산한 계획
	BaseURL       string
}

func resultsKey(testID, name string) string { return "results/" + testID + "/" + name }

// Execute는 시험을 끝까지 진행하고 결과를 기록한다.
func Execute(ctx context.Context, d Deps, r Run) (*report.Summary, error) {
	id := r.Input.TestID
	// 러너가 계획한 시각(planned_at)으로 다시 계산한다. 컨트롤러는 몇 분 늦게 뜰 수 있어 지금 시각으로 계산하면
	// open_at 검사에서 러너와 다르게 거절할 수 있다. 시작 시각이 촉박한지는 아래에서 따로 보고 취소 결과를 남긴다
	if r.Input.Now.IsZero() {
		r.Input.Now = d.Clock.Now()
	}
	p, err := plan.New(r.Config, r.Input)
	if err != nil {
		return nil, fmt.Errorf("계획을 다시 계산하지 못했다: %w", err)
	}
	mine, _ := json.Marshal(p)
	if !jsonEqual(mine, []byte(r.PlanJSON)) {
		return nil, errors.New("러너가 넘긴 계획과 다시 계산한 계획이 다르다(scenarios.yml 이나 코드가 다른 이미지일 수 있다)")
	}
	if err := d.Store.Put(ctx, resultsKey(id, "plan.json"), mine); err != nil {
		return nil, fmt.Errorf("plan.json 기록: %w", err)
	}

	s := &report.Summary{TestID: id, Scenario: p.Scenario}
	finish := func(res report.Result, reason string) (*report.Summary, error) {
		s.Result, s.Reason, s.EndedAt = res, reason, d.Clock.Now()
		return s, record(ctx, d, s)
	}
	defer func() {
		// 토큰은 시험이 끝나면 바로 지운다. 접두사 수명 주기(1일)는 남은 것을 치우는 안전망이다
		if err := d.Store.DeletePrefix(context.WithoutCancel(ctx), "tokens/"+id+"/"); err != nil {
			d.Logf("토큰 삭제 실패: %v", err)
		}
	}()

	// open_at 이 있으면 시작 시각이 정해져 있다. 토큰을 만들기 전에 이미 촉박하면 바로 취소한다
	tooClose := func() string {
		now := d.Clock.Now()
		if at := p.StartAt(r.Config, now); at.Before(now.Add(minLeadTime)) {
			return fmt.Sprintf("시작 시각 %s가 너무 가깝다(태스크가 뜰 시간이 없다)", at.Format(time.RFC3339))
		}
		return ""
	}
	if reason := tooClose(); reason != "" {
		return finish(report.Canceled, reason)
	}
	if p.Members != nil {
		if reason := mintTokens(ctx, d, r, p); reason != "" {
			return finish(report.Canceled, reason)
		}
		// 토큰 발급은 별도 Fargate 태스크라 몇 분 걸릴 수 있다. 시작 시각은 발급이 끝난 뒤에 정한다.
		// open_at 이 있으면 시작 시각은 그대로이므로 남은 시간을 다시 본다
		if reason := tooClose(); reason != "" {
			return finish(report.Canceled, "토큰을 만드는 동안 "+reason)
		}
	}
	startAt := p.StartAt(r.Config, d.Clock.Now())
	s.StartAt = startAt

	arns, reason := launchK6(ctx, d, r, p, startAt)
	if reason != "" {
		stopAll(ctx, d, arns, "시험 취소")
		return finish(report.Canceled, reason)
	}
	if reason := waitRunning(ctx, d, r, arns, startAt); reason != "" {
		stopAll(ctx, d, arns, "시험 취소")
		return finish(report.Canceled, reason)
	}

	mon := monitor(ctx, d, r, p, arns, startAt)
	// 멈춘 태스크의 종료 코드와 k6 요약은 stopTimeout 안에 나온다. 다 멈출 때까지 기다린 뒤 모은다
	waitStopped(ctx, d, r, arns)
	s.Tasks = taskResults(ctx, d, r, id, arns)
	s.Events, s.Progress = mon.events, mon.progress

	// 기준을 넘어도 멈추지 않는다(NV-780). 결과가 무엇이든 구간 값을 남긴다. 끝까지 돌았으면(끝 시각 30초 전 안에
	// 끝난 것 포함) 계획한 구간을 모두, 도중에 멈췄으면(stop.yml, 시간 초과) 멈춘 때까지를 판정한다
	plannedEnd := startAt.Add(time.Duration(p.MainEnd) * time.Second)
	to := mon.at
	if mon.result == "" {
		to = latest(mon.at, plannedEnd)
	}
	stopped := finishedEarly(ctx, d, r, p, arns)
	for i, at := range mon.died {
		stopped[i] = at
	}
	// 도중에 멈춘 시험(stop.yml 이면 중단)은 판정 조회가 실패해도 멈춘 까닭의 결과를 지킨다
	failed := func(why string) (*report.Summary, error) {
		if mon.result != "" {
			return finish(mon.result, mon.reason+". "+why)
		}
		return finish(report.Inconclusive, why)
	}
	ws, gaps, why := judge(ctx, d, r.Config, p, startAt, to, mon.result != "", stopped)
	if why != "" {
		// 조회한 구간 값은 남긴다. 빠진 구간이 있어 기준을 지킨 마지막 단계는 적지 않는다
		s.Windows = ws
		return failed(why)
	}
	s.Windows, s.LastPassingStep = ws, lastPassingStep(ws)
	var queueWhy string
	if p.Queue != nil && len(ws) > 0 {
		c, inc, why := processed(ctx, d, p, startAt)
		if why != "" {
			return failed(why)
		}
		w := &ws[len(ws)-1]
		w.ByName = append(w.ByName, c)
		w.Pass = w.Pass && c.Pass
		queueWhy = inc
	}
	switch {
	case mon.result != "":
		return finish(mon.result, mon.reason)
	case len(mon.broken) > 0:
		return finish(report.Inconclusive, "계획한 부하를 끝까지 보내지 못했다: "+strings.Join(mon.broken, "; "))
	case len(gaps) > 0:
		return finish(report.Inconclusive, strings.Join(gaps, "; "))
	case len(ws) != len(p.Windows):
		return finish(report.Inconclusive, fmt.Sprintf("판정 구간 %d개 중 %d개만 판정했다", len(p.Windows), len(ws)))
	case queueWhy != "":
		// 계획한 인원을 k6 가 보내지 못했거나 결과를 셀 수 없다. 뒷단이 처리하지 못한 것과 구별해 판정하지 않는다
		return finish(report.Inconclusive, queueWhy)
	}
	return finish(judgment(p, ws))
}

// processed 는 queue 에서 입력한 인원이 모두 접수까지 갔는지 본다. 등록 확인에서 REGISTERED 를 본 회원(registered)과
// 접수만 확인한 회원(accepted)을 처리로 센다. 처리 기한에 끊긴 회원은 결과를 남기지 않아 모자란 수로 드러난다.
// inconclusive 는 판정하지 않을 까닭이다(VU 가 모자라 시작하지 못한 회원이 있다, 결과 라벨이 없다). why 는 조회 실패다
func processed(ctx context.Context, d Deps, p *plan.Plan, startAt time.Time) (c report.Check, inconclusive, why string) {
	if err := d.Clock.Sleep(ctx, ingestDelay); err != nil {
		return report.Check{}, "", err.Error()
	}
	end := startAt.Add(time.Duration(p.Estimates.TotalSeconds)*time.Second + stopTimeout)
	if now := d.Clock.Now(); now.Before(end) {
		end = now
	}
	by, err := d.Metrics.Outcomes(ctx, startAt, end)
	if err != nil {
		return report.Check{}, "", "진입 결과를 조회하지 못했다: " + err.Error()
	}
	dropped, err := d.Metrics.Dropped(ctx, startAt, end)
	if err != nil {
		return report.Check{}, "", "시작하지 못한 반복을 조회하지 못했다: " + err.Error()
	}
	var all float64
	for _, v := range by {
		all += v
	}
	if v := by[""]; v > 0 {
		// 라벨이 수집 경로에서 빠지면 모든 결과가 이름 없이 하나로 합쳐진다. 처리 인원을 0 으로 보고하지 않는다
		// (2026-10-22 운영 queue: 게이트웨이 keep_keys 에 outcome 이 없어 "0명 처리"로 나왔다)
		c = report.Check{Name: "queue:processed", Requests: all, Note: fmt.Sprintf("진입 결과 %.0f건에 결과 라벨(outcome)이 없다", v)}
		return c, "진입 결과에 outcome 라벨이 없어 처리 인원을 셀 수 없다. 수집 게이트웨이(nova-infra gateway/collector.yaml)의 " +
			"keep_keys 에 outcome 이 있는지 본다", ""
	}
	n := by["registered"] + by["accepted"]
	c = report.Check{Name: "queue:processed", Requests: n, Pass: n >= float64(p.Queue.Members)}
	var rest []string
	for _, k := range slices.Sorted(maps.Keys(by)) {
		if k != "registered" && k != "accepted" && by[k] > 0 {
			rest = append(rest, fmt.Sprintf("%s %.0f", k, by[k]))
		}
	}
	c.Note = fmt.Sprintf("%d명 중 %.0f명 처리(registered %.0f, accepted %.0f)", p.Queue.Members, n, by["registered"], by["accepted"])
	if len(rest) > 0 {
		c.Note += ", 실패 " + strings.Join(rest, ", ")
	}
	if miss := float64(p.Queue.Members) - all; miss > 0 {
		// 결과를 남기지 않은 회원: VU 가 모자라 시작하지 못했거나(dropped_iterations) 처리 기한에 끊겼다
		c.Note += fmt.Sprintf(", 결과 없음 %.0f(시작하지 못함 %.0f, 나머지는 처리 기한에 끊겼다)", miss, dropped)
	}
	if dropped > 0 {
		inconclusive = fmt.Sprintf("k6 VU 가 모자라 %.0f명이 진입을 시작하지 못했다(dropped_iterations). "+
			"계획한 인원을 보내지 못해 판정하지 않는다(구간 값은 남긴다)", dropped)
	}
	return c, inconclusive, ""
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return bytes.Equal(ja, jb)
}

// record는 summary.json 과 summary.md 를 쓴다.
func record(ctx context.Context, d Deps, s *report.Summary) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	if err := d.Store.Put(ctx, resultsKey(s.TestID, "summary.json"), b); err != nil {
		return fmt.Errorf("summary.json 기록: %w", err)
	}
	if err := d.Store.Put(ctx, resultsKey(s.TestID, "summary.md"), []byte(s.Markdown(""))); err != nil {
		return fmt.Errorf("summary.md 기록: %w", err)
	}
	d.Logf("결과: %s %s", s.Result, s.Reason)
	return nil
}

func stopAll(ctx context.Context, d Deps, arns []string, reason string) {
	if err := d.Runner.Stop(context.WithoutCancel(ctx), arns, reason); err != nil {
		d.Logf("태스크 멈추기 실패: %v", err)
	}
}

// mintTokens는 토큰 발급기를 띄우고 끝날 때까지 기다린다. 실패하면 까닭을 돌려준다.
func mintTokens(ctx context.Context, d Deps, r Run, p *plan.Plan) string {
	t := r.RunTask.Tasks["tokenMinter"]
	members, _ := json.Marshal(p.Members)
	env := []runner.Env{
		{Name: "TEST_ID", Value: p.TestID},
		{Name: "TOKENS_URI", Value: fmt.Sprintf("s3://%s/tokens/%s/", r.RunTask.Bucket, p.TestID)},
		{Name: "MEMBERS", Value: string(members)},
		{Name: "TASKS", Value: strconv.Itoa(p.Tasks)},
		{Name: "TOKEN_TTL_SECONDS", Value: strconv.Itoa(p.TokenTTLSeconds)},
	}
	arn, err := d.Runner.Run(ctx, t, p.TestID, env)
	if err != nil {
		return "토큰 발급기를 띄우지 못했다: " + err.Error()
	}
	deadline := d.Clock.Now().Add(minterTimeout)
	for d.Clock.Now().Before(deadline) {
		states, err := d.Runner.Describe(ctx, []string{arn}, t.Container)
		if err == nil && states[0].LastStatus == "STOPPED" {
			if c := states[0].ExitCode; c != nil && *c == 0 {
				d.Logf("토큰 발급 끝")
				return ""
			}
			return fmt.Sprintf("토큰 발급기가 실패했다(종료 코드 %s, %s)", code(states[0].ExitCode), states[0].StoppedReason)
		}
		if err := d.Clock.Sleep(ctx, 10*time.Second); err != nil {
			return err.Error()
		}
	}
	stopAll(ctx, d, []string{arn}, "토큰 발급 시간 초과")
	return fmt.Sprintf("토큰 발급이 %s 안에 끝나지 않았다", minterTimeout)
}

func coalesce(a, b *int32) *int32 {
	if a != nil {
		return a
	}
	return b
}

// waitStopped 는 태스크가 모두 STOPPED 가 될 때까지(최대 stopTimeout + 30초) 기다린다
func waitStopped(ctx context.Context, d Deps, r Run, arns []string) {
	ctx = context.WithoutCancel(ctx)
	deadline := d.Clock.Now().Add(stopTimeout + 30*time.Second)
	for d.Clock.Now().Before(deadline) {
		states, err := d.Runner.Describe(ctx, arns, r.RunTask.Tasks["k6"].Container)
		if err == nil {
			done := true
			for _, s := range states {
				done = done && s.LastStatus == "STOPPED"
			}
			if done {
				return
			}
		}
		if d.Clock.Sleep(ctx, 5*time.Second) != nil {
			return
		}
	}
	d.Logf("k6 태스크가 %s 안에 모두 멈추지 않았다", stopTimeout+30*time.Second)
}

// exitWhy 는 종료 코드와 ECS 가 준 멈춘 까닭이다. 까닭이 없으면 코드만 쓴다
func exitWhy(c *int32, reason string) string {
	if reason == "" {
		return "종료 코드 " + code(c)
	}
	return "종료 코드 " + code(c) + ", " + reason
}

func code(c *int32) string {
	if c == nil {
		return "없음"
	}
	return strconv.Itoa(int(*c))
}

// k6Plan은 k6 에 넘기는 계획이다. RunTask 덮어쓰기 상한(8KiB) 때문에 k6 가 쓰는 값만 남긴다.
// k6 스크립트가 PLAN.<이름> 으로 새 값을 읽으면 여기에도 넣는다(TestK6PlanHasWhatK6Reads 가 확인한다)
func k6Plan(p *plan.Plan) string {
	b, _ := json.Marshal(map[string]any{
		"test_id": p.TestID, "flows": p.Flows, "product_id": p.ProductID, "prep_product_id": p.PrepProductID,
		"scenarios": p.Scenarios, "open_at": p.OpenAt, "rush": p.Rush, "catalog": p.Catalog,
	})
	return string(b)
}

// segment는 태스크 i 의 k6 실행 구간이다. 예: N=3 이면 0:1/3, 1/3:2/3, 2/3:1
func segment(i, n int) (string, string) {
	frac := func(k int) string {
		switch k {
		case 0:
			return "0"
		case n:
			return "1"
		}
		return fmt.Sprintf("%d/%d", k, n)
	}
	seq := make([]string, n+1)
	for k := range seq {
		seq[k] = frac(k)
	}
	return frac(i) + ":" + frac(i+1), strings.Join(seq, ",")
}

// launchK6는 k6 태스크 N 대를 띄운다. 실패하면 띄운 것까지의 ARN 과 까닭을 돌려준다.
func launchK6(ctx context.Context, d Deps, r Run, p *plan.Plan, startAt time.Time) ([]string, string) {
	t := r.RunTask.Tasks["k6"]
	planJSON := k6Plan(p)
	var arns []string
	for i := range p.Tasks {
		seg, seq := segment(i, p.Tasks)
		env := []runner.Env{
			{Name: "TEST_ID", Value: p.TestID},
			{Name: "TASK_INDEX", Value: strconv.Itoa(i)},
			{Name: "START_AT", Value: startAt.UTC().Format(time.RFC3339)},
			{Name: "SEGMENT", Value: seg},
			{Name: "SEGMENT_SEQUENCE", Value: seq},
			{Name: "PLAN_JSON", Value: planJSON},
			{Name: "PRODUCT_ID", Value: p.ProductID},
			{Name: "PREP_PRODUCT_ID", Value: p.PrepProductID},
			{Name: "RESULTS_URI", Value: fmt.Sprintf("s3://%s/results/%s/", r.RunTask.Bucket, p.TestID)},
			{Name: "BASE_URL", Value: r.BaseURL},
			// 태스크마다 다른 값. service.instance.id 가 없으면 여러 태스크의 지표가 한 시계열로 섞인다
			{Name: "OTEL_RESOURCE_ATTRIBUTES", Value: fmt.Sprintf("service.instance.id=%d,service.namespace=loadtest", i)},
			// 수집 주소는 컨트롤러가 덮어쓴다. 전송 방식 같은 고정값은 작업 정의에 있다
			{Name: "K6_OTEL_HTTP_EXPORTER_ENDPOINT", Value: r.Observability.OTLPEndpoint},
		}
		if p.Members != nil {
			env = append(env, runner.Env{Name: "TOKENS_URI", Value: fmt.Sprintf("s3://%s/tokens/%s/", r.RunTask.Bucket, p.TestID)})
		}
		arn, err := d.Runner.Run(ctx, t, p.TestID, env)
		if err != nil {
			return arns, fmt.Sprintf("k6 태스크 %d를 띄우지 못했다: %v", i, err)
		}
		arns = append(arns, arn)
	}
	d.Logf("k6 태스크 %d대를 띄웠다. 시작 시각 %s", len(arns), startAt.Format(time.RFC3339))
	return arns, ""
}

// waitRunning은 시작 시각 전에 모든 태스크가 RUNNING 이고 이미지가 같은지 본다. 아니면 까닭을 돌려준다.
func waitRunning(ctx context.Context, d Deps, r Run, arns []string, startAt time.Time) string {
	container := r.RunTask.Tasks["k6"].Container
	for {
		states, err := d.Runner.Describe(ctx, arns, container)
		if err == nil {
			running := 0
			digests := map[string]bool{}
			for _, s := range states {
				if s.LastStatus == "RUNNING" {
					running++
					digests[s.ImageDigest] = true
				}
				if s.LastStatus == "STOPPED" {
					return fmt.Sprintf("k6 태스크가 시작 전에 멈췄다(%s)", s.StoppedReason)
				}
			}
			if running == len(arns) && !d.Clock.Now().Before(startAt) {
				// 시작 시각이 지난 뒤에야 모두 떴다. k6-runner 는 지난 시작 시각을 "바로 시작"으로 받으므로
				// 태스크마다 시작 시각이 어긋난다
				return fmt.Sprintf("시작 시각 %s가 지난 뒤에야 k6 태스크가 모두 RUNNING 이 되었다", startAt.Format(time.RFC3339))
			}
			if running == len(arns) {
				// 시험 도중 build.yml 이 latest 를 옮기면 태스크마다 이미지가 다를 수 있다
				if len(digests) > 1 {
					list := make([]string, 0, len(digests))
					for k := range digests {
						list = append(list, k)
					}
					return "k6 태스크의 이미지가 섞였다: " + strings.Join(list, ", ")
				}
				return ""
			}
		}
		if !d.Clock.Now().Before(startAt) {
			return fmt.Sprintf("시작 시각 %s까지 k6 태스크가 모두 RUNNING 이 되지 않았다", startAt.Format(time.RFC3339))
		}
		if err := d.Clock.Sleep(ctx, 5*time.Second); err != nil {
			return err.Error()
		}
	}
}

type outcome struct {
	result report.Result // 비면 계획대로 끝났다. 도중에 멈췄으면 중단(stop.yml)이나 판정 불가(시간 초과)다
	reason string
	at     time.Time // 끝난(멈춘) 시각
	// broken 은 계획한 부하를 끝까지 보내지 못한 태스크다(일찍 끝났거나 0 이 아닌 코드로 끝났다). 나머지는 계속 돈다
	broken   []string
	died     map[int]time.Time // 일찍 끝난 태스크의 멈춘 시각. 그 뒤로는 지표를 요구하지 않는다
	events   []string
	progress []report.Progress
}

// monitor는 시험이 끝날 때까지 감시한다. 기준을 넘거나 지표가 끊겨도 멈추지 않고 적어 둔다(출시 전이라 지킬 고객이
// 없다. NV-780). 멈추는 경우는 stop.yml(사람이 멈췄다)과 계획한 시간이 지나도 끝나지 않는 경우뿐이다
func monitor(ctx context.Context, d Deps, r Run, p *plan.Plan, arns []string, startAt time.Time) outcome {
	container := r.RunTask.Tasks["k6"].Container
	plannedEnd := startAt.Add(time.Duration(p.MainEnd) * time.Second)
	deadline := startAt.Add(time.Duration(p.Estimates.TotalSeconds)*time.Second + stopTimeout + time.Minute)
	o := outcome{died: map[int]time.Time{}}
	done := map[int]bool{}   // 이미 멈춘 태스크. 지표가 끊겨도 적지 않는다
	silent := map[int]bool{} // 지표 끊김을 적어 둔 태스크
	var failSince, lastProgress time.Time
	mimirDown := false

	elapsed := func(at time.Time) string { return report.Clock(int(at.Sub(startAt).Seconds())) }
	note := func(at time.Time, format string, a ...any) {
		msg := elapsed(at) + " " + fmt.Sprintf(format, a...)
		d.Logf("기록: %s", msg)
		o.events = append(o.events, msg)
	}
	// at 은 판정을 끝낼 시각이다. 태스크가 멈춘 것을 늦게 보았으면 ECS 의 멈춘 시각을 넘긴다
	stop := func(res report.Result, reason string, at time.Time) outcome {
		d.Logf("멈춘다(%s): %s", res, reason)
		stopAll(ctx, d, arns, reason)
		o.result, o.reason, o.at = res, reason, at
		return o
	}

	// 첫 지표가 들어올 때까지 기다린다(k6 내보내기 간격 10초)
	if wait := startAt.Add(monitorAfter).Sub(d.Clock.Now()); wait > 0 {
		if err := d.Clock.Sleep(ctx, wait); err != nil {
			o.result, o.reason, o.at = report.Inconclusive, err.Error(), d.Clock.Now()
			return o
		}
	}
	for {
		now := d.Clock.Now()
		states, err := d.Runner.Describe(ctx, arns, container)
		if err == nil {
			stopped := 0
			for i, s := range states {
				if s.LastStatus != "STOPPED" {
					continue
				}
				stopped++
				if done[i] {
					continue
				}
				done[i] = true
				at := now
				if s.StoppedAt != nil {
					at = *s.StoppedAt
				}
				early := now.Before(plannedEnd.Add(-endTolerance))
				c := s.ExitCode
				switch {
				case c != nil && *c == 105:
					// stop.yml 은 시험의 태스크를 모두 멈춘다. 사람이 멈춘 것이라 남은 태스크를 기다리지 않는다(끝 시각 근처여도 같다)
					return stop(report.Aborted, fmt.Sprintf("k6 태스크 %d가 밖에서 멈췄다(105, stop.yml)", i), at)
				case c != nil && *c == 0 && (!early || p.Queue != nil):
					// 끝 시각에 정상 종료했다. queue 는 맡은 회원을 모두 처리하면 처리 기한 전에 끝난다
				case early:
					// 남은 태스크는 계속 돌린다. 계획한 부하는 아니므로 결과는 판정 불가지만 구간 값은 남는다
					o.died[i] = at
					msg := fmt.Sprintf("k6 태스크 %d가 일찍 끝났다(%s)", i, exitWhy(c, s.StoppedReason))
					o.broken = append(o.broken, msg)
					note(at, "%s. 나머지 태스크는 계속 돈다", msg)
				default:
					msg := fmt.Sprintf("k6 태스크 %d가 끝 시각에 정상이 아닌 코드로 끝났다(%s)", i, exitWhy(c, s.StoppedReason))
					o.broken = append(o.broken, msg)
					note(at, "%s", msg)
				}
			}
			if stopped == len(states) {
				o.at = now
				return o
			}
		}
		if now.After(deadline) {
			return stop(report.Inconclusive, "계획한 시간이 지나도 k6 태스크가 끝나지 않았다", now)
		}

		// 끊김은 15초마다 보고, 진행 값은 남길 차례에만 조회한다(조회가 실패하면 다음 확인에 다시 조회한다)
		seen, err := d.Metrics.LastSeen(ctx, now)
		var live mimir.Live
		due := now.Sub(lastProgress) >= progressEvery
		if err == nil && due {
			live, err = d.Metrics.Progress(ctx, now)
		}
		if err != nil {
			if failSince.IsZero() {
				failSince = now
			}
			if now.Sub(failSince) > silentLimit && !mimirDown {
				mimirDown = true
				note(now, "Mimir 를 1분 넘게 조회하지 못한다(%v). 시험은 계속 돈다", err)
			}
		} else {
			if mimirDown {
				mimirDown = false
				note(now, "Mimir 조회가 돌아왔다")
			}
			failSince = time.Time{}
			if due {
				lastProgress = now
				pr := report.Progress{At: now, Elapsed: int(now.Sub(startAt).Seconds()), RPS: live.RPS, P95: report.Ratio(live.P95),
					P99: report.Ratio(live.P99), ErrorRate: report.Ratio(live.ErrorRate), Rate5xx: report.Ratio(live.Rate5xx), VUs: report.Ratio(live.VUs)}
				o.progress = append(o.progress, pr)
				d.Logf("진행 %s / %s | 초당 %.1f건 | p95 %s, p99 %s | 실패 %s, 5xx %s | VU %s", elapsed(now), report.Clock(p.MainEnd),
					pr.RPS, report.Ms(pr.P95), report.Ms(pr.P99), pct(live.ErrorRate), pct(live.Rate5xx), report.Count(pr.VUs))
			}
			for i := range arns {
				if done[i] {
					continue
				}
				age, ok := seen[strconv.Itoa(i)]
				if !ok || math.IsNaN(age) {
					age = now.Sub(startAt).Seconds()
				}
				switch {
				case age > silentLimit.Seconds() && now.Before(plannedEnd) && !silent[i]:
					silent[i] = true
					note(now, "k6 태스크 %d의 지표가 1분 넘게 들어오지 않는다. 시험은 계속 돈다", i)
				case age <= silentLimit.Seconds() && silent[i]:
					silent[i] = false
					note(now, "k6 태스크 %d의 지표가 다시 들어온다", i)
				}
			}
		}
		if err := d.Clock.Sleep(ctx, monitorEvery); err != nil {
			o.result, o.reason, o.at = report.Inconclusive, err.Error(), d.Clock.Now()
			return o
		}
	}
}

func pct(v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%.2f%%", v*100)
}

// finishedEarly 는 queue 에서 맡은 회원을 모두 처리하고 0으로 끝난 태스크의 멈춘 시각이다. 그 뒤로는 지표가 없다.
// queue 가 아니면 비어 있다
func finishedEarly(ctx context.Context, d Deps, r Run, p *plan.Plan, arns []string) map[int]time.Time {
	out := map[int]time.Time{}
	if p.Queue == nil {
		return out
	}
	states, err := d.Runner.Describe(context.WithoutCancel(ctx), arns, r.RunTask.Tasks["k6"].Container)
	if err != nil {
		return out // 모르면 구간 전체를 요구한다(판정 불가 쪽으로 기운다)
	}
	for i, s := range states {
		if s.LastStatus == "STOPPED" && s.ExitCode != nil && *s.ExitCode == 0 && s.StoppedAt != nil {
			out[i] = *s.StoppedAt
		}
	}
	return out
}

// judge는 판정 구간을 모두 판정한다. endedAt 뒤에 끝나는 구간은 partial 이면 endedAt 까지만 보고(도중에 멈춘 시험),
// 아니면 건너뛴다. stopped 는 일찍 끝난 태스크의 멈춘 시각이고, 그 태스크는 멈춘 때까지만 지표를 요구한다.
// 지표가 빠진 태스크는 gaps 에 적고 구간 값은 그대로 남긴다. why 는 Mimir 를 조회하지 못한 구간의 까닭이다.
// 한 구간의 조회가 실패해도 나머지 구간은 판정해 돌려준다
func judge(ctx context.Context, d Deps, cfg *config.Config, p *plan.Plan, startAt, endedAt time.Time, partial bool, stopped map[int]time.Time) (out []report.Window, gaps []string, why string) {
	var failed []string
	for _, w := range p.Windows {
		start := startAt.Add(time.Duration(w.Start) * time.Second)
		end := startAt.Add(time.Duration(w.End) * time.Second)
		cut := false
		if end.After(endedAt) {
			if !partial || endedAt.Sub(start) < minPartial {
				continue // 멈추기 전에 시작하지 않았거나 너무 짧게 돈 구간
			}
			end, cut = endedAt.Truncate(time.Second), true
		}
		cov, err := d.Metrics.Coverage(ctx, start, end)
		if err != nil {
			failed = append(failed, "구간 "+w.Name+"의 지표를 조회하지 못했다: "+err.Error())
			continue
		}
		for i := range p.Tasks {
			to := end
			if at, ok := stopped[i]; ok && at.Before(end) {
				to = at
			}
			want := to.Sub(start).Seconds()/exportEvery - coverageSlack
			if got := cov[strconv.Itoa(i)]; got < want {
				gaps = append(gaps, fmt.Sprintf("구간 %s에서 k6 태스크 %d의 지표가 빠졌다(표본 %.0f개, 기대 %.0f개 이상)", w.Name, i, got, want))
			}
		}
		total, byName, err := d.Metrics.Window(ctx, start, end)
		if err != nil {
			failed = append(failed, "구간 "+w.Name+"을 판정하지 못했다: "+err.Error())
			continue
		}
		secs := end.Sub(start).Seconds()
		rw := report.Window{Name: w.Name, Start: start, End: end, Partial: cut, Overall: check("all", total, cfg.SLO, secs)}
		if !(total.Requests > 0) {
			rw.Overall.Pass, rw.Overall.Note = false, "요청이 없다"
		}
		rw.Pass = rw.Overall.Pass
		// 계획한 흐름마다 대표 요청이 있어야 한다. 흐름 하나가 통째로 빠져도 남은 흐름만으로 합격하지 않게 한다
		for _, f := range p.Flows {
			name := flowName[f]
			if st, ok := byName[name]; !ok || !(st.Requests > 0) {
				rw.ByName = append(rw.ByName, report.Check{Name: "flow:" + f, Pass: false, Note: name + " 요청이 없다"})
				rw.Pass = false
			}
		}
		// 요청 이름은 흐름끼리 겹친다(예약 목록은 조회 흐름과 진입의 접수가 같은 이름이다). 그래서 k6 시나리오마다도 요청이
		// 있어야 한다. 준비(prepare)를 뺀 시나리오는 모든 판정 구간에 걸쳐 돈다. 준비는 판정 구간 앞에서 끝난다
		byScenario, err := d.Metrics.Scenarios(ctx, start, end)
		if err != nil {
			failed = append(failed, "구간 "+w.Name+"을 판정하지 못했다: "+err.Error())
			continue
		}
		for _, s := range p.Scenarios {
			if s.Flow != "prepare" && !(byScenario[s.Name] > 0) {
				rw.ByName = append(rw.ByName, report.Check{Name: "scenario:" + s.Name, Pass: false, Note: "요청이 없다"})
				rw.Pass = false
			}
		}
		for _, name := range sortedNames(byName) {
			c := check(name, byName[name], cfg.SLO, secs)
			rw.ByName = append(rw.ByName, c)
			// 흐름별 기준도 지켜야 합격이다(전체의 80% 가까이가 상품 조회라 전체만 보면 가려진다)
			rw.Pass = rw.Pass && c.Pass
		}
		out = append(out, rw)
	}
	return out, gaps, strings.Join(failed, "; ")
}

// flowName 은 흐름마다 반드시 보내는 요청이다(k6/lib/flows)
var flowName = map[string]string{
	"catalog":   "/api/v1/products",
	"preorders": "/api/v1/preorders", // 화면은 /active 를 부르지 않는다. 접수(POST)와 같은 이름이라 시나리오로도 본다
	"entry":     "/api/v1/preorders/queue",
	"order":     "/api/v1/orders",
	"cancel":    "/api/v1/preorders/{id}/cancel",
}

// check 는 기준 하나를 판정한다. p95 판정은 "0.5초 안에 끝난 요청이 95% 이상인가"다. 500ms 가 히스토그램 버킷
// 경계라 이 비율은 근사 없이 정확하다(docs/design.md "결과 합치기와 판정"). 경계는 metrics.le_500 이다
// 지연 백분위수, 실패율, 초당 요청은 보여 주기만 하고 판정에 쓰지 않는다. secs 는 구간 길이다
func check(name string, st mimir.Stat, slo config.SLO, secs float64) report.Check {
	c := report.Check{Name: name, Requests: st.Requests, Within: report.Ratio(st.Within), Rate5xx: report.Ratio(st.Rate5xx),
		ErrorRate: report.Ratio(st.ErrorRate), P50: report.Ratio(st.P50), P95: report.Ratio(st.P95), P99: report.Ratio(st.P99)}
	if secs > 0 && st.Requests > 0 {
		c.RPS = st.Requests / secs
	}
	if !(st.Requests > 0) {
		// 요청이 없으면 이 이름은 판정하지 않는다
		c.Pass = true
		return c
	}
	c.Pass = st.Within >= 0.95 && st.Rate5xx <= slo.Max5xxRate
	return c
}

func sortedNames(m map[string]mimir.Stat) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// lastPassingStep 은 stress 에서 처음부터 이어서 기준을 지킨 마지막 단계다.
func lastPassingStep(ws []report.Window) string {
	last := ""
	for _, w := range ws {
		// 도중에 멈춰 끝까지 보지 못한 단계는 기준을 지켰다고 하지 않는다
		if !w.Pass || w.Partial {
			break
		}
		if strings.HasPrefix(w.Name, "step") {
			last = w.Name
		}
	}
	return last
}

// judgment 는 끝까지 돈 시험의 결과다. stress 는 합격, 불합격 대신 한계 측정이다.
func judgment(p *plan.Plan, ws []report.Window) (report.Result, string) {
	if p.Scenario == "stress" {
		return report.Limit, ""
	}
	if len(ws) == 0 {
		return report.Inconclusive, "판정할 구간이 없다"
	}
	for _, w := range ws {
		if !w.Pass {
			return report.Fail, "구간 " + w.Name + "에서 기준을 넘었다"
		}
	}
	return report.Pass, ""
}

// taskResults 는 태스크마다 종료 코드와 k6 요약(k6-<i>.json)을 모은다.
func taskResults(ctx context.Context, d Deps, r Run, testID string, arns []string) []report.Task {
	ctx = context.WithoutCancel(ctx)
	states, _ := d.Runner.Describe(ctx, arns, r.RunTask.Tasks["k6"].Container)
	out := make([]report.Task, len(arns))
	for i, a := range arns {
		t := report.Task{Index: i, ARN: a}
		if i < len(states) {
			t.ExitCode, t.ImageDigest = states[i].ExitCode, states[i].ImageDigest
		}
		b, err := d.Store.Get(ctx, resultsKey(testID, fmt.Sprintf("k6-%d.json", i)))
		if err != nil {
			t.Error = "k6 요약이 없다"
			out[i] = t
			continue
		}
		var res struct {
			ExitCode *int32 `json:"exit_code"`
			Error    string `json:"error"`
			Summary  *struct {
				Metrics map[string]struct {
					Values map[string]float64 `json:"values"`
				} `json:"metrics"`
			} `json:"summary"`
		}
		if json.Unmarshal(b, &res) != nil {
			t.Error = "k6 요약을 읽지 못했다"
		} else if res.Summary == nil {
			// k6 를 띄우지 못했거나(토큰 받기 실패 등) 요약을 쓰지 못한 경우다. 결과 파일에는 그 까닭이 있다
			t.ExitCode, t.Error = coalesce(t.ExitCode, res.ExitCode), res.Error
		} else {
			t.Summary = true
			t.Error = res.Error
			t.Requests = res.Summary.Metrics["http_reqs"].Values["count"]
			t.Failed = res.Summary.Metrics["http_req_failed"].Values["passes"]
			t.ExitCode = coalesce(t.ExitCode, res.ExitCode)
		}
		out[i] = t
	}
	return out
}
