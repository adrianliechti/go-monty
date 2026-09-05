package monty

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
)

// ErrUndefined may be returned by a HostClass's Lazy hook to report that an
// attribute does not exist; the sandbox then raises AttributeError.
var ErrUndefined = errors.New("monty: attribute undefined")

// Method is a method of a HostClass, invoked by sandbox code on an instance.
// The receiver is obj; call carries the remaining arguments.
type Method func(ctx context.Context, obj *HostObject, call *Call) (any, error)

// HostClass describes a Go-backed class the sandbox can see. Instances are
// HostObjects; passing a *HostClass itself makes the class available, e.g.
// so code can call class methods or, when Init is set, construct instances.
//
// Method calls, lazy attribute reads and construction all run on the host,
// with your authority. Sandbox code that sets attributes changes only its
// own copy; the host object is never touched.
type HostClass struct {
	// Name is the Python class name.
	Name string
	// Attrs are class-level constants, sent eagerly with the class.
	Attrs map[string]any
	// Methods are callable on instances.
	Methods map[string]Method
	// ClassMethods are callable on the class itself.
	ClassMethods map[string]Function
	// Lazy resolves instance attributes that are not in the instance's Attrs
	// at the moment the code reads them. Return ErrUndefined for a missing
	// attribute; any other error is raised in the sandbox.
	Lazy func(ctx context.Context, obj *HostObject, name string) (any, error)
	// Init lets sandbox code construct instances by calling the class. nil
	// forbids construction with a TypeError.
	Init func(ctx context.Context, call *Call) (*HostObject, error)
	// IsDataclass makes dataclasses.is_dataclass report true for instances.
	IsDataclass bool

	id string
}

// ID is the class's identity inside sandboxes, assigned on first use.
func (c *HostClass) ID() string {
	if c.id == "" {
		c.id = newUUID()
	}
	return c.id
}

// HostObject is an instance of a HostClass. Attrs are sent to the sandbox
// with the object; Value is whatever Go value the object stands for, for
// the host's own use in methods.
type HostObject struct {
	Class *HostClass
	Attrs map[string]any
	Value any

	id string
}

// NewObject creates an instance of class backed by value, exposing attrs.
func NewObject(class *HostClass, value any, attrs map[string]any) *HostObject {
	return &HostObject{Class: class, Value: value, Attrs: attrs}
}

// ID is the object's identity inside sandboxes, assigned on first use.
func (o *HostObject) ID() string {
	if o.id == "" {
		o.id = newUUID()
	}
	return o.id
}

// objectStore is a session's registry of host objects and classes, keyed by
// their uuid strings, so calls routed by id find the Go side again.
type objectStore struct {
	objects map[string]*HostObject
	classes map[string]*HostClass
	// sentClasses records classes whose Attrs went to the sandbox already.
	sentClasses map[string]bool
}

func newObjectStore() *objectStore {
	return &objectStore{
		objects:     map[string]*HostObject{},
		classes:     map[string]*HostClass{},
		sentClasses: map[string]bool{},
	}
}

func (st *objectStore) registerClass(c *HostClass) {
	st.classes[c.ID()] = c
}

func (st *objectStore) registerObject(o *HostObject) {
	st.objects[o.ID()] = o
	if o.Class != nil {
		st.registerClass(o.Class)
	}
}

// newUUID returns a random version 4 uuid in canonical form.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("monty: random source failed: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// uuidBytes parses a canonical uuid string back into 16 bytes.
func uuidBytes(s string) []byte {
	clean := make([]byte, 0, 32)
	for i := 0; i < len(s); i++ {
		if s[i] != '-' {
			clean = append(clean, s[i])
		}
	}
	b, err := hex.DecodeString(string(clean))
	if err != nil || len(b) != 16 {
		return nil
	}
	return b
}
