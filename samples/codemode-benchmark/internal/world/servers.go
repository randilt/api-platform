package world

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerNames are the four MCP servers, in the order the benchmark lists them.
var ServerNames = []string{"inventory", "crm", "billing", "logistics"}

// add registers a typed tool whose result is returned as structured content with a schema.
func add[In, Out any](s *mcp.Server, name, desc string, f func(In) (Out, error)) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc}, func(_ context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		out, err := f(in)
		return nil, out, err
	})
}

type (
	none  struct{}
	skuIn struct {
		SKU string `json:"sku" jsonschema:"product SKU, e.g. SKU-007"`
	}
	whIn struct {
		Warehouse string `json:"warehouse" jsonschema:"warehouse city: Colombo, Kandy, Galle or Jaffna"`
	}
	skuWhIn struct {
		SKU       string `json:"sku" jsonschema:"product SKU"`
		Warehouse string `json:"warehouse" jsonschema:"warehouse city"`
	}
	queryIn struct {
		Query string `json:"query" jsonschema:"text to search for in product names and categories"`
	}
	categoryIn struct {
		Category string `json:"category" jsonschema:"laptop, monitor, keyboard, mouse or cable"`
	}
	idIn struct {
		ID string `json:"id" jsonschema:"identifier"`
	}
	custIn struct {
		CustomerID string `json:"customerId" jsonschema:"customer ID, e.g. C-01"`
	}
	nameIn struct {
		Name string `json:"name" jsonschema:"full or partial customer name"`
	}
	cityIn struct {
		City string `json:"city" jsonschema:"city name"`
	}
	tierIn struct {
		Tier string `json:"tier" jsonschema:"gold, silver or bronze"`
	}
	statusIn struct {
		Status string `json:"status" jsonschema:"pending, shipped, delivered or cancelled"`
	}
	orderIn struct {
		OrderID string `json:"orderId" jsonschema:"order ID, e.g. O-1014"`
	}
	carrierIn struct {
		Carrier string `json:"carrier" jsonschema:"carrier name"`
	}
	rateIn struct {
		Carrier string `json:"carrier" jsonschema:"carrier name"`
		City    string `json:"city" jsonschema:"destination city"`
	}
	routeIn struct {
		From string `json:"from" jsonschema:"origin city"`
		To   string `json:"to" jsonschema:"destination city"`
	}
	currencyIn struct {
		Currency string `json:"currency" jsonschema:"ISO currency code: USD, EUR or GBP"`
	}
	trackIn struct {
		Tracking string `json:"tracking" jsonschema:"tracking number"`
	}

	products struct {
		Products []Product `json:"products"`
	}
	stocks struct {
		Stock []Stock `json:"stock"`
	}
	names struct {
		Names []string `json:"names"`
	}
	customers struct {
		Customers []Customer `json:"customers"`
	}
	orders struct {
		Orders []Order `json:"orders"`
	}
	invoices struct {
		Invoices []Invoice `json:"invoices"`
	}
	shipments struct {
		Shipments []Shipment `json:"shipments"`
	}
	skuNumber struct {
		SKU   string `json:"sku"`
		Value int    `json:"value"`
	}
	custNumber struct {
		CustomerID string `json:"customerId"`
		Value      int    `json:"value"`
	}
	keyNumber struct {
		Key   string `json:"key"`
		Value int    `json:"value"`
	}
	notes struct {
		CustomerID string `json:"customerId"`
		Notes      string `json:"notes"`
	}
	refund struct {
		OrderID   string `json:"orderId"`
		AmountLKR int    `json:"amountLkr"`
	}
	refunds struct {
		Refunds []refund `json:"refunds"`
	}
	route struct {
		From string `json:"from"`
		To   string `json:"to"`
		Km   int    `json:"km"`
	}
	hours struct {
		Warehouse string `json:"warehouse"`
		Opens     string `json:"opens"`
		Closes    string `json:"closes"`
	}
	dates struct {
		City  string   `json:"city"`
		Dates []string `json:"dates"`
	}
)

func filter[T any](xs []T, keep func(T) bool) []T {
	out := []T{}
	for _, x := range xs {
		if keep(x) {
			out = append(out, x)
		}
	}
	return out
}

func city(c string) (string, error) {
	for _, w := range Warehouses {
		if strings.EqualFold(w, c) {
			return w, nil
		}
	}
	return "", fmt.Errorf("unknown city %q", c)
}

