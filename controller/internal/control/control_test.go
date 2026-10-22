package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Grandis-2/nova-loadtest/config"
	"github.com/Grandis-2/nova-loadtest/controller/internal/clock"
	"github.com/Grandis-2/nova-loadtest/controller/internal/fakeecs"
	"github.com/Grandis-2/nova-loadtest/controller/internal/infra"
	"github.com/Grandis-2/nova-loadtest/controller/internal/mimir"
	"github.com/Grandis-2/nova-loadtest/controller/internal/plan"
	"github.com/Grandis-2/nova-loadtest/controller/internal/report"
	"github.com/Grandis-2/nova-loadtest/controller/internal/runner"
)

const (
	product = "a1b2c3d4-0000-4000-8000-000000000001"
	testID  = "lt-20261020t0630-1"
	minter  = "nova-prod-loadtest-token-minter"
	k6fam   = "nova-prod-loadtest-k6"
)

var t0 = time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)

type memStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *memStore) Put(_ context.Context, k string, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = b
	return nil
}

func (s *memStore) Get(_ context.Context, k string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[k]
	if !ok {
		return nil, ErrNotFound
	}
	return b, nil
}

func (s *memStore) DeletePrefix(_ context.Context, p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.m {
		if strings.HasPrefix(k, p) {
			delete(s.m, k)
		}
	}
	return nil
}

// fakeMetrics 는 Mimir 각본이다. 비어 있는 함수는 정상 값을 준다
type fakeMetrics struct {
	tasks int
	// live 가 nil 이면 진행 값은 정상이다
	live   func(at time.Time) (mimir.Live, error)
	seen   func(at time.Time) (map[string]float64, error)
	cov    func(start, end time.Time) map[string]float64
	window func(start, end time.Time) map[string]mimir.Stat
	// scenarios 가 nil 이면 모든 k6 시나리오가 요청을 보냈다
	scenarios func() map[string]float64
	// outcomes 가 nil 이면 queue 의 members 명이 모두 등록까지 갔다
	outcomes    func() map[string]float64
	members     int
	outcomesEnd time.Time // Outcomes 를 조회한 구간의 끝
	dropped     float64
	queried     bool
}

func (m *fakeMetrics) Progress(_ context.Context, at time.Time) (mimir.Live, error) {
	m.queried = true
	if m.live != nil {
		return m.live(at)
	}
	return mimir.Live{RPS: 100, P95: 200, P99: 400, ErrorRate: 0.001, VUs: 10}, nil
}

// errorRate 는 오류율만 바꾼 진행 값이다
func errorRate(f func(at time.Time) float64) func(time.Time) (mimir.Live, error) {
	return func(at time.Time) (mimir.Live, error) {
		return mimir.Live{RPS: 100, P95: 200, P99: 400, ErrorRate: f(at), Rate5xx: f(at), VUs: 10}, nil
	}
}

func (m *fakeMetrics) LastSeen(_ context.Context, at time.Time) (map[string]float64, error) {
	if m.seen != nil {
		return m.seen(at)
	}
	out := map[string]float64{}
	for i := range m.tasks {
		out[fmt.Sprint(i)] = 5
	}
	return out, nil
}

func (m *fakeMetrics) Coverage(_ context.Context, start, end time.Time) (map[string]float64, error) {
	if m.cov != nil {
		return m.cov(start, end), nil
	}
	out := map[string]float64{}
	for i := range m.tasks {
		out[fmt.Sprint(i)] = end.Sub(start).Seconds() / 10
	}
	return out, nil
}

func (m *fakeMetrics) Window(_ context.Context, start, end time.Time) (mimir.Stat, map[string]mimir.Stat, error) {
	by := allFlows(0.99)
	if m.window != nil {
		by = m.window(start, end)
	}
	var total mimir.Stat
	var w float64
	for _, s := range by {
		total.Requests += s.Requests
		w += s.Within * s.Requests
	}
	total.Within = w / total.Requests
	return total, by, nil
}

func (m *fakeMetrics) Scenarios(context.Context, time.Time, time.Time) (map[string]float64, error) {
	if m.scenarios != nil {
		return m.scenarios(), nil
	}
	return allScenarios(), nil
}

func (m *fakeMetrics) Outcomes(_ context.Context, _, end time.Time) (map[string]float64, error) {
	m.outcomesEnd = end
	if m.outcomes != nil {
		return m.outcomes(), nil
	}
	return map[string]float64{"registered": float64(m.members)}, nil
}

func (m *fakeMetrics) Dropped(context.Context, time.Time, time.Time) (float64, error) {
	return m.dropped, nil
}

// allScenarios 는 계획이 만드는 k6 시나리오가 모두 요청을 보낸 구간 값이다
func allScenarios() map[string]float64 {
	out := map[string]float64{}
	for _, n := range []string{"catalog", "preorders", "entry", "order", "cancel", "entry_burst", "entry_observe", "rush", "queue"} {
		out[n] = 100
	}
	return out
}

// allFlows 는 다섯 흐름의 대표 요청이 모두 있는 구간 값이다
func allFlows(within float64) map[string]mimir.Stat {
	out := map[string]mimir.Stat{}
	for _, name := range flowName {
		out[name] = mimir.Stat{Requests: 100, Within: within}
	}
	out["/api/v1/products"] = mimir.Stat{Requests: 1000, Within: within}
	return out
}

