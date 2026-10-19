# syntax=docker/dockerfile:1
# k6 이미지. grafana/k6 의 k6 실행 파일(정적 빌드) + k6-runner + 스크립트를 셸 없는 distroless 에 둔다.
# 저장소 루트에서 빌드한다: docker buildx build --platform linux/amd64 -f k6/Dockerfile .
FROM grafana/k6:2.3.0 AS k6

FROM --platform=$BUILDPLATFORM golang:1.26.8-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY config/ config/
COPY controller/ controller/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags='-s -w' -o /out/k6-runner ./controller/cmd/k6-runner

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=k6 /usr/bin/k6 /usr/bin/k6
COPY --from=build /out/k6-runner /k6-runner
COPY k6/main.js /k6/main.js
COPY k6/lib/ /k6/lib/
USER nonroot:nonroot
ENTRYPOINT ["/k6-runner"]
