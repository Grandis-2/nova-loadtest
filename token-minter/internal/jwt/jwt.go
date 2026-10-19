// Package jwt는 시험 회원의 액세스 토큰을 member와 같은 형식으로 서명한다.
//
// 헤더는 alg=RS256, typ=at+jwt, kid(JWT_KEY_ID 그대로)이고, 클레임은 iss, aud(배열), sub, sid, jti,
// role, type, iat, exp이다(be common/security JwtTokenProvider와 같다).
// role은 USER, type은 ACCESS로 고정하고 바꿀 수 있는 입력을 두지 않는다. 관리자 토큰을 만들 길을 막으려는 것이다.
package jwt

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

const (
	role      = "USER"
	tokenType = "ACCESS"
	typ       = "at+jwt"
	minBits   = 2048
)

// ParseKey는 PKCS#8 PEM(-----BEGIN PRIVATE KEY-----) 문자열에서 RSA 개인키를 읽는다.
// 다른 형식, RSA가 아닌 키, 2048비트보다 작은 키는 거절한다. 오류에 키 내용을 넣지 않는다.
func ParseKey(s string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("서명키가 PEM이 아니다")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("서명키는 PKCS#8(PRIVATE KEY)이어야 한다(받은 형식: %s)", block.Type)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("서명키를 PKCS#8로 읽지 못했다")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("서명키가 RSA가 아니다")
	}
	if rk.N.BitLen() < minBits {
		return nil, fmt.Errorf("서명키가 %d비트다(%d비트 이상이어야 한다)", rk.N.BitLen(), minBits)
	}
	return rk, nil
}

// Signer는 토큰을 만든다.
type Signer struct {
	key           *rsa.PrivateKey
	kid, iss, aud string
	header        string // 미리 인코딩한 헤더
}

// NewSigner는 kid, iss, aud가 모두 있어야 만든다. 하나라도 비면 member와 어긋난 토큰이 나오기 때문이다.
func NewSigner(key *rsa.PrivateKey, kid, iss, aud string) (*Signer, error) {
	if key == nil {
		return nil, errors.New("서명키가 없다")
	}
	var missing []string
	for _, v := range []struct{ name, val string }{{"JWT_KEY_ID", kid}, {"JWT_ISSUER", iss}, {"JWT_AUDIENCE", aud}} {
		if v.val == "" {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%v가 비었다", missing)
	}
	h, err := json.Marshal(map[string]string{"alg": "RS256", "typ": typ, "kid": kid})
	if err != nil {
		return nil, err
	}
	return &Signer{key: key, kid: kid, iss: iss, aud: aud, header: enc(h)}, nil
}

type claims struct {
	Iss  string   `json:"iss"`
	Aud  []string `json:"aud"`
	Sub  string   `json:"sub"`
	Sid  string   `json:"sid"`
	Jti  string   `json:"jti"`
	Role string   `json:"role"`
	Type string   `json:"type"`
	Iat  int64    `json:"iat"`
	Exp  int64    `json:"exp"`
}

// Sign은 sub(customers.id UUID)의 액세스 토큰을 만든다. sid와 jti는 토큰마다 새 UUID다.
func (s *Signer) Sign(sub string, now time.Time, ttl time.Duration) (string, error) {
	if sub == "" || ttl <= 0 {
		return "", errors.New("sub와 유효 시간이 필요하다")
	}
	sid, err := uuidV4()
	if err != nil {
		return "", err
	}
	jti, err := uuidV4()
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims{
		Iss: s.iss, Aud: []string{s.aud}, Sub: sub, Sid: sid, Jti: jti,
		Role: role, Type: tokenType, Iat: now.Unix(), Exp: now.Add(ttl).Unix(),
	})
	if err != nil {
		return "", err
	}
	input := s.header + "." + enc(c)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return input + "." + enc(sig), nil
}

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func uuidV4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
