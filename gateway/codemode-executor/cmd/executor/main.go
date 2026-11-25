// executor is the Code Mode spike: an MCP server exposing `search` and `execute`.
// It builds a typed API from the tools/list of each source MCP proxy (fetched through the
// gateway), runs model-written JavaScript in QuickJS-on-wasm, and sends every inner tool call
// back through the gateway as an ordinary tools/call.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fastschema/qjs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------- config ----------

type source struct{ name, url string }

type config struct {
	sources        []source
	forwardHeaders []string      // headers copied from the inbound request onto inner calls
	timeout        time.Duration // wall clock per run
	memoryLimit    int           // bytes
	maxCalls       int
	maxOutput      int // bytes of result returned to the model
	emptyDir       string
}

func loadConfig() config {
	c := config{timeout: 10 * time.Second, memoryLimit: 32 << 20, maxCalls: 50, maxOutput: 16 << 10, emptyDir: "/sandbox-root"}
	for _, kv := range strings.Split(os.Getenv("CODEMODE_SOURCES"), ";") {
		if p := strings.SplitN(strings.TrimSpace(kv), "=", 2); len(p) == 2 {
			c.sources = append(c.sources, source{p[0], p[1]})
		}
	}
	if v := os.Getenv("CODEMODE_FORWARD_HEADERS"); v != "" {
		c.forwardHeaders = strings.Split(v, ",")
	}
	if v := os.Getenv("CODEMODE_EMPTY_DIR"); v != "" {
		c.emptyDir = v
	}
	return c
}

// ---------- inner-call transport: identity + run id travel on every hop ----------

type hopTransport struct {
	base    http.RoundTripper
	headers http.Header
}

func (t *hopTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, vs := range t.headers {
		for _, v := range vs {
			r.Header.Set(k, v)
		}
	}
	var reqID json.RawMessage
	if r.GetBody != nil {
		if rb, e := r.GetBody(); e == nil {
			var env struct {
				ID json.RawMessage `json:"id"`
			}
			if b, e := io.ReadAll(io.LimitReader(rb, 1<<20)); e == nil && json.Unmarshal(b, &env) == nil {
				reqID = env.ID
			}
			rb.Close()
		}
	}
	resp, err := t.base.RoundTrip(r)
	if err != nil || resp.StatusCode < 400 || resp.StatusCode >= 500 {
		return resp, err
	}
	// The gateway answers a policy denial with HTTP 4xx and a JSON-RPC error body. The MCP
	// client SDK treats any 4xx as a transport failure and drops the body, so the script would
	// only see "Bad Request". Surface the JSON-RPC error instead.
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if rerr == nil && strings.Contains(string(body), `"jsonrpc"`) && strings.Contains(string(body), `"error"`) {
		resp.StatusCode, resp.Status = http.StatusOK, "200 OK"
		resp.Header.Set("Content-Type", "application/json")
		// Some gateway builds answer a denial with "id":null; the SDK then cannot match it to
		// the call. Put the request's id back so the script gets the real error message.
		var env map[string]json.RawMessage
		if json.Unmarshal(body, &env) == nil && (len(env["id"]) == 0 || string(env["id"]) == "null") && reqID != nil {
			env["id"] = reqID
			body, _ = json.Marshal(env)
		}
	}
	if os.Getenv("CODEMODE_DEBUG") != "" {
		log.Printf("gateway %d body: %s", resp.StatusCode, body)
	}
	resp.Body = io.NopCloser(strings.NewReader(string(body)))
	resp.ContentLength = int64(len(body))
	return resp, nil
}

type catalogTool struct {
	server, tool, jsName, description string
	destructive                       bool
	inputSchema, outputSchema         map[string]any
}

