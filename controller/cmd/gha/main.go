// gha 는 GitHub Actions(와 로컬 Forgejo) 러너에서 도는 명령이다. 워크플로가 셸 스크립트 대신 부른다.
//
//	gha plan      loadtest.yml plan 작업: 시험 ID 와 계획 시각을 정하고 계획 표를 쓴다(GITHUB_OUTPUT 에 test_id, planned_at)
//	gha run       loadtest.yml run 작업: SSM 값 읽기와 가리기, 사전 점검, 컨트롤러 실행과 대기, 결과 정리
//	gha after     loadtest.yml 뒷정리: 시험 ID 의 태스크를 모두 멈추고 토큰을 지운다
//	gha stop      stop.yml: 시험 ID 의 k6 태스크를 멈춘다
//	gha cleanup   cleanup.yml: 컨트롤러를 정리 모드로 띄우고 기다린다
//	gha ecr-login build.yml: ECR 에 docker login 한다(비밀번호는 가린 뒤 표준 입력으로 넘긴다)
//	gha idle      build.yml: 시험 클러스터에 도는 태스크가 없는지 본다(있으면 latest 를 옮기지 않는다)
//
// 입력은 환경 변수로 받는다(INPUT_<이름>, TEST_ID, PLANNED_AT, AWS_ROLE_ARN 등). docs/configuration.md 참고.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/Grandis-2/nova-loadtest/config"
	"github.com/Grandis-2/nova-loadtest/controller/internal/clock"
	"github.com/Grandis-2/nova-loadtest/controller/internal/gha"
)

func main() {
	if len(os.Args) < 2 {
		fail(fmt.Errorf("사용법: gha plan|run|after|stop|cleanup"))
	}
	if err := run(context.Background(), os.Args[1]); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Printf("::error::%v\n", err)
	os.Exit(1)
}

func run(ctx context.Context, cmd string) error {
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	e := gha.Env{HTTP: &http.Client{Timeout: 30 * time.Second}, Clock: clock.Real{}, Getenv: os.Getenv, Out: os.Stdout}
	testID := os.Getenv("TEST_ID")

	if cmd == "plan" {
		id, at, err := gha.Plan(e, cfg)
		if err != nil {
			return err
		}
		if os.Getenv("CHECK_IMAGES") == "1" {
			if err := checkImages(ctx, os.Getenv("AWS_ROLE_ARN")); err != nil {
				return err
			}
		}
		return gha.AppendFile(os.Getenv("GITHUB_OUTPUT"), fmt.Sprintf("test_id=%s\nplanned_at=%s\n", id, at))
	}

	awsCfg, err := gha.AWSConfig(ctx, os.Getenv("AWS_ROLE_ARN"), "nova-loadtest-"+cmd)
	if err != nil {
		return err
	}
	local := os.Getenv("AWS_ENDPOINT_URL") != ""
	e.SSM = ssm.NewFromConfig(awsCfg)
	e.ECS = ecs.NewFromConfig(awsCfg)
	e.S3 = s3.NewFromConfig(awsCfg, func(o *s3.Options) { o.UsePathStyle = local })
	e.Logs = cloudwatchlogs.NewFromConfig(awsCfg)

	switch cmd {
	case "run":
		return gha.Run(ctx, e, cfg)
	case "after":
		return gha.After(ctx, e, testID)
	case "stop":
		return gha.Stop(ctx, e, os.Getenv("LOADTEST_CLUSTER"), testID, os.Getenv("K6_FAMILY"))
	case "cleanup":
		return gha.Cleanup(ctx, e, testID)
	case "ecr-login":
		return ecrLogin(ctx, ecr.NewFromConfig(awsCfg))
	case "idle":
		return gha.Idle(ctx, e, os.Getenv("LOADTEST_CLUSTER"))
	}
	return fmt.Errorf("모르는 명령 %q", cmd)
}

// checkImages 는 ECR 의 세 저장소에 latest 가 있는지 본다(빌드 역할)
func checkImages(ctx context.Context, roleARN string) error {
	awsCfg, err := gha.AWSConfig(ctx, roleARN, "nova-loadtest-plan")
	if err != nil {
		return err
	}
	c := ecr.NewFromConfig(awsCfg)
	for _, repo := range []string{"nova/loadtest-k6", "nova/loadtest-controller", "nova/loadtest-token-minter"} {
		_, err := c.DescribeImages(ctx, &ecr.DescribeImagesInput{
			RepositoryName: aws.String(repo), ImageIds: []ecrtypes.ImageIdentifier{{ImageTag: aws.String("latest")}},
		})
		if err != nil {
			return fmt.Errorf("ECR %s 에 latest 이미지가 없다(build.yml 을 먼저 돌린다): %w", repo, err)
		}
	}
	return nil
}

func ecrLogin(ctx context.Context, c *ecr.Client) error {
	out, err := c.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil || len(out.AuthorizationData) == 0 {
		return fmt.Errorf("ECR 인증 토큰: %v", err)
	}
	d := out.AuthorizationData[0]
	raw, err := base64.StdEncoding.DecodeString(aws.ToString(d.AuthorizationToken))
	if err != nil {
		return err
	}
	_, pw, _ := strings.Cut(string(raw), ":")
	fmt.Printf("::add-mask::%s\n", pw)
	registry := strings.TrimPrefix(aws.ToString(d.ProxyEndpoint), "https://")
	fmt.Printf("::add-mask::%s\n", registry) // 계정 ID 가 들어 있다
	cmd := exec.CommandContext(ctx, "docker", "login", "--username", "AWS", "--password-stdin", registry)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(pw), os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	// 가린 값은 GITHUB_OUTPUT 으로 넘기면 버려지므로, 같은 작업의 뒤 단계에 환경 변수로 넘긴다
	return gha.AppendFile(os.Getenv("GITHUB_ENV"), "REGISTRY="+registry+"\n")
}
