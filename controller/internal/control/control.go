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
	"math"
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
	ErrorRate(ctx context.Context, at time.Time) (float64, error)
	Rate5xx(ctx context.Context, at time.Time) (float64, error)
	LastSeen(ctx context.Context, at time.Time) (map[string]float64, error)
	Coverage(ctx context.Context, start, end time.Time) (map[string]float64, error)
	Window(ctx context.Context, start, end time.Time) (mimir.Stat, map[string]mimir.Stat, error)
	Scenarios(ctx context.Context, start, end time.Time) (map[string]float64, error)
}

// 감시 규칙
const (
	monitorEvery  = 15 * time.Second
	monitorAfter  = time.Minute      // START_AT 뒤 이만큼 지나서 감시를 시작한다(첫 지표가 들어올 시간)
	silentLimit   = time.Minute      // 지표가 이보다 오래 없으면 판정 불가
	endTolerance  = 30 * time.Second // 계획한 끝 시각보다 이만큼 일찍 끝난 것까지 정상 종료로 본다
	stopTimeout   = 2 * time.Minute  // 태스크 정의의 stopTimeout. 멈춘 뒤 요약을 올릴 시간
	minterTimeout = 20 * time.Minute
	minLeadTime   = time.Minute // START_AT 이 지금부터 이보다 가까우면 태스크가 제때 뜨지 못한다
	exportEvery   = 10          // k6 OTel 내보내기 간격(초). 구간 누락 판단에 쓴다
	coverageSlack = 5           // 빠져도 되는 표본 수(약 50초. 1분 넘게 비면 판정 불가)
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

	outcome := monitor(ctx, d, r, p, arns, startAt)
	// 멈춘 태스크의 종료 코드와 k6 요약은 stopTimeout 안에 나온다. 다 멈출 때까지 기다린 뒤 모은다
	waitStopped(ctx, d, r, arns)
	s.Tasks = taskResults(ctx, d, r, id, arns)
	switch outcome.result {
	case report.Inconclusive:
		return finish(report.Inconclusive, outcome.reason)
	case report.Aborted:
		// stress 는 중단 기준에 걸려 멈추는 것도 정상 결과다. 멈추기 전에 끝난 단계까지 판정한다
		if p.Scenario == "stress" && outcome.byCriteria {
			ws, why := judge(ctx, d, r.Config, p, startAt, outcome.at)
			if why != "" {
				return finish(report.Inconclusive, why)
			}
			s.Windows, s.LastPassingStep = ws, lastPassingStep(ws)
			return finish(report.Limit, "중단 기준으로 멈췄다: "+outcome.reason)
		}
		return finish(report.Aborted, outcome.reason)
	}
	// 끝까지 돌았으면(끝 시각 30초 전 안에 끝난 것 포함) 계획한 구간을 모두 판정한다. 빠진 지표는 구간 누락으로 걸린다
	plannedEnd := startAt.Add(time.Duration(p.MainEnd) * time.Second)
	ws, why := judge(ctx, d, r.Config, p, startAt, latest(outcome.at, plannedEnd))
	if why != "" {
		return finish(report.Inconclusive, why)
	}
	if len(ws) != len(p.Windows) {
		return finish(report.Inconclusive, fmt.Sprintf("판정 구간 %d개 중 %d개만 판정했다", len(p.Windows), len(ws)))
	}
	s.Windows, s.LastPassingStep = ws, lastPassingStep(ws)
	return finish(judgment(p, ws))
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
		"abort_error_rate": p.AbortErrorRate, "scenarios": p.Scenarios, "open_at": p.OpenAt, "rush": p.Rush, "catalog": p.Catalog,
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
	result     report.Result // 비면 정상 종료
	reason     string
	at         time.Time // 끝난(멈춘) 시각
	byCriteria bool      // 중단 기준(오류율, 5xx)으로 멈췄는가
}

