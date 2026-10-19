package gha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
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

// credentialSource 는 기본 자격 증명(러너에 있는 자격 증명)으로 넘어가지 않게 막는다.
// GitHub 에서는 역할 ARN 이 있어야 하고, 역할 ARN 을 받았으면 OIDC 로만 맡는다.
func credentialSource(server, roleARN, reqURL string) error {
	if server == githubURL && roleARN == "" {
		return errors.New("AWS_ROLE_ARN 이 비었다(GitHub 비밀 AWS_RUN_ROLE_ARN, AWS_BUILD_ROLE_ARN 을 본다)")
	}
	if roleARN != "" && reqURL == "" {
		return errors.New("역할을 맡을 OIDC 토큰이 없다(작업에 permissions: id-token: write 가 있는지 본다)")
	}
	return nil
}

// AWSConfig 는 AWS 설정이다. Actions OIDC 가 있고 역할 ARN 을 받으면 그 역할을 맡는다.
//
// 세션은 1시간인데 컨트롤러를 기다리는 단계 하나가 몇 시간 걸릴 수 있다. 단계 안에서는 configure-aws-credentials 를
// 다시 부를 수 없으므로, 자격 증명 캐시가 만료 10분 전에 OIDC 토큰을 새로 받아 역할을 다시 맡는다(docs/design.md "실행").
// 로컬 Forgejo 에서는 역할 ARN 을 주지 않고 moto 용 가짜 자격 증명(환경 변수)을 쓴다.
//
// 설정을 만든 뒤 계정 ID 를 가린다(MaskAccount). 공개 저장소의 실행 로그에 계정 ID 가 남지 않게 한다.
func AWSConfig(ctx context.Context, out io.Writer, roleARN, sessionName string) (aws.Config, error) {
	// 역할을 맡다가 난 오류에도 계정 ID 가 들어 있을 수 있다. 역할 ARN 에서 계정 ID 를 꺼내 먼저 가린다
	if err := maskARNAccount(out, roleARN); err != nil {
		return aws.Config{}, err
	}
	cfg, err := awsConfig(ctx, roleARN, sessionName)
	if err != nil {
		return cfg, err
	}
	return cfg, MaskAccount(ctx, sts.NewFromConfig(cfg), out, os.Getenv("GITHUB_SERVER_URL") == githubURL)
}

var accountRe = regexp.MustCompile(`\b\d{12}\b`)

// maskARNAccount 는 ARN(arn:aws:iam::<계정>:role/...)의 계정 ID 를 가린다. ARN 이 아니면 아무것도 하지 않는다
func maskARNAccount(out io.Writer, arn string) error {
	parts := strings.Split(arn, ":")
	if len(parts) < 6 || !accountRe.MatchString(parts[4]) {
		return nil
	}
	return mask(out, parts[4])
}

func mask(out io.Writer, v string) error {
	if _, err := fmt.Fprintf(out, "::add-mask::%s\n", v); err != nil {
		return fmt.Errorf("로그 가리기 명령을 쓰지 못했다: %w", err)
	}
	return nil
}

// Redact 는 오류 메시지의 12자리 숫자(계정 ID)를 *** 로 바꾼다. 가리기 전에 난 오류를 찍을 때 쓴다
func Redact(s string) string { return accountRe.ReplaceAllString(s, "***") }

func awsConfig(ctx context.Context, roleARN, sessionName string) (aws.Config, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return cfg, err
	}
	reqURL, bearer := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if err := credentialSource(os.Getenv("GITHUB_SERVER_URL"), roleARN, reqURL); err != nil {
		return cfg, err
	}
	if roleARN == "" {
		return cfg, nil // 로컬 Forgejo: moto 용 가짜 자격 증명
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

// STS 는 MaskAccount 가 쓰는 API 다
type STS interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// MaskAccount 는 맡은 자격 증명의 계정 ID 를 ::add-mask:: 로 가린다. 태스크 ARN, AWS 오류 메시지(역할 ARN),
// 컨트롤러 로그 어디에 계정 ID 가 나와도 실행 로그에는 *** 로 남는다.
// GitHub 에서 계정 ID 를 알아내지 못하면 가리지 못한 채 계속하지 않고 실패한다(required).
func MaskAccount(ctx context.Context, c STS, out io.Writer, required bool) error {
	id, err := c.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil || aws.ToString(id.Account) == "" {
		if required {
			return fmt.Errorf("계정 ID 를 알아내지 못해 로그에서 가릴 수 없다: %s", Redact(fmt.Sprint(err)))
		}
		return nil // 로컬(moto)
	}
	return mask(out, aws.ToString(id.Account))
}
