package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	weftv1 "github.com/alethic/weft/api/v1"
	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/naming"
)

// reconcileLegacy runs one weft.run pass.
func (h *harness) reconcileLegacy(name string) {
	h.t.Helper()
	if _, err := h.legacy.Reconcile(h.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: h.namespace, Name: name},
	}); err != nil {
		h.t.Fatalf("reconcile weft.run: %v", err)
	}
}

func (h *harness) createLegacy(name, program string) {
	h.t.Helper()
	w := &v1alpha1.Weave{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.namespace},
		Spec:       weftv1.WeaveSpec{ServiceAccountName: "composer", Program: program},
	}
	if err := testK8s.Create(h.ctx, w); err != nil {
		h.t.Fatalf("creating weft.run weave: %v", err)
	}
	h.reconcileLegacy(name)
}

func (h *harness) legacyWeave(name string) *v1alpha1.Weave {
	h.t.Helper()
	var w v1alpha1.Weave
	if err := testK8s.Get(h.ctx, types.NamespacedName{Namespace: h.namespace, Name: name}, &w); err != nil {
		h.t.Fatalf("getting weft.run weave: %v", err)
	}
	return &w
}

// orphanLegacy does what kubectl delete --cascade=orphan and the garbage
// collector do together. The test control plane runs no garbage collector, so
// its half - stripping the owner references and then the orphan finalizer - is
// done here, after the controller has done its own.
func (h *harness) orphanLegacy(name string, outputs ...string) {
	h.t.Helper()
	w := h.legacyWeave(name)
	orphan := metav1.DeletePropagationOrphan
	if err := testK8s.Delete(h.ctx, w, &client.DeleteOptions{PropagationPolicy: &orphan}); err != nil {
		h.t.Fatalf("deleting weft.run weave: %v", err)
	}
	h.reconcileLegacy(name)

	for _, n := range outputs {
		cm, err := h.getConfigMap(n)
		if err != nil {
			continue
		}
		var kept []metav1.OwnerReference
		for _, ref := range cm.OwnerReferences {
			if ref.UID != w.UID {
				kept = append(kept, ref)
			}
		}
		cm.OwnerReferences = kept
		if err := testK8s.Update(h.ctx, cm); err != nil {
			h.t.Fatalf("stripping owner reference: %v", err)
		}
	}

	w = h.legacyWeave(name)
	if controllerutil.ContainsFinalizer(w, naming.Legacy.WeaveFinalizer) {
		h.t.Fatalf("weft.run controller kept its finalizer through an orphaning delete: %v", w.Finalizers)
	}
	controllerutil.RemoveFinalizer(w, metav1.FinalizerOrphanDependents)
	if err := testK8s.Update(h.ctx, w); err != nil {
		h.t.Fatalf("releasing the orphan finalizer: %v", err)
	}
}

func managers(cm *corev1.ConfigMap) map[string]bool {
	out := map[string]bool{}
	for _, e := range cm.ManagedFields {
		if e.Operation == metav1.ManagedFieldsOperationApply {
			out[e.Manager] = true
		}
	}
	return out
}

const legacyProgram = `
def compose(variable):
    resource({
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": "settings"},
        "data": {"tenantId": "abc"},
    })
    resource({
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": "keeper", "annotations": {"weft.run/owned": "false"}},
        "data": {"keep": "me"},
    })
`

// The same program, as it reads once moved: only the owned annotation's key
// changes.
const movedProgram = `
def compose(variable):
    resource({
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": "settings"},
        "data": {"tenantId": "abc"},
    })
    resource({
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": "keeper", "annotations": {"weft.alethic.solutions/owned": "false"}},
        "data": {"keep": "me"},
    })
`

