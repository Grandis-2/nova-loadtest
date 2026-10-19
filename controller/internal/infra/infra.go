// Package infra는 워크플로가 넘기는 인프라 값(SSM 두 파라미터의 JSON)을 읽고 검사한다.
// 형식은 nova-infra 시험 스택의 ssm.tf와 같다(docs/configuration.md "SSM 파라미터").
//
//	RUN_TASK       /nova-prod/loadtest/run-task
//	OBSERVABILITY  /nova-prod/loadtest/observability
//
// 값이 빠졌거나 형식이 틀리면 아무 태스크도 띄우기 전에 실패한다.
package infra

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// Task는 태스크 하나를 띄우는 데 필요한 값이다. RunTask에 그대로 쓴다.
type Task struct {
	TaskDefinition string   `json:"taskDefinition"` // 패밀리 이름. 최신 개정판이 뜬다
	Container      string   `json:"container"`      // containerOverrides의 name
	Subnets        []string `json:"subnets"`
	SecurityGroups []string `json:"securityGroups"`
	AssignPublicIP string   `json:"assignPublicIp"` // ENABLED, DISABLED
}

// RunTask는 /nova-prod/loadtest/run-task의 값이다.
type RunTask struct {
	Cluster string          `json:"cluster"`
	Bucket  string          `json:"bucket"`
	Tasks   map[string]Task `json:"tasks"` // controller, tokenMinter, k6, (나중에) data
}

// Mimir는 Mimir를 찾을 곳이다. 고정 주소가 없어 서비스의 RUNNING 태스크 IP로 조회한다.
type Mimir struct {
	Cluster   string `json:"cluster"`
	Service   string `json:"service"`
	Port      int    `json:"port"`
	QueryPath string `json:"queryPath"`
}

// Observability는 /nova-prod/loadtest/observability의 값이다.
type Observability struct {
	OTLPEndpoint string `json:"otlpEndpoint"` // scheme 없는 host:port
	Mimir        Mimir  `json:"mimir"`
}

// Mode는 컨트롤러가 하는 일이다.
type Mode string

const (
	ModeRun     Mode = "run"     // 시험 실행
	ModeCleanup Mode = "cleanup" // 시험 데이터 정리(cleanup.yml)
)

// ParseRunTask는 RUN_TASK를 읽고 모드에 필요한 태스크가 있는지 본다.
func ParseRunTask(raw string, mode Mode) (*RunTask, error) {
	var r RunTask
	if err := strictJSON(raw, &r); err != nil {
		return nil, fmt.Errorf("RUN_TASK: %w", err)
	}
	var errs []string
	if r.Cluster == "" {
		errs = append(errs, "cluster가 없다")
	}
	if r.Bucket == "" {
		errs = append(errs, "bucket이 없다")
	}
	need := []string{"tokenMinter", "k6"}
	if mode == ModeCleanup {
		need = []string{"data"}
	}
	for _, name := range need {
		t, ok := r.Tasks[name]
		if !ok {
			errs = append(errs, fmt.Sprintf("tasks.%s가 없다", name))
			continue
		}
		errs = append(errs, t.check(name)...)
	}
	if len(errs) > 0 {
		return nil, errors.New("RUN_TASK: " + strings.Join(errs, ", "))
	}
	return &r, nil
}

func (t Task) check(name string) []string {
	var errs []string
	if t.TaskDefinition == "" || t.Container == "" {
		errs = append(errs, fmt.Sprintf("tasks.%s의 taskDefinition, container가 필요하다", name))
	}
	if len(t.Subnets) == 0 || len(t.SecurityGroups) == 0 {
		errs = append(errs, fmt.Sprintf("tasks.%s의 subnets, securityGroups가 필요하다", name))
	}
	if t.AssignPublicIP != "ENABLED" && t.AssignPublicIP != "DISABLED" {
		errs = append(errs, fmt.Sprintf("tasks.%s.assignPublicIp는 ENABLED나 DISABLED여야 한다", name))
	}
	return errs
}

// ParseObservability는 OBSERVABILITY를 읽고 검사한다.
func ParseObservability(raw string) (*Observability, error) {
	var o Observability
	if err := strictJSON(raw, &o); err != nil {
		return nil, fmt.Errorf("OBSERVABILITY: %w", err)
	}
	var errs []string
	// k6의 K6_OTEL_HTTP_EXPORTER_ENDPOINT는 scheme 없는 host:port를 받는다. http://가 붙으면 k6가 보내지 못한다
	if strings.Contains(o.OTLPEndpoint, "://") {
		errs = append(errs, "otlpEndpoint에 scheme을 붙이지 않는다(host:port)")
	} else if host, port, err := net.SplitHostPort(o.OTLPEndpoint); err != nil || host == "" || !validPort(port) {
		errs = append(errs, "otlpEndpoint는 host:port여야 한다")
	}
	m := o.Mimir
	if m.Cluster == "" || m.Service == "" {
		errs = append(errs, "mimir.cluster, mimir.service가 필요하다")
	}
	if m.Port <= 0 || m.Port > 65535 {
		errs = append(errs, "mimir.port는 1~65535여야 한다")
	}
	if !strings.HasPrefix(m.QueryPath, "/") {
		errs = append(errs, "mimir.queryPath는 /로 시작해야 한다")
	}
	if len(errs) > 0 {
		return nil, errors.New("OBSERVABILITY: " + strings.Join(errs, ", "))
	}
	return &o, nil
}

func validPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n > 0 && n <= 65535
}

func strictJSON(raw string, v any) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("값이 없다")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("JSON이 아니다: %w", err)
	}
	return nil
}

// unmasked는 로그에 그대로 둬도 되는 값의 경로다. 이 저장소 문서에 이미 공개된 이름이다.
// 나머지 값(버킷 이름, 서브넷 ID, 보안 그룹 ID, 나중에 더해질 키)은 모두 가린다(docs/design.md "인프라 값").
// tasks.*.<키>는 태스크 이름과 관계없이 키로 본다.
func unmasked(path string) bool {
	switch path {
	case "cluster", "otlpEndpoint", "mimir.cluster", "mimir.service", "mimir.port", "mimir.queryPath":
		return true
	}
	if parts := strings.Split(path, "."); len(parts) == 3 && parts[0] == "tasks" {
		switch parts[2] {
		case "taskDefinition", "container", "assignPublicIp":
			return true
		}
	}
	return false
}

// SecretValues는 JSON에서 가려야 할 값을 모두 꺼낸다. 워크플로가 이 값마다 ::add-mask:: 를 낸다.
// 여러 줄 JSON을 통째로 가리면 값 일부가 따로 찍힐 때 가려지지 않으므로 값을 하나씩 꺼낸다.
func SecretValues(raw string) ([]string, error) {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				child := k
				if path != "" {
					child = path + "." + k
				}
				walk(child, t[k])
			}
		case []any:
			// 배열의 원소는 배열 경로로 판단한다(subnets, securityGroups)
			for _, c := range t {
				walk(path, c)
			}
		case nil:
		default:
			s := fmt.Sprint(t)
			if s == "" || unmasked(path) || seen[s] {
				return
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	walk("", v)
	return out, nil
}
