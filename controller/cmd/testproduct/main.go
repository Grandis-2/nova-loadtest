// testproduct 는 담당자가 자기 관리자 세션으로 시험 상품을 만드는 명령이다. 운영 시험 전에 사람이 직접 돌린다.
//
//	export NOVA_ADMIN_TOKEN=...   # 관리자 화면 로그인의 sessionToken. 셸 기록에 남지 않게 read -rs 로 넣는다
//	go run ./controller/cmd/testproduct                          # smoke, load, stress, soak: 상품 하나
//	go run ./controller/cmd/testproduct -spike                   # spike: 준비 상품과 진입 상품
//	go run ./controller/cmd/testproduct -rush                    # rush: 상품 하나. 회차가 열리는 시각이 open_at 이다
//	go run ./controller/cmd/testproduct -dry-run                 # 보낼 본문만 출력한다
//
// 관리자 토큰은 환경 변수로만 받고 출력하지 않는다. 회차는 be 규칙상 지금부터 30분 뒤부터 열 수 있다.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/Grandis-2/nova-loadtest/config"
	"github.com/Grandis-2/nova-loadtest/controller/internal/gha"
	"github.com/Grandis-2/nova-loadtest/controller/internal/testproduct"
)

const defaultBaseURL = "https://api.grandis-nova.link"

