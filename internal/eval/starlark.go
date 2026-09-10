package eval

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"go.starlark.net/resolve"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

func init() {
	// A composition author is untrusted by assumption, so the language is
	// bounded rather than trusted. Recursion in particular turns a step budget
	// into a stack-depth problem, which is not something a step budget catches.
	resolve.AllowRecursion = false
	// Reassigning globals mid-module makes a program's meaning depend on
	// evaluation order for no benefit here.
	resolve.AllowGlobalReassign = false
	// Sets are pure and occasionally useful for deduplication.
	resolve.AllowSet = true
}

// Options bounds an evaluation.
//
// Untrusted authorship is the premise of the project, so runtime bounding is
// what replaces the apply-time type checking a schema-generating design would
// have given us. Every limit here has a defined failure mode that surfaces as a
// Degraded condition rather than as a wedged controller.
type Options struct {
	// MaxSteps is the Starlark execution budget for one call to compose().
	MaxSteps uint64

	// MaxResources caps how many resources one evaluation may return.
	MaxResources int

	// MaxValues caps the total number of values across the converted result. A
	// program can build an enormous structure in very few steps, and the cost
	// of that structure lands on the API server rather than on us.
	MaxValues int

	// CacheSize is the number of compiled programs to retain. Compilation is
	// the expensive part and programs change rarely, so this is keyed by
	// content hash and shared across every Weave in the cluster.
	CacheSize int

	// Print receives output from the program's print() builtin. Nil discards.
	Print func(msg string)
}

// DefaultOptions are sized so that a composition doing real work is
// comfortable, while a runaway program stops in well under a second.
func DefaultOptions() Options {
	return Options{
		MaxSteps:     20_000_000,
		MaxResources: 250,
		MaxValues:    250_000,
		CacheSize:    128,
	}
}

func (o *Options) applyDefaults() {
	d := DefaultOptions()
	if o.MaxSteps == 0 {
		o.MaxSteps = d.MaxSteps
	}
	if o.MaxResources == 0 {
		o.MaxResources = d.MaxResources
	}
	if o.MaxValues == 0 {
		o.MaxValues = d.MaxValues
	}
	if o.CacheSize == 0 {
		o.CacheSize = d.CacheSize
	}
}

// Starlark is the default Evaluator.
type Starlark struct {
	opts Options

	mu    sync.Mutex
	cache map[string]*list.Element
	lru   *list.List // front is most recently used
}

type cacheEntry struct {
	hash string
	prog *starlark.Program
}

var _ Evaluator = (*Starlark)(nil)

// NewStarlark returns an Evaluator safe for concurrent use.
func NewStarlark(opts Options) *Starlark {
	opts.applyDefaults()
	return &Starlark{
		opts:  opts,
		cache: make(map[string]*list.Element),
		lru:   list.New(),
	}
}

// keyPattern bounds inventory keys. They are written into an annotation value
// and into status, and they are the stable identity of a live object, so they
// are restricted rather than arbitrary.
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)

// Evaluate compiles (or reuses) the program and calls compose().
func (s *Starlark) Evaluate(ctx context.Context, req Request) (*Result, error) {
	prog, err := s.program(req.Program)
	if err != nil {
		return nil, err
	}

	thread := &starlark.Thread{Name: "compose"}
	thread.SetMaxExecutionSteps(s.opts.MaxSteps)
	// Load is left nil; a program that reaches for load() is rejected at
	// compile time, and this is the belt to that suspenders.
	thread.Load = nil
	if s.opts.Print != nil {
		thread.Print = func(_ *starlark.Thread, msg string) { s.opts.Print(msg) }
	} else {
		thread.Print = func(_ *starlark.Thread, _ string) {}
	}

	// Starlark has no notion of a context, so cancellation is bridged. The
	// goroutine is bounded by the deferred close.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			thread.Cancel("context cancelled")
		case <-done:
		}
	}()

	inputsV, err := objectToStarlark(req.Inputs)
	if err != nil {
		return nil, programErrorf(ReasonInvalidArgument, "converting inputs: %v", err)
	}
	sourcesV, err := objectToStarlark(sourcesMap(req.Sources))
	if err != nil {
		return nil, programErrorf(ReasonInvalidArgument, "converting sources: %v", err)
	}
	observedV, err := objectToStarlark(req.Observed)
	if err != nil {
		return nil, programErrorf(ReasonInvalidArgument, "converting observed: %v", err)
	}

	globals, err := prog.Init(thread, builtins())
	if err != nil {
		return s.classify(ctx, thread, err)
	}
	globals.Freeze()

	composeFn, ok := globals["compose"]
	if !ok {
		return nil, programErrorf(ReasonNoComposeFunc,
			"program does not define compose(inputs, sources, observed)")
	}
	if _, callable := composeFn.(starlark.Callable); !callable {
		return nil, programErrorf(ReasonNoComposeFunc,
			"compose must be a function, got %s", composeFn.Type())
	}

	ret, err := starlark.Call(thread, composeFn.(starlark.Callable),
		starlark.Tuple{inputsV, sourcesV, observedV}, nil)
	if err != nil {
		return s.classify(ctx, thread, err)
	}

	return s.decode(ret)
}

