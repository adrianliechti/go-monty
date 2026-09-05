// Package tools turns ordinary Go functions into sandbox tools: arguments
// are bound and converted from Python values by reflection, and a Python
// stub is derived from each signature so the sandbox type checker (and the
// model, through Prompt) sees the exact contract.
//
//	reg := tools.New()
//	reg.Add("get_temperature", "Current temperature in °C.", func(city string) (float64, error) { ... }, "city")
//	reg.Add("send_email", "Sends an email.", sendEmail, "to", "subject", "body")
//
//	session, _ := rt.NewSession(ctx, monty.SessionOptions{TypeCheck: true, TypeCheckStubs: reg.Stubs()})
//	result, err := session.Run(ctx, code, reg.Options(monty.RunOptions{}))
//
// Supported parameter and result types: bool, all int and float kinds,
// string, []byte, slices, maps with string keys, structs (as TypedDicts,
// json tags honoured), pointers (optional, None when omitted), time.Time,
// time.Duration, any, and the monty value types. An optional leading
// context.Context parameter receives the run's context; an optional
// trailing error result raises in the sandbox.
package tools

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"time"

	monty "github.com/adrianliechti/go-monty"
)

// Tool is one registered function.
type Tool struct {
	Name        string
	Description string
	// Params names the Go function's parameters in order (context excluded).
	Params []string
	// Async runs the tool in its own goroutine; see monty.RunOptions.
	Async bool

	fn     reflect.Value
	ft     reflect.Type
	hasCtx bool
	// errOut reports whether the last result is an error.
	errOut bool
	// valueOut reports whether the function returns a value.
	valueOut bool
}

// Registry is an ordered set of tools sharing one stub namespace.
type Registry struct {
	tools map[string]*Tool
	order []string
	dicts map[reflect.Type]string
	names map[string]bool
}

// New creates an empty Registry.
func New() *Registry {
	return &Registry{tools: map[string]*Tool{}, dicts: map[reflect.Type]string{}, names: map[string]bool{}}
}

// Add registers fn under name. params names its parameters (the context
// parameter, if any, excluded); missing names default to arg0, arg1, ...
// Add panics when fn is not a function or uses an unsupported type, since
// that is a programming error.
func (r *Registry) Add(name, description string, fn any, params ...string) *Tool {
	t := r.add(name, description, fn, params)
	return t
}

// AddAsync is Add for a tool that should run concurrently with the sandbox
// (and with other async tools under asyncio.gather). Its stub is an
// `async def`, so sandbox code must await it.
func (r *Registry) AddAsync(name, description string, fn any, params ...string) *Tool {
	t := r.add(name, description, fn, params)
	t.Async = true
	return t
}

func (r *Registry) add(name, description string, fn any, params []string) *Tool {
	fv := reflect.ValueOf(fn)
	if fv.Kind() != reflect.Func {
		panic(fmt.Sprintf("tools: %s is not a function", name))
	}
	ft := fv.Type()
	t := &Tool{Name: name, Description: description, fn: fv, ft: ft}
	in := ft.NumIn()
	if in > 0 && ft.In(0) == reflect.TypeOf((*context.Context)(nil)).Elem() {
		t.hasCtx = true
	}
	n := in
	if t.hasCtx {
		n--
	}
	if ft.IsVariadic() {
		panic(fmt.Sprintf("tools: %s: variadic functions are not supported", name))
	}
	if len(params) > n {
		panic(fmt.Sprintf("tools: %s: %d parameter names for %d parameters", name, len(params), n))
	}
	t.Params = make([]string, n)
	for i := range t.Params {
		if i < len(params) && params[i] != "" {
			t.Params[i] = params[i]
		} else {
			t.Params[i] = fmt.Sprintf("arg%d", i)
		}
	}
	switch ft.NumOut() {
	case 0:
	case 1:
		if ft.Out(0) == errorType {
			t.errOut = true
		} else {
			t.valueOut = true
		}
	case 2:
		if ft.Out(1) != errorType {
			panic(fmt.Sprintf("tools: %s: second result must be error", name))
		}
		t.valueOut, t.errOut = true, true
	default:
		panic(fmt.Sprintf("tools: %s: too many results", name))
	}
	// Validate types now so mistakes surface at registration.
	for i := 0; i < n; i++ {
		r.pyType(t.paramType(i))
	}
	if t.valueOut {
		r.pyType(ft.Out(0))
	}
	if _, dup := r.tools[name]; !dup {
		r.order = append(r.order, name)
	}
	r.tools[name] = t
	return t
}

