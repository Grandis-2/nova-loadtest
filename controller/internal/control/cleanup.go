package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Grandis-2/nova-loadtest/config"
	"github.com/Grandis-2/nova-loadtest/controller/internal/infra"
	"github.com/Grandis-2/nova-loadtest/controller/internal/plan"
	"github.com/Grandis-2/nova-loadtest/controller/internal/runner"
)

// Queues는 정리 전에 비었는지 볼 SQS 큐다.
type Queues interface {
	// Pending은 큐마다 남은 메시지 수(보이는 것 + 처리 중인 것)를 돌려준다
	Pending(ctx context.Context, names []string) (map[string]int64, error)
}

// CleanupResult는 cleanup.json 이다.
type CleanupResult struct {
	TestID     string    `json:"test_id"`
	ProductIDs []string  `json:"product_ids"`
	Members    any       `json:"members,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
	Done       bool      `json:"done"`
	Reason     string    `json:"reason,omitempty"`
}

// ErrRefused는 정리를 시작하지 않고 거절했다는 뜻이다(cleanup.yml 은 실패로 끝난다).
var ErrRefused = errors.New("정리를 거절했다")

const queuePoll = 30 * time.Second

// Cleanup은 시험 하나의 시험 데이터를 지운다(docs/design.md "정리").
//
// 담당자가 회차를 마감한 뒤 cleanup.yml 이 정리 모드로 띄운다. 정리 대상(두 시험 상품, 회원 범위)은 입력으로 받지 않고
// 그 시험의 plan.json 에서 읽는다. 다른 시험이 돌거나 큐가 비지 않으면 지우지 않는다.
// selfStartedBy 는 이 컨트롤러의 startedBy(<시험 ID>-c)다. 다른 시험이 도는지 볼 때 뺀다.
func Cleanup(ctx context.Context, d Deps, q Queues, cfg *config.Config, rt *infra.RunTask, testID, selfStartedBy string) (*CleanupResult, error) {
	refuse := func(format string, a ...any) (*CleanupResult, error) {
		return nil, fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, a...))
	}
	data, ok := rt.Tasks["data"]
	if !ok {
		return refuse("정리 태스크(tasks.data)가 아직 없다(인프라 I-6)")
	}
	if len(cfg.Cleanup.Queues) == 0 {
		return refuse("scenarios.yml 의 cleanup.queues 가 비었다")
	}
	if _, err := d.Store.Get(ctx, resultsKey(testID, "summary.json")); err != nil {
		return refuse("끝나지 않은 시험이다(results/%s/summary.json 이 없다)", testID)
	}
	b, err := d.Store.Get(ctx, resultsKey(testID, "plan.json"))
	if err != nil {
		return refuse("plan.json 을 읽지 못했다: %v", err)
	}
	var p plan.Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return refuse("plan.json 을 읽지 못했다: %v", err)
	}
	products := uniq(p.ProductID, p.PrepProductID)
	if len(products) == 0 {
		return refuse("지울 시험 상품이 없다(상품 조회만 한 시험이다)")
	}

	// 다른 시험이 돌면 큐가 그 시험 때문에 비지 않고, 그 시험의 데이터를 건드릴 수 있다
	arns, err := d.Runner.ListRunning(ctx)
	if err != nil {
		return nil, fmt.Errorf("시험 클러스터 보기: %w", err)
	}
	if len(arns) > 0 {
		states, err := d.Runner.Describe(ctx, arns, "")
		if err != nil {
			return nil, fmt.Errorf("시험 클러스터 보기: %w", err)
		}
		for _, s := range states {
			if s.LastStatus != "STOPPED" && s.StartedBy != selfStartedBy {
				return refuse("다른 시험(%s)이 돌고 있다", s.StartedBy)
			}
		}
	}

	res := &CleanupResult{TestID: testID, ProductIDs: products, Members: p.Members, StartedAt: d.Clock.Now()}
	done := func(ok bool, reason string) (*CleanupResult, error) {
		res.Done, res.Reason, res.EndedAt = ok, reason, d.Clock.Now()
		out, _ := json.MarshalIndent(res, "", "  ")
		if err := d.Store.Put(context.WithoutCancel(ctx), resultsKey(testID, "cleanup.json"), out); err != nil {
			return res, fmt.Errorf("cleanup.json 기록: %w", err)
		}
		if !ok {
			return res, errors.New(reason)
		}
		return res, nil
	}

	// 큐가 빌 때까지 기다린다. CloudWatch 지표는 몇 분 늦게 나와 SQS 속성을 바로 본다
	deadline := d.Clock.Now().Add(cfg.Cleanup.MaxWait.D())
	for {
		pending, err := q.Pending(ctx, cfg.Cleanup.Queues)
		if err != nil {
			return done(false, "큐를 보지 못했다: "+err.Error())
		}
		left := nonEmpty(pending)
		if len(left) == 0 {
			break
		}
		if !d.Clock.Now().Before(deadline) {
			return done(false, fmt.Sprintf("%s 안에 큐가 비지 않아 지우지 않았다(%s). 나중에 다시 실행한다", cfg.Cleanup.MaxWait.D(), strings.Join(left, ", ")))
		}
		d.Logf("큐가 비기를 기다린다: %s", strings.Join(left, ", "))
		if err := d.Clock.Sleep(ctx, queuePoll); err != nil {
			return done(false, err.Error())
		}
	}

	// 정리 태스크에 넘기는 값은 인프라 I-6 과 정한다(docs/implementation-plan.md 2.4 "정리 모드")
	members, _ := json.Marshal(p.Members)
	env := []runner.Env{
		{Name: "TEST_ID", Value: testID},
		{Name: "PRODUCT_IDS", Value: strings.Join(products, ",")},
		{Name: "MEMBERS", Value: string(members)},
	}
	arn, err := d.Runner.Run(ctx, data, selfStartedBy, env)
	if err != nil {
		return done(false, "정리 태스크를 띄우지 못했다: "+err.Error())
	}
	for {
		states, err := d.Runner.Describe(ctx, []string{arn}, data.Container)
		if err == nil && states[0].LastStatus == "STOPPED" {
			if c := states[0].ExitCode; c != nil && *c == 0 {
				return done(true, "")
			}
			return done(false, fmt.Sprintf("정리 태스크가 실패했다(종료 코드 %s, %s)", code(states[0].ExitCode), states[0].StoppedReason))
		}
		if err := d.Clock.Sleep(ctx, 10*time.Second); err != nil {
			return done(false, err.Error())
		}
	}
}

func uniq(vs ...string) []string {
	var out []string
	for _, v := range vs {
		if v == "" {
			continue
		}
		dup := false
		for _, o := range out {
			dup = dup || o == v
		}
		if !dup {
			out = append(out, v)
		}
	}
	return out
}

func nonEmpty(m map[string]int64) []string {
	var out []string
	for k, v := range m {
		if v > 0 {
			out = append(out, fmt.Sprintf("%s %d건", k, v))
		}
	}
	sort.Strings(out)
	return out
}
