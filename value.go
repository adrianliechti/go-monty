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

// encode converts a plain Go value; host objects need a session codec.
func encode(v any) (*pb.MontyObject, error) { return (&codec{}).encode(v) }

// decode converts a wire value outside any session.
func decode(obj *pb.MontyObject) (any, error) { return (&codec{}).decode(obj) }

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

func encodeInt(i int64) *pb.MontyObject {
	return &pb.MontyObject{Kind: &pb.MontyObject_Int{Int: i}}
}

func encodeStr(s string) *pb.MontyObject {
	return &pb.MontyObject{Kind: &pb.MontyObject_Str{Str: s}}
}

func encodeBig(b *big.Int) *pb.MontyObject {
	if b.IsInt64() {
		return encodeInt(b.Int64())
	}
	mag := new(big.Int).Abs(b)
	return &pb.MontyObject{Kind: &pb.MontyObject_Bigint{Bigint: &pb.BigInt{
		Negative:  b.Sign() < 0,
		Magnitude: mag.Bytes(),
	}}}
}

func (c *codec) encodeList(items []any) (*pb.ObjectList, error) {
	out := make([]*pb.MontyObject, 0, len(items))
	for i, item := range items {
		obj, err := c.encode(item)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		out = append(out, obj)
	}
	return &pb.ObjectList{Items: out}, nil
}

func (c *codec) encodeStringMap(m map[string]any) (*pb.Dict, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]*pb.Pair, 0, len(keys))
	for _, k := range keys {
		v, err := c.encode(m[k])
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", k, err)
		}
		pairs = append(pairs, &pb.Pair{Key: encodeStr(k), Value: v})
	}
	return &pb.Dict{Pairs: pairs}, nil
}

func encodeTime(t time.Time) *pb.MontyObject {
	name, offset := t.Zone()
	off := int32(offset)
	dt := &pb.DateTime{
		Year:          int32(t.Year()),
		Month:         uint32(t.Month()),
		Day:           uint32(t.Day()),
		Hour:          uint32(t.Hour()),
		Minute:        uint32(t.Minute()),
		Second:        uint32(t.Second()),
		Microsecond:   uint32(t.Nanosecond() / 1000),
		OffsetSeconds: &off,
	}
	if name != "" {
		dt.TimezoneName = &name
	}
	return &pb.MontyObject{Kind: &pb.MontyObject_Datetime{Datetime: dt}}
}

func encodeDuration(d time.Duration) *pb.MontyObject {
	micros := d.Microseconds()
	days := micros / (86400 * 1_000_000)
	micros -= days * 86400 * 1_000_000
	if micros < 0 {
		days--
		micros += 86400 * 1_000_000
	}
	secs := micros / 1_000_000
	micros -= secs * 1_000_000
	return &pb.MontyObject{Kind: &pb.MontyObject_Timedelta{Timedelta: &pb.TimeDelta{
		Days: int32(days), Seconds: int32(secs), Microseconds: int32(micros),
	}}}
}

func encodeException(e *Exception) *pb.MontyObject {
	exc := &pb.Exception{ExcType: e.Type}
	if e.Message != "" {
		msg := e.Message
		exc.Arg = &msg
	}
	return &pb.MontyObject{Kind: &pb.MontyObject_Exception{Exception: exc}}
}

