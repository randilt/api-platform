// client drives the Code Mode spike end to end through the gateway and prints what happened.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func session(ctx context.Context, url string) *mcp.ClientSession {
	c := mcp.NewClient(&mcp.Implementation{Name: "spike-client", Version: "0.1.0"}, nil)
	s, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		log.Fatalf("connect %s: %v", url, err)
	}
	return s
}

func toolBytes(ctx context.Context, s *mcp.ClientSession) (int, []string) {
	res, err := s.ListTools(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}
	b, _ := json.Marshal(res.Tools)
	var names []string
	for _, t := range res.Tools {
		names = append(names, t.Name)
	}
	return len(b), names
}

func call(ctx context.Context, s *mcp.ClientSession, tool string, args map[string]any) {
	start := time.Now()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		fmt.Printf("  -> transport/JSON-RPC error: %v\n", err)
		return
	}
	b, _ := json.MarshalIndent(res.StructuredContent, "  ", "  ")
	fmt.Printf("  isError=%v in %s\n  %s\n", res.IsError, time.Since(start).Round(time.Millisecond), b)
}

func main() {
	gw := "http://localhost:8080"
	if v := os.Getenv("GW"); v != "" {
		gw = v
	}
	ctx := context.Background()

	inv, wea, cm := session(ctx, gw+"/inventory/mcp"), session(ctx, gw+"/weather/mcp"), session(ctx, gw+"/codemode/mcp")
	ib, in := toolBytes(ctx, inv)
	wb, wn := toolBytes(ctx, wea)
	cb, cn := toolBytes(ctx, cm)
	fmt.Println("== tool definitions a client would load (JSON bytes, ~4 bytes per token)")
	fmt.Printf("  direct: inventory %v %dB + weather %v %dB = %dB\n", in, ib, wn, wb, ib+wb)
	fmt.Printf("  code mode: %v %dB\n", cn, cb)

	// What the same multi-step task costs without Code Mode: every intermediate result goes
	// through the model's context.
	direct := 0
	r1, _ := inv.CallTool(ctx, &mcp.CallToolParams{Name: "list_items", Arguments: map[string]any{}})
	b1, _ := json.Marshal(r1.StructuredContent)
	direct += len(b1)
	for _, c := range []string{"Jaffna", "Galle", "Kandy", "Colombo"} {
		r, _ := wea.CallTool(ctx, &mcp.CallToolParams{Name: "get_forecast", Arguments: map[string]any{"city": c}})
		b, _ := json.Marshal(r.StructuredContent)
		direct += len(b)
	}
	fmt.Printf("  scenario 2 without Code Mode: 5 model round trips, %dB of tool results into context\n", direct)

	fmt.Println("\n== 1. search 'stock'")
	call(ctx, cm, "search", map[string]any{"query": "stock"})

	scenarios := []struct{ name, code string }{
		{"2. multi-step: low-stock items + weather at their warehouses, one round trip", `
const { items } = await codemode.inventory.list_items({});
const low = items.filter(i => i.qty < 5);
const cities = [...new Set(low.map(i => i.warehouse))];
const weather = {};
for (const c of cities) weather[c] = await codemode.weather.get_forecast({ city: c });
console.log('scanned', items.length, 'items, low stock:', low.length);
return low.map(i => ({ sku: i.sku, qty: i.qty, warehouse: i.warehouse, rainAtWarehouse: weather[i.warehouse].condition === 'rain' }));`},
		{"3. tool hidden by mcp-acl-list, called by computed name", `
const name = ['delete', 'item'].join('_');
try { await codemode.inventory[name]({ sku: 'SKU-001' }); return 'DELETE WENT THROUGH'; }
catch (e) { return 'blocked by gateway: ' + e.message; }`},
		{"4. tool error surfaces as a catchable exception", `
try { await codemode.inventory.get_stock({ sku: 'NOPE' }); } catch (e) { return 'caught: ' + e.message; }`},
		{"5. sandbox: no network, no file system", `
const out = {};
out.fetch = typeof fetch;
try { const os = await import('qjs:os'); out.readdir = os.readdir('/'); } catch (e) { out.osImport = 'error: ' + e.message; }
try { const std = await import('qjs:std'); out.passwd = std.loadFile('/etc/passwd'); out.env = std.getenv('HOME'); } catch (e) { out.stdImport = 'error: ' + e.message; }
return out;`},
		{"6. runaway loop is stopped by the wall-clock limit", `while (true) {}`},
		{"7. memory limit", `const a = []; while (true) a.push('x'.repeat(1 << 20));`},
	}
	for _, s := range scenarios {
		fmt.Println("\n== " + s.name)
		call(ctx, cm, "execute", map[string]any{"code": s.code})
	}
}
