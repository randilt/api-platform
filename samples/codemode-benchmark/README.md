# Code Mode benchmark

Measures what Code Mode changes for an agent: the same ten questions about a small company's
data, answered once with the four MCP servers' 48 tools loaded directly, and once through a
single Code Mode proxy that offers only `search` and `execute`. Both paths go through the gateway.
For each run it records whether the answer was right, the tokens the model read and wrote,
turns, tool calls, the tools Claude Code loaded, time and an API-equivalent cost.

The agent is Claude Code in headless mode (`claude -p`), so a Claude subscription is enough; no
API key is needed. Runs count against the plan's usage limits.

## What is here

| Path | What |
| --- | --- |
| `internal/world` | The dataset (20 products, 4 warehouses, 12 customers, 40 orders, invoices, shipments, weather), the four MCP servers (inventory, crm, billing, logistics; 12 tools each) and the ten tasks. Each task's expected answer is computed from the data and must be settled (no tie, no empty result); `TestTasksAreSettled` pins them. |
| `internal/runner` | Runs each job through `claude -p`, parses its stream-json, grades the `ANSWER:` line, appends to `results.jsonl` and renders the report. |
| `cmd/mockservers` | Serves the four servers at `/<name>/mcp` on port 7200. |
| `cmd/bench` | `setup` deploys the five Mcp proxies, `run` runs the jobs, `report` prints markdown. |
| `docker-compose.bench.yaml` | Adds `bench-mocks` to the gateway's compose stack. |

## Run it

1. Start the gateway, the Code Mode executor and the mock servers (from `gateway/`, after
   `./scripts/setup.sh`):

   ```sh
   docker compose -f docker-compose.yaml -f ../samples/codemode-benchmark/docker-compose.bench.yaml \
     --profile codemode up -d gateway-controller gateway-runtime codemode-executor bench-mocks
   ```

2. Deploy the proxies (`/bench-inventory`, `/bench-crm`, `/bench-billing`, `/bench-logistics` and
   the Code Mode proxy `/bench-codemode`, whose `set-headers` policy names the other four):

   ```sh
   cd samples/codemode-benchmark
   BENCH_ADMIN_PASSWORD='<admin password from setup.sh>' go run ./cmd/bench setup
   ```

3. Check one task in both modes before a full run:

   ```sh
   go run ./cmd/bench run -models haiku -tasks T01 -out check.jsonl && go run ./cmd/bench report -in check.jsonl
   ```

   In the report, direct runs should show 48 tools loaded and Code Mode runs 2. If direct shows
   fewer, Claude Code's tool search is hiding tool definitions; the runner sets
   `ENABLE_TOOL_SEARCH=false` (change it with `-tool-search`) to load every definition up front,
   as a client without tool search would.

4. Run and report:

   ```sh
   go run ./cmd/bench run -models haiku,sonnet -repeats 1
   go run ./cmd/bench report > report.md
   ```

`run` appends each result to `results.jsonl` as it finishes and skips jobs already there, so
stop it at any time (Ctrl-C) and start it again later. A run that failed (a usage limit, an MCP
server down, Claude Code not logged in) is recorded with `error` set and left out of the
correct/wrong counts; delete its line to run it again.

### On a Pro plan

One repeat of everything is 40 runs (10 tasks × 2 modes × 2 models). Jobs are ordered model by
model, so when a usage limit stops the run, the first model is complete. Run Haiku first, then
Sonnet in a later usage window: `-models haiku`, then `-models sonnet`. Add repeats the same way
(`-repeats 2` runs only the second repeat when the first is in the file).

## Reading the numbers

- **Input tokens** are everything the model read: fresh input plus cache writes and reads. The
  tool definitions (direct mode) and the intermediate tool results both land here.
- Both modes share Claude Code's own overhead, which `--tools ""`, a fixed system prompt and
  `--setting-sources ""` keep small; compare the modes with each other, not with raw API calls.
- **Cost** is Claude Code's API-equivalent estimate; on a subscription nothing is billed per token.
- A wrong answer and a run error are different things: errors are infrastructure, wrong answers
  are the model's.
