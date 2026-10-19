#!/bin/sh
# 로컬 Forgejo 와 러너를 띄우고 이 저장소를 올린다. 운영에 닿지 않는다(구현 계획 7절).
# 비밀번호, 러너 등록 토큰, 접근 토큰은 local/.data 에만 두고 커밋하지 않는다.
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
data="$here/.data"
user=loadtest

compose() { docker compose -f "$here/compose.yml" "$@"; }
fj() { compose exec -T -u git forgejo forgejo "$@"; }

umask 077
mkdir -p "$data"

compose up -d --wait forgejo

if ! fj admin user list --admin | awk '{print $2}' | grep -qx "$user"; then
  # 비밀번호가 프로세스 인자에 보이지 않도록 Forgejo 가 만들게 하고, 출력에서 읽어 둔다
  fj admin user create --admin --username "$user" --email "$user@localhost" \
    --random-password --random-password-length 32 --must-change-password=false \
    | sed -n "s/^generated random password is '\(.*\)'$/\1/p" > "$data/admin-password"
  [ -s "$data/admin-password" ] || { echo "관리자 비밀번호를 읽지 못했다" >&2; exit 1; }
fi
if [ ! -s "$data/token" ]; then
  fj admin user generate-access-token --username "$user" --token-name mirror \
    --scopes all --raw > "$data/token"
fi

if [ ! -s "$data/runner.env" ]; then
  echo "RUNNER_TOKEN=$(fj forgejo-cli actions generate-runner-token)" > "$data/runner.env"
fi
compose up -d --wait dind

# 작업 이미지는 호스트에서 받아 dind 에 넣는다. dind 가 레지스트리에 IPv6 로만 붙으려다
# 실패하는 환경이 있고, 받는 시간도 줄어든다. 이름은 runner.yml 의 라벨과 같아야 한다.
job_image=node:24.19.0-bookworm
if ! compose exec -T dind docker -H tcp://127.0.0.1:2375 image inspect "$job_image" > /dev/null 2>&1; then
  docker image inspect "$job_image" > /dev/null 2>&1 || docker pull -q "$job_image"
  docker save "$job_image" | compose exec -T dind docker -H tcp://127.0.0.1:2375 load -q
fi

compose up -d runner

# 3단계 각본에 쓰는 moto, fake-cluster, fake-api
compose up -d --build moto fake-cluster fake-api

"$here/forgejo/mirror.sh"

# 사전 점검이 부르는 주소(loadtest.yml 은 GitHub 이 아닐 때만 vars.BASE_URL 을 쓴다)
api=http://localhost:3000/api/v1/repos/$user/nova-loadtest/actions/variables/BASE_URL
body='{"value":"http://fake-api:8080"}'
# 토큰이 프로세스 인자에 보이지 않게 헤더를 파일로 넘긴다
printf 'Authorization: token %s\n' "$(cat "$data/token")" > "$data/auth-header"
curl -fsS -o /dev/null -X POST -H @"$data/auth-header" -H 'Content-Type: application/json' -d "$body" "$api" 2>/dev/null ||
  curl -fsS -o /dev/null -X PUT -H @"$data/auth-header" -H 'Content-Type: application/json' -d "$body" "$api"

echo "Forgejo: http://localhost:3000/$user/nova-loadtest/actions (계정 $user, 비밀번호 local/.data/admin-password)"
