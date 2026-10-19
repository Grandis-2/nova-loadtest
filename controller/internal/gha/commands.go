package gha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/Grandis-2/nova-loadtest/config"
	"github.com/Grandis-2/nova-loadtest/controller/internal/clock"
	"github.com/Grandis-2/nova-loadtest/controller/internal/infra"
	"github.com/Grandis-2/nova-loadtest/controller/internal/plan"
	"github.com/Grandis-2/nova-loadtest/controller/internal/report"
	"github.com/Grandis-2/nova-loadtest/controller/internal/runner"
)

// SSM 파라미터 이름. nova-infra 시험 스택 ssm.tf 와 같다
const (
	ParamRunTask       = "/nova-prod/loadtest/run-task"
	ParamObservability = "/nova-prod/loadtest/observability"
)

// SSM, S3, Logs 는 명령이 쓰는 AWS API 다. 테스트는 가짜로 바꾼다.
type SSM interface {
	GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

type S3 interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

type Logs interface {
	GetLogEvents(context.Context, *cloudwatchlogs.GetLogEventsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error)
}

// Env 는 명령이 쓰는 클라이언트와 환경이다.
type Env struct {
	SSM    SSM
	ECS    runner.ECS
	S3     S3
	Logs   Logs
	HTTP   *http.Client
	Clock  clock.Clock
	Getenv func(string) string
	Out    io.Writer // 표준 출력(Actions 가 ::add-mask:: 같은 명령을 읽는다)
}

func (e Env) printf(format string, a ...any) { fmt.Fprintf(e.Out, format+"\n", a...) }

func (e Env) param(ctx context.Context, name string) (string, error) {
	out, err := e.SSM.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name)})
	if err != nil {
		return "", fmt.Errorf("SSM %s: %w", name, err)
	}
	return aws.ToString(out.Parameter.Value), nil
}

// readInfra 는 SSM 두 값을 읽고, 다른 무엇을 출력하기 전에 가린다.
func (e Env) readInfra(ctx context.Context, mode infra.Mode) (string, string, *infra.RunTask, *infra.Observability, error) {
	rtRaw, err := e.param(ctx, ParamRunTask)
	if err != nil {
		return "", "", nil, nil, err
	}
	obsRaw := ""
	if mode == infra.ModeRun {
		if obsRaw, err = e.param(ctx, ParamObservability); err != nil {
			return "", "", nil, nil, err
		}
	}
	masks, err := MaskLines(rtRaw, obsRaw)
	if err != nil {
		return "", "", nil, nil, err
	}
	for _, m := range masks {
		e.printf("%s", m)
	}
	rt, err := infra.ParseRunTask(rtRaw, mode)
	if err != nil {
		return "", "", nil, nil, err
	}
	var obs *infra.Observability
	if mode == infra.ModeRun {
		if obs, err = infra.ParseObservability(obsRaw); err != nil {
			return "", "", nil, nil, err
		}
	}
	return rtRaw, obsRaw, rt, obs, nil
}

// Plan 은 loadtest.yml 의 plan 작업이다. 시험 ID 와 계획한 시각을 정하고, 계획 표를 실행 요약에 쓴다.
// 돌려주는 값은 GITHUB_OUTPUT 에 쓸 test_id, planned_at 이다.
func Plan(e Env, cfg *config.Config) (testID, plannedAt string, err error) {
	now := e.Clock.Now().Truncate(time.Second)
	in, err := Inputs(e.Getenv)
	if err != nil {
		return "", "", err
	}
	if in.TestID == "" {
		if in.TestID, err = TestID(now, e.Getenv("GITHUB_RUN_NUMBER")); err != nil {
			return "", "", err
		}
	}
	in.Now = now
	p, err := plan.New(cfg, in)
	if err != nil {
		return "", "", err
	}
	for _, w := range p.Warnings {
		e.printf("::warning::%s", w)
	}
	if err := AppendFile(e.Getenv("GITHUB_STEP_SUMMARY"), p.Markdown()); err != nil {
		return "", "", err
	}
	return in.TestID, now.UTC().Format(time.RFC3339), nil
}

