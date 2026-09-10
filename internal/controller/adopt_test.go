package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/alethic/weft/internal/naming"
)

func TestAdoptableBy(t *testing.T) {
	for _, tc := range []struct {
		annotation string
		weave      string
		want       bool
		why        string
	}{
		{"", "app", false, "no annotation is no consent"},
		{"app", "app", true, "the exact name"},
		{"other", "app", false, "somebody else's name"},
		{"*", "app", true, "a bare star consents to any Weave in the namespace"},
		{"*", "anything-at-all", true, "including one nobody thought of"},
		{"app-*", "app-staging", true, "a prefix pattern"},
		{"app-*", "app-", true, "matching nothing after the prefix is still a match"},
		{"app-*", "other-staging", false, "a prefix that does not match"},
		{"*-staging", "app-staging", true, "a suffix pattern"},
		{"*-staging", "app-prod", false, "a suffix that does not match"},

		// A pattern that will not compile is not consent. Matching nothing is
		// the safe direction to fail in, and the refusal quotes the value back.
		{"[", "app", false, "a malformed pattern grants nothing"},

		// Weave names are DNS-1123, so a pattern cannot accidentally span one:
		// there is no separator to escape and nothing outside the namespace to
		// reach.
		{"*", "", true, "an empty name still matches a bare star"},
	} {
		got := weftOwnership{adopt: tc.annotation}.adoptableBy(tc.weave)
		if got != tc.want {
			t.Errorf("adopt=%q weave=%q: got %v, want %v (%s)",
				tc.annotation, tc.weave, got, tc.want, tc.why)
		}
	}
}

// A star consents to any Weave in the namespace, which is the form for
// onboarding a set of objects at once: annotating each one with the same Weave
// name is typing that carries no information.
func TestAdoptsOnAWildcard(t *testing.T) {
	h := newHarness(t, nil)
	cm := h.configMap("open-house", map[string]string{"owner": "somebody-else"})
	cm.Annotations = map[string]string{naming.AdoptAnnotation: "*"}
	if err := testK8s.Update(h.ctx, cm); err != nil {
		t.Fatal(err)
	}

	h.create("comer", `
def compose(variable, observed):
    return {
        "open-house": {
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": "open-house"},
            "data": {"owner": "weft"},
        },
    }
`, "")
	h.settle("comer", 2)

	requireCondition(t, h.weave("comer"), naming.ConditionReady, metav1.ConditionTrue)

	adopted, err := h.getConfigMap("open-house")
	if err != nil {
		t.Fatal(err)
	}
	if adopted.Data["owner"] != "weft" {
		t.Errorf("the adopted object should now be managed: %v", adopted.Data)
	}
	refs := adopted.GetOwnerReferences()
	if len(refs) != 1 || refs[0].Name != "comer" {
		t.Errorf("the adopted object should be owned by the Weave: %+v", refs)
	}
}

// A pattern is not a blank cheque. One that does not match is refused like any
// other name that does not match, and the message quotes the pattern so a typo
// in it is visible.
func TestWildcardThatDoesNotMatchIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	cm := h.configMap("prod-only", nil)
	cm.Annotations = map[string]string{naming.AdoptAnnotation: "prod-*"}
	if err := testK8s.Update(h.ctx, cm); err != nil {
		t.Fatal(err)
	}

	h.create("staging-app", `
def compose(variable, observed):
    return {
        "x": {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "prod-only"}},
    }
`, "")
	h.settle("staging-app", 2)

	c := requireCondition(t, h.weave("staging-app"), naming.ConditionDegraded, metav1.ConditionTrue)
	if c.Reason != ReasonNotOurs {
		t.Fatalf("reason = %q (%s)", c.Reason, c.Message)
	}
	if !containsAll(c.Message, `"prod-*"`, `"staging-app"`) {
		t.Errorf("message should quote the pattern and this Weave's name: %s", c.Message)
	}

	adopted, err := h.getConfigMap("prod-only")
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted.GetOwnerReferences()) != 0 {
		t.Errorf("nothing should have been taken over: %+v", adopted.GetOwnerReferences())
	}
}

func containsAll(s string, want ...string) bool {
	for _, w := range want {
		found := false
		for i := 0; i+len(w) <= len(s); i++ {
			if s[i:i+len(w)] == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
