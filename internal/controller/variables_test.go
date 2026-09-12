package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/naming"
)

// echo returns every input back out as a ConfigMap, so a test can read what the
// program actually saw.
const echoVariables = `
def compose(variable):
    data = {}
    for k in variable:
        v = variable[k]
        data[k] = v if type(v) == "string" else to_json(v)
    resource({
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": "echo"},
        "data": data,
        })
`

func (h *harness) createWithVariables(name string, entries []v1alpha1.Variable) *v1alpha1.Weave {
	h.t.Helper()
	w := &v1alpha1.Weave{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.namespace},
		Spec: v1alpha1.WeaveSpec{
			ServiceAccountName: "composer",
			Variables:          entries,
			Program:            echoVariables,
		},
	}
	if err := testK8s.Create(h.ctx, w); err != nil {
		h.t.Fatalf("creating weave: %v", err)
	}
	h.reconcile(name)
	return w
}

func inlineValues(raw string) v1alpha1.Variable {
	return v1alpha1.Variable{Values: &apiextensionsv1.JSON{Raw: []byte(raw)}}
}

// A ConfigMap under a Weave's control, and one written inline over it. A
// program cannot tell which value came from where, which is what makes moving a
// setting between the two not a change to the composition.
func TestVariablesOverlayConfigMapWithInlineValues(t *testing.T) {
	h := newHarness(t, nil)
	h.configMap("platform", map[string]string{
		"region": "eastus",
		"tier":   "standard",
	})

	h.createWithVariables("layered", []v1alpha1.Variable{
		{ConfigMap: &v1alpha1.VariableRef{Name: "platform"}},
		inlineValues(`{"tier": "premium", "extra": "inline"}`),
	})
	h.settle("layered", 2)

	echo, err := h.getConfigMap("echo")
	if err != nil {
		t.Fatalf("the program should have run: %v", err)
	}
	for k, want := range map[string]string{
		"region": "eastus",  // only in the ConfigMap
		"tier":   "premium", // the later entry wins
		"extra":  "inline",  // only inline
	} {
		if echo.Data[k] != want {
			t.Errorf("%s = %q, want %q", k, echo.Data[k], want)
		}
	}
}

// A Secret's values arrive base64-encoded over the wire, and a program should
// see the same thing it would from a ConfigMap.
func TestVariablesFromSecretAreDecoded(t *testing.T) {
	h := newHarness(t, nil)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: h.namespace},
		StringData: map[string]string{"token": "s3cr3t"},
	}
	if err := testK8s.Create(h.ctx, secret); err != nil {
		t.Fatal(err)
	}

	h.createWithVariables("fromsecret", []v1alpha1.Variable{
		{Secret: &v1alpha1.VariableRef{Name: "creds"}},
	})
	h.settle("fromsecret", 2)

	echo, err := h.getConfigMap("echo")
	if err != nil {
		t.Fatal(err)
	}
	if echo.Data["token"] != "s3cr3t" {
		t.Errorf("token = %q, want the decoded value", echo.Data["token"])
	}
}

// A single key holding a document, which is how a values.yaml ends up in a
// ConfigMap in the first place.
func TestVariablesFromAKeyParsedAsYAML(t *testing.T) {
	h := newHarness(t, nil)
	h.configMap("values", map[string]string{
		"values.yaml": "image:\n  repository: base\n  tag: v1\nreplicas: 2\n",
	})

	h.createWithVariables("document", []v1alpha1.Variable{
		{ConfigMap: &v1alpha1.VariableRef{Name: "values", Key: "values.yaml"}},
		inlineValues(`{"image": {"tag": "v2"}}`),
	})
	h.settle("document", 2)

	echo, err := h.getConfigMap("echo")
	if err != nil {
		t.Fatal(err)
	}
	// The document was parsed into a mapping, and the inline entry merged into
	// it key by key rather than replacing the whole thing.
	if !strings.Contains(echo.Data["image"], `"repository":"base"`) ||
		!strings.Contains(echo.Data["image"], `"tag":"v2"`) {
		t.Errorf("image = %q, want repository kept and tag overridden", echo.Data["image"])
	}
	if echo.Data["replicas"] != "2" {
		t.Errorf("replicas = %q", echo.Data["replicas"])
	}
}

