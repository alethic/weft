<p align="center">
  <img src="docs/icon.svg" alt="" width="96">
</p>

<h1 align="center">Weft</h1>

<p align="center">
  Reactive, namespace-scoped resource composition for Kubernetes — running as a ServiceAccount you choose.
</p>

<p align="center">
  <a href="https://github.com/alethic/weft/actions/workflows/weft.yml"><img src="https://github.com/alethic/weft/actions/workflows/weft.yml/badge.svg?branch=main" alt="CI"></a>
  <a href="https://github.com/alethic/weft/releases"><img src="https://img.shields.io/github/v/release/alethic/weft?sort=semver" alt="Latest release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/alethic/weft" alt="License"></a>
</p>

---

Weft replaces the CronJob that runs `kubectl get | template | kubectl apply`.

You write one namespaced custom resource, a **`Weave`**. It reads resources that
already exist in its namespace, runs a small [Starlark](https://github.com/bazelbuild/starlark)
program over them, and applies whatever the program declares. When anything it
read changes, it runs again. What it created is owned, kept current, pruned when
no longer declared, and torn down in order when the `Weave` is deleted.

Two properties set it apart:

- **No platform team required.** A `Weave` is namespaced and self-contained.
  There is no cluster-scoped definition to register, and no CRD generated per
  composition. Anyone with `edit` in a namespace can write one.
- **It can never exceed its author's permissions.** Every read and every write
  is impersonated as a ServiceAccount named in the `Weave`. The controller holds
  no write access of its own to anything a program generates, so a `Weave`
  cannot create or read anything that ServiceAccount could not.

## At a glance

```yaml
apiVersion: weft.alethic.solutions/v1
kind: Weave
metadata:
  name: app
  namespace: my-namespace
spec:
  serviceAccountName: composer      # every read and write happens as this identity

  variables:                        # optional configuration, merged in order
  - configMap:
      name: platform                # a shared base somebody else maintains
  - values:
      prefix: demo                  # overrides for this Weave

  program: |
    def compose(variable):
        tenant = read("v1", "ConfigMap", "tenant")
        if not tenant:
            return wait("no tenant ConfigMap in this namespace yet")

        resource({
            "apiVersion": "v1",
            "kind": "ConfigMap",
            "metadata": {"name": variable.prefix + "-settings"},
            "data": {"tenantId": require(tenant, "data.tenantId")},
        })
```

```console
$ kubectl get weave
NAME   READY   WAITING   AGE
app    True    Applied   13s
```

Create it and `demo-settings` appears. Edit the `tenant` ConfigMap and it
updates. Delete the `Weave` and it goes away.

If `composer` is not allowed to read `tenant` or write `demo-settings`, nothing
is created, and the `Weave`'s status contains the exact `kubectl` commands that
would grant what is missing.

## Features

- **Reactive.** Watches are derived from what the program actually read, so
  there is nothing to declare and nothing to keep in step.
- **Waits are first-class.** A program can be *not ready yet* — blocked on a
  resource that does not exist, or on a status field another controller has not
  filled in. That is reported as `Waiting`, distinct from `Ready` and
  `Degraded`, and resolves itself when the field appears.
- **Staged rollout.** A program can see what it previously created, read back
  live, and declare only what it can so far — the natural shape for
  infrastructure that provisions in steps.
- **Safe ownership.** Weft refuses to take over an object it did not create.
  Handing one over takes an annotation on that object. Individual resources can
  opt out of ownership so they outlive the `Weave`.
- **Ordered teardown.** Declaration order is apply order, and teardown runs in
  reverse. `needs=` lets independent siblings be deleted in parallel.
- **Cautious pruning.** A resource that disappears from the program is deleted
  only after it has stayed gone for a configurable delay, so a provider restart
  does not churn real infrastructure.
- **Bounded evaluation.** Programs are untrusted by design: no I/O beyond
  `read()` and `select()`, no recursion, no clock, and step, size and read
  budgets. Exceeding one fails that `Weave`, never the controller.
- **Provider-agnostic.** Weft composes whatever CRDs are installed —
  Crossplane managed resources, cert-manager, your own — and knows nothing
  about any of them.

## Install

Requires Kubernetes 1.27 or later and Helm 3.8 or later.

```bash
helm install weft oci://ghcr.io/alethic/charts/weft \
  --namespace weft-system --create-namespace
```

That installs the latest release. Add `--version` with a version from the
[releases page](https://github.com/alethic/weft/releases) to pin one, which is
what you want anywhere you care about repeatability.

Each chart pins the controller image (`ghcr.io/alethic/weft`) by digest, so an
installed release cannot drift under a moved tag. The image and the chart are
both signed with [cosign](https://github.com/sigstore/cosign), keyless, and
each release lists the image digest. To verify an image before you run it:

```bash
cosign verify ghcr.io/alethic/weft@<digest> \
  --certificate-identity-regexp '^https://github.com/alethic/weft/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Every chart value is documented in [charts/weft/README.md](charts/weft/README.md),
including hardening options such as confining impersonation to a set of
namespaces.

## Quick start

With the controller installed, everything below runs as an ordinary namespace
user — there is no administrator step.

```bash
kubectl create namespace weft-quickstart
kubectl -n weft-quickstart create serviceaccount composer
kubectl -n weft-quickstart create rolebinding composer-edit \
  --clusterrole=edit --serviceaccount=weft-quickstart:composer
kubectl -n weft-quickstart create configmap tenant --from-literal=tenantId=acme-42
```

```bash
kubectl apply -f - <<'EOF'
apiVersion: weft.alethic.solutions/v1
kind: Weave
metadata:
  name: hello
  namespace: weft-quickstart
spec:
  serviceAccountName: composer
  program: |
    def compose(variable):
        tenant = read("v1", "ConfigMap", "tenant")
        if not tenant:
            return wait("no tenant ConfigMap yet")

        resource({
            "apiVersion": "v1",
            "kind": "ConfigMap",
            "metadata": {"name": "settings"},
            "data": {"tenantId": require(tenant, "data.tenantId")},
        })
EOF
```

```bash
kubectl -n weft-quickstart get weave,configmap
kubectl -n weft-quickstart patch configmap tenant -p '{"data":{"tenantId":"acme-43"}}'
kubectl -n weft-quickstart get configmap settings -o jsonpath='{.data.tenantId}'
```

The `edit` binding is the quick path. In production, scope the ServiceAccount
to exactly what its program needs — [docs/rbac.md](docs/rbac.md) explains how,
and a `Weave` that is missing a permission tells you which one.

To go further, [demo/](demo/README.md) walks through seven `Weave`s covering
staging, waiting, permission failures, ordered teardown, pruning, finalizers
and the evaluator's limits, using only core Kubernetes types.
[examples/](examples/) has longer compositions taken from real Crossplane
workloads.

## Documentation

| | |
|---|---|
| [The composition language](docs/language.md) | `compose()`, `resource()`, `read()`, `select()`, `wait()`, `pending()`, variables, and every built-in |
| [Ownership](docs/ownership.md) | what Weft manages, adopting existing objects, opting out, renaming, deletion |
| [Permissions](docs/rbac.md) | what a `Weave`'s ServiceAccount needs, and what the controller itself holds |
| [Operating Weft](docs/operating.md) | status conditions, pruning, teardown, controller flags, metrics, uninstalling |
| [Design and alternatives](docs/design.md) | how Weft compares with kro, Crossplane, Flux and Kyverno; why Starlark; non-goals |
| [Helm chart](charts/weft/README.md) | installation, every value, common configurations |
| [Moving from weft.run](docs/migrating.md) | for installations that predate `weft.alethic.solutions/v1` |
| [Security](SECURITY.md) | threat model, hardening, reporting a vulnerability |

## Releases and compatibility

Releases, with their notes, are on the
[releases page](https://github.com/alethic/weft/releases). The badge at the
top shows the current one.

The API is `weft.alethic.solutions/v1`. A change that would break a `v1`
Weave would come as a new API version, not as a change to this one.

Weft itself is pre-1.0. The controller, the chart and their defaults may still
change from one release to the next, and every such change is called out in
that release's notes.

Builds of `main` between releases are published too, as prereleases such as
`0.1.1-pre.3`. Helm only installs those with `--devel`; they are for trying a
fix before it is released, not for running.

Upgrading from a build that predates `weft.alethic.solutions/v1`? Weaves in the
older `weft.run/v1alpha1` group keep working unchanged, and
[docs/migrating.md](docs/migrating.md) explains how to move them across.

CI runs the reconcile loop against a real API server on Kubernetes 1.27 (the
minimum supported), 1.33 and 1.37, alongside unit tests of the evaluator and
planner, rendering tests of the Helm chart, and a check that every shipped
example evaluates.

## Contributing

Issues and pull requests are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) covers
building, the test layers, and the two design properties a change must not
weaken. Please report security issues privately as described in
[SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE).
