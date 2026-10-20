// Package config는 scenarios.yml을 읽는다. 파일은 실행 파일에 들어가므로(go:embed),
// 토큰 발급기의 시험 회원 범위는 실행 중에 바꿀 수 없다.
package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"go.yaml.in/yaml/v3"
)

//go:embed scenarios.yml
var embedded []byte

// Duration은 "2m", "4h" 같은 문자열로 적는 시간이다.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("줄 %d: 시간 %q: %w", n.Line, n.Value, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

// Pool은 회원 풀이다. none은 토큰이 필요 없는 흐름이다.
type Pool string

const (
	PoolNone Pool = "none"
	PoolA    Pool = "a" // 진입과 접수. 시험 하나에서 회원 한 명을 한 번만 쓴다
	PoolB    Pool = "b" // 조회와 결제. 준비 구간에서 예약을 만들어 둔다
	PoolC    Pool = "c" // 취소. 풀 B처럼 준비하지만 취소에만 쓴다
)

type Flow struct {
	Weight           float64 `yaml:"weight"`
	Pool             Pool    `yaml:"pool"`
	Requests         float64 `yaml:"requests"`
	QueueRequests    float64 `yaml:"queue_requests"`
	IterationSeconds float64 `yaml:"iteration_seconds"`
}

// CatalogPaths는 상품 조회 흐름이 화면을 옮겨 가는 비율이다(프론트 화면 기준, 비율은 추정).
// 흐름은 홈(인기 상품, 카테고리 트리, 대분류 줄)에서 시작해 카테고리 목록이나 검색으로 가고, 본 목록에서 상품을 누른다
type CatalogPaths struct {
	HomeRows int     `yaml:"home_rows"` // 홈의 대분류 줄 수. 줄마다 목록을 하나씩 부른다
	Category float64 `yaml:"category"`  // 카테고리 목록으로 가는 비율
	Search   float64 `yaml:"search"`    // 검색으로 가는 비율. 나머지는 홈에서 끝난다
	NextPage float64 `yaml:"next_page"` // 카테고리 목록에서 다음 쪽으로 넘어가는 비율
	MaxPages int     `yaml:"max_pages"` // 카테고리 목록에서 보는 최대 쪽 수
	Click    float64 `yaml:"click"`     // 마지막에 본 목록에서 상품을 누르는 비율. 누르면 상세와 배송 차수(사전예약) 또는 리뷰를 부른다
}

// Requests는 이 비율로 흐름 하나가 보내는 평균 요청 수다. 다음 쪽이 늘 있다고 보고 크게 잡는다
func (c CatalogPaths) Requests() float64 {
	pages, p := 1.0, 1.0
	for range c.MaxPages - 1 {
		p *= c.NextPage
		pages += p
	}
	return 2 + float64(c.HomeRows) + c.Category*pages + c.Search*2 + c.Click*2
}

// Shape은 시나리오의 부하 모양이다.
type Shape string

const (
	ShapeConstant Shape = "constant"
	ShapeRamping  Shape = "ramping"
	ShapeSpike    Shape = "spike"
	ShapeStep     Shape = "step"
	ShapeRush     Shape = "rush"
)

// Rush는 rush 시나리오에서 몰린 회원이 접수 뒤에 이어 가는 값이다
type Rush struct {
	PayRatio       float64  `yaml:"pay_ratio"`       // 등록 알림을 기다려 바로 결제하는 회원의 비율(0~1)
	NotifyInterval Duration `yaml:"notify_interval"` // 안 읽은 알림 수를 확인하는 간격
	NotifyTimeout  Duration `yaml:"notify_timeout"`  // 접수 뒤 알림을 기다리는 상한. 넘으면 결제를 건너뛴다
}

type Scenario struct {
	Shape           Shape              `yaml:"shape"`
	PeakRPS         float64            `yaml:"peak_rps"`
	Duration        Duration           `yaml:"duration"`
	RampUp          Duration           `yaml:"ramp_up"`
	BurstSeconds    float64            `yaml:"burst_seconds"`
	ObserveEntryRPS float64            `yaml:"observe_entry_rps"`
	Background      map[string]float64 `yaml:"background"`
	Steps           []float64          `yaml:"steps"`
	StepDuration    Duration           `yaml:"step_duration"`
	MaxRPS          float64            `yaml:"max_rps"`
	MaxDuration     Duration           `yaml:"max_duration"`
	MaxVUs          int                `yaml:"max_vus"`
	WAFExempt       bool               `yaml:"waf_exempt"`
	PoolB           int                `yaml:"pool_b"`
	Rush            Rush               `yaml:"rush"`
}

