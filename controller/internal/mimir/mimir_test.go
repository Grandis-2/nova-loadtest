package mimir

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Grandis-2/nova-loadtest/config"
)

var names = config.Metrics{Job: "loadtest/k6", InstanceLabel: "instance", Duration: "k6_http_req_duration_milliseconds",
	Reqs: "k6_http_reqs_total", VUs: "k6_vus", LE500: "500", Outcomes: "k6_entry_outcome_total",
	Dropped: "k6_dropped_iterations_total"}

func TestHTTPQueryAndRediscover(t *testing.T) {
	var gotQuery, gotTime string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prometheus/api/v1/query" {
			w.WriteHeader(404)
			return
		}
		gotQuery, gotTime = r.URL.Query().Get("query"), r.URL.Query().Get("time")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"instance":"0"},"value":[1,"0.25"]},{"metric":{"instance":"1"},"value":[1,"NaN"]}]}}`)
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	calls := 0
	h := &HTTP{Port: port, Path: "/prometheus/api/v1/query", Discover: func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return "127.0.0.2", nil // 처음 찾은 IP는 이미 사라졌다(재배포)
		}
		return host, nil
	}, Client: &http.Client{Timeout: 2 * time.Second}}
	at := time.Unix(1_800_000_000, 0)
	out, err := h.Query(context.Background(), "up", at)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("조회가 실패하면 IP를 다시 찾아야 한다(찾은 횟수 %d)", calls)
	}
	if gotQuery != "up" || gotTime != "1800000000" || len(out) != 2 || out[0].Value != 0.25 || !math.IsNaN(out[1].Value) {
		t.Fatalf("%q %q %+v", gotQuery, gotTime, out)
	}
}

type recorder struct {
	queries []string
	answer  func(q string) []Sample
}

func (r *recorder) Query(_ context.Context, q string, _ time.Time) ([]Sample, error) {
	r.queries = append(r.queries, q)
	if r.answer == nil {
		return nil, errors.New("응답 없음")
	}
	return r.answer(q), nil
}

func TestQueriesUseConfiguredNames(t *testing.T) {
	rec := &recorder{answer: func(string) []Sample { return []Sample{{Value: 0.01}} }}
	m := Metrics{Q: rec, Names: names, TestID: "lt-20261020t0630-1"}
	ctx, now := context.Background(), time.Now()
	if l, _ := m.Progress(ctx, now); l.ErrorRate != 0.01 || l.RPS != 0.01 || l.VUs != 0.01 {
		t.Fatal(l)
	}
	live := rec.queries
	rec.queries = nil
	m.LastSeen(ctx, now)
	m.Coverage(ctx, now.Add(-5*time.Minute), now)
	m.Scenarios(ctx, now.Add(-5*time.Minute), now)
	m.Outcomes(ctx, now.Add(-5*time.Minute), now)
	m.Dropped(ctx, now.Add(-5*time.Minute), now)
	for _, q := range append(live, rec.queries...) {
		if !strings.Contains(q, `test_id="lt-20261020t0630-1"`) || !strings.Contains(q, `job="loadtest/k6"`) {
			t.Errorf("시험 ID와 job으로 걸러야 한다: %s", q)
		}
	}
	// 진행 값: 초당 요청, 오류율, 5xx, p95, p99, VU
	for i, want := range []string{"sum(rate(k6_http_reqs_total{", `expected_response="false"`, `status=~"5.."`,
		"histogram_quantile(0.95, sum by (le) (rate(k6_http_req_duration_milliseconds_bucket{", "histogram_quantile(0.99,",
		"sum(last_over_time(k6_vus{"} {
		if !strings.Contains(live[i], want) || !strings.Contains(live[i], "[1m]") {
			t.Errorf("진행 값 %d 쿼리(%s, 최근 1분): %s", i, want, live[i])
		}
	}
	if !strings.Contains(rec.queries[0], "max by (instance)") || !strings.Contains(rec.queries[0], "k6_vus") {
		t.Errorf("살아 있는지는 태스크별 vus로 본다: %s", rec.queries[0])
	}
	if !strings.Contains(rec.queries[1], "sum by (instance) (count_over_time(k6_vus") || !strings.Contains(rec.queries[1], "[300s]") {
		t.Errorf("구간 누락은 태스크별 표본 수로 본다: %s", rec.queries[1])
	}
	if !strings.Contains(rec.queries[2], "sum by (scenario) (increase(k6_http_reqs_total{") || !strings.Contains(rec.queries[2], "[300s]") {
		t.Errorf("흐름이 빠졌는지는 시나리오별 요청 수로도 본다: %s", rec.queries[2])
	}
	// 시계열이 처음 생긴 표본까지 세려고 increase 대신 누적값의 최댓값을 더한다
	if !strings.Contains(rec.queries[3], "sum by (outcome) (max_over_time(k6_entry_outcome_total{") || !strings.Contains(rec.queries[3], "[300s]") {
		t.Errorf("queue 의 처리 인원은 진입 결과별 누적값으로 센다: %s", rec.queries[3])
	}
	if !strings.Contains(rec.queries[4], "sum(max_over_time(k6_dropped_iterations_total{") || !strings.Contains(rec.queries[4], "[300s]") {
		t.Errorf("시작하지 못한 반복은 누적값으로 센다: %s", rec.queries[4])
	}
}

func TestWindow(t *testing.T) {
	rec := &recorder{answer: func(q string) []Sample {
		switch {
		case strings.HasPrefix(q, "sum by (name) (increase(k6_http_reqs_total{") && !strings.Contains(q, ") / "):
			return []Sample{{Labels: map[string]string{"name": "/a"}, Value: 900}, {Labels: map[string]string{"name": "/b"}, Value: 100}, {Labels: map[string]string{"name": "/c"}, Value: 0}}
		case strings.HasPrefix(q, "histogram_quantile("):
			if !strings.Contains(q, "sum by (name, le)") {
				return []Sample{{Value: 250}} // 전체는 버킷을 합쳐 다시 구한다
			}
			return []Sample{{Labels: map[string]string{"name": "/a"}, Value: 100}, {Labels: map[string]string{"name": "/b"}, Value: 400}}
		case strings.Contains(q, "_bucket{"):
			if !strings.Contains(q, `le="500"`) {
				t.Errorf("0.5초 경계는 설정의 le 값을 쓴다: %s", q)
			}
			return []Sample{{Labels: map[string]string{"name": "/a"}, Value: 1}, {Labels: map[string]string{"name": "/b"}, Value: 0.5}, {Labels: map[string]string{"name": "/c"}, Value: math.NaN()}}
		default:
			return []Sample{{Labels: map[string]string{"name": "/a"}, Value: 0}, {Labels: map[string]string{"name": "/b"}, Value: 0.1}, {Labels: map[string]string{"name": "/c"}, Value: math.NaN()}}
		}
	}}
	m := Metrics{Q: rec, Names: names, TestID: "lt-20261020t0630-1"}
	end := time.Now()
	total, byName, err := m.Window(context.Background(), end.Add(-30*time.Minute), end)
	if err != nil {
		t.Fatal(err)
	}
	// 요청 수로 가중해 합친다. 요청이 없는 /c 의 NaN 은 건너뛴다
	if total.Requests != 1000 || math.Abs(total.Within-0.95) > 1e-9 || math.Abs(total.Rate5xx-0.01) > 1e-9 ||
		math.Abs(total.ErrorRate-0.01) > 1e-9 || total.P95 != 250 || total.P99 != 250 {
		t.Fatalf("전체 %+v", total)
	}
	if b := byName["/b"]; b.Within != 0.5 || b.Requests != 100 || b.P50 != 400 || b.P95 != 400 || b.ErrorRate != 0.1 {
		t.Fatalf("이름별 %+v", byName)
	}
	if !strings.Contains(rec.queries[0], "[1800s]") {
		t.Fatalf("범위는 구간 길이다: %s", rec.queries[0])
	}
}
