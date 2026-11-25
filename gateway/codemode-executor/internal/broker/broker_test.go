package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wso2/api-platform/gateway/codemode-executor/internal/config"
)

type echoIn struct {
	Size int `json:"size,omitempty"`
}

// seen records what the upstream received on its tool calls.
type seen struct {
	mu      sync.Mutex
	auth    []string
	runHdr  []string
	runMeta []any
	trace   []string
}

// upstream is a real MCP server behind a fake gateway: tools/call for "denied" is answered the
// way mcp-acl-list answers a denial (HTTP 400, JSON-RPC error, id echoed unless nullID).
func upstream(t *testing.T, nullID bool) (*httptest.Server, *seen) {
	t.Helper()
	sn := &seen{}
	s := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "echo"}, func(_ context.Context, req *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, map[string]any, error) {
		sn.mu.Lock()
		sn.auth = append(sn.auth, req.Extra.Header.Get("Authorization"))
		sn.runHdr = append(sn.runHdr, req.Extra.Header.Get(RunIDHeader))
		sn.trace = append(sn.trace, req.Extra.Header.Get("Traceparent"))
		sn.runMeta = append(sn.runMeta, req.Params.Meta[RunIDMeta])
		sn.mu.Unlock()
		return nil, map[string]any{"pad": strings.Repeat("x", in.Size)}, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "fails"}, func(context.Context, *mcp.CallToolRequest, echoIn) (*mcp.CallToolResult, any, error) {
		return nil, nil, errors.New("no such sku")
	})
	mcp.AddTool(s, &mcp.Tool{Name: "denied"}, func(context.Context, *mcp.CallToolRequest, echoIn) (*mcp.CallToolResult, any, error) {
		t.Error("denied tool reached the upstream")
		return nil, nil, nil
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	gw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &msg)
		if msg.Method == "tools/call" && msg.Params.Name == "denied" {
			id := string(msg.ID)
			if nullID {
				id = "null"
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"MCP capability not allowed"}}`, id)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return srv, sn
}

func newRun(srv *httptest.Server, hdr http.Header, maxCalls int, maxBytes int64) *Run {
	return NewRun(Options{
		RunID:          "run-1",
		Sources:        []config.Source{{Name: "up", URL: srv.URL + "/mcp"}},
		Headers:        hdr,
		Transport:      http.DefaultTransport,
		Timeout:        5 * time.Second,
		MaxCalls:       maxCalls,
		MaxResultBytes: maxBytes,
	})
}

func TestOutboundHeaders(t *testing.T) {
	in := http.Header{
		"Authorization":   {"Basic should-not-pass"},
		"X-Jwt-Assertion": {"eyJ.jwt"},
		"X-Tenant":        {"acme"},
		"Cookie":          {"session=secret"},
		"Traceparent":     {"00-abc-def-01"},
	}
	cfg := config.Headers{
		Forward: []string{"X-Tenant"},
		Map:     []config.HeaderMap{{From: "X-Jwt-Assertion", To: "Authorization", Prefix: "Bearer "}},
	}
	out := OutboundHeaders(in, cfg, "r-9")
	want := map[string]string{"Authorization": "Bearer eyJ.jwt", "X-Tenant": "acme", "Traceparent": "00-abc-def-01", RunIDHeader: "r-9"}
	for k, v := range want {
		if out.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, out.Get(k), v)
		}
	}
	if out.Get("Cookie") != "" {
		t.Error("headers not listed in config must not be forwarded")
	}
}

func TestCallCarriesIdentityRunIDAndTrace(t *testing.T) {
	srv, sn := upstream(t, false)
	hdr := http.Header{"Authorization": {"Bearer alice"}, "Traceparent": {"00-abc-def-01"}, RunIDHeader: {"run-1"}}
	r := newRun(srv, hdr, 10, 1<<20)
	defer r.Close()
	if _, err := r.Call(context.Background(), "up", "echo", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if sn.auth[0] != "Bearer alice" || sn.runHdr[0] != "run-1" || sn.trace[0] != "00-abc-def-01" || sn.runMeta[0] != "run-1" {
		t.Errorf("upstream saw auth=%q runHdr=%q trace=%q runMeta=%v", sn.auth, sn.runHdr, sn.trace, sn.runMeta)
	}
	if c := r.Calls(); len(c) != 1 || c[0].Outcome != OutcomeOK || c[0].Tool != "up.echo" {
		t.Errorf("calls = %+v", c)
	}
}

func TestGatewayDenialReachesScript(t *testing.T) {
	for _, nullID := range []bool{false, true} {
		t.Run(fmt.Sprintf("nullID=%v", nullID), func(t *testing.T) {
			srv, _ := upstream(t, nullID)
			r := newRun(srv, http.Header{}, 10, 1<<20)
			defer r.Close()
			_, err := r.Call(context.Background(), "up", "denied", map[string]any{})
			if err == nil || !strings.Contains(err.Error(), "MCP capability not allowed") {
				t.Fatalf("err = %v, want the policy's message", err)
			}
			if c := r.Calls(); c[0].Outcome != OutcomeFailed {
				t.Errorf("outcome = %q", c[0].Outcome)
			}
		})
	}
}

func TestToolErrorIsAnError(t *testing.T) {
	srv, _ := upstream(t, false)
	r := newRun(srv, http.Header{}, 10, 1<<20)
	defer r.Close()
	_, err := r.Call(context.Background(), "up", "fails", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "no such sku") || !errors.Is(err, errToolError) {
		t.Fatalf("err = %v", err)
	}
	if c := r.Calls(); c[0].Outcome != OutcomeToolError {
		t.Errorf("outcome = %q", c[0].Outcome)
	}
}

func TestCallLimit(t *testing.T) {
	srv, sn := upstream(t, false)
	r := newRun(srv, http.Header{}, 2, 1<<20)
	defer r.Close()
	for i := 0; i < 3; i++ {
		_, err := r.Call(context.Background(), "up", "echo", map[string]any{})
		if (i < 2) != (err == nil) {
			t.Fatalf("call %d: err = %v", i, err)
		}
	}
	if len(sn.auth) != 2 {
		t.Errorf("upstream got %d calls, want 2", len(sn.auth))
	}
	if c := r.Calls(); c[2].Outcome != OutcomeRefused {
		t.Errorf("third call outcome = %q", c[2].Outcome)
	}
}

func TestResultSizeCap(t *testing.T) {
	srv, _ := upstream(t, false)
	r := newRun(srv, http.Header{}, 10, 4<<10)
	defer r.Close()
	if _, err := r.Call(context.Background(), "up", "echo", map[string]any{"size": 100}); err != nil {
		t.Fatalf("small result: %v", err)
	}
	if _, err := r.Call(context.Background(), "up", "echo", map[string]any{"size": 64 << 10}); err == nil {
		t.Fatal("a result over the cap should fail")
	}
}

func TestUnknownServerAndLazySessions(t *testing.T) {
	srv, _ := upstream(t, false)
	r := newRun(srv, http.Header{}, 10, 1<<20)
	defer r.Close()
	if _, err := r.Call(context.Background(), "nope", "echo", nil); err == nil || !strings.Contains(err.Error(), "unknown server") {
		t.Errorf("err = %v", err)
	}
	if len(r.sessions) != 0 {
		t.Error("no session should open for an unknown server")
	}
	tools, _, err := r.ListTools(context.Background(), "up")
	if err != nil || len(tools) != 3 {
		t.Errorf("ListTools: %d tools, err %v", len(tools), err)
	}
}

func TestCappedBodyAtExactLimit(t *testing.T) {
	c := &capped{rc: io.NopCloser(strings.NewReader("abcd")), left: 4}
	if b, err := io.ReadAll(c); err != nil || string(b) != "abcd" {
		t.Errorf("exact-size body: %q %v", b, err)
	}
	c = &capped{rc: io.NopCloser(strings.NewReader("abcde")), left: 4}
	if _, err := io.ReadAll(c); !errors.Is(err, errTooLarge) {
		t.Errorf("oversize body: %v", err)
	}
}