// encode converts a Go value into its wire representation.
func (c *codec) encode(v any) (*pb.MontyObject, error) {
	switch x := v.(type) {
	case nil:
		return &pb.MontyObject{Kind: &pb.MontyObject_None{None: unit()}}, nil
	case bool:
		return &pb.MontyObject{Kind: &pb.MontyObject_Boolean{Boolean: x}}, nil
	case int:
		return encodeInt(int64(x)), nil
	case int8:
		return encodeInt(int64(x)), nil
	case int16:
		return encodeInt(int64(x)), nil
	case int32:
		return encodeInt(int64(x)), nil
	case int64:
		return encodeInt(x), nil
	case uint:
		return encodeBig(new(big.Int).SetUint64(uint64(x))), nil
	case uint8:
		return encodeInt(int64(x)), nil
	case uint16:
		return encodeInt(int64(x)), nil
	case uint32:
		return encodeInt(int64(x)), nil
	case uint64:
		return encodeBig(new(big.Int).SetUint64(x)), nil
	case uintptr:
		return encodeBig(new(big.Int).SetUint64(uint64(x))), nil
	case *big.Int:
		if x == nil {
			return c.encode(nil)
		}
		return encodeBig(x), nil
	case big.Int:
		return encodeBig(&x), nil
	case float32:
		return &pb.MontyObject{Kind: &pb.MontyObject_Float{Float: float64(x)}}, nil
	case float64:
		return &pb.MontyObject{Kind: &pb.MontyObject_Float{Float: x}}, nil
	case string:
		if !utf8.ValidString(x) {
			return nil, fmt.Errorf("monty: string is not valid UTF-8")
		}
		return encodeStr(x), nil
	case []byte:
		return &pb.MontyObject{Kind: &pb.MontyObject_Bytes{Bytes: x}}, nil
	case []any:
		l, err := c.encodeList(x)
		if err != nil {
			return nil, err
		}
		return &pb.MontyObject{Kind: &pb.MontyObject_List{List: l}}, nil
	case Tuple:
		l, err := c.encodeList(x)
		if err != nil {
			return nil, err
		}
		return &pb.MontyObject{Kind: &pb.MontyObject_Tuple{Tuple: l}}, nil
	case Set:
		l, err := c.encodeList(x)
		if err != nil {
			return nil, err
		}
		return &pb.MontyObject{Kind: &pb.MontyObject_Set{Set: l}}, nil
	case FrozenSet:
		l, err := c.encodeList(x)
		if err != nil {
			return nil, err
		}
		return &pb.MontyObject{Kind: &pb.MontyObject_FrozenSet{FrozenSet: l}}, nil
	case map[string]any:
		d, err := c.encodeStringMap(x)
		if err != nil {
			return nil, err
		}
		return &pb.MontyObject{Kind: &pb.MontyObject_Dict{Dict: d}}, nil
	case Dict:
		pairs := make([]*pb.Pair, 0, len(x))
		for i, item := range x {
			k, err := c.encode(item.Key)
			if err != nil {
				return nil, fmt.Errorf("dict key %d: %w", i, err)
			}
			val, err := c.encode(item.Value)
			if err != nil {
				return nil, fmt.Errorf("dict value %d: %w", i, err)
			}
			pairs = append(pairs, &pb.Pair{Key: k, Value: val})
		}
		return &pb.MontyObject{Kind: &pb.MontyObject_Dict{Dict: &pb.Dict{Pairs: pairs}}}, nil
	case NamedTuple:
		l, err := c.encodeList(x.Values)
		if err != nil {
			return nil, err
		}
		return &pb.MontyObject{Kind: &pb.MontyObject_NamedTuple{NamedTuple: &pb.NamedTuple{
			TypeName: x.TypeName, FieldNames: x.Fields, Values: l.Items,
		}}}, nil
	case time.Time:
		return encodeTime(x), nil
	case *time.Time:
		if x == nil {
			return c.encode(nil)
		}
		return encodeTime(*x), nil
	case time.Duration:
		return encodeDuration(x), nil
	case TimeDelta:
		return &pb.MontyObject{Kind: &pb.MontyObject_Timedelta{Timedelta: &pb.TimeDelta{
			Days: int32(x.Days), Seconds: int32(x.Seconds), Microseconds: int32(x.Microseconds),
		}}}, nil
	case Date:
		return &pb.MontyObject{Kind: &pb.MontyObject_Date{Date: &pb.Date{
			Year: int32(x.Year), Month: uint32(x.Month), Day: uint32(x.Day),
		}}}, nil
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
		return &pb.MontyObject{Kind: &pb.MontyObject_Time{Time: t}}, nil
	case *time.Location:
		if x == nil {
			return c.encode(nil)
		}
		name, offset := time.Now().In(x).Zone()
		tz := &pb.TimeZone{OffsetSeconds: int32(offset)}
		if name != "" {
			tz.Name = &name
		}
		return &pb.MontyObject{Kind: &pb.MontyObject_Timezone{Timezone: tz}}, nil
	case *Exception:
		if x == nil {
			return c.encode(nil)
		}
		return encodeException(x), nil
	case Exception:
		return encodeException(&x), nil
	case ellipsis:
		return &pb.MontyObject{Kind: &pb.MontyObject_Ellipsis{Ellipsis: unit()}}, nil
	case notImplemented:
		return &pb.MontyObject{Kind: &pb.MontyObject_NotImplemented{NotImplemented: unit()}}, nil
	case Path:
		return &pb.MontyObject{Kind: &pb.MontyObject_Path{Path: string(x)}}, nil
	case FileHandle:
		return &pb.MontyObject{Kind: &pb.MontyObject_FileHandle{FileHandle: &pb.FileHandle{
			Path: x.Path, Mode: x.Mode, Position: x.Position,
		}}}, nil
	case FunctionRef:
		if x.Builtin {
			return &pb.MontyObject{Kind: &pb.MontyObject_BuiltinFunction{BuiltinFunction: x.Name}}, nil
		}
		fn := &pb.Function{Name: x.Name}
		if x.Doc != "" {
			doc := x.Doc
			fn.Docstring = &doc
		}
		return &pb.MontyObject{Kind: &pb.MontyObject_Function{Function: fn}}, nil
	case TypeRef:
		if x.Origin != TypeOriginBuiltin && x.Origin != 0 {
			return nil, fmt.Errorf("monty: only builtin types can be passed into the sandbox, not %q", x.Name)
		}
		return &pb.MontyObject{Kind: &pb.MontyObject_Type{Type: &pb.Type{
			Name: x.Name, Origin: pb.TypeOrigin_TYPE_ORIGIN_BUILTIN,
		}}}, nil
	case Repr:
		return nil, fmt.Errorf("monty: a Repr value cannot be passed into the sandbox")
	case *ClassInstance:
		return nil, fmt.Errorf("monty: sandbox class instances cannot be passed back into the sandbox")
	case *HostObject:
		return c.encodeObject(x)
	case *HostClass:
		if x == nil {
			return c.encode(nil)
		}
		t, err := c.encodeClass(x, true)
		if err != nil {
			return nil, err
		}
		return &pb.MontyObject{Kind: &pb.MontyObject_Type{Type: t}}, nil
	case error:
		return encodeException(&Exception{Type: "RuntimeError", Message: x.Error()}), nil
	}
	return c.encodeReflect(reflect.ValueOf(v))
}