// A weft.run Weave behaves exactly as it did before v1 existed: every key it
// writes is a weft.run key, because those are what the objects in clusters
// that predate v1 already carry.
func TestLegacyWeaveWritesLegacyKeys(t *testing.T) {
	h := newHarness(t, nil)
	h.createLegacy("app", legacyProgram)
	h.reconcileLegacy("app")

	w := h.legacyWeave("app")
	if !controllerutil.ContainsFinalizer(w, "weft.run/weave") {
		t.Errorf("finalizers = %v, want weft.run/weave", w.Finalizers)
	}

	cm, err := h.getConfigMap("settings")
	if err != nil {
		t.Fatalf("output not created: %v", err)
	}
	if cm.Labels["weft.run/weave"] != "app" || cm.Annotations["weft.run/weave-uid"] != string(w.UID) {
		t.Errorf("provenance = %v / %v, want weft.run keys", cm.Labels, cm.Annotations)
	}
	for k := range cm.Labels {
		if strings.HasPrefix(k, naming.V1.Group) {
			t.Errorf("a weft.run Weave wrote the v1 label %s", k)
		}
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].APIVersion != "weft.run/v1alpha1" {
		t.Errorf("owner references = %+v, want one to weft.run/v1alpha1", cm.OwnerReferences)
	}
	if m := managers(cm); !m["weft.run"] || m[naming.V1.FieldManager] {
		t.Errorf("apply managers = %v, want weft.run only", m)
	}

	keeper, err := h.getConfigMap("keeper")
	if err != nil {
		t.Fatalf("unowned output not created: %v", err)
	}
	if len(keeper.OwnerReferences) != 0 {
		t.Error("weft.run/owned=false must still be honoured on a weft.run Weave")
	}
}

// Two Weaves of the same name in different groups are different objects. The
// v1 one must not mistake the weft.run one's objects for its own, and must say
// how to finish the move rather than just refusing.
func TestV1RefusesWhileLegacyWeaveLives(t *testing.T) {
	h := newHarness(t, nil)
	h.createLegacy("app", legacyProgram)
	h.reconcileLegacy("app")

	h.create("app", movedProgram, "")
	h.settle("app", 2)

	c := requireCondition(t, h.weave("app"), naming.ConditionDegraded, metav1.ConditionTrue)
	if c.Reason != ReasonNotOurs {
		t.Errorf("reason = %s, want %s", c.Reason, ReasonNotOurs)
	}
	for _, want := range []string{"weft.run/v1alpha1", "--cascade=orphan", "weaves.weft.run"} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("message does not mention %q:\n%s", want, c.Message)
		}
	}

	// And nothing changed hands.
	cm, _ := h.getConfigMap("settings")
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].APIVersion != "weft.run/v1alpha1" {
		t.Errorf("owner references = %+v; the v1 Weave took something it was refused", cm.OwnerReferences)
	}
	if m := managers(cm); m[naming.V1.FieldManager] {
		t.Error("the v1 Weave applied to an object it was refused")
	}
}

// The move itself: the weft.run Weave is deleted with --cascade=orphan, and
// the v1 Weave of the same name takes over what it made, in place.
func TestHandoverFromLegacy(t *testing.T) {
	h := newHarness(t, nil)
	h.createLegacy("app", legacyProgram)
	h.reconcileLegacy("app")

	before := map[string]types.UID{}
	for _, n := range []string{"settings", "keeper"} {
		cm, err := h.getConfigMap(n)
		if err != nil {
			t.Fatalf("%s not created: %v", n, err)
		}
		before[n] = cm.UID
	}

	h.orphanLegacy("app", "settings", "keeper")

	h.create("app", movedProgram, "")
	h.settle("app", 3)

	w := h.weave("app")
	requireCondition(t, w, naming.ConditionReady, metav1.ConditionTrue)
	if !h.recordedEvent("HandedOver", "weft.run/v1alpha1") {
		t.Error("no HandedOver event")
	}

	for _, n := range []string{"settings", "keeper"} {
		cm, err := h.getConfigMap(n)
		if err != nil {
			t.Fatalf("%s is gone: %v", n, err)
		}
		if cm.UID != before[n] {
			t.Errorf("%s was recreated; a handover must take it over in place", n)
		}

		// The old provenance goes, pruned by the apply now that the v1 manager
		// owns the fields it was written into.
		for k := range cm.Labels {
			if strings.HasPrefix(k, "weft.run/") {
				t.Errorf("%s still carries %s", n, k)
			}
		}
		for k := range cm.Annotations {
			if strings.HasPrefix(k, "weft.run/") {
				t.Errorf("%s still carries %s", n, k)
			}
		}
		if cm.Labels[naming.V1.WeaveLabel] != "app" || cm.Annotations[naming.V1.WeaveUIDAnnotation] != string(w.UID) {
			t.Errorf("%s provenance = %v / %v, want the v1 Weave's", n, cm.Labels, cm.Annotations)
		}
		if m := managers(cm); m["weft.run"] || !m[naming.V1.FieldManager] {
			t.Errorf("%s apply managers = %v, want the v1 manager only", n, m)
		}
	}

	settings, _ := h.getConfigMap("settings")
	if len(settings.OwnerReferences) != 1 || settings.OwnerReferences[0].UID != w.UID {
		t.Errorf("settings owner references = %+v, want the v1 Weave", settings.OwnerReferences)
	}
	keeper, _ := h.getConfigMap("keeper")
	if len(keeper.OwnerReferences) != 0 {
		t.Error("keeper is unowned in both programs and must stay unowned")
	}

	// Steady from here: the handover is a one-time step, not something every
	// pass repeats.
	h.recordedEvent("", "") // drain
	h.settle("app", 2)
	if h.recordedEvent("HandedOver", "") {
		t.Error("handover repeated on a steady-state pass")
	}
}

