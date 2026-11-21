package monty

import (
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/go-monty/internal/pb"
)

// Value conversion between Go and the sandbox.
//
// Go → Python (inputs, host function results):
//
//	nil                         None
//	bool                        bool
//	int*, uint*                 int
//	*big.Int                    int
//	float32, float64            float
//	string                      str
//	[]byte                      bytes
//	[]any, any slice or array   list
//	Tuple                       tuple
//	Set, FrozenSet              set, frozenset
//	map[string]T (sorted keys)  dict
//	Dict                        dict (insertion order kept)
//	time.Time                   datetime (aware; the zone offset is kept)
//	time.Duration               timedelta
//	Date, Time                  date, time
//	*time.Location              timezone (fixed offset)
//	*Exception                  exception value
//	Ellipsis, NotImplemented    ..., NotImplemented
//	Path                        pathlib.Path
//	FunctionRef                 external function
//	TypeRef (builtin)           type
//	NamedTuple                  named tuple
//	complex64, complex128       complex
//	Decimal                     decimal.Decimal
//	*Module                     module
//
// Python → Go (results, host function arguments):
//
//	None                        nil
//	bool                        bool
//	int                         int64, or *big.Int when it does not fit
//	float                       float64
//	str                         string
//	bytes                       []byte
//	list                        []any
//	tuple                       Tuple
//	set, frozenset              Set, FrozenSet
//	dict                        map[string]any when every key is a str,
//	                            otherwise Dict
//	datetime                    time.Time (naive values are taken as UTC)
//	timedelta                   time.Duration (TimeDelta when out of range)
//	date, time, timezone        Date, Time, *time.Location
//	exception                   *Exception
//	type                        TypeRef
//	class instance              *ClassInstance
//	function, builtin function  FunctionRef
//	pathlib.Path                Path
//	file handle                 FileHandle
//	complex                     complex128
//	decimal.Decimal             Decimal
//	module                      *Module
//	anything else               Repr (its repr() string)

// Tuple is a Python tuple.
type Tuple []any

// Set is a Python set.
type Set []any

// FrozenSet is a Python frozenset.
type FrozenSet []any

// Dict is an insertion-ordered Python dict with arbitrary hashable keys.
type Dict []DictItem

// DictItem is one key/value entry of a Dict.
type DictItem struct {
	Key   any
	Value any
}

// Get returns the value stored under key, comparing keys with ==.
func (d Dict) Get(key any) (any, bool) {
	for _, item := range d {
		if item.Key == key {
			return item.Value, true
		}
	}
	return nil, false
}

// NamedTuple is a Python named tuple such as os.stat_result.
type NamedTuple struct {
	TypeName string
	Fields   []string
	Values   []any
}

// Date is a Python datetime.date.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// Time is a Python datetime.time.
type Time struct {
	Hour, Minute, Second, Microsecond int
	// Offset is the fixed UTC offset in seconds for aware times; nil when naive.
	Offset *int
	// TZName is the timezone name; only meaningful when Offset is set.
	TZName string
	Fold   int
}

// TimeDelta is a Python datetime.timedelta that does not fit a time.Duration.
type TimeDelta struct {
	Days, Seconds, Microseconds int
}

// Path is a pathlib.Path value (always a virtual POSIX path inside the sandbox).
type Path string

// Repr is the repr() of a sandbox value that has no other Go representation.
type Repr string

// FunctionRef is an external (host) function or a builtin function value.
type FunctionRef struct {
	Name    string
	Doc     string
	Builtin bool
}

// TypeRef is a Python type object.
type TypeRef struct {
	Name string
	// ID identifies a sandbox or host class; empty for builtin types.
	ID          string
	Origin      TypeOrigin
	IsDataclass bool
	Attrs       map[string]any
}

// TypeOrigin says where a TypeRef's class was defined.
type TypeOrigin int

const (
	TypeOriginBuiltin TypeOrigin = iota + 1
	TypeOriginSandbox
	TypeOriginHost
)

// ClassInstance is an instance of a class defined inside the sandbox.
type ClassInstance struct {
	Type  TypeRef
	ID    string
	Attrs map[string]any
}

// Decimal is a decimal.Decimal as its canonical string, str(d).
type Decimal string

// Module is a host module: sandbox code that imports it reads Attrs as the
// module's attributes. Return one from an OSHandler's "import" call.
type Module struct {
	// Name is the dotted module name.
	Name  string
	Attrs map[string]any

	id string
}