// sourcesMap turns the resolved sources into a plain map, mapping an absent
// source to nil so it reaches the program as None.
func sourcesMap(src map[string]any) map[string]any {
	if src == nil {
		return map[string]any{}
	}
	return src
}

// classify turns an evaluation error into the right kind of outcome. The three
// possibilities are genuinely different: a deliberate wait, a program fault,
// and the caller giving up on us.
func (s *Starlark) classify(ctx context.Context, thread *starlark.Thread, err error) (*Result, error) {
	// A wait raised by require() is not a failure. Check this first: it travels
	// on the thread precisely so it survives Starlark's error wrapping.
	if w, ok := thread.Local(waitLocalKey).(*Wait); ok && w != nil {
		return &Result{Wait: w}, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	// Comparing against the budget avoids matching on the cancellation message.
	if thread.ExecutionSteps() >= s.opts.MaxSteps {
		return nil, programErrorf(ReasonBudgetExceeded,
			"program exceeded its budget of %d execution steps", s.opts.MaxSteps)
	}

	pe := &ProgramError{Reason: ReasonRuntimeError, Msg: err.Error()}
	var evalErr *starlark.EvalError
	if errorsAs(err, &evalErr) {
		pe.Msg = evalErr.Msg
		pe.Backtrace = evalErr.Backtrace()
	}
	return nil, pe
}

// decode validates and converts the value compose() returned.
func (s *Starlark) decode(ret starlark.Value) (*Result, error) {
	if w, ok := ret.(*waitValue); ok {
		return &Result{Wait: &Wait{Reason: w.reason}}, nil
	}

	keys, vals, err := orderedItems(ret)
	if err != nil {
		return nil, programErrorf(ReasonInvalidOutput, "%v", err)
	}
	if len(keys) > s.opts.MaxResources {
		return nil, programErrorf(ReasonOutputTooLarge,
			"program returned %d resources, the limit is %d", len(keys), s.opts.MaxResources)
	}

	b := &budget{maxNodes: s.opts.MaxValues}
	out := make([]Resource, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for i, k := range keys {
		if !keyPattern.MatchString(k) || len(k) > 253 {
			return nil, programErrorf(ReasonInvalidOutput,
				"resource key %q is not usable as an identity: keys must be alphanumeric with -, _ or . inside, at most 253 characters", k)
		}
		if seen[k] {
			return nil, programErrorf(ReasonInvalidOutput, "resource key %q returned twice", k)
		}
		seen[k] = true

		obj, err := asStringMap(vals[i], b)
		if err != nil {
			if strings.Contains(err.Error(), "exceeds") {
				return nil, programErrorf(ReasonOutputTooLarge, "resource %q: %v", k, err)
			}
			return nil, programErrorf(ReasonInvalidOutput, "resource %q: %v", k, err)
		}
		if err := checkShape(obj); err != nil {
			return nil, programErrorf(ReasonInvalidOutput, "resource %q: %v", k, err)
		}
		out = append(out, Resource{Key: k, Object: obj})
	}
	return &Result{Resources: out}, nil
}

// orderedItems extracts key/value pairs in their original order. Order is the
// whole point: it becomes apply order, and therefore teardown order.
func orderedItems(v starlark.Value) ([]string, []starlark.Value, error) {
	switch t := v.(type) {
	case *starlark.Dict:
		items := t.Items()
		keys := make([]string, 0, len(items))
		vals := make([]starlark.Value, 0, len(items))
		for _, kv := range items {
			k, ok := starlark.AsString(kv[0])
			if !ok {
				return nil, nil, fmt.Errorf("resource keys must be strings, got %s", kv[0].Type())
			}
			keys = append(keys, k)
			vals = append(vals, kv[1])
		}
		return keys, vals, nil
	case *Object:
		vals := make([]starlark.Value, 0, len(t.keys))
		for _, k := range t.keys {
			vals = append(vals, t.m[k])
		}
		return t.keys, vals, nil
	case starlark.NoneType:
		return nil, nil, fmt.Errorf(
			"compose() returned None; return an empty mapping to mean \"no resources\", or wait(reason) to mean \"not yet\"")
	default:
		return nil, nil, fmt.Errorf("compose() must return a mapping of key to resource, got %s", v.Type())
	}
}

// checkShape enforces the minimum a returned value must have to be a resource
// at all. Deeper validation belongs to the API server; this catches the class
// of mistake that would otherwise apply cleanly as something unintended.
func checkShape(obj map[string]any) error {
	apiVersion, _ := obj["apiVersion"].(string)
	if apiVersion == "" {
		return fmt.Errorf("missing apiVersion")
	}
	kind, _ := obj["kind"].(string)
	if kind == "" {
		return fmt.Errorf("missing kind")
	}
	meta, ok := obj["metadata"].(map[string]any)
	if !ok {
		return fmt.Errorf("missing metadata")
	}
	name, _ := meta["name"].(string)
	if name == "" {
		if gn, _ := meta["generateName"].(string); gn != "" {
			return fmt.Errorf("metadata.generateName is not usable here: a generated name would change on every apply, so a resource must have a stable metadata.name")
		}
		return fmt.Errorf("missing metadata.name")
	}
	return nil
}

// program returns a compiled program, compiling and caching on first use.
func (s *Starlark) program(src string) (*starlark.Program, error) {
	sum := sha256.Sum256([]byte(src))
	hash := hex.EncodeToString(sum[:])

	s.mu.Lock()
	if el, ok := s.cache[hash]; ok {
		s.lru.MoveToFront(el)
		prog := el.Value.(*cacheEntry).prog
		s.mu.Unlock()
		return prog, nil
	}
	s.mu.Unlock()

	prog, err := compile(src)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.cache[hash]; ok {
		s.lru.MoveToFront(el)
		return el.Value.(*cacheEntry).prog, nil
	}
	el := s.lru.PushFront(&cacheEntry{hash: hash, prog: prog})
	s.cache[hash] = el
	for s.lru.Len() > s.opts.CacheSize {
		oldest := s.lru.Back()
		s.lru.Remove(oldest)
		delete(s.cache, oldest.Value.(*cacheEntry).hash)
	}
	return prog, nil
}

func compile(src string) (*starlark.Program, error) {
	f, err := syntax.Parse("compose.star", []byte(src), 0)
	if err != nil {
		return nil, &ProgramError{Reason: ReasonSyntaxError, Msg: err.Error()}
	}
	// Rejecting load() at compile time gives a clear message instead of the
	// interpreter's generic complaint about a nil loader, and makes the rule
	// visible where an author will read it.
	for _, stmt := range f.Stmts {
		if ls, ok := stmt.(*syntax.LoadStmt); ok {
			return nil, &ProgramError{
				Reason: ReasonLoadNotAllowed,
				Msg: fmt.Sprintf("load(%q, ...) at line %d: a program must be self-contained",
					ls.Module.Value, ls.Load.Line),
			}
		}
	}
	prog, err := starlark.FileProgram(f, func(name string) bool { return predeclaredNames[name] })
	if err != nil {
		return nil, &ProgramError{Reason: ReasonSyntaxError, Msg: err.Error()}
	}
	return prog, nil
}

// errorsAs is errors.As, kept local so the import set of this file stays
// obvious about what it depends on.
func errorsAs(err error, target **starlark.EvalError) bool {
	for err != nil {
		if e, ok := err.(*starlark.EvalError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