type run struct {
	id       string
	sessions map[string]*mcp.ClientSession
	tools    []catalogTool
	calls    []map[string]any
	mu       sync.Mutex
}

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// connect opens one MCP client session per source, through the gateway, as the caller.
func connect(ctx context.Context, cfg config, inbound http.Header, runID string) (*run, error) {
	h := http.Header{}
	for _, name := range cfg.forwardHeaders {
		if v := inbound.Get(name); v != "" {
			h.Set(name, v)
		}
	}
	h.Set("X-Codemode-Run-Id", runID)
	hc := &http.Client{Transport: &hopTransport{base: http.DefaultTransport, headers: h}, Timeout: cfg.timeout}
	r := &run{id: runID, sessions: map[string]*mcp.ClientSession{}}
	for _, s := range cfg.sources {
		cl := mcp.NewClient(&mcp.Implementation{Name: "wso2-codemode-executor", Version: "0.1.0"}, nil)
		sess, err := cl.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: s.url, HTTPClient: hc, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
		if err != nil {
			r.close()
			return nil, fmt.Errorf("connect %s: %w", s.name, err)
		}
		r.sessions[s.name] = sess
		res, err := sess.ListTools(ctx, nil)
		if err != nil {
			r.close()
			return nil, fmt.Errorf("tools/list %s: %w", s.name, err)
		}
		for _, t := range res.Tools {
			ct := catalogTool{server: s.name, tool: t.Name, jsName: jsIdent(t.Name), description: t.Description,
				inputSchema: toMap(t.InputSchema), outputSchema: toMap(t.OutputSchema)}
			if t.Annotations != nil && t.Annotations.DestructiveHint != nil && *t.Annotations.DestructiveHint {
				ct.destructive = true
			}
			r.tools = append(r.tools, ct)
		}
	}
	return r, nil
}

func (r *run) close() {
	for _, s := range r.sessions {
		_ = s.Close()
	}
}

func toMap(v any) map[string]any {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// ---------- typed API generation (JSON Schema -> TypeScript declarations) ----------

var reserved = map[string]bool{"delete": true, "new": true, "class": true, "function": true, "default": true, "import": true, "export": true, "return": true}
var nonIdent = regexp.MustCompile(`[^A-Za-z0-9_$]`)

func jsIdent(s string) string {
	s = nonIdent.ReplaceAllString(s, "_")
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		s = "_" + s
	}
	if reserved[s] {
		s += "_"
	}
	return s
}

func tsType(s map[string]any) string {
	if s == nil {
		return "unknown"
	}
	if e, ok := s["enum"].([]any); ok {
		var parts []string
		for _, v := range e {
			b, _ := json.Marshal(v)
			parts = append(parts, string(b))
		}
		return strings.Join(parts, " | ")
	}
	for _, k := range []string{"anyOf", "oneOf"} {
		if alts, ok := s[k].([]any); ok {
			var parts []string
			for _, a := range alts {
				am, _ := a.(map[string]any)
				parts = append(parts, tsType(am))
			}
			return strings.Join(parts, " | ")
		}
	}
	t := s["type"]
	if arr, ok := t.([]any); ok { // e.g. ["array","null"]
		var parts []string
		for _, x := range arr {
			c := map[string]any{}
			for k, v := range s {
				c[k] = v
			}
			c["type"] = x
			parts = append(parts, tsType(c))
		}
		return strings.Join(parts, " | ")
	}
	switch t {
	case "string":
		return "string"
	case "integer", "number":
		return "number"
	case "boolean":
		return "boolean"
	case "null":
		return "null"
	case "array":
		it, _ := s["items"].(map[string]any)
		return "Array<" + tsType(it) + ">"
	case "object", nil:
		props, _ := s["properties"].(map[string]any)
		if len(props) == 0 {
			return "Record<string, unknown>"
		}
		req := map[string]bool{}
		if r, ok := s["required"].([]any); ok {
			for _, x := range r {
				req[fmt.Sprint(x)] = true
			}
		}
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("{ ")
		for _, k := range keys {
			pm, _ := props[k].(map[string]any)
			opt := "?"
			if req[k] {
				opt = ""
			}
			if d, ok := pm["description"].(string); ok && d != "" {
				fmt.Fprintf(&b, "/** %s */ ", d)
			}
			fmt.Fprintf(&b, "%s%s: %s; ", k, opt, tsType(pm))
		}
		b.WriteString("}")
		return b.String()
	}
	return "unknown"
}

func decl(t catalogTool) string {
	out := "unknown"
	if t.outputSchema != nil {
		out = tsType(t.outputSchema)
	}
	note := ""
	if t.destructive {
		note = " (destructive)"
	}
	return fmt.Sprintf("  /** %s%s */\n  %s.%s(args: %s): Promise<%s>;", t.description, note, jsIdent(t.server), t.jsName, tsType(t.inputSchema), out)
}

// ---------- sandbox ----------

