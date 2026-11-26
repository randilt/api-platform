// Package runner runs each benchmark task through Claude Code in headless mode (claude -p),
// either against the four MCP proxies directly or against the one Code Mode proxy, and records
// what each run cost and whether it answered correctly.
package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wso2/api-platform/samples/codemode-benchmark/internal/world"
)

// Modes.
const (
	Direct   = "direct"   // the agent sees all four servers' tools
	CodeMode = "codemode" // the agent sees only search and execute
)

// SystemPrompt is the same in both modes, so they differ only in their MCP servers.
const SystemPrompt = "You answer questions about a company's operational data. Use only the tools you have and never guess. " +
	"When you are done, put the final answer alone on the last line as `ANSWER: <value>`. " +
	"Write numbers as plain digits without separators or units, and lists as comma-separated IDs."

// Config says where the proxies are and how to run Claude Code.
type Config struct {
	Claude   string            // claude executable
	Gateway  string            // gateway router base URL, e.g. http://localhost:8080
	Contexts map[string]string // proxy context per server name, plus "codemode" for the Code Mode proxy
	Env      []string          // extra environment for claude, e.g. ENABLE_TOOL_SEARCH=false
	Timeout  time.Duration     // per run
	WorkDir  string            // where the MCP config files are written
}

// Job is one run.
type Job struct {
	Task   world.Task
	Mode   string
	Model  string
	Repeat int
}

// Key identifies a job in the results file.
func (j Job) Key() string { return fmt.Sprintf("%s|%s|%s|%d", j.Task.ID, j.Mode, j.Model, j.Repeat) }

// Result is one run as written to the results file.
type Result struct {
	Task                string    `json:"task"`
	Mode                string    `json:"mode"`
	Model               string    `json:"model"`
	Repeat              int       `json:"repeat"`
	OK                  bool      `json:"ok"`
	Answer              string    `json:"answer"`
	Expected            string    `json:"expected"`
	Turns               int       `json:"turns"`
	ToolCalls           int       `json:"toolCalls"`
	ToolsLoaded         int       `json:"toolsLoaded"` // tools Claude Code reported at start: shows whether tool search hid any
	InputTokens         int       `json:"inputTokens"`
	CacheCreationTokens int       `json:"cacheCreationTokens"`
	CacheReadTokens     int       `json:"cacheReadTokens"`
	OutputTokens        int       `json:"outputTokens"`
	CostUSD             float64   `json:"costUsd"`
	DurationMS          int64     `json:"durationMs"`
	Error               string    `json:"error,omitempty"` // the run itself failed: not counted as a wrong answer
	StartedAt           time.Time `json:"startedAt"`
}

// TotalInput is every input token the model read: fresh, cache writes and cache reads.
func (r Result) TotalInput() int { return r.InputTokens + r.CacheCreationTokens + r.CacheReadTokens }

// Exec runs a program and returns its standard output.
type Exec func(ctx context.Context, name string, args, env []string) ([]byte, error)

// RealExec runs the program with the current environment plus env.
func RealExec(ctx context.Context, name string, args, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(lastLine(stderr.String())))
	}
	return out, nil
}

func servers(mode string) []string {
	if mode == CodeMode {
		return []string{"codemode"}
	}
	return world.ServerNames
}

// MCPConfig is the --mcp-config file for a mode and the tool patterns to allow.
func MCPConfig(cfg Config, mode string) ([]byte, []string, error) {
	type server struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	conf := map[string]map[string]server{"mcpServers": {}}
	var allowed []string
	for _, name := range servers(mode) {
		ctx, ok := cfg.Contexts[name]
		if !ok {
			return nil, nil, fmt.Errorf("no proxy context for %q", name)
		}
		conf["mcpServers"][name] = server{Type: "http", URL: strings.TrimSuffix(cfg.Gateway, "/") + ctx + "/mcp"}
		allowed = append(allowed, "mcp__"+name)
	}
	b, err := json.MarshalIndent(conf, "", "  ")
	return b, allowed, err
}

