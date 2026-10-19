package jwt

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"regexp"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func pkcs8(t *testing.T, key any) string {
	t.Helper()
	b, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b}))
}

func rsaKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSignVerifiesLikeBe(t *testing.T) {
	key, err := ParseKey(pkcs8(t, rsaKey(t, 2048)))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSigner(key, "kid-1", "nova", "nova-api")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sub := "10ad7e57-0000-7000-8000-000000000042"
	tok, err := s.Sign(sub, now, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// be와 같은 규칙으로 독립 라이브러리가 검증한다: 서명, iss, aud, exp
	parsed, err := gojwt.Parse(tok, func(*gojwt.Token) (any, error) { return &key.PublicKey, nil },
		gojwt.WithValidMethods([]string{"RS256"}), gojwt.WithIssuer("nova"), gojwt.WithAudience("nova-api"),
		gojwt.WithExpirationRequired(), gojwt.WithIssuedAt())
	if err != nil {
		t.Fatalf("검증 실패: %v", err)
	}
	if parsed.Header["typ"] != "at+jwt" || parsed.Header["kid"] != "kid-1" {
		t.Fatalf("헤더가 다르다: %v", parsed.Header)
	}
	c := parsed.Claims.(gojwt.MapClaims)
	for _, name := range []string{"iss", "aud", "sub", "sid", "jti", "role", "type", "iat", "exp"} {
		if _, ok := c[name]; !ok {
			t.Errorf("필수 클레임 %s가 없다", name)
		}
	}
	if c["sub"] != sub || c["role"] != "USER" || c["type"] != "ACCESS" {
		t.Fatalf("클레임이 다르다: %v", c)
	}
	if aud, ok := c["aud"].([]any); !ok || len(aud) != 1 || aud[0] != "nova-api" {
		t.Fatalf("aud는 member처럼 배열이어야 한다: %v", c["aud"])
	}
	if !uuidRe.MatchString(c["sid"].(string)) || !uuidRe.MatchString(c["jti"].(string)) {
		t.Fatalf("sid와 jti는 UUID여야 한다: %v %v", c["sid"], c["jti"])
	}
	if exp := int64(c["exp"].(float64)); exp != now.Add(2*time.Hour).Unix() {
		t.Fatalf("exp %d", exp)
	}

	// sid와 jti는 토큰마다 새 값이다(폐기 확인에 걸리지 않게)
	tok2, _ := s.Sign(sub, now, time.Hour)
	p2, _, _ := gojwt.NewParser().ParseUnverified(tok2, gojwt.MapClaims{})
	c2 := p2.Claims.(gojwt.MapClaims)
	if c2["sid"] == c["sid"] || c2["jti"] == c["jti"] {
		t.Fatal("sid, jti가 토큰마다 달라야 한다")
	}
}

func TestParseKeyRejects(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	small := rsaKey(t, 1024)
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey(t, 2048))}))
	for _, tc := range []struct{ name, pem, want string }{
		{"PEM 아님", `{"key":"x"}`, "PEM"},
		{"PKCS#1", pkcs1, "PKCS#8"},
		{"EC 키", pkcs8(t, ec), "RSA"},
		{"1024비트", pkcs8(t, small), "2048"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseKey(tc.pem)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("거절해야 한다(%q): %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "BEGIN") {
				t.Fatal("오류에 키 내용을 넣으면 안 된다")
			}
		})
	}
}

func TestNewSignerNeedsAllValues(t *testing.T) {
	key := rsaKey(t, 2048)
	for _, tc := range []struct{ kid, iss, aud, want string }{
		{"", "nova", "nova-api", "JWT_KEY_ID"},
		{"k", "", "nova-api", "JWT_ISSUER"},
		{"k", "nova", "", "JWT_AUDIENCE"},
	} {
		if _, err := NewSigner(key, tc.kid, tc.iss, tc.aud); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s가 비면 실패해야 한다: %v", tc.want, err)
		}
	}
}
