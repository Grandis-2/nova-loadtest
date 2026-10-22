package fakelocal

import (
	"strings"
	"time"

	"github.com/Grandis-2/nova-loadtest/controller/internal/fakeecs"
)

// Families 는 SSM run-task 에 넣는 작업 정의 이름이다(nova-infra 와 같은 이름)
const (
	FamilyController = "nova-prod-loadtest-controller"
	FamilyMinter     = "nova-prod-loadtest-token-minter"
	FamilyK6         = "nova-prod-loadtest-k6"
	FamilyData       = "nova-prod-loadtest-data"
)

// Task 는 k6 태스크 하나의 각본 입력이다. 시각은 모두 그 시험의 START_AT 기준이다.
type Task struct {
	Index      int
	Start, End time.Time // START_AT, 계획한 끝 시각(START_AT + total_seconds)
}

// Scenario 는 각본 하나다. 비어 있는 항목은 정상으로 동작한다.
type Scenario struct {
	Name string

	// ECS
	K6     func(t Task) fakeecs.Behavior // 기본: 30초 뒤 RUNNING, 끝 시각에 0
	Minter func(now time.Time) fakeecs.Behavior
	Data   func(now time.Time) fakeecs.Behavior
	// OldObs 가 있으면 관측 서비스 태스크를 둘 둔다. 먼저 뜬 태스크(닿지 않는 IP)는 시험 도중에 사라진다
	OldObs bool

	// SSM
	WithData      bool   // run-task 에 tasks.data 를 넣는다(정리 각본)
	Observability string // 비어 있지 않으면 observability 값을 이것으로 바꾼다

	// Mimir. rel 은 START_AT 부터 지난 시간이다
	ErrorRate func(rel time.Duration) float64
	MimirDown func(rel time.Duration) bool
	// Gaps 는 태스크마다 지표가 비는 구간이다(START_AT 기준 [시작, 끝) 초)
	Gaps   map[int][][2]int
	Within func(name string, window int) float64 // 0.5초 안 비율(window 는 계획의 판정 구간 순번). 기본 0.99
}

// 각본 목록. 이름은 local/scenarios 실행기와 같다
var scenarios = map[string]Scenario{}

func add(s Scenario) { scenarios[s.Name] = s }

// running 은 멈출 때까지 도는 k6 다
func running(Task) fakeecs.Behavior {
	return fakeecs.Behavior{RunningAfter: 30 * time.Second, StopExitCode: 105, StopDelay: 5 * time.Second, ImageDigest: digest}
}

