package inventory

import (
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/eval"
	"github.com/alethic/weft/internal/kube"
	"github.com/alethic/weft/internal/naming"
)

func owner() kube.Owner {
	return kube.Owner{
		APIVersion: naming.GroupVersion,
		Kind:       naming.Kind,
		Name:       "app",
		UID:        "weave-uid",
	}
}

func res(name string, extra ...func(map[string]any)) map[string]any {
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name},
	}
	for _, f := range extra {
		f(obj)
	}
	return obj
}

func ref(name string) eval.Ref {
	return eval.Ref{APIVersion: "v1", Kind: "ConfigMap", Name: name}
}

func declared(name string) eval.Resource {
	return eval.Resource{Ref: ref(name), Object: res(name)}
}

func evaluated(names ...string) []eval.Resource {
	out := make([]eval.Resource, 0, len(names))
	for _, n := range names {
		out = append(out, declared(n))
	}
	return out
}

// Return order is the dependency order for a program that creates a thing
// before the things that consume it, which is the natural way to write one.
func TestBuildAssignsWavesByPosition(t *testing.T) {
	items, err := Build(evaluated("identity", "assignment", "binding"), "ns", owner())
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		key  string
		wave int32
	}{{"identity", 0}, {"assignment", 1}, {"binding", 2}} {
		if items[i].Ref.Name != want.key || items[i].Wave != want.wave {
			t.Errorf("item %d = %q wave %d, want %q wave %d", i, items[i].Ref.Name, items[i].Wave, want.key, want.wave)
		}
	}
}

// Depth in the dependency graph is the wave. Siblings that need the same thing
// and not each other come out together without anybody deciding that.
func TestBuildDerivesWavesFromNeeds(t *testing.T) {
	items, err := Build([]eval.Resource{
		declared("identity"),
		dependent("ra-a", "identity"),
		dependent("ra-b", "identity"),
		dependent("ra-c", "identity"),
	}, "ns", owner())
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]int32{"identity": 0, "ra-a": 1, "ra-b": 1, "ra-c": 1}
	for _, it := range items {
		if it.Wave != want[it.Ref.Name] {
			t.Errorf("%s is wave %d, want %d", it.Ref.Name, it.Wave, want[it.Ref.Name])
		}
	}

	groups := Waves(entriesOf(items))
	if len(groups) != 2 {
		t.Fatalf("got %d waves, want 2", len(groups))
	}
	if len(groups[0]) != 3 {
		t.Errorf("the fan-out has %d items in its wave, want 3 torn down together", len(groups[0]))
	}
}

// Declaring a dependency frees a resource from the positional chain, which is
// the whole point: it says this one is less constrained than its position.
func TestNeedsOverridesThePositionalChain(t *testing.T) {
	items, err := Build([]eval.Resource{
		declared("base"),
		declared("long"),
		declared("winded"),
		dependent("quick", "base"),
	}, "ns", owner())
	if err != nil {
		t.Fatal(err)
	}

	byKey := map[string]int32{}
	for _, it := range items {
		byKey[it.Ref.Name] = it.Wave
	}
	if byKey["winded"] != 2 {
		t.Errorf("the chain should still apply to what says nothing: %d", byKey["winded"])
	}
	if byKey["quick"] != 1 {
		t.Errorf("quick needs only base, so it belongs at depth 1, got %d", byKey["quick"])
	}
}

// An explicit empty needs says "nothing at all", which is how siblings built in
// a loop escape being chained to one another by their position.
func TestDeclaringNoNeedsBreaksTheChain(t *testing.T) {
	items, err := Build([]eval.Resource{
		dependent("a"),
		dependent("b"),
		dependent("c"),
	}, "ns", owner())
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Wave != 0 {
			t.Errorf("%s is wave %d, want them all together at 0", it.Ref.Name, it.Wave)
		}
	}
}