type fixture struct {
	ecs     *fakeecs.Fake
	store   *memStore
	metrics *fakeMetrics
	clock   *clock.Fake
	deps    Deps
	run     Run
	plan    *plan.Plan
}

// newFixture 는 다섯 흐름 smoke(2분)를 준비한다. k6 는 끝 시각에 0 으로 끝나는 것이 기본이다
func newFixture(t *testing.T, scenario string, k6 func(env map[string]string, n int, end time.Time) fakeecs.Behavior) *fixture {
	t.Helper()
	in := plan.Input{TestID: testID, Scenario: scenario, ProductID: product, Now: t0}
	if scenario == "smoke" {
		in.Duration = "2m"
	}
	if scenario == "rush" {
		in.OpenAt = t0.Add(20 * time.Minute).Format(time.RFC3339)
	}
	return newFixtureIn(t, in, k6)
}

// newFixtureIn 은 입력 in 으로 준비한다
func newFixtureIn(t *testing.T, in plan.Input, k6 func(env map[string]string, n int, end time.Time) fakeecs.Behavior) *fixture {
	t.Helper()
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(t0)
	p, err := plan.New(cfg, in)
	if err != nil {
		t.Fatal(err)
	}
	pj, _ := json.Marshal(p)
	f := &fixture{store: &memStore{m: map[string][]byte{}}, metrics: &fakeMetrics{tasks: p.Tasks}, clock: clk, plan: p}
	if p.Queue != nil {
		f.metrics.members = p.Queue.Members
	}
	f.ecs = &fakeecs.Fake{Clock: clk, Behave: func(family string, env map[string]string, n int) fakeecs.Behavior {
		if family == minter {
			return fakeecs.Behavior{RunningAfter: 5 * time.Second, ExitAt: clk.Now().Add(40 * time.Second), ExitCode: 0}
		}
		start, _ := time.Parse(time.RFC3339, env["START_AT"])
		end := start.Add(time.Duration(p.Estimates.TotalSeconds) * time.Second)
		b := fakeecs.Behavior{RunningAfter: 30 * time.Second, ExitAt: end, StopExitCode: 105, ImageDigest: "sha256:aaa"}
		if k6 != nil {
			b = k6(env, n, end)
		}
		return b
	}}
	rt := &infra.RunTask{Cluster: "nova-prod-loadtest", Bucket: "bucket-x", Tasks: map[string]infra.Task{
		"tokenMinter": {TaskDefinition: minter, Container: "token-minter", Subnets: []string{"s"}, SecurityGroups: []string{"g"}, AssignPublicIP: "DISABLED"},
		"k6":          {TaskDefinition: k6fam, Container: "k6", Subnets: []string{"p"}, SecurityGroups: []string{"g6"}, AssignPublicIP: "ENABLED"},
	}}
	obs := &infra.Observability{OTLPEndpoint: "otel-ingest.nova.local:4318"}
	f.deps = Deps{Runner: &runner.Runner{ECS: f.ecs, Cluster: rt.Cluster, Clock: clk}, Store: f.store, Metrics: f.metrics, Clock: clk,
		Logf: func(format string, a ...any) { t.Logf(format, a...) }}
	f.run = Run{Config: cfg, RunTask: rt, Observability: obs, Input: in, PlanJSON: string(pj), BaseURL: "https://api.example"}
	return f
}

func (f *fixture) execute(t *testing.T) *report.Summary {
	t.Helper()
	s, err := Execute(context.Background(), f.deps, f.run)
	if err != nil {
		t.Fatal(err)
	}
	var stored report.Summary
	if b, err := f.store.Get(context.Background(), "results/"+testID+"/summary.json"); err != nil || json.Unmarshal(b, &stored) != nil || stored.Result != s.Result {
		t.Fatalf("summary.json 을 남겨야 한다: %v", err)
	}
	if _, err := f.store.Get(context.Background(), "results/"+testID+"/plan.json"); err != nil {
		t.Fatal("plan.json 을 남겨야 한다")
	}
	return s
}

func expect(t *testing.T, s *report.Summary, want report.Result, reason string) {
	t.Helper()
	if s.Result != want || !strings.Contains(s.Reason, reason) {
		t.Fatalf("결과 %s(%s), 기대 %s(%q 포함)", s.Result, s.Reason, want, reason)
	}
}

func TestPass(t *testing.T) {
	f := newFixture(t, "smoke", nil)
	f.store.Put(context.Background(), "tokens/"+testID+"/a-0.json", []byte("[]"))
	s := f.execute(t)
	expect(t, s, report.Pass, "")
	if len(f.ecs.RunsOf(minter)) != 1 || len(f.ecs.RunsOf(k6fam)) != f.plan.Tasks {
		t.Fatalf("발급기 1번, k6 %d번 띄워야 한다", f.plan.Tasks)
	}
	if len(f.ecs.Stops) != 0 {
		t.Fatalf("합격이면 멈출 태스크가 없다: %v", f.ecs.Stops)
	}
	if _, err := f.store.Get(context.Background(), "tokens/"+testID+"/a-0.json"); !errors.Is(err, ErrNotFound) {
		t.Fatal("시험이 끝나면 토큰을 지워야 한다")
	}
}

