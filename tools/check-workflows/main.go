// check-workflows는 워크플로 파일을 파싱해 저장소 규칙을 검사한다(docs/design.md "저장소 보호").
//
//   - pull_request_target 이벤트를 쓰지 않는다
//   - continue-on-error를 쓰지 않는다. Forgejo가 무시하므로 실패 처리는 if:로 한다(구현 계획 7.3)
//   - 원격 액션과 재사용 워크플로는 커밋 해시(40자)로, docker:// 이미지는 sha256 다이제스트로 고정한다
//
// 글자 비교가 아니라 YAML을 파싱한 결과로 보므로, 이스케이프나 흐름 스타일로 우회할 수 없다.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/rhysd/actionlint"
)

var (
	commitPinned = regexp.MustCompile(`@[0-9a-f]{40}$`)
	digestPinned = regexp.MustCompile(`^docker://[^@\s]+@sha256:[0-9a-f]{64}$`)
)

// Problem은 규칙 위반 하나다.
type Problem struct {
	File string
	Line int
	Msg  string
}

func (p Problem) String() string { return fmt.Sprintf("%s:%d: %s", p.File, p.Line, p.Msg) }

// Check는 워크플로 파일 하나의 내용을 검사한다.
func Check(file string, src []byte) []Problem {
	var ps []Problem
	add := func(pos *actionlint.Pos, msg string) {
		line := 0
		if pos != nil {
			line = pos.Line
		}
		ps = append(ps, Problem{file, line, msg})
	}

	wf, errs := actionlint.Parse(src)
	for _, e := range errs {
		ps = append(ps, Problem{file, e.Line, "파싱 실패: " + e.Message})
	}
	if wf == nil {
		return ps
	}

	for _, ev := range wf.On {
		if w, ok := ev.(*actionlint.WebhookEvent); ok && w.Hook != nil &&
			strings.EqualFold(w.Hook.Value, "pull_request_target") {
			add(w.Hook.Pos, "pull_request_target은 쓰지 않는다")
		}
	}

	ids := make([]string, 0, len(wf.Jobs))
	for id := range wf.Jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		job := wf.Jobs[id]
		if job == nil {
			continue
		}
		if job.ContinueOnError != nil {
			add(job.ContinueOnError.Pos, "continue-on-error는 쓰지 않는다. 실패 처리는 if:로 한다")
		}
		if job.WorkflowCall != nil && job.WorkflowCall.Uses != nil {
			if msg := checkUses(job.WorkflowCall.Uses.Value); msg != "" {
				add(job.WorkflowCall.Uses.Pos, msg)
			}
		}
		for _, st := range job.Steps {
			if st == nil {
				continue
			}
			if st.ContinueOnError != nil {
				add(st.ContinueOnError.Pos, "continue-on-error는 쓰지 않는다. 실패 처리는 if:로 한다")
			}
			if a, ok := st.Exec.(*actionlint.ExecAction); ok && a.Uses != nil {
				if msg := checkUses(a.Uses.Value); msg != "" {
					add(a.Uses.Pos, msg)
				}
			}
		}
	}
	return ps
}

// checkUses는 uses 값이 고정되어 있지 않으면 이유를 돌려준다.
func checkUses(uses string) string {
	switch {
	case strings.HasPrefix(uses, "./"):
		return ""
	case strings.HasPrefix(uses, "docker://"):
		if digestPinned.MatchString(uses) {
			return ""
		}
		return fmt.Sprintf("%s: docker 이미지는 @sha256 다이제스트로 고정한다", uses)
	case commitPinned.MatchString(uses):
		return ""
	default:
		return fmt.Sprintf("%s: 액션은 커밋 해시(40자)로 고정한다", uses)
	}
}

func main() {
	dir := ".github/workflows"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	var files []string
	for _, pat := range []string{"*.yml", "*.yaml"} {
		m, _ := filepath.Glob(filepath.Join(dir, pat))
		files = append(files, m...)
	}
	if len(files) == 0 {
		fmt.Printf("::error::%s에 워크플로가 없다\n", dir)
		os.Exit(1)
	}

	failed := false
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			fmt.Printf("::error::%s: %v\n", f, err)
			failed = true
			continue
		}
		for _, p := range Check(f, src) {
			fmt.Printf("::error file=%s,line=%d::%s\n", p.File, p.Line, p.Msg)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
	fmt.Printf("워크플로 %d개 통과\n", len(files))
}
