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
	"slices"
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

// TestID 는 시험 ID 다: lt-<UTC yyyymmddThhmm>-<run_id>. 소문자, 숫자, - 만 쓴다(RunTask startedBy, S3 경로, 태그).
// 실행 번호(run_number)는 워크플로마다 따로 세서 시나리오별 워크플로끼리 겹친다. 실행 ID 는 저장소 안에서 하나뿐이다
func TestID(now time.Time, runID string) (string, error) {
	n, err := strconv.ParseInt(runID, 10, 64)
	if err != nil || n <= 0 {
		return "", fmt.Errorf("GITHUB_RUN_ID %q 가 숫자가 아니다", runID)
	}
	id := fmt.Sprintf("lt-%st%s-%d", now.UTC().Format("20060102"), now.UTC().Format("1504"), n)
	if len(id) > plan.MaxTestIDLen {
		return "", fmt.Errorf("시험 ID %s 가 %d자를 넘는다(GITHUB_RUN_ID 가 너무 길다)", id, plan.MaxTestIDLen)
	}
	return id, nil
}

var testIDPattern = regexp.MustCompile(`^lt-\d{8}t\d{4}-\d+$`)

// CheckTestID 는 손으로 넣은 시험 ID(stop, cleanup 입력)가 형식에 맞는지 본다
func CheckTestID(id string) error {
	if !testIDPattern.MatchString(id) || len(id) > plan.MaxTestIDLen {
		return fmt.Errorf("시험 ID %q 가 lt-<yyyymmdd>t<hhmm>-<번호> 형식이 아니거나 %d자를 넘는다", id, plan.MaxTestIDLen)
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
	integer := func(name string, set func(int)) {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			return
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %q 는 정수여야 한다", strings.ToLower(strings.TrimPrefix(name, "LT_")), v))
		}
		set(n)
	}
	integer("LT_RUNNERS", func(n int) { in.Runners = n })
	integer("LT_MEMBERS", func(n int) { in.Members = n })
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

// Phase 는 사전예약 회차의 접수 단계다(be catalog 상품 상세 campaign.status).
const (
	BeforeOpen = "BEFORE_OPEN"
	Open       = "OPEN"
)

// Want 는 사전 점검에서 볼 상품 하나와 그 상품의 회차가 있어야 할 접수 단계다. Phases 가 비면 단계를 보지 않는다.
// Promoted 가 true 면 지금 보이는 사전예약(홍보)에 묶여 있어야 한다. 고객은 사전예약 상세에서만 "예약하기"를 누르므로
// k6 도 묶이지 않은 상품으로는 시작하지 않는다
type Want struct {
	ID       string
	Phases   []string
	Promoted bool
}

// Precheck 은 공개 상품 API 로 시험 상품이 있고, 판매 중이며, 판매 중인 옵션이 있고, 회차가 원하는 접수 단계인지 본다.
// 상품의 status 는 판매 상태(SaleStatus)이고, 접수 단계는 campaign.status 에 따로 실린다(be catalog ProductDetailView, 27cbb895).
// 사전예약에 묶였는지는 사전예약 목록과 상세(GET /api/v1/promotions, /{id})로 본다.
// 회차 일정, 경보, 시험 회원 준비는 러너가 볼 수 없어 runbook 의 점검표로 본다.
func Precheck(ctx context.Context, client *http.Client, baseURL, testID string, wants ...Want) error {
	get := func(path string, out any) error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
		req.Header.Set("User-Agent", "nova-loadtest/"+testID)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("응답을 읽지 못했다: %w", err)
		}
		return nil
	}
	var promoted map[string]bool
	var failed []string
	for _, w := range wants {
		id := w.ID
		if id == "" {
			continue
		}
		var env struct {
			Data struct {
				Status   string `json:"status"`
				Campaign *struct {
					Status string `json:"status"`
				} `json:"campaign"`
				Variants []struct {
					Status string `json:"status"`
				} `json:"variants"`
			} `json:"data"`
		}
		if err := get("/api/v1/products/"+id, &env); err != nil {
			return fmt.Errorf("시험 상품 %s 를 확인하지 못했다: %w", id, err)
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
		if len(w.Phases) > 0 {
			phase := ""
			if env.Data.Campaign != nil {
				phase = env.Data.Campaign.Status
			}
			if !slices.Contains(w.Phases, phase) {
				return fmt.Errorf("시험 상품 %s 의 회차가 %s 이어야 하는데 %q 다(BEFORE_OPEN 오픈 전, OPEN 접수 중, CLOSED 마감)",
					id, strings.Join(w.Phases, " 또는 "), phase)
			}
		}
		if !w.Promoted {
			continue
		}
		if promoted == nil {
			var err error
			if promoted, failed, err = promotedProducts(get); err != nil {
				return fmt.Errorf("사전예약 목록을 확인하지 못했다: %w", err)
			}
		}
		if !promoted[strings.ToLower(id)] {
			note := ""
			if len(failed) > 0 {
				note = fmt.Sprintf(" 상세를 읽지 못한 사전예약이 있다: %s", strings.Join(failed, ", "))
			}
			return fmt.Errorf("시험 상품 %s 가 지금 보이는 사전예약(홍보)에 묶여 있지 않다. testproduct 로 만들거나 관리자 화면에서 묶고, 기간에 오늘이 드는지 본다.%s", id, note)
		}
	}
	return nil
}

// promotedProducts 는 지금 보이는 사전예약에 묶인 상품 ID(소문자)다. 목록은 쪽을 나누지 않는다.
// 상세를 읽지 못한 사전예약은 건너뛰고 failed 에 남긴다. 시험 상품과 상관없는 사전예약 하나 때문에 시험을 막지 않는다
// (예를 들어 막 끝난 사전예약이 캐시된 목록에 남아 있으면 상세는 404 다)
func promotedProducts(get func(string, any) error) (out map[string]bool, failed []string, err error) {
	var list struct {
		Data struct {
			Items []struct {
				PromotionID string `json:"promotionId"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := get("/api/v1/promotions", &list); err != nil {
		return nil, nil, err
	}
	out = map[string]bool{}
	for _, it := range list.Data.Items {
		var d struct {
			Data struct {
				Products []struct {
					ProductID string `json:"productId"`
				} `json:"products"`
			} `json:"data"`
		}
		if err := get("/api/v1/promotions/"+url.PathEscape(it.PromotionID), &d); err != nil {
			failed = append(failed, fmt.Sprintf("%s(%v)", it.PromotionID, err))
			continue
		}
		for _, p := range d.Data.Products {
			out[strings.ToLower(p.ProductID)] = true
		}
	}
	return out, failed, nil
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
