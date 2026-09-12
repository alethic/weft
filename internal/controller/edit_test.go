package controller

import (
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/naming"
)

// The full shape change: some stay, some go, some arrive, in one edit.
func TestEditingConvergesOnTheNewShape(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.PruneDelay = time.Millisecond
		o.PruneThreshold = 1
	})

	h.create("shape", `
def compose(variable):
    for n in ["keep", "drop-a", "drop-b"]:
        resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": n}})
    return
`, "")
	h.settle("shape", 2)

	for _, n := range []string{"keep", "drop-a", "drop-b"} {
		if !h.exists(n) {
			t.Fatalf("%s should exist", n)
		}
	}

	w := h.weave("shape")
	w.Spec.Program = `
def compose(variable):
    for n in ["keep", "added-a", "added-b"]:
        resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": n}})
    return
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

// Choosing a name must not be enough to take somebody else's resource. Applying
// would add Weft's owner reference, and deleting the Weave would then delete
// something it never made.
func TestRefusesToTakeOverAnExistingObject(t *testing.T) {
	h := newHarness(t, nil)
	h.configMap("preexisting", map[string]string{"owner": "somebody-else"})

	h.create("greedy", `
def compose(variable):
    resource({
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": "preexisting"},
        "data": {"owner": "weft"},
        })
`, "")
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
def compose(variable):
    resource({
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": "onboarded"},
        "data": {"owner": "weft"},
        })
`, "")
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
	if _, ok := h.weave("adopter").Status.InventoryFor("v1", "ConfigMap", "onboarded"); !ok {
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
def compose(variable):
    resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "spoken-for"}})
`, "")
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

// kubectl delete --cascade=orphan means "delete the owner, keep the children".
// The API server marks that with the orphan finalizer, and tearing the
// resources down anyway would silently ignore an explicit instruction - worse
// than not supporting it, because the person believes they protected them.
func TestOrphanPropagationKeepsTheResources(t *testing.T) {
	h := newHarness(t, nil)

	h.create("orphaning", `
def compose(variable):
    resource({
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": "keeper"}, "data": {"k": "v"},
        })
`, "")
	h.settle("orphaning", 2)

	if !h.exists("keeper") {
		t.Fatal("keeper should exist")
	}

	orphan := metav1.DeletePropagationOrphan
	if err := testK8s.Delete(h.ctx, h.weave("orphaning"), &client.DeleteOptions{
		PropagationPolicy: &orphan,
	}); err != nil {
		t.Fatal(err)
	}

	h.settle("orphaning", 3)

	cm, err := h.getConfigMap("keeper")
	if err != nil {
		t.Fatalf("the resource should have been left in place: %v", err)
	}
	if cm.DeletionTimestamp != nil {
		t.Error("the resource is being deleted despite orphan propagation")
	}

	// What Weft controls is that its own finalizer is gone. The remaining
	// "orphan" finalizer belongs to the API server and is removed by garbage
	// collection once it has stripped the owner references, on a schedule of
	// its own - asserting on that would be testing Kubernetes rather than this
	// controller, and it is slow enough on a small cluster to be flaky.
	var released v1alpha1.Weave
	err = testK8s.Get(h.ctx, k8stypes.NamespacedName{Namespace: h.namespace, Name: "orphaning"}, &released)
	switch {
	case apierrors.IsNotFound(err):
		// Collection already finished.
	case err != nil:
		t.Fatal(err)
	default:
		for _, f := range released.Finalizers {
			if f == naming.WeaveFinalizer {
				t.Errorf("Weft is still holding the Weave: %v", released.Finalizers)
			}
		}
	}
}

// And the default is still to take them: without cascade there would be no way
// to uninstall what a Weave produced except by hand.
func TestDefaultDeletionStillRemovesTheResources(t *testing.T) {
	h := newHarness(t, nil)

	h.create("cascading", `
def compose(variable):
    resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "goes"}})
`, "")
	h.settle("cascading", 2)

	if err := testK8s.Delete(h.ctx, h.weave("cascading")); err != nil {
		t.Fatal(err)
	}
	h.settle("cascading", 3)

	if h.exists("goes") {
		t.Error("an ordinary delete should take the resources with it")
	}
}
