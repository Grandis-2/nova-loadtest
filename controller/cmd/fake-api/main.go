// fake-api는 k6 흐름을 운영에 닿지 않고 돌려 보려고 be의 공개 API 모양을 흉내 내는 서버다(로컬, CI 전용).
// 모양은 be 4d5863cb의 컨트롤러와 DTO를 따른다(docs/design.md "흐름과 회원 풀").
//
//	FAKE_MODE=ok     정상 응답
//	FAKE_MODE=error  setup 이 쓰는 상품 목록, 상세, 카테고리 트리를 뺀 모든 API가 500을 준다(k6 중단 임계치 시험)
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
	"strconv"
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
	// Ready 는 worker 가 등록을 마치는 때다. 그 전에는 PENDING_SYNC 다
	Ready time.Time `json:"-"`
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

// 접수부터 등록(과 알림)까지의 시간. 짝수 번째 접수는 화면의 등록 확인(5초) 안에, 홀수 번째는 그 뒤에 등록되어
// rush 의 두 길(바로 결제, 알림을 기다려 결제)을 모두 지난다
const (
	registerFast = 2 * time.Second
	registerSlow = 8 * time.Second
)

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

	if s.mode == "error" && route != "GET /api/v1/products" && route != "GET /api/v1/products/{id}" && route != "GET /api/v1/categories" {
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
		s.products(w, r)
	case "GET /api/v1/products/{id}":
		id := lastID(r.URL.Path, 0)
		ok(w, 200, map[string]any{"productId": id, "status": "ACTIVE", "campaign": map[string]any{"status": "OPEN"},
			"variants": []map[string]any{{"variantId": "11111111-0000-4000-8000-" + id[len(id)-12:], "status": "ACTIVE"}}})
	case "GET /api/v1/promotions":
		items := []map[string]any{}
		for _, p := range fakePromotions {
			items = append(items, map[string]any{"promotionId": p.id, "name": p.name, "thumbnailUrl": "https://cdn.example/p.png"})
		}
		ok(w, 200, map[string]any{"items": items})
	case "GET /api/v1/promotions/{id}":
		id := lastID(r.URL.Path, 0)
		for _, p := range fakePromotions {
			if p.id != id {
				continue
			}
			products := []map[string]any{}
			for _, pid := range p.products {
				products = append(products, map[string]any{"productId": pid, "saleMode": "PREORDER", "preorderStatus": "OPEN"})
			}
			ok(w, 200, map[string]any{"promotionId": id, "name": p.name, "products": products})
			return
		}
		s.violate("unknown_promotion")
		fail(w, 404, "PROMOTION_NOT_FOUND")
	case "GET /api/v1/categories":
		ok(w, 200, map[string]any{"items": categoryTree()})
	case "GET /api/v1/products/{id}/reviews":
		if q := r.URL.Query(); q.Get("page") != "0" || q.Get("size") != "100" {
			s.violate("reviews_query")
		}
		ok(w, 200, map[string]any{"page": 0, "size": 100, "total": 0, "hasNext": false, "items": []any{}})
	case "GET /api/v1/preorders/products/{id}/shipment-batches":
		ok(w, 200, map[string]any{"items": []map[string]any{{"batchNumber": 1, "positionFrom": 1, "positionTo": nil}}})
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
		s.register(sub)
		items := []*preorder{}
		for _, p := range s.preorders[sub] {
			if (q.Get("productId") == "" || q.Get("productId") == p.ProductID) && (q.Get("status") == "" || q.Get("status") == p.Status) {
				items = append(items, p)
			}
		}
		s.mu.Unlock()
		ok(w, 200, map[string]any{"items": items, "nextCursor": nil})
	case "GET /api/v1/preorders/{id}":
		sub, good := s.auth(r)
		if !good {
			fail(w, 401, "UNAUTHENTICATED")
			return
		}
		id := lastID(r.URL.Path, 0)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.register(sub)
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
		now := time.Now()
		ready := now.Add(registerFast)
		if s.seq%2 == 1 {
			ready = now.Add(registerSlow)
		}
		s.preorders[sub] = append(s.preorders[sub], &preorder{ID: id, ProductID: body.ProductID, OptionID: body.OptionID,
			Status: "PENDING_SYNC", Ready: ready})
		if len(s.notes[sub]) == 0 {
			// 지난 시험에서 읽지 않고 남은 다른 예약의 알림
			s.notes[sub] = append(s.notes[sub], notification{ID: fmt.Sprintf("44444444-0000-4000-9000-%012d", s.seq),
				Target: "stale-" + id, At: now.Add(-time.Hour), Visible: now.Add(-time.Hour)})
		}
		s.notes[sub] = append(s.notes[sub], notification{ID: fmt.Sprintf("44444444-0000-4000-8000-%012d", s.seq), Target: id,
			At: now, Visible: ready})
	}
	s.mu.Unlock()
	w.Header().Set("X-Idempotent-Replay", fmt.Sprint(replay))
	ok(w, 202, map[string]any{"preorderId": id, "status": "PENDING_SYNC", "replayed": replay})
}

