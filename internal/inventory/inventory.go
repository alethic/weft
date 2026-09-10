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
	"strconv"
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
}

// GroupVersionKind of the item's object.
func (i Item) GroupVersionKind() schema.GroupVersionKind { return i.Object.GroupVersionKind() }

// Build normalises an evaluated resource set and assigns apply order.
//
// Order defaults to the position in the returned mapping, which is the order
// the program wrote and, for a program that creates a thing before the things
// that consume it, already the dependency order. A resource can override it
// with the wave annotation, which is how a fan-out of independent siblings gets
// torn down in one step instead of one at a time.
func Build(resources []eval.Resource, namespace string, owner kube.Owner) ([]Item, error) {
	items := make([]Item, 0, len(resources))
	explicit := 0

	for i, r := range resources {
		u, err := kube.Normalize(r.Object, r.Key, namespace, owner)
		if err != nil {
			return nil, fmt.Errorf("resource %q %w", r.Key, err)
		}
		wave := int32(i)
		if s, ok := u.GetAnnotations()[naming.WaveAnnotation]; ok {
			n, err := strconv.ParseInt(s, 10, 32)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("resource %q has %s=%q, which is not a non-negative integer",
					r.Key, naming.WaveAnnotation, s)
			}
			wave = int32(n)
			explicit++
		}
		items = append(items, Item{Key: r.Key, Object: u, Wave: wave})
	}

	// Mixing explicit waves with positional defaults produces an order nobody
	// intended: an annotated resource at wave 1 would be torn down after an
	// unannotated one that happens to sit at position 5.
	if explicit > 0 && explicit != len(items) {
		return nil, fmt.Errorf(
			"%d of %d resources set %s: either annotate all of them or none, because mixing explicit waves with positional order produces an ordering nobody wrote",
			explicit, len(items), naming.WaveAnnotation)
	}

	return items, nil
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

	wanted := make(map[string]bool, len(desired))
	for _, d := range desired {
		wanted[d.Key] = true
	}

	d := Diff{Apply: append([]Item(nil), desired...)}
	sort.SliceStable(d.Apply, func(i, j int) bool { return d.Apply[i].Wave < d.Apply[j].Wave })

	for _, e := range current {
		if wanted[e.Key] {
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
