// Package testproduct 는 담당자의 관리자 세션으로 시험 상품(사전예약 상품, 회차, 배송 차수)과 그 상품을 묶은 사전예약(홍보)을 만든다.
//
// 관리자 토큰은 사람이 실행할 때만 넘기고 어디에도 남기지 않는다. 워크플로와 AWS 태스크는 이 패키지를 쓰지 않는다.
// 요청 형식은 be catalog AdminProductController, ProductRegistrationRequest, AdminPromotionController, AdminImageController,
// preorder AdminProductPreorderController(27cbb895)를 따른다.
//   - POST /api/v1/admin/products + Idempotency-Key : 201(새로 저장), 200(준비 끝난 등록의 재생), 202(준비 전 등록의 재생).
//     회차(campaign)와 배송 차수는 이 본문에 함께 보낸다. 회차는 상품마다 하나이고 회차 id 가 상품 id 다
//   - GET  /api/v1/admin/products/registrations/{key} : completed 가 true 면 preorder 가 회차와 차수를 만들었다.
//     catalog 가 preorder 에 물어 정하므로 preorder 가 답하지 않으면 503 이다(다시 묻는다)
//   - 등록의 오픈은 지금부터 30분 뒤(catalog.registration.min-open-lead)부터다. 등록한 뒤
//     PUT /api/v1/admin/preorders/products/{id}/campaign 으로 지금부터 10분 뒤(nova.campaign.min-lead-time)까지 당길 수 있다
//   - 고객 화면은 사전예약 상품을 홍보(/api/v1/promotions)로만 보여 준다. 상품 목록에는 접수 중일 때만 나온다.
//     그래서 만든 상품을 홍보 하나에 묶는다. 홍보 사진은 purpose=PROMOTION 으로 새로 올린 것만 쓸 수 있다
//   - 비공개 상품은 대기열이 줄을 열지 않는다(preorder PreorderCampaignChanged). 그래서 공개로 만든다
package testproduct

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Token 은 붙여 넣은 관리자 토큰을 다듬고, 보내기 전에 모양과 만료를 본다. be 는 헤더 모양이 조금만 어긋나도
// "자격 증명 없음"으로 보고 401 만 주므로(common/security BearerTokens), 흔한 실수를 여기서 먼저 알려 준다.
// 서명은 확인하지 않는다(키가 없다). 오류에 토큰 값은 넣지 않는다.
func Token(raw string, now time.Time) (string, error) {
	t := strings.TrimSpace(raw)
	t = strings.Trim(t, `"'`)
	if len(t) > 7 && strings.EqualFold(t[:7], "bearer ") {
		t = strings.TrimSpace(t[7:])
	}
	if t == "" {
		return "", errors.New("NOVA_ADMIN_TOKEN 이 비었다")
	}
	parts := strings.Split(t, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("NOVA_ADMIN_TOKEN 이 JWT 모양(점으로 나뉜 세 부분)이 아니다(%d부분). 로그인 응답의 sessionToken 값만 넣는다(쿠키 admin_refresh_token 은 아니다)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", errors.New("NOVA_ADMIN_TOKEN 의 본문을 읽지 못했다. 복사할 때 잘리거나 다른 글자가 섞이지 않았는지 본다")
	}
	var claims struct {
		Role string `json:"role"`
		Type string `json:"type"`
		Exp  int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", errors.New("NOVA_ADMIN_TOKEN 의 클레임을 읽지 못했다")
	}
	if claims.Role != "ADMIN" {
		return "", fmt.Errorf("NOVA_ADMIN_TOKEN 이 관리자 토큰이 아니다(role=%s). 관리자 화면에 로그인한 세션의 토큰을 넣는다", claims.Role)
	}
	if claims.Type != "" && claims.Type != "ACCESS" {
		return "", fmt.Errorf("NOVA_ADMIN_TOKEN 이 액세스 토큰이 아니다(type=%s)", claims.Type)
	}
	if exp := time.Unix(claims.Exp, 0); claims.Exp > 0 && !exp.After(now) {
		return "", fmt.Errorf("NOVA_ADMIN_TOKEN 이 %s 에 만료됐다. 관리자 화면을 새로 고쳐 새 토큰을 받는다", exp.Local().Format("15:04:05"))
	}
	return t, nil
}

// CheckURL 은 관리자 토큰을 보낼 주소를 본다. 원격 주소는 HTTPS 만 받고, 로컬 가짜 서버를 위해 루프백만 HTTP 를 허용한다.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("API 주소 %q 를 읽지 못했다", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if ip := net.ParseIP(u.Hostname()); u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("API 주소 %s 로는 관리자 토큰을 보내지 않는다. 원격 주소는 https 여야 한다(http 는 루프백만)", raw)
}

