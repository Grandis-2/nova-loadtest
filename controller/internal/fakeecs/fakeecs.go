// Package fakeecs는 시험용 ECS다. 태스크가 언제 RUNNING이 되고, 언제 몇 번 종료 코드로 끝나는지를 각본으로 정한다.
// 상태는 시계(clock.Clock)를 기준으로 계산하므로 가짜 시계와 함께 쓰면 몇 시간짜리 시험도 바로 끝난다.
// 컨트롤러 단위 테스트와 로컬 Forgejo 의 fake-ecs 서버가 같이 쓴다.
package fakeecs

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/Grandis-2/nova-loadtest/controller/internal/clock"
)

// Behavior는 태스크 하나의 각본이다.
type Behavior struct {
	RunningAfter time.Duration // RunTask 뒤 RUNNING 이 되기까지
	Never        bool          // RUNNING 이 되지 않는다(PROVISIONING 에 머문다)
	ExitAt       time.Time     // 스스로 끝나는 시각. 비면 멈출 때까지 돈다
	ExitCode     int32
	StopExitCode int32 // StopTask 를 받으면 이 코드로 끝난다(k6 는 105)
	ImageDigest  string
	HiddenFor    time.Duration // RunTask 뒤 이만큼 DescribeTasks 에 나오지 않는다(ECS 조회가 늦게 반영되는 경우)
	StopDelay    time.Duration // StopTask 뒤 STOPPED 가 되기까지. 그동안 desiredStatus 는 STOPPED, lastStatus 는 STOPPING 이다
}

// RunCall은 받은 RunTask 하나다. 덮어쓰기를 검사하는 데 쓴다.
type RunCall struct {
	Family    string
	StartedBy string
	Container string
	Env       map[string]string
	Network   *types.AwsVpcConfiguration
}

// ServiceTask는 서비스에 속한 태스크다(Mimir 를 찾는 데 쓴다).
type ServiceTask struct {
	IP        string
	StartedAt time.Time
	Gone      time.Time // 이 시각부터 없어진다(재배포). 비면 계속 있다
}

type task struct {
	arn, cluster, family, startedBy, container string
	created                                    time.Time
	b                                          Behavior
	stoppedAt                                  *time.Time
	stopRequested                              bool
	stopReason                                 string
}

type Fake struct {
	Clock clock.Clock
	// Behave 는 RunTask 마다 각본을 고른다. n 은 패밀리별 몇 번째 RunTask 인지(0부터)다
	Behave func(family string, env map[string]string, n int) Behavior
	// Services 는 "클러스터/서비스" 마다 서비스 태스크다
	Services map[string][]ServiceTask

	mu    sync.Mutex
	tasks map[string]*task
	seq   map[string]int
	Runs  []RunCall
	Stops []string
}

func (f *Fake) init() {
	if f.tasks == nil {
		f.tasks, f.seq = map[string]*task{}, map[string]int{}
	}
}

func (f *Fake) RunTask(_ context.Context, in *ecs.RunTaskInput, _ ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	family := aws.ToString(in.TaskDefinition)
	call := RunCall{Family: family, StartedBy: aws.ToString(in.StartedBy), Env: map[string]string{}}
	if in.NetworkConfiguration != nil {
		call.Network = in.NetworkConfiguration.AwsvpcConfiguration
	}
	if in.Overrides != nil {
		for _, c := range in.Overrides.ContainerOverrides {
			call.Container = aws.ToString(c.Name)
			for _, kv := range c.Environment {
				call.Env[aws.ToString(kv.Name)] = aws.ToString(kv.Value)
			}
		}
	}
	f.Runs = append(f.Runs, call)
	n := f.seq[family]
	f.seq[family]++
	b := Behavior{StopExitCode: 105}
	if f.Behave != nil {
		b = f.Behave(family, call.Env, n)
	}
	arn := fmt.Sprintf("arn:aws:ecs:ap-northeast-2:000000000000:task/%s/%s-%d", aws.ToString(in.Cluster), family, n)
	f.tasks[arn] = &task{arn: arn, cluster: aws.ToString(in.Cluster), family: family, startedBy: call.StartedBy,
		container: call.Container, created: f.Clock.Now(), b: b}
	return &ecs.RunTaskOutput{Tasks: []types.Task{{TaskArn: aws.String(arn), LastStatus: aws.String("PROVISIONING")}}}, nil
}

// status 는 지금 시각의 상태와 종료 코드다
func (t *task) status(now time.Time) (string, *int32) {
	if t.stoppedAt != nil && !now.Before(*t.stoppedAt) {
		c := t.b.StopExitCode
		return "STOPPED", &c
	}
	if t.stopRequested {
		return "STOPPING", nil
	}
	if t.b.Never {
		return "PROVISIONING", nil
	}
	if !t.b.ExitAt.IsZero() && !now.Before(t.b.ExitAt) {
		c := t.b.ExitCode
		return "STOPPED", &c
	}
	if now.Before(t.created.Add(t.b.RunningAfter)) {
		return "PENDING", nil
	}
	return "RUNNING", nil
}

