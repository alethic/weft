// Package naming is the single source of truth for every externally visible
// identifier Weft writes into a cluster. The API group is provisional; renaming
// it must be one edit here, so nothing anywhere else may hardcode "weft.run".
package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const (
	// Group is the API group. Everything below derives from it.
	Group = "weft.run"
	// Version is the current (and only) API version.
	Version = "v1alpha1"
	// Kind is the singular kind name.
	Kind = "Weave"
	// Plural is the lowercase plural, used for CRD names and RBAC rules.
	Plural = "weaves"
)

// GroupVersion is the "group/version" string used in apiVersion fields.
var GroupVersion = Group + "/" + Version

// CRDName is the metadata.name a CustomResourceDefinition must carry.
var CRDName = Plural + "." + Group

// FieldManager is the server-side-apply field manager for every write Weft
// makes. Ownership of a field is determined by this string, so it must be
// stable across releases.
var FieldManager = Group

const (
	// WeaveFinalizer is placed on a Weave so outputs can be torn down in
	// reverse dependency order before the object disappears.
	weaveFinalizerPath = "weave"
	// sourceFinalizerPrefix begins every finalizer Weft places on a resource
	// it does not own. Fixed so a human can enumerate and strip them:
	//   kubectl get <kind> -o json | jq '.items[].metadata.finalizers'
	heldFinalizerPath = "held-"
)

// WeaveFinalizer is the finalizer Weft places on Weave objects.
var WeaveFinalizer = Group + "/" + weaveFinalizerPath

// HeldFinalizerPrefix is the common prefix of every finalizer Weft places on a resource a program holds. Used
// both to recognise our own finalizers during reaping and to document how to
// strip them by hand.
var HeldFinalizerPrefix = Group + "/" + heldFinalizerPath

// HeldFinalizer returns the finalizer a given Weave places on a resource it has
// asked to hold.
//
// A finalizer is a qualified name: the part after the slash is limited to 63
// characters and a restricted alphabet, so the Weave's namespace and name are
// hashed rather than interpolated. The prefix stays literal so the finalizer is
// still recognisable and greppable.
func HeldFinalizer(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return HeldFinalizerPrefix + hex.EncodeToString(sum[:])[:16]
}

// IsHeldFinalizer reports whether a finalizer string was placed by Weft.
func IsHeldFinalizer(f string) bool {
	return len(f) > len(HeldFinalizerPrefix) && f[:len(HeldFinalizerPrefix)] == HeldFinalizerPrefix
}

// Provenance stamped onto every resource Weft applies.
//
// Together these are the record on the object itself of what made it, which
// Weave, and under what identity. The Weave's own status says the same thing,
// but a status can be lost - restored from a backup, wiped, or never written
// because a pass failed between the apply and the status update. An object that
// carries its own provenance can be reclaimed from that; one that does not has
// to be adopted by hand.
//
// It is deliberately only facts that do not change from pass to pass. A
// timestamp or a revision here would rewrite every object on every reconcile,
// and every one of those writes is a watch event that wakes the Weave that just
// made it.
var (
	// WeaveLabel names the Weave that applied a resource. A label (not an
	// annotation) so outputs are selectable with kubectl.
	WeaveLabel = Group + "/weave"

	// WeaveUIDAnnotation records the UID of the Weave that applied it.
	//
	// The name is not enough on its own. A Weave deleted and recreated with the
	// same name is a different object with a different UID, and its
	// predecessor's unowned resources are not automatically its to take back -
	// whereas a UID match is the same Weave, whatever happened to its status in
	// between.
	WeaveUIDAnnotation = Group + "/weave-uid"

	// KeyAnnotation records the inventory key a resource was created under.
	// This is the stable identity returned by compose(), not the object name.
	KeyAnnotation = Group + "/key"

	// WaveAnnotation optionally overrides a resource's teardown wave. Lower
	// waves are applied first and deleted last.
	WaveAnnotation = Group + "/wave"

	// OwnedAnnotation decides whether Weft owns a resource it applies. Its
	// accepted values are "true" (the default) and "false".
	//
	// An unowned resource is still applied and still kept up to date, but no
	// owner reference is placed on it: it is not deleted when the program stops
	// returning it, and not collected when the Weave is deleted. It says the
	// object outlives the composition that describes it, which is a thing no
	// amount of pruning hysteresis can express - hysteresis is about how long
	// to wait, and this is about never.
	//
	// The absence of the reference is what makes the guarantee real rather than
	// conditional on this controller running at the moment somebody reaches for
	// it.
	OwnedAnnotation = Group + "/owned"

	// AdoptAnnotation opts an existing object into being taken over by a Weave.
	// Its value is a name, or a pattern: "*" consents to any Weave in this
	// namespace, and "app-*" to any whose name starts with app-.
	//
	// It lives on the object being adopted rather than in the Weave on purpose.
	// Consent has to come from whoever holds the thing, or "adoption" is just a
	// Weave author choosing a name and helping themselves to somebody else's
	// resource - which is the whole reason the unannotated case is refused.
	//
	// A pattern is still consent, and still namespaced: a Weave only ever acts
	// in its own namespace, so "*" grants no more than "anybody who can already
	// create a Weave here". It is the form to reach for when onboarding a set
	// of objects at once, where naming the Weave on each of them is busywork
	// that says nothing extra.
	AdoptAnnotation = Group + "/adopt"
)

// ConditionType values published on Weave.status.conditions.
const (
	// ConditionReady is true when every resource compose() returned has been
	// applied and nothing is outstanding.
	ConditionReady = "Ready"
	// ConditionWaiting is true when evaluation could not complete because
	// something it depends on has not resolved yet. This is a normal steady
	// state, not an error.
	ConditionWaiting = "Waiting"
	// ConditionDegraded is true when something is permanently wrong and will
	// not resolve without a change: a program error, a denied permission, an
	// output that violates a hard rule.
	ConditionDegraded = "Degraded"
)

// UserFor renders the impersonation username for a namespace/serviceaccount
// pair. The API server special-cases this form and authorises it against the
// serviceaccounts resource in that namespace, which is what lets the
// controller's impersonate grant stay namespace-scopeable.
func UserFor(namespace, serviceAccount string) string {
	return fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccount)
}
