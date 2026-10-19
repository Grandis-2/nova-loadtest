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
	if len(runs) != 1 || runs[0].StartedBy != self {
		t.Fatalf("정리 태스크를 <시험 ID>-c 로 한 번 띄워야 한다: %+v", runs)
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
