package fakelocal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/Grandis-2/nova-loadtest/config"
	"github.com/Grandis-2/nova-loadtest/controller/internal/clock"
	"github.com/Grandis-2/nova-loadtest/controller/internal/fakeecs"
	"github.com/Grandis-2/nova-loadtest/controller/internal/plan"
	"github.com/Grandis-2/nova-loadtest/controller/internal/runner"
)

// 로컬 값. 모두 가짜다
const (
	Bucket       = "nova-prod-loadtest-local"
	Cluster      = "nova-prod-loadtest"
	ObsCluster   = "nova-prod-observability"
	ObsService   = "nova-prod-obs"
	MimirPort    = 9009
	MimirPath    = "/prometheus/api/v1/query"
	unreachable  = "10.255.255.1" // 사라질 관측 태스크. 닿지 않는다
	paramRunTask = "/nova-prod/loadtest/run-task"
	paramObs     = "/nova-prod/loadtest/observability"
)

// Server 는 가짜 ECS, 가짜 Mimir, 각본 제어 API 다.
type Server struct {
	S3     *s3.Client
	SSM    *ssm.Client
	SQS    *sqs.Client
	Config *config.Config
	SelfIP string // 관측 서비스 태스크의 IP(이 프로세스). 컨트롤러가 이 IP 의 9009 로 조회한다

	mu       sync.Mutex
	scenario Scenario
	fake     *fakeecs.Fake
	plans    map[string]*plan.Plan
	starts   map[string]time.Time // 시험 ID → START_AT
}

func (s *Server) ecs() *fakeecs.Fake {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fake
}

func (s *Server) current() Scenario {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scenario
}

// Handlers 는 ECS(:8080), Mimir(:9009), 제어(:8081) 핸들러다
func (s *Server) Handlers() (ecsH, mimirH, controlH http.Handler) {
	c := http.NewServeMux()
	c.HandleFunc("POST /scenario", s.setScenario)
	c.HandleFunc("GET /state", s.state)
	c.HandleFunc("POST /inject", s.inject)
	m := http.NewServeMux()
	m.HandleFunc("GET "+MimirPath, s.serveMimir)
	return http.HandlerFunc(s.serveECS), m, c
}

// Use 는 각본을 바꾸고 ECS 상태를 비운 뒤 moto 의 SSM 값을 각본에 맞춘다.
func (s *Server) Use(ctx context.Context, name string) error {
	sc, ok := scenarios[name]
	if !ok {
		return fmt.Errorf("각본 %q 가 없다", name)
	}
	if s.SSM != nil { // 단위 테스트는 moto 없이 돈다
		if err := s.seed(ctx, sc); err != nil {
			return err
		}
	}
	now := time.Now()
	f := &fakeecs.Fake{Clock: clock.Real{}, Services: map[string][]fakeecs.ServiceTask{
		ObsCluster + "/" + ObsService: {{IP: s.SelfIP, StartedAt: now.Add(-10 * time.Minute)}},
	}}
	if sc.OldObs {
		// 먼저 뜬 태스크는 닿지 않고 4분 뒤 사라진다. 늦게 시작한 태스크를 골라야 한다
		f.Services[ObsCluster+"/"+ObsService] = append([]fakeecs.ServiceTask{{IP: unreachable, StartedAt: now.Add(-time.Hour), Gone: now.Add(4 * time.Minute)}},
			f.Services[ObsCluster+"/"+ObsService]...)
	}
	f.Behave = func(family string, env map[string]string, _ int) fakeecs.Behavior { return s.behave(sc, family, env) }
	s.mu.Lock()
	s.scenario, s.fake = sc, f
	s.plans, s.starts = map[string]*plan.Plan{}, map[string]time.Time{}
	s.mu.Unlock()
	log.Printf("각본 %s", name)
	return nil
}

func (s *Server) behave(sc Scenario, family string, env map[string]string) fakeecs.Behavior {
	now := time.Now()
	switch family {
	case FamilyMinter:
		return sc.minter(now)
	case FamilyData:
		return sc.data(now)
	case FamilyK6:
		if env["START_AT"] == "" {
			return running(Task{}) // inject 로 띄운 다른 시험
		}
		start, _ := time.Parse(time.RFC3339, env["START_AT"])
		i, _ := strconv.Atoi(env["TASK_INDEX"])
		total := 3 * time.Minute
		if p := s.plan(env["TEST_ID"]); p != nil {
			total = time.Duration(p.Estimates.TotalSeconds) * time.Second
		}
		s.mu.Lock()
		s.starts[env["TEST_ID"]] = start
		s.mu.Unlock()
		return sc.k6(Task{Index: i, Start: start, End: start.Add(total)})
	}
	// 컨트롤러는 로컬에서 작업 안에서 바로 돈다(CONTROLLER_INLINE). 여기로 오지 않는다
	return fakeecs.Behavior{RunningAfter: 5 * time.Second, ExitAt: now.Add(time.Minute)}
}

