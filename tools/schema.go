package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	monty "github.com/adrianliechti/go-monty"
)

// Schema describes a tool the way MCP and function-calling APIs do, for
// tools that are not Go functions with a typed signature.
type Schema struct {
	// Name is made a valid Python identifier; read the result from Tool.Name.
	Name        string
	Description string
	// Input is the JSON Schema of the arguments object. Its properties
	// become keyword-only parameters, required ones first.
	Input map[string]any
	// Output is the JSON Schema of the result; nil means Any.
	Output map[string]any
	// Async runs calls concurrently; see Registry.AddAsync.
	Async bool
}

// SchemaFunc runs a schema tool. args holds JSON-compatible values under
// the schema's property names; optional parameters passed as None are left
// out.
type SchemaFunc func(ctx context.Context, args map[string]any) (any, error)

// schemaTool is the schema-derived half of a Tool.
type schemaTool struct {
	params  []schemaParam
	returns string
	fn      SchemaFunc
}

type schemaParam struct {
	// name is the Python parameter, key the schema property it stands for.
	name, key string
	typ       string
	doc       string
	required  bool
}

// schemaDict is a TypedDict derived from an object schema.
type schemaDict struct {
	name string
	body string
}

// maxSchemaDepth bounds how deep object schemas become TypedDicts.
const maxSchemaDepth = 4

// maxLiteral bounds the enum values spelled out as a Literal.
const maxLiteral = 24

// maxFieldComment bounds the description shown beside a TypedDict field.
const maxFieldComment = 120

// AddSchema registers a tool described by JSON Schema. The checker and the
// model see its properties as keyword-only parameters with the types the
// schema declares.
func (r *Registry) AddSchema(s Schema, fn SchemaFunc) *Tool {
	name := r.uniqueName(Identifier(s.Name))
	t := &Tool{Name: name, Description: s.Description, Async: s.Async}
	st := &schemaTool{fn: fn, returns: "Any"}
	t.schema = st

	props, _ := s.Input["properties"].(map[string]any)
	required := stringSet(s.Input["required"])
	used := map[string]bool{}
	for _, key := range sortedKeys(props) {
		prop, _ := props[key].(map[string]any)
		p := schemaParam{key: key, required: required[key]}
		p.name = Identifier(key)
		for used[p.name] {
			p.name += "_"
		}
		used[p.name] = true
		p.typ = r.schemaType(prop, s.Input, camel(name)+camel(key), 1, map[string]bool{})
		p.doc, _ = prop["description"].(string)
		st.params = append(st.params, p)
		t.Params = append(t.Params, p.name)
	}
	sort.SliceStable(st.params, func(i, j int) bool { return st.params[i].required && !st.params[j].required })
	if s.Output != nil {
		st.returns = r.schemaType(s.Output, s.Output, camel(name)+"Result", 1, map[string]bool{})
	}

	r.order = append(r.order, name)
	r.tools[name] = t
	return t
}

// uniqueName makes name distinct from the registered tools.
func (r *Registry) uniqueName(name string) string {
	base := name
	for i := 2; r.tools[name] != nil; i++ {
		name = fmt.Sprintf("%s_%d", base, i)
	}
	return name
}

// Identifier turns s into a valid Python identifier: other characters
// become underscores, and keywords get a trailing underscore. Soft keywords
// such as type and match are valid names and stay as they are.
func Identifier(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r == '_' || unicode.IsLetter(r) && r < unicode.MaxASCII:
			b.WriteRune(r)
		case unicode.IsDigit(r) && r < unicode.MaxASCII:
			if i == 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "_"
	}
	if pythonKeywords[out] {
		out += "_"
	}
	return out
}

var pythonKeywords = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range strings.Fields(`False None True and as assert async await break class continue def del elif
		else except finally for from global if import in is lambda nonlocal not or pass raise return try while
		with yield`) {
		m[k] = true
	}
	return m
}()

func isIdentifier(s string) bool { return s != "" && Identifier(s) == s }

