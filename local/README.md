# 로컬 확인

워크플로를 운영에 닿지 않게 먼저 돌려 보는 곳이다. 로컬 Forgejo에 이 저장소를 올리면 GitHub과 같은 워크플로가 Forgejo Actions로 돈다. 왜 이렇게 하는지와 GitHub과 다른 점은 [구현 계획 7절](../docs/implementation-plan.md#7-테스트와-로컬-ci)에 있다.

## 구성

| 컨테이너 | 하는 일 |
|---|---|
| forgejo | 저장소와 Actions. `127.0.0.1:3000`에만 연다 |
| dind | 러너가 쓰는 Docker. 호스트의 Docker 소켓은 넘기지 않는다 |
| runner | 작업 실행. 라벨 `ubuntu-latest`는 `node:24.19.0-bookworm` 이미지다. 이 이미지는 `up.sh`가 호스트에서 받아 dind에 넣는다. `setup-go`가 받은 Go는 dind의 볼륨(`nova-loadtest-toolcache`)에 남아서, 두 번째 실행부터는 다시 받지 않는다 |
| moto | S3, SSM, SQS 흉내. `127.0.0.1:5000` |
| fake-cluster | 가짜 ECS와 가짜 Mimir. 각본 제어는 `127.0.0.1:8081` |
| fake-api | 사전 점검이 부르는 공개 상품 API |

러너는 작업에 `AWS_ENDPOINT_URL`(moto), `AWS_ENDPOINT_URL_ECS`(fake-cluster), 가짜 자격 증명, `CONTROLLER_INLINE=1`을 넘긴다(`forgejo/runner.yml`). 작업은 실제 AWS에 닿지 않는다.

## 띄우기

```sh
local/forgejo/up.sh
```

- Forgejo를 띄우고, 관리자 계정(`loadtest`)과 러너를 등록한 뒤, 지금 작업 트리를 `loadtest/nova-loadtest`의 main으로 올린다. push로 `ci.yml`이 돈다.
- 비밀번호, 러너 등록 토큰, 접근 토큰은 `local/.data/`에 만든다. 커밋하지 않는다(`.gitignore`).
- 결과는 `http://localhost:3000/loadtest/nova-loadtest/actions`에서 본다. 작업 컨테이너가 `http://forgejo:3000`으로 checkout 하기 때문에, 화면 위쪽에 접속 주소가 설정과 다르다는 경고가 나온다. 무시해도 된다.
- 이미 떠 있을 때 다시 실행해도 된다. 계정, 토큰, 러너 등록은 한 번만 만든다.

## 다시 올리기

```sh
local/forgejo/mirror.sh            # main 으로 올린다. push 로 ci.yml 이 돈다
local/forgejo/mirror.sh my-branch  # 브랜치로 올린다
```

- 커밋하지 않은 변경도 임시 커밋으로 만들어 올린다. 이 저장소의 브랜치와 인덱스는 바뀌지 않는다.
- PR로 도는 경로(`pull_request`)를 보려면 브랜치로 올린 뒤 웹 화면에서 main으로 PR을 연다.

## 3단계 각본 돌리기

```sh
go run ./controller/cmd/scenarios              # stress 를 뺀 모든 각본(몇 시간 걸린다)
go run ./controller/cmd/scenarios pass cancel  # 고른 각본만
go run ./controller/cmd/scenarios -long        # stress 한계 각본까지
```

- 각본마다 fake-cluster의 각본을 바꾸고, 워크플로를 dispatch 하고, 끝나면 워크플로 결과, `summary.json`(moto), 가짜 ECS가 받은 요청, 실행 로그를 확인한다. 각본 목록과 기대는 [구현 계획 7.2](../docs/implementation-plan.md#72-단계별-시험)에 있다.
- 각본이 도는 동안 `mirror.sh`를 돌리지 않는다. `ci.yml`이 같이 돌아 러너를 나눠 쓴다.
- 코드를 바꿨으면 `mirror.sh`로 올리고, fake 서버를 바꿨으면 `docker compose -f local/compose.yml up -d --build fake-cluster fake-api`로 다시 띄운다.

## 지우기

```sh
docker compose -f local/compose.yml down -v
rm -rf "${PWD:?}/local/.data"
```
