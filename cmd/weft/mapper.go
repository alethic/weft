package main

import (
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// newMapper builds a RESTMapper for the one-shot commands, which have no
// manager to borrow one from.
func newMapper(cfg *rest.Config) (meta.RESTMapper, error) {
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("building HTTP client: %w", err)
	}
	return newRESTMapper(cfg, httpClient)
}

func newRESTMapper(cfg *rest.Config, httpClient *http.Client) (meta.RESTMapper, error) {
	m, err := apiutil.NewDynamicRESTMapper(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("building REST mapper: %w", err)
	}
	return m, nil
}
