package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wso2/api-platform/gateway/codemode-executor/internal/config"
	"github.com/wso2/api-platform/gateway/codemode-executor/internal/sandbox"
)

var root string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "codemode-server-root-")
	if err != nil {
		panic(err)
	}
	root = dir
	if err := sandbox.Init(64<<20, root); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type skuIn struct {
	SKU string `json:"sku"`
}
type cityIn struct {
	City string `json:"city"`
}
type item struct {
	SKU       string `json:"sku"`
	Qty       int    `json:"qty"`
	Warehouse string `json:"warehouse"`
}

// lab starts inventory and weather MCP servers behind a fake gateway that denies
// inventory.delete_item the way mcp-acl-list does, and counts tools/list requests.
func lab(t *testing.T) (gwURL string, lists *atomic.Int32, deleted *atomic.Bool) {
	t.Helper()
	lists, deleted = &atomic.Int32{}, &atomic.Bool{}
	inv := mcp.NewServer(&mcp.Implementation{Name: "inventory", Version: "0"}, nil)
	mcp.AddTool(inv, &mcp.Tool{Name: "list_items", Description: "List every inventory item."},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct {
			Items []item `json:"items"`
		}, error) {
			return nil, struct {
				Items []item `json:"items"`
			}{[]item{{"SKU-1", 3, "Jaffna"}, {"SKU-2", 9, "Galle"}, {"SKU-3", 1, "Kandy"}}}, nil
		})
	mcp.AddTool(inv, &mcp.Tool{Name: "get_stock", Description: "Get the stock level for one SKU."},
		func(_ context.Context, _ *mcp.CallToolRequest, in skuIn) (*mcp.CallToolResult, item, error) {
			if in.SKU != "SKU-1" {
				return nil, item{}, fmt.Errorf("unknown sku %s", in.SKU)
			}
			return nil, item{"SKU-1", 3, "Jaffna"}, nil
		})
	yes := true
	mcp.AddTool(inv, &mcp.Tool{Name: "purge", Description: "Delete everything.", Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes}},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			deleted.Store(true)
			return nil, nil, nil
		})
	mcp.AddTool(inv, &mcp.Tool{Name: "delete_item", Description: "Hidden by the gateway."},
		func(context.Context, *mcp.CallToolRequest, skuIn) (*mcp.CallToolResult, any, error) {
			deleted.Store(true)
			return nil, nil, nil
		})
	wea := mcp.NewServer(&mcp.Implementation{Name: "weather", Version: "0"}, nil)
	mcp.AddTool(wea, &mcp.Tool{Name: "get_forecast", Description: "Forecast for a city."},
		func(_ context.Context, _ *mcp.CallToolRequest, in cityIn) (*mcp.CallToolResult, map[string]any, error) {
			return nil, map[string]any{"city": in.City, "condition": map[bool]string{true: "rain", false: "sunny"}[in.City == "Jaffna"]}, nil
		})
	opts := &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}
	invH := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return inv }, opts)
	weaH := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return wea }, opts)

	gw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &msg)
		if msg.Method == "tools/list" {
			lists.Add(1)
		}
		switch r.URL.Path {
		case "/inventory/mcp":
			if msg.Method == "tools/call" && msg.Params.Name == "delete_item" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"MCP capability not allowed"}}`, msg.ID)
				return
			}
			if msg.Method == "tools/list" {
				// Filter the list like mcp-acl-list does: delete_item is not visible.
				rec := httptest.NewRecorder()
				invH.ServeHTTP(rec, r)
				var env map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &env)
				if res, ok := env["result"].(map[string]any); ok {
					var kept []any
					for _, tl := range res["tools"].([]any) {
						if tl.(map[string]any)["name"] != "delete_item" {
							kept = append(kept, tl)
						}
					}
					res["tools"] = kept
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(env)
				return
			}
			invH.ServeHTTP(w, r)
		case "/weather/mcp":
			weaH.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return srv.URL, lists, deleted
}

func cfgFor(gw string, mut func(*config.Config)) config.Config {
	c := config.Config{
		Server:  config.Server{MaxConcurrentRuns: 4, MaxRequestBytes: 1 << 20},
		Sandbox: config.Sandbox{RootDir: root, MaxMemoryBytes: 64 << 20},
		Catalog: config.Catalog{TTL: time.Minute, MaxTTL: time.Hour, MaxEntries: 100},
		Endpoints: []config.Endpoint{{
			Name:    "ops",
			Sources: []config.Source{{Name: "inventory", URL: gw + "/inventory/mcp"}, {Name: "weather", URL: gw + "/weather/mcp"}},
			Limits:  config.DefaultLimits,
		}},
	}
	if mut != nil {
		mut(&c)
	}
	return c
}

func client(t *testing.T, s *Server, endpoint string) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	sess, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/endpoints/" + endpoint + "/mcp", MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func execute(t *testing.T, sess *mcp.ClientSession, code string) executeOut {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "execute", Arguments: map[string]any{"code": code}})
	if err != nil {
		t.Fatal(err)
	}
	var out executeOut
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if res.IsError != (out.Error != "") {
		t.Errorf("isError=%v but error=%q", res.IsError, out.Error)
	}
	return out
}

func search(t *testing.T, sess *mcp.ClientSession, q string) searchOut {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "search", Arguments: map[string]any{"query": q}})
	if err != nil {
		t.Fatal(err)
	}
	var out searchOut
	b, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(b, &out)
	return out
}

func TestSearchFiltersAndCaches(t *testing.T) {
	gw, lists, _ := lab(t)
	sess := client(t, New(cfgFor(gw, nil), quiet()), "ops")
	out := search(t, sess, "stock")
	if out.Matches != 1 || !strings.Contains(out.Declarations, "get_stock(args") {
		t.Errorf("search stock: %+v", out)
	}
	all := search(t, sess, "")
	if strings.Contains(all.Declarations, "delete_item") {
		t.Error("a tool the gateway hides must not appear in search")
	}
	if !strings.Contains(all.Declarations, "purge(args?: {}): Promise<unknown>;") || !strings.Contains(all.Declarations, "(destructive)") {
		t.Errorf("full listing:\n%s", all.Declarations)
	}
	if n := lists.Load(); n != 2 {
		t.Errorf("tools/list sent %d times for two searches, want 2 (one per source, then cached)", n)
	}
}

// Scenario 2: a multi-step task in one execute.
func TestMultiStep(t *testing.T) {
	gw, _, _ := lab(t)
	sess := client(t, New(cfgFor(gw, nil), quiet()), "ops")
	out := execute(t, sess, `
