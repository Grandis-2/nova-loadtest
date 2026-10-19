// Package config는 scenarios.yml을 읽는다. 파일은 실행 파일에 들어가므로(go:embed),
// 토큰 발급기의 시험 회원 범위는 실행 중에 바꿀 수 없다.
package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
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

// Shape은 시나리오의 부하 모양이다.
type Shape string

const (
	ShapeConstant Shape = "constant"
	ShapeRamping  Shape = "ramping"
	ShapeSpike    Shape = "spike"
	ShapeStep     Shape = "step"
)

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
}

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
var ScenarioOrder = []string{"smoke", "load", "spike", "stress", "soak"}

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
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("scenarios.yml: %w", err)
	}
	return &c, nil
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
		if f.Weight <= 0 || f.Requests <= 0 || f.IterationSeconds <= 0 {
			bad("flows.%s: weight, requests, iteration_seconds는 0보다 커야 한다", name)
		}
		switch f.Pool {
		case PoolNone, PoolA, PoolB, PoolC:
		default:
			bad("flows.%s.pool %q는 none, a, b, c 중 하나여야 한다", name, f.Pool)
		}
	}
	if c.Flows["entry"].Pool != PoolA {
		bad("flows.entry는 풀 a를 써야 한다")
	}
	if c.AdmissionRPS <= 0 || c.EntryCapRPS <= 0 || c.EntryCapRPS > c.AdmissionRPS {
		bad("entry_cap_rps는 0보다 크고 admission_rps 이하여야 한다")
	}
	if c.PollIntervalSeconds <= 0 || c.WAFLimitPer5m <= 0 {
		bad("poll_interval_seconds, waf_limit_per_5m은 0보다 커야 한다")
	}

	if len(c.Scenarios) != len(ScenarioOrder) {
		bad("scenarios는 %v 다섯 개여야 한다", ScenarioOrder)
	}
	for _, name := range ScenarioOrder {
		s, ok := c.Scenarios[name]
		if !ok {
			bad("scenarios.%s가 없다", name)
			continue
		}
		if s.PeakRPS <= 0 || s.MaxRPS < s.PeakRPS || s.MaxVUs <= 0 || s.MaxDuration <= 0 || s.PoolB <= 0 {
			bad("scenarios.%s: peak_rps, max_rps(≥ peak_rps), max_vus, max_duration, pool_b를 확인한다", name)
		}
		switch s.Shape {
		case ShapeConstant, ShapeRamping:
			if s.Duration <= 0 {
				bad("scenarios.%s.duration이 없다", name)
			}
		case ShapeSpike:
			if s.BurstSeconds <= 0 || s.Duration <= 0 || s.ObserveEntryRPS < 0 {
				bad("scenarios.%s: burst_seconds, duration(관찰 시간)이 필요하다", name)
			}
			for f := range s.Background {
				if _, ok := c.Flows[f]; !ok || f == "entry" {
					bad("scenarios.%s.background.%s: 진입을 뺀 흐름 이름이어야 한다", name, f)
				}
			}
		case ShapeStep:
			if len(s.Steps) == 0 || s.StepDuration <= 0 {
				bad("scenarios.%s: steps, step_duration이 필요하다", name)
			}
		default:
			bad("scenarios.%s.shape %q를 모른다", name, s.Shape)
		}
	}

	r := c.Runner
	if r.RequestsPerTask <= 0 || r.VUMemoryMB <= 0 || r.TaskMemoryMB <= 0 || r.Headroom < 0 ||
		r.VUBurstFactor < 1 || r.MaxTasks <= 0 || r.StartDelay <= 0 || r.JobLimit <= 0 {
		bad("runner 값을 확인한다")
	}
	m := c.Members
	if m.Start <= 0 || m.Count <= 0 || m.Start+m.Count > 1_000_000_000_000 {
		bad("members.start, count: 회원 번호는 1 이상 12자리 이하여야 한다")
	}
	if m.PrepRPS <= 0 || m.MaxTokenTTL <= 0 {
		bad("members.prep_rps, max_token_ttl은 0보다 커야 한다")
	}
	s := c.Series
	if s.Limit <= 0 || s.Names <= 0 || s.StatusesPerName <= 0 || s.Histogram <= 0 {
		bad("series 값을 확인한다")
	}
	if c.SLO.P95MS <= 0 || c.SLO.AbortErrorRate <= 0 || c.SLO.AbortAfter <= 0 {
		bad("slo 값을 확인한다")
	}
	return errors.Join(errs...)
}
