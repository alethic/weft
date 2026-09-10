# Weft — design brief

Hand this to Claude Code at the root of `github.com/alethic/weft`.

## What we're building

A Kubernetes operator that lets an **ordinary namespace user** apply one custom
resource that reads arbitrary existing resources in their namespace and
generates other resources from them — reactively, with proper ownership and
garbage collection.

The name comes from weaving: the **warp** is the existing threads held on the
loom (resources we read but do not own); the **weft** is what we pass across
them (resources we create). The CR kind is `Weave`.

## The two properties that define the project

Everything else is negotiable. These are not.

1. **Namespace-scoped authoring.** A user with `edit` in their own namespace can
   create a `Weave` and get resources. No cluster-scoped object is created at
   runtime, no CRD is generated per composition, no platform team is in the
   loop, no cluster-admin.
2. **Applies run as the user's ServiceAccount.** `spec.serviceAccountName` names
   a SA in the same namespace; the controller impersonates it for every read and
   every write. The controller holds `impersonate` rights, not blanket write
   access. A user can never cause the creation of anything their own SA could
   not create.

## Why existing tools don't fit

- **kro** — `ResourceGraphDefinition` is cluster-scoped and generates a CRD.
  Its Helm chart defaults to a ClusterRole with full control of every resource
  type; their own docs note that anyone who can create an RGD effectively has
  cluster admin. The alternative "aggregation" mode requires a cluster admin to
  author a labeled ClusterRole per RGD.
- **Crossplane v2** — XRs are namespaced now, but XRDs and Compositions are
  cluster-scoped admin artifacts, the RBAC manager runs with `escalate`, and
  there is no impersonation anywhere in the pipeline.
- **Metacontroller / Yoke ATC** — both cluster-scoped definitions, both apply
  with the controller's own identity.
- **Flux** — has exactly the impersonation model we want
  (`spec.serviceAccountName`, `--default-service-account`) but no
  status-reactive templating; only `dependsOn` between whole Kustomizations and
  `postBuild.substituteFrom` limited to ConfigMaps/Secrets.
- **Kyverno generate** — namespaced `Policy` with `synchronize: true` is the
  closest reactive templating available to a namespace user, but the background
  controller's SA does the creating (checked once at policy admission), which is
  the confused-deputy model we are trying to avoid.

The gap — namespaced + impersonated + status-reactive — is unoccupied.

## API shape

One namespaced CRD. Group `weft.run`, version `v1alpha1`, kind `Weave`.

> The group is provisional and may change before v1alpha1 is published.
> Keep it in one constant and derive everything (kubebuilder markers, RBAC,
> finalizer names, annotation keys) from it, so a rename is one edit.

```yaml
apiVersion: weft.run/v1alpha1
kind: Weave
metadata:
  name: app-roleassignments
  namespace: sweep-labs
spec:
  serviceAccountName: sweep-env-compose

  # Static configuration. Plain YAML, not expressions.
  inputs:
    subscriptionId: "..."
    providerConfigRef: {name: azure-default}

  # External resources to read. MUST be static and declarative — this is what
  # the RBAC precheck and the watch registration key off. Never derived from
  # evaluation.
  sources:
  - id: resourceGroup
    apiVersion: azure.m.upbound.io/v1beta1
    kind: ResourceGroup
    name: sweep-env
    required: true          # gate ordering even when no field is referenced
    # optional: finalize: true  (see "Finalizers on sources")

  # The evaluator.
  program: |
    def compose(inputs, sources, observed):
        ...
status:
  conditions: [...]
  inventory: [...]          # what we created, with apply order
```

## Semantics

### Evaluation contract

`compose(inputs, sources, observed)` returns a map of `key -> resource dict`.
Runs on every reconcile.

- `sources` — declared external resources, resolved via **impersonated get**, or
  `None` if absent/unreadable.
- `observed` — resources this Weave previously created, with live status.
  Self-reference is how multi-phase advancement works (create an identity, wait
  for its `status.atProvider.principalId`, then create role assignments that use
  it). Crossplane's observed/desired model, evaluated in-process.
- Return value keys are the **stable inventory identity**. Never index-based —
  reordering a list must not rename live objects.
- Returning fewer resources than last time means those resources are pruned
  (deleted). A distinct `wait("reason")` sentinel is available for "cannot
  proceed, and here's a legible reason for a status condition."

