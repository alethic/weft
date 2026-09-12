package eval

import (
	"strings"
	"testing"
)

// Declaring is the act. There is nothing to collect and hand back.
func TestDeclaringIsWhatProducesResources(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    resource("a", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "a"}})
`, Request{})

	if len(res.Resources) != 1 || res.Resources[0].Key != "a" {
		t.Fatalf("got %#v", res)
	}
	if res.Resources[0].NeedsDeclared {
		t.Error("a resource that says nothing declares nothing about its ordering")
	}
}

// Declaration order is apply order, and therefore reverse teardown order.
func TestDeclarationOrderIsKept(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    for n in ["first", "second", "third"]:
        resource(n, {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": n}})
`, Request{})

	var got []string
	for _, r := range res.Resources {
		got = append(got, r.Key)
	}
	if len(got) != 3 || got[0] != "first" || got[2] != "third" {
		t.Errorf("order = %v", got)
	}
}

// A dependency is a reference to the resource, not its key. The program already
// holds the thing, so making it name that thing again as a string is where a
// typo would come from.
func TestNeedsResolvesReferencesToKeys(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    identity = resource("identity", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "i"}})
    for n in ["a", "b"]:
        resource("ra-" + n,
                 {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "ra-" + n}},
                 needs=[identity])
`, Request{})

	byKey := map[string][]string{}
	for _, r := range res.Resources {
		byKey[r.Key] = r.Needs
	}
	if len(byKey["identity"]) != 0 {
		t.Errorf("identity needs %v", byKey["identity"])
	}
	for _, k := range []string{"ra-a", "ra-b"} {
		if len(byKey[k]) != 1 || byKey[k][0] != "identity" {
			t.Errorf("%s needs %v, want [identity]", k, byKey[k])
		}
	}
}

// A single resource is accepted where a list would be, because needing exactly
// one thing is the common case and brackets add nothing.
func TestNeedsAcceptsOneResource(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    base = resource("base", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "b"}})
    resource("on-top", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "t"}}, needs=base)
`, Request{})

	for _, r := range res.Resources {
		if r.Key == "on-top" && (len(r.Needs) != 1 || r.Needs[0] != "base") {
			t.Errorf("needs = %v", r.Needs)
		}
	}
}

// Saying nothing and saying "nothing at all" are different: the first keeps the
// order the program wrote, the second frees the resource from it.
func TestEmptyNeedsIsDeclared(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    resource("a", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "a"}}, needs=[])
    resource("b", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "b"}})
`, Request{})

	for _, r := range res.Resources {
		switch r.Key {
		case "a":
			if !r.NeedsDeclared || len(r.Needs) != 0 {
				t.Errorf("a: declared=%v needs=%v, want an explicit nothing", r.NeedsDeclared, r.Needs)
			}
		case "b":
			if r.NeedsDeclared {
				t.Error("b said nothing about its ordering")
			}
		}
	}
}

// An early return keeps what was declared, which is what makes staging safe.
// The previous shape made a composition responsible for handing back what it
// had built so far, and forgetting meant pruning everything.
func TestAnEarlyReturnKeepsWhatWasDeclared(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    resource("identity", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "i"}})
    if not variable.ready:
        pending("the identity has not reported yet")
        return
    resource("consumer", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "c"}})
`, Request{Variables: map[string]any{"ready": false}})

	if len(res.Resources) != 1 || res.Resources[0].Key != "identity" {
		t.Fatalf("got %#v", res)
	}
	if len(res.Pending) != 1 {
		t.Errorf("pending = %v", res.Pending)
	}
}

// A key names one object, so declaring it twice cannot mean two.
func TestDeclaringOneKeyTwice(t *testing.T) {
	_, err := run(t, `
def compose(variable, observed):
    resource("a", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "one"}})
    resource("a", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "two"}})
`, Request{})

	pe := programError(t, err)
	if !strings.Contains(pe.Msg, "declared twice") {
		t.Errorf("msg = %q", pe.Msg)
	}
}

// Passing a name rather than the value resource() returned is the mistake this
// design exists to prevent, so it says what to pass instead.
func TestNeedsRejectsAnythingElse(t *testing.T) {
	_, err := run(t, `
def compose(variable, observed):
    resource("a", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "a"}},
             needs=["identity"])
`, Request{})

	pe := programError(t, err)
	if !strings.Contains(pe.Msg, "resource() returned") {
		t.Errorf("msg = %q, should say what to pass", pe.Msg)
	}
}

// Returning a mapping is the old shape, and saying so is kinder than ignoring
// it: a composition that still returns its resources would otherwise create
// nothing at all and look like it worked.
func TestReturningAMappingIsRefused(t *testing.T) {
	_, err := run(t, `
def compose(variable, observed):
    return {"a": {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "a"}}}
`, Request{})

	pe := programError(t, err)
	if !strings.Contains(pe.Msg, "nothing to return") {
		t.Errorf("msg = %q", pe.Msg)
	}
}
