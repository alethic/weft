package eval

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func run(t *testing.T, program string, req Request, tweak ...func(*Options)) (*Result, error) {
	t.Helper()
	req.Program = program
	opts := Options{}
	for _, f := range tweak {
		f(&opts)
	}
	return NewStarlark(opts).Evaluate(context.Background(), req)
}

func mustRun(t *testing.T, program string, req Request) *Result {
	t.Helper()
	res, err := run(t, program, req)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return res
}

func programError(t *testing.T, err error) *ProgramError {
	t.Helper()
	var pe *ProgramError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a ProgramError, got %T: %v", err, err)
	}
	return pe
}

func TestReturnsResource(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    resource("cfg", {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": variable.name},
        "data": {"hello": "world"},
        })
`, Request{Variables: map[string]any{"name": "greeting"}})

	if len(res.Resources) != 1 {
		t.Fatalf("got %d resources, want 1", len(res.Resources))
	}
	r := res.Resources[0]
	if r.Key != "cfg" {
		t.Errorf("key = %q, want cfg", r.Key)
	}
	meta := r.Object["metadata"].(map[string]any)
	if meta["name"] != "greeting" {
		t.Errorf("name = %v, want greeting", meta["name"])
	}
}

// Return order is the apply order and therefore the reverse teardown order, so
// it has to survive evaluation exactly as written.
func TestReturnOrderIsPreserved(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    for n in ["zulu", "alpha", "mike", "bravo"]:
        resource(n, {
            "apiVersion": "v1",
            "kind": "ConfigMap",
            "metadata": {"name": n},
        })
    return
`, Request{})

	want := []string{"zulu", "alpha", "mike", "bravo"}
	if len(res.Resources) != len(want) {
		t.Fatalf("got %d resources, want %d", len(res.Resources), len(want))
	}
	for i, w := range want {
		if res.Resources[i].Key != w {
			t.Errorf("position %d = %q, want %q", i, res.Resources[i].Key, w)
		}
	}
}

func TestWaitSentinel(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    return wait("the tenant ConfigMap has not been created yet")
`, Request{})

	if !res.Waiting() {
		t.Fatal("expected a wait")
	}
	if res.Wait.Reason != "the tenant ConfigMap has not been created yet" {
		t.Errorf("reason = %q", res.Wait.Reason)
	}
	if res.Resources != nil {
		t.Error("a waiting result must not carry resources")
	}
}

// A missing status field is the ordinary case, not an error, and the reason has
// to name the specific field so the condition is actionable.
func TestRequireNamesTheUnresolvedField(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    rgid = require(read("v1", "Thing", "resourceGroup"), "status.atProvider.id")
    return
`, Request{
		Reader: fakeReader{
			"resourceGroup": map[string]any{
				"apiVersion": "azure.m.upbound.io/v1beta1",
				"kind":       "ResourceGroup",
				"metadata":   map[string]any{"name": "sweep-env"},
				"status":     map[string]any{"atProvider": map[string]any{}},
			},
		},
	})

	if !res.Waiting() {
		t.Fatal("expected a wait")
	}
	want := "status.atProvider.id is not set on ResourceGroup/sweep-env"
	if res.Wait.Reason != want {
		t.Errorf("reason = %q, want %q", res.Wait.Reason, want)
	}
}

func TestRequireOnAbsentSource(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    resource("x", require(read("v1", "Thing", "resourceGroup"), "status.atProvider.id"))
`, Request{Reader: fakeReader{"resourceGroup": nil}})

	if !res.Waiting() {
		t.Fatal("expected a wait")
	}
	if !strings.Contains(res.Wait.Reason, "does not exist yet") {
		t.Errorf("reason = %q", res.Wait.Reason)
	}
}

// Provider status fields are routinely present-but-empty between the apply and
// the write-back, which is not resolved.
func TestRequireTreatsEmptyStringAsUnresolved(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    resource("x", require(read("v1", "Thing", "rg"), "status.atProvider.id"))
`, Request{
		Reader: fakeReader{
			"rg": map[string]any{
				"apiVersion": "v1", "kind": "ResourceGroup",
				"metadata": map[string]any{"name": "rg"},
				"status":   map[string]any{"atProvider": map[string]any{"id": ""}},
			},
		},
	})

	if !res.Waiting() {
		t.Fatalf("expected a wait, got %#v", res)
	}
	if !strings.Contains(res.Wait.Reason, "is empty") {
		t.Errorf("reason = %q", res.Wait.Reason)
	}
}