// A dependency naming a key the program did not return is a typo, and catching
// it is the thing a wave number could never do.
func TestBuildRejectsAnUnknownDependency(t *testing.T) {
	_, err := Build([]eval.Resource{
		declared("a"),
		dependent("b", "idenity"),
	}, "ns", owner())

	if err == nil || !strings.Contains(err.Error(), "idenity") {
		t.Fatalf("err = %v, should name the key that matches nothing", err)
	}
}

// A cycle is reported rather than broken. Breaking one silently produces an
// apply order that looks fine and is not.
func TestBuildRejectsADependencyCycle(t *testing.T) {
	_, err := Build([]eval.Resource{
		dependent("a", "c"),
		dependent("b", "a"),
		dependent("c", "b"),
	}, "ns", owner())

	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v", err)
	}
}

// dependent builds an evaluated resource that declares what it depends on, the
// way the evaluator hands one over once a program has wrapped it in resource().
func dependent(name string, deps ...string) eval.Resource {
	refs := make([]eval.Ref, 0, len(deps))
	for _, d := range deps {
		refs = append(refs, ref(d))
	}
	return eval.Resource{Ref: ref(name), Object: res(name), Needs: refs, NeedsDeclared: true}
}

func entriesOf(items []Item) []v1alpha1.InventoryEntry {
	out := make([]v1alpha1.InventoryEntry, 0, len(items))
	for _, it := range items {
		out = append(out, Entry(it, nil))
	}
	return out
}

func TestBuildOwnsAndLabels(t *testing.T) {
	items, err := Build(evaluated("cfg"), "sweep-labs", owner())
	if err != nil {
		t.Fatal(err)
	}
	obj := items[0].Object

	if obj.GetNamespace() != "sweep-labs" {
		t.Errorf("namespace = %q", obj.GetNamespace())
	}
	refs := obj.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("got %d owner references, want 1", len(refs))
	}
	if refs[0].UID != "weave-uid" || refs[0].Kind != naming.Kind {
		t.Errorf("owner = %+v", refs[0])
	}
	if refs[0].Controller == nil || !*refs[0].Controller {
		t.Error("owner reference should be a controller reference")
	}
	if refs[0].BlockOwnerDeletion == nil || !*refs[0].BlockOwnerDeletion {
		t.Error("owner reference should block owner deletion")
	}
	if obj.GetLabels()[naming.WeaveLabel] != "app" {
		t.Errorf("labels = %v", obj.GetLabels())
	}
	if obj.GetAnnotations()[naming.WeaveUIDAnnotation] != "weave-uid" {
		t.Errorf("annotations = %v", obj.GetAnnotations())
	}
}

// Same-namespace is the hard rule that makes plain ownership sufficient, so an
// output that reaches outside is rejected rather than relocated.
func TestBuildRejectsForeignNamespace(t *testing.T) {
	_, err := Build([]eval.Resource{
		{Ref: ref("cfg"), Object: res("cfg", func(obj map[string]any) {
			obj["metadata"].(map[string]any)["namespace"] = "someone-else"
		})},
	}, "sweep-labs", owner())

	if err == nil || !strings.Contains(err.Error(), "its own namespace") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildRejectsHandWrittenOwnerReferences(t *testing.T) {
	_, err := Build([]eval.Resource{
		{Ref: ref("cfg"), Object: res("cfg", func(obj map[string]any) {
			obj["metadata"].(map[string]any)["ownerReferences"] = []any{
				map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "name": "exec", "uid": "abc"},
			}
		})},
	}, "ns", owner())

	if err == nil || !strings.Contains(err.Error(), "ownerReferences") {
		t.Fatalf("err = %v", err)
	}
}

