// Package v1 contains the Weave API, weft.alethic.solutions/v1.
//
// These are the canonical types. weft.run/v1alpha1 serves the same spec and
// status under the group Weft was first deployed with, by embedding them, so
// the two schemas cannot drift apart.
//
// The +groupName marker below is the one place the API group appears as a
// literal: kubebuilder reads comments, not constants, so it cannot be derived
// from naming.V1. TestGroupMarkerMatchesNaming asserts the two agree.
//
// +kubebuilder:object:generate=true
// +groupName=weft.alethic.solutions
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/alethic/weft/internal/naming"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: naming.V1.Group, Version: naming.V1.Version}

	// SchemeBuilder registers the types in this group-version.
	//
	// This is apimachinery's builder rather than controller-runtime's, which is
	// deprecated for api packages: an api package should be cheap to import,
	// and pulling controller-runtime in makes it anything but.
	SchemeBuilder = &runtime.SchemeBuilder{}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// addKnownTypes registers every type in this group-version.
func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &Weave{}, &WeaveList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

func init() {
	SchemeBuilder.Register(addKnownTypes)
}
