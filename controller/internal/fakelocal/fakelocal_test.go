package fakelocal

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"

	"github.com/Grandis-2/nova-loadtest/config"
	"github.com/Grandis-2/nova-loadtest/controller/internal/clock"
	"github.com/Grandis-2/nova-loadtest/controller/internal/infra"
	"github.com/Grandis-2/nova-loadtest/controller/internal/mimir"
	"github.com/Grandis-2/nova-loadtest/controller/internal/plan"
	"github.com/Grandis-2/nova-loadtest/controller/internal/runner"
)

const testID = "lt-20261020t0600-1"

func newServer(t *testing.T, scenario string) (*Server, *ecs.Client, *httptest.Server) {
	t.Helper()
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Config: cfg, SelfIP: "127.0.0.1"}
	if err := s.Use(context.Background(), scenario); err != nil {
		t.Fatal(err)
	}
	ecsH, mimirH, _ := s.Handlers()
	es := httptest.NewServer(ecsH)
	ms := httptest.NewServer(mimirH)
	t.Cleanup(func() { es.Close(); ms.Close() })
	c := ecs.New(ecs.Options{Region: "ap-northeast-2", BaseEndpoint: aws.String(es.URL), Credentials: aws.AnonymousCredentials{}})
	return s, c, ms
}

// 실제 SDK 로 RunTask, DescribeTasks, ListTasks, StopTask 가 오가는지 본다(프로토콜 모양)
func TestECSProtocol(t *testing.T) {
	_, c, _ := newServer(t, "manual-stop")
	r := &runner.Runner{ECS: c, Cluster: Cluster, Clock: clock.Real{}}
	task := infra.Task{TaskDefinition: FamilyK6, Container: "k6", Subnets: []string{"subnet-1"}, SecurityGroups: []string{"sg-1"}, AssignPublicIP: "ENABLED"}
	start := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	arn, err := r.Run(context.Background(), task, testID, []runner.Env{{Name: "START_AT", Value: start}, {Name: "TASK_INDEX", Value: "0"}, {Name: "TEST_ID", Value: testID}})
	if err != nil {
		t.Fatal(err)
	}
	states, err := r.Describe(context.Background(), []string{arn}, "k6")
	if err != nil || states[0].LastStatus != "PENDING" || states[0].StartedBy != testID || states[0].ImageDigest != digest {
		t.Fatalf("%+v %v", states, err)
	}
	arns, err := r.ListStartedBy(context.Background(), testID)
	if err != nil || len(arns) != 1 {
		t.Fatalf("%v %v", arns, err)
	}
	if err := r.Stop(context.Background(), arns, "test"); err != nil {
		t.Fatal(err)
	}
	// 멈추는 중(desiredStatus STOPPED)도 도는 것으로 센다
	running, err := r.ListRunning(context.Background())
	if err != nil || len(running) != 1 {
		t.Fatalf("멈추는 중인 태스크가 보여야 한다: %v %v", running, err)
	}
	ip, err := runner.FindMimir(context.Background(), c, infra.Mimir{Cluster: ObsCluster, Service: ObsService, Port: MimirPort, QueryPath: MimirPath})
	if err != nil || ip != "127.0.0.1" {
		t.Fatalf("관측 태스크 IP: %q %v", ip, err)
	}
}

// 관측 서비스가 재배포 중이면 늦게 시작한 태스크를 고른다
func TestObsRedeploy(t *testing.T) {
	_, c, _ := newServer(t, "obs-redeploy")
	ip, err := runner.FindMimir(context.Background(), c, infra.Mimir{Cluster: ObsCluster, Service: ObsService, Port: MimirPort, QueryPath: MimirPath})
	if err != nil || ip != "127.0.0.1" {
		t.Fatalf("늦게 시작한 태스크를 골라야 한다: %q %v", ip, err)
	}
}

