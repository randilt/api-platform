// Package broker sends a run's tool calls back through the gateway as ordinary MCP tools/call
// requests, carrying the caller's identity, the run id and the trace context on every hop.
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wso2/api-platform/gateway/codemode-executor/internal/config"
)

// RunIDHeader carries the run id on every inner request; RunIDMeta carries it in _meta.
const (
	RunIDHeader = "X-Codemode-Run-Id"
	RunIDMeta   = "com.wso2.apip/codemode.runId"
)

// maxListPages bounds tools/list pagination so a misbehaving server cannot loop us forever.
const maxListPages = 50

// OutboundHeaders builds the headers every inner call of a run carries: the forwarded and
// mapped caller headers, the W3C trace context and the run id.
func OutboundHeaders(in http.Header, cfg config.Headers, runID string) http.Header {
	out := http.Header{}
	for _, name := range cfg.Forward {
		if v := in.Get(name); v != "" {
			out.Set(name, v)
		}
	}
	for _, m := range cfg.Map {
		if v := in.Get(m.From); v != "" {
			out.Set(m.To, m.Prefix+v)
		}
	}
	for _, name := range []string{"Traceparent", "Tracestate"} {
		if v := in.Get(name); v != "" {
			out.Set(name, v)
		}
	}
	if runID != "" {
		out.Set(RunIDHeader, runID)
	}
	return out
}

// Outcome of one inner call, as recorded for the run's result and metrics.
const (
	OutcomeOK        = "ok"
	OutcomeToolError = "tool_error" // the tool answered isError
	OutcomeFailed    = "failed"     // denied by the gateway, transport error, bad response
	OutcomeRefused   = "refused"    // refused by the executor before any request
)

// CallRecord is one inner call as the run's result reports it.
type CallRecord struct {
	Tool    string `json:"tool"`
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

// Run holds the MCP sessions of one search or execute call. Sessions open on first use, so a
// run only talks to the sources its script actually calls.
type Run struct {
	id             string
	sources        map[string]string
	client         *http.Client
	maxCalls       int
	maxResultBytes int64

	mu       sync.Mutex
	sessions map[string]*mcp.ClientSession
	calls    []CallRecord
}

// Options configure a Run.
type Options struct {
	RunID          string
	Sources        []config.Source
	Headers        http.Header       // from OutboundHeaders
	Transport      http.RoundTripper // shared across runs, so connections are reused
	Timeout        time.Duration     // per HTTP request
	MaxCalls       int
	MaxResultBytes int64
}

// NewRun prepares a run. Nothing is sent until the first call.
func NewRun(o Options) *Run {
	src := make(map[string]string, len(o.Sources))
	for _, s := range o.Sources {
		src[s.Name] = s.URL
	}
	return &Run{
		id:             o.RunID,
		sources:        src,
		client:         &http.Client{Transport: &hop{base: o.Transport, headers: o.Headers, maxBytes: o.MaxResultBytes}, Timeout: o.Timeout},
		maxCalls:       o.MaxCalls,
		maxResultBytes: o.MaxResultBytes,
		sessions:       map[string]*mcp.ClientSession{},
	}
}

// ID is the run id.
func (r *Run) ID() string { return r.id }

func (r *Run) session(ctx context.Context, source string) (*mcp.ClientSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[source]; ok {
		return s, nil
	}
	u, ok := r.sources[source]
	if !ok {
		return nil, fmt.Errorf("unknown server %q", source)
	}
	cl := mcp.NewClient(&mcp.Implementation{Name: "wso2-codemode-executor", Version: "0.1.0"}, nil)
	s, err := cl.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: u, HTTPClient: r.client, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", source, err)
	}
	r.sessions[source] = s
	return s, nil
}

// ListTools returns every tool the caller may see on source, following pagination, and the
// TTL the server sent (0 when it sent none).
func (r *Run) ListTools(ctx context.Context, source string) ([]*mcp.Tool, time.Duration, error) {
	s, err := r.session(ctx, source)
	if err != nil {
		return nil, 0, err
	}
	var tools []*mcp.Tool
	var ttl time.Duration
	params := &mcp.ListToolsParams{}
	for page := 0; ; page++ {
		if page == maxListPages {
			return nil, 0, fmt.Errorf("tools/list on %s: more than %d pages", source, maxListPages)
		}
		res, err := s.ListTools(ctx, params)
		if err != nil {
			return nil, 0, fmt.Errorf("tools/list on %s: %w", source, err)
		}
		tools = append(tools, res.Tools...)
		if page == 0 && res.TTLMs > 0 {
			ttl = time.Duration(res.TTLMs) * time.Millisecond
		}
		if res.NextCursor == "" {
			return tools, ttl, nil
		}
		params.Cursor = res.NextCursor
	}
}