func TestFailPaymentFlow(t *testing.T) {
	f := newFixture(t, "smoke", nil)
	f.metrics.window = func(time.Time, time.Time) map[string]mimir.Stat {
		by := allFlows(0.999)
		by["/api/v1/products"] = mimir.Stat{Requests: 10000, Within: 0.999}
		by["/api/v1/orders/{id}/payment-attempts"] = mimir.Stat{Requests: 100, Within: 0.90}
		return by
	}
	s := f.execute(t)
	expect(t, s, report.Fail, "기준을 넘었다")
	// 전체는 0.95 를 넘지만 결제 흐름이 걸려 불합격이다
	w := s.Windows[0]
	if !w.Overall.Pass || w.Pass {
		t.Fatalf("전체는 통과, 흐름별에서 걸려야 한다: %+v", w)
	}
}

// 오류율이 계속 높아도 멈추지 않고 끝까지 돌아 구간을 판정한다(NV-780). 진행 값은 1분마다 남는다
func TestHighErrorRateDoesNotStop(t *testing.T) {
	f := newFixture(t, "smoke", nil)
	f.metrics.live = errorRate(func(time.Time) float64 { return 0.3 })
	s := f.execute(t)
	expect(t, s, report.Pass, "")
	if len(f.ecs.Stops) != 0 {
		t.Fatalf("기준을 넘어도 태스크를 멈추지 않는다: %v", f.ecs.Stops)
	}
	if len(s.Progress) < 2 || float64(s.Progress[0].ErrorRate) != 0.3 {
		t.Fatalf("진행 값을 1분마다 남겨야 한다: %+v", s.Progress)
	}
	if md := s.Markdown(""); !strings.Contains(md, "**진행(최근 1분 값") || !strings.Contains(md, "| p50 | p95 | p99 |") {
		t.Fatalf("결과에 진행 표와 지연 칸이 있어야 한다:\n%s", md)
	}
}

// 태스크 하나가 일찍 죽어도 나머지는 계속 돈다. 계획한 부하가 아니라 판정 불가지만 구간 값은 남긴다
func TestEarlyTaskExitKeepsRunning(t *testing.T) {
	in := plan.Input{TestID: testID, Scenario: "smoke", ProductID: product, Duration: "2m", Runners: 3, Now: t0}
	f := newFixtureIn(t, in, func(env map[string]string, n int, end time.Time) fakeecs.Behavior {
		b := fakeecs.Behavior{RunningAfter: 30 * time.Second, ExitAt: end, StopExitCode: 105, ImageDigest: "sha256:aaa"}
		if n == 2 {
			start, _ := time.Parse(time.RFC3339, env["START_AT"])
			b.ExitAt, b.ExitCode = start.Add(3*time.Minute), 99
		}
		return b
	})
	s := f.execute(t)
	expect(t, s, report.Inconclusive, "k6 태스크 2가 일찍 끝났다(종료 코드 99")
	if len(f.ecs.Stops) != 0 {
		t.Fatalf("나머지 태스크는 멈추지 않는다: %v", f.ecs.Stops)
	}
	if len(s.Windows) != len(f.plan.Windows) || len(s.Events) == 0 {
		t.Fatalf("구간 값과 있었던 일을 남겨야 한다: %d %v", len(s.Windows), s.Events)
	}
}

// stop.yml 로 멈추면 중단이고, 멈춘 때까지의 구간 값을 남긴다
func TestManualStop105(t *testing.T) {
	f := newFixture(t, "soak", func(env map[string]string, _ int, _ time.Time) fakeecs.Behavior {
		start, _ := time.Parse(time.RFC3339, env["START_AT"])
		return fakeecs.Behavior{RunningAfter: 30 * time.Second, ExitAt: start.Add(30 * time.Minute), ExitCode: 105, StopExitCode: 105, ImageDigest: "sha256:aaa"}
	})
	s := f.execute(t)
	expect(t, s, report.Aborted, "stop.yml")
	if len(s.Windows) != 1 || !s.Windows[0].Partial || !s.Windows[0].End.Before(s.StartAt.Add(31*time.Minute)) {
		t.Fatalf("멈춘 때까지의 구간 하나를 남겨야 한다: %+v", s.Windows)
	}
}

// 끝 시각 30초 전 안에 stop.yml 로 멈춰도 판정 불가가 아니라 중단이다
func TestManualStopNearEnd(t *testing.T) {
	f := newFixture(t, "smoke", func(_ map[string]string, _ int, end time.Time) fakeecs.Behavior {
		return fakeecs.Behavior{RunningAfter: 30 * time.Second, ExitAt: end.Add(-10 * time.Second), ExitCode: 105, StopExitCode: 105, ImageDigest: "sha256:aaa"}
	})
	expect(t, f.execute(t), report.Aborted, "stop.yml")
}

// 지표가 끊겨도 멈추지 않는다. 끊긴 것은 있었던 일과 구간 누락으로 남는다
func TestSilentTaskDoesNotStop(t *testing.T) {
	f := newFixture(t, "smoke", nil)
	f.metrics.seen = func(time.Time) (map[string]float64, error) { return map[string]float64{}, nil } // 태스크 0 의 vus 가 없다
	f.metrics.cov = func(time.Time, time.Time) map[string]float64 { return map[string]float64{} }
	s := f.execute(t)
	expect(t, s, report.Inconclusive, "지표가 빠졌다")
	if len(f.ecs.Stops) != 0 || len(s.Windows) != 1 {
		t.Fatalf("멈추지 않고 구간 값을 남긴다: %v %d", f.ecs.Stops, len(s.Windows))
	}
	if !slices.ContainsFunc(s.Events, func(e string) bool { return strings.Contains(e, "1분 넘게 들어오지 않는다") }) {
		t.Fatalf("끊긴 것을 적어야 한다: %v", s.Events)
	}
}

