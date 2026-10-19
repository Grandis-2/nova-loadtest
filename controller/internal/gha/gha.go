// Package gha는 GitHub Actions(와 로컬 Forgejo) 러너에서 도는 명령(cmd/gha)의 로직이다.
// 워크플로는 셸 스크립트 대신 이 명령을 부른다. 로컬 Forgejo 러너에는 jq, AWS CLI 가 없기 때문이다(구현 계획 7.3).
package gha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Grandis-2/nova-loadtest/controller/internal/infra"
	"github.com/Grandis-2/nova-loadtest/controller/internal/plan"
)

const githubURL = "https://github.com"

// Guard는 운영에 닿지 않게 실행 위치를 확인한다(구현 계획 7.4).
//   - GitHub 이 아니면(로컬 Forgejo) AWS_ENDPOINT_URL(moto)과 AWS_ENDPOINT_URL_ECS(fake-ecs)가 로컬 주소여야 한다
//   - CONTROLLER_INLINE=1 은 GitHub 이 아닐 때만 받는다
func Guard(getenv func(string) string) error {
	server := getenv("GITHUB_SERVER_URL")
	inline := getenv("CONTROLLER_INLINE") == "1"
	if server == githubURL {
		if inline {
			return errors.New("GitHub 에서는 CONTROLLER_INLINE 을 쓰지 않는다")
		}
		return nil
	}
	for _, k := range []string{"AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_ECS"} {
		if !localURL(getenv(k)) {
			return fmt.Errorf("GitHub 이 아닌 곳(%s)에서는 %s 가 로컬 주소여야 한다(받은 값 %q)", server, k, getenv(k))
		}
	}
	return nil
}

// lookupIP 는 테스트가 바꾼다
var lookupIP = net.LookupIP

// localURL 은 주소의 호스트가 루프백이나 사설 IP 로만 풀리는지 본다(compose 안의 이름도 풀어서 본다).
// 이름만 보고 판단하면 점이 없는 이름이 외부 주소로 풀려도 통과한다.
func localURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	h := u.Hostname()
	ips := []net.IP{net.ParseIP(h)}
	if ips[0] == nil {
		if ips, err = lookupIP(h); err != nil || len(ips) == 0 {
			return false
		}
	}
	for _, ip := range ips {
		if !ip.IsLoopback() && !ip.IsPrivate() {
			return false
		}
	}
	return true
}

// TestID 는 시험 ID 다: lt-<UTC yyyymmddThhmm>-<run_number>. 소문자, 숫자, - 만 쓴다(RunTask startedBy, S3 경로, 태그)
func TestID(now time.Time, runNumber string) (string, error) {
	n, err := strconv.Atoi(runNumber)
	if err != nil || n <= 0 {
		return "", fmt.Errorf("GITHUB_RUN_NUMBER %q 가 숫자가 아니다", runNumber)
	}
	return fmt.Sprintf("lt-%st%s-%d", now.UTC().Format("20060102"), now.UTC().Format("1504"), n), nil
}

var testIDPattern = regexp.MustCompile(`^lt-\d{8}t\d{4}-\d+$`)

// CheckTestID 는 손으로 넣은 시험 ID(stop, cleanup 입력)가 형식에 맞는지 본다
func CheckTestID(id string) error {
	if !testIDPattern.MatchString(id) || len(id) > 36 {
		return fmt.Errorf("시험 ID %q 가 lt-<yyyymmdd>t<hhmm>-<번호> 형식이 아니다", id)
	}
	return nil
}

