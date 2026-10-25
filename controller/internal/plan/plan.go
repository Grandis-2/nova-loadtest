// Package plan은 입력과 scenarios.yml로 시험 계획을 계산한다.
// 러너(입력 확인)와 컨트롤러가 같은 코드로 계산하므로, 같은 입력이면 늘 같은 계획이 나와야 한다.
// 그래서 지금 시각은 검사에만 쓰고 계획에는 넣지 않는다.
package plan

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Grandis-2/nova-loadtest/config"
)

var (
	uuidRe   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	testIDRe = regexp.MustCompile(`^lt-[0-9]{8}t[0-9]{4}-[0-9]+$`)
)

// MaxTestIDLen 은 시험 ID 의 최대 길이다. RunTask startedBy 는 36자까지이고, 정리 컨트롤러(-c)와
// 정리 태스크(-d)는 시험 ID 뒤에 두 글자를 붙인다
const MaxTestIDLen = 36 - 2

// Input은 시험 워크플로(loadtest-<시나리오>.yml)의 입력이다. 비어 있는 값은 시나리오 기본값을 쓴다.
// 워크플로가 컨트롤러에 INPUT_JSON 으로 넘길 때 아래 JSON 이름을 쓴다.
type Input struct {
	TestID        string    `json:"test_id"`
	Scenario      string    `json:"scenario"`
	ProductID     string    `json:"product_id,omitempty"`
	PrepProductID string    `json:"prep_product_id,omitempty"`
	OpenAt        string    `json:"open_at,omitempty"` // RFC 3339(UTC). 비면 바로 시작한다
	PeakRPS       float64   `json:"peak_rps,omitempty"`
	Duration      string    `json:"duration,omitempty"`
	RampUp        string    `json:"ramp_up,omitempty"`
	Runners       int       `json:"runners,omitempty"`
	Flows         []string  `json:"flows,omitempty"`     // 비면 다섯 흐름 모두
	Members       int       `json:"members,omitempty"`   // queue: 진입하는 인원
	Now           time.Time `json:"planned_at,omitzero"` // 러너가 계획한 시각. open_at이 너무 가까운지 볼 때만 쓴다
}

// Range는 회원 번호 범위 [Start, Start+Count)이다.
type Range struct {
	Start int64 `json:"start"`
	Count int64 `json:"count"`
}

type Members struct {
	A Range `json:"a"`
	B Range `json:"b"`
	C Range `json:"c"`
}

func (m Members) Total() int64 { return m.A.Count + m.B.Count + m.C.Count }

// Stage는 ramping-arrival-rate의 단계다. Target은 TimeUnit당 반복 수다.
type Stage struct {
	Target   int    `json:"target"`
	Duration string `json:"duration"`
}

// K6Scenario는 k6 시나리오 하나다. k6 스크립트는 이 값을 그대로 options.scenarios에 넣는다.
// 도착률은 정수여야 하므로 1분당 반복 수로 적는다.
type K6Scenario struct {
	Name            string  `json:"name"`
	Flow            string  `json:"flow"`
	Executor        string  `json:"executor"`
	StartTime       string  `json:"startTime"`
	TimeUnit        string  `json:"timeUnit"`
	Rate            int     `json:"rate,omitempty"`
	Duration        string  `json:"duration,omitempty"`
	StartRate       int     `json:"startRate,omitempty"`
	Stages          []Stage `json:"stages,omitempty"`
	PreAllocatedVUs int     `json:"preAllocatedVUs"`
	MaxVUs          int     `json:"maxVUs"`
	GracefulStop    string  `json:"gracefulStop,omitempty"`
	// Env는 k6 시나리오의 env다. 풀 A를 여러 시나리오가 나눠 쓸 때 POOL_FROM, POOL_TO(0~1 비율)로 구간을 준다
	Env map[string]string `json:"env,omitempty"`
}