// 시험 도중 Mimir 를 조회하지 못해도 멈추지 않는다. 끝난 뒤 조회되면 판정한다
func TestMimirDownDoesNotStop(t *testing.T) {
	f := newFixture(t, "smoke", nil)
	f.metrics.live = func(time.Time) (mimir.Live, error) { return mimir.Live{}, errors.New("HTTP 503") }
	s := f.execute(t)
	expect(t, s, report.Pass, "")
	if len(f.ecs.Stops) != 0 || !slices.ContainsFunc(s.Events, func(e string) bool { return strings.Contains(e, "Mimir 를 1분 넘게 조회하지 못한다") }) {
		t.Fatalf("멈추지 않고 적어 둔다: %v %v", f.ecs.Stops, s.Events)
	}
}

func TestInconclusiveGap(t *testing.T) {
	f := newFixture(t, "smoke", nil)
	f.metrics.cov = func(start, end time.Time) map[string]float64 {
		// 감시 중에는 보이지 않게 1분 미만씩 끊겨 모두 합쳐 2분이 빠졌다
		return map[string]float64{"0": end.Sub(start).Seconds()/10 - 12}
	}
	s := f.execute(t)
	expect(t, s, report.Inconclusive, "지표가 빠졌다")
	if len(s.Windows) != 1 {
		t.Fatalf("판정 불가여도 구간 값을 남긴다: %+v", s.Windows)
	}
}

func TestCanceledNotRunning(t *testing.T) {
	f := newFixture(t, "smoke", func(_ map[string]string, n int, end time.Time) fakeecs.Behavior {
		return fakeecs.Behavior{Never: true, StopExitCode: 105}
	})
	expect(t, f.execute(t), report.Canceled, "RUNNING 이 되지 않았다")
	if f.metrics.queried {
		t.Fatal("시작 취소면 Mimir 를 조회하지 않는다")
	}
	if len(f.ecs.Stops) != f.plan.Tasks {
		t.Fatalf("띄운 태스크를 모두 멈춰야 한다: %v", f.ecs.Stops)
	}
}

func TestCanceledMinterFailed(t *testing.T) {
	f := newFixture(t, "smoke", nil)
	behave := f.ecs.Behave
	f.ecs.Behave = func(family string, env map[string]string, n int) fakeecs.Behavior {
		if family == minter {
			return fakeecs.Behavior{RunningAfter: 5 * time.Second, ExitAt: f.clock.Now().Add(20 * time.Second), ExitCode: 1}
		}
		return behave(family, env, n)
	}
	expect(t, f.execute(t), report.Canceled, "토큰 발급기가 실패했다")
	if len(f.ecs.RunsOf(k6fam)) != 0 {
		t.Fatal("발급기가 실패하면 k6 를 띄우지 않는다")
	}
}

func TestCanceledMixedImages(t *testing.T) {
	f := newFixture(t, "load", func(_ map[string]string, n int, end time.Time) fakeecs.Behavior {
		digest := "sha256:aaa"
		if n == 1 {
			digest = "sha256:bbb"
		}
		return fakeecs.Behavior{RunningAfter: 30 * time.Second, ExitAt: end, StopExitCode: 105, ImageDigest: digest}
	})
	if f.plan.Tasks < 2 {
		f.run.Input.Runners = 2
		p, _ := plan.New(f.run.Config, f.run.Input)
		pj, _ := json.Marshal(p)
		f.run.PlanJSON, f.plan, f.metrics.tasks = string(pj), p, p.Tasks
	}
	s := f.execute(t)
	expect(t, s, report.Canceled, "이미지가 섞였다")
	if !strings.Contains(s.Reason, "sha256:aaa") || !strings.Contains(s.Reason, "sha256:bbb") {
		t.Fatalf("다이제스트 두 개를 적어야 한다: %s", s.Reason)
	}
}

// 수집 주소와 태스크마다 다른 값만 덮어쓴다. 전송 방식 같은 고정값은 작업 정의에 있다
func TestK6Overrides(t *testing.T) {
	f := newFixture(t, "load", nil)
	f.run.Input.Runners = 3
	p, _ := plan.New(f.run.Config, f.run.Input)
	pj, _ := json.Marshal(p)
	f.run.PlanJSON, f.plan, f.metrics.tasks = string(pj), p, p.Tasks
	f.execute(t)
	runs := f.ecs.RunsOf(k6fam)
	seen := map[string]bool{}
	for i, r := range runs {
		if r.StartedBy != testID || r.Container != "k6" || r.Network == nil || r.Network.AssignPublicIp != "ENABLED" {
			t.Fatalf("RunTask %d: %+v", i, r)
		}
		if r.Env["K6_OTEL_HTTP_EXPORTER_ENDPOINT"] != "otel-ingest.nova.local:4318" {
			t.Fatalf("수집 주소를 넘겨야 한다: %v", r.Env)
		}
		if _, ok := r.Env["K6_OTEL_EXPORTER_PROTOCOL"]; ok {
			t.Fatal("작업 정의의 고정값은 덮어쓰지 않는다")
		}
		attr := r.Env["OTEL_RESOURCE_ATTRIBUTES"]
		if seen[attr] || !strings.Contains(attr, fmt.Sprintf("service.instance.id=%d", i)) || !strings.Contains(attr, "service.namespace=loadtest") {
			t.Fatalf("태스크마다 다른 리소스 속성이어야 한다: %s", attr)
		}
		seen[attr] = true
		if r.Env["TASK_INDEX"] != fmt.Sprint(i) || r.Env["TOKENS_URI"] != "s3://bucket-x/tokens/"+testID+"/" {
			t.Fatalf("태스크 번호와 토큰 위치: %v", r.Env)
		}
		if runner.OverrideSize("k6", envList(r.Env)) > runner.MaxOverrideBytes {
			t.Fatal("덮어쓰기가 8KiB 를 넘는다")
		}
	}
	if runs[0].Env["SEGMENT"] != "0:1/3" || runs[2].Env["SEGMENT"] != "2/3:1" || runs[1].Env["SEGMENT_SEQUENCE"] != "0,1/3,2/3,1" {
		t.Fatalf("실행 구간: %v %v", runs[0].Env["SEGMENT"], runs[1].Env["SEGMENT_SEQUENCE"])
	}
}

