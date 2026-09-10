## What this changes

<!-- What behaviour is different afterwards, and why. -->

## Checks

- [ ] `make generate` produces no diff
- [ ] `make test` passes, and the controller and chart suites actually ran
      rather than skipping
- [ ] `golangci-lint run ./...` is clean
- [ ] Chart version bumped, if anything under `charts/weft` changed

## Does this touch the impersonation boundary?

<!--
Delete this section if not. If it does - anything in internal/kube, the
controller's RBAC, or how a client is built - say what the boundary is
afterwards and why it is still true that a Weave cannot do more than its
ServiceAccount can.
-->
