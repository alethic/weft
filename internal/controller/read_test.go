package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/naming"
)

// A program that reads something gets it, impersonated like everything else.
func TestReadResolvesThroughImpersonation(t *testing.T) {
	h := newHarness(t, nil)
	h.configMap("tenant", map[string]string{"tenantId": "acme-42"})

	h.create("reader", `
def compose(variable, observed):
    tenant = read("v1", "ConfigMap", "tenant")
    return {
        "greeting": {
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": "greeting"},
            "data": {"tenant": require(tenant, "data.tenantId")},
        },
    }
`, "")
	h.settle("reader", 2)

	cm, err := h.getConfigMap("greeting")
	if err != nil {
		t.Fatalf("the program should have run: %v", err)
	}
	if cm.Data["tenant"] != "acme-42" {
		t.Errorf("tenant = %q", cm.Data["tenant"])
	}
}

// The read set is recorded on status, because it is the only record of what
// this composition depends on. Nothing declares it any more.
func TestReadsAreRecordedOnStatus(t *testing.T) {
	h := newHarness(t, nil)
	h.configMap("tenant", map[string]string{"tenantId": "acme-42"})

	h.create("recorder", `
def compose(variable, observed):
    read("v1", "ConfigMap", "tenant")
    return {}
`, "")
	h.settle("recorder", 2)

	reads := h.weave("recorder").Status.Reads
	if !hasRead(reads, "v1", "ConfigMap") {
		t.Errorf("reads = %+v, want the ConfigMap it read", reads)
	}
}

// And the recording is load-bearing: a change to something the program read
// wakes the Weave, with nothing declared anywhere that says so.
func TestAChangeToSomethingReadWakesTheWeave(t *testing.T) {
	h := newHarness(t, nil)
	h.configMap("settings", map[string]string{"mode": "before"})

	h.create("reactive", `
def compose(variable, observed):
    settings = read("v1", "ConfigMap", "settings")
    return {
        "echo": {
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": "echo"},
            "data": {"mode": require(settings, "data.mode")},
        },
    }
`, "")
	h.settle("reactive", 2)

	if cm, _ := h.getConfigMap("echo"); cm.Data["mode"] != "before" {
		t.Fatalf("mode = %q", cm.Data["mode"])
	}

	settings, err := h.getConfigMap("settings")
	if err != nil {
		t.Fatal(err)
	}
	settings.Data["mode"] = "after"
	if err := testK8s.Update(h.ctx, settings); err != nil {
		t.Fatal(err)
	}
	h.settle("reactive", 2)

	cm, _ := h.getConfigMap("echo")
	if cm.Data["mode"] != "after" {
		t.Errorf("mode = %q, want the edited value", cm.Data["mode"])
	}
}

// A read of something that does not exist is a value, and the program decides
// what it means. This is the gate that a required flag on a declared source
// used to provide, minus the flag.
func TestAbsentReadGatesInTheProgram(t *testing.T) {
	h := newHarness(t, nil)

	h.create("gated", `
def compose(variable, observed):
    if not read("v1", "ConfigMap", "licence"):
        return wait("no licence ConfigMap in this namespace yet")
    return {
        "greeting": {
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": "greeting"},
        },
    }
`, "")
	h.settle("gated", 2)

	c := requireCondition(t, h.weave("gated"), naming.ConditionWaiting, metav1.ConditionTrue)
	if c.Message != "no licence ConfigMap in this namespace yet" {
		t.Errorf("message = %q, want the program's own reason", c.Message)
	}
	if h.exists("greeting") {
		t.Error("nothing should exist while the gate is closed")
	}

	// The recording has to include the read that came back empty, or nothing
	// would ever wake this Weave.
	if !hasRead(h.weave("gated").Status.Reads, "v1", "ConfigMap") {
		t.Fatal("a read that found nothing is still a dependency")
	}

	h.configMap("licence", map[string]string{"ok": "true"})
	h.settle("gated", 2)
	requireCondition(t, h.weave("gated"), naming.ConditionReady, metav1.ConditionTrue)
	if !h.exists("greeting") {
		t.Error("the gate opened, so the resource should exist")
	}
}

