// Package kube provides the impersonated data path.
//
// Every read and every write a Weave performs goes through a client built here,
// bound to the ServiceAccount the Weave names. There are no exceptions to that
// and the package offers no way to make one: creation authority is not read
// authority, a ServiceAccount may hold create without get, and the entire
// premise of the system is "read a value out of one object and write it into a
// sibling". An unimpersonated read on that path is a privilege leak, not an
// optimisation.
package kube

import (
	"context"
	"fmt"
	"strings"
	"sync"

	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"

	"github.com/alethic/weft/internal/naming"
)

// NamespacePlaceholder is replaced with the Weave's namespace in each entry of
// the impersonated group list.
const NamespacePlaceholder = "{namespace}"

// DefaultGroups are impersonated alongside the username.
//
// Impersonating only the username is the common mistake: the API server does
// not derive group membership from a username, so RBAC bound to
// system:authenticated would not apply and the impersonated identity would be
// strictly weaker than the real ServiceAccount. Weaker sounds safe but is not.
// It makes Weft fail on grants the user can plainly see are present, which is
// the exact confusion this tool exists to remove.
//
// The per-namespace group system:serviceaccounts:<namespace> is deliberately
// absent. A real token carries it, but including it would force the
// controller's ClusterRole to grant impersonate on groups without a
// resourceNames restriction, since the namespace set is open-ended. That
// unrestricted grant would let anyone who compromised the controller
// impersonate system:masters, which is a far worse position than the one this
// project set out to avoid. The two groups here are enumerable, so the shipped
// ClusterRole pins them by name.
//
// Add it back with --impersonate-groups if your namespaces bind RBAC to it;
// docs/rbac.md spells out the ClusterRole change that goes with it.
var DefaultGroups = []string{
	"system:serviceaccounts",
	"system:authenticated",
}

// UnknownKindError means the cluster has no such type installed.
type UnknownKindError struct {
	GVK schema.GroupVersionKind
	Err error
}

func (e *UnknownKindError) Error() string {
	return fmt.Sprintf("no resource type %s is installed in this cluster", e.GVK)
}
func (e *UnknownKindError) Unwrap() error { return e.Err }

// ClusterScopedError means a kind exists but is not namespaced.
type ClusterScopedError struct {
	GVK schema.GroupVersionKind
}

func (e *ClusterScopedError) Error() string {
	return fmt.Sprintf("%s is cluster-scoped", e.GVK.Kind)
}

// FactoryOptions configure impersonation.
type FactoryOptions struct {
	// Groups are impersonated alongside the username. NamespacePlaceholder is
	// substituted per Weave. A nil value means DefaultGroups; an explicitly
	// empty slice means impersonate no groups at all.
	Groups []string

	// UserAgent identifies the controller's requests in audit logs. Impersonated
	// requests record both the controller and the impersonated subject, which
	// is what makes this model auditable.
	UserAgent string

	// CacheSize bounds how many per-ServiceAccount clients are retained.
	CacheSize int
}

// Factory builds impersonated clients.
type Factory struct {
	base   *rest.Config
	mapper meta.RESTMapper
	groups []string
	agent  string
	size   int

	mu      sync.Mutex
	clients map[string]*Client
}

// NewFactory returns a Factory over a base config.
//
// The RESTMapper is the controller's own, built with the controller's identity.
// That is deliberate and is the one thing not impersonated: discovery returns
// the shape of the API, not the contents of anybody's namespace. Impersonating
// it would additionally require every ServiceAccount to hold the discovery
// role, which is a cluster-scoped grant a namespace user cannot make, and would
// break the property that a Weave needs no cluster-admin in the loop.
func NewFactory(base *rest.Config, mapper meta.RESTMapper, opts FactoryOptions) (*Factory, error) {
	if base == nil {
		return nil, fmt.Errorf("a base rest config is required")
	}
	if base.Impersonate.UserName != "" {
		return nil, fmt.Errorf("base config already impersonates %q; Weft must start from the controller's own identity", base.Impersonate.UserName)
	}
	groups := opts.Groups
	if groups == nil {
		groups = DefaultGroups
	}
	agent := opts.UserAgent
	if agent == "" {
		agent = naming.Group
	}
	size := opts.CacheSize
	if size <= 0 {
		size = 256
	}
	return &Factory{
		base:    base,
		mapper:  mapper,
		groups:  groups,
		agent:   agent,
		size:    size,
		clients: make(map[string]*Client),
	}, nil
}

// Mapper exposes the shared RESTMapper.
func (f *Factory) Mapper() meta.RESTMapper { return f.mapper }

