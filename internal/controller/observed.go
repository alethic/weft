package controller

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/eval"
	"github.com/alethic/weft/internal/inventory"
	"github.com/alethic/weft/internal/kube"
	"github.com/alethic/weft/internal/watches"
)

// readObserved reads back everything this Weave already created.
//
// Self-reference through these values is how a composition advances in stages,
// so they are read live rather than remembered: a resource we applied minutes
// ago is interesting precisely because its status has changed since.
func (r *WeaveReconciler) readObserved(ctx context.Context, c *kube.Client, weave *v1alpha1.Weave) (map[eval.Ref]map[string]any, error) {
	observed := make(map[eval.Ref]map[string]any, len(weave.Status.Inventory))

	for _, e := range weave.Status.Inventory {
		gvk, err := inventory.GroupVersionKind(e)
		if err != nil {
			return nil, degradedf(ReasonInvalidSpec, "%v", err)
		}

		obj, err := c.Get(ctx, gvk, e.Name)
		switch {
		case err == nil:
			stripNoise(obj)
			observed[eval.Ref{APIVersion: e.APIVersion, Kind: e.Kind, Name: e.Name}] = obj.Object

		case apierrors.IsNotFound(err):
			// Deleted out from under us. Leaving it out of observed lets the
			// program see it as absent and the apply below recreates it.

		default:
			var perm *kube.PermissionError
			if errors.As(err, &perm) {
				return nil, degradedf(ReasonForbidden, "reading %s %q that this Weave created:\n%s", e.Kind, e.Name, perm.Error())
			}
			var unknown *kube.UnknownKindError
			if errors.As(err, &unknown) {
				return nil, waitingf(ReasonKindNotInstalled,
					"%s is no longer installed in this cluster, so %s %q cannot be read back",
					gvk, e.Kind, e.Name)
			}
			return nil, fmt.Errorf("reading back %s %q: %w", e.Kind, e.Name, err)
		}
	}

	return observed, nil
}

// watchKeys is the union of what the program read and what it produced.
//
// Both halves matter and for the same reason: an output whose provider status
// populates minutes after the apply has to wake the Weave exactly the way an
// external resource does. There is one rule, not two.
//
// The read half comes from status, not from the spec, because there is no
// declaration to read it from - the program asks for what it wants while it
// runs. Persisting it is what lets a controller restart re-establish the
// watches for a Weave that is waiting and will not evaluate again until
// something it cannot currently see changes.
func watchKeys(weave *v1alpha1.Weave) []watches.Key {
	seen := make(map[watches.Key]bool)
	var keys []watches.Key

	add := func(gvk schema.GroupVersionKind) {
		k := watches.Key{
			GVK:            gvk,
			Namespace:      weave.Namespace,
			ServiceAccount: weave.Spec.ServiceAccountName,
		}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}

	for _, ref := range weave.Status.Reads {
		if gvk, err := parseGVK(ref.APIVersion, ref.Kind); err == nil {
			add(gvk)
		}
	}
	for _, e := range weave.Status.Inventory {
		if gvk, err := inventory.GroupVersionKind(e); err == nil {
			add(gvk)
		}
	}
	return keys
}

// stripNoise removes managed field bookkeeping before an object is handed to a
// program. It is large, it is never useful to a composition, and it counts
// against the evaluator's value budget.
func stripNoise(obj *unstructured.Unstructured) {
	unstructured.RemoveNestedField(obj.Object, "metadata", "managedFields")
}
