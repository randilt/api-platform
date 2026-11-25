// Package typegen turns MCP tool definitions into the TypeScript declarations that Code Mode's
// search tool returns, and gives every tool a JavaScript-safe method name on codemode.<server>.
package typegen

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Tool is one tool as Code Mode presents it.
type Tool struct {
	Server      string // source name; config validation keeps it a JS identifier
	Name        string // the tool's name on its MCP server
	Method      string // the method name on codemode.<Server>; set by AssignMethods
	Description string
	Destructive bool
	Input       map[string]any
	Output      map[string]any
}

var (
	nonIdent  = regexp.MustCompile(`[^A-Za-z0-9_$]`)
	identLike = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
)

// Ident maps any string onto a JavaScript identifier.
func Ident(s string) string {
	s = nonIdent.ReplaceAllString(s, "_")
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		s = "_" + s
	}
	return s
}

// AssignMethods sets Method on every tool so that it is a JS identifier unique within its
// server. A name that already is an identifier keeps it, so a script can call a tool by its real
// name; names that sanitise onto a taken identifier get _2, _3, ... in tool-name order, so the
// result does not depend on the order the server listed its tools in.
func AssignMethods(tools []Tool) {
	idx := make([]int, len(tools))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ta, tb := tools[idx[a]], tools[idx[b]]
		if ta.Server != tb.Server {
			return ta.Server < tb.Server
		}
		return ta.Name < tb.Name
	})
	taken := map[string]bool{}
	for _, exact := range []bool{true, false} {
		for _, i := range idx {
			base := Ident(tools[i].Name)
			if (base == tools[i].Name) != exact {
				continue
			}
			m := base
			for n := 2; taken[tools[i].Server+"."+m]; n++ {
				m = fmt.Sprintf("%s_%d", base, n)
			}
			taken[tools[i].Server+"."+m] = true
			tools[i].Method = m
		}
	}
}

// Declarations renders the tools as one `declare const codemode` block, servers and methods
// sorted so the output is stable.
func Declarations(tools []Tool) string {
	byServer := map[string][]Tool{}
	var servers []string
	for _, t := range tools {
		if _, ok := byServer[t.Server]; !ok {
			servers = append(servers, t.Server)
		}
		byServer[t.Server] = append(byServer[t.Server], t)
	}
	sort.Strings(servers)
	var b strings.Builder
	b.WriteString("declare const codemode: {\n")
	for _, s := range servers {
		ts := byServer[s]
		sort.Slice(ts, func(i, j int) bool { return ts[i].Method < ts[j].Method })
		fmt.Fprintf(&b, "  %s: {\n", propKey(s))
		for _, t := range ts {
			b.WriteString(declare(t))
		}
		b.WriteString("  };\n")
	}
	b.WriteString("};")
	return b.String()
}

func declare(t Tool) string {
	desc := t.Description
	if t.Destructive {
		desc = strings.TrimSpace(desc + " (destructive)")
	}
	var b strings.Builder
	if c := comment(desc); c != "" {
		fmt.Fprintf(&b, "    %s\n", c)
	}
	in := TypeOf(t.Input)
	if t.Input == nil {
		in = "Record<string, unknown>"
	}
	optional := ""
	if req, _ := t.Input["required"].([]any); len(req) == 0 {
		optional = "?"
	}
	out := "unknown"
	if t.Output != nil {
		out = TypeOf(t.Output)
	}
	fmt.Fprintf(&b, "    %s(args%s: %s): Promise<%s>;\n", propKey(t.Method), optional, in, out)
	return b.String()
}

// comment renders a one-line JSDoc comment. Descriptions come from upstream servers, so a "*/"
// in one must not end the comment early.
func comment(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	return "/** " + strings.ReplaceAll(s, "*/", "*\\/") + " */"
}

func propKey(k string) string {
	if identLike.MatchString(k) {
		return k
	}
	b, _ := json.Marshal(k)
	return string(b)
}

// maxDepth stops runaway nesting; a deeper schema renders as unknown.
const maxDepth = 32

// TypeOf renders a JSON Schema (2020-12, with draft-07 "definitions" accepted) as a TypeScript
// type. Local $refs are inlined; a reference cycle renders as unknown.
func TypeOf(schema map[string]any) string {
	g := &gen{root: schema, inRef: map[string]bool{}}
	return g.ts(schema, 0)
}

type gen struct {
	root  map[string]any
	inRef map[string]bool
}

