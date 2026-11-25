// toolserver is a tiny MCP server used as an upstream behind the WSO2 AI gateway in the
// Code Mode spike. It plays either the "inventory" or the "weather" server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Item struct {
	SKU       string `json:"sku" jsonschema:"stock keeping unit"`
	Name      string `json:"name"`
	Qty       int    `json:"qty" jsonschema:"units on hand"`
	Warehouse string `json:"warehouse" jsonschema:"city of the warehouse holding the item"`
}

type Empty struct{}
type ListItemsOut struct {
	Items []Item `json:"items"`
}
type SKUIn struct {
	SKU string `json:"sku" jsonschema:"the item SKU, e.g. SKU-001"`
}
type StockOut struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}
type DeleteOut struct {
	Deleted bool `json:"deleted"`
}
type CityIn struct {
	City string `json:"city" jsonschema:"city name"`
}
type ForecastOut struct {
	City      string  `json:"city"`
	TempC     float64 `json:"tempC"`
	Condition string  `json:"condition"`
}

var cities = []string{"Colombo", "Kandy", "Galle", "Jaffna"}

func items() []Item {
	var out []Item
	for i := 1; i <= 20; i++ {
		out = append(out, Item{
			SKU:       fmt.Sprintf("SKU-%03d", i),
			Name:      fmt.Sprintf("Item %d", i),
			Qty:       (i * 7) % 23,
			Warehouse: cities[i%len(cities)],
		})
	}
	return out
}

func logMeta(tool string, req *mcp.CallToolRequest) {
	run := ""
	if req.Params != nil && req.Params.Meta != nil {
		if v, ok := req.Params.Meta["com.wso2.apip/codemode.runId"]; ok {
			run = fmt.Sprint(v)
		}
	}
	log.Printf("tools/call %s runId=%q", tool, run)
}

func main() {
	name := flag.String("name", "inventory", "inventory | weather")
	addr := flag.String("addr", ":7000", "listen address")
	flag.Parse()

	s := mcp.NewServer(&mcp.Implementation{Name: *name + "-mcp", Version: "0.1.0"}, nil)
	switch *name {
	case "inventory":
		mcp.AddTool(s, &mcp.Tool{Name: "list_items", Description: "List every inventory item with its quantity and warehouse."},
			func(ctx context.Context, req *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, ListItemsOut, error) {
				logMeta("list_items", req)
				return nil, ListItemsOut{Items: items()}, nil
			})
		mcp.AddTool(s, &mcp.Tool{Name: "get_stock", Description: "Get the current stock level for one SKU."},
			func(ctx context.Context, req *mcp.CallToolRequest, in SKUIn) (*mcp.CallToolResult, StockOut, error) {
				logMeta("get_stock", req)
				for _, it := range items() {
					if strings.EqualFold(it.SKU, in.SKU) {
						return nil, StockOut{SKU: it.SKU, Qty: it.Qty}, nil
					}
				}
				return nil, StockOut{}, fmt.Errorf("unknown sku %s", in.SKU)
			})
		destructive := true
		mcp.AddTool(s, &mcp.Tool{Name: "delete_item", Description: "Permanently delete an item.",
			Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive}},
			func(ctx context.Context, req *mcp.CallToolRequest, in SKUIn) (*mcp.CallToolResult, DeleteOut, error) {
				logMeta("delete_item", req)
				log.Printf("!!! delete_item reached the upstream for %s", in.SKU)
				return nil, DeleteOut{Deleted: true}, nil
			})
	case "weather":
		mcp.AddTool(s, &mcp.Tool{Name: "get_forecast", Description: "Get today's forecast for a city."},
			func(ctx context.Context, req *mcp.CallToolRequest, in CityIn) (*mcp.CallToolResult, ForecastOut, error) {
				logMeta("get_forecast", req)
				t := 24.0 + float64(len(in.City)%5)
				cond := "sunny"
				if len(in.City)%2 == 0 {
					cond = "rain"
				}
				return nil, ForecastOut{City: in.City, TempC: t, Condition: cond}, nil
			})
	}

	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.MaxBytesHandler(h, 1<<20))
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 1 << 20}
	log.Printf("%s MCP server on %s/mcp", *name, *addr)
	log.Fatal(srv.ListenAndServe())
}