// fakePromotions 는 지금 보이는 사전예약(홍보)이다. 첫째는 e2e 의 시험 상품(k6/test/e2e.sh product)을, 둘째는
// 상품 목록의 사전예약 상품 둘을 묶는다
var fakePromotions = []struct {
	id, name string
	products []string
}{
	{"66666666-0000-4000-8000-000000000001", "[부하 시험] 사전예약", []string{"a1b2c3d4-0000-4000-8000-000000000001"}},
	{"66666666-0000-4000-8000-000000000002", "사전예약", []string{"00000000-0000-4000-8000-000000000000", "00000000-0000-4000-8000-000000000002"}},
}

// fakeProducts 는 상품 목록의 전체 수다. 필터가 없으면 카테고리 목록(size 12)이 세 쪽이 되게 둔다
const fakeProducts = 30

// fakeLeaves 는 상품이 속하는 하위 분류다. 상품 i 는 fakeLeaves[i % 4] 에 속하고, 상위 분류(1, 2)로 거르면 하위 분류의 상품도 나온다
var fakeLeaves = []int{11, 12, 21, 3}

func categoryID(n int) string { return fmt.Sprintf("55555555-0000-4000-8000-%012d", n) }

// products 는 상품 목록이다. categoryId 와 q 로 거른 뒤 쪽을 나눈다. 짝수 번째는 사전예약, 홀수 번째는 일반 판매다.
// 화면이 보내지 않는 정렬(sort), 숫자가 아닌 쪽, 범위를 벗어난 쪽은 400 과 위반으로 센다. 모르는 분류와 결과가 없는 검색도
// 위반으로 센다(k6 는 카테고리 트리와 이 목록에서 분류와 검색어를 고르므로 늘 결과가 있어야 한다)
func (s *server) products(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	size, page, bad := 20, 0, q.Has("sort")
	for _, p := range []struct {
		name string
		v    *int
	}{{"size", &size}, {"page", &page}} {
		if v := q.Get(p.name); v != "" {
			n, err := strconv.Atoi(v)
			bad = bad || err != nil
			*p.v = n
		}
	}
	if bad || size < 1 || size > 100 || page < 0 || page*size >= 10000 {
		s.violate("products_query")
		fail(w, 400, "VALIDATION_FAILED")
		return
	}
	in := map[int]bool{}
	if id := q.Get("categoryId"); id != "" {
		children := map[int][]int{1: {11, 12}, 2: {21}} // 상위 분류는 하위 분류의 상품을 포함한다
		for _, n := range []int{1, 2, 3, 11, 12, 21} {
			if categoryID(n) == id {
				in[n] = true
				for _, c := range children[n] {
					in[c] = true
				}
			}
		}
		if len(in) == 0 {
			s.violate("products_category")
		}
	}
	term := strings.ToLower(q.Get("q"))
	var matched []map[string]any
	for i := 1; i <= fakeProducts; i++ {
		title, model := fmt.Sprintf("무선 이어폰 %d", i), fmt.Sprintf("LT-%d", i)
		if len(in) > 0 && !in[fakeLeaves[i%len(fakeLeaves)]] {
			continue
		}
		if term != "" && !strings.Contains(strings.ToLower(title), term) && !strings.Contains(strings.ToLower(model), term) {
			continue
		}
		mode := "NORMAL"
		if i%2 == 0 {
			mode = "PREORDER"
		}
		matched = append(matched, map[string]any{"productId": fmt.Sprintf("00000000-0000-4000-8000-%012d", i), "saleMode": mode,
			"title": title, "modelName": model, "status": "ACTIVE"})
	}
	if term != "" && len(matched) == 0 {
		s.violate("search_empty")
	}
	items := []map[string]any{}
	if from := page * size; from < len(matched) {
		items = matched[from:min(len(matched), from+size)]
	}
	ok(w, 200, map[string]any{"page": page, "size": size, "total": len(matched), "hasNext": (page+1)*size < len(matched), "items": items})
}

func categoryTree() []map[string]any {
	node := func(n int, children ...map[string]any) map[string]any {
		return map[string]any{"categoryId": categoryID(n), "name": fmt.Sprintf("분류 %d", n), "children": append([]map[string]any{}, children...)}
	}
	return []map[string]any{node(1, node(11), node(12)), node(2, node(21)), node(3)}
}

// register 는 등록을 마칠 때가 지난 예약을 REGISTERED 로 바꾼다. s.mu 를 잡고 부른다
func (s *server) register(sub string) {
	now := time.Now()
	for _, p := range s.preorders[sub] {
		if p.Status == "PENDING_SYNC" && !now.Before(p.Ready) {
			p.Status = "REGISTERED"
		}
	}
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
