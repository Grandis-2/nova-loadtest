package gha

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/Grandis-2/nova-loadtest/config"
	"github.com/Grandis-2/nova-loadtest/controller/internal/plan"
)

// 시나리오별 워크플로(loadtest-<시나리오>.yml)는 입력만 다르고 공통 실행(_loadtest.yml)을 부른다.
// 손으로 나눠 쓴 파일이라, 입력을 빠뜨리거나 다른 시나리오 이름을 넘기는 실수를 여기서 잡는다.

const workflowDir = "../../../.github/workflows"

type workflowInput struct {
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
}

type workflowFile struct {
	On struct {
		Dispatch *struct {
			Inputs map[string]workflowInput `yaml:"inputs"`
		} `yaml:"workflow_dispatch"`
		Call *struct {
			Inputs map[string]workflowInput `yaml:"inputs"`
		} `yaml:"workflow_call"`
	} `yaml:"on"`
	Concurrency struct {
		Group            string `yaml:"group"`
		CancelInProgress bool   `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
	Jobs map[string]struct {
		Uses        string            `yaml:"uses"`
		Secrets     string            `yaml:"secrets"`
		Permissions map[string]string `yaml:"permissions"`
		With        map[string]string `yaml:"with"`
	} `yaml:"jobs"`
}

func readWorkflow(t *testing.T, name string) workflowFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(workflowDir, name))
	if err != nil {
		t.Fatal(err)
	}
	var w workflowFile
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return w
}

func TestScenarioWorkflows(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	common := readWorkflow(t, "_loadtest.yml")
	if common.On.Call == nil || common.On.Dispatch != nil {
		t.Fatal("_loadtest.yml 은 workflow_call 로만 불려야 한다")
	}
	if common.Concurrency.Group != "" {
		t.Fatal("_loadtest.yml 에 concurrency 를 두면 부르는 실행과 같은 그룹이라 교착으로 취소된다")
	}

	// 시나리오마다 파일이 하나 있고, 시나리오가 아닌 loadtest-*.yml 은 없다
	files, _ := filepath.Glob(filepath.Join(workflowDir, "loadtest-*.yml"))
	var got []string
	for _, f := range files {
		got = append(got, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "loadtest-"), ".yml"))
	}
	slices.Sort(got)
	want := slices.Sorted(slices.Values(config.ScenarioOrder))
	if !slices.Equal(got, want) {
		t.Fatalf("시나리오별 워크플로 %v 가 시나리오 %v 와 같아야 한다", got, want)
	}

	defaultRe := regexp.MustCompile(`기본 (\S+)$`)
	for _, name := range config.ScenarioOrder {
		t.Run(name, func(t *testing.T) {
			w := readWorkflow(t, "loadtest-"+name+".yml")
			if w.On.Dispatch == nil {
				t.Fatal("workflow_dispatch 로 실행해야 한다")
			}
			if w.Concurrency.Group != "loadtest" || w.Concurrency.CancelInProgress {
				t.Fatalf("모든 시나리오가 concurrency 그룹 loadtest 를 쓰고 진행 중인 시험을 취소하지 않아야 한다: %+v", w.Concurrency)
			}
			if len(w.Jobs) != 1 {
				t.Fatalf("작업은 공통 실행을 부르는 하나여야 한다: %d개", len(w.Jobs))
			}
			for _, job := range w.Jobs {
				if job.Uses != "./.github/workflows/_loadtest.yml" || job.Secrets != "inherit" || job.Permissions["id-token"] != "write" {
					t.Fatalf("공통 실행을 비밀과 OIDC 권한과 함께 불러야 한다: uses=%q secrets=%q permissions=%v", job.Uses, job.Secrets, job.Permissions)
				}
				if job.With["scenario"] != name {
					t.Fatalf("scenario 는 %q 여야 한다: %q", name, job.With["scenario"])
				}
				for k := range job.With {
					if _, ok := w.On.Dispatch.Inputs[k]; !ok && k != "scenario" {
						t.Errorf("입력에 없는 %s 를 넘긴다", k)
					}
				}
				for k := range w.On.Dispatch.Inputs {
					if _, ok := common.On.Call.Inputs[k]; !ok {
						t.Errorf("공통 실행에 없는 입력 %s", k)
					}
					if v := job.With[k]; v != "${{ inputs."+k+" }}" {
						t.Errorf("입력 %s 를 그대로 넘겨야 한다: %q", k, v)
					}
				}
			}

			// 설명에 적은 기본값이 scenarios.yml 과 같은가
			sc := cfg.Scenarios[name]
			for k, in := range w.On.Dispatch.Inputs {
				m := defaultRe.FindStringSubmatch(in.Description)
				if m == nil {
					continue
				}
				var ok bool
				switch k {
				case "peak_rps":
					ok = m[1] == strconv.FormatFloat(sc.PeakRPS, 'f', -1, 64)
				case "duration", "ramp_up":
					d, err := time.ParseDuration(m[1])
					ok = err == nil && (k == "duration" && d == sc.Duration.D() || k == "ramp_up" && d == sc.RampUp.D())
				case "abort_error_rate":
					ok = m[1] == strconv.FormatFloat(cfg.SLO.AbortErrorRate, 'f', -1, 64)
				default:
					t.Errorf("%s: 설명의 기본값을 검사하지 못한다", k)
					continue
				}
				if !ok {
					t.Errorf("%s 설명의 기본값 %s 가 scenarios.yml 과 다르다", k, m[1])
				}
			}

			// 보이는 입력은 이 시나리오가 쓰는 입력이고, 숨긴 입력은 계획이 "쓰지 않는다"로 거절하는 입력이다.
			// 숨긴 입력을 계획이 받아 쓰면 그 기능을 화면에서 쓸 수 없게 된다
			now := time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)
			openAt := now.Add(30 * time.Minute).Format(time.RFC3339)
			base := plan.Input{TestID: "lt-20261020t0600-1", Scenario: name, Now: now,
				ProductID: "a1b2c3d4-0000-4000-8000-000000000001"}
			if w.On.Dispatch.Inputs["open_at"].Required {
				base.OpenAt = openAt
			}
			for k := range common.On.Call.Inputs {
				in := base
				switch k {
				case "prep_product_id":
					// open_at 을 넣은 spike 도 받도록 다른 상품을 준다
					in.PrepProductID = "a1b2c3d4-0000-4000-8000-000000000002"
					in.OpenAt = openAt
				case "open_at":
					in.OpenAt = openAt
					if name == "spike" {
						in.PrepProductID = "a1b2c3d4-0000-4000-8000-000000000002"
					}
				case "duration":
					in.Duration = "20m"
				case "ramp_up":
					in.RampUp = "2m"
				case "flows":
					in.Flows = []string{"entry"}
				default:
					continue // 모든 시나리오가 쓰거나(scenario, product_id, peak_rps, runners, abort_error_rate) 따로 정한다
				}
				unused := ""
				if _, err := plan.New(cfg, in); err != nil {
					// 거절 문구는 "<시나리오>에는 <입력>를 쓰지 않는다" 꼴이다
					for _, line := range strings.Split(err.Error(), "\n") {
						if strings.Contains(line, "에는 "+k) && strings.Contains(line, "쓰지 않는다") {
							unused = line
						}
					}
				}
				_, shown := w.On.Dispatch.Inputs[k]
				switch {
				case shown && unused != "":
					t.Errorf("%s 는 이 시나리오에서 쓰지 않는 입력이라 보이지 않아야 한다: %s", k, unused)
				case !shown && unused == "":
					t.Errorf("%s 는 계획이 받아 쓰는 입력이라 보여야 한다", k)
				}
			}
		})
	}
}
