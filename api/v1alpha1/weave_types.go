package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

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

	// Program is a Starlark program defining compose(variable), which declares
	// the resources that should exist by calling resource() on each of them.
	//
	// A resource is identified by the apiVersion, kind and name in its own body,
	// which is the only identity anything outside the program can see. Renaming
	// one is not a rename: it stops declaring one object and starts declaring
	// another, so the first is pruned and the second created.
	//
	// What a declared object looks like in the cluster now is reached through the
	// value resource() returned, not through a second parameter, because it is a
	// fact about that resource rather than a mapping to look it up in.
	//
	// +kubebuilder:validation:MinLength=1
	Program string `json:"program"`
}

// InventoryEntry records one resource this Weave created.
type InventoryEntry struct {
	// APIVersion, Kind and Name identify the live object, and are its whole
	// identity. A composition describes objects, so there is nothing else for
	// an entry to be about. Its namespace is
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

	// Owned records whether this Weave holds an owner reference on the
	// resource. False means it was applied with weft.run/owned=false and will
	// never be deleted by this Weave.
	//
	// It is remembered here rather than read from the object, because the
	// moment it matters is the moment the program stopped returning it - there
	// is no returned object left to read it from.
	//
	// Written on every entry rather than omitted when false, so that reading
	// the inventory shows which resources this Weave will take with it and
	// which it will leave standing.
	Owned bool `json:"owned"`

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

// HeldResource is a resource this Weave has placed a finalizer on, because a
// program read it with hold=True.
//
// It is recorded rather than derived because the request lives in the program,
// and a program that stops asking - or stops running at all - must still leave
// something behind that says what to release.
type HeldResource struct {
	// APIVersion of the held resource.
	APIVersion string `json:"apiVersion"`

	// Kind of the held resource.
	Kind string `json:"kind"`

	// Name of the held resource, in the Weave's own namespace.
	Name string `json:"name"`

	// TeardownStartedAt is set when a held resource began deleting. It starts
	// the clock on the hard timeout after which the finalizer is released
	// regardless of teardown progress.
	//
	// +optional
	TeardownStartedAt *metav1.Time `json:"teardownStartedAt,omitempty"`
}

// ReadRef identifies one resource a program read, or one selection it made.
type ReadRef struct {
	// APIVersion of what was read.
	APIVersion string `json:"apiVersion"`

	// Kind of what was read.
	Kind string `json:"kind"`
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
	// "not yet" is the normal one. A resource that is not there, an unpopulated status
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
	// +listType=atomic
	Inventory []InventoryEntry `json:"inventory,omitempty"`

	// Reads records the kinds this composition read on its last successful
	// pass. It is the watch set: a program that reads a ResourceGroup has to
	// be woken when one changes, and the only record of that is what it
	// actually asked for.
	//
	// Kept across restarts so a Weave that is waiting still has its watches
	// re-established without having to evaluate first.
	//
	// +optional
	// +listType=atomic
	Reads []ReadRef `json:"reads,omitempty"`

	// Held records the resources this Weave has placed a finalizer on.
	//
	// +optional
	// +listType=atomic
	Held []HeldResource `json:"held,omitempty"`
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

// InventoryFor returns the inventory entry for an object.
func (s *WeaveStatus) InventoryFor(apiVersion, kind, name string) (*InventoryEntry, bool) {
	for i := range s.Inventory {
		e := &s.Inventory[i]
		if e.APIVersion == apiVersion && e.Kind == kind && e.Name == name {
			return e, true
		}
	}
	return nil, false
}

// HeldByRef returns the tracked hold on a resource.
func (s *WeaveStatus) HeldByRef(apiVersion, kind, name string) (*HeldResource, bool) {
	for i := range s.Held {
		h := &s.Held[i]
		if h.APIVersion == apiVersion && h.Kind == kind && h.Name == name {
			return h, true
		}
	}
	return nil, false
}
