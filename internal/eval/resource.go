package eval

import (
	"fmt"

	"go.starlark.net/starlark"
)

// resourcesLocalKey is where declared resources accumulate. Threads are
// per-evaluation, so this is not shared state.
const resourcesLocalKey = "weft.resources"

// resourceValue is what resource() returns: the body a program declared, the
// resources it depends on, and what that object looks like in the cluster now.
//
// Dependencies are references to the value, not to a name. A program building a
// fan-out already holds the thing each sibling depends on, so making it name
// that thing again as a string is asking for a typo that nothing can catch
// until it produces a wrong teardown order. Holding the value means a
// misspelling is an undefined name, reported by Starlark with a backtrace.
type resourceValue struct {
	ref      Ref
	body     starlark.Value
	observed starlark.Value
	needs    []*resourceValue

	// declared distinguishes resource(body) from resource(body, needs=[]).
	// The first says nothing and keeps the order the program wrote; the second
	// says "nothing at all", which is how siblings built in a loop escape being
	// chained to one another.
	declared bool
}

var (
	_ starlark.Value    = (*resourceValue)(nil)
	_ starlark.HasAttrs = (*resourceValue)(nil)
)

func (r *resourceValue) Type() string         { return "resource" }
func (r *resourceValue) Truth() starlark.Bool { return starlark.True }
func (r *resourceValue) String() string       { return fmt.Sprintf("resource(%s)", r.ref) }

func (r *resourceValue) Freeze() {
	r.body.Freeze()
	if r.observed != nil {
		r.observed.Freeze()
	}
	for _, n := range r.needs {
		n.Freeze()
	}
}

func (r *resourceValue) Hash() (uint32, error) {
	return 0, fmt.Errorf("unhashable type: resource")
}

// Attr exposes what this resource looks like in the cluster right now.
//
// Self-reference through observed is how a composition advances in stages: an
// identity is declared, and the things that consume the principalId its
// provider writes back minutes later are declared only once it is there. It
// hangs off the resource rather than off a separate mapping because it is a
// fact about that resource, and because a program holding the value has no
// business looking it up by name.
func (r *resourceValue) Attr(name string) (starlark.Value, error) {
	if name != "observed" {
		return nil, nil
	}
	if r.observed == nil {
		return starlark.None, nil
	}
	return r.observed, nil
}

func (r *resourceValue) AttrNames() []string { return []string{"observed"} }

// bResource implements resource(body, needs=None).
//
// Declaring is the act. There is nothing to collect and return, because a
// composition is a set of things that should exist and saying one exists is the
// whole statement - and because the alternative made a staged composition
// responsible for handing back what it had built so far, where forgetting meant
// declaring nothing and pruning everything.
func bResource(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var body starlark.Value
	var needs starlark.Value
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "body", &body, "needs?", &needs); err != nil {
		return nil, err
	}

	ref, err := refOf(body)
	if err != nil {
		return nil, &ProgramError{Reason: ReasonInvalidOutput, Msg: err.Error()}
	}

	deps, err := dependencies(needs)
	if err != nil {
		return nil, err
	}

	set, _ := thread.Local(resourcesLocalKey).(*declaredSet)
	if set == nil {
		return nil, fmt.Errorf("resource() is not available in this evaluation")
	}
	if _, dup := set.byRef[ref]; dup {
		return nil, &ProgramError{
			Reason: ReasonInvalidOutput,
			Msg: fmt.Sprintf(
				"%s is declared twice. An object is one thing, so two declarations cannot mean two of them",
				ref),
		}
	}

	rv := &resourceValue{ref: ref, body: body, needs: deps, declared: needs != nil}
	if obj, ok := set.observed[ref]; ok {
		v, err := objectToStarlark(obj)
		if err != nil {
			return nil, fmt.Errorf("%s: reading back what exists: %w", ref, err)
		}
		rv.observed = v
	}

	set.byRef[ref] = rv
	set.order = append(set.order, rv)
	return rv, nil
}

// refOf reads the identity out of a body.
//
// A resource is identified by the object it describes, which is the only
// identity anybody outside this program can see. Renaming what a program
// declares is not an edit to a thing, it is one thing no longer declared and
// another declared in its place.
func refOf(body starlark.Value) (Ref, error) {
	obj, ok := body.(*starlark.Dict)
	if !ok {
		return Ref{}, fmt.Errorf("resource: body must be a mapping, got %s", body.Type())
	}

	str := func(d *starlark.Dict, key string) string {
		v, found, err := d.Get(starlark.String(key))
		if err != nil || !found {
			return ""
		}
		s, _ := starlark.AsString(v)
		return s
	}

	ref := Ref{APIVersion: str(obj, "apiVersion"), Kind: str(obj, "kind")}
	if md, found, err := obj.Get(starlark.String("metadata")); err == nil && found {
		if mdd, ok := md.(*starlark.Dict); ok {
			ref.Name = str(mdd, "name")
		}
	}

	switch {
	case ref.APIVersion == "":
		return Ref{}, fmt.Errorf("resource: missing apiVersion")
	case ref.Kind == "":
		return Ref{}, fmt.Errorf("resource: missing kind")
	case ref.Name == "":
		return Ref{}, fmt.Errorf("resource: missing metadata.name")
	}
	return ref, nil
}

// declaredSet accumulates what a program declared, in the order it declared it.
// Order is meaningful: it is the apply order, and therefore the reverse of the
// teardown order, for everything that does not say otherwise.
type declaredSet struct {
	byRef    map[Ref]*resourceValue
	order    []*resourceValue
	observed map[Ref]map[string]any
}

func newDeclaredSet(observed map[Ref]map[string]any) *declaredSet {
	if observed == nil {
		observed = map[Ref]map[string]any{}
	}
	return &declaredSet{byRef: map[Ref]*resourceValue{}, observed: observed}
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
			if set.byRef[dep.ref] != dep {
				return fmt.Errorf(
					"%s needs a resource that was never declared. Everything passed to needs has to be "+
						"declared too, or there is nothing to order against", rv.ref)
			}
			if dep.ref == rv.ref {
				return fmt.Errorf("%s needs itself", rv.ref)
			}
		}
	}
	return nil
}

// needRefs renders a resource's dependencies as the objects they identify.
func needRefs(rv *resourceValue) []Ref {
	if len(rv.needs) == 0 {
		return nil
	}
	out := make([]Ref, 0, len(rv.needs))
	for _, dep := range rv.needs {
		out = append(out, dep.ref)
	}
	return out
}
