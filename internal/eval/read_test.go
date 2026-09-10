package eval

import (
	"errors"
	"strings"
	"testing"
)

func obj(kind, name string, labels map[string]any) map[string]any {
	meta := map[string]any{"name": name}
	if labels != nil {
		meta["labels"] = labels
	}
	return map[string]any{"apiVersion": "v1", "kind": kind, "metadata": meta}
}

// The ordinary read: a resource that is there arrives whole.
func TestReadReturnsTheObject(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    rg = read("azure.m.upbound.io/v1beta1", "ResourceGroup", "sweep-env")
    return {"x": {"apiVersion": "v1", "kind": "ConfigMap",
                  "metadata": {"name": rg.metadata.name}}}
`, Request{Reader: fakeReader{"sweep-env": obj("ResourceGroup", "sweep-env", nil)}})

	if len(res.Resources) != 1 {
		t.Fatalf("got %#v", res)
	}
	meta := res.Resources[0].Object["metadata"].(map[string]any)
	if meta["name"] != "sweep-env" {
		t.Errorf("name = %v", meta["name"])
	}
}

// Absence is falsey, which is what makes the gate read the way it does. This is
// the whole of what a required flag on a declared source used to buy.
func TestReadOfSomethingAbsentIsFalsey(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    if not read("v1", "ConfigMap", "licence"):
        return wait("no licence ConfigMap in this namespace yet")
    return {}
`, Request{Reader: fakeReader{}})

	if !res.Waiting() || res.Wait.Reason != "no licence ConfigMap in this namespace yet" {
		t.Fatalf("got %#v", res)
	}
}

// And the thing a static flag could not express: a gate that depends on
// configuration.
func TestConditionalGateOverARead(t *testing.T) {
	program := `
def compose(variable, observed):
    if variable.useSql and not read("v1", "ConfigMap", "database"):
        return wait("SQL is enabled but the database is not there yet")
    return {"cfg": {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "c"}}}
`
	res := mustRun(t, program, Request{
		Variables: map[string]any{"useSql": true},
		Reader:    fakeReader{},
	})
	if !res.Waiting() {
		t.Fatal("should gate when SQL is enabled and the database is absent")
	}

	res = mustRun(t, program, Request{
		Variables: map[string]any{"useSql": false},
		Reader:    fakeReader{},
	})
	if res.Waiting() || len(res.Resources) != 1 {
		t.Fatalf("should proceed when SQL is off, absent database and all: %#v", res)
	}
}

