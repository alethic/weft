package controller

import (
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/naming"
)

// Condition reasons. These are part of the user-visible surface: they are what
// shows in the Waiting column of kubectl get and what a person greps for.
const (
	ReasonApplied          = "Applied"
	ReasonSourceDeleting   = "SourceDeleting"
	ReasonFieldUnresolved  = "FieldUnresolved"
	ReasonProgramWaiting   = "ProgramWaiting"
	ReasonKindNotInstalled = "KindNotInstalled"
	ReasonForbidden        = "Forbidden"
	ReasonClusterScoped    = "ClusterScoped"
	ReasonInvalidSpec      = "InvalidSpec"
	ReasonApplyFailed      = "ApplyFailed"
	ReasonTearingDown      = "TearingDown"
	ReasonWatchDegraded    = "WatchDegraded"
	ReasonNotOurs          = "ResourceNotOurs"
	ReasonInputMissing     = "InputMissing"
)

// maxMessage keeps a condition inside the API server's limit with room to
// spare. RBAC diagnostics are deliberately verbose, so this is a real bound
// rather than a theoretical one.
const maxMessage = 30000

func truncate(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	return s[:maxMessage] + "\n... (truncated)"
}

// setCondition writes one condition, leaving LastTransitionTime alone when
// nothing changed so that a steady state does not churn the object.
func setCondition(w *v1alpha1.Weave, condType string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            truncate(message),
		ObservedGeneration: w.Generation,
	})
}

// markReady records that the returned set is fully applied.
func markReady(w *v1alpha1.Weave, message string) {
	setCondition(w, naming.ConditionReady, metav1.ConditionTrue, ReasonApplied, message)
	setCondition(w, naming.ConditionWaiting, metav1.ConditionFalse, ReasonApplied, "nothing outstanding")
	setCondition(w, naming.ConditionDegraded, metav1.ConditionFalse, ReasonApplied, "no errors")
}

// markWaiting records the third outcome: not resolved, not invalid, not yet.
//
// This is the normal steady state for a composition that spans several
// provisioning steps, not an error, and the message names the specific thing
// that has not resolved.
func markWaiting(w *v1alpha1.Weave, reason, message string) {
	setCondition(w, naming.ConditionReady, metav1.ConditionFalse, reason, message)
	setCondition(w, naming.ConditionWaiting, metav1.ConditionTrue, reason, message)
	setCondition(w, naming.ConditionDegraded, metav1.ConditionFalse, ReasonApplied, "no errors")
}

// markDegraded records something that will not resolve without a change.
func markDegraded(w *v1alpha1.Weave, reason, message string) {
	setCondition(w, naming.ConditionReady, metav1.ConditionFalse, reason, message)
	setCondition(w, naming.ConditionWaiting, metav1.ConditionFalse, reason, "blocked by an error")
	setCondition(w, naming.ConditionDegraded, metav1.ConditionTrue, reason, message)
}
