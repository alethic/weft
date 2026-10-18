// Package v1alpha1 contains weft.run/v1alpha1, the group Weft was first
// deployed under.
//
// It is served alongside weft.alethic.solutions/v1 so that installations built
// on it keep working and can move to v1 one Weave at a time. Its spec and
// status are v1's, embedded rather than copied, so the two cannot drift: the
// groups differ in name and in the keys a Weave writes onto what it manages,
// never in what a Weave can express.
//
// The +groupName marker below is the one place the API group appears as a
// literal: kubebuilder reads comments, not constants, so it cannot be derived
// from naming.Legacy. TestGroupMarkerMatchesNaming asserts the two agree.
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
	GroupVersion = schema.GroupVersion{Group: naming.Legacy.Group, Version: naming.Legacy.Version}

	// SchemeBuilder registers the types in this group-version.
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
