// mockservers serves the benchmark's four MCP servers at /<name>/mcp on one port.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wso2/api-platform/samples/codemode-benchmark/internal/world"
)

func main() {
	addr := flag.String("addr", ":7200", "listen address")
	flag.Parse()
	mux := http.NewServeMux()
	for name, s := range world.Servers(world.New()) {
		h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
		mux.Handle("/"+name+"/mcp", http.MaxBytesHandler(h, 1<<20))
	}
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 1 << 20}
	log.Printf("benchmark MCP servers on %s: %v", *addr, world.ServerNames)
	log.Fatal(srv.ListenAndServe())
}
