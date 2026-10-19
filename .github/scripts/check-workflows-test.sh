#!/bin/sh
# check-workflows.sh 의 사례 시험. ci.yml 이 돌린다.
set -u

G=$(cd "$(dirname "$0")" && pwd)/check-workflows.sh
S=$(mktemp -d)
trap 'rm -rf "${S:?}"' EXIT
failed=0

# run <파일 이름> <기대 종료 코드> <내용(\n 은 줄바꿈)>. 파일 이름이 비면 워크플로가 없는 경우다
run() {
  name=$1 want=$2 body=$3
  rm -rf "${S:?}/.github"
  mkdir -p "$S/.github/workflows"
  [ -n "$name" ] && printf '%b' "$body" > "$S/.github/workflows/$name"
  (cd "$S" && sh "$G" > /dev/null 2>&1)
  got=$?
  if [ "$got" = "$want" ]; then
    echo "PASS ${name:-<none>}"
  else
    echo "::error::FAIL ${name:-<none>}: want=$want got=$got"
    failed=1
  fi
}

run ok.yml 0 'on: push\njobs:\n  a:\n    steps:\n      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7\n      - uses: ./local\n'
run prt.yaml 1 'on: [push, pull_request_target]\n'
run comment.yml 0 'on: push # pull_request_target 은 안 쓴다\n'
run coe.yml 1 'jobs:\n  a:\n    continue-on-error: true\n'
run tag.yml 1 '      - uses: actions/checkout@v7\n'
run short.yml 1 '      - uses: actions/checkout@3d3c42e\n'
run dockertag.yml 1 '      - uses: docker://alpine:3\n'
run dockerdigest.yml 0 '      - uses: docker://alpine@sha256:0000000000000000000000000000000000000000000000000000000000000000\n'
run quoted.yml 0 "      - uses: 'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1'\n"
run reusable.yml 1 '    uses: org/repo/.github/workflows/x.yml@main\n'
run "" 1 ''

exit "$failed"
