# syntax=docker/dockerfile:1
# 토큰 발급기. 셸이 없는 distroless 에 실행 파일 하나만 둔다. RunTask 로 명령을 바꿔도 서명키를 꺼낼 방법이 없게 하려는 것이다.
# 저장소 루트에서 빌드한다: docker buildx build --platform linux/amd64 -f token-minter/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.26.8-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# 시험 회원 범위(config/scenarios.yml)는 이때 실행 파일에 들어간다
COPY config/ config/
COPY token-minter/ token-minter/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags='-s -w' -o /out/token-minter ./token-minter/cmd/token-minter

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/token-minter /token-minter
USER nonroot:nonroot
ENTRYPOINT ["/token-minter"]
