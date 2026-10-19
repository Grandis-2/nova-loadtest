package gha

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/Grandis-2/nova-loadtest/config"
	"github.com/Grandis-2/nova-loadtest/controller/internal/clock"
	"github.com/Grandis-2/nova-loadtest/controller/internal/fakeecs"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestGuard(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{"GitHub", map[string]string{"GITHUB_SERVER_URL": "https://github.com"}, true},
		{"GitHub 에서 인라인", map[string]string{"GITHUB_SERVER_URL": "https://github.com", "CONTROLLER_INLINE": "1"}, false},
		{"로컬", map[string]string{"GITHUB_SERVER_URL": "http://forgejo:3000", "AWS_ENDPOINT_URL": "http://moto:5000", "AWS_ENDPOINT_URL_ECS": "http://fake-ecs:8080", "CONTROLLER_INLINE": "1"}, true},
		{"로컬인데 실제 AWS", map[string]string{"GITHUB_SERVER_URL": "http://forgejo:3000", "AWS_ENDPOINT_URL": "http://moto:5000"}, false},
		{"로컬인데 외부 주소", map[string]string{"GITHUB_SERVER_URL": "http://forgejo:3000", "AWS_ENDPOINT_URL": "https://s3.amazonaws.com", "AWS_ENDPOINT_URL_ECS": "http://fake-ecs:8080"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Guard(envOf(tc.env)); (err == nil) != tc.ok {
				t.Fatalf("통과 %v 여야 한다: %v", tc.ok, err)
			}
		})
	}
}

func TestTestID(t *testing.T) {
	id, err := TestID(time.Date(2026, 10, 20, 6, 30, 59, 0, time.FixedZone("KST", 9*3600)), "12")
	if err != nil || id != "lt-20261019t2130-12" {
		t.Fatalf("UTC 로 만든다: %s %v", id, err)
	}
	if _, err := TestID(time.Now(), ""); err == nil {
		t.Fatal("run_number 가 없으면 실패해야 한다")
	}
}

func TestInputs(t *testing.T) {
	in, err := Inputs(envOf(map[string]string{
		"TEST_ID": "lt-20261020t0630-1", "INPUT_SCENARIO": "load", "INPUT_PRODUCT_ID": " p ", "INPUT_PEAK_RPS": "300",
		"INPUT_RUNNERS": "", "INPUT_FLOWS": "catalog, entry,", "PLANNED_AT": "2026-10-20T06:30:00Z",
	}))
	if err != nil || in.ProductID != "p" || in.PeakRPS != 300 || len(in.Flows) != 2 || in.Now.IsZero() {
		t.Fatalf("%+v %v", in, err)
	}
	if _, err := Inputs(envOf(map[string]string{"INPUT_PEAK_RPS": "많이"})); err == nil {
		t.Fatal("숫자가 아니면 실패해야 한다")
	}
}

const runTaskJSON = `{"cluster":"nova-prod-loadtest","bucket":"bucket-secret-1","tasks":{
"controller":{"taskDefinition":"nova-prod-loadtest-controller","container":"controller","subnets":["subnet-aaa"],"securityGroups":["sg-ccc"],"assignPublicIp":"DISABLED"},
"tokenMinter":{"taskDefinition":"nova-prod-loadtest-token-minter","container":"token-minter","subnets":["subnet-aaa"],"securityGroups":["sg-mmm"],"assignPublicIp":"DISABLED"},
"k6":{"taskDefinition":"nova-prod-loadtest-k6","container":"k6","subnets":["subnet-ppp"],"securityGroups":["sg-kkk"],"assignPublicIp":"ENABLED"}}}`

const obsJSON = `{"otlpEndpoint":"otel-ingest.nova.local:4318","mimir":{"cluster":"nova-prod-observability","service":"nova-prod-obs","port":9009,"queryPath":"/prometheus/api/v1/query"}}`

type fakeSSM map[string]string

func (f fakeSSM) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	v, ok := f[aws.ToString(in.Name)]
	if !ok {
		return nil, errors.New("ParameterNotFound")
	}
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String(v)}}, nil
}

type fakeS3 struct{ m map[string]string }

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	v, ok := f.m[aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(v))}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(false)}
	for k := range f.m {
		if strings.HasPrefix(k, aws.ToString(in.Prefix)) {
			out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k)})
		}
	}
	return out, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	delete(f.m, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

const product = "a1b2c3d4-0000-4000-8000-000000000001"

// catalogAPI 는 사전 점검이 보는 상품 상세다
func catalogAPI(status string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.UserAgent(), "nova-loadtest/lt-") {
			w.WriteHeader(400)
			return
		}
		fmt.Fprintf(w, `{"success":true,"data":{"status":%q,"variants":[{"status":"ACTIVE"}]}}`, status)
	}))
}

type runFixture struct {
	env    Env
	out    *bytes.Buffer
	ecs    *fakeecs.Fake
	s3     *fakeS3
	vars   map[string]string
	server *httptest.Server
}

