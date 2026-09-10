# Weft

A Kubernetes operator that lets an ordinary namespace user apply one custom
resource that reads existing resources in their namespace and generates other
resources from them — reactively, with proper ownership and garbage collection.

The name comes from weaving. The **warp** is the existing threads held on the
loom: resources we read but do not own. The **weft** is what we pass across
them: resources we create. The custom resource is a `Weave`.

## The two properties

Everything else in this project is negotiable. These are not.

**Namespace-scoped authoring.** A user with `edit` in their own namespace can
create a `Weave` and get resources. No cluster-scoped object is created at
runtime, no CRD is generated per composition, no platform team is in the loop,
no cluster-admin.

**Applies run as the user's ServiceAccount.** `spec.serviceAccountName` names a
ServiceAccount in the same namespace, and the controller impersonates it for
every read and every write. The controller holds impersonation rights, not
blanket write access. A user can never cause the creation of anything their own
ServiceAccount could not have created by hand.

The second property is worth stating as a consequence: if you run this
controller as cluster-admin and point a `Weave` at a ServiceAccount with no
permissions, nothing is created, and the `Weave` tells you which RoleBinding is
missing.

## Why this and not something else

| | namespaced authoring | impersonated writes | status-reactive |
|---|---|---|---|
| [kro](https://kro.run) | no — `ResourceGraphDefinition` is cluster-scoped and generates a CRD | no | yes |
| [Crossplane v2](https://crossplane.io) | XRs are namespaced, but XRDs and Compositions are cluster-scoped admin artifacts | no — the RBAC manager runs with `escalate` | yes |
| Metacontroller, Yoke ATC | no | no | yes |
| [Flux](https://fluxcd.io) | yes | **yes** — this is where the model comes from | no — `dependsOn` between whole Kustomizations, and `substituteFrom` limited to ConfigMaps and Secrets |
| Kyverno `generate` | yes | no — the background controller's own ServiceAccount does the creating, checked once at policy admission | yes |

Namespaced **and** impersonated **and** status-reactive was unoccupied.

## Quick start

```bash
helm install weft oci://ghcr.io/alethic/charts/weft \
  --namespace weft-system --create-namespace
```

The chart is the supported install path and is documented in
[charts/weft/README.md](charts/weft/README.md). It ships sensible defaults, a
values schema that rejects malformed input at install time, and refuses to
render a handful of configurations that would only fail later - a templated
impersonation group without an explicit acknowledgement of what it grants,
several replicas with leader election off, and so on.

Then, as an ordinary namespace user:

```yaml
apiVersion: weft.run/v1alpha1
kind: Weave
metadata:
  name: app
  namespace: my-namespace
spec:
  serviceAccountName: composer

  inputs:
    prefix: demo

  sources:
  - id: tenant
    apiVersion: v1
    kind: ConfigMap
    name: tenant
    required: true

  program: |
    def compose(inputs, sources, observed):
        tenant = require(sources.tenant, "data.tenantId")
        return {
            "settings": {
                "apiVersion": "v1",
                "kind": "ConfigMap",
                "metadata": {"name": inputs.prefix + "-settings"},
                "data": {"tenantId": tenant},
            },
        }
```

`composer` needs permission to read the `tenant` ConfigMap and to write the one
being generated. If it does not have them, the `Weave` says so, in words you can
paste into a shell. See [docs/rbac.md](docs/rbac.md).

## The model

### Three outcomes, not two

Resolution can succeed, fail permanently, or **not yet**. "Not yet" is the
normal case, not an edge case, and it has its own condition:

```
$ kubectl get weave
NAME   READY   WAITING
app    False   FieldUnresolved

$ kubectl get weave app -o jsonpath='{.status.conditions[?(@.type=="Waiting")].message}'
status.atProvider.principalId is not set on UserAssignedIdentity/demo-app
```

Existence does not unblock. **Field resolution** does. Applying a Crossplane
managed resource returns instantly, but its `status.atProvider` populates
minutes later, so a resource we created behaves exactly like an external source
that is not ready. One rule covers both.

### Self-reference is how staging works

`observed` holds the resources this `Weave` previously created, read back live.
A composition advances in phases by returning only what it can:

```python
def compose(inputs, sources, observed):
    out = {}
    out["identity"] = {...}                      # phase one

    pid = get(observed, ["identity", "status", "atProvider", "principalId"])
    if not pid:
        pending("principalId on the app identity")
        return out                               # the identity is applied anyway

    for role in inputs.roles:                    # phase two
        out["ra-" + role.name] = {...}
    return out
```

No `dependsOn`, no explicit graph. Conditional inclusion *is* the dependency
edge, and the watch on the identity wakes the `Weave` the moment its status is
written back.

`pending()` rather than `wait()` matters here: a wait produces no resources at
all, so the identity everything depends on would never be created and the
composition could never advance past it. `pending()` applies what is ready and
still reports what is outstanding.

### Identity is the key, not the position

The keys of the returned mapping are the inventory identity. Reordering a list
cannot rename a live object. Renaming a key is not a rename: it deletes one
resource and creates another.

### Ordering

Return order becomes apply order and therefore reverse teardown order. Cascading
garbage collection would remove everything a `Weave` owns, but in no particular
order, and self-reference makes the order real — a role assignment naming a
principal has to go before the identity that owns it.

For a fan-out of independent siblings, put them in the same wave so they tear
down together:

```python
out[key] = {
    "metadata": {"annotations": {"weft.run/wave": "1"}},
    ...
}
```

Either annotate every resource or none; mixing explicit waves with positional
defaults produces an order nobody wrote, and is rejected.

### Ordering gates with no field read

A dependency that is never referenced is invisible to any design that infers
edges from expression references. `required` exists for exactly that:

```yaml
sources:
- id: database
  apiVersion: sql.azure.m.upbound.io/v1beta1
  kind: MSSQLDatabase
  name: app
  required: true      # must exist before anything is generated
```

### Pruning

A resource that stops being returned is deleted — but not immediately. It has to
be absent from successful evaluations continuously for `--prune-delay`
(default 2 minutes) *and* for at least `--prune-threshold` of them.

The delay is the half that matters. Reconciles are event-driven and the applies
in a single pass generate watch events of their own, so a count of them measures
controller activity rather than elapsed time; in testing, a removed resource
accumulated 79 "consecutive evaluations" in 56 seconds. A managed resource drops
its status for as long as its provider takes to restart, and a program waiting
on that status legitimately stops returning what depends on it for that whole
period. Deleting on the first sight of that churns real infrastructure.

## The language

[Starlark](https://github.com/bazelbuild/starlark), embedded in-process. See
[docs/language.md](docs/language.md) for the full surface.

Untrusted authorship is the premise, so the language is bounded rather than
trusted: no `load()`, no recursion, no clock, no randomness, no I/O, an
execution-step budget and caps on result size. Every limit fails as a `Degraded`
condition with a distinct reason, not as a wedged controller.

Considered and rejected: **CEL** cannot emit variable-length structure, so
conditional inclusion and fan-out become YAML constructs that accumulate into a
bad programming language. **CUE** is an adoption tax with no cost budget for
untrusted evaluation. **WASM** needs a build pipeline per composition. **Go
templates** have unbounded recursion and `sprig` reaches the environment.

The evaluator sits behind an interface — a pure function over three JSON blobs —
so it is testable without a cluster and replaceable later.

## Operating it

```bash
weft --help
```

Notable flags:

| flag | default | why you would change it |
|---|---|---|
| `--prune-delay` | `2m` | how long a resource must be gone before deletion |
| `--source-finalizer-timeout` | `10m` | how long a finalizer may block somebody else's object |
| `--teardown-timeout` | `15m` | how long ordered teardown runs before cascading collection takes over |
| `--impersonate-groups` | `system:serviceaccounts,system:authenticated` | see [docs/rbac.md](docs/rbac.md) |
| `--max-steps` | `20000000` | the execution budget for one `compose()` |

### Uninstalling

If any `Weave` uses `finalize: true`, release those finalizers **before**
removing the controller, or the objects holding them cannot be deleted:

```bash
kubectl -n weft-system exec deploy/weft -- /weft reap --dry-run
kubectl -n weft-system exec deploy/weft -- /weft reap
helm uninstall weft --namespace weft-system
```

`helm uninstall` does not do this for you. It also leaves the CRD behind on
purpose: deleting it deletes every `Weave` in the cluster, and each one deleted
that way takes the resources it owns with it.

## Non-goals

- Generating a CRD per composition. One CRD, forever.
- A typed API surface for consumers. That is kro's model, and it requires the
  platform team this project exists to remove.
- Cross-namespace anything. Same-namespace is a hard rule and it is what makes
  ownership and garbage collection simple enough to need no machinery.
- Cloud provider integration. Weft composes whatever CRDs are installed and
  knows nothing about any of them.

## Status

`v1alpha1`, and the API group is provisional. Everything derives from one
constant so a rename is a single edit, with a test that keeps the kubebuilder
marker honest.

What is covered by tests: the evaluator (semantics, waiting, bounds,
ergonomics), inventory and wave planning, normalisation and ownership, RBAC
diagnostics, and naming. What has been verified by hand against a live cluster:
impersonation and denial, multi-phase staging, pruning, ordered teardown, and
source finalizers. There is no envtest suite for the reconciler yet — the
controller-level behaviours above were confirmed manually, and three of the bugs
fixed during development were found that way rather than by the unit tests.

## Development

```bash
make            # generate, fmt, vet, test, build
make run        # run against the current kubecontext
make lint-chart # lint and render the Helm chart
make deploy     # helm upgrade --install into a cluster
```

`make generate` regenerates the deepcopy functions, the CRD and the controller
ClusterRole, and copies the CRD into the chart. Tests fail if that copy is
stale, if the chart stops granting a permission controller-gen says the
controller needs, or if the chart renders arguments the binary cannot parse.
