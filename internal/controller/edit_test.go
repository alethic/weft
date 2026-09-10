package controller

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/alethic/weft/internal/naming"
)

// Editing a program has to converge: what it stopped describing goes, what it
// started describing arrives, and what it changed under a stable key does not
// end up orphaned.
func TestEditingReplacesRatherThanOrphans(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.PruneDelay = time.Millisecond
		o.PruneThreshold = 1
	})

	h.create("editable", `
def compose(inputs, sources, observed):
    return {
        "thing": {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "thing-one"}},
    }
`, nil, "")
	h.settle("editable", 2)

	if !h.exists("thing-one") {
		t.Fatal("thing-one should exist")
	}

	// The same key, addressing a different object. The inventory records the
	// new one under that key, so nothing would remember the old.
	w := h.weave("editable")
	w.Spec.Program = `
def compose(inputs, sources, observed):
    return {
        "thing": {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "thing-two"}},
    }
`
	if err := testK8s.Update(h.ctx, w); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	h.settle("editable", 4)

	if !h.exists("thing-two") {
		t.Error("the replacement should exist")
	}
	if h.exists("thing-one") {
		t.Error("the replaced object was orphaned: still in the cluster, absent from every record")
	}
	if s := h.weave("editable").Status.Superseded; len(s) != 0 {
		t.Errorf("nothing should still be awaiting removal: %+v", s)
	}
	inv := h.weave("editable").Status.Inventory
	if len(inv) != 1 || inv[0].Name != "thing-two" {
		t.Errorf("inventory = %+v", inv)
	}
}

// Changing the kind under a stable key is the same problem wearing a different
// hat.
func TestEditingTheKindReplacesTheObject(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.PruneDelay = time.Millisecond
		o.PruneThreshold = 1
	})

	h.create("kindchange", `
def compose(inputs, sources, observed):
    return {
        "payload": {
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": "payload"},
            "data": {"k": "v"},
        },
    }
`, nil, "")
	h.settle("kindchange", 2)

	if !h.exists("payload") {
		t.Fatal("the ConfigMap should exist")
	}

	w := h.weave("kindchange")
	w.Spec.Program = `
def compose(inputs, sources, observed):
    return {
        "payload": {
            "apiVersion": "v1", "kind": "Secret",
            "metadata": {"name": "payload"},
            "stringData": {"k": "v"},
        },
    }
`
	if err := testK8s.Update(h.ctx, w); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	h.settle("kindchange", 4)

	if h.exists("payload") {
		t.Error("the old ConfigMap should have been removed, not left beside the Secret")
	}
	var secret corev1.Secret
	key := k8stypes.NamespacedName{Namespace: h.namespace, Name: "payload"}
	if err := testK8s.Get(h.ctx, key, &secret); err != nil {
		t.Fatalf("the Secret should exist: %v", err)
	}
	inv := h.weave("kindchange").Status.Inventory
	if len(inv) != 1 || inv[0].Kind != "Secret" {
		t.Errorf("inventory = %+v", inv)
	}
}

// The full shape change: some stay, some go, some arrive, in one edit.
func TestEditingConvergesOnTheNewShape(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.PruneDelay = time.Millisecond
		o.PruneThreshold = 1
	})

	h.create("shape", `
def compose(inputs, sources, observed):
    out = {}
    for n in ["keep", "drop-a", "drop-b"]:
        out[n] = {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": n}}
    return out
`, nil, "")
	h.settle("shape", 2)

	for _, n := range []string{"keep", "drop-a", "drop-b"} {
		if !h.exists(n) {
			t.Fatalf("%s should exist", n)
		}
	}

	w := h.weave("shape")
	w.Spec.Program = `
def compose(inputs, sources, observed):
    out = {}
    for n in ["keep", "added-a", "added-b"]:
        out[n] = {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": n}}
    return out
`
	if err := testK8s.Update(h.ctx, w); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	h.settle("shape", 5)

	for _, n := range []string{"keep", "added-a", "added-b"} {
		if !h.exists(n) {
			t.Errorf("%s should exist after the edit", n)
		}
	}
	for _, n := range []string{"drop-a", "drop-b"} {
		if h.exists(n) {
			t.Errorf("%s should have been removed", n)
		}
	}
	if inv := h.weave("shape").Status.Inventory; len(inv) != 3 {
		t.Errorf("inventory should hold exactly the new shape, got %+v", inv)
	}
	requireCondition(t, h.weave("shape"), naming.ConditionReady, metav1.ConditionTrue)
}

// Replaced objects come down in reverse wave order, like everything else.
// Pinning the highest shows the lower ones are not touched past it.
func TestReplacedObjectsAreRemovedInOrder(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.PruneDelay = time.Millisecond
		o.PruneThreshold = 1
	})

	h.create("ordered", `
def compose(inputs, sources, observed):
    out = {}
    for n in ["base", "middle", "top"]:
        out[n] = {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": n + "-v1"}}
    return out
`, nil, "")
	h.settle("ordered", 2)

	topOld, err := h.getConfigMap("top-v1")
	if err != nil {
		t.Fatal(err)
	}
	topOld.Finalizers = []string{"example.com/hold"}
	if err := testK8s.Update(h.ctx, topOld); err != nil {
		t.Fatal(err)
	}

	w := h.weave("ordered")
	w.Spec.Program = `
def compose(inputs, sources, observed):
    out = {}
    for n in ["base", "middle", "top"]:
        out[n] = {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": n + "-v2"}}
    return out
`
	if err := testK8s.Update(h.ctx, w); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	h.settle("ordered", 4)

	for _, n := range []string{"base-v2", "middle-v2", "top-v2"} {
		if !h.exists(n) {
			t.Errorf("%s should exist", n)
		}
	}

	base, err := h.getConfigMap("base-v1")
	if err != nil {
		t.Fatal("base-v1 must not be removed while a higher wave is still standing")
	}
	if base.DeletionTimestamp != nil {
		t.Error("base-v1 is being deleted before the wave above it finished")
	}

	topOld, err = h.getConfigMap("top-v1")
	if err != nil {
		t.Fatal(err)
	}
	topOld.Finalizers = nil
	if err := testK8s.Update(h.ctx, topOld); err != nil {
		t.Fatal(err)
	}
	h.settle("ordered", 5)

	for _, n := range []string{"base-v1", "middle-v1", "top-v1"} {
		if h.exists(n) {
			t.Errorf("%s should be gone", n)
		}
	}
	if s := h.weave("ordered").Status.Superseded; len(s) != 0 {
		t.Errorf("nothing should still be awaiting removal: %+v", s)
	}
}

