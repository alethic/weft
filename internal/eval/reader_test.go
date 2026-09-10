package eval

import (
	"context"
	"fmt"
	"sort"
)

// fakeReader answers read() and select() from a map keyed by name.
//
// apiVersion and kind are ignored. Resolution is the controller's problem and
// has its own tests against a real API server; what these tests are about is
// what a program sees when a resource is there, when it is not, and when
// reading it fails.
type fakeReader map[string]any

var _ Reader = fakeReader(nil)

func (f fakeReader) Read(_ context.Context, apiVersion, kind, name string, _ bool) (map[string]any, error) {
	v, ok := f[name]
	if !ok || v == nil {
		return nil, nil
	}
	if err, isErr := v.(error); isErr {
		return nil, err
	}
	obj, isObj := v.(map[string]any)
	if !isObj {
		return nil, fmt.Errorf("fakeReader: %s/%s %q is not an object", apiVersion, kind, name)
	}
	return obj, nil
}

func (f fakeReader) Select(_ context.Context, _, _ string, labels map[string]string) ([]map[string]any, error) {
	names := make([]string, 0, len(f))
	for name := range f {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []map[string]any
	for _, name := range names {
		obj, ok := f[name].(map[string]any)
		if !ok {
			continue
		}
		if matchesLabels(obj, labels) {
			out = append(out, obj)
		}
	}
	return out, nil
}

func matchesLabels(obj map[string]any, want map[string]string) bool {
	meta, _ := obj["metadata"].(map[string]any)
	got, _ := meta["labels"].(map[string]any)
	for k, v := range want {
		if s, _ := got[k].(string); s != v {
			return false
		}
	}
	return true
}