type Runner struct {
	RequestsPerTask   float64  `yaml:"requests_per_task"`
	VUMemoryMB        float64  `yaml:"vu_memory_mb"`
	TaskMemoryMB      float64  `yaml:"task_memory_mb"`
	Headroom          float64  `yaml:"headroom"`
	VUBurstFactor     float64  `yaml:"vu_burst_factor"`
	MaxTasks          int      `yaml:"max_tasks"`
	StartDelay        Duration `yaml:"start_delay"`
	EntryGracefulStop Duration `yaml:"entry_graceful_stop"`
	JobLimit          Duration `yaml:"job_limit"`
	// open_at은 지금부터 이 시간 안이어야 한다. 이 대기를 토큰 유효 시간과 작업 시간 계산에 넣는다
	MaxOpenAtLead Duration `yaml:"max_open_at_lead"`
}

// StepRampSeconds는 stress 단계 사이에서 다음 배수로 올리는 시간이다. 단계 시간은 이보다 길어야 한다
const StepRampSeconds = 10

type Members struct {
	Start        int64    `yaml:"start"`
	Count        int64    `yaml:"count"`
	PrepRPS      float64  `yaml:"prep_rps"`
	RegisterWait Duration `yaml:"register_wait"`
	TokenMargin  Duration `yaml:"token_margin"`
	MaxTokenTTL  Duration `yaml:"max_token_ttl"`
}

// Contains는 회원 번호 n이 시험 회원 범위 안인지 본다.
func (m Members) Contains(n int64) bool { return n >= m.Start && n < m.Start+m.Count }

type Series struct {
	Limit           int `yaml:"limit"`
	Names           int `yaml:"names"`
	StatusesPerName int `yaml:"statuses_per_name"`
	Histogram       int `yaml:"histogram"`
	CountersPerName int `yaml:"counters_per_name"`
	ExtraPerTask    int `yaml:"extra_per_task"`
}

type SLO struct {
	P95MS          float64  `yaml:"p95_ms"`
	Max5xxRate     float64  `yaml:"max_5xx_rate"`
	AbortErrorRate float64  `yaml:"abort_error_rate"`
	Abort5xxRate   float64  `yaml:"abort_5xx_rate"`
	AbortAfter     Duration `yaml:"abort_after"`
}

type Cleanup struct {
	Queues  []string `yaml:"queues"`
	MaxWait Duration `yaml:"max_wait"`
}

type Metrics struct {
	Job           string `yaml:"job"`
	InstanceLabel string `yaml:"instance_label"`
	Duration      string `yaml:"duration"`
	Reqs          string `yaml:"reqs"`
	VUs           string `yaml:"vus"`
	LE500         string `yaml:"le_500"`
}

type Config struct {
	Flows               map[string]Flow     `yaml:"flows"`
	CatalogPaths        CatalogPaths        `yaml:"catalog_paths"`
	AdmissionRPS        float64             `yaml:"admission_rps"`
	EntryCapRPS         float64             `yaml:"entry_cap_rps"`
	PollIntervalSeconds float64             `yaml:"poll_interval_seconds"`
	WAFLimitPer5m       float64             `yaml:"waf_limit_per_5m"`
	Scenarios           map[string]Scenario `yaml:"scenarios"`
	Runner              Runner              `yaml:"runner"`
	Members             Members             `yaml:"members"`
	Series              Series              `yaml:"series"`
	SLO                 SLO                 `yaml:"slo"`
	Cleanup             Cleanup             `yaml:"cleanup"`
	Metrics             Metrics             `yaml:"metrics"`
}

// FlowOrder는 흐름을 늘 같은 순서로 다루려고 쓴다. 계획이 결정적이어야 러너와 컨트롤러가 같은 값을 낸다.
var FlowOrder = []string{"catalog", "preorders", "entry", "order", "cancel"}

// ScenarioOrder는 시나리오 이름의 순서다.
var ScenarioOrder = []string{"smoke", "load", "spike", "rush", "stress", "soak"}

