# Demos

Seven Weaves that between them exercise everything Weft does, using only core
Kubernetes types so they run anywhere — including a laptop cluster.

## Setup

```bash
helm install weft oci://ghcr.io/alethic/charts/weft --devel \
  --namespace weft-system --create-namespace \
  --set controller.pruneDelay=45s

kubectl create namespace weft-demo
kubectl -n weft-demo create serviceaccount composer
kubectl -n weft-demo create serviceaccount powerless
kubectl -n weft-demo create role composer \
  --verb=get,list,watch,create,update,patch,delete \
  --resource=configmaps,secrets,serviceaccounts,services,deployments.apps
kubectl -n weft-demo create rolebinding composer \
  --role=composer --serviceaccount=weft-demo:composer

kubectl -n weft-demo create configmap platform \
  --from-literal=tenantId=acme-42 --from-literal=region=eastus
kubectl -n weft-demo annotate configmap platform platform.example/cluster=labs-01
kubectl -n weft-demo create configmap upstream --from-literal=id=upstream-1
```

`pruneDelay=45s` is only so the pruning demo can be watched. The default is two
minutes, and production wants it above whatever your slowest provider takes to
restart.

## 01 — the whole model in one object

```bash
kubectl apply -f demo/01-app-stack.yaml
kubectl -n weft-demo get weave app-stack \
  -o jsonpath='{range .status.inventory[*]}{"wave "}{.wave}{"  "}{.kind}{"/"}{.name}{"\n"}{end}'
```

Eight resources across three waves, from one `Weave`:

```
wave 0  ServiceAccount/shop
wave 1  ConfigMap/shop-dev
wave 1  ConfigMap/shop-prod
wave 1  ConfigMap/shop-staging
wave 1  Secret/shop-derived
wave 1  ConfigMap/shop-values
wave 2  Deployment/shop
wave 2  Service/shop
```

Each wave boundary is a field the API server writes *after* the apply returns —
the ServiceAccount's uid, then the Secret's. That is the same shape as a cloud
provider writing back `status.atProvider` minutes later, just fast enough to
watch. The program does not describe a graph; it returns fewer resources until
the field it needs is there, and `pending()` reports what it is waiting for
while what is ready gets applied anyway.

```bash
kubectl -n weft-demo get cm shop-values -o jsonpath='{.data.values\.yaml}'
```

```yaml
tenant: acme-42
region: eastus
cluster: labs-01
identity:
  serviceAccountName: shop
  uid: ae0e3f04-612c-441c-999a-2085840bcde8
features:
  - search
  - checkout
environments:
  dev: {replicas: 1, debug: true}
  ...
```

Note what that shows: mapping order is the order the program wrote, not
alphabetical, because a person reads this. `cluster` came from an annotation key
that is not an identifier and had to be reached with a bracket path.
`recommendations` is missing from `features` because a comprehension filtered
it. And the `uid` was not knowable when the Weave was written.

The Deployment really runs — `kubectl -n weft-demo get pods -l app=shop`.

## 02 — the third outcome

```bash
kubectl apply -f demo/02-waiting.yaml
kubectl -n weft-demo get weave waiting \
  -o jsonpath='{.status.conditions[?(@.type=="Waiting")].message}'
```

> no licence ConfigMap in this namespace yet

Create them one at a time and watch it narrow, then resolve:

```bash
kubectl -n weft-demo create configmap tenant --from-literal=tenantId=acme-42
kubectl -n weft-demo create configmap licence --from-literal=ok=true
```

`licence` is never read for a value — its existence is the whole requirement.
The gate is a line in the program rather than a flag on a declaration, which is
what lets it be conditional:

```python
if variable.needsLicence and not read("v1", "ConfigMap", "licence"):
    return wait("no licence ConfigMap in this namespace yet")
```

## 03 — why impersonation is the whole point

```bash
kubectl apply -f demo/03-forbidden.yaml
kubectl -n weft-demo describe weave forbidden
```

This names a ServiceAccount with no permissions. The controller has far more and
creates nothing anyway:

