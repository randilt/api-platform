// Package sandbox runs model-written JavaScript in QuickJS-NG on wazero. Every run gets a fresh
// runtime with no network, an empty file system root, a wall-clock limit and a memory limit;
// the only way out is the call function the caller passes in.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/fastschema/qjs"
)

const wasmPage = 64 << 10

// Init caps the WebAssembly memory of every runtime, checks that rootDir is an empty directory
// (QuickJS mounts it as "/" and exposes it through qjs:std and qjs:os) and compiles QuickJS once,
// so the first run does not pay the compile. Call it once, before any Run.
func Init(maxMemoryBytes int64, rootDir string) error {
	if maxMemoryBytes < wasmPage || maxMemoryBytes > 4<<30 {
		return fmt.Errorf("sandbox memory cap %d out of range", maxMemoryBytes)
	}
	if err := checkEmptyDir(rootDir); err != nil {
		return err
	}
	qjs.MemoryLimitPages = uint32(maxMemoryBytes / wasmPage)
	// qjs builds its wazero config once, on the first New, and caches it with that call's
	// CloseOnContextDone. Without it here, no later run could be stopped by its deadline.
	rt, err := qjs.New(qjs.Option{CWD: rootDir, Context: context.Background(), CloseOnContextDone: true, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		return fmt.Errorf("compile QuickJS: %w", err)
	}
	closeQuietly(rt)
	return nil
}

func checkEmptyDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("sandbox root: %w", err)
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.IsDir() {
		return fmt.Errorf("sandbox root %q is not a directory", dir)
	}
	if names, _ := f.Readdirnames(1); len(names) > 0 {
		return fmt.Errorf("sandbox root %q must be empty: scripts can read it", dir)
	}
	return nil
}

// Limits bound one run.
type Limits struct {
	Timeout     time.Duration
	MemoryBytes int // QuickJS heap limit; the wasm cap from Init is the hard backstop
}

// Call performs one tool call on behalf of the script. A non-nil error reaches the script as an
// exception carrying the error's message.
type Call func(ctx context.Context, server, method string, args map[string]any) (any, error)

// ErrorKind says why a run failed.
type ErrorKind string

const (
	KindNone    ErrorKind = ""
	KindScript  ErrorKind = "script"  // the script threw, or did not parse
	KindTimeout ErrorKind = "timeout" // the wall-clock limit stopped it
	KindMemory  ErrorKind = "memory"  // it ran out of memory
	KindSandbox ErrorKind = "sandbox" // the sandbox itself failed
)

// Result is what a run produced. Error is empty when the run succeeded.
type Result struct {
	Value any
	Logs  []string
	Error string
	Kind  ErrorKind
}

// The prelude gives scripts console.log (kept, capped) and the codemode proxy that turns
// codemode.<server>.<method>(args) into a call through the host.
const prelude = `
const __logs = [];
const __log = (...a) => {
  if (__logs.length >= 200) return;
  const line = a.map(x => typeof x === 'string' ? x : JSON.stringify(x)).join(' ');
  __logs.push(line.length > 2000 ? line.slice(0, 2000) + '...' : line);
};
const console = { log: __log, info: __log, warn: __log, error: __log, debug: __log };
globalThis.print = undefined;
const codemode = new Proxy({}, { get: (_, server) => new Proxy({}, { get: (_, method) => async (args) => {
  const r = JSON.parse(await __call(String(server), String(method), JSON.stringify(args ?? {})));
  if (r.error !== undefined) { const e = new Error(r.error); e.tool = String(server) + '.' + String(method); throw e; }
  return r.result;
}})});
`

// Run executes code as the body of an async function and returns its result. It never panics:
// QuickJS panics instead of returning an error when the deadline closes the module, so every
// panic is recovered and reported as a failed run.
func Run(ctx context.Context, rootDir, code string, lim Limits, call Call) (res Result) {
	ctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			res = Result{Logs: res.Logs, Kind: KindSandbox, Error: "run stopped: " + firstLine(fmt.Sprint(p))}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				res.Kind, res.Error = KindTimeout, timeoutMsg(lim.Timeout)
			}
		}
	}()

	rt, err := qjs.New(qjs.Option{
		CWD:                rootDir,
		Context:            ctx,
		CloseOnContextDone: true, // qjs ignores MaxExecutionTime; closing the module is what stops a busy loop
		MemoryLimit:        lim.MemoryBytes,
		Stdout:             io.Discard,
		Stderr:             io.Discard,
	})
	if err != nil {
		return Result{Kind: KindSandbox, Error: "sandbox start: " + err.Error()}
	}
	defer closeQuietly(rt)

	jsctx := rt.Context()
	jsctx.SetAsyncFunc("__call", func(this *qjs.This) {
		reply := map[string]any{}
		args := this.Args()
		if len(args) != 3 {
			reply["error"] = "bad call"
		} else {
			var in map[string]any
			if err := json.Unmarshal([]byte(args[2].String()), &in); err != nil {
				reply["error"] = "tool arguments must be an object"
			} else if out, err := call(ctx, args[0].String(), args[1].String(), in); err != nil {
				reply["error"] = err.Error()
			} else {
				reply["result"] = out
			}
		}
		b, err := json.Marshal(reply)
		if err != nil {
			b = []byte(`{"error":"tool result is not JSON"}`)
		}
		_ = this.Promise().Resolve(this.Context().NewString(string(b)))
	})

	if _, err := jsctx.Eval("prelude.js", qjs.Code(prelude)); err != nil {
		return Result{Kind: KindSandbox, Error: "prelude: " + err.Error()}
	}
	wrapped := "async function __main() {\n" + code + "\n}\n" +
		"let __r, __e;\ntry { __r = await __main(); } catch (e) { __e = String(e && e.stack ? e + '\\n' + e.stack : e); }\n" +
		"JSON.stringify({ result: __r === undefined ? null : __r, error: __e, logs: __logs });"
	val, err := jsctx.Eval("script.js", qjs.Code(wrapped), qjs.FlagAsync())
	if err != nil {
		res = Result{Kind: KindScript, Error: err.Error()}
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			res.Kind, res.Error = KindTimeout, timeoutMsg(lim.Timeout)
		case strings.Contains(err.Error(), "out of memory"):
			res.Kind = KindMemory
		}
		return res
	}
	defer val.Free()
	var out struct {
		Result any      `json:"result"`
		Error  *string  `json:"error"`
		Logs   []string `json:"logs"`
	}
	if err := json.Unmarshal([]byte(val.String()), &out); err != nil {
		return Result{Kind: KindScript, Error: "result is not JSON-serialisable: " + err.Error()}
	}
	res = Result{Value: out.Result, Logs: out.Logs}
	if out.Error != nil {
		res.Error, res.Kind = *out.Error, KindScript
		if strings.Contains(*out.Error, "out of memory") {
			res.Kind = KindMemory
		}
	}
	return res
}

func timeoutMsg(d time.Duration) string {
	return fmt.Sprintf("run stopped: exceeded %s wall-clock limit", d)
}

func closeQuietly(rt *qjs.Runtime) {
	defer func() { _ = recover() }()
	rt.Close()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