// FileHandle is an open() result: purely virtual state, never a host file.
type FileHandle struct {
	Path     string
	Mode     string
	Position uint64
}

// codec converts values in the context of one session's object store, so
// host objects keep their identity across the boundary.
type codec struct {
	store *objectStore
}

type ellipsis struct{}
type notImplemented struct{}

func (ellipsis) String() string       { return "Ellipsis" }
func (notImplemented) String() string { return "NotImplemented" }

// Ellipsis is the Python `...` value.
var Ellipsis any = ellipsis{}

// NotImplemented is the Python NotImplemented value.
var NotImplemented any = notImplemented{}

// ---------------------------------------------------------------------------
// Go → protobuf
// ---------------------------------------------------------------------------

func unit() *pb.Unit { return &pb.Unit{} }

// arena collects the values of one message. Nodes are in post-order: every
// container is appended after its children and names them by index.
type arena struct {
	c     *codec
	nodes []*pb.MontyNode
	// classes shares one type node between the instances of a host class.
	classes map[*HostClass]uint32
	// sent lists the classes whose Attrs this arena carries.
	sent []*HostClass
}

func (c *codec) arena() *arena { return &arena{c: c} }

func (a *arena) proto() *pb.Arena {
	return &pb.Arena{NodeCount: uint32(len(a.nodes)), Nodes: a.nodes}
}

func (a *arena) push(n *pb.MontyNode) uint32 {
	a.nodes = append(a.nodes, n)
	return uint32(len(a.nodes) - 1)
}

// encode appends v and returns its index. A failed value leaves no nodes.
func (a *arena) encode(v any) (uint32, error) {
	mark, sent := len(a.nodes), len(a.sent)
	idx, err := a.node(v)
	if err != nil {
		a.nodes = a.nodes[:mark]
		for cl, i := range a.classes {
			if int(i) >= mark {
				delete(a.classes, cl)
			}
		}
		for _, cl := range a.sent[sent:] {
			delete(a.c.store.sentClasses, cl.ID())
		}
		a.sent = a.sent[:sent]
	}
	return idx, err
}

func (a *arena) indexes(items []any) (*pb.Indexes, error) {
	out := make([]uint32, len(items))
	for i, item := range items {
		idx, err := a.node(item)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		out[i] = idx
	}
	return &pb.Indexes{Items: out}, nil
}

func (a *arena) stringMap(m map[string]any) (*pb.NodePairs, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]*pb.NodePair, 0, len(keys))
	for _, k := range keys {
		if !utf8.ValidString(k) {
			return nil, fmt.Errorf("key %q: not valid UTF-8", k)
		}
		v, err := a.node(m[k])
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", k, err)
		}
		pairs = append(pairs, &pb.NodePair{Key: a.push(strNode(k)), Value: v})
	}
	return &pb.NodePairs{Pairs: pairs}, nil
}

func strNode(s string) *pb.MontyNode { return &pb.MontyNode{Kind: &pb.MontyNode_Str{Str: s}} }

func intNode(i int64) *pb.MontyNode { return &pb.MontyNode{Kind: &pb.MontyNode_Int{Int: i}} }

func bigNode(b *big.Int) *pb.MontyNode {
	if b.IsInt64() {
		return intNode(b.Int64())
	}
	return &pb.MontyNode{Kind: &pb.MontyNode_Bigint{Bigint: &pb.BigInt{
		Negative:  b.Sign() < 0,
		Magnitude: new(big.Int).Abs(b).Bytes(),
	}}}
}

// timeNode builds a datetime; naive drops the zone.
func timeNode(t time.Time, naive bool) *pb.MontyNode {
	dt := &pb.DateTime{
		Year:        int32(t.Year()),
		Month:       uint32(t.Month()),
		Day:         uint32(t.Day()),
		Hour:        uint32(t.Hour()),
		Minute:      uint32(t.Minute()),
		Second:      uint32(t.Second()),
		Microsecond: uint32(t.Nanosecond() / 1000),
	}
	if !naive {
		name, offset := t.Zone()
		off := int32(offset)
		dt.OffsetSeconds = &off
		if name != "" {
			dt.TimezoneName = &name
		}
	}
	return &pb.MontyNode{Kind: &pb.MontyNode_Datetime{Datetime: dt}}
}

