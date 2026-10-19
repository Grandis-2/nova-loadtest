package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Grandis-2/nova-loadtest/controller/internal/fakeecs"
	"github.com/Grandis-2/nova-loadtest/controller/internal/infra"
	"github.com/Grandis-2/nova-loadtest/controller/internal/plan"
)

const dataFam = "nova-prod-loadtest-data"

type fakeQueues struct{ pending func(at time.Time) int64 }

func (q fakeQueues) Pending(_ context.Context, names []string) (map[string]int64, error) {
	out := map[string]int64{}
	for _, n := range names {
		out[n] = 0
	}
	if q.pending != nil {
		out[names[0]] = q.pending(time.Time{})
	}
	return out, nil
}

// cleanupFixture 는 spike 시험이 끝난 상태(plan.json, summary.json)를 만든다. spike 는 두 상품을 쓴다
func cleanupFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t, "smoke", nil)
	in := plan.Input{TestID: testID, Scenario: "spike", ProductID: product, PrepProductID: "a1b2c3d4-0000-4000-8000-000000000002",
		OpenAt: t0.Add(30 * time.Minute).Format(time.RFC3339), Now: t0}
	p, err := plan.New(f.run.Config, in)
	if err != nil {
		t.Fatal(err)
	}
	pj, _ := json.Marshal(p)
	ctx := context.Background()
	f.store.Put(ctx, "results/"+testID+"/plan.json", pj)
	f.store.Put(ctx, "results/"+testID+"/summary.json", []byte(`{"result":"pass"}`))
	f.run.RunTask.Tasks["data"] = infra.Task{TaskDefinition: dataFam, Container: "data", Subnets: []string{"s"}, SecurityGroups: []string{"g"}, AssignPublicIP: "DISABLED"}
	f.ecs.Behave = func(family string, env map[string]string, n int) fakeecs.Behavior {
		return fakeecs.Behavior{RunningAfter: 5 * time.Second, ExitAt: f.clock.Now().Add(time.Minute), ExitCode: 0}
	}
	return f, testID + "-c"
}

func TestCleanup(t *testing.T) {
	f, self := cleanupFixture(t)
	res, err := Cleanup(context.Background(), f.deps, fakeQueues{}, f.run.Config, f.run.RunTask, testID, self)
	if err != nil || !res.Done {
		t.Fatalf("정리해야 한다: %v %+v", err, res)
	}
	runs := f.ecs.RunsOf(dataFam)
	if len(runs) != 1 || runs[0].StartedBy != DataStartedBy(testID) {
		t.Fatalf("정리 태스크를 <시험 ID>-d 로 한 번 띄워야 한다: %+v", runs)
	}
	if ids := runs[0].Env["PRODUCT_IDS"]; !strings.Contains(ids, product) || !strings.Contains(ids, "a1b2c3d4-0000-4000-8000-000000000002") {
		t.Fatalf("spike 는 진입 상품과 준비 상품을 모두 정리한다: %s", ids)
	}
	if _, err := f.store.Get(context.Background(), "results/"+testID+"/cleanup.json"); err != nil {
		t.Fatal("cleanup.json 을 남겨야 한다")
	}
}

func TestCleanupRefuses(t *testing.T) {
	cases := []struct {
		name string
		prep func(f *fixture)
		want string
	}{
		{"정리 태스크 없음", func(f *fixture) { delete(f.run.RunTask.Tasks, "data") }, "tasks.data"},
		{"끝나지 않은 시험", func(f *fixture) { f.store.DeletePrefix(context.Background(), "results/"+testID+"/summary.json") }, "끝나지 않은 시험"},
		{"다른 시험이 돈다", func(f *fixture) {
			f.ecs.Behave = func(string, map[string]string, int) fakeecs.Behavior {
				return fakeecs.Behavior{RunningAfter: time.Second}
			}
			f.deps.Runner.Run(context.Background(), f.run.RunTask.Tasks["k6"], "lt-20261020t0700-2", nil)
		}, "다른 시험"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, self := cleanupFixture(t)
			tc.prep(f)
			_, err := Cleanup(context.Background(), f.deps, fakeQueues{}, f.run.Config, f.run.RunTask, testID, self)
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("거절해야 한다(%q): %v", tc.want, err)
			}
			if len(f.ecs.RunsOf(dataFam)) != 0 {
				t.Fatal("정리 태스크를 띄우지 않아야 한다")
			}
		})
	}
}

