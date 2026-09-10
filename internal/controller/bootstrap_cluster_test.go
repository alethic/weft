//go:build windows

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/yaml"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/naming"
)

// TestMain runs the controller tests against the ambient kubecontext.
//
// envtest cannot be compiled on Windows against controller-runtime v0.25.0:
// pkg/internal/testing/process declares signalProcess twice, once
// unconditionally and once in signal_windows.go. Rather than leave the
// reconciler untested on this platform, the same tests run against whatever
// cluster kubectl is pointed at.
//
// This installs the CRD and creates namespaces, so it only runs when asked:
// set WEFT_TEST_CLUSTER=1. It removes everything it created on the way out.
func TestMain(m *testing.M) {
	if os.Getenv("WEFT_TEST_CLUSTER") == "" {
		fmt.Fprintln(os.Stderr,
			"controller tests need a control plane. envtest does not build on Windows with "+
				"controller-runtime v0.25.0, so set WEFT_TEST_CLUSTER=1 to run them against the "+
				"current kubecontext instead. Skipping.")
		os.Exit(0)
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

	var err error
	testCfg, err = ctrl.GetConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no kubeconfig: %v\n", err)
		os.Exit(1)
	}
	testK8s, err = client.New(testCfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}

	installed, err := installCRD(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "installing the CRD: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	// Only remove the CRD if this run installed it. Deleting one that was
	// already there would take every Weave in the cluster with it.
	if installed {
		removeCRD(context.Background())
	}
	os.Exit(code)
}

// installCRD applies the generated CRD, reporting whether it had to create it.
func installCRD(ctx context.Context) (bool, error) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", naming.Group+"_"+naming.Plural+".yaml"))
	if err != nil {
		return false, err
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		return false, err
	}

	var existing apiextensionsv1.CustomResourceDefinition
	switch err := testK8s.Get(ctx, types.NamespacedName{Name: crd.Name}, &existing); {
	case err == nil:
		return false, nil
	case !apierrors.IsNotFound(err):
		return false, err
	}

	if err := testK8s.Create(ctx, &crd); err != nil {
		return false, err
	}

	// The API server needs a moment before the new type is servable.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var list v1alpha1.WeaveList
		if err := testK8s.List(ctx, &list); err == nil {
			return true, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return true, fmt.Errorf("the CRD did not become servable in time")
}

func removeCRD(ctx context.Context) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	crd.Name = naming.CRDName
	if err := testK8s.Delete(ctx, crd); err != nil && !apierrors.IsNotFound(err) {
		fmt.Fprintf(os.Stderr, "removing the CRD: %v\n", err)
	}
}
