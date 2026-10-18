package main

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/yaml"

	"github.com/alethic/weft/internal/naming"
)

// The chart's rendered arguments are parsed here with the very flag set the
// binary uses.
//
// This exists because of a specific failure: YAML decodes 20000000 as a
// float64, Helm renders that as "2e+07", and the uint flag rejects it - so the
// chart installs cleanly and the container crash-loops on a value nobody typed
// wrong. Nothing short of actually parsing the arguments catches that, and a
// chart that produces an unstartable Deployment is worse than no chart.
func renderChart(t *testing.T, args ...string) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed; skipping chart rendering tests")
	}

	full := append([]string{"template", "weft", "../../charts/weft", "--namespace", "weft-system"}, args...)
	cmd := exec.Command(helm, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

// renderWithValues renders the chart from a values file.
//
// Helm's --set parser cannot carry a value containing braces, which is exactly
// the shape of a templated impersonation group. A values file is the only way
// to express it - worth knowing, because a user configuring one hits the same
// wall.
func renderWithValues(t *testing.T, values string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("helm", "template", "weft", "../../charts/weft",
		"--namespace", "weft-system", "--values", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stderr.String(), err
	}
	return stdout.String(), nil
}

// rbacRule is one rule of a rendered Role or ClusterRole.
type rbacRule struct {
	APIGroups     []string `json:"apiGroups"`
	Resources     []string `json:"resources"`
	Verbs         []string `json:"verbs"`
	ResourceNames []string `json:"resourceNames"`
}

type renderedRole struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Rules []rbacRule `json:"rules"`
}

// roles parses every Role and ClusterRole out of a rendered chart. Parsing
// rather than grepping matters: the templates carry comments that mention the
// very resource names a substring search would look for.
func roles(t *testing.T, rendered string) []renderedRole {
	t.Helper()
	var out []renderedRole
	for _, doc := range strings.Split(rendered, "\n---\n") {
		var r renderedRole
		if err := yaml.Unmarshal([]byte(doc), &r); err != nil {
			continue
		}
		if r.Kind == "Role" || r.Kind == "ClusterRole" {
			out = append(out, r)
		}
	}
	return out
}

// grants reports whether any of these roles allows a verb on a resource.
func grants(rs []renderedRole, kind, resource, verb string) *renderedRole {
	for i := range rs {
		if rs[i].Kind != kind {
			continue
		}
		for _, rule := range rs[i].Rules {
			hasResource, hasVerb := false, false
			for _, r := range rule.Resources {
				if r == resource {
					hasResource = true
				}
			}
			for _, v := range rule.Verbs {
				if v == verb {
					hasVerb = true
				}
			}
			if hasResource && hasVerb {
				return &rs[i]
			}
		}
	}
	return nil
}

type renderedDeployment struct {
	Kind string `json:"kind"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name  string   `json:"name"`
					Image string   `json:"image"`
					Args  []string `json:"args"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// containerArgs pulls the manager's arguments out of a rendered chart.
func containerArgs(t *testing.T, rendered string) []string {
	t.Helper()
	for _, doc := range strings.Split(rendered, "\n---\n") {
		if !strings.Contains(doc, "kind: Deployment") {
			continue
		}
		var d renderedDeployment
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			t.Fatalf("parsing rendered Deployment: %v", err)
		}
		if d.Kind != "Deployment" || len(d.Spec.Template.Spec.Containers) == 0 {
			continue
		}
		return d.Spec.Template.Spec.Containers[0].Args
	}
	t.Fatal("the chart rendered no Deployment")
	return nil
}

// parseArgs runs the rendered arguments through the real flag set.
func parseArgs(t *testing.T, args []string) *config {
	t.Helper()
	cfg := &config{}
	fs := flag.NewFlagSet("weft", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	bindFlags(fs, cfg)
	zapOpts := zap.Options{}
	zapOpts.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("the chart rendered arguments the controller cannot parse: %v\nargs: %v", err, args)
	}
	return cfg
}

