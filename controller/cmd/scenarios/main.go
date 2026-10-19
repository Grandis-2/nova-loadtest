// scenarios 는 로컬 Forgejo 에서 3단계 각본(구현 계획 7.2)을 돌리고 결과를 확인한다. 운영에 닿지 않는다.
//
//	go run ./controller/cmd/scenarios              모든 각본(stress 제외)
//	go run ./controller/cmd/scenarios pass fail    고른 각본만
//	go run ./controller/cmd/scenarios -long        stress 한계 각본까지(30분 넘게 걸린다)
//
// local/forgejo/up.sh 로 띄운 Forgejo, moto(127.0.0.1:5000), fake-cluster(127.0.0.1:8081)를 쓴다.
// 각본마다 fake-cluster 의 각본을 바꾸고, 워크플로를 dispatch 하고, 도중에 개입하고(중지, 취소, 컨트롤러 종료),
// 끝나면 워크플로 결과, summary.json, fake ECS 가 받은 요청을 확인한다.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/Grandis-2/nova-loadtest/config"
	"github.com/Grandis-2/nova-loadtest/controller/internal/fakelocal"
	"github.com/Grandis-2/nova-loadtest/controller/internal/plan"
)

const (
	forgejo = "http://localhost:3000/api/v1/repos/loadtest/nova-loadtest"
	control = "http://localhost:8081"
	product = "a1b2c3d4-0000-4000-8000-000000000001"
	prep    = "a1b2c3d4-0000-4000-8000-000000000002"
)

// Case 는 각본 하나다
type Case struct {
	Name     string // fake-cluster 의 각본 이름
	Workflow string // 기본 loadtest.yml
	Inputs   map[string]string
	Long     bool
	// Forgejo 에서는 확인할 수 없는 각본. 이름을 줄 때만 돈다(까닭은 구현 계획 7.3)
	ForgejoGap bool

	Conclusion string // success, failure, cancelled
	Result     string // summary.json 의 result. "-" 면 summary.json 이 없어야 한다. 비면 보지 않는다

	Setup  func(r *runner) error          // dispatch 전
	During func(r *runner, run int) error // dispatch 뒤, 끝나기 전
	Check  func(r *runner, o *outcome) error
}

type outcome struct {
	run     int
	status  string
	testID  string
	summary map[string]any
	state   state
	logs    string
}

type state struct {
	Runs []struct {
		Family    string            `json:"Family"`
		StartedBy string            `json:"StartedBy"`
		Env       map[string]string `json:"Env"`
	} `json:"runs"`
	Stops   []string `json:"stops"`
	Running []string `json:"running"`
}

func (s state) runsOf(family string) int {
	n := 0
	for _, r := range s.Runs {
		if r.Family == family {
			n++
		}
	}
	return n
}

func smoke(extra map[string]string) map[string]string {
	in := map[string]string{"scenario": "smoke", "product_id": product, "duration": "2m"}
	for k, v := range extra {
		in[k] = v
	}
	return in
}

