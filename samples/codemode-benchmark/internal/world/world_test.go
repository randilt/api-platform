package world

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestTasksAreSettled pins every task's answer: each must resolve without a tie or an empty
// result, and a change to the data must update this table on purpose.
func TestTasksAreSettled(t *testing.T) {
	want := map[string]string{
		"T01": "Kandy 14", "T02": "786000", "T03": "494800", "T04": "C-04, C-06, C-12", "T05": "Galle",
		"T06": "900 yes", "T07": "3", "T08": "IslandExpress", "T09": "C-05", "T10": "3",
	}
	d := New()
	if len(Tasks) != len(want) {
		t.Fatalf("%d tasks, want %d", len(Tasks), len(want))
	}
	for _, tk := range Tasks {
		got, err := tk.Expected(d)
		if err != nil || got != want[tk.ID] {
			t.Errorf("%s: got %q (%v), want %q", tk.ID, got, err, want[tk.ID])
		}
	}
}

func connect(t *testing.T, s *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := s.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	sess, err := c.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func TestServersExposeFortyEightTools(t *testing.T) {
	servers := Servers(New())
	total := 0
	for _, name := range ServerNames {
		res, err := connect(t, servers[name]).ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Tools) != 12 {
			t.Errorf("%s has %d tools, want 12", name, len(res.Tools))
		}
		for _, tl := range res.Tools {
			if tl.OutputSchema == nil || tl.Description == "" {
				t.Errorf("%s.%s needs a description and an output schema", name, tl.Name)
			}
		}
		total += len(res.Tools)
	}
	if total != 48 {
		t.Errorf("%d tools in all, want 48", total)
	}
}

func TestToolsAnswerFromTheData(t *testing.T) {
	servers := Servers(New())
	inv := connect(t, servers["inventory"])
	res, err := inv.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_stock", Arguments: map[string]any{"sku": "SKU-007", "warehouse": "kandy"}})
	if err != nil || res.IsError {
		t.Fatalf("get_stock: %v %+v", err, res)
	}
	if got := res.StructuredContent.(map[string]any)["qty"]; got != float64(14) {
		t.Errorf("SKU-007 in Kandy = %v, want 14", got)
	}
	res, err = inv.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_product", Arguments: map[string]any{"sku": "SKU-999"}})
	if err != nil || !res.IsError {
		t.Errorf("an unknown SKU should be a tool error: %v %+v", err, res)
	}
}

func TestExtractAnswer(t *testing.T) {
	cases := map[string]string{
		"Working...\nANSWER: 786000":                    "786000",
		"**ANSWER:** Kandy 14":                          "Kandy 14",
		"answer: `C-05`\n":                              "C-05",
		"ANSWER: 1\nchecked again\nANSWER: 2":           "2",
		"The final answer is below.\n\nANSWER: 900 yes": "900 yes",
	}
	for in, want := range cases {
		if got, ok := ExtractAnswer(in); !ok || got != want {
			t.Errorf("ExtractAnswer(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := ExtractAnswer("no answer here"); ok {
		t.Error("a reply without an ANSWER line has no answer")
	}
}

func TestGrade(t *testing.T) {
	cases := []struct {
		kind      Kind
		got, want string
		ok        bool
	}{
		{Number, "786,000 LKR", "786000", true},
		{Number, "Rs. 494800", "494800", true},
		{Number, "786001", "786000", false},
		{Number, "", "3", false},
		{List, "C-12, c-04;C-06", "C-04, C-06, C-12", true},
		{List, "C-04, C-06", "C-04, C-06, C-12", false},
		{Text, "kandy  14", "Kandy 14", true},
		{Text, "Kandy 13", "Kandy 14", false},
		{Text, "islandexpress", "IslandExpress", true},
	}
	for _, c := range cases {
		if got := Grade(c.kind, c.got, c.want); got != c.ok {
			t.Errorf("Grade(%s, %q, %q) = %v", c.kind, c.got, c.want, got)
		}
	}
}