func TestRequireNameOverride(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    resource("x", require(observed.get("app-identity"), "status.atProvider.principalId", name = "the app identity"))
`, Request{})

	if !res.Waiting() {
		t.Fatal("expected a wait")
	}
	if !strings.Contains(res.Wait.Reason, "the app identity") {
		t.Errorf("reason = %q", res.Wait.Reason)
	}
}

func TestGetWithDefault(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    resource("cfg", {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": "c"},
        "data": {
            "present": get(read("v1", "Thing", "rg"), "status.atProvider.id", "missing"),
            "absent": get(read("v1", "Thing", "rg"), "status.atProvider.nope", "fallback"),
            "throughNone": get(read("v1", "Thing", "nothing"), "a.b.c", "fallback"),
        },
        })
`, Request{
		Reader: fakeReader{
			"rg": map[string]any{
				"apiVersion": "v1", "kind": "ResourceGroup",
				"metadata": map[string]any{"name": "rg"},
				"status":   map[string]any{"atProvider": map[string]any{"id": "/subscriptions/x/rg"}},
			},
			"nothing": nil,
		},
	})

	data := res.Resources[0].Object["data"].(map[string]any)
	if data["present"] != "/subscriptions/x/rg" {
		t.Errorf("present = %v", data["present"])
	}
	if data["absent"] != "fallback" {
		t.Errorf("absent = %v", data["absent"])
	}
	if data["throughNone"] != "fallback" {
		t.Errorf("throughNone = %v", data["throughNone"])
	}
}

// The external-name annotation is a map index with no schema, written back
// asynchronously. It is one of the handful of fields these compositions read,
// so reaching it must not require ceremony.
func TestBracketPathReachesAnnotation(t *testing.T) {
	req := Request{
		Reader: fakeReader{
			"storage": map[string]any{
				"apiVersion": "azure.m.upbound.io/v1beta1",
				"kind":       "Account",
				"metadata": map[string]any{
					"name": "sweepstorage",
					"annotations": map[string]any{
						"crossplane.io/external-name": "sweepstorage",
					},
				},
			},
		},
	}

	res := mustRun(t, `
def compose(variable, observed):
    name = require(read("v1", "Thing", "storage"), 'metadata.annotations["crossplane.io/external-name"]')
    resource("cfg", {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": "c"},
        "data": {"account": name},
        })
`, req)

	data := res.Resources[0].Object["data"].(map[string]any)
	if data["account"] != "sweepstorage" {
		t.Errorf("account = %v", data["account"])
	}
}

// A missing external-name is Waiting, the same as a missing status field, not a
// hard error: the provider writes it back asynchronously.
func TestMissingExternalNameWaits(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    resource("x", require(read("v1", "Thing", "storage"), 'metadata.annotations["crossplane.io/external-name"]'))
`, Request{
		Reader: fakeReader{
			"storage": map[string]any{
				"apiVersion": "azure.m.upbound.io/v1beta1", "kind": "Account",
				"metadata": map[string]any{"name": "s", "annotations": map[string]any{}},
			},
		},
	})

	if !res.Waiting() {
		t.Fatalf("expected a wait, got %#v", res)
	}
	if !strings.Contains(res.Wait.Reason, "external-name") {
		t.Errorf("reason = %q, should name the annotation", res.Wait.Reason)
	}
}

// Serialising a structure into a string field is the most common thing these
// compositions do, so it gets a first-class test.
func TestNestedDocumentAsString(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    values = {
        "image": {"repository": variable.image, "tag": "latest"},
        "env": [
            {"name": "SB", "value": variable.serviceBus},
            {"name": "MODE", "value": "prod"},
        ],
    }
    resource("values", {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": "release-values"},
        "data": {"values.yaml": to_yaml(values)},
        })
`, Request{Variables: map[string]any{"image": "ghcr.io/x/y", "serviceBus": "sb://host"}})

	data := res.Resources[0].Object["data"].(map[string]any)
	got := data["values.yaml"].(string)
	want := "image:\n" +
		"  repository: ghcr.io/x/y\n" +
		"  tag: latest\n" +
		"env:\n" +
		"  - name: SB\n" +
		"    value: sb://host\n" +
		"  - name: MODE\n" +
		"    value: prod\n"
	if got != want {
		t.Errorf("to_yaml produced:\n%s\nwant:\n%s", got, want)
	}
}