func cases() []Case {
	noRuns := func(_ *runner, o *outcome) error {
		if len(o.state.Runs) != 0 {
			return fmt.Errorf("RunTask 가 오지 않아야 한다: %d번", len(o.state.Runs))
		}
		return nil
	}
	allStopped := func(_ *runner, o *outcome) error {
		if len(o.state.Running) != 0 {
			return fmt.Errorf("남은 태스크가 없어야 한다: %v", o.state.Running)
		}
		return nil
	}
	return []Case{
		{Name: "pass", Inputs: smoke(map[string]string{"runners": "2"}), Conclusion: "success", Result: "pass", Check: func(r *runner, o *outcome) error {
			// 수집 주소와 태스크마다 다른 OTEL_RESOURCE_ATTRIBUTES 를 덮어쓰고, 고정값은 덮어쓰지 않는다
			seen := map[string]bool{}
			for _, run := range o.state.Runs {
				if run.Family != fakelocal.FamilyK6 {
					continue
				}
				if run.Env["K6_OTEL_HTTP_EXPORTER_ENDPOINT"] != "fake-cluster:4318" {
					return fmt.Errorf("수집 주소 덮어쓰기: %q", run.Env["K6_OTEL_HTTP_EXPORTER_ENDPOINT"])
				}
				if _, ok := run.Env["K6_OTEL_EXPORTER_PROTOCOL"]; ok {
					return errors.New("작업 정의의 고정값(전송 방식)을 덮어쓰지 않아야 한다")
				}
				seen[run.Env["OTEL_RESOURCE_ATTRIBUTES"]] = true
			}
			if len(seen) < 2 {
				return fmt.Errorf("태스크마다 OTEL_RESOURCE_ATTRIBUTES 가 달라야 한다: %v", seen)
			}
			// 실행 로그에 버킷 이름, 서브넷, 보안 그룹 ID 가 찍히지 않는다
			for _, secret := range []string{fakelocal.Bucket, "subnet-0loc4l", "sg-0loc4l"} {
				if strings.Contains(o.logs, secret) {
					return fmt.Errorf("실행 로그에 %s 가 보인다", secret)
				}
			}
			return allStopped(r, o)
		}},
		{Name: "fail", Inputs: smoke(nil), Conclusion: "failure", Result: "fail", Check: func(_ *runner, o *outcome) error {
			// 흐름별 표는 실행 요약에 들어간다. summary.json 의 구간별 판정에서 결제(주문)만 걸렸는지 본다
			b, _ := json.Marshal(o.summary["windows"])
			var windows []struct {
				ByName []struct {
					Name string `json:"name"`
					Pass bool   `json:"pass"`
				} `json:"by_name"`
			}
			json.Unmarshal(b, &windows)
			var failed []string
			for _, w := range windows {
				for _, c := range w.ByName {
					if !c.Pass {
						failed = append(failed, c.Name)
					}
				}
			}
			if len(failed) == 0 || slices.ContainsFunc(failed, func(n string) bool {
				return !strings.Contains(n, "/api/v1/orders") && !strings.HasPrefix(n, "flow:order")
			}) {
				return fmt.Errorf("결제(주문) 요청만 걸려야 한다: %v", failed)
			}
			return nil
		}},
		{Name: "abort-error-rate", Inputs: smoke(map[string]string{"runners": "2"}), Conclusion: "failure", Result: "aborted", Check: func(r *runner, o *outcome) error {
			if len(o.state.Stops) < 2 {
				return fmt.Errorf("k6 태스크를 모두 멈춰야 한다: %v", o.state.Stops)
			}
			return allStopped(r, o)
		}},
		{Name: "abort-task-99", Inputs: smoke(map[string]string{"runners": "3"}), Conclusion: "failure", Result: "aborted", Check: func(_ *runner, o *outcome) error {
			if len(o.state.Stops) < 2 {
				return fmt.Errorf("나머지 태스크를 멈춰야 한다: %v", o.state.Stops)
			}
			return nil
		}},
		{Name: "end-99", Inputs: smoke(map[string]string{"runners": "3"}), Conclusion: "success", Result: "pass"},
		{Name: "inconclusive-silent", Inputs: smoke(map[string]string{"runners": "2"}), Conclusion: "failure", Result: "inconclusive", Check: allStopped},
		{Name: "inconclusive-mimir", Inputs: smoke(nil), Conclusion: "failure", Result: "inconclusive", Check: allStopped},
		{Name: "inconclusive-gap", Inputs: smoke(map[string]string{"runners": "2"}), Conclusion: "failure", Result: "inconclusive"},
		{Name: "start-cancel", Inputs: smoke(map[string]string{"runners": "4"}), Conclusion: "failure", Result: "canceled", Check: allStopped},
		{Name: "minter-fail", Inputs: smoke(nil), Conclusion: "failure", Result: "canceled", Check: func(_ *runner, o *outcome) error {
			if n := o.state.runsOf(fakelocal.FamilyK6); n != 0 {
				return fmt.Errorf("k6 RunTask 가 오지 않아야 한다: %d번", n)
			}
			return nil
		}},
		{Name: "bad-observability", Inputs: smoke(nil), Conclusion: "failure", Result: "-", Check: noRuns},
		{Name: "image-mix", Inputs: smoke(map[string]string{"runners": "2"}), Conclusion: "failure", Result: "canceled", Check: func(r *runner, o *outcome) error {
			if reason, _ := o.summary["reason"].(string); strings.Count(reason, "sha256:") < 2 {
				return fmt.Errorf("결과에 다이제스트 두 개가 적혀야 한다: %q", reason)
			}
			return allStopped(r, o)
		}},
		{Name: "obs-redeploy", Inputs: smoke(nil), Conclusion: "success", Result: "pass"},
		{Name: "manual-stop", Inputs: smoke(nil), Conclusion: "failure", Result: "aborted",
			During: func(r *runner, _ int) error {
				id, err := r.waitK6Running()
				if err != nil {
					return err
				}
				_, err = r.dispatch("stop.yml", map[string]string{"test_id": id})
				return err
			},
			Check: func(r *runner, o *outcome) error {
				if reason, _ := o.summary["reason"].(string); !strings.Contains(reason, "105") {
					return fmt.Errorf("stop.yml 로 멈춘 것이어야 한다: %q", reason)
				}
				return allStopped(r, o)
			}},
		// Forgejo 러너는 실행을 취소하면 작업을 바로 끊어 if: always() 뒷정리가 돌지 않는다. GitHub 에서 확인한다(C-4)
		{Name: "cancel", ForgejoGap: true, Inputs: smoke(nil), Conclusion: "cancelled",
			During: func(r *runner, run int) error {
				if _, err := r.waitK6Running(); err != nil {
					return err
				}
				return r.forgejo(http.MethodPost, fmt.Sprintf("/actions/runs/%d/cancel", run), nil, nil)
			},
			Check: func(r *runner, o *outcome) error {
				// 뒷정리(if: always())가 돌아 남은 태스크가 없다
				time.Sleep(20 * time.Second)
				st, err := r.state()
				if err != nil {
					return err
				}
				if len(st.Running) != 0 {
					return fmt.Errorf("취소한 뒤에도 뒷정리가 태스크를 멈춰야 한다: %v", st.Running)
				}
				return nil
			}},
		{Name: "controller-dies", Inputs: smoke(nil), Conclusion: "failure", Result: "-",
			During: func(r *runner, _ int) error {
				if _, err := r.waitK6Running(); err != nil {
					return err
				}
				return killController()
			},
			Check: func(r *runner, o *outcome) error {
				if !strings.Contains(o.logs, "판정 불가") {
					return errors.New("판정 불가로 끝나야 한다")
				}
				return allStopped(r, o)
			}},
		{Name: "input-rejected", Inputs: smoke(map[string]string{"peak_rps": "1000"}), Conclusion: "failure", Result: "-", Check: noRuns},

		// 정리. 끝난 spike 시험(상품 둘)을 moto 에 만들어 두고 정리한다
		{Name: "cleanup", Workflow: "cleanup.yml", Conclusion: "success",
			Setup:  func(r *runner) error { return r.finishedTest("lt-20261019t0000-901", true) },
			Inputs: map[string]string{"test_id": "lt-20261019t0000-901"},
			Check: func(r *runner, o *outcome) error {
				var env map[string]string
				for _, run := range o.state.Runs {
					if run.Family == fakelocal.FamilyData {
						env = run.Env
					}
				}
				if env == nil || !strings.Contains(env["PRODUCT_IDS"], product) || !strings.Contains(env["PRODUCT_IDS"], prep) {
					return fmt.Errorf("두 상품으로 정리 태스크를 띄워야 한다: %v", env)
				}
				if _, err := r.get("results/lt-20261019t0000-901/cleanup.json"); err != nil {
					return fmt.Errorf("cleanup.json: %w", err)
				}
				return nil
			}},
		// 큐에 메시지가 남아 있으면 기다리고, 비면 정리한다
		{Name: "cleanup", Workflow: "cleanup.yml", Conclusion: "success",
			Setup: func(r *runner) error {
				if err := r.finishedTest("lt-20261019t0000-902", true); err != nil {
					return err
				}
				return r.queueMessage()
			},
			During: func(r *runner, _ int) error {
				time.Sleep(90 * time.Second)
				return r.purgeQueues()
			},
			Inputs: map[string]string{"test_id": "lt-20261019t0000-902"},
			Check: func(_ *runner, o *outcome) error {
				if !strings.Contains(o.logs, "큐가 비기를 기다린다") {
					return errors.New("큐가 빌 때까지 기다려야 한다")
				}
				return nil
			}},
		{Name: "cleanup-refused", Workflow: "cleanup.yml", Conclusion: "failure", Inputs: map[string]string{"test_id": "lt-20261019t0000-903"},
			Setup: func(r *runner) error { return r.finishedTest("lt-20261019t0000-903", true) },
			Check: func(_ *runner, o *outcome) error {
				if !strings.Contains(o.logs, "tasks.data") {
					return errors.New("tasks.data 가 없어 거절해야 한다")
				}
				return noRuns(nil, o)
			}},
		{Name: "cleanup-other-test", Workflow: "cleanup.yml", Conclusion: "failure", Inputs: map[string]string{"test_id": "lt-20261019t0000-904"},
			Setup: func(r *runner) error {
				if err := r.finishedTest("lt-20261019t0000-904", false); err != nil {
					return err
				}
				return nil
			},
			Check: func(_ *runner, o *outcome) error {
				if !strings.Contains(o.logs, "끝나지 않은 시험") {
					return errors.New("끝나지 않은 시험이라 거절해야 한다")
				}
				return nil
			}},
		{Name: "cleanup-other-test", Workflow: "cleanup.yml", Conclusion: "failure", Inputs: map[string]string{"test_id": "lt-20261019t0000-905"},
			Setup: func(r *runner) error {
				if err := r.finishedTest("lt-20261019t0000-905", true); err != nil {
					return err
				}
				return r.post(control+"/inject?startedBy=lt-20261019t0100-77", nil)
			},
			Check: func(_ *runner, o *outcome) error {
				if !strings.Contains(o.logs, "다른 시험(lt-20261019t0100-77)") {
					return errors.New("다른 시험이 돌면 거절해야 한다")
				}
				if o.state.runsOf(fakelocal.FamilyData) != 0 {
					return errors.New("정리 태스크를 띄우지 않아야 한다")
				}
				return nil
			}},

		{Name: "stress-limit", Long: true, Inputs: map[string]string{"scenario": "stress", "product_id": product}, Conclusion: "success", Result: "limit",
			Check: func(_ *runner, o *outcome) error {
				if step, _ := o.summary["last_passing_step"].(string); step != "step2" {
					return fmt.Errorf("마지막 합격 단계는 step2 여야 한다: %q", step)
				}
				return nil
			}},
	}
}

