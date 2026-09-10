package eval

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.starlark.net/starlark"
)

// readerLocalKey is where the per-evaluation read session lives. Threads are
// per-evaluation, so this is not shared state.
const readerLocalKey = "weft.reader"

// readSession carries what read() needs and enforces the per-evaluation budget.
type readSession struct {
	ctx         context.Context
	reader      Reader
	max         int
	maxSelected int
	seen        map[string]bool
}

// errReadFailed carries a caller-side read failure back out through Starlark's
// error wrapping so the caller gets its own error rather than a program fault.
// A permission denial is a fact about the cluster, not a bug in the program.
type errReadFailed struct{ err error }

func (e *errReadFailed) Error() string { return e.err.Error() }
func (e *errReadFailed) Unwrap() error { return e.err }

// bRead implements read(apiVersion, kind, name, finalize=False).
//
// This is the whole of a composition's access to the cluster. It replaces a
// declared source list, and the reason it can is that the caller records every
// call: the recording is what the watch registration and the finalizer
// bookkeeping key off, so nothing is lost by not declaring it up front.
//
// Absence is a value. A resource that does not exist is None, and whether that
// should stop the composition is a decision only the program can make:
//
//	db = read("sql.azure.m.upbound.io/v1beta1", "MSSQLDatabase", "app")
//	if not db:
//	    return wait("the database has not been created yet")
func bRead(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var apiVersion, kind, name string
	var finalize bool
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"apiVersion", &apiVersion,
		"kind", &kind,
		"name", &name,
		"finalize?", &finalize,
	); err != nil {
		return nil, err
	}

	for label, v := range map[string]string{"apiVersion": apiVersion, "kind": kind, "name": name} {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("read: %s must not be empty", label)
		}
	}

	s, _ := thread.Local(readerLocalKey).(*readSession)
	if s == nil || s.reader == nil {
		return nil, &ProgramError{
			Reason: ReasonReadNotAllowed,
			Msg:    "read() is not available in this evaluation",
		}
	}

	// The budget counts distinct resources rather than calls, so a program that
	// reads the same thing in a loop is not punished for it while one that
	// walks a generated list of names still is.
	id := apiVersion + "/" + kind + "/" + name
	if !s.seen[id] {
		if s.max > 0 && len(s.seen) >= s.max {
			return nil, &ProgramError{
				Reason: ReasonBudgetExceeded,
				Msg:    fmt.Sprintf("read: this program reads more than %d distinct resources", s.max),
			}
		}
		s.seen[id] = true
	}

	obj, err := s.reader.Read(s.ctx, apiVersion, kind, name, finalize)
	if err != nil {
		return nil, &errReadFailed{err: err}
	}
	if obj == nil {
		return &absentValue{apiVersion: apiVersion, kind: kind, name: name}, nil
	}
	return objectToStarlark(obj)
}

// absentValue is what a read of something that does not exist returns.
//
// It is falsey, so the ordinary gate reads the way it should:
//
//	if not read("v1", "ConfigMap", "licence"):
//	    return wait("no licence yet")
//
// The reason it is not simply None is the typo. A declared source list caught a
// misspelled id at resolution time; without one, the only signal is a field
// access against nothing, and "NoneType has no .metadata field" does not say
// which of four reads came back empty. This does.
type absentValue struct {
	apiVersion string
	kind       string
	name       string
}

var (
	_ starlark.Value    = (*absentValue)(nil)
	_ starlark.HasAttrs = (*absentValue)(nil)
)

func (a *absentValue) Type() string          { return "absent" }
func (a *absentValue) Freeze()               {}
func (a *absentValue) Truth() starlark.Bool  { return starlark.False }
func (a *absentValue) Hash() (uint32, error) { return 0, nil }

func (a *absentValue) String() string {
	return fmt.Sprintf("absent(%s %s/%s)", a.kind, a.apiVersion, a.name)
}

func (a *absentValue) Attr(name string) (starlark.Value, error) {
	return nil, fmt.Errorf(
		"%s %q does not exist in this namespace, so it has no .%s: "+
			"guard the read before reaching into it, with `if not ...: return wait(...)`",
		a.kind, a.name, name)
}

func (a *absentValue) AttrNames() []string { return nil }

// bSelect implements select(apiVersion, kind, labels={}).
//
// A separate builtin rather than a mode of read(), because it returns a list
// where read() returns an object: a program should not have to look at which
// keyword was passed to know the shape of what came back.
//
// The result is sorted by name. A composition that derives resources from a
// selection would otherwise reorder its own output whenever the API server
// returned the same objects in a different order, and apply order is
// meaningful here.
func bSelect(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var apiVersion, kind string
	var labels starlark.Value
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"apiVersion", &apiVersion,
		"kind", &kind,
		"labels?", &labels,
	); err != nil {
		return nil, err
	}
	if strings.TrimSpace(apiVersion) == "" || strings.TrimSpace(kind) == "" {
		return nil, fmt.Errorf("select: apiVersion and kind must not be empty")
	}

	sel, err := labelMap(labels)
	if err != nil {
		return nil, err
	}

	s, _ := thread.Local(readerLocalKey).(*readSession)
	if s == nil || s.reader == nil {
		return nil, &ProgramError{
			Reason: ReasonReadNotAllowed,
			Msg:    "select() is not available in this evaluation",
		}
	}

	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	id := apiVersion + "/" + kind + "?" + strings.Join(keys, ",")
	if !s.seen[id] {
		if s.max > 0 && len(s.seen) >= s.max {
			return nil, &ProgramError{
				Reason: ReasonBudgetExceeded,
				Msg:    fmt.Sprintf("select: this program reads more than %d distinct resources", s.max),
			}
		}
		s.seen[id] = true
	}

	objs, err := s.reader.Select(s.ctx, apiVersion, kind, sel)
	if err != nil {
		return nil, &errReadFailed{err: err}
	}
	if s.maxSelected > 0 && len(objs) > s.maxSelected {
		return nil, &ProgramError{
			Reason: ReasonBudgetExceeded,
			Msg: fmt.Sprintf("select: %s %s matched %d objects, more than the limit of %d",
				apiVersion, kind, len(objs), s.maxSelected),
		}
	}

	sort.Slice(objs, func(i, j int) bool { return objectName(objs[i]) < objectName(objs[j]) })

	out := make([]starlark.Value, 0, len(objs))
	for _, o := range objs {
		v, err := objectToStarlark(o)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return starlark.NewList(out), nil
}

// labelMap converts the labels argument, which is a plain mapping of equality
// matches. Set-based selectors are deliberately absent: they are a string
// syntax with its own parser and error modes, and a composition that needs one
// can select on what it has and filter in the body.
func labelMap(v starlark.Value) (map[string]string, error) {
	if v == nil || v == starlark.None {
		return nil, nil
	}
	d, ok := v.(*starlark.Dict)
	if !ok {
		return nil, fmt.Errorf("select: labels must be a mapping, got %s", v.Type())
	}
	out := make(map[string]string, d.Len())
	for _, item := range d.Items() {
		k, kok := starlark.AsString(item[0])
		val, vok := starlark.AsString(item[1])
		if !kok || !vok {
			return nil, fmt.Errorf("select: labels must map strings to strings")
		}
		out[k] = val
	}
	return out, nil
}

func objectName(o map[string]any) string {
	meta, _ := o["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	return name
}
