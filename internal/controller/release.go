package controller

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/inventory"
	"github.com/alethic/weft/internal/kube"
	"github.com/alethic/weft/internal/naming"
)

// releaseAll drops the record of every unowned entry that is no longer
// described, and says so.
//
// There is nothing to undo on the object itself: an unowned resource never
// carried an owner reference, so letting go is bookkeeping. The one case that
// needs a write is a program edited from owned to unowned - server-side apply
// takes our reference back off when we stop setting it, and this catches an
// object that was already dropped from the returned set in the same pass.
//
// What stays is the weft.run/weave label and the weft.run/key annotation, so a
// released object is still findable as something this Weave once made.
func (r *WeaveReconciler) releaseAll(ctx context.Context, c *kube.Client, weave *v1alpha1.Weave, entries []v1alpha1.InventoryEntry) error {
	log := logf.FromContext(ctx)

	for _, e := range entries {
		gvk, err := inventory.GroupVersionKind(e)
		if err != nil {
			// Nothing addressable. The entry is dropped either way, which is
			// the whole of what releasing it means.
			continue
		}

		existing, err := c.Get(ctx, gvk, e.Name)
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			var unknown *kube.UnknownKindError
			if errors.As(err, &unknown) {
				continue
			}
			var perm *kube.PermissionError
			if errors.As(err, &perm) {
				return degradedf(ReasonForbidden, "releasing %s %q:\n%s", e.Kind, e.Name, perm.Error())
			}
			return fmt.Errorf("reading %s %q before releasing it: %w", e.Kind, e.Name, err)
		}

		// Usually there is nothing on the object to undo: it never carried our
		// reference. The exception is a program edited from owned to unowned in
		// the same pass that dropped it, where server-side apply never got the
		// chance to take the reference back off.
		if ownedBy(existing, weave) {
			if err := c.RemoveOwnerReference(ctx, gvk, e.Name, weave.UID); err != nil {
				var perm *kube.PermissionError
				if errors.As(err, &perm) {
					return degradedf(ReasonForbidden,
						"releasing %s %q needs permission to update it:\n%s", e.Kind, e.Name, perm.Error())
				}
				if !apierrors.IsNotFound(err) {
					return fmt.Errorf("releasing %s %q: %w", e.Kind, e.Name, err)
				}
			}
		}

		log.Info("released an unowned resource", "kind", e.Kind, "name", e.Name)
		r.eventf(weave, "Normal", "Released",
			"let go of %s %q, which was applied %s=false. It is no longer tracked by this Weave and "+
				"will not be deleted with it.",
			e.Kind, e.Name, naming.OwnedAnnotation)
	}

	return nil
}

// partitionOwned splits an inventory into what this Weave may delete and what
// it only ever tracked.
func partitionOwned(entries []v1alpha1.InventoryEntry) (owned, unowned []v1alpha1.InventoryEntry) {
	for _, e := range entries {
		if e.Owned {
			owned = append(owned, e)
		} else {
			unowned = append(unowned, e)
		}
	}
	return owned, unowned
}
