package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var root string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "codemode-sandbox-root-")
	if err != nil {
		panic(err)
	}
	root = dir
	if err := Init(64<<20, root); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var lim = Limits{Timeout: 5 * time.Second, MemoryBytes: 32 << 20}

func noCalls(context.Context, string, string, map[string]any) (any, error) {
	return nil, errors.New("no tools in this test")
}

func TestReturnsValueAndLogs(t *testing.T) {
	res := Run(context.Background(), root, `console.log('hi', {a: 1}); return [1, 'two'];`, lim, noCalls)
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if v, ok := res.Value.([]any); !ok || len(v) != 2 || v[1] != "two" {
		t.Errorf("value = %#v", res.Value)
	}
	if len(res.Logs) != 1 || res.Logs[0] != `hi {"a":1}` {
		t.Errorf("logs = %q", res.Logs)
	}
}

func TestToolCallsAndErrors(t *testing.T) {
	var seen []string
	call := func(_ context.Context, server, method string, args map[string]any) (any, error) {
		seen = append(seen, server+"."+method)
		if method == "boom" {
			return nil, errors.New("tool said no")
		}
		return map[string]any{"echo": args["x"]}, nil
	}
	code := `
const r = await codemode.svc.ok({ x: 42 });
try { await codemode.svc.boom({}); } catch (e) { return { r, caught: e.message, tool: e.tool }; }
return 'not reached';`
	res := Run(context.Background(), root, code, lim, call)
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	got, _ := res.Value.(map[string]any)
	if got["caught"] != "tool said no" || got["tool"] != "svc.boom" {
		t.Errorf("value = %#v", res.Value)
	}
	if r, _ := got["r"].(map[string]any); r["echo"] != float64(42) {
		t.Errorf("tool result = %#v", got["r"])
	}
	if strings.Join(seen, ",") != "svc.ok,svc.boom" {
		t.Errorf("calls = %v", seen)
	}
}

func TestNonObjectArgsAreRefused(t *testing.T) {
	called := false
	call := func(context.Context, string, string, map[string]any) (any, error) { called = true; return nil, nil }
	res := Run(context.Background(), root, `try { await codemode.s.t([1,2]); } catch (e) { return e.message; }`, lim, call)
	if res.Value != "tool arguments must be an object" || called {
		t.Errorf("value = %#v, called = %v", res.Value, called)
	}
}

func TestUncaughtThrowIsScriptError(t *testing.T) {
	res := Run(context.Background(), root, `console.log('before'); throw new Error('bad input');`, lim, noCalls)
	if res.Kind != KindScript || !strings.Contains(res.Error, "bad input") {
		t.Errorf("kind %q error %q", res.Kind, res.Error)
	}
	if len(res.Logs) != 1 {
		t.Errorf("logs before the throw should be kept, got %q", res.Logs)
	}
}

func TestSyntaxErrorIsScriptError(t *testing.T) {
	res := Run(context.Background(), root, `return (;`, lim, noCalls)
	if res.Kind != KindScript || res.Error == "" {
		t.Errorf("kind %q error %q", res.Kind, res.Error)
	}
}

func TestBusyLoopStops(t *testing.T) {
	start := time.Now()
	res := Run(context.Background(), root, `while (true) {}`, Limits{Timeout: 200 * time.Millisecond, MemoryBytes: lim.MemoryBytes}, noCalls)
	if res.Kind != KindTimeout {
		t.Errorf("kind %q error %q", res.Kind, res.Error)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %s to stop", d)
	}
}

func TestOutOfMemoryIsAnError(t *testing.T) {
	res := Run(context.Background(), root, `const a = []; while (true) a.push('x'.repeat(1 << 20));`, lim, noCalls)
	if res.Kind != KindMemory {
		t.Errorf("kind %q error %q", res.Kind, res.Error)
	}
}

func TestNoNetworkNoFileSystem(t *testing.T) {
	code := `
const out = { fetch: typeof fetch, xhr: typeof XMLHttpRequest };
const os = await import('qjs:os');
out.root = os.readdir('/')[0].filter(n => n !== '.' && n !== '..');
const std = await import('qjs:std');
out.passwd = std.loadFile('/etc/passwd');
return out;`
	res := Run(context.Background(), root, code, lim, noCalls)
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	got, _ := res.Value.(map[string]any)
	if got["fetch"] != "undefined" || got["xhr"] != "undefined" {
		t.Errorf("network globals present: %#v", got)
	}
	if r, _ := got["root"].([]any); len(r) != 0 {
		t.Errorf("sandbox root not empty: %#v", got["root"])
	}
	if got["passwd"] != nil {
		t.Errorf("/etc/passwd readable: %.60v", got["passwd"])
	}
}

func TestInitRejectsNonEmptyRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Init(64<<20, dir); err == nil || !strings.Contains(err.Error(), "must be empty") {
		t.Errorf("Init on a non-empty root: %v", err)
	}
	if err := Init(64<<20, filepath.Join(dir, "missing")); err == nil {
		t.Error("Init on a missing root should fail")
	}
}

// TestRunawayRunsInARow is the plan's "1,000 runaway runs in a row without a crash": busy loops
// and allocation bombs alternate, and a normal run must still work afterwards.
func TestRunawayRunsInARow(t *testing.T) {
	n := 1000
	if testing.Short() {
		n = 50
	}
	short := Limits{Timeout: 20 * time.Millisecond, MemoryBytes: 8 << 20}
	for i := 0; i < n; i++ {
		code := `while (true) {}`
		if i%2 == 1 {
			code = `const a = []; while (true) a.push(new Array(1 << 16).fill(1));`
		}
		if res := Run(context.Background(), root, code, short, noCalls); res.Error == "" {
			t.Fatalf("run %d: runaway script succeeded", i)
		}
	}
	if res := Run(context.Background(), root, `return 1 + 1;`, lim, noCalls); res.Value != float64(2) {
		t.Fatalf("normal run after runaways: %#v %s", res.Value, res.Error)
	}
}