func envList(m map[string]string) []runner.Env {
	var out []runner.Env
	for k, v := range m {
		out = append(out, runner.Env{Name: k, Value: v})
	}
	return out
}

func TestStressLimit(t *testing.T) {
	f := newFixture(t, "stress", nil)
	// 단계 3 은 본 시험 시작 + 10분부터다. 그 뒤로 0.5초 안 비율이 기준에 못 미친다
	mainStart := t0.Add(2*time.Minute + time.Duration(f.plan.MainStart)*time.Second)
	f.metrics.window = func(start, end time.Time) map[string]mimir.Stat {
		within := 0.99
		if !start.Before(mainStart.Add(10 * time.Minute)) {
			within = 0.8
		}
		return allFlows(within)
	}
	s := f.execute(t)
	expect(t, s, report.Limit, "")
	if s.LastPassingStep != "step2" || !s.Result.Success() {
		t.Fatalf("기준을 지킨 마지막 단계는 step2 다: %q", s.LastPassingStep)
	}
}

// stress 도 오류가 많아져도 멈추지 않고 다섯 단계를 모두 돈다
func TestStressRunsAllStepsDespiteErrors(t *testing.T) {
	f := newFixture(t, "stress", nil)
	mainStart := t0.Add(2*time.Minute + time.Duration(f.plan.MainStart)*time.Second)
	f.metrics.live = errorRate(func(at time.Time) float64 {
		if at.After(mainStart.Add(16 * time.Minute)) { // 단계 4 중간부터 오류가 많다
			return 0.3
		}
		return 0
	})
	s := f.execute(t)
	expect(t, s, report.Limit, "")
	if len(s.Windows) != 5 || len(f.ecs.Stops) != 0 {
		t.Fatalf("다섯 단계를 모두 판정해야 한다: %d %v", len(s.Windows), f.ecs.Stops)
	}
}

func TestPlanMismatch(t *testing.T) {
	f := newFixture(t, "smoke", nil)
	f.run.PlanJSON = strings.Replace(f.run.PlanJSON, `"tasks":1`, `"tasks":2`, 1)
	if _, err := Execute(context.Background(), f.deps, f.run); err == nil || !strings.Contains(err.Error(), "다르다") {
		t.Fatalf("계획이 다르면 멈춰야 한다: %v", err)
	}
	if len(f.ecs.Runs) != 0 {
		t.Fatal("아무것도 띄우지 않아야 한다")
	}
}

// 리뷰(#4)에서 나온 경우들

// 구간에 요청이 하나도 없으면 비율이 NaN 이다. 그래도 summary.json 을 남기고 불합격이어야 한다
func TestNoRequestsStillRecords(t *testing.T) {
	f := newFixture(t, "smoke", nil)
	f.metrics.window = func(time.Time, time.Time) map[string]mimir.Stat {
		return map[string]mimir.Stat{"/api/v1/products": {Requests: 0, Within: math.NaN(), Rate5xx: math.NaN()}}
	}
	s := f.execute(t)
	expect(t, s, report.Fail, "")
	b, _ := f.store.Get(context.Background(), "results/"+testID+"/summary.json")
	if !strings.Contains(string(b), `"within_500ms": null`) {
		t.Fatalf("NaN 은 null 로 남긴다: %s", b)
	}
}

// 계획한 흐름 하나의 요청이 통째로 없으면 나머지가 좋아도 불합격이다
func TestMissingFlowFails(t *testing.T) {
	f := newFixture(t, "smoke", nil)
	f.metrics.window = func(time.Time, time.Time) map[string]mimir.Stat {
		by := allFlows(0.99)
		delete(by, flowName["cancel"])
		return by
	}
	s := f.execute(t)
	expect(t, s, report.Fail, "")
	found := false
	for _, c := range s.Windows[0].ByName {
		found = found || (c.Name == "flow:cancel" && !c.Pass)
	}
	if !found {
		t.Fatalf("flow:cancel 이 미달로 나와야 한다: %+v", s.Windows[0].ByName)
	}
}

