package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wso2/api-platform/samples/codemode-benchmark/internal/world"
)

func testConfig(t *testing.T) Config {
	return Config{
		Claude:  "claude",
		Gateway: "http://localhost:8080/",
		Contexts: map[string]string{"inventory": "/b-inv", "crm": "/b-crm", "billing": "/b-bill",
			"logistics": "/b-logi", "codemode": "/b-cm"},
		Timeout: time.Minute,
		WorkDir: t.TempDir(),
	}
}

func TestMCPConfig(t *testing.T) {
	cfg := testConfig(t)
	b, allowed, err := MCPConfig(cfg, Direct)
	if err != nil {
		t.Fatal(err)
	}
	var conf struct {
		MCPServers map[string]struct{ Type, URL string } `json:"mcpServers"`
	}
	_ = json.Unmarshal(b, &conf)
	if len(conf.MCPServers) != 4 || conf.MCPServers["crm"].URL != "http://localhost:8080/b-crm/mcp" || conf.MCPServers["crm"].Type != "http" {
		t.Errorf("direct config: %s", b)
	}
	if strings.Join(allowed, ",") != "mcp__inventory,mcp__crm,mcp__billing,mcp__logistics" {
		t.Errorf("allowed = %v", allowed)
	}
	b, allowed, _ = MCPConfig(cfg, CodeMode)
	_ = json.Unmarshal(b, &conf)
	if len(allowed) != 1 || allowed[0] != "mcp__codemode" || !strings.Contains(string(b), "/b-cm/mcp") || strings.Contains(string(b), "b-crm") {
		t.Errorf("codemode config: %s %v", b, allowed)
	}
	delete(cfg.Contexts, "codemode")
	if _, _, err := MCPConfig(cfg, CodeMode); err == nil {
		t.Error("a missing context should be an error")
	}
}

func TestArgs(t *testing.T) {
	args := strings.Join(Args(Job{Task: world.Tasks[0], Mode: Direct, Model: "haiku"}, "/tmp/m.json", []string{"mcp__a", "mcp__b"}), "\x00")
	for _, want := range []string{"-p\x00" + world.Tasks[0].Prompt, "--model\x00haiku", "--output-format\x00stream-json\x00--verbose",
		"--mcp-config\x00/tmp/m.json\x00--strict-mcp-config", "--tools\x00\x00", "--allowedTools\x00mcp__a,mcp__b",
		"--setting-sources\x00\x00", "--no-session-persistence", "--system-prompt\x00" + SystemPrompt} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q", strings.ReplaceAll(want, "\x00", " "))
		}
	}
}

// stream is claude -p --output-format stream-json output for one run.
func stream(tools, calls int, servers map[string]string, reply string, isError bool) []byte {
	var b bytes.Buffer
	var names []string
	for i := 0; i < tools; i++ {
		names = append(names, fmt.Sprintf("mcp__s__t%d", i))
	}
	var ms []map[string]string
	for n, s := range servers {
		ms = append(ms, map[string]string{"name": n, "status": s})
	}
	line := func(v any) { j, _ := json.Marshal(v); b.Write(append(j, '\n')) }
	line(map[string]any{"type": "system", "subtype": "init", "tools": names, "mcp_servers": ms})
	for i := 0; i < calls; i++ {
		line(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]any{{"type": "text"}, {"type": "tool_use", "name": "mcp__s__t0"}}}})
		line(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{{"type": "tool_result"}}}})
	}
	line(map[string]any{"type": "result", "subtype": "success", "is_error": isError, "result": reply, "num_turns": calls + 1,
		"duration_ms": 4200, "total_cost_usd": 0.0123,
		"usage": map[string]int{"input_tokens": 100, "cache_creation_input_tokens": 2000, "cache_read_input_tokens": 3000, "output_tokens": 250}})
	return b.Bytes()
}

func TestParse(t *testing.T) {
	r, reply, err := Parse(stream(48, 3, map[string]string{"inventory": "connected"}, "work\nANSWER: 3", false))
	if err != nil {
		t.Fatal(err)
	}
	if r.ToolsLoaded != 48 || r.ToolCalls != 3 || r.Turns != 4 || r.TotalInput() != 5100 || r.OutputTokens != 250 || r.CostUSD != 0.0123 || r.DurationMS != 4200 || reply != "work\nANSWER: 3" {
		t.Errorf("parsed %+v reply %q", r, reply)
	}
	if _, _, err := Parse(stream(0, 0, map[string]string{"crm": "failed"}, "", false)); err == nil || !strings.Contains(err.Error(), "crm is failed") {
		t.Errorf("a server that did not connect must fail the run: %v", err)
	}
	if _, _, err := Parse(stream(4, 0, nil, "Usage limit reached", true)); err == nil || !strings.Contains(err.Error(), "Usage limit reached") {
		t.Errorf("an error result must fail the run: %v", err)
	}
	if _, _, err := Parse([]byte("not json\n")); err == nil {
		t.Error("output with no result event must fail the run")
	}
}

