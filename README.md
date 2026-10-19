# nova-loadtest

nova 의 부하 시험 도구다. k6 를 ECS Fargate 에 여러 대 띄워 운영과 같은 경로로 부하를 보내고, 결과는 Grafana 에서 서비스 지표와 함께 본다.

GitHub Actions 의 `loadtest` 워크플로를 Actions 탭에서 수동으로 실행한다. 시나리오, 시험 상품, 최대 초당 요청, 지속 시간은 실행할 때 입력하고, 필요한 회원 수는 워크플로가 계산한다. 시험 회원의 토큰은 AWS 안에서 발급해 회원마다 다른 요청을 보낸다.

시험에 쓰는 인프라는 [nova-infra](https://github.com/Grandis-2/nova-infra) 에서 관리한다. 설계는 [PLAN.md](PLAN.md), 구성과 작업 단위는 [docs/implementation-plan.md](docs/implementation-plan.md) 에 있다.
