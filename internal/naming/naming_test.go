package naming

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

// A finalizer is a qualified name whose path segment is limited to 63
// characters and a restricted alphabet. A Weave's namespace and name together
// can exceed that easily, so they are hashed rather than interpolated, and this
// guards the property for names that would otherwise overflow.
func TestSourceFinalizerIsAValidQualifiedName(t *testing.T) {
	cases := [][2]string{
		{"ns", "app"},
		{strings.Repeat("n", 63), strings.Repeat("w", 253)},
		{"a", "b"},
	}
	for _, api := range APIs {
		for _, c := range cases {
			f := api.HeldFinalizer(c[0], c[1])
			if errs := validation.IsQualifiedName(f); len(errs) > 0 {
				t.Errorf("HeldFinalizer(%q, %q) = %q is not a qualified name: %v", c[0], c[1], f, errs)
			}
			if !api.IsHeldFinalizer(f) {
				t.Errorf("%q is not recognised as one of ours", f)
			}
		}
	}
}

func TestSourceFinalizerIsStableAndDistinct(t *testing.T) {
	for _, api := range APIs {
		a := api.HeldFinalizer("ns", "app")
		if a != api.HeldFinalizer("ns", "app") {
			t.Error("the same Weave must always produce the same finalizer")
		}
		if a == api.HeldFinalizer("ns", "other") {
			t.Error("different Weaves must produce different finalizers")
		}
		// Namespace and name must not be able to trade characters and collide.
		if api.HeldFinalizer("a", "bc") == api.HeldFinalizer("ab", "c") {
			t.Error("namespace and name are not separated in the hash input")
		}
	}
}

func TestForeignFinalizersAreNotClaimed(t *testing.T) {
	for _, api := range APIs {
		for _, f := range []string{
			"kubernetes.io/pv-protection",
			"other.example/weft",
			api.HeldFinalizerPrefix, // the bare prefix is not a finalizer we placed
			api.WeaveFinalizer,
		} {
			if api.IsHeldFinalizer(f) {
				t.Errorf("%q should not be recognised as a source finalizer", f)
			}
		}
	}
	// Each group recognises only its own: a v1 Weave releasing holds must not
	// strip one a weft.run Weave of the same name still needs.
	if V1.IsHeldFinalizer(Legacy.HeldFinalizer("ns", "app")) {
		t.Error("v1 claims a weft.run finalizer")
	}
	if Legacy.IsHeldFinalizer(V1.HeldFinalizer("ns", "app")) {
		t.Error("weft.run claims a v1 finalizer")
	}
}

func TestWeaveFinalizerIsValid(t *testing.T) {
	for _, api := range APIs {
		if errs := validation.IsQualifiedName(api.WeaveFinalizer); len(errs) > 0 {
			t.Errorf("%q is not a qualified name: %v", api.WeaveFinalizer, errs)
		}
	}
}

func TestUserFor(t *testing.T) {
	got := UserFor("sweep-labs", "sweep-env-compose")
	want := "system:serviceaccount:sweep-labs:sweep-env-compose"
	if got != want {
		// The API server special-cases exactly this form to authorise
		// impersonation against the serviceaccounts resource in that namespace.
		t.Errorf("UserFor = %q, want %q", got, want)
	}
}

// Everything externally visible derives from the group, so no Weave writes
// another group's keys.
func TestIdentifiersDeriveFromGroup(t *testing.T) {
	for _, api := range APIs {
		for name, value := range map[string]string{
			"GroupVersion":        api.GroupVersion,
			"CRDName":             api.CRDName,
			"FieldManager":        api.FieldManager,
			"WeaveFinalizer":      api.WeaveFinalizer,
			"HeldFinalizerPrefix": api.HeldFinalizerPrefix,
			"WeaveLabel":          api.WeaveLabel,
			"WeaveUIDAnnotation":  api.WeaveUIDAnnotation,
			"OwnedAnnotation":     api.OwnedAnnotation,
			"AdoptAnnotation":     api.AdoptAnnotation,
		} {
			if !strings.HasPrefix(value, api.Group) && !strings.HasSuffix(value, api.Group) {
				t.Errorf("%s %s = %q does not derive from the group", api.Group, name, value)
			}
		}
	}
}

// The weft.run identifiers are on objects in clusters that predate v1. Changing
// any of them strands those objects: finalizers nobody releases, provenance
// nothing recognises, fields owned by a manager that no longer writes.
func TestLegacyIdentifiersAreFrozen(t *testing.T) {
	for got, want := range map[string]string{
		Legacy.GroupVersion:        "weft.run/v1alpha1",
		Legacy.CRDName:             "weaves.weft.run",
		Legacy.FieldManager:        "weft.run",
		Legacy.WeaveFinalizer:      "weft.run/weave",
		Legacy.HeldFinalizerPrefix: "weft.run/held-",
		Legacy.WeaveLabel:          "weft.run/weave",
		Legacy.WeaveUIDAnnotation:  "weft.run/weave-uid",
		Legacy.OwnedAnnotation:     "weft.run/owned",
		Legacy.AdoptAnnotation:     "weft.run/adopt",
	} {
		if got != want {
			t.Errorf("legacy identifier changed: %q, want %q", got, want)
		}
	}
	if Legacy.HeldFinalizer("ns", "app") != "weft.run/held-"+V1.HeldFinalizer("ns", "app")[len(V1.HeldFinalizerPrefix):] {
		t.Error("the held finalizer hash must not depend on the group")
	}
}

func TestV1SupersedesLegacy(t *testing.T) {
	if V1.Predecessor == nil || V1.Predecessor.Group != Legacy.Group {
		t.Error("v1 must name weft.run as its predecessor, or nothing can be handed over")
	}
	if Legacy.Predecessor != nil {
		t.Error("weft.run supersedes nothing")
	}
	if a, ok := IsWeaveOwnerRef("weft.run/v1alpha1", "Weave"); !ok || a.Group != Legacy.Group {
		t.Error("a weft.run owner reference is not recognised")
	}
	if _, ok := IsWeaveOwnerRef("weft.run/v1alpha1", "ConfigMap"); ok {
		t.Error("an owner reference of another kind is not a Weave")
	}
}

func TestLabelAndAnnotationKeysAreValid(t *testing.T) {
	for _, api := range APIs {
		for _, k := range []string{api.WeaveLabel, api.WeaveUIDAnnotation, api.OwnedAnnotation, api.AdoptAnnotation} {
			if errs := validation.IsQualifiedName(k); len(errs) > 0 {
				t.Errorf("%q is not a qualified name: %v", k, errs)
			}
		}
	}
}
