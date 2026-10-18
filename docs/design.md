# Design and alternatives

Why Weft exists alongside the other composition tools, why its language is
Starlark, and what it deliberately does not do.

## Compared with other tools

| | namespaced authoring | impersonated writes | status-reactive |
|---|---|---|---|
| **Weft** | **yes** | **yes** | **yes** |
| [kro](https://kro.run) | no — `ResourceGraphDefinition` is cluster-scoped and generates a CRD | no | yes |
| [Crossplane v2](https://crossplane.io) | XRs are namespaced, but XRDs and Compositions are cluster-scoped admin artifacts | no — the RBAC manager runs with `escalate` | yes |
| Metacontroller, Yoke ATC | no | no | yes |
| [Flux](https://fluxcd.io) | yes | **yes** — this is where the model comes from | no — `dependsOn` between whole Kustomizations, and `substituteFrom` limited to ConfigMaps and Secrets |
| Kyverno `generate` | yes | no — the background controller's own ServiceAccount does the creating, checked once at policy admission | yes |

- **Namespaced authoring** — an ordinary namespace user can write a
  composition without a cluster administrator installing or registering
  anything first.
- **Impersonated writes** — the composition acts as an identity the author
  chose and could have used directly, so it cannot create anything the author
  could not.
- **Status-reactive** — a composition can wait on a field another controller
  fills in later, and proceed when it appears.

If you already run Crossplane, or any provider whose resources report status
asynchronously, the third column is the one that will matter day to day.

## Why Starlark

[Starlark](https://github.com/bazelbuild/starlark) is Python you can read on
sight, without the parts that make Python unsafe to run on somebody else's
behalf. Untrusted authorship is the premise, so the language is bounded rather
than trusted: no `load()`, no recursion, no clock, no randomness, no I/O beyond
`read()` and `select()`, an execution-step budget, and caps on result size and on
how much of a namespace one pass may pull in. Every limit fails as a `Degraded`
condition with a distinct reason, not as a wedged controller.

Considered and rejected:

- **CEL** cannot emit variable-length structure, so conditional inclusion and
  fan-out become YAML constructs that accumulate into a bad programming
  language.
- **CUE** is an adoption tax, with no cost budget for untrusted evaluation.
- **WASM** needs a build pipeline per composition.
- **Go templates** have unbounded recursion, and `sprig` reaches the
  environment.

## Dependencies are written, not inferred

Weft does not infer ordering from expression references, because a dependency
that is never referenced would then be invisible. The read is written, and what
an absence means is the line after it:

```python
if variable.useSql and not read("sql.azure.m.upbound.io/v1beta1", "MSSQLDatabase", "app"):
    return wait("SQL is enabled but the database is not there yet")
```

That also expresses what a declared "required" flag could not: a gate that
depends on configuration. Turning `useSql` off releases the `Weave` with no
other edit.

## Watches come from what was read

Nothing about inputs is declared in the spec. The controller records what each
pass actually read and registers its watches from that recording, so a program
that starts reading something starts being woken by it, and one that stops,
stops. There is no second place to keep in step with the program.

## Non-goals

- **Generating a CRD per composition.** One CRD, forever.
- **A typed API surface for consumers.** That is kro's model, and it requires
  the platform team this project exists to remove.
- **Anything cross-namespace.** Same-namespace is a hard rule, and it is what
  keeps ownership and garbage collection simple enough to need no machinery.
- **Cloud-provider integration.** Weft composes whatever CRDs are installed and
  knows nothing about any of them.

## The name

From weaving. The **warp** is the threads already on the loom — resources a
`Weave` reads but does not own. The **weft** is what is passed across them.