// 예약 조회 흐름의 대표 요청(예약 목록)은 진입의 접수와 이름이 같다. 이름으로는 있어 보여도 시나리오가 요청을 보내지
// 않았으면 불합격이다
func TestMissingScenarioFails(t *testing.T) {
	f := newFixture(t, "smoke", nil)
	f.metrics.scenarios = func() map[string]float64 {
		by := allScenarios()
		delete(by, "preorders")
		return by
	}
	s := f.execute(t)
	expect(t, s, report.Fail, "")
	found := false
	for _, c := range s.Windows[0].ByName {
		found = found || (c.Name == "scenario:preorders" && !c.Pass)
	}
	if !found {
		t.Fatalf("scenario:preorders 가 미달로 나와야 한다: %+v", s.Windows[0].ByName)
	}
}

// rush 는 몰린 회원이 주문까지 간다. 등록 알림이 오지 않아 주문 요청이 없으면 불합격이다
func TestRushWithoutOrdersFails(t *testing.T) {
	f := newFixture(t, "rush", nil)
	f.metrics.window = func(time.Time, time.Time) map[string]mimir.Stat {
		by := allFlows(0.99)
		delete(by, flowName["order"])
		return by
	}
	s := f.execute(t)
	expect(t, s, report.Fail, "")
	if len(s.Windows) != 1 || s.Windows[0].Name != "rush" {
		t.Fatalf("판정 구간은 rush 하나다: %+v", s.Windows)
	}
	found := false
	for _, c := range s.Windows[0].ByName {
		found = found || (c.Name == "flow:order" && !c.Pass)
	}
	if !found {
		t.Fatalf("flow:order 가 미달로 나와야 한다: %+v", s.Windows[0].ByName)
	}
}

// rush 도 대표 요청이 모두 있고 기준 안이면 합격이다
func TestRushPasses(t *testing.T) {
	f := newFixture(t, "rush", nil)
	f.metrics.window = func(time.Time, time.Time) map[string]mimir.Stat { return allFlows(0.99) }
	expect(t, f.execute(t), report.Pass, "")
}

// 끝 시각 근처라도 0 이 아닌 종료 코드(예: 107 스크립트 오류)는 정상 종료가 아니다. 구간 값은 남긴다
func TestBadExitAtEnd(t *testing.T) {
	f := newFixture(t, "smoke", func(_ map[string]string, _ int, end time.Time) fakeecs.Behavior {
		return fakeecs.Behavior{RunningAfter: 30 * time.Second, ExitAt: end, ExitCode: 107, StopExitCode: 105, ImageDigest: "sha256:aaa"}
	})
	s := f.execute(t)
	expect(t, s, report.Inconclusive, "종료 코드 107")
	if len(s.Windows) != 1 {
		t.Fatalf("구간 값을 남긴다: %+v", s.Windows)
	}
}

// ECS 조회가 늦게 반영돼 방금 띄운 태스크가 잠시 안 보여도 시험을 취소하지 않는다
func TestDescribeLagDoesNotCancel(t *testing.T) {
	f := newFixture(t, "smoke", func(_ map[string]string, _ int, end time.Time) fakeecs.Behavior {
		return fakeecs.Behavior{HiddenFor: 20 * time.Second, RunningAfter: 30 * time.Second, ExitAt: end, StopExitCode: 105, ImageDigest: "sha256:aaa"}
	})
	expect(t, f.execute(t), report.Pass, "")
}

// 컨트롤러가 늦게 떠도 러너가 계획한 시각으로 다시 계산한다. 시작 시각이 촉박하면 결과(취소)를 남긴다
func TestPlannedAtUsedForRecompute(t *testing.T) {
	f := newFixture(t, "smoke", nil)
	in := plan.Input{TestID: testID, Scenario: "spike", ProductID: product, PrepProductID: "a1b2c3d4-0000-4000-8000-000000000002",
		OpenAt: t0.Add(5 * time.Minute).Format(time.RFC3339), Now: t0}
	p, err := plan.New(f.run.Config, in)
	if err != nil {
		t.Fatal(err)
	}
	pj, _ := json.Marshal(p)
	f.run.Input, f.run.PlanJSON = in, string(pj)
	f.clock.Sleep(context.Background(), 3*time.Minute) // 컨트롤러가 3분 늦게 떴다
	s := f.execute(t)
	expect(t, s, report.Canceled, "너무 가깝다")
	if len(f.ecs.Runs) != 0 {
		t.Fatal("시작 시각이 촉박하면 아무것도 띄우지 않는다")
	}
}

// k6 를 띄우지 못한 태스크의 결과 파일에는 요약이 없다. 요약 있음으로 보면 안 된다
func TestTaskResultWithoutSummary(t *testing.T) {
	f := newFixture(t, "smoke", nil)
	f.store.Put(context.Background(), "results/"+testID+"/k6-0.json", []byte(`{"exit_code":120,"error":"토큰 받기 실패"}`))
	s := f.execute(t)
	if s.Tasks[0].Summary || s.Tasks[0].Error != "토큰 받기 실패" {
		t.Fatalf("요약이 없는 결과다: %+v", s.Tasks[0])
	}
}

