// Package metrics publishes what an operator needs to be watched from outside.
//
// controller-runtime already exports queue depth, reconcile counts and
// latencies, which say how the controller is doing. These say how the
// compositions are doing, which is a different question and usually the one
// being asked: how many Weaves are waiting, what are they waiting on, and is
// anything quietly failing to apply.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const namespace = "weft"

var (
	// WeaveStatus is the current condition of every Weave, as a gauge per
	// state. Alerting on "degraded for more than N minutes" is the obvious use;
	// waiting is deliberately not alertable on its own, because waiting is the
	// normal steady state of a composition that spans several provisioning
	// steps.
	WeaveStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "weave_status",
			Help:      "Current status of a Weave, as one series per state with a value of 0 or 1.",
		},
		[]string{"namespace", "name", "state"},
	)

	// WeaveResources is how many resources each Weave currently owns. A count
	// that drops without a corresponding prune is worth looking at.
	WeaveResources = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "weave_resources",
			Help:      "Number of resources a Weave currently owns.",
		},
		[]string{"namespace", "name"},
	)

	// EvaluationDuration is how long compose() takes. Evaluation is
	// synchronous inside the reconcile, so a slow program is a slow controller.
	EvaluationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "evaluation_duration_seconds",
			Help:      "Time spent evaluating a composition program.",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
		},
		[]string{"namespace", "name"},
	)

	// EvaluationsTotal counts evaluations by outcome. The three outcomes are
	// genuinely different and a single error counter would hide that: waiting
	// is normal, failed is not.
	EvaluationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "evaluations_total",
			Help:      "Composition evaluations by outcome: resolved, waiting or failed.",
		},
		[]string{"outcome"},
	)

	// AppliesTotal counts server-side applies, so a Weave that reapplies
	// constantly is visible before it becomes an API server problem.
	AppliesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "applies_total",
			Help:      "Resources applied, by result.",
		},
		[]string{"result"},
	)

	// PrunesTotal counts resources actually deleted after their hysteresis
	// elapsed. This deletes real infrastructure, so it is worth a counter of
	// its own rather than being folded into applies.
	PrunesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "prunes_total",
			Help:      "Resources deleted because a program stopped returning them.",
		},
	)

	// PermissionDenialsTotal counts denials under impersonation, labelled by
	// verb and resource. A cluster-wide spike here usually means somebody
	// changed a RoleBinding rather than that anything is broken.
	PermissionDenialsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "permission_denials_total",
			Help:      "Reads or writes denied under impersonation.",
		},
		[]string{"verb", "resource"},
	)

	// WatchesActive is how many dynamic informers are running, and how many
	// fell back to polling. A rising degraded count means ServiceAccounts are
	// missing list and watch.
	WatchesActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "watches_active",
			Help:      "Dynamic watches currently held, by state.",
		},
		[]string{"state"},
	)

	// BuildInfo is the usual constant-one gauge carrying build labels.
	BuildInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "build_info",
			Help:      "Build information, always 1.",
		},
		[]string{"version", "commit", "goversion"},
	)
)

// States reported by WeaveStatus. Every Weave reports exactly one of these as 1
// and the rest as 0, so a sum over the state label counts Weaves.
const (
	StateReady    = "ready"
	StateWaiting  = "waiting"
	StateDegraded = "degraded"
)

// Evaluation outcomes.
const (
	OutcomeResolved = "resolved"
	OutcomeWaiting  = "waiting"
	OutcomeFailed   = "failed"
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		WeaveStatus,
		WeaveResources,
		EvaluationDuration,
		EvaluationsTotal,
		AppliesTotal,
		PrunesTotal,
		PermissionDenialsTotal,
		WatchesActive,
		BuildInfo,
	)
}

// SetWeaveState records a Weave's condition, zeroing the states it is not in so
// a Weave never appears to be in two at once.
func SetWeaveState(namespace, name, state string) {
	for _, s := range []string{StateReady, StateWaiting, StateDegraded} {
		v := 0.0
		if s == state {
			v = 1.0
		}
		WeaveStatus.WithLabelValues(namespace, name, s).Set(v)
	}
}

// ForgetWeave drops every series for a deleted Weave. Without this the gauges
// keep reporting objects that no longer exist, and the cardinality only ever
// grows.
func ForgetWeave(namespace, name string) {
	for _, s := range []string{StateReady, StateWaiting, StateDegraded} {
		WeaveStatus.DeleteLabelValues(namespace, name, s)
	}
	WeaveResources.DeleteLabelValues(namespace, name)
	EvaluationDuration.DeleteLabelValues(namespace, name)
}
