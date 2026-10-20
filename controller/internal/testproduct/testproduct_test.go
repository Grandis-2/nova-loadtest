package testproduct

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)

func spec() Spec {
	return Spec{OpensAt: now.Add(40 * time.Minute), ClosesAt: now.Add(12 * time.Hour), CategoryID: "c-leaf"}
}

func TestCheck(t *testing.T) {
	if err := spec().Check(now); err != nil {
		t.Fatal(err)
	}
	early := spec()
	early.OpensAt = now.Add(29 * time.Minute)
	if err := early.Check(now); err == nil {
		t.Fatal("30분 안의 오픈은 be 가 거절하므로 미리 막아야 한다")
	}
	closed := spec()
	closed.ClosesAt = closed.OpensAt
	if err := closed.Check(now); err == nil {
		t.Fatal("마감이 오픈과 같으면 막아야 한다")
	}
}

// be 는 모르는 칸을 400 으로 거절한다. 계약(ProductRegistrationRequest)에 있는 칸만 보낸다
func TestBodyFields(t *testing.T) {
	b := Body(spec())
	allowed := []string{"categoryId", "saleMode", "title", "brand", "modelName", "description", "tags", "visible",
		"basePrice", "discountRate", "warranty", "optionAxes", "combinations", "images", "campaign", "shipmentBatches"}
	for k := range b {
		if !slices.Contains(allowed, k) {
			t.Errorf("계약에 없는 칸 %q", k)
		}
	}
	if b["visible"] != true {
		t.Error("비공개 상품은 대기열이 줄을 열지 않는다. 공개로 만들어야 한다")
	}
	if b["saleMode"] != "PREORDER" {
		t.Error("사전예약 상품이어야 한다")
	}
	if b["title"] != "[부하 시험] 무선 이어폰 LT 1020-0640" {
		t.Errorf("상품이 하나면 이름에 쓰임새를 붙이지 않는다: %v", b["title"])
	}
	prep := spec()
	prep.Label = "준비"
	if got := Body(prep)["title"]; got != "[부하 시험] 무선 이어폰 LT 1020-0640 (준비)" {
		t.Errorf("spike 상품은 쓰임새를 붙인다: %v", got)
	}
	if n := len([]rune(b["title"].(string))); n > 100 {
		t.Errorf("상품명 %d자는 100자를 넘는다", n)
	}
	if n := len(b["modelName"].(string)); n > 40 {
		t.Errorf("모델명 %d자는 40자를 넘는다", n)
	}
	c := b["campaign"].(map[string]any)
	if c["opensAt"] != "2026-10-20T06:40:00Z" || c["closesAt"] != "2026-10-20T18:00:00Z" {
		t.Errorf("회차 시각 %v", c)
	}
	batch := b["shipmentBatches"].([]any)[0].(map[string]any)
	if _, ok := batch["positionTo"]; ok {
		t.Error("마지막 차수는 종료 순번을 비워야 한다")
	}
	if batch["batchNumber"] != 1 || batch["positionFrom"] != 1 {
		t.Errorf("첫 차수는 1번, 순번 1부터다: %v", batch)
	}
}

// fakeAPI 는 be 의 공개 카테고리, 관리자 등록, 등록 상태, 공개 상세를 흉내 낸다
type fakeAPI struct {
	mu        sync.Mutex
	token     string
	polls     int // 등록 상태를 몇 번 물어야 completed 가 되는가
	reject    bool
	keys      []string
	bodies    []map[string]any
	statusAsk int
}

