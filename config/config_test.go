package config

import (
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
