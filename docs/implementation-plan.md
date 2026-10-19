# 구현 계획

어디에 무엇을 만드는지와 작업 단위를 적는다. 무엇을 왜 하는지는 [PLAN.md](../PLAN.md), 규칙과 근거는 [design.md](design.md)에 있다.

- 전제: 시험용 인프라는 아직 없다. 인프라 없이 할 수 있는 것을 먼저 하고, nova-infra PR이 적용되면 이어 붙인다.
- 기준: be는 main(`4d5863cb`, 2026-10-19)의 컨트롤러와 DTO 코드, nova-infra는 main의 시험 스택(`terraform/aws/loadtest`)을 보고 맞췄다. be의 `contracts/` 문서는 기준으로 쓰지 않는다.

## 1. 저장소 구성

```
nova-loadtest/
├── .github/
│   ├── CODEOWNERS               token-minter/, config/, .github/workflows/, tools/check-workflows/, .github/CODEOWNERS 는 담당자 리뷰 필수
│   └── workflows/
│       ├── ci.yml               PR, main push: actionlint, 워크플로 규칙, 린트, 단위 테스트, k6 inspect, 이미지 빌드(푸시 없음), k6-safety
│       ├── build.yml            main push: 이미지 3개 → ECR (커밋 해시, latest)
│       ├── loadtest.yml         수동: 입력 확인 → 승인 → 컨트롤러 → 결과 → 뒷정리
│       ├── stop.yml             수동: 시험 ID 의 k6 태스크 중지
│       └── cleanup.yml          수동(승인): 회차 마감 뒤 시험 ID 의 시험 데이터 정리
├── go.mod                       Go 모듈 하나(config, controller, token-minter). tools/check-workflows 는 따로
├── config/
│   ├── scenarios.yml            시나리오 기본값, 상한, 흐름 비율, 회원 ID 범위. 유일한 출처
│   └── config.go                scenarios.yml 을 실행 파일에 넣고(go:embed) 읽고 검사한다
├── k6/
│   ├── Dockerfile               linux/amd64. grafana/k6:2.3.0 의 k6 실행 파일 + k6-runner (distroless)
│   ├── main.js                  PLAN_JSON 을 읽어 시나리오(흐름별 실행기)를 만든다. setup 에서 접수할 옵션을 고른다
│   ├── test/e2e.sh              k6 이미지를 fake-api 와 moto 에 붙여 끝까지 돌린다(ok, error, sigterm). CI k6-safety 가 쓴다
│   └── lib/
│       ├── config.js            환경 변수 읽기와 검사
│       ├── http.js              요청 함수(경로 템플릿 필수), 공통 헤더, 응답 봉투(ApiResponse) 해석, error_code 카운터
│       ├── users.js             SharedArray 토큰, 풀 A/B/C 에서 회원 고르기
│       ├── fake.js              한글 가짜 데이터(배송지, 취소 사유). 회원 번호를 시드로 쓴다
│       └── flows/
│           ├── entry.js         진입 → 순번 확인 → 입장권 → 접수
│           ├── entry_flows.js   본 시험의 진입(풀 A)과 준비 구간(풀 B, C)
│           ├── preorders.js     내 예약 조회
│           ├── cancel.js        예약 취소(풀 C)
│           ├── order.js         주문 생성 → 결제 시도
│           ├── catalog.js       상품 조회
│           └── skip.js          요청 없이 넘어간 반복(skipped) 카운터
├── controller/                  컨트롤러(Go)
│   ├── cmd/controller/          태스크 진입점
│   ├── cmd/plan/                입력 확인. 러너도 이 명령으로 계획을 계산한다
│   ├── cmd/gha/                 러너에서 도는 명령: 컨트롤러 띄우고 기다리기(OIDC 세션 갱신), 결과 정리, 뒷정리, 중지
│   ├── cmd/k6-runner/           k6 이미지의 시작 프로그램: 토큰 받기 → START_AT 대기 → k6 run → 요약 올리기
│   ├── cmd/fake-api/            로컬·CI 시험용. be 공개 API 모양을 흉내 내고, User-Agent·헤더·토큰·멱등 키 규칙 위반을 센다
│   ├── cmd/fake-mimir/          로컬·CI 시험용. 정해 둔 각본(합격, 오류율 초과, 지표 끊김 등)대로 PromQL 에 답한다
│   ├── cmd/fake-ecs/            로컬·CI 시험용. RunTask/ListTasks/DescribeTasks/StopTask 를 받아 각본대로 태스크 상태와 종료 코드를 바꾼다
│   ├── Dockerfile               컨트롤러 이미지(distroless)
│   └── internal/
│       ├── plan/                입력 + scenarios.yml → 계획(도착률, 대수, 회원, VU, 시간)
│       ├── infra/               RUN_TASK, OBSERVABILITY 읽기와 검사, 로그에서 가릴 값
│       ├── runner/              RunTask(startedBy=시험 ID, tags 없음), 상태 보기, 중지, obs 태스크 IP 찾기
│       ├── mimir/               PromQL 조회(IP 다시 찾기), 오류율·5xx·지표 끊김·구간 누락·구간 판정 값
│       ├── control/             시험 한 번의 흐름(발급 → k6 → 감시 → 판정 → 결과)과 정리 모드
│       ├── report/              summary.json, summary.md
│       ├── awsio/               S3, SQS 를 control 의 인터페이스에 맞춤
│       ├── clock/               시계(테스트는 가짜 시계로 시간을 바로 흘린다)
│       └── fakeecs/             각본대로 도는 ECS. 단위 테스트와 fake-ecs 서버가 같이 쓴다
├── token-minter/                토큰 발급기(Go, distroless)
│   ├── Dockerfile
│   ├── cmd/token-minter/        환경 변수를 읽어 토큰 파일을 S3 에 올린다
│   └── internal/
│       ├── jwt/                 서명키 읽기(PKCS#8, RSA 2048 이상), 클레임 고정(role=USER, type=ACCESS), RS256 서명
│       └── mint/                회원 범위 검사, 태스크 수만큼 나누기, 토큰 파일 만들기
├── tools/
│   └── check-workflows/         워크플로 규칙 검사(Go, actionlint 파서). pull_request_target, continue-on-error, 해시로 고정하지 않은 uses 를 막는다
├── local/                       로컬 확인용(운영에 쓰지 않는다). 7절
│   ├── compose.yml              Forgejo, 러너, dind, moto, fake-ecs, fake-mimir
│   ├── forgejo/                 up.sh(띄우기, 계정과 러너 등록), mirror.sh(작업 트리 올리기), runner.yml
│   └── README.md                띄우는 법, 워크플로 수동 실행, 로컬 be 로 smoke 돌리는 법
└── docs/
    ├── implementation-plan.md   이 문서
    └── runbook.md               실행 절차, 승인 전 점검표(회차 일정, 경보), 시험 상품 준비와 마감, 정리, 멈추는 법 (C-4 에서 쓴다)
```

- 컨트롤러와 발급기는 같은 언어로 쓴다. 발급기를 distroless에 실행 파일 하나로 두려면 Go가 가장 단순하다. 그래서 둘 다 Go를 제안한다. 계획 계산(`cmd/plan`)을 러너와 컨트롤러가 같이 쓰므로 계산이 두 벌로 갈라지지 않는다.
- 결과, 토큰, 시험 회원 목록은 커밋하지 않는다(`.gitignore`).
- 이미지 3개는 모두 `linux/amd64`로 빌드한다(`docker buildx build --platform linux/amd64`, Go는 `CGO_ENABLED=0 GOARCH=amd64`). k6 실행 파일도 정적 빌드라 distroless에서 돈다.

## 2. 구성 요소

### 2.1 `config/scenarios.yml`

입력 확인, 컨트롤러, k6가 모두 이 파일에서 값을 읽는다.

