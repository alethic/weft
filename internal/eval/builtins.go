package eval

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"go.starlark.net/starlark"
	yaml "go.yaml.in/yaml/v3"
)

// waitLocalKey is where require() stashes the wait it raised, so the caller can
// tell a deliberate "not yet" from a genuine program fault after the fact.
// Threads are per-evaluation, so this is not shared state.
const waitLocalKey = "weft.wait"

// pendingLocalKey accumulates the reasons pending() was called with.
const pendingLocalKey = "weft.pending"

// waitValue is the sentinel wait() returns. Returning it from compose() means
// the program declined to produce a result, with a reason.
type waitValue struct{ reason string }

var _ starlark.Value = (*waitValue)(nil)

func (w *waitValue) Type() string          { return "wait" }
func (w *waitValue) Freeze()               {}
func (w *waitValue) Truth() starlark.Bool  { return starlark.True }
func (w *waitValue) String() string        { return fmt.Sprintf("wait(%q)", w.reason) }
func (w *waitValue) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable type: wait") }

// errWaitRaised is returned by require() to unwind evaluation. The reason
// travels on the thread rather than in the message, so it survives Starlark's
// error wrapping intact.
type errWaitRaised struct{ reason string }

func (e *errWaitRaised) Error() string { return e.reason }

// builtins returns the predeclared environment for a program.
//
// Everything here is a pure function of its arguments. There is no clock, no
// randomness, no I/O and no way to reach the process: an author is assumed to
// be untrusted, so anything non-deterministic is also a way to make a
// composition that behaves differently on each reconcile.
func builtins() starlark.StringDict {
	return starlark.StringDict{
		"get":       starlark.NewBuiltin("get", bGet),
		"has":       starlark.NewBuiltin("has", bHas),
		"require":   starlark.NewBuiltin("require", bRequire),
		"wait":      starlark.NewBuiltin("wait", bWait),
		"pending":   starlark.NewBuiltin("pending", bPending),
		"to_yaml":   starlark.NewBuiltin("to_yaml", bToYAML),
		"from_yaml": starlark.NewBuiltin("from_yaml", bFromYAML),
		"to_json":   starlark.NewBuiltin("to_json", bToJSON),
		"from_json": starlark.NewBuiltin("from_json", bFromJSON),
		"b64encode": starlark.NewBuiltin("b64encode", bB64Encode),
		"b64decode": starlark.NewBuiltin("b64decode", bB64Decode),
		"sha256":    starlark.NewBuiltin("sha256", bSHA256),
	}
}

// predeclaredNames is the set of names resolvable outside the Starlark
// universe.
//
// The parameters of compose() are deliberately absent: they are parameters, not
// globals. Leaving them out turns a module-level reference into "undefined:
// variable" at compile time instead of a confusing lookup failure during
// evaluation.
var predeclaredNames = func() map[string]bool {
	m := map[string]bool{}
	for k := range builtins() {
		m[k] = true
	}
	return m
}()

// bGet reads a field path, yielding a default when any link is missing.
//
// Registering this early is not a nicety. Without it every nested read becomes
// a defensive chain of .get() calls and the programs stop being readable, which
// is most of what makes this approach worth choosing over YAML in the first
// place.
func bGet(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var obj, path starlark.Value
	def := starlark.Value(starlark.None)
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "obj", &obj, "path", &path, "default?", &def); err != nil {
		return nil, err
	}
	segs, err := pathFromValue(path)
	if err != nil {
		return nil, err
	}
	v, ok, _ := resolvePath(obj, segs)
	if !ok || v == nil {
		return def, nil
	}
	return v, nil
}

// bHas reports whether a field path resolves to a value other than None.
func bHas(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var obj, path starlark.Value
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "obj", &obj, "path", &path); err != nil {
		return nil, err
	}
	segs, err := pathFromValue(path)
	if err != nil {
		return nil, err
	}
	v, ok, _ := resolvePath(obj, segs)
	return starlark.Bool(ok && v != nil && v != starlark.None), nil
}

// bRequire reads a field path that must be resolved, and stops evaluation with
// a wait when it is not.
//
// Existence does not unblock; field resolution does. An applied managed
// resource returns immediately but its provider status populates minutes later,
// so a resource we created behaves exactly like an external source that is not
// ready. One rule covers both.
func bRequire(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var obj, path starlark.Value
	var name string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "obj", &obj, "path", &path, "name?", &name); err != nil {
		return nil, err
	}
	segs, err := pathFromValue(path)
	if err != nil {
		return nil, err
	}
	subject := name
	if subject == "" {
		subject = describe(obj)
	}

	v, ok, resolved := resolvePath(obj, segs)
	if ok && v != nil && v != starlark.None {
		// An empty string is not a resolved value. Provider status fields are
		// routinely present-but-empty between the apply and the write-back.
		if s, isStr := starlark.AsString(v); !isStr || s != "" {
			return v, nil
		}
		return nil, raiseWait(t, fmt.Sprintf("%s is empty on %s", pathString(segs), subject))
	}

	if obj == nil || obj == starlark.None {
		return nil, raiseWait(t, fmt.Sprintf("%s does not exist yet (needed for %s)", subject, pathString(segs)))
	}
	// Name the link that actually failed rather than the whole path, so the
	// condition points at the field that has not been written back.
	missing := pathString(segs[:resolved+1])
	return nil, raiseWait(t, fmt.Sprintf("%s is not set on %s", missing, subject))
}

