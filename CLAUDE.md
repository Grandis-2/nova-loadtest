# nova-loadtest

nova 의 부하 시험 도구를 모아 둔 저장소다. k6 를 ECS Fargate 에 여러 대 띄워 운영과 같은 경로로 부하를 보내고, 결과는 Grafana 에서 본다. 설계와 작업 순서는 [PLAN.md](PLAN.md) 에 있다.

## 관련 저장소

- [Grandis-2/nova-infra](https://github.com/Grandis-2/nova-infra): 인프라. 시험에 쓰는 클러스터, IAM 역할, ECR, S3, WAF 예외, 대시보드는 모두 여기서 PR 로 만든다.
- [Grandis-2/be](https://github.com/Grandis-2/be): 백엔드. 시나리오의 API 경로와 요청 형식은 be 의 main 브랜치를 기준으로 맞춘다.

## 규칙

- 공개 저장소다. 부하 시험 헤더 값, 토큰, 시험 회원 목록, 계정 ID 가 들어간 ARN 은 커밋하지 않는다. 비밀은 Secrets Manager 에 두고 실행할 때 읽는다.
- AWS 리소스는 이 저장소에서 만들지 않는다. 필요하면 nova-infra 에 PR 을 올린다.
- 운영에 부하를 보내는 실행은 담당자 승인을 받은 뒤에 한다. 처음에는 스모크 시험(초당 5건)으로 시작한다.
- 브랜치는 `feat|fix|chore/NV-<번호>-<설명>`, 커밋 제목은 끝에 `(NV-<번호>)` 를 붙인다.
