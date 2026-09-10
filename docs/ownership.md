# Ownership

Weft manages what it created, and refuses to touch anything else until somebody
says otherwise. This page is the whole story: how an object becomes Weft's, what
happens when a program changes its mind, and how to hand an existing resource
over on purpose.

## Weft only manages what it made

Server-side apply is create-or-update, so a program that names an object which
already exists would otherwise take it over silently — the apply adds Weft's
owner reference, and from then on deleting the `Weave` deletes a resource Weft
never created. Choosing a name should not be enough to do that.

So a resource that already exists and is not Weft's produces:

```
resource "legacy" would create ConfigMap "legacy", which already exists and this
Weave did not create.

It was last written by "kubectl-create".

Weft will not take over an object it did not create. Applying would add its
owner reference, and deleting this Weave would then delete something it never
made.

To hand it over deliberately, annotate the object itself. Consent belongs to
whoever holds the resource, not to whoever wrote the program:
  kubectl -n weft-demo annotate ConfigMap legacy weft.run/adopt=onboard

From then on this Weave manages it, and deleting the Weave deletes it.
Otherwise change the name the program produces, or remove what is there:
  kubectl -n weft-demo delete ConfigMap legacy
```

Nothing is written, and nothing is recorded in the inventory.

## Onboarding an existing resource

Annotate the object with the name of the `Weave` allowed to take it:

```bash
kubectl -n weft-demo annotate ConfigMap legacy weft.run/adopt=onboard
```

The annotation goes **on the object, not in the Weave**. That is the point: if a
`Weave` could declare its own adoptions, "onboarding" would just be a program
author helping themselves to somebody else's resource. Consent has to come from
whoever holds the thing.

The annotation names one `Weave`. One naming a different `Weave` is not consent
for this one.

On the next reconcile Weft takes ownership and records an event, because this is
a change worth being able to find later:

```
Normal  Adopted  took ownership of ConfigMap "legacy", which was annotated
                 weft.run/adopt=onboard. Deleting this Weave now deletes it.
```

That last sentence is the part to understand before annotating. Adoption is
full ownership: the object gets Weft's owner reference, the program becomes the
authority for every field it sets, and deleting the `Weave` deletes the object.

There is no half-managed state where Weft writes an object but leaves it behind
on delete. That would need a separate ownership mode, and until there is a
coherent design for one, ownership means ownership.

### What adoption does not do

- It does not merge. The program's output is applied over the object, and every
  field the program sets becomes Weft's.
- It does not survive a rename. If the program later addresses a different
  name, the adopted object is treated like any other replaced object and
  deleted — see below.
- It does not transfer between Weaves. An object already owned by one `Weave`
  is refused for another regardless of the annotation, because the two would
  apply over each other on every reconcile.

## Two keys cannot name one object

A key is the identity of a resource in the inventory. Two of them addressing one
object is a contradiction — whichever applied last would win, and the other
would be recorded pointing at something it does not control — so it is refused:

```
resources "first" and "second" both produce ConfigMap "shared".
```

## When a program changes what a key addresses

The key is the identity; the name and kind are what it currently addresses.
Editing a program to change either, under a key it still returns, means the old
object has to go — or it is orphaned: present in the cluster, absent from every
record, never cleaned up.

That was the worst property of the arrangement Weft replaces, where `kubectl
apply` without `--prune` left objects behind and reclaiming them was a manual
job.

```yaml
# before
"thing": {"kind": "ConfigMap", "metadata": {"name": "thing-one"}}
# after
"thing": {"kind": "ConfigMap", "metadata": {"name": "thing-two"}}
```

`thing-two` is created, then `thing-one` is deleted. In between, the old object
is recorded in `status.superseded` rather than in the inventory, because the key
it used to occupy now records its replacement:

```bash
kubectl get weave editable -o jsonpath='{.status.superseded}'
```

Replacements are removed in reverse wave order, the same as everything else, and
no hysteresis applies. A replacement is a statement rather than an absence: the
program said the object is different, so there is nothing to wait out.

## When an object moves to a different key

The opposite edit — the same object returned under a new key — deletes nothing.
The object is still wanted, just recorded elsewhere, and pruning it would
destroy live state that the very same evaluation asked for.

```yaml
# before                                    # after
"old-key": {"metadata": {"name": "x"}}      "new-key": {"metadata": {"name": "x"}}
```

The record moves; the object does not move at all.

## Summary

| situation | what happens |
|---|---|
| object does not exist | created, owned, recorded |
| object exists, this `Weave` owns it | updated |
| object exists, another `Weave` owns it | refused; no annotation helps |
| object exists, nobody owns it | refused, with the annotate command |
| object exists and is annotated for this `Weave` | adopted, with an event |
| two keys name one object | refused |
| a key changes name or kind | new one applied, old one deleted |
| an object moves to a new key | record moves, object untouched |
| a key stops being returned | pruned, after the delay |
