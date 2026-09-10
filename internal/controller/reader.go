package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/kube"
	"github.com/alethic/weft/internal/metrics"
)

// weaveReader answers a program's read() and select() calls, and remembers what
// it was asked for.
//
// The remembering is the point. There is no declared source list any more, so
// the only record of what a composition depends on is what it actually asked
// for while running. That recording becomes the watch set and the finalizer
// bookkeeping, which means a program that stops reading something stops being
// woken by it without anybody editing the Weave.
//
// Reads are cached for the life of one pass. A program that reads the same
// resource in three branches makes one API call, and - more importantly - sees
// the same value each time, so an evaluation cannot contradict itself.
type weaveReader struct {
	c     *kube.Client
	weave *v1alpha1.Weave

	objects map[string]*unstructured.Unstructured
	lists   map[string][]unstructured.Unstructured

	// kinds is the recorded read set, by GroupVersionKind. Watches are
	// registered per kind rather than per name, so this is the granularity that
	// matters.
	kinds map[schema.GroupVersionKind]bool

	// holds is every resource the program asked to hold, by identity.
	holds map[string]v1alpha1.HeldResource

	// deleting names the held resources found mid-deletion during this pass.
	deleting []v1alpha1.HeldResource
}

func newWeaveReader(c *kube.Client, weave *v1alpha1.Weave) *weaveReader {
	return &weaveReader{
		c:       c,
		weave:   weave,
		objects: map[string]*unstructured.Unstructured{},
		lists:   map[string][]unstructured.Unstructured{},
		kinds:   map[schema.GroupVersionKind]bool{},
		holds:   map[string]v1alpha1.HeldResource{},
	}
}

func refKey(apiVersion, kind, name string) string {
	return apiVersion + "/" + kind + "/" + name
}

// Read implements eval.Reader.
func (r *weaveReader) Read(ctx context.Context, apiVersion, kind, name string, hold bool) (map[string]any, error) {
	gvk, err := parseGVK(apiVersion, kind)
	if err != nil {
		return nil, degradedf(ReasonInvalidSpec, "read(%q, %q, %q): %v", apiVersion, kind, name, err)
	}
	r.kinds[gvk] = true

	// Absence is a value the program sees, not a verdict the controller reaches
	// on its behalf. Whether it should block, and under what conditions, is a
	// decision only the composition can make.
	obj, err := r.readRaw(ctx, gvk, name)
	if err != nil {
		return nil, err
	}

	if hold && obj != nil {
		held := v1alpha1.HeldResource{APIVersion: apiVersion, Kind: kind, Name: name}
		r.holds[refKey(apiVersion, kind, name)] = held
		if obj.GetDeletionTimestamp() != nil {
			r.deleting = append(r.deleting, held)
		}
	}

	if obj == nil {
		return nil, nil
	}
	return obj.Object, nil
}

// readRaw is the cached get behind Read, for callers inside the controller that
// need the object rather than its body.
func (r *weaveReader) readRaw(ctx context.Context, gvk schema.GroupVersionKind, name string) (*unstructured.Unstructured, error) {
	r.kinds[gvk] = true

	key := refKey(gvk.GroupVersion().String(), gvk.Kind, name)
	if obj, cached := r.objects[key]; cached {
		return obj, nil
	}

	var obj *unstructured.Unstructured
	got, err := r.c.Get(ctx, gvk, name)
	switch {
	case err == nil:
		stripNoise(got)
		obj = got
	case apierrors.IsNotFound(err):
		obj = nil
	default:
		return nil, readError(gvk, name, err)
	}
	r.objects[key] = obj
	return obj, nil
}

// Select implements eval.Reader.
func (r *weaveReader) Select(ctx context.Context, apiVersion, kind string, labels map[string]string) ([]map[string]any, error) {
	gvk, err := parseGVK(apiVersion, kind)
	if err != nil {
		return nil, degradedf(ReasonInvalidSpec, "select(%q, %q): %v", apiVersion, kind, err)
	}
	r.kinds[gvk] = true

	sel := make([]string, 0, len(labels))
	for k, v := range labels {
		sel = append(sel, k+"="+v)
	}
	sort.Strings(sel)
	key := apiVersion + "/" + kind + "?" + fmt.Sprint(sel)

	items, cached := r.lists[key]
	if !cached {
		got, err := r.c.List(ctx, gvk, labels)
		if err != nil {
			return nil, readError(gvk, "", err)
		}
		for i := range got {
			stripNoise(&got[i])
		}
		items = got
		r.lists[key] = items
	}

	out := make([]map[string]any, 0, len(items))
	for i := range items {
		out = append(out, items[i].Object)
	}
	return out, nil
}

// reads returns the recorded read set for the status, in a stable order.
func (r *weaveReader) reads() []v1alpha1.ReadRef {
	out := make([]v1alpha1.ReadRef, 0, len(r.kinds))
	for gvk := range r.kinds {
		out = append(out, v1alpha1.ReadRef{
			APIVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].APIVersion != out[j].APIVersion {
			return out[i].APIVersion < out[j].APIVersion
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// requestedHolds returns the hold requests made this pass, in a stable
// order.
func (r *weaveReader) requestedHolds() []v1alpha1.HeldResource {
	out := make([]v1alpha1.HeldResource, 0, len(r.holds))
	for _, h := range r.holds {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		return refKey(out[i].APIVersion, out[i].Kind, out[i].Name) <
			refKey(out[j].APIVersion, out[j].Kind, out[j].Name)
	})
	return out
}

// readError classifies a failed read.
//
// The predecessor to this system piped kubectl get into a template and used a
// bare shell wait, which reports success regardless of how its children exited;
// when only some resources resolved, the template rendered against a partial
// list and reported "resource not found" instead of the API error that actually
// caused it. The lesson is structural, not incidental: a failure to read aborts
// the evaluation and the API error is what gets surfaced. Absence is a value a
// program can see; a failure to read is not.
func readError(gvk schema.GroupVersionKind, name string, err error) error {
	what := gvk.Kind
	if name != "" {
		what = fmt.Sprintf("%s %q", gvk.Kind, name)
	}

	var perm *kube.PermissionError
	if errors.As(err, &perm) {
		metrics.PermissionDenialsTotal.WithLabelValues(perm.Verb, perm.Resource.Resource).Inc()
		// The controller does not fill the gap with its own privileges. It says
		// what to grant and stops.
		return degradedf(ReasonForbidden, "reading %s:\n%s", what, perm.Error())
	}

	var unknown *kube.UnknownKindError
	if errors.As(err, &unknown) {
		// Waiting rather than Degraded: a provider that installs this CRD later
		// resolves it without anybody editing the Weave.
		return waitingf(ReasonKindNotInstalled,
			"this program reads %s, and no such resource type is installed in this cluster", gvk)
	}

	var scoped *kube.ClusterScopedError
	if errors.As(err, &scoped) {
		return degradedf(ReasonClusterScoped,
			"this program reads %s, which is cluster-scoped: reading it would need a ClusterRoleBinding, "+
				"which a namespace user cannot create, so Weft only reads namespaced resources",
			gvk.Kind)
	}

	// Anything else is transient. Requeue with backoff rather than recording a
	// verdict about a cluster we could not reach.
	return fmt.Errorf("reading %s: %w", what, err)
}

func parseGVK(apiVersion, kind string) (schema.GroupVersionKind, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("apiVersion %q is not parseable: %w", apiVersion, err)
	}
	if kind == "" {
		return schema.GroupVersionKind{}, fmt.Errorf("kind must not be empty")
	}
	return gv.WithKind(kind), nil
}
