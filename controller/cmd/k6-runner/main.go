// k6-runner는 k6 이미지의 시작 프로그램이다(docs/implementation-plan.md 2.2).
//
//  1. TOKENS_URI(s3://<버킷>/tokens/<시험 ID>/)에서 이 태스크의 토큰 파일 {a,b,c}-<TASK_INDEX>.json을 받는다.
//     상품 조회만 하는 시험은 토큰이 없어 TOKENS_URI를 넘기지 않는다
//  2. START_AT까지 기다린다. k6에는 시작 시각을 정하는 옵션이 없다
//  3. k6를 자식 프로세스로 띄운다. SIGTERM을 받으면 k6에 넘기고 끝날 때까지 기다린다(k6는 105로 끝난다)
//  4. k6가 끝나면 {exit_code, image_digest, summary}를 RESULTS_URI(s3://<버킷>/results/<시험 ID>/)의
//     k6-<TASK_INDEX>.json에 올리고, k6의 종료 코드로 끝난다
//
// 미리 서명한 URL은 쓰지 않는다. 태스크 역할의 임시 자격 증명으로 서명한 URL은 자격 증명과 함께 만료되어,
// soak처럼 긴 시험에서는 끝날 때 올리지 못할 수 있다. SDK는 자격 증명을 스스로 갱신한다.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var testIDRe = regexp.MustCompile(`^lt-[0-9]{8}t[0-9]{4}-[0-9]+$`)

// 이 프로그램 자체가 실패한 경우의 종료 코드. k6의 종료 코드(0, 99, 105, 107, 108 등)와 겹치지 않게 둔다
const runnerFailed = 120

type result struct {
	TestID      string          `json:"test_id"`
	TaskIndex   string          `json:"task_index"`
	ExitCode    int             `json:"exit_code"`
	ImageDigest string          `json:"image_digest,omitempty"`
	StartedAt   time.Time       `json:"started_at"`
	EndedAt     time.Time       `json:"ended_at"`
	Summary     json.RawMessage `json:"summary,omitempty"`
	Error       string          `json:"error,omitempty"`
}

func main() {
	log.SetFlags(0)
	os.Exit(run())
}

func run() int {
	ctx := context.Background()
	testID := os.Getenv("TEST_ID")
	task := os.Getenv("TASK_INDEX")
	if !testIDRe.MatchString(testID) || task == "" {
		log.Printf("TEST_ID(%q), TASK_INDEX(%q)를 확인한다", testID, task)
		return runnerFailed
	}
	if os.Getenv("LOCAL") == "1" && strings.Contains(os.Getenv("BASE_URL"), "grandis-nova.link") {
		log.Printf("LOCAL=1 인데 BASE_URL이 운영 주소라 시작하지 않는다")
		return runnerFailed
	}

	// 신호는 처음부터 받아 둔다. 기다리는 중에 멈추면 k6를 띄우지 않고 끝낸다
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	resultsBucket, resultsPrefix, err := parseURI(os.Getenv("RESULTS_URI"), "results", testID)
	if err != nil {
		log.Print(err)
		return runnerFailed
	}
	client, err := newS3(ctx)
	if err != nil {
		log.Print(err)
		return runnerFailed
	}
	if uri := os.Getenv("TOKENS_URI"); uri != "" {
		bucket, prefix, err := parseURI(uri, "tokens", testID)
		if err != nil {
			log.Print(err)
			return runnerFailed
		}
		if err := downloadTokens(ctx, client, bucket, prefix, task, tokensDir()); err != nil {
			log.Printf("토큰 받기 실패: %v", err)
			return runnerFailed
		}
	}
	dest := destination{client, resultsBucket, resultsPrefix + "k6-" + task + ".json"}

	res := result{TestID: testID, TaskIndex: task, ImageDigest: imageDigest(ctx)}
	if err := waitUntil(os.Getenv("START_AT"), sigs); err != nil {
		log.Print(err)
		res.ExitCode, res.Error = runnerFailed, err.Error()
		dest.upload(ctx, res)
		return runnerFailed
	}

	res.StartedAt = time.Now().UTC()
	res.ExitCode = runK6(sigs)
	res.EndedAt = time.Now().UTC()
	if b, err := os.ReadFile(summaryPath()); err == nil && json.Valid(b) {
		res.Summary = b
	} else {
		res.Error = "k6 요약 파일이 없다"
	}
	dest.upload(ctx, res)
	log.Printf("k6 종료 코드 %d", res.ExitCode)
	return res.ExitCode
}

