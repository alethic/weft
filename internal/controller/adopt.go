package controller

import (
	"context"
	"errors"
	"fmt"
	"path"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	weftv1 "github.com/alethic/weft/api/v1"
	"github.com/alethic/weft/internal/inventory"
	"github.com/alethic/weft/internal/kube"
	"github.com/alethic/weft/internal/naming"
)

// checkOwnership refuses to write over an object this Weave did not create.
//
// Server-side apply is create-or-update, so without this a program that names
// an object somebody else already owns would silently take it over: the apply
// adds Weft's owner reference, and deleting the Weave then deletes a resource
// Weft never made. That is a lot of damage to do by choosing a name.
//
// The check runs only for resources not already recorded in the inventory. In a
// steady state nothing here costs a request; it is the first apply of a key, or
// an apply after the program changed what a key addresses, that pays for one
// Get.
func (r *WeaveReconciler) checkOwnership(
	ctx context.Context,
	c *kube.Client,
	weave *weftv1.Weave,
	item inventory.Item,
) error {
	gvk := item.GroupVersionKind()
	name := item.Object.GetName()

	// Already ours: the inventory records this object.
	if _, ok := weave.Status.InventoryFor(gvk.GroupVersion().String(), gvk.Kind, name); ok {
		return nil
	}

	existing, err := c.Get(ctx, gvk, name)
	switch {
	case apierrors.IsNotFound(err):
		// Nothing there. This is a create, which is the ordinary case.
		return nil
	case err != nil:
		var perm *kube.PermissionError
		if errors.As(err, &perm) {
			// The ServiceAccount cannot read what it is about to write. That is
			// its own problem and is reported as one, rather than being taken
			// as permission to overwrite blindly.
			return degradedf(ReasonForbidden,
				"checking whether %s already exists before creating it:\n%s", item.Ref, perm.Error())
		}
		var unknown *kube.UnknownKindError
		if errors.As(err, &unknown) {
			// Reported properly by the apply itself.
			return nil
		}
		var scoped *kube.ClusterScopedError
		if errors.As(err, &scoped) {
			return nil
		}
		return fmt.Errorf("checking whether %s already exists: %w", item.Ref, err)
	}

	api := r.Served.API
	owner := weftOwner(existing, weave, api)
	switch {
	case owner.otherWeave != "" && api.Predecessor != nil &&
		owner.otherAPI.Group == api.Predecessor.Group && owner.otherWeave == weave.Name:
		// The same Weave in the old group, still alive. This is the first half
		// of a move to this group, and the instruction is the second half.
		return degradedf(ReasonNotOurs, "%s", handoverMessage(gvk.Kind, name, weave, *api.Predecessor))

	case owner.otherWeave != "":
		// Another Weave owns it and would keep re-applying it. Adoption cannot
		// resolve that; the two would simply fight over the object.
		which := fmt.Sprintf("the Weave %q", owner.otherWeave)
		if owner.otherAPI.Group != api.Group {
			which = fmt.Sprintf("the %s Weave %q", owner.otherAPI.GroupVersion, owner.otherWeave)
		}
		return degradedf(ReasonNotOurs,
			"this program declares %s %q, which %s already owns. Two Weaves cannot manage one "+
				"object: they would apply over each other on every reconcile. Remove it from one of them.",
			gvk.Kind, name, which)

	case owner.handover:
		return r.handOver(ctx, c, weave, gvk, name)

	case !owner.ours && !owner.adoptableBy(weave.Name):
		return degradedf(ReasonNotOurs, "%s", adoptionMessage(item, existing, owner, weave.Name, api))

	case !owner.ours:
		// Consented to, so take it. Worth an event: ownership is a one-way door
		// in the sense that deleting the Weave will now delete this object.
		r.eventf(weave, "Normal", "Adopted",
			"took ownership of %s %q, which was annotated %s=%s. Deleting this Weave now deletes it.",
			gvk.Kind, name, api.AdoptAnnotation, owner.adopt)
		return nil

	case owner.reclaimed:
		// Ours by provenance but absent from the inventory: a status that was
		// lost, or an unowned resource that left the returned set and has come
		// back. Taking it up again is right - it is our object - but it is not
		// silent, because "Weft started managing this again" is the kind of
		// thing somebody looking at the object later wants to be able to find.
		//
		// After the key-collision check, not before it: an object reclaimed
		// under a different key than it was applied with is still two keys
		// naming one object.
		r.eventf(weave, "Normal", "Reclaimed",
			"took up %s %q again, which this Weave applied before but the inventory no longer recorded",
			gvk.Kind, name)
		return nil
	}

	// Ours, under this key, but the inventory did not know - a status that was
	// lost or rolled back. Reclaiming it is right: it is our object.
	return nil
}