| 항목 | 내용 |
|---|---|
| `flows` | 흐름별 평시 비율(상품 80, 예약 조회 10, 진입 5, 결제 시도 5, 취소 1), 회원 풀, 흐름당 요청 수(계획의 예상 요청 수 계산용), 진입 상한(입장 속도 150/초의 80% = 120/초), WAF 예외 없는 시나리오의 IP당 5분 요청 상한(2,400) |
| `scenarios.<이름>` | 모양(constant, ramping, spike, step), 기본 `peak_rps`(흐름 시작 수), `duration`, `ramp_up`, 상한(`max_rps`, `max_duration`, `max_vus`), WAF 예외 필요 여부, 풀 B 크기(smoke 50, 그 밖 1,000). spike는 몰림(500/초 × 10초), 배경 비율, 관찰 시간(10분). stress는 단계 배수(1·2·4·6·8)와 단계 시간(5분) |
| `runner` | 태스크당 초당 요청(처음 1,000), VU당 메모리(5MB), 태스크 크기(4 vCPU, 8GB), 여유 30%, 최대 태스크 수(`max_tasks`, Fargate vCPU 할당량에서 운영 사용분을 뺀 값) |
| `members` | 시험 회원 ID 범위(시작 번호, 개수), 풀 B 크기(예: 1,000명), 준비 구간 속도(초당 100건 이하), 등록 대기 시간(2분) |
| `cleanup` | 정리 전에 비어야 하는 SQS 큐 이름(등록, 취소, `preorder-events`, `order-events`), 최대 대기 30분. 이름만 두고 계정 ID는 넣지 않는다 |
| `slo` | 판정 기준(p95 0.5초, 5xx 0.1%), 중단 기준 기본값(오류율 5%, 5xx 1%, 둘 다 1분 넘게) |
| `metrics` | Mimir의 지표 이름(`k6_http_reqs_total` 등), `job`, 태스크 라벨, `le` 값. C-2에서 확정한 값으로 고친다 |

### 2.2 k6 (`k6/`)

**컨트롤러가 넘기는 값**

| 환경 변수 | 내용 |
|---|---|
| `TEST_ID` | 시험 ID(`lt-<yyyymmddThhmm>-<run_number>`). 모든 지표의 `test_id` 태그, User-Agent, 요약 경로에 쓴다 |
| `TASK_INDEX` | 태스크 번호 i. 토큰 파일과 요약 파일 이름에 쓴다 |
| `START_AT` | k6를 시작할 시각(UTC, RFC 3339). `k6-runner`가 이때까지 기다린다. 준비 구간이 여기서 시작하고, 본 시험은 그 뒤(`open_at`이 있으면 그 시각)에 시작한다 |
| `SEGMENT`, `SEGMENT_SEQUENCE` | `i/N:(i+1)/N`, `0,1/N,…,1` |
| `PLAN_JSON` | 흐름별 도착률, 단계, VU 수 |
| `PRODUCT_ID`, `PREP_PRODUCT_ID` | 진입 상품, 준비 구간 상품. 옵션 ID는 `setup()`에서 상품 상세로 읽는다 |
| `TOKENS_URI` | 토큰 위치(`s3://<버킷>/tokens/<test_id>/`). `k6-runner`가 여기서 받아 로컬 파일로 두고, k6는 로컬 파일만 읽는다. 상품 조회만 하는 시험에는 넘기지 않는다 |
| `RESULTS_URI` | 결과 위치(`s3://<버킷>/results/<test_id>/`). `k6-runner`가 `k6-<i>.json`을 여기에 올린다 |
| `BASE_URL` | `https://api.grandis-nova.link` |
| `LOADTEST_HEADER` | 부하 시험 헤더 값(작업 정의 `secrets`). 시나리오와 관계없이 늘 붙인다. 첫 smoke는 WAF 예외(I-5)를 적용하기 전에 돌므로, 헤더가 있어도 요청 제한을 받는다는 전제로 계획한다 |
| `OTEL_RESOURCE_ATTRIBUTES` | `service.instance.id=<i>,service.namespace=loadtest` |
| `K6_OTEL_HTTP_EXPORTER_ENDPOINT` | `observability.otlpEndpoint` 그대로(scheme 없는 `host:port`) |

- `LOADTEST_HEADER`는 작업 정의의 `secrets`로 들어간다. 나머지 값은 컨트롤러가 RunTask의 `containerOverrides`로 넣는다. k6 RunTask 하나의 덮어쓰기도 8KiB 안이어야 하므로 `PLAN_JSON` 크기를 함께 검사한다. `K6_OTEL_*`가 표준 `OTEL_*`보다 우선하므로, 수집 주소를 `OTEL_EXPORTER_OTLP_ENDPOINT`로 넣으면 무시될 수 있다.
- 시험마다 같은 OTel 설정은 작업 정의에 고정으로 있고, 컨트롤러는 넣지 않는다: `K6_OTEL_EXPORTER_PROTOCOL=http/protobuf`(기본은 grpc), `K6_OTEL_HTTP_EXPORTER_INSECURE=true`, `K6_OTEL_SERVICE_NAME`, `K6_OTEL_METRIC_PREFIX`. 내보내기 주기는 k6 기본 10초를 쓴다(지표 끊김 1분 기준의 바탕). 작업 정의에 `K6_OTEL_EXPORT_INTERVAL`을 넣지 않는다

**스크립트 규칙**

- 흐름 하나가 k6 시나리오 하나다. 실행기는 도착률 기반만 쓴다.
- 모든 요청에 `tags.name`을 경로 템플릿으로 붙인다. `systemTags`는 `status, method, name, scenario, expected_response`다(`url`은 넣지 않는다).
  - 요청은 `lib/http.js`의 함수로만 보낸다. 이 함수는 경로 템플릿과 값을 따로 받아(`req('GET', '/api/v1/products/{id}', {id})`) URL을 만들고, 템플릿을 그대로 `tags.name`에 넣는다. 템플릿이 아래 목록에 없으면 예외를 던진다.
  - 템플릿 목록(10개, 메서드까지 치면 12가지): `/api/v1/products`, `/api/v1/products/{id}`, `/api/v1/categories`, `/api/v1/preorders/queue`, `/api/v1/preorders`, `/api/v1/preorders/active`, `/api/v1/preorders/{id}`, `/api/v1/preorders/{id}/cancel`, `/api/v1/orders`, `/api/v1/orders/{id}/payment-attempts`. 대기열과 예약 목록은 GET과 POST가 같은 이름을 쓰고 `method` 태그로 갈린다. 쿼리 문자열은 이름에 넣지 않는다.
  - `ci.yml`의 검사: `lib/http.js` 밖에서 `k6/http`를 바로 부르면 실패한다. k6-safety 작업에서 시험용 응답 서버로 흐름을 돌리고, 나온 `name` 값이 모두 목록 안에 있는지 확인한다.