func (c *codec) encodeReflect(rv reflect.Value) (*pb.MontyObject, error) {
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return c.encode(nil)
		}
		return c.encode(rv.Elem().Interface())
	case reflect.Bool:
		return c.encode(rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return c.encode(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return c.encode(rv.Uint())
	case reflect.Float32, reflect.Float64:
		return c.encode(rv.Float())
	case reflect.String:
		return c.encode(rv.String())
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
			return c.encode(rv.Bytes())
		}
		items := make([]any, rv.Len())
		for i := range items {
			items[i] = rv.Index(i).Interface()
		}
		return c.encode(items)
	case reflect.Struct:
		return c.encode(structToMap(rv))
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("monty: cannot convert map with %s keys; use monty.Dict", rv.Type().Key())
		}
		m := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			m[iter.Key().String()] = iter.Value().Interface()
		}
		return c.encode(m)
	}
	return nil, fmt.Errorf("monty: cannot convert Go value of type %s", rv.Type())
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

// encodeClass builds the wire type for a host class, sending its Attrs the
// first time the session sees the class (or whenever withAttrs asks).
func (c *codec) encodeClass(cl *HostClass, withAttrs bool) (*pb.Type, error) {
	if c.store == nil {
		return nil, fmt.Errorf("monty: host objects can only be passed through a session")
	}
	c.store.registerClass(cl)
	t := &pb.Type{
		Name:        cl.Name,
		Id:          &pb.Uuid{Data: uuidBytes(cl.ID())},
		Origin:      pb.TypeOrigin_TYPE_ORIGIN_HOST,
		IsDataclass: cl.IsDataclass,
	}
	if withAttrs && len(cl.Attrs) > 0 && !c.store.sentClasses[cl.ID()] {
		attrs, err := c.encodeStringMap(cl.Attrs)
		if err != nil {
			return nil, fmt.Errorf("class %s attrs: %w", cl.Name, err)
		}
		t.Attrs = attrs
		c.store.sentClasses[cl.ID()] = true
	}
	return t, nil
}

