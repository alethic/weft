package inventory

import (
	"strings"
	"testing"

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

func evaluated(keys ...string) []eval.Resource {
	out := make([]eval.Resource, 0, len(keys))
	for _, k := range keys {
		out = append(out, eval.Resource{Key: k, Object: res(k)})
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
		if items[i].Key != want.key || items[i].Wave != want.wave {
			t.Errorf("item %d = %q wave %d, want %q wave %d", i, items[i].Key, items[i].Wave, want.key, want.wave)
		}
	}
}

func TestBuildHonoursWaveAnnotation(t *testing.T) {
	withWave := func(w string) func(map[string]any) {
		return func(obj map[string]any) {
			md := obj["metadata"].(map[string]any)
			md["annotations"] = map[string]any{naming.WaveAnnotation: w}
		}
	}
	items, err := Build([]eval.Resource{
		{Key: "ra-a", Object: res("ra-a", withWave("1"))},
		{Key: "identity", Object: res("identity", withWave("0"))},
		{Key: "ra-b", Object: res("ra-b", withWave("1"))},
	}, "ns", owner())
	if err != nil {
		t.Fatal(err)
	}

	waves := ApplyWaves(items)
	if len(waves) != 2 {
		t.Fatalf("got %d waves, want 2", len(waves))
	}
	if len(waves[0]) != 1 || waves[0][0].Key != "identity" {
		t.Errorf("first wave = %v", waves[0])
	}
	// The fan-out siblings share a wave, so they tear down together rather than
	// one round trip at a time.
	if len(waves[1]) != 2 {
		t.Errorf("second wave has %d items, want 2", len(waves[1]))
	}
}

func TestBuildRejectsMixedWaves(t *testing.T) {
	_, err := Build([]eval.Resource{
		{Key: "a", Object: res("a", func(obj map[string]any) {
			obj["metadata"].(map[string]any)["annotations"] = map[string]any{naming.WaveAnnotation: "0"}
		})},
		{Key: "b", Object: res("b")},
	}, "ns", owner())

	if err == nil || !strings.Contains(err.Error(), "all of them or none") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildRejectsBadWaveValue(t *testing.T) {
	for _, bad := range []string{"-1", "soon", ""} {
		_, err := Build([]eval.Resource{
			{Key: "a", Object: res("a", func(obj map[string]any) {
				obj["metadata"].(map[string]any)["annotations"] = map[string]any{naming.WaveAnnotation: bad}
			})},
		}, "ns", owner())
		if err == nil {
			t.Errorf("wave %q should have been rejected", bad)
		}
	}
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
	if obj.GetAnnotations()[naming.KeyAnnotation] != "cfg" {
		t.Errorf("annotations = %v", obj.GetAnnotations())
	}
}

// Same-namespace is the hard rule that makes plain ownership sufficient, so an
// output that reaches outside is rejected rather than relocated.
func TestBuildRejectsForeignNamespace(t *testing.T) {
	_, err := Build([]eval.Resource{
		{Key: "cfg", Object: res("cfg", func(obj map[string]any) {
			obj["metadata"].(map[string]any)["namespace"] = "someone-else"
		})},
	}, "sweep-labs", owner())

	if err == nil || !strings.Contains(err.Error(), "its own namespace") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildRejectsHandWrittenOwnerReferences(t *testing.T) {
	_, err := Build([]eval.Resource{
		{Key: "cfg", Object: res("cfg", func(obj map[string]any) {
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
		{Key: "cfg", Object: res("cfg", func(obj map[string]any) {
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

func inventoryEntry(key string, wave int32, missing int32) v1alpha1.InventoryEntry {
	return v1alpha1.InventoryEntry{
		Key: key, APIVersion: "v1", Kind: "ConfigMap", Name: key, Wave: wave, MissingCount: missing,
	}
}

// A resource absent from one evaluation is not deleted. Managed resources drop
// their status while a provider restarts, and a program waiting on that status
// legitimately stops returning what depends on it.
func TestComputeHysteresis(t *testing.T) {
	current := []v1alpha1.InventoryEntry{inventoryEntry("gone", 1, 0), inventoryEntry("kept", 0, 0)}
	desired, err := Build(evaluated("kept"), "ns", owner())
	if err != nil {
		t.Fatal(err)
	}

	d := Compute(current, desired, 3)
	if len(d.Prune) != 0 {
		t.Fatalf("nothing should be pruned on the first absence, got %v", d.Prune)
	}
	if len(d.Retained) != 1 || d.Retained[0].MissingCount != 1 {
		t.Fatalf("retained = %+v", d.Retained)
	}

	// Second absence.
	d = Compute([]v1alpha1.InventoryEntry{d.Retained[0], inventoryEntry("kept", 0, 0)}, desired, 3)
	if len(d.Prune) != 0 || d.Retained[0].MissingCount != 2 {
		t.Fatalf("second pass: prune=%v retained=%+v", d.Prune, d.Retained)
	}

	// Third absence reaches the threshold.
	d = Compute([]v1alpha1.InventoryEntry{d.Retained[0], inventoryEntry("kept", 0, 0)}, desired, 3)
	if len(d.Prune) != 1 || d.Prune[0].Key != "gone" {
		t.Fatalf("third pass should prune, got %v", d.Prune)
	}
	if len(d.Retained) != 0 {
		t.Errorf("retained = %v", d.Retained)
	}
}

func TestComputeReappearanceResetsTheCount(t *testing.T) {
	current := []v1alpha1.InventoryEntry{inventoryEntry("flaky", 0, 2)}
	desired, err := Build(evaluated("flaky"), "ns", owner())
	if err != nil {
		t.Fatal(err)
	}

	d := Compute(current, desired, 3)
	if len(d.Prune) != 0 || len(d.Retained) != 0 {
		t.Fatalf("a returned resource is neither pruned nor retained: %+v", d)
	}
	// The entry recorded after the apply carries a zero count, which is what
	// resets the hysteresis.
	e := Entry(d.Apply[0], nil)
	if e.MissingCount != 0 {
		t.Errorf("MissingCount = %d, want 0", e.MissingCount)
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
	if groups[2][0].Key != "identity" {
		t.Errorf("last group should be the identity, got %+v", groups[2])
	}
	// Deterministic ordering inside a wave keeps status churn down.
	if groups[0][0].Key != "ra-a" || groups[0][1].Key != "ra-b" {
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
	if groups[0][0].Key != "a" || groups[2][0].Key != "c" {
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
		if out[i].Key != w {
			t.Errorf("position %d = %q, want %q", i, out[i].Key, w)
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

	if _, err := GroupVersionKind(v1alpha1.InventoryEntry{Key: "x", APIVersion: "a/b/c"}); err == nil {
		t.Error("an unparseable apiVersion should be an error")
	}
}