func (t *Tool) paramType(i int) reflect.Type {
	if t.hasCtx {
		i++
	}
	return t.ft.In(i)
}

var (
	errorType    = reflect.TypeOf((*error)(nil)).Elem()
	anyType      = reflect.TypeOf((*any)(nil)).Elem()
	timeType     = reflect.TypeOf(time.Time{})
	durationType = reflect.TypeOf(time.Duration(0))
	bigIntType   = reflect.TypeOf((*big.Int)(nil))
)

// Function adapts the tool to monty.Function.
func (t *Tool) Function() monty.Function {
	return func(ctx context.Context, call *monty.Call) (any, error) {
		args, err := t.bind(call)
		if err != nil {
			return nil, err
		}
		if t.hasCtx {
			args = append([]reflect.Value{reflect.ValueOf(ctx)}, args...)
		}
		out := t.fn.Call(args)
		if t.errOut {
			if e, _ := out[len(out)-1].Interface().(error); e != nil {
				return nil, e
			}
		}
		if t.valueOut {
			return out[0].Interface(), nil
		}
		return nil, nil
	}
}

// bind maps positional and keyword arguments onto the Go parameters.
func (t *Tool) bind(call *monty.Call) ([]reflect.Value, error) {
	n := len(t.Params)
	if len(call.Args) > n {
		return nil, monty.Raise("TypeError", "%s() takes %d positional arguments but %d were given", t.Name, n, len(call.Args))
	}
	for k := range call.Kwargs {
		if indexOf(t.Params, k) < 0 {
			return nil, monty.Raise("TypeError", "%s() got an unexpected keyword argument '%s'", t.Name, k)
		}
	}
	args := make([]reflect.Value, n)
	for i, name := range t.Params {
		pt := t.paramType(i)
		var v any
		have := false
		if i < len(call.Args) {
			v, have = call.Args[i], true
			if _, dup := call.Kwargs[name]; dup {
				return nil, monty.Raise("TypeError", "%s() got multiple values for argument '%s'", t.Name, name)
			}
		} else if kv, ok := call.Kwargs[name]; ok {
			v, have = kv, true
		}
		if !have {
			if pt.Kind() == reflect.Pointer || pt.Kind() == reflect.Interface || pt.Kind() == reflect.Slice || pt.Kind() == reflect.Map {
				args[i] = reflect.Zero(pt)
				continue
			}
			return nil, monty.Raise("TypeError", "%s() missing required argument '%s'", t.Name, name)
		}
		rv, err := convert(v, pt)
		if err != nil {
			return nil, monty.Raise("TypeError", "%s() argument '%s': %v", t.Name, name, err)
		}
		args[i] = rv
	}
	return args, nil
}

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

