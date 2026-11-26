# The benchmark's four MCP servers (mockservers). Build from this directory.
FROM --platform=$BUILDPLATFORM golang:1.26.11-bookworm AS builder
ARG TARGETARCH
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GOWORK=off go build -trimpath -ldflags="-s -w" -o /out/mockservers ./cmd/mockservers

FROM scratch
COPY --from=builder /out/mockservers /mockservers
USER 65532:65532
EXPOSE 7200
CMD ["/mockservers", "-addr", ":7200"]