// monitor는 시험이 끝날 때까지 감시한다.
func monitor(ctx context.Context, d Deps, r Run, p *plan.Plan, arns []string, startAt time.Time) outcome {
	cfg := r.Config
	container := r.RunTask.Tasks["k6"].Container
	plannedEnd := startAt.Add(time.Duration(p.MainEnd) * time.Second)
	deadline := startAt.Add(time.Duration(p.Estimates.TotalSeconds)*time.Second + stopTimeout + time.Minute)
	abortErr := p.AbortErrorRate / 100
	abort5xx := cfg.SLO.Abort5xxRate / 100
	var breachSince, failSince time.Time

	stop := func(res report.Result, reason string, byCriteria bool) outcome {
		now := d.Clock.Now()
		d.Logf("멈춘다(%s): %s", res, reason)
		stopAll(ctx, d, arns, reason)
		return outcome{res, reason, now, byCriteria}
	}

	// 첫 지표가 들어올 때까지 기다린다(k6 내보내기 간격 10초)
	if wait := startAt.Add(monitorAfter).Sub(d.Clock.Now()); wait > 0 {
		if err := d.Clock.Sleep(ctx, wait); err != nil {
			return outcome{report.Inconclusive, err.Error(), d.Clock.Now(), false}
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
				// 계획한 끝 시각보다 일찍 끝난 태스크가 있으면 남은 태스크만으로는 계획한 부하가 아니다
				if now.Before(plannedEnd.Add(-endTolerance)) {
					switch c := s.ExitCode; {
					case c != nil && *c == 99:
						// k6 임계치는 컨트롤러의 오류율 중단 기준과 같은 기준이다. stress 에서는 한계 측정으로 본다
						return stop(report.Aborted, fmt.Sprintf("k6 태스크 %d가 임계치로 멈췄다(99)", i), true)
					case c != nil && *c == 105:
						return stop(report.Aborted, fmt.Sprintf("k6 태스크 %d가 밖에서 멈췄다(105, stop.yml)", i), false)
					default:
						return stop(report.Aborted, fmt.Sprintf("k6 태스크 %d가 일찍 끝났다(종료 코드 %s, %s)", i, code(c), s.StoppedReason), false)
					}
				}
			}
			if stopped == len(states) {
				// 끝 시각 근처에 끝났어도 0(정상)과 99(임계치, 판정은 Mimir 로 한다)가 아니면 정상 종료가 아니다
				for i, s := range states {
					if c := s.ExitCode; c == nil || (*c != 0 && *c != 99) {
						return outcome{report.Aborted, fmt.Sprintf("k6 태스크 %d가 종료 코드 %s로 끝났다(%s)", i, code(c), s.StoppedReason), now, false}
					}
				}
				return outcome{at: now}
			}
		}
		if now.After(deadline) {
			return stop(report.Inconclusive, "계획한 시간이 지나도 k6 태스크가 끝나지 않았다", false)
		}

		er, err1 := d.Metrics.ErrorRate(ctx, now)
		r5, err2 := d.Metrics.Rate5xx(ctx, now)
		seen, err3 := d.Metrics.LastSeen(ctx, now)
		if err := errors.Join(err1, err2, err3); err != nil {
			if failSince.IsZero() {
				failSince = now
			}
			if now.Sub(failSince) > silentLimit {
				return stop(report.Inconclusive, "Mimir 를 1분 넘게 조회하지 못했다: "+err.Error(), false)
			}
		} else {
			failSince = time.Time{}
			// NaN(요청 없음)은 넘지 않은 것으로 본다
			if er > abortErr || r5 > abort5xx {
				if breachSince.IsZero() {
					breachSince = now
				}
				if now.Sub(breachSince) > cfg.SLO.AbortAfter.D() {
					return stop(report.Aborted, fmt.Sprintf("중단 기준을 1분 넘게 넘었다(오류율 %s, 5xx %s)", pct(er), pct(r5)), true)
				}
			} else {
				breachSince = time.Time{}
			}
			for i := range arns {
				age, ok := seen[strconv.Itoa(i)]
				if !ok || math.IsNaN(age) {
					age = now.Sub(startAt).Seconds()
				}
				if age > silentLimit.Seconds() && now.Before(plannedEnd) {
					return stop(report.Inconclusive, fmt.Sprintf("k6 태스크 %d의 지표가 1분 넘게 들어오지 않았다", i), false)
				}
			}
		}
		if err := d.Clock.Sleep(ctx, monitorEvery); err != nil {
			return outcome{report.Inconclusive, err.Error(), d.Clock.Now(), false}
		}
	}
}

func pct(v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%.2f%%", v*100)
}

// judge는 끝난 판정 구간을 모두 판정한다. 구간에 지표가 빠진 태스크가 있으면 판정 불가다.
// 돌려주는 reason 은 판정 불가일 때의 까닭이다.
func judge(ctx context.Context, d Deps, cfg *config.Config, p *plan.Plan, startAt, endedAt time.Time) ([]report.Window, string) {
	var out []report.Window
	for _, w := range p.Windows {
		start := startAt.Add(time.Duration(w.Start) * time.Second)
		end := startAt.Add(time.Duration(w.End) * time.Second)
		if end.After(endedAt) {
			continue // 멈추기 전에 끝나지 않은 구간(stress)
		}
		cov, err := d.Metrics.Coverage(ctx, start, end)
		if err != nil {
			return nil, "구간 " + w.Name + "의 지표를 조회하지 못했다: " + err.Error()
		}
		want := float64(w.End-w.Start)/exportEvery - coverageSlack
		for i := range p.Tasks {
			if got := cov[strconv.Itoa(i)]; got < want {
				return nil, fmt.Sprintf("구간 %s에서 k6 태스크 %d의 지표가 빠졌다(표본 %.0f개, 기대 %.0f개 이상)", w.Name, i, got, want)
			}
		}
		total, byName, err := d.Metrics.Window(ctx, start, end)
		if err != nil {
			return nil, "구간 " + w.Name + "을 판정하지 못했다: " + err.Error()
		}
		rw := report.Window{Name: w.Name, Start: start, End: end, Overall: check("all", total, cfg.SLO)}
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
			return nil, "구간 " + w.Name + "을 판정하지 못했다: " + err.Error()
		}
		for _, s := range p.Scenarios {
			if s.Flow != "prepare" && !(byScenario[s.Name] > 0) {
				rw.ByName = append(rw.ByName, report.Check{Name: "scenario:" + s.Name, Pass: false, Note: "요청이 없다"})
				rw.Pass = false
			}
		}
		for _, name := range sortedNames(byName) {
			c := check(name, byName[name], cfg.SLO)
			rw.ByName = append(rw.ByName, c)
			// 흐름별 기준도 지켜야 합격이다(전체의 80% 가까이가 상품 조회라 전체만 보면 가려진다)
			rw.Pass = rw.Pass && c.Pass
		}
		out = append(out, rw)
	}
	return out, ""
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
func check(name string, st mimir.Stat, slo config.SLO) report.Check {
	c := report.Check{Name: name, Requests: st.Requests, Within: report.Ratio(st.Within), Rate5xx: report.Ratio(st.Rate5xx)}
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
		if !w.Pass {
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