// Choosing a name must not be enough to take somebody else's resource. Applying
// would add Weft's owner reference, and deleting the Weave would then delete
// something it never made.
func TestRefusesToTakeOverAnExistingObject(t *testing.T) {
	h := newHarness(t, nil)
	h.configMap("preexisting", map[string]string{"owner": "somebody-else"})

	h.create("greedy", `
def compose(inputs, sources, observed):
    return {
        "grab": {
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": "preexisting"},
            "data": {"owner": "weft"},
        },
    }
`, nil, "")
	h.settle("greedy", 2)

	c := requireCondition(t, h.weave("greedy"), naming.ConditionDegraded, metav1.ConditionTrue)
	if c.Reason != ReasonNotOurs {
		t.Errorf("reason = %q, want %q", c.Reason, ReasonNotOurs)
	}
	if !strings.Contains(c.Message, naming.AdoptAnnotation) {
		t.Error("the refusal should say how to hand the object over deliberately")
	}

	cm, err := h.getConfigMap("preexisting")
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data["owner"] != "somebody-else" {
		t.Error("the existing object was overwritten")
	}
	if len(cm.GetOwnerReferences()) != 0 {
		t.Error("the existing object was taken over")
	}
	if len(h.weave("greedy").Status.Inventory) != 0 {
		t.Error("nothing should have been recorded")
	}
}

// Onboarding. Consent lives on the object, so whoever holds it decides - not
// whoever wrote the program.
func TestAdoptsWhenTheObjectConsents(t *testing.T) {
	h := newHarness(t, nil)
	cm := h.configMap("onboarded", map[string]string{"owner": "somebody-else"})
	cm.Annotations = map[string]string{naming.AdoptAnnotation: "adopter"}
	if err := testK8s.Update(h.ctx, cm); err != nil {
		t.Fatal(err)
	}

	h.create("adopter", `
def compose(inputs, sources, observed):
    return {
        "onboarded": {
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": "onboarded"},
            "data": {"owner": "weft"},
        },
    }
`, nil, "")
	h.settle("adopter", 2)

	requireCondition(t, h.weave("adopter"), naming.ConditionReady, metav1.ConditionTrue)

	adopted, err := h.getConfigMap("onboarded")
	if err != nil {
		t.Fatal(err)
	}
	if adopted.Data["owner"] != "weft" {
		t.Errorf("the adopted object should now be managed: %v", adopted.Data)
	}
	refs := adopted.GetOwnerReferences()
	if len(refs) != 1 || refs[0].Name != "adopter" {
		t.Errorf("the adopted object should be owned by the Weave: %+v", refs)
	}
	if _, ok := h.weave("adopter").Status.InventoryByKey("onboarded"); !ok {
		t.Error("the adopted object should be in the inventory")
	}
}

// An annotation naming a different Weave is not consent for this one.
func TestAdoptionAnnotationIsSpecific(t *testing.T) {
	h := newHarness(t, nil)
	cm := h.configMap("spoken-for", nil)
	cm.Annotations = map[string]string{naming.AdoptAnnotation: "some-other-weave"}
	if err := testK8s.Update(h.ctx, cm); err != nil {
		t.Fatal(err)
	}

	h.create("hopeful", `
def compose(inputs, sources, observed):
    return {
        "x": {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "spoken-for"}},
    }
`, nil, "")
	h.settle("hopeful", 2)

	c := requireCondition(t, h.weave("hopeful"), naming.ConditionDegraded, metav1.ConditionTrue)
	if c.Reason != ReasonNotOurs {
		t.Errorf("reason = %q", c.Reason)
	}

	cm, err := h.getConfigMap("spoken-for")
	if err != nil {
		t.Fatal(err)
	}
	if len(cm.GetOwnerReferences()) != 0 {
		t.Error("it was taken anyway")
	}
}

// Two keys naming one object is a contradiction: whichever applied last would
// win, and the other would be recorded pointing at something it does not
// control.
func TestRefusesTwoKeysForOneObject(t *testing.T) {
	h := newHarness(t, nil)

	h.create("colliding", `
def compose(inputs, sources, observed):
    return {
        "first": {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "shared"}},
        "second": {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "shared"}},
    }
`, nil, "")
	h.settle("colliding", 3)

	c := requireCondition(t, h.weave("colliding"), naming.ConditionDegraded, metav1.ConditionTrue)
	if c.Reason != ReasonNotOurs {
		t.Errorf("reason = %q, want %q (%s)", c.Reason, ReasonNotOurs, c.Message)
	}
}
