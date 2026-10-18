package v1_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	v1 "github.com/alethic/weft/api/v1"
	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/naming"
)

// served pairs each API package with the naming entry it must agree with.
var served = []struct {
	dir string
	gv  schema.GroupVersion
	api naming.API
}{
	{"../v1", v1.GroupVersion, naming.V1},
	{"../v1alpha1", v1alpha1.GroupVersion, naming.Legacy},
}

// Everything derives from the naming package - everything except the
// +groupName marker, which kubebuilder reads as a comment and which therefore
// cannot reference a constant.
//
// This test is what keeps that one exception honest: a marker that disagrees
// with naming fails here instead of shipping a CRD in one group and a
// controller that talks to another.
func TestGroupMarkerMatchesNaming(t *testing.T) {
	marker := regexp.MustCompile(`(?m)^//\s*\+groupName=(\S+)\s*$`)
	for _, s := range served {
		src, err := os.ReadFile(s.dir + "/groupversion_info.go")
		if err != nil {
			t.Fatal(err)
		}
		m := marker.FindSubmatch(src)
		if m == nil {
			t.Fatalf("%s has no +groupName marker; controller-gen needs one to emit the CRD", s.dir)
		}
		if got := string(m[1]); got != s.api.Group {
			t.Errorf("%s: +groupName=%s but naming says %q; the CRD and the controller would disagree",
				s.dir, got, s.api.Group)
		}
	}
}

func TestGroupVersionMatchesNaming(t *testing.T) {
	for _, s := range served {
		if s.gv.String() != s.api.GroupVersion {
			t.Errorf("GroupVersion = %q, want %q", s.gv.String(), s.api.GroupVersion)
		}
	}
}

// The generated CRDs must agree too, since they are what the API server
// enforces.
func TestGeneratedCRDMatchesNaming(t *testing.T) {
	for _, s := range served {
		path := "../../config/crd/" + s.api.Group + "_" + naming.Plural + ".yaml"
		src, err := os.ReadFile(path)
		if err != nil {
			t.Skipf("generated CRD not present at %s; run make generate", path)
		}
		text := string(src)

		for _, want := range []string{
			"name: " + s.api.CRDName,
			"group: " + s.api.Group,
			"kind: " + naming.Kind,
			"plural: " + naming.Plural,
			"name: " + s.api.Version,
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s is missing %q", path, want)
			}
		}

		if !strings.Contains(text, "scope: Namespaced") {
			t.Errorf("%s: the CRD must be namespaced: a cluster-scoped kind would put a platform team back "+
				"in the loop, which is the property the project exists to remove", path)
		}
	}
}

// The two groups serve one schema. If they ever differed, a Weave moved from
// weft.run to v1 could be accepted by one and rejected by the other. Only the
// kind's own description may differ, so the comparison starts below it.
func TestServedSchemasAreIdentical(t *testing.T) {
	read := func(api naming.API) string {
		src, err := os.ReadFile("../../config/crd/" + api.Group + "_" + naming.Plural + ".yaml")
		if err != nil {
			t.Skipf("generated CRDs not present; run make generate")
		}
		text := strings.ReplaceAll(string(src), "\r\n", "\n")
		i := strings.Index(text, "        properties:\n          apiVersion:")
		j := strings.Index(text, "    served: true")
		if i < 0 || j < i {
			t.Fatalf("cannot find the schema in the %s CRD", api.Group)
		}
		return text[i:j]
	}
	if read(naming.V1) != read(naming.Legacy) {
		t.Error("weft.run/v1alpha1 and weft.alethic.solutions/v1 have different schemas")
	}
}

func TestHelpers(t *testing.T) {
	status := v1.WeaveStatus{
		Inventory: []v1.InventoryEntry{
			{APIVersion: "v1", Kind: "ConfigMap", Name: "a"},
			{APIVersion: "v1", Kind: "ConfigMap", Name: "b"},
		},
		Held: []v1.HeldResource{
			{APIVersion: "v1", Kind: "ConfigMap", Name: "upstream"},
		},
	}
	if e, ok := status.InventoryFor("v1", "ConfigMap", "b"); !ok || e.Name != "b" {
		t.Error("InventoryFor should find an entry")
	}
	if _, ok := status.InventoryFor("v1", "ConfigMap", "c"); ok {
		t.Error("InventoryFor should not invent one")
	}
	if _, ok := status.InventoryFor("v1", "Secret", "b"); ok {
		t.Error("InventoryFor must match on kind, not only name")
	}
	if _, ok := status.HeldByRef("v1", "ConfigMap", "upstream"); !ok {
		t.Error("HeldByRef should find a recorded hold")
	}
	if _, ok := status.HeldByRef("v1", "Secret", "upstream"); ok {
		t.Error("HeldByRef must match on kind, not only name")
	}
}
