package variables

import (
	"reflect"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/alethic/weft/api/v1alpha1"
)

func TestMergeLaterEntryWins(t *testing.T) {
	got := Merge(
		map[string]any{"a": "base", "b": "base"},
		map[string]any{"b": "over", "c": "over"},
	)
	want := map[string]any{"a": "base", "b": "over", "c": "over"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Mappings merge key by key, so a entry can override one nested setting without
// restating everything around it. That is the whole reason a base can live in a
// ConfigMap somebody else maintains.
func TestMergeIsDeepForMappings(t *testing.T) {
	got := Merge(
		map[string]any{
			"image": map[string]any{"repository": "base", "tag": "v1"},
			"other": "kept",
		},
		map[string]any{
			"image": map[string]any{"tag": "v2"},
		},
	)
	want := map[string]any{
		"image": map[string]any{"repository": "base", "tag": "v2"},
		"other": "kept",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Lists replace outright. Merging them positionally is never what anybody
// means, and it is the rule Helm values already follow.
func TestMergeReplacesLists(t *testing.T) {
	got := Merge(
		map[string]any{"roles": []any{"a", "b", "c"}},
		map[string]any{"roles": []any{"z"}},
	)
	want := map[string]any{"roles": []any{"z"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A scalar replacing a mapping, or the reverse, takes the later value rather
// than trying to reconcile two different shapes.
func TestMergeAcrossShapes(t *testing.T) {
	got := Merge(
		map[string]any{"x": map[string]any{"deep": true}},
		map[string]any{"x": "flat"},
	)
	if got["x"] != "flat" {
		t.Errorf("got %v", got["x"])
	}

	got = Merge(
		map[string]any{"x": "flat"},
		map[string]any{"x": map[string]any{"deep": true}},
	)
	if _, ok := got["x"].(map[string]any); !ok {
		t.Errorf("got %v", got["x"])
	}
}

func TestMergeHandlesNilBase(t *testing.T) {
	got := Merge(nil, map[string]any{"a": 1})
	if got["a"] != 1 {
		t.Errorf("got %v", got)
	}
}

// A entry that resolved to nothing - an optional object that does not exist -
// leaves what came before alone.
func TestMergeWithNothingOver(t *testing.T) {
	got := Merge(map[string]any{"a": 1}, nil)
	if len(got) != 1 || got["a"] != 1 {
		t.Errorf("got %v", got)
	}
}

func values(raw string) v1alpha1.Variable {
	return v1alpha1.Variable{Values: &apiextensionsv1.JSON{Raw: []byte(raw)}}
}

func TestInlineMergesOnlyWrittenEntries(t *testing.T) {
	got := Inline([]v1alpha1.Variable{
		values(`{"a": "first", "shared": "first"}`),
		{ConfigMap: &v1alpha1.VariableRef{Name: "ignored-without-a-cluster"}},
		values(`{"b": "second", "shared": "second"}`),
	})

	want := map[string]any{"a": "first", "b": "second", "shared": "second"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Integers have to survive, or a replica count reaches a program as a float and
// comes back out as one.
func TestInlineKeepsTypes(t *testing.T) {
	got := Inline([]v1alpha1.Variable{values(`{"replicas": 3, "debug": true}`)})
	if r, ok := got["replicas"].(int64); !ok || r != 3 {
		t.Errorf("replicas = %#v, want int64(3)", got["replicas"])
	}
	if d, ok := got["debug"].(bool); !ok || !d {
		t.Errorf("debug = %#v", got["debug"])
	}
}

func TestInlineWithNoEntries(t *testing.T) {
	if got := Inline(nil); len(got) != 0 {
		t.Errorf("got %v, want an empty mapping", got)
	}
}