func durationNode(d time.Duration) *pb.MontyNode {
	micros := d.Microseconds()
	days := micros / (86400 * 1_000_000)
	micros -= days * 86400 * 1_000_000
	if micros < 0 {
		days--
		micros += 86400 * 1_000_000
	}
	secs := micros / 1_000_000
	micros -= secs * 1_000_000
	return &pb.MontyNode{Kind: &pb.MontyNode_Timedelta{Timedelta: &pb.TimeDelta{
		Days: int32(days), Seconds: int32(secs), Microseconds: int32(micros),
	}}}
}

func exceptionNode(e *Exception) *pb.MontyNode {
	exc := &pb.Exception{ExcType: e.Type}
	if e.Message != "" {
		msg := e.Message
		exc.Arg = &msg
	}
	return &pb.MontyNode{Kind: &pb.MontyNode_Exception{Exception: exc}}
}

func timezoneProto(loc *time.Location) *pb.TimeZone {
	name, offset := time.Now().In(loc).Zone()
	tz := &pb.TimeZone{OffsetSeconds: int32(offset)}
	if name != "" {
		tz.Name = &name
	}
	return tz
}

// leaf converts the values that have no children; ok is false for the rest.
func leaf(v any) (n *pb.MontyNode, ok bool, err error) {
	switch x := v.(type) {
	case nil:
		n = &pb.MontyNode{Kind: &pb.MontyNode_None{None: unit()}}
	case bool:
		n = &pb.MontyNode{Kind: &pb.MontyNode_Boolean{Boolean: x}}
	case int:
		n = intNode(int64(x))
	case int8:
		n = intNode(int64(x))
	case int16:
		n = intNode(int64(x))
	case int32:
		n = intNode(int64(x))
	case int64:
		n = intNode(x)
	case uint:
		n = bigNode(new(big.Int).SetUint64(uint64(x)))
	case uint8:
		n = intNode(int64(x))
	case uint16:
		n = intNode(int64(x))
	case uint32:
		n = intNode(int64(x))
	case uint64:
		n = bigNode(new(big.Int).SetUint64(x))
	case uintptr:
		n = bigNode(new(big.Int).SetUint64(uint64(x)))
	case *big.Int:
		if x == nil {
			return leaf(nil)
		}
		n = bigNode(x)
	case big.Int:
		n = bigNode(&x)
	case float32:
		n = &pb.MontyNode{Kind: &pb.MontyNode_Float{Float: float64(x)}}
	case float64:
		n = &pb.MontyNode{Kind: &pb.MontyNode_Float{Float: x}}
	case complex64:
		return leaf(complex128(x))
	case complex128:
		re, im := real(x), imag(x)
		n = &pb.MontyNode{Kind: &pb.MontyNode_Complex{Complex: &pb.Complex{Real: &re, Imag: &im}}}
	case string:
		if !utf8.ValidString(x) {
			return nil, true, fmt.Errorf("monty: string is not valid UTF-8")
		}
		n = strNode(x)
	case []byte:
		n = &pb.MontyNode{Kind: &pb.MontyNode_Bytes{Bytes: x}}
	case Decimal:
		n = &pb.MontyNode{Kind: &pb.MontyNode_Decimal{Decimal: string(x)}}
	case time.Time:
		n = timeNode(x, false)
	case *time.Time:
		if x == nil {
			return leaf(nil)
		}
		n = timeNode(*x, false)
	case time.Duration:
		n = durationNode(x)
	case TimeDelta:
		n = &pb.MontyNode{Kind: &pb.MontyNode_Timedelta{Timedelta: &pb.TimeDelta{
			Days: int32(x.Days), Seconds: int32(x.Seconds), Microseconds: int32(x.Microseconds),
		}}}
	case Date:
		n = &pb.MontyNode{Kind: &pb.MontyNode_Date{Date: &pb.Date{
			Year: int32(x.Year), Month: uint32(x.Month), Day: uint32(x.Day),
		}}}
	case Time:
		t := &pb.Time{
			Hour: uint32(x.Hour), Minute: uint32(x.Minute), Second: uint32(x.Second),
			Microsecond: uint32(x.Microsecond), Fold: uint32(x.Fold),
		}
		if x.Offset != nil {
			off := int32(*x.Offset)
			t.OffsetSeconds = &off
			if x.TZName != "" {
				name := x.TZName
				t.TimezoneName = &name
			}
		}
		n = &pb.MontyNode{Kind: &pb.MontyNode_Time{Time: t}}
	case *time.Location:
		if x == nil {
			return leaf(nil)
		}
		n = &pb.MontyNode{Kind: &pb.MontyNode_Timezone{Timezone: timezoneProto(x)}}
	case *Exception:
		if x == nil {
			return leaf(nil)
		}
		n = exceptionNode(x)
	case Exception:
		n = exceptionNode(&x)
	case ellipsis:
		n = &pb.MontyNode{Kind: &pb.MontyNode_Ellipsis{Ellipsis: unit()}}
	case notImplemented:
		n = &pb.MontyNode{Kind: &pb.MontyNode_NotImplemented{NotImplemented: unit()}}
	case Path:
		n = &pb.MontyNode{Kind: &pb.MontyNode_Path{Path: string(x)}}
	case FileHandle:
		n = &pb.MontyNode{Kind: &pb.MontyNode_FileHandle{FileHandle: &pb.FileHandle{
			Path: x.Path, Mode: x.Mode, Position: x.Position,
		}}}
	case FunctionRef:
		if x.Builtin {
			n = &pb.MontyNode{Kind: &pb.MontyNode_BuiltinFunction{BuiltinFunction: x.Name}}
			break
		}
		fn := &pb.Function{Name: x.Name}
		if x.Doc != "" {
			doc := x.Doc
			fn.Docstring = &doc
		}
		n = &pb.MontyNode{Kind: &pb.MontyNode_Function{Function: fn}}
	case TypeRef:
		if x.Origin != TypeOriginBuiltin && x.Origin != 0 {
			return nil, true, fmt.Errorf("monty: only builtin types can be passed into the sandbox, not %q", x.Name)
		}
		n = &pb.MontyNode{Kind: &pb.MontyNode_Type{Type: &pb.Type{
			Name: x.Name, Origin: pb.TypeOrigin_TYPE_ORIGIN_BUILTIN,
		}}}
	case Repr:
		return nil, true, fmt.Errorf("monty: a Repr value cannot be passed into the sandbox")
	case *ClassInstance:
		return nil, true, fmt.Errorf("monty: sandbox class instances cannot be passed back into the sandbox")
	default:
		return nil, false, nil
	}
	return n, true, nil
}