const prelude = `
const __logs = [];
const console = { log: (...a) => __logs.push(a.map(x => typeof x === 'string' ? x : JSON.stringify(x)).join(' ')) };
console.error = console.log; console.warn = console.log;
globalThis.print = undefined;
const codemode = new Proxy({}, { get: (_, server) => new Proxy({}, { get: (_, tool) => async (args) => {
  const raw = await __call(String(server), String(tool), JSON.stringify(args ?? {}));
  const r = JSON.parse(raw);
  if (r.error) { const e = new Error(r.error); e.tool = String(server) + '.' + String(tool); throw e; }
  return r.result;
}})});
`

type execResult struct {
	RunID     string           `json:"runId"`
	Result    any              `json:"result,omitempty"`
	Error     string           `json:"error,omitempty"`
	Logs      []string         `json:"logs"`
	ToolCalls []map[string]any `json:"toolCalls"`
	Truncated bool             `json:"truncated,omitempty"`
}

func execute(ctx context.Context, cfg config, r *run, code string) (res execResult) {
	res = execResult{RunID: r.id, Logs: []string{}}
	// qjs panics (instead of returning an error) when wazero closes the module on deadline or
	// when the sandbox is torn down mid-call. Without this recover one runaway script kills the
	// whole executor.
	defer func() {
		if p := recover(); p != nil {
			res.Error = fmt.Sprintf("run stopped: %v", firstLine(fmt.Sprint(p)))
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				res.Error = fmt.Sprintf("run stopped: exceeded %s wall-clock limit", cfg.timeout)
			}
			r.mu.Lock()
			res.ToolCalls = append([]map[string]any{}, r.calls...)
			r.mu.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	start := time.Now()
	rt, err := qjs.New(qjs.Option{
		CWD:                cfg.emptyDir, // qjs mounts CWD as "/" in WASI: it must be an empty dir
		Context:            ctx,
		CloseOnContextDone: true, // the only way to stop a busy loop: qjs ignores MaxExecutionTime
		MemoryLimit:        cfg.memoryLimit,
		Stdout:             io.Discard,
		Stderr:             io.Discard,
	})
	if err != nil {
		res.Error = "sandbox start: " + err.Error()
		return res
	}
	defer func() { defer func() { _ = recover() }(); rt.Close() }()
	log.Printf("run %s: sandbox ready in %s", r.id, time.Since(start))

	known := map[string]catalogTool{}
	for _, t := range r.tools {
		known[jsIdent(t.server)+"."+t.jsName] = t
	}

	jsctx := rt.Context()
	jsctx.SetAsyncFunc("__call", func(this *qjs.This) {
		args := this.Args()
		server, tool, argJSON := args[0].String(), args[1].String(), args[2].String()
		reply := func(v map[string]any) {
			b, _ := json.Marshal(v)
			_ = this.Promise().Resolve(this.Context().NewString(string(b)))
		}
		r.mu.Lock()
		n := len(r.calls)
		r.mu.Unlock()
		if n >= cfg.maxCalls {
			reply(map[string]any{"error": fmt.Sprintf("tool call limit of %d reached for this run", cfg.maxCalls)})
			return
		}
		rec := map[string]any{"tool": server + "." + tool}
		defer func() { r.mu.Lock(); r.calls = append(r.calls, rec); r.mu.Unlock() }()

		ct, ok := known[server+"."+tool]
		sess := r.sessions[server]
		if !ok || sess == nil {
			// Not in this caller's catalog. Still forwarded: the gateway is the authority, the
			// catalog is only a hint. This proves the per-call policy chain is the real control.
			if sess == nil {
				rec["outcome"] = "unknown server"
				reply(map[string]any{"error": "unknown server " + server})
				return
			}
			ct = catalogTool{server: server, tool: tool}
		}
		var a map[string]any
		_ = json.Unmarshal([]byte(argJSON), &a)
		out, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name: ct.tool, Arguments: a,
			Meta: mcp.Meta{"com.wso2.apip/codemode.runId": r.id},
		})
		if err != nil {
			rec["outcome"] = "denied or failed: " + err.Error()
			reply(map[string]any{"error": err.Error()})
			return
		}
		if out.IsError {
			var msg []string
			for _, c := range out.Content {
				if t, ok := c.(*mcp.TextContent); ok {
					msg = append(msg, t.Text)
				}
			}
			rec["outcome"] = "isError"
			reply(map[string]any{"error": strings.Join(msg, "; ")})
			return
		}
		rec["outcome"] = "ok"
		if out.StructuredContent != nil {
			reply(map[string]any{"result": out.StructuredContent})
			return
		}
		var texts []string
		for _, c := range out.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				texts = append(texts, t.Text)
			}
		}
		reply(map[string]any{"result": strings.Join(texts, "\n")})
	})

	if _, err := jsctx.Eval("prelude.js", qjs.Code(prelude)); err != nil {
		res.Error = "prelude: " + err.Error()
		return res
	}
	wrapped := "async function __main() {\n" + code + "\n}\nconst __r = await __main();\nJSON.stringify({ result: __r === undefined ? null : __r, logs: __logs });"
	val, err := jsctx.Eval("model.js", qjs.Code(wrapped), qjs.FlagAsync())
	if err == nil {
		defer val.Free()
		var parsed struct {
			Result any      `json:"result"`
			Logs   []string `json:"logs"`
		}
		if jerr := json.Unmarshal([]byte(val.String()), &parsed); jerr == nil {
			res.Result, res.Logs = parsed.Result, parsed.Logs
		} else {
			res.Result = val.String()
		}
	} else {
		res.Error = err.Error()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			res.Error = fmt.Sprintf("run stopped: exceeded %s wall-clock limit", cfg.timeout)
		}
	}
	r.mu.Lock()
	res.ToolCalls = append([]map[string]any{}, r.calls...)
	r.mu.Unlock()
	return res
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------- MCP surface ----------

