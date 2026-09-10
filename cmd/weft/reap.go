package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/kube"
	"github.com/alethic/weft/internal/naming"
)

// runReap removes every finalizer Weft has placed anywhere.
//
// Uninstalling a controller that has placed finalizers on objects it does not
// own is how a namespace ends up undeletable, so reaping is part of the
// product rather than a runbook step. The set of objects to visit is exactly
// known: Weft only ever finalizes a source a Weave declared, so walking the
// Weaves finds all of them.
func runReap(args []string) error {
	fs := flag.NewFlagSet("weft reap", flag.ExitOnError)
	namespace := fs.String("namespace", "", "Restrict to a single namespace. Empty covers all of them.")
	dryRun := fs.Bool("dry-run", false, "Report what would be released without changing anything.")
	weaveFinalizers := fs.Bool("include-weaves", true,
		"Also release the finalizer on the Weave objects themselves. Their resources are then collected by "+
			"ordinary cascading garbage collection, in no particular order.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := ctrl.SetupSignalHandler()
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}

	c, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("building client: %w", err)
	}

	mapper, err := newMapper(restCfg)
	if err != nil {
		return err
	}
	factory, err := kube.NewFactory(restCfg, mapper, kube.FactoryOptions{
		UserAgent: naming.Group + "/reap",
	})
	if err != nil {
		return err
	}

	var list v1alpha1.WeaveList
	var opts []client.ListOption
	if *namespace != "" {
		opts = append(opts, client.InNamespace(*namespace))
	}
	if err := c.List(ctx, &list, opts...); err != nil {
		return fmt.Errorf("listing Weaves: %w", err)
	}

	released := 0
	for i := range list.Items {
		w := &list.Items[i]
		n, err := reapWeave(ctx, c, factory, w, *dryRun, *weaveFinalizers)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s/%s: %v\n", w.Namespace, w.Name, err)
			continue
		}
		released += n
	}

	verb := "released"
	if *dryRun {
		verb = "would release"
	}
	fmt.Printf("%s %d finalizers across %d Weaves\n", verb, released, len(list.Items))
	fmt.Printf("\nAnything Weft missed carries a finalizer starting %q and can be found with:\n"+
		"  kubectl get <kind> -A -o json | jq -r '.items[] | select(.metadata.finalizers[]? | startswith(\"%s\")) | \"\\(.metadata.namespace)/\\(.metadata.name)\"'\n",
		naming.SourceFinalizerPrefix, naming.SourceFinalizerPrefix)
	return nil
}

func reapWeave(ctx context.Context, c client.Client, factory *kube.Factory, w *v1alpha1.Weave, dryRun, includeWeave bool) (int, error) {
	released := 0
	want := naming.SourceFinalizer(w.Namespace, w.Name)

	var impersonated *kube.Client
	for _, src := range w.Spec.Sources {
		if !src.Finalize {
			continue
		}
		gv, err := schema.ParseGroupVersion(src.APIVersion)
		if err != nil {
			continue
		}
		gvk := gv.WithKind(src.Kind)

		if dryRun {
			fmt.Printf("  %s/%s: would release %s from %s %q\n", w.Namespace, w.Name, want, gvk.Kind, src.Name)
			released++
			continue
		}

		if impersonated == nil {
			impersonated, err = factory.For(w.Namespace, w.Spec.ServiceAccountName)
			if err != nil {
				return released, err
			}
		}
		changed, err := impersonated.MutateFinalizers(ctx, gvk, src.Name, func(current []string) ([]string, bool) {
			out := make([]string, 0, len(current))
			for _, f := range current {
				if f != want {
					out = append(out, f)
				}
			}
			return out, len(out) != len(current)
		})
		switch {
		case err == nil:
		case apierrors.IsNotFound(err):
			continue
		default:
			fmt.Fprintf(os.Stderr, "  %s/%s: releasing from %s %q: %v\n", w.Namespace, w.Name, gvk.Kind, src.Name, err)
			continue
		}
		if changed {
			fmt.Printf("  %s/%s: released %s from %s %q\n", w.Namespace, w.Name, want, gvk.Kind, src.Name)
			released++
		}
	}

	if includeWeave && hasFinalizer(w.Finalizers, naming.WeaveFinalizer) {
		if dryRun {
			fmt.Printf("  %s/%s: would release %s from the Weave itself\n", w.Namespace, w.Name, naming.WeaveFinalizer)
			return released + 1, nil
		}
		w.Finalizers = withoutFinalizer(w.Finalizers, naming.WeaveFinalizer)
		if err := c.Update(ctx, w); err != nil && !apierrors.IsNotFound(err) {
			return released, fmt.Errorf("releasing the Weave finalizer: %w", err)
		}
		fmt.Printf("  %s/%s: released %s from the Weave itself\n", w.Namespace, w.Name, naming.WeaveFinalizer)
		released++
	}

	return released, nil
}

func hasFinalizer(list []string, want string) bool {
	for _, f := range list {
		if f == want {
			return true
		}
	}
	return false
}

func withoutFinalizer(list []string, want string) []string {
	out := make([]string, 0, len(list))
	for _, f := range list {
		if f != want {
			out = append(out, f)
		}
	}
	return out
}
