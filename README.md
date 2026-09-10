# Weft

Weft is a Kubernetes operator for the job you currently do with a CronJob that
runs `kubectl get | template | kubectl apply`.

You write one namespaced custom resource, a `Weave`. It reads resources that
already exist in your namespace, runs a small program over them, and applies
whatever that program returns — reactively, whenever anything it read changes,
with ownership and garbage collection handled for you.

Two things make it different from the other tools in this space. You do not need
a platform team to install anything for you: a `Weave` is namespaced and there
is no second, cluster-scoped object to register. And every read and every write
runs as a ServiceAccount **you** name, so a `Weave` can never create anything
you could not have created by hand.

The name comes from weaving. The **warp** is the threads already on the loom —
resources you read but do not own. The **weft** is what you pass across them.

## What one looks like

```yaml
apiVersion: weft.run/v1alpha1
kind: Weave
metadata:
  name: app
  namespace: my-namespace
spec:
  # Every read and write below happens as this ServiceAccount.
  serviceAccountName: composer

  # Optional. Configuration, merged in order, later entries winning.
  variables:
  - configMap:
      name: platform          # a base somebody else maintains
  - values:
      prefix: demo            # overrides for this Weave

  program: |
    def compose(variable, observed):
        tenant = read("v1", "ConfigMap", "tenant")
        if not tenant:
            return wait("no tenant ConfigMap in this namespace yet")

        return {
            "settings": {
                "apiVersion": "v1",
                "kind": "ConfigMap",
                "metadata": {"name": variable.prefix + "-settings"},
                "data": {"tenantId": require(tenant, "data.tenantId")},
            },
        }
```

```console
$ kubectl get weave
NAME   READY   WAITING   AGE
app    True    Applied   13s
```

Create it, and `demo-settings` appears. Edit the `tenant` ConfigMap, and it
updates. Delete the `Weave`, and it goes away.

If `composer` cannot read the `tenant` ConfigMap or write the one being
generated, nothing is created and the `Weave` tells you exactly which
RoleBinding is missing, in a command you can paste.

## Install

No stable release is cut yet, so install the latest prerelease:

```bash
helm install weft oci://ghcr.io/alethic/charts/weft --devel \
  --namespace weft-system --create-namespace
```

Pin `--version 0.1.0-pre.23` (or whichever) for anything you care about; once
`1.0` exists, pin that instead. The chart and the controller image are published
to GitHub Packages on every build of `main`, both signed with cosign.

The chart is the supported install path and is documented in
[charts/weft/README.md](charts/weft/README.md). It ships sensible defaults, a
values schema that rejects malformed input at install time, and refuses to
render a handful of configurations that would only fail later.

Then, as an ordinary namespace user with `edit`, apply a `Weave`. There is no
second step and nothing for an administrator to register.

## Why you might want it

