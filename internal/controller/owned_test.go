package controller

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/naming"
)

const unownedProgram = `
def compose(variable, observed):
    out = {
        "ordinary": {
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": "ordinary"},
        },
    }
    if not variable.get("dropped", False):
        out["keeper"] = {
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {
                "name": "keeper",
                "annotations": {"weft.run/owned": "false"},
            },
            "data": {"kept": "yes"},
        }
    return out
`

// An unowned resource is applied like any other, but with no owner reference on
// it. That absence is the whole guarantee: nothing collects it, whatever
// happens to the Weave or to this controller.
func TestUnownedResourceGetsNoOwnerReference(t *testing.T) {
	h := newHarness(t, nil)
	h.create("keeping", unownedProgram, "")
	h.settle("keeping", 2)

	requireCondition(t, h.weave("keeping"), naming.ConditionReady, metav1.ConditionTrue)

	keeper, err := h.getConfigMap("keeper")
	if err != nil {
		t.Fatal(err)
	}
	if len(keeper.GetOwnerReferences()) != 0 {
		t.Errorf("owner references = %+v, want none", keeper.GetOwnerReferences())
	}
	if keeper.Data["kept"] != "yes" {
		t.Errorf("it should still be applied and kept current: %v", keeper.Data)
	}

	// Still findable as something this Weave made.
	if keeper.Labels[naming.WeaveLabel] != "keeping" {
		t.Errorf("labels = %v, want the Weave label kept", keeper.Labels)
	}

	// The ordinary one is owned, which is what makes the contrast meaningful.
	ordinary, err := h.getConfigMap("ordinary")
	if err != nil {
		t.Fatal(err)
	}
	if len(ordinary.GetOwnerReferences()) != 1 {
		t.Errorf("owner references = %+v, want one", ordinary.GetOwnerReferences())
	}

	// And the inventory records which is which, because at prune time the
	// returned object is gone and this is all that is left to read.
	w := h.weave("keeping")
	for _, e := range w.Status.Inventory {
		switch e.Key {
		case "keeper":
			if e.Owned {
				t.Error("the keeper entry should be recorded unowned")
			}
		case "ordinary":
			if !e.Owned {
				t.Error("the ordinary entry should be recorded owned")
			}
		}
	}
}

// When the program stops returning it, an unowned resource is let go rather
// than deleted, with no hysteresis: nothing is being destroyed, so there is
// nothing to wait out.
func TestUnownedResourceSurvivesLeavingTheWeave(t *testing.T) {
	h := newHarness(t, nil)
	h.create("dropping", unownedProgram, `{"dropped": false}`)
	h.settle("dropping", 2)

	if !h.exists("keeper") || !h.exists("ordinary") {
		t.Fatal("both should exist to start with")
	}

	w := h.weave("dropping")
	w.Spec.Variables = []v1alpha1.Variable{
		{Values: &apiextensionsv1.JSON{Raw: []byte(`{"dropped": true}`)}},
	}
	if err := testK8s.Update(h.ctx, w); err != nil {
		t.Fatal(err)
	}
	h.settle("dropping", 3)

	if !h.exists("keeper") {
		t.Error("an unowned resource must survive leaving the Weave")
	}

	// The record is dropped immediately, though: Weft is no longer tracking it.
	if _, ok := h.weave("dropping").Status.InventoryByKey("keeper"); ok {
		t.Error("the inventory entry should be released at once, without hysteresis")
	}
}

// Deleting the Weave is the strongest form of "no longer described", and the
// annotation has to hold there or it is a guarantee that fails exactly when
// somebody is relying on it.
func TestUnownedResourceSurvivesTheWeave(t *testing.T) {
	h := newHarness(t, nil)
	h.create("temporary", unownedProgram, "")
	h.settle("temporary", 2)

	if !h.exists("keeper") || !h.exists("ordinary") {
		t.Fatal("both should exist to start with")
	}

	if err := testK8s.Delete(h.ctx, h.weave("temporary")); err != nil {
		t.Fatal(err)
	}
	h.settle("temporary", 3)

	if !h.exists("keeper") {
		t.Error("an unowned resource must survive its Weave being deleted")
	}
	if h.exists("ordinary") {
		t.Error("an owned resource should have been torn down with the Weave")
	}

	// Leaving something behind silently is the wrong half of this. An object
	// that outlives its Weave is exactly the one somebody will later wonder
	// about, so letting go is recorded.
	if !h.recordedEvent("Released", "keeper") {
		t.Error("releasing a resource should be reported as an event")
	}
}

// A typo in the annotation is refused rather than ignored. "no" quietly meaning
// "owned" would only ever be discovered by the object being deleted.
func TestOwnedAnnotationRejectsAnythingElse(t *testing.T) {
	h := newHarness(t, nil)
	h.create("typo", `
def compose(variable, observed):
    return {
        "x": {
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": "x", "annotations": {"weft.run/owned": "no"}},
        },
    }
`, "")
	h.settle("typo", 2)

	c := requireCondition(t, h.weave("typo"), naming.ConditionDegraded, metav1.ConditionTrue)
	if !containsAll(c.Message, "weft.run/owned", `"true"`, `"false"`) {
		t.Errorf("message should say what the accepted values are: %s", c.Message)
	}
	if h.exists("x") {
		t.Error("nothing should have been applied")
	}
}
