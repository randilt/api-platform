// executor is the Code Mode executor: an MCP server per configured endpoint with search and
// execute tools. Scripts run in a QuickJS sandbox and every tool call they make goes back
// through the gateway as an ordinary MCP tools/call.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wso2/api-platform/gateway/codemode-executor/internal/config"
	"github.com/wso2/api-platform/gateway/codemode-executor/internal/sandbox"
	"github.com/wso2/api-platform/gateway/codemode-executor/internal/server"
)

func main() {
	path := flag.String("config", "/etc/codemode/config.toml", "path to the TOML configuration")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*path, log); err != nil {
		log.Error("executor stopped", "error", err)
		os.Exit(1)
	}
}

func run(path string, log *slog.Logger) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	start := time.Now()
	if err := sandbox.Init(cfg.Sandbox.MaxMemoryBytes, cfg.Sandbox.RootDir); err != nil {
		return err
	}
	log.Info("sandbox ready", "compile_ms", time.Since(start).Milliseconds())

	var longest time.Duration
	for _, ep := range cfg.Endpoints {
		longest = max(longest, ep.Limits.Timeout)
	}
	srv := server.New(cfg, log)
	mcpSrv := &http.Server{Addr: cfg.Server.Address, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 2*longest + 20*time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 1 << 20}
	opsSrv := &http.Server{Addr: cfg.Server.OpsAddress, Handler: srv.OpsHandler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 16}

	errs := make(chan error, 2)
	for _, s := range []*http.Server{mcpSrv, opsSrv} {
		go func(s *http.Server) {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}(s)
	}
	names := make([]string, len(cfg.Endpoints))
	for i, ep := range cfg.Endpoints {
		names[i] = ep.Name
	}
	log.Info("executor listening", "address", cfg.Server.Address, "ops_address", cfg.Server.OpsAddress, "endpoints", names)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	select {
	case err := <-errs:
		return err
	case sig := <-stop:
		log.Info("draining", "signal", sig.String(), "delay", cfg.Server.ShutdownDelay.String())
	}
	srv.Drain()
	time.Sleep(cfg.Server.ShutdownDelay)
	// Let running scripts finish: the longest run plus slack for the reply.
	ctx, cancel := context.WithTimeout(context.Background(), longest+5*time.Second)
	defer cancel()
	err = mcpSrv.Shutdown(ctx)
	_ = opsSrv.Shutdown(ctx)
	return err
}
