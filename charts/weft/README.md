# weft

A Kubernetes operator that lets an ordinary namespace user apply one custom
resource that reads existing resources in their namespace and generates others
from them, reactively, running as their own ServiceAccount.

See the [project README](../../README.md) for what a `Weave` is and
[docs/rbac.md](../../docs/rbac.md) for the permission model.

## Install

```bash
helm install weft oci://ghcr.io/alethic/charts/weft \
  --version 0.1.0 \
  --namespace weft-system --create-namespace
```

The chart lives in GitHub Packages as an OCI artifact, published on every build
of `main`. Omitting `--version` takes the newest, which on a repository that
publishes every build means a prerelease such as `0.1.0-pre.9`; pin it for
anything you care about.

```bash
helm show chart oci://ghcr.io/alethic/charts/weft --version 0.1.0
```

The chart's `appVersion` selects the image tag, so a chart and the controller it
installs always carry the same version. A released chart additionally pins
`image.digest`, so it cannot drift under a moved tag.

Or from a checkout:

```bash
helm install weft ./charts/weft --namespace weft-system --create-namespace
```

The chart creates no namespace of its own; use `--create-namespace`.

## Uninstall

**Release finalizers first.** If any `Weave` declares a source with
`finalize: true`, Weft has placed finalizers on objects it does not own. Remove
the controller without releasing them and those objects cannot be deleted, and
neither can their namespaces:

```bash
kubectl -n weft-system exec deploy/weft -- /weft reap --dry-run
kubectl -n weft-system exec deploy/weft -- /weft reap
helm uninstall weft --namespace weft-system
```

To remove the controller but keep everything the Weaves produced, delete them
with `kubectl delete weave --all --cascade=orphan` first. Weft honours orphan
propagation: the resources stay and simply stop being managed.

The CRD is left behind on purpose (`crds.keep`). Deleting it deletes every
`Weave` in the cluster, and each `Weave` deleted that way takes the resources it
owns with it — an uninstall should not be able to destroy infrastructure.
Remove it deliberately when you mean to:

```bash
kubectl delete crd weaves.weft.run
```

## Values

### Deployment

| key | default | |
|---|---|---|
| `replicaCount` | `1` | Leader election means only one replica reconciles; a second buys faster handover, not throughput. |
| `image.repository` | `ghcr.io/alethic/weft` | |
| `image.tag` | `""` | Falls back to the chart's `appVersion`, which is the version the chart itself carries, so the two never disagree. |
| `image.digest` | `""` | Wins over `tag` when set. Pin this in production. |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | |
| `resources.requests.cpu` | `50m` | |
| `resources.requests.memory` | `128Mi` | |
| `resources.limits.memory` | `512Mi` | No CPU limit by default: a controller's work is bursty and a CPU limit turns a burst into CFS throttling, which shows up as reconcile latency. |
| `podDisruptionBudget.enabled` | `false` | Worth enabling when `replicaCount > 1`. |
| `terminationGracePeriodSeconds` | `30` | Long enough to finish the reconcile in flight and release the lease. |
| `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, `priorityClassName` | — | Standard scheduling controls. |
| `podAnnotations`, `podLabels`, `extraEnv`, `extraVolumes`, `extraVolumeMounts` | — | |

### CRD

| key | default | |
|---|---|---|
| `crds.install` | `true` | Turn off when CRDs are managed separately. |
| `crds.keep` | `true` | Keep the CRD on uninstall. Read the warning above before changing this. |

### RBAC

| key | default | |
|---|---|---|
| `rbac.create` | `true` | |
| `rbac.impersonation.scope` | `cluster` | `cluster` lets Weft impersonate a ServiceAccount in any namespace. `namespaced` confines it to `rbac.impersonation.namespaces`. |
| `rbac.impersonation.namespaces` | `[]` | Required when scope is `namespaced`. |
| `rbac.allowUnrestrictedGroupImpersonation` | `false` | See below. |
| `serviceAccount.create` / `.name` / `.annotations` | | |

The controller's whole grant is worth reading for what is absent: **no write
access to any resource type a composition might generate**. It cannot create a
ConfigMap on its own account. It can only become a ServiceAccount that already
could.

Group impersonation is pinned to the two groups Weft actually uses, because an
unrestricted grant would let anyone who compromises the controller impersonate
`system:masters`. If you add a templated group such as
`system:serviceaccounts:{namespace}`, the chart **refuses to render** until you
set `rbac.allowUnrestrictedGroupImpersonation=true`, and it explains why.

Note that `--set` cannot carry a value containing braces. Use a values file:

```yaml
rbac:
  allowUnrestrictedGroupImpersonation: true
