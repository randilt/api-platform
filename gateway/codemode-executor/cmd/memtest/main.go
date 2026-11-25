package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/fastschema/qjs"
)

func try(label string, opt qjs.Option, code string) {
	start := time.Now()
	func() {
		defer func() {
			if p := recover(); p != nil {
				fmt.Printf("%s: PANIC after %s: %.120v\n", label, time.Since(start).Round(time.Millisecond), p)
			}
		}()
		rt, err := qjs.New(opt)
		if err != nil {
			fmt.Println(label, "new:", err)
			return
		}
		defer func() { defer func() { recover() }(); rt.Close() }()
		v, err := rt.Context().Eval("t.js", qjs.Code(code))
		if err != nil {
			fmt.Printf("%s: error after %s: %.160v\n", label, time.Since(start).Round(time.Millisecond), err)
			return
		}
		fmt.Printf("%s: ok after %s: %s\n", label, time.Since(start).Round(time.Millisecond), v.String())
	}()
}

func main() {
	code := `const a=[]; for (let i=0;i<200;i++) { a.push(new Array(1<<20).fill(i)); } a.length`
	str := `let s='x'; for (let i=0;i<26;i++) { s = s + s; } s.length`
	base := qjs.Option{CWD: "/tmp/empty", Stdout: io.Discard, Stderr: io.Discard}
	o := base
	o.MemoryLimit = 32 << 20
	try("arrays, 32MB limit, no ctx", o, code)
	try("string doubling, 32MB limit, no ctx", o, str)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	o2 := o
	o2.Context = ctx
	o2.CloseOnContextDone = true
	try("arrays, 32MB limit, ctx+close", o2, code)
}
