# Moving from weft.run/v1alpha1

Weft was first deployed under the API group `weft.run`, at version `v1alpha1`.
The current API is `weft.alethic.solutions/v1`. This page is for installations
that have `weft.run` Weaves.

If you have never had a `weft.run` Weave, none of this applies, and you can
install with `--set legacyAPI.enabled=false` to leave the old group out
entirely.

## What upgrading does

Nothing changes for your existing Weaves. The controller serves both groups
side by side:

- Both CRDs are installed: `weaves.weft.run` and
  `weaves.weft.alethic.solutions`.
- A `weft.run` Weave is reconciled exactly as before. Everything it writes onto
  what it manages still uses `weft.run` keys — the `weft.run/weave` label, the
  `weft.run/weave-uid` annotation, the `weft.run/weave` and `weft.run/held-…`
  finalizers, and the `weft.run` field manager — because those are what its
  objects already carry.
- A v1 Weave uses `weft.alethic.solutions` keys for all of the same things, and
  never writes `weft.run` onto anything.
- The leader election lease keeps its old name, so during a rolling upgrade the
  old and new controller replicas contend for one lease instead of both acting.

### Upgrading from a 0.1.0 prerelease

Prerelease charts (`0.1.0-pre.N`) stored the image digest in the release's
values. `helm upgrade --reuse-values` carries that digest forward, so the new
chart would run the old controller image. The old binary rejects the new
chart's `--serve-legacy-api` flag, so the new pod crash-loops while the old one
keeps running. Clear the digest on that first upgrade:

```bash
helm upgrade weft oci://ghcr.io/alethic/charts/weft --version <new>   --namespace weft-system --reuse-values --set image.digest=
```

An upgrade without `--reuse-values` needs nothing extra. From the first release
onward the published digest ships inside the chart rather than its values, so
this does not recur.

The two schemas are identical. Moving a Weave means changing its `apiVersion`
and the keys of any Weft annotations in its program. Nothing else changes.

## Moving one Weave

Kubernetes cannot convert an object from one API group to another, so moving
a Weave means deleting the old one and creating the new one. The step that
matters is deleting it **without** deleting what it made. Weft handles the
rest: a v1 Weave with the **same name** in the same namespace takes over each
object its predecessor made, in place, as its program declares it. Nothing is
recreated.

### 1. Update the manifest

```diff
-apiVersion: weft.run/v1alpha1
+apiVersion: weft.alethic.solutions/v1
 kind: Weave
 metadata:
   name: app
```

In the program, rename any Weft annotation keys:

```diff
 "metadata": {
     "name": "app-db",
-    "annotations": {"weft.run/owned": "false"},
+    "annotations": {"weft.alethic.solutions/owned": "false"},
 }
```

A v1 Weave whose program still says `weft.run/owned` is refused with
`Degraded`, and nothing is applied. The alternative would be worse: the
resource you meant to keep would be owned by the new Weave and deleted along
with it.

If you hand objects over with the adoption annotation, rename that the same
way: `weft.run/adopt` becomes `weft.alethic.solutions/adopt`.

### 2. Delete the old Weave, orphaning what it made

```bash
kubectl -n my-namespace delete weaves.weft.run app --cascade=orphan
```

Use the full resource name `weaves.weft.run`. While both CRDs are installed,
plain `weave` (and the short name `wv`) matches both groups, and kubectl
resolves it to one of them — in practice `weft.alethic.solutions`, which sorts
first. Scripts and runbooks that say `kubectl get weave` will start showing v1
Weaves after the upgrade; give them the full name while you have both.

`--cascade=orphan` is what keeps everything standing. The controller releases
the Weave and any `hold=True` finalizers it placed, and the garbage collector
strips its owner references. The objects stay as they are, unmanaged for the
moment.

### 3. Apply the v1 Weave

```bash
kubectl apply -f app.yaml
kubectl -n my-namespace get weaves.weft.alethic.solutions app
```

