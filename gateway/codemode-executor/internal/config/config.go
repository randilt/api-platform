// Package config loads and validates the Code Mode executor's TOML configuration.
package config

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Config is the whole executor configuration.
type Config struct {
	Server    Server     `koanf:"server"`
	Sandbox   Sandbox    `koanf:"sandbox"`
	Catalog   Catalog    `koanf:"catalog"`
	Endpoints []Endpoint `koanf:"endpoints"`
}

// Server holds the listeners and process-wide bounds.
type Server struct {
	Address           string `koanf:"address"`             // MCP listener, serves /endpoints/{name}/mcp
	OpsAddress        string `koanf:"ops_address"`         // /healthz and /metrics
	MaxConcurrentRuns int    `koanf:"max_concurrent_runs"` // execute calls past this are refused at once
	MaxRequestBytes   int64  `koanf:"max_request_bytes"`
}

// Sandbox holds settings shared by every run.
type Sandbox struct {
	RootDir        string `koanf:"root_dir"`         // must be an empty directory
	MaxMemoryBytes int64  `koanf:"max_memory_bytes"` // hard WebAssembly memory cap per run
}

// Catalog holds the tools/list cache settings.
type Catalog struct {
	TTL        time.Duration `koanf:"ttl"`         // used when a server sends no TTL
	MaxTTL     time.Duration `koanf:"max_ttl"`     // ceiling on a server-sent TTL
	MaxEntries int           `koanf:"max_entries"` // cached lists across all callers
}

// Endpoint is one Code Mode MCP server, served at /endpoints/{name}/mcp.
type Endpoint struct {
	Name             string   `koanf:"name"`
	AllowDestructive bool     `koanf:"allow_destructive"`
	Sources          []Source `koanf:"sources"`
	Limits           Limits   `koanf:"limits"`
	Headers          Headers  `koanf:"headers"`
}

// Source is one MCP server the endpoint exposes, reached through its gateway route.
type Source struct {
	Name string `koanf:"name"` // codemode.<name> in scripts
	URL  string `koanf:"url"`
}

// Limits bound one execute call.
type Limits struct {
	Timeout            time.Duration `koanf:"timeout"`
	MemoryBytes        int64         `koanf:"memory_bytes"`
	MaxToolCalls       int           `koanf:"max_tool_calls"`
	MaxCodeBytes       int           `koanf:"max_code_bytes"`
	MaxOutputBytes     int           `koanf:"max_output_bytes"`
	MaxToolResultBytes int64         `koanf:"max_tool_result_bytes"`
}

// Headers says which inbound request headers travel on inner calls.
type Headers struct {
	Forward []string    `koanf:"forward"` // copied as they are
	Map     []HeaderMap `koanf:"map"`     // copied under another name, e.g. a JWT into Authorization
}

// HeaderMap copies inbound header From to outbound header To, with Prefix in front.
type HeaderMap struct {
	From   string `koanf:"from"`
	To     string `koanf:"to"`
	Prefix string `koanf:"prefix"`
}

// Defaults applied to anything the file leaves out.
var (
	DefaultLimits = Limits{
		Timeout:            10 * time.Second,
		MemoryBytes:        32 << 20,
		MaxToolCalls:       50,
		MaxCodeBytes:       64 << 10,
		MaxOutputBytes:     16 << 10,
		MaxToolResultBytes: 1 << 20,
	}
	defaults = Config{
		Server:  Server{Address: ":7100", OpsAddress: ":9100", MaxConcurrentRuns: 16, MaxRequestBytes: 1 << 20},
		Sandbox: Sandbox{RootDir: "/sandbox-root", MaxMemoryBytes: 64 << 20},
		Catalog: Catalog{TTL: 60 * time.Second, MaxTTL: time.Hour, MaxEntries: 1000},
	}
	endpointName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	sourceName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)
	headerName   = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// Load reads a TOML file, fills defaults and validates the result.
