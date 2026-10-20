package plan

import (
	"fmt"
	"strings"
	"time"
)

// Markdown은 실행 요약에 넣을 계획 표다.
func (p *Plan) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### 계획: %s (%s)\n\n", p.Scenario, p.TestID)
	b.WriteString("| 항목 | 값 |\n|---|---|\n")
	row := func(k, v string) { fmt.Fprintf(&b, "| %s | %s |\n", k, v) }
	row("흐름", strings.Join(p.Flows, ", "))
	if p.ProductID != "" {
		row("시험 상품", "`"+p.ProductID+"`")
	}
	if p.PrepProductID != "" && p.PrepProductID != p.ProductID {
		row("준비 상품", "`"+p.PrepProductID+"`")
	}
	if p.OpenAt != "" {
		row("회차 열림(open_at)", p.OpenAt)
	}
	row("배수(평시 대비)", fmt.Sprintf("%.2f", p.Multiplier))
	row("k6 태스크", fmt.Sprintf("%d대", p.Tasks))
	row("최대 흐름 시작 수", fmt.Sprintf("%.1f/초", p.Estimates.PeakFlowRPS))
	row("최대 요청 수(대기열 포함)", fmt.Sprintf("%.1f/초", p.Estimates.PeakRequestRPS))
	if !p.WAFExempt {
		row("태스크당 5분 요청(WAF 예외 없음)", fmt.Sprintf("%.0f건", p.Estimates.WAFPer5mPerTask))
	}
	row("최대 VU", fmt.Sprintf("%d (태스크당 메모리 약 %.0fMB)", p.Estimates.MaxVUs, p.Estimates.MemoryMBPerTask))
	row("예상 시계열", fmt.Sprintf("%d개", p.Estimates.Series))
	if m := p.Members; m != nil {
		row("회원", fmt.Sprintf("%d명 (풀 A %d, B %d, C %d)", m.Total(), m.A.Count, m.B.Count, m.C.Count))
	}
	if r := p.Rush; r != nil {
		row("rush 결제", fmt.Sprintf("접수한 회원의 %.0f%%, 알림 확인 %d초 간격, 상한 %s", r.PayRatio*100,
			r.NotifyIntervalSeconds, hms(r.NotifyTimeoutSeconds)))
	}
	if q := p.Queue; q != nil {
		row("queue 인원", fmt.Sprintf("%d명, 진입 %s, 최소 처리 %s, 처리 기한 %s", q.Members, hms(q.ArrivalSeconds),
			hms(q.MinSeconds), hms(q.DeadlineSeconds)))
	}
	row("본 시험", fmt.Sprintf("%s ~ %s (START_AT 기준)", hms(p.MainStart), hms(p.MainEnd)))
	row("예상 시간", hms(p.Estimates.TotalSeconds))
	row("중단 기준 오류율", fmt.Sprintf("%.1f%%", p.AbortErrorRate))

	b.WriteString("\n| 흐름 | 최대 시작 수(초당) |\n|---|---|\n")
	for _, f := range p.FlowRates {
		fmt.Fprintf(&b, "| %s | %.2f |\n", f.Flow, f.PeakRPS)
	}
	if len(p.Warnings) > 0 {
		b.WriteString("\n")
		for _, w := range p.Warnings {
			fmt.Fprintf(&b, "> 경고: %s\n", w)
		}
	}
	return b.String()
}

func hms(s int) string { return (time.Duration(s) * time.Second).String() }