func raiseWait(t *starlark.Thread, reason string) error {
	t.SetLocal(waitLocalKey, &Wait{Reason: reason})
	return &errWaitRaised{reason: reason}
}

// describe renders a short identifier for a resource, used in wait reasons.
func describe(v starlark.Value) string {
	if v == nil || v == starlark.None {
		return "the resource"
	}
	o, ok := v.(*Object)
	if !ok {
		return "the " + v.Type()
	}
	kind := lookupString(o, "kind")
	name := lookupString(o, "metadata", "name")
	switch {
	case kind != "" && name != "":
		return kind + "/" + name
	case kind != "":
		return kind
	case name != "":
		return name
	default:
		return "the resource"
	}
}

func lookupString(o *Object, path ...string) string {
	var cur starlark.Value = o
	for _, k := range path {
		m, ok := cur.(starlark.Mapping)
		if !ok {
			return ""
		}
		v, found, err := m.Get(starlark.String(k))
		if err != nil || !found {
			return ""
		}
		cur = v
	}
	s, _ := starlark.AsString(cur)
	return s
}

// bWait returns the sentinel meaning "cannot proceed, and here is why".
func bWait(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var reason string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "reason", &reason); err != nil {
		return nil, err
	}
	if reason == "" {
		return nil, fmt.Errorf("wait() needs a reason: it becomes the message on the Waiting condition")
	}
	return &waitValue{reason: reason}, nil
}

// bPending notes something unresolved without stopping the evaluation.
//
// This is the difference between "cannot proceed" and "proceeded as far as
// possible". A composition that stages - create an identity, wait for its
// provider status, then create what consumes it - has to return the identity
// while still reporting that it is not finished, and neither returning a plain
// mapping nor returning wait() says that.
func bPending(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var reason string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "reason", &reason); err != nil {
		return nil, err
	}
	if reason == "" {
		return nil, fmt.Errorf("pending() needs a reason: it becomes the message on the Waiting condition")
	}
	existing, _ := t.Local(pendingLocalKey).([]string)
	for _, r := range existing {
		if r == reason {
			// Reporting the same unresolved thing once per loop iteration is
			// easy to write by accident and useless to read.
			return starlark.None, nil
		}
	}
	t.SetLocal(pendingLocalKey, append(existing, reason))
	return starlark.None, nil
}

// bToYAML serialises a value to a YAML document.
//
// Serialising a structure to a string is the single most common thing these
// compositions do - Helm values, SQL scripts, connection strings - so it is a
// first-class builtin rather than something to assemble by hand. Mapping order
// follows the order the program wrote, because the result is frequently a
// document a human will read.
func bToYAML(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var v starlark.Value
	indent := 2
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "value", &v, "indent?", &indent); err != nil {
		return nil, err
	}
	if indent < 1 || indent > 10 {
		return nil, fmt.Errorf("indent must be between 1 and 10, got %d", indent)
	}
	node, err := yamlNode(v, 0)
	if err != nil {
		return nil, err
	}
	out, err := marshalYAML(node, indent)
	if err != nil {
		return nil, err
	}
	return starlark.String(out), nil
}

func bFromYAML(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var s string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "text", &s); err != nil {
		return nil, err
	}
	var raw any
	if err := yaml.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}
	norm, err := normalizeDecoded(raw, 0)
	if err != nil {
		return nil, err
	}
	return toStarlark(norm)
}

// bToJSON serialises a value to JSON, preserving mapping order.
func bToJSON(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var v starlark.Value
	indent := 0
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "value", &v, "indent?", &indent); err != nil {
		return nil, err
	}
	if indent < 0 || indent > 10 {
		return nil, fmt.Errorf("indent must be between 0 and 10, got %d", indent)
	}
	out, err := encodeJSON(v, indent)
	if err != nil {
		return nil, err
	}
	return starlark.String(out), nil
}

func bFromJSON(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var s string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "text", &s); err != nil {
		return nil, err
	}
	// YAML is a superset of JSON and this decoder keeps integers integral,
	// which encoding/json into `any` would not.
	var raw any
	if err := yaml.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("parsing JSON: %w", err)
	}
	norm, err := normalizeDecoded(raw, 0)
	if err != nil {
		return nil, err
	}
	return toStarlark(norm)
}

func bB64Encode(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var s string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "text", &s); err != nil {
		return nil, err
	}
	return starlark.String(base64.StdEncoding.EncodeToString([]byte(s))), nil
}

func bB64Decode(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var s string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "text", &s); err != nil {
		return nil, err
	}
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decoding base64: %w", err)
	}
	return starlark.String(out), nil
}

func bSHA256(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var s string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "text", &s); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(s))
	return starlark.String(hex.EncodeToString(sum[:])), nil
}

// normalizeDecoded coerces a YAML-decoded value into the type set the rest of
// the package expects, matching what unstructured guarantees.
func normalizeDecoded(v any, depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("value nested deeper than %d levels", maxDepth)
	}
	switch t := v.(type) {
	case nil, bool, string, float64, int64:
		return t, nil
	case int:
		return int64(t), nil
	case uint64:
		return int64(t), nil
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			c, err := normalizeDecoded(e, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			c, err := normalizeDecoded(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = c
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("mapping key %v is not a string", k)
			}
			c, err := normalizeDecoded(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[ks] = c
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported value of type %T", v)
	}
}