// Reading something the ServiceAccount cannot read is a permission failure, not
// an absence, and it names the resource.
func TestReadIsDeniedWithoutPermission(t *testing.T) {
	h := newHarness(t, nil)
	h.configMap("classified", map[string]string{"secret": "yes"})

	w := &v1alpha1.Weave{
		ObjectMeta: metav1.ObjectMeta{Name: "nosy", Namespace: h.namespace},
		Spec: v1alpha1.WeaveSpec{
			ServiceAccountName: "powerless",
			Program: `
def compose(variable, observed):
    return {"x": read("v1", "ConfigMap", "classified")}
`,
		},
	}
	if err := testK8s.Create(h.ctx, w); err != nil {
		t.Fatal(err)
	}
	h.settle("nosy", 2)

	c := requireCondition(t, h.weave("nosy"), naming.ConditionDegraded, metav1.ConditionTrue)
	if c.Reason != ReasonForbidden {
		t.Errorf("reason = %q, want %q (%s)", c.Reason, ReasonForbidden, c.Message)
	}
}

// select() matches by label and comes back sorted, so a composition built from
// a selection applies in a stable order.
func TestSelectMatchesByLabel(t *testing.T) {
	h := newHarness(t, nil)
	for _, tc := range []struct {
		name string
		role string
	}{{"beta", "tenant"}, {"alpha", "tenant"}, {"other", "noise"}} {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tc.name,
				Namespace: h.namespace,
				Labels:    map[string]string{"role": tc.role},
			},
			Data: map[string]string{"n": tc.name},
		}
		if err := testK8s.Create(h.ctx, cm); err != nil {
			t.Fatal(err)
		}
	}

	h.create("selector", `
def compose(variable, observed):
    found = select("v1", "ConfigMap", labels={"role": "tenant"})
    return {
        "list": {
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": "list"},
            "data": {"names": ",".join([c.metadata.name for c in found])},
        },
    }
`, "")
	h.settle("selector", 2)

	cm, err := h.getConfigMap("list")
	if err != nil {
		t.Fatalf("the program should have run: %v", err)
	}
	if cm.Data["names"] != "alpha,beta" {
		t.Errorf("names = %q, want the two labelled ones, sorted", cm.Data["names"])
	}
}

// hold=True holds a resource against deletion, and the hold is recorded so
// it can be released by a controller that never runs the program again.
func TestFinalizeRecordsAndPlacesAHold(t *testing.T) {
	h := newHarness(t, nil)
	h.configMap("upstream", map[string]string{"ok": "true"})

	h.create("holder", `
def compose(variable, observed):
    up = read("v1", "ConfigMap", "upstream", hold=True)
    if not up:
        return wait("no upstream yet")
    return {
        "derived": {
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": "derived"},
        },
    }
`, "")
	h.settle("holder", 2)

	held := h.weave("holder").Status.Held
	if len(held) != 1 || held[0].Kind != "ConfigMap" || held[0].Name != "upstream" {
		t.Fatalf("held = %+v", held)
	}

	upstream, err := h.getConfigMap("upstream")
	if err != nil {
		t.Fatal(err)
	}
	want := naming.HeldFinalizer(h.namespace, "holder")
	if !hasFinalizer(upstream.Finalizers, want) {
		t.Errorf("finalizers = %v, want %q", upstream.Finalizers, want)
	}
}

// A program that stops asking for a hold gets it released, without anybody
// editing anything but the program.
func TestHoldIsReleasedWhenTheProgramStopsAskingForIt(t *testing.T) {
	h := newHarness(t, nil)
	h.configMap("upstream", map[string]string{"ok": "true"})

	h.create("relaxing", `
def compose(variable, observed):
    read("v1", "ConfigMap", "upstream", hold=True)
    return {}
`, "")
	h.settle("relaxing", 2)
	if len(h.weave("relaxing").Status.Held) != 1 {
		t.Fatal("expected a hold to be placed")
	}

	w := h.weave("relaxing")
	w.Spec.Program = `
def compose(variable, observed):
    read("v1", "ConfigMap", "upstream")
    return {}
`
	if err := testK8s.Update(h.ctx, w); err != nil {
		t.Fatal(err)
	}
	h.settle("relaxing", 2)

	if held := h.weave("relaxing").Status.Held; len(held) != 0 {
		t.Errorf("held = %+v, want it released", held)
	}
	upstream, err := h.getConfigMap("upstream")
	if err != nil {
		t.Fatal(err)
	}
	if hasFinalizer(upstream.Finalizers, naming.HeldFinalizer(h.namespace, "relaxing")) {
		t.Errorf("finalizer still on the object: %v", upstream.Finalizers)
	}
}

func hasRead(reads []v1alpha1.ReadRef, apiVersion, kind string) bool {
	for _, r := range reads {
		if r.APIVersion == apiVersion && r.Kind == kind {
			return true
		}
	}
	return false
}

func hasFinalizer(list []string, want string) bool {
	for _, f := range list {
		if f == want {
			return true
		}
	}
	return false
}