func TestChartArgsParse(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"defaults", nil},
		{"metrics off", []string{"--set", "metrics.enabled=false"}},
		{"no leader election", []string{"--set", "controller.leaderElection.enabled=false"}},
		{"single namespace", []string{"--set", "controller.watchNamespace=team-a"}},
		{"tuned", []string{
			"--set", "controller.pruneDelay=15m",
			"--set", "controller.pruneThreshold=10",
			"--set", "controller.evaluator.maxSteps=100000000",
			"--set", "controller.evaluator.maxValues=1000000",
			"--set", "controller.log.level=debug",
			"--set", "controller.log.encoder=console",
		}},
		{"extra args", []string{"--set", "controller.extraArgs={--zap-devel}"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := containerArgs(t, renderChart(t, tc.args...))
			if len(args) == 0 {
				t.Fatal("no arguments rendered")
			}
			// A value that round-tripped through a float is the failure this
			// test exists for, and it is worth naming rather than leaving to a
			// generic parse error.
			for _, a := range args {
				if strings.Contains(a, "e+") {
					t.Errorf("argument %q is in scientific notation; pass the value through int or int64 in the template", a)
				}
			}
			parseArgs(t, args)
		})
	}
}

// The flags the chart emits must be the ones the binary actually has. A renamed
// flag would otherwise surface as a crash-loop after deployment.
func TestChartValuesReachTheController(t *testing.T) {
	args := containerArgs(t, renderChart(t,
		"--set", "controller.pruneDelay=9m",
		"--set", "controller.evaluator.maxSteps=12345678",
		"--set", "controller.watchNamespace=team-a",
		"--set", "controller.impersonateGroups={system:authenticated}",
	))
	cfg := parseArgs(t, args)

	if cfg.opts.PruneDelay.String() != "9m0s" {
		t.Errorf("PruneDelay = %s, want 9m0s", cfg.opts.PruneDelay)
	}
	if cfg.evalOpt.MaxSteps != 12345678 {
		t.Errorf("MaxSteps = %d, want 12345678", cfg.evalOpt.MaxSteps)
	}
	if cfg.watchNamespace != "team-a" {
		t.Errorf("watchNamespace = %q", cfg.watchNamespace)
	}
	if got := parseGroups(cfg.groups); len(got) != 1 || got[0] != "system:authenticated" {
		t.Errorf("impersonate groups = %v", got)
	}
}

// Impersonating groups without a resourceNames restriction is the one
// configuration that hands cluster-admin to anyone who compromises the
// controller, so the chart refuses to render it silently.
func TestChartRefusesUnrestrictedGroupImpersonation(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	out, err := renderWithValues(t, `
controller:
  impersonateGroups:
    - system:serviceaccounts
    - system:serviceaccounts:{namespace}
`)
	if err == nil {
		t.Fatal("a templated group should be refused without an explicit acknowledgement")
	}
	if !strings.Contains(out, "system:masters") {
		t.Errorf("the refusal should say what the risk is, got: %s", out)
	}
}

// And renders it when the risk is accepted, without the restriction that cannot
// express it.
func TestChartAllowsUnrestrictedGroupImpersonationWhenAccepted(t *testing.T) {
	out, err := renderWithValues(t, `
rbac:
  allowUnrestrictedGroupImpersonation: true
controller:
  impersonateGroups:
    - system:serviceaccounts
    - system:serviceaccounts:{namespace}
`)
	if err != nil {
		t.Fatalf("render: %s", out)
	}

	role := grants(roles(t, out), "ClusterRole", "groups", "impersonate")
	if role == nil {
		t.Fatal("the group impersonation rule is missing")
	}
	for _, rule := range role.Rules {
		for _, r := range rule.Resources {
			if r == "groups" && len(rule.ResourceNames) > 0 {
				t.Error("a templated group cannot be pinned by name, so the rule must not carry resourceNames")
			}
		}
	}
}

// Confining impersonation is the reason the scope value exists.
func TestChartNamespacedImpersonation(t *testing.T) {
	out := renderChart(t,
		"--set", "rbac.impersonation.scope=namespaced",
		"--set", "rbac.impersonation.namespaces={team-a,team-b}")

	rs := roles(t, out)

	// The cluster-wide rule must be gone, or confining it achieved nothing.
	if grants(rs, "ClusterRole", "serviceaccounts", "impersonate") != nil {
		t.Error("the ClusterRole still grants cluster-wide ServiceAccount impersonation")
	}

	// And each namespace must have gained one of its own.
	for _, ns := range []string{"team-a", "team-b"} {
		found := false
		for _, r := range rs {
			if r.Kind != "Role" || r.Metadata.Namespace != ns {
				continue
			}
			for _, rule := range r.Rules {
				for _, res := range rule.Resources {
					if res == "serviceaccounts" {
						found = true
					}
				}
			}
		}
		if !found {
			t.Errorf("no ServiceAccount impersonation Role in %q", ns)
		}
	}
}

