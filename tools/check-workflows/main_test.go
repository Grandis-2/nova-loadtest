package main

import (
	"strings"
	"testing"
)

const sha = "3d3c42e5aac5ba805825da76410c181273ba90b1"

func TestCheck(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string // 문제 메시지에 들어 있어야 하는 글자. 비면 통과해야 한다
	}{
		{
			name: "통과",
			src: `on: push
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@` + sha + ` # v7
      - uses: ./local
      - uses: docker://alpine@sha256:` + strings.Repeat("0", 64) + `
      - run: echo ok
`,
		},
		{
			name: "pull_request_target",
			src:  "on: [push, pull_request_target]\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n",
			want: []string{"pull_request_target"},
		},
		{
			name: "이스케이프한 pull_request_target",
			src:  "on: [\"pull_request_\\u0074arget\"]\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n",
			want: []string{"pull_request_target"},
		},
		{
			name: "매핑 형태의 pull_request_target",
			src:  "on:\n  pull_request_target:\n    types: [opened]\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n",
			want: []string{"pull_request_target"},
		},
		{
			name: "주석 속 pull_request_target",
			src:  "on: push # pull_request_target은 쓰지 않는다\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n",
		},
		{
			name: "작업의 continue-on-error",
			src:  "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    continue-on-error: true\n    steps:\n      - run: echo\n",
			want: []string{"continue-on-error"},
		},
		{
			name: "흐름 스타일 단계의 continue-on-error",
			src:  "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - {run: \"exit 1\", continue-on-error: true}\n",
			want: []string{"continue-on-error"},
		},
		{
			name: "태그로 고정한 액션",
			src:  "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v7\n",
			want: []string{"커밋 해시"},
		},
		{
			name: "짧은 해시",
			src:  "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@3d3c42e\n",
			want: []string{"커밋 해시"},
		},
		{
			name: "흐름 스타일 단계의 uses",
			src:  "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - {uses: evil/action@main}\n",
			want: []string{"evil/action@main"},
		},
		{
			name: "따옴표 키의 uses",
			src:  "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - \"uses\": evil/action@main\n",
			want: []string{"evil/action@main"},
		},
		{
			name: "태그로 고정한 docker 이미지",
			src:  "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: docker://alpine:3\n",
			want: []string{"sha256"},
		},
		{
			name: "브랜치로 부르는 재사용 워크플로",
			src:  "on: push\njobs:\n  a:\n    uses: org/repo/.github/workflows/x.yml@main\n",
			want: []string{"x.yml@main"},
		},
		{
			name: "해시로 부르는 재사용 워크플로",
			src:  "on: push\njobs:\n  a:\n    uses: org/repo/.github/workflows/x.yml@" + sha + "\n",
		},
		{
			name: "파싱할 수 없는 파일",
			src:  "on: [push\n",
			want: []string{"파싱 실패"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ps := Check("w.yml", []byte(c.src))
			if len(c.want) == 0 {
				if len(ps) != 0 {
					t.Fatalf("통과해야 하는데 문제가 나왔다: %v", ps)
				}
				return
			}
			if len(ps) == 0 {
				t.Fatalf("문제를 찾아야 하는데 통과했다")
			}
			all := ""
			for _, p := range ps {
				all += p.Msg + "\n"
			}
			for _, w := range c.want {
				if !strings.Contains(all, w) {
					t.Errorf("%q가 메시지에 없다: %s", w, all)
				}
			}
		})
	}
}