func newRun(t *testing.T, summary string) *runFixture {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC))
	f := &runFixture{out: &bytes.Buffer{}, s3: &fakeS3{m: map[string]string{}}, server: catalogAPI("ACTIVE")}
	t.Cleanup(f.server.Close)
	f.ecs = &fakeecs.Fake{Clock: clk, Behave: func(string, map[string]string, int) fakeecs.Behavior {
		// 컨트롤러가 10분 뒤 결과를 남기고 끝난다
		if summary != "" {
			f.s3.m["results/lt-20261020t0600-1/summary.json"] = summary
		}
		return fakeecs.Behavior{RunningAfter: 30 * time.Second, ExitAt: clk.Now().Add(10 * time.Minute)}
	}}
	stepSummary := filepath.Join(t.TempDir(), "summary.md")
	f.vars = map[string]string{
		"GITHUB_SERVER_URL": "https://github.com", "TEST_ID": "lt-20261020t0600-1", "PLANNED_AT": "2026-10-20T06:00:00Z",
		"INPUT_SCENARIO": "smoke", "INPUT_PRODUCT_ID": product, "INPUT_DURATION": "2m", "BASE_URL": f.server.URL,
		"GITHUB_STEP_SUMMARY": stepSummary,
	}
	f.env = Env{SSM: fakeSSM{ParamRunTask: runTaskJSON, ParamObservability: obsJSON}, ECS: f.ecs, S3: f.s3,
		HTTP: f.server.Client(), Clock: clk, Getenv: envOf(f.vars), Out: f.out}
	return f
}

