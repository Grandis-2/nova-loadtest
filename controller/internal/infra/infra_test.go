package infra

import (
	"slices"
	"strings"
	"testing"
)

// nova-infra 시험 스택 ssm.tf 가 만드는 모양(ID는 가짜)
const runTask = `{
  "cluster": "nova-prod-loadtest",
  "bucket": "nova-prod-loadtest-abc123",
  "tasks": {
    "controller":  {"taskDefinition": "nova-prod-loadtest-controller", "container": "controller", "subnets": ["subnet-app1"], "securityGroups": ["sg-ctl"], "assignPublicIp": "DISABLED"},
    "tokenMinter": {"taskDefinition": "nova-prod-loadtest-token-minter", "container": "token-minter", "subnets": ["subnet-app1"], "securityGroups": ["sg-minter"], "assignPublicIp": "DISABLED"},
    "k6":          {"taskDefinition": "nova-prod-loadtest-k6", "container": "k6", "subnets": ["subnet-pub1", "subnet-pub2"], "securityGroups": ["sg-k6"], "assignPublicIp": "ENABLED"}
  }
}`

const observability = `{
  "otlpEndpoint": "otel-ingest.nova.local:4318",
  "mimir": {"cluster": "nova-prod-observability", "service": "nova-prod-obs", "port": 9009, "queryPath": "/prometheus/api/v1/query"}
}`

func TestParseRunTask(t *testing.T) {
	r, err := ParseRunTask(runTask, ModeRun)
	if err != nil {
		t.Fatal(err)
	}
	if r.Tasks["k6"].AssignPublicIP != "ENABLED" || len(r.Tasks["k6"].Subnets) != 2 {
		t.Fatalf("%+v", r.Tasks["k6"])
	}
	// 정리 모드에는 tasks.data 가 필요하다(인프라 I-6 전에는 없다)
	if _, err := ParseRunTask(runTask, ModeCleanup); err == nil || !strings.Contains(err.Error(), "tasks.data") {
		t.Fatalf("tasks.data 가 없으면 정리 모드를 거절해야 한다: %v", err)
	}
	for _, tc := range []struct{ name, from, to, want string }{
		{"클러스터 없음", `"cluster": "nova-prod-loadtest",`, ``, "cluster"},
		{"k6 없음", `"k6":`, `"k7":`, "tasks.k6"},
		{"서브넷 없음", `["subnet-pub1", "subnet-pub2"]`, `[]`, "subnets"},
		{"공인 IP 값", `"ENABLED"`, `"YES"`, "assignPublicIp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseRunTask(strings.Replace(runTask, tc.from, tc.to, 1), ModeRun); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("거절해야 한다(%q): %v", tc.want, err)
			}
		})
	}
	if _, err := ParseRunTask("", ModeRun); err == nil {
		t.Fatal("빈 값을 거절해야 한다")
	}
}

func TestParseObservability(t *testing.T) {
	o, err := ParseObservability(observability)
	if err != nil {
		t.Fatal(err)
	}
	if o.OTLPEndpoint != "otel-ingest.nova.local:4318" || o.Mimir.Port != 9009 {
		t.Fatalf("%+v", o)
	}
	for _, tc := range []struct{ name, from, to, want string }{
		{"수집 주소 없음", `"otlpEndpoint": "otel-ingest.nova.local:4318",`, ``, "otlpEndpoint"},
		{"scheme 붙음", `"otel-ingest.nova.local:4318"`, `"http://otel-ingest.nova.local:4318"`, "scheme"},
		{"포트 없음", `"otel-ingest.nova.local:4318"`, `"otel-ingest.nova.local"`, "host:port"},
		{"조회 경로", `"/prometheus/api/v1/query"`, `"prometheus/api/v1/query"`, "queryPath"},
		{"Mimir 포트", `"port": 9009`, `"port": 0`, "mimir.port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseObservability(strings.Replace(observability, tc.from, tc.to, 1)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("거절해야 한다(%q): %v", tc.want, err)
			}
		})
	}
}

// 버킷 이름, 서브넷 ID, 보안 그룹 ID는 가리고, 문서에 공개된 이름은 그대로 둔다
func TestSecretValues(t *testing.T) {
	got, err := SecretValues(runTask)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"nova-prod-loadtest-abc123", "subnet-app1", "subnet-pub1", "subnet-pub2", "sg-ctl", "sg-minter", "sg-k6"} {
		if !slices.Contains(got, v) {
			t.Errorf("%s를 가려야 한다", v)
		}
	}
	for _, v := range []string{"nova-prod-loadtest", "nova-prod-loadtest-k6", "k6", "ENABLED", "DISABLED"} {
		if slices.Contains(got, v) {
			t.Errorf("%s는 가리지 않는다(역할 이름까지 ***로 바뀐다)", v)
		}
	}
	obs, _ := SecretValues(observability)
	if len(obs) != 0 {
		t.Fatalf("관측 값은 모두 공개된 이름이다: %v", obs)
	}
	// 인프라가 나중에 더하는 키는 기본으로 가린다
	more, _ := SecretValues(`{"cluster": "c", "newKey": "secret-value", "tasks": {"data": {"subnets": ["subnet-x"], "container": "data"}}}`)
	if !slices.Contains(more, "secret-value") || !slices.Contains(more, "subnet-x") || slices.Contains(more, "data") {
		t.Fatalf("새 키는 가리고 container 는 두어야 한다: %v", more)
	}
}