### Waiting

Three outcomes, not two: resolved, permanently invalid, and **not yet**.
"Not yet" is the normal case, not an edge case. A missing source, a missing
status field, or a `wait()` return all produce a `Waiting` condition naming the
specific unresolved thing.

Existence does not unblock; **field resolution** does. Applying a Crossplane MR
returns instantly but its `status.atProvider` populates minutes later. So an
output we created behaves exactly like an external source that isn't ready —
one rule for both.

### Identity and permissions

- Every read *and* every write goes through the impersonated client. No
  exceptions. Creation authority is not read authority — a SA may have `create`
  and not `get`, and the whole data path is "read a value, write it into a
  sibling," so an unimpersonated read is a real privilege leak.
- If the SA can't `get` a source, the Weave fails with a condition naming the
  missing verb and kind — the controller must not fill the gap with its own
  privileges.
- The condition message should be copy-pasteable into a RoleBinding. Debugging
  RBAC is the primary user experience of this tool.

### Watches

- Watch set is the union of declared `sources` plus our own outputs, per
  namespace, keyed by `(identity, GVK, namespace)` with refcounting so watches
  are torn down when the last referencing Weave goes away.
- Prefer namespace-scoped watches over a cluster-wide ClusterRole: it lets the
  *user* grant read access with a RoleBinding they can create themselves, which
  preserves property (1). No cluster-scoped RBAC edits after install.
- Authorization is checked when a watch is established, not continuously, so
  bound watch lifetimes and re-verify permissions each reconcile.
- If the SA lacks `list`/`watch` but has `get`, degrade to polling with backoff
  rather than failing.
- Acceptable alternative: controller-identity **metadata-only** watch purely as
  a change trigger (`PartialObjectMetadata`), with the impersonated `get` as the
  sole path to any field value. If you do this, do NOT keep a full-object cache
  you aren't allowed to read from — someone will read it.

### Ownership, GC, deletion order

- All outputs are in the Weave's own namespace, so plain `ownerReferences`
  cascading GC works. The KEP-3659 blockers (no cross-namespace ownerRefs,
  namespaced can't own cluster-scoped) do not apply. **Do not build ApplySet
  machinery for ownership.**
- But cascade is *unordered*, and self-references create a real internal DAG, so
  teardown must be reverse-topological. Record an apply order per inventory item
  and use a finalizer on the Weave to delete in reverse waves.
- Sources are never owned and never cascade-deleted. We observe them; we don't
  own them.

### Finalizers on sources (opt-in)

Optionally place a finalizer on a *watched* resource so our outputs are torn
down before it disappears. Rules:

- Declared on the Weave (`sources[].finalize: true`), not inferred.
- Requires `update` on the source's GVR (and possibly its `finalizers`
  subresource) under impersonation. Missing permission = condition, not a
  silent downgrade.
- **Hard timeout.** After N minutes of failed teardown, release the finalizer
  anyway and log loudly. Blocking someone else's object — and their namespace
  deletion — forever is worse than the ordering violation.
- Controller uninstall must reap its own finalizers. Use a fixed, discoverable
  prefix so a human can enumerate and strip them with kubectl.
- Re-check on every reconcile that we can still remove what we added; a revoked
  RoleBinding otherwise deadlocks.
- Understand the limit: this orders *API object* deletion, not the destruction
  of whatever the object represents.

### Pruning hysteresis

When a resource stops appearing in the returned set it gets deleted. Crossplane
MRs transiently drop status during provider restarts, so a naive implementation
churns real cloud resources. Require a condition to hold for N consecutive
reconciles before tearing down.

## Language

**Starlark** (`go.starlark.net`), embedded in-process.

- `resolve.AllowRecursion = false`; no `load()` (or an in-memory allowlist only);
  no `time`, no `math.random`, nothing with I/O.
- `thread.SetMaxExecutionSteps()` for the budget. Untrusted authors are the
  premise, so runtime bounding replaces apply-time type checking.
- `Freeze()` injected values.
- `starlarkstruct` for `inputs`/`sources`/`observed` (attribute access reads
  well); plain dicts for returned resources.
- Register a `get(obj, "status.atProvider.principalId", default)` builtin
  **early**. Without it, nested access becomes defensive `.get()` chains and the
  ergonomics collapse.
