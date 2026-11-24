package controller

import (
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gpillon/k4all-operator/internal/engine"
)

// NewClient is the client of the operator's manager (ctrl.Options.NewClient):
// controller-runtime's, its creates, updates and patches attributed to the
// field manager k4all-operator (engine.FieldManager), as its server-side
// applies are (spec R3, §16.2). A write that names another field owner
// keeps it: the create-only objects, k4all-operator-seed.
func NewClient(config *rest.Config, options client.Options) (client.Client, error) {
	c, err := client.New(config, options)
	if err != nil {
		return nil, err
	}
	return client.WithFieldOwner(c, engine.FieldManager), nil
}