func (f *Fake) DescribeTasks(_ context.Context, in *ecs.DescribeTasksInput, _ ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	now := f.Clock.Now()
	out := &ecs.DescribeTasksOutput{}
	for _, arn := range in.Tasks {
		if t, ok := f.tasks[arn]; ok {
			if now.Before(t.created.Add(t.b.HiddenFor)) {
				out.Failures = append(out.Failures, types.Failure{Arn: aws.String(arn), Reason: aws.String("MISSING")})
				continue
			}
			st, code := t.status(now)
			started := t.created.Add(t.b.RunningAfter)
			out.Tasks = append(out.Tasks, types.Task{
				TaskArn: aws.String(arn), LastStatus: aws.String(st), StartedBy: aws.String(t.startedBy),
				TaskDefinitionArn: aws.String(t.family), StartedAt: &started, StoppedReason: aws.String(t.stopReason),
				Containers: []types.Container{{Name: aws.String(t.container), ExitCode: code, ImageDigest: aws.String(t.b.ImageDigest)}},
			})
			continue
		}
		if st, ok := f.serviceTask(arn, now); ok {
			out.Tasks = append(out.Tasks, st)
		}
	}
	return out, nil
}

func (f *Fake) serviceTask(arn string, now time.Time) (types.Task, bool) {
	for key, list := range f.Services {
		for i, s := range list {
			if serviceARN(key, i) != arn || (!s.Gone.IsZero() && !now.Before(s.Gone)) {
				continue
			}
			started := s.StartedAt
			return types.Task{
				TaskArn: aws.String(arn), LastStatus: aws.String("RUNNING"), StartedAt: &started,
				Attachments: []types.Attachment{{Details: []types.KeyValuePair{{Name: aws.String("privateIPv4Address"), Value: aws.String(s.IP)}}}},
			}, true
		}
	}
	return types.Task{}, false
}

func serviceARN(key string, i int) string {
	return fmt.Sprintf("arn:aws:ecs:ap-northeast-2:000000000000:task/%s-%d", strings.ReplaceAll(key, "/", "-"), i)
}

func (f *Fake) ListTasks(_ context.Context, in *ecs.ListTasksInput, _ ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	now := f.Clock.Now()
	out := &ecs.ListTasksOutput{}
	if in.ServiceName != nil {
		key := aws.ToString(in.Cluster) + "/" + aws.ToString(in.ServiceName)
		for i, s := range f.Services[key] {
			if s.Gone.IsZero() || now.Before(s.Gone) {
				out.TaskArns = append(out.TaskArns, serviceARN(key, i))
			}
		}
		return out, nil
	}
	for _, t := range f.tasks {
		if t.cluster != aws.ToString(in.Cluster) {
			continue
		}
		if in.StartedBy != nil && t.startedBy != aws.ToString(in.StartedBy) {
			continue
		}
		// ECS 의 ListTasks 는 desiredStatus 로 거른다(기본 RUNNING). StopTask 를 받았거나 끝난 태스크는 STOPPED 다
		desired := "RUNNING"
		if st, _ := t.status(now); st == "STOPPED" || t.stopRequested {
			desired = "STOPPED"
		}
		want := "RUNNING"
		if in.DesiredStatus != "" {
			want = string(in.DesiredStatus)
		}
		if desired != want {
			continue
		}
		out.TaskArns = append(out.TaskArns, t.arn)
	}
	return out, nil
}

func (f *Fake) StopTask(_ context.Context, in *ecs.StopTaskInput, _ ...func(*ecs.Options)) (*ecs.StopTaskOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	arn := aws.ToString(in.Task)
	f.Stops = append(f.Stops, arn)
	t, ok := f.tasks[arn]
	if !ok {
		return nil, fmt.Errorf("task %s not found", arn)
	}
	if st, _ := t.status(f.Clock.Now()); st != "STOPPED" {
		at := f.Clock.Now().Add(t.b.StopDelay)
		t.stoppedAt, t.stopReason, t.stopRequested = &at, aws.ToString(in.Reason), true
	}
	return &ecs.StopTaskOutput{}, nil
}

// Snapshot 은 받은 RunTask, StopTask 기록의 사본이다(다른 고루틴이 기록하는 중에도 읽을 수 있다)
func (f *Fake) Snapshot() ([]RunCall, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RunCall(nil), f.Runs...), append([]string(nil), f.Stops...)
}

// RunsOf 는 패밀리 하나의 RunTask 기록이다.
func (f *Fake) RunsOf(family string) []RunCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []RunCall
	for _, r := range f.Runs {
		if r.Family == family {
			out = append(out, r)
		}
	}
	return out
}
