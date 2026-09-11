package kube

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/alethic/weft/internal/naming"
)

// Owner is the object every generated resource is owned by.
type Owner struct {
	APIVersion string
	Kind       string
	Name       string
	UID        types.UID
}

// derivedMetadata is server-populated metadata a program will pick up if it
// derives an output from an observed object, which is a normal thing to do.
// Carrying any of it into an apply is at best noise and at worst an attempt to
// claim another object's identity, so it is dropped rather than rejected.
var derivedMetadata = []string{
	"uid",
	"resourceVersion",
	"generation",
	"creationTimestamp",
	"deletionTimestamp",
	"deletionGracePeriodSeconds",
	"managedFields",
	"selfLink",
}

// Normalize turns a resource as returned by a program into an object ready to
// apply.
//
// Ownership is not negotiable here. Every output lives in the Weave's own
// namespace and is owned by the Weave, which is what makes plain cascading
// garbage collection sufficient and is why there is no ApplySet machinery
// anywhere in this codebase. The cross-namespace and cluster-scoped cases that
// would force something more elaborate are not expressible.
func Normalize(obj map[string]any, key, namespace string, owner Owner, owned bool) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{Object: obj}

	if ns := u.GetNamespace(); ns != "" && ns != namespace {
		return nil, fmt.Errorf(
			"sets metadata.namespace to %q: a Weave can only create resources in its own namespace (%q)",
			ns, namespace)
	}
	u.SetNamespace(namespace)

	if len(u.GetOwnerReferences()) > 0 {
		return nil, fmt.Errorf(
			"sets metadata.ownerReferences: Weft owns what it creates and sets the reference itself, so that deleting the Weave collects the resource")
	}

	md, found, err := unstructured.NestedMap(u.Object, "metadata")
	if err != nil {
		return nil, fmt.Errorf("metadata is not an object: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("missing metadata")
	}
	for _, f := range derivedMetadata {
		delete(md, f)
	}
	if err := unstructured.SetNestedMap(u.Object, md, "metadata"); err != nil {
		return nil, err
	}

	// status is a subresource on anything worth generating; a value here is
	// never applied and only ever reflects a misunderstanding.
	delete(u.Object, "status")

	if name := u.GetName(); len(name) > 253 {
		return nil, fmt.Errorf("metadata.name is %d characters; the limit is 253", len(name))
	}

	// An unowned resource gets no reference at all. Setting one and taking it
	// off later would leave a window in which deleting the Weave collects an
	// object that was never meant to be collected.
	if owned {
		blockOwnerDeletion := true
		controller := true
		u.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion:         owner.APIVersion,
			Kind:               owner.Kind,
			Name:               owner.Name,
			UID:                owner.UID,
			Controller:         &controller,
			BlockOwnerDeletion: &blockOwnerDeletion,
		}})
	}

	labels := u.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[naming.WeaveLabel] = owner.Name
	u.SetLabels(labels)

	annotations := u.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[naming.KeyAnnotation] = key
	annotations[naming.WeaveUIDAnnotation] = string(owner.UID)
	u.SetAnnotations(annotations)

	return u, nil
}