// Args are claude's arguments for a job.
func Args(job Job, mcpConfigPath string, allowed []string) []string {
	return []string{
		"-p", job.Task.Prompt,
		"--model", job.Model,
		"--output-format", "stream-json", "--verbose",
		"--mcp-config", mcpConfigPath, "--strict-mcp-config",
		"--tools", "",
		"--allowedTools", strings.Join(allowed, ","),
		"--system-prompt", SystemPrompt,
		"--setting-sources", "",
		"--no-session-persistence",
	}
}

// Parse reads claude's stream-json output into the measured parts of a result and the reply.
func Parse(out []byte) (Result, string, error) {
	var r Result
	var reply string
	var sawResult bool
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var ev struct {
			Type       string   `json:"type"`
			Subtype    string   `json:"subtype"`
			Tools      []string `json:"tools"`
			MCPServers []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"mcp_servers"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			} `json:"message"`
			IsError    bool    `json:"is_error"`
			Result     string  `json:"result"`
			NumTurns   int     `json:"num_turns"`
			DurationMS int64   `json:"duration_ms"`
			CostUSD    float64 `json:"total_cost_usd"`
			Usage      struct {
				Input         int `json:"input_tokens"`
				CacheCreation int `json:"cache_creation_input_tokens"`
				CacheRead     int `json:"cache_read_input_tokens"`
				Output        int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "system":
			if ev.Subtype != "init" {
				continue
			}
			r.ToolsLoaded = len(ev.Tools)
			for _, s := range ev.MCPServers {
				if s.Status != "connected" {
					return r, "", fmt.Errorf("MCP server %s is %s", s.Name, s.Status)
				}
			}
		case "assistant":
			for _, c := range ev.Message.Content {
				if c.Type == "tool_use" {
					r.ToolCalls++
				}
			}
		case "result":
			sawResult = true
			reply = ev.Result
			r.Turns, r.DurationMS, r.CostUSD = ev.NumTurns, ev.DurationMS, ev.CostUSD
			r.InputTokens, r.CacheCreationTokens = ev.Usage.Input, ev.Usage.CacheCreation
			r.CacheReadTokens, r.OutputTokens = ev.Usage.CacheRead, ev.Usage.Output
			if ev.IsError {
				return r, reply, fmt.Errorf("claude ended with an error (%s): %s", ev.Subtype, lastLine(ev.Result))
			}
		}
	}
	if err := sc.Err(); err != nil {
		return r, "", err
	}
	if !sawResult {
		return r, "", errors.New("claude produced no result event")
	}
	return r, reply, nil
}

// Done reads the keys of the runs already in a results file.
func Done(path string) (map[string]bool, error) {
	done := map[string]bool{}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return done, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	results, err := Read(f)
	for _, r := range results {
		done[fmt.Sprintf("%s|%s|%s|%d", r.Task, r.Mode, r.Model, r.Repeat)] = true
	}
	return done, err
}

// Read parses a results file.
func Read(r io.Reader) ([]Result, error) {
	var out []Result
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var res Result
		if err := json.Unmarshal(sc.Bytes(), &res); err != nil {
			return out, fmt.Errorf("results line %d: %w", len(out)+1, err)
		}
		out = append(out, res)
	}
	return out, sc.Err()
}

