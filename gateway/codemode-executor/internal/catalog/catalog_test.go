package catalog

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func counting(ttl time.Duration, n *int) Lister {
	return func(context.Context) ([]*mcp.Tool, time.Duration, error) {
		*n++
		return []*mcp.Tool{{Name: "t"}}, ttl, nil
	}
}

func newCat(clk *clock, max int) *Catalog {
	c := New(time.Minute, time.Hour, max)
	c.now = clk.now
	return c
}

// TestSecondSearchWithinTTLIsCached is the plan's done-when: a second search in a minute makes
// no upstream call.
func TestSecondSearchWithinTTLIsCached(t *testing.T) {
	clk := &clock{time.Unix(0, 0)}
	c, n := newCat(clk, 10), 0
	for i := 0; i < 2; i++ {
		if _, err := c.Get(context.Background(), "k", counting(0, &n)); err != nil {
			t.Fatal(err)
		}
		clk.t = clk.t.Add(59 * time.Second)
	}
	if n != 1 {
		t.Errorf("lister called %d times within the TTL, want 1", n)
	}
	clk.t = clk.t.Add(2 * time.Second)
	_, _ = c.Get(context.Background(), "k", counting(0, &n))
	if n != 2 {
		t.Errorf("lister not called again after expiry")
	}
}

func TestServerTTLIsHonouredAndCapped(t *testing.T) {
	clk := &clock{time.Unix(0, 0)}
	c, n := newCat(clk, 10), 0
	_, _ = c.Get(context.Background(), "short", counting(5*time.Second, &n))
	clk.t = clk.t.Add(6 * time.Second)
	_, _ = c.Get(context.Background(), "short", counting(5*time.Second, &n))
	if n != 2 {
		t.Errorf("a 5 s server TTL should expire after 6 s; lister calls = %d", n)
	}
	n = 0
	_, _ = c.Get(context.Background(), "long", counting(24*time.Hour, &n))
	clk.t = clk.t.Add(61 * time.Minute)
	_, _ = c.Get(context.Background(), "long", counting(24*time.Hour, &n))
	if n != 2 {
		t.Errorf("a server TTL above max_ttl should be capped; lister calls = %d", n)
	}
}

func TestFailuresAreNotCached(t *testing.T) {
	c := newCat(&clock{time.Unix(0, 0)}, 10)
	fail := func(context.Context) ([]*mcp.Tool, time.Duration, error) { return nil, 0, errors.New("down") }
	if _, err := c.Get(context.Background(), "k", fail); err == nil {
		t.Fatal("want error")
	}
	n := 0
	if _, err := c.Get(context.Background(), "k", counting(0, &n)); err != nil || n != 1 {
		t.Errorf("source should be retried after a failure: err=%v calls=%d", err, n)
	}
}

func TestBounded(t *testing.T) {
	clk := &clock{time.Unix(0, 0)}
	c, n := newCat(clk, 3), 0
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		_, _ = c.Get(context.Background(), k, counting(0, &n))
		clk.t = clk.t.Add(time.Second)
	}
	if c.Len() != 3 {
		t.Errorf("cache holds %d entries, want 3", c.Len())
	}
}

func TestKeySeparatesCallers(t *testing.T) {
	alice := http.Header{"Authorization": {"Bearer alice"}, "X-Codemode-Run-Id": {"r1"}}
	alice2 := http.Header{"Authorization": {"Bearer alice"}, "X-Codemode-Run-Id": {"r2"}, "Traceparent": {"00-x"}}
	bob := http.Header{"Authorization": {"Bearer bob"}}
	if Key("http://gw/a/mcp", alice) != Key("http://gw/a/mcp", alice2) {
		t.Error("run id and trace headers must not change the key")
	}
	if Key("http://gw/a/mcp", alice) == Key("http://gw/a/mcp", bob) {
		t.Error("different credentials must not share a key")
	}
	if Key("http://gw/a/mcp", alice) == Key("http://gw/b/mcp", alice) {
		t.Error("different sources must not share a key")
	}
	if Key("http://gw/a/mcp", http.Header{}) == Key("http://gw/a/mcp", bob) {
		t.Error("anonymous and authenticated callers must not share a key")
	}
}