// 컨트롤러의 쿼리(mimir.Metrics)에 각본대로 답한다
func metricsFor(t *testing.T, scenario string) (mimir.Metrics, *plan.Plan, time.Time) {
	t.Helper()
	s, _, ms := newServer(t, scenario)
	p, err := plan.New(s.Config, plan.Input{TestID: testID, Scenario: "smoke", ProductID: "a1b2c3d4-0000-4000-8000-000000000001", Duration: "2m", Runners: 2, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	s.plans[testID], s.starts[testID] = p, start
	u, _ := url.Parse(ms.URL)
	port, _ := strconv.Atoi(u.Port())
	q := &mimir.HTTP{Port: port, Path: MimirPath, Discover: func(context.Context) (string, error) { return u.Hostname(), nil }}
	return mimir.Metrics{Q: q, Names: s.Config.Metrics, TestID: testID}, p, start
}

func TestMimirPass(t *testing.T) {
	m, p, start := metricsFor(t, "pass")
	ctx := context.Background()
	if l, err := m.Progress(ctx, start.Add(90*time.Second)); err != nil || l.ErrorRate > 0.01 || l.RPS == 0 || l.VUs == 0 || !(l.P95 > 0 && l.P95 < 500) {
		t.Fatalf("%+v %v", l, err)
	}
	seen, err := m.LastSeen(ctx, start.Add(90*time.Second))
	if err != nil || len(seen) != p.Tasks || seen["1"] > 10 {
		t.Fatalf("%v %v", seen, err)
	}
	w := p.Windows[0]
	ws, we := start.Add(time.Duration(w.Start)*time.Second), start.Add(time.Duration(w.End)*time.Second)
	cov, err := m.Coverage(ctx, ws, we)
	if err != nil || cov["0"] < float64(w.End-w.Start)/10-1 {
		t.Fatalf("%v %v", cov, err)
	}
	total, byName, err := m.Window(ctx, ws, we)
	if err != nil || len(byName) != len(p.Flows) || math.Abs(total.Within-0.99) > 1e-9 || total.Requests == 0 || !(total.P99 > total.P50) {
		t.Fatalf("%+v %v %v", total, byName, err)
	}
}

func TestMimirScenarios(t *testing.T) {
	ctx := context.Background()
	m, _, start := metricsFor(t, "high-error-rate")
	if l, _ := m.Progress(ctx, start.Add(2*time.Minute)); l.ErrorRate < 0.2 || l.Rate5xx < 0.2 {
		t.Fatalf("오류율이 높아야 한다: %+v", l)
	}
	m, _, start = metricsFor(t, "inconclusive-silent")
	if seen, _ := m.LastSeen(ctx, start.Add(100*time.Second)); seen["1"] < 60 || seen["0"] > 10 {
		t.Fatalf("태스크 1만 1분 넘게 비어야 한다: %v", seen)
	}
	m, p, start := metricsFor(t, "inconclusive-gap")
	for sec := 0; sec < 300; sec += 15 {
		if seen, _ := m.LastSeen(ctx, start.Add(time.Duration(sec)*time.Second)); seen["0"] >= 60 {
			t.Fatalf("%d초: 감시에는 걸리지 않아야 한다: %v", sec, seen)
		}
	}
	w := p.Windows[len(p.Windows)-1]
	cov, _ := m.Coverage(ctx, start.Add(time.Duration(w.Start)*time.Second), start.Add(time.Duration(w.End)*time.Second))
	if cov["0"] >= float64(w.End-w.Start)/10-5 || cov["1"] < float64(w.End-w.Start)/10-1 {
		t.Fatalf("구간 판정에서 걸려야 한다: %v (%+v)", cov, w)
	}
	m, _, start = metricsFor(t, "mimir-down-recovers")
	if _, err := m.Progress(ctx, start.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("503 이어야 한다: %v", err)
	}
	if _, err := m.Progress(ctx, start.Add(3*time.Minute)); err != nil {
		t.Fatalf("다시 조회되어야 한다: %v", err)
	}
	m, p, start = metricsFor(t, "fail")
	w = p.Windows[0]
	_, byName, _ := m.Window(ctx, start.Add(time.Duration(w.Start)*time.Second), start.Add(time.Duration(w.End)*time.Second))
	if byName["/api/v1/orders"].Within != 0.90 {
		t.Fatalf("결제 흐름만 90%%: %+v", byName)
	}
}

// /state 의 running 은 멈추는 중인 태스크도 센다. 동시에 RunTask 가 와도 기록을 안전하게 읽는다(-race)
func TestStateCountsStoppingAndIsSafe(t *testing.T) {
	s, c, _ := newServer(t, "manual-stop")
	_, _, controlH := s.Handlers()
	cs := httptest.NewServer(controlH)
	t.Cleanup(cs.Close)
	r := &runner.Runner{ECS: c, Cluster: Cluster, Clock: clock.Real{}}
	task := infra.Task{TaskDefinition: FamilyK6, Container: "k6", Subnets: []string{"s"}, SecurityGroups: []string{"g"}, AssignPublicIP: "ENABLED"}
	arn, err := r.Run(context.Background(), task, testID, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Stop(context.Background(), []string{arn}, "test")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 20 {
			r.Run(context.Background(), task, "lt-20261020t0700-2", nil)
		}
	}()
	var st struct {
		Running []string `json:"running"`
		Stops   []string `json:"stops"`
	}
	for range 20 {
		resp, err := http.Get(cs.URL + "/state")
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
	}
	<-done
	if !slices.Contains(st.Running, arn) || len(st.Stops) != 1 {
		t.Fatalf("멈추는 중인 태스크가 running 에 있어야 한다: %+v", st)
	}
}

// 흐름별 요청 수는 계획의 흐름 비율(시작 수 × 흐름 하나의 요청 수)대로다
func TestMimirFlowWeights(t *testing.T) {
	m, p, start := metricsFor(t, "pass")
	cfg, _ := config.Default()
	w := p.Windows[0]
	_, byName, err := m.Window(context.Background(), start.Add(time.Duration(w.Start)*time.Second), start.Add(time.Duration(w.End)*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, fr := range p.FlowRates {
		name := flowNames[fr.Flow]
		want := fr.PeakRPS * cfg.Flows[fr.Flow].Requests * float64(w.End-w.Start)
		if got := byName[name].Requests; math.Abs(got-want) > 1e-6 {
			t.Errorf("%s: 요청 수 %v, 기대 %v", name, got, want)
		}
	}
	if byName["/api/v1/products"].Requests == byName["/api/v1/orders"].Requests {
		t.Fatal("흐름마다 요청 수가 달라야 한다")
	}
}