```
ServiceAccount "powerless" cannot get configmaps "platform" in namespace "weft-demo"

Grant it with:
  kubectl create role weft-read-configmaps --namespace=weft-demo --verb=get,list,watch --resource=configmaps
  kubectl create rolebinding weft-read-configmaps --namespace=weft-demo --role=weft-read-configmaps --serviceaccount=weft-demo:powerless
```

Paste those and within about ten seconds it advances to the *next* missing
permission, one at a time, until it is done. That loop is the primary user
experience of the tool.

## 04 — ordered teardown

```bash
kubectl apply -f demo/04-teardown.yaml
# Pin the middle wave so teardown cannot pass it.
kubectl -n weft-demo patch cm layer-middle --type=merge \
  -p '{"metadata":{"finalizers":["example.test/hold"]}}'
kubectl -n weft-demo delete weave teardown --wait=false
```

Then:

```
layer-base     configmap/layer-base
layer-middle   configmap/layer-middle  (Terminating)
layer-top      <deleted>
```

Wave 2 gone, wave 1 blocked, and wave 0 **untouched** — no deletion timestamp at
all. Cascading collection would have taken all three at once in no particular
order. Release the hold and the rest drains:

```bash
kubectl -n weft-demo patch cm layer-middle --type=merge -p '{"metadata":{"finalizers":null}}'
```

## 05 — pruning waits on a clock

```bash
kubectl apply -f demo/05-pruning.yaml
kubectl -n weft-demo patch weave pruning --type=json \
  -p '[{"op":"remove","path":"/spec/variables/0/values/regions/1"}]'
```

Watch `missingCount` and the object:

```
t+0s   missingCount=3  object=configmap/region-westeurope
t+26s  missingCount=3  object=configmap/region-westeurope
t+51s  missingCount=0  object=<pruned>
```

The count reaches its threshold immediately, because reconciles are event-driven
and the applies in one pass generate watch events of their own. Only the clock
held it. A count-based hysteresis is no protection at all against the thing it
exists for — during development a removed resource reached 79 "consecutive
evaluations" in 56 seconds.

## 06 — a finalizer on somebody else's object

```bash
kubectl apply -f demo/06-finalizer.yaml
kubectl -n weft-demo get cm upstream -o jsonpath='{.metadata.finalizers}'
kubectl -n weft-demo delete cm upstream
```

The derived output is torn down first, then the finalizer is released and the
held resource disappears. Three rules make this safe to hand to a namespace
user: the permission to *remove* the finalizer is re-checked every pass, it is
released after `--hold-timeout` regardless of progress, and `weft reap` lifts
every one Weft has placed — from `status.held`, so a program that will never
run again is still undoable.

## 07 — every guardrail

```bash
kubectl apply -f demo/07-bounds.yaml
kubectl -n weft-demo get weave -o custom-columns=\
'NAME:.metadata.name,REASON:.status.conditions[?(@.type=="Degraded")].reason'
```

```
bound-budget           ProgramBudgetExceeded
bound-cluster-scoped   ClusterScoped
bound-load             ProgramLoadNotAllowed
bound-mixed-waves      ProgramInvalidOutput
bound-namespace        ProgramInvalidOutput
bound-owner            ProgramInvalidOutput
bound-recursion        ProgramFailed
bound-typo             ProgramFailed
```

Eight ways to be wrong, eight distinguishable reasons, and a controller that is
still running. `bound-typo` is worth reading — a mistyped field names itself and
lists the ones that exist, rather than quietly becoming `None` and rendering a
resource with a blank field:

> object has no field "dta" (has: apiVersion, data, kind, metadata)

**Delete these when you are done.** `bound-budget` re-evaluates on every degraded
retry, which is bounded but not free.

```bash
kubectl -n weft-demo delete -f demo/07-bounds.yaml
```

## Cleaning up

```bash
kubectl -n weft-demo delete weave --all
kubectl delete namespace weft-demo
kubectl -n weft-system exec deploy/weft -- /weft reap
helm uninstall weft --namespace weft-system
kubectl delete crd weaves.weft.run
```

Delete the Weaves before the namespace: they hold a finalizer so their resources
come down in order, and `reap` releases anything Weft placed on objects it does
not own.
