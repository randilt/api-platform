# Code Mode spike for the WSO2 AI gateway

Throwaway prototype backing the "Code Mode on the WSO2 AI Gateway" doc. It proves that a Code Mode
executor behind an ordinary `kind: Mcp` proxy works on a stock gateway, with every inner tool call
going back through the gateway's policy chain.

## What is here

| Path | What |
| --- | --- |
| `cmd/executor` | MCP server with `search` and `execute`. Builds TypeScript declarations from `tools/list` of each source (fetched through the gateway), runs model JS in QuickJS-NG on wazero, brokers each inner call as a `tools/call` back through the gateway |
| `cmd/toolserver` | Test MCP servers: `inventory` (`list_items`, `get_stock`, `delete_item`) and `weather` (`get_forecast`) |
| `cmd/client` | Drives the 7 scenarios through the gateway and prints results |
| `cmd/memtest` | Isolated test of the QuickJS memory limit |
| `deploy/*.yaml` | The three `Mcp` proxies (`/inventory` with `mcp-acl-list` denying `delete_item`, `/weather`, `/codemode`) |
| `qjs-memory-cap.patch` | One-line patch to `fastschema/qjs` v0.0.6: cap wasm linear memory with wazero `WithMemoryLimitPages` |

`third_party/qjs` is `fastschema/qjs` v0.0.6 (MIT) with `qjs-memory-cap.patch` applied; `go.mod`
replaces the module with it. Drop the replace line to see the unpatched behaviour (an allocation
loop gets the process OOM-killed). It goes away once the cap is upstream or in a fork (M1).

## Run it

1. Gateway: in a copy of `wso2/api-platform/gateway`, run `./scripts/setup.sh`, then
   `docker compose up -d gateway-controller gateway-runtime` (images `2026.09.24` were used).
2. Build the image from this directory: `docker build -t codemode-spike .` (executor and toolserver
   binaries, empty `/sandbox-root`, non-root user). Build the client with `go build -o bin/ ./cmd/client`.
3. Start `inventory`, `weather` (`/toolserver -name ... -addr :7000`) and `codemode` (`/executor`)
   on the compose network. Executor env:
   `CODEMODE_SOURCES="inventory=http://gateway-runtime:8080/inventory/mcp;weather=http://gateway-runtime:8080/weather/mcp"`.
4. Deploy the proxies: `POST http://localhost:9090/api/management/v1/mcp-proxies` with each YAML
   (basic auth, admin password from setup). Upstream URLs must include `/mcp`.
5. `./bin/client`

## Gotchas found

- `qjs` ignores `MaxExecutionTime`; use a Go context with `CloseOnContextDone`.
- On deadline `qjs` panics; recover around the run and around `Close`, or one script kills the executor.
- `qjs` mounts `CWD` as `/` and exposes `qjs:std` / `qjs:os`; point `CWD` at an empty directory.
- QuickJS `MemoryLimit` alone did not protect the host at 16 MB and above; the wasm page cap does.
- Gateway `2026.09.24` answers an `mcp-acl-list` denial with `"id": null`; the executor restores the id.
- After an executor restart Envoy returned 503 for a few seconds.