const { items } = await codemode.inventory.list_items({});
const low = items.filter(i => i.qty < 5);
const res = [];
for (const i of low) res.push({ sku: i.sku, rain: (await codemode.weather.get_forecast({ city: i.warehouse })).condition === 'rain' });
console.log('low', low.length);
return res;`)
	if out.Error != "" {
		t.Fatal(out.Error)
	}
	b, _ := json.Marshal(out.Result)
	if string(b) != `[{"rain":true,"sku":"SKU-1"},{"rain":false,"sku":"SKU-3"}]` {
		t.Errorf("result = %s", b)
	}
	if len(out.ToolCalls) != 3 || out.Logs[0] != "low 2" || out.RunID == "" {
		t.Errorf("calls %+v logs %q run %q", out.ToolCalls, out.Logs, out.RunID)
	}
}

// Scenario 3: a tool the gateway hides, called by a computed name, is refused by the gateway.
func TestHiddenToolRefusedByGateway(t *testing.T) {
	gw, _, deleted := lab(t)
	sess := client(t, New(cfgFor(gw, nil), quiet()), "ops")
	out := execute(t, sess, `
const name = ['delete', 'item'].join('_');
try { await codemode.inventory[name]({ sku: 'SKU-1' }); return 'WENT THROUGH'; } catch (e) { return e.message; }`)
	if s, _ := out.Result.(string); !strings.Contains(s, "MCP capability not allowed") {
		t.Errorf("result = %#v", out.Result)
	}
	if deleted.Load() {
		t.Error("delete_item reached the upstream")
	}
}

// Scenario 4: isError becomes a catchable exception.
func TestToolErrorIsCatchable(t *testing.T) {
	gw, _, _ := lab(t)
	sess := client(t, New(cfgFor(gw, nil), quiet()), "ops")
	out := execute(t, sess, `try { await codemode.inventory.get_stock({ sku: 'NOPE' }); } catch (e) { return 'caught: ' + e.message; }`)
	if out.Result != "caught: unknown sku NOPE" || out.ToolCalls[0].Outcome != "tool_error" {
		t.Errorf("result %#v calls %+v", out.Result, out.ToolCalls)
	}
}

func TestDestructiveRefusedUnlessAllowed(t *testing.T) {
	gw, _, deleted := lab(t)
	sess := client(t, New(cfgFor(gw, nil), quiet()), "ops")
	out := execute(t, sess, `try { await codemode.inventory.purge({}); return 'ran'; } catch (e) { return e.message; }`)
	if s, _ := out.Result.(string); !strings.Contains(s, "destructive") || deleted.Load() {
		t.Errorf("result %#v, upstream reached: %v", out.Result, deleted.Load())
	}
	if out.ToolCalls[0].Outcome != "refused" {
		t.Errorf("outcome = %q", out.ToolCalls[0].Outcome)
	}
	allowed := client(t, New(cfgFor(gw, func(c *config.Config) { c.Endpoints[0].AllowDestructive = true }), quiet()), "ops")
	if out := execute(t, allowed, `await codemode.inventory.purge({}); return 'ran';`); out.Result != "ran" || !deleted.Load() {
		t.Errorf("allowed endpoint: %#v %s", out.Result, out.Error)
	}
}

func TestLimits(t *testing.T) {
	gw, _, _ := lab(t)
	cfg := cfgFor(gw, func(c *config.Config) {
		c.Endpoints[0].Limits.MaxCodeBytes = 200
		c.Endpoints[0].Limits.MaxToolCalls = 2
		c.Endpoints[0].Limits.MaxOutputBytes = 300
		c.Endpoints[0].Limits.Timeout = 300 * time.Millisecond
	})
	sess := client(t, New(cfg, quiet()), "ops")
	t.Run("code size", func(t *testing.T) {
		out := execute(t, sess, "return 1;"+strings.Repeat(" ", 300))
		if !strings.Contains(out.Error, "limit is 200") {
			t.Errorf("error = %q", out.Error)
		}
	})
	t.Run("tool calls", func(t *testing.T) {
		out := execute(t, sess, `for (let i = 0; i < 3; i++) { try { await codemode.inventory.get_stock({sku:'SKU-1'}); } catch (e) { return e.message; } } return 'no limit';`)
		if s, _ := out.Result.(string); !strings.Contains(s, "limit of 2") {
			t.Errorf("result = %#v", out.Result)
		}
	})
	t.Run("output", func(t *testing.T) {
		out := execute(t, sess, `console.log('x'.repeat(100)); return 'y'.repeat(1000);`)
		b, _ := json.Marshal(out)
		if !out.Truncated || len(b) > 300 || len(out.Logs) != 0 {
			t.Errorf("truncated=%v size=%d logs=%d", out.Truncated, len(b), len(out.Logs))
		}
	})
	t.Run("timeout", func(t *testing.T) {
		if out := execute(t, sess, `while (true) {}`); !strings.Contains(out.Error, "wall-clock") {
			t.Errorf("error = %q", out.Error)
		}
	})
}

func TestBusyIsRefusedAtOnce(t *testing.T) {
	gw, _, _ := lab(t)
	s := New(cfgFor(gw, func(c *config.Config) { c.Server.MaxConcurrentRuns = 1 }), quiet())
	s.slots <- struct{}{} // the only slot is taken
	sess := client(t, s, "ops")
	start := time.Now()
	out := execute(t, sess, `return 1;`)
	if !strings.Contains(out.Error, "concurrent run limit") || time.Since(start) > 2*time.Second {
		t.Errorf("error %q after %s", out.Error, time.Since(start))
	}
}

func TestUnknownEndpointIs404(t *testing.T) {
	s := New(cfgFor("http://127.0.0.1:1", nil), quiet())
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/endpoints/nope/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestSourceDownIsReportedNotFatal(t *testing.T) {
	gw, _, _ := lab(t)
	sess := client(t, New(cfgFor(gw, func(c *config.Config) {
		c.Endpoints[0].Sources = append(c.Endpoints[0].Sources, config.Source{Name: "down", URL: "http://127.0.0.1:1/mcp"})
	}), quiet()), "ops")
	out := search(t, sess, "")
	if len(out.Unavailable) != 1 || out.Unavailable[0] != "down" || out.Matches == 0 {
		t.Errorf("search with one source down: %+v", out)
	}
}

func TestOpsEndpoints(t *testing.T) {
	gw, _, _ := lab(t)
	s := New(cfgFor(gw, nil), quiet())
	execute(t, client(t, s, "ops"), `await codemode.inventory.get_stock({sku:'SKU-1'}); return 1;`)
	ops := httptest.NewServer(s.OpsHandler())
	defer ops.Close()
	for path, want := range map[string]string{
		"/healthz": "ok",
		"/metrics": `codemode_runs_total{endpoint="ops",outcome="ok"} 1`,
	} {
		resp, err := http.Get(ops.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), want) {
			t.Errorf("%s: %d, missing %q", path, resp.StatusCode, want)
		}
	}
	if out := execute(t, client(t, s, "ops"), `return 1;`); out.ToolCalls == nil {
		t.Error("toolCalls must be [] when the script made no calls")
	}
	if !strings.Contains(metricsText(t, ops.URL), `codemode_inner_calls_total{endpoint="ops",outcome="ok"} 1`) {
		t.Error("inner call not counted")
	}
}

func metricsText(t *testing.T, base string) string {
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return string(b)
}
