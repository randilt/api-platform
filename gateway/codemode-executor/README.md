# Code Mode executor

An MCP server that gives an agent two tools, `search` and `execute`, in place of a gateway's whole
tool list. `search` returns TypeScript declarations for the tools the caller may use; `execute`
runs model-written JavaScript in a QuickJS sandbox, and every tool call the script makes goes back
through the gateway as an ordinary MCP `tools/call`, so the gateway's policies (ACLs, authz, rate
limits, guardrails) apply to each one.

It runs as its own component behind an ordinary `Mcp` proxy; no gateway change is needed. The
design and milestones are in the "Code Mode on the WSO2 AI Gateway" proposal.

## Layout

| Path | What |
| --- | --- |
| `cmd/executor` | The executor: loads the config, serves MCP and ops, drains on SIGTERM |
| `internal/config` | TOML config (koanf), defaults and fail-closed validation |
| `internal/typegen` | JSON Schema 2020-12 → TypeScript declarations; unique JS method names |
| `internal/sandbox` | One fresh QuickJS-on-wazero runtime per run: empty root, timeout, memory cap |
| `internal/broker` | MCP sessions to each source per run, identity and run id on every hop |
| `internal/catalog` | `tools/list` cache per source and caller credential |
| `internal/server` | `/endpoints/{name}/mcp` (`search`, `execute`) and the ops handler |
| `configs/config.toml` | The image's default config (one `default` endpoint, sources from the gateway); every setting documented |
| `third_party/qjs` | `fastschema/qjs` v0.0.6 (MIT), vendored with our patches (`qjs-memory-cap.patch`) |
| `cmd/toolserver`, `cmd/client`, `deploy/` | Test MCP servers, the scenario client and the lab's `Mcp` proxies |

## Configure

Each `[[endpoints]]` block is one Code Mode MCP server at `/endpoints/<name>/mcp`. Its sources
are `Mcp` proxy routes on the gateway; scripts call them as `codemode.<source>.<method>(args)`.
Point an `Mcp` proxy's upstream at `http://<executor>:7100/endpoints/<name>/mcp` (the upstream
URL must include the path). See `configs/config.toml` for limits and header mapping.

## Run

Next to the gateway, with its compose file (the service is in the `codemode` profile, so a plain
`docker compose up` leaves it out):

```sh
cd gateway && docker compose --profile codemode up -d
```

The image's built-in config serves one endpoint, `default`, whose sources come from the
`X-Codemode-Sources` header. Deploy the Code Mode proxy like `deploy/codemode.yaml`: upstream
`http://codemode-executor:7100/endpoints/default/mcp`, and a `set-headers` policy naming the member
proxies by context (`inventory=/inventory/mcp,weather=/weather/mcp`). In Kubernetes, set
`gateway.codeModeExecutor.enabled=true` in the gateway Helm chart.

On its own, with your own config:

```sh
make build   # ghcr.io/wso2/api-platform/codemode-executor:<gateway VERSION>
docker run -p 7100:7100 -p 9100:9100 -v $PWD/my-config.toml:/etc/codemode/config.toml \
  ghcr.io/wso2/api-platform/codemode-executor:latest
```

The image is built and pushed with the gateway release (`make -C gateway build-and-push-multiarch`),
and `make -C gateway version-set` moves its tag in the compose files and the Helm chart.

`GET :9100/healthz` and `GET :9100/metrics` (`codemode_runs_total`, `codemode_run_duration_seconds`,
`codemode_inner_calls_total`, `codemode_sandbox_errors_total`, `codemode_searches_total`,
`codemode_runs_in_flight`). Logs are JSON with the run id.

## Test

```sh
go test ./...          # -short runs 50 instead of 1,000 runaway scripts in the sandbox test
go test ./internal/typegen -update   # rewrite the golden declarations
```

## Things to know

- `qjs` caches its wazero config from the first `qjs.New`, including `CloseOnContextDone`;
  `sandbox.Init` sets it, or no deadline would ever stop a busy loop.
- `qjs` panics instead of returning an error when a deadline closes the module; every run and
  close is wrapped in `recover()`.
- QuickJS's own memory limit did not protect the host at 16 MB and above; the wasm page cap does.
- `qjs` mounts the sandbox root as `/` and exposes `qjs:std`/`qjs:os`; the executor refuses to
  start unless the root is an empty directory.
- The gateway answers a policy denial with HTTP 400 and a JSON-RPC error; the MCP Go SDK drops 4xx
  bodies, so the broker turns that response into the JSON-RPC error the script sees.
