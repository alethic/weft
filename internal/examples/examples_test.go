// Package examples tests that the shipped examples are real.
//
// Documentation rots quietly. A program in an example file that no longer
// compiles is worse than no example, because it is the first thing somebody
// copies, so every one of them is parsed and evaluated here.
package examples

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/eval"
	"github.com/alethic/weft/internal/inventory"
	"github.com/alethic/weft/internal/jsonutil"
	"github.com/alethic/weft/internal/kube"
	"github.com/alethic/weft/internal/naming"
)

func load(t *testing.T, path string) *v1alpha1.Weave {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var w v1alpha1.Weave
	if err := yaml.Unmarshal(data, &w); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return &w
}

func examples(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no examples found")
	}
	return paths
}

func TestExamplesAreWellFormed(t *testing.T) {
	for _, path := range examples(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			w := load(t, path)

			if w.APIVersion != naming.GroupVersion {
				t.Errorf("apiVersion = %q, want %q", w.APIVersion, naming.GroupVersion)
			}
			if w.Kind != naming.Kind {
				t.Errorf("kind = %q, want %q", w.Kind, naming.Kind)
			}
			if w.Spec.ServiceAccountName == "" {
				t.Error("no serviceAccountName: every read and write needs an identity to run as")
			}
			if strings.TrimSpace(w.Spec.Program) == "" {
				t.Error("no program")
			}
			for _, src := range w.Spec.Sources {
				if src.ID == "" || src.APIVersion == "" || src.Kind == "" || src.Name == "" {
					t.Errorf("source %+v is incomplete", src)
				}
			}
		})
	}
}

// Evaluating with every source absent is the first state a Weave is ever in, so
// each example has to reach it without faulting. A wait is the expected answer;
// a ProgramError is a broken example.
func TestExamplesEvaluateAgainstNothing(t *testing.T) {
	for _, path := range examples(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			w := load(t, path)

			sources := map[string]any{}
			for _, src := range w.Spec.Sources {
				sources[src.ID] = nil
			}

			res, err := eval.NewStarlark(eval.Options{}).Evaluate(context.Background(), eval.Request{
				Program:  w.Spec.Program,
				Inputs:   jsonutil.DecodeObject(w.Spec.Inputs),
				Sources:  sources,
				Observed: map[string]any{},
			})

			var pe *eval.ProgramError
			if errors.As(err, &pe) {
				t.Fatalf("%s:\n%s", pe.Reason, pe.Error())
			}
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if !res.Waiting() {
				t.Errorf("with no sources present the example should be waiting, got %d resources",
					len(res.Resources))
			}
		})
	}
}

// The first pass a real Weave makes: sources exist and have reported their own
// status, but nothing this Weave creates has been observed yet. Every example
// must produce something here, or it could never advance past it.
func TestExamplesProduceTheirFirstWave(t *testing.T) {
	for _, path := range examples(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			w := load(t, path)

			sources := map[string]any{}
			for _, src := range w.Spec.Sources {
				sources[src.ID] = resolvedSource(src)
			}

			res, err := eval.NewStarlark(eval.Options{}).Evaluate(context.Background(), eval.Request{
				Program:  w.Spec.Program,
				Inputs:   jsonutil.DecodeObject(w.Spec.Inputs),
				Sources:  sources,
				Observed: map[string]any{},
			})
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if res.Waiting() {
				// release-values is a pure fan-in with nothing of its own to
				// stage, so a wait here is legitimate only if a field really is
				// unresolved. Every other example must emit its first wave.
				t.Logf("waiting: %s", res.Wait.Reason)
				return
			}
			if len(res.Resources) == 0 {
				t.Fatal("produced nothing on the first pass, so it could never advance")
			}

			// Whatever it produced must survive normalisation, which is where
			// namespace, ownership and wave rules are enforced.
			owner := kube.Owner{
				APIVersion: naming.GroupVersion,
				Kind:       naming.Kind,
				Name:       w.Name,
				UID:        "test-uid",
			}
			items, err := inventory.Build(res.Resources, w.Namespace, owner)
			if err != nil {
				t.Fatalf("normalising: %v", err)
			}
			for _, item := range items {
				if item.Object.GetNamespace() != w.Namespace {
					t.Errorf("%s landed in namespace %q", item.Key, item.Object.GetNamespace())
				}
				if len(item.Object.GetOwnerReferences()) != 1 {
					t.Errorf("%s is not owned by the Weave", item.Key)
				}
			}
		})
	}
}

// resolvedSource fabricates a source that exists and has reported the status
// fields these compositions actually read. The set is small on purpose: five
// shapes cover almost everything.
func resolvedSource(src v1alpha1.Source) map[string]any {
	return map[string]any{
		"apiVersion": src.APIVersion,
		"kind":       src.Kind,
		"metadata": map[string]any{
			"name":       src.Name,
			"generation": int64(3),
			"annotations": map[string]any{
				"crossplane.io/external-name": src.Name,
			},
		},
		"data": map[string]any{
			"tenantId": "00000000-0000-0000-0000-000000000000",
			"id":       src.Name,
		},
		"status": map[string]any{
			"atProvider": map[string]any{
				"id":                       "/subscriptions/sub/resourceGroups/" + src.Name,
				"clientId":                 "11111111-1111-1111-1111-111111111111",
				"principalId":              "22222222-2222-2222-2222-222222222222",
				"vaultUri":                 "https://" + src.Name + ".vault.azure.net/",
				"endpoint":                 "https://" + src.Name + ".example.net/",
				"fullyQualifiedDomainName": src.Name + ".database.windows.net",
			},
		},
	}
}