// Default는 실행 파일에 들어 있는 scenarios.yml이다.
func Default() (*Config, error) { return Parse(embedded) }

// Parse는 YAML을 읽고 검사한다. 모르는 키가 있으면 실패한다.
func Parse(b []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("scenarios.yml: %w", err)
	}
	// 문서가 하나 더 있으면(---) 그 내용이 말없이 무시되므로 거절한다
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("scenarios.yml: YAML 문서는 하나여야 한다(--- 뒤에 문서가 더 있다)")
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("scenarios.yml: %w", err)
	}
	return &c, nil
}

// pos는 0보다 크고 유한한 수인지 본다. NaN은 모든 비교가 거짓이라 v <= 0 으로는 걸러지지 않는다
func pos(vs ...float64) bool {
	for _, v := range vs {
		if !(v > 0) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// nonneg는 0 이상이고 유한한 수인지 본다
func nonneg(vs ...float64) bool {
	for _, v := range vs {
		if !(v >= 0) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// Validate는 계산에 쓰는 값이 빠지거나 틀리지 않았는지 본다.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if len(c.Flows) != len(FlowOrder) {
		bad("flows는 %v 다섯 개여야 한다", FlowOrder)
	}
	for _, name := range FlowOrder {
		f, ok := c.Flows[name]
		if !ok {
			bad("flows.%s가 없다", name)
			continue
		}
		if !pos(f.Weight, f.Requests, f.IterationSeconds) || !nonneg(f.QueueRequests) {
			bad("flows.%s: weight, requests, iteration_seconds는 0보다 크고 queue_requests는 0 이상이어야 한다", name)
		}
		switch f.Pool {
		case PoolNone, PoolA, PoolB, PoolC:
		default:
			bad("flows.%s.pool %q는 none, a, b, c 중 하나여야 한다", name, f.Pool)
		}
	}
	cp := c.CatalogPaths
	if cp.HomeRows < 0 || cp.MaxPages < 1 || !nonneg(cp.Category, cp.Search, cp.NextPage, cp.Click) ||
		cp.Category+cp.Search > 1 || cp.NextPage > 1 || cp.Click > 1 {
		bad("catalog_paths: 비율은 0~1(category + search ≤ 1), home_rows는 0 이상, max_pages는 1 이상이어야 한다")
	} else if want := cp.Requests(); math.Abs(c.Flows["catalog"].Requests-want) > 0.05 {
		// 계획의 요청 수 계산(태스크 대수, WAF)이 k6가 실제로 보내는 양과 어긋나지 않게 한다
		bad("flows.catalog.requests %.2f가 catalog_paths로 계산한 %.2f와 다르다", c.Flows["catalog"].Requests, want)
	}
	if c.Flows["entry"].Pool != PoolA {
		bad("flows.entry는 풀 a를 써야 한다")
	}
	if !pos(c.AdmissionRPS, c.EntryCapRPS) || c.EntryCapRPS > c.AdmissionRPS {
		bad("entry_cap_rps는 0보다 크고 admission_rps 이하여야 한다")
	}
	if !pos(c.PollIntervalSeconds, c.WAFLimitPer5m) {
		bad("poll_interval_seconds, waf_limit_per_5m은 0보다 커야 한다")
	}

	if len(c.Scenarios) != len(ScenarioOrder) {
		bad("scenarios는 %v %d개여야 한다", ScenarioOrder, len(ScenarioOrder))
	}
	for _, name := range ScenarioOrder {
		s, ok := c.Scenarios[name]
		if !ok {
			bad("scenarios.%s가 없다", name)
			continue
		}
		// rush는 준비 구간이 없어 pool_b를 쓰지 않는다
		if !pos(s.PeakRPS, s.MaxRPS) || s.MaxRPS < s.PeakRPS || s.MaxVUs <= 0 || s.MaxDuration <= 0 ||
			(s.PoolB <= 0 && s.Shape != ShapeRush) {
			bad("scenarios.%s: peak_rps, max_rps(≥ peak_rps), max_vus, max_duration, pool_b를 확인한다", name)
		}
		switch s.Shape {
		case ShapeConstant, ShapeRamping:
			if s.Duration < Duration(time.Second) {
				bad("scenarios.%s.duration은 1초 이상이어야 한다", name)
			}
			if s.RampUp < 0 || (s.Shape == ShapeRamping && s.RampUp < Duration(time.Second)) {
				bad("scenarios.%s.ramp_up은 음수일 수 없고, ramping이면 1초 이상이어야 한다", name)
			}
		case ShapeSpike, ShapeRush:
			if s.Shape == ShapeRush {
				r := s.Rush
				if !(r.PayRatio >= 0 && r.PayRatio <= 1) || r.NotifyInterval < Duration(time.Second) ||
					r.NotifyTimeout < r.NotifyInterval || s.Duration <= r.NotifyTimeout || s.ObserveEntryRPS != 0 {
					bad("scenarios.%s.rush: pay_ratio는 0~1, notify_interval은 1초 이상, notify_timeout은 그 이상이고 "+
						"duration(관찰 시간)보다 짧아야 한다. observe_entry_rps는 쓰지 않는다", name)
				}
			}
			if !pos(s.BurstSeconds) || s.Duration < Duration(time.Second) || !nonneg(s.ObserveEntryRPS) {
				bad("scenarios.%s: burst_seconds, duration(관찰 시간 1초 이상)이 필요하고 observe_entry_rps는 0 이상이어야 한다", name)
			}
			for f, v := range s.Background {
				if _, ok := c.Flows[f]; !ok || f == "entry" {
					bad("scenarios.%s.background.%s: 진입을 뺀 흐름 이름이어야 한다", name, f)
				}
				// rush는 준비 구간이 없어 예약이 필요한 배경(풀 B, C)을 돌릴 수 없다
				if s.Shape == ShapeRush && c.Flows[f].Pool != PoolNone {
					bad("scenarios.%s.background.%s: rush의 배경은 회원이 필요 없는 흐름이어야 한다", name, f)
				}
				if !nonneg(v) {
					bad("scenarios.%s.background.%s: 0 이상이어야 한다", name, f)
				}
			}
		case ShapeStep:
			if len(s.Steps) == 0 || s.StepDuration <= Duration(StepRampSeconds*time.Second) {
				bad("scenarios.%s: steps가 필요하고 step_duration은 %d초보다 길어야 한다", name, StepRampSeconds)
			}
			for i, m := range s.Steps {
				if !pos(m) {
					bad("scenarios.%s.steps[%d]: 배수는 0보다 커야 한다", name, i)
				}
			}
		default:
			bad("scenarios.%s.shape %q를 모른다", name, s.Shape)
		}
	}

	r := c.Runner
	if !pos(r.RequestsPerTask, r.VUMemoryMB, r.TaskMemoryMB) || !nonneg(r.Headroom) || !(r.VUBurstFactor >= 1) ||
		math.IsInf(r.VUBurstFactor, 0) || r.MaxTasks <= 0 || r.StartDelay <= 0 || r.JobLimit <= 0 ||
		r.EntryGracefulStop < Duration(time.Second) || r.MaxOpenAtLead <= 0 {
		bad("runner 값을 확인한다(entry_graceful_stop은 1초 이상, max_open_at_lead는 0보다 커야 한다)")
	}
	m := c.Members
	// 더하기 전에 비교한다. 큰 두 수를 더하면 int64가 넘쳐 검사를 통과할 수 있다
	const maxMember = 1_000_000_000_000 // 회원 번호는 12자리
	if m.Start <= 0 || m.Count <= 0 || m.Start >= maxMember || m.Count > maxMember-m.Start {
		bad("members.start, count: 회원 번호는 1 이상 12자리 이하여야 한다")
	}
	if !pos(m.PrepRPS) || m.MaxTokenTTL <= 0 || m.RegisterWait < 0 || m.TokenMargin < 0 {
		bad("members.prep_rps, max_token_ttl은 0보다 커야 한다")
	}
	s := c.Series
	if s.Limit <= 0 || s.Names <= 0 || s.StatusesPerName <= 0 || s.Histogram <= 0 || s.CountersPerName < 0 || s.ExtraPerTask < 0 {
		bad("series 값을 확인한다")
	}
	if !pos(c.SLO.P95MS, c.SLO.AbortErrorRate, c.SLO.Abort5xxRate) || !nonneg(c.SLO.Max5xxRate) || c.SLO.AbortAfter <= 0 {
		bad("slo 값을 확인한다")
	}
	return errors.Join(errs...)
}
