package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// FeatureManager handles the setup and management of K4all features
type FeatureManager struct {
	Client     client.Client
	ToolsImage string
}

// NewFeatureManager creates a new FeatureManager
func NewFeatureManager(c client.Client, toolsImage string) *FeatureManager {
	return &FeatureManager{
		Client:     c,
		ToolsImage: toolsImage,
	}
}

// SyncFeatures processes all features defined in the ClusterConfig
func (fm *FeatureManager) SyncFeatures(ctx context.Context, clusterConfig *k4allv1alpha1.ClusterConfig) error {
	logger := log.FromContext(ctx)
	logger.Info("Syncing features (using ConfigMap-based feature controllers)")

	// The actual feature reconciliation is now handled by the ArgocdReconciler
	// which watches the k4all-config ConfigMap for changes - no action needed here

	return nil
}
