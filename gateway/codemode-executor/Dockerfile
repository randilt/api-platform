# Code Mode spike image: the executor and the test tool servers.
# Build from this directory: docker build -t codemode-spike .
FROM --platform=$BUILDPLATFORM golang:1.26.11-bookworm AS builder
ARG TARGETARCH
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GOWORK=off \
    go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/executor ./cmd/toolserver \
 && mkdir -p /out/sandbox-root /out/tmp && chmod 1777 /out/tmp

FROM scratch
# qjs mounts its CWD as "/" inside the sandbox, so /sandbox-root must stay empty.
COPY --from=builder --chown=65532:65532 /out/sandbox-root /sandbox-root
COPY --from=builder /out/tmp /tmp
COPY --from=builder /out/executor /out/toolserver /
COPY configs/config.toml /etc/codemode/config.toml
EXPOSE 7100 9100
ENV HOME=/tmp
USER 65532:65532
CMD ["/executor", "-config", "/etc/codemode/config.toml"]