// Window는 판정 구간이다. 초는 START_AT 기준이다.
type Window struct {
	Name  string `json:"name"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// FlowRate는 계획 표에 보이는 흐름별 최대 시작 수다.
type FlowRate struct {
	Flow    string  `json:"flow"`
	PeakRPS float64 `json:"peak_rps"`
}

type Estimates struct {
	PeakFlowRPS     float64 `json:"peak_flow_rps"`
	PeakRequestRPS  float64 `json:"peak_request_rps"`    // 대기열 경로까지 센 초당 요청
	PeakWAFRPS      float64 `json:"peak_waf_rps"`        // WAF 요청 제한에 세는 초당 요청
	WAFPer5mPerTask float64 `json:"waf_per_5m_per_task"` // WAF 예외가 없을 때만 의미가 있다
	MaxVUs          int     `json:"max_vus"`
	MemoryMBPerTask float64 `json:"memory_mb_per_task"`
	Series          int     `json:"series"`
	TotalSeconds    int     `json:"total_seconds"`
}

// Plan은 계획이다. 컨트롤러에 PLAN_JSON으로, S3에 plan.json으로 넘어간다.
type Plan struct {
	TestID          string       `json:"test_id"`
	Scenario        string       `json:"scenario"`
	Flows           []string     `json:"flows"`
	ProductID       string       `json:"product_id,omitempty"`
	PrepProductID   string       `json:"prep_product_id,omitempty"`
	OpenAt          string       `json:"open_at,omitempty"`
	Multiplier      float64      `json:"multiplier"`
	WAFExempt       bool         `json:"waf_exempt"`
	Tasks           int          `json:"tasks"`
	MainStart       int          `json:"main_start"` // 본 시험 시작(초, START_AT 기준)
	MainEnd         int          `json:"main_end"`
	Members         *Members     `json:"members,omitempty"`
	TokenTTLSeconds int          `json:"token_ttl_seconds,omitempty"`
	FlowRates       []FlowRate   `json:"flow_rates"`
	Scenarios       []K6Scenario `json:"scenarios"`
	Windows         []Window     `json:"windows"`
	Estimates       Estimates    `json:"estimates"`
	Warnings        []string     `json:"warnings,omitempty"`
	Rush            *Rush        `json:"rush,omitempty"`
	Queue           *Queue       `json:"queue,omitempty"`
	// Catalog는 상품 조회 흐름의 화면 이동 비율이다(scenarios.yml catalog_paths). k6가 PLAN_JSON에서 읽는다
	Catalog *CatalogPaths `json:"catalog,omitempty"`
}

type CatalogPaths struct {
	HomeRows int     `json:"home_rows"`
	Category float64 `json:"category"`
	Search   float64 `json:"search"`
	Preorder float64 `json:"preorder"`
	NextPage float64 `json:"next_page"`
	MaxPages int     `json:"max_pages"`
	Click    float64 `json:"click"`
}

// Rush는 rush 흐름이 접수 뒤에 쓰는 값이다(scenarios.yml rush). k6가 PLAN_JSON에서 읽는다
type Rush struct {
	PayRatio              float64 `json:"pay_ratio"`
	NotifyIntervalSeconds int     `json:"notify_interval_seconds"`
	NotifyTimeoutSeconds  int     `json:"notify_timeout_seconds"`
}

// Queue는 queue 시나리오의 인원과 시간이다. 초는 진입 시작(본 시험 시작 + 1초) 기준이다. 컨트롤러가 모두 처리되었는지 볼 때 쓴다
type Queue struct {
	Members         int     `json:"members"`
	PeakRPS         float64 `json:"peak_rps"`               // 초당 진입 수(입력, 비우면 기본값이나 낮춘 값)
	PeakLowered     bool    `json:"peak_lowered,omitempty"` // peak_rps 를 비워서 동시 인원이 VU 상한 안에 들게 낮췄다
	ArrivalSeconds  int     `json:"arrival_seconds"`        // 모두 진입하는 시간
	MinSeconds      int     `json:"min_seconds"`            // 입장 속도로 모두 들여보내고 접수까지 가는 최소 시간
	DeadlineSeconds int     `json:"deadline_seconds"`       // 처리 기한(duration). k6는 이때 남은 회원을 끊는다
}

// rushPayRequests는 rush에서 결제하는 회원이 알림을 받은 뒤 보내는 요청이다(알림 목록, 예약 상세, 주문, 결제 시도).
// 알림 수 확인은 따로 센다
const rushPayRequests = 4

// StartAt은 컨트롤러가 정할 시작 시각이다. open_at이 있으면 본 시험이 그 시각에 시작하도록 거꾸로 잡는다.
func (p *Plan) StartAt(cfg *config.Config, now time.Time) time.Time {
	if p.OpenAt != "" {
		t, _ := time.Parse(time.RFC3339, p.OpenAt)
		return t.Add(-time.Duration(p.MainStart) * time.Second)
	}
	return now.Add(cfg.Runner.StartDelay.D()).Truncate(time.Second)
}

// phase는 부하 계산을 위한 구간 하나다. rates는 흐름별 초당 시작 수다.
type phase struct {
	rates  map[string]float64
	extraQ float64 // 대기열 확인처럼 따로 더하는 대기열 경로 요청
	extraW float64 // 따로 더하는 WAF 요청(음수면 뺀다)
	prep   bool    // 준비 구간. 몇 초뿐이라 흐름별 최대치와 WAF 5분 계산에서는 뺀다
}

// New는 계획을 계산한다. 거절할 이유가 있으면 모두 모아 돌려준다.
func New(cfg *config.Config, in Input) (*Plan, error) {
	var errs []string
	fail := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	sc, ok := cfg.Scenarios[in.Scenario]
	if !ok {
		return nil, fmt.Errorf("시나리오 %q를 모른다(%s 중 하나)", in.Scenario, strings.Join(config.ScenarioOrder, ", "))
	}
	if !testIDRe.MatchString(in.TestID) || len(in.TestID) > MaxTestIDLen {
		fail("시험 ID %q는 lt-<yyyymmddThhmm>-<번호> 형식이고 %d자 이하여야 한다", in.TestID, MaxTestIDLen)
	}

	// 흐름. rush는 흐름이 정해져 있다. 몰린 회원 한 명이 진입부터 결제 시도까지 가므로 진입과 주문을 같이 센다
	rush := sc.Shape == config.ShapeRush
	queue := sc.Shape == config.ShapeQueue
	flows := in.Flows
	if len(flows) == 0 {
		flows = config.FlowOrder
	}
	if queue {
		if len(in.Flows) > 0 {
			fail("queue에는 flows를 쓰지 않는다(진입 흐름만 돈다)")
		}
		flows = []string{"entry"}
	}
	if rush {
		if len(in.Flows) > 0 {
			fail("rush에는 flows를 쓰지 않는다(배경 상품 조회, 진입, 주문이 정해져 있다)")
		}
		flows = []string{"entry"}
		for _, f := range config.FlowOrder {
			if sc.Background[f] > 0 {
				flows = append(flows, f)
			}
		}
		if sc.Rush.PayRatio > 0 {
			flows = append(flows, "order")
		}
	}
	flows = slices.Clone(flows)
	for _, f := range flows {
		if !slices.Contains(config.FlowOrder, f) {
			fail("흐름 %q를 모른다(%s 중에서 고른다)", f, strings.Join(config.FlowOrder, ", "))
		}
	}
	slices.SortFunc(flows, func(a, b string) int {
		return slices.Index(config.FlowOrder, a) - slices.Index(config.FlowOrder, b)
	})
	flows = slices.Compact(flows)
	has := func(f string) bool { return slices.Contains(flows, f) }
	usesPool := func(p config.Pool) bool {
		// rush의 주문은 몰린 회원(풀 A)이 한다
		if (rush || queue) && p != config.PoolA {
			return false
		}
		for _, f := range flows {
			if cfg.Flows[f].Pool == p {
				return true
			}
		}
		return false
	}

	// 값
	peak := in.PeakRPS
	if peak == 0 {
		peak = sc.PeakRPS
	}
	if !(peak > 0) || peak > sc.MaxRPS {
		fail("peak_rps %v는 0보다 크고 %s 상한 %.0f 이하여야 한다", peak, in.Scenario, sc.MaxRPS)
	}
	dur, err := durationOr(in.Duration, sc.Duration.D())
	if err != nil {
		fail("duration: %v", err)
	}
	ramp, err := durationOr(in.RampUp, sc.RampUp.D())
	if err != nil {
		fail("ramp_up: %v", err)
	}
	// 이 시나리오에서 쓰지 않는 입력은 말없이 무시하지 않고 거절한다
	if in.Duration != "" && sc.Shape == config.ShapeStep {
		fail("stress에는 duration을 쓰지 않는다. 단계 시간은 scenarios.yml의 step_duration이다")
	}
	if in.RampUp != "" && sc.Shape != config.ShapeRamping {
		fail("%s에는 ramp_up을 쓰지 않는다(올리는 구간이 있는 시나리오는 load뿐이다)", in.Scenario)
	}
	members := in.Members
	if in.Members != 0 && !queue {
		fail("%s에는 members를 쓰지 않는다(인원을 정하는 시나리오는 queue뿐이다)", in.Scenario)
	}
	if queue {
		if members == 0 {
			members = sc.Members
		}
		if members < 1 || members > sc.MaxMembers {
			fail("members %d는 1 이상 %d 이하여야 한다", members, sc.MaxMembers)
		}
	}

	// 상품
	prep := in.PrepProductID
	if prep == "" {
		prep = in.ProductID
	}
	for _, id := range []struct{ name, v string }{{"product_id", in.ProductID}, {"prep_product_id", in.PrepProductID}} {
		if id.v != "" && !uuidRe.MatchString(id.v) {
			hint := ""
			if isFlowList(cfg, id.v) {
				hint = ". 흐름 이름이면 flows 칸에 넣는다"
			}
			fail("%s %q는 소문자 UUID여야 한다(대문자가 섞이면 서버가 400을 준다)%s", id.name, id.v, hint)
		}
	}
	if has("entry") && in.ProductID == "" {
		fail("진입 흐름에는 product_id가 필요하다")
	}
	needPrep := usesPool(config.PoolB) || usesPool(config.PoolC)
	if needPrep && prep == "" {
		fail("조회, 결제, 취소 흐름에는 prep_product_id(또는 product_id)가 필요하다")
	}

	if rush && in.OpenAt == "" {
		fail("rush에는 open_at이 필요하다")
	}
	if (rush || queue) && in.PrepProductID != "" {
		fail("%s에는 prep_product_id를 쓰지 않는다(준비 구간이 없다)", in.Scenario)
	}

	// spike
	if sc.Shape == config.ShapeSpike {
		if !has("entry") {
			fail("spike에는 진입 흐름이 있어야 한다")
		}
		// open_at 이 없으면 이미 열린 상품으로 보고 준비 구간이 끝나는 대로 몰림을 시작한다. 그때는 준비 구간도
		// 같은 상품에 접수할 수 있다(준비 회원은 풀 B, C 라 몰림의 풀 A 와 겹치지 않는다)
		if needPrep && in.OpenAt != "" && (in.PrepProductID == "" || in.PrepProductID == in.ProductID) {
			fail("open_at을 넣은 spike에는 product_id와 다른 prep_product_id가 필요하다(진입 상품은 아직 열리지 않아 준비 구간이 SALE_NOT_OPEN으로 막힌다). " +
				"이미 열린 상품이면 open_at을 비운다")
		}
	}
	var openAt time.Time
	if in.OpenAt != "" {
		openAt, err = time.Parse(time.RFC3339, in.OpenAt)
		if err != nil {
			fail("open_at %q는 RFC 3339 시각이어야 한다(예: 2026-10-20T06:30:00Z)", in.OpenAt)
		} else if lead := cfg.Runner.MaxOpenAtLead.D(); openAt.After(in.Now.Add(lead)) {
			// 기다리는 동안에도 작업 시간과 토큰 유효 시간이 흐른다. 그 상한을 정하려고 대기를 묶는다
			fail("open_at은 지금부터 %s 안이어야 한다(작업 6시간 상한과 토큰 유효 시간에 대기가 들어간다)", lead)
		}
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}

	p := &Plan{
		TestID:    in.TestID,
		Scenario:  in.Scenario,
		Flows:     flows,
		ProductID: in.ProductID,
		WAFExempt: sc.WAFExempt,
	}
	if needPrep {
		p.PrepProductID = prep
	}
	if has("catalog") {
		c := cfg.CatalogPaths
		p.Catalog = &CatalogPaths{HomeRows: c.HomeRows, Category: c.Category, Search: c.Search, Preorder: c.Preorder, NextPage: c.NextPage,
			MaxPages: c.MaxPages, Click: c.Click}
	}
	if !openAt.IsZero() {
		p.OpenAt = openAt.UTC().Format(time.RFC3339)
	}

	// 흐름별 평시 비율. 고른 흐름만으로 합계를 다시 맞춘다
	var sumW float64
	for _, f := range flows {
		sumW += cfg.Flows[f].Weight
	}
	// 도착률은 k6가 실제로 쓰는 값(1분당 정수 반복)으로 맞춘다. 그래야 회원 수가 k6의 반복 수와 같아진다
	base := func(mult float64) map[string]float64 {
		r := map[string]float64{}
		for _, f := range flows {
			r[f] = quant(cfg.Flows[f].Weight * mult)
		}
		return r
	}
	capEntry := func(r map[string]float64, label string) {
		if r["entry"] > cfg.EntryCapRPS {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s: 진입 %.1f/초를 상한 %.0f/초로 묶었다(입장 속도 %.0f/초의 80%%)",
				label, r["entry"], cfg.EntryCapRPS, cfg.AdmissionRPS))
			r["entry"] = quant(cfg.EntryCapRPS)
		}
	}

	// 준비 구간과 등록 대기
	var poolB, poolC int64
	if usesPool(config.PoolB) {
		poolB = int64(sc.PoolB)
	}
	prepS, waitS := 0, 0

	var phases []phase
	var windows []Window
	var scenarios []K6Scenario
	var mainLen int // 본 시험 길이(초)
	var entryTotal, cancelTotal float64
	var vuHint string // VU 상한을 넘을 때 덧붙이는 안내

	addConstant := func(name, flow string, rps float64, start, length int, vus float64) {
		if rps <= 0 || length <= 0 {
			return
		}
		scenarios = append(scenarios, K6Scenario{
			Name: name, Flow: flow, Executor: "constant-arrival-rate",
			StartTime: secs(start), TimeUnit: "1m", Rate: perMinute(rps), Duration: secs(length),
		})
		setVUs(&scenarios[len(scenarios)-1], vus, cfg.Runner.VUBurstFactor)
	}

	// 시나리오 모양별 계산. 본 시험은 mainStart(준비 + 등록 대기 뒤)에 시작한다.
	// 준비 구간 길이는 풀 C 크기에 달려 있으므로, 먼저 흐름을 계산하고 나서 시작 시각을 밀어 준다.
	var build func(mainStart int)

	switch sc.Shape {
	case config.ShapeConstant, config.ShapeRamping:
		p.Multiplier = peak / sumW
		r := base(p.Multiplier)
		capEntry(r, in.Scenario)
		rampS := 0
		if sc.Shape == config.ShapeRamping {
			rampS = int(ramp.Seconds())
		}
		holdS := int(dur.Seconds())
		mainLen = rampS + holdS
		phases = append(phases, phase{rates: r})
		for _, f := range flows {
			// 올리는 구간은 절반만큼 센다
			n := r[f] * (float64(holdS) + float64(rampS)/2)
			switch cfg.Flows[f].Pool {
			case config.PoolA:
				entryTotal += n
			case config.PoolC:
				cancelTotal += n
			}
		}
		build = func(ms int) {
			for _, f := range flows {
				vus := r[f] * cfg.Flows[f].IterationSeconds
				if sc.Shape == config.ShapeConstant {
					addConstant(f, f, r[f], ms, holdS, vus)
					continue
				}
				scenarios = append(scenarios, K6Scenario{
					Name: f, Flow: f, Executor: "ramping-arrival-rate",
					StartTime: secs(ms), TimeUnit: "1m",
					Stages: []Stage{{perMinute(r[f]), secs(rampS)}, {perMinute(r[f]), secs(holdS)}},
				})
				setVUs(&scenarios[len(scenarios)-1], vus, cfg.Runner.VUBurstFactor)
			}
			windows = append(windows, Window{"hold", ms + rampS, ms + rampS + holdS})
		}

	case config.ShapeStep:
		last := sc.Steps[len(sc.Steps)-1]
		p.Multiplier = peak / (sumW * last) // 평시 비율의 몇 배를 1배로 볼지
		stepS := int(sc.StepDuration.D().Seconds())
		const stepRamp = config.StepRampSeconds
		mainLen = stepS * len(sc.Steps)
		stepRates := make([]map[string]float64, len(sc.Steps))
		for i, m := range sc.Steps {
			r := base(p.Multiplier * m)
			capEntry(r, fmt.Sprintf("stress 단계 %d", i+1))
			stepRates[i] = r
			phases = append(phases, phase{rates: r})
			for _, f := range flows {
				n := r[f] * float64(stepS)
				switch cfg.Flows[f].Pool {
				case config.PoolA:
					entryTotal += n
				case config.PoolC:
					cancelTotal += n
				}
			}
		}
		build = func(ms int) {
			for _, f := range flows {
				var stages []Stage
				var maxRate float64
				for i := range sc.Steps {
					rt := stepRates[i][f]
					maxRate = max(maxRate, rt)
					stages = append(stages, Stage{perMinute(rt), secs(stepRamp)}, Stage{perMinute(rt), secs(stepS - stepRamp)})
				}
				scenarios = append(scenarios, K6Scenario{
					Name: f, Flow: f, Executor: "ramping-arrival-rate",
					StartTime: secs(ms), TimeUnit: "1m", Stages: stages,
				})
				setVUs(&scenarios[len(scenarios)-1], maxRate*cfg.Flows[f].IterationSeconds, cfg.Runner.VUBurstFactor)
			}
			for i := range sc.Steps {
				s := ms + i*stepS
				windows = append(windows, Window{fmt.Sprintf("step%d", i+1), s + stepRamp, s + stepS})
			}
		}

	case config.ShapeSpike:
		p.Multiplier = 1
		burstS := int(sc.BurstSeconds)
		observeS := int(dur.Seconds())
		mainLen = 1 + burstS + observeS // 몰림은 open_at + 1초에 시작한다(시계 어긋남)
		bg := map[string]float64{}
		for _, f := range flows {
			if v, ok := sc.Background[f]; ok && v > 0 {
				bg[f] = quant(v)
			}
		}
		peak = quant(peak)
		observeRPS := 0.0
		if sc.ObserveEntryRPS > 0 {
			observeRPS = quant(sc.ObserveEntryRPS)
		}
		// 몰림 동안 줄에 서는 회원과 그 확인 요청
		waiting := math.Max(0, (peak-cfg.AdmissionRPS)*float64(burstS))
		burst := phase{rates: clone(bg), extraQ: waiting / cfg.PollIntervalSeconds}
		burst.rates["entry"] = peak
		// 입장한 뒤의 요청(상품 상세, 접수, 등록 확인)은 입장 속도를 넘지 않는다. rates의 진입(peak)으로 센 만큼을 줄인다
		burst.extraW = (math.Min(peak, cfg.AdmissionRPS) - peak) * cfg.Flows["entry"].AdmittedRequests
		observe := phase{rates: clone(bg)}
		observe.rates["entry"] = observeRPS
		phases = append(phases, burst, observe)
		// 몰림과 관찰이 풀 A를 나눠 쓰므로 시나리오마다 여유를 둔다(아래 poolSlack)
		burstN := peak*float64(burstS) + poolSlack(cfg)
		observeN := 0.0
		if observeRPS > 0 && observeS > 0 {
			observeN = observeRPS*float64(observeS) + poolSlack(cfg)
		}
		entryTotal = burstN + observeN
		// 배경은 본 시험 동안만 돈다(아래 build)
		cancelTotal = bg["cancel"] * float64(mainLen)
		build = func(ms int) {
			// 배경은 본 시험 시작(open_at)부터 끝까지 돈다. 등록 대기 중에 돌리면 주문과 취소가
			// 아직 REGISTERED가 아닌 예약을 찾게 된다
			bgStart := ms
			bgLen := mainLen
			for _, f := range flows {
				if f == "entry" {
					continue
				}
				addConstant(f, f, bg[f], bgStart, bgLen, bg[f]*cfg.Flows[f].IterationSeconds)
			}
			entryIter := cfg.Flows["entry"].IterationSeconds
			// 몰림의 회원은 입장할 때까지 VU를 하나씩 잡는다
			addConstant("entry_burst", "entry", peak, ms+1, burstS, peak*float64(burstS))
			// 몰림과 관찰이 풀 A를 앞뒤로 나눠 쓴다. 회원 한 명은 시험 하나에서 한 번만 진입한다
			split := fmt.Sprintf("%.6f", burstN/entryTotal)
			scenarios[len(scenarios)-1].Env = map[string]string{"POOL_FROM": "0", "POOL_TO": split}
			addConstant("entry_observe", "entry", observeRPS, ms+1+burstS, observeS, observeRPS*entryIter)
			if observeRPS > 0 && observeS > 0 {
				scenarios[len(scenarios)-1].Env = map[string]string{"POOL_FROM": split, "POOL_TO": "1"}
			}
			windows = append(windows, Window{"spike", ms, ms + mainLen})
		}

	case config.ShapeRush:
		p.Multiplier = 1
		burstS := int(sc.BurstSeconds)
		observeS := int(dur.Seconds())
		mainLen = 1 + burstS + observeS // 몰림은 open_at + 1초에 시작한다(spike와 같다)
		r := sc.Rush
		if dur <= r.NotifyTimeout.D() {
			fail("rush의 duration %s는 알림 대기 상한 %s보다 길어야 한다(결제가 관찰 시간 안에 끝나게)", dur, r.NotifyTimeout.D())
		}
		p.Rush = &Rush{PayRatio: r.PayRatio, NotifyIntervalSeconds: int(r.NotifyInterval.D().Seconds()),
			NotifyTimeoutSeconds: int(r.NotifyTimeout.D().Seconds())}
		bg := map[string]float64{}
		for _, f := range flows {
			if v := sc.Background[f]; v > 0 {
				bg[f] = quant(v)
			}
		}
		peak = quant(peak)
		admitted := math.Min(peak, cfg.AdmissionRPS)
		waiting := math.Max(0, (peak-cfg.AdmissionRPS)*float64(burstS))
		// 몰림: 진입 전 요청(사전예약 상세)은 몰린 만큼, 입장한 뒤의 요청은 입장 속도만큼이다
		burst := phase{rates: clone(bg), extraQ: waiting / cfg.PollIntervalSeconds}
		burst.rates["entry"] = peak
		burst.extraW = (admitted - peak) * cfg.Flows["entry"].AdmittedRequests
		if r.PayRatio > 0 {
			// 결제: 접수한 순서(입장 속도)대로 등록 알림을 받는다고 본다. 입장이 이어지는 동안 앞쪽 회원의 결제가
			// 시작되므로 몰림과 같은 구간에 더하고, 알림 수 확인은 결제할 회원이 모두 함께 기다리는 경우로 크게 잡는다
			payRate := quant(admitted * r.PayRatio)
			payers := peak * float64(burstS) * r.PayRatio
			burst.rates["order"] = payRate
			burst.extraW += payRate*(rushPayRequests-cfg.Flows["order"].Requests) + payers/r.NotifyInterval.D().Seconds()
		}
		phases = append(phases, burst)
		entryTotal = peak*float64(burstS) + poolSlack(cfg)
		build = func(ms int) {
			for _, f := range flows {
				if _, ok := bg[f]; ok {
					addConstant(f, f, bg[f], ms, mainLen, bg[f]*cfg.Flows[f].IterationSeconds)
				}
			}
			// 몰린 회원은 진입부터 결제 시도까지 VU를 하나씩 잡는다. 관찰 시간 끝까지 기다릴 수 있게 둔다
			addConstant("rush", "rush", peak, ms+1, burstS, peak*float64(burstS))
			windows = append(windows, Window{"rush", ms, ms + mainLen})
		}

	case config.ShapeQueue:
		p.Multiplier = 1
		e := cfg.Flows["entry"]
		n := float64(members)
		// 줄에 선 회원도 VU 를 하나씩 잡는다. peak_rps 를 비웠으면 기본값과 VU 상한 안에 드는 가장 큰 값 중 작은 쪽을 쓴다
		maxPeak, limited := queueMaxPeak(cfg, n, float64(sc.MaxVUs))
		lowered := false
		if in.PeakRPS == 0 && limited && maxPeak >= 1 && maxPeak < peak {
			peak, lowered = maxPeak, true
		}
		arrivalS, rate, waiting, inFlight := queueLoad(cfg, n, peak)
		admitted := math.Min(rate, cfg.AdmissionRPS)
		minS := int(math.Ceil(n/admitted + e.IterationSeconds))
		deadline := minS + int(sc.DeadlineMargin.D().Seconds())
		if in.Duration != "" {
			deadline = int(dur.Seconds())
			if deadline < minS {
				fail("queue의 duration %s는 최소 처리 시간 %s 이상이어야 한다(%d명을 입장 속도 %.0f/초로 들여보내고 접수까지)",
					dur, hms(minS), members, admitted)
			}
		}
		dur = time.Duration(deadline) * time.Second
		mainLen = 1 + deadline // 진입은 본 시험 시작(open_at) + 1초에 시작한다(spike, rush와 같다. 시계 어긋남)
		p.Queue = &Queue{Members: members, PeakRPS: peak, PeakLowered: lowered, ArrivalSeconds: arrivalS, MinSeconds: minS, DeadlineSeconds: deadline}
		if math.Ceil(inFlight) > float64(sc.MaxVUs) && limited && maxPeak >= 1 {
			vuHint = fmt.Sprintf(". 대기열은 초당 %.0f명만 입장시키므로 나머지는 줄에 서서 VU를 하나씩 잡는다. "+
				"%d명이면 peak_rps를 %.0f 이하로 두거나 비운다", cfg.AdmissionRPS, members, maxPeak)
		}
		ph := phase{rates: map[string]float64{"entry": rate}, extraQ: waiting / cfg.PollIntervalSeconds}
		ph.extraW = (admitted - rate) * e.AdmittedRequests
		phases = append(phases, ph)
		entryTotal = n
		build = func(ms int) {
			scenarios = append(scenarios, K6Scenario{
				Name: "queue", Flow: "entry", Executor: "constant-arrival-rate",
				StartTime: secs(ms + 1), TimeUnit: secs(arrivalS), Rate: members, Duration: secs(arrivalS),
				// 줄에 선 회원은 처리 기한까지 기다린다
				GracefulStop: secs(deadline - arrivalS),
			})
			// 예상 동시 인원은 입장이 초당 150명으로 이어질 때다. 입장이 늦어지면 회원이 줄에 더 남으므로 VU 는 인원
			// 전체(상한 안)까지 늘 수 있게 둔다. 모자라면 k6 가 회원을 들여보내지 못한다(운영에서 입장이 초당 약 1명일 때
			// 7,750개로 잡은 VU 가 모자라 2,124명이 시작하지 못했다).
			// VU 는 모두 미리 만든다. 회원 한 명이 반복 한 번이라 인원보다 많이 쓰지 않는다. 시험 중에 VU 를 늘리면 만드는
			// 동안 도착한 회원을 k6 가 버린다(4,250개를 미리 만들고 5,000개까지 늘리게 두었더니 매번 217~353명을 버렸다)
			q := &scenarios[len(scenarios)-1]
			setVUs(q, math.Max(inFlight, math.Min(n, float64(sc.MaxVUs))), 1)
			windows = append(windows, Window{"queue", ms, ms + 1 + minS})
		}
	}

	// 풀 A(spike는 위에서 시나리오마다 더했다)와 풀 C에도 여유를 둔다
	if sc.Shape != config.ShapeSpike && sc.Shape != config.ShapeRush {
		entryTotal += poolSlack(cfg)
	}
	if usesPool(config.PoolC) {
		poolC = int64(math.Ceil(cancelTotal + poolSlack(cfg)))
	}
	if n := poolB + poolC; n > 0 {
		prepS = int(math.Ceil(float64(n) / cfg.Members.PrepRPS))
		waitS = int(cfg.Members.RegisterWait.D().Seconds())
	}
	mainStart := prepS + waitS
	if prepS > 0 {
		prepRate := quant(math.Min(cfg.Members.PrepRPS, float64(poolB+poolC)/float64(prepS)))
		// 준비는 등록 확인(1초마다)을 기다리지 않는다
		e := cfg.Flows["entry"]
		addConstant("prepare", "prepare", prepRate, 0, prepS, prepRate*(e.IterationSeconds-e.RegisterRequests))
		phases = append(phases, phase{rates: map[string]float64{"entry": prepRate}, prep: true})
	}
	build(mainStart)
	p.Scenarios = scenarios
	p.Windows = windows
	p.MainStart = mainStart
	p.MainEnd = mainStart + mainLen

	// 진입 시나리오는 줄에 선 회원이 잘리지 않게 오래 기다린다
	graceful := int(cfg.Runner.EntryGracefulStop.D().Seconds())
	for i := range p.Scenarios {
		switch p.Scenarios[i].Flow {
		case "entry", "prepare":
			if queue {
				continue // 처리 기한으로 위에서 정했다
			}
			p.Scenarios[i].GracefulStop = secs(graceful)
		case "rush":
			// 몰림 10초 뒤에도 줄, 등록 알림, 결제가 관찰 시간 동안 이어진다
			p.Scenarios[i].GracefulStop = secs(p.MainEnd - p.MainStart - 1 - int(sc.BurstSeconds) + graceful)
		}
	}
	total := p.MainEnd
	// queue는 본 시험 끝이 처리 기한이다
	if (has("entry") || prepS > 0) && !queue {
		total += graceful
	}

	// 흐름별 최대 시작 수
	for _, f := range flows {
		var m float64
		for _, ph := range phases {
			if !ph.prep {
				m = max(m, ph.rates[f])
			}
		}
		p.FlowRates = append(p.FlowRates, FlowRate{f, round2(m)})
	}

	// 요청 수
	// 준비 구간은 태스크 대수(순간 요청)에는 넣고, 흐름 수와 WAF 5분 계산에서는 뺀다(접수 수는 따로 더한다)
	var peakFlow, peakReq, peakWAF float64
	for _, ph := range phases {
		var fl, rq, wf float64
		// 맵은 순회 순서가 매번 달라 부동소수점 합계의 끝자리가 바뀔 수 있다. 늘 같은 순서로 더한다
		for _, f := range config.FlowOrder {
			r := ph.rates[f]
			fl += r
			rq += r * (cfg.Flows[f].Requests + cfg.Flows[f].QueueRequests)
			wf += r * cfg.Flows[f].Requests
		}
		rq += ph.extraQ + ph.extraW
		wf += ph.extraW
		peakReq = max(peakReq, rq)
		if !ph.prep {
			peakFlow, peakWAF = max(peakFlow, fl), max(peakWAF, wf)
		}
	}

	// VU와 태스크 대수
	var vus int
	for _, s := range p.Scenarios {
		vus += s.MaxVUs
	}
	h := 1 + cfg.Runner.Headroom
	cpuTasks := int(math.Ceil(peakReq * h / cfg.Runner.RequestsPerTask))
	memTasks := int(math.Ceil(float64(vus) * cfg.Runner.VUMemoryMB * h / cfg.Runner.TaskMemoryMB))
	tasks := max(1, cpuTasks, memTasks)
	if in.Runners > 0 {
		if in.Runners < tasks {
			fail("runners %d가 계산한 대수 %d보다 적다(요청 기준 %d, 메모리 기준 %d)", in.Runners, tasks, cpuTasks, memTasks)
		}
		tasks = in.Runners
	}
	if tasks > cfg.Runner.MaxTasks {
		fail("태스크 %d대가 상한 max_tasks %d를 넘는다", tasks, cfg.Runner.MaxTasks)
	}
	p.Tasks = tasks

	if vus > sc.MaxVUs {
		fail("최대 VU %d가 %s 상한 %d를 넘는다%s", vus, in.Scenario, sc.MaxVUs, vuHint)
	}

	// WAF 예외 없이 도는 시나리오: 태스크(IP) 하나가 5분 동안 보내는 요청
	// 5분 창은 두 경우 중 큰 쪽이다. 본 시험 안의 5분, 또는 준비 구간 끝을 품는 5분(준비 요청 + 등록 대기 뒤에 남는
	// 본 시험 앞부분)이다. 준비는 등록 대기보다 앞에서 끝나므로 본 시험 5분 창에 통째로 더하지 않는다
	prepReq := float64(poolB+poolC) * (cfg.Flows["entry"].Requests - cfg.Flows["entry"].RegisterRequests)
	mainWin := peakWAF * math.Min(300, float64(mainLen))
	prepWin := prepReq + peakWAF*math.Max(0, math.Min(float64(mainLen), float64(300-waitS)))
	waf := math.Max(mainWin, prepWin) / float64(tasks)
	if !sc.WAFExempt && waf > cfg.WAFLimitPer5m {
		fail("태스크당 5분 요청 %.0f건이 WAF 예외 없는 상한 %.0f건을 넘는다", waf, cfg.WAFLimitPer5m)
	}

	// 시계열
	s := cfg.Series
	series := tasks * (s.Names*s.StatusesPerName*(s.Histogram+s.CountersPerName) + s.ExtraPerTask)
	if series > s.Limit {
		fail("예상 시계열 %d개가 상한 %d개를 넘는다", series, s.Limit)
	}

	// 컨트롤러가 시작한 뒤 START_AT까지 기다리는 최대 시간. open_at이 있으면 그 상한, 없으면 start_delay다.
	// 지금 시각에 기대지 않아야 러너와 컨트롤러의 계획이 같다
	lead := cfg.Runner.StartDelay.D()
	if p.OpenAt != "" {
		lead = cfg.Runner.MaxOpenAtLead.D()
	}

	// 회원
	if usesPool(config.PoolA) || poolB+poolC > 0 {
		m := cfg.Members
		var a int64
		if usesPool(config.PoolA) {
			a = int64(math.Ceil(entryTotal))
		}
		p.Members = &Members{
			B: Range{m.Start, poolB},
			C: Range{m.Start + poolB, poolC},
			A: Range{m.Start + poolB + poolC, a},
		}
		if t := p.Members.Total(); t > m.Count {
			fail("필요한 회원 %d명(풀 A %d, B %d, C %d)이 시험 회원 %d명보다 많다", t, a, poolB, poolC, m.Count)
		}
		// 토큰은 컨트롤러가 시작할 때 만든다. 시작 시각까지의 대기도 유효 시간에 넣는다
		ttl := lead + time.Duration(total)*time.Second + m.TokenMargin.D()
		if ttl > m.MaxTokenTTL.D() {
			fail("토큰 유효 시간 %s가 상한 %s를 넘는다", ttl, m.MaxTokenTTL.D())
		}
		p.TokenTTLSeconds = int(ttl.Seconds())
	}

	// 시간
	if dur > sc.MaxDuration.D() {
		if q := p.Queue; q != nil && in.Duration == "" {
			// duration 을 비우면 처리 기한은 계산한 값이다. 입력한 값처럼 보이지 않게 계산을 보여 준다
			fail("처리 기한 %s(최소 처리 시간 %s + 여유 %s)가 queue 상한 %s를 넘는다. 인원을 줄이거나, peak_rps가 입장 속도 %.0f보다 작으면 높인다",
				dur, hms(q.MinSeconds), sc.DeadlineMargin.D(), sc.MaxDuration.D(), cfg.AdmissionRPS)
		} else {
			fail("duration %s가 %s 상한 %s를 넘는다", dur, in.Scenario, sc.MaxDuration.D())
		}
	}
	if t := lead + time.Duration(total)*time.Second; t > cfg.Runner.JobLimit.D() {
		fail("예상 시간 %s가 GitHub Actions 작업 상한 %s를 넘는다", t, cfg.Runner.JobLimit.D())
	}
	if !openAt.IsZero() {
		earliest := in.Now.Add(cfg.Runner.StartDelay.D()).Add(time.Duration(mainStart) * time.Second)
		if openAt.Before(earliest) {
			fail("open_at %s까지 준비 구간을 돌릴 시간이 없다(가장 이른 시각 %s)",
				p.OpenAt, earliest.UTC().Truncate(time.Second).Format(time.RFC3339))
		}
	}

	p.Estimates = Estimates{
		PeakFlowRPS:     round2(peakFlow),
		PeakRequestRPS:  round2(peakReq),
		PeakWAFRPS:      round2(peakWAF),
		WAFPer5mPerTask: math.Round(waf),
		MaxVUs:          vus,
		MemoryMBPerTask: math.Round(float64(vus) * cfg.Runner.VUMemoryMB / float64(tasks)),
		Series:          series,
		TotalSeconds:    total,
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	return p, nil
}

// queueLoad 는 queue 에서 인원 n명을 초당 peak로 들여보낼 때의 도착 시간, 실제 진입 수, 진입이 끝날 때 줄에 선 회원,
// 동시 인원(줄 + 입장 뒤 접수와 등록 확인 중인 회원)이다. 회원 한 명이 VU 하나를 잡는다.
// 도착률을 "arrivalS초에 n번"으로 적어 k6 의 반복 수가 인원과 같다. 도착 시간은 올림해서 실제 진입 수가 peak 를
// 넘지 않게 한다(반올림하면 2,500명을 초당 2,000으로 넣을 때 1초에 2,500명이 들어가 max_rps 도 넘었다)
func queueLoad(cfg *config.Config, n, peak float64) (arrivalS int, rate, waiting, inFlight float64) {
	arrivalS = max(1, int(math.Ceil(n/peak)))
	rate = n / float64(arrivalS)
	waiting = math.Max(0, (rate-cfg.AdmissionRPS)*float64(arrivalS))
	inFlight = math.Min(n, waiting+math.Min(rate, cfg.AdmissionRPS)*cfg.Flows["entry"].IterationSeconds)
	return
}

// queueMaxPeak 는 n명의 동시 인원이 maxVUs 안에 드는 가장 큰 정수 peak 다. 어떤 peak 로도 넘지 않으면 limited 가 false 다.
// 줄 (r-A)·n/r 과 입장 뒤 A·iter 의 합이 상한 안인 r 은 A·n/(n-room) 이하다. 도착 시간을 올림하므로 실제 진입 수는
// 이 값 이하라 보통 바로 맞는다. 부동소수 오차에 대비해 계획과 같은 계산으로 확인하며 낮춘다
func queueMaxPeak(cfg *config.Config, n, maxVUs float64) (peak float64, limited bool) {
	room := maxVUs - cfg.AdmissionRPS*cfg.Flows["entry"].IterationSeconds
	if n <= room {
		return 0, false
	}
	if room <= 0 {
		return 0, true
	}
	for r := math.Floor(cfg.AdmissionRPS * n / (n - room)); r >= 1; r-- {
		if _, _, _, f := queueLoad(cfg, n, r); math.Ceil(f) <= maxVUs {
			return r, true
		}
	}
	return 0, true
}

// isFlowList 는 v 가 흐름 이름(쉼표로 여럿)인가. product_id 칸에 flows 값을 넣은 실수를 알려 주는 데 쓴다
func isFlowList(cfg *config.Config, v string) bool {
	for _, f := range strings.Split(v, ",") {
		if _, ok := cfg.Flows[strings.TrimSpace(f)]; !ok {
			return false
		}
	}
	return true
}

func durationOr(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	// k6 시나리오는 초 단위로 만든다. 1초 미만이나 초 단위가 아닌 값은 잘려서 시나리오가 사라질 수 있다
	if err != nil || d < time.Second || d%time.Second != 0 {
		return 0, fmt.Errorf("%q는 2m, 30m, 4h 같은 1초 이상, 초 단위의 시간이어야 한다", s)
	}
	return d, nil
}

// setVUs는 예상 VU로 preAllocatedVUs와 maxVUs를 정한다.
func setVUs(s *K6Scenario, est, factor float64) {
	s.PreAllocatedVUs = max(1, int(math.Ceil(est)))
	s.MaxVUs = max(s.PreAllocatedVUs, int(math.Ceil(est*factor)))
}

func perMinute(rps float64) int { return max(1, int(math.Round(rps*60))) }

// poolSlack은 한 번만 쓰는 풀(A, C)에 더하는 여유다. 토큰 파일은 mint.Split이 앞쪽 태스크에 하나씩 더 주는 방식으로
// 나누고, k6의 반복은 실행 구간(segment) 방식으로 나눈다. 두 방식이 달라 태스크마다 많아야 1명씩 모자랄 수 있으므로
// 태스크 상한만큼 더 둔다
func poolSlack(cfg *config.Config) float64 { return float64(cfg.Runner.MaxTasks) }

// quant는 초당 도착률을 k6가 쓰는 1분당 정수 반복으로 맞춘 값이다
func quant(rps float64) float64 { return float64(perMinute(rps)) / 60 }

func secs(s int) string { return fmt.Sprintf("%ds", s) }

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func clone(m map[string]float64) map[string]float64 {
	c := make(map[string]float64, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}