// RegisterLead 는 등록이 받는 가장 이른 오픈(지금 + 30분)이다. 요청이 가는 동안의 시간을 생각해 1분을 더 둔다
const RegisterLead = 31 * time.Minute

// MinOpenLead 는 회차를 당겨 열 수 있는 가장 이른 오픈이다. be 는 지금 + 10분부터 받는다. 등록과 준비를 기다리는 몇 분을 더 둔다
const MinOpenLead = 13 * time.Minute

// rescheduleLead 는 회차를 당길 때 지켜야 하는 오픈까지의 여유다(be 는 10분, 요청이 가는 시간 30초를 더한다)
const rescheduleLead = 10*time.Minute + 30*time.Second

// Spec 은 만들 상품 하나다.
type Spec struct {
	Label      string // spike 에서 상품 이름 끝에 붙는 쓰임새(준비, 진입). 상품이 하나면 비운다
	OpensAt    time.Time
	ClosesAt   time.Time
	CategoryID string
}

// RegisterOpens 는 등록 본문에 넣을 오픈이다. 원하는 오픈이 30분 안이면 30분 뒤로 등록하고, 준비가 끝난 뒤 당긴다(Reschedule)
func (s Spec) RegisterOpens(now time.Time) time.Time {
	if early := now.Add(RegisterLead).Truncate(time.Minute).Add(time.Minute); s.OpensAt.Before(early) {
		return early
	}
	return s.OpensAt
}

// Product 는 만든 상품이다. OpensAt 은 지금 회차의 오픈이고, Want 는 원하는 오픈이다(당기기 전에는 다를 수 있다)
type Product struct {
	Label          string
	ID             string
	IdempotencyKey string
	OpensAt        time.Time
	ClosesAt       time.Time
	Want           time.Time
	Ready          bool
}

// Promotion 은 만든 사전예약(홍보)이다.
type Promotion struct {
	ID             string
	IdempotencyKey string
	EndDate        string
}

// APIError 는 be 의 오류 응답이다. Retryable 은 503 이거나 details.retryable 이 true 인 409 처럼 같은 요청을 다시 보내도 되는 경우다
type APIError struct {
	Status    int
	Code      string
	Message   string
	Details   json.RawMessage
	Retryable bool
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d %s: %s %s", e.Status, e.Code, e.Message, e.Details)
}

func retryable(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Retryable
}

// Client 는 관리자 API 를 부른다. Token 은 관리자 세션 토큰(POST /api/v1/admin/session 의 sessionToken)이다.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	Now     func() time.Time
	// Poll 은 준비 확인 간격이다. 비면 2초
	Poll time.Duration
}

// Check 는 보내기 전에 회차 시각을 본다. be 가 거절할 값을 미리 걸러 관리자 API 를 헛되이 부르지 않는다.
func (s Spec) Check(now time.Time) error {
	if s.OpensAt.Before(now.Add(MinOpenLead)) {
		return fmt.Errorf("%s 상품의 오픈 %s 는 지금부터 %s 뒤 이후여야 한다(be 는 회차를 10분 뒤까지 당길 수 있다)",
			name(s.Label), s.OpensAt.Format(time.RFC3339), MinOpenLead)
	}
	if !s.ClosesAt.After(s.OpensAt) {
		return fmt.Errorf("%s 상품의 마감은 오픈 뒤여야 한다", name(s.Label))
	}
	return nil
}

