package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Source declares an existing resource this composition reads, or waits on.
//
// Sources are resources; variables are keys. A program reaches a source through
// sources.<id> and gets the whole object, or None when it does not exist.
//
// Whether an absent source should block is the program's decision, written in
// its body rather than declared here:
//
//	if not sources.database:
//	    return wait("the database has not been created yet")
//
// which also expresses the thing a flag on this struct could not - gating that
// depends on configuration:
//
//	if variable.useSql and not sources.database:
//	    return wait("SQL is enabled but the database is not there yet")
//
// The list itself is static and declarative, because it is what the permission
// checks and the watch registration key off, and neither can wait for an
// evaluation that needs the sources first. A program cannot reach a resource
// not declared here.
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

// Variable is one entry in spec.variables. Exactly one field is set.
//
// +kubebuilder:validation:XValidation:rule="(has(self.values) ? 1 : 0) + (has(self.configMap) ? 1 : 0) + (has(self.secret) ? 1 : 0) == 1",message="set exactly one of values, configMap or secret"
type Variable struct {
	// Values is configuration written here. Plain YAML, never templated or
	// evaluated.
	//
	// The explicit type is what lets the validation rule above see this field
	// at all: a property with preserved unknown fields and no type is invisible
	// to CEL, and the rule fails to compile rather than failing to apply.
	//
	// +optional
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	Values *apiextensionsv1.JSON `json:"values,omitempty"`

	// ConfigMap takes configuration from a ConfigMap in this namespace.
	//
	// +optional
	ConfigMap *VariableRef `json:"configMap,omitempty"`

	// Secret takes configuration from a Secret in this namespace.
	//
	// Read through the same impersonated client as everything else, so a Weave
	// can only read a Secret its ServiceAccount could read directly. Be aware
	// that a value reaching a program can be written into any resource that
	// ServiceAccount may create.
	//
	// +optional
	Secret *VariableRef `json:"secret,omitempty"`
}

// VariableRef selects a ConfigMap or Secret to take configuration from.
type VariableRef struct {
	// Name of the object, in the Weave's own namespace.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Key selects a single entry, whose content is parsed as YAML and merged as
	// a mapping. Without it every entry becomes one variable, with its value as a
	// string.
	//
	// This is the difference between a ConfigMap of flat settings and one
	// holding a values.yaml document, and both are common enough to deserve
	// support.
	//
	// +optional
	Key string `json:"key,omitempty"`

	// Optional skips this entry when the object does not exist. Without it a
	// missing object leaves the Weave waiting, because a composition built on
	// configuration that has not arrived is not ready.
	//
	// +optional
	Optional bool `json:"optional,omitempty"`
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

	// Variables is static configuration, assembled from these entries and
	// reaching the program as one mapping bound to variable.
	//
	// Entries are merged in order and later ones win, so a base can come from a
	// ConfigMap somebody else maintains and be overridden inline here. Mappings
	// merge key by key; anything else is replaced outright, which is the same
	// rule Helm values follow and the one people already expect.
	//
	// A program cannot tell where a value came from, which is the point: moving
	// a setting from inline to a ConfigMap is not a change to the composition.
	//
	// Variables are keys; sources are resources. Nothing here is watched for
	// the sake of an ordering edge - a ConfigMap named here is read for the
	// values in it, and a change to it re-runs the program.
	//
	// +optional
	// +listType=atomic
	Variables []Variable `json:"variables,omitempty"`

	// Sources are the existing resources this composition reads.
	//
	// +optional
	// +listType=map
	// +listMapKey=id
	Sources []Source `json:"sources,omitempty"`

	// Program is a Starlark program defining
	// compose(variable, sources, observed), returning a mapping of stable key to
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

	// Superseded are objects this Weave created and no longer describes, which
	// are waiting to be deleted.
	//
	// They arrive here when a program is edited to change the name or kind
	// under a key it still returns. The new object is recorded in the inventory
	// under that key, so the old one has nowhere left to be recorded and would
	// otherwise be orphaned - present in the cluster, absent from every record,
	// never cleaned up. This is an atomic list rather than a map because an
	// entry here shares its key with the live object that replaced it.
	//
	// +optional
	// +listType=atomic
	Superseded []InventoryEntry `json:"superseded,omitempty"`

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