func TestToYAMLUsesBlockScalarForMultilineStrings(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    script = "CREATE USER [x] FROM EXTERNAL PROVIDER;\nGO\n"
    resource("cfg", {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": "c"},
        "data": {"doc": to_yaml({"script": script})},
        })
`, Request{})

	doc := res.Resources[0].Object["data"].(map[string]any)["doc"].(string)
	if !strings.Contains(doc, "script: |") {
		t.Errorf("multi-line string should use a block scalar, got:\n%s", doc)
	}
}

// Fan-out over a list in variables replaces build-time templating that produced N
// copies of a whole CronJob.
func TestFanOutOverVariables(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    for role in variable.roles:
        key = "ra-" + role.name
        resource(key, {
            "apiVersion": "authorization.azure.m.upbound.io/v1beta1",
            "kind": "RoleAssignment",
            "metadata": {"name": key},
            "spec": {"forProvider": {"roleDefinitionName": role.role, "scope": variable.scope}},
        })
    return
`, Request{Variables: map[string]any{
		"scope": "/subscriptions/x",
		"roles": []any{
			map[string]any{"name": "blob", "role": "Storage Blob Data Owner"},
			map[string]any{"name": "queue", "role": "Storage Queue Data Contributor"},
			map[string]any{"name": "sb", "role": "Azure Service Bus Data Owner"},
		},
	}})

	if len(res.Resources) != 3 {
		t.Fatalf("got %d resources, want 3", len(res.Resources))
	}
	if res.Resources[0].Key != "ra-blob" || res.Resources[2].Key != "ra-sb" {
		t.Errorf("unexpected keys: %v, %v", res.Resources[0].Key, res.Resources[2].Key)
	}
}

// Self-reference through observed is how multi-phase advancement works: emit
// the identity, and only emit what consumes it once its status has populated.
func TestSelfReferenceStaging(t *testing.T) {
	program := `
def compose(variable, observed):
    resource("identity", {
        "apiVersion": "managedidentity.azure.m.upbound.io/v1beta1",
        "kind": "UserAssignedIdentity",
        "metadata": {"name": "app"},
    })
    pid = get(observed, ["identity", "status", "atProvider", "principalId"])
    if pid:
        resource("assignment", {
            "apiVersion": "authorization.azure.m.upbound.io/v1beta1",
            "kind": "RoleAssignment",
            "metadata": {"name": "app-sb"},
            "spec": {"forProvider": {"principalId": pid}},
        })
    return
`

	// First pass: nothing observed, so only the identity is emitted.
	first := mustRun(t, program, Request{})
	if len(first.Resources) != 1 || first.Resources[0].Key != "identity" {
		t.Fatalf("first pass returned %d resources", len(first.Resources))
	}

	// Second pass: the identity's status has populated.
	second := mustRun(t, program, Request{Observed: map[string]any{
		"identity": map[string]any{
			"apiVersion": "managedidentity.azure.m.upbound.io/v1beta1",
			"kind":       "UserAssignedIdentity",
			"metadata":   map[string]any{"name": "app"},
			"status":     map[string]any{"atProvider": map[string]any{"principalId": "0000-1111"}},
		},
	}})
	if len(second.Resources) != 2 {
		t.Fatalf("second pass returned %d resources, want 2", len(second.Resources))
	}
	if second.Resources[0].Key != "identity" || second.Resources[1].Key != "assignment" {
		t.Errorf("dependency order lost: %q then %q", second.Resources[0].Key, second.Resources[1].Key)
	}
	spec := second.Resources[1].Object["spec"].(map[string]any)["forProvider"].(map[string]any)
	if spec["principalId"] != "0000-1111" {
		t.Errorf("principalId = %v", spec["principalId"])
	}
}

func TestFailIsAProgramError(t *testing.T) {
	_, err := run(t, `
def compose(variable, observed):
    fail("the role table is empty")
`, Request{})

	pe := programError(t, err)
	if pe.Reason != ReasonRuntimeError {
		t.Errorf("reason = %q, want %q", pe.Reason, ReasonRuntimeError)
	}
	if !strings.Contains(pe.Msg, "the role table is empty") {
		t.Errorf("msg = %q", pe.Msg)
	}
	if pe.Backtrace == "" {
		t.Error("a runtime failure should carry a backtrace")
	}
}

func TestLoadIsRejected(t *testing.T) {
	_, err := run(t, `
load("helpers.star", "helper")

def compose(variable, observed):
    return
`, Request{})

	pe := programError(t, err)
	if pe.Reason != ReasonLoadNotAllowed {
		t.Errorf("reason = %q, want %q", pe.Reason, ReasonLoadNotAllowed)
	}
}

