# nova-loadtest

nova의 부하 시험 도구다. k6를 ECS Fargate에 여러 대 띄워 운영과 같은 경로로 부하를 보내고, 결과는 Grafana에서 서비스 지표와 함께 본다.

GitHub Actions의 `loadtest` 워크플로를 Actions 탭에서 수동으로 실행한다. 시나리오, 시험 상품, 초당 흐름 시작 수의 최대치, 지속 시간은 실행할 때 입력하고, 필요한 회원 수는 워크플로가 계산한다. 시험 회원의 토큰은 AWS 안에서 발급해 회원마다 다른 요청을 보낸다.

시험에 쓰는 인프라는 [nova-infra](https://github.com/Grandis-2/nova-infra)에서 관리한다. 무엇을 왜 하는지는 [PLAN.md](PLAN.md), 규칙과 근거는 [docs/design.md](docs/design.md), 구성과 작업 단위는 [docs/implementation-plan.md](docs/implementation-plan.md)에 있다. 워크플로를 로컬에서 먼저 돌려 보는 방법은 [local/README.md](local/README.md)에 있다.
