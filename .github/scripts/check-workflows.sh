#!/bin/sh
# 워크플로 규칙 검사. 로컬 Forgejo 러너에도 있는 sh 와 grep 만 쓴다.
#   - pull_request_target 금지(docs/design.md "저장소 보호")
#   - continue-on-error 금지. Forgejo 가 무시한다(구현 계획 7.3)
#   - 액션은 커밋 해시로 고정(구현 계획 7.3)
set -u

fail=0
files=$(ls .github/workflows/*.yml .github/workflows/*.yaml 2> /dev/null)
if [ -z "$files" ]; then
  echo "::error::.github/workflows 에 워크플로가 없다"
  exit 1
fi

# shellcheck disable=SC2086 # 파일 이름에 공백이 없다
if grep -nE '^[^#]*pull_request_target' $files; then
  echo "::error::pull_request_target 은 쓰지 않는다"
  fail=1
fi

# shellcheck disable=SC2086
if grep -nE '^[[:space:]]*continue-on-error:' $files; then
  echo "::error::continue-on-error 는 쓰지 않는다. 실패 처리는 if: 로 한다"
  fail=1
fi

# 로컬 액션(./)은 빼고, 액션은 @<40자 해시>, docker:// 는 @sha256:<해시> 여야 한다
# shellcheck disable=SC2086
unpinned=$(grep -nE '^[[:space:]-]*uses:' $files \
  | grep -vE 'uses:[[:space:]]*["'\'']?\./' \
  | grep -vE '@[0-9a-f]{40}(["'\'']?[[:space:]]|["'\'']?$)' \
  | grep -vE 'uses:[[:space:]]*["'\'']?docker://[^@[:space:]]+@sha256:[0-9a-f]{64}')
if [ -n "$unpinned" ]; then
  echo "$unpinned"
  echo "::error::액션은 커밋 해시로 고정한다"
  fail=1
fi

exit "$fail"