// For returns a client that acts as the given ServiceAccount.
func (f *Factory) For(namespace, serviceAccount string) (*Client, error) {
	if namespace == "" || serviceAccount == "" {
		return nil, fmt.Errorf("namespace and serviceAccountName are both required")
	}
	key := namespace + "/" + serviceAccount

	f.mu.Lock()
	if c, ok := f.clients[key]; ok {
		f.mu.Unlock()
		return c, nil
	}
	f.mu.Unlock()

	cfg := rest.CopyConfig(f.base)
	cfg.UserAgent = f.agent
	cfg.Impersonate = rest.ImpersonationConfig{
		// The API server special-cases this username form and authorises it
		// against the serviceaccounts resource in that namespace, which is what
		// lets the controller's impersonate grant be scoped by namespace rather
		// than being a blanket right to become any user.
		UserName: naming.UserFor(namespace, serviceAccount),
		Groups:   expandGroups(f.groups, namespace),
	}

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building impersonated dynamic client: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building impersonated typed client: %w", err)
	}
	md, err := metadata.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building impersonated metadata client: %w", err)
	}

	c := &Client{
		namespace:      namespace,
		serviceAccount: serviceAccount,
		dyn:            dyn,
		typed:          cs,
		meta:           md,
		mapper:         f.mapper,
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.clients[key]; ok {
		return existing, nil
	}
	if len(f.clients) >= f.size {
		// Clients are stateless handles over a shared transport; dropping the
		// map wholesale is cheaper than tracking recency and costs one rebuild.
		f.clients = make(map[string]*Client, f.size)
	}
	f.clients[key] = c
	return c, nil
}

func expandGroups(groups []string, namespace string) []string {
	if len(groups) == 0 {
		return nil
	}
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, strings.ReplaceAll(g, NamespacePlaceholder, namespace))
	}
	return out
}

// Client performs namespaced operations as one ServiceAccount.
type Client struct {
	namespace      string
	serviceAccount string
	dyn            dynamic.Interface
	typed          kubernetes.Interface
	meta           metadata.Interface
	mapper         meta.RESTMapper
}

// Namespace is the namespace this client is bound to.
func (c *Client) Namespace() string { return c.namespace }

// ServiceAccount is the impersonated subject.
func (c *Client) ServiceAccount() string { return c.serviceAccount }

// Metadata returns a metadata-only client for this subject.
//
// Metadata-only is not a performance choice. A watch established here caches
// object metadata and nothing else, so there is structurally no field value in
// it for anything to read by mistake: the impersonated Get stays the only path
// to a value. Keeping a full-object cache that we are not supposed to read from
// would work right up until somebody read from it.
func (c *Client) Metadata() metadata.Interface { return c.meta }

// ResourceFor maps a kind to its namespaced resource.
func (c *Client) ResourceFor(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return c.resourceFor(gvk)
}

// resourceFor maps a kind to a namespaced resource, rejecting anything that
// cannot participate in the same-namespace model.
func (c *Client) resourceFor(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return schema.GroupVersionResource{}, &UnknownKindError{GVK: gvk, Err: err}
		}
		return schema.GroupVersionResource{}, err
	}
	if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
		return schema.GroupVersionResource{}, &ClusterScopedError{GVK: gvk}
	}
	return mapping.Resource, nil
}

// Get reads one object in the client's namespace.
func (c *Client) Get(ctx context.Context, gvk schema.GroupVersionKind, name string) (*unstructured.Unstructured, error) {
	gvr, err := c.resourceFor(gvk)
	if err != nil {
		return nil, err
	}
	obj, err := c.dyn.Resource(gvr).Namespace(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, asPermissionError(err, c.namespace, c.serviceAccount, "get", gvr, gvk.Kind, name)
	}
	return obj, nil
}

// List reads every object of a kind in the client's namespace matching the
// labels. An empty selector matches everything of that kind.
//
// Namespaced like every other read: there is no cross-namespace form, because
// the permission to perform one could not be granted by a namespace user.
func (c *Client) List(ctx context.Context, gvk schema.GroupVersionKind, labels map[string]string) ([]unstructured.Unstructured, error) {
	gvr, err := c.resourceFor(gvk)
	if err != nil {
		return nil, err
	}
	opts := metav1.ListOptions{}
	if len(labels) > 0 {
		opts.LabelSelector = klabels.SelectorFromSet(labels).String()
	}
	list, err := c.dyn.Resource(gvr).Namespace(c.namespace).List(ctx, opts)
	if err != nil {
		return nil, asPermissionError(err, c.namespace, c.serviceAccount, "list", gvr, gvk.Kind, "")
	}
	return list.Items, nil
}

