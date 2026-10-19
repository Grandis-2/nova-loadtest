// Package fakelocal 은 로컬 Forgejo 3단계 각본(구현 계획 7.2)에 쓰는 가짜 ECS 와 가짜 Mimir 다.
// 두 서버는 한 프로세스에서 같은 각본과 같은 시험 계획을 본다. 시각을 맞추려는 것이다.
package fakelocal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// ECS 프로토콜(awsJson1.1)에서 쓰는 필드만 옮긴다. SDK 의 입력 구조체에는 JSON 태그가 없다.
type kv struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type runTaskReq struct {
	Cluster              string `json:"cluster"`
	TaskDefinition       string `json:"taskDefinition"`
	StartedBy            string `json:"startedBy"`
	Count                int32  `json:"count"`
	NetworkConfiguration *struct {
		AwsvpcConfiguration struct {
			Subnets        []string `json:"subnets"`
			SecurityGroups []string `json:"securityGroups"`
			AssignPublicIp string   `json:"assignPublicIp"`
		} `json:"awsvpcConfiguration"`
	} `json:"networkConfiguration"`
	Overrides *struct {
		ContainerOverrides []struct {
			Name        string `json:"name"`
			Environment []kv   `json:"environment"`
		} `json:"containerOverrides"`
	} `json:"overrides"`
}

type taskOut struct {
	TaskArn           string       `json:"taskArn"`
	LastStatus        string       `json:"lastStatus"`
	StartedBy         string       `json:"startedBy,omitempty"`
	TaskDefinitionArn string       `json:"taskDefinitionArn,omitempty"`
	StartedAt         *float64     `json:"startedAt,omitempty"`
	StoppedReason     string       `json:"stoppedReason,omitempty"`
	Containers        []contOut    `json:"containers,omitempty"`
	Attachments       []attachment `json:"attachments,omitempty"`
}

type contOut struct {
	Name        string `json:"name"`
	ExitCode    *int32 `json:"exitCode,omitempty"`
	ImageDigest string `json:"imageDigest,omitempty"`
}

type attachment struct {
	Details []kv `json:"details"`
}

type failure struct {
	Arn    string `json:"arn"`
	Reason string `json:"reason"`
}

const targetPrefix = "AmazonEC2ContainerServiceV20141113."

// serveECS 는 SDK 가 AWS_ENDPOINT_URL_ECS 로 보내는 요청에 답한다.
func (s *Server) serveECS(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), targetPrefix)
	body, _ := io.ReadAll(r.Body)
	ctx := r.Context()
	f := s.ecs()
	var out any
	var err error
	switch op {
	case "RunTask":
		out, err = runTask(ctx, f, body)
	case "DescribeTasks":
		out, err = describeTasks(ctx, f, body)
	case "ListTasks":
		out, err = listTasks(ctx, f, body)
	case "StopTask":
		out, err = stopTask(ctx, f, body)
	default:
		err = fmt.Errorf("fake-ecs 는 %q 를 모른다", op)
	}
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	if err != nil {
		w.Header().Set("X-Amzn-ErrorType", "ClientException")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"__type": "ClientException", "message": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(out)
}

func runTask(ctx context.Context, f ecsAPI, body []byte) (any, error) {
	var in runTaskReq
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	sdk := &ecs.RunTaskInput{Cluster: aws.String(in.Cluster), TaskDefinition: aws.String(in.TaskDefinition), StartedBy: aws.String(in.StartedBy), Count: aws.Int32(in.Count)}
	if n := in.NetworkConfiguration; n != nil {
		sdk.NetworkConfiguration = &types.NetworkConfiguration{AwsvpcConfiguration: &types.AwsVpcConfiguration{
			Subnets: n.AwsvpcConfiguration.Subnets, SecurityGroups: n.AwsvpcConfiguration.SecurityGroups,
			AssignPublicIp: types.AssignPublicIp(n.AwsvpcConfiguration.AssignPublicIp),
		}}
	}
	if o := in.Overrides; o != nil {
		sdk.Overrides = &types.TaskOverride{}
		for _, c := range o.ContainerOverrides {
			co := types.ContainerOverride{Name: aws.String(c.Name)}
			for _, e := range c.Environment {
				co.Environment = append(co.Environment, types.KeyValuePair{Name: aws.String(e.Name), Value: aws.String(e.Value)})
			}
			sdk.Overrides.ContainerOverrides = append(sdk.Overrides.ContainerOverrides, co)
		}
	}
	res, err := f.RunTask(ctx, sdk)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"tasks": tasksOut(res.Tasks), "failures": failuresOut(res.Failures)}
	return out, nil
}

