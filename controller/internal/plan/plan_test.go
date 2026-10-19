package plan

import (
	"encoding/json"
	"math"
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
		{"spike에 open_at이 없다", with("spike", func(in *Input) { in.OpenAt = "" }), "open_at"},
		{"spike의 open_at이 너무 가깝다", with("spike", func(in *Input) { in.OpenAt = "2026-10-20T00:03:00Z" }), "준비 구간을 돌릴 시간"},
		{"spike에 prep_product_id가 없다", with("spike", func(in *Input) { in.PrepProductID = "" }), "prep_product_id"},
		{"spike의 두 상품이 같다", with("spike", func(in *Input) { in.PrepProductID = product }), "prep_product_id"},
		{"대문자 UUID", with("load", func(in *Input) { in.ProductID = strings.ToUpper(product) }), "소문자 UUID"},
		{"진입이 있는데 상품이 없다", with("load", func(in *Input) { in.ProductID = "" }), "product_id"},
		{"모르는 시나리오", with("load", func(in *Input) { in.Scenario = "chaos" }), "모른다"},
		{"모르는 흐름", with("load", func(in *Input) { in.Flows = []string{"login"} }), "흐름"},
		{"시험 ID 형식", with("load", func(in *Input) { in.TestID = "Test 1" }), "시험 ID"},
		{"abort_error_rate 범위", with("load", func(in *Input) { in.AbortErrorRate = 80 }), "abort_error_rate"},
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
	c.Flows["catalog"] = config.Flow{Weight: 80, Pool: config.PoolNone, Requests: 6, IterationSeconds: 1}
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
		{"NaN abort_error_rate", with("load", func(in *Input) { in.AbortErrorRate = math.NaN() }), "abort_error_rate"},
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
	prep := float64(p.Members.B.Count + p.Members.C.Count)
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