// node appends v's nodes, children first, and returns v's index.
func (a *arena) node(v any) (uint32, error) {
	if n, ok, err := leaf(v); ok {
		if err != nil {
			return 0, err
		}
		return a.push(n), nil
	}
	switch x := v.(type) {
	case []any:
		l, err := a.indexes(x)
		if err != nil {
			return 0, err
		}
		return a.push(&pb.MontyNode{Kind: &pb.MontyNode_List{List: l}}), nil
	case Tuple:
		l, err := a.indexes(x)
		if err != nil {
			return 0, err
		}
		return a.push(&pb.MontyNode{Kind: &pb.MontyNode_Tuple{Tuple: l}}), nil
	case Set:
		l, err := a.indexes(x)
		if err != nil {
			return 0, err
		}
		return a.push(&pb.MontyNode{Kind: &pb.MontyNode_Set{Set: l}}), nil
	case FrozenSet:
		l, err := a.indexes(x)
		if err != nil {
			return 0, err
		}
		return a.push(&pb.MontyNode{Kind: &pb.MontyNode_FrozenSet{FrozenSet: l}}), nil
	case map[string]any:
		d, err := a.stringMap(x)
		if err != nil {
			return 0, err
		}
		return a.push(&pb.MontyNode{Kind: &pb.MontyNode_Dict{Dict: d}}), nil
	case Dict:
		pairs := make([]*pb.NodePair, 0, len(x))
		for i, item := range x {
			k, err := a.node(item.Key)
			if err != nil {
				return 0, fmt.Errorf("dict key %d: %w", i, err)
			}
			val, err := a.node(item.Value)
			if err != nil {
				return 0, fmt.Errorf("dict value %d: %w", i, err)
			}
			pairs = append(pairs, &pb.NodePair{Key: k, Value: val})
		}
		return a.push(&pb.MontyNode{Kind: &pb.MontyNode_Dict{Dict: &pb.NodePairs{Pairs: pairs}}}), nil
	case NamedTuple:
		l, err := a.indexes(x.Values)
		if err != nil {
			return 0, err
		}
		return a.push(&pb.MontyNode{Kind: &pb.MontyNode_NamedTuple{NamedTuple: &pb.NamedTupleNode{
			TypeName: x.TypeName, FieldNames: x.Fields, Values: l.Items,
		}}}), nil
	case *HostObject:
		return a.object(x)
	case *HostClass:
		if x == nil {
			return a.node(nil)
		}
		return a.class(x)
	case *Module:
		if x == nil {
			return a.node(nil)
		}
		attrs, err := a.stringMap(x.Attrs)
		if err != nil {
			return 0, fmt.Errorf("module %s: %w", x.Name, err)
		}
		if x.id == "" {
			x.id = newUUID()
		}
		return a.push(&pb.MontyNode{Kind: &pb.MontyNode_Module{Module: &pb.ModuleNode{
			Name: x.Name, Id: &pb.Uuid{Data: uuidBytes(x.id)}, Attrs: attrs,
		}}}), nil
	case error:
		return a.push(exceptionNode(&Exception{Type: "RuntimeError", Message: x.Error()})), nil
	}
	return a.reflect(reflect.ValueOf(v))
}