// encodeObject builds the wire instance for a host object.
func (c *codec) encodeObject(o *HostObject) (*pb.MontyObject, error) {
	if o == nil {
		return c.encode(nil)
	}
	if o.Class == nil {
		return nil, fmt.Errorf("monty: host object has no class")
	}
	if c.store == nil {
		return nil, fmt.Errorf("monty: host objects can only be passed through a session")
	}
	t, err := c.encodeClass(o.Class, true)
	if err != nil {
		return nil, err
	}
	c.store.registerObject(o)
	attrs, err := c.encodeStringMap(o.Attrs)
	if err != nil {
		return nil, fmt.Errorf("object attrs: %w", err)
	}
	return &pb.MontyObject{Kind: &pb.MontyObject_ClassInstance{ClassInstance: &pb.ClassInstance{
		Type:       t,
		InstanceId: &pb.Uuid{Data: uuidBytes(o.ID())},
		Attrs:      attrs,
	}}}, nil
}

// ---------------------------------------------------------------------------
// protobuf → Go
// ---------------------------------------------------------------------------

func (c *codec) decodeList(l *pb.ObjectList) ([]any, error) {
	if l == nil {
		return []any{}, nil
	}
	out := make([]any, 0, len(l.Items))
	for _, item := range l.Items {
		v, err := c.decode(item)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (c *codec) decodeDict(d *pb.Dict) (any, error) {
	if d == nil {
		return map[string]any{}, nil
	}
	items := make(Dict, 0, len(d.Pairs))
	allStrings := true
	for _, p := range d.Pairs {
		k, err := c.decode(p.Key)
		if err != nil {
			return nil, err
		}
		v, err := c.decode(p.Value)
		if err != nil {
			return nil, err
		}
		if _, ok := k.(string); !ok {
			allStrings = false
		}
		items = append(items, DictItem{Key: k, Value: v})
	}
	if !allStrings {
		return items, nil
	}
	m := make(map[string]any, len(items))
	for _, item := range items {
		m[item.Key.(string)] = item.Value
	}
	return m, nil
}

// decodeStringDict decodes a dict whose keys must all be strings.
func (c *codec) decodeStringDict(d *pb.Dict) (map[string]any, error) {
	v, err := c.decodeDict(d)
	if err != nil {
		return nil, err
	}
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	return nil, fmt.Errorf("monty: dict has non-string keys")
}

func uuidString(u *pb.Uuid) string {
	if u == nil || len(u.Data) != 16 {
		return ""
	}
	h := hex.EncodeToString(u.Data)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func (c *codec) decodeType(t *pb.Type) (TypeRef, error) {
	if t == nil {
		return TypeRef{}, fmt.Errorf("monty: empty type")
	}
	attrs, err := c.decodeStringDict(t.Attrs)
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

// decode converts a wire value into its Go representation.
func (c *codec) decode(obj *pb.MontyObject) (any, error) {
	if obj == nil {
		return nil, nil
	}
	switch k := obj.Kind.(type) {
	case nil, *pb.MontyObject_None:
		return nil, nil
	case *pb.MontyObject_Ellipsis:
		return Ellipsis, nil
	case *pb.MontyObject_NotImplemented:
		return NotImplemented, nil
	case *pb.MontyObject_Boolean:
		return k.Boolean, nil
	case *pb.MontyObject_Int:
		return k.Int, nil
	case *pb.MontyObject_Bigint:
		b := new(big.Int).SetBytes(k.Bigint.GetMagnitude())
		if k.Bigint.GetNegative() {
			b.Neg(b)
		}
		return b, nil
	case *pb.MontyObject_Float:
		return k.Float, nil
	case *pb.MontyObject_Str:
		return k.Str, nil
	case *pb.MontyObject_Bytes:
		return k.Bytes, nil
	case *pb.MontyObject_List:
		return c.decodeList(k.List)
	case *pb.MontyObject_Tuple:
		items, err := c.decodeList(k.Tuple)
		if err != nil {
			return nil, err
		}
		return Tuple(items), nil
	case *pb.MontyObject_Set:
		items, err := c.decodeList(k.Set)
		if err != nil {
			return nil, err
		}
		return Set(items), nil
	case *pb.MontyObject_FrozenSet:
		items, err := c.decodeList(k.FrozenSet)
		if err != nil {
			return nil, err
		}
		return FrozenSet(items), nil
	case *pb.MontyObject_NamedTuple:
		values, err := c.decodeList(&pb.ObjectList{Items: k.NamedTuple.GetValues()})
		if err != nil {
			return nil, err
		}
		return NamedTuple{
			TypeName: k.NamedTuple.GetTypeName(),
			Fields:   k.NamedTuple.GetFieldNames(),
			Values:   values,
		}, nil
	case *pb.MontyObject_Dict:
		return c.decodeDict(k.Dict)
	case *pb.MontyObject_Date:
		d := k.Date
		return Date{Year: int(d.GetYear()), Month: time.Month(d.GetMonth()), Day: int(d.GetDay())}, nil
	case *pb.MontyObject_Time:
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
	case *pb.MontyObject_Datetime:
		dt := k.Datetime
		loc := time.UTC
		if dt.OffsetSeconds != nil {
			loc = time.FixedZone(dt.GetTimezoneName(), int(*dt.OffsetSeconds))
		}
		return time.Date(int(dt.GetYear()), time.Month(dt.GetMonth()), int(dt.GetDay()),
			int(dt.GetHour()), int(dt.GetMinute()), int(dt.GetSecond()),
			int(dt.GetMicrosecond())*1000, loc), nil
	case *pb.MontyObject_Timedelta:
		td := k.Timedelta
		days, secs, micros := int64(td.GetDays()), int64(td.GetSeconds()), int64(td.GetMicroseconds())
		totalMicros := days*86400*1_000_000 + secs*1_000_000 + micros
		const maxMicros = math.MaxInt64 / 1000
		if days > maxMicros/(86400*1_000_000) || days < -maxMicros/(86400*1_000_000) ||
			totalMicros > maxMicros || totalMicros < -maxMicros {
			return TimeDelta{Days: int(days), Seconds: int(secs), Microseconds: int(micros)}, nil
		}
		return time.Duration(totalMicros) * time.Microsecond, nil
	case *pb.MontyObject_Timezone:
		return time.FixedZone(k.Timezone.GetName(), int(k.Timezone.GetOffsetSeconds())), nil
	case *pb.MontyObject_Exception:
		return &Exception{Type: k.Exception.GetExcType(), Message: k.Exception.GetArg()}, nil
	case *pb.MontyObject_Type:
		if c.store != nil && k.Type.GetOrigin() == pb.TypeOrigin_TYPE_ORIGIN_HOST {
			if cl, ok := c.store.classes[uuidString(k.Type.GetId())]; ok {
				return cl, nil
			}
		}
		return c.decodeType(k.Type)
	case *pb.MontyObject_ClassInstance:
		ci := k.ClassInstance
		if c.store != nil && ci.GetType().GetOrigin() == pb.TypeOrigin_TYPE_ORIGIN_HOST {
			if o, ok := c.store.objects[uuidString(ci.GetInstanceId())]; ok {
				return o, nil
			}
		}
		t, err := c.decodeType(ci.GetType())
		if err != nil {
			return nil, err
		}
		attrs, err := c.decodeStringDict(ci.GetAttrs())
		if err != nil {
			return nil, err
		}
		return &ClassInstance{Type: t, ID: uuidString(ci.GetInstanceId()), Attrs: attrs}, nil
	case *pb.MontyObject_Function:
		return FunctionRef{Name: k.Function.GetName(), Doc: k.Function.GetDocstring()}, nil
	case *pb.MontyObject_BuiltinFunction:
		return FunctionRef{Name: k.BuiltinFunction, Builtin: true}, nil
	case *pb.MontyObject_Path:
		return Path(k.Path), nil
	case *pb.MontyObject_FileHandle:
		fh := k.FileHandle
		return FileHandle{Path: fh.GetPath(), Mode: fh.GetMode(), Position: fh.GetPosition()}, nil
	case *pb.MontyObject_Repr:
		return Repr(k.Repr), nil
	case *pb.MontyObject_Cycle:
		return Repr(k.Cycle.GetPlaceholder()), nil
	case *pb.MontyObject_Uuid:
		return nil, fmt.Errorf("monty: uuid values are not supported")
	}
	return nil, fmt.Errorf("monty: unknown value kind %T", obj.Kind)
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
