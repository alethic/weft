package controller

import "k8s.io/apimachinery/pkg/util/json"

// jsonUnmarshal decodes spec.inputs.
//
// This is apimachinery's decoder rather than encoding/json because that one
// turns every JSON number into a float64. A replica count that reaches a
// program as 3.0 and comes back out as 3.0 is a different resource body than
// the one the API server holds, which churns the object on every single apply.
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
