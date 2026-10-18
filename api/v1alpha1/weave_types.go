package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/alethic/weft/api/v1"
)

// Weave reads existing resources in its namespace and generates others from
// them, reactively, as the ServiceAccount it names.
//
// This is the weft.run/v1alpha1 form. New Weaves should use
// weft.alethic.solutions/v1, which has the same spec.
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

	Spec   v1.WeaveSpec   `json:"spec,omitempty"`
	Status v1.WeaveStatus `json:"status,omitempty"`
}

// WeaveList contains a list of Weave.
//
// +kubebuilder:object:root=true
type WeaveList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Weave `json:"items"`
}
