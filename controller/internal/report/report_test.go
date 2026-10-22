package report

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// vus 지표가 없어 VU 가 NaN 이어도 summary.json 과 summary.md 를 만든다
func TestProgressWithoutVUs(t *testing.T) {
	s := Summary{Result: Pass, Progress: []Progress{{Elapsed: 60, RPS: 10, P95: 200, P99: Ratio(math.NaN()), ErrorRate: 0, Rate5xx: 0, VUs: Ratio(math.NaN())}}}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"vus":null`) {
		t.Fatalf("VU 가 없으면 null 이다: %s", b)
	}
	if md := s.Markdown(""); !strings.Contains(md, "| 1:00 | 10.0 | 200ms | - | 0.00% | 0.00% | - |") {
		t.Fatalf("진행 표:\n%s", md)
	}
}
