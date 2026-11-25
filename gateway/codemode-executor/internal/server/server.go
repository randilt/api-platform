// Package server exposes each configured endpoint as a stateless MCP server with two tools,
// search and execute, plus an ops handler with /healthz and /metrics.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/wso2/api-platform/gateway/codemode-executor/internal/broker"
	"github.com/wso2/api-platform/gateway/codemode-executor/internal/catalog"
	"github.com/wso2/api-platform/gateway/codemode-executor/internal/config"
	"github.com/wso2/api-platform/gateway/codemode-executor/internal/sandbox"
	"github.com/wso2/api-platform/gateway/codemode-executor/internal/typegen"
)

// Server serves every configured endpoint.
type Server struct {
	cfg       config.Config
	cat       *catalog.Catalog
	transport http.RoundTripper
	slots     chan struct{}
	m         *metrics
	log       *slog.Logger
	servers   map[string]*mcp.Server
	mcp       http.Handler
	reg       *prometheus.Registry
}

// New builds the server. sandbox.Init must have been called.
func New(cfg config.Config, log *slog.Logger) *Server {
	reg := prometheus.NewRegistry()
	s := &Server{
		cfg: cfg,
		cat: catalog.New(cfg.Catalog.TTL, cfg.Catalog.MaxTTL, cfg.Catalog.MaxEntries),
		transport: &http.Transport{
			Proxy:                 nil, // inner calls go to the gateway, never through an environment proxy
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
		slots:   make(chan struct{}, cfg.Server.MaxConcurrentRuns),
		m:       newMetrics(reg),
		log:     log,
		servers: map[string]*mcp.Server{},
		reg:     reg,
	}
	for i := range cfg.Endpoints {
		ep := &cfg.Endpoints[i]
		s.servers[ep.Name] = s.endpointServer(ep)
	}
	s.mcp = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return s.servers[r.PathValue("name")] },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return s
}

// Handler serves /endpoints/{name}/mcp.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/endpoints/{name}/mcp", func(w http.ResponseWriter, r *http.Request) {
		if s.servers[r.PathValue("name")] == nil {
			http.NotFound(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxRequestBytes)
		s.mcp.ServeHTTP(w, r)
	})
	return mux
}

// OpsHandler serves /healthz and /metrics.
func (s *Server) OpsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.reg, promhttp.HandlerOpts{}))
	return mux
}

type searchIn struct {
	Query string `json:"query" jsonschema:"keywords for what you want to do, e.g. 'stock level' or 'forecast'; empty lists every tool"`
}

type searchOut struct {
	Declarations string   `json:"declarations"`
	Matches      int      `json:"matches"`
	Unavailable  []string `json:"unavailable,omitempty"` // sources whose tools could not be listed
}

type executeIn struct {
	Code string `json:"code" jsonschema:"body of an async JavaScript function. Call tools as await codemode.<server>.<method>(args) using the declarations from search. Use console.log for notes and return the final value. No network, no imports, no file system."`
}

type executeOut struct {
	RunID     string              `json:"runId"`
	Result    any                 `json:"result,omitempty"`
	Error     string              `json:"error,omitempty"`
	Logs      []string            `json:"logs"`
	ToolCalls []broker.CallRecord `json:"toolCalls"`
	Truncated bool                `json:"truncated,omitempty"`
}

func (s *Server) endpointServer(ep *config.Endpoint) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "wso2-codemode-" + ep.Name, Version: "0.1.0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "search",
		Description: "Find tools you can call from `execute`. Returns TypeScript declarations of the matching methods on the `codemode` object. An empty query lists every tool."},
		func(ctx context.Context, req *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
			return nil, s.search(ctx, ep, req.Extra.Header, in.Query), nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "execute",
		Description: "Run JavaScript that calls tools found with `search`, in a sandbox. Intermediate results stay in the sandbox; only the returned value and console.log lines come back."},
		func(ctx context.Context, req *mcp.CallToolRequest, in executeIn) (*mcp.CallToolResult, executeOut, error) {
			out := s.execute(ctx, ep, req.Extra.Header, in.Code)
			return &mcp.CallToolResult{IsError: out.Error != ""}, out, nil
		})
	return srv
}