// Run runs every job not already in the results file, appending each result as it finishes,
// so an interrupted benchmark resumes where it stopped. A run that fails (Claude Code error,
// usage limit, MCP server down) is recorded with Error set and not retried; delete its line to
// run it again.
func Run(ctx context.Context, cfg Config, jobs []Job, resultsPath string, run Exec, progress io.Writer) error {
	done, err := Done(resultsPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(resultsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	data := world.New()
	for i, job := range jobs {
		if done[job.Key()] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		res := runOne(ctx, cfg, job, data, run)
		b, _ := json.Marshal(res)
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		status := map[bool]string{true: "ok", false: "wrong"}[res.OK]
		if res.Error != "" {
			status = "error: " + res.Error
		}
		fmt.Fprintf(progress, "[%d/%d] %s %-8s %-7s #%d  %s  (in %d, out %d, %d calls)\n",
			i+1, len(jobs), job.Task.ID, job.Mode, job.Model, job.Repeat, status, res.TotalInput(), res.OutputTokens, res.ToolCalls)
	}
	return nil
}

func runOne(ctx context.Context, cfg Config, job Job, data *world.Data, run Exec) Result {
	res := Result{Task: job.Task.ID, Mode: job.Mode, Model: job.Model, Repeat: job.Repeat, StartedAt: time.Now().UTC()}
	want, err := job.Task.Expected(data)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Expected = want
	conf, allowed, err := MCPConfig(cfg, job.Mode)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	path := filepath.Join(cfg.WorkDir, "mcp-"+job.Mode+".json")
	if err := os.WriteFile(path, conf, 0o644); err != nil {
		res.Error = err.Error()
		return res
	}
	rctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	out, runErr := run(rctx, cfg.Claude, Args(job, path, allowed), cfg.Env)
	measured, reply, parseErr := Parse(out)
	measured.Task, measured.Mode, measured.Model, measured.Repeat, measured.StartedAt, measured.Expected =
		res.Task, res.Mode, res.Model, res.Repeat, res.StartedAt, res.Expected
	res = measured
	switch {
	case parseErr != nil:
		res.Error = parseErr.Error()
	case runErr != nil:
		res.Error = runErr.Error()
	default:
		answer, ok := world.ExtractAnswer(reply)
		res.Answer = answer
		res.OK = ok && world.Grade(job.Task.Kind, answer, want)
	}
	return res
}

// Jobs expands tasks × modes × models × repeats, grouped so a usage limit cuts a whole model
// last rather than leaving every model half done.
func Jobs(tasks []world.Task, modes, models []string, repeats int) []Job {
	var jobs []Job
	for _, model := range models {
		for r := 1; r <= repeats; r++ {
			for _, t := range tasks {
				for _, m := range modes {
					jobs = append(jobs, Job{Task: t, Mode: m, Model: model, Repeat: r})
				}
			}
		}
	}
	return jobs
}

// Report renders results as markdown: one row per model and mode, then each task's success.
func Report(results []Result) string {
	type key struct{ model, mode string }
	groups := map[key][]Result{}
	var keys []key
	for _, r := range results {
		k := key{r.Model, r.Mode}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], r)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].model != keys[j].model {
			return keys[i].model < keys[j].model
		}
		return keys[i].mode > keys[j].mode // direct before codemode
	})
	var b strings.Builder
	b.WriteString("| Model | Mode | Runs | Correct | Errors | Median input tokens | Median output tokens | Median turns | Median tool calls | Tools loaded | Median seconds | Cost (USD, API-equivalent) |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, k := range keys {
		rs := groups[k]
		var ok, errs int
		var in, out, turns, calls, loaded []int
		var secs []float64
		var cost float64
		for _, r := range rs {
			if r.Error != "" {
				errs++
				continue
			}
			if r.OK {
				ok++
			}
			in, out, turns, calls, loaded = append(in, r.TotalInput()), append(out, r.OutputTokens), append(turns, r.Turns), append(calls, r.ToolCalls), append(loaded, r.ToolsLoaded)
			secs = append(secs, float64(r.DurationMS)/1000)
			cost += r.CostUSD
		}
		graded := len(rs) - errs
		fmt.Fprintf(&b, "| %s | %s | %d | %d/%d | %d | %d | %d | %d | %d | %d | %.1f | %.2f |\n",
			k.model, k.mode, len(rs), ok, graded, errs, median(in), median(out), median(turns), median(calls), median(loaded), medianF(secs), cost)
	}
	b.WriteString("\n| Task |")
	for _, k := range keys {
		fmt.Fprintf(&b, " %s %s |", k.model, k.mode)
	}
	b.WriteString("\n| --- |" + strings.Repeat(" --- |", len(keys)) + "\n")
	var tasks []string
	seen := map[string]bool{}
	for _, r := range results {
		if !seen[r.Task] {
			seen[r.Task] = true
			tasks = append(tasks, r.Task)
		}
	}
	sort.Strings(tasks)
	for _, t := range tasks {
		fmt.Fprintf(&b, "| %s |", t)
		for _, k := range keys {
			var ok, n int
			for _, r := range groups[k] {
				if r.Task == t && r.Error == "" {
					n++
					if r.OK {
						ok++
					}
				}
			}
			fmt.Fprintf(&b, " %d/%d |", ok, n)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func median(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int(nil), xs...)
	sort.Ints(s)
	return s[len(s)/2]
}

func medianF(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