func camel(s string) string {
	var b strings.Builder
	upper := true
	for _, r := range s {
		if r == '_' || r == '-' || r == ' ' || r == '.' {
			upper = true
			continue
		}
		if upper {
			r = unicode.ToUpper(r)
			upper = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	switch list := v.(type) {
	case []any:
		for _, item := range list {
			if s, ok := item.(string); ok {
				out[s] = true
			}
		}
	case []string:
		for _, s := range list {
			out[s] = true
		}
	}
	return out
}

func schemaList(v any) []map[string]any {
	list, _ := v.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// union joins annotations, dropping duplicates and collapsing to Any.
func union(types ...string) string {
	var out []string
	none := false
	for _, t := range types {
		for _, part := range strings.Split(t, " | ") {
			switch {
			case part == "Any":
				return "Any"
			case part == "None":
				none = true
			case !slices.Contains(out, part):
				out = append(out, part)
			}
		}
	}
	if none {
		out = append(out, "None")
	}
	if len(out) == 0 {
		return "Any"
	}
	return strings.Join(out, " | ")
}

// literal renders enum values as a Literal, or "" when they are not all
// strings, integers or booleans.
func literal(values []any) string {
	if len(values) == 0 || len(values) > maxLiteral {
		return ""
	}
	parts := make([]string, 0, len(values))
	null := false
	for _, v := range values {
		switch x := v.(type) {
		case nil:
			null = true
		case string:
			parts = append(parts, pyString(x))
		case bool:
			if x {
				parts = append(parts, "True")
			} else {
				parts = append(parts, "False")
			}
		case float64:
			if x != float64(int64(x)) {
				return ""
			}
			parts = append(parts, strconv.FormatInt(int64(x), 10))
		case int:
			parts = append(parts, strconv.Itoa(x))
		case int64:
			parts = append(parts, strconv.FormatInt(x, 10))
		default:
			return ""
		}
	}
	if len(parts) == 0 {
		return "None"
	}
	out := "Literal[" + strings.Join(parts, ", ") + "]"
	if null {
		out += " | None"
	}
	return out
}

// pyString renders s as a single-quoted Python string literal.
func pyString(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch {
		case r == '\'' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// schemaType renders the annotation for a JSON Schema. root resolves local
// $refs, hint names the TypedDict an object becomes, and refs holds the
// $refs being expanded, so a recursive schema ends in Any.
func (r *Registry) schemaType(s, root map[string]any, hint string, depth int, refs map[string]bool) string {
	if s == nil {
		return "Any"
	}
	if ref, ok := s["$ref"].(string); ok {
		target, name := resolveRef(root, ref)
		if target == nil || refs[ref] {
			return "Any"
		}
		refs[ref] = true
		defer delete(refs, ref)
		return r.schemaType(target, root, camel(name), depth, refs)
	}
	if values, ok := s["enum"].([]any); ok {
		if lit := literal(values); lit != "" {
			return lit
		}
	}
	if v, ok := s["const"]; ok {
		if lit := literal([]any{v}); lit != "" {
			return lit
		}
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if alts := schemaList(s[key]); len(alts) > 0 {
			types := make([]string, len(alts))
			for i, alt := range alts {
				types[i] = r.schemaType(alt, root, fmt.Sprintf("%s%d", hint, i+1), depth, refs)
			}
			return union(types...)
		}
	}
	if all := schemaList(s["allOf"]); len(all) == 1 {
		return r.schemaType(all[0], root, hint, depth, refs)
	}

	var kinds []string
	switch t := s["type"].(type) {
	case string:
		kinds = []string{t}
	case []any:
		for _, item := range t {
			if k, ok := item.(string); ok {
				kinds = append(kinds, k)
			}
		}
	}
	if len(kinds) == 0 {
		switch {
		case s["properties"] != nil:
			kinds = []string{"object"}
		case s["items"] != nil:
			kinds = []string{"array"}
		default:
			return "Any"
		}
	}
	types := make([]string, len(kinds))
	for i, kind := range kinds {
		switch kind {
		case "string":
			types[i] = "str"
		case "integer":
			types[i] = "int"
		case "number":
			types[i] = "float"
		case "boolean":
			types[i] = "bool"
		case "null":
			types[i] = "None"
		case "array":
			items, _ := s["items"].(map[string]any)
			types[i] = "list[" + r.schemaType(items, root, hint+"Item", depth, refs) + "]"
		case "object":
			types[i] = r.objectType(s, root, hint, depth, refs)
		default:
			types[i] = "Any"
		}
	}
	return union(types...)
}

func (r *Registry) objectType(s, root map[string]any, hint string, depth int, refs map[string]bool) string {
	props, _ := s["properties"].(map[string]any)
	keys := sortedKeys(props)
	if len(keys) == 0 || depth > maxSchemaDepth || !allIdentifiers(keys) {
		if extra, ok := s["additionalProperties"].(map[string]any); ok && len(keys) == 0 {
			return "dict[str, " + r.schemaType(extra, root, hint+"Value", depth, refs) + "]"
		}
		return "dict[str, Any]"
	}
	required := stringSet(s["required"])
	var body strings.Builder
	for _, key := range keys {
		prop, _ := props[key].(map[string]any)
		typ := r.schemaType(prop, root, hint+camel(key), depth+1, refs)
		if !required[key] {
			typ = "NotRequired[" + typ + "]"
		}
		// A field's description is often the only place its conventions are
		// stated ("0 on the last page"), so it travels as a comment.
		comment := ""
		if doc, _ := prop["description"].(string); doc != "" {
			doc = strings.Join(strings.Fields(doc), " ")
			if len(doc) > maxFieldComment {
				doc = strings.ToValidUTF8(doc[:maxFieldComment], "") + "…"
			}
			comment = "  # " + doc
		}
		fmt.Fprintf(&body, "    %s: %s%s\n", key, typ, comment)
	}
	return r.schemaDict(hint, body.String())
}

func allIdentifiers(keys []string) bool {
	for _, k := range keys {
		if !isIdentifier(k) {
			return false
		}
	}
	return true
}

// schemaDict registers a TypedDict body, sharing one name between equal
// bodies.
func (r *Registry) schemaDict(hint, body string) string {
	for _, d := range r.schemaDicts {
		if d.body == body {
			return d.name
		}
	}
	name := hint
	if !isIdentifier(name) {
		name = "Record"
	}
	base := name
	for i := 2; r.names[name]; i++ {
		name = fmt.Sprintf("%s%d", base, i)
	}
	r.names[name] = true
	r.schemaDicts = append(r.schemaDicts, schemaDict{name: name, body: body})
	return name
}

// resolveRef follows a local "#/$defs/Name" or "#/definitions/Name" pointer.
func resolveRef(root map[string]any, ref string) (map[string]any, string) {
	for _, prefix := range []string{"#/$defs/", "#/definitions/"} {
		if name, ok := strings.CutPrefix(ref, prefix); ok {
			defs, _ := root[strings.Trim(prefix, "#/")].(map[string]any)
			target, _ := defs[name].(map[string]any)
			return target, name
		}
	}
	return nil, ""
}

// signature renders the tool's parameter list and return annotation.
func (st *schemaTool) signature() string {
	var b strings.Builder
	b.WriteByte('(')
	for i, p := range st.params {
		if i == 0 {
			b.WriteString("*, ")
		} else {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s: ", p.name)
		if p.required {
			b.WriteString(p.typ)
		} else {
			b.WriteString(union(p.typ, "None") + " = None")
		}
	}
	b.WriteString(") -> " + st.returns)
	return b.String()
}

// docstring is the tool's description followed by its documented parameters.
func (st *schemaTool) docstring(description string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(description))
	first := true
	for _, p := range st.params {
		doc := strings.Join(strings.Fields(p.doc), " ")
		if doc == "" {
			continue
		}
		if first && b.Len() > 0 {
			b.WriteString("\n")
		}
		first = false
		fmt.Fprintf(&b, "\n    %s: %s", p.name, doc)
	}
	return b.String()
}

// call binds the sandbox call's keyword arguments and runs the tool.
func (st *schemaTool) call(ctx context.Context, name string, call *monty.Call) (any, error) {
	if len(call.Args) > 0 {
		names := make([]string, len(st.params))
		for i, p := range st.params {
			names[i] = p.name + "=..."
		}
		return nil, monty.Raise("TypeError", "%s() takes keyword arguments only: %s(%s)", name, name, strings.Join(names, ", "))
	}
	args := map[string]any{}
	seen := map[string]bool{}
	for _, p := range st.params {
		v, ok := call.Kwargs[p.name]
		if !ok || (v == nil && !p.required) {
			if p.required {
				return nil, monty.Raise("TypeError", "%s() missing required keyword argument '%s'", name, p.name)
			}
			seen[p.name] = ok
			continue
		}
		seen[p.name] = true
		value, err := jsonValue(v)
		if err != nil {
			return nil, monty.Raise("TypeError", "%s() argument '%s': %v", name, p.name, err)
		}
		args[p.key] = value
	}
	for k := range call.Kwargs {
		if !seen[k] {
			return nil, monty.Raise("TypeError", "%s() got an unexpected keyword argument '%s'", name, k)
		}
	}
	return st.fn(ctx, args)
}

// jsonValue converts a sandbox value into the shape encoding/json decodes
// to, with integers kept as int64.
func jsonValue(v any) (any, error) {
	switch x := v.(type) {
	case nil, bool, int64, float64, string:
		return x, nil
	case *big.Int:
		return json.Number(x.String()), nil
	case monty.Decimal:
		return json.Number(string(x)), nil
	case monty.Path:
		return string(x), nil
	case time.Time:
		return x.Format(time.RFC3339Nano), nil
	case monty.Date:
		return fmt.Sprintf("%04d-%02d-%02d", x.Year, int(x.Month), x.Day), nil
	case []any:
		return jsonList(x)
	case monty.Tuple:
		return jsonList(x)
	case monty.Set:
		return jsonList(x)
	case monty.FrozenSet:
		return jsonList(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			value, err := jsonValue(item)
			if err != nil {
				return nil, fmt.Errorf("key %q: %w", k, err)
			}
			out[k] = value
		}
		return out, nil
	case monty.Dict:
		return nil, fmt.Errorf("dict keys must be strings")
	}
	return nil, fmt.Errorf("%s is not JSON-compatible", pyValueName(v))
}

func jsonList(items []any) (any, error) {
	out := make([]any, len(items))
	for i, item := range items {
		value, err := jsonValue(item)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		out[i] = value
	}
	return out, nil
}
