#!/bin/sh
# k6 이미지를 시험용 응답 서버(fake-api)와 moto(S3)에 붙여 끝까지 돌린다. 운영에 닿지 않는다(구현 계획 7.2 의 2단계).
# 로컬과 CI(k6-safety)가 같이 쓴다. Linux 에서 Docker 와 Go 가 있어야 한다.
#
#   k6/test/e2e.sh ok       다섯 흐름 smoke(30초). 0으로 끝나고, 규칙 위반이 없고, 모든 경로를 불렀는지 본다
#   k6/test/e2e.sh rush     rush 를 작게 줄여(회원 10명) 접수, 등록 알림, 주문, 결제 시도까지 가는지 본다(약 40초)
#   k6/test/e2e.sh queue    queue 30명이 모두 접수하고, 처리 기한(약 3분)을 기다리지 않고 바로 끝나는지 본다(약 20초)
#   k6/test/e2e.sh error    오류가 계속되어도 멈추지 않고 계획한 시간(30초)까지 돌아 0으로 끝나는지 본다(NV-780)
#   k6/test/e2e.sh sigterm  도중에 멈추면 105로 끝나고 요약을 올리는지 본다
#   k6/test/e2e.sh tokenfail 토큰을 받지 못하면 k6 를 띄우지 않고 120으로 끝나며, 오류를 담은 결과를 올리는지 본다
#
# 이미지는 미리 빌드해 둔다: docker buildx build --platform linux/amd64 -f k6/Dockerfile -t nova-loadtest/k6:dev --load .
set -eu

mode=${1:?ok, rush, queue, error, sigterm, tokenfail 중 하나}
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
  rush)
    # 계획은 기본값으로 계산하고, 시간이 짧게 끝나도록 몰림과 배경만 줄인다(회원 10명, 알림 확인 1초 간격)
    "$work/plan" -test-id "$test_id" -scenario rush -product-id "$product" -open-at 2026-10-20T06:30:00Z \
      -now 2026-10-20T06:00:00Z -json "$work/plan-full.json"
    python3 - "$work/plan-full.json" "$work/plan.json" << 'PY'
import json, sys
p = json.load(open(sys.argv[1]))
p["open_at"] = None
p["members"]["a"]["count"] = 10
p["rush"].update(pay_ratio=1, notify_interval_seconds=1, notify_timeout_seconds=20)
for s in p["scenarios"]:
    if s["flow"] == "rush":
        s.update(rate=60, duration="10s", preAllocatedVUs=10, maxVUs=10, gracefulStop="40s")
    else:
        s.update(rate=60, duration="30s", preAllocatedVUs=2, maxVUs=2)
json.dump(p, open(sys.argv[2], "w"))
PY
    ;;
  queue)
    # 계획을 줄이지 않고 그대로 쓴다. 30명을 3초 동안 들여보낸다
    "$work/plan" -test-id "$test_id" -scenario queue -product-id "$product" -members 30 -peak-rps 10 -json "$work/plan.json"
    ;;
  error) "$work/plan" -test-id "$test_id" -scenario smoke -duration 30s -flows catalog -json "$work/plan.json" ;;
  sigterm | tokenfail) "$work/plan" -test-id "$test_id" -scenario smoke -duration 2m -flows catalog -json "$work/plan.json" ;;
  *) echo "모드 $mode 를 모른다" >&2; exit 2 ;;
esac

tokens_env=""
if [ "$mode" = ok ] || [ "$mode" = rush ] || [ "$mode" = queue ]; then
  # 토큰. 시험용 키를 새로 만든다(운영 키는 쓰지 않는다)
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$work/key.pem" 2> /dev/null
  members=$(python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1]))["members"]))' "$work/plan.json")
  ttl=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["token_ttl_seconds"])' "$work/plan.json")
  TEST_ID=$test_id TOKENS_URI=s3://e2e-bucket/tokens/$test_id/ MEMBERS=$members TASKS=1 TOKEN_TTL_SECONDS=$ttl \
    JWT_KEY_ID=e2e JWT_ISSUER=nova JWT_AUDIENCE=nova-api JWT_PRIVATE_KEY="$(cat "$work/key.pem")" "$work/token-minter"
  tokens_env="-e TOKENS_URI=s3://e2e-bucket/tokens/$test_id/"
