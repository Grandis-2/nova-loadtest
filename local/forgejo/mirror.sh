#!/bin/sh
# 지금 작업 트리를 로컬 Forgejo 의 브랜치(기본 main)로 올린다. 커밋하지 않은 변경도 올라간다.
#   mirror.sh            main 으로 올린다. push 로 ci.yml 이 돈다
#   mirror.sh <브랜치>   그 브랜치로 올린다. 웹 화면에서 main 으로 PR 을 열면 pull_request 로 돈다
# 작업 트리를 임시 커밋으로 만들 뿐, 이 저장소의 브랜치와 인덱스는 건드리지 않는다.
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$here/.." && pwd)
data="$here/.data"
url=http://localhost:3000/loadtest/nova-loadtest.git
branch=${1:-main}

token=$(cat "$data/token")
index=$(mktemp)
trap 'rm -f "$index"' EXIT

cd "$repo"
# 사용자의 인덱스를 복사하지 않고 HEAD 에서 새로 만든다. 무시 목록에 있는 파일을 강제로
# 스테이징해 두었어도 미러에 섞이지 않는다.
GIT_INDEX_FILE=$index git read-tree HEAD
GIT_INDEX_FILE=$index git add -A
if GIT_INDEX_FILE=$index git ls-files | grep -q '^local/\.data/'; then
  echo "local/.data 가 커밋에 들어 있어 올리지 않는다" >&2
  exit 1
fi
tree=$(GIT_INDEX_FILE=$index git write-tree)
commit=$(git commit-tree "$tree" -p HEAD -m "local mirror of $(git rev-parse --short HEAD)")

# 토큰이 프로세스 인자에 보이지 않게 환경 변수로 설정을 넘긴다
GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraHeader \
  GIT_CONFIG_VALUE_0="Authorization: token $token" \
  git push --force "$url" "$commit:refs/heads/$branch"
echo "올림: $commit -> $branch"
