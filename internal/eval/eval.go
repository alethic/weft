// Package eval evaluates a composition program.
//
// The evaluator is deliberately a pure function over three JSON-shaped blobs.
// It never touches a cluster, holds no client, and knows nothing about
// Kubernetes beyond the shape of a resource. That keeps the interesting half of
// the system testable without a cluster, and leaves the language replaceable.
package eval

import (
	"context"
	"fmt"
)

// Request is one evaluation.
type Request struct {
	// Program is the Starlark source defining compose(inputs, sources, observed).
	Program string

	// Inputs is static configuration from spec.inputs.
	Inputs map[string]any

	// Sources maps declared source id to the resolved object, or nil when the
	// resource does not exist. A source that could not be *read* never reaches
	// here: an unreadable source is a permission failure, not an absence.
	Sources map[string]any

	// Observed maps inventory key to the live object this Weave previously
	// created, including current status. Self-reference through this map is how
	// multi-phase advancement works: create an identity, wait for its provider
	// status to populate, then create the things that consume it.
	Observed map[string]any
}

// Resource is one entry of a successful evaluation, in return order.
type Resource struct {
	// Key is the stable inventory identity: the key compose() returned it
	// under.
	Key string

	// Object is the resource body as returned, unmodified. Normalisation
	// (namespace, owner references, labels) is the caller's job so that
	// evaluation stays pure.
	Object map[string]any
}

// Wait is the third outcome: not resolved, not invalid, not yet.
type Wait struct {
	// Reason is a human-legible description of the specific thing that has not
	// resolved, suitable for a status condition message.
	Reason string
}

// Result is the outcome of a successful evaluation. Exactly one of Resources or
// Wait is meaningful: when Wait is non-nil the program declined to produce a
// result and Resources is nil.
type Result struct {
	// Resources is the returned set, in return order. Order is meaningful: the
	// caller derives apply order from it, and therefore teardown order.
	Resources []Resource

	// Wait is non-nil when the program signalled that it cannot proceed yet.
	Wait *Wait

	// Pending lists things the program noted as unresolved while still
	// returning resources.
	//
	// Staging needs this. A composition that creates an identity and then role
	// assignments that consume it emits the identity on the first pass and
	// nothing else, and reporting that as fully converged would be a lie:
	// half the composition does not exist. Wait cannot express it either,
	// because a wait produces no resources at all and the identity would never
	// be created.
	Pending []string
}

// Waiting reports whether this result is a wait rather than a resource set.
func (r *Result) Waiting() bool { return r != nil && r.Wait != nil }

// Reasons a program can be permanently wrong. These map onto the reason field
// of the Degraded condition, so they are part of the user-visible surface.
const (
	ReasonSyntaxError     = "ProgramSyntaxError"
	ReasonNoComposeFunc   = "ProgramMissingCompose"
	ReasonRuntimeError    = "ProgramFailed"
	ReasonBudgetExceeded  = "ProgramBudgetExceeded"
	ReasonInvalidOutput   = "ProgramInvalidOutput"
	ReasonLoadNotAllowed  = "ProgramLoadNotAllowed"
	ReasonOutputTooLarge  = "ProgramOutputTooLarge"
	ReasonInvalidArgument = "ProgramInvalidArgument"
)

// ProgramError is a fault in the program itself: it will not resolve on its own
// and no amount of waiting helps. Distinct from a Wait, and distinct from an
// infrastructure error on the caller's side.
type ProgramError struct {
	// Reason is one of the Reason constants above.
	Reason string
	// Msg is the message shown to the author.
	Msg string
	// Backtrace is the Starlark call stack, when there is one.
	Backtrace string
}

func (e *ProgramError) Error() string {
	if e.Backtrace != "" {
		return fmt.Sprintf("%s: %s\n%s", e.Reason, e.Msg, e.Backtrace)
	}
	return fmt.Sprintf("%s: %s", e.Reason, e.Msg)
}

func programErrorf(reason, format string, args ...any) *ProgramError {
	return &ProgramError{Reason: reason, Msg: fmt.Sprintf(format, args...)}
}

// Evaluator turns a Request into a Result.
//
// Implementations must be safe for concurrent use.
type Evaluator interface {
	Evaluate(ctx context.Context, req Request) (*Result, error)
}