type runner struct {
	auth string
	http *http.Client
	s3   *s3.Client
	sqs  *sqs.Client
	cfg  *config.Config
}

func main() {
	long := flag.Bool("long", false, "stress 한계 각본까지 돌린다")
	flag.Parse()
	root, _ := os.Getwd()
	token, err := os.ReadFile(filepath.Join(root, "local/.data/token"))
	if err != nil {
		fail("local/forgejo/up.sh 를 먼저 돌린다: %v", err)
	}
	cfg, err := config.Default()
	if err != nil {
		fail("%v", err)
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion("ap-northeast-2"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("local", "local", "")),
		awsconfig.WithBaseEndpoint("http://localhost:5000"))
	if err != nil {
		fail("%v", err)
	}
	r := &runner{
		auth: "token " + strings.TrimSpace(string(token)), http: &http.Client{Timeout: 30 * time.Second},
		s3: s3.NewFromConfig(awsCfg, func(o *s3.Options) {
			o.UsePathStyle, o.DisableLogOutputChecksumValidationSkipped = true, true
		}),
		sqs: sqs.NewFromConfig(awsCfg), cfg: cfg,
	}
	// 이름을 주면 그 순서대로 돌린다(같은 이름의 각본이 여럿이면 모두)
	var selected []Case
	if want := flag.Args(); len(want) > 0 {
		for _, name := range want {
			n := len(selected)
			for _, c := range cases() {
				if c.Name == name {
					selected = append(selected, c)
				}
			}
			if len(selected) == n {
				fail("각본 %q 가 없다", name)
			}
		}
	} else {
		for _, c := range cases() {
			if (!c.Long || *long) && !c.ForgejoGap {
				selected = append(selected, c)
			}
		}
	}
	var failed []string
	for _, c := range selected {
		if c.Workflow == "" {
			c.Workflow = "loadtest.yml"
		}
		start := time.Now()
		err := r.run(c)
		status := "통과"
		if err != nil {
			status = "실패: " + err.Error()
			failed = append(failed, c.Name)
		}
		fmt.Printf("%-22s %-12s %5.0f초  %s\n", c.Name, c.Workflow, time.Since(start).Seconds(), status)
	}
	if len(failed) > 0 {
		fail("실패한 각본: %s", strings.Join(failed, ", "))
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func (r *runner) run(c Case) error {
	if err := r.post(control+"/scenario?name="+c.Name, nil); err != nil {
		return fmt.Errorf("각본: %w", err)
	}
	if c.Setup != nil {
		if err := c.Setup(r); err != nil {
			return fmt.Errorf("준비: %w", err)
		}
	}
	run, err := r.dispatch(c.Workflow, c.Inputs)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	if c.During != nil {
		go func() { errc <- c.During(r, run) }()
	}
	status, err := r.wait(run, 40*time.Minute)
	if err != nil {
		return err
	}
	if c.During != nil {
		if err := <-errc; err != nil {
			return fmt.Errorf("도중: %w", err)
		}
	}
	o := &outcome{run: run, status: status}
	o.logs = r.logs(run)
	if o.state, err = r.state(); err != nil {
		return err
	}
	for _, run := range o.state.Runs {
		if run.Family == fakelocal.FamilyMinter || run.Family == fakelocal.FamilyK6 {
			o.testID = run.StartedBy
		}
	}
	if status != c.Conclusion {
		return fmt.Errorf("워크플로 %s(기대 %s), 실행 %d", status, c.Conclusion, run)
	}
	if c.Result != "" {
		if c.Result == "-" {
			if o.testID != "" {
				if _, err := r.get("results/" + o.testID + "/summary.json"); err == nil {
					return errors.New("summary.json 이 없어야 한다")
				}
			}
		} else {
			if o.testID == "" {
				return errors.New("시험 ID 를 찾지 못했다(태스크가 하나도 뜨지 않았다)")
			}
			b, err := r.get("results/" + o.testID + "/summary.json")
			if err != nil {
				return fmt.Errorf("summary.json: %w", err)
			}
			if err := json.Unmarshal(b, &o.summary); err != nil {
				return err
			}
			if got := o.summary["result"]; got != c.Result {
				return fmt.Errorf("결과 %v(기대 %s): %v", got, c.Result, o.summary["reason"])
			}
		}
	}
	if c.Check != nil {
		return c.Check(r, o)
	}
	return nil
}

func (r *runner) forgejo(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, forgejo+path, rd)
	req.Header.Set("Authorization", r.auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: HTTP %d %s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type runList struct {
	Runs []struct {
		ID     int    `json:"id"`
		Status string `json:"status"`
	} `json:"workflow_runs"`
}

func (r *runner) latest(workflow string) (int, error) {
	var l runList
	if err := r.forgejo(http.MethodGet, "/actions/runs?limit=1&workflow_id="+workflow, nil, &l); err != nil {
		return 0, err
	}
	if len(l.Runs) == 0 {
		return 0, nil
	}
	return l.Runs[0].ID, nil
}

func (r *runner) dispatch(workflow string, inputs map[string]string) (int, error) {
	before, err := r.latest(workflow)
	if err != nil {
		return 0, err
	}
	if err := r.forgejo(http.MethodPost, "/actions/workflows/"+workflow+"/dispatches", map[string]any{"ref": "main", "inputs": inputs}, nil); err != nil {
		return 0, err
	}
	for range 30 {
		id, err := r.latest(workflow)
		if err == nil && id != before {
			return id, nil
		}
		time.Sleep(time.Second)
	}
	return 0, errors.New("dispatch 한 실행을 찾지 못했다")
}

func (r *runner) wait(run int, limit time.Duration) (string, error) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		var v struct {
			Status string `json:"status"`
		}
		if err := r.forgejo(http.MethodGet, fmt.Sprintf("/actions/runs/%d", run), nil, &v); err == nil {
			switch v.Status {
			case "success", "failure", "cancelled", "skipped":
				return v.Status, nil
			}
		}
		time.Sleep(10 * time.Second)
	}
	return "", fmt.Errorf("실행 %d 가 %s 안에 끝나지 않았다", run, limit)
}

// logs 는 실행의 작업 로그를 모두 합친다
func (r *runner) logs(run int) string {
	var jobs []struct {
		ID int `json:"id"`
	}
	if err := r.forgejo(http.MethodGet, fmt.Sprintf("/actions/runs/%d/jobs", run), nil, &jobs); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, j := range jobs {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/actions/jobs/%d/logs", forgejo, j.ID), nil)
		req.Header.Set("Authorization", r.auth)
		if resp, err := r.http.Do(req); err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			sb.Write(b)
		}
	}
	return sb.String()
}

