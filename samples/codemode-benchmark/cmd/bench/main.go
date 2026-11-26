// bench deploys the benchmark's MCP proxies, runs the tasks through Claude Code, and reports.
//
//	bench setup                                   deploy the five Mcp proxies on the gateway
//	bench run -models haiku,sonnet -repeats 1     run the tasks (resumes from results.jsonl)
//	bench report                                  print the results as markdown
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/wso2/api-platform/samples/codemode-benchmark/internal/runner"
	"github.com/wso2/api-platform/samples/codemode-benchmark/internal/world"
)

// contexts are the proxy contexts setup deploys and run calls.
var contexts = map[string]string{
	"inventory": "/bench-inventory", "crm": "/bench-crm", "billing": "/bench-billing",
	"logistics": "/bench-logistics", "codemode": "/bench-codemode",
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: bench setup|run|report [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "setup":
		err = setup(os.Args[2:])
	case "run":
		err = run(os.Args[2:])
	case "report":
		err = report(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

// proxyYAML is one Mcp proxy. The Code Mode proxy points at the executor and names the four
// member proxies in X-Codemode-Sources with set-headers.
func proxyYAML(name, upstream, policies string) string {
	return fmt.Sprintf(`apiVersion: gateway.api-platform.wso2.com/v1
kind: Mcp
metadata:
  name: %s
spec:
  displayName: %s
  version: v1
  context: %s
  specVersion: "2025-06-18"
  upstream:
    url: %s
%s`, "bench-"+name, "Benchmark "+name, contexts[name], upstream, policies)
}

func setup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	api := fs.String("api", "http://localhost:9090/api/management/v1", "gateway management API")
	user := fs.String("user", "admin", "management API user; the password is read from BENCH_ADMIN_PASSWORD")
	mocks := fs.String("mocks", "http://bench-mocks:7200", "benchmark MCP servers, as the gateway reaches them")
	executor := fs.String("executor", "http://codemode-executor:7100/endpoints/default/mcp", "Code Mode executor endpoint, as the gateway reaches it")
	_ = fs.Parse(args)
	password := os.Getenv("BENCH_ADMIN_PASSWORD")
	if password == "" {
		return fmt.Errorf("set BENCH_ADMIN_PASSWORD to the gateway admin password")
	}
	var sources []string
	docs := map[string]string{}
	for _, n := range world.ServerNames {
		docs[n] = proxyYAML(n, *mocks+"/"+n+"/mcp", "")
		sources = append(sources, n+"="+contexts[n]+"/mcp")
	}
	docs["codemode"] = proxyYAML("codemode", *executor, fmt.Sprintf(`  policies:
    - name: set-headers
      version: v1
      params:
        request:
          headers:
            - name: X-Codemode-Sources
              value: "%s"
`, strings.Join(sources, ",")))
	client := &http.Client{Timeout: 30 * time.Second}
	for _, n := range append(append([]string{}, world.ServerNames...), "codemode") {
		status, err := send(client, http.MethodPost, *api+"/mcp-proxies", *user, password, docs[n])
		if err == nil && status == http.StatusConflict {
			status, err = send(client, http.MethodPut, *api+"/mcp-proxies/bench-"+n, *user, password, docs[n])
		}
		if err != nil {
			return fmt.Errorf("deploy %s: %w", n, err)
		}
		if status >= 300 {
			return fmt.Errorf("deploy %s: HTTP %d", n, status)
		}
		fmt.Printf("deployed bench-%s at %s\n", n, contexts[n])
	}
	return nil
}

func send(c *http.Client, method, url, user, password, body string) (int, error) {
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth(user, password)
	req.Header.Set("Content-Type", "application/yaml")
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, nil
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	gateway := fs.String("gateway", "http://localhost:8080", "gateway router")
	claude := fs.String("claude", "claude", "Claude Code executable")
	models := fs.String("models", "haiku,sonnet", "comma-separated models (claude --model values)")
	modes := fs.String("modes", "direct,codemode", "comma-separated modes")
	taskIDs := fs.String("tasks", "", "comma-separated task IDs; empty runs all")
	repeats := fs.Int("repeats", 1, "runs per task, mode and model")
	out := fs.String("out", "results.jsonl", "results file; existing runs are skipped")
	timeout := fs.Duration("timeout", 5*time.Minute, "per run")
	toolSearch := fs.String("tool-search", "false", "ENABLE_TOOL_SEARCH for claude: false loads every tool definition up front, as a client without tool search would")
	_ = fs.Parse(args)

	var tasks []world.Task
	want := map[string]bool{}
	for _, id := range strings.Split(*taskIDs, ",") {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = true
		}
	}
	for _, t := range world.Tasks {
		if len(want) == 0 || want[t.ID] {
			tasks = append(tasks, t)
		}
	}
	if len(tasks) == 0 {
		return fmt.Errorf("no tasks match %q", *taskIDs)
	}
	cfg := runner.Config{Claude: *claude, Gateway: *gateway, Contexts: contexts, Timeout: *timeout,
		Env: []string{"ENABLE_TOOL_SEARCH=" + *toolSearch}, WorkDir: ".bench"}
	jobs := runner.Jobs(tasks, split(*modes), split(*models), *repeats)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fmt.Printf("%d runs, results in %s\n", len(jobs), *out)
	return runner.Run(ctx, cfg, jobs, *out, runner.RealExec, os.Stdout)
}

func report(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	in := fs.String("in", "results.jsonl", "results file")
	_ = fs.Parse(args)
	b, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	results, err := runner.Read(bytes.NewReader(b))
	if err != nil {
		return err
	}
	fmt.Print(runner.Report(results))
	return nil
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
