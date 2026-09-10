# Multi-stage, cross-compiled: the build stage runs on the runner's native arch and Go
# emits the target arch, so linux/arm64 builds need no QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/saves

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 65532 -h /app app
WORKDIR /app
COPY --from=build /out/app /app/app
RUN mkdir -p /app/data && chown -R 65532:65532 /app
# Runs unprivileged: rootless podman maps this uid into the host's subuid range, and 20020
# needs no capability to bind. /app/data holds the allowance's bytes: mount a volume there.
USER 65532:65532
ENTRYPOINT ["/app/app"]