- Compile once, cache `*starlark.Program` by content hash, fresh `Thread` per
  reconcile (threads are not concurrency-safe).

Considered and rejected: CEL (can't emit variable-length structure; conditional
inclusion and fan-out become YAML constructs that accumulate into a bad
programming language — this is how patch-and-transform died). CUE (adoption tax,
no cost budget for untrusted evaluation). WASM (build pipeline per composition
defeats "quick and easy"). Go templates (unbounded recursion, sprig reaches the
environment).

Keep the evaluator behind an interface — pure function over three JSON blobs —
so it's testable without a cluster and replaceable later.

## Non-goals

- Generating a CRD per composition. One CRD, forever.
- A typed API surface for consumers. That's kro's model and it requires the
  platform team we're trying to remove.
- Cross-namespace anything. Same-namespace is a hard rule and it's what makes
  ownership and GC simple.
- Cloud provider integration. We compose whatever CRDs are installed.

## Build order

1. **Impersonation spike, before any scaffolding.** A `main.go` that sets
   `rest.Config.Impersonate.UserName` to `system:serviceaccount:<ns>:<name>`,
   server-side applies a hardcoded ConfigMap with a field manager, run against
   one SA with `edit` and one with nothing. Confirm the 403 can be decomposed
   into verb + resource for a status condition. This proves the one mechanism
   nothing else in the ecosystem has.
2. **Evaluator, standalone.** `compose()` over fixture JSON, no cluster.
3. `kubebuilder init` + `create api`, wire 1 and 2 together.
4. Inventory, apply order, finalizer, reverse-order teardown.
5. Dynamic watch registry with refcounting.
6. Source finalizers, hysteresis.

## Test corpus

There are twelve real compositions to port, currently implemented as
gomplate-in-CronJob in the `sweep-env` Helm chart (ConfigMap holds a template
plus a `resources=(...)` list; a CronJob every 5 min pipes
`kubectl get -o yaml` through gomplate into `kubectl apply --server-side`).

Use them as fixtures — the `kubectl get` output they already produce *is* the
`sources`/`observed` input. Notable cases:

- `app-release-values.yaml` — nine sources, near-pure leaf substitution,
  produces a nested YAML document as a ConfigMap string value.
- `app-roleassignments.yaml` — seven outputs from a role table; the fan-out case.
- `app-identity.yaml` + `app-roleassignments.yaml` — should merge into one Weave
  via self-reference; today they're two CronJobs chained by polling.
- `sqlcmd.yaml` — a CronJob that generates a CronJob, because there was no way
  to say "run when the identity changes." Should collapse.
