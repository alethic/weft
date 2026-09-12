package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/inventory"
	"github.com/alethic/weft/internal/kube"
	"github.com/alethic/weft/internal/metrics"
)

// applyAll applies items in ascending wave order.
//
// It returns the entries it managed to apply even when it stops early, so that
// a partial apply is still recorded: an unrecorded resource is an orphan, and
// orphans were the single worst property of the cron-and-template arrangement
// this replaces.
func (r *WeaveReconciler) applyAll(ctx context.Context, c *kube.Client, weave *v1alpha1.Weave, items []inventory.Item) ([]v1alpha1.InventoryEntry, error) {
	applied := make([]v1alpha1.InventoryEntry, 0, len(items))

	for _, wave := range inventory.ApplyWaves(items) {
		var waveErr error
		for _, item := range wave {
			// Refuse before writing, not after. Server-side apply is
			// create-or-update, so by the time it returns the takeover has
			// already happened.
			if err := r.checkOwnership(ctx, c, weave, item); err != nil {
				if waveErr == nil {
					waveErr = err
				}
				continue
			}

			out, err := c.Apply(ctx, item.Object)
			if err != nil {
				metrics.AppliesTotal.WithLabelValues("error").Inc()
				if waveErr == nil {
					waveErr = applyError(item, err)
				}
				continue
			}
			metrics.AppliesTotal.WithLabelValues("ok").Inc()
			applied = append(applied, inventory.Entry(item, out))
		}
		// Stop at the wave boundary rather than at the first failure: siblings
		// in a wave are independent, but a later wave may well depend on the
		// one that just failed.
		if waveErr != nil {
			return applied, waveErr
		}
	}

	return applied, nil
}

func applyError(item inventory.Item, err error) error {
	gvk := item.GroupVersionKind()

	var perm *kube.PermissionError
	if errors.As(err, &perm) {
		return degradedf(ReasonForbidden, "applying %s:\n%s", item.Ref, perm.Error())
	}

	var unknown *kube.UnknownKindError
	if errors.As(err, &unknown) {
		return waitingf(ReasonKindNotInstalled,
			"resource %q is a %s, and no such resource type is installed in this cluster",
			item.Ref, gvk)
	}

	var scoped *kube.ClusterScopedError
	if errors.As(err, &scoped) {
		return degradedf(ReasonClusterScoped,
			"resource %q is a %s, which is cluster-scoped: a namespaced Weave cannot own a cluster-scoped object, "+
				"so it could never be garbage collected",
			item.Ref, gvk.Kind)
	}

	if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return degradedf(ReasonApplyFailed, "%s was rejected by the API server: %v", item.Ref, err)
	}

	return fmt.Errorf("applying %s: %w", item.Ref, err)
}

// deleteWaves deletes entries one wave at a time, highest wave first, and
// returns what is still standing.
//
// Cascading garbage collection would delete all of this too, but in no
// particular order. Self-reference means the order is real: a role assignment
// that names a principal has to go before the identity that owns the principal,
// or the provider is left reconciling an assignment whose subject no longer
// exists.
func (r *WeaveReconciler) deleteWaves(ctx context.Context, c *kube.Client, weave *v1alpha1.Weave, entries []v1alpha1.InventoryEntry) ([]v1alpha1.InventoryEntry, error) {
	groups := inventory.Waves(entries)

	for i, group := range groups {
		standing, err := r.deleteGroup(ctx, c, weave, group)
		if err != nil {
			return entries, err
		}
		if len(standing) == 0 {
			continue
		}
		// This wave is still going. Everything below it waits.
		remaining := append([]v1alpha1.InventoryEntry(nil), standing...)
		for _, lower := range groups[i+1:] {
			remaining = append(remaining, lower...)
		}
		return remaining, nil
	}

	return nil, nil
}

// deleteGroup issues deletes for one wave and reports which of them have not
// gone away yet.
func (r *WeaveReconciler) deleteGroup(ctx context.Context, c *kube.Client, weave *v1alpha1.Weave, group []v1alpha1.InventoryEntry) ([]v1alpha1.InventoryEntry, error) {
	var standing []v1alpha1.InventoryEntry

	for _, e := range group {
		gvk, err := inventory.GroupVersionKind(e)
		if err != nil {
			// Nothing addressable to delete. Dropping the entry is the only
			// move that does not wedge teardown forever.
			continue
		}

		existing, err := c.Get(ctx, gvk, e.Name)
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			var unknown *kube.UnknownKindError
			if errors.As(err, &unknown) {
				// The type is gone, so the object is too.
				continue
			}
			var perm *kube.PermissionError
			if errors.As(err, &perm) {
				return nil, degradedf(ReasonForbidden, "tearing down %s %q:\n%s", e.Kind, e.Name, perm.Error())
			}
			return nil, fmt.Errorf("reading %s %q before deleting it: %w", e.Kind, e.Name, err)
		}

		// Only delete what is still ours.
		//
		// This is what makes orphan propagation work. Garbage collection strips
		// the owner reference as soon as it processes the deletion, which can
		// happen before this controller ever reconciles - so checking for the
		// orphan finalizer is a race, and losing it means deleting the very
		// resources somebody asked to keep. Ownership is not transient, so it
		// is what gets checked.
		if !ownedBy(existing, weave) {
			continue
		}

		if err := c.Delete(ctx, gvk, e.Name, e.UID); err != nil {
			var perm *kube.PermissionError
			if errors.As(err, &perm) {
				return nil, degradedf(ReasonForbidden, "deleting %s %q:\n%s", e.Kind, e.Name, perm.Error())
			}
			return nil, fmt.Errorf("deleting %s %q: %w", e.Kind, e.Name, err)
		}
		standing = append(standing, e)
	}

	return standing, nil
}

// ownedBy reports whether an object still carries this Weave's owner reference.
func ownedBy(obj *unstructured.Unstructured, weave *v1alpha1.Weave) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == weave.UID {
			return true
		}
	}
	return false
}

// describeEntries renders a short list for a condition message.
func describeEntries(entries []v1alpha1.InventoryEntry, limit int) string {
	if len(entries) == 0 {
		return ""
	}
	out := ""
	for i, e := range entries {
		if i == limit {
			out += fmt.Sprintf(", and %d more", len(entries)-limit)
			break
		}
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%s %q", e.Kind, e.Name)
	}
	return out
}

// elapsed reports how long ago a time was, guarding against a nil.
func elapsed(t *metav1.Time) time.Duration {
	if t == nil {
		return 0
	}
	return time.Since(t.Time)
}