func (a *arena) reflect(rv reflect.Value) (uint32, error) {
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return a.node(nil)
		}
		return a.node(rv.Elem().Interface())
	case reflect.Bool:
		return a.node(rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return a.node(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return a.node(rv.Uint())
	case reflect.Float32, reflect.Float64:
		return a.node(rv.Float())
	case reflect.Complex64, reflect.Complex128:
		return a.node(rv.Complex())
	case reflect.String:
		return a.node(rv.String())
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
			return a.node(rv.Bytes())
		}
		items := make([]any, rv.Len())
		for i := range items {
			items[i] = rv.Index(i).Interface()
		}
		return a.node(items)
	case reflect.Struct:
		return a.node(structToMap(rv))
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return 0, fmt.Errorf("monty: cannot convert map with %s keys; use monty.Dict", rv.Type().Key())
		}
		m := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			m[iter.Key().String()] = iter.Value().Interface()
		}
		return a.node(m)
	}
	return 0, fmt.Errorf("monty: cannot convert Go value of type %s", rv.Type())
}

// structToMap flattens an exported-field struct into a map, honouring json
// tags for names and "-" for exclusion.
func structToMap(rv reflect.Value) map[string]any {
	m := map[string]any{}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		name, omitEmpty := fieldName(f)
		if name == "" {
			continue
		}
		if f.Anonymous && f.Type.Kind() == reflect.Struct && f.Tag.Get("json") == "" {
			for k, v := range structToMap(rv.Field(i)) {
				m[k] = v
			}
			continue
		}
		fv := rv.Field(i)
		if omitEmpty && fv.IsZero() {
			continue
		}
		m[name] = fv.Interface()
	}
	return m
}

// fieldName returns a struct field's wire name from its json tag.
func fieldName(f reflect.StructField) (name string, omitEmpty bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name = f.Name
	if tag != "" {
		parts := strings.Split(tag, ",")
		if parts[0] != "" {
			name = parts[0]
		}
		for _, p := range parts[1:] {
			if p == "omitempty" {
				omitEmpty = true
			}
		}
	}
	return name, omitEmpty
}

// class appends the type node of a host class, once per arena, sending its
// Attrs the first time the session sees the class.
func (a *arena) class(cl *HostClass) (uint32, error) {
	if a.c.store == nil {
		return 0, fmt.Errorf("monty: host objects can only be passed through a session")
	}
	if idx, ok := a.classes[cl]; ok {
		return idx, nil
	}
	a.c.store.registerClass(cl)
	t := &pb.Type{
		Name:        cl.Name,
		Id:          &pb.Uuid{Data: uuidBytes(cl.ID())},
		Origin:      pb.TypeOrigin_TYPE_ORIGIN_HOST,
		IsDataclass: cl.IsDataclass,
	}
	if len(cl.Attrs) > 0 && !a.c.store.sentClasses[cl.ID()] {
		attrs, err := a.stringMap(cl.Attrs)
		if err != nil {
			return 0, fmt.Errorf("class %s attrs: %w", cl.Name, err)
		}
		t.Attrs = attrs
		a.c.store.sentClasses[cl.ID()] = true
		a.sent = append(a.sent, cl)
	}
	idx := a.push(&pb.MontyNode{Kind: &pb.MontyNode_Type{Type: t}})
	if a.classes == nil {
		a.classes = map[*HostClass]uint32{}
	}
	a.classes[cl] = idx
	return idx, nil
}

