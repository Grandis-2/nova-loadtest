// Package mimir는 Mimir에 PromQL을 보내 감시와 판정에 쓰는 값을 계산한다(docs/implementation-plan.md 2.4 "판정 쿼리").
//
// 지표 이름, job, 태스크 라벨, le 값은 scenarios.yml의 metrics에서 읽는다. smoke(C-2)에서 확정한 값으로 고친다.
// 모든 쿼리는 즉시 쿼리(/api/v1/query)다. 지난 구간은 time 인자에 구간 끝을 주고 범위를 구간 길이로 잡는다.
package mimir

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/Grandis-2/nova-loadtest/config"
)

// Sample은 즉시 쿼리 결과의 한 줄이다.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Querier는 PromQL 즉시 쿼리를 보낸다.
type Querier interface {
	Query(ctx context.Context, promql string, at time.Time) ([]Sample, error)
}

// HTTP는 Mimir의 Prometheus 호환 API로 조회한다. 조회가 실패하면 IP를 다시 찾고 한 번 더 시도한다.
type HTTP struct {
	Discover func(ctx context.Context) (string, error) // obs 태스크의 사설 IP
	Port     int
	Path     string
	Client   *http.Client

	mu sync.Mutex
	ip string
}

func (h *HTTP) Query(ctx context.Context, promql string, at time.Time) ([]Sample, error) {
	out, err := h.query(ctx, promql, at, false)
	if err == nil {
		return out, nil
	}
	// obs가 다시 떠 IP가 바뀌었을 수 있다
	return h.query(ctx, promql, at, true)
}

func (h *HTTP) query(ctx context.Context, promql string, at time.Time, rediscover bool) ([]Sample, error) {
	h.mu.Lock()
	ip := h.ip
	h.mu.Unlock()
	if ip == "" || rediscover {
		var err error
		if ip, err = h.Discover(ctx); err != nil {
			return nil, err
		}
		h.mu.Lock()
		h.ip = ip
		h.mu.Unlock()
	}
	q := url.Values{"query": {promql}, "time": {strconv.FormatInt(at.Unix(), 10)}}
	u := fmt.Sprintf("http://%s:%d%s?%s", ip, h.Port, h.Path, q.Encode())
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Mimir 응답 %d", resp.StatusCode)
	}
	return decode(resp)
}

func decode(resp *http.Response) ([]Sample, error) {
	var body struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("Mimir 응답을 읽지 못했다: %w", err)
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("Mimir 오류: %s", body.Error)
	}
	if body.Data.ResultType != "vector" {
		return nil, fmt.Errorf("Mimir 결과 형식 %q(vector 여야 한다)", body.Data.ResultType)
	}
	out := make([]Sample, 0, len(body.Data.Result))
	for _, r := range body.Data.Result {
		s, _ := r.Value[1].(string)
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("Mimir 값 %q: %w", s, err)
		}
		out = append(out, Sample{Labels: r.Metric, Value: v})
	}
	return out, nil
}

// Metrics는 시험 하나의 지표를 조회한다.
type Metrics struct {
	Q      Querier
	Names  config.Metrics
	TestID string
}

func (m Metrics) sel(extra string) string {
	s := fmt.Sprintf(`test_id=%q,job=%q`, m.TestID, m.Names.Job)
	if extra != "" {
		s += "," + extra
	}
	return s
}

func rng(d time.Duration) string { return fmt.Sprintf("%ds", int(d.Seconds())) }

// scalar는 결과가 한 줄인 쿼리의 값이다. 결과가 없으면(요청이 없었다) NaN이다
func (m Metrics) scalar(ctx context.Context, q string, at time.Time) (float64, error) {
	out, err := m.Q.Query(ctx, q, at)
	if err != nil {
		return 0, err
	}
	if len(out) == 0 {
		return math.NaN(), nil
	}
	return out[0].Value, nil
}

// ErrorRate는 최근 1분의 오류율이다. k6의 http_req_failed와 같은 정의다(expected_response="false"인 요청의 비율).
func (m Metrics) ErrorRate(ctx context.Context, at time.Time) (float64, error) {
	r := m.Names.Reqs
	q := fmt.Sprintf(`sum(rate(%s{%s}[1m])) / sum(rate(%s{%s}[1m]))`, r, m.sel(`expected_response="false"`), r, m.sel(""))
	return m.scalar(ctx, q, at)
}

// Rate5xx는 최근 1분의 5xx 비율이다.
func (m Metrics) Rate5xx(ctx context.Context, at time.Time) (float64, error) {
	r := m.Names.Reqs
	q := fmt.Sprintf(`sum(rate(%s{%s}[1m])) / sum(rate(%s{%s}[1m]))`, r, m.sel(`status=~"5.."`), r, m.sel(""))
	return m.scalar(ctx, q, at)
}

