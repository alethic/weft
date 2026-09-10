package controller

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/kube"
	"github.com/alethic/weft/internal/naming"
)

// Holds are the opt-in finalizers a program asks for with
// read(..., hold=True).
//
// This orders the deletion of an API object, and nothing more. It does not
// order the destruction of whatever that object represents: a managed resource
// removed from the API server may leave its provider still tearing down a cloud
// resource for minutes afterwards. Anyone reaching for this should know which
// of the two problems they actually have.
//
// The request lives in the program, which runs after the point where a deletion
// has to be noticed. That is why status.held exists: it is the record of what
// was asked for last time, and it is what the pre-evaluation check reads. A
// program that stops asking, or stops running at all, still leaves behind
// enough to release what it placed.

// checkHolds looks for a held resource that has started deleting, before
// anything is evaluated.
//
// A held resource going away means the answer is "tear down", not "recompute".
// It also has to stop the pass outright: a resource being deleted stays
// readable until its last finalizer clears, so evaluation would succeed against
// it and re-apply everything that was just torn down. That recreated output
// would then outlive the resource entirely, orphaned, which is precisely the
// failure this feature exists to prevent.
func (r *WeaveReconciler) checkHolds(ctx context.Context, c *kube.Client, rd *weaveReader, weave *v1alpha1.Weave) error {
	for i := range weave.Status.Held {
		held := weave.Status.Held[i]
		gvk, err := parseGVK(held.APIVersion, held.Kind)
		if err != nil {
			continue
		}

		obj, err := rd.readRaw(ctx, gvk, held.Name)
		if err != nil {
			return err
		}
		if obj == nil {
			// Already gone. Nothing to release and nothing to hold.
			weave.Status.Held = withoutHold(weave.Status.Held, held)
			continue
		}
		if obj.GetDeletionTimestamp() == nil {
			continue
		}

		return r.tearDownForHold(ctx, c, weave, &weave.Status.Held[i], gvk)
	}
	return nil
}

// reconcileHolds settles status.held against what the program asked for on this
// pass: places what is new, releases what is no longer wanted, and stops the
// pass if anything it asked to hold is already deleting.
func (r *WeaveReconciler) reconcileHolds(ctx context.Context, c *kube.Client, weave *v1alpha1.Weave, requested, deleting []v1alpha1.HeldResource) error {
	log := logf.FromContext(ctx)

	// Release first. A program that moved a hold from one object to another
	// should not be able to hold both at once.
	for _, existing := range weave.Status.Held {
		if containsHold(requested, existing) {
			continue
		}
		if err := r.releaseHold(ctx, c, weave, existing); err != nil {
			return err
		}
		log.Info("released hold", "kind", existing.Kind, "name", existing.Name)
	}

	kept := make([]v1alpha1.HeldResource, 0, len(requested))
	for _, want := range requested {
		gvk, err := parseGVK(want.APIVersion, want.Kind)
		if err != nil {
			return degradedf(ReasonInvalidSpec, "read(%q, %q, %q): %v", want.APIVersion, want.Kind, want.Name, err)
		}

		// Carry the teardown clock across passes.
		if prev, ok := weave.Status.HeldByRef(want.APIVersion, want.Kind, want.Name); ok {
			want.TeardownStartedAt = prev.TeardownStartedAt
		}

		// Check the permission to remove the finalizer on every pass, not only
		// when adding it. A RoleBinding revoked after the fact would otherwise
		// leave a finalizer nobody can lift, deadlocking the object and the
		// namespace it lives in.
		allowed, err := c.CanI(ctx, "update", gvk, want.Name)
		if err != nil {
			return fmt.Errorf("checking update permission on %s %q: %w", gvk.Kind, want.Name, err)
		}
		if !allowed {
			perm := c.PermissionErrorFor("update", gvk, want.Name)
			return degradedf(ReasonForbidden,
				"this program reads %s %q with hold=True, which needs permission to update it.\n%s\n\n%s",
				gvk.Kind, want.Name, perm.Summary(), perm.Fix())
		}

		added, err := c.MutateFinalizers(ctx, gvk, want.Name, addFinalizer(naming.HeldFinalizer(weave.Namespace, weave.Name)))
		if err != nil {
			return holdError(gvk, want.Name, err)
		}
		if added {
			log.Info("placed hold", "kind", gvk.Kind, "name", want.Name)
		}
		kept = append(kept, want)
	}
	weave.Status.Held = kept

	// Something the program asked to hold is already on its way out. The
	// evaluation that just ran is discarded: see checkHolds.
	if len(deleting) > 0 {
		d := deleting[0]
		gvk, err := parseGVK(d.APIVersion, d.Kind)
		if err != nil {
			return nil
		}
		if h, ok := weave.Status.HeldByRef(d.APIVersion, d.Kind, d.Name); ok {
			return r.tearDownForHold(ctx, c, weave, h, gvk)
		}
	}
	return nil
}