func (f *fakeAPI) handler(t *testing.T) http.Handler {
	ok := func(w http.ResponseWriter, code int, data any) {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.Contains(r.URL.Path, "/admin/") && r.Header.Get("Authorization") != "Bearer "+f.token {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"success":false,"error":{"code":"UNAUTHORIZED","message":"인증이 필요합니다."}}`)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/categories":
			ok(w, 200, map[string]any{"items": []any{
				map[string]any{"categoryId": "c-mobile", "name": "모바일", "children": []any{
					map[string]any{"categoryId": "c-phone", "name": "스마트폰", "children": []any{}},
				}},
				map[string]any{"categoryId": "c-top", "name": "웨어러블", "children": []any{
					map[string]any{"categoryId": "c-leaf", "name": "무선이어폰", "children": []any{}},
				}},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/products":
			if f.reject {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"success":false,"error":{"code":"VALIDATION_FAILED","message":"입력값을 확인하세요.","details":{"field":"campaign.opensAt"}}}`)
				return
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.keys = append(f.keys, r.Header.Get("Idempotency-Key"))
			f.bodies = append(f.bodies, body)
			ok(w, 201, map[string]any{"registration": map[string]any{
				"productId": "0199F0E0-0000-7000-8000-00000000000" + string(rune('0'+len(f.keys))), "completed": false,
			}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/admin/products/registrations/"):
			f.statusAsk++
			ok(w, 200, map[string]any{"completed": f.statusAsk >= f.polls})
		default:
			t.Errorf("예상하지 않은 요청 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestCreateAndWait(t *testing.T) {
	f := &fakeAPI{token: "admin-token", polls: 2}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "admin-token", Poll: time.Millisecond}
	ctx := context.Background()

	cat, err := c.Category(ctx)
	if err != nil || cat != "c-leaf" {
		t.Fatalf("이어폰 카테고리를 골라야 한다: %q %v", cat, err)
	}
	s := spec()
	s.CategoryID = cat
	p, err := c.Create(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "0199f0e0-0000-7000-8000-000000000001" {
		t.Errorf("상품 ID 는 소문자로 바꿔야 한다(대문자가 섞이면 접수가 400): %s", p.ID)
	}
	if !strings.HasPrefix(f.keys[0], "loadtest-") || p.IdempotencyKey != f.keys[0] {
		t.Errorf("멱등 키 %q, 돌려준 키 %q", f.keys[0], p.IdempotencyKey)
	}
	if f.bodies[0]["categoryId"] != "c-leaf" {
		t.Errorf("카테고리 %v", f.bodies[0]["categoryId"])
	}
	if err := c.WaitReady(ctx, &p, time.Second); err != nil || !p.Ready {
		t.Fatalf("준비를 기다려야 한다: %v", err)
	}
	if f.statusAsk != 2 {
		t.Errorf("completed 가 될 때까지 물어야 한다: %d번", f.statusAsk)
	}

	// 상품마다 새 키를 쓴다. 같은 키면 be 가 첫 등록을 돌려준다
	if _, err := c.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	if f.keys[0] == f.keys[1] {
		t.Error("상품마다 다른 멱등 키여야 한다")
	}
}

func TestWaitTimeout(t *testing.T) {
	f := &fakeAPI{token: "x", polls: 1 << 30}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "x", Poll: time.Millisecond}
	p := Product{Label: "시험", ID: "id", IdempotencyKey: "k"}
	if err := c.WaitReady(context.Background(), &p, 20*time.Millisecond); err == nil {
		t.Fatal("준비되지 않으면 실패해야 한다")
	}
}

// 실패 메시지에는 be 오류 코드와 칸이 나오고, 관리자 토큰은 나오지 않는다
func TestErrorsShowCodeNotToken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
		f     *fakeAPI
		want  string
	}{
		{"거절", "secret-admin-token", &fakeAPI{token: "secret-admin-token", reject: true}, "VALIDATION_FAILED"},
		{"인증", "secret-admin-token", &fakeAPI{token: "other"}, "UNAUTHORIZED"},
	} {
		srv := httptest.NewServer(tc.f.handler(t))
		c := &Client{BaseURL: srv.URL, Token: tc.token}
		_, err := c.Create(context.Background(), spec())
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v 에 %s 가 있어야 한다", tc.name, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), tc.token) {
			t.Errorf("%s: 오류에 토큰이 나왔다: %v", tc.name, err)
		}
		if !strings.Contains(err.Error(), "Idempotency-Key loadtest-") {
			t.Errorf("%s: 다시 보낼 수 있게 멱등 키를 알려야 한다: %v", tc.name, err)
		}
	}
}