// fake answers every task correctly in codemode and wrongly for T02 in direct mode, and checks
// it was given the MCP config for the right mode.
func fake(t *testing.T, calls *int) Exec {
	return func(_ context.Context, name string, args, env []string) ([]byte, error) {
		*calls++
		var prompt, confPath string
		for i, a := range args {
			switch a {
			case "-p":
				prompt = args[i+1]
			case "--mcp-config":
				confPath = args[i+1]
			}
		}
		conf, err := os.ReadFile(confPath)
		if err != nil {
			t.Fatalf("mcp config not written: %v", err)
		}
		codemode := strings.Contains(string(conf), "/b-cm/mcp")
		for _, tk := range world.Tasks {
			if tk.Prompt == prompt {
				want, _ := tk.Expected(world.New())
				if tk.ID == "T02" && !codemode {
					want = "1"
				}
				tools := 48
				if codemode {
					tools = 2
				}
				return stream(tools, 2, map[string]string{"x": "connected"}, "ANSWER: "+want, false), nil
			}
		}
		return nil, errors.New("unknown prompt")
	}
}

func TestRunGradesWritesAndResumes(t *testing.T) {
	cfg := testConfig(t)
	path := filepath.Join(t.TempDir(), "results.jsonl")
	jobs := Jobs(world.Tasks[:3], []string{Direct, CodeMode}, []string{"haiku"}, 1)
	calls := 0
	var log bytes.Buffer
	if err := Run(context.Background(), cfg, jobs, path, fake(t, &calls), &log); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(path)
	results, err := Read(f)
	f.Close()
	if err != nil || len(results) != 6 || calls != 6 {
		t.Fatalf("results %d, calls %d, err %v", len(results), calls, err)
	}
	for _, r := range results {
		wantOK := !(r.Task == "T02" && r.Mode == Direct)
		if r.OK != wantOK || r.Error != "" || r.Expected == "" {
			t.Errorf("%s %s: ok=%v err=%q expected=%q", r.Task, r.Mode, r.OK, r.Error, r.Expected)
		}
	}
	if !strings.Contains(log.String(), "T02 direct   haiku   #1  wrong") {
		t.Errorf("progress log:\n%s", log.String())
	}
	// A second run finds every job done and starts none.
	if err := Run(context.Background(), cfg, jobs, path, fake(t, &calls), &log); err != nil || calls != 6 {
		t.Errorf("resume ran %d more jobs: %v", calls-6, err)
	}
	// Adding a repeat runs only the new jobs.
	if err := Run(context.Background(), cfg, Jobs(world.Tasks[:3], []string{Direct, CodeMode}, []string{"haiku"}, 2), path, fake(t, &calls), &log); err != nil || calls != 12 {
		t.Errorf("second repeat ran %d jobs: %v", calls-6, err)
	}
}

func TestRunRecordsFailuresWithoutGrading(t *testing.T) {
	cfg := testConfig(t)
	path := filepath.Join(t.TempDir(), "results.jsonl")
	exec := func(context.Context, string, []string, []string) ([]byte, error) {
		return stream(48, 0, map[string]string{"x": "connected"}, "I could not find it.", false), nil
	}
	_ = Run(context.Background(), cfg, Jobs(world.Tasks[:1], []string{Direct}, []string{"haiku"}, 1), path, exec, &bytes.Buffer{})
	exec = func(context.Context, string, []string, []string) ([]byte, error) {
		return nil, errors.New("exit status 1: not logged in")
	}
	_ = Run(context.Background(), cfg, Jobs(world.Tasks[1:2], []string{Direct}, []string{"haiku"}, 1), path, exec, &bytes.Buffer{})
	f, _ := os.Open(path)
	results, _ := Read(f)
	f.Close()
	if len(results) != 2 || results[0].OK || results[0].Error != "" || results[0].Answer != "" {
		t.Errorf("a reply with no ANSWER line is wrong, not an error: %+v", results[0])
	}
	if results[1].Error == "" || results[1].OK {
		t.Errorf("a run that failed must carry its error: %+v", results[1])
	}
}

func TestJobsOrder(t *testing.T) {
	jobs := Jobs(world.Tasks[:2], []string{Direct, CodeMode}, []string{"haiku", "sonnet"}, 2)
	if len(jobs) != 16 || jobs[0].Model != "haiku" || jobs[7].Model != "haiku" || jobs[8].Model != "sonnet" || jobs[1].Mode != CodeMode || jobs[4].Repeat != 2 {
		t.Errorf("jobs not grouped by model, then repeat: %+v", jobs[:9])
	}
}

func TestReport(t *testing.T) {
	r := func(task, mode string, ok bool, in, calls int, errMsg string) Result {
		return Result{Task: task, Mode: mode, Model: "haiku", OK: ok, InputTokens: in, ToolCalls: calls, Turns: calls + 1, ToolsLoaded: map[string]int{Direct: 48, CodeMode: 2}[mode], DurationMS: 2000, CostUSD: 0.01, Error: errMsg}
	}
	out := Report([]Result{
		r("T01", Direct, true, 9000, 4, ""), r("T02", Direct, false, 11000, 6, ""), r("T03", Direct, false, 0, 0, "usage limit"),
		r("T01", CodeMode, true, 3000, 2, ""), r("T02", CodeMode, true, 4000, 2, ""),
	})
	for _, want := range []string{
		"| haiku | direct | 3 | 1/2 | 1 | 11000 | 0 | 7 | 6 | 48 | 2.0 | 0.02 |",
		"| haiku | codemode | 2 | 2/2 | 0 | 4000 | 0 | 3 | 2 | 2 | 2.0 | 0.02 |",
		"| T02 | 0/1 | 1/1 |",
		"| T03 | 0/0 | 0/0 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "| haiku | direct |") > strings.Index(out, "| haiku | codemode |") {
		t.Error("direct should come before codemode")
	}
}
