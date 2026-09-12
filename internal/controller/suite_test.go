package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/eval"
	"github.com/alethic/weft/internal/kube"
	"github.com/alethic/weft/internal/naming"
	"github.com/alethic/weft/internal/watches"
)

// The control plane is supplied per platform.
//
// envtest cannot be compiled on Windows against controller-runtime v0.25.0:
// process.go declares signalProcess unconditionally and signal_windows.go
// redeclares it. So the bootstrap is split by build tag - a hermetic control
// plane where that works, and the ambient kubecontext where it does not - and
// the tests themselves are identical either way.
var (
	testCfg *rest.Config
	testK8s client.Client
)

// harness is one reconciler wired to a fresh namespace.
type harness struct {
	t          *testing.T
	ctx        context.Context
	namespace  string
	reconciler *WeaveReconciler
	registry   *watches.Registry
	events     *record.FakeRecorder
	cancel     context.CancelFunc
}

func newHarness(t *testing.T, tune func(*Options)) *harness {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ns := fmt.Sprintf("test-%d", time.Now().UnixNano()%1_000_000_000)
	if err := testK8s.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	// The composer ServiceAccount, and a grant wide enough for the tests to
	// write ConfigMaps as it.
	//
	// The grant is not incidental scaffolding: without it every Weave here is
	// Degraded with a permission error, which is the system working. That is
	// the whole design, and it is why these tests have to set up an identity
	// that can actually do the work rather than relying on the controller's own
	// privileges.
	if err := testK8s.Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "composer", Namespace: ns},
	}); err != nil {
		t.Fatalf("creating service account: %v", err)
	}
	// Granted nothing, so a test can show what a Weave with no permissions does.
	if err := testK8s.Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "powerless", Namespace: ns},
	}); err != nil {
		t.Fatalf("creating service account: %v", err)
	}
	if err := testK8s.Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "composer", Namespace: ns},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"configmaps", "secrets"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
		}},
	}); err != nil {
		t.Fatalf("creating role: %v", err)
	}
	if err := testK8s.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "composer", Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "composer"},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount", Name: "composer", Namespace: ns,
		}},
	}); err != nil {
		t.Fatalf("creating role binding: %v", err)
	}

	// A Weave left holding its finalizer would block the namespace forever, so
	// the cleanup strips them before deleting rather than waiting on a
	// reconciler that has already stopped.
	t.Cleanup(func() {
		cleanup := context.Background()
		var weaves v1alpha1.WeaveList
		if err := testK8s.List(cleanup, &weaves, client.InNamespace(ns)); err == nil {
			for i := range weaves.Items {
				w := &weaves.Items[i]
				if len(w.Finalizers) == 0 {
					continue
				}
				w.Finalizers = nil
				_ = testK8s.Update(cleanup, w)
			}
		}
		_ = testK8s.Delete(cleanup, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	httpClient, err := rest.HTTPClientFor(testCfg)
	if err != nil {
		t.Fatal(err)
	}
	mapper, err := apiutil.NewDynamicRESTMapper(testCfg, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := kube.NewFactory(testCfg, mapper, kube.FactoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	registry := watches.NewRegistry(watches.Options{Log: logf.Log})
	go func() { _ = registry.Start(ctx) }()

	opts := Options{
		// Short enough that a test can wait it out, long enough that a resource
		// is not pruned by accident mid-test.
		PruneDelay:     2 * time.Second,
		PruneThreshold: 1,
	}
	if tune != nil {
		tune(&opts)
	}
	// Buffered well past what any one test produces, so a full channel never
	// silently drops the event a test is about to assert on.
	events := record.NewFakeRecorder(256)

	r := &WeaveReconciler{
		Client:    testK8s,
		Scheme:    testK8s.Scheme(),
		Factory:   factory,
		Evaluator: eval.NewStarlark(eval.Options{}),
		Watches:   registry,
		Recorder:  events,
		Opts:      opts,
	}
	r.Opts.applyDefaults()

	return &harness{
		t: t, ctx: ctx, namespace: ns,
		reconciler: r, registry: registry, events: events, cancel: cancel,
	}
}

// recordedEvent drains what has been emitted so far and reports whether any of
// it carries both the reason and the substring.
func (h *harness) recordedEvent(reason, substr string) bool {
	h.t.Helper()
	found := false
	for {
		select {
		case e := <-h.events.Events:
			if strings.Contains(e, reason) && strings.Contains(e, substr) {
				found = true
			}
		default:
			return found
		}
	}
}

// reconcile runs one pass and fails the test on an unexpected error.
func (h *harness) reconcile(name string) ctrl.Result {
	h.t.Helper()
	res, err := h.reconciler.Reconcile(h.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: h.namespace, Name: name},
	})
	if err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	return res
}

// settle reconciles until the Weave stops changing, which is what a real
// controller does when watches fire. It also proves the loop terminates.
func (h *harness) settle(name string, passes int) {
	h.t.Helper()
	for i := 0; i < passes; i++ {
		h.reconcile(name)
	}
}

func (h *harness) weave(name string) *v1alpha1.Weave {
	h.t.Helper()
	var w v1alpha1.Weave
	if err := testK8s.Get(h.ctx, types.NamespacedName{Namespace: h.namespace, Name: name}, &w); err != nil {
		h.t.Fatalf("getting weave: %v", err)
	}
	return &w
}

func (h *harness) create(name, program, values string) *v1alpha1.Weave {
	h.t.Helper()
	w := &v1alpha1.Weave{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.namespace},
		Spec: v1alpha1.WeaveSpec{
			ServiceAccountName: "composer",
			Program:            program,
		},
	}
	if values != "" {
		w.Spec.Variables = []v1alpha1.Variable{
			{Values: &apiextensionsv1.JSON{Raw: []byte(values)}},
		}
	}
	if err := testK8s.Create(h.ctx, w); err != nil {
		h.t.Fatalf("creating weave: %v", err)
	}

	// The first reconcile of a new Weave only installs the finalizer and
	// returns; in a running controller that update immediately triggers the
	// next pass. Doing it here keeps every reconcile a test makes a real one.
	h.reconcile(name)
	return w
}

func (h *harness) configMap(name string, data map[string]string) *corev1.ConfigMap {
	h.t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.namespace},
		Data:       data,
	}
	if err := testK8s.Create(h.ctx, cm); err != nil {
		h.t.Fatalf("creating configmap: %v", err)
	}
	return cm
}

