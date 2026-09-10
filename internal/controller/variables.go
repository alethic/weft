package controller

import (
	"context"
	"encoding/base64"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	yaml "sigs.k8s.io/yaml"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/jsonutil"
	"github.com/alethic/weft/internal/variables"
)

var (
	configMapGVK = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	secretGVK    = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
)

// resolveVariables assembles the entries into the single mapping a program sees.
//
// Entries merge in order and later ones win, so a base held in a ConfigMap
// somebody else maintains can be overridden inline. The program cannot tell
// where any value came from, which is what makes moving a setting between the
// two not a change to the composition.
//
// Every read is impersonated, like every other read: a Weave can take
// configuration only from objects its ServiceAccount could read directly.
func (r *WeaveReconciler) resolveVariables(ctx context.Context, rd *weaveReader, weave *v1alpha1.Weave) (map[string]any, error) {
	out := map[string]any{}

	for i, entry := range weave.Spec.Variables {
		values, err := r.resolveVariable(ctx, rd, entry, i)
		if err != nil {
			return nil, err
		}
		out = variables.Merge(out, values)
	}
	return out, nil
}

func (r *WeaveReconciler) resolveVariable(ctx context.Context, rd *weaveReader, entry v1alpha1.Variable, index int) (map[string]any, error) {
	switch {
	case entry.Values != nil:
		return jsonutil.DecodeObject(entry.Values), nil

	case entry.ConfigMap != nil:
		return r.readVariableObject(ctx, rd, configMapGVK, *entry.ConfigMap, index, false)

	case entry.Secret != nil:
		return r.readVariableObject(ctx, rd, secretGVK, *entry.Secret, index, true)

	default:
		// The CRD's validation rule should have caught this at admission.
		return nil, degradedf(ReasonInvalidSpec,
			"variables[%d] sets none of values, configMap or secret", index)
	}
}

// readVariableObject reads one ConfigMap or Secret and turns it into a mapping.
func (r *WeaveReconciler) readVariableObject(
	ctx context.Context,
	rd *weaveReader,
	gvk schema.GroupVersionKind,
	ref v1alpha1.VariableRef,
	index int,
	encoded bool,
) (map[string]any, error) {
	// The same read a program would make, through the same reader: cached,
	// recorded, and therefore watched.
	obj, err := rd.readRaw(ctx, gvk, ref.Name)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		if ref.Optional {
			return nil, nil
		}
		// A composition built on configuration that has not arrived is not
		// ready, and this is the ordinary shape of that: waiting, not failing.
		return nil, waitingf(ReasonVariableMissing,
			"variables[%d] needs %s %q, which does not exist", index, gvk.Kind, ref.Name)
	}

	data, err := inputData(obj, encoded)
	if err != nil {
		return nil, degradedf(ReasonInvalidSpec, "variables[%d] (%s %q): %v", index, gvk.Kind, ref.Name, err)
	}

	if ref.Key == "" {
		out := make(map[string]any, len(data))
		for k, v := range data {
			out[k] = v
		}
		return out, nil
	}

	raw, ok := data[ref.Key]
	if !ok {
		if ref.Optional {
			return nil, nil
		}
		return nil, waitingf(ReasonVariableMissing,
			"variables[%d] needs the key %q of %s %q, which is not set", index, ref.Key, gvk.Kind, ref.Name)
	}

	// A single key holding a document, which is how a values.yaml ends up in a
	// ConfigMap in the first place.
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, degradedf(ReasonInvalidSpec,
			"variables[%d]: the key %q of %s %q is not a YAML mapping: %v",
			index, ref.Key, gvk.Kind, ref.Name, err)
	}
	return parsed, nil
}

// inputData pulls the string data out of a ConfigMap or Secret. A Secret's
// values arrive base64-encoded over the wire and are decoded here, so a program
// sees the same thing either way.
func inputData(obj *unstructured.Unstructured, encoded bool) (map[string]string, error) {
	raw, found, err := unstructured.NestedStringMap(obj.Object, "data")
	if err != nil {
		return nil, fmt.Errorf("data is not a mapping of strings: %w", err)
	}
	if !found {
		return map[string]string{}, nil
	}
	if !encoded {
		return raw, nil
	}

	out := make(map[string]string, len(raw))
	for k, v := range raw {
		decoded, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("the value of %q is not valid base64: %w", k, err)
		}
		out[k] = string(decoded)
	}
	return out, nil
}
