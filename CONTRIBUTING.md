# Contributing

## Getting set up

```bash
make            # generate, fmt, vet, test, build
make envtest    # fetch the control-plane binaries the controller tests need
make test       # everything
```

Two toolchain dependencies are pinned in `go.mod` as tool dependencies, so
`go tool` resolves them without a separate install: `controller-gen` and
`setup-envtest`. `helm` and a control plane come from `make envtest` and your
package manager.

## How the tests are layered

| layer | needs | covers |
|---|---|---|
| `internal/eval` | nothing | language semantics, waiting, bounds, ergonomics |
| `internal/inventory` | nothing | wave planning, hysteresis, normalisation, ownership |
| `internal/kube` | nothing | RBAC diagnostics |
| `internal/examples` | nothing | every shipped example parses, evaluates and normalises |
| `cmd/weft` | helm | the chart renders, and its arguments parse with the real flag set |
| `internal/controller` | a control plane | the reconcile loop end to end |

Everything except the last two runs with a plain `go test ./...`. The chart and
controller tests **skip** rather than fail when their dependency is absent, so
check the output rather than assuming a green run covered them.

### The controller tests

They run against a real API server, because the reconcile loop is where every
bug found during development actually was — pruning that deleted too eagerly, a
status write that woke the controller into a permanent loop, a teardown that
recreated what it had just removed. None of those are visible without something
to react to.

On Linux and macOS this is a hermetic `envtest` control plane. On Windows it is
not: `envtest` does not compile against controller-runtime v0.25.0 there, since
`pkg/internal/testing/process` declares `signalProcess` both unconditionally and
again in `signal_windows.go`. The bootstrap is split by build tag, and on
Windows the same tests run against your current kubecontext:

```bash
WEFT_TEST_CLUSTER=1 go test ./internal/controller/
```

That installs the CRD, creates a namespace per test, and removes both
afterwards. It only removes the CRD if that run installed it, because deleting
one that was already there would take every `Weave` in the cluster with it.
Point it at a scratch cluster.

## What the tests are for

Several of them exist because of a specific failure and say so in a comment. If
one of those looks arbitrary, read the comment before changing it:

- **`TestSteadyStateDoesNotChurn`** — a status write wakes the Weave through its
  own watch, so any field that changes on every pass makes every Weave in the
  cluster spin forever.
- **`TestPruneWaitsOutTheDelay`** — hysteresis has to be measured on a clock.
  Counting reconciles measures controller activity: in testing, a removed
  resource reached 79 "consecutive evaluations" in 56 seconds.
- **`TestChartArgsParse`** — YAML decodes `20000000` as a float64 and Helm
  renders it as `2e+07`, which the flag parser rejects. The chart would install
  cleanly and the container would crash-loop.
- **`TestChartCoversGeneratedRBAC`** — the chart's ClusterRole is hand-written
  while `controller-gen` derives the authoritative rules from markers.

## Changing generated files

```bash
make generate
```

This regenerates the deepcopy functions, the CRD and the controller ClusterRole,
and copies the CRD into the chart. CI fails if the result differs from what is
committed, so run it before pushing.

## The parts that are not negotiable

Two properties define the project. A change that weakens either is not a
trade-off to be discussed in review; it is a different project:

1. **Namespace-scoped authoring.** No cluster-scoped object is created at
   runtime, no CRD is generated per composition, no platform team in the loop.
2. **Every read and every write is impersonated** as the Weave's ServiceAccount.
   The `kube` package deliberately offers no way to make an exception, and
   `SECURITY.md` explains why an unimpersonated read is a privilege leak rather
   than an optimisation.

If a change seems to need an unimpersonated read, that is worth raising as an
issue before writing the code.

## Style

Match the surrounding code. Comments explain *why*, particularly where the
obvious implementation is wrong for a reason that is not obvious — most of the
comments in this codebase are there because someone would otherwise
reasonably simplify the code back into a bug.

Commit messages describe what changed and why, in prose.
