// Package world is the benchmark's company: a fixed dataset, the four MCP servers that expose
// it (48 tools), and ten multi-step tasks whose expected answers are computed from the same
// data, so a change to the data can never leave an answer stale. Nothing here is random.
package world

import (
	"fmt"
)

var (
	Warehouses = []string{"Colombo", "Kandy", "Galle", "Jaffna"}
	Categories = []string{"laptop", "monitor", "keyboard", "mouse", "cable"}
	Carriers   = []string{"SwiftLanka", "IslandExpress", "CeyFreight"}
	Tiers      = []string{"gold", "silver", "bronze"}

	categoryBase  = map[string]int{"laptop": 250000, "monitor": 60000, "keyboard": 8000, "mouse": 3000, "cable": 1500}
	categoryStep  = map[string]int{"laptop": 1000, "monitor": 500, "keyboard": 100, "mouse": 50, "cable": 20}
	customerNames = []string{"Nimal Perera", "Kumari Silva", "Ruwan Fernando", "Ayesha Jayasinghe", "Sunil Bandara",
		"Dilani Wickramasinghe", "Kasun Rajapaksha", "Tharushi Gunawardena", "Mahesh Dissanayake",
		"Nadeesha Herath", "Chaminda Weerasinghe", "Ishara Senanayake"}
	weather = map[string]Forecast{
		"Colombo": {City: "Colombo", Condition: "sunny", TempC: 31, WindKmh: 14},
		"Kandy":   {City: "Kandy", Condition: "rain", TempC: 24, WindKmh: 9},
		"Galle":   {City: "Galle", Condition: "cloudy", TempC: 29, WindKmh: 22},
		"Jaffna":  {City: "Jaffna", Condition: "rain", TempC: 33, WindKmh: 18},
	}
	taxPercent  = map[string]int{"Colombo": 8, "Kandy": 8, "Galle": 10, "Jaffna": 12}
	carrierBase = map[string]int{"SwiftLanka": 900, "IslandExpress": 750, "CeyFreight": 600}
	distanceKm  = [4][4]int{{0, 115, 126, 396}, {115, 0, 230, 320}, {126, 230, 0, 520}, {396, 320, 520, 0}}
)

type Product struct {
	SKU          string `json:"sku"`
	Name         string `json:"name"`
	Category     string `json:"category"`
	PriceLKR     int    `json:"priceLkr" jsonschema:"unit price in Sri Lankan rupees"`
	ReorderLevel int    `json:"reorderLevel" jsonschema:"stock at or below this level should be reordered"`
}

type Stock struct {
	SKU       string `json:"sku"`
	Warehouse string `json:"warehouse"`
	Qty       int    `json:"qty"`
}

type Customer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	City string `json:"city"`
	Tier string `json:"tier" jsonschema:"gold, silver or bronze"`
}

type Order struct {
	ID         string `json:"id"`
	CustomerID string `json:"customerId"`
	SKU        string `json:"sku"`
	Qty        int    `json:"qty"`
	Status     string `json:"status" jsonschema:"pending, shipped, delivered or cancelled"`
	Warehouse  string `json:"warehouse" jsonschema:"warehouse the order ships from"`
	Date       string `json:"date"`
}

type Invoice struct {
	ID        string `json:"id"`
	OrderID   string `json:"orderId"`
	AmountLKR int    `json:"amountLkr"`
	Paid      bool   `json:"paid"`
}

type Shipment struct {
	OrderID  string `json:"orderId"`
	Carrier  string `json:"carrier"`
	Tracking string `json:"tracking"`
	EtaDays  int    `json:"etaDays"`
	Delayed  bool   `json:"delayed"`
}

type Forecast struct {
	City      string `json:"city"`
	Condition string `json:"condition" jsonschema:"sunny, cloudy or rain"`
	TempC     int    `json:"tempC"`
	WindKmh   int    `json:"windKmh"`
}

// Data is the whole dataset.
type Data struct {
	Products  []Product
	Stock     []Stock
	Customers []Customer
	Orders    []Order
	Invoices  []Invoice
	Shipments []Shipment
}