// convert turns a decoded sandbox value into a value of type t.
func convert(v any, t reflect.Type) (reflect.Value, error) {
	if v == nil {
		switch t.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map:
			return reflect.Zero(t), nil
		}
		return reflect.Value{}, fmt.Errorf("expected %s, got None", pyName(t))
	}
	rv := reflect.ValueOf(v)
	if t == anyType {
		return rv, nil
	}
	if rv.Type().AssignableTo(t) {
		return rv, nil
	}
	switch t.Kind() {
	case reflect.Pointer:
		inner, err := convert(v, t.Elem())
		if err != nil {
			return reflect.Value{}, err
		}
		p := reflect.New(t.Elem())
		p.Elem().Set(inner)
		return p, nil
	case reflect.Bool:
		if b, ok := v.(bool); ok {
			return reflect.ValueOf(b).Convert(t), nil
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		switch x := v.(type) {
		case int64:
			if reflect.Zero(t).OverflowInt(x) {
				return reflect.Value{}, fmt.Errorf("%d overflows %s", x, t)
			}
			if t == durationType {
				return reflect.ValueOf(time.Duration(x) * time.Second), nil
			}
			return reflect.ValueOf(x).Convert(t), nil
		case float64:
			if x == math.Trunc(x) && !reflect.Zero(t).OverflowInt(int64(x)) {
				return reflect.ValueOf(int64(x)).Convert(t), nil
			}
		case time.Duration:
			if t == durationType {
				return reflect.ValueOf(x), nil
			}
		case bool:
			if x {
				return reflect.ValueOf(int64(1)).Convert(t), nil
			}
			return reflect.ValueOf(int64(0)).Convert(t), nil
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if x, ok := v.(int64); ok && x >= 0 && !reflect.Zero(t).OverflowUint(uint64(x)) {
			return reflect.ValueOf(uint64(x)).Convert(t), nil
		}
		if x, ok := v.(*big.Int); ok && x.Sign() >= 0 && x.IsUint64() && !reflect.Zero(t).OverflowUint(x.Uint64()) {
			return reflect.ValueOf(x.Uint64()).Convert(t), nil
		}
	case reflect.Float32, reflect.Float64:
		switch x := v.(type) {
		case float64:
			return reflect.ValueOf(x).Convert(t), nil
		case int64:
			return reflect.ValueOf(float64(x)).Convert(t), nil
		case *big.Int:
			f, _ := new(big.Float).SetInt(x).Float64()
			return reflect.ValueOf(f).Convert(t), nil
		}
	case reflect.String:
		switch x := v.(type) {
		case string:
			return reflect.ValueOf(x).Convert(t), nil
		case monty.Path:
			return reflect.ValueOf(string(x)).Convert(t), nil
		}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			if b, ok := v.([]byte); ok {
				return reflect.ValueOf(b).Convert(t), nil
			}
		}
		items, ok := sequence(v)
		if !ok {
			break
		}
		out := reflect.MakeSlice(t, len(items), len(items))
		for i, item := range items {
			ev, err := convert(item, t.Elem())
			if err != nil {
				return reflect.Value{}, fmt.Errorf("item %d: %w", i, err)
			}
			out.Index(i).Set(ev)
		}
		return out, nil
	case reflect.Array:
		items, ok := sequence(v)
		if !ok || len(items) != t.Len() {
			break
		}
		out := reflect.New(t).Elem()
		for i, item := range items {
			ev, err := convert(item, t.Elem())
			if err != nil {
				return reflect.Value{}, fmt.Errorf("item %d: %w", i, err)
			}
			out.Index(i).Set(ev)
		}
		return out, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			break
		}
		m, ok := mapping(v)
		if !ok {
			break
		}
		out := reflect.MakeMapWithSize(t, len(m))
		for k, item := range m {
			ev, err := convert(item, t.Elem())
			if err != nil {
				return reflect.Value{}, fmt.Errorf("key %q: %w", k, err)
			}
			out.SetMapIndex(reflect.ValueOf(k).Convert(t.Key()), ev)
		}
		return out, nil
	case reflect.Struct:
		if t == timeType {
			if x, ok := v.(time.Time); ok {
				return reflect.ValueOf(x), nil
			}
			if x, ok := v.(monty.Date); ok {
				return reflect.ValueOf(time.Date(x.Year, x.Month, x.Day, 0, 0, 0, 0, time.UTC)), nil
			}
			break
		}
		m, ok := mapping(v)
		if !ok {
			break
		}
		out := reflect.New(t).Elem()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := jsonName(f)
			if name == "" {
				continue
			}
			item, ok := m[name]
			if !ok {
				continue
			}
			fv, err := convert(item, f.Type)
			if err != nil {
				return reflect.Value{}, fmt.Errorf("field %q: %w", name, err)
			}
			out.Field(i).Set(fv)
		}
		return out, nil
	case reflect.Interface:
		if rv.Type().Implements(t) {
			return rv, nil
		}
	}
	return reflect.Value{}, fmt.Errorf("expected %s, got %s", pyName(t), pyValueName(v))
}

