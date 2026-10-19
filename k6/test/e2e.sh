#!/bin/sh
# k6 이미지를 시험용 응답 서버(fake-api)와 moto(S3)에 붙여 끝까지 돌린다. 운영에 닿지 않는다(구현 계획 7.2 의 2단계).
# 로컬과 CI(k6-safety)가 같이 쓴다. Linux 에서 Docker 와 Go 가 있어야 한다.
#
#   k6/test/e2e.sh ok       다섯 흐름 smoke(30초). 0으로 끝나고, 규칙 위반이 없고, 모든 경로를 불렀는지 본다
#   k6/test/e2e.sh error    오류가 계속되면 중단 임계치로 99로 끝나는지 본다(약 1분 30초)
#   k6/test/e2e.sh sigterm  도중에 멈추면 105로 끝나고 요약을 올리는지 본다
#
# 이미지는 미리 빌드해 둔다: docker buildx build --platform linux/amd64 -f k6/Dockerfile -t nova-loadtest/k6:dev --load .
set -eu

mode=${1:?ok, error, sigterm 중 하나}
image=${K6_IMAGE:-nova-loadtest/k6:dev}
root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
moto=nova-e2e-moto-$$
k6c=nova-e2e-k6-$$
fake_pid=""
fail=0

# shellcheck disable=SC2317 # trap 으로 부른다
cleanup() {
  if [ -n "$fake_pid" ]; then kill "$fake_pid" 2> /dev/null || true; fi
  docker rm -f "$moto" "$k6c" > /dev/null 2>&1 || true
  rm -rf "${work:?}"
}
trap cleanup EXIT

expect() { # expect <설명> <받은 값> <기대 값>
  if [ "$2" = "$3" ]; then echo "PASS $1"; else echo "::error::FAIL $1: 받은 값 '$2', 기대 '$3'"; fail=1; fi
}

product=a1b2c3d4-0000-4000-8000-000000000001
test_id=lt-20261020t0630-$$
s3port=${E2E_S3_PORT:-5055}
apiport=${E2E_API_PORT:-8080}
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_REGION=ap-northeast-2
export AWS_ENDPOINT_URL="http://127.0.0.1:$s3port"

echo "== 준비"
(cd "$root" && go build -o "$work/" ./controller/cmd/plan ./controller/cmd/fake-api ./token-minter/cmd/token-minter ./k6/test/s3get)
docker run -d --name "$moto" -p "127.0.0.1:$s3port:5000" motoserver/moto:5.2.3 > /dev/null
fake_mode=ok
[ "$mode" = error ] && fake_mode=error
FAKE_MODE=$fake_mode FAKE_HEADER=e2e-header FAKE_ADDR=127.0.0.1:$apiport "$work/fake-api" > "$work/fake.log" 2>&1 &
fake_pid=$!
for _ in $(seq 1 30); do
  curl -sf "$AWS_ENDPOINT_URL" > /dev/null 2>&1 && curl -sf "http://127.0.0.1:$apiport/__stats" > /dev/null 2>&1 && break
  sleep 1
done
curl -sf -X PUT "$AWS_ENDPOINT_URL/e2e-bucket" > /dev/null

# 계획. ok 는 다섯 흐름, 나머지는 상품 조회만(준비 구간과 등록 대기가 없어 바로 시작한다)
case "$mode" in
  ok) "$work/plan" -test-id "$test_id" -scenario smoke -duration 30s -product-id "$product" -json "$work/plan.json" ;;
  error | sigterm) "$work/plan" -test-id "$test_id" -scenario smoke -duration 2m -flows catalog -json "$work/plan.json" ;;
  *) echo "모드 $mode 를 모른다" >&2; exit 2 ;;
esac

tokens_env=""
if [ "$mode" = ok ]; then
  # 토큰. 시험용 키를 새로 만든다(운영 키는 쓰지 않는다)
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$work/key.pem" 2> /dev/null
  members=$(python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1]))["members"]))' "$work/plan.json")
  ttl=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["token_ttl_seconds"])' "$work/plan.json")
  TEST_ID=$test_id TOKENS_URI=s3://e2e-bucket/tokens/$test_id/ MEMBERS=$members TASKS=1 TOKEN_TTL_SECONDS=$ttl \
    JWT_KEY_ID=e2e JWT_ISSUER=nova JWT_AUDIENCE=nova-api JWT_PRIVATE_KEY="$(cat "$work/key.pem")" "$work/token-minter"
  tokens_env="-e TOKENS_URI=s3://e2e-bucket/tokens/$test_id/"
fi

echo "== k6 실행($mode)"
start_at=$(date -u -d '+5 seconds' +%Y-%m-%dT%H:%M:%SZ)
# shellcheck disable=SC2086 # tokens_env 는 비거나 -e 하나다
docker run -d --name "$k6c" --network host \
  -e TEST_ID="$test_id" -e TASK_INDEX=0 -e START_AT="$start_at" -e LOCAL=1 \
  -e SEGMENT=0:1 -e SEGMENT_SEQUENCE=0,1 \
  -e BASE_URL="http://127.0.0.1:$apiport" -e LOADTEST_HEADER=e2e-header \
  -e PLAN_JSON="$(cat "$work/plan.json")" \
  -e RESULTS_URI="s3://e2e-bucket/results/$test_id/" $tokens_env \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_REGION -e AWS_ENDPOINT_URL \
  "$image" > /dev/null

if [ "$mode" = sigterm ]; then
  sleep 25
  docker stop -t 60 "$k6c" > /dev/null
fi
code=$(docker wait "$k6c")
docker logs "$k6c" > "$work/k6.log" 2>&1 || true
tail -n 5 "$work/k6.log"

echo "== 확인"
summary=$("$work/s3get" e2e-bucket "results/$test_id/k6-0.json" || true)
stats=$(curl -sf "http://127.0.0.1:$apiport/__stats")
echo "fake-api: $stats"
# 올린 요약의 종료 코드와 k6 요약이 들어 있는지
uploaded=$(printf '%s' "$summary" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["exit_code"], d.get("summary") is not None)' 2> /dev/null || echo "없음")
violations=$(printf '%s' "$stats" | python3 -c 'import json,sys; print(json.load(sys.stdin)["violations"])')
route_count() { printf '%s' "$stats" | R="$1" python3 -c 'import json,os,sys; print(json.load(sys.stdin)["routes"].get(os.environ["R"], 0))'; }

case "$mode" in
  ok)
    expect "종료 코드" "$code" 0
    expect "올린 요약" "$uploaded" "0 True"
    expect "fake-api 가 센 규칙 위반" "$violations" "{}"
    for r in "GET /api/v1/products" "GET /api/v1/products/{id}" "GET /api/v1/categories" \
      "POST /api/v1/preorders/queue" "GET /api/v1/preorders/queue" "POST /api/v1/preorders" "GET /api/v1/preorders" \
      "GET /api/v1/preorders/active" "POST /api/v1/preorders/{id}/cancel" "POST /api/v1/orders" "POST /api/v1/orders/{id}/payment-attempts"; do
      n=$(route_count "$r")
      expect "경로를 불렀다: $r ($n번)" "$([ "$n" -gt 0 ] && echo yes || echo no)" yes
    done
    ;;
  error)
    expect "중단 임계치로 끝난 종료 코드" "$code" 99
    expect "올린 요약" "$uploaded" "99 True"
    ;;
  sigterm)
    expect "외부 중지로 끝난 종료 코드" "$code" 105
    expect "올린 요약" "$uploaded" "105 True"
    ;;
esac
exit "$fail"
