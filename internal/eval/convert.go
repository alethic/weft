package eval

import (
	"fmt"
	"sort"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// maxDepth bounds nesting in both directions. Resource bodies are shallow; a
// structure deep enough to hit this is either a mistake or an attempt to
// exhaust the stack during conversion, where a Starlark step budget no longer
// applies because the work is happening in Go.
const maxDepth = 100

// toStarlark converts a decoded JSON value into a Starlark value.
//
// The input is expected to hold only the types unstructured.Unstructured
// guarantees: map[string]any, []any, string, bool, float64, int64 and nil.
// Integers survive as integers precisely because that guarantee holds; nothing
// here re-derives intness from a float, which would silently rewrite a genuine
// floating-point field.
func toStarlark(v any) (starlark.Value, error) { return toStarlarkDepth(v, 0) }

func toStarlarkDepth(v any, depth int) (starlark.Value, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("value nested deeper than %d levels", maxDepth)
	}
	switch t := v.(type) {
	case nil:
		return starlark.None, nil
	case bool:
		return starlark.Bool(t), nil
	case string:
		return starlark.String(t), nil
	case int:
		return starlark.MakeInt(t), nil
	case int32:
		return starlark.MakeInt64(int64(t)), nil
	case int64:
		return starlark.MakeInt64(t), nil
	case float64:
		return starlark.Float(t), nil
	case float32:
		return starlark.Float(float64(t)), nil
	case []any:
		items := make([]starlark.Value, 0, len(t))
		for i, e := range t {
			c, err := toStarlarkDepth(e, depth+1)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			items = append(items, c)
		}
		l := starlark.NewList(items)
		l.Freeze()
		return l, nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		// Go map iteration is unordered; sorting makes rendering, iteration and
		// error messages deterministic across reconciles.
		sort.Strings(keys)
		m := make(map[string]starlark.Value, len(t))
		for _, k := range keys {
			c, err := toStarlarkDepth(t[k], depth+1)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			m[k] = c
		}
		return NewObject(keys, m), nil
	default:
		return nil, fmt.Errorf("cannot expose Go value of type %T to a program", v)
	}
}

// objectToStarlark converts a map into an Object, mapping a nil map to an empty
// Object rather than None so that programs can iterate it unconditionally.
func objectToStarlark(m map[string]any) (starlark.Value, error) {
	if m == nil {
		return NewObject(nil, nil), nil
	}
	return toStarlark(m)
}

// budget bounds the size of a converted result. compose() runs under a step
// budget, but a program can produce an enormous structure in few steps, and the
// cost of that structure lands on the API server rather than on us.
type budget struct {
	nodes    int
	maxNodes int
}

func (b *budget) charge() error {
	b.nodes++
	if b.maxNodes > 0 && b.nodes > b.maxNodes {
		return fmt.Errorf("result exceeds %d values", b.maxNodes)
	}
	return nil
}

// fromStarlark converts a Starlark value back into plain Go data.
func fromStarlark(v starlark.Value, b *budget) (any, error) {
	return fromStarlarkDepth(v, b, 0)
}

func fromStarlarkDepth(v starlark.Value, b *budget, depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("value nested deeper than %d levels", maxDepth)
	}
	if err := b.charge(); err != nil {
		return nil, err
	}
	switch t := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(t), nil
	case starlark.String:
		return string(t), nil
	case starlark.Int:
		i, ok := t.Int64()
		if !ok {
			return nil, fmt.Errorf("integer %s does not fit in 64 bits", t.String())
		}
		return i, nil
	case starlark.Float:
		return float64(t), nil
	case *starlark.List:
		out := make([]any, 0, t.Len())
		for i := 0; i < t.Len(); i++ {
			e, err := fromStarlarkDepth(t.Index(i), b, depth+1)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out = append(out, e)
		}
		return out, nil
	case starlark.Tuple:
		out := make([]any, 0, t.Len())
		for i := 0; i < t.Len(); i++ {
			e, err := fromStarlarkDepth(t.Index(i), b, depth+1)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out = append(out, e)
		}
		return out, nil
	case *starlark.Dict:
		out := make(map[string]any, t.Len())
		for _, item := range t.Items() {
			k, ok := starlark.AsString(item[0])
			if !ok {
				return nil, fmt.Errorf("mapping key must be a string, got %s", item[0].Type())
			}
			e, err := fromStarlarkDepth(item[1], b, depth+1)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = e
		}
		return out, nil
	case *Object:
		out := make(map[string]any, len(t.keys))
		for _, k := range t.keys {
			e, err := fromStarlarkDepth(t.m[k], b, depth+1)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = e
		}
		return out, nil
	case *starlarkstruct.Struct:
		out := make(map[string]any, len(t.AttrNames()))
		for _, k := range t.AttrNames() {
			av, err := t.Attr(k)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			e, err := fromStarlarkDepth(av, b, depth+1)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = e
		}
		return out, nil
	case *waitValue:
		return nil, fmt.Errorf("wait() may only be returned from compose(), not embedded in a resource")
	default:
		return nil, fmt.Errorf("cannot serialise a %s", v.Type())
	}
}

// asStringMap converts a value expected to be a JSON object.
func asStringMap(v starlark.Value, b *budget) (map[string]any, error) {
	out, err := fromStarlark(v, b)
	if err != nil {
		return nil, err
	}
	m, ok := out.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected a mapping, got %s", v.Type())
	}
	return m, nil
}