// object appends the instance node of a host object.
func (a *arena) object(o *HostObject) (uint32, error) {
	if o == nil {
		return a.node(nil)
	}
	if o.Class == nil {
		return 0, fmt.Errorf("monty: host object has no class")
	}
	class, err := a.class(o.Class)
	if err != nil {
		return 0, err
	}
	a.c.store.registerObject(o)
	attrs, err := a.stringMap(o.Attrs)
	if err != nil {
		return 0, fmt.Errorf("object attrs: %w", err)
	}
	return a.push(&pb.MontyNode{Kind: &pb.MontyNode_ClassInstance{ClassInstance: &pb.ClassInstanceNode{
		ClassType:  class,
		InstanceId: &pb.Uuid{Data: uuidBytes(o.ID())},
		Attrs:      attrs,
	}}}), nil
}

// ---------------------------------------------------------------------------
// protobuf → Go
// ---------------------------------------------------------------------------

// values is a decoded arena.
type values struct {
	c     *codec
	nodes []*pb.MontyNode
	vals  []any
}

// decode converts every node of a in one forward pass: nodes are in
// post-order, so a container's children are decoded before it.
func (c *codec) decode(a *pb.Arena) (*values, error) {
	v := &values{c: c, nodes: a.GetNodes(), vals: make([]any, 0, len(a.GetNodes()))}
	for i, n := range v.nodes {
		val, err := v.build(n)
		if err != nil {
			return nil, fmt.Errorf("monty: value node %d: %w", i, err)
		}
		v.vals = append(v.vals, val)
	}
	return v, nil
}

// at returns the value at idx. While decoding, only earlier nodes exist.
func (v *values) at(idx uint32) (any, error) {
	if int(idx) >= len(v.vals) {
		return nil, fmt.Errorf("value index %d out of range", idx)
	}
	return v.vals[idx], nil
}

func (v *values) list(idxs []uint32) ([]any, error) {
	out := make([]any, len(idxs))
	for i, idx := range idxs {
		val, err := v.at(idx)
		if err != nil {
			return nil, err
		}
		out[i] = val
	}
	return out, nil
}

func (v *values) pairs(pairs []*pb.NodePair) (Dict, error) {
	items := make(Dict, 0, len(pairs))
	for _, p := range pairs {
		k, err := v.at(p.GetKey())
		if err != nil {
			return nil, err
		}
		val, err := v.at(p.GetValue())
		if err != nil {
			return nil, err
		}
		items = append(items, DictItem{Key: k, Value: val})
	}
	return items, nil
}

// stringDict decodes pairs whose keys must all be strings.
func (v *values) stringDict(pairs []*pb.NodePair) (map[string]any, error) {
	items, err := v.pairs(pairs)
	if err != nil {
		return nil, err
	}
	m := make(map[string]any, len(items))
	for _, item := range items {
		k, ok := item.Key.(string)
		if !ok {
			return nil, fmt.Errorf("non-string key %v", item.Key)
		}
		m[k] = item.Value
	}
	return m, nil
}

