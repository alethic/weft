package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/alethic/weft/internal/naming"
)

// Everything Weft applies carries a record of what applied it. The Weave's own
// status says the same thing, but a status can be lost and an object cannot
// look it up.
func TestProvenanceIsStampedOnEverything(t *testing.T) {
	h := newHarness(t, nil)
	h.create("stamper", `
def compose(variable, observed):
    return {
        "thing": {
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": "thing"},
        },
    }
`, "")
	h.settle("stamper", 2)

	cm, err := h.getConfigMap("thing")
	if err != nil {
		t.Fatal(err)
	}
	w := h.weave("stamper")

	if got := cm.Labels[naming.WeaveLabel]; got != "stamper" {
		t.Errorf("%s = %q", naming.WeaveLabel, got)
	}
	if got := cm.Annotations[naming.KeyAnnotation]; got != "thing" {
		t.Errorf("%s = %q", naming.KeyAnnotation, got)
	}
	if got := cm.Annotations[naming.WeaveUIDAnnotation]; got != string(w.UID) {
		t.Errorf("%s = %q, want the Weave's UID %q", naming.WeaveUIDAnnotation, got, w.UID)
	}
}

// The case this exists for: an unowned resource carries no owner reference, so
// if the inventory loses it there is nothing left to say Weft made it - except
// what is written on the object.
func TestUnownedResourceIsReclaimedAfterStatusLoss(t *testing.T) {
	h := newHarness(t, nil)
	h.create("forgetful", `
def compose(variable, observed):
    return {
        "keeper": {
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": "keeper", "annotations": {"weft.run/owned": "false"}},
            "data": {"v": "1"},
        },
    }
`, "")
	h.settle("forgetful", 2)

	keeper, err := h.getConfigMap("keeper")
	if err != nil {
		t.Fatal(err)
	}
	if len(keeper.GetOwnerReferences()) != 0 {
		t.Fatal("the premise is that nothing on the object points back at the Weave")
	}

	// Lose the inventory, the way a restored backup or a rolled-back status
	// would. Without provenance the next pass refuses: an object that exists
	// and is not ours.
	w := h.weave("forgetful")
	w.Status.Inventory = nil
	if err := testK8s.Status().Update(h.ctx, w); err != nil {
		t.Fatal(err)
	}
	h.settle("forgetful", 2)

	requireCondition(t, h.weave("forgetful"), naming.ConditionReady, metav1.ConditionTrue)
	if _, ok := h.weave("forgetful").Status.InventoryByKey("keeper"); !ok {
		t.Error("the resource should be back in the inventory")
	}
	if !h.recordedEvent("Reclaimed", "keeper") {
		t.Error("taking an object up again is worth an event")
	}
}

// An owned resource whose owner reference was stripped by hand is the same
// problem, and reclaims the same way.
func TestOwnedResourceIsReclaimedAfterItsReferenceIsStripped(t *testing.T) {
	h := newHarness(t, nil)
	h.create("stripped", `
def compose(variable, observed):
    return {"thing": {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "thing"}}}
`, "")
	h.settle("stripped", 2)

	cm, err := h.getConfigMap("thing")
	if err != nil {
		t.Fatal(err)
	}
	cm.OwnerReferences = nil
	if err := testK8s.Update(h.ctx, cm); err != nil {
		t.Fatal(err)
	}

	w := h.weave("stripped")
	w.Status.Inventory = nil
	if err := testK8s.Status().Update(h.ctx, w); err != nil {
		t.Fatal(err)
	}
	h.settle("stripped", 2)

	requireCondition(t, h.weave("stripped"), naming.ConditionReady, metav1.ConditionTrue)

	// And applying it again restores the reference, so it is collectable again.
	cm, err = h.getConfigMap("thing")
	if err != nil {
		t.Fatal(err)
	}
	if len(cm.GetOwnerReferences()) != 1 {
		t.Errorf("re-applying an owned resource should restore its owner reference: %+v",
			cm.GetOwnerReferences())
	}
}

// The UID is what makes reclaiming safe. A Weave recreated under the same name
// is a different object, and its predecessor's resources are not automatically
// its to take back - otherwise the label, which anyone who can write the object
// can set, would be a way to hand Weft something it never made.
func TestProvenanceFromAnotherWeaveIsNotReclaimed(t *testing.T) {
	h := newHarness(t, nil)
	cm := h.configMap("impostor", map[string]string{"owner": "somebody-else"})
	cm.Labels = map[string]string{naming.WeaveLabel: "claimant"}
	cm.Annotations = map[string]string{
		naming.KeyAnnotation:      "thing",
		naming.WeaveUIDAnnotation: "11111111-1111-1111-1111-111111111111",
	}
	if err := testK8s.Update(h.ctx, cm); err != nil {
		t.Fatal(err)
	}

	h.create("claimant", `
def compose(variable, observed):
    return {"thing": {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "impostor"}}}
`, "")
	h.settle("claimant", 2)

	c := requireCondition(t, h.weave("claimant"), naming.ConditionDegraded, metav1.ConditionTrue)
	if c.Reason != ReasonNotOurs {
		t.Fatalf("reason = %q (%s)", c.Reason, c.Message)
	}
	if !containsAll(c.Message, "claimant") {
		t.Errorf("the refusal should name the provenance it found: %s", c.Message)
	}

	after, err := h.getConfigMap("impostor")
	if err != nil {
		t.Fatal(err)
	}
	if after.Data["owner"] != "somebody-else" {
		t.Errorf("nothing should have been written over: %v", after.Data)
	}
}
