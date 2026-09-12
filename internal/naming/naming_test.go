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
	for _, c := range cases {
		f := HeldFinalizer(c[0], c[1])
		if errs := validation.IsQualifiedName(f); len(errs) > 0 {
			t.Errorf("HeldFinalizer(%q, %q) = %q is not a qualified name: %v", c[0], c[1], f, errs)
		}
		if !IsHeldFinalizer(f) {
			t.Errorf("%q is not recognised as one of ours", f)
		}
	}
}

func TestSourceFinalizerIsStableAndDistinct(t *testing.T) {
	a := HeldFinalizer("ns", "app")
	if a != HeldFinalizer("ns", "app") {
		t.Error("the same Weave must always produce the same finalizer")
	}
	if a == HeldFinalizer("ns", "other") {
		t.Error("different Weaves must produce different finalizers")
	}
	// Namespace and name must not be able to trade characters and collide.
	if HeldFinalizer("a", "bc") == HeldFinalizer("ab", "c") {
		t.Error("namespace and name are not separated in the hash input")
	}
}

func TestForeignFinalizersAreNotClaimed(t *testing.T) {
	for _, f := range []string{
		"kubernetes.io/pv-protection",
		"other.example/weft",
		HeldFinalizerPrefix, // the bare prefix is not a finalizer we placed
		WeaveFinalizer,
	} {
		if IsHeldFinalizer(f) {
			t.Errorf("%q should not be recognised as a source finalizer", f)
		}
	}
}

func TestWeaveFinalizerIsValid(t *testing.T) {
	if errs := validation.IsQualifiedName(WeaveFinalizer); len(errs) > 0 {
		t.Errorf("%q is not a qualified name: %v", WeaveFinalizer, errs)
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

// Everything externally visible derives from Group, so a rename is one edit.
func TestIdentifiersDeriveFromGroup(t *testing.T) {
	for name, value := range map[string]string{
		"GroupVersion":        GroupVersion,
		"CRDName":             CRDName,
		"FieldManager":        FieldManager,
		"WeaveFinalizer":      WeaveFinalizer,
		"HeldFinalizerPrefix": HeldFinalizerPrefix,
		"WeaveLabel":          WeaveLabel,
		"KeyAnnotation":       KeyAnnotation,
		"NeedsAnnotation":     NeedsAnnotation,
	} {
		if !strings.Contains(value, Group) {
			t.Errorf("%s = %q does not derive from Group %q", name, value, Group)
		}
	}
}

func TestLabelAndAnnotationKeysAreValid(t *testing.T) {
	for _, k := range []string{WeaveLabel, KeyAnnotation, NeedsAnnotation, WeaveUIDAnnotation, OwnedAnnotation} {
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			t.Errorf("%q is not a qualified name: %v", k, errs)
		}
	}
}