func TestStepBudget(t *testing.T) {
	_, err := NewStarlark(Options{MaxSteps: 10_000}).Evaluate(context.Background(), Request{Program: `
def compose(variable, observed):
    total = 0
    for i in range(1000000):
        total += i
    return
`})

	pe := programError(t, err)
	if pe.Reason != ReasonBudgetExceeded {
		t.Errorf("reason = %q, want %q", pe.Reason, ReasonBudgetExceeded)
	}
}

func TestResourceCountLimit(t *testing.T) {
	_, err := NewStarlark(Options{MaxResources: 3}).Evaluate(context.Background(), Request{Program: `
def compose(variable, observed):
    for i in range(10):
        resource("cfg" + str(i), {
            "apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "c" + str(i)},
        })
    return
`})

	pe := programError(t, err)
	if pe.Reason != ReasonOutputTooLarge {
		t.Errorf("reason = %q, want %q", pe.Reason, ReasonOutputTooLarge)
	}
}

func TestRecursionIsRejected(t *testing.T) {
	_, err := run(t, `
def countdown(n):
    if n <= 0:
        return 0
    return countdown(n - 1)

def compose(variable, observed):
    countdown(5)
    return
`, Request{})

	if err == nil {
		t.Fatal("recursion should be rejected")
	}
	programError(t, err)
}

func TestInvalidOutputs(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		reason  string
		message string
	}{
		{
			name:    "returns a list",
			body:    `    return [1, 2]`,
			reason:  ReasonInvalidOutput,
			message: "nothing to return",
		},
		{
			name:   "resource is not a mapping",
			body:   `    resource("a", "not a resource")`,
			reason: ReasonInvalidOutput,
		},
		{
			name:    "missing apiVersion",
			body:    `    resource("a", {"kind": "ConfigMap", "metadata": {"name": "x"}})`,
			reason:  ReasonInvalidOutput,
			message: "missing apiVersion",
		},
		{
			name:    "missing name",
			body:    `    resource("a", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {}})`,
			reason:  ReasonInvalidOutput,
			message: "missing metadata.name",
		},
		{
			name:    "generateName",
			body:    `    resource("a", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"generateName": "x-"}})`,
			reason:  ReasonInvalidOutput,
			message: "generateName",
		},
		{
			name:    "unusable key",
			body:    `    resource("not a key!", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "x"}})`,
			reason:  ReasonInvalidOutput,
			message: "not usable as an identity",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := run(t, "def compose(variable, observed):\n"+tc.body+"\n", Request{})
			pe := programError(t, err)
			if pe.Reason != tc.reason {
				t.Errorf("reason = %q, want %q (%s)", pe.Reason, tc.reason, pe.Msg)
			}
			if tc.message != "" && !strings.Contains(pe.Msg, tc.message) {
				t.Errorf("msg = %q, want it to mention %q", pe.Msg, tc.message)
			}
		})
	}
}

func TestMissingComposeFunction(t *testing.T) {
	_, err := run(t, `x = 1`, Request{})
	pe := programError(t, err)
	if pe.Reason != ReasonNoComposeFunc {
		t.Errorf("reason = %q", pe.Reason)
	}
}

func TestSyntaxError(t *testing.T) {
	_, err := run(t, `def compose(:`, Request{})
	pe := programError(t, err)
	if pe.Reason != ReasonSyntaxError {
		t.Errorf("reason = %q", pe.Reason)
	}
}

// A typo in a field name must not quietly become None: that produces a resource
// with a blank field which applies cleanly, and is far worse than an error.
func TestMissingFieldIsAnErrorNotNone(t *testing.T) {
	_, err := run(t, `
def compose(variable, observed):
    resource("x", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": read("v1", "Thing", "rg").staus}})
`, Request{Reader: fakeReader{
		"rg": map[string]any{"apiVersion": "v1", "kind": "ResourceGroup", "metadata": map[string]any{"name": "rg"}},
	}})

	pe := programError(t, err)
	if !strings.Contains(pe.Msg, "staus") {
		t.Errorf("msg = %q, should name the missing field", pe.Msg)
	}
	if !strings.Contains(pe.Msg, "apiVersion") {
		t.Errorf("msg = %q, should list the fields that do exist", pe.Msg)
	}
}