For each object the program declares, the v1 Weave finds the old Weave's
provenance and takes the object over:

- It moves server-side-apply field ownership from the `weft.run` field manager
  to its own. The new Weave then prunes fields the program no longer sets
  exactly as if it had always been the one writing them. That includes the old
  `weft.run/weave` label and `weft.run/weave-uid` annotation, which disappear
  on the first apply.
- It places its own owner reference and provenance.
- It records a `HandedOver` event on the Weave for each object.

If the old Weave is still being deleted when the new one first reconciles,
the new one shows `Waiting` with reason `HandoverPending` and carries on once
the old one is gone. If the old Weave was not deleted at all, the new one is
`Degraded` and its message gives the exact `kubectl delete … --cascade=orphan`
command to run.

### With Helm, Flux or another deployment tool

If your Weaves are deployed from a chart or a Kustomization, change the
`apiVersion` (and any annotation keys) in the manifest and roll it out as one
release. That is the whole procedure.

Helm, Flux's helm-controller and kustomize-controller all apply a release the
same way: they create or update everything in the new release first, and only
then delete what is no longer in it. That deletion uses background propagation,
and none of them can be told to orphan. So by the time the old Weave is
deleted, its v1 successor already exists. A `weft.run` Weave that is deleted
while a live v1 Weave of the same name exists in its namespace does not tear
anything down. It removes its own owner references, so the garbage collector
has nothing to collect, records a `HandingOver` event, and lets go. The v1
Weave then takes each object over as described above.

This depends on the new Weave existing before the old one is deleted. Keep the
change in a single release:

- **Do not** remove the old Weave in one release and add the new one in a later
  one.
- **Do not** move it between two Kustomizations or two HelmReleases in the same
  change. Each is reconciled on its own schedule, so the old one may be pruned
  before the new one exists.

If the ordering cannot be guaranteed, delete the old Weave by hand with
`--cascade=orphan` first, as above, and let the tool's prune find it already
gone.

Upgrade Weft itself before rolling out any v1 Weave. The release that creates
one needs the `weaves.weft.alethic.solutions` CRD to exist. If it does not, the
release fails before it deletes anything, so the old Weave is left untouched.

### What is not handed over

- **Objects the new program does not declare.** Handover happens as the program
  declares each object, so anything the old Weave made that the new one never
  declares stays orphaned. Delete it by hand, or declare it.
- **A Weave under a different name.** Handover matches on namespace and name. To
  rename as well as move, move it first, then rename. Renaming is a delete and
  a create for the objects too; see [ownership.md](ownership.md).

## When every Weave has moved

Check that nothing is left in the old group:

```bash
kubectl get weaves.weft.run --all-namespaces
```

Then stop serving it:

```bash
helm upgrade weft oci://ghcr.io/alethic/charts/weft --namespace weft-system \
  --reuse-values --set legacyAPI.enabled=false
```

That stops the controller reconciling `weft.run` and drops its grant on it.
It does not delete the `weaves.weft.run` CRD, because `crds.keep` leaves CRDs
in place on purpose. Delete the CRD yourself once the list above is empty:

```bash
kubectl delete crd weaves.weft.run
```

Do not turn `legacyAPI.enabled` off while `weft.run` Weaves remain. Nothing
would reconcile them, and nothing would release their finalizers. They could
not be deleted, and neither could the objects they hold.

## Metrics and dashboards

Per-Weave series (`weft_weave_status`, `weft_weave_resources`,
`weft_evaluation_duration_seconds`) now carry a `group` label, because a
`weft.run` Weave and a v1 Weave can share a namespace and name during a move.
Queries that aggregate with `sum by (namespace, name)` or match on specific
labels keep working. A query that relies on the exact label set does not.

Two controllers run: `weave` reconciles v1, and `weave-weft-run` reconciles
`weft.run`. controller-runtime's own metrics (`controller_runtime_*`) are
labelled with these names. They were previously labelled `weave` for
`weft.run` Weaves.