// 멈춘 뒤 모든 태스크가 STOPPED 가 될 때까지 기다려 종료 코드를 남긴다
func TestAbortRecordsExitCodes(t *testing.T) {
	in := plan.Input{TestID: testID, Scenario: "smoke", ProductID: product, Duration: "2m", Runners: 2, Now: t0}
	f := newFixtureIn(t, in, func(env map[string]string, n int, end time.Time) fakeecs.Behavior {
		b := fakeecs.Behavior{RunningAfter: 30 * time.Second, ExitAt: end, StopExitCode: 105, ImageDigest: "sha256:aaa"}
		if n == 0 { // stop.yml 이 태스크 0 부터 멈췄다
			start, _ := time.Parse(time.RFC3339, env["START_AT"])
			b.ExitAt, b.ExitCode = start.Add(2*time.Minute), 105
		}
		return b
	})
	s := f.execute(t)
	expect(t, s, report.Aborted, "stop.yml")
	for _, task := range s.Tasks {
		if task.ExitCode == nil || *task.ExitCode != 105 {
			t.Fatalf("멈춘 태스크의 종료 코드(105)를 남겨야 한다: %+v", task)
		}
	}
}

// 토큰 발급이 몇 분 걸려도 시작 시각은 발급이 끝난 뒤에 정하므로 취소되지 않는다
func TestSlowMinterStillRuns(t *testing.T) {
	f := newFixture(t, "smoke", nil)
	behave := f.ecs.Behave
	f.ecs.Behave = func(family string, env map[string]string, n int) fakeecs.Behavior {
		if family == minter {
			return fakeecs.Behavior{RunningAfter: time.Minute, ExitAt: f.clock.Now().Add(3 * time.Minute), ExitCode: 0}
		}
		return behave(family, env, n)
	}
	s := f.execute(t)
	expect(t, s, report.Pass, "")
	if !s.StartAt.After(t0.Add(3 * time.Minute)) {
		t.Fatalf("시작 시각 %s는 발급이 끝난 뒤여야 한다", s.StartAt)
	}
}

// 시작 시각이 지난 뒤에야 모두 RUNNING 이 되면 취소한다(k6 가 태스크마다 어긋난 시각에 바로 시작한다)
func TestRunningAfterStartAtCancels(t *testing.T) {
	f := newFixture(t, "smoke", nil)
	arn, err := f.deps.Runner.Run(context.Background(), f.run.RunTask.Tasks["k6"], testID, []runner.Env{{Name: "START_AT", Value: t0.Format(time.RFC3339)}})
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Sleep(context.Background(), 5*time.Minute) // 모두 RUNNING 이지만 시작 시각이 지났다
	reason := waitRunning(context.Background(), f.deps, f.run, []string{arn}, t0.Add(2*time.Minute))
	if !strings.Contains(reason, "지난 뒤에야") {
		t.Fatalf("시작 시각이 지났으면 취소해야 한다: %q", reason)
	}
}

// stress 태스크가 끝 시각 조금 전에 정상 종료해도 마지막 단계까지 판정한다
func TestStressEarlyNormalExitJudgesAllSteps(t *testing.T) {
	f := newFixture(t, "stress", nil)
	behave := f.ecs.Behave
	f.ecs.Behave = func(family string, env map[string]string, n int) fakeecs.Behavior {
		b := behave(family, env, n)
		if family == k6fam {
			start, _ := time.Parse(time.RFC3339, env["START_AT"])
			b.ExitAt = start.Add(time.Duration(f.plan.MainEnd)*time.Second - 20*time.Second)
		}
		return b
	}
	s := f.execute(t)
	if len(s.Windows) != 5 || s.LastPassingStep != "step5" {
		t.Fatalf("다섯 단계를 모두 판정해야 한다: %d %q (%s %s)", len(s.Windows), s.LastPassingStep, s.Result, s.Reason)
	}
}

// queue 는 모두 처리되면 처리 기한 전에 끝난다. 먼저 끝난 태스크는 지표가 끊겨도 판정 불가가 아니다
func TestQueueEndsEarly(t *testing.T) {
	var startAt time.Time
	f := newFixture(t, "queue", func(env map[string]string, n int, _ time.Time) fakeecs.Behavior {
		startAt, _ = time.Parse(time.RFC3339, env["START_AT"])
		exit := startAt.Add(150 * time.Second) // 줄이 늦게 빠져 처리 기한(219초) 안에 끝난다
		if n == 0 {
			exit = startAt.Add(40 * time.Second)
		}
		return fakeecs.Behavior{RunningAfter: 30 * time.Second, ExitAt: exit, ExitCode: 0, StopExitCode: 105, ImageDigest: "sha256:aaa"}
	})
	f.metrics.seen = func(at time.Time) (map[string]float64, error) {
		out := map[string]float64{}
		for i := range f.plan.Tasks {
			out[fmt.Sprint(i)] = 5
		}
		out["0"] = at.Sub(startAt.Add(40 * time.Second)).Seconds() // 태스크 0 은 먼저 끝났다
		return out, nil
	}
	s := f.execute(t)
	expect(t, s, report.Pass, "")
	c := s.Windows[0].ByName[len(s.Windows[0].ByName)-1]
	if c.Name != "queue:processed" || !c.Pass || !strings.Contains(c.Note, "5000명 중 5000명 처리") {
		t.Fatalf("처리 인원을 판정해야 한다: %+v", c)
	}
	if len(f.ecs.Stops) != 0 {
		t.Fatalf("일찍 끝난 것은 중단이 아니다: %v", f.ecs.Stops)
	}
	// 마지막 태스크가 끝난 뒤 마지막 지표가 Mimir 에 들어올 때까지 기다렸다가 센다
	if got := f.metrics.outcomesEnd.Sub(startAt.Add(150 * time.Second)); got < ingestDelay {
		t.Fatalf("처리 인원을 마지막 태스크가 끝나고 %s 뒤에 셌다(%s 이상 기다려야 한다)", got, ingestDelay)
	}
}