func (h *harness) getConfigMap(name string) (*corev1.ConfigMap, error) {
	var cm corev1.ConfigMap
	err := testK8s.Get(h.ctx, types.NamespacedName{Namespace: h.namespace, Name: name}, &cm)
	return &cm, err
}

func (h *harness) exists(name string) bool {
	_, err := h.getConfigMap(name)
	return err == nil
}

func condition(w *v1alpha1.Weave, condType string) *metav1.Condition {
	for i := range w.Status.Conditions {
		if w.Status.Conditions[i].Type == condType {
			return &w.Status.Conditions[i]
		}
	}
	return nil
}

func requireCondition(t *testing.T, w *v1alpha1.Weave, condType string, status metav1.ConditionStatus) *metav1.Condition {
	t.Helper()
	c := condition(w, condType)
	if c == nil {
		t.Fatalf("no %s condition; conditions are %+v", condType, w.Status.Conditions)
	}
	if c.Status != status {
		t.Fatalf("%s = %s (%s: %s), want %s", condType, c.Status, c.Reason, c.Message, status)
	}
	return c
}

// The ordinary path: a source resolves, resources are applied, the Weave is
// Ready and owns what it made.
func TestReconcileAppliesAndOwns(t *testing.T) {
	h := newHarness(t, nil)
	h.configMap("tenant", map[string]string{"tenantId": "abc-123"})

	h.create("app", `
def compose(variable):
    resource({
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": "settings"},
        "data": {"tenantId": require(read("v1", "ConfigMap", "tenant"), "data.tenantId")},
        })
`, "")

	h.settle("app", 2)

	w := h.weave("app")
	requireCondition(t, w, naming.ConditionReady, metav1.ConditionTrue)

	cm, err := h.getConfigMap("settings")
	if err != nil {
		t.Fatalf("the output was not created: %v", err)
	}
	if cm.Data["tenantId"] != "abc-123" {
		t.Errorf("tenantId = %q", cm.Data["tenantId"])
	}

	refs := cm.GetOwnerReferences()
	if len(refs) != 1 || refs[0].Kind != naming.Kind || refs[0].UID != w.UID {
		t.Errorf("output is not owned by the Weave: %+v", refs)
	}
	if cm.Labels[naming.WeaveLabel] != "app" {
		t.Errorf("labels = %v", cm.Labels)
	}
	if len(w.Status.Inventory) != 1 || w.Status.Inventory[0].Name != "settings" {
		t.Errorf("inventory = %+v", w.Status.Inventory)
	}
}

