# syntax=docker/dockerfile:1
# 로컬 3단계 각본에 쓰는 가짜 서버(fake-cluster, fake-api). 운영에 쓰지 않는다.
# 저장소 루트에서 빌드한다(local/compose.yml 이 빌드한다).
FROM golang:1.26.8-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY config/ config/
COPY controller/ controller/
RUN CGO_ENABLED=0 go build -trimpath -o /out/ ./controller/cmd/fake-cluster ./controller/cmd/fake-api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /
USER nonroot:nonroot