func uuidString(u *pb.Uuid) string {
	if u == nil || len(u.Data) != 16 {
		return ""
	}
	h := hex.EncodeToString(u.Data)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func (v *values) typeRef(t *pb.Type) (TypeRef, error) {
	if t == nil {
		return TypeRef{}, fmt.Errorf("empty type")
	}
	attrs, err := v.stringDict(t.GetAttrs().GetPairs())
	if err != nil {
		return TypeRef{}, err
	}
	return TypeRef{
		Name:        t.Name,
		ID:          uuidString(t.Id),
		Origin:      TypeOrigin(t.Origin),
		IsDataclass: t.IsDataclass,
		Attrs:       attrs,
	}, nil
}

// build converts one node whose children are already decoded.
func (v *values) build(n *pb.MontyNode) (any, error) {
	store := v.c.store
	switch k := n.GetKind().(type) {
	case nil, *pb.MontyNode_None:
		return nil, nil
	case *pb.MontyNode_Ellipsis:
		return Ellipsis, nil
	case *pb.MontyNode_NotImplemented:
		return NotImplemented, nil
	case *pb.MontyNode_Boolean:
		return k.Boolean, nil
	case *pb.MontyNode_Int:
		return k.Int, nil
	case *pb.MontyNode_Bigint:
		b := new(big.Int).SetBytes(k.Bigint.GetMagnitude())
		if k.Bigint.GetNegative() {
			b.Neg(b)
		}
		return b, nil
	case *pb.MontyNode_Float:
		return k.Float, nil
	case *pb.MontyNode_Complex:
		return complex(k.Complex.GetReal(), k.Complex.GetImag()), nil
	case *pb.MontyNode_Decimal:
		return Decimal(k.Decimal), nil
	case *pb.MontyNode_Str:
		return k.Str, nil
	case *pb.MontyNode_Bytes:
		return k.Bytes, nil
	case *pb.MontyNode_List:
		return v.list(k.List.GetItems())
	case *pb.MontyNode_Tuple:
		items, err := v.list(k.Tuple.GetItems())
		return Tuple(items), err
	case *pb.MontyNode_Set:
		items, err := v.list(k.Set.GetItems())
		return Set(items), err
	case *pb.MontyNode_FrozenSet:
		items, err := v.list(k.FrozenSet.GetItems())
		return FrozenSet(items), err
	case *pb.MontyNode_NamedTuple:
		vals, err := v.list(k.NamedTuple.GetValues())
		if err != nil {
			return nil, err
		}
		return NamedTuple{
			TypeName: k.NamedTuple.GetTypeName(),
			Fields:   k.NamedTuple.GetFieldNames(),
			Values:   vals,
		}, nil
	case *pb.MontyNode_Dict:
		items, err := v.pairs(k.Dict.GetPairs())
		if err != nil {
			return nil, err
		}
		m := make(map[string]any, len(items))
		for _, item := range items {
			key, ok := item.Key.(string)
			if !ok {
				return items, nil
			}
			m[key] = item.Value
		}
		return m, nil
	case *pb.MontyNode_Date:
		d := k.Date
		return Date{Year: int(d.GetYear()), Month: time.Month(d.GetMonth()), Day: int(d.GetDay())}, nil
	case *pb.MontyNode_Time:
		t := k.Time
		out := Time{
			Hour: int(t.GetHour()), Minute: int(t.GetMinute()), Second: int(t.GetSecond()),
			Microsecond: int(t.GetMicrosecond()), Fold: int(t.GetFold()), TZName: t.GetTimezoneName(),
		}
		if t.OffsetSeconds != nil {
			off := int(*t.OffsetSeconds)
			out.Offset = &off
		}
		return out, nil
	case *pb.MontyNode_Datetime:
		dt := k.Datetime
		loc := time.UTC
		if dt.OffsetSeconds != nil {
			loc = time.FixedZone(dt.GetTimezoneName(), int(*dt.OffsetSeconds))
		}
		return time.Date(int(dt.GetYear()), time.Month(dt.GetMonth()), int(dt.GetDay()),
			int(dt.GetHour()), int(dt.GetMinute()), int(dt.GetSecond()),
			int(dt.GetMicrosecond())*1000, loc), nil
	case *pb.MontyNode_Timedelta:
		td := k.Timedelta
		days, secs, micros := int64(td.GetDays()), int64(td.GetSeconds()), int64(td.GetMicroseconds())
		totalMicros := days*86400*1_000_000 + secs*1_000_000 + micros
		const maxMicros = math.MaxInt64 / 1000
		if days > maxMicros/(86400*1_000_000) || days < -maxMicros/(86400*1_000_000) ||
			totalMicros > maxMicros || totalMicros < -maxMicros {
			return TimeDelta{Days: int(days), Seconds: int(secs), Microseconds: int(micros)}, nil
		}
		return time.Duration(totalMicros) * time.Microsecond, nil
	case *pb.MontyNode_Timezone:
		return time.FixedZone(k.Timezone.GetName(), int(k.Timezone.GetOffsetSeconds())), nil
	case *pb.MontyNode_Exception:
		return &Exception{Type: k.Exception.GetExcType(), Message: k.Exception.GetArg()}, nil
	case *pb.MontyNode_Type:
		if store != nil && k.Type.GetOrigin() == pb.TypeOrigin_TYPE_ORIGIN_HOST {
			if cl, ok := store.classes[uuidString(k.Type.GetId())]; ok {
				return cl, nil
			}
		}
		return v.typeRef(k.Type)
	case *pb.MontyNode_ClassInstance:
		ci := k.ClassInstance
		if int(ci.GetClassType()) >= len(v.vals) {
			return nil, fmt.Errorf("class index %d out of range", ci.GetClassType())
		}
		class := v.nodes[ci.GetClassType()].GetType()
		if store != nil && class.GetOrigin() == pb.TypeOrigin_TYPE_ORIGIN_HOST {
			if o, ok := store.objects[uuidString(ci.GetInstanceId())]; ok {
				return o, nil
			}
		}
		t, err := v.typeRef(class)
		if err != nil {
			return nil, err
		}
		attrs, err := v.stringDict(ci.GetAttrs().GetPairs())
		if err != nil {
			return nil, err
		}
		return &ClassInstance{Type: t, ID: uuidString(ci.GetInstanceId()), Attrs: attrs}, nil
	case *pb.MontyNode_Module:
		attrs, err := v.stringDict(k.Module.GetAttrs().GetPairs())
		if err != nil {
			return nil, err
		}
		return &Module{Name: k.Module.GetName(), Attrs: attrs, id: uuidString(k.Module.GetId())}, nil
	case *pb.MontyNode_Function:
		return FunctionRef{Name: k.Function.GetName(), Doc: k.Function.GetDocstring()}, nil
	case *pb.MontyNode_BuiltinFunction:
		return FunctionRef{Name: k.BuiltinFunction, Builtin: true}, nil
	case *pb.MontyNode_Path:
		return Path(k.Path), nil
	case *pb.MontyNode_FileHandle:
		fh := k.FileHandle
		return FileHandle{Path: fh.GetPath(), Mode: fh.GetMode(), Position: fh.GetPosition()}, nil
	case *pb.MontyNode_Repr:
		return Repr(k.Repr), nil
	case *pb.MontyNode_Cycle:
		return Repr(k.Cycle), nil
	case *pb.MontyNode_Uuid:
		return nil, fmt.Errorf("uuid values are not supported")
	}
	return nil, fmt.Errorf("unknown value kind %T", n.GetKind())
}

// exceptionFromProto converts a raised exception, rendering its traceback.
func exceptionFromProto(e *pb.RaisedException) *Exception {
	if e == nil {
		return &Exception{Type: "RuntimeError", Message: "worker reported an empty exception"}
	}
	frames := make([]Frame, 0, len(e.Traceback))
	for _, f := range e.Traceback {
		frames = append(frames, Frame{
			Filename:  f.GetFilename(),
			Line:      int(f.GetStart().GetLine()),
			Column:    int(f.GetStart().GetColumn()),
			EndLine:   int(f.GetEnd().GetLine()),
			EndColumn: int(f.GetEnd().GetColumn()),
			Name:      f.GetFrameName(),
			Preview:   f.GetPreviewLine(),
		})
	}
	exc := &Exception{Type: e.GetExcType(), Message: e.GetMessage(), Frames: frames}
	exc.Traceback = renderTraceback(frames, exc.Type, exc.Message)
	switch d := e.GetData().GetKind().(type) {
	case *pb.ExcData_Unicode:
		u := &UnicodeErrorData{
			Encoding: d.Unicode.GetEncoding(),
			Start:    d.Unicode.GetStart(),
			End:      d.Unicode.GetEnd(),
			Reason:   d.Unicode.GetReason(),
		}
		switch o := d.Unicode.GetObject().(type) {
		case *pb.UnicodeErrorData_ObjectBytes:
			u.Object = o.ObjectBytes
		case *pb.UnicodeErrorData_ObjectStr:
			u.Object = o.ObjectStr
		}
		exc.Data = u
	case *pb.ExcData_Json:
		j := &JSONErrorData{
			Msg:    d.Json.GetMsg(),
			Pos:    d.Json.GetPos(),
			Lineno: d.Json.GetLineno(),
			Colno:  d.Json.GetColno(),
		}
		if d.Json.Doc != nil {
			doc := d.Json.GetDoc()
			j.Doc = &doc
		}
		exc.Data = j
	}
	return exc
}

// raisedFromError converts a host-side error into a wire exception.
func raisedFromError(err error) *pb.RaisedException {
	var exc *Exception
	if e, ok := err.(*Exception); ok {
		exc = e
	} else {
		exc = &Exception{Type: "RuntimeError", Message: err.Error()}
	}
	msg := exc.Message
	return &pb.RaisedException{ExcType: exc.Type, Message: &msg}
}
