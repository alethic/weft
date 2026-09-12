package eval

import (
	"fmt"
	"sort"
	"strings"

	"go.starlark.net/starlark"
)

// Object is an immutable JSON object exposed to a program.
//
// It is both a mapping and an attribute holder, so obj.status.atProvider.id and
// obj["crossplane.io/external-name"] are both spelled naturally. That matters
// more than it looks: half the fields these compositions read are dotted status
// paths that want attribute syntax, and the other half are annotation keys that
// are not identifiers and cannot have it. Supporting only one of the two makes
// every program that touches the other read badly.
//
// Objects are immutable by construction, which is what "freeze the injected
// values" amounts to here: there is no mutating operation to guard.
type Object struct {
	keys []string
	m    map[string]starlark.Value
}

// Compile-time interface assertions.
var (
	_ starlark.Value    = (*Object)(nil)
	_ starlark.HasAttrs = (*Object)(nil)
	_ starlark.Mapping  = (*Object)(nil)
	_ starlark.Sequence = (*Object)(nil)
)

// NewObject builds an Object from an ordered key list and a value map. The
// caller retains no reference to either.
func NewObject(keys []string, m map[string]starlark.Value) *Object {
	return &Object{keys: keys, m: m}
}

// Type implements starlark.Value.
func (o *Object) Type() string { return "object" }

// Freeze implements starlark.Value. Objects are already immutable.
func (o *Object) Freeze() {}

// Truth implements starlark.Value: an empty object is falsy, like a dict.
func (o *Object) Truth() starlark.Bool { return starlark.Bool(len(o.keys) > 0) }

// Hash implements starlark.Value. Objects are unhashable, like dicts.
func (o *Object) Hash() (uint32, error) {
	return 0, fmt.Errorf("unhashable type: object")
}

// String implements starlark.Value, rendering in dict syntax.
func (o *Object) String() string {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(starlark.String(k).String())
		b.WriteString(": ")
		b.WriteString(o.m[k].String())
	}
	b.WriteByte('}')
	return b.String()
}

// Len implements starlark.Sequence.
func (o *Object) Len() int { return len(o.keys) }

// Iterate implements starlark.Iterable, yielding keys like a dict does.
func (o *Object) Iterate() starlark.Iterator { return &objectIterator{o: o} }

type objectIterator struct {
	o *Object
	i int
}

func (it *objectIterator) Next(p *starlark.Value) bool {
	if it.i >= len(it.o.keys) {
		return false
	}
	*p = starlark.String(it.o.keys[it.i])
	it.i++
	return true
}

func (it *objectIterator) Done() {}

// Get implements starlark.Mapping, backing both obj[k] and `k in obj`.
func (o *Object) Get(k starlark.Value) (starlark.Value, bool, error) {
	s, ok := starlark.AsString(k)
	if !ok {
		return nil, false, fmt.Errorf("object keys are strings, got %s", k.Type())
	}
	v, found := o.m[s]
	return v, found, nil
}

// Lookup returns a field by name without going through Starlark values.
func (o *Object) Lookup(name string) (starlark.Value, bool) {
	v, ok := o.m[name]
	return v, ok
}

// Keys returns the field names in their original order.
func (o *Object) Keys() []string { return o.keys }

var objectMethods = map[string]*starlark.Builtin{}

func init() {
	objectMethods["get"] = starlark.NewBuiltin("get", objectGet)
	objectMethods["keys"] = starlark.NewBuiltin("keys", objectKeys)
	objectMethods["values"] = starlark.NewBuiltin("values", objectValues)
	objectMethods["items"] = starlark.NewBuiltin("items", objectItems)
}

// Attr implements starlark.HasAttrs.
//
// A missing field is None. These are ordinary objects and a field that is not
// set reads as nothing, which is what a program navigating into status has to
// deal with constantly: a provider writes it back when it gets to it, and until
// then it is simply not there.
func (o *Object) Attr(name string) (starlark.Value, error) {
	if v, ok := o.m[name]; ok {
		return v, nil
	}
	if m, ok := objectMethods[name]; ok {
		return m.BindReceiver(o), nil
	}
	return starlark.None, nil
}

// AttrNames implements starlark.HasAttrs.
func (o *Object) AttrNames() []string {
	names := make([]string, 0, len(o.keys)+len(objectMethods))
	names = append(names, o.keys...)
	for name := range objectMethods {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// receiverObject recovers the object a bound method was called on. The
// assertion is checked rather than assumed: an unchecked one here would be a
// panic inside the interpreter, which takes down the reconcile rather than
// failing the Weave.
func receiverObject(b *starlark.Builtin) (*Object, error) {
	o, ok := b.Receiver().(*Object)
	if !ok {
		return nil, fmt.Errorf("%s: receiver is a %s, not an object", b.Name(), b.Receiver().Type())
	}
	return o, nil
}

func objectGet(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var key starlark.Value
	def := starlark.Value(starlark.None)
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 1, &key, &def); err != nil {
		return nil, err
	}
	o, err := receiverObject(b)
	if err != nil {
		return nil, err
	}
	v, found, err := o.Get(key)
	if err != nil {
		return nil, err
	}
	if !found {
		return def, nil
	}
	return v, nil
}

func objectKeys(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 0); err != nil {
		return nil, err
	}
	o, err := receiverObject(b)
	if err != nil {
		return nil, err
	}
	items := make([]starlark.Value, 0, len(o.keys))
	for _, k := range o.keys {
		items = append(items, starlark.String(k))
	}
	return starlark.NewList(items), nil
}

func objectValues(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 0); err != nil {
		return nil, err
	}
	o, err := receiverObject(b)
	if err != nil {
		return nil, err
	}
	items := make([]starlark.Value, 0, len(o.keys))
	for _, k := range o.keys {
		items = append(items, o.m[k])
	}
	return starlark.NewList(items), nil
}

func objectItems(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 0); err != nil {
		return nil, err
	}
	o, err := receiverObject(b)
	if err != nil {
		return nil, err
	}
	items := make([]starlark.Value, 0, len(o.keys))
	for _, k := range o.keys {
		items = append(items, starlark.Tuple{starlark.String(k), o.m[k]})
	}
	return starlark.NewList(items), nil
}