// The regression that mattered most: an idle Weave rewrote its status forever,
// because a status write wakes the Weave through its own watch. Two reconciles
// over an unchanged world must leave the object byte-identical.
func TestSteadyStateDoesNotChurn(t *testing.T) {
	h := newHarness(t, nil)
	h.configMap("tenant", map[string]string{"tenantId": "abc"})

	h.create("app", `
def compose(variable):
    resource({
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": "settings"},
        "data": {"tenantId": require(read("v1", "ConfigMap", "tenant"), "data.tenantId")},
        })
`, "")

	h.settle("app", 3)
	before := h.weave("app").ResourceVersion

	// Several more passes over an unchanged world.
	h.settle("app", 5)
	after := h.weave("app").ResourceVersion

	if before != after {
		t.Errorf("the Weave was rewritten while nothing changed (%s -> %s); "+
			"a status write wakes it through its own watch, so this spins forever", before, after)
	}
}

// Staging: the second phase is emitted only once the first phase's
// server-populated field appears, and the Weave reports what it waits on
// without withholding the resources it can already create.
func TestStagingThroughObserved(t *testing.T) {
	h := newHarness(t, nil)
	h.configMap("tenant", map[string]string{"tenantId": "abc"})

	h.create("app", `
def compose(variable):
    identity = resource({
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": "identity"},
        "data": {"tenantId": require(read("v1", "ConfigMap", "tenant"), "data.tenantId")},
    })
    uid = get(identity.observed, "metadata.uid")
    if not uid:
        pending("the identity has no uid yet")
        return
    resource({
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": "consumer"},
        "data": {"principalId": uid},
    })
    return
`, "")

	// First pass: only the identity, and the Weave says why it is not finished.
	h.reconcile("app")
	w := h.weave("app")
	if !h.exists("identity") {
		t.Fatal("the identity should have been applied on the first pass")
	}
	if h.exists("consumer") {
		t.Fatal("the consumer must not exist before the identity has a uid")
	}
	c := requireCondition(t, w, naming.ConditionWaiting, metav1.ConditionTrue)
	if c.Reason != ReasonFieldUnresolved {
		t.Errorf("waiting reason = %q", c.Reason)
	}
	requireCondition(t, w, naming.ConditionReady, metav1.ConditionFalse)

	// Second pass: the uid is now observable, so the rest is emitted.
	h.reconcile("app")
	if !h.exists("consumer") {
		t.Fatal("the consumer should have been applied once the uid resolved")
	}

	identity, _ := h.getConfigMap("identity")
	consumer, _ := h.getConfigMap("consumer")
	if consumer.Data["principalId"] != string(identity.UID) {
		t.Errorf("principalId = %q, want the identity's uid %q", consumer.Data["principalId"], identity.UID)
	}

	h.reconcile("app")
	requireCondition(t, h.weave("app"), naming.ConditionReady, metav1.ConditionTrue)
}

// A source that does not exist gates everything, even though the program never
// reads a field from it - a pure ordering edge, written where the composition
// can see it rather than declared as a flag.
func TestSourceGateWithoutBeingRead(t *testing.T) {
	h := newHarness(t, nil)

	h.create("gated", `
def compose(variable):
    if not read("v1", "ConfigMap", "gate"):
        return wait("the gate ConfigMap has not been created yet")
    resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "out"}})
`, "")

	h.reconcile("gated")
	w := h.weave("gated")

	c := requireCondition(t, w, naming.ConditionWaiting, metav1.ConditionTrue)
	if c.Reason != ReasonFieldUnresolved {
		t.Errorf("reason = %q, want %q", c.Reason, ReasonFieldUnresolved)
	}
	if !strings.Contains(c.Message, "gate ConfigMap") {
		t.Errorf("the reason the program gave should reach the condition: %q", c.Message)
	}
	if h.exists("out") {
		t.Fatal("nothing should be created while a required source is missing")
	}

	// The gate appears and the composition proceeds.
	h.configMap("gate", nil)
	h.reconcile("gated")
	if !h.exists("out") {
		t.Fatal("the output should appear once the gate exists")
	}
	requireCondition(t, h.weave("gated"), naming.ConditionReady, metav1.ConditionTrue)
}