func TestQueueNotAllProcessed(t *testing.T) {
	f := newFixture(t, "queue", nil)
	f.metrics.outcomes = func() map[string]float64 {
		return map[string]float64{"registered": 4900, "accepted": 90, "poll_limit": 4, "accept_failed": 1}
	}
	s := f.execute(t)
	expect(t, s, report.Fail, "기준을 넘었다")
	c := s.Windows[0].ByName[len(s.Windows[0].ByName)-1]
	if c.Pass || c.Note != "5000명 중 4990명 처리(registered 4900, accepted 90), 실패 accept_failed 1, poll_limit 4, "+
		"결과 없음 5(시작하지 못함 0, 나머지는 처리 기한에 끊겼다)" {
		t.Fatalf("모자란 인원과 실패를 보여야 한다: %+v", c)
	}
}

// queue 도 0 이 아닌 종료 코드로 일찍 끝나면 계획한 부하가 아니다(판정 불가)
func TestQueueEarlyFailureIsInconclusive(t *testing.T) {
	f := newFixture(t, "queue", func(env map[string]string, _ int, _ time.Time) fakeecs.Behavior {
		start, _ := time.Parse(time.RFC3339, env["START_AT"])
		return fakeecs.Behavior{RunningAfter: 30 * time.Second, ExitAt: start.Add(90 * time.Second), ExitCode: 1, StopExitCode: 105, ImageDigest: "sha256:aaa"}
	})
	expect(t, f.execute(t), report.Inconclusive, "일찍 끝났다")
}

// 판정 구간(약 11분)보다 훨씬 일찍 0으로 끝난 태스크는 멈춘 때까지만 지표를 요구한다(PR #16 리뷰).
// 0이 아닌 코드로 끝났거나 계속 돈 태스크는 구간 전체를 요구한다
func TestQueueTaskFinishesFarEarly(t *testing.T) {
	in := plan.Input{TestID: testID, Scenario: "queue", ProductID: product, Members: 99980, PeakRPS: 165, Now: t0}
	var startAt time.Time
	f := newFixtureIn(t, in, func(env map[string]string, n int, _ time.Time) fakeecs.Behavior {
		startAt, _ = time.Parse(time.RFC3339, env["START_AT"])
		exit := startAt.Add(700 * time.Second)
		if n == 0 {
			exit = startAt.Add(100 * time.Second)
		}
		return fakeecs.Behavior{RunningAfter: 30 * time.Second, ExitAt: exit, ExitCode: 0, StopExitCode: 105, ImageDigest: "sha256:aaa"}
	})
	if w := f.plan.Windows[0]; w.End-w.Start < 600 {
		t.Fatalf("판정 구간이 길어야 이 경우를 본다: %+v", w)
	}
	f.metrics.seen = func(at time.Time) (map[string]float64, error) {
		out := map[string]float64{}
		for i := range f.plan.Tasks {
			out[fmt.Sprint(i)] = 5
		}
		out["0"] = at.Sub(startAt.Add(100 * time.Second)).Seconds()
		return out, nil
	}
	// 태스크 0 은 100초까지만 표본이 있다
	f.metrics.cov = func(start, end time.Time) map[string]float64 {
		out := map[string]float64{}
		for i := range f.plan.Tasks {
			out[fmt.Sprint(i)] = end.Sub(start).Seconds() / 10
		}
		out["0"] = startAt.Add(100*time.Second).Sub(start).Seconds() / 10
		return out
	}
	expect(t, f.execute(t), report.Pass, "")
}

// k6 가 VU 부족으로 회원을 들여보내지 못했으면 뒷단의 불합격이 아니라 판정 불가다(PR #16 리뷰)
func TestQueueDroppedIsInconclusive(t *testing.T) {
	f := newFixture(t, "queue", nil)
	f.metrics.dropped = 30
	f.metrics.outcomes = func() map[string]float64 { return map[string]float64{"registered": 4970} }
	s := f.execute(t)
	expect(t, s, report.Inconclusive, "30명이 진입을 시작하지 못했다")
	c := s.Windows[0].ByName[len(s.Windows[0].ByName)-1]
	if c.Pass || !strings.Contains(c.Note, "결과 없음 30(시작하지 못함 30") {
		t.Fatalf("구간 값과 처리 인원은 남긴다: %+v", c)
	}
}

// 수집 경로에서 outcome 라벨이 빠지면 결과가 이름 없이 합쳐진다. "0명 처리"로 불합격시키지 않고 판정 불가로 알린다
func TestQueueOutcomeLabelMissing(t *testing.T) {
	f := newFixture(t, "queue", nil)
	f.metrics.outcomes = func() map[string]float64 { return map[string]float64{"": 3102} }
	s := f.execute(t)
	expect(t, s, report.Inconclusive, "outcome 라벨이 없어")
	c := s.Windows[0].ByName[len(s.Windows[0].ByName)-1]
	if c.Name != "queue:processed" || !strings.Contains(c.Note, "3102건에 결과 라벨(outcome)이 없다") {
		t.Fatalf("라벨이 없다고 적어야 한다: %+v", c)
	}
}
