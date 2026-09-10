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
	sourceFinalizerPath = "src-"
)

// WeaveFinalizer is the finalizer Weft places on Weave objects.
var WeaveFinalizer = Group + "/" + weaveFinalizerPath

// SourceFinalizerPrefix is the common prefix of every source finalizer. Used
// both to recognise our own finalizers during reaping and to document how to
// strip them by hand.
var SourceFinalizerPrefix = Group + "/" + sourceFinalizerPath

// SourceFinalizer returns the finalizer a given Weave places on a source it has
// asked to finalize.
//
// A finalizer is a qualified name: the part after the slash is limited to 63
// characters and a restricted alphabet, so the Weave's namespace and name are
// hashed rather than interpolated. The prefix stays literal so the finalizer is
// still recognisable and greppable.
func SourceFinalizer(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return SourceFinalizerPrefix + hex.EncodeToString(sum[:])[:16]
}

// IsSourceFinalizer reports whether a finalizer string was placed by Weft.
func IsSourceFinalizer(f string) bool {
	return len(f) > len(SourceFinalizerPrefix) && f[:len(SourceFinalizerPrefix)] == SourceFinalizerPrefix
}

// Label and annotation keys stamped onto every resource Weft creates.
var (
	// WeaveLabel names the Weave that owns a resource. A label (not an
	// annotation) so outputs are selectable with kubectl.
	WeaveLabel = Group + "/weave"
	// KeyAnnotation records the inventory key a resource was created under.
	// This is the stable identity returned by compose(), not the object name.
	KeyAnnotation = Group + "/key"
	// WaveAnnotation optionally overrides a resource's teardown wave. Lower
	// waves are applied first and deleted last.
	WaveAnnotation = Group + "/wave"
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
