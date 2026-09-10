# Porting the CronJob compositions

Weft was built against a real workload: twelve compositions in a Helm chart,
each one a ConfigMap holding a gomplate template and a `resources=(...)` list,
plus a CronJob running every five minutes that piped `kubectl get -o yaml`
through gomplate into `kubectl apply --server-side`.

They are not a specification — Weft knows nothing about Azure, Crossplane or
identities. They are evidence that the shape is real, a source of fixtures, and
the bar the language had to clear.

## Twelve into four

| original | becomes |
|---|---|
| `azure/app-identity` + `azure/app-roleassignments` | [`examples/app.yaml`](../examples/app.yaml) |
| `azure/env-identity` + `azure/env-roleassignments` + `azure/roleassignments` + `env-serviceaccount` | [`examples/env.yaml`](../examples/env.yaml) |
| `azure/sql/sqladmin-identity` + `azure/sql/sqlserver` + `azure/sql/sqlcmd` + `sqladmin-serviceaccount` | [`examples/sql.yaml`](../examples/sql.yaml) |
| `app-release-values` | [`examples/release-values.yaml`](../examples/release-values.yaml) |
| `azure/admins-roleassignments` + `azure/sql/admins-sqlcmd` | fold into `app` and `sql` as another entry in the role and user tables |

Most of the merging is one observation: once staging is expressible, the
identity composition and the composition that consumes the identity are the same
composition. They were only ever separate because there was no way to say "run
when the identity changes", so they were chained by both polling.

## What disappears

**The exec ConfigMap.** Every original listed its own ConfigMap as a source.
That existed for one reason: to obtain a `uid` for the `ownerReferences` on the
outputs. In Weft the Weave is the owner and writes the reference itself, so the
whole device goes away — and with it the `assert "ConfigMap not found."` at the
top of every template.

**The five-minute hop.** Latency compounded per dependency. An identity created
at T+0 was not seen by the role assignment job until T+5, which was not seen by
the next until T+10. Watches make each hop the time it takes a provider to write
status back.

**`kubectl apply` with no `--prune`.** Removing a resource from a template
orphaned the live object forever, and reclaiming it meant renaming the ConfigMap
by hand. Weft records an inventory and prunes, with a delay so a provider
restart does not churn real infrastructure.

**Errors in Job logs.** Failures landed in a Job's output and on no object's
status. Now they land on the Weave, with the specific unresolved field named and
denials rendered as a RoleBinding you can paste.

**`cluster-admin`.** The exec ServiceAccount was bound to it. The whole point of
this project is that the composition runs as an identity you scope yourself.

**The second-order reconciler.** `sqlcmd` was a CronJob that generated a CronJob,
which existed only because "run when the identity changes" was inexpressible. It
becomes a Job keyed off the identity's generation.

## What stays in Helm

Anything static. `app-identity` also emitted two `FederatedIdentityCredential`
resources directly, outside the gomplate template — they read no live status, so
there is nothing for Weft to react to and no reason to move them. Weft is for
the parts that have to observe something.

## Translating the patterns

### Dictionary lookup becomes a source id

The originals built a map keyed by `apiVersion/kind/name` and indexed into it:

```gotemplate
{{- $m := dict }}
{{- range $i := .items }}
{{-   $m = set (printf "%s/%s/%s" $i.apiVersion $i.kind $i.metadata.name) $i $m }}
{{- end }}
{{- $rg := index $m "azure.m.upbound.io/v1beta1/ResourceGroup/sweep-env" }}
{{- $rg | isKind "map" | assert "ResourceGroup not found." }}
```

That whole preamble is `spec.sources`:

```yaml
sources:
- id: resourceGroup
  apiVersion: azure.m.upbound.io/v1beta1
  kind: ResourceGroup
  name: sweep-env
```

```python
sources.resourceGroup
```

### `required` becomes `require`

```gotemplate
{{- $id := index $rg "status" "atProvider" "id" | required "Missing ResourceGroup Id." }}
```

```python
rg_id = require(sources.resourceGroup, "status.atProvider.id")
```

The difference is what happens next. `required` failed the render, the CronJob
exited non-zero, and the message went to a Job log. `require` produces a
`Waiting` condition on the Weave naming the field, and the Weave resumes on its
own when the provider writes it back.

### Helm ranges become loops

`admins-roleassignments` and `admins-sqlcmd` used `range` over
`.Values.azure.permissions.admin` at *build* time, emitting N copies of an
entire CronJob. In Weft the list is `spec.variables` and the loop is in the
program: one Weave, N outputs. Adding an admin is a one-line edit rather than a
chart re-render.

### `nindent` becomes `to_yaml`

Building a nested document by hand with indentation counts is how
`app-release-values` produced its `values.yaml`, and it is the thing most likely
to break when someone adds a level of nesting:

```python
"data": {"values.yaml": to_yaml(values)}
```

### The ordering gate stays explicit

One composition declared `MSSQLDatabase` as a source, asserted it existed, and
never read a field from it. There is no expression to infer that dependency
from, so any design that derives edges from references drops it silently. Weft
does not infer - the source is declared, and the assertion ports directly:

```python
if not sources.database:
    return wait("the MSSQLDatabase has not been created yet")
```

which is the `assert` from the original template, in the same place it was.

## A bug worth carrying forward

`pipeline.sh` originally used a bare `wait`, which reports success regardless of
how its children exited. When only some resources resolved, `kubectl get` still
emitted a List of the rest and exited non-zero, so a partial List reached the
template, which then reported "resource not found" instead of the API error that
actually caused it.

The lesson is structural rather than incidental, and it is built into the
controller: an error reading *any* source aborts before evaluation, and the API
error is what gets surfaced. Weft never evaluates against a partially resolved
source set. Absence is a value a program can see; a failure to read is not.