// Run 은 loadtest.yml 의 run 작업이다. 계획을 다시 계산하고, SSM 값을 읽어 가리고, 시험 상품을 점검하고,
// 컨트롤러를 띄워 기다린 뒤 결과를 실행 요약과 Slack 에 남긴다. 결과가 성공이 아니면 오류를 돌려준다.
func Run(ctx context.Context, e Env, cfg *config.Config) error {
	if err := Guard(e.Getenv); err != nil {
		return err
	}
	in, err := Inputs(e.Getenv)
	if err != nil {
		return err
	}
	if in.TestID == "" || in.Now.IsZero() {
		return errors.New("TEST_ID 와 PLANNED_AT(plan 작업의 출력)이 필요하다")
	}
	p, err := plan.New(cfg, in)
	if err != nil {
		return fmt.Errorf("계획: %w", err)
	}
	rtRaw, obsRaw, rt, _, err := e.readInfra(ctx, infra.ModeRun)
	if err != nil {
		return err
	}
	baseURL := e.Getenv("BASE_URL")
	entryProduct := ""
	for _, f := range p.Flows {
		if f == "entry" {
			entryProduct = p.ProductID
		}
	}
	if err := Precheck(ctx, e.HTTP, baseURL, p.TestID, entryProduct, p.PrepProductID); err != nil {
		return fmt.Errorf("사전 점검: %w", err)
	}

	planJSON, _ := json.Marshal(p)
	inputJSON, _ := json.Marshal(in)
	env := []runner.Env{
		{Name: "MODE", Value: string(infra.ModeRun)},
		{Name: "TEST_ID", Value: p.TestID},
		{Name: "RUN_TASK", Value: rtRaw},
		{Name: "OBSERVABILITY", Value: obsRaw},
		{Name: "INPUT_JSON", Value: string(inputJSON)},
		{Name: "PLAN_JSON", Value: string(planJSON)},
		{Name: "BASE_URL", Value: baseURL},
	}
	code, err := e.controller(ctx, rt, p.TestID, env)
	if err != nil {
		return err
	}
	return e.report(ctx, rt, p.TestID, code)
}

// controller 는 컨트롤러를 띄우고 끝날 때까지 기다린 뒤 종료 코드를 돌려준다.
// 로컬 Forgejo 에서는 컨테이너를 띄울 수 없으므로 실행 파일을 바로 돌린다(CONTROLLER_INLINE=1).
func (e Env) controller(ctx context.Context, rt *infra.RunTask, startedBy string, env []runner.Env) (int, error) {
	t := rt.Tasks["controller"]
	if n := runner.OverrideSize(t.Container, env); n > runner.MaxOverrideBytes {
		return 0, fmt.Errorf("컨트롤러 덮어쓰기가 %d바이트로 RunTask 상한 %d바이트를 넘는다(계획을 줄인다)", n, runner.MaxOverrideBytes)
	}
	if e.Getenv("CONTROLLER_INLINE") == "1" {
		cmd := exec.CommandContext(ctx, e.Getenv("CONTROLLER_BIN"))
		cmd.Env = os.Environ()
		for _, kv := range env {
			cmd.Env = append(cmd.Env, kv.Name+"="+kv.Value)
		}
		cmd.Stdout, cmd.Stderr = e.Out, e.Out
		err := cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), nil
		}
		return 0, err
	}

	r := &runner.Runner{ECS: e.ECS, Cluster: rt.Cluster, Clock: e.Clock}
	arn, err := r.Run(ctx, t, startedBy, env)
	if err != nil {
		return 0, err
	}
	e.printf("컨트롤러를 띄웠다: %s", arn)
	logs := logTail{logs: e.Logs, group: "/ecs/" + rt.Cluster, stream: t.Container + "/" + t.Container + "/" + taskID(arn)}
	for {
		logs.flush(ctx, e)
		states, err := r.Describe(ctx, []string{arn}, t.Container)
		if err == nil && states[0].LastStatus == "STOPPED" {
			logs.flush(ctx, e)
			if c := states[0].ExitCode; c != nil {
				return int(*c), nil
			}
			return 0, fmt.Errorf("컨트롤러가 종료 코드 없이 멈췄다(%s)", states[0].StoppedReason)
		}
		if err := e.Clock.Sleep(ctx, 20*time.Second); err != nil {
			return 0, err
		}
	}
}