func main() {
	var (
		baseURL, opensAt, openAt, categoryID string
		length                               time.Duration
		spike, rush, dryRun                  bool
	)
	flag.StringVar(&baseURL, "base-url", envOr("BASE_URL", defaultBaseURL), "API 주소")
	flag.StringVar(&opensAt, "opens-at", "", "회차가 열리는 시각(RFC 3339). 비우면 지금 + 31분(분 단위 올림). spike 에서는 준비 상품의 오픈")
	flag.StringVar(&openAt, "open-at", "", "spike 진입 상품이 열리는 시각(RFC 3339). 비우면 준비 상품 오픈 + 30분")
	flag.DurationVar(&length, "length", 12*time.Hour, "회차 길이(오픈부터 마감까지). 시험이 끝나는 시각보다 길게 둔다")
	flag.StringVar(&categoryID, "category-id", "", "카테고리 ID. 비우면 공개 카테고리 트리의 이어폰 카테고리(없으면 하위가 없는 첫 카테고리)")
	flag.BoolVar(&spike, "spike", false, "spike 용으로 준비 상품과 진입 상품을 만든다")
	flag.BoolVar(&rush, "rush", false, "rush 용 상품을 만든다. 회차가 열리기 전에 실행하도록 안내한다")
	flag.BoolVar(&dryRun, "dry-run", false, "보내지 않고 본문만 출력한다")
	flag.Parse()

	now := time.Now().UTC()
	first, err := parseOr(opensAt, now.Add(testproduct.MinOpenLead).Truncate(time.Minute).Add(time.Minute))
	if err != nil {
		fatal("-opens-at: %v", err)
	}
	specs := []testproduct.Spec{{OpensAt: first, ClosesAt: first.Add(length)}}
	if spike && rush {
		fatal("-spike 와 -rush 는 같이 쓰지 않는다")
	}
	if spike {
		entry, err := parseOr(openAt, first.Add(30*time.Minute))
		if err != nil {
			fatal("-open-at: %v", err)
		}
		// 준비 상품이 열린 뒤, 진입 상품 오픈 15분 전까지 실행한다. 그 구간이 비지 않게 한다
		if !entry.After(first.Add(runLead)) {
			fatal("-open-at 은 준비 상품 오픈(%s) %s 뒤보다 늦어야 한다. 준비 상품이 열린 뒤, 진입 상품 오픈 %s 전까지 실행하기 때문이다",
				first.Format(time.RFC3339), runLead, runLead)
		}
		// 준비 상품은 진입 상품의 회차가 끝날 때까지 열어 둔다
		specs = []testproduct.Spec{
			{Label: "준비", OpensAt: first, ClosesAt: entry.Add(length)},
			{Label: "진입", OpensAt: entry, ClosesAt: entry.Add(length)},
		}
	} else if openAt != "" {
		fatal("-open-at 은 -spike 와 같이 쓴다")
	}
	for _, s := range specs {
		if err := s.Check(now); err != nil {
			fatal("%v", err)
		}
	}

	// 관리자 토큰을 보낼 주소인지 먼저 본다. 본문만 출력할 때는 토큰을 보내지 않는다
	if !dryRun {
		if err := testproduct.CheckURL(baseURL); err != nil {
			fatal("%v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := &testproduct.Client{BaseURL: baseURL, Token: os.Getenv("NOVA_ADMIN_TOKEN"), HTTP: &http.Client{Timeout: 30 * time.Second}}
	if categoryID == "" {
		if categoryID, err = c.Category(ctx); err != nil {
			fatal("%v", err)
		}
	}
	for i := range specs {
		specs[i].CategoryID = categoryID
	}

	if dryRun {
		for _, s := range specs {
			b, _ := json.MarshalIndent(testproduct.Body(s), "", "  ")
			fmt.Printf("POST %s/api/v1/admin/products (%s상품)\n%s\n", baseURL, prefix(s.Label), b)
		}
		return
	}
	if c.Token, err = testproduct.Token(c.Token, time.Now()); err != nil {
		fatal("%v. 관리자 화면에 로그인한 세션의 토큰을 read -rs NOVA_ADMIN_TOKEN; export NOVA_ADMIN_TOKEN 으로 넣는다", err)
	}

	var made []testproduct.Product
	for _, s := range specs {
		p, err := c.Create(ctx, s)
		if err != nil {
			report(made)
			fatal("%v", err)
		}
		if err := c.WaitReady(ctx, &p, 2*time.Minute); err != nil {
			report(append(made, p))
			fatal("%v", err)
		}
		// 시험 사전 점검과 같은 확인: 공개 상세가 판매 중이고 판매 중인 옵션이 있다
		if err := gha.Precheck(ctx, c.HTTP, baseURL, "testproduct", p.ID); err != nil {
			report(append(made, p))
			fatal("%v", err)
		}
		made = append(made, p)
	}
	report(made)
	guide(made, spike, rush)
}

// report 는 만든 상품을 출력한다. 중간에 실패해도 이미 만든 상품은 남으므로 먼저 보여 준다(정리할 때 쓴다).
func report(ps []testproduct.Product) {
	for _, p := range ps {
		fmt.Printf("%s상품  %s  오픈 %s  마감 %s  (Idempotency-Key %s)\n",
			prefix(p.Label), p.ID, p.OpensAt.Format(time.RFC3339), p.ClosesAt.Format(time.RFC3339), p.IdempotencyKey)
	}
}

func guide(ps []testproduct.Product, spike, rush bool) {
	fmt.Println()
	fmt.Println("loadtest.yml 입력")
	if rush {
		p := ps[0]
		cfg, err := config.Default()
		if err != nil {
			fatal("%v", err)
		}
		// 준비 상품이 없으므로 지금부터 실행할 수 있다. open_at 이 plan 의 상한 안에 들어야 한다
		start, end := runWindow(time.Now(), p.OpensAt, cfg.Runner.MaxOpenAtLead.D())
		fmt.Printf("  scenario: rush\n  product_id: %s\n  open_at: %s\n", p.ID, p.OpensAt.Format(time.RFC3339))
		fmt.Printf("\n%s 뒤, %s 전에 실행한다(회차가 열리기 전).\n", local(start), local(end))
		return
	}
	if !spike {
		p := ps[0]
		fmt.Printf("  product_id: %s\n", p.ID)
		fmt.Printf("\n%s 이후에 실행한다(회차가 열린 뒤).\n", local(p.OpensAt))
		return
	}
	prep, entry := ps[0], ps[1]
	cfg, err := config.Default()
	if err != nil {
		fatal("%v", err)
	}
	start, end := runWindow(prep.OpensAt, entry.OpensAt, cfg.Runner.MaxOpenAtLead.D())
	fmt.Printf("  scenario: spike\n  product_id: %s\n  prep_product_id: %s\n  open_at: %s\n",
		entry.ID, prep.ID, entry.OpensAt.Format(time.RFC3339))
	fmt.Printf("\n%s 뒤, %s 전에 실행한다.\n", local(start), local(end))
	fmt.Printf("준비 상품이 열려 있어야 하고, open_at 은 실행 시각부터 %s 안이어야 하며, 진입 상품 오픈 %s 전까지 실행한다.\n",
		cfg.Runner.MaxOpenAtLead.D(), runLead)
}

func prefix(label string) string {
	if label == "" {
		return "시험 "
	}
	return label + " "
}

// runLead 는 spike 실행을 진입 상품 오픈보다 얼마나 앞서 해야 하는가다. 준비 구간, 등록 대기, 토큰 발급, k6 기동이 그 사이에 든다
const runLead = 15 * time.Minute

// runWindow 는 spike, rush 를 실행해도 되는 구간이다. 준비 상품이 열린 뒤(rush 는 지금)이면서 open_at 이 plan 의 상한(maxLead,
// scenarios.yml runner.max_open_at_lead) 안에 들고, 진입 상품 오픈 15분 전까지다
func runWindow(prepOpens, entryOpens time.Time, maxLead time.Duration) (time.Time, time.Time) {
	start := prepOpens
	if s := entryOpens.Add(-maxLead); s.After(start) {
		start = s
	}
	return start, entryOpens.Add(-runLead)
}

func local(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04 MST")
}

func parseOr(s string, def time.Time) (time.Time, error) {
	if s == "" {
		return def, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	return t.UTC(), err
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "testproduct: "+format+"\n", a...)
	os.Exit(1)
}
