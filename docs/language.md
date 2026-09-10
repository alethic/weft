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
| `observed` | resources this `Weave` previously created, keyed by inventory key, read back live with current status. |

Everything else in the namespace is reached with `read()` and `select()`.

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

### Gating on a resource

A read of something that does not exist comes back falsey, so an ordering edge —
a resource that must exist before anything is created, even though no field is
ever read off it — is an ordinary line in the program:

```python
if not read("sql.azure.m.upbound.io/v1beta1", "MSSQLDatabase", "app"):
    return wait("the database has not been created yet")
```

Nothing declares this as required, because a declaration could only ever say
"always". Written here the gate can say something a flag could not:

```python
if variable.useSql and not read("sql.azure.m.upbound.io/v1beta1", "MSSQLDatabase", "app"):
    return wait("SQL is enabled but the database is not there yet")
```

Turning `useSql` off releases the `Weave` with no other edit.

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
NAME   READY   WAITING           AGE
app    False   FieldUnresolved   2m

still waiting for principalId on the app identity
```

Repeating the same reason inside a loop is collapsed, so calling it once per
iteration is harmless.

Use `wait()` when nothing can be produced. Use `pending()` when some of it can.

## Reading the cluster

```python
read(apiVersion, kind, name, finalize=False)   # one object, or nothing
select(apiVersion, kind, labels={})            # a list, sorted by name
```

Both are namespaced to the `Weave`'s own namespace and both go through the
impersonated client, so a program can only reach what its ServiceAccount could
read directly. In RBAC terms `read` needs `get` and `select` needs `list`.

Nothing is declared anywhere. The controller records what a pass actually read
and registers its watches from that recording, so a program that starts reading
something starts being woken by it, and one that stops, stops — with no second
place to keep in step. The recording is on `status.reads`.

Reads are cached for the length of a pass. Reaching for the same resource in
three branches is one API call and one value, so a program cannot contradict
itself part-way through, and branching costs nothing.

### Absence

A resource that does not exist comes back falsey rather than as an object:

```python
licence = read("v1", "ConfigMap", "licence")
if not licence:
    return wait("no licence ConfigMap in this namespace yet")
```

`get()` and `has()` reach through it without raising. Reaching into it any other
way is an error that names it:

```
MSSQLDatabase "tpyo" does not exist in this namespace, so it has no .status
```

which is where a misspelled name gets caught, there being no declared list to
catch it any earlier.

A failure to *read* is not an absence. A permission denial, or a kind that is
not installed, ends the pass and lands on the `Weave` as itself — never as
`None` that a program might mistake for "not yet".

### `select`

```python
for tenant in select("v1", "ConfigMap", labels={"role": "tenant"}):
    out["db-" + tenant.metadata.name] = {...}
```

Results are sorted by name, because return order is apply order and a
composition built from a selection would otherwise reorder its own output
whenever the API server answered in a different order.

`labels` is a mapping of equality matches. Set-based selectors (`in`, `notin`)
are a string syntax with their own parser and failure modes; select on what you
have and filter in the body.

It returns a list where `read` returns an object, which is why it has its own
name rather than being a keyword away — the shape of the answer should not
depend on which argument was passed.

### `finalize=True`

```python
up = read("v1", "ConfigMap", "upstream", finalize=True)
```

Places a finalizer on the resource, so that when somebody deletes it this
`Weave` tears down what it derived from it *first*. It needs `update` permission
on that resource, and it is released after `--hold-timeout` regardless of
progress: blocking somebody else's object, and their namespace deletion, forever
is worse than an ordering violation.

This orders the deletion of an API object and nothing more. A managed resource
removed from the API server may leave its provider tearing down a cloud resource
for minutes afterwards.

## Where variables come from

Variables are keys; `read()` returns resources. A ConfigMap named here is read
for the values inside it, and never for the sake of an ordering edge — for that,
read it in the program and gate on it.

Everything here could be written as a read in the body. It has a declarative
form because configuration held in a ConfigMap or a Secret is common enough to
be worth seeing without opening the program, and because the values arrive
merged. Underneath it is a couple of implicit reads: same client, same cache,
same watches.

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
rg.status.atProvider.id
storage.metadata.annotations["crossplane.io/external-name"]
```

A missing field is an **error**, not `None`. A typo that silently produced
`None` would render a resource with a blank field that applies cleanly, which is
a far worse failure than a backtrace. Optional access is spelled explicitly.

### `get(obj, path, default=None)`

```python
get(rg, "status.atProvider.id", "")
get(observed, ["identity", "status", "atProvider", "principalId"])
get(storage, 'metadata.annotations["crossplane.io/external-name"]')
```

Paths accept dotted segments, bracketed quoted keys, and numeric indices
(`spec.rules[0].host`, negative indices count from the end). A list of segments
works too, which avoids quoting when the path is built from data.

Traversal through a `None` yields the default rather than an error, so
`get(missing, "a.b.c", "x")` is safe, and so is `get()` over a read that found nothing.

### `require(obj, path, name=None)`

Reads a field that must be resolved, and stops the whole evaluation with a
`Waiting` condition when it is not:

```python
rg_id = require(rg, "status.atProvider.id")
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
- A cap on distinct resources one evaluation may read (`--max-reads`) and on
  what a single `select()` may match (`--max-selected`). Every read is also a
  watch the controller keeps alive afterwards.

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

    rg = read("azure.m.upbound.io/v1beta1", "ResourceGroup", variable.resourceGroupName)
    rg_id = require(rg, "status.atProvider.id")

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
