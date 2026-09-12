package eval

import (
	"fmt"

	"go.starlark.net/starlark"
)

// resourceValue is what resource() returns: a body, and references to the other
// resources it depends on.
//
// The references are to the values themselves, not to their keys. A program
// building a fan-out already holds the thing each sibling depends on, so making
// it name that thing again as a string is asking for a typo that nothing can
// catch until it produces a wrong teardown order. Holding the value means a
// misspelling is an undefined name, reported by Starlark with a backtrace,
// before the controller ever sees the result.
type resourceValue struct {
	key   string
	body  starlark.Value
	needs []*resourceValue

	// declared distinguishes resource(body) from resource(body, needs=[]).
	// The first says nothing and keeps the order the program wrote; the second
	// says "nothing at all", which is how siblings built in a loop escape being
	// chained to one another.
	declared bool
}

var _ starlark.Value = (*resourceValue)(nil)

func (r *resourceValue) Type() string         { return "resource" }
func (r *resourceValue) Truth() starlark.Bool { return starlark.True }
func (r *resourceValue) String() string       { return fmt.Sprintf("resource(%q)", r.key) }

func (r *resourceValue) Freeze() {
	r.body.Freeze()
	for _, n := range r.needs {
		n.Freeze()
	}
}

func (r *resourceValue) Hash() (uint32, error) {
	return 0, fmt.Errorf("unhashable type: resource")
}

// resourcesLocalKey is where declared resources accumulate. Threads are
// per-evaluation, so this is not shared state.
const resourcesLocalKey = "weft.resources"

// bResource implements resource(key, body, needs=None).
//
// Declaring is the act. There is nothing to collect and return, because a
// composition is a set of things that should exist and saying one exists is the
// whole statement - and because the alternative made a staged composition
// responsible for remembering to hand back what it had built so far, where
// forgetting meant returning an empty set and pruning everything.
//
// It returns a handle so other resources can point at it.
func bResource(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var key string
	var body starlark.Value
	var needs starlark.Value
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"key", &key, "body", &body, "needs?", &needs); err != nil {
		return nil, err
	}
	if body == nil || body == starlark.None {
		return nil, fmt.Errorf("resource: %q has no body", key)
	}

	deps, err := dependencies(needs)
	if err != nil {
		return nil, err
	}

	declared, _ := thread.Local(resourcesLocalKey).(*declaredSet)
	if declared == nil {
		return nil, fmt.Errorf("resource() is not available in this evaluation")
	}
	if _, dup := declared.byKey[key]; dup {
		return nil, fmt.Errorf(
			"resource %q is declared twice. A key is the identity of an object, so two declarations "+
				"cannot mean two objects", key)
	}

	rv := &resourceValue{key: key, body: body, needs: deps, declared: needs != nil}
	declared.byKey[key] = rv
	declared.order = append(declared.order, rv)
	return rv, nil
}

// declaredSet accumulates what a program declared, in the order it declared it.
// Order is meaningful: it is the apply order, and therefore the reverse of the
// teardown order, for everything that does not say otherwise.
type declaredSet struct {
	byKey map[string]*resourceValue
	order []*resourceValue
}

func newDeclaredSet() *declaredSet {
	return &declaredSet{byKey: map[string]*resourceValue{}}
}

// dependencies converts the needs argument, which is a resource or a sequence
// of them.
//
// An empty sequence is meaningful and different from omitting the argument: it
// says this resource depends on nothing, which is how siblings built in a loop
// escape being chained to one another by their position.
func dependencies(v starlark.Value) ([]*resourceValue, error) {
	if v == nil || v == starlark.None {
		return nil, nil
	}
	if one, ok := v.(*resourceValue); ok {
		return []*resourceValue{one}, nil
	}

	seq, ok := v.(starlark.Sequence)
	if !ok {
		return nil, fmt.Errorf(
			"resource: needs must be a resource or a list of them, got %s", v.Type())
	}

	out := make([]*resourceValue, 0, seq.Len())
	iter := seq.Iterate()
	defer iter.Done()
	var item starlark.Value
	for iter.Next(&item) {
		dep, ok := item.(*resourceValue)
		if !ok {
			return nil, fmt.Errorf(
				"resource: needs must hold resources, got %s. Pass the value that resource() returned "+
					"rather than a name or a body", item.Type())
		}
		out = append(out, dep)
	}
	return out, nil
}

// checkNeeds reports a reference to something that was never declared.
//
// Holding the value makes a misspelled name an undefined variable, caught by
// Starlark before this runs. What is left is referencing a resource that was
// built but never declared, which this catches at the one point where both
// halves are known.
func checkNeeds(set *declaredSet) error {
	for _, rv := range set.order {
		for _, dep := range rv.needs {
			if set.byKey[dep.key] != dep {
				return fmt.Errorf(
					"resource %q needs a resource that was never declared. Everything passed to needs "+
						"has to be declared too, or there is nothing to order against", rv.key)
			}
			if dep.key == rv.key {
				return fmt.Errorf("resource %q needs itself", rv.key)
			}
		}
	}
	return nil
}

// needKeys renders a resource's dependencies as the keys they were declared
// under.
func needKeys(rv *resourceValue) []string {
	if len(rv.needs) == 0 {
		return nil
	}
	out := make([]string, 0, len(rv.needs))
	for _, dep := range rv.needs {
		out = append(out, dep.key)
	}
	return out
}