func TestChartNamespacedImpersonationNeedsNamespaces(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	cmd := exec.Command("helm", "template", "weft", "../../charts/weft",
		"--namespace", "weft-system", "--set", "rbac.impersonation.scope=namespaced")
	if err := cmd.Run(); err == nil {
		t.Fatal("namespaced scope with no namespaces would leave Weft unable to impersonate anywhere")
	}
}

// Several replicas without leader election would reconcile the same Weave and
// apply over each other.
func TestChartRefusesMultipleReplicasWithoutLeaderElection(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	cmd := exec.Command("helm", "template", "weft", "../../charts/weft",
		"--namespace", "weft-system",
		"--set", "replicaCount=3",
		"--set", "controller.leaderElection.enabled=false")
	if err := cmd.Run(); err == nil {
		t.Fatal("this combination should be refused")
	}
}

// Uninstalling a controller must not be able to destroy infrastructure.
func TestChartKeepsTheCRDByDefault(t *testing.T) {
	out := renderChart(t)
	for _, api := range naming.APIs {
		crd := documentContaining(t, out, "name: "+api.CRDName)
		if !strings.Contains(crd, "helm.sh/resource-policy: keep") {
			t.Errorf("the %s CRD should be kept on uninstall by default: deleting it deletes every Weave, "+
				"and each takes the resources it owns with it", api.Group)
		}
	}

	out = renderChart(t, "--set", "crds.keep=false")
	for _, api := range naming.APIs {
		crd := documentContaining(t, out, "name: "+api.CRDName)
		if strings.Contains(crd, "helm.sh/resource-policy: keep") {
			t.Errorf("crds.keep=false should drop the annotation from the %s CRD", api.Group)
		}
	}

	out = renderChart(t, "--set", "crds.install=false")
	if strings.Contains(out, "kind: CustomResourceDefinition") {
		t.Error("crds.install=false should render no CRD")
	}
}

// crdFiles names the generated CRD for every served group.
func crdFiles() []string {
	out := make([]string, 0, len(naming.APIs))
	for _, api := range naming.APIs {
		out = append(out, api.Group+"_"+naming.Plural+".yaml")
	}
	return out
}

// The chart's CRDs are the generated ones, so the two cannot drift.
func TestChartCRDMatchesGenerated(t *testing.T) {
	for _, f := range crdFiles() {
		generated := readFile(t, "../../config/crd/"+f)
		shipped := readFile(t, "../../charts/weft/files/crds/"+f)
		if generated != shipped {
			t.Errorf("charts/weft/files/crds/%s differs from the generated CRD; run make generate", f)
		}
	}
}

// Both groups are served by default, so an upgrade from a release that only
// knew weft.run keeps every existing Weave reconciled. Turning the legacy group
// off removes all of it - CRD, grant and controller - together, since any one
// without the others is a controller that cannot start or a CRD nothing serves.
func TestChartServesLegacyAPIUnlessDisabled(t *testing.T) {
	out := renderChart(t)
	for _, want := range []string{
		"name: " + naming.V1.CRDName,
		"name: " + naming.Legacy.CRDName,
		`apiGroups: ["` + naming.Legacy.Group + `"]`,
		"--serve-legacy-api=true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("default render is missing %q", want)
		}
	}

	out = renderChart(t, "--set", "legacyAPI.enabled=false")
	if !strings.Contains(out, "name: "+naming.V1.CRDName) {
		t.Error("disabling the legacy API removed the v1 CRD")
	}
	for _, unwanted := range []string{
		"name: " + naming.Legacy.CRDName,
		`apiGroups: ["` + naming.Legacy.Group + `"]`,
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("legacyAPI.enabled=false still renders %q", unwanted)
		}
	}
	if !strings.Contains(out, "--serve-legacy-api=false") {
		t.Error("legacyAPI.enabled=false must tell the controller not to serve it")
	}
}

// The published chart pins its image by a digest written into
// files/image-digest. It must lose to anything the user set, and must not live
// in values.yaml, where helm upgrade --reuse-values would carry a previous
// release's digest - and so its binary - forward under a newer chart.
func TestChartImagePrecedence(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	const published = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const user = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../charts/weft")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "files", "image-digest"), []byte(published+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	render := func(args ...string) string {
		t.Helper()
		full := append([]string{"template", "weft", dir, "--namespace", "weft-system"}, args...)
		out, err := exec.Command("helm", full...).CombinedOutput()
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, out)
		}
		return string(out)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "ghcr.io/alethic/weft@" + published},
		{[]string{"--set", "image.tag=custom"}, "ghcr.io/alethic/weft:custom"},
		{[]string{"--set", "image.digest=" + user}, "ghcr.io/alethic/weft@" + user},
		{[]string{"--set", "image.repository=mirror.example/weft"}, "mirror.example/weft@" + published},
	} {
		if out := render(c.args...); !strings.Contains(out, "image: "+c.want) {
			t.Errorf("with %v the image is not %s", c.args, c.want)
		}
	}

	if strings.Contains(readFile(t, "../../charts/weft/values.yaml"), "sha256:") {
		t.Error("values.yaml carries a digest; --reuse-values would pin upgrades to it")
	}
}

