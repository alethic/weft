// Package controller reconciles Weave objects.
//
// The shape of a reconcile is: read the declared sources and everything this
// Weave already created, all as the Weave's own ServiceAccount; evaluate the
// program over them; apply what comes back; record what exists. The
// interesting parts are the places it declines to proceed - a source that has
// not appeared, a status field that has not been written back, a permission
// that was never granted - because those are the states a composition spends
// most of its life in.
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/eval"
	"github.com/alethic/weft/internal/inventory"
	"github.com/alethic/weft/internal/kube"
	"github.com/alethic/weft/internal/naming"
	"github.com/alethic/weft/internal/watches"
)

// Options tune the reconciler.
type Options struct {
	// PruneThreshold is how many consecutive successful evaluations a resource
	// must be absent from before it is deleted.
	PruneThreshold int32

	// SourceFinalizerTimeout bounds how long a finalizer placed on somebody
	// else's object may block its deletion.
	SourceFinalizerTimeout time.Duration

	// TeardownTimeout bounds ordered teardown of a deleting Weave before its
	// finalizer is released and ordinary cascading collection takes over.
	TeardownTimeout time.Duration

	// PollInterval is the requeue period when a watch could not be established
	// and the Weave is running on polling instead.
	PollInterval time.Duration

	// Backstop is the requeue period in the normal case. Watches drive
	// reconciles; this only bounds how long a missed event can go unnoticed.
	Backstop time.Duration

	// DegradedRetry is the requeue period while Degraded. The fix is usually a
	// RoleBinding, which is not something this controller watches for.
	DegradedRetry time.Duration
}

// DefaultOptions returns sensible tunables.
func DefaultOptions() Options {
	return Options{
		PruneThreshold:         3,
		SourceFinalizerTimeout: 10 * time.Minute,
		TeardownTimeout:        15 * time.Minute,
		PollInterval:           30 * time.Second,
		Backstop:               10 * time.Minute,
		DegradedRetry:          2 * time.Minute,
	}
}

func (o *Options) applyDefaults() {
	d := DefaultOptions()
	if o.PruneThreshold <= 0 {
		o.PruneThreshold = d.PruneThreshold
	}
	if o.SourceFinalizerTimeout == 0 {
		o.SourceFinalizerTimeout = d.SourceFinalizerTimeout
	}
	if o.TeardownTimeout == 0 {
		o.TeardownTimeout = d.TeardownTimeout
	}
	if o.PollInterval == 0 {
		o.PollInterval = d.PollInterval
	}
	if o.Backstop == 0 {
		o.Backstop = d.Backstop
	}
	if o.DegradedRetry == 0 {
		o.DegradedRetry = d.DegradedRetry
	}
}

// WeaveReconciler reconciles Weave objects.
type WeaveReconciler struct {
	// Client reads and writes Weave objects with the controller's own
	// identity. It is used for nothing else: every resource a composition
	// touches goes through the impersonated Factory instead.
	Client client.Client

	Scheme    *runtime.Scheme
	Factory   *kube.Factory
	Evaluator eval.Evaluator
	Watches   *watches.Registry
	Recorder  record.EventRecorder
	Opts      Options
}

