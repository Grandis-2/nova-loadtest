package fakelocal

import (
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// flowNames 는 흐름마다 반드시 보내는 요청 이름이다(controller/internal/control 의 flowName 과 같다)
var flowNames = map[string]string{
	"catalog":   "/api/v1/products",
	"preorders": "/api/v1/preorders", // 진입의 접수(POST)와 같은 이름이다
	"entry":     "/api/v1/preorders/queue",
	"order":     "/api/v1/orders",
	"cancel":    "/api/v1/preorders/{id}/cancel",
}

var (
	testIDRe = regexp.MustCompile(`test_id="([^"]+)"`)
	rangeRe  = regexp.MustCompile(`\[(\d+)s\]`)
)

type sample struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

// serveMimir 는 컨트롤러가 보내는 쿼리(controller/internal/mimir)를 모양으로 알아보고 각본대로 답한다.
// PromQL 을 계산하지 않는다. 쿼리 모양이 바뀌면 여기도 바꾼다.
func (s *Server) serveMimir(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	ts, _ := strconv.ParseInt(r.URL.Query().Get("time"), 10, 64)
	at := time.Unix(ts, 0)
	sc := s.current()

	m := testIDRe.FindStringSubmatch(q)
	if m == nil {
		http.Error(w, "test_id 가 없는 쿼리", http.StatusBadRequest)
		return
	}
	testID := m[1]
	s.mu.Lock()
	start, ok := s.starts[testID]
	s.mu.Unlock()
	p := s.plan(testID)
	if !ok || p == nil {
		writeVector(w, at, nil) // 아직 k6 를 띄우기 전이다
		return
	}
	rel := at.Sub(start)
	if sc.MimirDown != nil && sc.MimirDown(rel) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	relSec := int(rel.Seconds())
	rng := 0
	if rm := rangeRe.FindStringSubmatch(q); rm != nil {
		rng, _ = strconv.Atoi(rm[1])
	}
	instance := s.Config.Metrics.InstanceLabel
	one := func(v float64) []sample {
		return []sample{{Metric: map[string]string{}, Value: [2]any{ts, fmtFloat(v)}}}
	}

	// window 는 쿼리 범위가 계획의 판정 구간과 같으면 그 순번이다(아니면 -1)
	window := -1
	for wi, w := range p.Windows {
		if w.End == relSec && w.End-w.Start == rng {
			window = wi
		}
	}
	errRate := func() float64 {
		if sc.ErrorRate != nil {
			return sc.ErrorRate(rel)
		}
		return 0.001
	}
	// 5xx 는 오류율을 바꾼 각본에서만 오류율과 같게 준다
	rate5xx := func() float64 {
		if sc.ErrorRate != nil {
			return sc.ErrorRate(rel)
		}
		return 0
	}

	var out []sample
	switch {
	case strings.HasPrefix(q, "histogram_quantile("):
		// 지연 백분위수. 0.5초 안 비율이 기준에 못 미치는 이름은 p95 가 0.5초를 넘게 준다
		qv, _ := strconv.ParseFloat(strings.TrimSuffix(strings.SplitN(strings.TrimPrefix(q, "histogram_quantile("), ",", 2)[0], " "), 64)
		lat := func(within float64) float64 {
			base := map[bool]float64{true: 300, false: 900}[within >= 0.95]
			return base * qv / 0.95
		}
		if !strings.Contains(q, "sum by (name, le)") {
			out = one(lat(sc.within("", window)))
			break
		}
		for _, name := range names(p.Flows) {
			out = append(out, sample{Metric: map[string]string{"name": name}, Value: [2]any{ts, fmtFloat(lat(sc.within(name, window)))}})
		}
	case strings.HasPrefix(q, "sum(rate(") && !strings.Contains(q, " / "):
		// 초당 요청(최근 1분)
		var v float64
		for _, fr := range p.FlowRates {
			v += fr.PeakRPS * s.Config.Flows[fr.Flow].Requests
		}
		out = one(v)
	case strings.HasPrefix(q, "sum(last_over_time("):
		out = one(float64(10 * p.Tasks))
	case strings.Contains(q, "time() - max by"):
		// 태스크마다 마지막 표본이 몇 초 전인가. 시작 전이면 표본이 없다
		for i := range p.Tasks {
			if relSec < 10 {
				continue
			}
			age := 5
			if gap, since := sc.silentAt(i, relSec); gap {
				age = since + 5
			}
			out = append(out, sample{Metric: map[string]string{instance: strconv.Itoa(i)}, Value: [2]any{ts, fmtFloat(float64(age))}})
		}
	case strings.Contains(q, "count_over_time"):
		for i := range p.Tasks {
			n := sc.samples(i, relSec-rng, relSec)
			out = append(out, sample{Metric: map[string]string{instance: strconv.Itoa(i)}, Value: [2]any{ts, fmtFloat(float64(n))}})
		}
	case strings.Contains(q, "sum by (scenario)"):
		// 준비를 뺀 시나리오는 판정 구간 내내 요청을 보낸다
		for _, k := range p.Scenarios {
			if k.Flow != "prepare" {
				out = append(out, sample{Metric: map[string]string{"scenario": k.Name}, Value: [2]any{ts, fmtFloat(float64(rng))}})
			}
		}
	case strings.Contains(q, "sum by (outcome)"):
		// queue 는 인원이 모두 등록까지 간다
		if p.Queue != nil {
			out = append(out, sample{Metric: map[string]string{"outcome": "registered"}, Value: [2]any{ts, fmtFloat(float64(p.Queue.Members))}})
		}
	case strings.Contains(q, "sum(max_over_time("):
		out = nil // 시작하지 못한 반복이 없으면 시계열이 없다
	case strings.Contains(q, "sum by (name)"):
		rates := map[string]float64{}
		for _, fr := range p.FlowRates {
			// 요청 수는 흐름 시작 수 × 흐름 하나의 요청 수다. 판정의 가중 평균이 계획의 흐름 비율대로 나오게 한다
			// 이름이 같은 흐름(예약 조회와 진입)은 더한다
			rates[flowNames[fr.Flow]] += fr.PeakRPS * s.Config.Flows[fr.Flow].Requests
		}
		for _, name := range names(p.Flows) {
			v := 0.0
			switch {
			case strings.Contains(q, "_bucket"):
				v = sc.within(name, window)
			case strings.Contains(q, `status=~"5.."`):
				v = rate5xx()
			case strings.Contains(q, `expected_response="false"`):
				v = errRate()
			default:
				v = float64(rng) * rates[name]
			}
			out = append(out, sample{Metric: map[string]string{"name": name}, Value: [2]any{ts, fmtFloat(v)}})
		}
	case strings.Contains(q, `expected_response="false"`):
		out = one(errRate())
	case strings.Contains(q, `status=~"5.."`):
		out = one(rate5xx())
	default:
		http.Error(w, "fake-mimir 가 모르는 쿼리: "+q, http.StatusBadRequest)
		return
	}
	writeVector(w, at, out)
}

func names(flows []string) []string {
	var out []string
	for _, f := range flows {
		if n, ok := flowNames[f]; ok && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

func writeVector(w http.ResponseWriter, _ time.Time, out []sample) {
	if out == nil {
		out = []sample{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": out}})
}

func fmtFloat(v float64) string {
	if math.IsNaN(v) {
		return "NaN"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
