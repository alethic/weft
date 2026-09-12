// Package eval evaluates a composition program.
//
// The evaluator holds no client and knows nothing about Kubernetes beyond the
// shape of a resource. Reads a program makes go out through a Reader the caller
// supplies, so the interesting half of the system stays testable against a map
// and the language stays replaceable.
package eval

import (
	"context"
	"fmt"
)

// Request is one evaluation.
type Request struct {
	// Program is the Starlark source defining compose(variable, observed).
	Program string

	// Variables is static configuration from spec.variables.
	Variables map[string]any

	// Reader resolves the resources a program reads. Nil means a program that
	// calls read() fails, which is what the pure unit tests want.
	Reader Reader

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

	// Needs are the keys of the resources this one depends on, resolved from
	// the references the program made with resource(..., needs=[...]).
	//
	// Empty means the program said nothing, which the caller reads as "after
	// whatever came before me" - the order the program already wrote. A
	// resource that named an empty list says it depends on nothing, and that
	// distinction is the difference between a chain and a fan-out.
	Needs []string

	// NeedsDeclared is true when the program passed needs at all, empty or not.
	NeedsDeclared bool
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
	ReasonReadNotAllowed  = "ProgramReadNotAllowed"
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

// Reader resolves a resource a program asked for by name.
//
// There is no list, no label selector and no cross-namespace form. A program
// names what it wants, which is what makes the set of things it depends on
// recoverable afterwards: the caller records the calls and that recording is
// the watch set.
type Reader interface {
	// Read returns the object, or nil when it does not exist. Absence is a
	// value; a failure to read is an error and aborts the evaluation.
	//
	// Finalize asks the caller to hold the resource against deletion until this
	// composition's outputs are torn down. It is a request recorded for the
	// caller to act on after evaluation, not a write performed during it.
	Read(ctx context.Context, apiVersion, kind, name string, hold bool) (map[string]any, error)

	// Select returns every object of this kind matching the labels, in any
	// order. An empty selector matches everything of that kind in the
	// namespace.
	//
	// There is no hold form. Holding a set against deletion means holding
	// membership that changes underneath the hold, and a composition that needs
	// ordering against one specific object can read it by name.
	Select(ctx context.Context, apiVersion, kind string, labels map[string]string) ([]map[string]any, error)
}

// Evaluator turns a Request into a Result.
//
// Implementations must be safe for concurrent use.
type Evaluator interface {
	Evaluate(ctx context.Context, req Request) (*Result, error)
}
