package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, toml string) (Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

const minimal = `
[[endpoints]]
name = "ops"
[[endpoints.sources]]
name = "inventory"
url = "http://gateway-runtime:8080/inventory/mcp"
`

func TestDefaults(t *testing.T) {
	cfg, err := load(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Address != ":7100" || cfg.Server.MaxConcurrentRuns != 16 || cfg.Catalog.TTL != time.Minute {
		t.Errorf("server/catalog defaults not applied: %+v %+v", cfg.Server, cfg.Catalog)
	}
	l := cfg.Endpoints[0].Limits
	if l.Timeout != 10*time.Second || l.MemoryBytes != 32<<20 || l.MaxToolCalls != 50 || l.MaxOutputBytes != 16<<10 {
		t.Errorf("limit defaults not applied: %+v", l)
	}
}

func TestFullFile(t *testing.T) {
	cfg, err := load(t, `
[server]
address = ":8000"
max_concurrent_runs = 4

[catalog]
ttl = "30s"

[[endpoints]]
name = "ops"
allow_destructive = true
[endpoints.limits]
timeout = "3s"
max_tool_calls = 5
[endpoints.headers]
forward = ["authorization"]
[[endpoints.headers.map]]
from = "x-jwt-assertion"
to = "authorization"
prefix = "Bearer "
[[endpoints.sources]]
name = "inventory"
url = "http://gw:8080/inventory/mcp"
[[endpoints.sources]]
name = "weather"
url = "https://gw:8443/weather/mcp"
`)
	if err != nil {
		t.Fatal(err)
	}
	e := cfg.Endpoints[0]
	if cfg.Server.Address != ":8000" || cfg.Server.MaxConcurrentRuns != 4 || cfg.Catalog.TTL != 30*time.Second {
		t.Errorf("server/catalog: %+v %+v", cfg.Server, cfg.Catalog)
	}
	if !e.AllowDestructive || e.Limits.Timeout != 3*time.Second || e.Limits.MaxToolCalls != 5 || len(e.Sources) != 2 {
		t.Errorf("endpoint: %+v", e)
	}
	if e.Headers.Forward[0] != "Authorization" || e.Headers.Map[0].From != "X-Jwt-Assertion" || e.Headers.Map[0].Prefix != "Bearer " {
		t.Errorf("headers not canonicalised: %+v", e.Headers)
	}
}

func TestRejects(t *testing.T) {
	src := "[[endpoints.sources]]\nname = \"inventory\"\nurl = \"http://gw/mcp\"\n"
	cases := map[string]string{
		"no endpoints":       ``,
		"bad endpoint name":  "[[endpoints]]\nname = \"Ops!\"\n" + src,
		"duplicate endpoint": "[[endpoints]]\nname = \"a\"\n" + src + "[[endpoints]]\nname = \"a\"\n" + src,
		"no sources":         "[[endpoints]]\nname = \"a\"\n",
		"source not an identifier": "[[endpoints]]\nname = \"a\"\n" +
			"[[endpoints.sources]]\nname = \"in-ventory\"\nurl = \"http://gw/mcp\"\n",
		"duplicate source": "[[endpoints]]\nname = \"a\"\n" + src + src,
		"file url": "[[endpoints]]\nname = \"a\"\n" +
			"[[endpoints.sources]]\nname = \"s\"\nurl = \"file:///etc/passwd\"\n",
		"credentials in url": "[[endpoints]]\nname = \"a\"\n" +
			"[[endpoints.sources]]\nname = \"s\"\nurl = \"http://u:p@gw/mcp\"\n",
		"memory above cap": "[[endpoints]]\nname = \"a\"\n[endpoints.limits]\nmemory_bytes = 134217728\n" + src,
		"timeout too long": "[[endpoints]]\nname = \"a\"\n[endpoints.limits]\ntimeout = \"1h\"\n" + src,
		"negative limit":   "[[endpoints]]\nname = \"a\"\n[endpoints.limits]\nmax_tool_calls = -1\n" + src,
		"bad header":       "[[endpoints]]\nname = \"a\"\n[endpoints.headers]\nforward = [\"bad header\"]\n" + src,
		"zero runs":        "[server]\nmax_concurrent_runs = 0\n[[endpoints]]\nname = \"a\"\n" + src,
	}
	for name, toml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := load(t, toml); err == nil {
				t.Errorf("accepted:\n%s", toml)
			}
		})
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.toml")); err == nil || !strings.Contains(err.Error(), "nope.toml") {
		t.Errorf("err = %v", err)
	}
}
