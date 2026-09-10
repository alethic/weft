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

// reconcileSourceFinalizers maintains the opt-in finalizers on sources.
//
// This orders the deletion of an API object, and nothing more. It does not
// order the destruction of whatever that object represents: a managed resource
// removed from the API server may leave its provider still tearing down a cloud
// resource for minutes afterwards. Anyone reaching for this should know which
// of the two problems they actually have.
func (r *WeaveReconciler) reconcileSourceFinalizers(ctx context.Context, c *kube.Client, weave *v1alpha1.Weave, resolved *resolvedSources) error {
	log := logf.FromContext(ctx)

	var keep []v1alpha1.SourceStatus

	for _, src := range weave.Spec.Sources {
		if !src.Finalize {
			// A source that stopped asking for finalization gets released.
			if st, ok := weave.Status.SourceStatusByID(src.ID); ok && st.Finalized {
				if err := r.releaseSourceFinalizer(ctx, c, weave, src); err != nil {
					return err
				}
			}
			continue
		}

		gvk, err := sourceGVK(src)
		if err != nil {
			return degradedf(ReasonInvalidSpec, "source %q: %v", src.ID, err)
		}

		obj := resolved.objects[src.ID]
		if obj == nil {
			// Nothing to hold. If we had a finalizer on it, it is already gone.
			continue
		}

		status := v1alpha1.SourceStatus{ID: src.ID}
		if existing, ok := weave.Status.SourceStatusByID(src.ID); ok {
			status = *existing
		}

		if obj.GetDeletionTimestamp() != nil {
			done, err := r.finalizeDeletingSource(ctx, c, weave, src, gvk, &status)
			if err != nil {
				return err
			}
			if !done {
				keep = append(keep, status)
				weave.Status.Sources = keep
				return waitingf(ReasonSourceDeleting,
					"source %q (%s %q) is being deleted; tearing down this Weave's resources before releasing it",
					src.ID, gvk.Kind, src.Name)
			}
			log.Info("released source finalizer", "source", src.ID, "kind", gvk.Kind, "name", src.Name)
			continue
		}

		status.TeardownStartedAt = nil

		// Check the permission to remove the finalizer on every pass, not only
		// when adding it. A RoleBinding revoked after the fact would otherwise
		// leave a finalizer nobody can lift, deadlocking the object and the
		// namespace it lives in.
		allowed, err := c.CanI(ctx, "update", gvk, src.Name)
		if err != nil {
			return fmt.Errorf("checking update permission on source %q: %w", src.ID, err)
		}
		if !allowed {
			perm := c.PermissionErrorFor("update", gvk, src.Name)
			return degradedf(ReasonForbidden,
				"source %q asks for finalize: true, which needs permission to update it.\n%s\n\n%s",
				src.ID, perm.Summary(), perm.Fix())
		}

		if !status.Finalized {
			added, err := c.MutateFinalizers(ctx, gvk, src.Name, addFinalizer(naming.SourceFinalizer(weave.Namespace, weave.Name)))
			if err != nil {
				return sourceFinalizerError(src, gvk, err)
			}
			if added {
				log.Info("placed source finalizer", "source", src.ID, "kind", gvk.Kind, "name", src.Name)
			}
			status.Finalized = true
		}

		keep = append(keep, status)
	}

	weave.Status.Sources = keep
	return nil
}

// finalizeDeletingSource tears this Weave's resources down ahead of a source
// that is going away, and reports whether the finalizer has been released.
func (r *WeaveReconciler) finalizeDeletingSource(
	ctx context.Context,
	c *kube.Client,
	weave *v1alpha1.Weave,
	src v1alpha1.Source,
	gvk schema.GroupVersionKind,
	status *v1alpha1.SourceStatus,
) (bool, error) {
	log := logf.FromContext(ctx)

	if status.TeardownStartedAt == nil {
		now := metav1.Now()
		status.TeardownStartedAt = &now
	}

	overdue := elapsed(status.TeardownStartedAt) > r.Opts.SourceFinalizerTimeout
	if !overdue {
		remaining, err := r.deleteWaves(ctx, c, weave.Status.Inventory)
		if err != nil {
			return false, err
		}
		weave.Status.Inventory = remaining
		if len(remaining) > 0 {
			return false, nil
		}
	} else {
		// Blocking somebody else's object, and their namespace deletion,
		// forever is worse than the ordering violation. Give up loudly.
		log.Error(nil, "source finalizer timed out; releasing it with resources still standing",
			"source", src.ID, "kind", gvk.Kind, "name", src.Name,
			"timeout", r.Opts.SourceFinalizerTimeout.String(),
			"remaining", len(weave.Status.Inventory))
		r.eventf(weave, "Warning", "FinalizerTimeout",
			"released the finalizer on %s %q after %s with %d resources still standing; deletion ordering was not honoured",
			gvk.Kind, src.Name, r.Opts.SourceFinalizerTimeout, len(weave.Status.Inventory))
	}

	if _, err := c.MutateFinalizers(ctx, gvk, src.Name, removeFinalizer(naming.SourceFinalizer(weave.Namespace, weave.Name))); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, sourceFinalizerError(src, gvk, err)
	}
	status.Finalized = false
	return true, nil
}

// releaseAllSourceFinalizers lifts every finalizer this Weave placed. Used when
// the Weave itself is going away.
func (r *WeaveReconciler) releaseAllSourceFinalizers(ctx context.Context, c *kube.Client, weave *v1alpha1.Weave) error {
	for _, src := range weave.Spec.Sources {
		if !src.Finalize {
			continue
		}
		if err := r.releaseSourceFinalizer(ctx, c, weave, src); err != nil {
			return err
		}
	}
	weave.Status.Sources = nil
	return nil
}

func (r *WeaveReconciler) releaseSourceFinalizer(ctx context.Context, c *kube.Client, weave *v1alpha1.Weave, src v1alpha1.Source) error {
	gvk, err := sourceGVK(src)
	if err != nil {
		// An unparseable source cannot have been finalized in the first place.
		return nil
	}
	_, err = c.MutateFinalizers(ctx, gvk, src.Name, removeFinalizer(naming.SourceFinalizer(weave.Namespace, weave.Name)))
	switch {
	case err == nil, apierrors.IsNotFound(err):
		return nil
	default:
		var unknown *kube.UnknownKindError
		if errors.As(err, &unknown) {
			return nil
		}
		return sourceFinalizerError(src, gvk, err)
	}
}

func sourceFinalizerError(src v1alpha1.Source, gvk schema.GroupVersionKind, err error) error {
	var perm *kube.PermissionError
	if errors.As(err, &perm) {
		return degradedf(ReasonForbidden,
			"source %q asks for finalize: true, which needs permission to update it:\n%s", src.ID, perm.Error())
	}
	var unknown *kube.UnknownKindError
	if errors.As(err, &unknown) {
		return waitingf(ReasonKindNotInstalled, "source %q needs %s, which is not installed", src.ID, gvk)
	}
	return fmt.Errorf("updating finalizers on source %q (%s %q): %w", src.ID, gvk.Kind, src.Name, err)
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