// 공개 카테고리 조회에는 관리자 토큰을 싣지 않는다
func TestTokenOnlyOnAdmin(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		io.WriteString(w, `{"success":true,"data":{"items":[{"categoryId":"c","children":[]}]}}`)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "secret"}
	if _, err := c.Category(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("공개 API 에 토큰을 실었다: %q", got)
	}
}

func TestFirstLeafFallback(t *testing.T) {
	tree := []category{{CategoryID: "top", Name: "모바일", Children: []category{{CategoryID: "phone", Name: "스마트폰"}}}}
	if id := firstLeaf(tree, "이어폰"); id != "" {
		t.Errorf("이어폰 카테고리가 없으면 비어야 한다: %q", id)
	}
	if id := firstLeaf(tree, ""); id != "phone" {
		t.Errorf("하위가 없는 첫 카테고리여야 한다: %q", id)
	}
}

func jwtOf(claims string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","typ":"at+jwt"}`)) + "." + enc([]byte(claims)) + ".c2ln"
}

func TestToken(t *testing.T) {
	good := jwtOf(fmt.Sprintf(`{"sub":"admin","role":"ADMIN","type":"ACCESS","exp":%d}`, now.Add(time.Hour).Unix()))
	for _, raw := range []string{good, "Bearer " + good, "bearer  " + good, `"` + good + `"`, good + "\n", "  " + good + "  "} {
		got, err := Token(raw, now)
		if err != nil || got != good {
			t.Errorf("%q 를 다듬지 못했다: %v", raw, err)
		}
	}
	for name, raw := range map[string]string{
		"빈 값":    "  ",
		"JWT 아님": "abc.def",
		"회원 토큰":  jwtOf(fmt.Sprintf(`{"role":"USER","exp":%d}`, now.Add(time.Hour).Unix())),
		"만료":     jwtOf(fmt.Sprintf(`{"role":"ADMIN","exp":%d}`, now.Add(-time.Minute).Unix())),
		"리프레시":   jwtOf(fmt.Sprintf(`{"role":"ADMIN","type":"REFRESH","exp":%d}`, now.Add(time.Hour).Unix())),
	} {
		_, err := Token(raw, now)
		if err == nil {
			t.Errorf("%s: 거절해야 한다", name)
			continue
		}
		if strings.TrimSpace(raw) != "" && strings.Contains(err.Error(), strings.TrimSpace(raw)) {
			t.Errorf("%s: 오류에 토큰이 나왔다", name)
		}
	}
}

func TestCheckURL(t *testing.T) {
	for _, u := range []string{"https://api.grandis-nova.link", "http://127.0.0.1:18089", "http://localhost:8080", "http://[::1]:80"} {
		if err := CheckURL(u); err != nil {
			t.Errorf("%s 는 받아야 한다: %v", u, err)
		}
	}
	for _, u := range []string{"http://api.grandis-nova.link", "http://10.0.0.5", "ftp://x", "api.grandis-nova.link", ""} {
		if err := CheckURL(u); err == nil {
			t.Errorf("%s 로는 관리자 토큰을 보내지 않아야 한다", u)
		}
	}
}

// 원격 HTTP 주소면 요청을 보내기 전에 멈춘다(토큰이 나가지 않는다)
func TestNoTokenOverHTTP(t *testing.T) {
	sent := false
	c := &Client{BaseURL: "http://api.example.com", Token: "secret", HTTP: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		sent = true
		return nil, io.EOF
	})}}
	if _, err := c.Create(context.Background(), spec()); err == nil || sent {
		t.Fatalf("보내지 않고 실패해야 한다: sent=%v err=%v", sent, err)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