// New builds the dataset. Every value follows from its index, so it is the same on every run.
func New() *Data {
	d := &Data{}
	for i := 1; i <= 20; i++ {
		cat := Categories[(i-1)%len(Categories)]
		d.Products = append(d.Products, Product{SKU: fmt.Sprintf("SKU-%03d", i), Name: fmt.Sprintf("%s model %d", cat, i),
			Category: cat, PriceLKR: categoryBase[cat] + i*categoryStep[cat], ReorderLevel: 5 + (i%4)*5})
		for j, w := range Warehouses {
			d.Stock = append(d.Stock, Stock{SKU: fmt.Sprintf("SKU-%03d", i), Warehouse: w, Qty: (i*7 + j*11) % 23})
		}
	}
	for k, n := range customerNames {
		d.Customers = append(d.Customers, Customer{ID: fmt.Sprintf("C-%02d", k+1), Name: n, City: Warehouses[k%4], Tier: Tiers[k%3]})
	}
	statuses := []string{"pending", "shipped", "delivered", "shipped", "delivered", "cancelled"}
	for n := 0; n < 40; n++ {
		o := Order{ID: fmt.Sprintf("O-%d", 1001+n), CustomerID: fmt.Sprintf("C-%02d", (n*5)%12+1), SKU: fmt.Sprintf("SKU-%03d", (n*3)%20+1),
			Qty: n%5 + 1, Status: statuses[n%6], Warehouse: Warehouses[(n*3)%4], Date: fmt.Sprintf("2026-11-%02d", n%28+1)}
		d.Orders = append(d.Orders, o)
		if o.Status != "cancelled" {
			p, _ := d.Product(o.SKU)
			d.Invoices = append(d.Invoices, Invoice{ID: fmt.Sprintf("INV-%d", 1001+n), OrderID: o.ID, AmountLKR: o.Qty * p.PriceLKR, Paid: n%3 != 0})
		}
		if o.Status == "shipped" || o.Status == "delivered" {
			d.Shipments = append(d.Shipments, Shipment{OrderID: o.ID, Carrier: Carriers[n%3], Tracking: fmt.Sprintf("TRK%05d", 40000+n*37),
				EtaDays: 1 + n%4, Delayed: o.Status == "shipped" && (n%7 == 1 || n%7 == 3)})
		}
	}
	return d
}

func (d *Data) Product(sku string) (Product, bool) {
	for _, p := range d.Products {
		if p.SKU == sku {
			return p, true
		}
	}
	return Product{}, false
}

func (d *Data) Customer(id string) (Customer, bool) {
	for _, c := range d.Customers {
		if c.ID == id {
			return c, true
		}
	}
	return Customer{}, false
}

func (d *Data) Order(id string) (Order, bool) {
	for _, o := range d.Orders {
		if o.ID == id {
			return o, true
		}
	}
	return Order{}, false
}

func (d *Data) StockOf(sku, warehouse string) (int, bool) {
	for _, s := range d.Stock {
		if s.SKU == sku && s.Warehouse == warehouse {
			return s.Qty, true
		}
	}
	return 0, false
}

func (d *Data) InvoiceFor(orderID string) (Invoice, bool) {
	for _, i := range d.Invoices {
		if i.OrderID == orderID {
			return i, true
		}
	}
	return Invoice{}, false
}

func (d *Data) ShipmentFor(orderID string) (Shipment, bool) {
	for _, s := range d.Shipments {
		if s.OrderID == orderID {
			return s, true
		}
	}
	return Shipment{}, false
}

func warehouseIndex(city string) int {
	for i, w := range Warehouses {
		if w == city {
			return i
		}
	}
	return -1
}

// CarrierRate is a carrier's price to deliver to a city.
func CarrierRate(carrier, city string) (int, bool) {
	base, ok := carrierBase[carrier]
	i := warehouseIndex(city)
	if !ok || i < 0 {
		return 0, false
	}
	return base + 150*i, true
}
