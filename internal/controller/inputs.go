package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	yaml "sigs.k8s.io/yaml"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/inputs"
	"github.com/alethic/weft/internal/jsonutil"
	"github.com/alethic/weft/internal/kube"
	"github.com/alethic/weft/internal/watches"
)

var (
	configMapGVK = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	secretGVK    = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
)

// resolveInputs assembles the layers into the single mapping a program sees.
//
// Layers merge in order and later ones win, so a base held in a ConfigMap
// somebody else maintains can be overridden inline. The program cannot tell
// where any value came from, which is what makes moving a setting between the
// two not a change to the composition.
//
// Every read is impersonated, like every other read: a Weave can take
// configuration only from objects its ServiceAccount could read directly.
func (r *WeaveReconciler) resolveInputs(ctx context.Context, c *kube.Client, weave *v1alpha1.Weave) (map[string]any, error) {
	out := map[string]any{}

	for i, layer := range weave.Spec.Inputs {
		values, err := r.resolveInputLayer(ctx, c, layer, i)
		if err != nil {
			return nil, err
		}
		out = inputs.Merge(out, values)
	}
	return out, nil
}

func (r *WeaveReconciler) resolveInputLayer(ctx context.Context, c *kube.Client, layer v1alpha1.InputSource, index int) (map[string]any, error) {
	switch {
	case layer.Values != nil:
		return jsonutil.DecodeObject(layer.Values), nil

	case layer.ConfigMap != nil:
		return r.readInputObject(ctx, c, configMapGVK, *layer.ConfigMap, index, false)

	case layer.Secret != nil:
		return r.readInputObject(ctx, c, secretGVK, *layer.Secret, index, true)

	default:
		// The CRD's validation rule should have caught this at admission.
		return nil, degradedf(ReasonInvalidSpec,
			"inputs[%d] sets none of values, configMap or secret", index)
	}
}

// readInputObject reads one ConfigMap or Secret and turns it into a mapping.
func (r *WeaveReconciler) readInputObject(
	ctx context.Context,
	c *kube.Client,
	gvk schema.GroupVersionKind,
	ref v1alpha1.InputRef,
	index int,
	encoded bool,
) (map[string]any, error) {
	obj, err := c.Get(ctx, gvk, ref.Name)
	switch {
	case err == nil:

	case apierrors.IsNotFound(err):
		if ref.Optional {
			return nil, nil
		}
		// A composition built on configuration that has not arrived is not
		// ready, and this is the ordinary shape of that: waiting, not failing.
		return nil, waitingf(ReasonInputMissing,
			"inputs[%d] needs %s %q, which does not exist", index, gvk.Kind, ref.Name)

	default:
		var perm *kube.PermissionError
		if errors.As(err, &perm) {
			return nil, degradedf(ReasonForbidden,
				"reading inputs[%d]:\n%s", index, perm.Error())
		}
		return nil, fmt.Errorf("reading inputs[%d] (%s %q): %w", index, gvk.Kind, ref.Name, err)
	}

	data, err := inputData(obj, encoded)
	if err != nil {
		return nil, degradedf(ReasonInvalidSpec, "inputs[%d] (%s %q): %v", index, gvk.Kind, ref.Name, err)
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
		return nil, waitingf(ReasonInputMissing,
			"inputs[%d] needs the key %q of %s %q, which is not set", index, ref.Key, gvk.Kind, ref.Name)
	}

	// A single key holding a document, which is how a values.yaml ends up in a
	// ConfigMap in the first place.
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, degradedf(ReasonInvalidSpec,
			"inputs[%d]: the key %q of %s %q is not a YAML mapping: %v",
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

// inputWatchKeys returns the objects the inputs read, so a change to one wakes
// the Weave the same way a change to a source does.
func inputWatchKeys(weave *v1alpha1.Weave) []watches.Key {
	var keys []watches.Key
	seen := map[schema.GroupVersionKind]bool{}

	add := func(gvk schema.GroupVersionKind) {
		if seen[gvk] {
			return
		}
		seen[gvk] = true
		keys = append(keys, watches.Key{
			GVK:            gvk,
			Namespace:      weave.Namespace,
			ServiceAccount: weave.Spec.ServiceAccountName,
		})
	}

	for _, layer := range weave.Spec.Inputs {
		switch {
		case layer.ConfigMap != nil:
			add(configMapGVK)
		case layer.Secret != nil:
			add(secretGVK)
		}
	}
	return keys
}
