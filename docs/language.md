# The composition language

Weft evaluates [Starlark](https://github.com/bazelbuild/starlark) — a small,
deterministic dialect of Python — in-process, once per reconcile.

## The contract

```python
def compose(variable, observed):
    return {"key": {...resource...}, ...}
```

| argument | is |
|---|---|
| `variable` | `spec.variables`, assembled into one mapping. Keys, not resources. |
| `sources` | declared sources by id, resolved through an impersonated read. A source that does not exist is `None`, and whether that should block is the program's decision. |
| `observed` | resources this Weave previously created, keyed by inventory key, read back live with current status. |

The return value is a mapping of **stable key** to resource. The key is the
identity of the live object: reordering the mapping cannot rename anything, and
changing a key deletes one resource and creates another.

Return order is apply order, and therefore reverse teardown order.

Three returns are meaningful:

```python
return {...}          # this is what should exist
return {}             # nothing should exist; anything left over is pruned
return wait("...")    # cannot proceed at all yet, and here is why
```

`fail("...")` reports a permanent error and lands on the `Degraded` condition
with a backtrace.

### Gating on a source

A source that does not exist resolves to `None`, so an ordering edge — a
resource that must exist before anything is created, even though no field is
ever read off it — is an ordinary line in the program:

```python
if not sources.database:
    return wait("the database has not been created yet")
```

There is no `required` flag on a source, because the flag could only ever say
"always". Written here the gate can say something a flag could not:

```python
if variable.useSql and not sources.database:
    return wait("SQL is enabled but the database is not there yet")
```

Turning `useSql` off releases the Weave without touching the source list.

### `pending(reason)`

`wait()` produces **no resources**. That makes it the wrong tool for staging: a
composition that creates an identity and then role assignments consuming it
would never create the identity, so it could never advance past the wait.

`pending()` is the other half. It notes something unresolved without stopping
the evaluation, so what is ready gets applied and the Weave still reports that
it has not finished:

```python
principal = get(observed, ["identity", "status", "atProvider", "principalId"])
if not principal:
    pending("principalId on the app identity")
    return out                     # the identity is applied; nothing else is
```

```
NAME   READY   WAITING
app    False   FieldUnresolved

still waiting for principalId on the app identity
```

Repeating the same reason inside a loop is collapsed, so calling it once per
iteration is harmless.

Use `wait()` when nothing can be produced. Use `pending()` when some of it can.

## Where variables come from

Variables are keys; sources are resources. A ConfigMap named here is read for
the values inside it, and never for the sake of an ordering edge — for that,
declare it as a source and gate on it.

`spec.variables` is a list, merged in order, later entries winning:

```yaml
variables:
  # A base somebody else maintains.
  - configMap:
      name: platform
  # A whole values.yaml document held under one key.
  - configMap:
      name: release-values
      key: values.yaml
  # Credentials, read as the Weave's own ServiceAccount.
  - secret:
      name: database
      optional: true
  # Overrides for this Weave.
  - values:
      replicas: 3
      image:
        tag: v2
```

Mappings merge key by key, so the last entry above overrides `image.tag`
without restating `image.repository`. Anything else replaces outright, lists
included — the rule Helm values follow.

A program cannot tell where a value came from. That is the point: moving a
setting from inline to a ConfigMap is not a change to the composition.

| | |
|---|---|
| `values` | Written in the `Weave`. Plain YAML, never templated. |
| `configMap` / `secret` | Every entry of `data` becomes one variable, with its value as a string. A `Secret` is decoded, so it reads the same as a `ConfigMap`. |
| `key` | Take one entry instead, parsing its content as YAML. For a `values.yaml` living in a ConfigMap. |
| `optional` | Skip the entry when the object is missing. Without it the `Weave` waits. |

These objects are read through the same impersonated client as everything else,
so a `Weave` can only take configuration from objects its ServiceAccount could
read directly — and they are watched, so editing one reconciles the `Weave`
without touching it.

Be aware that a value reaching a program can be written into any resource that
ServiceAccount may create. A `Secret` entry is a convenience, not a boundary.

## Reading fields

Objects support both attribute and index access, because half the fields worth
reading are dotted status paths and the other half are annotation keys that
cannot be identifiers:

```python
sources.resourceGroup.status.atProvider.id
sources.storage.metadata.annotations["crossplane.io/external-name"]
```

A missing field is an **error**, not `None`. A typo that silently produced
`None` would render a resource with a blank field that applies cleanly, which is
a far worse failure than a backtrace. Optional access is spelled explicitly.

### `get(obj, path, default=None)`

```python
get(sources.rg, "status.atProvider.id", "")
get(observed, ["identity", "status", "atProvider", "principalId"])
get(sources.storage, 'metadata.annotations["crossplane.io/external-name"]')
```

Paths accept dotted segments, bracketed quoted keys, and numeric indices
(`spec.rules[0].host`, negative indices count from the end). A list of segments
works too, which avoids quoting when the path is built from data.

Traversal through a `None` yields the default rather than an error, so
`get(sources.missing, "a.b.c", "x")` is safe.

### `require(obj, path, name=None)`

Reads a field that must be resolved, and stops the whole evaluation with a
`Waiting` condition when it is not:

```python
rg_id = require(sources.resourceGroup, "status.atProvider.id")
```

```
status.atProvider.id is not set on ResourceGroup/sweep-env
```

The message names the link of the chain that actually failed, and identifies the
object from its own `kind` and `metadata.name`. Pass `name=` to override the
label when the object is `None` and cannot describe itself.

An **empty string counts as unresolved**. Provider status fields are routinely
present-but-empty between the apply and the write-back.

Use `require` for a value you cannot proceed without, and `get` with a
conditional when the right answer is to emit fewer resources this pass.

### `has(obj, path)`

True when the path resolves to something other than `None`.

## Producing documents

Serialising a structure into a string field is the single most common thing
these compositions do — Helm values, SQL scripts, connection strings — so it is
a builtin rather than something to assemble by hand.

```python
"data": {"values.yaml": to_yaml({
    "image": {"repository": variable.image, "tag": "latest"},
    "env": [{"name": "SB", "value": sb_endpoint}],
})}
```

Mapping order follows the order the program wrote it, because the result is
usually a document a person will read. Multi-line strings come out as block
scalars:

```yaml
script: |
  CREATE USER [x] FROM EXTERNAL PROVIDER;
  GO
```

| builtin | |
|---|---|
| `to_yaml(value, indent=2)` | YAML document, order preserved |
| `from_yaml(text)` | parse YAML |
| `to_json(value, indent=0)` | JSON, order preserved; `indent` pretty-prints |
| `from_json(text)` | parse JSON |
| `b64encode(text)` / `b64decode(text)` | for Secret payloads |
| `sha256(text)` | hex digest, for deriving stable names |

Integers stay integers all the way through. A replica count that came in as `3`
and went out as `3.0` would be a different resource body on every apply.

## What is not available

An author is assumed to be untrusted, so anything non-deterministic is also a
way to write a composition that behaves differently on each reconcile.

- No `load()`. A program is self-contained. Rejected at compile time.
- No recursion.
- No clock, no randomness, no environment, no I/O of any kind.
- An execution-step budget (`--max-steps`), a cap on returned resources
  (`--max-resources`) and on total values in the result (`--max-values`).

Every one of these fails as a `Degraded` condition with its own reason:
`ProgramSyntaxError`, `ProgramFailed`, `ProgramBudgetExceeded`,
`ProgramInvalidOutput`, `ProgramLoadNotAllowed`, `ProgramOutputTooLarge`,
`ProgramMissingCompose`.

The Starlark universe is otherwise intact: `len`, `range`, `sorted`,
`enumerate`, `zip`, `min`, `max`, `any`, `all`, `str`, `int`, `dict`, `list`,
`set`, string methods, list and dict comprehensions, `print` (to the controller
log), and `fail`.

## Rules for returned resources

Each value must carry `apiVersion`, `kind` and `metadata.name`.

- `metadata.namespace` may be omitted or match the Weave's. Anything else is
  rejected: same-namespace is what makes plain ownership sufficient.
- `metadata.ownerReferences` may not be set. Weft owns what it creates and
  writes the reference itself.
- `metadata.generateName` is not usable — a generated name would change on every
  apply, so a resource needs a stable name.
- `status` is dropped. It is a subresource and is never applied.
- Server-populated metadata (`uid`, `resourceVersion`, `creationTimestamp`,
  `managedFields`, …) is dropped, because deriving an output from an `observed`
  object is a normal thing to do and drags all of it along.
- Cluster-scoped kinds are rejected. A namespaced owner cannot own one, so it
  could never be garbage collected.

Weft adds a `weft.run/weave` label and a `weft.run/key` annotation to everything
it creates, so `kubectl get <kind> -l weft.run/weave=<name>` works.

A resource that already exists and was not created by this `Weave` is refused
rather than taken over, unless the object itself carries
`weft.run/adopt: <weave-name>`. See [ownership.md](ownership.md).

## Worked example

Two phases and a fan-out, which between them cover most of what compositions do:

```python
def compose(variable, observed):
    out = {}

    rg_id = require(sources.resourceGroup, "status.atProvider.id")

    out["identity"] = {
        "apiVersion": "managedidentity.azure.m.upbound.io/v1beta1",
        "kind": "UserAssignedIdentity",
        "metadata": {
            "name": variable.prefix + "-app",
            "annotations": {
                "crossplane.io/external-name":
                    rg_id + "/providers/Microsoft.ManagedIdentity/userAssignedIdentities/" + variable.prefix + "-app",
            },
        },
        "spec": {
            "providerConfigRef": variable.providerConfigRef,
            "forProvider": {
                "name": variable.prefix + "-app",
                "location": variable.location,
                "resourceGroupName": variable.resourceGroupName,
            },
        },
    }

    # The provider writes principalId back minutes after the apply returns.
    # Until it lands, emit only what exists so far.
    principal = get(observed, ["identity", "status", "atProvider", "principalId"])
    if not principal:
        return out

    for role in variable.roles:
        out["ra-" + role.name] = {
            "apiVersion": "authorization.azure.m.upbound.io/v1beta1",
            "kind": "RoleAssignment",
            "metadata": {
                "name": variable.prefix + "-" + role.name,
                # Siblings share a wave so they tear down in one step rather
                # than one round trip at a time.
                "annotations": {"weft.run/wave": "1"},
            },
            "spec": {
                "providerConfigRef": variable.providerConfigRef,
                "forProvider": {
                    "principalId": principal,
                    "roleDefinitionName": role.roleDefinitionName,
                    "scope": role.scope,
                },
            },
        }

    # Every resource is annotated once any is, so the identity gets wave 0.
    out["identity"]["metadata"]["annotations"]["weft.run/wave"] = "0"
    return out
```
