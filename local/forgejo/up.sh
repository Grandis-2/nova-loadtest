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
  openssl rand -hex 16 > "$data/admin-password"
  fj admin user create --admin --username "$user" --email "$user@localhost" \
    --password "$(cat "$data/admin-password")" --must-change-password=false
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

"$here/forgejo/mirror.sh"

echo "Forgejo: http://localhost:3000/$user/nova-loadtest/actions (계정 $user, 비밀번호 local/.data/admin-password)"