// Integers must survive the round trip: a replica count that comes back as 3.0
// is a different resource body and churns the object on every apply.
func TestIntegersSurviveRoundTrip(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    resource("d", {
        "apiVersion": "apps/v1",
        "kind": "Deployment",
        "metadata": {"name": "d"},
        "spec": {"replicas": variable.replicas, "doubled": variable.replicas * 2},
        })
`, Request{Variables: map[string]any{"replicas": int64(3)}})

	spec := res.Resources[0].Object["spec"].(map[string]any)
	if got, ok := spec["replicas"].(int64); !ok || got != 3 {
		t.Errorf("replicas = %#v, want int64(3)", spec["replicas"])
	}
	if got, ok := spec["doubled"].(int64); !ok || got != 6 {
		t.Errorf("doubled = %#v, want int64(6)", spec["doubled"])
	}
}

func TestToJSONPreservesOrder(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    d = {}
    d["zebra"] = 1
    d["apple"] = [1, 2]
    resource("c", {
        "apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "c"},
        "data": {"compact": to_json(d), "pretty": to_json(d, indent = 2)},
        })
`, Request{})

	data := res.Resources[0].Object["data"].(map[string]any)
	if data["compact"] != `{"zebra":1,"apple":[1,2]}` {
		t.Errorf("compact = %q", data["compact"])
	}
	want := "{\n  \"zebra\": 1,\n  \"apple\": [\n    1,\n    2\n  ]\n}"
	if data["pretty"] != want {
		t.Errorf("pretty = %q, want %q", data["pretty"], want)
	}
}

func TestRoundTripHelpers(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    parsed = from_yaml("a: 1\nb: [x, y]\n")
    resource("c", {
        "apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "c"},
        "data": {
            "a": str(parsed.a),
            "b1": parsed.b[1],
            "json": to_json(from_json('{"k": "v"}')),
            "b64": b64encode("hello"),
            "back": b64decode(b64encode("hello")),
            "hash": sha256("x")[:8],
        },
        })
`, Request{})

	data := res.Resources[0].Object["data"].(map[string]any)
	for k, want := range map[string]string{
		"a":    "1",
		"b1":   "y",
		"json": `{"k":"v"}`,
		"b64":  "aGVsbG8=",
		"back": "hello",
		"hash": "2d711642",
	} {
		if data[k] != want {
			t.Errorf("%s = %v, want %v", k, data[k], want)
		}
	}
}

func TestHasBuiltin(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    resource("c", {
        "apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "c"},
        "data": {
            "yes": str(has(read("v1", "Thing", "rg"), "status.atProvider.id")),
            "no": str(has(read("v1", "Thing", "rg"), "status.atProvider.other")),
            "nullIsAbsent": str(has(read("v1", "Thing", "rg"), "status.nulled")),
        },
        })
`, Request{Reader: fakeReader{
		"rg": map[string]any{
			"apiVersion": "v1", "kind": "ResourceGroup", "metadata": map[string]any{"name": "rg"},
			"status": map[string]any{
				"atProvider": map[string]any{"id": "abc"},
				"nulled":     nil,
			},
		},
	}})

	data := res.Resources[0].Object["data"].(map[string]any)
	if data["yes"] != "True" || data["no"] != "False" || data["nullIsAbsent"] != "False" {
		t.Errorf("has() results: %v", data)
	}
}

// The program cache is keyed by content, so the same source compiled twice must
// come back as the same program, and different source must not.
func TestProgramCache(t *testing.T) {
	s := NewStarlark(Options{CacheSize: 2})
	src := "def compose(variable, observed):\n    return {}\n"

	p1, err := s.program(src)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.program(src)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Error("identical source should reuse the compiled program")
	}

	other := "def compose(variable, observed):\n    return {}\n# different\n"
	p3, err := s.program(other)
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p3 {
		t.Error("different source must not share a compiled program")
	}

	// Overflowing the cache evicts, but correctness does not depend on a hit.
	if _, err := s.program("def compose(i, s, o):\n    return {}\n# third\n"); err != nil {
		t.Fatal(err)
	}
	if len(s.cache) > 2 {
		t.Errorf("cache holds %d entries, limit is 2", len(s.cache))
	}
}

func TestContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewStarlark(Options{}).Evaluate(ctx, Request{Program: `
def compose(variable, observed):
    total = 0
    for i in range(100000000):
        total += i
    return
`})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// A resource that does not exist is falsey rather than an object, and reaching
// into it says which read came back empty. Without a declared source list this
// is the only place a typo can be caught, so the message has to carry the name.
func TestReachingIntoSomethingAbsent(t *testing.T) {
	_, err := run(t, `
def compose(variable, observed):
    resource("x", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": read("v1", "Thing", "nope").metadata.name}})
`, Request{Reader: fakeReader{"rg": nil}})

	pe := programError(t, err)
	if !strings.Contains(pe.Msg, "nope") {
		t.Errorf("msg = %q", pe.Msg)
	}
}

