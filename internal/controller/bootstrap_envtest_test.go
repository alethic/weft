//go:build !windows

package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/alethic/weft/api/v1alpha1"
)

// TestMain runs the controller tests against a hermetic control plane.
//
// This is the preferred bootstrap: a real API server with real admission,
// finalizers, garbage collection and optimistic concurrency, started and thrown
// away per run, with nothing to clean up and nothing shared.
func TestMain(m *testing.M) {
	// setup-envtest puts the control plane binaries here. Without them there is
	// nothing to test against, and skipping beats failing on a machine that has
	// not run "make envtest".
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		matches, _ := filepath.Glob(filepath.Join("..", "..", "bin", "envtest", "k8s", "*"))
		if len(matches) == 0 {
			fmt.Fprintln(os.Stderr, "envtest binaries not found; run 'make envtest'. Skipping controller tests.")
			os.Exit(0)
		}
		abs, err := filepath.Abs(matches[0])
		if err != nil {
			panic(err)
		}
		os.Setenv("KUBEBUILDER_ASSETS", abs)
	}

	logf.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		v1alpha1.AddToScheme,
		apiextensionsv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			panic(err)
		}
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
		Scheme:                scheme,
	}

	var err error
	testCfg, err = env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting envtest: %v\n", err)
		os.Exit(1)
	}

	testK8s, err = client.New(testCfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}

	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}