func TestCleanupQueueNotEmpty(t *testing.T) {
	f, self := cleanupFixture(t)
	q := fakeQueues{pending: func(time.Time) int64 { return 3 }}
	res, err := Cleanup(context.Background(), f.deps, q, f.run.Config, f.run.RunTask, testID, self)
	if err == nil || res == nil || res.Done || !strings.Contains(res.Reason, "비지 않아") {
		t.Fatalf("큐가 비지 않으면 지우지 않고 실패해야 한다: %v %+v", err, res)
	}
	if len(f.ecs.RunsOf(dataFam)) != 0 {
		t.Fatal("정리 태스크를 띄우지 않아야 한다")
	}
	if b, _ := f.store.Get(context.Background(), "results/"+testID+"/cleanup.json"); !strings.Contains(string(b), `"done": false`) {
		t.Fatalf("실패도 cleanup.json 에 남긴다: %s", b)
	}
}

// 큐를 기다리는 동안 다른 시험이 시작하면 지우지 않는다
func TestCleanupOtherTestStartsDuringWait(t *testing.T) {
	f, self := cleanupFixture(t)
	calls := 0
	q := fakeQueues{pending: func(time.Time) int64 {
		calls++
		if calls == 1 {
			f.ecs.Behave = func(string, map[string]string, int) fakeecs.Behavior {
				return fakeecs.Behavior{RunningAfter: time.Second}
			}
			f.deps.Runner.Run(context.Background(), f.run.RunTask.Tasks["k6"], "lt-20261020t0700-2", nil)
			return 5
		}
		return 0
	}}
	res, err := Cleanup(context.Background(), f.deps, q, f.run.Config, f.run.RunTask, testID, self)
	if err == nil || res.Done || !strings.Contains(res.Reason, "다른 시험") {
		t.Fatalf("지우기 직전에 다른 시험을 보고 멈춰야 한다: %v %+v", err, res)
	}
	if len(f.ecs.RunsOf(dataFam)) != 0 {
		t.Fatal("정리 태스크를 띄우지 않아야 한다")
	}
}

// 상품 조회만 한 시험은 product_id 가 있어도 지울 상품이 없다
func TestCleanupCatalogOnlyHasNoTargets(t *testing.T) {
	f, self := cleanupFixture(t)
	in := plan.Input{TestID: testID, Scenario: "smoke", ProductID: product, Flows: []string{"catalog"}, Now: t0}
	p, _ := plan.New(f.run.Config, in)
	pj, _ := json.Marshal(p)
	f.store.Put(context.Background(), "results/"+testID+"/plan.json", pj)
	_, err := Cleanup(context.Background(), f.deps, fakeQueues{}, f.run.Config, f.run.RunTask, testID, self)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "지울 시험 상품이 없다") {
		t.Fatalf("쓰지 않은 상품을 정리 대상으로 삼으면 안 된다: %v", err)
	}
}

// 정리 태스크가 끝나지 않으면 무한히 기다리지 않고 멈춘 뒤 실패로 남긴다
func TestCleanupDataTaskTimeout(t *testing.T) {
	f, self := cleanupFixture(t)
	f.ecs.Behave = func(string, map[string]string, int) fakeecs.Behavior {
		return fakeecs.Behavior{RunningAfter: time.Second}
	}
	res, err := Cleanup(context.Background(), f.deps, fakeQueues{}, f.run.Config, f.run.RunTask, testID, self)
	if err == nil || res.Done || !strings.Contains(res.Reason, "끝나지 않아") {
		t.Fatalf("시간이 지나면 실패해야 한다: %v %+v", err, res)
	}
	if len(f.ecs.Stops) != 1 {
		t.Fatal("끝나지 않은 정리 태스크를 멈춰야 한다")
	}
}

// 같은 시험의 정리 태스크가 이미 돌고 있으면(정리를 다시 실행) 두 번 돌리지 않는다
func TestCleanupAlreadyRunning(t *testing.T) {
	f, self := cleanupFixture(t)
	f.ecs.Behave = func(string, map[string]string, int) fakeecs.Behavior {
		return fakeecs.Behavior{RunningAfter: time.Second}
	}
	f.deps.Runner.Run(context.Background(), f.run.RunTask.Tasks["data"], DataStartedBy(testID), nil)
	_, err := Cleanup(context.Background(), f.deps, fakeQueues{}, f.run.Config, f.run.RunTask, testID, self)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), DataStartedBy(testID)) {
		t.Fatalf("먼저 띄운 정리 태스크가 돌면 거절해야 한다: %v", err)
	}
}