- `options.userAgent = 'nova-loadtest/' + TEST_ID`로 둔다. `lib/http.js`의 공통 헤더에 `User-Agent`를 넣지 않는다(넣으면 옵션을 덮는다).
- `handleSummary`는 JSON 요약 하나만 파일로 쓴다. `setup()`은 토큰이나 헤더 값을 반환하지 않는다(요약에 들어간다).
- `discardResponseBodies: true`로 두고, 본문이 필요한 요청만 `responseType: 'text'`로 받는다.
- 진입 흐름은 202이면 `Retry-After` 초만큼 `sleep` 한 뒤 다시 확인한다. 확인 응답에 새 `queueToken`이 오면 그 값으로 바꾼다(만료가 가까울 때 서버가 바꿔 준다). 서버가 주는 간격을 그대로 따른다(늦으면 서버가 이탈자로 보고 줄에서 뺀다). 진입부터 접수 응답까지 걸린 시간을 `entry_to_accept` Trend로 남긴다. `new Trend('entry_to_accept', true)`로 만들어 단위를 ms로 맞춘다.
- 진입 시나리오의 `gracefulStop`은 3분이다. 줄에 서 있던 회원이 시험 끝에 잘리지 않게 한다.
- spike의 몰림 시나리오는 `open_at` + 1초에 시작한다(`startTime`을 컨트롤러가 계산해 `PLAN_JSON`에 넣는다).
- 결제와 취소 흐름은 `GET /api/v1/preorders?productId=<PREP_PRODUCT_ID>`(결제는 `&status=REGISTERED`)로 예약을 찾는다. `/active`는 상품 필터가 없어 지난 시험의 예약이 섞인다. `CLOSED/NOT_IN_QUEUE`이면 한 번 다시 진입하고, `SALE_CLOSED`이면 끝낸다.
- 접수의 `Idempotency-Key`는 `TEST_ID`와 회원 번호로 만든다. 같은 회원이 다시 보내도 중복 접수가 생기지 않는다.
- 준비 구간은 `startTime` 0의 별도 시나리오다. 본 시나리오의 `startTime`은 준비 구간 + 등록 대기 시간이다. 컨트롤러가 계산해 `PLAN_JSON`에 넣는다.
- 결제 흐름은 `REGISTERED` 예약이 없으면 요청을 보내지 않고 `skipped`로 센다. 준비가 늦었는지 결과에서 보인다.
- 모든 태스크에 같은 임계치를 둔다: `http_req_failed: [{ threshold: 'rate<0.05', abortOnFail: true, delayAbortEval: '1m' }]`. 값은 `PLAN_JSON`의 `abort_error_rate`(%, 기본 5)를 100으로 나눠 넣는다. 누적 비율이라 늦게 걸릴 수 있다([설계 세부 "결과 합치기와 판정"](design.md#결과-합치기와-판정)).
- 기대 상태는 요청마다 `responseCallback: http.expectedStatuses(...)`로 준다(대기열 200·202, 접수 202 등). 목록은 `lib/http.js` 한 곳에 둔다. 콜백은 상태 코드만 보므로, 409처럼 오류 코드(`ACTIVE_PREORDER_EXISTS` 등)로 갈라야 하는 응답은 본문을 받아 `check`와 `error_code` 태그 카운터로 센다.

**`k6-runner`**

- 태스크 역할로 S3에서 `tokens/<test_id>/{a,b,c}-<i>.json`을 받아 로컬 파일로 둔다.
- `START_AT`까지 기다렸다가 k6를 자식 프로세스로 띄운다. SIGTERM을 받으면 k6에 넘기고 끝날 때까지 기다린다.
- k6가 끝나면 `{exit_code, image_digest, summary}`를 `results/<test_id>/k6-<i>.json`에 올리고, k6의 종료 코드로 끝난다.
- 미리 서명한 URL은 쓰지 않는다. 태스크 역할의 임시 자격 증명으로 서명한 URL은 그 자격 증명이 끝나면 함께 만료된다. soak처럼 긴 시험에서는 끝날 때 요약을 올리지 못할 수 있다.

**참고할 be 자산**: `waitingroom/load-test/entry-spike.js`, `preorder/load-test/accept-spike.js`, `preorder/load-test/admission-ticket.js`, `waitingroom/load-test/make-tokens.py`

### 2.3 토큰 발급기 (`token-minter/`)

| 항목 | 내용 |
|---|---|
| 입력 | 시험 ID, 회원 번호 범위(풀 A, B, C), 태스크 수 N, 유효 시간. 환경 변수 이름은 [설정과 비밀 값](configuration.md#컨트롤러가-토큰-발급기에-넘기는-값runtask-덮어쓰기)에 있다 |
| 출력 | S3 `tokens/<test_id>/{a,b,c}-<i>.json`. 원소는 `{n, sub, token}` |
| 고정 | `role=USER`, `type=ACCESS`, `typ=at+jwt`. 입력으로 바꿀 수 없다 |
| 작업 정의 값 | `JWT_KEY_ID`(헤더 `kid`에 그대로, 개인키에서 계산하지 않는다), `JWT_ISSUER`(`iss`), `JWT_AUDIENCE`(`aud`). 인프라가 member 설정에서 읽어 넣는다. 하나라도 비면 시작하자마자 실패한다. 컨트롤러는 이 값을 덮어쓰지 않는다 |
| 거절 | 시험 회원 범위 밖 번호, 유효 시간 6시간 30분 초과. 범위는 빌드할 때 `scenarios.yml`에서 이미지에 넣는다. 실행 입력이나 환경 변수로 바꿀 수 없다 |
| 서명 | 환경 변수의 서명키(작업 정의 `secrets`)로 RS256. 값은 RSA 2048의 PKCS#8 PEM 문자열 하나(JSON 아님, 줄바꿈 포함)다. `x509.ParsePKCS8PrivateKey`로만 읽고, RSA가 아니거나 2048 비트 미만이면 실패한다. 키와 토큰은 로그에 남기지 않는다 |
| 이미지 | distroless, 실행 파일 하나, 셸 없음 |

- 시험 회원 `sub`는 회원 번호에서 계산한다: `10ad7e57-0000-7000-8000-<12자리 번호>`(be와 확정).
- 단위 테스트는 시험용 키쌍으로 서명하고, be의 검증 규칙(필수 클레임 `iss`, `aud`, `sub`, `sid`, `jti`, `role`, `type`, `iat`, `exp`와 헤더 `typ`, `kid`)대로 다시 검증해 본다. `kid`, `iss`, `aud`가 환경 변수 값과 같은지, 셋 중 하나가 비면 실패하는지도 본다.

### 2.4 컨트롤러 (`controller/`)

| 단계 | 하는 일 |
|---|---|
| 입력 검사 | 워크플로가 넘긴 `RUN_TASK`, `OBSERVABILITY`(SSM 두 값의 JSON, 아래 형식)의 필수 항목과 형식을 본다. `otlpEndpoint`는 scheme 없는 `host:port`여야 하고(`://`가 있으면 거절), `mimir.port`는 숫자, `queryPath`는 `/`로 시작해야 한다. 시험 모드면 `tasks`에 `tokenMinter`, `k6`가, 정리 모드면 `data`가 있어야 한다. 틀리면 아무것도 띄우지 않고 실패한다 |
| 계획 | `cmd/plan`과 같은 코드로 계획을 다시 계산하고, 러너가 넘긴 값과 다르면 멈춘다 |
| 토큰 | 발급기 태스크를 실행하고 끝날 때까지 기다린다 |
| 시작 | k6에 넘길 `K6_OTEL_*`를 수집 주소로 만든다(2.2). RunTask는 `RUN_TASK.tasks.<이름>`의 작업 정의 패밀리, 컨테이너 이름(`containerOverrides[].name`), 네트워크 세 키(`awsvpcConfiguration`에 그대로)와 `launchType=FARGATE`로 부른다. `START_AT`은 지금 + 2분이다. `open_at`이 있으면 `open_at` − (준비 구간 + 등록 대기)로 잡는다. k6 태스크 N 대를 실행한다. RunTask는 한 번에 10대까지라 나눠 부르고, 용량 부족 오류는 다시 시도한다. `tags`는 넘기지 않고 `startedBy`에 시험 ID를 넣는다. 뒷정리와 `stop.yml`은 `ListTasks(startedBy)`로 찾고 `DescribeTasks`의 작업 정의로 k6와 컨트롤러를 가른다. 그 시각까지 모두 RUNNING이 아니면 취소한다 |
| 감시 | `START_AT` + 1분부터 15초마다 Mimir를 조회한다. Mimir 주소는 `OBSERVABILITY.mimir`의 클러스터와 서비스로 찾는다(`ListTasks` RUNNING → `DescribeTasks`의 ENI `privateIPv4Address`, 여럿이면 `startedAt`이 가장 늦은 것). 조회가 실패하면 다시 찾는다. 오류율이나 5xx 비율이 중단 기준을 1분 넘게 넘으면 모두 멈춘다(중단). 태스크 하나가 계획한 끝 시각 전에 99로 끝나도 나머지를 모두 멈춘다(중단). 태스크마다 `vus`의 마지막 시각을 보고, 1분 넘게 비거나 Mimir 조회가 1분 넘게 실패하면 모두 멈춘다(판정 불가) |
| 판정 | 먼저 태스크마다 시험 구간 전체의 지표가 있는지 본다. 빠진 곳이 있으면 판정 불가로 둔다. 다 있으면 전체 구간과 유지 구간을 따로, 전체와 흐름(`name`)별로 판정한다. 결과는 합격, 불합격, 중단, 판정 불가, 한계 측정(stress), 취소 중 하나다. 취소는 부하를 보내기 전에 멈춘 경우다(시작 시각까지 RUNNING 아님, 이미지 섞임, 발급기 실패, 시작 시각이 너무 가까움). stress는 단계마다 판정해 기준을 지킨 마지막 단계를 적는다. k6 종료 코드, 태스크별 요약(`k6-<i>.json`)의 요청 수와 실패 수를 함께 적는다. 요약이 없는 태스크는 그렇다고 적는다. 실행기 CPU(Container Insights)는 아직 적지 않는다(C-5에서 더한다) |
| 기록 | S3 `results/<test_id>/plan.json`, `summary.json`, `summary.md`. 태스크별 `k6-<i>.json`은 `k6-runner`가 쓴다 |
| 정리 | 토큰 파일을 지운다. 시험 데이터는 지우지 않는다 |
| 정리 모드 | `cleanup.yml`이 `MODE=cleanup`으로 띄운다. `results/<test_id>/plan.json`에서 시험 상품 ID(`product_id`와 `prep_product_id`. spike에서는 둘이 다르다)와 회원 범위를 읽고, 두 상품을 모두 정리한다. 시험 클러스터에 다른 시험 ID의 태스크가 있으면 거절한다. `scenarios.yml`의 큐가 빌 때까지(최대 30분) SQS `GetQueueAttributes`로 기다린 뒤 정리 태스크를 실행하고, `cleanup.json`을 쓴다. 비지 않으면 정리하지 않고 실패한다. 정리 태스크에는 지금 `TEST_ID`, `PRODUCT_IDS`(쉼표로 구분), `MEMBERS`(풀 A, B, C 범위의 JSON)를 넘긴다. 이 형식은 인프라 I-6과 정한다 |

**판정 쿼리**. 지금 예상하는 이름은 `<dur>` = `k6_http_req_duration_milliseconds`, `<reqs>` = `k6_http_reqs_total`, `<vus>` = `k6_vus`다(접두사 `k6_`에 Mimir가 단위와 종류 접미사를 붙인다). 모든 쿼리에 `job="loadtest/k6"`도 건다. 이름과 `le` 값의 형식(`500`인지 `500.0`인지)은 C-2에서 확정하고 `scenarios.yml`에 둔다. 코드에 박지 않는다.

- 0.5초 안 비율: `sum(rate(<dur>_bucket{test_id="X",le="500"}[w])) / sum(rate(<dur>_count{test_id="X"}[w]))` ≥ 0.95
- 5xx 비율: `sum(rate(<reqs>{test_id="X",status=~"5.."}[w])) / sum(rate(<reqs>{test_id="X"}[w]))` ≤ 0.001
- 오류율(중단 기준): `sum(rate(<reqs>{test_id="X",expected_response="false"}[1m])) / sum(rate(<reqs>{test_id="X"}[1m]))`. k6의 `http_req_failed`와 같은 정의다
- 실제 부하: `sum(rate(<reqs>{test_id="X"}[1m]))`. 계획의 95% 미만이면 "부하 부족"으로 표시한다
- 밀린 반복: `dropped_iterations`가 0보다 크면 VU가 모자란 것이다
- 지표 끊김: `time() - max by (instance) (timestamp(<vus>{test_id="X"}))`가 60을 넘거나, 계획한 태스크 수보다 적게 나오면 끊긴 것이다. 요청 지표는 흐름이 쉬는 구간(등록 대기)에 멈출 수 있어 `vus`를 쓴다
- 태스크 라벨은 `instance`로 본다. `job`이 `loadtest/k6`로 나온다는 것은 Mimir가 리소스 속성을 OTLP 규칙대로 바꾼다는 뜻이고, 같은 규칙에서 `service.instance.id`는 `instance`가 된다. C-2에서 확인한다

### 2.5 워크플로

| 워크플로 | 작업(job) | 역할 |
|---|---|---|
| `ci.yml` | workflow-lint, lint-test, k6-inspect, image-build, k6-safety(7.2의 2단계) | 없음 |
| `build.yml` | build-push(이미지 3개 행렬) | `nova-prod-loadtest-github-build` |
| `loadtest.yml` | `plan` → `run`(environment `loadtest`) | `plan`은 build 역할로 ECR만 조회한다. `run`은 `nova-prod-loadtest-github-run` |
| `stop.yml` | stop | `nova-prod-loadtest-github-build` |
| `cleanup.yml` | cleanup(environment `loadtest`) | `nova-prod-loadtest-github-run` |

- `plan`: `cmd/plan`을 실행하고 ECR에 `latest`가 있는지 확인한다. 회원 수는 DB를 볼 수 없으므로 `scenarios.yml`의 시험 회원 수와 비교한다. 계획 표를 실행 요약에 쓴다.
- `run`: 승인을 한 번만 받으려고 사전 점검, 컨트롤러 실행, 결과 정리, 뒷정리를 한 작업의 단계로 둔다.
  - 사전 점검: 공개 상품 API로 시험 상품, 판매 상태, 옵션을 확인한다.
  - 컨트롤러: `cmd/gha run`이 SSM `run-task`와 `observability`를 읽어, 값을 컨트롤러 RunTask의 환경 변수(`RUN_TASK`, `OBSERVABILITY`)로 넘겨 띄우고 기다린다. 값은 출력하지 않고, 읽은 직후 JSON의 값마다 `::add-mask::`를 낸다. 가리지 않는 키 목록([설계 세부 "인프라 값"](design.md#인프라-값))에 없는 값은 모두 가린다. RunTask의 덮어쓰기는 모두 합쳐 8KiB까지라 `PLAN_JSON`과 함께 크기를 검사한다. 50분마다 `ACTIONS_ID_TOKEN_REQUEST_URL`로 OIDC 토큰을 다시 받아 세션을 바꾼다. 컨트롤러가 `summary.json` 없이 끝나면 판정 불가로 둔다.
  - 결과 정리: S3 결과를 실행 요약과 Slack으로 보낸다. Slack 웹훅은 GitHub 비밀이다.
  - 뒷정리: `if: always()`. 역할을 다시 받은 뒤 `startedBy=<시험 ID>`인 태스크를 모두 멈추고 토큰을 지운다.
  - 마지막 단계에서 판정 결과가 합격이 아니면 작업을 실패로 끝낸다.
- `stop`: `startedBy=<시험 ID>`인 k6 태스크를 멈춘다. 컨트롤러가 끝까지 남아 "중단" 결과를 쓴다.
- `cleanup`: 입력은 시험 ID 하나다. `results/<test_id>/summary.json`이 있는지 보고(끝난 시험만), `tasks.data`가 없으면 바로 실패한다. 컨트롤러를 정리 모드로 띄우고 기다린다. `startedBy`는 `<시험 ID>-c`로 해 시험 태스크와 가른다.
- `loadtest.yml`은 `concurrency: { group: loadtest, cancel-in-progress: false }`. 한 그룹에서 기다릴 수 있는 실행은 하나뿐이라, 시험이 도는 동안 두 번 넘게 실행하면 앞서 기다리던 것이 취소된다(runbook에 적는다).
- `build.yml`은 이 그룹에 넣지 않는다(기다리던 빌드가 시험 실행에 밀려 취소된다). 커밋 해시 태그를 올린 뒤 시험 클러스터의 RUNNING 태스크를 `ListTasks`로 보고, 없을 때만 `latest`를 옮긴다. 있으면 `latest`를 옮기지 않고 실패한다. 남는 틈은 컨트롤러가 막는다: 시작 시각 전에 k6 태스크들의 `imageDigest`(`DescribeTasks`)가 다르면 시험을 취소한다

**SSM 값 형식**(인프라가 정했다. 경로 `/nova-prod/loadtest/run-task`, `/nova-prod/loadtest/observability`, String, 값은 JSON)

```json
{
  "cluster": "nova-prod-loadtest",
  "bucket": "nova-prod-loadtest-<suffix>",
  "tasks": {
    "controller":  { "taskDefinition": "nova-prod-loadtest-controller",   "container": "controller",   "subnets": [], "securityGroups": [], "assignPublicIp": "DISABLED" },
    "tokenMinter": { "taskDefinition": "nova-prod-loadtest-token-minter", "container": "token-minter", "subnets": [], "securityGroups": [], "assignPublicIp": "DISABLED" },
    "k6":          { "taskDefinition": "nova-prod-loadtest-k6",           "container": "k6",           "subnets": [], "securityGroups": [], "assignPublicIp": "ENABLED" }
  }
}
```

```json
{
  "otlpEndpoint": "otel-ingest.nova.local:4318",
  "mimir": { "cluster": "nova-prod-observability", "service": "nova-prod-obs", "port": 9009, "queryPath": "/prometheus/api/v1/query" }
}
```

- `taskDefinition`은 패밀리 이름이라 최신 개정판이 뜬다.
- 준비·정리 작업 정의(I-6)가 생기면 `tasks.data`가 같은 형식으로 더해진다(인프라에 요청).

### 2.6 S3

| 버킷 | 경로 | 보관 |
|---|---|---|
| `run-task.bucket` 하나 | `tokens/<test_id>/{a,b,c}-<i>.json` | 암호화. 시험이 끝나면 컨트롤러가 바로 지운다. `tokens/` 접두사 수명 주기(1일)는 남은 것을 치우는 안전망이다. 수명 주기 삭제는 하루쯤 늦을 수 있어 이것에 기대지 않는다 |
| 같은 버킷 | `results/<test_id>/{plan.json,summary.json,summary.md,k6-<i>.json}` | 180일 뒤 삭제(`results/` 접두사 수명 주기). 버킷 전체에 거는 규칙은 없다 |

## 3. nova-infra 변경

nova-infra의 "PR 하나에 층 하나" 규칙을 따른다. 층 번호는 `atlantis.yaml`의 적용 순서다.

| PR | 층 | 위치 | 내용 |
|---|---|---|---|
| I-1 | 2 | ecr, s3, secretsmanager 스택 | ECR 3개(`nova/loadtest-k6`, `nova/loadtest-controller`, `nova/loadtest-token-minter`), S3 1개(접두사별 수명 주기: `tokens/` 1일, `results/` 180일), 부하 시험 헤더 비밀(생성값), 정리용 DB 계정 비밀(값은 나중에 넣는다) |
| I-1 | 2 | `securitygroup/nova_apnortheast2/loadtest.tf` | SG 4개(k6, 컨트롤러, 발급기, 정리). k6: 443 인터넷, 4318 → `otel_gateway`. 컨트롤러: 9009 → obs, 443. 발급기: 443. `otel_gateway`, obs에 인바운드 규칙(`edge_logs.tf`와 같은 방식) |
| I-2 | 4 | `iam/grandis-nova/loadtest.tf` | GitHub OIDC 역할 2개, 태스크 역할 4개(컨트롤러, 발급기, k6, 데이터), 실행 역할 3개(발급기: 서명키, k6: 부하 시험 헤더, 데이터: DB 계정. 컨트롤러는 공용 실행 역할) |
| I-3 | 5 | `ecs/nova_apnortheast2/loadtest.tf` | 클러스터 `nova-prod-loadtest`(FARGATE, Container Insights 켬), 작업 정의 3개, 로그 그룹, SSM `/nova-prod/loadtest/run-task` |
| I-4 | 6 | `monitoring/lgtm/nova_apnortheast2/` | SSM `/nova-prod/loadtest/observability`, `gateway/collector.yaml`에 k6 지표 필터(`K6_OTEL_METRIC_PREFIX`가 붙은 이름), `config/grafana/dashboards/loadtest/k6.json` |
| I-5 | 6 | `edge/grandis-nova/waf.tf` | `ApiRateLimit`, `QueueRateLimit`의 scope-down에 "부하 시험 헤더가 값과 같지 않음"을 더한다 |
| I-6 | 5 | `ecs/nova_apnortheast2/loadtest.tf` | 데이터 작업 정의(준비: 시험 회원 넣기, 정리: 시험 데이터 지우기. 같은 이미지에 명령만 다르다), `run-task`에 `tasks.data` 추가. be SQL과 DB 계정이 정해진 뒤. SG, 역할, 비밀 자리는 I-1, I-2에서 미리 만든다(층을 섞지 않으려고) |

**IAM에서 지킬 것**

- OIDC 역할을 둘로 나눈다. 하나로 두면 environment 승인을 거치지 않는 main 워크플로도 시험을 띄울 수 있다.
  - `nova-prod-loadtest-github-run`: `sub` = `repo:<저장소>:environment:loadtest`. 컨트롤러 RunTask, 컨트롤러 태스크·실행 역할 PassRole, 시험 클러스터 DescribeTasks/ListTasks/StopTask(뒷정리), 결과 S3 읽기, 토큰 S3 지우기, SSM `run-task`·`observability` 읽기
  - `nova-prod-loadtest-github-build`: `sub` = `repo:<저장소>:ref:refs/heads/main`. ECR 푸시와 조회(`plan`의 `latest` 확인), 시험 클러스터 ListTasks/DescribeTasks/StopTask(`stop.yml`이 k6만 고르려면 DescribeTasks가 필요하다)
  - 저장소 지정은 기존 역할처럼 immutable subject 형식을 쓴다. 최대 세션은 1시간(기본)이다.
- **서명키는 실행 역할이 읽는다.** 작업 정의의 `secrets`는 실행 역할 권한으로 꺼내기 때문이다. 그래서 발급기는 공용 `nova-prod-ecs-task-execution`을 쓰지 않고 전용 실행 역할을 쓴다. 서명키 비밀(`nova-prod/app/member/JWT_PRIVATEKEY`)은 member와 이 역할만 읽는다.
- k6 실행 역할은 부하 시험 헤더 비밀만 읽는다. k6 태스크 역할은 버킷의 `tokens/*` 읽기와 `results/*/k6-*.json` 쓰기만 가진다.
- RunTask에 `tags`를 넘기지 않으므로 어느 역할에도 `ecs:TagResource`는 필요 없다.
- 컨트롤러 역할: k6, 발급기, 데이터 작업 정의 RunTask/StopTask/DescribeTasks(조건 `ecs:cluster` = 시험 클러스터), 이 세 태스크의 태스크 역할과 실행 역할 PassRole, 관측 클러스터 ListTasks/DescribeTasks(obs IP), `cloudwatch:GetMetricData`(실행기 CPU), 정리 전에 볼 큐의 `sqs:GetQueueUrl`, `sqs:GetQueueAttributes`(`scenarios.yml`에는 큐 이름만 있어 URL을 먼저 찾는다), 시험 클러스터 ListTasks(정리 모드에서 다른 시험 확인), 결과 S3 읽기(정리 모드의 `plan.json`)와 쓰기, 토큰 S3 지우기

**작업 정의**

| 작업 정의 | 크기 | 네트워크 | 설정 |
|---|---|---|---|
| 공통 | | | `runtimePlatform` X86_64 / LINUX |
| k6 | 4 vCPU, 8GB | public 서브넷, `assignPublicIp=ENABLED` | `stopTimeout` 120, `ulimits nofile` 65536, `secrets` 부하 시험 헤더. OTel 고정값(전송 방식, 평문, 서비스 이름, 지표 접두사). 수집 주소는 컨트롤러가 넣는다 |
| 컨트롤러 | 0.25 vCPU, 512MB | app 서브넷 | SSM 값(버킷 이름 포함)은 워크플로가 RunTask 덮어쓰기로 넘긴다. 태스크 역할에 SSM 권한은 없다 |
| 발급기 | 0.5 vCPU, 1GB | app 서브넷 | 전용 실행 역할, `secrets` 서명키, 환경 변수 `JWT_KEY_ID`, `JWT_ISSUER`, `JWT_AUDIENCE`(member 설정에서) |

## 4. be에 요청할 것

| # | 요청 | 쓰는 곳 |
|---|---|---|
| R-1 | 시험 회원 생성 SQL. 규칙 `id=10ad7e57-0000-7000-8000-<12자리>`, `kakao_id=loadtest-<n>`, 10만 명 | 시험 데이터 준비 |
| R-2 | 정리 SQL. 시험 회원 범위와 시험 상품 ID로 [설계 세부 "정리"](design.md#정리) 표의 대상을 지운다. 지운 뒤 확인 쿼리(be `preorder/load-test/invariant-check.sql` 참고) | 정리 태스크 |
| R-4 | 시험 상품을 목록에서 뺄 방법이 있는지 | 시험 상품 노출 |
| R-5 | (나중) breakscale 04~08을 위해 운영 설정을 바꿀 때 협조 | 확대 시험 |
| R-6 | 로컬 전체 스택을 `docker/aot-training`의 구성(MySQL, Redis, Floci, 서비스 이미지, 매번 새로 만드는 JWT 키)으로 띄워 둘 수 있는지(`KEEP=1`). worker가 부르는 Mock과 결제 쪽 토스 흉내가 들어 있는지 | 7.2의 4단계 |

## 5. 작업 단위

### A. 인프라 없이

| # | 작업 | 완료 기준 |
|---|---|---|
| A-0 | 로컬 Forgejo 환경(`local/`) | `docker compose up`으로 Forgejo와 러너가 뜨고, 이 저장소를 올리면 Actions가 돈다. `github.server_url`이 채워지는지 확인해 적는다 |
| A-1 | 저장소 뼈대, `ci.yml`, CODEOWNERS | 로컬 Forgejo와 GitHub PR 양쪽에서 `ci.yml`이 같은 결과로 돈다. `ci.yml`은 `pull_request`와 main push에서 돌고 `permissions: contents: read`이며, `actionlint`와 `pull_request_target` 금지 검사가 있다 |
| A-1a | GitHub 저장소 설정(저장소 관리자) | main 브랜치 보호(리뷰 필수, 코드 소유자 리뷰 필수), `loadtest` environment(승인자 지정, 배포 브랜치 main만). **B-2보다 먼저 끝낸다** |
| A-2 | `scenarios.yml`, `cmd/plan` | 시나리오 5개의 계획이 표로 나온다. 상한 초과, 회원 부족, VU 초과가 실패하는 테스트가 있다 |
| A-3 | k6 흐름 스크립트, Dockerfile, `k6-runner` | 로컬 be에 시험용 공개키를 넣고 smoke를 돌리면 흐름 다섯 개가 모두 성공한다. be 로그의 User-Agent가 `nova-loadtest/<시험 ID>`다. moto S3에 `k6-0.json`이 올라가고, 도중에 SIGTERM을 보내도 올라간다. `k6 inspect` 통과 |
| A-3a | k6 안전장치 | `ci.yml`의 k6-safety 작업이 통과한다: 시험용 응답 서버가 오류를 내면 1분 뒤 99로 끝나고, 기대 상태(대기열 202 등)만 낼 때는 0으로 끝난다 |
| A-4 | 토큰 발급기 | 시험용 키로 만든 토큰을 로컬 be가 받는다. `role` 고정, 범위 밖 거절, `JWT_KEY_ID`·`JWT_ISSUER`·`JWT_AUDIENCE`가 비면 실패하는 테스트가 있다. distroless 이미지가 빌드된다 |
| A-5 | 컨트롤러 | ECS와 Mimir를 가짜로 바꾼 테스트로 결과 다섯이 모두 확인된다(한계 측정: stress 단계 3에서 기준 초과): 시작 취소, 중단(오류율, 태스크 하나 99), 판정 불가(태스크 하나의 지표 끊김, Mimir 조회 실패, 구간 누락), 이미지가 섞이면 시작 전 취소, 합격·불합격. 끝 시각에 99로 끝난 태스크는 중단으로 보지 않는다 |
| A-6 | `loadtest.yml`, `stop.yml`, `cleanup.yml` | 로컬 Forgejo에서 7.2의 3단계 각본이 모두 기대한 결과로 끝난다 |

### B. nova-infra

| # | 작업 | 선행 | 완료 기준 |
|---|---|---|---|
| B-1 | I-1 (ECR, S3, 비밀, SG) | | Atlantis apply |
| B-2 | I-2 (IAM) | B-1, A-1a | apply. 시험 실행 역할을 environment 없이 맡으려 하면 거절된다. PR 본문에 A-1a 설정 화면을 붙인다 |
| B-3 | I-3 (ECS, SSM `run-task`) | B-2 | apply. `run-task` 값으로 RunTask 할 수 있다 |
| B-4 | I-4 (SSM `observability`, 게이트웨이 필터, 대시보드) | C-2에서 확정한 지표 이름 | apply. 대시보드에 smoke 결과가 보인다. 필터를 기다리느라 늦어지면 SSM만 먼저 올려도 된다 |
| B-5 | I-5 (WAF 예외) | B-1 | apply. 헤더가 없는 요청은 그대로 막힌다 |
| B-6 | I-6 (준비·정리 태스크) | R-1, R-2, DB 계정 | apply. 준비 태스크로 시험 회원이 들어가고, 정리 태스크 뒤 확인 쿼리가 0 건이다 |

### C. 이어 붙이기

| # | 작업 | 선행 | 완료 기준 |
|---|---|---|---|
| C-1 | `build.yml` | B-3 | main push로 이미지 3개가 ECR에 올라간다 |
| C-2 | 상품 조회만 smoke, k6 1대 RunTask(손으로) | C-1 | Mimir에서 k6 지표가 보인다. 지표 이름, 시계열 수, `service.instance.id`를 확인한다 |
| C-3 | 발급기 운영 실행 | C-1, R-1 | 발급한 토큰으로 `GET /api/v1/preorders`가 200이다 |
| C-4 | `loadtest.yml` 전체 smoke(시험 상품, 회원 수백 명), `docs/runbook.md` | C-2, C-3, B-4 | 실행 요약에 결과가 나오고 정리가 끝난다. 7.3의 GitHub에서만 확인할 것을 모두 확인한다 |
| C-5 | 실측 반영 | C-4 | 태스크당 초당 요청, VU 메모리를 `scenarios.yml`에 반영한다 |

### D. 확대

load → spike → stress, soak 순서로 올린다. 매번 담당자 승인을 받는다. 그다음 운영 설정을 바꾸는 breakscale 시나리오(04~08)를 따로 계획한다.

## 6. smoke에서 확인할 것

- Mimir에 들어간 k6 지표 이름(단위 접미사)과 시험 하나의 시계열 수
- `OTEL_RESOURCE_ATTRIBUTES`가 k6 리소스 속성에 실제로 들어가는지. k6 문서는 표준 `OTEL_*` 변수를 쓸 수 있다고만 하고 이 변수를 따로 적지 않았다. 안 들어가면 `options.tags`에 `instance`를 넣고 쿼리의 라벨을 바꾼다
- k6가 컨트롤러가 넣은 수집 주소와 작업 정의의 고정값(`http/protobuf`, 평문)으로 게이트웨이에 보내는지. k6 로그에 내보내기 오류가 없는지
- 실제 지표 이름과 라벨: `k6_http_reqs_total`, `k6_http_req_duration_milliseconds_bucket`(`le="500"`의 형식), `k6_vus`, `job="loadtest/k6"`, 태스크 라벨 `instance`. `scenarios.yml`의 지표 이름과 fake-mimir 각본을 이 값으로 맞춘다
- `http_req_failed` 같은 Rate 지표가 `condition` 속성으로 나오는지(k6 문서에 형식이 바뀌었다고 되어 있다). 오류율은 `http_reqs`의 `expected_response`로 구하므로 판정에는 영향이 없지만 대시보드 쿼리에 쓴다
- `k6-runner`가 운영 S3에서 토큰을 받고 `k6-<i>.json`을 올리는지. `StopTask`로 멈췄을 때도 120초 안에 올라가는지
- CloudFront 로그에 User-Agent `nova-loadtest/<시험 ID>`가 그대로 남는지
- 태스크 하나가 낼 수 있는 초당 요청과 VU 하나의 메모리

## 7. 테스트와 로컬 CI

PR의 CI는 GitHub Actions에서 돈다. 워크플로 자체(입력, 단계 순서, 실패 처리, 실행 요약)는 운영에 닿지 않게 로컬 Forgejo에서 먼저 돌려 본다. Forgejo Actions는 GitHub Actions와 문법이 비슷하지만 호환을 목표로 하지 않으므로, 두 곳에서 다르게 도는 부분을 아래처럼 나눠 둔다.

### 7.1 로컬 구성 (`local/compose.yml`)

| 컨테이너 | 이미지 | 하는 일 |
|---|---|---|
| forgejo | `codeberg.org/forgejo/forgejo:17.0.0` | 저장소와 Actions. `[actions] DEFAULT_ACTIONS_URL=https://github.com`으로 두어 `uses:`가 GitHub과 같은 액션을 받는다 |
| runner | `code.forgejo.org/forgejo/runner:13.2.0` | 작업 실행. 13.2부터 실행 요약을 지원한다. 라벨 `ubuntu-latest`는 `node:24.19.0-bookworm`이다. 이 이미지는 `up.sh`가 호스트에서 받아 dind에 넣는다(dind가 레지스트리에 IPv6로만 붙으려다 실패하는 환경이 있다). Go 도구 캐시는 dind 볼륨에 남겨 실행마다 다시 받지 않는다 |
| dind | `docker:29.7.2-dind` | 러너가 쓰는 Docker. 호스트의 Docker 소켓은 넘기지 않는다 |
| moto | `motoserver/moto` (고정) | S3, ECR, SSM, SQS 흉내(SQS는 정리 각본의 큐). SSM의 두 경로에 인프라와 같은 형식의 가짜 JSON을 넣어 둔다(수집 주소는 compose 안의 가짜 수신기, Mimir는 fake-ecs가 돌려주는 가짜 obs 태스크 IP) |
| fake-ecs | 이 저장소 `cmd/fake-ecs` | ECS 흉내. moto의 ECS는 태스크를 RUNNING으로 둘 뿐 끝내지 않고 종료 코드도 줄 수 없다. 그래서 "정해진 시각에 0으로 끝남", "태스크 2가 99로 끝남", "제때 RUNNING이 안 됨" 같은 각본을 직접 만든다. 관측 클러스터의 `ListTasks`/`DescribeTasks`에도 답해 fake-mimir의 IP를 돌려준다. SDK는 `AWS_ENDPOINT_URL_ECS`로 여기를 본다 |
| fake-mimir | 이 저장소 `cmd/fake-mimir` | 각본대로 PromQL에 답한다. fake-ecs와 같은 각본 이름을 받아 시각을 맞춘다 |

- `local/forgejo/up.sh`가 관리자 계정과 러너를 등록하고, `mirror.sh`가 작업 트리를 로컬 Forgejo의 main으로 push 한다. 띄우는 법은 [local/README.md](../local/README.md)에 있다.
- 확인한 것(2026-10-19, NV-656)
  - 빈 상태에서 `up.sh` 한 번으로 Forgejo, 러너, 저장소가 준비되고, main push와 PR(`pull_request`) 양쪽에서 `ci.yml`이 성공한다. 다시 실행해도 된다.
  - `pull_request_target`과 태그로 고정한 액션을 넣은 PR은 워크플로 규칙 검사에서 실패한다. actionlint는 이 PR을 통과시키므로 규칙 검사가 따로 필요하다.
  - YAML 이스케이프(`"pull_request_\u0074arget"`)와 흐름 스타일(`- {uses: evil/action@main}`, `- {run: ..., continue-on-error: true}`)로 쓴 PR도 규칙 검사에서 실패한다.
  - `local/.data`가 커밋에 들어 있으면 `mirror.sh`가 올리지 않는다.
  - `github.server_url`은 Forgejo의 `ROOT_URL`(`http://forgejo:3000`)로 채워진다. 7.4의 실행 위치 검사에 쓸 수 있다.
  - 미러에는 `local/.data`와 `CLAUDE.local.md`가 올라가지 않는다(`.gitignore`).
- 로컬 Forgejo에는 비밀을 넣지 않는다. AWS 값은 moto용 가짜 값만 넣는다.
- 끝나면 `docker compose down -v`로 지운다.

### 7.2 단계별 시험

| 단계 | 어디서 | 무엇을 | 운영에 닿는가 |
|---|---|---|---|
| 1. 단위 | `ci.yml`(GitHub, 로컬 Forgejo) | `cmd/plan` 계산(아래 "계획 계산 시험"), 발급기의 클레임 고정과 범위 거절, 컨트롤러 판정(가짜 ECS, 가짜 Mimir로 결과 다섯), k6 스크립트 `k6 inspect` | 아니요 |
| 2. 이미지 | `ci.yml` k6-safety 작업 | 이미지 3개를 `linux/amd64`로 빌드. k6 이미지를 시험용 응답 서버(`services`)에 붙여 돌린다: User-Agent, `responseCallback`, 오류를 내면 99, SIGTERM에 105와 요약 업로드(moto S3) | 아니요 |
| 3. 워크플로 | 로컬 Forgejo 수동 실행 | `loadtest.yml`, `stop.yml`, `cleanup.yml`을 moto, fake-ecs, fake-mimir에 붙여 각본별로 돌린다(아래 표) | 아니요 |
| 4. 흐름 | 로컬 be | k6 흐름 다섯 개를 로컬 be와 시험용 키로 돌린다(A-3, A-4) | 아니요 |
| 5. 운영 | GitHub | 상품 조회 smoke → 전체 smoke(C-2, C-4). 7.3을 여기서 확인한다 | 예, 승인 뒤 |

**3단계 각본**(시험 시간은 모두 2~3분짜리 계획으로 줄여 돌린다)

| 각본 | fake-ecs | fake-mimir | 기대 |
|---|---|---|---|
| 합격 | 모두 끝 시각에 0 | 정상 | 워크플로 성공. 실행 요약에 계획 표, 전체·흐름별 결과 |
| 불합격 | 모두 끝 시각에 0 | 결제 흐름의 0.5초 안 비율 90% | 워크플로 실패, 결과 "불합격", 흐름별 표에 결제가 걸린다 |
| 중단(오류율) | 멈출 때까지 RUNNING | 오류율 1분 넘게 기준 초과 | 모든 k6 태스크가 StopTask 되고 실패, "중단" |
| 중단(태스크 99) | 태스크 2가 도중에 99 | 정상 | 나머지가 StopTask 되고 "중단" |
| 끝 시각 99 | 태스크 2가 끝 시각에 99 | 정상 | 중단이 아니다. Mimir 판정대로 "합격" |
| 판정 불가(끊김) | RUNNING | 태스크 1의 `vus`가 1분 넘게 없음 | 모두 멈추고 실패, "판정 불가" |
| 판정 불가(Mimir) | RUNNING | 1분 넘게 HTTP 503 | 위와 같다. obs IP 다시 찾기를 한 번 한다 |
| 판정 불가(구간 누락) | 모두 끝 시각에 0 | 태스크 0의 지표가 중간 2분 비어 있음(감시 중에는 보이지 않게 1분 미만씩) | "판정 불가", 수치를 내지 않는다 |
| 시작 취소 | 태스크 3이 `START_AT`까지 PROVISIONING | 없음 | 띄운 태스크를 모두 멈추고 실패. Mimir를 조회하지 않는다 |
| 발급기 실패 | 발급기 태스크가 1로 끝남 | 없음 | k6 RunTask가 한 번도 불리지 않고 실패 |
| 관측 값 틀림 | 없음 | 없음 | moto의 `observability`에서 `otlpEndpoint`를 빼거나 `http://`를 붙인다. 컨트롤러가 입력 검사에서 실패하고 fake-ecs에 k6 RunTask가 오지 않는다 |
| 이미지 섞임 | 태스크 1의 `imageDigest`가 다름 | 없음 | 시작 시각 전에 모든 태스크를 멈추고 실패. 결과에 다이제스트 두 개가 적힌다 |
| 수집 주소 덮어쓰기 | 받은 RunTask를 기록 | 정상 | 모든 k6 RunTask의 덮어쓰기에 `K6_OTEL_HTTP_EXPORTER_ENDPOINT=<host:port>`와 태스크마다 다른 `OTEL_RESOURCE_ATTRIBUTES`가 있다. 작업 정의 고정값(전송 방식 등)은 덮어쓰기에 없다. 실행 로그에 버킷 이름, 서브넷, 보안 그룹 ID가 찍히지 않았다 |
| obs 재배포 | 관측 서비스 태스크가 둘(`startedAt` 다름), 도중에 이전 것이 사라짐 | 늦게 시작한 IP에서만 정상 | 늦게 시작한 태스크를 고르고, 판정대로 "합격" |
| 정리 | 데이터 태스크가 0으로 끝남 | 없음 | `cleanup.yml`이 `plan.json`의 상품 ID 두 개(spike 계획)로 데이터 태스크를 띄우고 `cleanup.json`을 쓴다. moto SQS 큐에 메시지를 남겨 두면 기다리다 실패한다 |
| 정리 거절 | 다른 시험 ID의 k6가 RUNNING | 없음 | `tasks.data`가 없을 때, 끝나지 않은 시험 ID 일 때, 다른 시험이 돌 때 데이터 태스크를 띄우지 않고 실패한다 |
| 컨트롤러 사망 | 컨트롤러가 도중에 137 | 정상 | `summary.json`이 없어 "판정 불가". 뒷정리가 남은 k6 태스크를 멈춘다 |
| 수동 중지 | RUNNING | 정상 | 도중에 `stop.yml`을 돌리면 k6만 멈추고 컨트롤러가 "중단" 결과를 쓴다 |
| 취소 | RUNNING | 정상 | 도중에 워크플로를 취소해도 뒷정리(`if: always()`)가 돌아 `ListTasks(startedBy)`가 빈다 |
| stress 한계 | 끝 시각에 0 | 단계 3부터 0.5초 안 비율 미달 | 워크플로 성공, "한계 측정", 마지막 합격 단계 2 |
| 입력 거절 | 없음 | 없음 | 아래 "계획 계산 시험"의 거절 경우마다 `plan` 작업이 실패하고 fake-ecs에 요청이 한 번도 오지 않는다 |

- moto와 fake-ecs는 컨테이너를 띄우지 않으므로 컨트롤러도 뜨지 않는다. 로컬에서는 `run` 작업이 컨트롤러 실행 파일을 작업 컨테이너 안에서 바로 돌린다(`CONTROLLER_INLINE=1`). 컨트롤러 사망 각본은 `cmd/gha`가 각본 시각에 인라인 컨트롤러 프로세스를 SIGKILL로 끝내 흉내 낸다.
- fake-ecs는 실제 ECS와 다르게 동작할 수 있다. RunTask 10대 제한, `startedBy` 거르기, StopTask 뒤 STOPPED까지 걸리는 시간은 실제 동작을 문서로 확인해 넣고, 실제 ECS와 맞는지는 C-4에서 다시 본다.

**4단계 흐름 시험**(로컬 be, 시험용 키쌍. 운영 키는 쓰지 않는다)

로컬 be는 be의 `docker/aot-training` 구성을 바탕으로 띄운다(R-6). 이 구성은 JWT 키를 매번 새로 만들므로, 그 개인키로 로컬 발급기를 돌린다. 시험 회원은 R-1 SQL을 로컬 DB에 넣는다. 이 구성은 `openssl genpkey`로 키를 만들어 운영과 같은 PKCS#8 PEM이므로 변환 없이 발급기에 넣는다.

| 확인 | 방법 | 기대 |
|---|---|---|
| 진입과 접수 | 회원 20명, 회차를 연 시험 상품 | 모두 `ADMITTED` 뒤 접수 202, `entry_to_accept`가 남는다. 서버가 준 `Retry-After`대로 확인했다 |
| 열리기 전 진입 | 회차 `opensAt`을 1분 뒤로 | `SALE_NOT_OPEN`이 오고 실패로 센다. spike의 `open_at` + 1초 근거를 확인한다 |
| 중복 접수 | 같은 회원으로 두 번 | 409 `ACTIVE_PREORDER_EXISTS`가 `error_code` 카운터에 잡힌다 |
| 같은 키로 다시 보냄 | 같은 `Idempotency-Key` | 새 예약이 생기지 않는다 |
| 준비 구간 | 풀 B 10명, 풀 C 5명 | 등록 대기 뒤 모두 `REGISTERED`다(worker, Mock이 돌아야 한다) |
| 결제 시도 | 풀 B | 주문 생성 뒤 결제 시도가 만들어진다. 같은 회원이 되풀이해도 주문은 하나다 |
| 결제 건너뜀 | worker를 멈춰 등록이 안 되게 | 요청을 보내지 않고 `skipped`로 센다 |
| 지난 시험 예약 | 같은 회원에게 다른 상품의 예약을 미리 둔다 | 결제와 취소가 준비 상품의 예약만 고른다 |
| 취소 | 풀 C | 취소 뒤 목록에서 `CANCELED`, 다시 취소하지 않는다 |
| 태그 | 모든 흐름 | `name`이 템플릿 목록 안에만 있다. User-Agent가 `nova-loadtest/<시험 ID>`다 |
| 시험 끝에 줄에 남은 회원 | 입장 속도를 낮춰 끝 시각에 줄이 남게 | `gracefulStop` 안에 접수까지 간다 |

**계획 계산 시험**(`cmd/plan` 단위 시험)

| 경우 | 기대 |
|---|---|
| 시나리오 다섯의 기본값 | 계획 표가 나온다. 흐름별 시작 수, 예상 요청 수, 태스크 수, 회원 수(풀 A, B, C), 최대 VU, 시계열 수, 시간 |
| smoke의 IP당 5분 요청 수 | 2,400 이하 |
| 상한 초과(smoke에 `peak_rps=50`) | 거절 |
| 진입이 120/초를 넘는 배수 | 진입만 120/초에 묶이고 경고가 나온다 |
| 회원 부족(soak 6시간) | 거절 |
| VU 상한 초과(spike 몰림 2만 명) | 거절 |
| 시계열 2만 개 초과 | 거절 |
| `runners`가 계산보다 적음, `max_tasks` 초과 | 거절 |
| spike에서 `open_at`이 없음, 너무 가까움, `prep_product_id`가 없음 | 거절 |
| `product_id`에 대문자 UUID | 거절(서버가 400을 준다) |
| 같은 입력 두 번 | 같은 계획(결정적). 러너와 컨트롤러가 같은 값을 낸다 |

### 7.3 두 곳에서 다른 것

| 항목 | GitHub | Forgejo | 처리 |
|---|---|---|---|
| environment 승인 | 승인자 지정 가능 | 같은 기능을 확인하지 못했다 | 로컬에서는 승인 없이 돈다. "승인이 한 번만 걸린다"는 C-4에서 확인한다 |
| OIDC | `permissions: id-token: write`, `sub`에 `environment:loadtest` | `enable-openid-connect: true`, `sub`는 `ref:` 또는 `pull_request` 형식뿐이고 Forgejo 16부터 저장소가 숫자 ID 일 수 있다 | 로컬은 OIDC를 쓰지 않고 moto용 가짜 자격 증명을 넣는다. 신뢰 조건은 B-2와 C-4에서 확인한다 |
| `permissions`, `continue-on-error` | 동작 | 무시 | `continue-on-error`는 쓰지 않는다. 실패 처리는 `if:`로만 한다 |
| 실행 요약 | `$GITHUB_STEP_SUMMARY` | Forgejo 17 + 러너 13.2부터 같은 변수 지원 | `$GITHUB_STEP_SUMMARY`만 쓴다 |
| 기본 이미지 | ubuntu, 도구가 많다 | Debian + node.js 정도 | `jq`, AWS CLI 같은 기본 도구에 기대지 않는다. Go는 `setup-go`로 깔고, 계산과 AWS 호출은 Go 명령(`cmd/plan`, 컨트롤러)으로 한다 |
| CODEOWNERS, 브랜치 보호, environment | 동작 | 이 저장소 설정과 무관 | A-1a에서 GitHub에 설정한다(B-2 전) |

- 액션은 커밋 해시로 고정한다(`actions/checkout@<sha>`). 두 곳이 같은 코드를 받고, 공개 저장소에서 태그가 바뀌는 위험도 막는다.

### 7.4 운영에 닿지 않게

- 워크플로의 첫 단계가 어디서 도는지 확인한다. `github.server_url`이 `https://github.com`이 아니면 `AWS_ENDPOINT_URL`(moto)과 `AWS_ENDPOINT_URL_ECS`(fake-ecs)가 로컬 주소여야 하고, 아니면 바로 실패한다.
- `CONTROLLER_INLINE=1`은 GitHub이 아닐 때만 받아들인다. GitHub에서 켜져 있으면 바로 실패한다.
- 로컬 러너와 dind에는 `~/.aws`를 넣지 않는다. 컨트롤러와 `k6-runner`도 `AWS_ENDPOINT_URL`을 그대로 따른다.
- 로컬 be로 돌리는 4단계에서 `BASE_URL`이 `api.grandis-nova.link`이면 `k6-runner`가 시작하지 않는다(`LOCAL=1` 일 때).