func TestObservedIsIterable(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    names = sorted([k for k in observed])
    resource("c", {
        "apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "c"},
        "data": {"keys": ",".join(names), "count": str(len(observed))},
        })
`, Request{Observed: map[string]any{
		"b": map[string]any{"kind": "X"},
		"a": map[string]any{"kind": "Y"},
	}})

	data := res.Resources[0].Object["data"].(map[string]any)
	if data["keys"] != "a,b" || data["count"] != "2" {
		t.Errorf("observed iteration: %v", data)
	}
}

// Staging needs a third thing that neither a plain mapping nor wait() can say:
// "these resources are correct, and I am still not finished". Returning wait()
// would produce no resources at all, so the identity everything else depends on
// would never be created.
func TestPendingReturnsResourcesAndAReason(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    resource("identity", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "id"}})
    principal = get(observed, ["identity", "status", "principalId"])
    if not principal:
        pending("principalId on the app identity")
        return
    resource("assignment", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "ra"}})
    return
`, Request{})

	if res.Waiting() {
		t.Fatal("pending() must not discard the resources the way wait() does")
	}
	if len(res.Resources) != 1 || res.Resources[0].Key != "identity" {
		t.Fatalf("resources = %v", res.Resources)
	}
	if len(res.Pending) != 1 || res.Pending[0] != "principalId on the app identity" {
		t.Errorf("pending = %v", res.Pending)
	}
}

// Reporting the same unresolved thing once per loop iteration is easy to write
// by accident and useless to read.
func TestPendingDeduplicates(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    for i in range(5):
        pending("the resource group id")
    pending("the service bus id")
    return
`, Request{})

	if len(res.Pending) != 2 {
		t.Errorf("pending = %v, want two distinct reasons", res.Pending)
	}
}

func TestPendingNeedsAReason(t *testing.T) {
	_, err := run(t, `
def compose(variable, observed):
    pending("")
    return
`, Request{})
	if err == nil {
		t.Fatal("an empty reason should be rejected: it becomes a condition message")
	}
}

// A finished composition reports nothing pending, so Ready means Ready.
func TestNoPendingWhenResolved(t *testing.T) {
	res := mustRun(t, `
def compose(variable, observed):
    resource("a", {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "a"}})
`, Request{})
	if len(res.Pending) != 0 {
		t.Errorf("pending = %v, want none", res.Pending)
	}
}

// The language bounds live in a per-compilation FileOptions rather than the
// package-level resolve.Allow* variables, which are process-global and could be
// changed by anything else in the binary. These assert the resulting dialect is
// the intended one, since a silently wrong option would either break working
// programs or quietly widen what an untrusted author can do.
func TestLanguageBounds(t *testing.T) {
	t.Run("sets are available", func(t *testing.T) {
		res := mustRun(t, `
def compose(variable, observed):
    unique = set(["a", "b", "a"])
    resource("c", {
        "apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "c"},
        "data": {"n": str(len(unique))},
        })
`, Request{})
		if got := res.Resources[0].Object["data"].(map[string]any)["n"]; got != "2" {
			t.Errorf("set deduplication produced %v, want 2", got)
		}
	})

	t.Run("while is rejected", func(t *testing.T) {
		_, err := run(t, `
def compose(variable, observed):
    while True:
        break
    return
`, Request{})
		pe := programError(t, err)
		if pe.Reason != ReasonSyntaxError {
			t.Errorf("reason = %q, want %q", pe.Reason, ReasonSyntaxError)
		}
	})

	t.Run("top-level control flow is rejected", func(t *testing.T) {
		_, err := run(t, `
if 1 == 1:
    x = 2

def compose(variable, observed):
    return
`, Request{})
		pe := programError(t, err)
		if pe.Reason != ReasonSyntaxError {
			t.Errorf("reason = %q, want %q", pe.Reason, ReasonSyntaxError)
		}
	})

	t.Run("global reassignment is rejected", func(t *testing.T) {
		_, err := run(t, `
X = 1
X = 2

def compose(variable, observed):
    return
`, Request{})
		pe := programError(t, err)
		if pe.Reason != ReasonSyntaxError {
			t.Errorf("reason = %q, want %q", pe.Reason, ReasonSyntaxError)
		}
	})
}
