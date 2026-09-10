# Security

## Reporting a vulnerability

Report privately via [GitHub security advisories](https://github.com/alethic/weft/security/advisories/new),
or by email to security@alethic.solutions. Please do not open a public issue for
a vulnerability.

Include what you did, what happened, and what you expected. A cluster-role dump
and the `Weave` that triggered it are usually enough to reproduce.

## The threat model

Weft exists to *narrow* a privilege boundary, so it is worth being explicit
about where that boundary is and what it does not cover.

### What Weft is designed to prevent

**A composition author gaining privileges they do not have.** Every read and
every write is performed as `spec.serviceAccountName`, impersonated. The
controller holds no write access of its own to anything a composition might
generate. A `Weave` cannot create a resource its ServiceAccount could not have
created by hand, and cannot read a resource its ServiceAccount could not have
read. This holds even when the controller itself runs as cluster-admin, and is
verified by pointing a `Weave` at a ServiceAccount with no permissions and
observing that nothing is created.

**Reading a value the author cannot see.** Creation authority is not read
authority, and the whole data path is "read a value out of one object and write
it into a sibling". An unimpersonated read on that path would leak the contents
of an object the author cannot open, so there is no unimpersonated read: the
`kube` package offers no way to make one, and the informer caches are
metadata-only so there is structurally no cached field value to read by mistake.

**An untrusted program affecting the controller.** Programs run in a bounded
Starlark interpreter: no `load()`, no recursion, no clock, no randomness, no
I/O, an execution-step budget, and caps on result size and nesting depth. Every
limit fails as a condition on the `Weave` rather than as a wedged controller.

**Escaping the namespace.** Outputs must live in the `Weave`'s own namespace,
cannot be cluster-scoped, and cannot set their own owner references. These are
rejected rather than corrected.

**Taking over a resource by naming it.** Server-side apply is create-or-update,
so a program naming an object that already exists would otherwise adopt it
silently — and deleting the `Weave` would then delete something it never made.
Weft refuses instead. Handing an object over requires an annotation *on that
object* naming the `Weave`, so consent comes from whoever holds the resource
rather than from whoever wrote the program. See
[docs/ownership.md](docs/ownership.md).

### What Weft does not prevent

**A ServiceAccount that is over-granted.** Weft faithfully performs whatever its
ServiceAccount may do. Binding `cluster-admin` to a composer ServiceAccount hands
that to anybody who can write a `Weave` in that namespace. The security of a
`Weave` is exactly the security of the ServiceAccount it names, which is the
point, and means the grant is where the review should happen.

**Resource exhaustion by volume.** Limits bound one evaluation, not the number
of `Weave` objects a user creates. A namespace user who can create `Weave`
objects can create many. Use quotas.

**Anything the author of a resource being read writes into it.** What a program
reads is data, and it may write that data into an output. A user who can edit a
ConfigMap a `Weave` reads can influence what that `Weave` produces, within the
bounds of what the ServiceAccount may create.

**Reading Secret contents into a less-protected object.** If the ServiceAccount
can read a Secret and create a ConfigMap, a program can copy one into the other.
That is a property of the grant, not of Weft, and it is another reason to scope
the ServiceAccount rather than reuse a broad one.

## The controller's own privileges

The full grant is in `charts/weft/templates/rbac.yaml` and is deliberately
short. It is worth reading for what is absent: no write access to any resource
type a composition might generate.

Two rules deserve attention.

**`impersonate` on `serviceaccounts`.** This is the mechanism. Because the API
server authorises a username of the form `system:serviceaccount:<ns>:<name>`
against the `serviceaccounts` resource *in that namespace*, this grant can be
confined per namespace — see `rbac.impersonation.scope` in the chart.

**`impersonate` on `groups`, pinned by name.** Weft impersonates
`system:serviceaccounts` and `system:authenticated` so the impersonated identity
matches what a real ServiceAccount token carries. The grant is restricted with
`resourceNames` because an unrestricted one would let anyone who compromises the
controller impersonate `system:masters`, which is a worse position than the one
this project set out to avoid.

The chart refuses to render an unrestricted group grant unless
`rbac.allowUnrestrictedGroupImpersonation` is set explicitly. If you set it,
understand that **compromising the controller then means cluster-admin.**

## Auditing

An impersonated request records both identities, so the audit log shows who a
change was really made for:

```json
"user": {"username": "system:serviceaccount:weft-system:weft-controller"},
"impersonatedUser": {"username": "system:serviceaccount:sweep-labs:composer"}
```

Filtering on `impersonatedUser` gives every change a given composition made.

## Hardening

- Pin `image.digest` rather than a tag.
- Set `rbac.impersonation.scope: namespaced` if Weft only needs some namespaces.
- Leave `rbac.allowUnrestrictedGroupImpersonation` off.
- Scope composer ServiceAccounts narrowly. Weft tells users exactly which verbs
  and resources they need, so a minimal Role is not much more work than `edit`.
- Leave `crds.keep` on, so an uninstall cannot cascade into deleting every
  `Weave` and the infrastructure they own.
- Run `weft reap` before removing the controller if any program uses
  `read(..., hold=True)`.

## Supported versions

Pre-1.0. Fixes land on `main` and in the next tagged release.
