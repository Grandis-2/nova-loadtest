package plan

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Grandis-2/nova-loadtest/config"
)

const (
	product = "a1b2c3d4-0000-4000-8000-000000000001"
	prepPID = "a1b2c3d4-0000-4000-8000-000000000002"
	testID  = "lt-20261020t0630-1"
)

var now = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

func cfg(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func input(scenario string) Input {
	in := Input{TestID: testID, Scenario: scenario, ProductID: product, Now: now}
	if scenario == "spike" {
		in.PrepProductID = prepPID
	}
	if scenario == "spike" || scenario == "rush" {
		in.OpenAt = "2026-10-20T00:30:00Z" // 지금부터 30분 뒤(최대 1시간)
	}
	return in
}

func mustPlan(t *testing.T, c *config.Config, in Input) *Plan {
	t.Helper()
	p, err := New(c, in)
	if err != nil {
		t.Fatalf("계획이 나와야 한다: %v", err)
	}
	return p
}

func mustReject(t *testing.T, c *config.Config, in Input, want string) {
	t.Helper()
	_, err := New(c, in)
	if err == nil {
		t.Fatalf("거절해야 하는데 계획이 나왔다")
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("오류에 %q가 없다: %v", want, err)
	}
}

func TestDefaults(t *testing.T) {
	c := cfg(t)
	for _, s := range config.ScenarioOrder {
		if s == "rush" || s == "queue" {
			continue // 흐름이 정해져 있다(TestRush, TestQueue)
		}
		t.Run(s, func(t *testing.T) {
			p := mustPlan(t, c, input(s))
			if p.Tasks < 1 || len(p.Scenarios) == 0 || len(p.Windows) == 0 || len(p.FlowRates) != 5 {
				t.Fatalf("계획 표가 비었다: %+v", p)
			}
			if p.Members == nil || p.Members.A.Count == 0 || p.Members.B.Count == 0 || p.Members.C.Count == 0 {
				t.Fatalf("다섯 흐름이면 풀 A, B, C가 모두 있어야 한다: %+v", p.Members)
			}
			if p.Estimates.Series <= 0 || p.Estimates.MaxVUs <= 0 || p.Estimates.TotalSeconds <= p.MainEnd-1 {
				t.Fatalf("추정치가 이상하다: %+v", p.Estimates)
			}
			// 회원 범위는 겹치지 않고 시험 회원 범위 안이다
			m := p.Members
			if m.C.Start != m.B.Start+m.B.Count || m.A.Start != m.C.Start+m.C.Count ||
				!c.Members.Contains(m.B.Start) || !c.Members.Contains(m.A.Start+m.A.Count-1) {
				t.Fatalf("회원 범위가 이상하다: %+v", m)
			}
			// PLAN_JSON은 RunTask 덮어쓰기 8KiB 안에 다른 값과 함께 들어가야 한다
			b, _ := json.Marshal(p)
			if len(b) > 6000 {
				t.Fatalf("계획 JSON이 %d바이트로 너무 크다", len(b))
			}
		})
	}
}

// 문서(docs/design.md "회원 수")에 적은 값과 맞는지 본다
func TestMembersMatchDocs(t *testing.T) {
	c := cfg(t)
	for _, tc := range []struct {
		scenario string
		a, c     int64
	}{
		// 한 번만 쓰는 풀에는 태스크 상한(20)만큼 여유를 둔다. spike의 풀 A는 몰림과 관찰에 각각 둔다
		{"stress", 31500 + 20, 6300 + 20},
		{"soak", 72000 + 20, 14400 + 20},
		{"spike", 8000 + 40, 611 + 20}, // 배경 취소는 본 시험 611초 동안 초당 1건
	} {
		p := mustPlan(t, c, input(tc.scenario))
		if p.Members.A.Count != tc.a || p.Members.C.Count != tc.c {
			t.Errorf("%s: 풀 A %d, C %d (기대 %d, %d)", tc.scenario, p.Members.A.Count, p.Members.C.Count, tc.a, tc.c)
		}
	}
}

func TestSmokeWAF(t *testing.T) {
	c := cfg(t)
	p := mustPlan(t, c, input("smoke"))
	if p.WAFExempt {
		t.Fatal("smoke는 WAF 예외 없이 돌 수 있어야 한다")
	}
	if p.Estimates.WAFPer5mPerTask > c.WAFLimitPer5m || p.Estimates.WAFPer5mPerTask < 1500 {
		t.Fatalf("smoke의 태스크당 5분 요청 %.0f건(문서 계산 약 1,800건, 상한 2,400건)", p.Estimates.WAFPer5mPerTask)
	}
}

func TestCatalogOnlySmoke(t *testing.T) {
	c := cfg(t)
	in := Input{TestID: testID, Scenario: "smoke", Flows: []string{"catalog"}, Now: now}
	p := mustPlan(t, c, in)
	if p.Members != nil || p.MainStart != 0 || p.TokenTTLSeconds != 0 {
		t.Fatalf("상품 조회만이면 회원, 준비 구간, 토큰이 없어야 한다: %+v", p)
	}
	if len(p.Scenarios) != 1 || p.Scenarios[0].Flow != "catalog" {
		t.Fatalf("시나리오는 catalog 하나여야 한다: %+v", p.Scenarios)
	}
}

func TestRejects(t *testing.T) {
	c := cfg(t)
	with := func(s string, f func(*Input)) Input { in := input(s); f(&in); return in }

	cases := []struct {
		name string
		in   Input
		want string
	}{
		{"smoke 상한 초과", with("smoke", func(in *Input) { in.PeakRPS = 50 }), "상한"},
		{"회원 부족(soak 6시간)", with("soak", func(in *Input) { in.Duration = "6h" }), "회원"},
		{"VU 상한 초과(spike 몰림 2만 명)", with("spike", func(in *Input) { in.PeakRPS = 2000 }), "VU"},
		{"runners가 계산보다 적다", with("spike", func(in *Input) { in.Runners = 1 }), "runners"},
		{"max_tasks 초과", with("load", func(in *Input) { in.Runners = 99 }), "max_tasks"},
		{"spike의 open_at이 너무 가깝다", with("spike", func(in *Input) { in.OpenAt = "2026-10-20T00:03:00Z" }), "준비 구간을 돌릴 시간"},
		{"spike에 prep_product_id가 없다", with("spike", func(in *Input) { in.PrepProductID = "" }), "prep_product_id"},
		{"spike의 두 상품이 같다", with("spike", func(in *Input) { in.PrepProductID = product }), "prep_product_id"},
		{"대문자 UUID", with("load", func(in *Input) { in.ProductID = strings.ToUpper(product) }), "소문자 UUID"},
		{"진입이 있는데 상품이 없다", with("load", func(in *Input) { in.ProductID = "" }), "product_id"},
		{"모르는 시나리오", with("load", func(in *Input) { in.Scenario = "chaos" }), "모른다"},
		{"모르는 흐름", with("load", func(in *Input) { in.Flows = []string{"login"} }), "흐름"},
		{"시험 ID 형식", with("load", func(in *Input) { in.TestID = "Test 1" }), "시험 ID"},
		{"product_id 칸에 흐름 이름", with("soak", func(in *Input) { in.ProductID = "catalog" }), "흐름 이름이면 flows 칸에 넣는다"},
		{"duration 형식", with("load", func(in *Input) { in.Duration = "30분" }), "duration"},
		{"duration 상한", with("load", func(in *Input) { in.Duration = "3h" }), "duration"},
		{"작업 시간 상한", with("soak", func(in *Input) { in.Duration = "5h59m"; in.Flows = []string{"catalog"} }), "작업 상한"},
		{"open_at 형식", with("load", func(in *Input) { in.OpenAt = "내일" }), "RFC 3339"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { mustReject(t, c, tc.in, tc.want) })
	}
}

func TestSeriesLimit(t *testing.T) {
	c := cfg(t)
	c.Series.StatusesPerName = 30 // 상태 코드가 많이 갈리는 경우
	mustReject(t, c, input("stress"), "시계열")
}

func TestWAFLimit(t *testing.T) {
	c := cfg(t)
	c.Flows["catalog"] = config.Flow{Weight: 80, Pool: config.PoolNone, Requests: 15, IterationSeconds: 2} // 기본(7.9건)의 두 배 가까이
	mustReject(t, c, input("smoke"), "WAF")
}

func TestEntryCap(t *testing.T) {
	c := cfg(t)
	in := input("load")
	in.PeakRPS = 101 * 30 // 진입 150/초
	in.Runners = 0
	c.Scenarios["load"] = func() config.Scenario { s := c.Scenarios["load"]; s.MaxRPS = 5000; s.MaxVUs = 100000; return s }()
	c.Runner.MaxTasks = 100
	c.Series.Limit = 1_000_000
	c.Members.Count = 10_000_000 // 진입 상한만 보려고 회원 상한을 푼다
	p := mustPlan(t, c, in)
	var entry float64
	for _, f := range p.FlowRates {
		if f.Flow == "entry" {
			entry = f.PeakRPS
		}
	}
	if entry != c.EntryCapRPS {
		t.Fatalf("진입이 %.0f/초에 묶여야 하는데 %.2f/초다", c.EntryCapRPS, entry)
	}
	if len(p.Warnings) == 0 || !strings.Contains(p.Warnings[0], "진입") {
		t.Fatalf("진입을 묶었다는 경고가 있어야 한다: %v", p.Warnings)
	}
}

func TestSpikeTimeline(t *testing.T) {
	c := cfg(t)
	p := mustPlan(t, c, input("spike"))
	start := p.StartAt(c, now)
	open, _ := time.Parse(time.RFC3339, p.OpenAt)
	if !start.Add(time.Duration(p.MainStart) * time.Second).Equal(open) {
		t.Fatalf("본 시험이 open_at에 시작해야 한다: START_AT %s + %ds != %s", start, p.MainStart, open)
	}
	var burst *K6Scenario
	for i := range p.Scenarios {
		if p.Scenarios[i].Name == "entry_burst" {
			burst = &p.Scenarios[i]
		}
	}
	if burst == nil || burst.StartTime != secs(p.MainStart+1) || burst.Rate != 500*60 {
		t.Fatalf("몰림은 open_at + 1초에 초당 500건이어야 한다: %+v", burst)
	}
	if burst.GracefulStop != "180s" {
		t.Fatalf("진입 시나리오의 gracefulStop은 3분이어야 한다: %q", burst.GracefulStop)
	}
}

func TestStressWindows(t *testing.T) {
	c := cfg(t)
	p := mustPlan(t, c, input("stress"))
	if len(p.Windows) != 5 {
		t.Fatalf("stress는 단계마다 판정 구간이 있어야 한다: %+v", p.Windows)
	}
	for i := 1; i < len(p.Windows); i++ {
		if p.Windows[i].Start <= p.Windows[i-1].End-1 {
			t.Fatalf("판정 구간이 겹친다: %+v", p.Windows)
		}
	}
}

func TestDeterministic(t *testing.T) {
	c := cfg(t)
	for _, s := range config.ScenarioOrder {
		a, _ := json.Marshal(mustPlan(t, c, input(s)))
		in := input(s)
		in.Now = now.Add(5 * time.Minute) // 러너와 컨트롤러는 다른 시각에 계산한다
		b, _ := json.Marshal(mustPlan(t, c, in))
		if string(a) != string(b) {
			t.Fatalf("%s: 같은 입력인데 계획이 다르다", s)
		}
	}
}

func TestFlowOrderIgnored(t *testing.T) {
	c := cfg(t)
	a := input("load")
	a.Flows = []string{"cancel", "catalog", "entry", "order", "preorders", "catalog"}
	b := input("load")
	pa, _ := json.Marshal(mustPlan(t, c, a))
	pb, _ := json.Marshal(mustPlan(t, c, b))
	if string(pa) != string(pb) {
		t.Fatal("흐름을 적는 순서나 중복에 따라 계획이 달라지면 안 된다")
	}
}

// 리뷰(#2)에서 나온 경우들
func TestRejectsInvalidInputs(t *testing.T) {
	c := cfg(t)
	with := func(s string, f func(*Input)) Input { in := input(s); f(&in); return in }
	for _, tc := range []struct {
		name string
		in   Input
		want string
	}{
		{"1초 미만 duration", with("smoke", func(in *Input) { in.Flows = []string{"catalog"}; in.Duration = "500ms" }), "1초 이상"},
		{"초 단위가 아닌 duration", with("smoke", func(in *Input) { in.Duration = "1.5s" }), "초 단위"},
		{"NaN peak_rps", with("load", func(in *Input) { in.PeakRPS = math.NaN() }), "peak_rps"},
		{"무한대 peak_rps", with("load", func(in *Input) { in.PeakRPS = math.Inf(1) }), "peak_rps"},
		{"stress의 duration", with("stress", func(in *Input) { in.Duration = "1h" }), "step_duration"},
		{"smoke의 ramp_up", with("smoke", func(in *Input) { in.RampUp = "5m" }), "ramp_up"},
		{"먼 open_at", with("spike", func(in *Input) { in.OpenAt = "2026-10-20T06:30:00Z" }), "1h0m0s 안"},
	} {
		t.Run(tc.name, func(t *testing.T) { mustReject(t, c, tc.in, tc.want) })
	}
}

// 토큰은 컨트롤러가 시작할 때 만들므로 시작 시각까지의 대기를 덮어야 한다
func TestTokenTTLCoversWait(t *testing.T) {
	c := cfg(t)
	p := mustPlan(t, c, input("spike"))
	min := c.Runner.MaxOpenAtLead.D() + time.Duration(p.Estimates.TotalSeconds)*time.Second
	if got := time.Duration(p.TokenTTLSeconds) * time.Second; got < min {
		t.Fatalf("토큰 유효 시간 %s가 대기 + 시험 시간 %s보다 짧다", got, min)
	}
}

// k6가 실제로 도는 반복 수(1분당 정수 도착률 × 시간)만큼 회원이 있어야 한다
func TestMembersCoverK6Iterations(t *testing.T) {
	c := cfg(t)
	iterations := func(p *Plan, flow string) int64 {
		var n int64
		for _, s := range p.Scenarios {
			if s.Flow != flow {
				continue
			}
			d, _ := time.ParseDuration(s.Duration)
			n += int64(s.Rate) * int64(d.Seconds()) / 60
		}
		return n
	}
	soak := input("soak")
	soak.PeakRPS = 103 // 진입 5.099/초가 1분당 306회(5.1/초)로 반올림된다
	p := mustPlan(t, c, soak)
	if a := iterations(p, "entry"); a > p.Members.A.Count {
		t.Fatalf("soak: 진입 반복 %d회가 풀 A %d명보다 많다", a, p.Members.A.Count)
	}
	sp := mustPlan(t, c, input("spike"))
	if n := iterations(sp, "cancel"); n > sp.Members.C.Count {
		t.Fatalf("spike: 취소 반복 %d회가 풀 C %d명보다 많다", n, sp.Members.C.Count)
	}
}

// 맵 순회 순서와 상관없이 늘 같은 계획이 나와야 한다
func TestDeterministicRepeated(t *testing.T) {
	c := cfg(t)
	for _, s := range config.ScenarioOrder {
		first, _ := json.Marshal(mustPlan(t, c, input(s)))
		for range 50 {
			again, _ := json.Marshal(mustPlan(t, c, input(s)))
			if string(again) != string(first) {
				t.Fatalf("%s: 같은 입력인데 계획이 다르다", s)
			}
		}
	}
}

// 배경 트래픽은 본 시험 시작부터 돈다. 등록 대기 중에 돌면 주문과 취소가 등록 전 예약을 찾는다
func TestSpikeBackgroundStartsWithMain(t *testing.T) {
	c := cfg(t)
	p := mustPlan(t, c, input("spike"))
	for _, s := range p.Scenarios {
		if s.Flow == "entry" || s.Flow == "prepare" {
			continue
		}
		if s.StartTime != secs(p.MainStart) {
			t.Fatalf("배경 %s가 %s에 시작한다(본 시험 %ds)", s.Name, s.StartTime, p.MainStart)
		}
	}
}

// WAF 5분 창: 준비 요청은 등록 대기 앞에서 끝나므로, 준비 끝을 품는 창에는 본 시험이 (300 - 대기)초만 들어간다
func TestWAFWindow(t *testing.T) {
	c := cfg(t)
	p := mustPlan(t, c, input("smoke"))
	// 준비는 등록 확인을 보내지 않는다
	e := c.Flows["entry"]
	prep := float64(p.Members.B.Count+p.Members.C.Count) * (e.Requests - e.RegisterRequests)
	wait := c.Members.RegisterWait.D().Seconds()
	main := float64(p.MainEnd - p.MainStart)
	want := math.Max(p.Estimates.PeakWAFRPS*math.Min(300, main), prep+p.Estimates.PeakWAFRPS*math.Min(main, 300-wait))
	if got := p.Estimates.WAFPer5mPerTask; math.Abs(got-math.Round(want)) > 1 {
		t.Fatalf("태스크당 5분 요청 %.0f건(기대 %.0f건)", got, want)
	}
}

// 토큰 파일은 태스크마다 나누고(mint.Split과 같은 방식), k6는 POOL_FROM/POOL_TO 구간을 반올림해 쓴다.
// 태스크마다 몰림과 관찰 구간이 그 태스크가 돌릴 반복 수(올림)보다 크거나 같아야 회원이 모자라지 않는다
func TestSpikePoolSplitPerTask(t *testing.T) {
	c := cfg(t)
	p := mustPlan(t, c, input("spike"))
	var burst, observe *K6Scenario
	for i := range p.Scenarios {
		switch p.Scenarios[i].Name {
		case "entry_burst":
			burst = &p.Scenarios[i]
		case "entry_observe":
			observe = &p.Scenarios[i]
		}
	}
	iters := func(s *K6Scenario) int64 {
		d, _ := time.ParseDuration(s.Duration)
		return int64(s.Rate) * int64(d.Seconds()) / 60
	}
	split, _ := strconv.ParseFloat(burst.Env["POOL_TO"], 64)
	n := int64(p.Tasks)
	a := p.Members.A.Count
	for i := range n {
		size := a / n
		if i < a%n {
			size++
		}
		to := int64(math.Round(split * float64(size)))
		if need := (iters(burst) + n - 1) / n; to < need {
			t.Fatalf("태스크 %d: 몰림 구간 %d명 < 반복 %d회", i, to, need)
		}
		if need := (iters(observe) + n - 1) / n; size-to < need {
			t.Fatalf("태스크 %d: 관찰 구간 %d명 < 반복 %d회", i, size-to, need)
		}
	}
}

// rush: 몰린 회원(풀 A)만 쓰고 준비 구간이 없다. 결제는 관찰 시간 안에 끝나야 한다
func TestRush(t *testing.T) {
	c := cfg(t)
	p := mustPlan(t, c, input("rush"))
	if strings.Join(p.Flows, ",") != "catalog,entry,order" || p.PrepProductID != "" || p.MainStart != 0 {
		t.Fatalf("흐름 %v, 준비 상품 %q, 본 시험 시작 %d", p.Flows, p.PrepProductID, p.MainStart)
	}
	if m := p.Members; m == nil || m.A.Count != 5000+20 || m.B.Count != 0 || m.C.Count != 0 {
		t.Fatalf("풀 A만 몰림 5,000명 + 여유 20명이어야 한다: %+v", p.Members)
	}
	if p.Rush == nil || p.Rush.PayRatio != 0.5 || p.Rush.NotifyIntervalSeconds != 10 || p.Rush.NotifyTimeoutSeconds != 480 {
		t.Fatalf("rush 값: %+v", p.Rush)
	}
	graceful := int(c.Runner.EntryGracefulStop.D().Seconds())
	if p.Estimates.TotalSeconds != p.MainEnd+graceful {
		t.Fatalf("예상 시간 %d초(기대 %d초)", p.Estimates.TotalSeconds, p.MainEnd+graceful)
	}
	for _, s := range p.Scenarios {
		if s.Flow != "rush" {
			continue
		}
		start, _ := time.ParseDuration(s.StartTime)
		d, _ := time.ParseDuration(s.Duration)
		g, _ := time.ParseDuration(s.GracefulStop)
		// 몰린 회원은 관찰 시간 끝까지 기다릴 수 있고, 예상 시간을 넘지 않는다
		if end := int((start + d + g).Seconds()); end != p.Estimates.TotalSeconds || s.MaxVUs < 5000 {
			t.Fatalf("rush 시나리오가 %d초에 끝난다(예상 %d초), maxVUs %d", end, p.Estimates.TotalSeconds, s.MaxVUs)
		}
		return
	}
	t.Fatal("rush 시나리오가 없다")
}

func TestRushRejects(t *testing.T) {
	c := cfg(t)
	with := func(f func(*Input)) Input { in := input("rush"); f(&in); return in }
	for _, tc := range []struct {
		name string
		in   Input
		want string
	}{
		{"open_at 없음", with(func(in *Input) { in.OpenAt = "" }), "open_at이 필요"},
		{"flows", with(func(in *Input) { in.Flows = []string{"entry"} }), "flows를 쓰지 않는다"},
		{"준비 상품", with(func(in *Input) { in.PrepProductID = prepPID }), "prep_product_id를 쓰지 않는다"},
		{"알림 상한보다 짧은 관찰", with(func(in *Input) { in.Duration = "8m" }), "알림 대기 상한"},
	} {
		t.Run(tc.name, func(t *testing.T) { mustReject(t, c, tc.in, tc.want) })
	}
}

// queue: 인원이 한 번씩 진입하고, 처리 기한까지 기다린다. 값은 docs/design.md "queue" 와 같다
func TestQueue(t *testing.T) {
	c := cfg(t)
	for _, tc := range []struct {
		members      int
		peak         float64
		arrival, min int
		vus, tasks   int // vus 는 예상 동시 인원이다. VU 는 인원(상한 15,000 안)만큼 모두 미리 만든다
	}{
		// 줄 (500-150)×10 = 3,500명 + 입장 뒤 150×5 = 750명
		{5000, 0, 10, 39, 4250, 6},
		{10000, 0, 20, 72, 7750, 9},
		// 2만 명을 초당 500명: 줄 14,000명 + 750명. 상한 15,000 안이다
		{20000, 500, 40, 139, 14750, 13},
		// 99,980명을 165/초: 줄 약 9,000명
		{99980, 165, 606, 672, 9830, 12},
		// 입장 속도보다 느리면 줄이 없다. 동시 인원은 인원을 넘지 않는다
		{100, 50, 2, 7, 100, 1},
		// 도착 시간은 올림한다. 반올림하면 1초에 2,500명(max_rps 2,000 초과)이 들어간다. 실제 진입은 초당 1,250
		{2500, 2000, 2, 22, 2500, 9},
	} {
		in := input("queue")
		in.Members, in.PeakRPS = tc.members, tc.peak
		p := mustPlan(t, c, in)
		q := p.Queue
		if strings.Join(p.Flows, ",") != "entry" || p.PrepProductID != "" || p.MainStart != 0 || len(p.Scenarios) != 1 {
			t.Fatalf("%d명: 흐름 %v, 준비 상품 %q, 본 시험 시작 %d, 시나리오 %d개", tc.members, p.Flows, p.PrepProductID, p.MainStart, len(p.Scenarios))
		}
		deadline := tc.min + 180 // deadline_margin 3분
		if q == nil || q.Members != tc.members || q.ArrivalSeconds != tc.arrival || q.MinSeconds != tc.min || q.DeadlineSeconds != deadline {
			t.Fatalf("%d명: queue 값 %+v", tc.members, q)
		}
		s := p.Scenarios[0]
		// 도착률을 "도착 시간에 인원만큼"으로 적어 k6의 반복 수가 인원과 같다
		// 진입은 open_at + 1초에 시작한다(시계 어긋남. 열리기 전에 들어가면 SALE_NOT_OPEN 으로 실패한다)
		if s.Name != "queue" || s.Flow != "entry" || s.StartTime != "1s" || s.Rate != tc.members || s.TimeUnit != secs(tc.arrival) || s.Duration != secs(tc.arrival) ||
			s.GracefulStop != secs(deadline-tc.arrival) || s.PreAllocatedVUs != s.MaxVUs || s.MaxVUs != max(tc.vus, min(tc.members, 15000)) {
			t.Fatalf("%d명: k6 시나리오 %+v", tc.members, s)
		}
		if p.Tasks != tc.tasks || p.MainEnd != 1+deadline || p.Estimates.TotalSeconds != 1+deadline {
			t.Fatalf("%d명: 태스크 %d, 본 시험 끝 %d, 예상 시간 %d", tc.members, p.Tasks, p.MainEnd, p.Estimates.TotalSeconds)
		}
		if p.Members.A.Count != int64(tc.members)+20 || len(p.Windows) != 1 || p.Windows[0] != (Window{"queue", 0, 1 + tc.min}) {
			t.Fatalf("%d명: 풀 A %d, 판정 구간 %v", tc.members, p.Members.A.Count, p.Windows)
		}
	}
}

func TestQueueRejects(t *testing.T) {
	c := cfg(t)
	with := func(f func(*Input)) Input { in := input("queue"); f(&in); return in }
	for _, tc := range []struct {
		name string
		in   Input
		want string
	}{
		// 줄이 2만 명이 넘는다. 상한 안인 가장 큰 진입 수를 알려 준다(비우면 그 값으로 낮춘다)
		{"동시 인원", with(func(in *Input) { in.Members = 30000; in.PeakRPS = 500 }), "30000명이면 peak_rps를 285 이하로 두거나 비운다"},
		// duration 을 비웠을 때의 처리 기한은 입력이 아니라 계산한 값이다. 계산을 보여 준다
		{"계산한 처리 기한", with(func(in *Input) { in.Members = 99980; in.PeakRPS = 60 }), "처리 기한 30m52s(최소 처리 시간 27m52s + 여유 3m0s)가 queue 상한"},
		{"인원 상한", with(func(in *Input) { in.Members = 99981 }), "1 이상 99980 이하"},
		{"짧은 기한", with(func(in *Input) { in.Duration = "38s" }), "최소 처리 시간 39s 이상이어야 한다"},
		{"flows", with(func(in *Input) { in.Flows = []string{"entry"} }), "queue에는 flows를 쓰지 않는다"},
		{"준비 상품", with(func(in *Input) { in.PrepProductID = prepPID }), "queue에는 prep_product_id를 쓰지 않는다"},
		{"상품 없음", with(func(in *Input) { in.ProductID = "" }), "product_id가 필요하다"},
		{"다른 시나리오의 인원", func() Input { in := input("load"); in.Members = 100; return in }(), "load에는 members를 쓰지 않는다"},
	} {
		t.Run(tc.name, func(t *testing.T) { mustReject(t, c, tc.in, tc.want) })
	}
	// 기한은 최소 처리 시간과 같아도 된다
	p := mustPlan(t, c, with(func(in *Input) { in.Duration = "39s" }))
	if p.Queue.DeadlineSeconds != 39 || p.Scenarios[0].GracefulStop != "29s" {
		t.Fatalf("기한 %+v", p.Queue)
	}
}

// admission_rps 를 넣으면 최소 처리 시간, 처리 기한, 판정 구간을 그 입장 속도로 계산한다.
// 2026-10-26 운영은 입장이 초당 20명이었는데 150명 기준 기한(3m39s)으로 돌아 줄에 선 회원이 끊겼다
func TestQueueAdmissionRPS(t *testing.T) {
	c := cfg(t)
	for _, tc := range []struct {
		admission float64
		min       int // ⌈5,000 ÷ 입장 속도 + 5초⌉
		vus       int // 줄 (500-입장)×10 + 입장×5. VU 는 인원만큼 미리 만든다
		row       string
	}{
		{0, 39, 4250, "입장 초당 150명, 최소 처리 39s, 처리 기한 3m39s"},
		{20, 255, 4900, "입장 초당 20명, 최소 처리 4m15s, 처리 기한 7m15s"},
		{7, 720, 4965, "입장 초당 7명, 최소 처리 12m0s, 처리 기한 15m0s"},
	} {
		in := input("queue")
		in.AdmissionRPS = tc.admission
		p := mustPlan(t, c, in)
		q, s := p.Queue, p.Scenarios[0]
		deadline := tc.min + 180
		want := tc.admission
		if want == 0 {
			want = 150
		}
		if q.AdmissionRPS != want || q.MinSeconds != tc.min || q.DeadlineSeconds != deadline || s.GracefulStop != secs(deadline-10) {
			t.Fatalf("입장 %v: queue 값 %+v, gracefulStop %s", tc.admission, q, s.GracefulStop)
		}
		if p.Windows[0] != (Window{"queue", 0, 1 + tc.min}) || p.MainEnd != 1+deadline || s.MaxVUs != 5000 || s.PreAllocatedVUs != 5000 {
			t.Fatalf("입장 %v: 판정 구간 %v, 본 시험 끝 %d, VU %d/%d", tc.admission, p.Windows, p.MainEnd, s.PreAllocatedVUs, s.MaxVUs)
		}
		if !strings.Contains(p.Markdown(), tc.row) {
			t.Fatalf("입장 %v: 계획 표에 %q 가 없다:\n%s", tc.admission, tc.row, p.Markdown())
		}
		// 예상 동시 인원은 VU 상한 판단에만 쓰인다. 입장이 느릴수록 줄이 길다
		if _, _, _, inFlight := queueLoad(&config.Config{AdmissionRPS: want, Flows: c.Flows}, 5000, 500); int(math.Ceil(inFlight)) != tc.vus {
			t.Fatalf("입장 %v: 예상 동시 인원 %.1f", tc.admission, inFlight)
		}
	}
	// 입력은 이번 계획에만 쓴다. 공용 설정을 바꾸지 않는다
	if c.AdmissionRPS != 150 {
		t.Fatalf("설정의 입장 속도가 %v로 바뀌었다", c.AdmissionRPS)
	}
	with := func(scenario string, f func(*Input)) Input { in := input(scenario); f(&in); return in }
	for _, tc := range []struct {
		name string
		in   Input
		want string
	}{
		{"다른 시나리오", with("load", func(in *Input) { in.AdmissionRPS = 20 }), "load에는 admission_rps를 쓰지 않는다"},
		{"음수", with("queue", func(in *Input) { in.AdmissionRPS = -1 }), "admission_rps -1는 1 이상의 정수여야 한다"},
		// 1 미만이면 최소 처리 시간이 int 범위를 넘는다(1e-16 이면 "최소 처리 -1s" 계획이 나왔다)
		{"1 미만", with("queue", func(in *Input) { in.AdmissionRPS = 1e-16 }), "1 이상의 정수여야 한다"},
		{"소수", with("queue", func(in *Input) { in.AdmissionRPS = 12.5 }), "admission_rps 12.5는 1 이상의 정수여야 한다"},
		{"무한", with("queue", func(in *Input) { in.AdmissionRPS = math.Inf(1) }), "1 이상의 정수여야 한다"},
		{"NaN", with("queue", func(in *Input) { in.AdmissionRPS = math.NaN() }), "1 이상의 정수여야 한다"},
		// 기한을 직접 넣었으면 넣은 입장 속도의 최소 처리 시간과 비교한다
		{"짧은 기한", with("queue", func(in *Input) { in.AdmissionRPS = 20; in.Duration = "4m" }), "최소 처리 시간 4m15s 이상이어야 한다(5000명을 입장 속도 20/초로"},
	} {
		t.Run(tc.name, func(t *testing.T) { mustReject(t, c, tc.in, tc.want) })
	}
}

// peak_rps 를 비우면 기본 500이고, 동시 인원이 VU 상한을 넘으면 넘지 않는 가장 큰 값으로 낮춘다. 넣은 값은 낮추지 않는다
func TestQueueAutoPeak(t *testing.T) {
	c := cfg(t)
	for _, tc := range []struct {
		members int
		peak    float64 // 입력
		want    float64
		lowered bool
	}{
		{5000, 0, 500, false},
		{20000, 0, 500, false},
		{30000, 0, 285, true},
		{99980, 0, 174, true},
		{99980, 100, 100, false},
	} {
		in := input("queue")
		in.Members, in.PeakRPS = tc.members, tc.peak
		p := mustPlan(t, c, in)
		if q := p.Queue; q.PeakRPS != tc.want || q.PeakLowered != tc.lowered || p.Estimates.MaxVUs > 15000 {
			t.Fatalf("%d명, 입력 %v: %+v, VU %d", tc.members, tc.peak, q, p.Estimates.MaxVUs)
		}
		if tc.lowered && !strings.Contains(p.Markdown(), "초당 "+strconv.Itoa(int(tc.want))+"명(VU 상한에 맞춰 낮췄다)") {
			t.Fatalf("계획 표에 낮춘 값을 보여야 한다:\n%s", p.Markdown())
		}
	}
}

// 실제 진입 수(인원 ÷ 도착 시간)는 입력한 peak_rps를 넘지 않는다
func TestQueueRateWithinPeak(t *testing.T) {
	c := cfg(t)
	for _, peak := range []float64{1, 7, 150, 499, 500, 1999, 2000} {
		for n := 1; n <= 9000; n += 97 {
			in := input("queue")
			in.Members, in.PeakRPS = n, peak
			p, err := New(c, in)
			if err != nil {
				continue // 시간이나 VU 상한으로 거절한 경우
			}
			if s := p.Scenarios[0]; float64(n)/float64(p.Queue.ArrivalSeconds) > peak || s.Rate != n {
				t.Fatalf("%d명, peak_rps %v: %d초에 %d번(초당 %.1f)", n, peak, p.Queue.ArrivalSeconds, s.Rate, float64(n)/float64(p.Queue.ArrivalSeconds))
			}
		}
	}
}

// VU 상한을 넘을 때 안내한 peak_rps를 넣으면 상한 안에 든다. 도착 시간을 반올림해 실제 진입 수가 안내 값보다 커지는
// 경우가 있었다(인원 10,037명에 1,913을 안내했는데 다시 넘었다)
func TestQueueVUHint(t *testing.T) {
	c := cfg(t)
	re := regexp.MustCompile(`peak_rps를 (\d+) 이하`)
	checked := 0
	for n := 10001; n <= 99980; n += 37 {
		in := input("queue")
		in.Members, in.PeakRPS = n, 2000
		_, err := New(c, in)
		m := re.FindStringSubmatch(fmt.Sprint(err))
		if m == nil {
			continue
		}
		hint, _ := strconv.ParseFloat(m[1], 64)
		checked++
		// 진입 수를 낮추면 도착 시간이 줄지 않아 동시 인원도 늘지 않는다. 그래서 "이하"로 안내한다
		// (반올림 때문에 안내 값보다 조금 큰 값이 상한 안일 수는 있다)
		for _, r := range []float64{hint, hint - 1, math.Floor(hint / 2)} {
			in.PeakRPS = r
			if _, err := New(c, in); err != nil && strings.Contains(err.Error(), "최대 VU") {
				t.Fatalf("%d명: 안내 %v 이하인 %v를 넣어도 상한을 넘는다: %v", n, hint, r, err)
			}
		}
	}
	if checked < 2000 {
		t.Fatalf("안내를 %d건만 확인했다", checked)
	}
}

// 바로 결제하는 회원이 없으면 주문 흐름이 없고, 판정에서 주문 요청을 찾지 않는다
func TestRushWithoutPayment(t *testing.T) {
	c := cfg(t)
	sc := c.Scenarios["rush"]
	sc.Rush.PayRatio = 0
	c.Scenarios["rush"] = sc
	p := mustPlan(t, c, input("rush"))
	if strings.Join(p.Flows, ",") != "catalog,entry" {
		t.Fatalf("흐름 %v", p.Flows)
	}
}

// open_at 이 없는 spike 는 이미 열린 상품으로 본다. 준비 구간이 끝나는 대로 몰림을 시작하고, 준비도 같은 상품에 할 수 있다
func TestSpikeOpenProduct(t *testing.T) {
	c := cfg(t)
	in := input("spike")
	in.OpenAt = ""
	in.PrepProductID = ""
	p := mustPlan(t, c, in)
	if p.OpenAt != "" || p.PrepProductID != product {
		t.Fatalf("open_at 없이 같은 상품으로 준비해야 한다: open_at=%q prep=%q", p.OpenAt, p.PrepProductID)
	}
	start := p.StartAt(c, now)
	if want := now.Add(c.Runner.StartDelay.D()); !start.Equal(want) {
		t.Errorf("START_AT 은 지금 + start_delay 여야 한다: %s", start)
	}
	if p.MainStart <= 0 {
		t.Error("준비 구간이 본 시험 앞에 있어야 한다")
	}
	burst := false
	for _, s := range p.Scenarios {
		if s.Name == "entry_burst" {
			burst = true
			if s.StartTime != secs(p.MainStart+1) {
				t.Errorf("몰림은 본 시험 시작 + 1초다: %s", s.StartTime)
			}
		}
	}
	if !burst {
		t.Fatal("몰림 시나리오가 있어야 한다")
	}
	// 같은 상품을 따로 넣어도 된다
	in.PrepProductID = product
	mustPlan(t, c, in)
}
