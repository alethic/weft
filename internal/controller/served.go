package controller

import (
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	weftv1 "github.com/alethic/weft/api/v1"
	"github.com/alethic/weft/api/v1alpha1"
	"github.com/alethic/weft/internal/naming"
)

// Served is one API group's Weave type, and how to move between it and the
// v1 form the reconciler works on.
//
// The two groups share their spec and status types, so the conversion is a
// change of envelope and nothing else. Reconciling everything as v1 is what
// keeps the reconciler single: the only things that differ between groups are
// the object the API server is sent and the keys written onto what a Weave
// manages, and both come from here.
type Served struct {
	// API is the group and every identifier derived from it.
	API naming.API

	// New returns an empty object of this group's type.
	New func() client.Object

	// ToV1 views an object of this group's type as a v1 Weave. The result
	// shares nothing mutable with its argument.
	ToV1 func(client.Object) *weftv1.Weave

	// FromV1 builds an object of this group's type from a v1 Weave, for
	// writing back.
	FromV1 func(*weftv1.Weave) client.Object
}

// ServedV1 is weft.alethic.solutions/v1.
var ServedV1 = Served{
	API: naming.V1,
	New: func() client.Object { return &weftv1.Weave{} },
	ToV1: func(o client.Object) *weftv1.Weave {
		w, ok := o.(*weftv1.Weave)
		if !ok {
			panic(fmt.Sprintf("v1 reconciler handed a %T", o))
		}
		return w.DeepCopy()
	},
	FromV1: func(w *weftv1.Weave) client.Object {
		return w.DeepCopy()
	},
}

// ServedLegacy is weft.run/v1alpha1.
var ServedLegacy = Served{
	API: naming.Legacy,
	New: func() client.Object { return &v1alpha1.Weave{} },
	ToV1: func(o client.Object) *weftv1.Weave {
		legacy, ok := o.(*v1alpha1.Weave)
		if !ok {
			panic(fmt.Sprintf("weft.run reconciler handed a %T", o))
		}
		w := legacy.DeepCopy()
		return &weftv1.Weave{ObjectMeta: w.ObjectMeta, Spec: w.Spec, Status: w.Status}
	},
	FromV1: func(w *weftv1.Weave) client.Object {
		c := w.DeepCopy()
		return &v1alpha1.Weave{ObjectMeta: c.ObjectMeta, Spec: c.Spec, Status: c.Status}
	},
}

// AllServed lists every group the controller reconciles, current first.
var AllServed = []Served{ServedV1, ServedLegacy}