// Between the orphaning delete and the garbage collector finishing, the old
// Weave still exists. That is not a refusal, just not yet.
func TestHandoverWaitsForPredecessorDeletion(t *testing.T) {
	h := newHarness(t, nil)
	h.createLegacy("app", legacyProgram)
	h.reconcileLegacy("app")

	// Something other than the controller holds the old Weave, so it stays in
	// deletion while the v1 Weave reconciles.
	w := h.legacyWeave("app")
	controllerutil.AddFinalizer(w, "test.example/hold")
	if err := testK8s.Update(h.ctx, w); err != nil {
		t.Fatal(err)
	}
	orphan := metav1.DeletePropagationOrphan
	if err := testK8s.Delete(h.ctx, w, &client.DeleteOptions{PropagationPolicy: &orphan}); err != nil {
		t.Fatal(err)
	}
	h.reconcileLegacy("app")

	// keeper never had an owner reference, so it needs nothing from garbage
	// collection and is what a v1 Weave reaches first.

	h.create("app", `
def compose(variable):
    resource({
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": "keeper", "annotations": {"weft.alethic.solutions/owned": "false"}},
        "data": {"keep": "me"},
    })
`, "")
	h.settle("app", 2)

	c := requireCondition(t, h.weave("app"), naming.ConditionWaiting, metav1.ConditionTrue)
	if c.Reason != ReasonHandover {
		t.Errorf("reason = %s (%s), want %s", c.Reason, c.Message, ReasonHandover)
	}

	w = h.legacyWeave("app")
	w.Finalizers = nil
	if err := testK8s.Update(h.ctx, w); err != nil {
		t.Fatal(err)
	}
	h.settle("app", 2)
	requireCondition(t, h.weave("app"), naming.ConditionReady, metav1.ConditionTrue)
}

// A program moved to v1 that still says weft.run/owned: "false" would have
// everything it meant to keep owned by the new Weave, and deleted with it.
func TestV1RejectsLegacyOwnedAnnotation(t *testing.T) {
	h := newHarness(t, nil)
	h.create("app", legacyProgram, "")
	h.settle("app", 2)

	c := requireCondition(t, h.weave("app"), naming.ConditionDegraded, metav1.ConditionTrue)
	if !strings.Contains(c.Message, "weft.run/owned") || !strings.Contains(c.Message, "weft.alethic.solutions/owned") {
		t.Errorf("message should name both annotations:\n%s", c.Message)
	}
	if h.exists("keeper") {
		t.Error("keeper was created owned despite the program asking for it not to be")
	}
}

