// Package report는 시험 결과(summary.json, summary.md)를 만든다.
package report

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Result는 판정 결과다(docs/design.md, PLAN.md "판정").
type Result string

const (
	Pass         Result = "pass"         // 합격: 기준을 모두 지켰다
	Fail         Result = "fail"         // 불합격: 끝까지 돌았지만 기준을 넘었다
	Aborted      Result = "aborted"      // 중단: 중단 기준, k6 임계치, stop.yml 로 멈췄다
	Inconclusive Result = "inconclusive" // 판정 불가: 지표가 끊겼거나 Mimir를 조회할 수 없었다
	Limit        Result = "limit"        // 한계 측정: stress. 기준을 지킨 마지막 단계를 적는다
	Canceled     Result = "canceled"     // 취소: 부하를 보내기 전에 멈췄다(시작 시각까지 RUNNING 아님, 이미지 섞임, 발급기 실패 등)
)

var labels = map[Result]string{
	Pass: "합격", Fail: "불합격", Aborted: "중단", Inconclusive: "판정 불가", Limit: "한계 측정", Canceled: "취소",
}

// Success는 워크플로를 성공으로 끝낼지다. 합격과, stress의 한계 측정만 성공이다.
func (r Result) Success() bool { return r == Pass || r == Limit }

// Ratio는 비율이다. 요청이 없으면 NaN(0/0)인데 JSON 은 NaN 을 담지 못하므로 null 로 쓴다.
// 그렇지 않으면 summary.json 을 만들지 못해 결과가 통째로 사라진다
type Ratio float64

func (r Ratio) MarshalJSON() ([]byte, error) {
	if math.IsNaN(float64(r)) || math.IsInf(float64(r), 0) {
		return []byte("null"), nil
	}
	return json.Marshal(float64(r))
}

func (r *Ratio) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*r = Ratio(math.NaN())
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*r = Ratio(f)
	return nil
}

// Check는 기준 하나의 값과 합격 여부다.
type Check struct {
	Name     string  `json:"name"` // 전체는 "all", 흐름 확인은 "flow:<흐름>", 그 밖에는 요청 이름(경로 템플릿)
	Requests float64 `json:"requests"`
	Within   Ratio   `json:"within_500ms"`
	Rate5xx  Ratio   `json:"rate_5xx"`
	Pass     bool    `json:"pass"`
	Note     string  `json:"note,omitempty"`
}

// Window는 판정 구간 하나의 결과다.
type Window struct {
	Name    string    `json:"name"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Pass    bool      `json:"pass"`
	Overall Check     `json:"overall"`
	ByName  []Check   `json:"by_name"`
}

// Task는 k6 태스크 하나의 결과다.
type Task struct {
	Index       int     `json:"index"`
	ARN         string  `json:"arn"`
	ExitCode    *int32  `json:"exit_code,omitempty"`
	ImageDigest string  `json:"image_digest,omitempty"`
	Summary     bool    `json:"summary"` // k6-<i>.json 이 있는가
	Requests    float64 `json:"requests,omitempty"`
	Failed      float64 `json:"failed,omitempty"`
	Error       string  `json:"error,omitempty"`
}

// Summary는 summary.json 이다.
type Summary struct {
	TestID          string    `json:"test_id"`
	Scenario        string    `json:"scenario"`
	Result          Result    `json:"result"`
	Reason          string    `json:"reason,omitempty"`
	StartAt         time.Time `json:"start_at,omitzero"`
	EndedAt         time.Time `json:"ended_at"`
	Windows         []Window  `json:"windows,omitempty"`
	LastPassingStep string    `json:"last_passing_step,omitempty"`
	Tasks           []Task    `json:"tasks,omitempty"`
}

func pct(v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%.2f%%", v*100)
}

// Markdown은 실행 요약에 넣을 결과다.
func (s Summary) Markdown(grafanaURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### 결과: %s (%s)\n\n", labels[s.Result], s.Result)
	if s.Reason != "" {
		fmt.Fprintf(&b, "> %s\n\n", s.Reason)
	}
	if !s.StartAt.IsZero() {
		fmt.Fprintf(&b, "- 시험 시간: %s ~ %s\n", s.StartAt.Format(time.RFC3339), s.EndedAt.Format(time.RFC3339))
	}
	if s.LastPassingStep != "" {
		fmt.Fprintf(&b, "- 기준을 지킨 마지막 단계: %s\n", s.LastPassingStep)
	}
	if grafanaURL != "" {
		fmt.Fprintf(&b, "- Grafana: %s\n", grafanaURL)
	}
	for _, w := range s.Windows {
		fmt.Fprintf(&b, "\n**구간 %s** (%s)\n\n| 요청 | 요청 수 | 0.5초 안 | 5xx | 판정 |\n|---|---|---|---|---|\n", w.Name, mark(w.Pass))
		rows := append([]Check{w.Overall}, w.ByName...)
		for _, c := range rows {
			fmt.Fprintf(&b, "| %s | %.0f | %s | %s | %s |\n", c.Name+note(c.Note), c.Requests, pct(float64(c.Within)), pct(float64(c.Rate5xx)), mark(c.Pass))
		}
	}
	if len(s.Tasks) > 0 {
		b.WriteString("\n**k6 태스크**\n\n| 태스크 | 종료 코드 | 요약 | 요청 수 | 실패 수 | 비고 |\n|---|---|---|---|---|---|\n")
		tasks := append([]Task(nil), s.Tasks...)
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].Index < tasks[j].Index })
		for _, t := range tasks {
			code := "-"
			if t.ExitCode != nil {
				code = fmt.Sprint(*t.ExitCode)
			}
			fmt.Fprintf(&b, "| %d | %s | %s | %.0f | %.0f | %s |\n", t.Index, code, mark(t.Summary), t.Requests, t.Failed, t.Error)
		}
	}
	return b.String()
}

func note(n string) string {
	if n == "" {
		return ""
	}
	return " (" + n + ")"
}

func mark(ok bool) string {
	if ok {
		return "통과"
	}
	return "미달"
}
