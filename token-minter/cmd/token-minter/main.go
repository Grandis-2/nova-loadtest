// token-minter는 시험 회원의 토큰을 만들어 S3에 올리고 끝나는 태스크다(docs/configuration.md).
//
// 작업 정의가 넣는 값: JWT_PRIVATE_KEY(비밀), JWT_KEY_ID, JWT_ISSUER, JWT_AUDIENCE
// 컨트롤러가 넣는 값: TEST_ID, TOKENS_URI, MEMBERS, TASKS, TOKEN_TTL_SECONDS
//
// 서명키와 토큰은 로그에 남기지 않는다. 시험 회원 범위는 실행 파일에 들어 있는 scenarios.yml로 다시 확인한다.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/Grandis-2/nova-loadtest/config"
	"github.com/Grandis-2/nova-loadtest/token-minter/internal/jwt"
	"github.com/Grandis-2/nova-loadtest/token-minter/internal/mint"
)

var testIDRe = regexp.MustCompile(`^lt-[0-9]{8}t[0-9]{4}-[0-9]+$`)

func main() {
	log.SetFlags(0)
	if err := run(context.Background()); err != nil {
		log.Fatalf("토큰 발급 실패: %v", err)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Default()
	if err != nil {
		return err
	}

	testID := os.Getenv("TEST_ID")
	if !testIDRe.MatchString(testID) {
		return fmt.Errorf("TEST_ID %q 형식이 틀렸다", testID)
	}
	bucket, prefix, err := parseURI(os.Getenv("TOKENS_URI"), testID)
	if err != nil {
		return err
	}
	var members mint.Members
	if err := json.Unmarshal([]byte(os.Getenv("MEMBERS")), &members); err != nil {
		return fmt.Errorf("MEMBERS: %w", err)
	}
	tasks, err := strconv.Atoi(os.Getenv("TASKS"))
	if err != nil {
		return fmt.Errorf("TASKS: %w", err)
	}
	ttlSec, err := strconv.Atoi(os.Getenv("TOKEN_TTL_SECONDS"))
	if err != nil {
		return fmt.Errorf("TOKEN_TTL_SECONDS: %w", err)
	}
	// 초를 시간 값으로 바꾸기 전에 범위를 본다. 아주 큰 값은 곱하는 중에 넘쳐 작은 양수가 될 수 있다
	if max := int(cfg.Members.MaxTokenTTL.D().Seconds()); ttlSec <= 0 || ttlSec > max {
		return fmt.Errorf("TOKEN_TTL_SECONDS %d는 1에서 %d 사이여야 한다", ttlSec, max)
	}
	req := mint.Request{Members: members, Tasks: tasks, TTL: time.Duration(ttlSec) * time.Second}
	if err := mint.Check(cfg, req); err != nil {
		return err
	}

	key, err := jwt.ParseKey(os.Getenv("JWT_PRIVATE_KEY"))
	// 다른 코드가 실수로 환경 변수를 출력하지 않도록 읽은 뒤 바로 지운다
	os.Unsetenv("JWT_PRIVATE_KEY")
	if err != nil {
		return err
	}
	signer, err := jwt.NewSigner(key, os.Getenv("JWT_KEY_ID"), os.Getenv("JWT_ISSUER"), os.Getenv("JWT_AUDIENCE"))
	if err != nil {
		return err
	}

	started := time.Now()
	files, err := mint.Files(signer, req, started)
	if err != nil {
		return err
	}
	log.Printf("토큰 %d개를 %s 동안 만들었다(풀 A %d, B %d, C %d, 태스크 %d)",
		members.A.Count+members.B.Count+members.C.Count, time.Since(started).Round(time.Second),
		members.A.Count, members.B.Count, members.C.Count, tasks)

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		// moto 같은 로컬 S3는 경로 방식 주소만 받는다
		o.UsePathStyle = os.Getenv("AWS_ENDPOINT_URL") != ""
	})
	for _, f := range files {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(bucket),
			Key:         aws.String(prefix + f.Name),
			Body:        bytes.NewReader(f.Body),
			ContentType: aws.String("application/json"),
		})
		if err != nil {
			return fmt.Errorf("%s 올리기: %w", f.Name, err)
		}
	}
	log.Printf("토큰 파일 %d개를 올렸다", len(files))
	return nil
}

// parseURI는 s3://<버킷>/tokens/<시험 ID>/ 만 받는다. 다른 경로에 쓰지 않게 하려는 것이다.
func parseURI(raw, testID string) (bucket, prefix string, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "s3" || u.Host == "" {
		return "", "", fmt.Errorf("TOKENS_URI는 s3://<버킷>/tokens/<시험 ID>/ 형식이어야 한다")
	}
	want := "tokens/" + testID + "/"
	if p := strings.TrimPrefix(u.Path, "/"); p != want {
		return "", "", fmt.Errorf("TOKENS_URI의 경로는 %s 이어야 한다", want)
	}
	return u.Host, want, nil
}
