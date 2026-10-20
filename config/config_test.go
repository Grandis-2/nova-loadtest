package config

import (
	"math"
	"strings"
	"testing"
)

func TestDefaultParses(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, f := range FlowOrder {
		sum += c.Flows[f].Weight
	}
	if sum != 101 {
		t.Fatalf("평시 흐름 시작 수 합계는 101이어야 한다(breakscale 01): %.0f", sum)
	}
	if len(c.Cleanup.Queues) != 4 {
		t.Fatalf("정리 전에 볼 큐는 넷이다: %v", c.Cleanup.Queues)
	}
	if !c.Members.Contains(c.Members.Start) || c.Members.Contains(c.Members.Start+c.Members.Count) {
		t.Fatal("Contains의 범위 끝이 틀렸다")
	}
}

func TestParseRejects(t *testing.T) {
	for _, tc := range []struct{ name, from, to, want string }{
		{"모르는 키", "admission_rps: 150", "admission_rps: 150\nadmision_rps: 1", "admision_rps"},
		{"틀린 시간", "duration: 2m", "duration: 2분", "시간"},
		{"모르는 모양", "shape: step", "shape: zigzag", "zigzag"},
		{"진입 상한이 입장 속도보다 크다", "entry_cap_rps: 120", "entry_cap_rps: 200", "entry_cap_rps"},
		{"진입 흐름의 풀", "weight: 5\n    pool: a", "weight: 5\n    pool: b", "풀 a"},
		// 리뷰(#2)에서 나온 경우들
		{"두 번째 YAML 문서", "  le_500: \"500\"\n", "  le_500: \"500\"\n---\nadmission_rps: 999\n", "문서는 하나"},
		{"NaN 비율", "  catalog:\n    weight: 80", "  catalog:\n    weight: .nan", "flows.catalog"},
		{"음수 queue_requests", "queue_requests: 2", "queue_requests: -2", "queue_requests"},
		{"음수 ramp_up", "ramp_up: 1m", "ramp_up: -1m", "ramp_up"},
		{"0 배수 단계", "steps: [1, 2, 4, 6, 8]", "steps: [1, 0, 4, 6, 8]", "steps[1]"},
		{"램프보다 짧은 단계", "step_duration: 5m", "step_duration: 5s", "step_duration"},
		{"음수 배경 트래픽", "      cancel: 1\n", "      cancel: -1\n", "background.cancel"},
		{"0초 gracefulStop", "entry_graceful_stop: 3m", "entry_graceful_stop: 0s", "entry_graceful_stop"},
		{"음수 시계열 값", "counters_per_name: 2", "counters_per_name: -100", "series"},
		{"rush 결제 비율이 1보다 크다", "pay_ratio: 0.5", "pay_ratio: 1.5", "scenarios.rush.rush"},
		{"rush 알림 상한이 관찰 시간보다 길다", "notify_timeout: 8m", "notify_timeout: 15m", "scenarios.rush.rush"},
		{"rush 배경에 회원 흐름", "    background:            # 준비 구간이 없어 회원이 필요 없는 상품 조회만 둔다\n      catalog: 200",
			"    background:\n      catalog: 200\n      preorders: 30", "scenarios.rush.background.preorders"},
		{"상품 조회 요청 수가 화면 이동 비율과 다르다", "requests: 7.94", "requests: 3", "flows.catalog.requests"},
		{"검색과 카테고리 비율 합이 1보다 크다", "  search: 0.3", "  search: 0.6", "catalog_paths"},
		{"회원 범위 넘침", "  start: 1 ", "  start: 9223372036854775000 ", "members.start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := string(embedded)
			if !strings.Contains(src, tc.from) {
				t.Fatalf("scenarios.yml에 %q가 없다", tc.from)
			}
			_, err := Parse([]byte(strings.Replace(src, tc.from, tc.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("거절해야 한다(%q): %v", tc.want, err)
			}
		})
	}
}

// 흐름 하나의 평균 요청: 홈 2 + 대분류 3, 카테고리 0.5 × (1 + 0.3 + 0.09), 검색 0.3 × 2, 사전예약 0.15 × 3,
// 상품 누르기(사전예약 메뉴가 아닐 때) 0.85 × 0.7 × 2
func TestCatalogRequests(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.CatalogPaths.Requests(); math.Abs(got-7.935) > 1e-9 {
		t.Fatalf("평균 요청 %.4f(기대 7.935)", got)
	}
}
