package gha

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// githubToken 은 Actions 의 OIDC 토큰을 받는다. 역할을 맡을 때마다(세션이 끝나 갈 때마다) 새로 받는다.
type githubToken struct {
	url, bearer string
	client      *http.Client
}

func (g githubToken) GetIdentityToken() ([]byte, error) {
	u, err := url.Parse(g.url)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("audience", "sts.amazonaws.com")
	u.RawQuery = q.Encode()
	req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
	req.Header.Set("Authorization", "Bearer "+g.bearer)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OIDC 토큰 받기: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OIDC 토큰 받기: HTTP %d(작업에 permissions: id-token: write 가 있는지 본다)", resp.StatusCode)
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Value == "" {
		return nil, fmt.Errorf("OIDC 토큰 응답을 읽지 못했다: %v", err)
	}
	return []byte(body.Value), nil
}

// AWSConfig 는 AWS 설정이다. Actions OIDC 가 있고 역할 ARN 을 받으면 그 역할을 맡는다.
//
// 세션은 1시간인데 컨트롤러를 기다리는 단계 하나가 몇 시간 걸릴 수 있다. 단계 안에서는 configure-aws-credentials 를
// 다시 부를 수 없으므로, 자격 증명 캐시가 만료 10분 전에 OIDC 토큰을 새로 받아 역할을 다시 맡는다(docs/design.md "실행").
// 로컬 Forgejo 에서는 역할 ARN 을 주지 않고 moto 용 가짜 자격 증명(환경 변수)을 쓴다.
func AWSConfig(ctx context.Context, roleARN, sessionName string) (aws.Config, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return cfg, err
	}
	reqURL, bearer := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if roleARN == "" || reqURL == "" {
		return cfg, nil
	}
	tok := githubToken{url: reqURL, bearer: bearer, client: &http.Client{Timeout: 30 * time.Second}}
	p := stscreds.NewWebIdentityRoleProvider(sts.NewFromConfig(cfg), roleARN, tok, func(o *stscreds.WebIdentityRoleOptions) {
		o.RoleSessionName = sessionName
		o.Duration = time.Hour
	})
	cfg.Credentials = aws.NewCredentialsCache(p, func(o *aws.CredentialsCacheOptions) { o.ExpiryWindow = 10 * time.Minute })
	// 역할을 맡을 수 있는지 바로 확인한다(신뢰 조건이 틀리면 여기서 실패한다)
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return cfg, fmt.Errorf("역할을 맡지 못했다: %w", err)
	}
	return cfg, nil
}
