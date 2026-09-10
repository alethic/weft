// Package jsonutil decodes the free-form parts of a Weave spec.
package jsonutil

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/json"
)

// DecodeObject turns spec.variables into plain data for a program.
//
// This is apimachinery's decoder rather than encoding/json because that one
// turns every JSON number into a float64. A replica count that reaches a
// program as 3.0 and comes back out as 3.0 is a different resource body than
// the one the API server holds, which churns the object on every single apply.
//
// A malformed value yields an empty map rather than an error: the CRD schema
// already guarantees this is an object, so a failure here would mean the API
// server accepted something it should not have, and a composition that sees no
// variables reports that far more legibly than a decode error would.
func DecodeObject(raw *apiextensionsv1.JSON) map[string]any {
	if raw == nil || len(raw.Raw) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw.Raw, &out); err != nil {
		return map[string]any{}
	}
	if out == nil {
		return map[string]any{}
	}
	return out
}