// Inputs 는 워크플로 입력(LT_<이름> 환경 변수)을 계획 입력으로 바꾼다.
func Inputs(getenv func(string) string) (plan.Input, error) {
	in := plan.Input{
		TestID:        getenv("TEST_ID"),
		Scenario:      getenv("LT_SCENARIO"),
		ProductID:     strings.TrimSpace(getenv("LT_PRODUCT_ID")),
		PrepProductID: strings.TrimSpace(getenv("LT_PREP_PRODUCT_ID")),
		OpenAt:        strings.TrimSpace(getenv("LT_OPEN_AT")),
		Duration:      strings.TrimSpace(getenv("LT_DURATION")),
		RampUp:        strings.TrimSpace(getenv("LT_RAMP_UP")),
	}
	var errs []error
	num := func(name string, set func(float64)) {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			return
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %q 는 숫자여야 한다", strings.ToLower(strings.TrimPrefix(name, "LT_")), v))
			return
		}
		set(f)
	}
	num("LT_PEAK_RPS", func(f float64) { in.PeakRPS = f })
	num("LT_ABORT_ERROR_RATE", func(f float64) { in.AbortErrorRate = f })
	if v := strings.TrimSpace(getenv("LT_RUNNERS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("runners %q 는 정수여야 한다", v))
		}
		in.Runners = n
	}
	if f := strings.TrimSpace(getenv("LT_FLOWS")); f != "" {
		for _, x := range strings.Split(f, ",") {
			if x = strings.TrimSpace(x); x != "" {
				in.Flows = append(in.Flows, x)
			}
		}
	}
	if pa := getenv("PLANNED_AT"); pa != "" {
		t, err := time.Parse(time.RFC3339, pa)
		if err != nil {
			errs = append(errs, fmt.Errorf("PLANNED_AT %q: %w", pa, err))
		}
		in.Now = t
	}
	return in, errors.Join(errs...)
}

// MaskLines 는 SSM 값에서 가릴 값마다 ::add-mask:: 줄을 만든다. 다른 출력보다 먼저 내보내야 한다.
func MaskLines(raws ...string) ([]string, error) {
	var out []string
	for _, raw := range raws {
		if raw == "" {
			continue // 정리 모드는 관측 값을 읽지 않는다
		}
		vals, err := infra.SecretValues(raw)
		if err != nil {
			return nil, err
		}
		for _, v := range vals {
			// 여러 줄 값은 줄마다 가린다
			for _, line := range strings.Split(v, "\n") {
				if line = strings.TrimSpace(line); line != "" {
					out = append(out, "::add-mask::"+line)
				}
			}
		}
	}
	return out, nil
}

// Precheck 은 공개 상품 API 로 시험 상품이 있고, 판매 중이며, 판매 중인 옵션이 있는지 본다.
// 회차 일정, 경보, 시험 회원 준비는 러너가 볼 수 없어 runbook 의 점검표로 본다.
func Precheck(ctx context.Context, client *http.Client, baseURL, testID string, productIDs ...string) error {
	for _, id := range productIDs {
		if id == "" {
			continue
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/v1/products/"+id, nil)
		req.Header.Set("User-Agent", "nova-loadtest/"+testID)
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("시험 상품 %s 확인: %w", id, err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("시험 상품 %s 를 찾지 못했다(HTTP %d)", id, resp.StatusCode)
		}
		var env struct {
			Data struct {
				Status   string `json:"status"`
				Variants []struct {
					Status string `json:"status"`
				} `json:"variants"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return fmt.Errorf("시험 상품 %s 응답을 읽지 못했다: %w", id, err)
		}
		if env.Data.Status != "ACTIVE" {
			return fmt.Errorf("시험 상품 %s 가 판매 중(ACTIVE)이 아니다(%s)", id, env.Data.Status)
		}
		active := false
		for _, v := range env.Data.Variants {
			active = active || v.Status == "ACTIVE"
		}
		if !active {
			return fmt.Errorf("시험 상품 %s 에 판매 중인 옵션이 없다", id)
		}
	}
	return nil
}

// AppendFile 은 GITHUB_STEP_SUMMARY, GITHUB_OUTPUT 같은 파일에 덧붙인다. 경로가 비면 아무것도 하지 않는다.
func AppendFile(path, text string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}
