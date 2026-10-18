# Operating Weft

How a `Weave` reports what it is doing, how the controller decides what to
delete and when, and the knobs an operator has. Installation and chart values
are in [charts/weft/README.md](../charts/weft/README.md).

## Reading a Weave's status

```console
$ kubectl get weave
NAME   READY   WAITING           AGE
app    False   FieldUnresolved   7s
```

Resolution has three outcomes, not two: it succeeded, it failed permanently, or
it cannot proceed **yet**. Each has its own condition.

| condition | meaning |
|---|---|
| `Ready` | the program ran and everything it declared is applied |
| `Waiting` | the program is blocked on something that has not happened yet; the message says what |
| `Degraded` | a permanent error — a program fault, a permission denial, a limit exceeded; the reason is distinct for each |

```console
$ kubectl get weave app -o jsonpath='{.status.conditions[?(@.type=="Waiting")].message}'
status.atProvider.principalId is not set on UserAssignedIdentity/demo-app
```

"Not yet" is the normal case. Existence does not unblock a program — **field
resolution** does. Applying a Crossplane managed resource returns instantly, but
its `status.atProvider` populates minutes later, so a resource the `Weave`
created behaves exactly like an external one that is not ready, and one rule
covers both.

Alert on `Degraded`. Do not alert on `Waiting`: it is the steady state of any
composition that spans several provisioning steps.

A permission denial is reported as `Degraded` with the exact `kubectl` commands
that would grant what is missing. See [rbac.md](rbac.md).

## Apply order and teardown

Declaration order becomes apply order, and therefore reverse teardown order.
Cascading garbage collection would remove everything a `Weave` owns, but in no
particular order — wrong for anything with a dependency. The `if` statements in
a composition already put a thing before the things that consume it, so the
default is correct with nothing annotated.

What the default costs is parallelism: teardown waits for each object to be
confirmed gone before the next, and a managed resource can take minutes to
delete. Siblings that depend on something in common but not on each other can
say so with `needs`:

```python
identity = resource({...})

for role in variable.roles:
    resource({...}, needs=identity)
```

Seven role assignments naming one identity come out at the same depth and are
deleted together — one wait instead of seven. Waves are derived from what each
resource names, never written as a number, and `needs` takes the value
`resource()` returned rather than a name, so a misspelling is an undefined
variable with a backtrace rather than an ordering that is quietly wrong.

If ordered teardown has not finished after `--teardown-timeout`, cascading
garbage collection takes over.

## Pruning

A resource the program stops declaring is deleted — but not immediately. It
has to be absent from successful evaluations continuously for `--prune-delay`
(default 2 minutes) *and* for at least `--prune-threshold` of them.

The delay is the half that matters. Reconciles are event-driven, and the applies
in a single pass generate watch events of their own, so a count of evaluations
measures controller activity rather than elapsed time; in testing, a removed
resource accumulated 79 "consecutive evaluations" in 56 seconds. A managed
resource loses its status for as long as its provider takes to restart, and a
program waiting on that status legitimately stops declaring what depends on it
for that whole period. Deleting on first sight of that churns real
infrastructure.

Set `--prune-delay` above the longest restart of your slowest provider.

Editing a `Weave` converges the same way: what the program stopped declaring is
pruned, and what it started declaring is applied.

## Controller flags

| flag | default | why you would change it |
|---|---|---|
| `--prune-delay` | `2m` | how long a resource must be undeclared before it is deleted |
| `--prune-threshold` | `3` | how many successful evaluations it must be absent from |
| `--hold-timeout` | `10m` | how long a `hold=True` read may block deletion of somebody else's object |
| `--teardown-timeout` | `15m` | how long ordered teardown runs before cascading collection takes over |
| `--max-steps` | `20000000` | the execution budget for one `compose()` |
| `--max-resources` | `250` | resources one evaluation may declare |
| `--max-reads` | `100` | distinct resources one evaluation may read |
| `--max-selected` | `500` | objects one `select()` may match |
| `--namespace` | *(all)* | restrict the controller to one namespace |
| `--impersonate-groups` | `system:serviceaccounts,system:authenticated` | see [rbac.md](rbac.md) |

`weft --help` lists the rest. Every flag is exposed as a chart value; see
[charts/weft/README.md](../charts/weft/README.md).

## Metrics

`weft_weave_status` reports each `Weave` as ready, waiting or degraded, so
alerting on "degraded for more than N minutes" is a single expression.
Alongside it: resources owned per `Weave`, evaluation duration and outcome,
applies, prunes, permission denials by verb and resource, and active versus
degraded watches.

Enable the `ServiceMonitor` with `metrics.serviceMonitor.enabled=true`.

## Deleting a Weave

Deleting a `Weave` deletes what it created, in reverse wave order. To keep the
resources instead, use the verb Kubernetes already has for it:

```bash
kubectl delete weave app --cascade=orphan
```

Resources annotated `weft.run/owned: "false"` are never deleted with the
`Weave`. See [ownership.md](ownership.md).

## Uninstalling the controller

If any program uses `read(..., hold=True)`, release those finalizers **before**
removing the controller, or the objects holding them cannot be deleted:

```bash
kubectl -n weft-system exec deploy/weft -- /weft reap --dry-run
kubectl -n weft-system exec deploy/weft -- /weft reap
helm uninstall weft --namespace weft-system
```

`helm uninstall` does not do this for you. It also leaves the CRD behind on
purpose: deleting the CRD deletes every `Weave` in the cluster, and each one
deleted that way takes the resources it owns with it.