func taskID(arn string) string { return arn[strings.LastIndex(arn, "/")+1:] }

// logTail 은 컨트롤러 로그를 실행 로그로 옮긴다(실행 역할은 controller/* 스트림만 읽는다)
type logTail struct {
	logs          Logs
	group, stream string
	next          *string
}

func (l *logTail) flush(ctx context.Context, e Env) {
	if l.logs == nil {
		return
	}
	for {
		out, err := l.logs.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
			LogGroupName: aws.String(l.group), LogStreamName: aws.String(l.stream), StartFromHead: aws.Bool(true), NextToken: l.next,
		})
		if err != nil {
			return // 스트림이 아직 없을 수 있다
		}
		for _, ev := range out.Events {
			e.printf("[controller] %s", aws.ToString(ev.Message))
		}
		same := l.next != nil && aws.ToString(l.next) == aws.ToString(out.NextForwardToken)
		l.next = out.NextForwardToken
		if same || len(out.Events) == 0 {
			return
		}
	}
}

// report 는 summary.json 을 읽어 실행 요약과 Slack 에 남긴다. 결과가 성공이 아니면 오류를 돌려준다.
func (e Env) report(ctx context.Context, rt *infra.RunTask, testID string, code int) error {
	b, err := e.getObject(ctx, rt.Bucket, "results/"+testID+"/summary.json")
	var s report.Summary
	if err != nil || json.Unmarshal(b, &s) != nil {
		// 컨트롤러가 결과를 남기지 못하고 끝났다(태스크 실패, 강제 종료)
		msg := fmt.Sprintf("### 결과: 판정 불가\n\n> 컨트롤러가 결과(summary.json) 없이 끝났다(종료 코드 %d)\n", code)
		_ = AppendFile(e.Getenv("GITHUB_STEP_SUMMARY"), msg)
		e.slack(ctx, testID, "판정 불가: 컨트롤러가 결과 없이 끝났다")
		return errors.New("컨트롤러가 결과 없이 끝났다(판정 불가)")
	}
	if err := AppendFile(e.Getenv("GITHUB_STEP_SUMMARY"), s.Markdown("")); err != nil {
		return err
	}
	e.slack(ctx, testID, fmt.Sprintf("%s: %s %s", s.Scenario, s.Result, s.Reason))
	if !s.Result.Success() {
		return fmt.Errorf("결과 %s: %s", s.Result, s.Reason)
	}
	e.printf("결과 %s", s.Result)
	return nil
}

func (e Env) getObject(ctx context.Context, bucket, key string) ([]byte, error) {
	out, err := e.S3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

// slack 은 결과를 Slack 웹훅으로 보낸다. 웹훅이 없거나 실패해도 시험 결과는 바꾸지 않는다.
func (e Env) slack(ctx context.Context, testID, text string) {
	hook := e.Getenv("SLACK_WEBHOOK_URL")
	if hook == "" {
		return
	}
	run := e.Getenv("GITHUB_SERVER_URL") + "/" + e.Getenv("GITHUB_REPOSITORY") + "/actions/runs/" + e.Getenv("GITHUB_RUN_ID")
	body, _ := json.Marshal(map[string]string{"text": fmt.Sprintf("부하 시험 %s %s\n%s", testID, text, run)})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, hook, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.HTTP.Do(req)
	if err != nil {
		e.printf("::warning::Slack 보내기 실패: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		e.printf("::warning::Slack 보내기 실패: HTTP %d", resp.StatusCode)
	}
}

// After 는 뒷정리다(실패하거나 취소돼도 돈다). 시험 ID 의 태스크를 모두 멈추고 토큰을 지운다.
func After(ctx context.Context, e Env, testID string) error {
	if testID == "" {
		return nil // plan 작업이 시험 ID 를 정하기 전에 끝났다
	}
	_, _, rt, _, err := e.readInfra(ctx, infra.ModeRun)
	if err != nil {
		return err
	}
	r := &runner.Runner{ECS: e.ECS, Cluster: rt.Cluster, Clock: e.Clock}
	arns, err := r.ListStartedBy(ctx, testID)
	if err != nil {
		return err
	}
	var errs []error
	if len(arns) > 0 {
		e.printf("남은 태스크 %d대를 멈춘다", len(arns))
		errs = append(errs, r.Stop(ctx, arns, "뒷정리"))
	}
	errs = append(errs, deletePrefix(ctx, e.S3, rt.Bucket, "tokens/"+testID+"/"))
	return errors.Join(errs...)
}

func deletePrefix(ctx context.Context, c S3, bucket, prefix string) error {
	var token *string
	for {
		out, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix), ContinuationToken: token})
		if err != nil {
			return err
		}
		for _, o := range out.Contents {
			if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: o.Key}); err != nil {
				return err
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			return nil
		}
		token = out.NextContinuationToken
	}
}

