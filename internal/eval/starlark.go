package eval

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// fileOptions bounds the language, per compilation.
//
// The equivalent resolve.Allow* variables are process-global, so setting them
// would reach every other user of Starlark in the binary and be reachable by
// them in turn. A guarantee that another library can switch off is not a
// guarantee, and untrusted authorship is the premise here.
//
// Zero values are the restrictive ones. Recursion in particular is named
// backwards: it disables the recursion *check*, and leaving it false is what
// keeps a program from turning a step budget into a stack-depth problem, which
// a step budget does not catch.
var fileOptions = &syntax.FileOptions{
	// Sets are pure and occasionally useful for deduplication.
	Set: true,
	// while and top-level control flow add nothing a comprehension or a
	// function cannot express, and both make a program harder to bound.
	While:           false,
	TopLevelControl: false,
	// Reassigning globals mid-module makes a program's meaning depend on
	// evaluation order, for no benefit.
	GlobalReassign: false,
	Recursion:      false,
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

	// MaxReads caps how many distinct resources one evaluation may read. A
	// program that walks a generated list of names would otherwise turn one
	// reconcile into an unbounded number of API calls, and every read is also a
	// watch the controller has to keep alive afterwards.
	MaxReads int

	// MaxSelected caps how many objects one select() may match. A namespace
	// with thousands of ConfigMaps should produce a legible error rather than
	// an evaluation that converts all of them.
	MaxSelected int

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
		MaxReads:     100,
		MaxSelected:  500,
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
	if o.MaxReads == 0 {
		o.MaxReads = d.MaxReads
	}
	if o.MaxSelected == 0 {
		o.MaxSelected = d.MaxSelected
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

	// The read session lives on the thread for the same reason wait and pending
	// do: threads are per-evaluation, so nothing here is shared state.
	declared := newDeclaredSet()
	thread.SetLocal(resourcesLocalKey, declared)

	thread.SetLocal(readerLocalKey, &readSession{
		ctx:         ctx,
		reader:      req.Reader,
		max:         s.opts.MaxReads,
		maxSelected: s.opts.MaxSelected,
		seen:        map[string]bool{},
	})

	variablesV, err := objectToStarlark(req.Variables)
	if err != nil {
		return nil, programErrorf(ReasonInvalidArgument, "converting variables: %v", err)
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
			"program does not define compose(variable, observed)")
	}
	callable, isCallable := composeFn.(starlark.Callable)
	if !isCallable {
		return nil, programErrorf(ReasonNoComposeFunc,
			"compose must be a function, got %s", composeFn.Type())
	}

	ret, err := starlark.Call(thread, callable,
		starlark.Tuple{variablesV, observedV}, nil)
	if err != nil {
		return s.classify(ctx, thread, err)
	}

	res, err := s.decode(ret, declared)
	if err != nil {
		return nil, err
	}
	if reasons, ok := thread.Local(pendingLocalKey).([]string); ok {
		res.Pending = reasons
	}
	return res, nil
}

// classify turns an evaluation error into the right kind of outcome. The three
// possibilities are genuinely different: a deliberate wait, a program fault,
// and the caller giving up on us.
func (s *Starlark) classify(ctx context.Context, thread *starlark.Thread, err error) (*Result, error) {
	// A read that failed is the caller's error, not the program's. A permission
	// denial or an uninstalled kind is a fact about the cluster, and the caller
	// renders it far better than a Starlark backtrace would.
	var readErr *errReadFailed
	if errors.As(err, &readErr) {
		return nil, readErr.err
	}

	// A ProgramError raised inside a builtin already knows what it is.
	var raised *ProgramError
	if errors.As(err, &raised) {
		return nil, raised
	}

	// A wait raised by require() is not a failure. It travels on the thread
	// precisely so it survives Starlark's error wrapping.
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
	if errors.As(err, &evalErr) {
		pe.Msg = evalErr.Msg
		pe.Backtrace = evalErr.Backtrace()
	}
	return nil, pe
}

// decode validates and converts the value compose() returned.
func (s *Starlark) decode(ret starlark.Value, set *declaredSet) (*Result, error) {
	if w, ok := ret.(*waitValue); ok {
		return &Result{Wait: &Wait{Reason: w.reason}}, nil
	}
	if ret != starlark.None {
		return nil, programErrorf(ReasonInvalidOutput,
			"compose returned %s. Declaring resources is what produces them, so there is nothing to "+
				"return except wait(...)", ret.Type())
	}

	if err := checkNeeds(set); err != nil {
		return nil, programErrorf(ReasonInvalidOutput, "%v", err)
	}
	if len(set.order) > s.opts.MaxResources {
		return nil, programErrorf(ReasonOutputTooLarge,
			"program declared %d resources, the limit is %d", len(set.order), s.opts.MaxResources)
	}

	b := &budget{maxNodes: s.opts.MaxValues}
	out := make([]Resource, 0, len(set.order))
	for _, rv := range set.order {
		if !keyPattern.MatchString(rv.key) || len(rv.key) > 253 {
			return nil, programErrorf(ReasonInvalidOutput,
				"resource key %q is not usable as an identity: keys must be alphanumeric with -, _ or . inside, at most 253 characters", rv.key)
		}

		obj, err := asStringMap(rv.body, b)
		if err != nil {
			if strings.Contains(err.Error(), "exceeds") {
				return nil, programErrorf(ReasonOutputTooLarge, "resource %q: %v", rv.key, err)
			}
			return nil, programErrorf(ReasonInvalidOutput, "resource %q: %v", rv.key, err)
		}
		if err := checkShape(obj); err != nil {
			return nil, programErrorf(ReasonInvalidOutput, "resource %q: %v", rv.key, err)
		}
		out = append(out, Resource{
			Key: rv.key, Object: obj,
			Needs: needKeys(rv), NeedsDeclared: rv.declared,
		})
	}
	return &Result{Resources: out}, nil
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
		// A checked assertion, so an impossible value is a cache miss and a
		// recompile rather than a panic that takes down the reconcile.
		if entry, ok := el.Value.(*cacheEntry); ok {
			s.lru.MoveToFront(el)
			prog := entry.prog
			s.mu.Unlock()
			return prog, nil
		}
	}
	s.mu.Unlock()

	prog, err := compile(src)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.cache[hash]; ok {
		if entry, ok := el.Value.(*cacheEntry); ok {
			s.lru.MoveToFront(el)
			return entry.prog, nil
		}
	}
	el := s.lru.PushFront(&cacheEntry{hash: hash, prog: prog})
	s.cache[hash] = el
	for s.lru.Len() > s.opts.CacheSize {
		oldest := s.lru.Back()
		s.lru.Remove(oldest)
		if evicted, ok := oldest.Value.(*cacheEntry); ok {
			delete(s.cache, evicted.hash)
		}
	}
	return prog, nil
}

func compile(src string) (*starlark.Program, error) {
	f, err := fileOptions.Parse("compose.star", []byte(src), 0)
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
