package controller

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/config"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SyncClusterConfig reads the config file and synchronizes the ClusterConfig.
func SyncClusterConfig(ctx context.Context, c client.Client, configFilePath string, toolsImage string) error {
	logger := log.FromContext(ctx)

	// Check if the config file exists
	fileInfo, err := os.Stat(configFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Info("Config file does not exist, skipping file->k8s sync", "path", configFilePath)
			return nil
		}
		return fmt.Errorf("error checking config file: %w", err)
	}

	// If file exists but is empty, don't try to parse it
	if fileInfo.Size() == 0 {
		logger.Info("Config file exists but is empty, skipping file->k8s sync", "path", configFilePath)
		return nil
	}

	// Get file modification time
	fileModTime := fileInfo.ModTime().Unix()

	// Read and parse the config file
	clusterConfigSpec, err := config.ParseConfigFile(configFilePath)
	if err != nil {
		return fmt.Errorf("unable to parse configuration file: %w", err)
	}

	// Skip if the parsed config appears empty
	if isEmptyConfigFile(clusterConfigSpec) {
		logger.Info("Parsed config appears empty/invalid, skipping file->k8s sync", "path", configFilePath)
		return nil
	}

	logger.Info("ClusterConfigSpec parsed from file", "clusterConfigSpec", clusterConfigSpec)

	// Get the current timestamp for update tracking
	nowTimestamp := strconv.FormatInt(time.Now().Unix(), 10)

	// Convert config.Config to v1alpha1.ClusterConfigSpec
	clusterConfig := &k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "k4all-cluster-config",
			Namespace: "k4all-operator-system",
			Annotations: map[string]string{
				FileSyncAnnotation:   "true",
				LastUpdateAnnotation: nowTimestamp,
			},
		},
		Spec: *config.ConvertToClusterConfigSpec(clusterConfigSpec),
	}

	// Update existing ClusterConfig
	existingConfig := &k4allv1alpha1.ClusterConfig{}
	err = c.Get(ctx, client.ObjectKey{Name: clusterConfig.Name, Namespace: clusterConfig.Namespace}, existingConfig)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("ClusterConfig resource not found. Creating it...")
			// Create the resource
			if err := c.Create(ctx, clusterConfig); err != nil {
				return fmt.Errorf("failed to create ClusterConfig: %w", err)
			}
			logger.Info("Created ClusterConfig resource", "name", clusterConfig.Name)
			return nil
		}
		logger.Error(err, "failed to get ClusterConfig")
		return fmt.Errorf("failed to get ClusterConfig: %w", err)
	}

	// Check if the k8s resource is newer than the file
	if existingConfig.Annotations != nil {
		lastUpdateStr, hasTimestamp := existingConfig.Annotations[LastUpdateAnnotation]
		if hasTimestamp {
			lastUpdate, err := strconv.ParseInt(lastUpdateStr, 10, 64)
			if err == nil && lastUpdate > fileModTime {
				logger.Info("K8s resource is newer than file, skipping file->k8s sync",
					"resourceTime", lastUpdate, "fileTime", fileModTime)
				return nil
			}
		}
	}

	// Update the existing config
	logger.Info("Updating ClusterConfig resource from file...", "name", clusterConfig.Name)
	existingConfig.Spec = clusterConfig.Spec

	// Make sure annotations exist and set the tracking annotations
	if existingConfig.Annotations == nil {
		existingConfig.Annotations = make(map[string]string)
	}
	existingConfig.Annotations[FileSyncAnnotation] = "true"
	existingConfig.Annotations[LastUpdateAnnotation] = nowTimestamp

	if err := c.Update(ctx, existingConfig); err != nil {
		return fmt.Errorf("failed to update ClusterConfig: %w", err)
	}

	// Sync features using the feature manager
	featureManager := NewFeatureManager(c, toolsImage)
	if err := featureManager.SyncFeatures(ctx, clusterConfig); err != nil {
		return fmt.Errorf("failed to sync features: %w", err)
	}

	return nil
}

func isEmptyConfigFile(config *config.Config) bool {
	return config == nil || config.Version == ""
}