// Servers returns the four MCP servers over d, 12 tools each.
func Servers(d *Data) map[string]*mcp.Server {
	impl := func(n string) *mcp.Server { return mcp.NewServer(&mcp.Implementation{Name: n, Version: "1.0.0"}, nil) }
	inv, crm, bill, logi := impl("inventory"), impl("crm"), impl("billing"), impl("logistics")

	add(inv, "list_products", "List every product with its category, unit price and reorder level.", func(none) (products, error) { return products{d.Products}, nil })
	add(inv, "get_product", "Get one product by SKU.", func(in skuIn) (Product, error) {
		if p, ok := d.Product(in.SKU); ok {
			return p, nil
		}
		return Product{}, fmt.Errorf("unknown sku %q", in.SKU)
	})
	add(inv, "search_products", "Find products whose name or category contains the query.", func(in queryIn) (products, error) {
		q := strings.ToLower(in.Query)
		return products{filter(d.Products, func(p Product) bool { return strings.Contains(strings.ToLower(p.Name+" "+p.Category), q) })}, nil
	})
	add(inv, "list_categories", "List product categories.", func(none) (names, error) { return names{Categories}, nil })
	add(inv, "products_by_category", "List the products in one category.", func(in categoryIn) (products, error) {
		return products{filter(d.Products, func(p Product) bool { return strings.EqualFold(p.Category, in.Category) })}, nil
	})
	add(inv, "get_stock", "Get the units of one SKU held in one warehouse.", func(in skuWhIn) (Stock, error) {
		w, err := city(in.Warehouse)
		if err != nil {
			return Stock{}, err
		}
		if q, ok := d.StockOf(in.SKU, w); ok {
			return Stock{SKU: in.SKU, Warehouse: w, Qty: q}, nil
		}
		return Stock{}, fmt.Errorf("unknown sku %q", in.SKU)
	})
	add(inv, "stock_by_warehouse", "List the stock of every SKU in one warehouse.", func(in whIn) (stocks, error) {
		w, err := city(in.Warehouse)
		return stocks{filter(d.Stock, func(s Stock) bool { return s.Warehouse == w })}, err
	})
	add(inv, "stock_by_sku", "List the stock of one SKU in every warehouse.", func(in skuIn) (stocks, error) {
		return stocks{filter(d.Stock, func(s Stock) bool { return s.SKU == in.SKU })}, nil
	})
	add(inv, "total_stock", "Total units of one SKU across all warehouses.", func(in skuIn) (skuNumber, error) {
		t := 0
		for _, s := range d.Stock {
			if s.SKU == in.SKU {
				t += s.Qty
			}
		}
		return skuNumber{in.SKU, t}, nil
	})
	add(inv, "list_warehouses", "List the warehouses.", func(none) (names, error) { return names{Warehouses}, nil })
	add(inv, "reorder_level", "Get the reorder level of one SKU.", func(in skuIn) (skuNumber, error) {
		p, ok := d.Product(in.SKU)
		if !ok {
			return skuNumber{}, fmt.Errorf("unknown sku %q", in.SKU)
		}
		return skuNumber{p.SKU, p.ReorderLevel}, nil
	})
	add(inv, "low_stock", "List the SKUs in one warehouse whose stock is at or below their reorder level.", func(in whIn) (stocks, error) {
		w, err := city(in.Warehouse)
		return stocks{filter(d.Stock, func(s Stock) bool {
			p, _ := d.Product(s.SKU)
			return s.Warehouse == w && s.Qty <= p.ReorderLevel
		})}, err
	})

	add(crm, "list_customers", "List every customer with city and tier.", func(none) (customers, error) { return customers{d.Customers}, nil })
	add(crm, "get_customer", "Get one customer by ID.", func(in custIn) (Customer, error) {
		if c, ok := d.Customer(in.CustomerID); ok {
			return c, nil
		}
		return Customer{}, fmt.Errorf("unknown customer %q", in.CustomerID)
	})
	add(crm, "find_customer_by_name", "Find customers whose name contains the text.", func(in nameIn) (customers, error) {
		n := strings.ToLower(in.Name)
		return customers{filter(d.Customers, func(c Customer) bool { return strings.Contains(strings.ToLower(c.Name), n) })}, nil
	})
	add(crm, "customers_by_city", "List the customers in one city.", func(in cityIn) (customers, error) {
		return customers{filter(d.Customers, func(c Customer) bool { return strings.EqualFold(c.City, in.City) })}, nil
	})
	add(crm, "customers_by_tier", "List the customers in one tier.", func(in tierIn) (customers, error) {
		return customers{filter(d.Customers, func(c Customer) bool { return strings.EqualFold(c.Tier, in.Tier) })}, nil
	})
	add(crm, "list_orders", "List every order.", func(none) (orders, error) { return orders{d.Orders}, nil })
	add(crm, "get_order", "Get one order by ID.", func(in orderIn) (Order, error) {
		if o, ok := d.Order(in.OrderID); ok {
			return o, nil
		}
		return Order{}, fmt.Errorf("unknown order %q", in.OrderID)
	})
	add(crm, "orders_by_customer", "List one customer's orders.", func(in custIn) (orders, error) {
		return orders{filter(d.Orders, func(o Order) bool { return o.CustomerID == in.CustomerID })}, nil
	})
	add(crm, "orders_by_status", "List the orders with one status.", func(in statusIn) (orders, error) {
		return orders{filter(d.Orders, func(o Order) bool { return strings.EqualFold(o.Status, in.Status) })}, nil
	})
	add(crm, "orders_by_sku", "List the orders for one SKU.", func(in skuIn) (orders, error) {
		return orders{filter(d.Orders, func(o Order) bool { return o.SKU == in.SKU })}, nil
	})
	add(crm, "order_count", "Count one customer's orders.", func(in custIn) (custNumber, error) {
		return custNumber{in.CustomerID, len(filter(d.Orders, func(o Order) bool { return o.CustomerID == in.CustomerID }))}, nil
	})
	add(crm, "customer_notes", "Read the account manager's notes on a customer.", func(in custIn) (notes, error) {
		c, ok := d.Customer(in.CustomerID)
		if !ok {
			return notes{}, fmt.Errorf("unknown customer %q", in.CustomerID)
		}
		return notes{c.ID, fmt.Sprintf("%s prefers deliveries before noon and email over calls.", c.Name)}, nil
	})

	add(bill, "list_invoices", "List every invoice.", func(none) (invoices, error) { return invoices{d.Invoices}, nil })
	add(bill, "get_invoice", "Get one invoice by ID.", func(in idIn) (Invoice, error) {
		for _, i := range d.Invoices {
			if i.ID == in.ID {
				return i, nil
			}
		}
		return Invoice{}, fmt.Errorf("unknown invoice %q", in.ID)
	})
	add(bill, "invoice_for_order", "Get the invoice of one order (cancelled orders have none).", func(in orderIn) (Invoice, error) {
		if i, ok := d.InvoiceFor(in.OrderID); ok {
			return i, nil
		}
		return Invoice{}, fmt.Errorf("no invoice for order %q", in.OrderID)
	})
	add(bill, "unpaid_invoices", "List the invoices not yet paid.", func(none) (invoices, error) {
		return invoices{filter(d.Invoices, func(i Invoice) bool { return !i.Paid })}, nil
	})
	add(bill, "invoices_by_customer", "List one customer's invoices.", func(in custIn) (invoices, error) {
		return invoices{filter(d.Invoices, func(i Invoice) bool { o, _ := d.Order(i.OrderID); return o.CustomerID == in.CustomerID })}, nil
	})
	add(bill, "customer_balance", "Total of one customer's unpaid invoices, in LKR.", func(in custIn) (custNumber, error) {
		t := 0
		for _, i := range d.Invoices {
			if o, _ := d.Order(i.OrderID); o.CustomerID == in.CustomerID && !i.Paid {
				t += i.AmountLKR
			}
		}
		return custNumber{in.CustomerID, t}, nil
	})
	add(bill, "tax_rate", "Sales tax percent in one city.", func(in cityIn) (keyNumber, error) {
		c, err := city(in.City)
		return keyNumber{c, taxPercent[c]}, err
	})
	add(bill, "discount_for_tier", "Discount percent a customer tier gets.", func(in tierIn) (keyNumber, error) {
		pct := map[string]int{"gold": 10, "silver": 5, "bronze": 0}
		v, ok := pct[strings.ToLower(in.Tier)]
		if !ok {
			return keyNumber{}, fmt.Errorf("unknown tier %q", in.Tier)
		}
		return keyNumber{in.Tier, v}, nil
	})
	add(bill, "credit_limit", "One customer's credit limit, in LKR.", func(in custIn) (custNumber, error) {
		c, ok := d.Customer(in.CustomerID)
		if !ok {
			return custNumber{}, fmt.Errorf("unknown customer %q", in.CustomerID)
		}
		return custNumber{c.ID, map[string]int{"gold": 5000000, "silver": 2000000, "bronze": 500000}[c.Tier]}, nil
	})
	add(bill, "payment_terms", "Days one customer has to pay an invoice.", func(in custIn) (custNumber, error) {
		c, ok := d.Customer(in.CustomerID)
		if !ok {
			return custNumber{}, fmt.Errorf("unknown customer %q", in.CustomerID)
		}
		return custNumber{c.ID, map[string]int{"gold": 60, "silver": 30, "bronze": 14}[c.Tier]}, nil
	})
	add(bill, "list_refunds", "List the refunds owed on cancelled orders.", func(none) (refunds, error) {
		out := refunds{Refunds: []refund{}}
		for _, o := range d.Orders {
			if o.Status == "cancelled" {
				p, _ := d.Product(o.SKU)
				out.Refunds = append(out.Refunds, refund{o.ID, o.Qty * p.PriceLKR})
			}
		}
		return out, nil
	})
	add(bill, "currency_rate", "Rupees per unit of a foreign currency.", func(in currencyIn) (keyNumber, error) {
		r, ok := map[string]int{"USD": 300, "EUR": 325, "GBP": 380}[strings.ToUpper(in.Currency)]
		if !ok {
			return keyNumber{}, fmt.Errorf("unknown currency %q", in.Currency)
		}
		return keyNumber{strings.ToUpper(in.Currency), r}, nil
	})

	add(logi, "get_shipment", "Get the shipment of one order (pending and cancelled orders have none).", func(in orderIn) (Shipment, error) {
		if s, ok := d.ShipmentFor(in.OrderID); ok {
			return s, nil
		}
		return Shipment{}, fmt.Errorf("no shipment for order %q", in.OrderID)
	})
	add(logi, "list_shipments", "List every shipment.", func(none) (shipments, error) { return shipments{d.Shipments}, nil })
	add(logi, "shipments_by_carrier", "List one carrier's shipments.", func(in carrierIn) (shipments, error) {
		return shipments{filter(d.Shipments, func(s Shipment) bool { return strings.EqualFold(s.Carrier, in.Carrier) })}, nil
	})
	add(logi, "delayed_shipments", "List the shipments running late.", func(none) (shipments, error) {
		return shipments{filter(d.Shipments, func(s Shipment) bool { return s.Delayed })}, nil
	})
	add(logi, "list_carriers", "List the carriers.", func(none) (names, error) { return names{Carriers}, nil })
	add(logi, "carrier_rate", "What one carrier charges to deliver to one city, in LKR.", func(in rateIn) (keyNumber, error) {
		c, err := city(in.City)
		if err != nil {
			return keyNumber{}, err
		}
		r, ok := CarrierRate(in.Carrier, c)
		if !ok {
			return keyNumber{}, fmt.Errorf("unknown carrier %q", in.Carrier)
		}
		return keyNumber{in.Carrier + " to " + c, r}, nil
	})
	add(logi, "forecast", "Today's weather forecast for one city.", func(in cityIn) (Forecast, error) {
		c, err := city(in.City)
		return weather[c], err
	})
	add(logi, "route_distance", "Road distance between two cities, in km.", func(in routeIn) (route, error) {
		f, err := city(in.From)
		if err != nil {
			return route{}, err
		}
		t, err := city(in.To)
		return route{f, t, distanceKm[warehouseIndex(f)][warehouseIndex(t)]}, err
	})
	add(logi, "estimate_delivery_days", "Days a delivery takes between two cities.", func(in routeIn) (keyNumber, error) {
		f, err := city(in.From)
		if err != nil {
			return keyNumber{}, err
		}
		t, err := city(in.To)
		return keyNumber{f + " to " + t, 1 + distanceKm[warehouseIndex(f)][warehouseIndex(t)]/150}, err
	})
	add(logi, "warehouse_hours", "Opening hours of one warehouse.", func(in whIn) (hours, error) {
		w, err := city(in.Warehouse)
		return hours{w, "08:00", "18:00"}, err
	})
	add(logi, "holidays", "Public holidays this month in one city, when no deliveries run.", func(in cityIn) (dates, error) {
		c, err := city(in.City)
		return dates{c, []string{"2026-11-26"}}, err
	})
	add(logi, "track", "Find a shipment by tracking number.", func(in trackIn) (Shipment, error) {
		for _, s := range d.Shipments {
			if s.Tracking == in.Tracking {
				return s, nil
			}
		}
		return Shipment{}, fmt.Errorf("unknown tracking number %q", in.Tracking)
	})

	return map[string]*mcp.Server{"inventory": inv, "crm": crm, "billing": bill, "logistics": logi}
}