func (r *runner) post(url string, body io.Reader) error {
	resp, err := r.http.Post(url, "application/json", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: HTTP %d %s", url, resp.StatusCode, b)
	}
	return nil
}

func (r *runner) state() (state, error) {
	var s state
	resp, err := r.http.Get(control + "/state")
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	return s, json.NewDecoder(resp.Body).Decode(&s)
}

// waitK6Running 은 k6 태스크가 뜰 때까지 기다리고 시험 ID 를 돌려준다
func (r *runner) waitK6Running() (string, error) {
	for range 60 {
		s, err := r.state()
		if err == nil {
			for _, run := range s.Runs {
				if run.Family == fakelocal.FamilyK6 && len(s.Running) > 0 {
					// START_AT 이 지나 부하가 시작될 때까지 조금 더 기다린다
					start, _ := time.Parse(time.RFC3339, run.Env["START_AT"])
					time.Sleep(time.Until(start.Add(30 * time.Second)))
					return run.StartedBy, nil
				}
			}
		}
		time.Sleep(10 * time.Second)
	}
	return "", errors.New("k6 태스크가 뜨지 않았다")
}

func (r *runner) get(key string) ([]byte, error) {
	out, err := r.s3.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(fakelocal.Bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (r *runner) put(key string, b []byte) error {
	_, err := r.s3.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String(fakelocal.Bucket), Key: aws.String(key), Body: bytes.NewReader(b)})
	return err
}

