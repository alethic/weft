package kube

import (
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// PermissionError is a denial under impersonation.
//
// Debugging RBAC is the primary user experience of this tool, so this type
// exists to turn a denial into something a person can act on without first
// learning how RBAC is spelled. The controller deliberately does not fill the
// gap with its own privileges: a Weave that cannot read something fails, and
// says exactly what to grant.
type PermissionError struct {
	// ServiceAccount is the impersonated subject, as namespace/name.
	Namespace      string
	ServiceAccount string

	// Verb and Resource are what was attempted.
	Verb     string
	Resource schema.GroupVersionResource

	// Kind is carried for the human-readable part of the message; the RBAC
	// rule itself is written in terms of the resource.
	Kind string

	// Name of the object, when the attempt was against a specific one.
	Name string

	// Err is the underlying API error.
	Err error
}

func (e *PermissionError) Error() string {
	return e.Summary() + "\n\n" + e.Fix()
}

func (e *PermissionError) Unwrap() error { return e.Err }

// Summary is the one-line statement of what was denied.
func (e *PermissionError) Summary() string {
	subject := fmt.Sprintf("ServiceAccount %q", e.ServiceAccount)
	target := e.resourceName()
	if e.Name != "" {
		target = fmt.Sprintf("%s %q", target, e.Name)
	}
	return fmt.Sprintf("%s cannot %s %s in namespace %q", subject, e.Verb, target, e.Namespace)
}

// Fix renders commands that grant exactly what was missing.
//
// Written as single-line commands on purpose: kubectl describe indents
// continuation lines, which silently breaks a backslash-continued command when
// someone copies it out of a condition message.
func (e *PermissionError) Fix() string {
	roleName := e.roleName()
	verbs := strings.Join(e.grantVerbs(), ",")
	return fmt.Sprintf(
		"Grant it with:\n"+
			"  kubectl create role %s --namespace=%s --verb=%s --resource=%s\n"+
			"  kubectl create rolebinding %s --namespace=%s --role=%s --serviceaccount=%s:%s",
		roleName, e.Namespace, verbs, e.resourceName(),
		roleName, e.Namespace, roleName, e.Namespace, e.ServiceAccount,
	)
}

// resourceName is the resource.group form kubectl expects in --resource.
func (e *PermissionError) resourceName() string {
	if e.Resource.Group == "" {
		return e.Resource.Resource
	}
	return e.Resource.Resource + "." + e.Resource.Group
}

// grantVerbs widens the denied verb to the set that goes with it, because
// granting exactly one verb almost always produces a second denial on the next
// reconcile.
func (e *PermissionError) grantVerbs() []string {
	switch e.Verb {
	case "get", "list", "watch":
		return []string{"get", "list", "watch"}
	case "create", "patch", "update":
		return []string{"get", "list", "watch", "create", "patch", "update"}
	case "delete":
		return []string{"get", "list", "watch", "delete"}
	default:
		return []string{e.Verb}
	}
}

func (e *PermissionError) roleName() string {
	action := "read"
	switch e.Verb {
	case "create", "patch", "update":
		action = "write"
	case "delete":
		action = "delete"
	}
	name := fmt.Sprintf("weft-%s-%s", action, e.Resource.Resource)
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

// Reason is the condition reason for a denial.
func (e *PermissionError) Reason() string { return "Forbidden" }

// asPermissionError wraps a Forbidden API error, and leaves anything else
// alone. A denial is a distinct outcome from a transient API failure and the
// two must not be conflated: one needs a RoleBinding, the other needs a retry.
func asPermissionError(err error, namespace, serviceAccount, verb string, gvr schema.GroupVersionResource, kind, name string) error {
	if err == nil || !apierrors.IsForbidden(err) {
		return err
	}
	return &PermissionError{
		Namespace:      namespace,
		ServiceAccount: serviceAccount,
		Verb:           verb,
		Resource:       gvr,
		Kind:           kind,
		Name:           name,
		Err:            err,
	}
}