- One composition declares `MSSQLDatabase` as a source, asserts it exists, and
  never reads a field from it — a pure ordering gate. Any design that infers
  dependencies only from expression references (kro's model) silently drops this
  edge. This is why `sources[].required` exists.

Twelve CronJobs should become three or four Weaves.

## Known weaknesses of the current cronjob implementation (things to fix)

- `kubectl apply` with no `--prune`: removing a resource from a template orphans
  the live object forever. Reclaim is a manual ConfigMap rename.
- Latency compounds ~5 min per dependency hop.
- A source disappearing does nothing; outputs remain.
- Errors land in Job logs, not on any object's status.
- The exec ServiceAccount is bound to `cluster-admin`.

---

# Appendix A — the twelve existing compositions

Extracted from the `sweep-env` Helm chart. Each is one ConfigMap (holding a
gomplate template, a `resources=(...)` source list, and the pipe scripts) plus a
CronJob running every 5 minutes. `{{ fullname }}` resolves to the release name,
e.g. `sweep-env`.

Every one of them lists its own exec ConfigMap as a source. That exists only to
obtain a `uid` for the `ownerReferences` on the outputs, and **disappears in
Weft** — the Weave is the owner. It is omitted from the source lists below.

| # | file | sources | outputs |
|---|------|---------|---------|
| 1 | `app-release-values.yaml` | UserAssignedIdentity `-app`, UserAssignedIdentity `-keda`, Vault, ServiceBusNamespace, MSSQLServer, MSSQLDatabase, storage Account, cosmosdb Account | 1 ConfigMap (nested values.yaml document) |
| 2 | `azure/app-identity.yaml` | ResourceGroup | 2 UserAssignedIdentity (`-app`, `-keda`) |
| 3 | `azure/app-roleassignments.yaml` | ResourceGroup, UAI `-app`, UAI `-keda`, ServiceBusNamespace, cosmosdb Account | 5 RoleAssignment + 2 SQLRoleAssignment |
| 4 | `azure/env-identity.yaml` | ResourceGroup | 1 UserAssignedIdentity (`-env`) |
| 5 | `azure/env-roleassignments.yaml` | ResourceGroup, UAI `-env` | 1 RoleAssignment |
| 6 | `azure/roleassignments.yaml` | ConfigMap `tenant`, ResourceGroup | 1 RoleAssignment |
| 7 | `azure/admins-roleassignments.yaml` | ResourceGroup, cosmosdb Account | 6 RoleAssignment + 2 SQLRoleAssignment |
| 8 | `azure/sql/sqladmin-identity.yaml` | ResourceGroup | 1 UserAssignedIdentity (`-sqladmin`) |
| 9 | `azure/sql/sqlserver.yaml` | UAI `-sqladmin` | 1 MSSQLServer |
| 10 | `azure/sql/sqlcmd.yaml` | UAI `-app` | 1 CronJob (which itself runs sqlcmd) |
| 11 | `azure/sql/admins-sqlcmd.yaml` | per-admin, Helm-ranged | 1 CronJob per admin principal |
| 12 | `sqladmin-serviceaccount.yaml` / `env-serviceaccount.yaml` | UAI `-sqladmin` / UAI `-env` | 1 ServiceAccount each (workload-identity annotation) |

## Fields actually read

Almost everything is one of these five shapes. The evaluator has to make all of
them ergonomic:

- `status.atProvider.clientId` — UserAssignedIdentity
- `status.atProvider.principalId` — UserAssignedIdentity
- `status.atProvider.id` — ResourceGroup, ServiceBusNamespace, cosmosdb Account
- `status.atProvider.vaultUri` / `.endpoint` / `.fullyQualifiedDomainName`
- `metadata.annotations["crossplane.io/external-name"]` — storage Account,
  ServiceBusNamespace, ResourceGroup

Note the last one: it's a map index with no schema, written back asynchronously
by Crossplane. Treat a missing `crossplane.io/external-name` as **Waiting**, the
same as a missing status field — not as a hard error.

## The merges these enable

- **2 + 3** → one Weave. `app-identity` creates the identities; `app-roleassignments`
  reads their `principalId`. Today two CronJobs chained only by both polling.
- **4 + 5**, **8 + 9** → same pattern, same merge.
- **9 + 10** → the SQL server, then the grant. `sqlcmd.yaml` currently generates a
  *CronJob that generates nothing* — a second-order reconciler that exists purely
  because there's no way to say "run when the identity changes." With real
  watches it becomes a Job keyed off the identity's generation.
- **1** stays separate; it's a leaf-substitution fan-in with no outputs anyone
  else reads.

Target: twelve CronJobs → three or four Weaves.

## Specific cases to get right

**Ordering gate with no field read (#1).** `MSSQLDatabase` is asserted present
and never read. Any design inferring dependencies purely from expression
references drops this edge silently. `sources[].required` exists for this.

**Cross-chart source (#6).** `ConfigMap/tenant` is not created by this chart at
all. Provenance of a source is arbitrary — this is the general case, not an edge
case, and it's why every read must be impersonated.

**Helm-level fan-out (#7, #11).** These `range` over `.Values.azure.permissions.admin`
at *build* time, producing N copies of the whole CronJob. In Weft this should be
a loop inside one program over an `inputs` list — one Weave, N outputs.

**Nested document as a string (#1).** The output is a ConfigMap whose
`data.values.yaml` is an entire YAML document. Serializing a structure to a
string is the single most common thing these compositions do (Helm values, SQL
scripts, connection strings). It must not be a corner case in the evaluator.

**A real bug they already hit.** `pipeline.sh` originally used a bare `wait`,
which reports success regardless of child exit status. When only some resources
resolved, `kubectl get` still emitted a List of the rest and exited non-zero, so
a partial List reached the template and produced "resource not found" instead of
the actual API error. Lesson for the controller: **never evaluate against a
partially-resolved source set**, and surface the API error rather than the
downstream symptom.
