// Package naming is the single source of truth for every externally visible
// identifier Weft writes into a cluster.
//
// Weft serves two API groups. weft.alethic.solutions/v1 is the current one.
// weft.run/v1alpha1 is the group Weft was first deployed under, and is served
// alongside it so that installations built on it keep working and can move
// across one Weave at a time.
//
// Kubernetes cannot convert between groups the way it converts between
// versions of one group, so the two are separate CRDs and a Weave belongs to
// exactly one of them. Everything a Weave writes into the cluster - its
// finalizers, the provenance on what it applies, the field manager it applies
// with - carries its own group's keys. A weft.run Weave behaves exactly as it
// always has, and a v1 Weave never puts weft.run on anything.
//
// Nothing outside this package may hardcode either group.
package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	// Kind is the singular kind name, the same in every group.
	Kind = "Weave"
	// Plural is the lowercase plural, used for CRD names and RBAC rules.
	Plural = "weaves"
)

// API is one served API group and every identifier derived from it.
type API struct {
	// Group is the API group.
	Group string
	// Version is the API version served in that group.
	Version string

	// GroupVersion is the "group/version" string used in apiVersion fields.
	GroupVersion string

	// CRDName is the metadata.name a CustomResourceDefinition must carry.
	CRDName string

	// FieldManager is the server-side-apply field manager for every write a
	// Weave in this group makes. Ownership of a field is determined by this
	// string, so it must be stable across releases.
	FieldManager string

	// WeaveFinalizer is placed on a Weave so outputs can be torn down in
	// reverse dependency order before the object disappears.
	WeaveFinalizer string

	// HeldFinalizerPrefix begins every finalizer a Weave places on a resource
	// a program holds. Fixed so a human can enumerate and strip them:
	//   kubectl get <kind> -o json | jq '.items[].metadata.finalizers'
	HeldFinalizerPrefix string

	// Provenance stamped onto every resource a Weave applies.
	//
	// Together these are the record on the object itself of what made it,
	// which Weave, and under what identity. The Weave's own status says the
	// same thing, but a status can be lost - restored from a backup, wiped, or
	// never written because a pass failed between the apply and the status
	// update. An object that carries its own provenance can be reclaimed from
	// that; one that does not has to be adopted by hand.
	//
	// It is deliberately only facts that do not change from pass to pass. A
	// timestamp or a revision here would rewrite every object on every
	// reconcile, and every one of those writes is a watch event that wakes the
	// Weave that just made it.

	// WeaveLabel names the Weave that applied a resource. A label (not an
	// annotation) so outputs are selectable with kubectl.
	WeaveLabel string

	// WeaveUIDAnnotation records the UID of the Weave that applied it.
	//
	// The name is not enough on its own. A Weave deleted and recreated with
	// the same name is a different object with a different UID, and its
	// predecessor's unowned resources are not automatically its to take back -
	// whereas a UID match is the same Weave, whatever happened to its status in
	// between.
	WeaveUIDAnnotation string

	// OwnedAnnotation decides whether Weft owns a resource it applies. Its
	// accepted values are "true" (the default) and "false".
	//
	// An unowned resource is still applied and still kept up to date, but no
	// owner reference is placed on it: it is not deleted when the program
	// stops returning it, and not collected when the Weave is deleted. It says
	// the object outlives the composition that describes it, which is a thing
	// no amount of pruning hysteresis can express - hysteresis is about how
	// long to wait, and this is about never.
	//
	// The absence of the reference is what makes the guarantee real rather
	// than conditional on this controller running at the moment somebody
	// reaches for it.
	OwnedAnnotation string

	// AdoptAnnotation opts an existing object into being taken over by a
	// Weave. Its value is a name, or a pattern: "*" consents to any Weave in
	// this namespace, and "app-*" to any whose name starts with app-.
	//
	// It lives on the object being adopted rather than in the Weave on
	// purpose. Consent has to come from whoever holds the thing, or "adoption"
	// is just a Weave author choosing a name and helping themselves to
	// somebody else's resource - which is the whole reason the unannotated case
	// is refused.
	//
	// A pattern is still consent, and still namespaced: a Weave only ever acts
	// in its own namespace, so "*" grants no more than "anybody who can already
	// create a Weave here". It is the form to reach for when onboarding a set
	// of objects at once, where naming the Weave on each of them is busywork
	// that says nothing extra.
	AdoptAnnotation string

	// Predecessor is the group this one supersedes, if any. A Weave in this
	// group may take over what a Weave of the same name in the predecessor
	// made, once that Weave is gone; see the controller's handover.
	Predecessor *API
}

func newAPI(group, version string, predecessor *API) API {
	return API{
		Group:               group,
		Version:             version,
		GroupVersion:        group + "/" + version,
		CRDName:             Plural + "." + group,
		FieldManager:        group,
		WeaveFinalizer:      group + "/weave",
		HeldFinalizerPrefix: group + "/held-",
		WeaveLabel:          group + "/weave",
		WeaveUIDAnnotation:  group + "/weave-uid",
		OwnedAnnotation:     group + "/owned",
		AdoptAnnotation:     group + "/adopt",
		Predecessor:         predecessor,
	}
}

var (
	// Legacy is weft.run/v1alpha1, the group Weft was first deployed under.
	// Its identifiers must never change: they are on objects in clusters that
	// predate v1.
	Legacy = newAPI("weft.run", "v1alpha1", nil)

	// V1 is weft.alethic.solutions/v1, the current API.
	V1 = newAPI("weft.alethic.solutions", "v1", &Legacy)

	// APIs lists every served group, current first.
	APIs = []API{V1, Legacy}
)

// ForGroup returns the API for a group name.
func ForGroup(group string) (API, bool) {
	for _, a := range APIs {
		if a.Group == group {
			return a, true
		}
	}
	return API{}, false
}

// IsWeaveOwnerRef reports whether an owner reference names a Weave in any
// served group, and which.
func IsWeaveOwnerRef(apiVersion, kind string) (API, bool) {
	if kind != Kind {
		return API{}, false
	}
	for _, a := range APIs {
		if a.GroupVersion == apiVersion {
			return a, true
		}
	}
	return API{}, false
}

// HeldFinalizer returns the finalizer a given Weave places on a resource it has
// asked to hold.
//
// A finalizer is a qualified name: the part after the slash is limited to 63
// characters and a restricted alphabet, so the Weave's namespace and name are
// hashed rather than interpolated. The prefix stays literal so the finalizer is
// still recognisable and greppable.
func (a API) HeldFinalizer(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return a.HeldFinalizerPrefix + hex.EncodeToString(sum[:])[:16]
}

// IsHeldFinalizer reports whether a finalizer string was placed by a Weave in
// this group.
func (a API) IsHeldFinalizer(f string) bool {
	return len(f) > len(a.HeldFinalizerPrefix) && strings.HasPrefix(f, a.HeldFinalizerPrefix)
}

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
