ARG GO_VERSION=1.24.13
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
ENV GOTOOLCHAIN=local

COPY go.mod go.sum ./
RUN go mod download

COPY api ./api
COPY cmd/platform ./cmd/platform
COPY internal ./internal
ARG TARGETOS
ARG TARGETARCH
RUN GOOS="$TARGETOS" GOARCH="$TARGETARCH" CGO_ENABLED=0 \
    go build -mod=readonly -trimpath -o /out/platform ./cmd/platform

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/platform /usr/local/bin/platform
COPY --chmod=0755 deploy/platform-entrypoint.sh /usr/local/bin/platform-entrypoint

ENV PLATFORM_ADDR=:8080 PLATFORM_GRPC_ADDR=127.0.0.1:9090
EXPOSE 8080
USER 65532:65532
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/platform-entrypoint"]