func sequence(v any) ([]any, bool) {
	switch x := v.(type) {
	case []any:
		return x, true
	case monty.Tuple:
		return x, true
	case monty.Set:
		return x, true
	case monty.FrozenSet:
		return x, true
	}
	return nil, false
}

func mapping(v any) (map[string]any, bool) {
	switch x := v.(type) {
	case map[string]any:
		return x, true
	case monty.Dict:
		m := make(map[string]any, len(x))
		for _, item := range x {
			k, ok := item.Key.(string)
			if !ok {
				return nil, false
			}
			m[k] = item.Value
		}
		return m, true
	case *monty.HostObject:
		return x.Attrs, true
	case *monty.ClassInstance:
		return x.Attrs, true
	}
	return nil, false
}

func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	if tag != "" {
		if name := strings.Split(tag, ",")[0]; name != "" {
			return name
		}
	}
	return f.Name
}

func pyValueName(v any) string {
	switch v.(type) {
	case nil:
		return "None"
	case bool:
		return "bool"
	case int64, *big.Int:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []byte:
		return "bytes"
	case []any:
		return "list"
	case monty.Tuple:
		return "tuple"
	case map[string]any, monty.Dict:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

// ---------------------------------------------------------------------------
// Stubs and prompts
// ---------------------------------------------------------------------------

// pyType renders the annotation for a Go type, registering struct types as
// TypedDicts.
func (r *Registry) pyType(t reflect.Type) string {
	switch {
	case t == anyType:
		return "Any"
	case t == timeType:
		return "datetime.datetime"
	case t == durationType:
		return "datetime.timedelta"
	case t == bigIntType:
		return "int"
	case t == reflect.TypeOf(monty.Date{}):
		return "datetime.date"
	case t == reflect.TypeOf(monty.Time{}):
		return "datetime.time"
	case t == reflect.TypeOf(monty.Tuple(nil)):
		return "tuple[Any, ...]"
	case t == reflect.TypeOf(monty.Set(nil)), t == reflect.TypeOf(monty.FrozenSet(nil)):
		return "set[Any]"
	case t == reflect.TypeOf(monty.Dict(nil)):
		return "dict[Any, Any]"
	case t == reflect.TypeOf(monty.Path("")):
		return "pathlib.Path"
	case t == reflect.TypeOf((*monty.HostObject)(nil)), t == reflect.TypeOf((*monty.HostClass)(nil)), t == reflect.TypeOf((*monty.ClassInstance)(nil)):
		return "Any"
	case t == errorType:
		return "None"
	}
	switch t.Kind() {
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "int"
	case reflect.Float32, reflect.Float64:
		return "float"
	case reflect.String:
		return "str"
	case reflect.Pointer:
		return r.pyType(t.Elem()) + " | None"
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return "bytes"
		}
		return "list[" + r.pyType(t.Elem()) + "]"
	case reflect.Array:
		return "list[" + r.pyType(t.Elem()) + "]"
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			panic(fmt.Sprintf("tools: unsupported map key type %s", t.Key()))
		}
		return "dict[str, " + r.pyType(t.Elem()) + "]"
	case reflect.Struct:
		return r.typedDict(t)
	case reflect.Interface:
		return "Any"
	}
	panic(fmt.Sprintf("tools: unsupported type %s", t))
}

// typedDict registers a struct as a TypedDict and returns its name.
func (r *Registry) typedDict(t reflect.Type) string {
	if name, ok := r.dicts[t]; ok {
		return name
	}
	name := t.Name()
	if name == "" {
		name = fmt.Sprintf("Record%d", len(r.dicts)+1)
	}
	base := name
	for i := 2; r.names[name]; i++ {
		name = fmt.Sprintf("%s%d", base, i)
	}
	r.names[name] = true
	r.dicts[t] = name
	// Register nested types so they render before use.
	for i := 0; i < t.NumField(); i++ {
		if f := t.Field(i); f.IsExported() && jsonName(f) != "" {
			r.pyType(f.Type)
		}
	}
	return name
}

