package kube

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

func forbidden() error {
	return apierrors.NewForbidden(
		schema.GroupResource{Group: "azure.m.upbound.io", Resource: "resourcegroups"},
		"sweep-env",
		nil,
	)
}

// Debugging RBAC is the primary user experience of this tool, so the message a
// denial produces is a feature with a test, not incidental formatting.
func TestPermissionErrorIsActionable(t *testing.T) {
	err := asPermissionError(forbidden(), "sweep-labs", "sweep-env-compose", "get",
		schema.GroupVersionResource{Group: "azure.m.upbound.io", Version: "v1beta1", Resource: "resourcegroups"},
		"ResourceGroup", "sweep-env")

	perm, ok := err.(*PermissionError)
	if !ok {
		t.Fatalf("got %T, want *PermissionError", err)
	}

	summary := perm.Summary()
	for _, want := range []string{"sweep-env-compose", "cannot get", "resourcegroups.azure.m.upbound.io", "sweep-labs"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q is missing %q", summary, want)
		}
	}

	fix := perm.Fix()
	for _, want := range []string{
		"kubectl create role weft-read-resourcegroups --namespace=sweep-labs --verb=get,list,watch --resource=resourcegroups.azure.m.upbound.io",
		"kubectl create rolebinding weft-read-resourcegroups --namespace=sweep-labs --role=weft-read-resourcegroups --serviceaccount=sweep-labs:sweep-env-compose",
	} {
		if !strings.Contains(fix, want) {
			t.Errorf("fix is missing the line:\n  %s\ngot:\n%s", want, fix)
		}
	}

	// kubectl describe indents continuation lines, so a backslash-continued
	// command silently breaks when copied out of a condition message.
	if strings.Contains(fix, "\\\n") {
		t.Error("commands must be single-line to survive being copied out of kubectl describe")
	}
}

// Granting exactly the denied verb reliably produces a second denial on the
// next reconcile, so the suggestion widens to the set that goes together.
func TestGrantVerbsWiden(t *testing.T) {
	cases := map[string]string{
		"get":    "get,list,watch",
		"list":   "get,list,watch",
		"patch":  "get,list,watch,create,patch,update",
		"update": "get,list,watch,create,patch,update",
		"delete": "get,list,watch,delete",
	}
	for verb, want := range cases {
		perm := &PermissionError{
			Namespace: "ns", ServiceAccount: "sa", Verb: verb,
			Resource: schema.GroupVersionResource{Resource: "configmaps"},
		}
		if got := strings.Join(perm.grantVerbs(), ","); got != want {
			t.Errorf("%s widened to %q, want %q", verb, got, want)
		}
	}
}

func TestCoreGroupResourceName(t *testing.T) {
	perm := &PermissionError{
		Namespace: "ns", ServiceAccount: "sa", Verb: "get",
		Resource: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
	}
	if got := perm.resourceName(); got != "configmaps" {
		t.Errorf("core group resource rendered as %q, want %q", got, "configmaps")
	}
}

// A denial and a transient failure need different responses: one needs a
// RoleBinding, the other needs a retry. Conflating them would either spam
// RBAC advice at an unreachable API server or silently retry a permanent
// refusal forever.
func TestNonForbiddenErrorsPassThrough(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "x")
	if got := asPermissionError(notFound, "ns", "sa", "get", schema.GroupVersionResource{}, "ConfigMap", "x"); got != notFound {
		t.Errorf("NotFound was rewrapped as %T", got)
	}
	if got := asPermissionError(nil, "ns", "sa", "get", schema.GroupVersionResource{}, "ConfigMap", "x"); got != nil {
		t.Errorf("nil became %v", got)
	}

	timeout := apierrors.NewTimeoutError("slow", 1)
	if _, isPerm := asPermissionError(timeout, "ns", "sa", "get", schema.GroupVersionResource{}, "ConfigMap", "x").(*PermissionError); isPerm {
		t.Error("a timeout should not be reported as a permission problem")
	}
}

func TestRoleNameStaysWithinLimits(t *testing.T) {
	perm := &PermissionError{
		Namespace: "ns", ServiceAccount: "sa", Verb: "get",
		Resource: schema.GroupVersionResource{Resource: strings.Repeat("verylongresource", 8)},
	}
	name := perm.roleName()
	if len(name) > 63 {
		t.Errorf("role name is %d characters", len(name))
	}
	if strings.HasSuffix(name, "-") {
		t.Errorf("role name %q ends with a separator", name)
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		t.Errorf("role name %q is not a usable object name: %v", name, errs)
	}
}