// helm upgrade --reuse-values carries the previous release's values, which
// predate legacyAPI. The chart must render with the key missing altogether -
// and treat it as on, since off would stop reconciling every weft.run Weave.
func TestChartRendersWithoutLegacyAPIValue(t *testing.T) {
	out := renderChart(t, "--set", "legacyAPI=null")
	for _, want := range []string{"name: " + naming.Legacy.CRDName, "--serve-legacy-api=true"} {
		if !strings.Contains(out, want) {
			t.Errorf("with legacyAPI unset the render is missing %q", want)
		}
	}
}

// documentContaining returns the first rendered document containing a marker.
func documentContaining(t *testing.T, rendered, marker string) string {
	t.Helper()
	for _, doc := range strings.Split(rendered, "\n---\n") {
		if strings.Contains(doc, marker) {
			return doc
		}
	}
	t.Fatalf("no rendered document contains %q", marker)
	return ""
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

// The chart parses the generated CRD in order to attach labels and the keep
// annotation, then re-serialises it. That round trip has to be lossless: a
// silently dropped validation rule would let the API server accept a Weave the
// controller cannot honour.
func TestChartCRDRoundTripIsLossless(t *testing.T) {
	out := renderChart(t)
	for _, api := range naming.APIs {
		rendered := documentContaining(t, out, "name: "+api.CRDName)

		var fromChart, fromGenerator map[string]any
		if err := yaml.Unmarshal([]byte(rendered), &fromChart); err != nil {
			t.Fatalf("parsing the chart's %s CRD: %v", api.Group, err)
		}
		generated := readFile(t, "../../config/crd/"+api.Group+"_"+naming.Plural+".yaml")
		if err := yaml.Unmarshal([]byte(generated), &fromGenerator); err != nil {
			t.Fatalf("parsing the generated %s CRD: %v", api.Group, err)
		}

		if !reflect.DeepEqual(fromChart["spec"], fromGenerator["spec"]) {
			t.Errorf("the chart's rendered %s CRD spec differs from the generated one; the templating is lossy", api.Group)
		}

		// Only metadata should differ, and only by additions.
		chartMeta := fromChart["metadata"].(map[string]any)
		genMeta := fromGenerator["metadata"].(map[string]any)
		if chartMeta["name"] != genMeta["name"] {
			t.Errorf("name = %v, want %v", chartMeta["name"], genMeta["name"])
		}
	}
}

// The chart's ClusterRole is written by hand, while controller-gen derives the
// authoritative rules from the kubebuilder markers on the reconciler. Those two
// can drift, and drift here means the controller is deployed without a
// permission it needs and fails at runtime rather than at install.
//
// Every rule the generator produces must be granted by the chart. The chart may
// grant more (leader election lives in a namespaced Role of its own), but never
// less.
func TestChartCoversGeneratedRBAC(t *testing.T) {
	var generated renderedRole
	if err := yaml.Unmarshal([]byte(readFile(t, "../../config/rbac/role.yaml")), &generated); err != nil {
		t.Fatalf("parsing the generated ClusterRole: %v", err)
	}
	if len(generated.Rules) == 0 {
		t.Fatal("the generated ClusterRole has no rules; run make generate")
	}

	chart := roles(t, renderChart(t))

	for _, rule := range generated.Rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					if !chartGrants(chart, group, resource, verb) {
						t.Errorf("the chart does not grant %q on %q in API group %q, which controller-gen says the controller needs",
							verb, resource, group)
					}
				}
			}
		}
	}
}

// chartGrants reports whether any rendered Role or ClusterRole allows a verb on
// a resource in an API group.
func chartGrants(rs []renderedRole, group, resource, verb string) bool {
	for _, r := range rs {
		for _, rule := range r.Rules {
			if !contains(rule.APIGroups, group) && !contains(rule.APIGroups, "*") {
				continue
			}
			if !contains(rule.Resources, resource) && !contains(rule.Resources, "*") {
				continue
			}
			if contains(rule.Verbs, verb) || contains(rule.Verbs, "*") {
				return true
			}
		}
	}
	return false
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
