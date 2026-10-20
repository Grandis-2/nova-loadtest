// Package runner는 시험 클러스터의 태스크를 띄우고, 보고, 멈춘다(ECS RunTask, DescribeTasks, ListTasks, StopTask).
//
// - RunTask에는 tags를 넘기지 않고 startedBy에 시험 ID를 넣는다. 찾을 때는 ListTasks(startedBy)를 쓴다
// - 태스크마다 환경 변수가 달라(TASK_INDEX, SEGMENT) 한 번에 한 대씩 띄운다
// - 용량 부족으로 띄우지 못하면 잠시 뒤 다시 시도한다
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/Grandis-2/nova-loadtest/controller/internal/clock"
	"github.com/Grandis-2/nova-loadtest/controller/internal/infra"
)

// ECS는 runner가 쓰는 ECS API다. *ecs.Client가 그대로 맞는다.
type ECS interface {
	RunTask(context.Context, *ecs.RunTaskInput, ...func(*ecs.Options)) (*ecs.RunTaskOutput, error)
	DescribeTasks(context.Context, *ecs.DescribeTasksInput, ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
	ListTasks(context.Context, *ecs.ListTasksInput, ...func(*ecs.Options)) (*ecs.ListTasksOutput, error)
	StopTask(context.Context, *ecs.StopTaskInput, ...func(*ecs.Options)) (*ecs.StopTaskOutput, error)
}

// MaxOverrideBytes는 RunTask 덮어쓰기(환경 변수 이름과 값)의 상한이다
const MaxOverrideBytes = 8192

// 용량 부족으로 띄우지 못했을 때 다시 시도하는 횟수와 간격
const (
	capacityRetries = 10
	capacityBackoff = 10 * time.Second
)

type Runner struct {
	ECS     ECS
	Cluster string
	Clock   clock.Clock
}

// Env는 컨테이너 환경 변수 하나다.
type Env struct{ Name, Value string }

// OverrideSize는 덮어쓰기를 JSON 으로 바꾼 크기다. ECS 는 구조와 따옴표 이스케이프까지 포함한 JSON 길이로
// 8KiB 를 센다. 이름과 값의 길이만 더하면 PLAN_JSON 처럼 따옴표가 많은 값에서 실제보다 작게 나온다.
func OverrideSize(container string, env []Env) int {
	type kv struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	list := make([]kv, len(env))
	for i, e := range env {
		list[i] = kv{e.Name, e.Value}
	}
	b, _ := json.Marshal(map[string]any{"containerOverrides": []any{map[string]any{"name": container, "environment": list}}})
	return len(b)
}

// Run은 태스크 한 대를 띄우고 ARN을 돌려준다.
func (r *Runner) Run(ctx context.Context, t infra.Task, startedBy string, env []Env) (string, error) {
	if n := OverrideSize(t.Container, env); n > MaxOverrideBytes {
		return "", fmt.Errorf("%s 덮어쓰기가 %d바이트로 RunTask 상한 %d바이트를 넘는다", t.Container, n, MaxOverrideBytes)
	}
	kv := make([]types.KeyValuePair, len(env))
	for i, e := range env {
		kv[i] = types.KeyValuePair{Name: aws.String(e.Name), Value: aws.String(e.Value)}
	}
	in := &ecs.RunTaskInput{
		Cluster:        aws.String(r.Cluster),
		TaskDefinition: aws.String(t.TaskDefinition),
		LaunchType:     types.LaunchTypeFargate,
		Count:          aws.Int32(1),
		StartedBy:      aws.String(startedBy),
		NetworkConfiguration: &types.NetworkConfiguration{AwsvpcConfiguration: &types.AwsVpcConfiguration{
			Subnets:        t.Subnets,
			SecurityGroups: t.SecurityGroups,
			AssignPublicIp: types.AssignPublicIp(t.AssignPublicIP),
		}},
		Overrides: &types.TaskOverride{ContainerOverrides: []types.ContainerOverride{{
			Name:        aws.String(t.Container),
			Environment: kv,
		}}},
	}
	for attempt := 0; ; attempt++ {
		out, err := r.ECS.RunTask(ctx, in)
		if err == nil && len(out.Tasks) == 1 {
			return aws.ToString(out.Tasks[0].TaskArn), nil
		}
		reason := ""
		if err != nil {
			reason = err.Error()
		} else if len(out.Failures) > 0 {
			reason = aws.ToString(out.Failures[0].Reason) + " " + aws.ToString(out.Failures[0].Detail)
		}
		if !capacityShort(reason) || attempt >= capacityRetries {
			return "", fmt.Errorf("%s를 띄우지 못했다: %s", t.Container, strings.TrimSpace(reason))
		}
		if err := r.Clock.Sleep(ctx, capacityBackoff); err != nil {
			return "", err
		}
	}
}

func capacityShort(reason string) bool {
	r := strings.ToLower(reason)
	// Fargate: "Capacity is unavailable", EC2 계열: "RESOURCE:CPU", "RESOURCE:MEMORY", "RESOURCE:ENI"
	return strings.Contains(r, "capacity") || strings.Contains(r, "resource:") ||
		strings.Contains(r, "throttl") || strings.Contains(r, "rate exceeded")
}

// Missing은 DescribeTasks 가 아직 태스크를 돌려주지 않은 상태다.
const Missing = "MISSING"

// State는 태스크 하나의 상태다.
type State struct {
	ARN           string
	LastStatus    string // PROVISIONING, PENDING, RUNNING, STOPPED 등
	ExitCode      *int32 // 컨테이너가 끝났을 때만 있다
	ImageDigest   string
	StartedAt     *time.Time
	StoppedAt     *time.Time // 멈췄을 때만 있다
	StoppedReason string
	PrivateIP     string
	TaskDef       string
	StartedBy     string
}

// Describe는 태스크 상태를 돌려준다. 요청한 순서를 지킨다. 없는 태스크는 STOPPED로 본다.
func (r *Runner) Describe(ctx context.Context, arns []string, container string) ([]State, error) {
	byARN := map[string]State{}
	for chunk := range slices.Chunk(arns, 100) {
		out, err := r.ECS.DescribeTasks(ctx, &ecs.DescribeTasksInput{Cluster: aws.String(r.Cluster), Tasks: chunk})
		if err != nil {
			return nil, err
		}
		for _, t := range out.Tasks {
			s := State{
				ARN: aws.ToString(t.TaskArn), LastStatus: aws.ToString(t.LastStatus), StartedAt: t.StartedAt, StoppedAt: t.StoppedAt,
				StoppedReason: aws.ToString(t.StoppedReason), TaskDef: aws.ToString(t.TaskDefinitionArn),
				PrivateIP: privateIP(t), StartedBy: aws.ToString(t.StartedBy),
			}
			for _, c := range t.Containers {
				if container == "" || aws.ToString(c.Name) == container {
					s.ExitCode, s.ImageDigest = c.ExitCode, aws.ToString(c.ImageDigest)
				}
			}
			byARN[s.ARN] = s
		}
	}
	states := make([]State, len(arns))
	for i, a := range arns {
		s, ok := byARN[a]
		if !ok {
			// ECS 조회는 늦게 반영될 수 있어, 방금 띄운 태스크가 잠시 보이지 않는다. 멈춘 것으로 보지 않는다
			s = State{ARN: a, LastStatus: Missing}
		}
		states[i] = s
	}
	return states, nil
}

func privateIP(t types.Task) string {
	for _, a := range t.Attachments {
		for _, d := range a.Details {
			if aws.ToString(d.Name) == "privateIPv4Address" {
				return aws.ToString(d.Value)
			}
		}
	}
	return ""
}

// Stop은 태스크를 멈춘다. 이미 멈춘 태스크의 오류는 무시하고, 나머지 오류는 모아 돌려준다.
func (r *Runner) Stop(ctx context.Context, arns []string, reason string) error {
	var errs []error
	for _, a := range arns {
		_, err := r.ECS.StopTask(ctx, &ecs.StopTaskInput{Cluster: aws.String(r.Cluster), Task: aws.String(a), Reason: aws.String(reason)})
		if err != nil && !strings.Contains(strings.ToLower(err.Error()), "stopped") {
			errs = append(errs, fmt.Errorf("%s: %w", a, err))
		}
	}
	return errors.Join(errs...)
}

// ListStartedBy는 startedBy가 같은 태스크를 모두 돌려준다(desiredStatus RUNNING).
func (r *Runner) ListStartedBy(ctx context.Context, startedBy string) ([]string, error) {
	return r.list(ctx, &ecs.ListTasksInput{Cluster: aws.String(r.Cluster), StartedBy: aws.String(startedBy)})
}

// ListRunning은 클러스터에서 아직 멈추지 않은 태스크를 모두 돌려준다.
// StopTask 를 받은 태스크는 desiredStatus 가 STOPPED 라 기본 ListTasks 에 나오지 않지만, 멈출 때까지는
// 아직 돌고 있다(요청을 보내고, 요약을 올린다). 그래서 desiredStatus STOPPED 목록도 보고, 아직 STOPPED 가
// 아닌 것(조회에 아직 안 나온 것 포함)을 더한다.
func (r *Runner) ListRunning(ctx context.Context) ([]string, error) {
	arns, err := r.list(ctx, &ecs.ListTasksInput{Cluster: aws.String(r.Cluster)})
	if err != nil {
		return nil, err
	}
	stopping, err := r.list(ctx, &ecs.ListTasksInput{Cluster: aws.String(r.Cluster), DesiredStatus: types.DesiredStatusStopped})
	if err != nil || len(stopping) == 0 {
		return arns, err
	}
	states, err := r.Describe(ctx, stopping, "")
	if err != nil {
		return nil, err
	}
	for _, s := range states {
		if s.LastStatus != "STOPPED" {
			arns = append(arns, s.ARN)
		}
	}
	return arns, nil
}

func (r *Runner) list(ctx context.Context, in *ecs.ListTasksInput) ([]string, error) {
	var arns []string
	for {
		out, err := r.ECS.ListTasks(ctx, in)
		if err != nil {
			return nil, err
		}
		arns = append(arns, out.TaskArns...)
		if out.NextToken == nil {
			return arns, nil
		}
		in.NextToken = out.NextToken
	}
}

// FindMimir는 관측 서비스의 RUNNING 태스크 중 가장 늦게 시작한 것의 사설 IP를 돌려준다.
// 재배포 중에는 태스크가 잠시 둘일 수 있다.
func FindMimir(ctx context.Context, api ECS, m infra.Mimir) (string, error) {
	r := &Runner{ECS: api, Cluster: m.Cluster}
	arns, err := r.list(ctx, &ecs.ListTasksInput{
		Cluster: aws.String(m.Cluster), ServiceName: aws.String(m.Service), DesiredStatus: types.DesiredStatusRunning,
	})
	if err != nil {
		return "", fmt.Errorf("Mimir 태스크 찾기: %w", err)
	}
	if len(arns) == 0 {
		return "", fmt.Errorf("%s/%s에 도는 태스크가 없다", m.Cluster, m.Service)
	}
	states, err := r.Describe(ctx, arns, "")
	if err != nil {
		return "", fmt.Errorf("Mimir 태스크 보기: %w", err)
	}
	var best *State
	for i := range states {
		s := &states[i]
		if s.LastStatus != "RUNNING" || s.PrivateIP == "" || s.StartedAt == nil {
			continue
		}
		if best == nil || s.StartedAt.After(*best.StartedAt) {
			best = s
		}
	}
	if best == nil {
		return "", fmt.Errorf("%s/%s에 IP가 있는 RUNNING 태스크가 없다", m.Cluster, m.Service)
	}
	return best.PrivateIP, nil
}