// A weft.run Weave and a v1 Weave are independent even with one name: each is
// reconciled by its own controller, and neither one's teardown touches the
// other's objects.
func TestSameNameAcrossGroupsIsIndependent(t *testing.T) {
	h := newHarness(t, nil)
	h.createLegacy("app", `
def compose(variable):
    resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "old"}, "data": {}})
`)
	h.reconcileLegacy("app")
	h.create("app", `
def compose(variable):
    resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "new"}, "data": {}})
`, "")
	h.settle("app", 2)
	requireCondition(t, h.weave("app"), naming.ConditionReady, metav1.ConditionTrue)

	if err := testK8s.Delete(h.ctx, h.weave("app")); err != nil {
		t.Fatal(err)
	}
	h.settle("app", 3)
	if h.exists("new") {
		t.Error("the v1 Weave's output survived its deletion")
	}
	if !h.exists("old") {
		t.Error("deleting the v1 Weave tore down the weft.run Weave's output")
	}
	if !controllerutil.ContainsFinalizer(h.legacyWeave("app"), naming.Legacy.WeaveFinalizer) {
		t.Error("the weft.run Weave lost its finalizer")
	}
}

// What Helm, Flux and anything else that applies manifests do when an
// apiVersion changes: create the new object, then delete the old one with
// background propagation. None of them can pass --cascade=orphan, so the old
// Weave must recognise its successor and let go instead of tearing down.
func TestLegacyDeletedAfterSuccessorExistsHandsOver(t *testing.T) {
	h := newHarness(t, nil)
	h.createLegacy("app", legacyProgram)
	h.reconcileLegacy("app")

	before := map[string]types.UID{}
	for _, n := range []string{"settings", "keeper"} {
		cm, err := h.getConfigMap(n)
		if err != nil {
			t.Fatalf("%s not created: %v", n, err)
		}
		before[n] = cm.UID
	}

	// The new release's object first...
	h.create("app", movedProgram, "")
	h.settle("app", 1)

	// ...then the old one's removal, as a deploying tool issues it.
	background := metav1.DeletePropagationBackground
	if err := testK8s.Delete(h.ctx, h.legacyWeave("app"), &client.DeleteOptions{PropagationPolicy: &background}); err != nil {
		t.Fatal(err)
	}
	h.reconcileLegacy("app")

	if !h.recordedEvent("HandingOver", "weft.alethic.solutions/v1") {
		t.Error("no HandingOver event")
	}
	// The owner references are what the garbage collector acts on, and the
	// test control plane runs none, so their absence is the assertion.
	for _, n := range []string{"settings", "keeper"} {
		cm, err := h.getConfigMap(n)
		if err != nil {
			t.Fatalf("%s was torn down: %v", n, err)
		}
		for _, ref := range cm.OwnerReferences {
			if ref.APIVersion == naming.Legacy.GroupVersion {
				t.Errorf("%s still carries the weft.run owner reference; garbage collection would delete it", n)
			}
		}
	}

	h.settle("app", 3)
	requireCondition(t, h.weave("app"), naming.ConditionReady, metav1.ConditionTrue)
	for n, uid := range before {
		cm, err := h.getConfigMap(n)
		if err != nil {
			t.Fatalf("%s is gone: %v", n, err)
		}
		if cm.UID != uid {
			t.Errorf("%s was recreated", n)
		}
	}
	settings, _ := h.getConfigMap("settings")
	if len(settings.OwnerReferences) != 1 || settings.OwnerReferences[0].APIVersion != naming.V1.GroupVersion {
		t.Errorf("settings owner references = %+v, want the v1 Weave only", settings.OwnerReferences)
	}
}

// Without a successor, deleting a weft.run Weave still means what it always
// meant.
func TestLegacyDeletedWithoutSuccessorTearsDown(t *testing.T) {
	h := newHarness(t, nil)
	h.createLegacy("app", legacyProgram)
	h.reconcileLegacy("app")

	if err := testK8s.Delete(h.ctx, h.legacyWeave("app")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		h.reconcileLegacy("app")
	}
	if h.exists("settings") {
		t.Error("an owned resource survived its weft.run Weave's deletion")
	}
	if !h.exists("keeper") {
		t.Error("an unowned resource was deleted with its weft.run Weave")
	}
}
