// Package watches maintains the dynamic watch set.
//
// Two things make this simpler than it looks. Watches are only a trigger:
// because every field value is read through an impersonated Get at reconcile
// time, a watch that cannot be established costs latency and nothing else, so
// the fallback is to poll rather than to fail. And the informers are
// metadata-only and run as the Weave's own ServiceAccount, so a user grants
// read access with a RoleBinding they can write themselves, and there is
// structurally no cached field value for anything to read by mistake.
package watches

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/metadata/metadatainformer"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/kube"
)

// ErrNotStarted is returned when the registry is used before the manager has
// started it. Callers should treat it as a reason to requeue.
var ErrNotStarted = errors.New("watch registry has not started yet")

// Key identifies one watch: a type, in a namespace, observed as one identity.
//
// The identity is part of the key because the watch runs as that
// ServiceAccount. Two Weaves in the same namespace watching the same kind
// through different ServiceAccounts have genuinely different authorisation and
// cannot share an informer.
type Key struct {
	GVK            schema.GroupVersionKind
	Namespace      string
	ServiceAccount string
}

func (k Key) String() string {
	return fmt.Sprintf("%s %s/%s as %s", k.GVK.Kind, k.GVK.GroupVersion(), k.Namespace, k.ServiceAccount)
}

// Failure records a watch that could not be established.
type Failure struct {
	Key Key
	Err error
}

// Report is the outcome of reconciling one Weave's watch set.
type Report struct {
	// Established are the keys with a running informer.
	Established []Key

	// Failed are the keys with no informer. The caller falls back to polling:
	// a ServiceAccount that holds get but not list or watch can still be read,
	// just not observed.
	Failed []Failure
}

// Degraded reports whether any watch had to fall back to polling.
func (r Report) Degraded() bool { return len(r.Failed) > 0 }

// Options configure the registry.
type Options struct {
	// Resync is the informer's resync period. Resyncs are filtered out before
	// they reach the queue, so this exists to bound how long a missed event can
	// go unnoticed, not to drive reconciles.
	Resync time.Duration

	// Lifetime bounds how long one watch runs before it is torn down and
	// re-established.
	//
	// Authorisation is checked when a watch is opened and not continuously, so
	// a watch that outlives the RoleBinding that permitted it keeps delivering.
	// Restarting periodically is what turns a revoked grant into a closed watch
	// within a bounded time.
	Lifetime time.Duration

	// Buffer sizes the event channel.
	Buffer int

	Log logr.Logger
}

func (o *Options) applyDefaults() {
	if o.Resync == 0 {
		o.Resync = 10 * time.Minute
	}
	if o.Lifetime == 0 {
		o.Lifetime = 30 * time.Minute
	}
	if o.Buffer == 0 {
		o.Buffer = 1024
	}
}

// Registry owns every dynamic watch, reference-counted by Weave.
type Registry struct {
	opts   Options
	events chan event.GenericEvent

	mu      sync.Mutex
	started bool
	ctx     context.Context
	entries map[Key]*entry
	refs    map[types.NamespacedName]map[Key]bool
}

// entry is one running informer and the Weaves that depend on it.
type entry struct {
	key    Key
	client *kube.Client
	refs   map[types.NamespacedName]bool
	cancel context.CancelFunc

	mu     sync.Mutex
	failed error
}

// NewRegistry returns a registry. It must be added to a manager before use.
func NewRegistry(opts Options) *Registry {
	opts.applyDefaults()
	return &Registry{
		opts:    opts,
		events:  make(chan event.GenericEvent, opts.Buffer),
		entries: make(map[Key]*entry),
		refs:    make(map[types.NamespacedName]map[Key]bool),
	}
}

// Events is the channel to wire into the controller as a source.
func (r *Registry) Events() <-chan event.GenericEvent { return r.events }

// Start implements manager.Runnable. It holds the parent context for every
// informer and tears them all down on shutdown.
func (r *Registry) Start(ctx context.Context) error {
	r.mu.Lock()
	r.started = true
	r.ctx = ctx
	r.mu.Unlock()

	<-ctx.Done()

	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.entries {
		e.cancel()
		delete(r.entries, k)
	}
	r.refs = make(map[types.NamespacedName]map[Key]bool)
	return nil
}

// NeedLeaderElection implements manager.LeaderElectionRunnable. Watches exist
// to drive reconciliation, so they belong to whichever replica is reconciling.
func (r *Registry) NeedLeaderElection() bool { return true }

// Ensure makes the registry's watch set for one Weave exactly the given keys,
// starting what is missing and releasing what is no longer referenced.
func (r *Registry) Ensure(weave types.NamespacedName, c *kube.Client, want []Key) (Report, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.started {
		return Report{}, ErrNotStarted
	}

	wanted := make(map[Key]bool, len(want))
	for _, k := range want {
		wanted[k] = true
	}

	for k := range r.refs[weave] {
		if !wanted[k] {
			r.releaseLocked(weave, k)
		}
	}

	var report Report
	for k := range wanted {
		e := r.acquireLocked(weave, k, c)
		if err := e.err(); err != nil {
			report.Failed = append(report.Failed, Failure{Key: k, Err: err})
		} else {
			report.Established = append(report.Established, k)
		}
	}

	if len(wanted) == 0 {
		delete(r.refs, weave)
	} else {
		r.refs[weave] = wanted
	}
	return report, nil
}

