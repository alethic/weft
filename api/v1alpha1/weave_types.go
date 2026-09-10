package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Source declares an existing resource to read.
//
// The source list is static and declarative on purpose: it is what the
// permission precheck and the watch registration key off, so it can never be
// derived from evaluation. A program cannot reach a resource not declared here.
type Source struct {
	// ID is the name this source is bound to inside the program, as
	// sources.<id>. It must be a legal Starlark identifier.
	//
	// +kubebuilder:validation:Pattern="^[A-Za-z_][A-Za-z0-9_]*$"
	// +kubebuilder:validation:MaxLength=63
	ID string `json:"id"`

	// APIVersion of the resource to read, e.g. "azure.m.upbound.io/v1beta1",
	// or "v1" for core types.
	//
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`

	// Kind of the resource to read.
	//
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Name of the resource to read. Always in the Weave's own namespace;
	// cross-namespace reads are not expressible.
	//
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Required gates evaluation on this resource existing, even when the
	// program never reads a field from it. Without this there is no way to
	// express a pure ordering edge, because a dependency that is never
	// referenced is invisible to any form of reference inference.
	//
	// +optional
	Required bool `json:"required,omitempty"`

	// Finalize places a finalizer on this resource so outputs derived from it
	// are torn down before it is allowed to disappear.
	//
	// This requires update permission on the resource under the Weave's
	// ServiceAccount. Missing permission surfaces as Degraded rather than
	// silently doing nothing. The finalizer is released after the configured
	// timeout regardless of teardown progress: blocking somebody else's
	// object, and their namespace deletion, forever is worse than an ordering
	// violation.
	//
	// +optional
	Finalize bool `json:"finalize,omitempty"`
}

// WeaveSpec defines a composition.
type WeaveSpec struct {
	// ServiceAccountName names a ServiceAccount in this namespace. Every read
	// and every write Weft performs for this Weave is impersonated as that
	// ServiceAccount.
	//
	// The controller holds impersonation rights, not write access, so a Weave
	// can never cause the creation of anything its ServiceAccount could not
	// have created directly.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ServiceAccountName string `json:"serviceAccountName"`

	// Inputs is static configuration, passed to the program as-is. Plain
	// YAML, never templated or evaluated.
	//
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Inputs *apiextensionsv1.JSON `json:"inputs,omitempty"`

	// Sources are the existing resources this composition reads.
	//
	// +optional
	// +listType=map
	// +listMapKey=id
	Sources []Source `json:"sources,omitempty"`

	// Program is a Starlark program defining
	// compose(inputs, sources, observed), returning a mapping of stable key to
	// resource. Keys are the inventory identity: renaming a key does not
	// rename anything, it deletes one resource and creates another.
	//
	// +kubebuilder:validation:MinLength=1
	Program string `json:"program"`
}

// InventoryEntry records one resource this Weave created.
type InventoryEntry struct {
	// Key is the stable identity: the key compose() returned this resource
	// under. Never positional, so reordering a list cannot rename a live
	// object.
	Key string `json:"key"`

	// APIVersion, Kind and Name identify the live object. Its namespace is
	// always the Weave's own.
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`

	// UID of the applied object, recorded so teardown can tell a resource we
	// created from a later resource that happens to share its name.
	//
	// +optional
	UID types.UID `json:"uid,omitempty"`

	// Wave is the apply order. Resources are applied in ascending wave and
	// deleted in descending wave, because cascading garbage collection is
	// unordered and self-reference creates a real internal DAG.
	Wave int32 `json:"wave"`

	// MissingCount is the number of consecutive successful evaluations in
	// which this resource was not returned.
	//
	// It stops climbing once it reaches the threshold. That is not cosmetic:
	// a status write wakes the Weave through its own watch, so a field that
	// changes on every pass is a self-sustaining reconcile loop.
	//
	// +optional
	MissingCount int32 `json:"missingCount,omitempty"`

	// MissingSince is when this resource first stopped being returned by a
	// successful evaluation, and is cleared the moment it comes back.
	//
	// Pruning waits on elapsed time, not only on a count. Reconciles are
	// event-driven and the applies in a single pass generate watch events of
	// their own, so several "consecutive evaluations" can complete inside a
	// second - which is no protection at all against the thing hysteresis
	// exists for. A managed resource drops its status for as long as its
	// provider takes to restart, and a program waiting on that status
	// legitimately stops returning whatever depends on it for that whole
	// period. Only a clock measures that.
	//
	// +optional
	MissingSince *metav1.Time `json:"missingSince,omitempty"`

	// There is deliberately no "last applied" timestamp here. Every reconcile
	// re-applies, so such a field would change on every pass, and because a
	// status write wakes the Weave through its own watch it would spin every
	// Weave in the cluster forever for a purely informational value. The
	// object's own managedFields already record when Weft last wrote to it.
}

// SourceStatus tracks per-source state that has to survive a controller
// restart.
type SourceStatus struct {
	// ID matches the declared source.
	ID string `json:"id"`

	// Finalized is true once Weft has successfully placed its finalizer on
	// the source.
	//
	// +optional
	Finalized bool `json:"finalized,omitempty"`

	// TeardownStartedAt is set when a finalized source began deleting. It
	// starts the clock on the hard timeout after which the finalizer is
	// released regardless of teardown progress.
	//
	// +optional
	TeardownStartedAt *metav1.Time `json:"teardownStartedAt,omitempty"`
}

// WeaveStatus is the observed state of a Weave.
type WeaveStatus struct {
	// ObservedGeneration is the spec generation most recently evaluated.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follow the standard Ready / Waiting / Degraded split.
	//
	// Waiting deserves emphasis: resolution has three outcomes, not two, and
	// "not yet" is the normal one. A missing source, an unpopulated status
	// field and an explicit wait() all land here, each naming the specific
	// thing that has not resolved.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Inventory is every resource this Weave currently owns, with its apply
	// order.
	//
	// +optional
	// +listType=map
	// +listMapKey=key
	Inventory []InventoryEntry `json:"inventory,omitempty"`

	// Sources carries per-source bookkeeping for finalized sources.
	//
	// +optional
	// +listType=map
	// +listMapKey=id
	Sources []SourceStatus `json:"sources,omitempty"`
}

// Weave reads existing resources in its namespace and generates others from
// them, reactively, as the ServiceAccount it names.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=wv
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Waiting",type=string,JSONPath=".status.conditions[?(@.type=='Waiting')].reason"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type Weave struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WeaveSpec   `json:"spec,omitempty"`
	Status WeaveStatus `json:"status,omitempty"`
}

// WeaveList contains a list of Weave.
//
// +kubebuilder:object:root=true
type WeaveList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Weave `json:"items"`
}

// SourceByID returns the declared source with the given id.
func (s *WeaveSpec) SourceByID(id string) (Source, bool) {
	for _, src := range s.Sources {
		if src.ID == id {
			return src, true
		}
	}
	return Source{}, false
}

// InventoryByKey returns the inventory entry for a key.
func (s *WeaveStatus) InventoryByKey(key string) (*InventoryEntry, bool) {
	for i := range s.Inventory {
		if s.Inventory[i].Key == key {
			return &s.Inventory[i], true
		}
	}
	return nil, false
}

// SourceStatusByID returns the tracked status for a source id.
func (s *WeaveStatus) SourceStatusByID(id string) (*SourceStatus, bool) {
	for i := range s.Sources {
		if s.Sources[i].ID == id {
			return &s.Sources[i], true
		}
	}
	return nil, false
}