func cfg(t *testing.T) *config.Config {
	c, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRunPass(t *testing.T) {
	f := newRun(t, `{"test_id":"lt-20261020t0600-1","scenario":"smoke","result":"pass"}`)
	if err := Run(context.Background(), f.env, cfg(t)); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	// 가릴 값을 다른 출력보다 먼저 낸다. 공개된 이름은 가리지 않는다
	if !strings.HasPrefix(out, "::add-mask::") || !strings.Contains(out, "::add-mask::bucket-secret-1") || !strings.Contains(out, "::add-mask::subnet-ppp") {
		t.Fatalf("SSM 값을 먼저 가려야 한다:\n%s", out)
	}
	if strings.Contains(out, "::add-mask::nova-prod-loadtest\n") {
		t.Fatal("클러스터 이름은 가리지 않는다")
	}
	runs := f.ecs.RunsOf("nova-prod-loadtest-controller")
	if len(runs) != 1 || runs[0].StartedBy != "lt-20261020t0600-1" {
		t.Fatalf("컨트롤러를 시험 ID 로 한 번 띄워야 한다: %+v", runs)
	}
	e := runs[0].Env
	if e["MODE"] != "run" || e["RUN_TASK"] != runTaskJSON || e["OBSERVABILITY"] != obsJSON || !strings.Contains(e["INPUT_JSON"], `"planned_at":"2026-10-20T06:00:00Z"`) || e["PLAN_JSON"] == "" {
		t.Fatalf("컨트롤러 환경 변수: %v", e)
	}
	b, _ := os.ReadFile(f.vars["GITHUB_STEP_SUMMARY"])
	if !strings.Contains(string(b), "합격") {
		t.Fatalf("실행 요약에 결과를 써야 한다: %s", b)
	}
}

func TestRunFailResult(t *testing.T) {
	f := newRun(t, `{"test_id":"lt-20261020t0600-1","scenario":"smoke","result":"fail","reason":"구간 hold 에서 기준을 넘었다"}`)
	if err := Run(context.Background(), f.env, cfg(t)); err == nil || !strings.Contains(err.Error(), "fail") {
		t.Fatalf("불합격이면 작업이 실패해야 한다: %v", err)
	}
}

func TestRunNoSummaryIsInconclusive(t *testing.T) {
	f := newRun(t, "")
	err := Run(context.Background(), f.env, cfg(t))
	if err == nil || !strings.Contains(err.Error(), "판정 불가") {
		t.Fatalf("결과가 없으면 판정 불가로 실패해야 한다: %v", err)
	}
}

func TestRunPrecheckBlocks(t *testing.T) {
	f := newRun(t, `{"result":"pass"}`)
	f.server.Close()
	f.server = catalogAPI("PAUSED")
	f.vars["BASE_URL"] = f.server.URL
	f.env.HTTP = f.server.Client()
	err := Run(context.Background(), f.env, cfg(t))
	if err == nil || !strings.Contains(err.Error(), "사전 점검") {
		t.Fatalf("판매 중이 아닌 상품이면 컨트롤러를 띄우지 않는다: %v", err)
	}
	if len(f.ecs.Runs) != 0 {
		t.Fatal("컨트롤러를 띄우지 않아야 한다")
	}
}

func TestRunNeedsPlanOutputs(t *testing.T) {
	f := newRun(t, "")
	delete(f.vars, "PLANNED_AT")
	if err := Run(context.Background(), f.env, cfg(t)); err == nil || !strings.Contains(err.Error(), "PLANNED_AT") {
		t.Fatalf("plan 작업의 출력이 필요하다: %v", err)
	}
}

func TestAfterStopsAndDeletesTokens(t *testing.T) {
	f := newRun(t, "")
	f.s3.m["tokens/lt-20261020t0600-1/a-0.json"] = "[]"
	f.s3.m["tokens/lt-20261020t0700-2/a-0.json"] = "[]"
	f.ecs.Behave = func(string, map[string]string, int) fakeecs.Behavior {
		return fakeecs.Behavior{RunningAfter: time.Second}
	}
	f.env.ECS.RunTask(context.Background(), runInput("nova-prod-loadtest-k6", "lt-20261020t0600-1"))
	f.env.ECS.RunTask(context.Background(), runInput("nova-prod-loadtest-k6", "lt-20261020t0700-2"))
	if err := After(context.Background(), f.env, "lt-20261020t0600-1"); err != nil {
		t.Fatal(err)
	}
	if len(f.ecs.Stops) != 1 {
		t.Fatalf("이 시험의 태스크만 멈춘다: %v", f.ecs.Stops)
	}
	if _, ok := f.s3.m["tokens/lt-20261020t0600-1/a-0.json"]; ok {
		t.Fatal("이 시험의 토큰을 지워야 한다")
	}
	if _, ok := f.s3.m["tokens/lt-20261020t0700-2/a-0.json"]; !ok {
		t.Fatal("다른 시험의 토큰은 두어야 한다")
	}
}

func TestStopOnlyK6(t *testing.T) {
	f := newRun(t, "")
	f.ecs.Behave = func(string, map[string]string, int) fakeecs.Behavior {
		return fakeecs.Behavior{RunningAfter: time.Second}
	}
	f.env.ECS.RunTask(context.Background(), runInput("nova-prod-loadtest-controller", "lt-20261020t0600-1"))
	f.env.ECS.RunTask(context.Background(), runInput("nova-prod-loadtest-k6", "lt-20261020t0600-1"))
	if err := Stop(context.Background(), f.env, "nova-prod-loadtest", "lt-20261020t0600-1", "nova-prod-loadtest-k6"); err != nil {
		t.Fatal(err)
	}
	if len(f.ecs.Stops) != 1 || !strings.Contains(f.ecs.Stops[0], "-k6-") {
		t.Fatalf("k6 만 멈추고 컨트롤러는 남긴다(중단 결과를 쓴다): %v", f.ecs.Stops)
	}
}

func TestCleanupRefusedWithoutDataTask(t *testing.T) {
	f := newRun(t, "")
	err := Cleanup(context.Background(), f.env, "lt-20261020t0600-1")
	if err == nil || !strings.Contains(err.Error(), "tasks.data") {
		t.Fatalf("정리 태스크가 없으면 컨트롤러를 띄우기 전에 실패한다: %v", err)
	}
	if len(f.ecs.Runs) != 0 {
		t.Fatal("컨트롤러를 띄우지 않아야 한다")
	}
}

func TestPlanOutputs(t *testing.T) {
	f := newRun(t, "")
	f.vars["TEST_ID"] = ""
	f.vars["GITHUB_RUN_NUMBER"] = "7"
	id, at, err := Plan(f.env, cfg(t))
	if err != nil || id != "lt-20261020t0600-7" || at != "2026-10-20T06:00:00Z" {
		t.Fatalf("%s %s %v", id, at, err)
	}
	b, _ := os.ReadFile(f.vars["GITHUB_STEP_SUMMARY"])
	if !strings.Contains(string(b), "### 계획: smoke") {
		t.Fatalf("계획 표를 써야 한다: %s", b)
	}
}

func runInput(family, startedBy string) *ecs.RunTaskInput {
	return &ecs.RunTaskInput{Cluster: aws.String("nova-prod-loadtest"), TaskDefinition: aws.String(family), StartedBy: aws.String(startedBy), Count: aws.Int32(1)}
}

func TestIdle(t *testing.T) {
	f := newRun(t, "")
	if err := Idle(context.Background(), f.env, "nova-prod-loadtest"); err != nil {
		t.Fatal(err)
	}
	f.ecs.Behave = func(string, map[string]string, int) fakeecs.Behavior {
		return fakeecs.Behavior{RunningAfter: time.Second}
	}
	f.env.ECS.RunTask(context.Background(), runInput("nova-prod-loadtest-k6", "lt-20261020t0600-1"))
	if err := Idle(context.Background(), f.env, "nova-prod-loadtest"); err == nil {
		t.Fatal("도는 태스크가 있으면 latest 를 옮기지 않아야 한다")
	}
}

func TestCheckTestID(t *testing.T) {
	for id, ok := range map[string]bool{"lt-20261020t0600-1": true, "lt-20261020t0600-": false, "LT-20261020T0600-1": false, "lt-20261020t0600-1/../x": false, "": false} {
		if err := CheckTestID(id); (err == nil) != ok {
			t.Errorf("%q: %v", id, err)
		}
	}
}