// Pruning waits on a clock, not on a count of reconciles.
//
// Reconciles are event-driven and the applies in a single pass generate watch
// events of their own, so a count of them measures controller activity rather
// than how long something has actually been gone. The delay is moved rather
// than waited out, so the test asserts the rule instead of racing it.
func TestPruneWaitsOutTheDelay(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.PruneDelay = time.Hour
		o.PruneThreshold = 1
	})
	h.configMap("tenant", map[string]string{"keep": "yes", "extra": "yes"})

	program := `
def compose(variable):
    resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "keep"}})
    if has(read("v1", "ConfigMap", "tenant"), "data.extra"):
        resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "extra"}})
    return
`
	h.create("pruner", program, "")
	h.settle("pruner", 2)

	if !h.exists("extra") {
		t.Fatal("extra should exist initially")
	}

	// Drop the field, so the program stops returning "extra".
	cm, _ := h.getConfigMap("tenant")
	delete(cm.Data, "extra")
	if err := testK8s.Update(h.ctx, cm); err != nil {
		t.Fatal(err)
	}

	// Hammer the reconciler. A count-based threshold would have deleted it
	// several times over by now.
	for i := 0; i < 20; i++ {
		h.reconcile("pruner")
	}
	if !h.exists("extra") {
		t.Fatal("extra was pruned by repeated reconciles alone; the hysteresis is counting passes, not elapsed time")
	}

	w := h.weave("pruner")
	entry, ok := w.Status.InventoryFor("v1", "ConfigMap", "extra")
	if !ok {
		t.Fatal("extra should still be in the inventory while it waits out the delay")
	}
	if entry.MissingSince == nil {
		t.Fatal("MissingSince should have been stamped on the first absence")
	}
	if entry.MissingCount < 1 {
		t.Errorf("MissingCount = %d, want at least 1", entry.MissingCount)
	}

	// Now let the clock have passed, and it goes.
	h.reconciler.Opts.PruneDelay = 0
	h.reconcile("pruner")

	if h.exists("extra") {
		t.Fatal("extra should have been pruned once its delay had elapsed")
	}

	// The delete has been issued but not yet confirmed, so the entry is still
	// recorded and the Weave says it is tearing down. Dropping it before the
	// object is actually gone would orphan anything that failed to delete.
	if _, ok := h.weave("pruner").Status.InventoryFor("v1", "ConfigMap", "extra"); !ok {
		t.Error("the entry should be held until the deletion is confirmed")
	}
	requireCondition(t, h.weave("pruner"), naming.ConditionWaiting, metav1.ConditionTrue)

	// The next pass confirms it and drops the record.
	h.reconcile("pruner")
	if _, ok := h.weave("pruner").Status.InventoryFor("v1", "ConfigMap", "extra"); ok {
		t.Error("the pruned entry should be out of the inventory once its deletion is confirmed")
	}
	requireCondition(t, h.weave("pruner"), naming.ConditionReady, metav1.ConditionTrue)
	if !h.exists("keep") {
		t.Error("pruning removed the wrong resource")
	}
}

// A resource that reappears before its delay elapses is not deleted, and does
// not carry its accumulated absence forward.
func TestReappearanceCancelsThePrune(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.PruneDelay = 30 * time.Second })
	h.configMap("tenant", map[string]string{"extra": "yes"})

	program := `
def compose(variable):
    resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "keep"}})
    if has(read("v1", "ConfigMap", "tenant"), "data.extra"):
        resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "extra"}})
    return
`
	h.create("flapper", program, "")
	h.settle("flapper", 2)

	cm, _ := h.getConfigMap("tenant")
	delete(cm.Data, "extra")
	if err := testK8s.Update(h.ctx, cm); err != nil {
		t.Fatal(err)
	}
	h.reconcile("flapper")

	if e, ok := h.weave("flapper").Status.InventoryFor("v1", "ConfigMap", "extra"); !ok || e.MissingSince == nil {
		t.Fatal("the clock should be running")
	}

	// It comes back.
	cm, _ = h.getConfigMap("tenant")
	cm.Data = map[string]string{"extra": "yes"}
	if err := testK8s.Update(h.ctx, cm); err != nil {
		t.Fatal(err)
	}
	h.reconcile("flapper")

	e, ok := h.weave("flapper").Status.InventoryFor("v1", "ConfigMap", "extra")
	if !ok {
		t.Fatal("extra should be back in the inventory")
	}
	if e.MissingSince != nil || e.MissingCount != 0 {
		t.Errorf("hysteresis survived a reappearance: since=%v count=%d", e.MissingSince, e.MissingCount)
	}
	if !h.exists("extra") {
		t.Error("extra should exist again")
	}
}