func tokensDir() string   { return envOr("TOKENS_DIR", "/tmp/tokens") }
func summaryPath() string { return envOr("SUMMARY_PATH", "/tmp/summary.json") }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// parseURI는 s3://<버킷>/<kind>/<시험 ID>/ 만 받는다. 다른 시험이나 다른 경로를 건드리지 않게 하려는 것이다
func parseURI(raw, kind, testID string) (bucket, prefix string, err error) {
	u, err := url.Parse(raw)
	want := kind + "/" + testID + "/"
	if err != nil || u.Scheme != "s3" || u.Host == "" || strings.TrimPrefix(u.Path, "/") != want {
		return "", "", fmt.Errorf("%s 위치는 s3://<버킷>/%s 여야 한다(받은 값 %q)", kind, want, raw)
	}
	return u.Host, want, nil
}

func newS3(ctx context.Context) (*s3.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		// moto 같은 로컬 S3는 경로 방식 주소만 받는다
		o.UsePathStyle = os.Getenv("AWS_ENDPOINT_URL") != ""
	}), nil
}

func downloadTokens(ctx context.Context, c *s3.Client, bucket, prefix, task, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, pool := range []string{"a", "b", "c"} {
		name := fmt.Sprintf("%s-%s.json", pool, task)
		out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(prefix + name)})
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		b, err := io.ReadAll(out.Body)
		out.Body.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// waitUntil은 START_AT(RFC 3339)까지 기다린다. 비어 있으면 바로 시작한다
func waitUntil(startAt string, sigs <-chan os.Signal) error {
	if startAt == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, startAt)
	if err != nil {
		return fmt.Errorf("START_AT %q: %w", startAt, err)
	}
	d := time.Until(t)
	if d <= 0 {
		log.Printf("START_AT %s가 이미 지났다(%s 늦음). 바로 시작한다", startAt, -d.Round(time.Second))
		return nil
	}
	log.Printf("START_AT %s까지 %s 기다린다", startAt, d.Round(time.Second))
	select {
	case <-time.After(d):
		return nil
	case s := <-sigs:
		return fmt.Errorf("시작 전에 %s를 받아 k6를 띄우지 않았다", s)
	}
}

// runK6는 k6를 띄우고, 받은 신호를 넘기고, k6의 종료 코드를 돌려준다
func runK6(sigs <-chan os.Signal) int {
	args := []string{"run", "--quiet"}
	if seg := os.Getenv("SEGMENT"); seg != "" {
		args = append(args, "--execution-segment", seg, "--execution-segment-sequence", os.Getenv("SEGMENT_SEQUENCE"))
	}
	if os.Getenv("K6_OTEL_HTTP_EXPORTER_ENDPOINT") != "" {
		args = append(args, "--out", "opentelemetry")
	}
	if extra := os.Getenv("K6_EXTRA_ARGS"); extra != "" {
		args = append(args, strings.Fields(extra)...)
	}
	args = append(args, envOr("K6_SCRIPT", "/k6/main.js"))

	cmd := exec.Command(envOr("K6_BIN", "/usr/bin/k6"), args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("k6를 띄우지 못했다: %v", err)
		return runnerFailed
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case s := <-sigs:
			log.Printf("%s를 받아 k6에 넘긴다", s)
			_ = cmd.Process.Signal(s)
		case err := <-done:
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return exit.ExitCode()
			}
			if err != nil {
				log.Printf("k6 실행 오류: %v", err)
				return runnerFailed
			}
			return 0
		}
	}
}

// imageDigest는 ECS 태스크 메타데이터에서 이 컨테이너의 이미지 다이제스트를 읽는다. 로컬에서는 빈 값이다
func imageDigest(ctx context.Context) string {
	base := os.Getenv("ECS_CONTAINER_METADATA_URI_V4")
	if base == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var m struct {
		ImageID string `json:"ImageID"`
	}
	if json.NewDecoder(resp.Body).Decode(&m) != nil {
		return ""
	}
	return m.ImageID
}

type destination struct {
	client      *s3.Client
	bucket, key string
}

func (d destination) upload(ctx context.Context, r result) {
	b, err := json.Marshal(r)
	if err != nil {
		log.Printf("요약 만들기 실패: %v", err)
		return
	}
	// 멈출 때는 stopTimeout(120초) 안에 올려야 한다
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, err = d.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(d.bucket), Key: aws.String(d.key), Body: bytes.NewReader(b), ContentType: aws.String("application/json"),
	})
	if err != nil {
		log.Printf("요약 올리기 실패(%s): %v", d.key, err)
		return
	}
	log.Printf("요약을 올렸다: %s", d.key)
}