// tools lists every source's tools as this caller sees them, through the cache. Sources that
// fail are named in unavailable; the others are still returned.
func (s *Server) tools(ctx context.Context, ep *config.Endpoint, run *broker.Run, hdr http.Header) ([]typegen.Tool, []string) {
	var (
		mu          sync.Mutex
		wg          sync.WaitGroup
		tools       []typegen.Tool
		unavailable []string
	)
	for _, src := range ep.Sources {
		wg.Add(1)
		go func(src config.Source) {
			defer wg.Done()
			listed, err := s.cat.Get(ctx, catalog.Key(src.URL, hdr), func(ctx context.Context) ([]*mcp.Tool, time.Duration, error) {
				return run.ListTools(ctx, src.Name)
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				s.log.Warn("tools/list failed", "endpoint", ep.Name, "source", src.Name, "run_id", run.ID(), "error", err)
				unavailable = append(unavailable, src.Name)
				return
			}
			for _, t := range listed {
				tools = append(tools, typegen.Tool{Server: src.Name, Name: t.Name, Description: t.Description,
					Destructive: destructive(t), Input: toMap(t.InputSchema), Output: toMap(t.OutputSchema)})
			}
		}(src)
	}
	wg.Wait()
	sort.Strings(unavailable)
	typegen.AssignMethods(tools)
	return tools, unavailable
}

// destructive follows the MCP annotation hints. The spec defaults destructiveHint to true when
// readOnlyHint is false, but almost no server sets either, so treating unannotated tools as
// destructive would refuse nearly everything; only an explicit destructiveHint counts.
func destructive(t *mcp.Tool) bool {
	a := t.Annotations
	return a != nil && !a.ReadOnlyHint && a.DestructiveHint != nil && *a.DestructiveHint
}

func (s *Server) newRun(ep *config.Endpoint, inbound http.Header) (*broker.Run, http.Header) {
	id := newRunID()
	hdr := broker.OutboundHeaders(inbound, ep.Headers, id)
	return broker.NewRun(broker.Options{
		RunID:          id,
		Sources:        ep.Sources,
		Headers:        hdr,
		Transport:      s.transport,
		Timeout:        ep.Limits.Timeout,
		MaxCalls:       ep.Limits.MaxToolCalls,
		MaxResultBytes: ep.Limits.MaxToolResultBytes,
	}), hdr
}

func (s *Server) search(ctx context.Context, ep *config.Endpoint, inbound http.Header, query string) searchOut {
	run, hdr := s.newRun(ep, inbound)
	defer run.Close()
	ctx, cancel := context.WithTimeout(ctx, ep.Limits.Timeout)
	defer cancel()
	tools, unavailable := s.tools(ctx, ep, run, hdr)
	words := strings.Fields(strings.ToLower(query))
	var hits []typegen.Tool
	for _, t := range tools {
		hay := strings.ToLower(t.Server + " " + t.Name + " " + t.Method + " " + t.Description)
		hit := len(words) == 0
		for _, w := range words {
			hit = hit || strings.Contains(hay, w)
		}
		if hit {
			hits = append(hits, t)
		}
	}
	s.m.searches.WithLabelValues(ep.Name).Inc()
	return searchOut{Declarations: typegen.Declarations(hits), Matches: len(hits), Unavailable: unavailable}
}

func (s *Server) execute(ctx context.Context, ep *config.Endpoint, inbound http.Header, code string) (out executeOut) {
	start := time.Now()
	run, hdr := s.newRun(ep, inbound)
	defer run.Close()
	out = executeOut{RunID: run.ID(), Logs: []string{}, ToolCalls: []broker.CallRecord{}}
	outcome := "ok"
	defer func() {
		s.m.runs.WithLabelValues(ep.Name, outcome).Inc()
		s.m.duration.WithLabelValues(ep.Name).Observe(time.Since(start).Seconds())
		for _, c := range out.ToolCalls {
			s.m.innerCalls.WithLabelValues(ep.Name, c.Outcome).Inc()
		}
		s.log.Info("run finished", "endpoint", ep.Name, "run_id", run.ID(), "outcome", outcome,
			"tool_calls", len(out.ToolCalls), "code_bytes", len(code), "duration_ms", time.Since(start).Milliseconds())
	}()

	if len(code) > ep.Limits.MaxCodeBytes {
		outcome, out.Error = "rejected", fmt.Sprintf("code is %d bytes; the limit is %d", len(code), ep.Limits.MaxCodeBytes)
		return out
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		// Refuse at once rather than queue: a queue in front of the slots would only grow.
		outcome, out.Error = "busy", "the executor is at its concurrent run limit; try again shortly"
		return out
	}
	s.m.inFlight.Inc()
	defer s.m.inFlight.Dec()

	ctx, cancel := context.WithTimeout(ctx, ep.Limits.Timeout)
	defer cancel()
	tools, _ := s.tools(ctx, ep, run, hdr)
	byMethod := make(map[string]typegen.Tool, len(tools))
	for _, t := range tools {
		byMethod[t.Server+"."+t.Method] = t
	}
	call := func(ctx context.Context, server, method string, args map[string]any) (any, error) {
		name := method
		if t, ok := byMethod[server+"."+method]; ok {
			name = t.Name
			if t.Destructive && !ep.AllowDestructive {
				return nil, run.Refuse(server, name, fmt.Sprintf("%s.%s is destructive and this endpoint does not allow destructive tools", server, method))
			}
		}
		// A method missing from the caller's catalog is still sent: the gateway's policies on the
		// member route are the authority, and they refuse what the caller may not call.
		return run.Call(ctx, server, name, args)
	}
	res := sandbox.Run(ctx, s.cfg.Sandbox.RootDir, code, sandbox.Limits{Timeout: ep.Limits.Timeout, MemoryBytes: int(ep.Limits.MemoryBytes)}, call)
	out.Result, out.Error, out.Logs, out.ToolCalls = res.Value, res.Error, res.Logs, run.Calls()
	if out.Logs == nil {
		out.Logs = []string{}
	}
	if res.Kind != sandbox.KindNone {
		outcome = string(res.Kind)
		s.m.sandboxErrors.WithLabelValues(ep.Name, string(res.Kind)).Inc()
	}
	limitOutput(&out, ep.Limits.MaxOutputBytes)
	return out
}

// limitOutput keeps the reply under max bytes: logs go first, then the result is cut to a
// prefix of its JSON, marked truncated.
func limitOutput(out *executeOut, max int) {
	if size(out) <= max {
		return
	}
	out.Truncated = true
	out.Logs = []string{}
	if size(out) <= max {
		return
	}
	b, _ := json.Marshal(out.Result)
	out.Result = nil
	budget := max - size(out) - 16
	if budget < 0 {
		budget = 0
	}
	if budget < len(b) {
		b = b[:budget]
	}
	out.Result = string(b)
}

func size(v any) int {
	b, _ := json.Marshal(v)
	return len(b)
}

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func toMap(v any) map[string]any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

type metrics struct {
	runs          *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	innerCalls    *prometheus.CounterVec
	sandboxErrors *prometheus.CounterVec
	searches      *prometheus.CounterVec
	inFlight      prometheus.Gauge
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "codemode_runs_total",
			Help: "execute calls by outcome (ok, script, timeout, memory, sandbox, rejected, busy)."}, []string{"endpoint", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "codemode_run_duration_seconds",
			Help: "execute call duration.", Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}}, []string{"endpoint"}),
		innerCalls: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "codemode_inner_calls_total",
			Help: "Tool calls scripts made, by outcome (ok, tool_error, failed, refused)."}, []string{"endpoint", "outcome"}),
		sandboxErrors: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "codemode_sandbox_errors_total",
			Help: "Runs that ended in a sandbox error, by kind (script, timeout, memory, sandbox)."}, []string{"endpoint", "kind"}),
		searches: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "codemode_searches_total",
			Help: "search calls."}, []string{"endpoint"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{Name: "codemode_runs_in_flight", Help: "execute calls running now."}),
	}
	reg.MustRegister(m.runs, m.duration, m.innerCalls, m.sandboxErrors, m.searches, m.inFlight,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}