// Signature renders a tool's Python def line without the body.
func (r *Registry) Signature(t *Tool) string {
	var b strings.Builder
	if t.Async {
		b.WriteString("async ")
	}
	fmt.Fprintf(&b, "def %s(", t.Name)
	optionalFrom := len(t.Params)
	for i := len(t.Params) - 1; i >= 0 && t.paramType(i).Kind() == reflect.Pointer; i-- {
		optionalFrom = i
	}
	for i, p := range t.Params {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s: %s", p, r.pyType(t.paramType(i)))
		if i >= optionalFrom {
			b.WriteString(" = None")
		}
	}
	b.WriteString(") -> ")
	if t.valueOut {
		b.WriteString(r.pyType(t.ft.Out(0)))
	} else {
		b.WriteString("None")
	}
	return b.String()
}

func (r *Registry) header() string {
	return "import datetime\nimport pathlib\nfrom typing import Any, TypedDict\n\n"
}

func (r *Registry) typedDicts() string {
	types := make([]reflect.Type, 0, len(r.dicts))
	for t := range r.dicts {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool { return r.dicts[types[i]] < r.dicts[types[j]] })
	var b strings.Builder
	for _, t := range types {
		fmt.Fprintf(&b, "class %s(TypedDict):\n", r.dicts[t])
		n := 0
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() || jsonName(f) == "" {
				continue
			}
			fmt.Fprintf(&b, "    %s: %s\n", jsonName(f), r.pyType(f.Type))
			n++
		}
		if n == 0 {
			b.WriteString("    pass\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Stubs renders the .pyi text for monty.SessionOptions.TypeCheckStubs.
func (r *Registry) Stubs() string {
	var b strings.Builder
	b.WriteString(r.header())
	b.WriteString(r.typedDicts())
	for _, name := range r.order {
		fmt.Fprintf(&b, "%s: ...\n", r.Signature(r.tools[name]))
	}
	return b.String()
}

// Prompt renders the same declarations with docstrings, for a model's
// system prompt.
func (r *Registry) Prompt() string {
	var b strings.Builder
	b.WriteString("The following Python functions are available; they run outside the sandbox.\n\n")
	b.WriteString(r.typedDicts())
	for _, name := range r.order {
		t := r.tools[name]
		fmt.Fprintf(&b, "%s:\n", r.Signature(t))
		if t.Description != "" {
			fmt.Fprintf(&b, "    \"\"\"%s\"\"\"\n", t.Description)
		} else {
			b.WriteString("    ...\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Tools lists the registered tools in registration order.
func (r *Registry) Tools() []*Tool {
	out := make([]*Tool, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name])
	}
	return out
}

// Functions returns the synchronous tools as monty.Functions.
func (r *Registry) Functions() map[string]monty.Function {
	out := map[string]monty.Function{}
	for _, t := range r.Tools() {
		if !t.Async {
			out[t.Name] = t.Function()
		}
	}
	return out
}

// AsyncFunctions returns the asynchronous tools as monty.Functions.
func (r *Registry) AsyncFunctions() map[string]monty.Function {
	out := map[string]monty.Function{}
	for _, t := range r.Tools() {
		if t.Async {
			out[t.Name] = t.Function()
		}
	}
	return out
}

// Options returns opts with the registry's tools merged into Functions and
// AsyncFunctions.
func (r *Registry) Options(opts monty.RunOptions) monty.RunOptions {
	if opts.Functions == nil {
		opts.Functions = map[string]monty.Function{}
	}
	if opts.AsyncFunctions == nil {
		opts.AsyncFunctions = map[string]monty.Function{}
	}
	for name, fn := range r.Functions() {
		opts.Functions[name] = fn
	}
	for name, fn := range r.AsyncFunctions() {
		opts.AsyncFunctions[name] = fn
	}
	return opts
}

func pyName(t reflect.Type) string {
	defer func() { _ = recover() }()
	return (&Registry{dicts: map[reflect.Type]string{}, names: map[string]bool{}}).pyType(t)
}
