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
this program declares ConfigMap "legacy", which already exists and this Weave
did not create.

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

### Patterns

The value is a name or a shell-style pattern, so a set can be onboarded without
naming the same `Weave` on every object:

```bash
# Any Weave in this namespace.
kubectl -n weft-demo annotate ConfigMap legacy weft.run/adopt='*'

# Any whose name starts with app-.
kubectl -n weft-demo annotate ConfigMap legacy weft.run/adopt='app-*'
```

A pattern is still consent and still namespaced. A `Weave` only ever acts in its
own namespace, so `*` grants no more than "anybody who can already create a
`Weave` here" — and creating one there is not something an outsider can do.
Reach for it when annotating each object with the same name is typing that
carries no extra information; use the exact name when it does carry some.

An annotation that does not match is not consent, and the refusal quotes it
back, so a typo in a pattern shows up as a refusal that names it rather than as
an adoption nobody meant. A pattern that will not compile matches nothing, which
is the safe direction to fail in.

On the next reconcile Weft takes ownership and records an event, because this is
a change worth being able to find later:

```
Normal  Adopted  took ownership of ConfigMap "legacy", which was annotated
                 weft.run/adopt=onboard. Deleting this Weave now deletes it.
```

That last sentence is the part to understand before annotating. Adoption is
full ownership: the object gets Weft's owner reference, the program becomes the
authority for every field it sets, and deleting the `Weave` deletes the object.

If you later want it handed back rather than destroyed, release it with
`--cascade=orphan` — see below. There is no half-managed mode where Weft writes
an object and leaves it behind on delete; releasing is the deliberate act
instead.

### What adoption does not do

- It does not merge. The program's output is applied over the object, and every
  field the program sets becomes Weft's.
- It does not survive a rename. If the program later addresses a different
  name, the adopted object is treated like any other replaced object and
  deleted — see below.
- It does not transfer between Weaves. An object already owned by one `Weave`
  is refused for another regardless of the annotation, because the two would
  apply over each other on every reconcile.

## Opting a resource out of ownership

A resource annotated `weft.run/owned: "false"` is applied without an owner
reference. Weft keeps it current for as long as the program declares it, and
never deletes it: not when it stops being declared, not when the `Weave` is
deleted.

```python
"metadata": {
    "name": "app-db",
    "annotations": {"weft.run/owned": "false"},
}
```

This is the per-resource form of `--cascade=orphan`, decided by whoever wrote
the composition rather than by whoever deletes the `Weave`. Reach for it when
the object outlives the thing describing it.

Ceasing to declare it drops the inventory entry immediately, with no
hysteresis: hysteresis exists to avoid destroying something over a transient
absence, and nothing here is destroyed. An event records it, and the object
keeps its `weft.run/weave` label.

## Deleting a Weave takes its resources

Deleting a `Weave` deletes what it created, in reverse wave order. That is not
only convenience: without it the model would contradict itself. Removing one
resource from a program deletes that resource, so removing the whole program
deleting nothing would be the strange case, and there would be no way to
uninstall what a `Weave` produced except by hand.

### Keeping them

Kubernetes already has the verb for "delete the owner, keep the children", and
Weft honours it:

```bash
kubectl delete weave app --cascade=orphan
```

The resources stay, garbage collection strips Weft's owner references, and they
end up belonging to nobody — unmanaged, exactly as if they had been created by
hand. The `Weave` records an event saying so before it goes.

Ignoring that flag and deleting them anyway would be worse than not supporting
it, because the person believes they have protected the resources.

It works by checking ownership rather than by looking for the flag. Garbage
collection strips the owner references as soon as it processes the deletion,
which can happen before this controller reconciles at all — so watching for the
`orphan` finalizer is a race, and losing it means deleting exactly the resources
somebody asked to keep. Weft instead deletes only what still carries its owner
reference, which is not transient and cannot be missed.

That has a consequence worth knowing: **removing Weft's owner reference from an
object releases it.** The `Weave` stops managing it and will not delete it,
though it will notice the object still exists and refuse to recreate one by that
name until it is adopted again.

This is also the answer for anything that was **adopted**. Taking over an
existing object means the `Weave` would delete it on the way out, which is a
real consequence for something that existed first. If you want it handed back
rather than destroyed, release it with `--cascade=orphan` — and if you want it
back under management later, annotate it again.

Orphaned resources keep their `weft.run/weave` label and `weft.run/weave-uid`
annotation. A new `Weave` that tries to produce them will be refused as
belonging to nobody, and can be given them again with the adopt annotation.

## Reclaiming what a Weave already made

Everything Weft applies carries `weft.run/weave`, `weft.run/weave-uid` and
`weft.run/weave-uid`. The `Weave`'s status records the same thing, but a status can be
lost, and an unowned resource has no owner reference to fall back on — so
without something written on the object, a lost inventory would leave a `Weave`
permanently refusing to touch resources it made itself.

An object carrying this `Weave`'s UID is taken up again rather than refused, and
an event records it. The same applies to an owned resource whose owner reference
was stripped by hand: it is reclaimed, and applying it again puts the reference
back.

The UID, not the name. A `Weave` deleted and recreated under the same name is a
different object, and its predecessor's resources are not automatically its to
take back — otherwise the label, which anybody who can write the object can set,
would be a way to hand Weft something it never made. Those need the adopt
annotation like anything else.

## Renaming is a prune and a create

A resource is identified by the object it describes. There is no slot for a
rename to happen inside: editing a program to change a name or a kind is one
object no longer declared and another declared in its place.

```python
# before                                    # after
resource({"kind": "ConfigMap",              resource({"kind": "ConfigMap",
          "metadata": {"name": "one"}})               "metadata": {"name": "two"}})
```

`two` is created. `one` stops being declared, so it is pruned like anything else
that stopped being declared — which means it waits out `--prune-delay` first,
and the two exist alongside each other until it does.

That delay is the honest cost of saying it this way. An earlier design gave a
resource a key of its own so a rename could be recognised as a replacement and
ordered — new applied, old deleted immediately — but the key was a slot that
existed only for the controller's benefit, and nothing an author has. Stop
declaring one thing and start declaring another is what a rename *is*.

Declaring the same object twice is refused rather than treated as two:

```
ConfigMap v1/settings is declared twice. An object is one thing, so two
declarations cannot mean two of them
```

## Summary

| situation | what happens |
|---|---|
| object does not exist | created, owned, recorded |
| object exists, this `Weave` owns it | updated |
| object exists, another `Weave` owns it | refused; no annotation helps |
| object exists, nobody owns it | refused, with the annotate command |
| object exists and carries this `Weave`'s UID | reclaimed, with an event |
| object exists and carries a different `Weave`'s UID | refused; the annotation is not consent |
| object exists and is annotated for this `Weave` | adopted, with an event |
| the same object is declared twice | refused |
| a declared object changes name or kind | the new one is created, the old one pruned after the delay |
| an object stops being declared | pruned, after the delay |
| the `Weave` is deleted | its resources are deleted, in reverse wave order |
| a resource is annotated `weft.run/owned: "false"` | never deleted; released when it leaves the set |
| the `Weave` is deleted with `--cascade=orphan` | resources kept, owner references stripped |
