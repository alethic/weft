// Package inventory turns an evaluated resource set into a plan against what a
// Weave already owns.
//
// Two ideas carry most of the weight. Identity is the key a program returned a
// resource under, never its position, so reordering a list cannot rename a live
// object. And order is recorded, because cascading garbage collection is
// unordered while self-reference creates a real dependency graph: an identity
// and the role assignments that consume it must not be collected in an
// arbitrary sequence.
package inventory

import (
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/eval"
	"github.com/alethic/weft/internal/kube"
	"github.com/alethic/weft/internal/naming"
)

// Item is one normalised resource ready to apply.
type Item struct {
	// Key is the stable identity from the program's returned mapping.
	Key string

	// Object is normalised and owned, ready for server-side apply.
	Object *unstructured.Unstructured

	// Wave is the apply order. Resources are applied in ascending wave and
	// deleted in descending wave.
	Wave int32

	// Owned is false when the resource carries weft.run/owned=false. An unowned
	// resource is applied and kept current, but no owner reference is placed on
	// it and it is never deleted by this Weave.
	Owned bool
}

// GroupVersionKind of the item's object.
func (i Item) GroupVersionKind() schema.GroupVersionKind { return i.Object.GroupVersionKind() }

// Build normalises an evaluated resource set and assigns apply order.
//
// Order defaults to the position in the returned mapping, which is the order
// the program wrote. For a program that creates a thing before the things that
// consume it - which is what the control flow of a composition already
// enforces - that is the dependency order, and nothing has to be annotated for
// teardown to be correct.
//
// What the needs annotation buys is parallelism, not correctness. Teardown
// waits for a wave to be confirmed gone before starting the next, so a
// composition with no annotations is torn down one object at a time. Siblings
// that depend on something in common but not on each other - seven role
// assignments under one identity - say so, and go together. For a managed
// resource that takes minutes to delete that is the difference between one wait
// and seven.
//
// The wave is derived, never written: it is the depth of a resource in the
// dependency graph. Resources that need the same things and not each other come
// out at the same depth without anybody deciding that they should.
func Build(resources []eval.Resource, namespace string, owner kube.Owner) ([]Item, error) {
	items := make([]Item, 0, len(resources))
	needs := make(map[string][]string, len(resources))
	index := make(map[string]int, len(resources))

	for i, r := range resources {
		owned, err := ownedFrom(r.Object, r.Key)
		if err != nil {
			return nil, err
		}

		u, err := kube.Normalize(r.Object, r.Key, namespace, owner, owned)
		if err != nil {
			return nil, fmt.Errorf("resource %q %w", r.Key, err)
		}

		deps, declared, err := needsFrom(u.GetAnnotations(), r.Key)
		if err != nil {
			return nil, err
		}
		if !declared && i > 0 {
			// Undeclared means "after whatever came before me", which is what
			// the program already said by returning it in that order. Only a
			// resource that names its dependencies is freed from the chain.
			deps = []string{resources[i-1].Key}
		}

		needs[r.Key] = deps
		index[r.Key] = i
		items = append(items, Item{Key: r.Key, Object: u, Owned: owned})
	}

	levels, err := depths(items, needs)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Wave = levels[items[i].Key]
	}

	return items, nil
}

// needsFrom parses the needs annotation. The second result distinguishes "named
// nothing" from "did not say", which are different: an explicit empty value is
// a resource declaring it depends on nothing at all.
func needsFrom(annotations map[string]string, key string) ([]string, bool, error) {
	raw, ok := annotations[naming.NeedsAnnotation]
	if !ok {
		return nil, false, nil
	}

	var out []string
	for _, part := range strings.Split(raw, ",") {
		dep := strings.TrimSpace(part)
		if dep == "" {
			continue
		}
		if dep == key {
			return nil, false, fmt.Errorf("resource %q sets %s to itself", key, naming.NeedsAnnotation)
		}
		out = append(out, dep)
	}
	return out, true, nil
}

// depths assigns each resource the length of the longest dependency chain
// reaching it, which is its wave.
//
// A cycle is reported rather than broken. Breaking one silently would produce an
// apply order that looks fine and is not, and the program said something
// impossible: the author has to decide which edge is wrong.
func depths(items []Item, needs map[string][]string) (map[string]int32, error) {
	known := make(map[string]bool, len(items))
	for _, it := range items {
		known[it.Key] = true
	}
	for _, it := range items {
		for _, dep := range needs[it.Key] {
			if !known[dep] {
				return nil, fmt.Errorf(
					"resource %q needs %q, which this program does not return. %s names the keys of "+
						"other resources in the same result, so this is either a typo or a resource that "+
						"is only returned on some passes",
					it.Key, dep, naming.NeedsAnnotation)
			}
		}
	}

	const (
		unvisited = 0
		onStack   = 1
		done      = 2
	)
	state := make(map[string]int, len(items))
	level := make(map[string]int32, len(items))

	var visit func(key string, path []string) error
	visit = func(key string, path []string) error {
		switch state[key] {
		case done:
			return nil
		case onStack:
			return fmt.Errorf("resources %s form a dependency cycle through %s, so there is no order "+
				"in which they can be applied", describeCycle(append(path, key)), naming.NeedsAnnotation)
		}

		state[key] = onStack
		var deepest int32
		for _, dep := range needs[key] {
			if err := visit(dep, append(path, key)); err != nil {
				return err
			}
			if level[dep]+1 > deepest {
				deepest = level[dep] + 1
			}
		}
		state[key] = done
		level[key] = deepest
		return nil
	}

	for _, it := range items {
		if err := visit(it.Key, nil); err != nil {
			return nil, err
		}
	}
	return level, nil
}