// Body 는 등록 요청 본문이다. be 는 모르는 칸을 400 으로 거절하므로 계약에 있는 칸만 넣는다.
// 오픈은 RegisterOpens 다. 이름의 시각은 원하는 오픈이라 당긴 뒤에도 회차와 맞는다
func Body(s Spec, now time.Time) map[string]any {
	stamp := s.OpensAt.UTC().Format("0102-1504")
	shipStart := s.ClosesAt.UTC().AddDate(0, 0, 14)
	return map[string]any{
		"categoryId": s.CategoryID,
		"saleMode":   "PREORDER",
		"title":      title(s.Label, stamp),
		"brand":      "SAMSUNG",
		"modelName":  fmt.Sprintf("LT-%s", stamp),
		"description": "부하 시험에 쓰는 상품이다. 실제로 판매하지 않으며, 시험이 끝나면 사전예약을 끝내고 회차를 마감한 뒤 지운다.\n" +
			"노이즈 캔슬링, 최대 24시간 재생, IPX4 생활 방수.",
		"tags":         "loadtest",
		"visible":      true,
		"basePrice":    199000,
		"discountRate": 0,
		"warranty":     map[string]any{"offered": false, "surcharge": 0},
		"optionAxes": []any{map[string]any{
			"key":   "color",
			"label": "색상",
			"values": []any{
				map[string]any{"value": "블랙", "hex": "#1F1F1F"},
				map[string]any{"value": "화이트", "hex": "#F5F5F5"},
			},
		}},
		"campaign": map[string]any{
			"opensAt":  s.RegisterOpens(now).UTC().Format(time.RFC3339),
			"closesAt": s.ClosesAt.UTC().Format(time.RFC3339),
		},
		// 순번 1부터 끝까지 한 차수. 마지막 차수는 종료 순번을 비운다
		"shipmentBatches": []any{map[string]any{
			"batchNumber":        1,
			"positionFrom":       1,
			"estimatedShipStart": shipStart.Format(time.DateOnly),
			"estimatedShipEnd":   shipStart.AddDate(0, 0, 7).Format(time.DateOnly),
		}},
	}
}

// name 은 메시지에 쓰는 상품 이름이다
func name(label string) string {
	if label == "" {
		return "시험"
	}
	return label
}

func title(label, stamp string) string {
	t := "[부하 시험] 무선 이어폰 LT " + stamp
	if label != "" {
		t += " (" + label + ")"
	}
	return t
}

