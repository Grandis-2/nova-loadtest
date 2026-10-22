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

// Live 는 최근 1분의 진행 값이다. 시험 도중 1분마다 로그와 결과에 남긴다. 요청이 없으면 비율과 지연은 NaN 이다
type Live struct {
	RPS       float64 // 초당 요청
	P95, P99  float64 // 지연(ms). 히스토그램 버킷 사이를 직선으로 이은 근사다
	ErrorRate float64 // k6 의 http_req_failed 와 같은 정의다(expected_response="false"인 요청의 비율)
	Rate5xx   float64
	VUs       float64
}

// Progress 는 시각 at 까지 1분의 진행 값이다.
func (m Metrics) Progress(ctx context.Context, at time.Time) (Live, error) {
	r, d := m.Names.Reqs, m.Names.Duration
	var l Live
	for _, x := range []struct {
		dst *float64
		q   string
	}{
		{&l.RPS, fmt.Sprintf(`sum(rate(%s{%s}[1m]))`, r, m.sel(""))},
		{&l.ErrorRate, fmt.Sprintf(`sum(rate(%s{%s}[1m])) / sum(rate(%s{%s}[1m]))`, r, m.sel(`expected_response="false"`), r, m.sel(""))},
		{&l.Rate5xx, fmt.Sprintf(`sum(rate(%s{%s}[1m])) / sum(rate(%s{%s}[1m]))`, r, m.sel(`status=~"5.."`), r, m.sel(""))},
		{&l.P95, fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (rate(%s_bucket{%s}[1m])))`, d, m.sel(""))},
		{&l.P99, fmt.Sprintf(`histogram_quantile(0.99, sum by (le) (rate(%s_bucket{%s}[1m])))`, d, m.sel(""))},
		// vus 는 태스크마다 게이지다. 마지막 값을 더한다
		{&l.VUs, fmt.Sprintf(`sum(last_over_time(%s{%s}[1m]))`, m.Names.VUs, m.sel(""))},
	} {
		v, err := m.scalar(ctx, x.q, at)
		if err != nil {
			return Live{}, err
		}
		*x.dst = v
	}
	if math.IsNaN(l.RPS) {
		l.RPS = 0
	}
	// 요청이 있는데 실패나 5xx 가 한 건도 없으면 분자 시계열이 없어 NaN 이다. 0 으로 둔다(Window 와 같다)
	if l.RPS > 0 {
		for _, v := range []*float64{&l.ErrorRate, &l.Rate5xx} {
			if math.IsNaN(*v) {
				*v = 0
			}
		}
	}
	return l, nil
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

// Outcomes는 구간 [start, end]의 진입 결과(outcome)별 수다. 누적 카운터의 구간 최댓값을 더한다. 시계열이 처음 생긴
// 표본은 increase 가 세지 않아(앞 표본이 없다) 결과가 몇 건뿐인 시계열을 0으로 셀 수 있다
func (m Metrics) Outcomes(ctx context.Context, start, end time.Time) (map[string]float64, error) {
	q := fmt.Sprintf(`sum by (outcome) (max_over_time(%s{%s}[%s]))`, m.Names.Outcomes, m.sel(""), rng(end.Sub(start)))
	return m.byLabel(ctx, q, end, "outcome")
}

// Dropped는 구간 [start, end]에 VU 가 모자라 시작하지 못한 반복 수다. 없었으면 시계열이 없어 0이다
func (m Metrics) Dropped(ctx context.Context, start, end time.Time) (float64, error) {
	q := fmt.Sprintf(`sum(max_over_time(%s{%s}[%s]))`, m.Names.Dropped, m.sel(""), rng(end.Sub(start)))
	v, err := m.scalar(ctx, q, end)
	if math.IsNaN(v) {
		v = 0
	}
	return v, err
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
	Requests  float64 `json:"requests"`
	Within    float64 `json:"within_p95_ms"` // 0.5초 안에 끝난 비율
	Rate5xx   float64 `json:"rate_5xx"`
	ErrorRate float64 `json:"error_rate"` // 기대하지 않은 응답(5xx, 4xx, 연결 실패, 시간 초과)의 비율
	// 지연(ms). 히스토그램 버킷 사이를 직선으로 이은 근사라 판정에는 쓰지 않는다(판정은 Within)
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`
}

// quantiles 는 백분위수와 Stat 의 칸이다
var quantiles = []struct {
	q   float64
	set func(*Stat, float64)
}{
	{0.50, func(s *Stat, v float64) { s.P50 = v }},
	{0.95, func(s *Stat, v float64) { s.P95 = v }},
	{0.99, func(s *Stat, v float64) { s.P99 = v }},
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
		"error": fmt.Sprintf(`sum by (name) (increase(%s{%s}[%s])) / sum by (name) (increase(%s{%s}[%s]))`,
			reqs, m.sel(`expected_response="false"`), r, reqs, m.sel(""), r),
	}
	quantile := func(q float64, by string) string {
		return fmt.Sprintf(`histogram_quantile(%g, sum by (%s) (increase(%s_bucket{%s}[%s])))`, q, by, d, m.sel(""), r)
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
	// 실패율이 없는 이름(실패가 한 건도 없으면 시계열이 없다)은 0 이다
	for k, st := range byName {
		st.ErrorRate = math.NaN()
		byName[k] = st
	}
	if err := set("error", func(s *Stat, v float64) { s.ErrorRate = v }); err != nil {
		return Stat{}, nil, err
	}
	for _, x := range quantiles {
		queries["q"] = quantile(x.q, "name, le")
		if err := set("q", x.set); err != nil {
			return Stat{}, nil, err
		}
	}
	// 전체는 요청 수로 가중해 합친다
	var total Stat
	var within, fivexx, failed float64
	// 요청이 없는 이름의 비율은 NaN(0/0)이라 건너뛴다
	for k, s := range byName {
		if s.Requests > 0 && math.IsNaN(s.ErrorRate) {
			s.ErrorRate = 0
			byName[k] = s
		}
		if !(s.Requests > 0) || math.IsNaN(s.Within) || math.IsNaN(s.Rate5xx) {
			continue
		}
		total.Requests += s.Requests
		within += s.Within * s.Requests
		fivexx += s.Rate5xx * s.Requests
		failed += s.ErrorRate * s.Requests
	}
	if total.Requests > 0 {
		total.Within, total.Rate5xx, total.ErrorRate = within/total.Requests, fivexx/total.Requests, failed/total.Requests
	} else {
		total.Within, total.Rate5xx, total.ErrorRate = math.NaN(), math.NaN(), math.NaN()
	}
	// 백분위수는 이름별 값을 평균하지 않고 버킷을 합쳐 다시 구한다
	for _, x := range quantiles {
		v, err := m.scalar(ctx, quantile(x.q, "le"), end)
		if err != nil {
			return Stat{}, nil, err
		}
		x.set(&total, v)
	}
	return total, byName, nil
}