func (g *gen) ts(v any, depth int) string {
	if depth > maxDepth {
		return "unknown"
	}
	switch s := v.(type) {
	case bool:
		if s {
			return "unknown"
		}
		return "never"
	case map[string]any:
		return g.schema(s, depth)
	}
	return "unknown"
}

func (g *gen) schema(s map[string]any, depth int) string {
	if ref, ok := s["$ref"].(string); ok {
		target, ok := g.resolve(ref)
		if !ok || g.inRef[ref] {
			return "unknown"
		}
		g.inRef[ref] = true
		defer delete(g.inRef, ref)
		return g.ts(target, depth+1)
	}
	if c, ok := s["const"]; ok {
		return literal(c)
	}
	if e, ok := s["enum"].([]any); ok && len(e) > 0 {
		parts := make([]string, len(e))
		for i, x := range e {
			parts[i] = literal(x)
		}
		return strings.Join(parts, " | ")
	}
	for _, k := range []string{"anyOf", "oneOf"} {
		if alts, ok := s[k].([]any); ok && len(alts) > 0 {
			return g.join(alts, " | ", depth)
		}
	}
	if all, ok := s["allOf"].([]any); ok && len(all) > 0 {
		return g.join(all, " & ", depth)
	}
	if arr, ok := s["type"].([]any); ok {
		parts := make([]string, 0, len(arr))
		for _, t := range arr {
			c := make(map[string]any, len(s))
			for k, v := range s {
				c[k] = v
			}
			c["type"] = t
			parts = append(parts, g.schema(c, depth+1))
		}
		return strings.Join(parts, " | ")
	}
	t, _ := s["type"].(string)
	if t == "" {
		switch {
		case s["properties"] != nil || s["additionalProperties"] != nil:
			t = "object"
		case s["items"] != nil || s["prefixItems"] != nil:
			t = "array"
		}
	}
	switch t {
	case "string":
		return "string"
	case "integer", "number":
		return "number"
	case "boolean":
		return "boolean"
	case "null":
		return "null"
	case "array":
		return g.array(s, depth)
	case "object":
		return g.object(s, depth)
	}
	return "unknown"
}

func (g *gen) join(alts []any, sep string, depth int) string {
	parts := make([]string, len(alts))
	for i, a := range alts {
		p := g.ts(a, depth+1)
		if strings.Contains(p, " | ") || strings.Contains(p, " & ") {
			p = "(" + p + ")"
		}
		parts[i] = p
	}
	return strings.Join(parts, sep)
}

func (g *gen) array(s map[string]any, depth int) string {
	if pre, ok := s["prefixItems"].([]any); ok && len(pre) > 0 {
		parts := make([]string, len(pre))
		for i, p := range pre {
			parts[i] = g.ts(p, depth+1)
		}
		if rest, ok := s["items"]; ok && rest != false {
			parts = append(parts, "...Array<"+g.ts(rest, depth+1)+">")
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	if it, ok := s["items"]; ok {
		return "Array<" + g.ts(it, depth+1) + ">"
	}
	return "Array<unknown>"
}

func (g *gen) object(s map[string]any, depth int) string {
	props, _ := s["properties"].(map[string]any)
	extra, hasExtra := s["additionalProperties"]
	if len(props) == 0 {
		if m, ok := extra.(map[string]any); ok {
			return "Record<string, " + g.ts(m, depth+1) + ">"
		}
		if extra == false {
			return "{}"
		}
		return "Record<string, unknown>"
	}
	req := map[string]bool{}
	if r, ok := s["required"].([]any); ok {
		for _, x := range r {
			if k, ok := x.(string); ok {
				req[k] = true
			}
		}
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{ ")
	for _, k := range keys {
		if pm, ok := props[k].(map[string]any); ok {
			if c := comment(str(pm["description"])); c != "" {
				b.WriteString(c + " ")
			}
		}
		opt := "?"
		if req[k] {
			opt = ""
		}
		fmt.Fprintf(&b, "%s%s: %s; ", propKey(k), opt, g.ts(props[k], depth+1))
	}
	if m, ok := extra.(map[string]any); ok && hasExtra {
		fmt.Fprintf(&b, "[key: string]: %s; ", g.ts(m, depth+1))
	}
	b.WriteString("}")
	return b.String()
}

// resolve follows a local JSON pointer ("#", "#/$defs/X", "#/definitions/X", ...).
func (g *gen) resolve(ref string) (any, bool) {
	if ref == "#" {
		return g.root, true
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, false // remote refs are never fetched
	}
	var cur any = g.root
	for _, part := range strings.Split(ref[2:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func literal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "unknown"
	}
	return string(b)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