func describeTasks(ctx context.Context, f ecsAPI, body []byte) (any, error) {
	var in struct {
		Cluster string   `json:"cluster"`
		Tasks   []string `json:"tasks"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	res, err := f.DescribeTasks(ctx, &ecs.DescribeTasksInput{Cluster: aws.String(in.Cluster), Tasks: in.Tasks})
	if err != nil {
		return nil, err
	}
	return map[string]any{"tasks": tasksOut(res.Tasks), "failures": failuresOut(res.Failures)}, nil
}

func listTasks(ctx context.Context, f ecsAPI, body []byte) (any, error) {
	var in struct {
		Cluster       string `json:"cluster"`
		StartedBy     string `json:"startedBy"`
		ServiceName   string `json:"serviceName"`
		DesiredStatus string `json:"desiredStatus"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	sdk := &ecs.ListTasksInput{Cluster: aws.String(in.Cluster), DesiredStatus: types.DesiredStatus(in.DesiredStatus)}
	if in.StartedBy != "" {
		sdk.StartedBy = aws.String(in.StartedBy)
	}
	if in.ServiceName != "" {
		sdk.ServiceName = aws.String(in.ServiceName)
	}
	res, err := f.ListTasks(ctx, sdk)
	if err != nil {
		return nil, err
	}
	arns := res.TaskArns
	if arns == nil {
		arns = []string{}
	}
	return map[string]any{"taskArns": arns}, nil
}

func stopTask(ctx context.Context, f ecsAPI, body []byte) (any, error) {
	var in struct {
		Cluster string `json:"cluster"`
		Task    string `json:"task"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	if _, err := f.StopTask(ctx, &ecs.StopTaskInput{Cluster: aws.String(in.Cluster), Task: aws.String(in.Task), Reason: aws.String(in.Reason)}); err != nil {
		return nil, err
	}
	return map[string]any{"task": map[string]string{"taskArn": in.Task}}, nil
}

func tasksOut(ts []types.Task) []taskOut {
	out := []taskOut{}
	for _, t := range ts {
		o := taskOut{
			TaskArn: aws.ToString(t.TaskArn), LastStatus: aws.ToString(t.LastStatus), StartedBy: aws.ToString(t.StartedBy),
			TaskDefinitionArn: aws.ToString(t.TaskDefinitionArn), StoppedReason: aws.ToString(t.StoppedReason),
		}
		if t.StartedAt != nil {
			v := float64(t.StartedAt.UnixMilli()) / 1000
			o.StartedAt = &v
		}
		for _, c := range t.Containers {
			o.Containers = append(o.Containers, contOut{Name: aws.ToString(c.Name), ExitCode: c.ExitCode, ImageDigest: aws.ToString(c.ImageDigest)})
		}
		for _, a := range t.Attachments {
			var at attachment
			for _, d := range a.Details {
				at.Details = append(at.Details, kv{Name: aws.ToString(d.Name), Value: aws.ToString(d.Value)})
			}
			o.Attachments = append(o.Attachments, at)
		}
		out = append(out, o)
	}
	return out
}

func failuresOut(fs []types.Failure) []failure {
	out := []failure{}
	for _, f := range fs {
		out = append(out, failure{Arn: aws.ToString(f.Arn), Reason: aws.ToString(f.Reason)})
	}
	return out
}

// ecsAPI 는 fakeecs.Fake 의 네 메서드다
type ecsAPI interface {
	RunTask(context.Context, *ecs.RunTaskInput, ...func(*ecs.Options)) (*ecs.RunTaskOutput, error)
	DescribeTasks(context.Context, *ecs.DescribeTasksInput, ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
	ListTasks(context.Context, *ecs.ListTasksInput, ...func(*ecs.Options)) (*ecs.ListTasksOutput, error)
	StopTask(context.Context, *ecs.StopTaskInput, ...func(*ecs.Options)) (*ecs.StopTaskOutput, error)
}
