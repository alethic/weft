// Package v1alpha1 contains the Weave API.
//
// The +groupName marker below is the one place the API group appears as a
// literal: kubebuilder reads comments, not constants, so it cannot be derived
// from naming.Group. TestGroupMarkerMatchesNaming asserts the two agree, so a
// rename that misses this file fails the tests rather than shipping a CRD in
// the wrong group.
//
// +kubebuilder:object:generate=true
// +groupName=weft.run
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/alethic/weft/internal/naming"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: naming.Group, Version: naming.Version}

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