// finishedTest 는 정리할 spike 시험(상품 둘)의 plan.json 을 둔다. finished 면 summary.json 도 둔다
func (r *runner) finishedTest(testID string, finished bool) error {
	now := time.Now().UTC().Truncate(time.Second)
	p, err := plan.New(r.cfg, plan.Input{TestID: testID, Scenario: "spike", ProductID: product, PrepProductID: prep,
		OpenAt: now.Add(30 * time.Minute).Format(time.RFC3339), Now: now})
	if err != nil {
		return err
	}
	b, _ := json.Marshal(p)
	if err := r.put("results/"+testID+"/plan.json", b); err != nil {
		return err
	}
	if !finished {
		return nil
	}
	return r.put("results/"+testID+"/summary.json", []byte(`{"test_id":"`+testID+`","scenario":"spike","result":"pass"}`))
}

func (r *runner) queueMessage() error {
	ctx := context.Background()
	u, err := r.sqs.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(r.cfg.Cleanup.Queues[0])})
	if err != nil {
		return err
	}
	_, err = r.sqs.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: u.QueueUrl, MessageBody: aws.String("남은 메시지")})
	return err
}

func (r *runner) purgeQueues() error {
	ctx := context.Background()
	for _, q := range r.cfg.Cleanup.Queues {
		u, err := r.sqs.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(q)})
		if err != nil {
			return err
		}
		if _, err := r.sqs.PurgeQueue(ctx, &sqs.PurgeQueueInput{QueueUrl: u.QueueUrl}); err != nil {
			return err
		}
	}
	return nil
}

// killController 는 작업 컨테이너 안의 인라인 컨트롤러를 SIGKILL 로 끝낸다(컨트롤러 사망 각본)
func killController() error {
	dind := func(args ...string) ([]byte, error) {
		all := append([]string{"compose", "-f", "local/compose.yml", "exec", "-T", "dind", "docker", "-H", "tcp://127.0.0.1:2375"}, args...)
		return exec.Command("docker", all...).Output()
	}
	out, err := dind("ps", "-q")
	if err != nil {
		return fmt.Errorf("작업 컨테이너 목록: %w", err)
	}
	for _, id := range strings.Fields(string(out)) {
		if _, err := dind("exec", id, "pkill", "-KILL", "-x", "controller"); err == nil {
			return nil
		}
	}
	return errors.New("인라인 컨트롤러 프로세스를 찾지 못했다")
}
