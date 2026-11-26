package world

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Kind says how an answer is compared.
type Kind string

const (
	Number Kind = "number" // compared as a number; commas, units and currency words ignored
	List   Kind = "list"   // comma-separated items, compared as a set, case-insensitive
	Text   Kind = "text"   // compared case-insensitively after collapsing spaces
)

// Task is one benchmark question. Expected computes the answer from the data and fails on a
// tie or an empty result, so a task can never ask for something the data does not settle.
type Task struct {
	ID       string
	Prompt   string
	Kind     Kind
	Expected func(*Data) (string, error)
}

var errAmbiguous = errors.New("the data does not settle this task")

// maxBy returns the key with the largest value, failing on a tie or no positive value.
func maxBy(m map[string]int) (string, error) {
	best, bestV, tie := "", 0, false
	for k, v := range m {
		switch {
		case v > bestV:
			best, bestV, tie = k, v, false
		case v == bestV && v > 0:
			tie = true
		}
	}
	if best == "" || tie {
		return "", errAmbiguous
	}
	return best, nil
}

func positive(n int) (string, error) {
	if n <= 0 {
		return "", errAmbiguous
	}
	return strconv.Itoa(n), nil
}

// Tasks are the ten questions. Each needs data from more than one tool, most from more than
// one server.
var Tasks = []Task{
	{ID: "T01", Kind: Text,
		Prompt: "Which warehouse holds the most units of SKU-007, and how many units does it hold? Answer as `<warehouse> <units>`.",
		Expected: func(d *Data) (string, error) {
			m := map[string]int{}
			for _, s := range d.Stock {
				if s.SKU == "SKU-007" {
					m[s.Warehouse] = s.Qty
				}
			}
			w, err := maxBy(m)
			return fmt.Sprintf("%s %d", w, m[w]), err
		}},
	{ID: "T02", Kind: Number,
		Prompt: "What is the total value in LKR of all pending orders, where an order's value is its quantity times the product's unit price?",
		Expected: func(d *Data) (string, error) {
			t := 0
			for _, o := range d.Orders {
				if o.Status == "pending" {
					p, _ := d.Product(o.SKU)
					t += o.Qty * p.PriceLKR
				}
			}
			return positive(t)
		}},
	{ID: "T03", Kind: Number,
		Prompt: "How much does Ayesha Jayasinghe owe in unpaid invoices, in LKR?",
		Expected: func(d *Data) (string, error) {
			t := 0
			for _, i := range d.Invoices {
				o, _ := d.Order(i.OrderID)
				if c, _ := d.Customer(o.CustomerID); c.Name == "Ayesha Jayasinghe" && !i.Paid {
					t += i.AmountLKR
				}
			}
			return positive(t)
		}},
	{ID: "T04", Kind: List,
		Prompt: "Which customers have at least one delayed shipment? Give their customer IDs.",
		Expected: func(d *Data) (string, error) {
			set := map[string]bool{}
			for _, s := range d.Shipments {
				if s.Delayed {
					o, _ := d.Order(s.OrderID)
					set[o.CustomerID] = true
				}
			}
			if len(set) == 0 {
				return "", errAmbiguous
			}
			ids := make([]string, 0, len(set))
			for id := range set {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			return strings.Join(ids, ", "), nil
		}},
	{ID: "T05", Kind: Text,
		Prompt: "Which warehouse has the most products at or below their reorder level? Give the warehouse name.",
		Expected: func(d *Data) (string, error) {
			m := map[string]int{}
			for _, s := range d.Stock {
				if p, _ := d.Product(s.SKU); s.Qty <= p.ReorderLevel {
					m[s.Warehouse]++
				}
			}
			return maxBy(m)
		}},
	{ID: "T06", Kind: Text,
		Prompt: "Order O-1002 is delivered by the carrier on its shipment to the customer's city. What does that carrier charge to deliver to that city, in LKR, and is it raining there today? Answer as `<rate> <yes|no>`.",
		Expected: func(d *Data) (string, error) {
			o, _ := d.Order("O-1002")
			s, ok := d.ShipmentFor(o.ID)
			if !ok {
				return "", errAmbiguous
			}
			c, _ := d.Customer(o.CustomerID)
			r, _ := CarrierRate(s.Carrier, c.City)
			rain := "no"
			if weather[c.City].Condition == "rain" {
				rain = "yes"
			}
			return fmt.Sprintf("%d %s", r, rain), nil
		}},
	{ID: "T07", Kind: Number,
		Prompt: "How many monitor units in total have customers based in Kandy ordered, counting every order that is not cancelled?",
		Expected: func(d *Data) (string, error) {
			t := 0
			for _, o := range d.Orders {
				p, _ := d.Product(o.SKU)
				c, _ := d.Customer(o.CustomerID)
				if o.Status != "cancelled" && p.Category == "monitor" && c.City == "Kandy" {
					t += o.Qty
				}
			}
			return positive(t)
		}},
	{ID: "T08", Kind: Text,
		Prompt: "Which carrier handles the most shipments, counting both delivered and in-transit ones? Give the carrier name.",
		Expected: func(d *Data) (string, error) {
			m := map[string]int{}
			for _, s := range d.Shipments {
				m[s.Carrier]++
			}
			return maxBy(m)
		}},
	{ID: "T09", Kind: Text,
		Prompt: "Among silver-tier customers, who has the highest total value of orders that are not cancelled (quantity times unit price)? Give the customer ID.",
		Expected: func(d *Data) (string, error) {
			m := map[string]int{}
			for _, o := range d.Orders {
				c, _ := d.Customer(o.CustomerID)
				if o.Status != "cancelled" && c.Tier == "silver" {
					p, _ := d.Product(o.SKU)
					m[c.ID] += o.Qty * p.PriceLKR
				}
			}
			return maxBy(m)
		}},
	{ID: "T10", Kind: Number,
		Prompt: "How many pending orders ship from a warehouse that holds fewer than 10 units of the ordered product?",
		Expected: func(d *Data) (string, error) {
			n := 0
			for _, o := range d.Orders {
				if q, _ := d.StockOf(o.SKU, o.Warehouse); o.Status == "pending" && q < 10 {
					n++
				}
			}
			return positive(n)
		}},
}

var (
	answerLine = regexp.MustCompile(`(?im)^\s*\**\s*ANSWER\s*\**\s*:\s*\**\s*(.+?)\s*\**\s*$`)
	firstNum   = regexp.MustCompile(`-?\d[\d,]*(?:\.\d+)?`)
)

// ExtractAnswer returns the value on the last "ANSWER:" line of a reply.
func ExtractAnswer(reply string) (string, bool) {
	m := answerLine.FindAllStringSubmatch(reply, -1)
	if len(m) == 0 {
		return "", false
	}
	return strings.Trim(m[len(m)-1][1], "`'\" "), true
}

// Grade reports whether got matches want for this kind of task.
func Grade(kind Kind, got, want string) bool {
	switch kind {
	case Number:
		g, err1 := strconv.ParseFloat(strings.ReplaceAll(firstNum.FindString(got), ",", ""), 64)
		w, err2 := strconv.ParseFloat(want, 64)
		return err1 == nil && err2 == nil && g == w
	case List:
		norm := func(s string) string {
			parts := strings.FieldsFunc(strings.ToUpper(s), func(r rune) bool { return r == ',' || r == ' ' || r == ';' })
			sort.Strings(parts)
			return strings.Join(parts, ",")
		}
		return norm(got) == norm(want)
	default:
		return strings.EqualFold(strings.Join(strings.Fields(got), " "), strings.Join(strings.Fields(want), " "))
	}
}
