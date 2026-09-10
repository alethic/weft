// Package variables assembles the configuration entries a program sees.
package variables

import (
	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/jsonutil"
)

// Merge entries one mapping over another, in place over base.
//
// Mappings merge key by key, so a later entry can override one nested setting
// without restating the rest. Anything else replaces outright - notably lists,
// because merging those positionally is never what anybody means. This is the
// rule Helm values follow, which is the one people already have in their heads.
func Merge(base, over map[string]any) map[string]any {
	if base == nil {
		base = map[string]any{}
	}
	for k, v := range over {
		existing, ok := base[k]
		if !ok {
			base[k] = v
			continue
		}
		em, eIsMap := existing.(map[string]any)
		vm, vIsMap := v.(map[string]any)
		if eIsMap && vIsMap {
			base[k] = Merge(em, vm)
			continue
		}
		base[k] = v
	}
	return base
}

// Inline merges only the entries written in the Weave itself.
//
// It exists for anything that has to understand a composition without a cluster
// to read the referenced objects from: tests over the shipped examples, and
// tooling that wants to know what a program would see from its spec alone.
func Inline(entries []v1alpha1.Variable) map[string]any {
	out := map[string]any{}
	for _, entry := range entries {
		if entry.Values == nil {
			continue
		}
		out = Merge(out, jsonutil.DecodeObject(entry.Values))
	}
	return out
}
