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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"

	"github.com/alethic/weft/internal/naming"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: naming.Group, Version: naming.Version}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
