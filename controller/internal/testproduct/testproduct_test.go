package testproduct

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/png"
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
	early.OpensAt = now.Add(12 * time.Minute)
	if err := early.Check(now); err == nil {
		t.Fatal("13분 안의 오픈은 당길 수 없으므로(be 는 10분 뒤부터) 미리 막아야 한다")
	}
	closed := spec()
	closed.ClosesAt = closed.OpensAt
	if err := closed.Check(now); err == nil {
		t.Fatal("마감이 오픈과 같으면 막아야 한다")
	}
}

// be 는 모르는 칸을 400 으로 거절한다. 계약(ProductRegistrationRequest)에 있는 칸만 보낸다
func TestBodyFields(t *testing.T) {
	b := Body(spec(), now)
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
	if got := Body(prep, now)["title"]; got != "[부하 시험] 무선 이어폰 LT 1020-0640 (준비)" {
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
	busy      int // 등록 상태 조회가 처음 몇 번 503 인가(preorder 가 답하지 않는다)
	keys      []string
	bodies    []map[string]any
	statusAsk int
	campaigns []map[string]any // 회차 당기기 본문
	images    []string         // 올린 사진의 purpose 와 형식
	promos    []map[string]any
	promoKeys []string
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
			if f.statusAsk <= f.busy {
				w.WriteHeader(http.StatusServiceUnavailable)
				io.WriteString(w, `{"success":false,"error":{"code":"DEPENDENCY_UNAVAILABLE","message":"잠시 후 다시 시도해 주세요."}}`)
				return
			}
			ok(w, 200, map[string]any{"completed": f.statusAsk >= f.polls+f.busy})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/campaign"):
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.campaigns = append(f.campaigns, body)
			ok(w, 200, map[string]any{"productId": strings.Split(r.URL.Path, "/")[6], "opensAt": body["opensAt"], "closesAt": body["closesAt"]})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/images":
			file, hdr, err := r.FormFile("file")
			if err != nil {
				t.Errorf("사진 파트 file 이 없다: %v", err)
				w.WriteHeader(400)
				return
			}
			img, format, err := image.Decode(file)
			if err != nil || img.Bounds().Dx() < 1 {
				t.Errorf("사진을 읽지 못했다(%s): %v", hdr.Filename, err)
			}
			f.images = append(f.images, r.URL.Query().Get("purpose")+"/"+format)
			ok(w, 201, map[string]any{"imageUrl": "https://cdn.example/promotion/1.png"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/promotions":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.promos = append(f.promos, body)
			f.promoKeys = append(f.promoKeys, r.Header.Get("Idempotency-Key"))
			ok(w, 201, map[string]any{"promotionId": "0199F0E0-0000-7000-8000-0000000000AA", "status": "ONGOING"})
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
	c := &Client{BaseURL: srv.URL, Token: "admin-token", Poll: time.Millisecond, Now: func() time.Time { return now }}
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

// 원하는 오픈이 30분 안이면 30분 뒤로 등록하고, 준비가 끝난 뒤 회차를 당긴다. 일시 장애(503)는 다시 묻는다
func TestRegisterThenReschedule(t *testing.T) {
	f := &fakeAPI{token: "admin-token", polls: 1, busy: 2}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "admin-token", Poll: time.Millisecond, Now: func() time.Time { return now }}
	ctx := context.Background()
	s := spec()
	s.OpensAt = now.Add(15 * time.Minute)
	p, err := c.Create(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	reg := f.bodies[0]["campaign"].(map[string]any)["opensAt"]
	if reg != "2026-10-20T06:32:00Z" {
		t.Errorf("등록 오픈은 지금 + 31분 뒤의 다음 분이어야 한다: %v", reg)
	}
	if err := c.WaitReady(ctx, &p, time.Second); err != nil {
		t.Fatalf("503 은 다시 물어야 한다: %v", err)
	}
	if err := c.Reschedule(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if len(f.campaigns) != 1 || f.campaigns[0]["opensAt"] != "2026-10-20T06:15:00Z" || f.campaigns[0]["closesAt"] != "2026-10-20T18:00:00Z" {
		t.Errorf("회차 당기기 본문 %v", f.campaigns)
	}
	if !p.OpensAt.Equal(now.Add(15 * time.Minute)) {
		t.Errorf("당긴 오픈을 기억해야 한다: %s", p.OpensAt)
	}

	// 이미 원하는 오픈이면 당기지 않는다
	if err := c.Reschedule(ctx, &p); err != nil || len(f.campaigns) != 1 {
		t.Errorf("다시 당기지 않아야 한다: %v %d", err, len(f.campaigns))
	}
	// 당길 시간이 지났으면 등록한 오픈을 그대로 두고 알린다
	late := Product{ID: "x", OpensAt: now.Add(31 * time.Minute), Want: now.Add(10 * time.Minute), ClosesAt: now.Add(time.Hour)}
	if err := c.Reschedule(ctx, &late); err == nil || !late.OpensAt.Equal(now.Add(31*time.Minute)) {
		t.Errorf("10분 30초 안은 당기지 않아야 한다: %v %s", err, late.OpensAt)
	}
}

// 만든 상품을 홍보 하나에 묶는다. 사진은 PROMOTION 으로 새로 올리고, 기간은 오늘(한국 날짜)부터 가장 늦은 마감 날짜까지다
func TestCreatePromotion(t *testing.T) {
	f := &fakeAPI{token: "admin-token"}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "admin-token", Poll: time.Millisecond, Now: func() time.Time { return now }}
	ps := []Product{
		{ID: "p1", Want: now.Add(15 * time.Minute), ClosesAt: now.Add(12 * time.Hour)},
		{ID: "p2", Want: now.Add(35 * time.Minute), ClosesAt: now.Add(20 * time.Hour)},
	}
	promo, err := c.CreatePromotion(context.Background(), ps)
	if err != nil {
		t.Fatal(err)
	}
	if promo.ID != "0199f0e0-0000-7000-8000-0000000000aa" {
		t.Errorf("홍보 ID 는 소문자로: %s", promo.ID)
	}
	if len(f.images) != 1 || f.images[0] != "PROMOTION/png" {
		t.Errorf("홍보 사진은 purpose=PROMOTION 의 PNG 하나다: %v", f.images)
	}
	b := f.promos[0]
	allowed := []string{"name", "startDate", "endDate", "thumbnailUrl", "detailImageUrls", "productIds"}
	for k := range b {
		if !slices.Contains(allowed, k) {
			t.Errorf("계약에 없는 칸 %q", k)
		}
	}
	// 지금은 KST 15:00 이고, 늦은 마감은 UTC 2026-10-21 02:00 = KST 11:00 이다
	if b["startDate"] != "2026-10-20" || b["endDate"] != "2026-10-21" || promo.EndDate != "2026-10-21" {
		t.Errorf("홍보 기간 %v ~ %v", b["startDate"], b["endDate"])
	}
	if ids := fmt.Sprint(b["productIds"]); ids != "[p1 p2]" {
		t.Errorf("묶은 상품 %s", ids)
	}
	if b["thumbnailUrl"] != "https://cdn.example/promotion/1.png" {
		t.Errorf("썸네일은 올린 사진이어야 한다: %v", b["thumbnailUrl"])
	}
	if n := len([]rune(b["name"].(string))); n > 100 {
		t.Errorf("홍보 이름 %d자는 100자를 넘는다", n)
	}
	if !strings.HasPrefix(f.promoKeys[0], "loadtest-") {
		t.Errorf("홍보 만들기에 멱등 키가 있어야 한다: %q", f.promoKeys[0])
	}
}
