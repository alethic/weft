## What this changes

<!-- What behaviour is different afterwards, and why. -->

## Checks

- [ ] `make generate` produces no diff
- [ ] `make test` passes, and the controller and chart suites actually ran
      rather than skipping
- [ ] `make lint` is clean

Versions are derived by GitVersion, so there is nothing to bump. A commit
message containing `+semver: minor` or `+semver: major` moves the next version
if this needs one.

## Does this touch the impersonation boundary?

<!--
Delete this section if not. If it does - anything in internal/kube, the
controller's RBAC, or how a client is built - say what the boundary is
afterwards and why it is still true that a Weave cannot do more than its
ServiceAccount can.
-->