// Configuration that has not arrived yet is the ordinary "not yet", not a
// failure.
func TestVariablesWaitForAMissingObject(t *testing.T) {
	h := newHarness(t, nil)

	h.createWithVariables("waiting", []v1alpha1.Variable{
		{ConfigMap: &v1alpha1.VariableRef{Name: "not-there"}},
	})
	h.settle("waiting", 2)

	c := requireCondition(t, h.weave("waiting"), naming.ConditionWaiting, metav1.ConditionTrue)
	if c.Reason != ReasonVariableMissing {
		t.Errorf("reason = %q, want %q", c.Reason, ReasonVariableMissing)
	}
	if h.exists("echo") {
		t.Error("nothing should be created while configuration is missing")
	}

	// It arrives, and the Weave resolves itself.
	h.configMap("not-there", map[string]string{"now": "here"})
	h.settle("waiting", 2)
	requireCondition(t, h.weave("waiting"), naming.ConditionReady, metav1.ConditionTrue)

	echo, err := h.getConfigMap("echo")
	if err != nil {
		t.Fatal(err)
	}
	if echo.Data["now"] != "here" {
		t.Errorf("data = %v", echo.Data)
	}
}

// Optional entries are for configuration that may legitimately not be there.
func TestVariablesSkipAnOptionalMissingObject(t *testing.T) {
	h := newHarness(t, nil)

	h.createWithVariables("optional", []v1alpha1.Variable{
		inlineValues(`{"base": "yes"}`),
		{ConfigMap: &v1alpha1.VariableRef{Name: "absent", Optional: true}},
	})
	h.settle("optional", 2)

	requireCondition(t, h.weave("optional"), naming.ConditionReady, metav1.ConditionTrue)
	echo, err := h.getConfigMap("echo")
	if err != nil {
		t.Fatal(err)
	}
	if echo.Data["base"] != "yes" {
		t.Errorf("data = %v", echo.Data)
	}
}

// Editing the ConfigMap changes what the composition produces, without touching
// the Weave.
func TestVariablesReactToAConfigMapChange(t *testing.T) {
	h := newHarness(t, nil)
	h.configMap("settings", map[string]string{"mode": "before"})

	h.createWithVariables("reactive", []v1alpha1.Variable{
		{ConfigMap: &v1alpha1.VariableRef{Name: "settings"}},
	})
	h.settle("reactive", 2)

	echo, _ := h.getConfigMap("echo")
	if echo.Data["mode"] != "before" {
		t.Fatalf("mode = %q", echo.Data["mode"])
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

	echo, _ = h.getConfigMap("echo")
	if echo.Data["mode"] != "after" {
		t.Errorf("mode = %q, want the edited value", echo.Data["mode"])
	}
}

// Configuration is read as the Weave's ServiceAccount like everything else, so
// a Weave can only take configuration from objects it could read directly.
func TestVariablesAreReadUnderImpersonation(t *testing.T) {
	h := newHarness(t, nil)
	h.configMap("secrets-of-state", map[string]string{"classified": "yes"})

	w := &v1alpha1.Weave{
		ObjectMeta: metav1.ObjectMeta{Name: "nosy", Namespace: h.namespace},
		Spec: v1alpha1.WeaveSpec{
			// Granted nothing at all.
			ServiceAccountName: "powerless",
			Variables: []v1alpha1.Variable{
				{ConfigMap: &v1alpha1.VariableRef{Name: "secrets-of-state"}},
			},
			Program: echoVariables,
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
	if h.exists("echo") {
		t.Error("nothing should have been produced from configuration it cannot read")
	}
}
