package server

import (
	"context"
	"errors"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

var errResourceNameInUse = errors.New("resource name is already used by another Orkano kind")

type namedKind struct {
	name string
	new  func() client.Object
}

var topLevelNameKinds = []namedKind{
	{name: "App", new: func() client.Object { return &orkanov1alpha1.App{} }},
	{name: "Postgres", new: func() client.Object { return &orkanov1alpha1.Postgres{} }},
	{name: "Mongo", new: func() client.Object { return &orkanov1alpha1.Mongo{} }},
}

// conflictingResourceKind checks the top-level Orkano kinds whose controllers
// derive same-named child objects. Kubernetes names are unique only within one
// kind, so the dashboard enforces the product-level namespace before create.
// The caller holds Server.nameMu across this check and the following Create.
func (s *Server) conflictingResourceKind(ctx context.Context, name, requestedKind string) (string, error) {
	key := client.ObjectKey{Namespace: appsNamespace, Name: name}
	for _, candidate := range topLevelNameKinds {
		if candidate.name == requestedKind {
			continue
		}
		if err := s.cfg.K8s.Get(ctx, key, candidate.new()); err == nil {
			return candidate.name, nil
		} else if !apierrors.IsNotFound(err) {
			return "", err
		}
	}
	return "", nil
}

func writeNameInUse(w http.ResponseWriter, existingKind string) {
	writeJSON(w, http.StatusConflict, map[string]string{
		"error":        "name_in_use",
		"existingKind": existingKind,
	})
}