// weftOwnership is what an existing object says about who made it.
type weftOwnership struct {
	// ours is true when this Weave made the object: it carries our owner
	// reference, or it carries our UID in its provenance.
	ours bool

	// reclaimed is true when ours was established from the provenance
	// annotation rather than an owner reference. That is a real event - the
	// inventory had lost the object - and worth reporting as one.
	reclaimed bool

	// weave is the name recorded in the provenance, when there is one. Used to
	// describe an object made by a different Weave that carries no owner
	// reference, which is what an unowned resource looks like.
	weave string
	// otherWeave names a different Weave that owns it, when one does, and
	// otherAPI is that Weave's group.
	otherWeave string
	otherAPI   naming.API

	// handover is true when the object was made by the Weave of the same name
	// in the group this one supersedes, and nothing owns it now.
	handover bool
	// manager is whichever field manager last wrote it, when nobody owns it.
	manager string
	// adopt is the value of the adopt annotation, when it carries one.
	adopt string
}

// adoptableBy reports whether the object has consented to being taken over by
// the named Weave.
//
// The annotation is a name or a shell-style pattern, so "*" consents to any
// Weave in the namespace. Onboarding a directory of existing objects otherwise
// means annotating each one with the same Weave name, which is typing that
// carries no information.
//
// A malformed pattern is not consent. Silently matching nothing is the right
// failure: the refusal message quotes the value back, so a broken pattern shows
// up as a refusal that names it rather than as an adoption nobody meant.
func (o weftOwnership) adoptableBy(weave string) bool {
	if o.adopt == "" {
		return false
	}
	if o.adopt == weave {
		return true
	}
	ok, err := path.Match(o.adopt, weave)
	return err == nil && ok
}

// weftOwner reports whether an object was created by this Weave.
//
// An owner reference decides it where there is one, because that is what
// garbage collection acts on. Where there is not - every unowned resource, and
// any owned one whose reference was stripped - the provenance annotation
// decides it instead. The rest is read only to describe what was found.
//
// Provenance is read under this Weave's own group's keys. A Weave in another
// group is recognised by its owner reference whichever group it is in, so two
// groups can never both believe they manage one object.
func weftOwner(obj *unstructured.Unstructured, weave *weftv1.Weave, api naming.API) weftOwnership {
	annotations := obj.GetAnnotations()
	labels := obj.GetLabels()
	out := weftOwnership{
		adopt: annotations[api.AdoptAnnotation],
		weave: labels[api.WeaveLabel],
	}

	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == weave.UID {
			out.ours = true
			return out
		}
		if other, ok := naming.IsWeaveOwnerRef(ref.APIVersion, ref.Kind); ok {
			// Another Weave's. Name it, since that is the useful thing to say.
			out.otherWeave = ref.Name
			out.otherAPI = other
			return out
		}
	}

	// No owner reference, which is what an unowned resource always looks like
	// and what an owned one looks like after somebody stripped it. The
	// provenance is then the only record that this Weave made the object, and
	// it is the record that lets it be reclaimed rather than refused.
	//
	// The UID is what makes this safe to do silently. Matching on the name
	// would let a Weave recreated under the same name help itself to its
	// predecessor's resources, and would make the label - which anybody who can
	// write the object can set - into a way to hand Weft something it never
	// made.
	if annotations[api.WeaveUIDAnnotation] == string(weave.UID) {
		out.ours = true
		out.reclaimed = true
		return out
	}

	// Made by the same-named Weave in the group this one supersedes, and owned
	// by nothing now: what a Weave moved to this group finds when its
	// predecessor was deleted with --cascade=orphan.
	//
	// This matches on the name, which the UID rule above deliberately does not.
	// That is safe here because it only ever applies across groups, where the
	// UID cannot match by construction, and because a name is the whole of what
	// a move preserves. Somebody who could forge the label could equally write
	// the adopt annotation, so it grants nothing that was not already theirs to
	// grant.
	if p := api.Predecessor; p != nil && annotations[p.WeaveUIDAnnotation] != "" && labels[p.WeaveLabel] == weave.Name {
		out.handover = true
		return out
	}

	if managers := obj.GetManagedFields(); len(managers) > 0 {
		out.manager = managers[0].Manager
	}
	return out
}