// get() and has() reach through an absent read without raising, because their
// whole purpose is tolerating what is not there yet.
func TestGetAndHasTolerateAnAbsentRead(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    missing = read("v1", "ConfigMap", "nope")
    return {"x": {"apiVersion": "v1", "kind": "ConfigMap",
                  "metadata": {"name": "x"},
                  "data": {"got": get(missing, "data.key", "fallback"),
                           "has": str(has(missing, "data.key"))}}}
`, Request{Reader: fakeReader{}})

	data := res.Resources[0].Object["data"].(map[string]any)
	if data["got"] != "fallback" {
		t.Errorf("got = %v", data["got"])
	}
	if data["has"] != "False" {
		t.Errorf("has = %v", data["has"])
	}
}

// Reaching into something absent names it. Without a declared source list this
// is the only place a misspelled name gets caught.
func TestAbsentReadNamesItselfOnFieldAccess(t *testing.T) {
	_, err := run(t, `
def compose(variable, observed):
    return {"x": read("v1", "MSSQLDatabase", "tpyo").status.atProvider.id}
`, Request{Reader: fakeReader{}})

	pe := programError(t, err)
	if !strings.Contains(pe.Msg, "tpyo") || !strings.Contains(pe.Msg, "MSSQLDatabase") {
		t.Errorf("msg = %q, should name what came back empty", pe.Msg)
	}
}

// A failure to read is not an absence. It leaves the caller's error intact
// rather than turning a permission denial into a program fault.
func TestReadFailurePropagatesTheCallerError(t *testing.T) {
	boom := errors.New("configmaps is forbidden: User cannot get resource")
	_, err := run(t, `
def compose(variable, observed):
    return {"x": read("v1", "ConfigMap", "denied")}
`, Request{Reader: fakeReader{"denied": boom}})

	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the reader's own error", err)
	}
	var pe *ProgramError
	if errors.As(err, &pe) {
		t.Errorf("a read failure is not a program fault, got %v", pe)
	}
}

func TestReadWithoutAReaderIsRefused(t *testing.T) {
	_, err := run(t, `
def compose(variable, observed):
    return {"x": read("v1", "ConfigMap", "any")}
`, Request{})

	pe := programError(t, err)
	if pe.Reason != ReasonReadNotAllowed {
		t.Errorf("reason = %q", pe.Reason)
	}
}

// select() returns a list, sorted by name so that a composition derived from a
// selection cannot reorder its own output between passes.
func TestSelectReturnsMatchesSortedByName(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    tenants = select("v1", "ConfigMap", labels={"role": "tenant"})
    out = {}
    for i in range(len(tenants)):
        out["t" + str(i)] = {"apiVersion": "v1", "kind": "ConfigMap",
                             "metadata": {"name": tenants[i].metadata.name}}
    return out
`, Request{Reader: fakeReader{
		"beta":    obj("ConfigMap", "beta", map[string]any{"role": "tenant"}),
		"alpha":   obj("ConfigMap", "alpha", map[string]any{"role": "tenant"}),
		"unreled": obj("ConfigMap", "unrelated", map[string]any{"role": "other"}),
	}})

	if len(res.Resources) != 2 {
		t.Fatalf("got %d resources, want the two tenants", len(res.Resources))
	}
	var names []string
	for _, r := range res.Resources {
		names = append(names, r.Object["metadata"].(map[string]any)["name"].(string))
	}
	if names[0] != "alpha" || names[1] != "beta" {
		t.Errorf("names = %v, want them sorted", names)
	}
}

func TestSelectWithNoLabelsMatchesEverythingOfThatKind(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    all = select("v1", "ConfigMap")
    return {"x": {"apiVersion": "v1", "kind": "ConfigMap",
                  "metadata": {"name": "x"}, "data": {"n": str(len(all))}}}
`, Request{Reader: fakeReader{
		"a": obj("ConfigMap", "a", nil),
		"b": obj("ConfigMap", "b", map[string]any{"role": "tenant"}),
	}})

	if got := res.Resources[0].Object["data"].(map[string]any)["n"]; got != "2" {
		t.Errorf("n = %v", got)
	}
}

// The read budget bounds how much of a namespace one pass can pull in. Every
// read is also a watch the controller keeps alive afterwards.
func TestReadBudget(t *testing.T) {
	_, err := run(t, `
def compose(variable, observed):
    for i in range(10):
        read("v1", "ConfigMap", "cm-" + str(i))
    return {}
`, Request{Reader: fakeReader{}}, func(o *Options) { o.MaxReads = 3 })

	pe := programError(t, err)
	if pe.Reason != ReasonBudgetExceeded {
		t.Errorf("reason = %q (%s)", pe.Reason, pe.Msg)
	}
}

// Reading the same resource repeatedly is one read, so a program that reaches
// for the same thing in several branches is not punished for it.
func TestRepeatedReadsCountOnce(t *testing.T) {
	_, err := run(t, `
def compose(variable, observed):
    for i in range(50):
        read("v1", "ConfigMap", "same")
    return {}
`, Request{Reader: fakeReader{"same": obj("ConfigMap", "same", nil)}}, func(o *Options) { o.MaxReads = 2 })

	if err != nil {
		t.Fatalf("one distinct resource should be within any budget: %v", err)
	}
}

func TestSelectSizeLimit(t *testing.T) {
	many := fakeReader{}
	for _, n := range []string{"a", "b", "c", "d"} {
		many[n] = obj("ConfigMap", n, nil)
	}
	_, err := run(t, `
def compose(variable, observed):
    select("v1", "ConfigMap")
    return {}
`, Request{Reader: many}, func(o *Options) { o.MaxSelected = 2 })

	pe := programError(t, err)
	if pe.Reason != ReasonBudgetExceeded {
		t.Errorf("reason = %q (%s)", pe.Reason, pe.Msg)
	}
}