// describeCycle renders the loop starting from where it closes.
func describeCycle(path []string) string {
	closing := path[len(path)-1]
	from := 0
	for i, k := range path {
		if k == closing {
			from = i
			break
		}
	}
	quoted := make([]string, 0, len(path)-from)
	for _, k := range path[from:] {
		quoted = append(quoted, fmt.Sprintf("%q", k))
	}
	return strings.Join(quoted, " -> ")
}

// ownedFrom reads the ownership annotation off a returned resource.
//
// A typo is rejected rather than ignored. "owned: no" quietly meaning "owned"
// is precisely the failure this annotation exists to prevent, and it would only
// be discovered by the object being deleted.
func ownedFrom(obj map[string]any, key string) (bool, error) {
	md, _ := obj["metadata"].(map[string]any)
	annotations, _ := md["annotations"].(map[string]any)
	raw, ok := annotations[naming.OwnedAnnotation]
	if !ok {
		return true, nil
	}
	switch raw {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("resource %q has %s=%v, which must be \"true\" or \"false\"",
			key, naming.OwnedAnnotation, raw)
	}
}

// Diff classifies an existing inventory against a freshly evaluated set.
type Diff struct {
	// Apply is everything the program returned, in ascending wave.
	Apply []Item

	// Retained is inventory that was not returned this time but has not been
	// absent long enough to act on. Its MissingCount has been incremented.
	Retained []v1alpha1.InventoryEntry

	// Prune is inventory absent for long enough to delete, in descending wave.
	Prune []v1alpha1.InventoryEntry

	// Released is inventory that is no longer described and was applied
	// unowned, so the record is dropped and the object left standing.
	//
	// No hysteresis applies. Waiting exists to avoid destroying something over
	// a transient absence, and nothing here is destroyed - a release that turns
	// out to be premature is undone by the next pass recording it again.
	Released []v1alpha1.InventoryEntry

	// Replaced is inventory whose key is still returned but which now names a
	// different object, because the program was edited to change a name or a
	// kind under a stable key.
	//
	// The old object has to be deleted or it is orphaned: the inventory records
	// the new one under that key and nothing remembers the old, which is
	// exactly the failure of applying without pruning. No hysteresis applies -
	// the program said the object is different, which is a statement rather
	// than an absence.
	Replaced []v1alpha1.InventoryEntry
}

// objectID is what an inventory entry actually addresses, as distinct from the
// key that identifies it.
type objectID struct {
	apiVersion string
	kind       string
	name       string
}

func idOfEntry(e v1alpha1.InventoryEntry) objectID {
	return objectID{apiVersion: e.APIVersion, kind: e.Kind, name: e.Name}
}

func idOfItem(i Item) objectID {
	gvk := i.GroupVersionKind()
	return objectID{
		apiVersion: gvk.GroupVersion().String(),
		kind:       gvk.Kind,
		name:       i.Object.GetName(),
	}
}

