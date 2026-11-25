// Package catalog caches each caller's tools/list per source. The gateway filters the list by
// who is asking, so an entry is keyed by the source and a hash of the caller's headers and is
// never shared between callers with different credentials.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Headers that differ on every request and say nothing about who is calling.
var perRequest = map[string]bool{"X-Codemode-Run-Id": true, "Traceparent": true, "Tracestate": true}

// Lister fetches a source's tools and the TTL its server sent (0 when none).
type Lister func(ctx context.Context) ([]*mcp.Tool, time.Duration, error)

// Catalog is a bounded TTL cache of tool lists.
type Catalog struct {
	defaultTTL, maxTTL time.Duration
	maxEntries         int
	now                func() time.Time

	mu      sync.Mutex
	entries map[string]entry
}

type entry struct {
	tools   []*mcp.Tool
	expires time.Time
}

// New returns an empty catalog. A server-sent TTL is used when present, capped at maxTTL;
// otherwise entries live for defaultTTL.
func New(defaultTTL, maxTTL time.Duration, maxEntries int) *Catalog {
	return &Catalog{defaultTTL: defaultTTL, maxTTL: maxTTL, maxEntries: maxEntries, now: time.Now, entries: map[string]entry{}}
}

// Key identifies one caller's view of one source.
func Key(sourceURL string, h http.Header) string {
	names := make([]string, 0, len(h))
	for k := range h {
		if !perRequest[http.CanonicalHeaderKey(k)] {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	sum := sha256.New()
	sum.Write([]byte(sourceURL))
	for _, k := range names {
		sum.Write([]byte{0})
		sum.Write([]byte(http.CanonicalHeaderKey(k)))
		for _, v := range h.Values(k) {
			sum.Write([]byte{1})
			sum.Write([]byte(v))
		}
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// Get returns the cached list for key, or calls list and caches what it returns. Failures are
// not cached, so a source that comes back is used on the next call.
func (c *Catalog) Get(ctx context.Context, key string, list Lister) ([]*mcp.Tool, error) {
	now := c.now()
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.tools, nil
	}
	c.mu.Unlock()

	tools, ttl, err := list(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case ttl <= 0:
		ttl = c.defaultTTL // the Go SDK always sends ttlMs, as 0 when the server set none
	case ttl > c.maxTTL:
		ttl = c.maxTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; !ok && len(c.entries) >= c.maxEntries {
		c.evict(now)
	}
	c.entries[key] = entry{tools: tools, expires: now.Add(ttl)}
	return tools, nil
}

// evict drops expired entries, then the one closest to expiry if the cache is still full.
func (c *Catalog) evict(now time.Time) {
	var oldest string
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
			continue
		}
		if oldest == "" || e.expires.Before(c.entries[oldest].expires) {
			oldest = k
		}
	}
	if len(c.entries) >= c.maxEntries && oldest != "" {
		delete(c.entries, oldest)
	}
}

// Len reports how many lists are cached.
func (c *Catalog) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