// SetupWithManager registers the controller.
func (r *WeaveReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Opts.applyDefaults()
	if r.Evaluator == nil {
		r.Evaluator = eval.NewStarlark(eval.Options{})
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("weave").
		For(&v1alpha1.Weave{}).
		// Everything a composition observes arrives through the registry's
		// channel rather than through a statically declared watch, because the
		// set of types to watch is whatever the Weaves in the cluster happen to
		// declare, and it changes as they are edited.
		WatchesRawSource(source.Channel(r.Watches.Events(), &handler.EnqueueRequestForObject{})).
		Complete(r)
}

// The complete set of privileges this controller holds. It is worth reading as
// a list, because what is absent is the point: there is no write access to any
// resource type a composition might generate. The controller cannot create a
// RoleAssignment or a ConfigMap on its own account. It can only become a
// ServiceAccount that already could, which is what makes a Weave incapable of
// producing anything its author could not have produced by hand.
//
// update on weaves is for the finalizer, which is what buys ordered teardown.
//
// impersonate on serviceaccounts is namespace-scopeable: the API server checks
// a username of the form system:serviceaccount:<ns>:<name> against the
// serviceaccounts resource in that namespace, so an operator who wants to
// confine Weft to some namespaces can replace this ClusterRole with Roles.
//
// impersonate on groups is pinned to two names. Granting it unrestricted would
// let anyone who compromised the controller impersonate system:masters.
//
// +kubebuilder:rbac:groups=weft.run,resources=weaves,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=weft.run,resources=weaves/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=weft.run,resources=weaves/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=impersonate
// +kubebuilder:rbac:groups="",resources=groups,verbs=impersonate,resourceNames=system:serviceaccounts;system:authenticated
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile brings one Weave's resources in line with its program.
func (r *WeaveReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var weave v1alpha1.Weave
	if err := r.Client.Get(ctx, req.NamespacedName, &weave); err != nil {
		if apierrors.IsNotFound(err) {
			r.Watches.Release(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !weave.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, &weave)
	}

	if !controllerutil.ContainsFinalizer(&weave, naming.WeaveFinalizer) {
		// The finalizer is what buys the chance to tear resources down in
		// order. Without it, deleting a Weave hands everything to cascading
		// collection at once.
		controllerutil.AddFinalizer(&weave, naming.WeaveFinalizer)
		if err := r.Client.Update(ctx, &weave); err != nil {
			return ctrl.Result{}, err
		}
		// The update re-triggers this reconcile; nothing more to do here.
		return ctrl.Result{}, nil
	}

	before := weave.DeepCopy()
	result, err := r.reconcileActive(ctx, &weave)

	// Status is written on every path. A Weave that cannot be reconciled has
	// to say why on itself, because the alternative is what this replaces:
	// errors that land in a Job log and on no object at all.
	if serr := r.writeStatus(ctx, before, &weave); serr != nil {
		if err == nil {
			return ctrl.Result{}, serr
		}
		logf.FromContext(ctx).Error(serr, "writing status")
	}
	return result, err
}

// reconcileActive is the steady-state path.
func (r *WeaveReconciler) reconcileActive(ctx context.Context, weave *v1alpha1.Weave) (ctrl.Result, error) {
	c, err := r.Factory.For(weave.Namespace, weave.Spec.ServiceAccountName)
	if err != nil {
		markDegraded(weave, ReasonInvalidSpec, err.Error())
		return ctrl.Result{RequeueAfter: r.Opts.DegradedRetry}, nil
	}

	// Register watches before doing any work, so that a Weave which is about to
	// stop with "the ResourceGroup does not exist" still wakes up when it does.
	report, werr := r.Watches.Ensure(
		types.NamespacedName{Namespace: weave.Namespace, Name: weave.Name}, c, watchKeys(weave))
	if werr != nil && !errors.Is(werr, watches.ErrNotStarted) {
		return ctrl.Result{}, werr
	}
	requeue := r.Opts.Backstop
	if report.Degraded() || errors.Is(werr, watches.ErrNotStarted) {
		// A ServiceAccount that holds get but not list or watch can still be
		// read, just not observed. Polling is the difference between slower and
		// broken.
		requeue = r.Opts.PollInterval
	}

	result, err := r.compose(ctx, c, weave)
	if err != nil {
		var h *halt
		if errors.As(err, &h) {
			switch h.kind {
			case haltWaiting:
				markWaiting(weave, h.reason, h.message)
				return ctrl.Result{RequeueAfter: requeue}, nil
			case haltDegraded:
				markDegraded(weave, h.reason, h.message)
				r.eventf(weave, "Warning", h.reason, "%s", firstLine(h.message))
				return ctrl.Result{RequeueAfter: r.Opts.DegradedRetry}, nil
			}
		}
		// Anything else is transient: let the workqueue back off.
		return ctrl.Result{}, err
	}

	if report.Degraded() {
		setCondition(weave, naming.ConditionReady, metav1.ConditionTrue, ReasonApplied, result)
		setCondition(weave, naming.ConditionWaiting, metav1.ConditionFalse, ReasonApplied, "nothing outstanding")
		setCondition(weave, naming.ConditionDegraded, metav1.ConditionTrue, ReasonWatchDegraded,
			watchFailureMessage(report))
		return ctrl.Result{RequeueAfter: requeue}, nil
	}

	markReady(weave, result)
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// compose runs one full pass and returns a summary for the Ready condition.
func (r *WeaveReconciler) compose(ctx context.Context, c *kube.Client, weave *v1alpha1.Weave) (string, error) {
	resolved, err := r.resolveSources(ctx, c, weave)
	if err != nil {
		return "", err
	}

	// Finalizer handling runs before evaluation because a source that is going
	// away means the answer is "tear down", not "recompute".
	if err := r.reconcileSourceFinalizers(ctx, c, weave, resolved); err != nil {
		return "", err
	}

	if len(resolved.missingRequired) > 0 {
		// A required source gates evaluation even when no field is read from
		// it. This is the only way to express a pure ordering edge, and any
		// design that infers dependencies from expression references alone
		// drops it silently.
		return "", waitingf(ReasonSourceMissing,
			"waiting for %s", joinWithAnd(resolved.missingRequired))
	}

	observed, err := r.readObserved(ctx, c, weave)
	if err != nil {
		return "", err
	}

	res, err := r.Evaluator.Evaluate(ctx, eval.Request{
		Program:  weave.Spec.Program,
		Inputs:   decodeInputs(weave),
		Sources:  resolved.values,
		Observed: observed,
	})
	if err != nil {
		var pe *eval.ProgramError
		if errors.As(err, &pe) {
			return "", degradedf(pe.Reason, "%s", programMessage(pe))
		}
		return "", err
	}

	if res.Waiting() {
		return "", waitingf(ReasonFieldUnresolved, "%s", res.Wait.Reason)
	}

	owner := kube.Owner{
		APIVersion: naming.GroupVersion,
		Kind:       naming.Kind,
		Name:       weave.Name,
		UID:        weave.UID,
	}
	items, err := inventory.Build(res.Resources, weave.Namespace, owner)
	if err != nil {
		return "", degradedf(eval.ReasonInvalidOutput, "%v", err)
	}

	diff := inventory.Compute(weave.Status.Inventory, items, r.Opts.PruneThreshold)

	applied, applyErr := r.applyAll(ctx, c, diff.Apply)

	// Record what was applied before reporting any failure, so that a resource
	// created just before an error is not left off the inventory and orphaned.
	weave.Status.Inventory = inventory.Merge(applied, concat(diff.Retained, diff.Prune))
	if applyErr != nil {
		return "", applyErr
	}

	if len(diff.Prune) > 0 {
		remaining, err := r.deleteWaves(ctx, c, diff.Prune)
		if err != nil {
			return "", err
		}
		weave.Status.Inventory = inventory.Merge(applied, concat(diff.Retained, remaining))
		if len(remaining) > 0 {
			return "", waitingf(ReasonTearingDown,
				"removing %s, which the program no longer returns", describeEntries(remaining, 5))
		}
		r.eventf(weave, "Normal", "Pruned", "removed %d resources the program no longer returns", len(diff.Prune))
	}

	return summarize(len(applied), len(diff.Retained)), nil
}

// reconcileDeletion tears resources down in reverse dependency order before
// letting the Weave go.
func (r *WeaveReconciler) reconcileDeletion(ctx context.Context, weave *v1alpha1.Weave) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	key := types.NamespacedName{Namespace: weave.Namespace, Name: weave.Name}

	if !controllerutil.ContainsFinalizer(weave, naming.WeaveFinalizer) {
		r.Watches.Release(key)
		return ctrl.Result{}, nil
	}

	c, err := r.Factory.For(weave.Namespace, weave.Spec.ServiceAccountName)
	if err != nil {
		// Nothing can be torn down in order without a client. Releasing is
		// better than wedging: cascading collection still removes everything,
		// just unordered.
		log.Error(err, "releasing finalizer without ordered teardown")
		return r.finishDeletion(ctx, weave, key)
	}

	overdue := elapsed(weave.DeletionTimestamp) > r.Opts.TeardownTimeout
	if !overdue {
		remaining, err := r.deleteWaves(ctx, c, weave.Status.Inventory)
		if err != nil {
			var h *halt
			if errors.As(err, &h) && h.kind == haltDegraded {
				// A permission that has been revoked cannot be waited out, but
				// the timeout still applies: the object is not held forever.
				before := weave.DeepCopy()
				markDegraded(weave, h.reason, h.message)
				if serr := r.writeStatus(ctx, before, weave); serr != nil {
					log.Error(serr, "writing status during teardown")
				}
				return ctrl.Result{RequeueAfter: r.Opts.DegradedRetry}, nil
			}
			return ctrl.Result{}, err
		}

		if len(remaining) > 0 {
			before := weave.DeepCopy()
			weave.Status.Inventory = remaining
			markWaiting(weave, ReasonTearingDown,
				fmt.Sprintf("deleting %s in reverse dependency order", describeEntries(remaining, 5)))
			if serr := r.writeStatus(ctx, before, weave); serr != nil {
				log.Error(serr, "writing status during teardown")
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		weave.Status.Inventory = nil
	} else {
		log.Error(nil, "ordered teardown timed out; handing the rest to cascading collection",
			"timeout", r.Opts.TeardownTimeout.String(), "remaining", len(weave.Status.Inventory))
		r.eventf(weave, "Warning", "TeardownTimeout",
			"ordered teardown did not finish within %s; %d resources will be collected in no particular order",
			r.Opts.TeardownTimeout, len(weave.Status.Inventory))
	}

	if err := r.releaseAllSourceFinalizers(ctx, c, weave); err != nil {
		var h *halt
		if errors.As(err, &h) && h.kind == haltDegraded && !overdue {
			before := weave.DeepCopy()
			markDegraded(weave, h.reason, h.message)
			if serr := r.writeStatus(ctx, before, weave); serr != nil {
				log.Error(serr, "writing status during teardown")
			}
			return ctrl.Result{RequeueAfter: r.Opts.DegradedRetry}, nil
		}
		log.Error(err, "releasing source finalizers")
	}

	return r.finishDeletion(ctx, weave, key)
}

func (r *WeaveReconciler) finishDeletion(ctx context.Context, weave *v1alpha1.Weave, key types.NamespacedName) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(weave, naming.WeaveFinalizer)
	if err := r.Client.Update(ctx, weave); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	r.Watches.Release(key)
	return ctrl.Result{}, nil
}

// writeStatus persists status, skipping the write when nothing changed so a
// steady state does not generate update traffic.
func (r *WeaveReconciler) writeStatus(ctx context.Context, before, weave *v1alpha1.Weave) error {
	weave.Status.ObservedGeneration = weave.Generation
	if equality.Semantic.DeepEqual(before.Status, weave.Status) {
		return nil
	}
	err := r.Client.Status().Patch(ctx, weave, client.MergeFrom(before))
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		// The object moved on. The next reconcile recomputes everything anyway.
		return nil
	}
	return err
}

func (r *WeaveReconciler) eventf(weave *v1alpha1.Weave, eventType, reason, format string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(weave, eventType, reason, format, args...)
}

// decodeInputs turns spec.inputs into plain data for the program.
func decodeInputs(weave *v1alpha1.Weave) map[string]any {
	if weave.Spec.Inputs == nil || len(weave.Spec.Inputs.Raw) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := jsonUnmarshal(weave.Spec.Inputs.Raw, &out); err != nil {
		// The CRD schema already guarantees this is an object; a failure here
		// would mean the API server accepted something it should not have.
		return map[string]any{}
	}
	return out
}

// programMessage renders a program failure with its backtrace, which is the
// only thing that makes a Starlark error debuggable.
func programMessage(pe *eval.ProgramError) string {
	if pe.Backtrace == "" {
		return pe.Msg
	}
	return pe.Msg + "\n\n" + pe.Backtrace
}

func watchFailureMessage(report watches.Report) string {
	msg := "running on polling instead of watches, so changes are noticed on the poll interval rather than immediately:"
	for _, f := range report.Failed {
		msg += fmt.Sprintf("\n  %s: %v", f.Key.String(), f.Err)
	}
	msg += "\n\nGranting list and watch on these types to the Weave's ServiceAccount restores immediate updates."
	return msg
}

func summarize(applied, retained int) string {
	s := fmt.Sprintf("applied %d resources", applied)
	if retained > 0 {
		s += fmt.Sprintf("; %d absent from this evaluation but not yet pruned", retained)
	}
	return s
}

func joinWithAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		out := ""
		for i, s := range items[:len(items)-1] {
			if i > 0 {
				out += ", "
			}
			out += s
		}
		return out + ", and " + items[len(items)-1]
	}
}

// concat joins two entry slices without appending into either one's backing
// array, which would otherwise let a second call quietly overwrite the first
// call's result.
func concat(a, b []v1alpha1.InventoryEntry) []v1alpha1.InventoryEntry {
	out := make([]v1alpha1.InventoryEntry, 0, len(a)+len(b))
	out = append(out, a...)
	return append(out, b...)
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