// Deleting a Weave tears its resources down in reverse dependency order rather
// than handing them all to cascading collection at once.
func TestOrderedTeardown(t *testing.T) {
	h := newHarness(t, nil)

	h.create("stack", `
def compose(variable):
    for i, name in enumerate(["base", "middle", "top"]):
        resource({
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": name},
        })
    return
`, "")
	h.settle("stack", 2)

	w := h.weave("stack")
	if len(w.Status.Inventory) != 3 {
		t.Fatalf("inventory = %+v", w.Status.Inventory)
	}
	// Return order became apply order.
	for i, want := range []string{"base", "middle", "top"} {
		if w.Status.Inventory[i].Name != want || w.Status.Inventory[i].Wave != int32(i) {
			t.Fatalf("inventory[%d] = %s wave %d, want %s wave %d",
				i, w.Status.Inventory[i].Name, w.Status.Inventory[i].Wave, want, i)
		}
	}

	// Hold the middle wave so teardown cannot pass it.
	middle, _ := h.getConfigMap("middle")
	middle.Finalizers = []string{"example.com/hold"}
	if err := testK8s.Update(h.ctx, middle); err != nil {
		t.Fatal(err)
	}

	if err := testK8s.Delete(h.ctx, h.weave("stack")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		h.reconcile("stack")
	}

	if h.exists("top") {
		t.Error("the highest wave should have been deleted first")
	}
	base, err := h.getConfigMap("base")
	if err != nil {
		t.Fatal("the lowest wave must not be touched while a higher one is still standing")
	}
	if base.DeletionTimestamp != nil {
		t.Error("the lowest wave is being deleted before the wave above it finished")
	}

	// The Weave is still held by its own finalizer, and says so.
	w = h.weave("stack")
	if len(w.Finalizers) == 0 {
		t.Fatal("the Weave should still hold its finalizer while teardown is unfinished")
	}
	requireCondition(t, w, naming.ConditionWaiting, metav1.ConditionTrue)

	// Release the hold and let it finish.
	middle, _ = h.getConfigMap("middle")
	middle.Finalizers = nil
	if err := testK8s.Update(h.ctx, middle); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		h.reconcile("stack")
	}

	if h.exists("base") || h.exists("middle") {
		t.Error("everything should be gone once the hold is released")
	}
	var gone v1alpha1.Weave
	err = testK8s.Get(h.ctx, types.NamespacedName{Namespace: h.namespace, Name: "stack"}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the Weave should have been released, got %v", err)
	}
}

// A program fault is permanent: it will not resolve on its own, so it is
// Degraded rather than Waiting, and it carries a backtrace.
func TestProgramFaultIsDegraded(t *testing.T) {
	h := newHarness(t, nil)

	h.create("broken", `
def compose(variable):
    fail("the role table is empty")
`, "")

	h.reconcile("broken")
	w := h.weave("broken")

	c := requireCondition(t, w, naming.ConditionDegraded, metav1.ConditionTrue)
	if c.Reason != eval.ReasonRuntimeError {
		t.Errorf("reason = %q, want %q", c.Reason, eval.ReasonRuntimeError)
	}
	requireCondition(t, w, naming.ConditionWaiting, metav1.ConditionFalse)
	requireCondition(t, w, naming.ConditionReady, metav1.ConditionFalse)
}

// An output that reaches outside its namespace is refused rather than
// relocated: same-namespace is what makes plain ownership sufficient.
func TestCrossNamespaceOutputIsRefused(t *testing.T) {
	h := newHarness(t, nil)

	h.create("escapee", `
def compose(variable):
    resource({
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": "out", "namespace": "somewhere-else"},
        })
`, "")

	h.reconcile("escapee")
	requireCondition(t, h.weave("escapee"), naming.ConditionDegraded, metav1.ConditionTrue)

	// Rejected before any request is made, so there is nothing to look for
	// anywhere; the inventory staying empty is the observable proof.
	if len(h.weave("escapee").Status.Inventory) != 0 {
		t.Error("a cross-namespace output reached the inventory")
	}
}

