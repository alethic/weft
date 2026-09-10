# Permissions

Debugging RBAC is the primary user experience of this tool, so this page is
about two things: what a `Weave` author needs to grant, and what the controller
itself holds.

## What a Weave needs

Every read and every write is performed as `spec.serviceAccountName`. The
controller has no write access of its own to fall back on, so a `Weave` can do
exactly what its ServiceAccount can do and nothing more.

That ServiceAccount needs, in the Weave's own namespace:

- `get` on every declared source. `list` and `watch` too, if you want changes
  noticed immediately rather than on the poll interval.
- `get`, `create` and `patch` on every kind the program returns. Server-side
  apply requires `patch`.
- `delete` on those kinds, for pruning and teardown.
- `update` on any source declared with `finalize: true`.

You do not have to work this out in advance. Apply the `Weave` and read the
condition:

```
$ kubectl describe weave app
...
  Degraded    True    Forbidden
    reading source "resourceGroup":
    ServiceAccount "composer" cannot get resourcegroups.azure.m.upbound.io in namespace "sweep-labs"

    Grant it with:
      kubectl create role weft-read-resourcegroups --namespace=sweep-labs --verb=get,list,watch --resource=resourcegroups.azure.m.upbound.io
      kubectl create rolebinding weft-read-resourcegroups --namespace=sweep-labs --role=weft-read-resourcegroups --serviceaccount=sweep-labs:composer
```

Those commands are meant to be pasted. They are single-line on purpose:
`kubectl describe` indents continuation lines, which silently breaks a
backslash-continued command when somebody copies it.

The suggested grant widens the denied verb to the set that goes with it —
asking for `get` gets you `get,list,watch` — because granting exactly one verb
reliably produces a second denial on the next reconcile.

The controller re-checks after `--degraded-retry` (30 seconds), so a grant is
picked up shortly after you make it. It resolves one denial at a time: fix the
read, and the next pass tells you about the write.

An `edit` RoleBinding covers everything for built-in types, and is the fast path
when you are not trying to be minimal:

```bash
kubectl create rolebinding composer-edit --clusterrole=edit \
  --serviceaccount=my-namespace:composer -n my-namespace
```

Note that `edit` does **not** cover custom resources such as Crossplane managed
resources. Those need explicit rules.

## What the controller holds

The whole grant, from `config/rbac/role.yaml`:

| resource | verbs |
|---|---|
| `weaves` | `get, list, watch, update, patch` |
| `weaves/status` | `get, update, patch` |
| `weaves/finalizers` | `update` |
| `serviceaccounts` | `impersonate` |
| `groups` (`system:serviceaccounts`, `system:authenticated`) | `impersonate` |
| `events` | `create, patch` |

What is absent is the point. There is no write access to any resource type a
composition might generate. The controller cannot create a RoleAssignment or a
ConfigMap on its own account; it can only become a ServiceAccount that already
could.

`update` on `weaves` is for the finalizer, which is what buys ordered teardown.

### Why groups are impersonated at all

The API server does not derive group membership from a username. Impersonating
only `system:serviceaccount:ns:name` produces an identity strictly *weaker* than
the real ServiceAccount — RBAC bound to `system:authenticated` would not apply.
Weaker sounds safe, but it means Weft fails on grants you can plainly see are
present, which is the exact confusion this tool exists to remove.

### Why the group grant is pinned by name

`impersonate` on `groups` without a `resourceNames` restriction lets anyone who
compromises the controller impersonate `system:masters`. That is a far worse
position than the one this project set out to avoid, so the shipped ClusterRole
pins the two groups it actually uses.

This is why `system:serviceaccounts:<namespace>` is **not** impersonated by
default, even though a real ServiceAccount token carries it: the namespace set
is open-ended, so including it would force the unrestricted grant. If your
namespaces bind RBAC to that group, opt in explicitly:

```yaml
args:
- --impersonate-groups=system:serviceaccounts,system:serviceaccounts:{namespace},system:authenticated
```

and widen the ClusterRole to match, accepting the trade:

```yaml
- apiGroups: [""]
  resources: ["groups"]
  verbs: ["impersonate"]
  # No resourceNames: the per-namespace group cannot be enumerated ahead of
  # time. This grants the ability to impersonate any group, including
  # system:masters.
```

### Confining the controller to some namespaces

Impersonating a username of the form `system:serviceaccount:<ns>:<name>` is
authorised against the `serviceaccounts` resource *in that namespace*. So the
ClusterRole can be replaced with per-namespace Roles:

```bash
kubectl create role weft-impersonate --namespace=team-a \
  --verb=impersonate --resource=serviceaccounts
kubectl create rolebinding weft-impersonate --namespace=team-a \
  --role=weft-impersonate --serviceaccount=weft-system:weft-controller
```

The controller still needs the cluster-scoped rules for `weaves` themselves and
for `groups`.

## Watches and the polling fallback

Watches are only a trigger. Every field value is read through an impersonated
`get` at reconcile time, so a watch that cannot be established costs latency and
nothing else.

That is why the informers run as the Weave's own ServiceAccount rather than as
the controller: a user grants read access with a RoleBinding they can write
themselves, and no cluster-scoped RBAC change is needed after install. It is
also why they are metadata-only — a `PartialObjectMetadata` cache structurally
cannot serve a field value, so the impersonated `get` stays the only path to
one. A full-object cache nobody is supposed to read from works right up until
somebody reads from it.

A ServiceAccount holding `get` but not `list` and `watch` still works. The Weave
reports:

```
Degraded  True  WatchDegraded
  running on polling instead of watches, so changes are noticed on the poll
  interval rather than immediately: ...
```

and reconciles every `--poll-interval` instead. Granting `list` and `watch`
restores immediate updates.

Authorisation is checked when a watch opens, not continuously, so watches are
torn down and re-established every `--watch-lifetime` (30 minutes). That bounds
how long a revoked grant keeps being honoured.

## Auditing

An impersonated request records both the controller and the impersonated
subject, so the audit log shows who a change was really made for:

```
"user": {"username": "system:serviceaccount:weft-system:weft-controller"},
"impersonatedUser": {"username": "system:serviceaccount:sweep-labs:composer"}
```

## Finalizers

`finalize: true` on a source places a finalizer on an object the Weave does not
own. Weft checks it can *remove* that finalizer on every pass, not only when
adding it, because a RoleBinding revoked afterwards would otherwise leave a
finalizer nobody can lift — deadlocking the object and the namespace it lives
in.

It also releases the finalizer after `--source-finalizer-timeout` regardless of
teardown progress. Blocking somebody else's object forever is worse than the
ordering violation.

Before uninstalling the controller, release everything it has placed:

```bash
weft reap --dry-run
weft reap
```

Anything missed carries a finalizer beginning `weft.run/src-`, which is a fixed
prefix precisely so a human can find them:

```bash
kubectl get <kind> -A -o json \
  | jq -r '.items[] | select(.metadata.finalizers[]? | startswith("weft.run/src-"))
           | "\(.metadata.namespace)/\(.metadata.name)"'
```