controller:
  impersonateGroups:
    - system:serviceaccounts
    - system:serviceaccounts:{namespace}
    - system:authenticated
```

### Controller

| key | default | |
|---|---|---|
| `controller.watchNamespace` | `""` | Restrict to one namespace. Empty watches all. |
| `controller.leaderElection.enabled` | `true` | |
| `controller.leaderElection.id` | `weft.run` | Change only to run two independent installations. |
| `controller.impersonateGroups` | `[system:serviceaccounts, system:authenticated]` | Impersonating the username alone yields an identity weaker than the real ServiceAccount, so RBAC bound to `system:authenticated` would silently not apply. |
| `controller.pruneDelay` | `2m` | How long a resource must be continuously absent before deletion. Raise it above your provider's restart time. |
| `controller.pruneThreshold` | `3` | Successful evaluations it must also be absent from. |
| `controller.sourceFinalizerTimeout` | `10m` | How long a finalizer on somebody else's object may block it. |
| `controller.teardownTimeout` | `15m` | How long ordered teardown runs before cascading collection takes over. |
| `controller.pollInterval` | `30s` | Requeue period when watches could not be established. |
| `controller.backstopInterval` | `10m` | Requeue period in the normal case. |
| `controller.degradedRetry` | `30s` | Short, because the fix is usually a RoleBinding and RBAC is not watched. |
| `controller.evaluator.maxSteps` | `20000000` | Execution budget for one `compose()`. |
| `controller.evaluator.maxResources` | `250` | |
| `controller.evaluator.maxValues` | `250000` | |
| `controller.evaluator.programCacheSize` | `128` | |
| `controller.watches.resync` | `10m` | |
| `controller.watches.lifetime` | `30m` | Bounds how long a revoked grant keeps being honoured, since authorisation is checked when a watch opens. |
| `controller.log.level` | `info` | |
| `controller.log.encoder` | `json` | |
| `controller.extraArgs` | `[]` | Escape hatch for flags the chart does not model. |

### Observability

| key | default | |
|---|---|---|
| `metrics.enabled` | `true` | |
| `metrics.port` | `8080` | |
| `metrics.service.enabled` | `true` | |
| `metrics.serviceMonitor.enabled` | `false` | Requires the Prometheus Operator. |
| `metrics.serviceMonitor.interval` | `30s` | |
| `health.port` | `8081` | |
| `livenessProbe`, `readinessProbe` | | Standard probe knobs. |

## What the chart refuses to do

Rendering fails, with an explanation, for configurations that would be broken or
dangerous in a way you would only discover later:

- a templated impersonation group without
  `rbac.allowUnrestrictedGroupImpersonation`
- `rbac.impersonation.scope=namespaced` with no namespaces, which would leave
  Weft unable to impersonate anywhere
- `replicaCount > 1` with leader election off, which would have several replicas
  reconciling the same `Weave` and applying over each other
- both `podDisruptionBudget.minAvailable` and `.maxUnavailable`

`values.schema.json` additionally rejects malformed durations, ports out of
range, and unknown enum values at install time rather than at runtime.

## Common configurations

**Confined to two namespaces:**

```yaml
controller:
  watchNamespace: ""
rbac:
  impersonation:
    scope: namespaced
    namespaces: [team-a, team-b]
```

**Slow provider — do not prune through a restart:**

```yaml
controller:
  pruneDelay: 30m
```

**Highly available:**

```yaml
replicaCount: 2
podDisruptionBudget:
  enabled: true
  minAvailable: 1
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        app.kubernetes.io/name: weft
```
