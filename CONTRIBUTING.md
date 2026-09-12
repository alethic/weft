# Contributing

## Getting set up

```bash
make            # generate, fmt, vet, lint, test, build
make envtest    # fetch the control-plane binaries the controller tests need
make test       # everything
make lint       # golangci-lint, pinned
make vulncheck  # dependencies against the Go vulnerability database
```

`controller-gen` and `setup-envtest` are pinned in `go.mod` as tool
dependencies, so `go tool` resolves them without a separate install.
`golangci-lint` is pinned in the Makefile and installed on first use. CI runs
`make lint` and `make vulncheck` rather than reimplementing them, so a local run
and a CI run cannot disagree about versions.

`helm` you supply; without it the chart tests skip.

## Versioning

[GitVersion](https://gitversion.net) derives the version from the branch and
the history, so nothing is tagged or bumped by hand:

| branch | shape | increment |
|---|---|---|
| `main` | `0.1.0-pre.9` | patch |
| `develop` | `0.2.0-dev.4` | minor |
| a release tag | `0.1.0` | — |

`make version` prints what the current checkout would produce.

To move the next version, say so in a commit message:

```
+semver: minor
```

`next-version` in `GitVersion.yml` sets the floor.

## Cutting a release

Run the workflow by hand with **release** checked. That publishes under the
clean version rather than a prerelease, and tags the commit with it.

Do not create the tag yourself. The version is derived, so typing one is the one
way to end up with two artifacts claiming the same version while carrying
different content — and nothing downstream can tell them apart afterwards.

Once `0.1.0` is tagged, GitVersion moves on by itself: the next commit on `main`
is `0.1.1-pre.1`, and a commit carrying `+semver: minor` makes it `0.2.0-pre.1`.
So the release after that is whatever `make version` reports, with no decision
to make.

The chart and the image always carry the same version. `Chart.yaml` holds a
placeholder that is replaced at package time, and the chart's `appVersion` is
what selects the image tag, so a packaged chart points at the image the same run
pushed. Releases additionally pin `image.digest`, so an installed release cannot
drift under a moved tag.

`latest` follows real releases only. A prerelease carries a label and never
moves it.

## Where builds go

Every build of `main` or `develop` publishes to **GitHub Packages** — the image
to `ghcr.io/alethic/weft` and the chart, as an OCI artifact, to
`ghcr.io/alethic/charts/weft`. Pull requests publish nothing, which is why the
publish job is the only one holding `packages: write`.

A **GitHub release** is separate and deliberate: it happens when a tag drove the
build, or when the workflow is run by hand with **release** checked. The release
action creates the tag from the derived version, so tagging is not a manual step
either — see above.

## What CI checks

Beyond the tests: generated files are current, formatting is clean,
`golangci-lint` passes, dependencies have no known vulnerabilities, the image
builds and reports its version, the image has no fixable HIGH or CRITICAL
findings, and the chart renders every configuration it claims to support while
refusing the ones it should.

The test job runs against three Kubernetes versions — the floor the chart's
`kubeVersion` claims, a middle one, and the newest — because claiming
`>=1.27.0-0` without ever running against 1.27 is a guess rather than a claim.

It also asserts that the controller and chart suites *actually ran*. Both skip
when their dependency is missing, and a green run that silently skipped the two
most valuable suites is worse than a red one.

## How the tests are layered

| layer | needs | covers |
|---|---|---|
| `internal/eval` | nothing | language semantics, waiting, bounds, ergonomics |
| `internal/inventory` | nothing | dependency ordering, hysteresis, normalisation, ownership |
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
