# syntax=docker/dockerfile:1
# 컨트롤러. 셸 없는 distroless 에 실행 파일 하나만 둔다.
# 저장소 루트에서 빌드한다: docker buildx build --platform linux/amd64 -f controller/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.26.8-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY config/ config/
COPY controller/ controller/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags='-s -w' -o /out/controller ./controller/cmd/controller

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/controller /controller
USER nonroot:nonroot
ENTRYPOINT ["/controller"]