// Category 는 상품을 넣을 카테고리를 고른다. 공개 카테고리 트리에서 이름에 "이어폰"이 든 하위 카테고리를 고르고,
// 없으면 하위가 없는 첫 카테고리다.
func (c *Client) Category(ctx context.Context) (string, error) {
	var tree struct {
		Items []category `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/categories", nil, nil, &tree); err != nil {
		return "", fmt.Errorf("카테고리 조회: %w", err)
	}
	if id := firstLeaf(tree.Items, "이어폰"); id != "" {
		return id, nil
	}
	if id := firstLeaf(tree.Items, ""); id != "" {
		return id, nil
	}
	return "", errors.New("카테고리가 없다. -category-id 로 넣는다")
}

type category struct {
	CategoryID string     `json:"categoryId"`
	Name       string     `json:"name"`
	Children   []category `json:"children"`
}

// firstLeaf 는 이름에 word 가 든 첫 하위 카테고리다. word 가 비면 아무 이름이나 된다
func firstLeaf(cs []category, word string) string {
	for _, c := range cs {
		if len(c.Children) == 0 {
			if strings.Contains(c.Name, word) {
				return c.CategoryID
			}
			continue
		}
		if id := firstLeaf(c.Children, word); id != "" {
			return id
		}
	}
	return ""
}

// Create 는 상품 하나를 등록한다. 응답을 잃었을 때 같은 키로 다시 보낼 수 있게 키를 함께 돌려준다.
func (c *Client) Create(ctx context.Context, s Spec) (Product, error) {
	key, err := newKey()
	if err != nil {
		return Product{}, err
	}
	var out struct {
		Registration struct {
			ProductID string `json:"productId"`
			Completed bool   `json:"completed"`
		} `json:"registration"`
	}
	headers := map[string]string{"Idempotency-Key": key}
	// 보낸 오픈을 그대로 기억하도록 지금 시각은 한 번만 읽는다(재시도하는 동안 분이 바뀔 수 있다)
	now := c.now()
	body := Body(s, now)
	// 응답을 잃었거나 일시 장애면 같은 키로 다시 보낸다. be 는 같은 키의 등록을 재생한다
	err = c.retry(ctx, func() error { return c.do(ctx, http.MethodPost, "/api/v1/admin/products", headers, body, &out) })
	if err != nil {
		return Product{}, fmt.Errorf("%s 상품 등록(Idempotency-Key %s): %w", name(s.Label), key, err)
	}
	if out.Registration.ProductID == "" {
		return Product{}, fmt.Errorf("%s 상품 등록 응답에 productId 가 없다(Idempotency-Key %s)", name(s.Label), key)
	}
	return Product{
		Label:          s.Label,
		ID:             strings.ToLower(out.Registration.ProductID),
		IdempotencyKey: key,
		OpensAt:        s.RegisterOpens(now),
		ClosesAt:       s.ClosesAt,
		Want:           s.OpensAt,
		Ready:          out.Registration.Completed,
	}, nil
}

// retry 는 일시 장애(503, 다시 보내도 되는 409)를 1분 동안 다시 시도한다
func (c *Client) retry(ctx context.Context, f func() error) error {
	deadline := c.now().Add(time.Minute)
	for {
		err := f()
		if err == nil || !retryable(err) || c.now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.poll()):
		}
	}
}

func (c *Client) poll() time.Duration {
	if c.Poll == 0 {
		return 2 * time.Second
	}
	return c.Poll
}

// WaitReady 는 preorder 가 회차와 차수를 만들 때까지 기다린다(등록 이벤트를 받아 처리한다).
func (c *Client) WaitReady(ctx context.Context, p *Product, timeout time.Duration) error {
	deadline := c.now().Add(timeout)
	for !p.Ready {
		var st struct {
			Completed bool `json:"completed"`
		}
		err := c.do(ctx, http.MethodGet, "/api/v1/admin/products/registrations/"+p.IdempotencyKey, nil, nil, &st)
		if err != nil && !retryable(err) {
			return fmt.Errorf("%s 상품 준비 확인: %w", name(p.Label), err)
		}
		if p.Ready = err == nil && st.Completed; p.Ready {
			break
		}
		if c.now().After(deadline) {
			return fmt.Errorf("%s 상품(%s)이 %s 안에 준비되지 않았다. 관리자 화면에서 회차가 생겼는지 본다", name(p.Label), p.ID, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.poll()):
		}
	}
	return nil
}

// Reschedule 은 회차를 원하는 오픈(p.Want)으로 당긴다. 이미 원하는 오픈이면 아무것도 하지 않는다.
// 당길 시간이 모자라면(be 는 지금 + 10분부터 받는다) 등록한 오픈을 그대로 두고 오류를 돌려준다. 상품은 그 시각에 열려 쓸 수 있다
func (c *Client) Reschedule(ctx context.Context, p *Product) error {
	if p.Want.IsZero() || p.Want.Equal(p.OpensAt) {
		return nil
	}
	if p.Want.Before(c.now().Add(rescheduleLead)) {
		return fmt.Errorf("%s 상품의 회차를 %s 로 당길 시간이 지났다(지금부터 10분 뒤부터 받는다). %s 에 열린다",
			name(p.Label), p.Want.Format(time.RFC3339), p.OpensAt.Format(time.RFC3339))
	}
	var out struct {
		OpensAt time.Time `json:"opensAt"`
	}
	body := map[string]any{"opensAt": p.Want.UTC().Format(time.RFC3339), "closesAt": p.ClosesAt.UTC().Format(time.RFC3339)}
	path := "/api/v1/admin/preorders/products/" + p.ID + "/campaign"
	if err := c.retry(ctx, func() error { return c.do(ctx, http.MethodPut, path, nil, body, &out) }); err != nil {
		return fmt.Errorf("%s 상품의 회차 당기기: %w. %s 에 열린다", name(p.Label), err, p.OpensAt.Format(time.RFC3339))
	}
	p.OpensAt = p.Want
	if !out.OpensAt.IsZero() {
		p.OpensAt = out.OpensAt.UTC()
	}
	return nil
}

// kst 는 홍보 기간(한국 날짜)을 정할 때 쓴다. 이미지에 시간대 자료가 없어도 되게 고정 오프셋으로 둔다
var kst = time.FixedZone("KST", 9*60*60)

// CreatePromotion 은 만든 상품을 홍보 하나에 묶는다. 홍보 기간은 오늘(한국 날짜)부터 가장 늦은 마감 날짜까지다.
// 사진은 홍보마다 새로 올려야 한다(이미 쓴 사진은 400)
func (c *Client) CreatePromotion(ctx context.Context, ps []Product) (Promotion, error) {
	if len(ps) == 0 {
		return Promotion{}, errors.New("홍보에 묶을 상품이 없다")
	}
	thumb, err := c.uploadImage(ctx)
	if err != nil {
		return Promotion{}, fmt.Errorf("홍보 사진 올리기: %w", err)
	}
	key, err := newKey()
	if err != nil {
		return Promotion{}, err
	}
	ids := make([]string, 0, len(ps))
	end := ps[0].ClosesAt
	for _, p := range ps {
		ids = append(ids, p.ID)
		if p.ClosesAt.After(end) {
			end = p.ClosesAt
		}
	}
	endDate := end.In(kst).Format(time.DateOnly)
	body := map[string]any{
		"name":         "[부하 시험] 사전예약 LT " + ps[0].Want.UTC().Format("0102-1504"),
		"startDate":    c.now().In(kst).Format(time.DateOnly),
		"endDate":      endDate,
		"thumbnailUrl": thumb,
		"productIds":   ids,
	}
	var out struct {
		PromotionID string `json:"promotionId"`
	}
	headers := map[string]string{"Idempotency-Key": key}
	err = c.retry(ctx, func() error { return c.do(ctx, http.MethodPost, "/api/v1/admin/promotions", headers, body, &out) })
	if err != nil {
		return Promotion{}, fmt.Errorf("홍보 만들기(Idempotency-Key %s): %w", key, err)
	}
	if out.PromotionID == "" {
		return Promotion{}, fmt.Errorf("홍보 응답에 promotionId 가 없다(Idempotency-Key %s)", key)
	}
	return Promotion{ID: strings.ToLower(out.PromotionID), IdempotencyKey: key, EndDate: endDate}, nil
}

// uploadImage 는 홍보 썸네일로 쓸 단색 PNG 를 올리고 주소를 돌려준다
func (c *Client) uploadImage(ctx context.Context) (string, error) {
	var buf bytes.Buffer
	mp := multipart.NewWriter(&buf)
	part, err := mp.CreateFormFile("file", "loadtest.png")
	if err != nil {
		return "", err
	}
	if err := png.Encode(part, Thumbnail()); err != nil {
		return "", err
	}
	if err := mp.Close(); err != nil {
		return "", err
	}
	var out struct {
		ImageURL string `json:"imageUrl"`
	}
	raw := buf.Bytes()
	err = c.retry(ctx, func() error {
		return c.send(ctx, http.MethodPost, "/api/v1/admin/images?purpose=PROMOTION", mp.FormDataContentType(), nil, bytes.NewReader(raw), &out)
	})
	if err != nil {
		return "", err
	}
	if out.ImageURL == "" {
		return "", errors.New("사진 응답에 imageUrl 이 없다")
	}
	return out.ImageURL, nil
}

// Thumbnail 은 홍보 썸네일이다. be 는 RGB · 회색조 JPEG, PNG 만 받는다. 불투명한 단색이라 RGB 로 읽힌다
func Thumbnail() image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, 600, 600))
	c := color.NRGBA{R: 0x2B, G: 0x3A, B: 0x67, A: 0xFF}
	for y := range 600 {
		for x := range 600 {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// do 는 JSON 요청을 보내고 응답 봉투의 data 를 out 에 읽는다. 실패하면 *APIError 를 돌려준다(토큰은 넣지 않는다).
func (c *Client) do(ctx context.Context, method, path string, headers map[string]string, body, out any) error {
	if body == nil {
		return c.send(ctx, method, path, "", headers, nil, out)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.send(ctx, method, path, "application/json", headers, bytes.NewReader(b), out)
}

func (c *Client) send(ctx context.Context, method, path, contentType string, headers map[string]string, r io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "nova-loadtest/testproduct")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if strings.Contains(path, "/admin/") {
		if err := CheckURL(c.BaseURL); err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Details json.RawMessage `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode/100 != 2 {
		e := &APIError{Status: resp.StatusCode, Retryable: resp.StatusCode == http.StatusServiceUnavailable}
		if env.Error != nil {
			e.Code, e.Message, e.Details = env.Error.Code, env.Error.Message, env.Error.Details
			var d struct {
				Retryable bool `json:"retryable"`
			}
			if resp.StatusCode == http.StatusConflict && json.Unmarshal(env.Error.Details, &d) == nil && d.Retryable {
				e.Retryable = true
			}
		}
		if len(e.Details) == 0 {
			e.Details = json.RawMessage("null")
		}
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("응답을 읽지 못했다: %w", err)
	}
	return nil
}

func newKey() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "loadtest-" + hex.EncodeToString(b), nil
}
