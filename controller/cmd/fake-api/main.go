// fake-api는 k6 흐름을 운영에 닿지 않고 돌려 보려고 be의 공개 API 모양을 흉내 내는 서버다(로컬, CI 전용).
// 모양은 be 4d5863cb의 컨트롤러와 DTO를 따른다(docs/design.md "흐름과 회원 풀").
//
//	FAKE_MODE=ok     정상 응답
//	FAKE_MODE=error  상품 목록과 상세(setup이 쓴다)를 뺀 모든 API가 500을 준다(k6 중단 임계치 시험)
//	FAKE_HEADER      값이 있으면 x-nova-loadtest 헤더가 이 값인지 센다
//
// GET /__stats 로 받은 요청을 센 결과(경로별 수, 규칙 위반)를 돌려준다.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type preorder struct {
	ID        string `json:"preorderId"`
	ProductID string `json:"productId"`
	OptionID  string `json:"optionId"`
	Status    string `json:"status"`
}

// notification 은 등록 알림이다. 실제로는 worker 가 등록하고 member 가 저장한 뒤에 보이므로 visible 뒤부터 보인다
type notification struct {
	ID      string
	Target  string
	At      time.Time
	Visible time.Time
}

type server struct {
	mode, header string

	mu         sync.Mutex
	routes     map[string]int
	violations map[string]int
	polls      map[string]int            // 회원별 순번 확인 수
	preorders  map[string][]*preorder    // 회원 sub → 예약
	idem       map[string]string         // 멱등 키 → 예약 ID
	notes      map[string][]notification // 회원 sub → 알림
	seq        int
}

// notifyDelay 는 접수부터 등록 알림이 보일 때까지의 시간이다
const notifyDelay = 2 * time.Second

func main() {
	s := &server{
		mode: envOr("FAKE_MODE", "ok"), header: os.Getenv("FAKE_HEADER"),
		routes: map[string]int{}, violations: map[string]int{}, polls: map[string]int{},
		preorders: map[string][]*preorder{}, idem: map[string]string{}, notes: map[string][]notification{},
	}
	addr := envOr("FAKE_ADDR", ":8080")
	log.Printf("fake-api %s (mode=%s)", addr, s.mode)
	log.Fatal(http.ListenAndServe(addr, s))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__stats" {
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"routes": s.routes, "violations": s.violations})
		return
	}
	route := routeOf(r.Method, r.URL.Path)
	s.mu.Lock()
	s.routes[route]++
	if !strings.HasPrefix(r.UserAgent(), "nova-loadtest/lt-") {
		s.violations["user_agent"]++
	}
	if s.header != "" && r.Header.Get("x-nova-loadtest") != s.header {
		s.violations["loadtest_header"]++
	}
	s.mu.Unlock()

	if s.mode == "error" && route != "GET /api/v1/products" && route != "GET /api/v1/products/{id}" {
		fail(w, 500, "INTERNAL_ERROR")
		return
	}
	s.handle(w, r, route)
}

// routeOf는 경로를 템플릿으로 바꾼다. k6의 name 태그와 같은 모양이다
func routeOf(method, path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if uuidRe.MatchString(p) {
			parts[i] = "{id}"
		}
	}
	return method + " /" + strings.Join(parts, "/")
}