// Deriving an output from an observed object is a normal thing to do, and it
// drags along server-populated metadata that must not be applied.
func TestBuildStripsDerivedMetadata(t *testing.T) {
	items, err := Build([]eval.Resource{
		{Ref: ref("cfg"), Object: res("cfg", func(obj map[string]any) {
			md := obj["metadata"].(map[string]any)
			md["uid"] = "someone-elses-uid"
			md["resourceVersion"] = "12345"
			md["creationTimestamp"] = "2024-01-01T00:00:00Z"
			md["managedFields"] = []any{map[string]any{"manager": "kubectl"}}
			obj["status"] = map[string]any{"atProvider": map[string]any{"id": "x"}}
		})},
	}, "ns", owner())
	if err != nil {
		t.Fatal(err)
	}

	obj := items[0].Object
	if obj.GetUID() != "" || obj.GetResourceVersion() != "" {
		t.Errorf("identity fields survived: uid=%q rv=%q", obj.GetUID(), obj.GetResourceVersion())
	}
	if _, found := obj.Object["status"]; found {
		t.Error("status should be dropped; it is a subresource and is never applied")
	}
	md := obj.Object["metadata"].(map[string]any)
	if _, found := md["managedFields"]; found {
		t.Error("managedFields should be dropped")
	}
}

func inventoryEntry(name string, wave int32, missing int32) v1alpha1.InventoryEntry {
	return v1alpha1.InventoryEntry{
		APIVersion: "v1", Kind: "ConfigMap", Name: name,
		Wave: wave, MissingCount: missing, Owned: true,
	}
}

const testDelay = 2 * time.Minute

// A resource absent from one evaluation is not deleted. Managed resources drop
// their status while a provider restarts, and a program waiting on that status
// legitimately stops returning what depends on it.
func TestComputeHysteresis(t *testing.T) {
	t0 := time.Now()
	current := []v1alpha1.InventoryEntry{inventoryEntry("gone", 1, 0), inventoryEntry("kept", 0, 0)}
	desired, err := Build(evaluated("kept"), "ns", owner())
	if err != nil {
		t.Fatal(err)
	}

	d := Compute(current, desired, 3, testDelay, t0)
	if len(d.Prune) != 0 {
		t.Fatalf("nothing should be pruned on the first absence, got %v", d.Prune)
	}
	if len(d.Retained) != 1 || d.Retained[0].MissingCount != 1 {
		t.Fatalf("retained = %+v", d.Retained)
	}
	if d.Retained[0].MissingSince == nil {
		t.Fatal("the first absence must start the clock")
	}

	// The count reaches the threshold almost immediately, because reconciles
	// are event-driven and the applies in one pass generate events of their
	// own. The delay is what actually holds the resource.
	next := d.Retained[0]
	for i := 2; i <= 6; i++ {
		d = Compute([]v1alpha1.InventoryEntry{next, inventoryEntry("kept", 0, 0)}, desired, 3, testDelay,
			t0.Add(time.Duration(i)*time.Second))
		if len(d.Prune) != 0 {
			t.Fatalf("pass %d pruned after %d seconds; the delay is %s", i, i, testDelay)
		}
		next = d.Retained[0]
	}
	if next.MissingCount < 3 {
		t.Fatalf("MissingCount = %d; the count threshold should long since have passed", next.MissingCount)
	}

	// Once the delay has elapsed, it goes.
	d = Compute([]v1alpha1.InventoryEntry{next, inventoryEntry("kept", 0, 0)}, desired, 3, testDelay,
		t0.Add(testDelay+time.Second))
	if len(d.Prune) != 1 || d.Prune[0].Name != "gone" {
		t.Fatalf("should prune once the delay has elapsed, got %v", d.Prune)
	}
	if len(d.Retained) != 0 {
		t.Errorf("retained = %v", d.Retained)
	}
}

// The count alone must not be sufficient, or a busy reconcile loop deletes
// through the delay.
func TestComputeCountAloneDoesNotPrune(t *testing.T) {
	t0 := time.Now()
	since := metav1.NewTime(t0)
	entry := inventoryEntry("gone", 0, 99)
	entry.MissingSince = &since

	desired, err := Build(evaluated("kept"), "ns", owner())
	if err != nil {
		t.Fatal(err)
	}

	d := Compute([]v1alpha1.InventoryEntry{entry}, desired, 3, testDelay, t0.Add(time.Second))
	if len(d.Prune) != 0 {
		t.Errorf("a high count with no elapsed time pruned anyway: %v", d.Prune)
	}
}