// plan 은 컨트롤러가 S3 에 쓴 plan.json 이다(k6 를 띄우기 전에 쓴다)
func (s *Server) plan(testID string) *plan.Plan {
	s.mu.Lock()
	p, ok := s.plans[testID]
	s.mu.Unlock()
	if ok {
		return p
	}
	if s.S3 == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := s.S3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(Bucket), Key: aws.String("results/" + testID + "/plan.json")})
	if err != nil {
		log.Printf("plan.json(%s): %v", testID, err)
		return nil
	}
	defer out.Body.Close()
	b, _ := io.ReadAll(out.Body)
	p = &plan.Plan{}
	if err := json.Unmarshal(b, p); err != nil {
		log.Printf("plan.json(%s): %v", testID, err)
		return nil
	}
	s.mu.Lock()
	s.plans[testID] = p
	s.mu.Unlock()
	return p
}

// RunTaskJSON 은 SSM run-task 값이다. 인프라와 같은 모양에 가짜 ID 를 넣는다
func RunTaskJSON(withData bool) string {
	task := func(def, container, subnet, sg, public string) map[string]any {
		return map[string]any{"taskDefinition": def, "container": container, "subnets": []string{subnet},
			"securityGroups": []string{sg}, "assignPublicIp": public}
	}
	tasks := map[string]any{
		"controller":  task(FamilyController, "controller", "subnet-0loc4l0app0000001", "sg-0loc4l0ctl00000001", "DISABLED"),
		"tokenMinter": task(FamilyMinter, "token-minter", "subnet-0loc4l0app0000001", "sg-0loc4l0mnt00000001", "DISABLED"),
		"k6":          task(FamilyK6, "k6", "subnet-0loc4l0pub0000001", "sg-0loc4l0k6000000001", "ENABLED"),
	}
	if withData {
		tasks["data"] = task(FamilyData, "data", "subnet-0loc4l0app0000001", "sg-0loc4l0dat00000001", "DISABLED")
	}
	b, _ := json.Marshal(map[string]any{"cluster": Cluster, "bucket": Bucket, "tasks": tasks})
	return string(b)
}

// ObservabilityJSON 은 SSM observability 값이다. 수집 주소에는 아무도 받지 않는다(k6 가 실제로 돌지 않는다)
const ObservabilityJSON = `{"otlpEndpoint":"fake-cluster:4318","mimir":{"cluster":"` + ObsCluster + `","service":"` + ObsService + `","port":9009,"queryPath":"` + MimirPath + `"}}`

func (s *Server) seed(ctx context.Context, sc Scenario) error {
	in := &s3.CreateBucketInput{Bucket: aws.String(Bucket),
		CreateBucketConfiguration: &s3types.CreateBucketConfiguration{LocationConstraint: s3types.BucketLocationConstraintApNortheast2}}
	if _, err := s.S3.CreateBucket(ctx, in); err != nil && !alreadyOwned(err) {
		return fmt.Errorf("버킷: %w", err)
	}
	obs := ObservabilityJSON
	if sc.Observability != "" {
		obs = sc.Observability
	}
	for name, v := range map[string]string{paramRunTask: RunTaskJSON(sc.WithData), paramObs: obs} {
		if _, err := s.SSM.PutParameter(ctx, &ssm.PutParameterInput{Name: aws.String(name), Value: aws.String(v), Type: ssmtypes.ParameterTypeString, Overwrite: aws.Bool(true)}); err != nil {
			return fmt.Errorf("SSM %s: %w", name, err)
		}
	}
	for _, q := range s.Config.Cleanup.Queues {
		out, err := s.SQS.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(q)})
		if err != nil {
			return fmt.Errorf("SQS %s: %w", q, err)
		}
		if _, err := s.SQS.PurgeQueue(ctx, &sqs.PurgeQueueInput{QueueUrl: out.QueueUrl}); err != nil {
			return fmt.Errorf("SQS %s 비우기: %w", q, err)
		}
	}
	return nil
}

func alreadyOwned(err error) bool {
	msg := err.Error()
	return contains(msg, "BucketAlreadyOwnedByYou") || contains(msg, "BucketAlreadyExists")
}

func (s *Server) setScenario(w http.ResponseWriter, r *http.Request) {
	if err := s.Use(r.Context(), r.URL.Query().Get("name")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// state 는 실행기가 확인할 값이다: 받은 RunTask, StopTask, 지금 도는 태스크
func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	f := s.ecs()
	sc := s.current()
	// 멈추는 중(StopTask 를 받았지만 아직 STOPPED 가 아님)인 태스크도 도는 것으로 센다. 컨트롤러와 같은 방법이다
	running, err := (&runner.Runner{ECS: f, Cluster: Cluster, Clock: clock.Real{}}).ListRunning(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	runs, stops := f.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"scenario": sc.Name, "runs": runs, "stops": stops, "running": running})
}

// inject 는 다른 시험의 k6 태스크를 하나 띄운다(정리 거절 각본). 멈출 때까지 돈다
func (s *Server) inject(w http.ResponseWriter, r *http.Request) {
	startedBy := r.URL.Query().Get("startedBy")
	if startedBy == "" {
		http.Error(w, "startedBy 가 필요하다", http.StatusBadRequest)
		return
	}
	f := s.ecs()
	out, err := f.RunTask(r.Context(), &ecs.RunTaskInput{Cluster: aws.String(Cluster), TaskDefinition: aws.String(FamilyK6), StartedBy: aws.String(startedBy)})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"taskArn": aws.ToString(out.Tasks[0].TaskArn)})
}