// tearDownForHold removes this Weave's resources ahead of a held resource that
// is going away, then releases the hold. It always ends the pass.
func (r *WeaveReconciler) tearDownForHold(
	ctx context.Context,
	c *kube.Client,
	weave *v1alpha1.Weave,
	held *v1alpha1.HeldResource,
	gvk schema.GroupVersionKind,
) error {
	log := logf.FromContext(ctx)

	if held.TeardownStartedAt == nil {
		now := metav1.Now()
		held.TeardownStartedAt = &now
	}

	overdue := elapsed(held.TeardownStartedAt) > r.Opts.HoldTimeout
	if !overdue {
		remaining, err := r.deleteWaves(ctx, c, weave, weave.Status.Inventory)
		if err != nil {
			return err
		}
		weave.Status.Inventory = remaining
		if len(remaining) > 0 {
			return waitingf(ReasonHeldDeleting,
				"%s %q is being deleted; tearing down this Weave's resources before releasing it",
				gvk.Kind, held.Name)
		}
	} else {
		// Blocking somebody else's object, and their namespace deletion,
		// forever is worse than the ordering violation. Give up loudly.
		log.Error(nil, "hold timed out; releasing it with resources still standing",
			"kind", gvk.Kind, "name", held.Name,
			"timeout", r.Opts.HoldTimeout.String(),
			"remaining", len(weave.Status.Inventory))
		r.eventf(weave, "Warning", "HoldTimeout",
			"released the finalizer on %s %q after %s with %d resources still standing; deletion ordering was not honoured",
			gvk.Kind, held.Name, r.Opts.HoldTimeout, len(weave.Status.Inventory))
	}

	released := *held
	if _, err := c.MutateFinalizers(ctx, gvk, held.Name, removeFinalizer(naming.HeldFinalizer(weave.Namespace, weave.Name))); err != nil {
		if !apierrors.IsNotFound(err) {
			return holdError(gvk, held.Name, err)
		}
	}
	weave.Status.Held = withoutHold(weave.Status.Held, released)
	log.Info("released hold on a deleting resource", "kind", gvk.Kind, "name", released.Name)

	return waitingf(ReasonHeldDeleting,
		"%s %q has been released and is going away; this Weave's resources have been torn down",
		gvk.Kind, released.Name)
}

// releaseAllHolds lifts every finalizer this Weave placed. Used when the Weave
// itself is going away.
func (r *WeaveReconciler) releaseAllHolds(ctx context.Context, c *kube.Client, weave *v1alpha1.Weave) error {
	for _, held := range weave.Status.Held {
		if err := r.releaseHold(ctx, c, weave, held); err != nil {
			return err
		}
	}
	weave.Status.Held = nil
	return nil
}

func (r *WeaveReconciler) releaseHold(ctx context.Context, c *kube.Client, weave *v1alpha1.Weave, held v1alpha1.HeldResource) error {
	gvk, err := parseGVK(held.APIVersion, held.Kind)
	if err != nil {
		// An unparseable reference cannot have been finalized in the first place.
		return nil
	}
	_, err = c.MutateFinalizers(ctx, gvk, held.Name, removeFinalizer(naming.HeldFinalizer(weave.Namespace, weave.Name)))
	switch {
	case err == nil, apierrors.IsNotFound(err):
		return nil
	default:
		var unknown *kube.UnknownKindError
		if errors.As(err, &unknown) {
			return nil
		}
		return holdError(gvk, held.Name, err)
	}
}

func holdError(gvk schema.GroupVersionKind, name string, err error) error {
	var perm *kube.PermissionError
	if errors.As(err, &perm) {
		return degradedf(ReasonForbidden,
			"holding %s %q needs permission to update it:\n%s", gvk.Kind, name, perm.Error())
	}
	var unknown *kube.UnknownKindError
	if errors.As(err, &unknown) {
		return waitingf(ReasonKindNotInstalled, "%s is not installed in this cluster", gvk)
	}
	return fmt.Errorf("updating finalizers on %s %q: %w", gvk.Kind, name, err)
}

func containsHold(list []v1alpha1.HeldResource, want v1alpha1.HeldResource) bool {
	for _, h := range list {
		if h.APIVersion == want.APIVersion && h.Kind == want.Kind && h.Name == want.Name {
			return true
		}
	}
	return false
}

func withoutHold(list []v1alpha1.HeldResource, drop v1alpha1.HeldResource) []v1alpha1.HeldResource {
	out := make([]v1alpha1.HeldResource, 0, len(list))
	for _, h := range list {
		if h.APIVersion == drop.APIVersion && h.Kind == drop.Kind && h.Name == drop.Name {
			continue
		}
		out = append(out, h)
	}
	return out
}

func addFinalizer(want string) func([]string) ([]string, bool) {
	return func(current []string) ([]string, bool) {
		for _, f := range current {
			if f == want {
				return current, false
			}
		}
		return append(append([]string(nil), current...), want), true
	}
}

func removeFinalizer(want string) func([]string) ([]string, bool) {
	return func(current []string) ([]string, bool) {
		out := make([]string, 0, len(current))
		for _, f := range current {
			if f != want {
				out = append(out, f)
			}
		}
		if len(out) == len(current) {
			return current, false
		}
		return out, true
	}
}
