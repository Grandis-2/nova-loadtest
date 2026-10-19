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
cp "$(git rev-parse --git-path index)" "$index"
GIT_INDEX_FILE=$index git add -A
tree=$(GIT_INDEX_FILE=$index git write-tree)
commit=$(git commit-tree "$tree" -p HEAD -m "local mirror of $(git rev-parse --short HEAD)")

git -c http.extraHeader="Authorization: token $token" \
  push --force "$url" "$commit:refs/heads/$branch"
echo "올림: $commit -> $branch"