| | namespaced authoring | impersonated writes | status-reactive |
|---|---|---|---|
| **Weft** | **yes** | **yes** | **yes** |
| [kro](https://kro.run) | no — `ResourceGraphDefinition` is cluster-scoped and generates a CRD | no | yes |
| [Crossplane v2](https://crossplane.io) | XRs are namespaced, but XRDs and Compositions are cluster-scoped admin artifacts | no — the RBAC manager runs with `escalate` | yes |
| Metacontroller, Yoke ATC | no | no | yes |
| [Flux](https://fluxcd.io) | yes | **yes** — this is where the model comes from | no — `dependsOn` between whole Kustomizations, and `substituteFrom` limited to ConfigMaps and Secrets |
| Kyverno `generate` | yes | no — the background controller's own ServiceAccount does the creating, checked once at policy admission | yes |

If you already have Crossplane or a provider whose resources report status
asynchronously, the third column is the one that will matter to you day to day.

## How it behaves

### Three outcomes, not two

Resolution can succeed, fail permanently, or **not yet**. "Not yet" is the
normal case, not an edge case, and it has a condition of its own:

```console
$ kubectl get weave
NAME   READY   WAITING           AGE
app    False   FieldUnresolved   7s

$ kubectl get weave app -o jsonpath='{.status.conditions[?(@.type=="Waiting")].message}'
status.atProvider.principalId is not set on UserAssignedIdentity/demo-app
```

Existence does not unblock. **Field resolution** does. Applying a Crossplane
managed resource returns instantly, but its `status.atProvider` populates
minutes later — so a resource you created behaves exactly like an external one
that is not ready, and one rule covers both.

Alerting on `Degraded` is useful. Alerting on `Waiting` is not: it is the steady
state of any composition that spans several provisioning steps.

### Reading the cluster

```python
rg = read("azure.m.upbound.io/v1beta1", "ResourceGroup", "sweep-env")
tenants = select("v1", "ConfigMap", labels={"role": "tenant"})
```

Nothing is declared in the spec. The controller records what each pass actually
read and registers its watches from that recording, so a program that starts
reading something starts being woken by it, and one that stops, stops — there is
no second place to keep in step.

Both are namespaced and both are impersonated: `read` needs `get` on that kind,
`select` needs `list`. A resource that does not exist comes back falsey; a
resource you are not allowed to read is an error, never a quiet `None`.

### Staging happens through `observed`

`observed` holds what this `Weave` previously created, read back live. A
composition advances in phases by returning only what it can:

```python
def compose(variable, observed):
    out = {"identity": {...}}

    # The provider writes principalId back minutes after the apply returns.
    principal = get(observed, ["identity", "status", "atProvider", "principalId"])
    if not principal:
        pending("principalId on the app identity")
        return out                        # the identity exists; nothing else yet

    for role in variable.roles:
        out["ra-" + role.name] = {...}
    return out
```

`pending()` rather than `wait()` matters: a wait produces no resources at all,
so the identity would never be created and the composition could never advance.

### It manages only what it made

A program that names an object somebody else owns is **refused**, not allowed to
take it over — otherwise choosing a name would be enough to make Weft's owner
reference appear on it, and deleting the `Weave` would delete something Weft
never created.

Handing an existing resource over is deliberate, and the consent lives on the
object:

```bash
kubectl annotate configmap legacy weft.run/adopt=<weave-name>
```

The value is a pattern, so `weft.run/adopt='*'` consents to any `Weave` in that
namespace — the form for onboarding a set of objects at once, where naming the
same `Weave` on each of them says nothing extra.

[docs/ownership.md](docs/ownership.md) covers the whole story, including what
happens when a program is edited to change what a key addresses.

### Keys are identity, not position

The keys of the returned mapping identify the live objects. Reordering a list
cannot rename anything. Renaming a key is not a rename: it deletes one resource
and creates another.

### Ordering

Return order becomes apply order, and therefore reverse teardown order.
Cascading garbage collection would remove everything a `Weave` owns but in no
particular order, which is wrong for anything with a dependency.

For explicit control, annotate:

```python
"metadata": {"annotations": {"weft.run/wave": "1"}}
```

Either annotate every resource or none; mixing explicit waves with positional
defaults produces an order nobody wrote, and is rejected.

### Waiting on a resource nothing reads

A dependency that is never referenced is invisible to any design that infers
edges from expression references. Weft does not infer — the read is written, and
what an absence means is the line after it:

```python
if not read("sql.azure.m.upbound.io/v1beta1", "MSSQLDatabase", "app"):
    return wait("the database has not been created yet")
```

Which also expresses what a declared flag could not — a gate that depends on
configuration:

```python
if variable.useSql and not read("sql.azure.m.upbound.io/v1beta1", "MSSQLDatabase", "app"):
    return wait("SQL is enabled but the database is not there yet")
```

Turning `useSql` off releases the `Weave` with no other edit.

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

[Starlark](https://github.com/bazelbuild/starlark), embedded in-process — Python
you can read on sight, without the parts that make Python unsafe to run for
somebody else. See [docs/language.md](docs/language.md) for the full surface.

Untrusted authorship is the premise, so the language is bounded rather than
trusted: no `load()`, no recursion, no clock, no randomness, no I/O beyond
`read()` and `select()`, an execution-step budget and caps on result size and on
how much of a namespace one pass may pull in. Every limit fails as a `Degraded`
condition with a distinct reason, not as a wedged controller.

Considered and rejected: **CEL** cannot emit variable-length structure, so
conditional inclusion and fan-out become YAML constructs that accumulate into a
bad programming language. **CUE** is an adoption tax with no cost budget for
untrusted evaluation. **WASM** needs a build pipeline per composition. **Go
templates** have unbounded recursion and `sprig` reaches the environment.

## Trying it

[demo/](demo/) has seven `Weave`s covering staging, waiting, permission
failures, ordered teardown, pruning, held finalizers and the evaluator's bounds,
with a walkthrough in [demo/README.md](demo/README.md). [examples/](examples/)
has four longer compositions taken from real Crossplane workloads.

## Operating it

| flag | default | why you would change it |
|---|---|---|
| `--prune-delay` | `2m` | how long a resource must be gone before deletion |
| `--hold-timeout` | `10m` | how long a `finalize=True` read may block somebody else's object |
| `--teardown-timeout` | `15m` | how long ordered teardown runs before cascading collection takes over |
| `--max-reads` | `100` | distinct resources one evaluation may read |
| `--max-selected` | `500` | objects one `select()` may match |
| `--max-steps` | `20000000` | the execution budget for one `compose()` |
| `--impersonate-groups` | `system:serviceaccounts,system:authenticated` | see [docs/rbac.md](docs/rbac.md) |

`weft --help` lists the rest. The chart exposes all of them; see
[charts/weft/README.md](charts/weft/README.md).

Editing a `Weave` converges: what the program stopped returning is pruned, what
it started returning is applied, and what it changed under a stable key is
replaced rather than left behind.

### Metrics

`weft_weave_status` reports each `Weave` as ready, waiting or degraded, so
alerting on "degraded for more than N minutes" is a single expression. Alongside
it: resources owned per `Weave`, evaluation duration and outcome, applies,
prunes, permission denials by verb and resource, and active versus degraded
watches. Enable the `ServiceMonitor` with `metrics.serviceMonitor.enabled=true`.

### Uninstalling

Deleting a `Weave` deletes what it created, in reverse wave order. To keep the
resources instead, use the verb Kubernetes already has for it:

```bash
kubectl delete weave app --cascade=orphan
```

If any program uses `read(..., finalize=True)`, release those finalizers
**before** removing the controller, or the objects holding them cannot be
deleted:

```bash
kubectl -n weft-system exec deploy/weft -- /weft reap --dry-run
kubectl -n weft-system exec deploy/weft -- /weft reap
helm uninstall weft --namespace weft-system
```

`helm uninstall` does not do this for you. It also leaves the CRD behind on
purpose: deleting it deletes every `Weave` in the cluster, and each one deleted
that way takes the resources it owns with it.

## What it will not do

- Generate a CRD per composition. One CRD, forever.
- Offer a typed API surface for consumers. That is kro's model, and it requires
  the platform team this project exists to remove.
- Anything cross-namespace. Same-namespace is a hard rule, and it is what keeps
  ownership and garbage collection simple enough to need no machinery.
- Integrate with any cloud provider. Weft composes whatever CRDs are installed
  and knows nothing about any of them.

## Status

`v1alpha1`. The API group is provisional and the shape may still move.

Tested at four layers: the evaluator, inventory planning, normalisation and RBAC
diagnostics with no dependencies; every shipped example parsed, evaluated and
normalised, so a broken example fails the build; the Helm chart rendered and its
arguments parsed with the binary's own flag set; and the reconcile loop against a
real API server — apply and ownership, staging, pruning hysteresis, ordered
teardown, program faults, recreation, and steady-state stability.

That last layer matters most: every bug found during development was in the
reconcile loop, and none were visible without an API server to react to.

Verified by hand against a live cluster with RBAC enforced, and not yet
automated: impersonation *denial* specifically.

Contributions and the repository layout are covered in
[CONTRIBUTING.md](CONTRIBUTING.md).