// And the delay alone must not be sufficient either: one anomalous evaluation
// long after the fact should not delete anything.
func TestComputeDelayAloneDoesNotPrune(t *testing.T) {
	t0 := time.Now()
	desired, err := Build(evaluated("kept"), "ns", owner())
	if err != nil {
		t.Fatal(err)
	}

	// First absence, observed long after the entry was created. The clock
	// starts now, not retroactively.
	d := Compute([]v1alpha1.InventoryEntry{inventoryEntry("gone", 0, 0)}, desired, 3, testDelay, t0)
	if len(d.Prune) != 0 {
		t.Errorf("first absence pruned immediately: %v", d.Prune)
	}
}

func TestComputeReappearanceResetsTheHysteresis(t *testing.T) {
	since := metav1.NewTime(time.Now().Add(-time.Hour))
	entry := inventoryEntry("flaky", 0, 2)
	entry.MissingSince = &since

	desired, err := Build(evaluated("flaky"), "ns", owner())
	if err != nil {
		t.Fatal(err)
	}

	d := Compute([]v1alpha1.InventoryEntry{entry}, desired, 3, testDelay, time.Now())
	if len(d.Prune) != 0 || len(d.Retained) != 0 {
		t.Fatalf("a returned resource is neither pruned nor retained: %+v", d)
	}
	// The entry recorded after the apply carries no count and no clock, which
	// is what resets the hysteresis. A resource that flaps must not accumulate
	// its way to deletion.
	e := Entry(d.Apply[0], nil)
	if e.MissingCount != 0 || e.MissingSince != nil {
		t.Errorf("hysteresis survived a reappearance: count=%d since=%v", e.MissingCount, e.MissingSince)
	}
}

func TestWavesAreDescendingForTeardown(t *testing.T) {
	groups := Waves([]v1alpha1.InventoryEntry{
		inventoryEntry("identity", 0, 0),
		inventoryEntry("ra-b", 2, 0),
		inventoryEntry("ra-a", 2, 0),
		inventoryEntry("middle", 1, 0),
	})

	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3", len(groups))
	}
	if groups[0][0].Wave != 2 || len(groups[0]) != 2 {
		t.Errorf("first group should be the two wave-2 entries, got %+v", groups[0])
	}
	if groups[2][0].Name != "identity" {
		t.Errorf("last group should be the identity, got %+v", groups[2])
	}
	// Deterministic ordering inside a wave keeps status churn down.
	if groups[0][0].Name != "ra-a" || groups[0][1].Name != "ra-b" {
		t.Errorf("wave contents should be sorted by key, got %+v", groups[0])
	}
}

func TestApplyWavesAreAscending(t *testing.T) {
	items, err := Build(evaluated("a", "b", "c"), "ns", owner())
	if err != nil {
		t.Fatal(err)
	}
	groups := ApplyWaves(items)
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3", len(groups))
	}
	if groups[0][0].Ref.Name != "a" || groups[2][0].Ref.Name != "c" {
		t.Errorf("apply order is wrong: %v", groups)
	}
}

func TestMergeIsStable(t *testing.T) {
	out := Merge(
		[]v1alpha1.InventoryEntry{inventoryEntry("b", 1, 0), inventoryEntry("a", 0, 0)},
		[]v1alpha1.InventoryEntry{inventoryEntry("z", 0, 1)},
	)
	want := []string{"a", "z", "b"}
	for i, w := range want {
		if out[i].Name != w {
			t.Errorf("position %d = %q, want %q", i, out[i].Name, w)
		}
	}
}

