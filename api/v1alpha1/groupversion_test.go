package v1alpha1

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/alethic/weft/internal/naming"
)

// The API group is provisional and expected to change before v1alpha1 is
// published. Everything derives from naming.Group so a rename is one edit -
// everything except the +groupName marker, which kubebuilder reads as a comment
// and which therefore cannot reference a constant.
//
// This test is what keeps that one exception honest: a rename that misses the
// marker fails here instead of shipping a CRD in one group and a controller
// that talks to another.
func TestGroupMarkerMatchesNaming(t *testing.T) {
	src, err := os.ReadFile("groupversion_info.go")
	if err != nil {
		t.Fatal(err)
	}

	marker := regexp.MustCompile(`(?m)^//\s*\+groupName=(\S+)\s*$`)
	m := marker.FindSubmatch(src)
	if m == nil {
		t.Fatal("groupversion_info.go has no +groupName marker; controller-gen needs one to emit the CRD")
	}

	if got := string(m[1]); got != naming.Group {
		t.Errorf("+groupName=%s but naming.Group is %q; the CRD and the controller would disagree", got, naming.Group)
	}
}

func TestGroupVersionMatchesNaming(t *testing.T) {
	if GroupVersion.Group != naming.Group {
		t.Errorf("GroupVersion.Group = %q, want %q", GroupVersion.Group, naming.Group)
	}
	if GroupVersion.Version != naming.Version {
		t.Errorf("GroupVersion.Version = %q, want %q", GroupVersion.Version, naming.Version)
	}
	if GroupVersion.String() != naming.GroupVersion {
		t.Errorf("GroupVersion = %q, want %q", GroupVersion.String(), naming.GroupVersion)
	}
}

// The generated CRD must agree too, since it is what the API server enforces.
func TestGeneratedCRDMatchesNaming(t *testing.T) {
	path := "../../config/crd/" + naming.Group + "_" + naming.Plural + ".yaml"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("generated CRD not present at %s; run make generate", path)
	}
	text := string(src)

	for _, want := range []string{
		"name: " + naming.CRDName,
		"group: " + naming.Group,
		"kind: " + naming.Kind,
		"plural: " + naming.Plural,
		"name: " + naming.Version,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("generated CRD is missing %q", want)
		}
	}

	if !strings.Contains(text, "scope: Namespaced") {
		t.Error("the CRD must be namespaced: a cluster-scoped kind would put a platform team back in the loop, " +
			"which is the property the project exists to remove")
	}
}

func TestHelpers(t *testing.T) {
	status := WeaveStatus{
		Inventory: []InventoryEntry{{Key: "a"}, {Key: "b"}},
		Held: []HeldResource{
			{APIVersion: "v1", Kind: "ConfigMap", Name: "upstream"},
		},
	}
	if e, ok := status.InventoryByKey("b"); !ok || e.Key != "b" {
		t.Error("InventoryByKey should find an entry")
	}
	if _, ok := status.InventoryByKey("c"); ok {
		t.Error("InventoryByKey should not invent one")
	}
	if _, ok := status.HeldByRef("v1", "ConfigMap", "upstream"); !ok {
		t.Error("HeldByRef should find a recorded hold")
	}
	if _, ok := status.HeldByRef("v1", "Secret", "upstream"); ok {
		t.Error("HeldByRef must match on kind, not only name")
	}
}
