package eval

import "testing"

// The gate a `required: true` source provides, written in the program instead.
func TestGatingInTheBody(t *testing.T) {
	program := `
def compose(variable, sources, observed):
    # Never reads a field off it. A pure ordering gate.
    if not sources.database:
        return wait("MSSQLDatabase has not been created yet")
    return {"cfg": {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "c"}}}
`
	res := mustRun(t, program, Request{Sources: map[string]any{"database": nil}})
	if !res.Waiting() || res.Wait.Reason != "MSSQLDatabase has not been created yet" {
		t.Fatalf("absent source should gate: %#v", res)
	}

	res = mustRun(t, program, Request{Sources: map[string]any{
		"database": map[string]any{"apiVersion": "v1", "kind": "MSSQLDatabase",
			"metadata": map[string]any{"name": "db"}},
	}})
	if res.Waiting() || len(res.Resources) != 1 {
		t.Fatalf("present source should proceed: %#v", res)
	}
}

// And the thing a static boolean cannot express: gating that depends on
// configuration.
func TestConditionalGatingInTheBody(t *testing.T) {
	program := `
def compose(variable, sources, observed):
    if variable.useSql and not sources.database:
        return wait("SQL is enabled but the database does not exist yet")
    return {"cfg": {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "c"}}}
`
	res := mustRun(t, program, Request{
		Variables: map[string]any{"useSql": true},
		Sources:   map[string]any{"database": nil},
	})
	if !res.Waiting() {
		t.Fatal("should gate when SQL is enabled and the database is absent")
	}

	res = mustRun(t, program, Request{
		Variables: map[string]any{"useSql": false},
		Sources:   map[string]any{"database": nil},
	})
	if res.Waiting() || len(res.Resources) != 1 {
		t.Fatalf("should proceed when SQL is off, absent database and all: %#v", res)
	}
}