fi
if [ "$mode" = tokenfail ]; then
  # 토큰 파일을 올리지 않은 위치를 준다
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
elapsed=$(($(date -u +%s) - $(date -u -d "$start_at" +%s)))
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
      "GET /api/v1/products/{id}/reviews" "GET /api/v1/preorders/products/{id}/shipment-batches" \
      "POST /api/v1/preorders/queue" "GET /api/v1/preorders/queue" "POST /api/v1/preorders" "GET /api/v1/preorders" \
      "GET /api/v1/promotions" "GET /api/v1/promotions/{id}" "GET /api/v1/preorders/{id}" "POST /api/v1/preorders/{id}/cancel" "POST /api/v1/orders" "POST /api/v1/orders/{id}/payment-attempts"; do
      n=$(route_count "$r")
      expect "경로를 불렀다: $r ($n번)" "$([ "$n" -gt 0 ] && echo yes || echo no)" yes
    done
    ;;
  rush)
    expect "종료 코드" "$code" 0
    expect "올린 요약" "$uploaded" "0 True"
    expect "fake-api 가 센 규칙 위반" "$violations" "{}"
    # 회원 10명이 모두 결제 시도까지 갔는지(pay_ratio 1)
    expect "결제 시도까지 간 회원" "$(route_count "POST /api/v1/orders/{id}/payment-attempts")" 10
    for r in "GET /api/v1/products/{id}" "POST /api/v1/preorders/queue" "POST /api/v1/preorders" \
      "GET /api/v1/notifications/unread-count" "GET /api/v1/notifications" "GET /api/v1/preorders/{id}" \
      "POST /api/v1/orders" "POST /api/v1/orders/{id}/payment-attempts"; do
      n=$(route_count "$r")
      expect "경로를 불렀다: $r ($n번)" "$([ "$n" -gt 0 ] && echo yes || echo no)" yes
    done
    ;;
  queue)
    expect "종료 코드" "$code" 0
    expect "올린 요약" "$uploaded" "0 True"
    expect "fake-api 가 센 규칙 위반" "$violations" "{}"
    expect "접수한 회원" "$(route_count "POST /api/v1/preorders")" 30
    outcomes=$(printf '%s' "$summary" | python3 -c 'import json,sys; print(int(json.load(sys.stdin)["summary"]["metrics"]["entry_outcome"]["values"]["count"]))' 2> /dev/null || echo "없음")
    expect "진입 결과(entry_outcome)" "$outcomes" 30
    # 처리 기한은 180초가 넘는다. 모두 처리되면 그 전에 끝나야 한다
    expect "처리 기한 전에 끝났다(${elapsed}초)" "$([ "$elapsed" -lt 60 ] && echo yes || echo no)" yes
    ;;
  error)
    # 임계치가 없으니 오류가 많아도 끝까지 돈다. 실패한 요청은 요약에 남는다
    expect "오류가 계속되어도 정상 종료" "$code" 0
    expect "올린 요약" "$uploaded" "0 True"
    expect "계획한 시간까지 돌았다(${elapsed}초)" "$([ "$elapsed" -ge 30 ] && echo yes || echo no)" yes
    failed=$(printf '%s' "$summary" | python3 -c 'import json,sys; print(int(json.load(sys.stdin)["summary"]["metrics"]["http_req_failed"]["values"]["passes"]) > 0)' 2> /dev/null || echo "없음")
    expect "실패한 요청을 셌다" "$failed" True
    ;;
  sigterm)
    expect "외부 중지로 끝난 종료 코드" "$code" 105
    expect "올린 요약" "$uploaded" "105 True"
    ;;
  tokenfail)
    expect "토큰 실패로 끝난 종료 코드" "$code" 120
    expect "올린 결과(k6 요약 없음)" "$uploaded" "120 False"
    expect "k6 를 띄우지 않아 fake-api 가 받은 요청" "$(printf '%s' "$stats" | python3 -c 'import json,sys; print(sum(json.load(sys.stdin)["routes"].values()))')" 0
    ;;
esac
exit "$fail"