// Compute diffs a desired set against the recorded inventory.
//
// Absence is not always meaningful. A managed resource drops its status while
// its provider restarts, and a program waiting on that status legitimately
// stops returning whatever depends on it for as long as the restart takes.
// Deleting on the first sight of that churns real infrastructure.
//
// Both a count and a delay have to pass, and the delay is the one doing the
// work. Reconciles are event-driven, and the applies in a single pass generate
// watch events of their own, so a handful of "consecutive evaluations" can
// complete inside a second. A count alone measures controller activity; only a
// clock measures how long something has actually been gone.
//
// Callers must only apply this to a successful evaluation. One that waited or
// failed says nothing about what should exist, and counting it would make a
// provider outage look like a deletion.
func Compute(current []v1alpha1.InventoryEntry, desired []Item, threshold int32, delay time.Duration, now time.Time) Diff {
	if threshold < 1 {
		threshold = 1
	}

	wanted := make(map[string]objectID, len(desired))
	claimed := make(map[objectID]bool, len(desired))
	for _, item := range desired {
		id := idOfItem(item)
		wanted[item.Key] = id
		claimed[id] = true
	}

	d := Diff{Apply: append([]Item(nil), desired...)}
	sort.SliceStable(d.Apply, func(i, j int) bool { return d.Apply[i].Wave < d.Apply[j].Wave })

	for _, e := range current {
		if id, ok := wanted[e.Key]; ok {
			// The key survived, but check what it now addresses. An edit that
			// changes a name or a kind under a stable key leaves the old object
			// behind, and nothing would ever look at it again.
			if idOfEntry(e) != id {
				// The old object is no longer described by anything. Releasing
				// it honours the annotation it was applied with; deleting it
				// would make "do not delete this" mean "unless the program is
				// edited", which is not what it says.
				if !e.Owned {
					d.Released = append(d.Released, e)
				} else {
					d.Replaced = append(d.Replaced, e)
				}
			}
			continue
		}

		// The key is gone, but the object it addressed may have been re-keyed
		// rather than dropped. Deleting it would destroy something the program
		// still asks for; the new key owns the record now.
		if claimed[idOfEntry(e)] {
			continue
		}

		// Marked to outlive the composition. Let it go now: hysteresis is about
		// how long to wait before destroying something, and this is never
		// destroyed.
		if !e.Owned {
			d.Released = append(d.Released, e)
			continue
		}
		// Capped, because status writes wake the Weave through its own watch
		// and a forever-climbing counter is a forever-spinning controller.
		if e.MissingCount < threshold {
			e.MissingCount++
		}
		if e.MissingSince == nil {
			since := metav1.NewTime(now)
			e.MissingSince = &since
		}
		if e.MissingCount >= threshold && !now.Before(e.MissingSince.Add(delay)) {
			d.Prune = append(d.Prune, e)
		} else {
			d.Retained = append(d.Retained, e)
		}
	}
	sortDescending(d.Prune)
	sortDescending(d.Replaced)
	sortDescending(d.Released)
	return d
}

func sortDescending(entries []v1alpha1.InventoryEntry) {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Wave > entries[j].Wave })
}

// Entry records an applied item in the inventory.
func Entry(item Item, applied *unstructured.Unstructured) v1alpha1.InventoryEntry {
	gvk := item.GroupVersionKind()
	e := v1alpha1.InventoryEntry{
		Key:        item.Key,
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Name:       item.Object.GetName(),
		Wave:       item.Wave,
		Owned:      item.Owned,
	}
	if applied != nil {
		e.UID = applied.GetUID()
		// A server-side apply can rename nothing, but reading the name back
		// keeps the record true to what exists rather than to what was asked
		// for.
		e.Name = applied.GetName()
	}
	return e
}

// Merge produces the inventory to record: the entries just applied, plus the
// ones being retained through a transient absence, in a stable order.
func Merge(applied []v1alpha1.InventoryEntry, retained []v1alpha1.InventoryEntry) []v1alpha1.InventoryEntry {
	out := make([]v1alpha1.InventoryEntry, 0, len(applied)+len(retained))
	out = append(out, applied...)
	out = append(out, retained...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Wave != out[j].Wave {
			return out[i].Wave < out[j].Wave
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Waves groups entries into descending wave order for teardown.
//
// Deletion runs one wave at a time and waits for a wave to be gone before
// starting the next, because that ordering is the entire reason the wave is
// recorded. Entries within a wave have no dependency on each other and are
// deleted together, which is what keeps a fan-out of seven role assignments
// from becoming seven sequential round trips against a cloud API.
func Waves(entries []v1alpha1.InventoryEntry) [][]v1alpha1.InventoryEntry {
	if len(entries) == 0 {
		return nil
	}
	byWave := map[int32][]v1alpha1.InventoryEntry{}
	for _, e := range entries {
		byWave[e.Wave] = append(byWave[e.Wave], e)
	}
	waves := make([]int32, 0, len(byWave))
	for w := range byWave {
		waves = append(waves, w)
	}
	sort.Slice(waves, func(i, j int) bool { return waves[i] > waves[j] })

	out := make([][]v1alpha1.InventoryEntry, 0, len(waves))
	for _, w := range waves {
		group := byWave[w]
		sort.SliceStable(group, func(i, j int) bool { return group[i].Key < group[j].Key })
		out = append(out, group)
	}
	return out
}

// ApplyWaves groups items into ascending wave order.
func ApplyWaves(items []Item) [][]Item {
	if len(items) == 0 {
		return nil
	}
	byWave := map[int32][]Item{}
	for _, i := range items {
		byWave[i.Wave] = append(byWave[i.Wave], i)
	}
	waves := make([]int32, 0, len(byWave))
	for w := range byWave {
		waves = append(waves, w)
	}
	sort.Slice(waves, func(i, j int) bool { return waves[i] < waves[j] })

	out := make([][]Item, 0, len(waves))
	for _, w := range waves {
		group := byWave[w]
		sort.SliceStable(group, func(i, j int) bool { return group[i].Key < group[j].Key })
		out = append(out, group)
	}
	return out
}

// GroupVersionKind parses an inventory entry's type.
func GroupVersionKind(e v1alpha1.InventoryEntry) (schema.GroupVersionKind, error) {
	gv, err := schema.ParseGroupVersion(e.APIVersion)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("inventory entry %q has an unparseable apiVersion %q: %w", e.Key, e.APIVersion, err)
	}
	return gv.WithKind(e.Kind), nil
}
