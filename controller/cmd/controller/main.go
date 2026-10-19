// controller는 시험 클러스터에서 도는 컨트롤러 태스크다(docs/implementation-plan.md 2.4).
//
// 워크플로가 RunTask 덮어쓰기로 넘기는 값:
//
//	MODE           run(시험) 또는 cleanup(정리)
//	TEST_ID        시험 ID
//	RUN_TASK       SSM /nova-prod/loadtest/run-task 의 JSON
//	OBSERVABILITY  SSM /nova-prod/loadtest/observability 의 JSON(run)
//	INPUT_JSON     워크플로 입력(plan.Input 의 JSON). 계획을 다시 계산한다(run)
//	PLAN_JSON      러너가 계산한 계획. 다시 계산한 계획과 다르면 멈춘다(run)
//	BASE_URL       부하를 보낼 API 주소(run)
//
// 종료 코드: 0 결과를 남겼다(시험 결과와 무관), 1 실패, 2 정리를 거절했다.
// SSM 은 읽지 않는다. 값은 워크플로가 읽어 넘기고, 로그에는 가린 값만 남는다.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/Grandis-2/nova-loadtest/config"
	"github.com/Grandis-2/nova-loadtest/controller/internal/awsio"
	"github.com/Grandis-2/nova-loadtest/controller/internal/clock"
	"github.com/Grandis-2/nova-loadtest/controller/internal/control"
	"github.com/Grandis-2/nova-loadtest/controller/internal/infra"
	"github.com/Grandis-2/nova-loadtest/controller/internal/mimir"
	"github.com/Grandis-2/nova-loadtest/controller/internal/plan"
	"github.com/Grandis-2/nova-loadtest/controller/internal/runner"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	os.Exit(run(context.Background()))
}

func run(ctx context.Context) int {
	cfg, err := config.Default()
	if err != nil {
		log.Print(err)
		return 1
	}
	mode := infra.Mode(os.Getenv("MODE"))
	testID := os.Getenv("TEST_ID")
	rt, err := infra.ParseRunTask(os.Getenv("RUN_TASK"), mode)
	if err != nil {
		log.Print(err)
		if mode == infra.ModeCleanup {
			return 2
		}
		return 1
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Print(err)
		return 1
	}
	local := os.Getenv("AWS_ENDPOINT_URL") != ""
	ecsClient := ecs.NewFromConfig(awsCfg)
	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) { o.UsePathStyle = local })
	clk := clock.Real{}
	deps := control.Deps{
		Runner: &runner.Runner{ECS: ecsClient, Cluster: rt.Cluster, Clock: clk},
		Store:  awsio.Store{Client: s3Client, Bucket: rt.Bucket},
		Clock:  clk,
		Logf:   log.Printf,
	}

	switch mode {
	case infra.ModeRun:
		obs, err := infra.ParseObservability(os.Getenv("OBSERVABILITY"))
		if err != nil {
			log.Print(err)
			return 1
		}
		var in plan.Input
		if err := json.Unmarshal([]byte(os.Getenv("INPUT_JSON")), &in); err != nil {
			log.Printf("INPUT_JSON: %v", err)
			return 1
		}
		if in.TestID != testID {
			log.Printf("INPUT_JSON 의 시험 ID(%s)가 TEST_ID(%s)와 다르다", in.TestID, testID)
			return 1
		}
		q := &mimir.HTTP{
			Port: obs.Mimir.Port, Path: obs.Mimir.QueryPath, Client: &http.Client{Timeout: 10 * time.Second},
			Discover: func(ctx context.Context) (string, error) {
				if ip := os.Getenv("MIMIR_IP"); ip != "" && local {
					return ip, nil // 로컬 시험(fake-mimir)
				}
				return runner.FindMimir(ctx, ecsClient, obs.Mimir)
			},
		}
		deps.Metrics = mimir.Metrics{Q: q, Names: cfg.Metrics, TestID: testID}
		s, err := control.Execute(ctx, deps, control.Run{
			Config: cfg, RunTask: rt, Observability: obs, Input: in, PlanJSON: os.Getenv("PLAN_JSON"), BaseURL: os.Getenv("BASE_URL"),
		})
		if err != nil {
			log.Print(err)
			return 1
		}
		log.Printf("시험 결과 %s", s.Result)
		return 0

	case infra.ModeCleanup:
		q := awsio.Queues{Client: sqs.NewFromConfig(awsCfg)}
		res, err := control.Cleanup(ctx, deps, q, cfg, rt, testID, testID+"-c")
		if errors.Is(err, control.ErrRefused) {
			log.Print(err)
			return 2
		}
		if err != nil {
			log.Print(err)
			return 1
		}
		log.Printf("정리했다: %v", res.ProductIDs)
		return 0
	}
	log.Printf("MODE %q 를 모른다(run, cleanup)", mode)
	return 1
}