// Release drops every watch held for a Weave.
func (r *Registry) Release(weave types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.refs[weave] {
		r.releaseLocked(weave, k)
	}
	delete(r.refs, weave)
}

// Len reports how many informers are running, for tests and metrics.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func (r *Registry) acquireLocked(weave types.NamespacedName, k Key, c *kube.Client) *entry {
	if e, ok := r.entries[k]; ok {
		e.refs[weave] = true
		return e
	}
	e := &entry{key: k, client: c, refs: map[types.NamespacedName]bool{weave: true}}
	r.entries[k] = e
	r.startLocked(e)
	return e
}

func (r *Registry) releaseLocked(weave types.NamespacedName, k Key) {
	e, ok := r.entries[k]
	if !ok {
		return
	}
	delete(e.refs, weave)
	if len(e.refs) > 0 {
		return
	}
	// Nothing references this watch any more, so it goes away rather than
	// accumulating for the lifetime of the process.
	e.cancel()
	delete(r.entries, k)
	r.opts.Log.V(1).Info("stopped watch", "key", k.String())
}

// startLocked builds and runs the informer for an entry. The caller holds the
// registry lock.
func (r *Registry) startLocked(e *entry) {
	gvr, err := e.client.ResourceFor(e.key.GVK)
	if err != nil {
		e.setErr(err)
		e.cancel = func() {}
		return
	}

	ctx, cancel := context.WithCancel(r.ctx)
	e.cancel = cancel

	factory := metadatainformer.NewFilteredMetadataInformer(
		e.client.Metadata(), gvr, e.key.Namespace, r.opts.Resync, toolscache.Indexers{}, nil)
	informer := factory.Informer()

	// A watch failure is reported, not retried into oblivion: the caller polls
	// instead, and a Weave whose ServiceAccount holds get but not list keeps
	// working at lower resolution.
	if err := informer.SetWatchErrorHandler(func(_ *toolscache.Reflector, err error) {
		e.setErr(err)
		r.opts.Log.V(1).Info("watch failed, falling back to polling", "key", e.key.String(), "err", err.Error())
	}); err != nil {
		e.setErr(err)
		cancel()
		return
	}

	if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(any) { r.notify(e.key) },
		UpdateFunc: func(old, updated any) {
			// Resyncs re-deliver unchanged objects. Enqueueing on those would
			// turn every informer's resync into a cluster-wide reconcile storm.
			om, ok1 := old.(*metav1.PartialObjectMetadata)
			nm, ok2 := updated.(*metav1.PartialObjectMetadata)
			if ok1 && ok2 && om.ResourceVersion == nm.ResourceVersion {
				return
			}
			r.notify(e.key)
		},
		DeleteFunc: func(any) { r.notify(e.key) },
	}); err != nil {
		e.setErr(err)
		cancel()
		return
	}

	go informer.Run(ctx.Done())
	go r.expire(ctx, e.key)
	r.opts.Log.V(1).Info("started watch", "key", e.key.String())
}

// expire tears a watch down after its lifetime and starts a replacement, so a
// grant that has been revoked stops being honoured within a bounded time.
func (r *Registry) expire(ctx context.Context, k Key) {
	d := r.opts.Lifetime
	// Jitter keeps every watch in the process from expiring in the same second
	// after a restart.
	d += time.Duration(rand.Int63n(int64(d / 4)))

	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[k]
	if !ok || len(e.refs) == 0 {
		return
	}
	e.cancel()
	replacement := &entry{key: k, client: e.client, refs: e.refs}
	r.entries[k] = replacement
	r.startLocked(replacement)
}

// notify enqueues every Weave that depends on a key.
func (r *Registry) notify(k Key) {
	r.mu.Lock()
	e, ok := r.entries[k]
	if !ok {
		r.mu.Unlock()
		return
	}
	weaves := make([]types.NamespacedName, 0, len(e.refs))
	for w := range e.refs {
		weaves = append(weaves, w)
	}
	ctx := r.ctx
	r.mu.Unlock()

	for _, w := range weaves {
		ev := event.GenericEvent{Object: weaveRef(w)}
		select {
		case r.events <- ev:
		case <-ctx.Done():
			return
		}
	}
}

// weaveRef is the minimum object the enqueue handler needs: a name and a
// namespace.
func weaveRef(w types.NamespacedName) client.Object {
	return &v1alpha1.Weave{
		ObjectMeta: metav1.ObjectMeta{Name: w.Name, Namespace: w.Namespace},
	}
}

func (e *entry) setErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failed = err
}

func (e *entry) err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.failed
}