const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func init() {
	add(Scenario{Name: "pass"})
	// 결제(주문) 흐름의 0.5초 안 비율이 90% 다
	add(Scenario{Name: "fail", Within: func(name string, _ int) float64 {
		if name == "/api/v1/orders" {
			return 0.90
		}
		return 0.99
	}})
	// 70초부터 오류와 5xx 가 30% 다. 멈추지 않고 끝까지 돌아 불합격이다(NV-780)
	add(Scenario{Name: "high-error-rate", ErrorRate: func(rel time.Duration) float64 {
		if rel > 70*time.Second {
			return 0.30
		}
		return 0.001
	}})
	// 태스크 2가 도중에 99로 끝난다. 나머지는 끝까지 돌고 판정 불가다. runners 3
	add(Scenario{Name: "task-dies-early", K6: func(t Task) fakeecs.Behavior {
		b := running(t)
		b.ExitAt = t.End
		if t.Index == 2 {
			b.ExitAt, b.ExitCode = t.Start.Add(80*time.Second), 99
		}
		return b
	}})
	// 태스크 1의 지표가 30초부터 끊긴다. 멈추지 않고 끝까지 돌아 구간 누락으로 판정 불가다
	add(Scenario{Name: "inconclusive-silent", Gaps: map[int][][2]int{1: {{30, 1 << 20}}}})
	// 70초부터 80초 동안 Mimir 를 조회하지 못한다. 멈추지 않고, 돌아온 뒤 판정한다
	add(Scenario{Name: "mimir-down-recovers", MimirDown: func(rel time.Duration) bool { return rel > 70*time.Second && rel < 150*time.Second }})
	// 태스크 0의 지표가 smoke 의 판정 구간(121~241초) 안에서 50초씩 두 번 빈다. 감시(1분)에는 걸리지 않고 구간 판정에서 걸린다
	add(Scenario{Name: "inconclusive-gap", Gaps: map[int][][2]int{0: {{130, 180}, {185, 235}}}})
	// 태스크 3이 START_AT 까지 RUNNING 이 되지 않는다. runners 4
	add(Scenario{Name: "start-cancel", K6: func(t Task) fakeecs.Behavior {
		b := running(t)
		b.Never = t.Index == 3
		return b
	}})
	add(Scenario{Name: "minter-fail", Minter: func(now time.Time) fakeecs.Behavior {
		return fakeecs.Behavior{RunningAfter: 5 * time.Second, ExitAt: now.Add(20 * time.Second), ExitCode: 1}
	}})
	add(Scenario{Name: "bad-observability", Observability: `{"otlpEndpoint":"http://otel-ingest.nova.local:4318","mimir":{"cluster":"nova-prod-observability","service":"nova-prod-obs","port":9009,"queryPath":"/prometheus/api/v1/query"}}`})
	add(Scenario{Name: "image-mix", K6: func(t Task) fakeecs.Behavior {
		b := running(t)
		if t.Index == 1 {
			b.ImageDigest = strings.Replace(digest, "1111", "2222", 1)
		}
		return b
	}})
	add(Scenario{Name: "obs-redeploy", OldObs: true})
	add(Scenario{Name: "manual-stop", K6: running})
	add(Scenario{Name: "cancel", K6: running})
	add(Scenario{Name: "controller-dies", K6: running})
	add(Scenario{Name: "cleanup", WithData: true})
	add(Scenario{Name: "cleanup-refused"})
	add(Scenario{Name: "cleanup-other-test", WithData: true})
	add(Scenario{Name: "input-rejected"})
	// stress: 단계 3부터 0.5초 안 비율이 기준에 못 미친다
	add(Scenario{Name: "stress-limit", Within: func(_ string, window int) float64 {
		if window >= 2 {
			return 0.80
		}
		return 0.99
	}})
}

func (s Scenario) k6(t Task) fakeecs.Behavior {
	if s.K6 != nil {
		return s.K6(t)
	}
	b := running(t)
	b.ExitAt = t.End
	return b
}

func (s Scenario) minter(now time.Time) fakeecs.Behavior {
	if s.Minter != nil {
		return s.Minter(now)
	}
	return fakeecs.Behavior{RunningAfter: 5 * time.Second, ExitAt: now.Add(20 * time.Second)}
}

func (s Scenario) data(now time.Time) fakeecs.Behavior {
	if s.Data != nil {
		return s.Data(now)
	}
	return fakeecs.Behavior{RunningAfter: 5 * time.Second, ExitAt: now.Add(20 * time.Second)}
}

func (s Scenario) within(name string, window int) float64 {
	if s.Within != nil {
		return s.Within(name, window)
	}
	return 0.99
}

// silentAt 은 태스크 i 의 지표가 rel 에 비어 있는지, 비었다면 마지막 표본이 몇 초 전인지다
func (s Scenario) silentAt(i int, rel int) (bool, int) {
	for _, g := range s.Gaps[i] {
		if rel >= g[0] && rel < g[1] {
			return true, rel - g[0]
		}
	}
	return false, 0
}

// samples 는 태스크 i 가 [from, to) 초에 내보낸 표본 수다(10초마다)
func (s Scenario) samples(i, from, to int) int {
	n := 0
	for t := from - from%10; t < to; t += 10 {
		if t < from || t < 0 {
			continue
		}
		if gap, _ := s.silentAt(i, t); !gap {
			n++
		}
	}
	return n
}