func (s *server) handle(w http.ResponseWriter, r *http.Request, route string) {
	switch route {
	case "GET /api/v1/products":
		items := []map[string]any{}
		for i := 1; i <= 5; i++ {
			items = append(items, map[string]any{"productId": fmt.Sprintf("00000000-0000-4000-8000-%012d", i), "status": "ACTIVE"})
		}
		ok(w, 200, map[string]any{"page": 0, "size": 20, "total": 5, "hasNext": false, "items": items})
	case "GET /api/v1/products/{id}":
		id := lastID(r.URL.Path, 0)
		ok(w, 200, map[string]any{"productId": id, "status": "ACTIVE", "variants": []map[string]any{
			{"variantId": "11111111-0000-4000-8000-" + id[len(id)-12:], "status": "ACTIVE"},
		}})
	case "GET /api/v1/categories":
		ok(w, 200, map[string]any{"items": []map[string]any{{"categoryId": "c1", "name": "스마트폰"}}})
	case "POST /api/v1/preorders/queue":
		sub, good := s.auth(r)
		if !good {
			fail(w, 401, "UNAUTHENTICATED")
			return
		}
		// 처음에는 줄에 세우고, 확인 한 번 뒤에 입장시킨다
		w.Header().Set("Retry-After", "1")
		ok(w, 202, map[string]any{"status": "WAITING", "queueToken": "qt-" + sub, "position": 1, "etaSeconds": 1})
	case "GET /api/v1/preorders/queue":
		sub, good := s.auth(r)
		if !good {
			fail(w, 401, "UNAUTHENTICATED")
			return
		}
		if r.Header.Get("Queue-Token") == "" {
			fail(w, 400, "VALIDATION_FAILED")
			return
		}
		s.mu.Lock()
		s.polls[sub]++
		n := s.polls[sub]
		s.mu.Unlock()
		if n < 2 {
			// 만료가 가까운 대기 토큰을 바꿔 주는 경우도 흉내 낸다
			w.Header().Set("Retry-After", "1")
			ok(w, 200, map[string]any{"status": "WAITING", "queueToken": fmt.Sprintf("qt2-%s", sub), "position": 1, "etaSeconds": 1})
			return
		}
		ok(w, 200, map[string]any{"status": "ADMITTED", "admissionTicket": "ticket-" + sub, "expiresIn": 60})
	case "POST /api/v1/preorders":
		s.accept(w, r)
	case "GET /api/v1/preorders":
		sub, good := s.auth(r)
		if !good {
			fail(w, 401, "UNAUTHENTICATED")
			return
		}
		q := r.URL.Query()
		s.mu.Lock()
		items := []*preorder{}
		for _, p := range s.preorders[sub] {
			if (q.Get("productId") == "" || q.Get("productId") == p.ProductID) && (q.Get("status") == "" || q.Get("status") == p.Status) {
				items = append(items, p)
			}
		}
		s.mu.Unlock()
		ok(w, 200, map[string]any{"items": items, "nextCursor": nil})
	case "GET /api/v1/preorders/active":
		if _, good := s.auth(r); !good {
			fail(w, 401, "UNAUTHENTICATED")
			return
		}
		ok(w, 200, map[string]any{"items": []any{}})
	case "GET /api/v1/preorders/{id}":
		sub, good := s.auth(r)
		if !good {
			fail(w, 401, "UNAUTHENTICATED")
			return
		}
		id := lastID(r.URL.Path, 0)
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, p := range s.preorders[sub] {
			if p.ID == id {
				ok(w, 200, p)
				return
			}
		}
		fail(w, 404, "PREORDER_NOT_FOUND")
	case "GET /api/v1/notifications/unread-count":
		sub, good := s.auth(r)
		if !good {
			fail(w, 401, "UNAUTHENTICATED")
			return
		}
		ok(w, 200, map[string]any{"count": len(s.visible(sub))})
	case "GET /api/v1/notifications":
		sub, good := s.auth(r)
		if !good {
			fail(w, 401, "UNAUTHENTICATED")
			return
		}
		items := []map[string]any{}
		for _, n := range s.visible(sub) {
			items = append(items, map[string]any{"notificationId": n.ID, "kind": "PREORDER_REGISTERED",
				"title": "사전예약 등록 완료", "message": "", "target": map[string]any{"type": "PREORDER", "id": n.Target},
				"read": false, "createdAt": n.At})
		}
		ok(w, 200, map[string]any{"items": items, "nextCursor": nil})
	case "POST /api/v1/preorders/{id}/cancel":
		sub, good := s.auth(r)
		if !good {
			fail(w, 401, "UNAUTHENTICATED")
			return
		}
		id := lastID(r.URL.Path, 1)
		s.mu.Lock()
		for _, p := range s.preorders[sub] {
			if p.ID == id {
				p.Status = "CANCELED"
			}
		}
		s.mu.Unlock()
		ok(w, 202, map[string]any{"preorderId": id, "status": "CANCELING"})
	case "POST /api/v1/orders":
		if _, good := s.auth(r); !good {
			fail(w, 401, "UNAUTHENTICATED")
			return
		}
		var body struct {
			Source     string            `json:"source"`
			PreorderID string            `json:"preorderId"`
			ShipTo     map[string]string `json:"shipTo"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Source != "PREORDER" || body.PreorderID == "" ||
			!validShipTo(body.ShipTo) {
			s.violate("order_body")
			fail(w, 400, "VALIDATION_FAILED")
			return
		}
		ok(w, 201, map[string]any{"orderId": "22222222-0000-4000-8000-" + body.PreorderID[len(body.PreorderID)-12:], "reused": false})
	case "POST /api/v1/orders/{id}/payment-attempts":
		if _, good := s.auth(r); !good {
			fail(w, 401, "UNAUTHENTICATED")
			return
		}
		ok(w, 201, map[string]any{"tossOrderId": "fake-attempt", "amount": 1000})
	default:
		s.violate("unknown_route:" + route)
		fail(w, 404, "NOT_FOUND")
	}
}

func (s *server) accept(w http.ResponseWriter, r *http.Request) {
	sub, good := s.auth(r)
	if !good {
		fail(w, 401, "UNAUTHENTICATED")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if n := len([]rune(key)); n < 8 || n > 64 {
		s.violate("idempotency_key")
		fail(w, 400, "VALIDATION_FAILED")
		return
	}
	if r.Header.Get("X-Admission-Ticket") != "ticket-"+sub {
		s.violate("admission_ticket")
		fail(w, 403, "ADMISSION_TICKET_INVALID")
		return
	}
	var body struct{ ProductID, OptionID string }
	if json.NewDecoder(r.Body).Decode(&body) != nil || !uuidRe.MatchString(body.ProductID) || !uuidRe.MatchString(body.OptionID) ||
		body.ProductID != r.URL.Query().Get("productId") {
		s.violate("accept_body")
		fail(w, 400, "VALIDATION_FAILED")
		return
	}
	s.mu.Lock()
	id, replay := s.idem[key]
	if !replay {
		s.seq++
		id = fmt.Sprintf("33333333-0000-4000-8000-%012d", s.seq)
		s.idem[key] = id
		// 실제로는 worker가 등록한 뒤 REGISTERED가 된다. 여기서는 바로 등록된 것으로 둔다
		s.preorders[sub] = append(s.preorders[sub], &preorder{ID: id, ProductID: body.ProductID, OptionID: body.OptionID, Status: "REGISTERED"})
		now := time.Now()
		if len(s.notes[sub]) == 0 {
			// 지난 시험에서 읽지 않고 남은 다른 예약의 알림
			s.notes[sub] = append(s.notes[sub], notification{ID: fmt.Sprintf("44444444-0000-4000-9000-%012d", s.seq),
				Target: "stale-" + id, At: now.Add(-time.Hour), Visible: now.Add(-time.Hour)})
		}
		s.notes[sub] = append(s.notes[sub], notification{ID: fmt.Sprintf("44444444-0000-4000-8000-%012d", s.seq), Target: id,
			At: now, Visible: now.Add(notifyDelay)})
	}
	s.mu.Unlock()
	w.Header().Set("X-Idempotent-Replay", fmt.Sprint(replay))
	ok(w, 202, map[string]any{"preorderId": id, "status": "PENDING_SYNC", "replayed": replay})
}

// visible 은 지금 보이는 알림이다. 최신순이다
func (s *server) visible(sub string) []notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var out []notification
	for i := len(s.notes[sub]) - 1; i >= 0; i-- {
		if n := s.notes[sub][i]; !now.Before(n.Visible) {
			out = append(out, n)
		}
	}
	return out
}

// auth는 Bearer 토큰의 sub를 꺼낸다. 서명은 보지 않는다(시험용 서버다)
func (s *server) auth(r *http.Request) (string, bool) {
	tok, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(tok, ".")
	if !found || len(parts) != 3 {
		s.violate("missing_token")
		return "", false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Sub  string `json:"sub"`
		Role string `json:"role"`
	}
	if err != nil || json.Unmarshal(b, &c) != nil || c.Sub == "" || c.Role != "USER" {
		s.violate("bad_token")
		return "", false
	}
	return c.Sub, true
}

func (s *server) violate(k string) {
	s.mu.Lock()
	s.violations[k]++
	s.mu.Unlock()
}

func validShipTo(m map[string]string) bool {
	limits := map[string]int{"name": 50, "phone": 20, "postalCode": 10, "line1": 200}
	for k, max := range limits {
		if v := m[k]; strings.TrimSpace(v) == "" || len([]rune(v)) > max {
			return false
		}
	}
	return len([]rune(m["line2"])) <= 200
}

func lastID(path string, fromEnd int) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return parts[len(parts)-1-fromEnd]
}

func ok(w http.ResponseWriter, status int, data any) {
	writeJSON(w, status, map[string]any{"success": true, "data": data, "error": nil, "timestamp": time.Now().UTC(), "traceId": "fake"})
}

func fail(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"success": false, "data": nil,
		"error": map[string]any{"code": code, "message": code, "details": nil}, "timestamp": time.Now().UTC(), "traceId": "fake"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
