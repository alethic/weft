package controller

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/inventory"
	"github.com/alethic/weft/internal/kube"
	"github.com/alethic/weft/internal/metrics"
	"github.com/alethic/weft/internal/watches"
)

// resolvedSources is the outcome of reading every declared source.
type resolvedSources struct {
	// values maps source id to the object, or nil when it does not exist.
	values map[string]any

	// objects keeps the live objects for the sources that exist, needed for
	// finalizer handling.
	objects map[string]*unstructured.Unstructured
}

// resolveSources reads every declared source through the impersonated client.
//
// A partially resolved set is never returned. The predecessor to this system
// piped kubectl get into a template and used a bare shell wait, which reports
// success regardless of how its children exited; when only some resources
// resolved, the template rendered against a partial list and reported "resource
// not found" instead of the API error that actually caused it. The lesson is
// structural, not incidental: an error reading any source aborts before
// evaluation, and the API error is what gets surfaced.
func (r *WeaveReconciler) resolveSources(ctx context.Context, c *kube.Client, weave *v1alpha1.Weave) (*resolvedSources, error) {
	out := &resolvedSources{
		values:  make(map[string]any, len(weave.Spec.Sources)),
		objects: make(map[string]*unstructured.Unstructured, len(weave.Spec.Sources)),
	}

	for _, src := range weave.Spec.Sources {
		gvk, err := sourceGVK(src)
		if err != nil {
			return nil, degradedf(ReasonInvalidSpec, "source %q: %v", src.ID, err)
		}

		obj, err := c.Get(ctx, gvk, src.Name)
		switch {
		case err == nil:
			stripNoise(obj)
			out.values[src.ID] = obj.Object
			out.objects[src.ID] = obj

		case apierrors.IsNotFound(err):
			// Absence is a value the program sees, not a verdict the controller
			// reaches on its behalf. Whether it should block, and under what
			// conditions, is a decision only the composition can make.
			out.values[src.ID] = nil

		default:
			return nil, sourceReadError(src, gvk, err)
		}
	}

	return out, nil
}

// sourceReadError classifies a failed read.
func sourceReadError(src v1alpha1.Source, gvk schema.GroupVersionKind, err error) error {
	var perm *kube.PermissionError
	if errors.As(err, &perm) {
		metrics.PermissionDenialsTotal.WithLabelValues(perm.Verb, perm.Resource.Resource).Inc()
		// The controller does not fill the gap with its own privileges. It says
		// what to grant and stops.
		return degradedf(ReasonForbidden, "reading source %q:\n%s", src.ID, perm.Error())
	}

	var unknown *kube.UnknownKindError
	if errors.As(err, &unknown) {
		// Waiting rather than Degraded: a provider that installs this CRD later
		// resolves it without anybody editing the Weave.
		return waitingf(ReasonKindNotInstalled,
			"source %q needs %s, and no such resource type is installed in this cluster",
			src.ID, gvk)
	}

	var scoped *kube.ClusterScopedError
	if errors.As(err, &scoped) {
		return degradedf(ReasonClusterScoped,
			"source %q is %s, which is cluster-scoped: reading it would need a ClusterRoleBinding, "+
				"which a namespace user cannot create, so Weft only reads namespaced resources",
			src.ID, gvk.Kind)
	}

	// Anything else is transient. Requeue with backoff rather than recording a
	// verdict about a cluster we could not reach.
	return fmt.Errorf("reading source %q (%s %q): %w", src.ID, gvk.Kind, src.Name, err)
}

// readObserved reads back everything this Weave already created.
//
// Self-reference through these values is how a composition advances in stages,
// so they are read live rather than remembered: a resource we applied minutes
// ago is interesting precisely because its status has changed since.
func (r *WeaveReconciler) readObserved(ctx context.Context, c *kube.Client, weave *v1alpha1.Weave) (map[string]any, error) {
	observed := make(map[string]any, len(weave.Status.Inventory))

	for _, e := range weave.Status.Inventory {
		gvk, err := inventory.GroupVersionKind(e)
		if err != nil {
			return nil, degradedf(ReasonInvalidSpec, "%v", err)
		}

		obj, err := c.Get(ctx, gvk, e.Name)
		switch {
		case err == nil:
			stripNoise(obj)
			observed[e.Key] = obj.Object

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
					"%s is no longer installed in this cluster, so %q cannot be read back", gvk, e.Key)
			}
			return nil, fmt.Errorf("reading observed %q (%s %q): %w", e.Key, e.Kind, e.Name, err)
		}
	}

	return observed, nil
}

// watchKeys is the union of declared sources and current outputs.
//
// Both halves matter and for the same reason: an output whose provider status
// populates minutes after the apply has to wake the Weave exactly the way an
// external source does. There is one rule, not two.
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

	for _, src := range weave.Spec.Sources {
		if gvk, err := sourceGVK(src); err == nil {
			add(gvk)
		}
	}
	for _, e := range weave.Status.Inventory {
		if gvk, err := inventory.GroupVersionKind(e); err == nil {
			add(gvk)
		}
	}

	// Configuration is watched too. A ConfigMap holding a base layer changes
	// what the composition produces just as surely as a source does.
	for _, k := range inputWatchKeys(weave) {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys
}

func sourceGVK(src v1alpha1.Source) (schema.GroupVersionKind, error) {
	gv, err := schema.ParseGroupVersion(src.APIVersion)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("apiVersion %q is not parseable: %w", src.APIVersion, err)
	}
	return gv.WithKind(src.Kind), nil
}

// stripNoise removes managed field bookkeeping before an object is handed to a
// program. It is large, it is never useful to a composition, and it counts
// against the evaluator's value budget.
func stripNoise(obj *unstructured.Unstructured) {
	unstructured.RemoveNestedField(obj.Object, "metadata", "managedFields")
}