func TestGroupVersionKind(t *testing.T) {
	gvk, err := GroupVersionKind(v1alpha1.InventoryEntry{
		APIVersion: "azure.m.upbound.io/v1beta1", Kind: "ResourceGroup",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gvk.Group != "azure.m.upbound.io" || gvk.Version != "v1beta1" || gvk.Kind != "ResourceGroup" {
		t.Errorf("gvk = %v", gvk)
	}

	if _, err := GroupVersionKind(v1alpha1.InventoryEntry{Name: "x", APIVersion: "a/b/c"}); err == nil {
		t.Error("an unparseable apiVersion should be an error")
	}
}

// A status write wakes the Weave through its own watch. If two reconciles over
// an unchanged world produce different inventories, the controller writes
// status, wakes itself, writes status again, and never stops - which is exactly
// what an informational "last applied" timestamp did before it was removed.
func TestSteadyStateInventoryIsIdentical(t *testing.T) {
	desired, err := Build(evaluated("identity", "assignment"), "ns", owner())
	if err != nil {
		t.Fatal(err)
	}

	pass := func() []v1alpha1.InventoryEntry {
		applied := make([]v1alpha1.InventoryEntry, 0, len(desired))
		for _, item := range desired {
			applied = append(applied, Entry(item, nil))
		}
		return Merge(applied, nil)
	}

	first := pass()
	time.Sleep(2 * time.Millisecond)
	second := pass()

	if !reflect.DeepEqual(first, second) {
		t.Errorf("two identical reconciles produced different inventories, which spins the controller forever:\n %+v\n %+v",
			first, second)
	}
}

// The counter has to stop climbing too, for the same reason.
func TestMissingCountStopsAtTheThreshold(t *testing.T) {
	t0 := time.Now()
	desired, err := Build(evaluated("kept"), "ns", owner())
	if err != nil {
		t.Fatal(err)
	}

	entry := inventoryEntry("gone", 0, 0)
	var last v1alpha1.InventoryEntry
	for i := 0; i < 20; i++ {
		d := Compute([]v1alpha1.InventoryEntry{entry}, desired, 3, time.Hour, t0.Add(time.Duration(i)*time.Second))
		if len(d.Retained) != 1 {
			t.Fatalf("pass %d: retained %d entries", i, len(d.Retained))
		}
		last = d.Retained[0]
		entry = last
	}

	if last.MissingCount != 3 {
		t.Errorf("MissingCount = %d after 20 passes, want it capped at the threshold of 3", last.MissingCount)
	}
}

// A rename is one object no longer declared and another declared in its place.
// There is no slot for it to happen inside: the old one is pruned like anything
// that stopped being described, and the new one is created.
func TestRenamingIsAPruneAndACreate(t *testing.T) {
	t0 := time.Now()
	current := []v1alpha1.InventoryEntry{{
		APIVersion: "v1", Kind: "ConfigMap", Name: "thing-one", Wave: 0, Owned: true,
		MissingCount: 3, MissingSince: timePtr(t0.Add(-time.Hour)),
	}}

	renamed, err := Build([]eval.Resource{declared("thing-two")}, "ns", owner())
	if err != nil {
		t.Fatal(err)
	}

	d := Compute(current, renamed, 3, testDelay, t0)
	if len(d.Apply) != 1 || d.Apply[0].Ref.Name != "thing-two" {
		t.Fatalf("the new object should be applied: %+v", d.Apply)
	}
	if len(d.Prune) != 1 || d.Prune[0].Name != "thing-one" {
		t.Fatalf("the old object should be pruned: %+v", d.Prune)
	}
}

// Unchanged entries are left exactly alone, which is what keeps a settled Weave
// from writing its own status on every pass.
func TestComputeIgnoresUnchangedEntries(t *testing.T) {
	current := []v1alpha1.InventoryEntry{{
		APIVersion: "v1", Kind: "ConfigMap", Name: "thing", Wave: 0, Owned: true,
	}}
	desired, err := Build([]eval.Resource{declared("thing")}, "ns", owner())
	if err != nil {
		t.Fatal(err)
	}

	d := Compute(current, desired, 3, testDelay, time.Now())
	if len(d.Prune) != 0 || len(d.Retained) != 0 || len(d.Released) != 0 {
		t.Errorf("nothing should have moved: %+v", d)
	}
}

func timePtr(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t)
	return &mt
}