// LastSeen은 태스크(instance)마다 vus 지표의 마지막 시각에서 지난 초를 돌려준다.
// 요청 지표는 흐름이 쉬는 구간(등록 대기)에 멈출 수 있어, 매초 나오는 vus로 살아 있는지 본다.
func (m Metrics) LastSeen(ctx context.Context, at time.Time) (map[string]float64, error) {
	q := fmt.Sprintf(`time() - max by (%s) (timestamp(%s{%s}))`, m.Names.InstanceLabel, m.Names.VUs, m.sel(""))
	return m.byLabel(ctx, q, at, m.Names.InstanceLabel)
}

// Coverage는 구간 [start, end] 동안 태스크마다 vus 표본 수를 돌려준다. 빠진 구간을 찾는 데 쓴다.
func (m Metrics) Coverage(ctx context.Context, start, end time.Time) (map[string]float64, error) {
	// count by 는 시계열 수를 센다. 표본 수를 세려면 count_over_time 을 sum 으로 합친다
	q := fmt.Sprintf(`sum by (%s) (count_over_time(%s{%s}[%s]))`, m.Names.InstanceLabel, m.Names.VUs, m.sel(""), rng(end.Sub(start)))
	return m.byLabel(ctx, q, end, m.Names.InstanceLabel)
}

// Scenarios는 구간 [start, end]의 k6 시나리오(scenario)별 요청 수다. 요청 이름은 흐름끼리 겹치므로, 흐름 하나가 통째로
// 빠졌는지는 시나리오로도 본다
func (m Metrics) Scenarios(ctx context.Context, start, end time.Time) (map[string]float64, error) {
	q := fmt.Sprintf(`sum by (scenario) (increase(%s{%s}[%s]))`, m.Names.Reqs, m.sel(""), rng(end.Sub(start)))
	return m.byLabel(ctx, q, end, "scenario")
}

func (m Metrics) byLabel(ctx context.Context, q string, at time.Time, label string) (map[string]float64, error) {
	out, err := m.Q.Query(ctx, q, at)
	if err != nil {
		return nil, err
	}
	res := map[string]float64{}
	for _, s := range out {
		res[s.Labels[label]] = s.Value
	}
	return res, nil
}

// Stat은 구간 하나의 판정 값이다.
type Stat struct {
	Requests float64 `json:"requests"`
	Within   float64 `json:"within_p95_ms"` // 0.5초 안에 끝난 비율
	Rate5xx  float64 `json:"rate_5xx"`
}

// Window는 구간 [start, end]의 전체 값과 요청 이름(name)별 값을 돌려준다.
func (m Metrics) Window(ctx context.Context, start, end time.Time) (Stat, map[string]Stat, error) {
	r := rng(end.Sub(start))
	d, reqs := m.Names.Duration, m.Names.Reqs
	le := fmt.Sprintf(`le=%q`, m.Names.LE500)
	queries := map[string]string{
		"requests": fmt.Sprintf(`sum by (name) (increase(%s{%s}[%s]))`, reqs, m.sel(""), r),
		"within": fmt.Sprintf(`sum by (name) (increase(%s_bucket{%s}[%s])) / sum by (name) (increase(%s_count{%s}[%s]))`,
			d, m.sel(le), r, d, m.sel(""), r),
		"5xx": fmt.Sprintf(`sum by (name) (increase(%s{%s}[%s])) / sum by (name) (increase(%s{%s}[%s]))`,
			reqs, m.sel(`status=~"5.."`), r, reqs, m.sel(""), r),
	}
	byName := map[string]Stat{}
	set := func(kind string, f func(*Stat, float64)) error {
		out, err := m.Q.Query(ctx, queries[kind], end)
		if err != nil {
			return err
		}
		for _, s := range out {
			st := byName[s.Labels["name"]]
			f(&st, s.Value)
			byName[s.Labels["name"]] = st
		}
		return nil
	}
	if err := set("requests", func(s *Stat, v float64) { s.Requests = v }); err != nil {
		return Stat{}, nil, err
	}
	if err := set("within", func(s *Stat, v float64) { s.Within = v }); err != nil {
		return Stat{}, nil, err
	}
	if err := set("5xx", func(s *Stat, v float64) { s.Rate5xx = v }); err != nil {
		return Stat{}, nil, err
	}
	// 전체는 요청 수로 가중해 합친다
	var total Stat
	var within, fivexx float64
	// 요청이 없는 이름의 비율은 NaN(0/0)이라 건너뛴다
	for _, s := range byName {
		if !(s.Requests > 0) || math.IsNaN(s.Within) || math.IsNaN(s.Rate5xx) {
			continue
		}
		total.Requests += s.Requests
		within += s.Within * s.Requests
		fivexx += s.Rate5xx * s.Requests
	}
	if total.Requests > 0 {
		total.Within, total.Rate5xx = within/total.Requests, fivexx/total.Requests
	} else {
		total.Within, total.Rate5xx = math.NaN(), math.NaN()
	}
	return total, byName, nil
}
