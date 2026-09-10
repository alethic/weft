package controller

import "fmt"

// haltKind distinguishes the two ways a reconcile can stop early.
//
// Keeping them apart in the type system rather than in an if-statement is
// deliberate: the whole design turns on "not yet" being a first-class outcome
// rather than a soft error, and a codebase that carries them in one error type
// eventually starts treating one like the other.
type haltKind int

const (
	// haltWaiting means something has not resolved. It will resolve on its own
	// if the thing it depends on appears, so nothing is wrong.
	haltWaiting haltKind = iota
	// haltDegraded means something will not resolve without a change: a denied
	// permission, a program fault, a spec that cannot be honoured.
	haltDegraded
)

// halt stops a reconcile with a reason fit for a status condition.
type halt struct {
	kind    haltKind
	reason  string
	message string
}

func (h *halt) Error() string { return h.reason + ": " + h.message }

func waitingf(reason, format string, args ...any) *halt {
	return &halt{kind: haltWaiting, reason: reason, message: fmt.Sprintf(format, args...)}
}

func degradedf(reason, format string, args ...any) *halt {
	return &halt{kind: haltDegraded, reason: reason, message: fmt.Sprintf(format, args...)}
}