// Apply server-side applies an object, taking ownership of the fields it sets.
//
// Force is on because Weft is the authority for the fields it writes: a
// composition that has been edited must be able to take a field back from a
// previous manager, and the alternative is a conflict that no user action can
// clear.
func (c *Client) Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	gvk := obj.GroupVersionKind()
	gvr, err := c.resourceFor(gvk)
	if err != nil {
		return nil, err
	}
	name := obj.GetName()
	applied, err := c.dyn.Resource(gvr).Namespace(c.namespace).Apply(ctx, name, obj, metav1.ApplyOptions{
		FieldManager: naming.FieldManager,
		Force:        true,
	})
	if err != nil {
		// Apply is create-or-update, so the verb a denial should point at is
		// whichever the user is missing; patch is the one server-side apply
		// actually requires and the one worth naming.
		return nil, asPermissionError(err, c.namespace, c.serviceAccount, "patch", gvr, gvk.Kind, name)
	}
	return applied, nil
}

// Delete removes an object, guarded by the UID recorded when it was applied so
// that a resource recreated by somebody else under the same name is not
// destroyed by our teardown.
func (c *Client) Delete(ctx context.Context, gvk schema.GroupVersionKind, name string, uid types.UID) error {
	gvr, err := c.resourceFor(gvk)
	if err != nil {
		return err
	}
	opts := metav1.DeleteOptions{}
	if uid != "" {
		opts.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	err = c.dyn.Resource(gvr).Namespace(c.namespace).Delete(ctx, name, opts)
	switch {
	case err == nil, apierrors.IsNotFound(err):
		return nil
	case apierrors.IsConflict(err):
		// The UID precondition failed: what is there now is not what we made.
		return nil
	default:
		return asPermissionError(err, c.namespace, c.serviceAccount, "delete", gvr, gvk.Kind, name)
	}
}

// MutateFinalizers reads an object, lets the caller edit its finalizer list,
// and writes it back, retrying on conflict. It returns false when the callback
// reported no change.
func (c *Client) MutateFinalizers(ctx context.Context, gvk schema.GroupVersionKind, name string, mutate func(finalizers []string) ([]string, bool)) (bool, error) {
	gvr, err := c.resourceFor(gvk)
	if err != nil {
		return false, err
	}
	ri := c.dyn.Resource(gvr).Namespace(c.namespace)

	changed := false
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj, err := ri.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return asPermissionError(err, c.namespace, c.serviceAccount, "get", gvr, gvk.Kind, name)
		}
		next, ok := mutate(obj.GetFinalizers())
		if !ok {
			changed = false
			return nil
		}
		obj.SetFinalizers(next)
		if _, err := ri.Update(ctx, obj, metav1.UpdateOptions{FieldManager: naming.FieldManager}); err != nil {
			if apierrors.IsConflict(err) {
				return err
			}
			return asPermissionError(err, c.namespace, c.serviceAccount, "update", gvr, gvk.Kind, name)
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// CanI asks the API server whether the impersonated subject may perform a verb,
// without attempting it.
//
// Used where attempting the operation would be the wrong way to find out: a
// finalizer that cannot later be removed deadlocks the object it is on, so the
// permission to remove it is checked before it is added.
func (c *Client) CanI(ctx context.Context, verb string, gvk schema.GroupVersionKind, name string) (bool, error) {
	gvr, err := c.resourceFor(gvk)
	if err != nil {
		return false, err
	}
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: c.namespace,
				Verb:      verb,
				Group:     gvr.Group,
				Version:   gvr.Version,
				Resource:  gvr.Resource,
				Name:      name,
			},
		},
	}
	out, err := c.typed.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("checking whether %s may %s %s: %w", c.serviceAccount, verb, gvr.Resource, err)
	}
	return out.Status.Allowed, nil
}

// PermissionErrorFor builds the diagnostic for a verb the subject is known to
// lack, for cases discovered through CanI rather than through a denial.
func (c *Client) PermissionErrorFor(verb string, gvk schema.GroupVersionKind, name string) *PermissionError {
	gvr, err := c.resourceFor(gvk)
	if err != nil {
		// Fall back to a plausible resource name; this path only formats a
		// message and must not itself fail.
		gvr = schema.GroupVersionResource{
			Group:    gvk.Group,
			Version:  gvk.Version,
			Resource: strings.ToLower(gvk.Kind) + "s",
		}
	}
	return &PermissionError{
		Namespace:      c.namespace,
		ServiceAccount: c.serviceAccount,
		Verb:           verb,
		Resource:       gvr,
		Kind:           gvk.Kind,
		Name:           name,
	}
}