// A resource deleted out from under the Weave is recreated, because the program
// still returns it.
func TestDeletedOutputIsRecreated(t *testing.T) {
	h := newHarness(t, nil)

	h.create("healer", `
def compose(variable):
    resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "out"}})
`, "")
	h.settle("healer", 2)

	cm, err := h.getConfigMap("out")
	if err != nil {
		t.Fatal(err)
	}
	originalUID := cm.UID
	if err := testK8s.Delete(h.ctx, cm); err != nil {
		t.Fatal(err)
	}

	h.reconcile("healer")
	recreated, err := h.getConfigMap("out")
	if err != nil {
		t.Fatalf("the output should have been recreated: %v", err)
	}
	if recreated.UID == originalUID {
		t.Error("expected a genuinely new object")
	}
	if _, ok := h.weave("healer").Status.InventoryFor("v1", "ConfigMap", "out"); !ok {
		t.Error("the inventory should still record it")
	}
}

// Editing a program applies the new result and prunes what it no longer
// returns, which is the thing the CronJob arrangement could never do.
func TestEditingTheProgramConverges(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.PruneDelay = time.Millisecond
		o.PruneThreshold = 1
	})

	h.create("editable", `
def compose(variable):
    resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "a"}})
    resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "b"}})
`, "")
	h.settle("editable", 2)

	if !h.exists("a") || !h.exists("b") {
		t.Fatal("both outputs should exist")
	}

	w := h.weave("editable")
	w.Spec.Program = `
def compose(variable):
    resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "a"}})
`
	if err := testK8s.Update(h.ctx, w); err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Millisecond)
	h.settle("editable", 3)

	if !h.exists("a") {
		t.Error("a should still exist")
	}
	if h.exists("b") {
		t.Error("b should have been pruned once the program stopped returning it")
	}
}

// Renaming a key is not a rename. It deletes one resource and creates another,
// and the inventory has to reflect that rather than silently re-labelling a
// live object.
func TestRenamingPrunesTheOldAndCreatesTheNew(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.PruneDelay = time.Millisecond
		o.PruneThreshold = 1
	})

	h.create("renamer", `
def compose(variable):
    resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "old-name"}})
`, "")
	h.settle("renamer", 2)

	w := h.weave("renamer")
	w.Spec.Program = `
def compose(variable):
    resource({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "new-name"}})
`
	if err := testK8s.Update(h.ctx, w); err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Millisecond)
	h.settle("renamer", 3)

	if !h.exists("new-name") {
		t.Error("the new resource should exist")
	}
	if h.exists("old-name") {
		t.Error("the old resource should have been pruned")
	}
	inv := h.weave("renamer").Status.Inventory
	if len(inv) != 1 || inv[0].Name != "new-name" {
		t.Errorf("inventory = %+v", inv)
	}
}

// The finalizer is added before anything is created, so a Weave can always be
// torn down in order.
func TestFinalizerIsAddedFirst(t *testing.T) {
	h := newHarness(t, nil)
	h.create("finalized", `
def compose(variable):
    return
`, "")

	h.reconcile("finalized")
	w := h.weave("finalized")

	found := false
	for _, f := range w.Finalizers {
		if f == naming.WeaveFinalizer {
			found = true
		}
	}
	if !found {
		t.Errorf("finalizers = %v, want %q", w.Finalizers, naming.WeaveFinalizer)
	}
}

// variables reach the program with their types intact. An integer that arrives as
// a float comes back out as one, which is a different resource body on every
// apply.
func TestVariablesKeepTheirTypes(t *testing.T) {
	h := newHarness(t, nil)

	h.create("typed", `
def compose(variable):
    resource({
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": "out"},
        "data": {"replicas": str(variable.replicas), "doubled": str(variable.replicas * 2)},
        })
`, `{"replicas": 3}`)

	h.settle("typed", 2)
	cm, err := h.getConfigMap("out")
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data["replicas"] != "3" || cm.Data["doubled"] != "6" {
		t.Errorf("data = %v; an integer became a float somewhere", cm.Data)
	}
}