// adoptionMessage explains the refusal and what to do about it.
func adoptionMessage(item inventory.Item, existing *unstructured.Unstructured, owner weftOwnership, ownerName string, api naming.API) string {
	gvk := item.GroupVersionKind()
	msg := fmt.Sprintf("this program declares %s %q, which already exists and this Weave did not create.",
		gvk.Kind, item.Object.GetName())

	if owner.manager != "" {
		msg += fmt.Sprintf("\n\nIt was last written by %q.", owner.manager)
	}
	if owner.adopt != "" {
		msg += fmt.Sprintf("\n\nIt is annotated %s=%q, which does not match this Weave, %q.",
			api.AdoptAnnotation, owner.adopt, ownerName)
	}

	msg += "\n\nWeft will not take over an object it did not create. Applying would add its owner " +
		"reference, and deleting this Weave would then delete something it never made.\n\n" +
		"To hand it over deliberately, annotate the object itself. Consent belongs to whoever holds " +
		"the resource, not to whoever wrote the program:\n" +
		fmt.Sprintf("  kubectl -n %s annotate %s %s %s=%s\n\n",
			existing.GetNamespace(), gvk.Kind, item.Object.GetName(),
			api.AdoptAnnotation, ownerName) +
		"From then on this Weave manages it, and deleting the Weave deletes it. The value is a pattern, " +
		"so \"*\" consents to any Weave in this namespace - the form to reach for when onboarding a set " +
		"of objects at once.\n\nOtherwise change the name the program produces, or remove what is " +
		"there:\n" +
		fmt.Sprintf("  kubectl -n %s delete %s %s",
			existing.GetNamespace(), gvk.Kind, item.Object.GetName())
	return msg
}

// handOver takes over an object the same-named Weave in the predecessor group
// made, once that Weave is gone.
//
// Moving a Weave between groups is a delete and a create, because Kubernetes
// cannot convert an object from one group to another. Deleting the old Weave
// with --cascade=orphan leaves everything it made standing and unowned, and
// this is the other half: the new Weave finds each object as it declares it,
// recognises its predecessor's provenance, and takes it over in place. Nothing
// is recreated, which for a managed resource is the difference between a
// migration and an outage.
//
// It refuses while the predecessor still exists. Two live Weaves applying one
// object would fight over it on every reconcile, and the predecessor deleted
// without orphaning would be tearing the object down underneath this one.
func (r *WeaveReconciler) handOver(
	ctx context.Context,
	c *kube.Client,
	weave *weftv1.Weave,
	gvk schema.GroupVersionKind,
	name string,
) error {
	pred := *r.Served.API.Predecessor

	exists, deleting, err := r.weaveExists(ctx, pred, weave.Namespace, weave.Name)
	if err != nil {
		return fmt.Errorf("checking for the %s Weave %q: %w", pred.GroupVersion, weave.Name, err)
	}
	switch {
	case deleting:
		// Being deleted, with --cascade=orphan or not. Either way the answer is
		// soon; until then taking the object would race whatever it is doing.
		return waitingf(ReasonHandover,
			"%s %q was made by the %s Weave %q, which is being deleted; it is taken over once that finishes",
			gvk.Kind, name, pred.GroupVersion, weave.Name)
	case exists:
		return degradedf(ReasonNotOurs, "%s", handoverMessage(gvk.Kind, name, weave, pred))
	}

	// Its fields are owned by the predecessor's field manager. Moving them to
	// ours is what lets the apply that follows prune the predecessor's
	// provenance, and anything else the program no longer sets.
	if _, err := c.TransferFieldOwnership(ctx, gvk, name, pred.FieldManager); err != nil {
		var perm *kube.PermissionError
		if errors.As(err, &perm) {
			return degradedf(ReasonForbidden,
				"taking over %s %q from the %s Weave of the same name:\n%s",
				gvk.Kind, name, pred.GroupVersion, perm.Error())
		}
		if apierrors.IsNotFound(err) {
			// Gone in between. The apply creates it, which is fine.
			return nil
		}
		return fmt.Errorf("taking over %s %q: %w", gvk.Kind, name, err)
	}

	r.eventf(weave, "Normal", "HandedOver",
		"took over %s %q from the %s Weave of the same name", gvk.Kind, name, pred.GroupVersion)
	return nil
}

// weaveExists reports whether a Weave of the given group exists, and whether it
// is being deleted. A group whose CRD is not installed has no Weaves.
func (r *WeaveReconciler) weaveExists(ctx context.Context, api naming.API, namespace, name string) (exists, deleting bool, err error) {
	obj := &metav1.PartialObjectMetadata{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: api.Group, Version: api.Version, Kind: naming.Kind})
	err = r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj)
	switch {
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return false, false, nil
	case err != nil:
		return false, false, err
	}
	return true, !obj.DeletionTimestamp.IsZero(), nil
}

// handoverMessage explains how to finish moving a Weave from its predecessor
// group.
func handoverMessage(kind, name string, weave *weftv1.Weave, pred naming.API) string {
	return fmt.Sprintf("this program declares %s %q, which the %s Weave %q still manages.\n\n"+
		"To move it to this Weave, delete that one without deleting what it made:\n"+
		"  kubectl -n %s delete %s %s --cascade=orphan\n\n"+
		"Nothing is recreated: once it is gone this Weave takes over each object it declares, in place.",
		kind, name, pred.GroupVersion, weave.Name,
		weave.Namespace, pred.CRDName, weave.Name)
}