type searchIn struct {
	Query string `json:"query" jsonschema:"keywords describing what you want to do, e.g. 'stock level' or 'weather'"`
}
type searchOut struct {
	Declarations string `json:"declarations"`
	Matches      int    `json:"matches"`
}
type executeIn struct {
	Code string `json:"code" jsonschema:"body of an async JavaScript function. Call tools as await codemode.<server>.<tool>(args). Use console.log for notes and 'return' the final value. No network, no imports."`
}

func main() {
	cfg := loadConfig()
	s := mcp.NewServer(&mcp.Implementation{Name: "wso2-codemode", Version: "0.1.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "search",
		Description: "Find tools you can call from `execute`. Returns TypeScript declarations of matching methods on the `codemode` object. Empty query lists everything."},
		func(ctx context.Context, req *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
			r, err := connect(ctx, cfg, req.Extra.Header, newRunID())
			if err != nil {
				return nil, searchOut{}, err
			}
			defer r.close()
			words := strings.Fields(strings.ToLower(in.Query))
			var lines []string
			for _, t := range r.tools {
				hay := strings.ToLower(t.server + " " + t.tool + " " + t.description)
				hit := len(words) == 0
				for _, w := range words {
					if strings.Contains(hay, w) {
						hit = true
					}
				}
				if hit {
					lines = append(lines, decl(t))
				}
			}
			return nil, searchOut{Matches: len(lines), Declarations: "declare const codemode: {\n" + strings.Join(lines, "\n") + "\n};"}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "execute",
		Description: "Run JavaScript that calls tools found with `search`, in a sandbox. Intermediate results stay in the sandbox; only the returned value and console.log lines come back."},
		func(ctx context.Context, req *mcp.CallToolRequest, in executeIn) (*mcp.CallToolResult, execResult, error) {
			runID := newRunID()
			r, err := connect(ctx, cfg, req.Extra.Header, runID)
			if err != nil {
				return nil, execResult{}, err
			}
			defer r.close()
			out := execute(ctx, cfg, r, in.Code)
			b, _ := json.Marshal(out)
			if len(b) > cfg.maxOutput {
				out.Result, out.Truncated = string(b[:cfg.maxOutput]), true
				out.Logs = nil
			}
			log.Printf("run %s finished: %d tool calls, error=%q", runID, len(out.ToolCalls), out.Error)
			return &mcp.CallToolResult{IsError: out.Error != ""}, out, nil
		})

	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.MaxBytesHandler(h, 1<<20))
	srv := &http.Server{Addr: ":7100", Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: cfg.timeout + 20*time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 1 << 20}
	log.Printf("codemode executor on %s/mcp, sources=%v", srv.Addr, cfg.sources)
	log.Fatal(srv.ListenAndServe())
}