// Stop 은 stop.yml 이다. 시험 ID 의 k6 태스크만 멈춘다. 컨트롤러는 남아 "중단" 결과를 쓴다.
// 빌드 역할에는 SSM 권한이 없어 클러스터 이름(공개된 이름)을 받는다.
func Stop(ctx context.Context, e Env, cluster, testID, k6Family string) error {
	if err := CheckTestID(testID); err != nil {
		return err
	}
	r := &runner.Runner{ECS: e.ECS, Cluster: cluster, Clock: e.Clock}
	arns, err := r.ListStartedBy(ctx, testID)
	if err != nil {
		return err
	}
	states, err := r.Describe(ctx, arns, "")
	if err != nil {
		return err
	}
	var k6 []string
	for _, s := range states {
		if strings.Contains(s.TaskDef, k6Family) {
			k6 = append(k6, s.ARN)
		}
	}
	if len(k6) == 0 {
		return fmt.Errorf("시험 %s 의 k6 태스크가 없다", testID)
	}
	e.printf("k6 태스크 %d대를 멈춘다", len(k6))
	return r.Stop(ctx, k6, "stop.yml")
}

// Cleanup 은 cleanup.yml 이다. 컨트롤러를 정리 모드로 띄우고 기다린다.
func Cleanup(ctx context.Context, e Env, testID string) error {
	if err := CheckTestID(testID); err != nil {
		return err
	}
	if err := Guard(e.Getenv); err != nil {
		return err
	}
	rtRaw, _, rt, _, err := e.readInfra(ctx, infra.ModeCleanup)
	if err != nil {
		return err
	}
	env := []runner.Env{
		{Name: "MODE", Value: string(infra.ModeCleanup)},
		{Name: "TEST_ID", Value: testID},
		{Name: "RUN_TASK", Value: rtRaw},
	}
	code, err := e.controller(ctx, rt, testID+"-c", env)
	if err != nil {
		return err
	}
	if b, err := e.getObject(ctx, rt.Bucket, "results/"+testID+"/cleanup.json"); err == nil {
		_ = AppendFile(e.Getenv("GITHUB_STEP_SUMMARY"), "### 정리 결과\n\n```json\n"+string(b)+"\n```\n")
	}
	switch code {
	case 0:
		return nil
	case 2:
		return errors.New("정리를 거절했다(컨트롤러 로그에 까닭이 있다)")
	default:
		return fmt.Errorf("정리 실패(컨트롤러 종료 코드 %d)", code)
	}
}

// Idle 은 build.yml 이 latest 를 옮기기 전에 시험 클러스터에 도는 태스크가 없는지 본다.
// 남는 틈(이 확인 뒤에 시험이 시작되는 경우)은 컨트롤러가 k6 이미지 다이제스트를 비교해 막는다.
func Idle(ctx context.Context, e Env, cluster string) error {
	r := &runner.Runner{ECS: e.ECS, Cluster: cluster, Clock: e.Clock}
	arns, err := r.ListRunning(ctx)
	if err != nil {
		return err
	}
	if len(arns) > 0 {
		return fmt.Errorf("시험 클러스터에 도는 태스크가 %d대 있어 latest 를 옮기지 않는다(커밋 태그는 올렸다). 시험이 끝난 뒤 다시 돌린다", len(arns))
	}
	return nil
}
