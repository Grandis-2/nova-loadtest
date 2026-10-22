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
	Aborted      Result = "aborted"      // 중단: stop.yml 로 멈췄다. 기준을 넘었다고 멈추지는 않는다(NV-780)
	Inconclusive Result = "inconclusive" // 판정 불가: 지표가 끊겼거나, 태스크가 일찍 죽었거나, Mimir를 조회할 수 없었다
	Limit        Result = "limit"        // 한계 측정: stress. 기준을 지킨 마지막 단계를 적는다
	Canceled     Result = "canceled"     // 취소: 부하를 보내기 전에 멈췄다(시작 시각까지 RUNNING 아님, 이미지 섞임, 발급기 실패 등)
)

var labels = map[Result]string{
	Pass: "합격", Fail: "불합격", Aborted: "중단", Inconclusive: "판정 불가", Limit: "한계 측정", Canceled: "취소",
}

// Success는 워크플로를 성공으로 끝낼지다. 계획대로 끝까지 돌아 결과를 낸 시험(합격, 불합격, stress의 한계 측정)은
// 성공이다. 불합격도 시험 결과이고, 도구가 제대로 돌지 못한 경우(중단, 판정 불가, 취소)만 실패로 보인다
func (r Result) Success() bool { return r == Pass || r == Fail || r == Limit }

// Ratio는 비율이나 지연(ms)이다. 요청이 없으면 NaN(0/0)인데 JSON 은 NaN 을 담지 못하므로 null 로 쓴다.
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
	RPS      float64 `json:"rps"` // 구간 평균 초당 요청
	Within   Ratio   `json:"within_500ms"`
	Rate5xx  Ratio   `json:"rate_5xx"`
	// ErrorRate 는 기대하지 않은 응답(5xx, 4xx, 연결 실패, 시간 초과)의 비율이다. 판정에는 쓰지 않고 보여 준다
	ErrorRate Ratio `json:"error_rate"`
	// 지연(ms). 히스토그램 버킷 사이를 직선으로 이은 근사라 판정에는 쓰지 않는다(판정은 Within)
	P50  Ratio  `json:"p50_ms"`
	P95  Ratio  `json:"p95_ms"`
	P99  Ratio  `json:"p99_ms"`
	Pass bool   `json:"pass"`
	Note string `json:"note,omitempty"`
}

// Progress 는 시험 도중 1분마다 본 최근 1분의 값이다.
type Progress struct {
	At        time.Time `json:"at"`
	Elapsed   int       `json:"elapsed_seconds"` // START_AT 부터
	RPS       float64   `json:"rps"`
	P95       Ratio     `json:"p95_ms"`
	P99       Ratio     `json:"p99_ms"`
	ErrorRate Ratio     `json:"error_rate"`
	Rate5xx   Ratio     `json:"rate_5xx"`
	VUs       Ratio     `json:"vus"` // vus 지표가 1분 넘게 없으면 NaN(null)이다
}

// Window는 판정 구간 하나의 결과다.
type Window struct {
	Name    string    `json:"name"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Pass    bool      `json:"pass"`
	Partial bool      `json:"partial,omitempty"` // 구간이 끝나기 전에 시험이 멈춰 멈춘 때까지만 봤다
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
	// Events 는 시험 도중 있었던 일이다(태스크가 일찍 끝남, 지표 끊김, Mimir 조회 실패). 시험을 멈추지 않고 적어 둔다
	Events   []string   `json:"events,omitempty"`
	Progress []Progress `json:"progress,omitempty"`
}

// maxProgressRows 는 summary.md 의 진행 표에 넣는 줄 수 상한이다(soak 4시간이면 240줄이라 고르게 줄인다)
const maxProgressRows = 30

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
	if len(s.Events) > 0 {
		b.WriteString("\n**시험 도중 있었던 일**\n\n")
		for _, e := range s.Events {
			fmt.Fprintf(&b, "- %s\n", e)
		}
	}
	for _, w := range s.Windows {
		partial := ""
		if w.Partial {
			partial = ", 구간 중간에 멈춰 멈춘 때까지만 봤다"
		}
		fmt.Fprintf(&b, "\n**구간 %s** (%s%s, %s ~ %s)\n\n", w.Name, mark(w.Pass), partial, w.Start.Format(time.TimeOnly), w.End.Format(time.TimeOnly))
		b.WriteString("| 요청 | 요청 수 | 초당 | p50 | p95 | p99 | 0.5초 안 | 실패 | 5xx | 판정 |\n|---|---|---|---|---|---|---|---|---|---|\n")
		rows := append([]Check{w.Overall}, w.ByName...)
		for _, c := range rows {
			if strings.Contains(c.Name, ":") {
				// flow:, scenario:, queue: 는 요청 이름이 아닌 확인 항목이다. 수와 메모만 있다
				fmt.Fprintf(&b, "| %s | %.0f | - | - | - | - | - | - | - | %s |\n", c.Name+note(c.Note), c.Requests, mark(c.Pass))
				continue
			}
			fmt.Fprintf(&b, "| %s | %.0f | %.1f | %s | %s | %s | %s | %s | %s | %s |\n", c.Name+note(c.Note), c.Requests, c.RPS,
				Ms(c.P50), Ms(c.P95), Ms(c.P99), pct(float64(c.Within)), pct(float64(c.ErrorRate)), pct(float64(c.Rate5xx)), mark(c.Pass))
		}
	}
	if len(s.Progress) > 0 {
		step := (len(s.Progress) + maxProgressRows - 1) / maxProgressRows
		fmt.Fprintf(&b, "\n**진행(최근 1분 값, %d분 간격)**\n\n| 경과 | 초당 요청 | p95 | p99 | 실패 | 5xx | VU |\n|---|---|---|---|---|---|---|\n", step)
		for i := 0; i < len(s.Progress); i += step {
			p := s.Progress[i]
			fmt.Fprintf(&b, "| %s | %.1f | %s | %s | %s | %s | %s |\n", Clock(p.Elapsed), p.RPS, Ms(p.P95), Ms(p.P99),
				pct(float64(p.ErrorRate)), pct(float64(p.Rate5xx)), Count(p.VUs))
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

// Count 는 개수다. 값이 없으면(NaN) "-"다
func Count(v Ratio) string {
	if f := float64(v); !math.IsNaN(f) && !math.IsInf(f, 0) {
		return fmt.Sprintf("%.0f", f)
	}
	return "-"
}

// Ms 는 지연을 읽기 쉽게 쓴다. 1초가 넘으면 초로 쓴다
func Ms(v Ratio) string {
	f := float64(v)
	switch {
	case math.IsNaN(f) || math.IsInf(f, 0):
		return "-"
	case f >= 1000:
		return fmt.Sprintf("%.2fs", f/1000)
	}
	return fmt.Sprintf("%.0fms", f)
}

// Clock 은 초를 m:ss(한 시간이 넘으면 h:mm:ss)로 쓴다
func Clock(s int) string {
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s%3600/60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
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