func Load(path string) (Config, error) {
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), toml.Parser()); err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	cfg := defaults
	if err := k.Unmarshal("", &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.finish(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// finish fills per-endpoint defaults and refuses anything unsafe or ambiguous.
func (c *Config) finish() error {
	if c.Server.Address == "" || c.Server.OpsAddress == "" {
		return fmt.Errorf("server.address and server.ops_address are required")
	}
	if c.Server.MaxConcurrentRuns < 1 || c.Server.MaxRequestBytes < 1 {
		return fmt.Errorf("server.max_concurrent_runs and server.max_request_bytes must be positive")
	}
	if c.Sandbox.RootDir == "" {
		return fmt.Errorf("sandbox.root_dir is required")
	}
	if c.Catalog.TTL <= 0 || c.Catalog.MaxTTL < c.Catalog.TTL || c.Catalog.MaxEntries < 1 {
		return fmt.Errorf("catalog: need 0 < ttl <= max_ttl and max_entries > 0")
	}
	if len(c.Endpoints) == 0 {
		return fmt.Errorf("at least one [[endpoints]] entry is required")
	}
	seen := map[string]bool{}
	for i := range c.Endpoints {
		e := &c.Endpoints[i]
		if !endpointName.MatchString(e.Name) {
			return fmt.Errorf("endpoint %q: name must match %s", e.Name, endpointName)
		}
		if seen[e.Name] {
			return fmt.Errorf("endpoint %q is defined twice", e.Name)
		}
		seen[e.Name] = true
		if err := e.finish(c.Sandbox.MaxMemoryBytes); err != nil {
			return fmt.Errorf("endpoint %q: %w", e.Name, err)
		}
	}
	return nil
}

func (e *Endpoint) finish(maxMemory int64) error {
	if len(e.Sources) == 0 {
		return fmt.Errorf("needs at least one source")
	}
	names := map[string]bool{}
	for _, s := range e.Sources {
		if !sourceName.MatchString(s.Name) {
			return fmt.Errorf("source %q: name must be a JavaScript identifier (%s)", s.Name, sourceName)
		}
		if names[s.Name] {
			return fmt.Errorf("source %q is listed twice", s.Name)
		}
		names[s.Name] = true
		// Sources are operator-configured gateway routes, usually on a private network, so no
		// private-address denylist applies; scripts can only pick among these names.
		u, err := url.Parse(s.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return fmt.Errorf("source %q: url must be an absolute http(s) URL without credentials", s.Name)
		}
	}
	l, d := &e.Limits, DefaultLimits
	if l.Timeout == 0 {
		l.Timeout = d.Timeout
	}
	if l.MemoryBytes == 0 {
		l.MemoryBytes = min(d.MemoryBytes, maxMemory/2)
	}
	if l.MaxToolCalls == 0 {
		l.MaxToolCalls = d.MaxToolCalls
	}
	if l.MaxCodeBytes == 0 {
		l.MaxCodeBytes = d.MaxCodeBytes
	}
	if l.MaxOutputBytes == 0 {
		l.MaxOutputBytes = d.MaxOutputBytes
	}
	if l.MaxToolResultBytes == 0 {
		l.MaxToolResultBytes = d.MaxToolResultBytes
	}
	if l.Timeout < 0 || l.MemoryBytes < 0 || l.MaxToolCalls < 0 || l.MaxCodeBytes < 0 || l.MaxOutputBytes < 0 || l.MaxToolResultBytes < 0 {
		return fmt.Errorf("limits must not be negative")
	}
	if l.Timeout > 5*time.Minute {
		return fmt.Errorf("limits.timeout %s is above the 5m ceiling", l.Timeout)
	}
	if l.MemoryBytes >= maxMemory {
		return fmt.Errorf("limits.memory_bytes must be below sandbox.max_memory_bytes (%d)", maxMemory)
	}
	for i, h := range e.Headers.Forward {
		if !headerName.MatchString(h) {
			return fmt.Errorf("headers.forward: %q is not a header name", h)
		}
		e.Headers.Forward[i] = http.CanonicalHeaderKey(h)
	}
	for i, m := range e.Headers.Map {
		if !headerName.MatchString(m.From) || !headerName.MatchString(m.To) {
			return fmt.Errorf("headers.map: from and to must be header names")
		}
		e.Headers.Map[i].From, e.Headers.Map[i].To = http.CanonicalHeaderKey(m.From), http.CanonicalHeaderKey(m.To)
	}
	return nil
}
