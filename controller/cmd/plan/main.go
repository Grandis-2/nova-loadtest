// plan은 시험 워크플로의 입력 확인 단계에서 계획을 계산한다. 컨트롤러도 같은 코드로 다시 계산한다.
//
//	plan -test-id lt-20261020t0630-1 -scenario smoke -product-id <uuid> -json plan.json -summary plan.md
//
// 거절할 이유가 있으면 모두 출력하고 1로 끝난다.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Grandis-2/nova-loadtest/config"
	"github.com/Grandis-2/nova-loadtest/controller/internal/plan"
)

func main() {
	var in plan.Input
	var flows, now, jsonOut, summaryOut string
	flag.StringVar(&in.TestID, "test-id", "", "시험 ID(lt-<yyyymmddThhmm>-<번호>)")
	flag.StringVar(&in.Scenario, "scenario", "smoke", "smoke, load, spike, rush, stress, soak")
	flag.StringVar(&in.ProductID, "product-id", "", "진입과 접수에 쓰는 시험 상품 ID")
	flag.StringVar(&in.PrepProductID, "prep-product-id", "", "준비 구간 시험 상품 ID(기본 product-id)")
	flag.StringVar(&in.OpenAt, "open-at", "", "회차가 열리는 시각(RFC 3339, UTC)")
	flag.Float64Var(&in.PeakRPS, "peak-rps", 0, "최대 흐름 시작 수(초당). 0이면 시나리오 기본값")
	flag.StringVar(&in.Duration, "duration", "", "최대치를 유지하는 시간")
	flag.StringVar(&in.RampUp, "ramp-up", "", "최대치까지 올리는 시간")
	flag.IntVar(&in.Runners, "runners", 0, "k6 태스크 대수. 0이면 계산한다")
	flag.Float64Var(&in.AbortErrorRate, "abort-error-rate", 0, "중단 기준 오류율(%). 0이면 기본값")
	flag.StringVar(&flows, "flows", "", "쉼표로 구분한 흐름(비면 모두)")
	flag.StringVar(&now, "now", "", "지금 시각(시험용, RFC 3339)")
	flag.StringVar(&jsonOut, "json", "", "계획 JSON을 쓸 파일(비면 표준 출력)")
	flag.StringVar(&summaryOut, "summary", "", "계획 표(markdown)를 덧붙일 파일. 보통 $GITHUB_STEP_SUMMARY")
	flag.Parse()

	if flows != "" {
		for _, f := range strings.Split(flows, ",") {
			if f = strings.TrimSpace(f); f != "" {
				in.Flows = append(in.Flows, f)
			}
		}
	}
	in.Now = time.Now().UTC()
	if now != "" {
		t, err := time.Parse(time.RFC3339, now)
		if err != nil {
			fatal("-now: %v", err)
		}
		in.Now = t
	}

	cfg, err := config.Default()
	if err != nil {
		fatal("%v", err)
	}
	p, err := plan.New(cfg, in)
	if err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintf(os.Stderr, "::error::%s\n", line)
		}
		os.Exit(1)
	}

	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		fatal("%v", err)
	}
	if jsonOut == "" {
		fmt.Println(string(b))
	} else if err := os.WriteFile(jsonOut, append(b, '\n'), 0o644); err != nil {
		fatal("%v", err)
	}
	// 경고는 stderr로 쓴다. 표준 출력에는 계획 JSON만 나가야 PLAN_JSON=$(plan ...) 처럼 받을 수 있다.
	// Actions는 stderr의 ::warning:: 도 주석으로 보인다
	for _, w := range p.Warnings {
		fmt.Fprintf(os.Stderr, "::warning::%s\n", w)
	}
	if summaryOut != "" {
		f, err := os.OpenFile(summaryOut, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fatal("%v", err)
		}
		defer f.Close()
		if _, err := f.WriteString(p.Markdown()); err != nil {
			fatal("%v", err)
		}
	}
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "::error::"+format+"\n", a...)
	os.Exit(1)
}
