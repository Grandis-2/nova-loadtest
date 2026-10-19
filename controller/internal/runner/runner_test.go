package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/Grandis-2/nova-loadtest/controller/internal/clock"
	"github.com/Grandis-2/nova-loadtest/controller/internal/infra"
)

// failingECS 는 처음 몇 번 RunTask 를 실패시킨다
type failingECS struct {
	ECS
	reasons []string
	calls   int
}

func (f *failingECS) RunTask(context.Context, *ecs.RunTaskInput, ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	f.calls++
	if f.calls <= len(f.reasons) {
		return &ecs.RunTaskOutput{Failures: []types.Failure{{Reason: aws.String(f.reasons[f.calls-1])}}}, nil
	}
	return &ecs.RunTaskOutput{Tasks: []types.Task{{TaskArn: aws.String("arn:task")}}}, nil
}

var task = infra.Task{TaskDefinition: "td", Container: "k6", Subnets: []string{"s"}, SecurityGroups: []string{"g"}, AssignPublicIP: "ENABLED"}

func TestRunRetriesCapacity(t *testing.T) {
	for _, reason := range []string{"Capacity is unavailable at this time", "RESOURCE:MEMORY", "RESOURCE:CPU"} {
		f := &failingECS{reasons: []string{reason, reason}}
		r := &Runner{ECS: f, Cluster: "c", Clock: clock.NewFake(time.Now())}
		if arn, err := r.Run(context.Background(), task, "lt-x", nil); err != nil || arn != "arn:task" || f.calls != 3 {
			t.Fatalf("%s: 용량 부족은 다시 시도해야 한다: %v %d", reason, err, f.calls)
		}
	}
	f := &failingECS{reasons: []string{"INVALID_TASK_DEFINITION"}}
	r := &Runner{ECS: f, Cluster: "c", Clock: clock.NewFake(time.Now())}
	if _, err := r.Run(context.Background(), task, "lt-x", nil); err == nil || f.calls != 1 {
		t.Fatalf("다른 실패는 다시 시도하지 않는다: %v %d", err, f.calls)
	}
}

// 8KiB 는 덮어쓰기 JSON 전체(구조, 컨테이너 이름, 따옴표 이스케이프)로 센다
func TestOverrideSizeCountsJSON(t *testing.T) {
	plan := strings.Repeat(`{"a":"b"},`, 500) // 따옴표가 많은 값
	env := []Env{{Name: "PLAN_JSON", Value: plan}}
	naive := len("PLAN_JSON") + len(plan)
	if got := OverrideSize("k6", env); got <= naive+len(`"containerOverrides"`) {
		t.Fatalf("JSON 크기 %d가 이름+값 %d보다 충분히 커야 한다(이스케이프)", got, naive)
	}
	big := []Env{{Name: "PLAN_JSON", Value: strings.Repeat(`"`, 4500)}} // 이스케이프하면 9000바이트가 넘는다
	if _, err := (&Runner{ECS: &failingECS{}, Cluster: "c", Clock: clock.NewFake(time.Now())}).Run(context.Background(), task, "lt-x", big); err == nil || !strings.Contains(err.Error(), "8192") {
		t.Fatalf("이스케이프까지 세면 상한을 넘는다: %v", err)
	}
}
