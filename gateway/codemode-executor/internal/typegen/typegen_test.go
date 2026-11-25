package typegen

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden .d.ts files")

// TestGolden renders every captured tools/list in testdata and compares it with its golden file.
// The inputs were captured from the testbench MCP service and the spike's test servers.
func TestGolden(t *testing.T) {
	files, err := filepath.Glob("testdata/*.tools.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no testdata: %v", err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".tools.json")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var listed []struct {
				Name         string         `json:"name"`
				Description  string         `json:"description"`
				InputSchema  map[string]any `json:"inputSchema"`
				OutputSchema map[string]any `json:"outputSchema"`
				Annotations  *struct {
					DestructiveHint *bool `json:"destructiveHint"`
				} `json:"annotations"`
			}
			if err := json.Unmarshal(raw, &listed); err != nil {
				t.Fatal(err)
			}
			server := strings.SplitN(name, "-", 2)[1]
			tools := make([]Tool, len(listed))
			for i, l := range listed {
				tools[i] = Tool{Server: server, Name: l.Name, Description: l.Description, Input: l.InputSchema, Output: l.OutputSchema,
					Destructive: l.Annotations != nil && l.Annotations.DestructiveHint != nil && *l.Annotations.DestructiveHint}
			}
			AssignMethods(tools)
			got := Declarations(tools) + "\n"
			golden := filepath.Join("testdata", name+".d.ts")
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if got != string(want) {
				t.Errorf("declarations differ from %s\n--- got\n%s\n--- want\n%s", golden, got, want)
			}
		})
	}
}

func TestTypeOf(t *testing.T) {
	cases := []struct {
		name, schema, want string
	}{
		{"enum", `{"enum":["a","b",1]}`, `"a" | "b" | 1`},
		{"const", `{"const":"fixed"}`, `"fixed"`},
		{"type array", `{"type":["string","null"]}`, `string | null`},
		{"anyOf with nested union", `{"anyOf":[{"type":"number"},{"enum":["x","y"]}]}`, `number | ("x" | "y")`},
		{"oneOf", `{"oneOf":[{"type":"string"},{"type":"boolean"}]}`, `string | boolean`},
		{"allOf", `{"allOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"type":"object","properties":{"b":{"type":"number"}}}]}`, `{ a: string; } & { b?: number; }`},
		{"$defs ref", `{"type":"object","properties":{"p":{"$ref":"#/$defs/Point"}},"$defs":{"Point":{"type":"object","properties":{"x":{"type":"number"}},"required":["x"]}}}`, `{ p?: { x: number; }; }`},
		{"definitions ref", `{"$ref":"#/definitions/Id","definitions":{"Id":{"type":"string"}}}`, `string`},
		{"recursive ref", `{"type":"object","properties":{"child":{"$ref":"#"}}}`, `{ child?: { child?: unknown; }; }`},
		{"remote ref", `{"$ref":"https://example.com/s.json"}`, `unknown`},
		{"tuple", `{"type":"array","prefixItems":[{"type":"string"},{"type":"number"}],"items":false}`, `[string, number]`},
		{"tuple with rest", `{"type":"array","prefixItems":[{"type":"string"}],"items":{"type":"boolean"}}`, `[string, ...Array<boolean>]`},
		{"map", `{"type":"object","additionalProperties":{"type":"integer"}}`, `Record<string, number>`},
		{"props plus map", `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":{"type":"number"}}`, `{ a?: string; [key: string]: number; }`},
		{"empty closed object", `{"type":"object","additionalProperties":false}`, `{}`},
		{"quoted key", `{"type":"object","properties":{"content-type":{"type":"string"}}}`, `{ "content-type"?: string; }`},
		{"comment escape", `{"type":"object","properties":{"a":{"type":"string","description":"evil */ x\n y"}}}`, `{ /** evil *\/ x y */ a?: string; }`},
		{"untyped with items", `{"items":{"type":"string"}}`, `Array<string>`},
		{"false schema", `{"type":"object","properties":{"a":false}}`, `{ a?: never; }`},
		{"no type", `{}`, `unknown`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s map[string]any
			if err := json.Unmarshal([]byte(c.schema), &s); err != nil {
				t.Fatal(err)
			}
			if got := TypeOf(s); got != c.want {
				t.Errorf("TypeOf(%s)\n got %s\nwant %s", c.schema, got, c.want)
			}
		})
	}
}

func TestTypeOfDepthLimit(t *testing.T) {
	s := map[string]any{"type": "string"}
	for i := 0; i < 100; i++ {
		s = map[string]any{"type": "array", "items": s}
	}
	if got := TypeOf(s); !strings.Contains(got, "unknown") {
		t.Errorf("deep schema should bottom out at unknown, got %.80s...", got)
	}
}

func TestAssignMethods(t *testing.T) {
	tools := []Tool{
		{Server: "s", Name: "get-item"},
		{Server: "s", Name: "get.item"},
		{Server: "s", Name: "get_item"},
		{Server: "s", Name: "9lives"},
		{Server: "other", Name: "get-item"},
	}
	AssignMethods(tools)
	want := []string{"get_item_2", "get_item_3", "get_item", "_9lives", "get_item"}
	for i, w := range want {
		if tools[i].Method != w {
			t.Errorf("tool %q on %q: method %q, want %q", tools[i].Name, tools[i].Server, tools[i].Method, w)
		}
	}
}

func TestDeclarationsOptionalArgs(t *testing.T) {
	tools := []Tool{
		{Server: "s", Name: "none"},
		{Server: "s", Name: "opt", Input: map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}},
		{Server: "s", Name: "req", Input: map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}, "required": []any{"a"}}},
	}
	AssignMethods(tools)
	got := Declarations(tools)
	for _, w := range []string{
		"none(args?: Record<string, unknown>): Promise<unknown>;",
		"opt(args?: { a?: string; }): Promise<unknown>;",
		"req(args: { a: string; }): Promise<unknown>;",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in\n%s", w, got)
		}
	}
}