// Call sends one tools/call. A tool that answers isError, a gateway denial and a transport
// failure all come back as errors with a message the script can read.
func (r *Run) Call(ctx context.Context, source, tool string, args map[string]any) (any, error) {
	rec := CallRecord{Tool: source + "." + tool}
	if !r.reserve() {
		rec.Outcome, rec.Error = OutcomeRefused, fmt.Sprintf("tool call limit of %d reached for this run", r.maxCalls)
		r.record(rec)
		return nil, errors.New(rec.Error)
	}
	out, err := r.call(ctx, source, tool, args)
	switch {
	case err == nil:
		rec.Outcome = OutcomeOK
	case errors.Is(err, errToolError):
		rec.Outcome, rec.Error = OutcomeToolError, err.Error()
	default:
		rec.Outcome, rec.Error = OutcomeFailed, err.Error()
	}
	r.record(rec)
	return out, err
}

var errToolError = errors.New("tool error")

type toolError struct{ msg string }

func (e toolError) Error() string        { return e.msg }
func (e toolError) Is(target error) bool { return target == errToolError }

func (r *Run) call(ctx context.Context, source, tool string, args map[string]any) (any, error) {
	s, err := r.session(ctx, source)
	if err != nil {
		return nil, err
	}
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args, Meta: mcp.Meta{RunIDMeta: r.id}})
	if err != nil {
		return nil, err
	}
	var texts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, t.Text)
		}
	}
	if res.IsError {
		msg := strings.Join(texts, "; ")
		if msg == "" {
			msg = "tool returned an error"
		}
		return nil, toolError{msg}
	}
	if res.StructuredContent != nil {
		return res.StructuredContent, nil
	}
	return strings.Join(texts, "\n"), nil
}

// reserve claims one of the run's tool calls; false once the limit is used up.
func (r *Run) reserve() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c.Outcome != OutcomeRefused {
			n++
		}
	}
	return n < r.maxCalls
}

func (r *Run) record(c CallRecord) {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
}

// Refuse records a call the executor refused before sending it.
func (r *Run) Refuse(source, tool, reason string) error {
	r.record(CallRecord{Tool: source + "." + tool, Outcome: OutcomeRefused, Error: reason})
	return errors.New(reason)
}

// Calls returns the run's inner calls so far.
func (r *Run) Calls() []CallRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]CallRecord(nil), r.calls...)
}

// Close ends every session the run opened.
func (r *Run) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sessions {
		_ = s.Close()
	}
	r.sessions = map[string]*mcp.ClientSession{}
}

// hop sets the run's headers on every request and caps response bodies. The gateway answers a
// policy denial with HTTP 4xx and a JSON-RPC error body; the MCP client SDK treats any 4xx as a
// transport failure and drops the body, so hop turns such a response into a 200 carrying the
// same JSON-RPC error, and the script sees the policy's message instead of "Bad Request".
type hop struct {
	base     http.RoundTripper
	headers  http.Header
	maxBytes int64
}

func (t *hop) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, vs := range t.headers {
		r.Header[k] = append([]string(nil), vs...)
	}
	var reqID json.RawMessage
	if r.GetBody != nil {
		if rb, err := r.GetBody(); err == nil {
			var env struct {
				ID json.RawMessage `json:"id"`
			}
			if b, err := io.ReadAll(io.LimitReader(rb, 1<<20)); err == nil && json.Unmarshal(b, &env) == nil {
				reqID = env.ID
			}
			rb.Close()
		}
	}
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		resp.Body = &capped{rc: resp.Body, left: t.maxBytes}
		return resp, nil
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	var env map[string]json.RawMessage
	if rerr == nil && json.Unmarshal(body, &env) == nil && env["jsonrpc"] != nil && env["error"] != nil {
		resp.StatusCode, resp.Status = http.StatusOK, "200 OK"
		resp.Header.Set("Content-Type", "application/json")
		// Gateway releases up to 2026.09.24 answered a denial with "id": null, which the SDK
		// cannot match to its call; main echoes the id, so this only helps older gateways.
		if (len(env["id"]) == 0 || string(env["id"]) == "null") && reqID != nil {
			env["id"] = reqID
			body, _ = json.Marshal(env)
		}
	}
	resp.Body = io.NopCloser(strings.NewReader(string(body)))
	resp.ContentLength = int64(len(body))
	return resp, nil
}

// errTooLarge reaches the script when a tool result is bigger than max_tool_result_bytes.
var errTooLarge = errors.New("tool result exceeds the size limit")

type capped struct {
	rc   io.ReadCloser
	left int64
}

func (c *capped) Read(p []byte) (int, error) {
	if c.left <= 0 {
		// At the limit: only a body that ends right here is acceptable.
		var one [1]byte
		if n, err := c.rc.Read(one[:]); n == 0 && err != nil {
			return 0, err
		}
		return 0, errTooLarge
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.rc.Read(p)
	c.left -= int64(n)
	return n, err
}

func (c *capped) Close() error { return c.rc.Close() }
