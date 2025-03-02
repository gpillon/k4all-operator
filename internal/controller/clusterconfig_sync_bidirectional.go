package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/log"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/config"
)

// SyncK8sToConfigFile reads the ClusterConfig resource and writes it to the config file,
// but only if the resource is not empty and is newer than the file.
func SyncK8sToConfigFile(ctx context.Context, clusterConfig *k4allv1alpha1.ClusterConfig, configFilePath string) error {
	logger := log.FromContext(ctx)

	// Skip if the ClusterConfig is empty (default values)
	if isEmptyK8sConfig(&clusterConfig.Spec) {
		logger.Info("Skipping sync to file because ClusterConfig appears to be empty/default values")
		return nil
	}

	// Check if we have a file and compare timestamps
	fileInfo, err := os.Stat(configFilePath)

	// If file exists, compare modification times
	if err == nil {
		fileModTime := fileInfo.ModTime().Unix()

		// Get the last update time from the k8s resource annotation
		lastUpdateStr, hasAnnotation := clusterConfig.Annotations[LastUpdateAnnotation]
		if hasAnnotation {
			lastUpdate, err := strconv.ParseInt(lastUpdateStr, 10, 64)
			if err == nil && lastUpdate <= fileModTime {
				logger.Info("File is newer than ClusterConfig, skipping write to file",
					"fileModTime", fileModTime, "resourceLastUpdate", lastUpdate)
				return nil
			}
		}
	} else if !os.IsNotExist(err) {
		// If error is not "file doesn't exist", return it
		return fmt.Errorf("failed to check config file: %w", err)
	}

	logger.Info("Converting ClusterConfig to config file format", "name", clusterConfig.Name)

	// Convert the ClusterConfig to internal config format
	cfg := config.ConvertToConfig(&clusterConfig.Spec)

	// Validate the config is not empty
	if isEmptyConfigStruct(cfg) {
		logger.Info("Refusing to write empty config to file, this would destroy existing configuration")
		return nil
	}

	// Check if we have write permissions to the config file
	if err == nil && fileInfo.Mode().Perm()&0200 == 0 {
		logger.Info("Config file exists but is not writable, attempting to make it writable",
			"path", configFilePath)
		err = os.Chmod(configFilePath, fileInfo.Mode().Perm()|0200)
		if err != nil {
			return fmt.Errorf("failed to make config file writable: %w", err)
		}
	}

	// Create directory if it doesn't exist
	if os.IsNotExist(err) {
		dirPath := "/etc"
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			return fmt.Errorf("failed to create directory for config file: %w", err)
		}
	}

	// Write the config to file
	logger.Info("Writing ClusterConfig to file", "path", configFilePath)
	if err := config.WriteConfigFile(configFilePath, cfg); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	logger.Info("Successfully updated config file", "path", configFilePath)
	return nil
}

// isEmptyK8sConfig checks if the ClusterConfigSpec has default/empty values
func isEmptyK8sConfig(spec *k4allv1alpha1.ClusterConfigSpec) bool {
	if spec.Version == "" ||
		spec.Networking.CNI.Type == "" ||
		spec.Networking.Interface.Dev == "" {
		return true
	}
	return false
}

// isEmptyConfigStruct checks if the Config struct has default/empty values
func isEmptyConfigStruct(cfg *config.Config) bool {
	// Check required fields
	if cfg.Version == "" ||
		cfg.Networking.CNI.Type == "" ||
		cfg.Networking.Interface.Dev == "" {
		return true
	}

	// Additional validation: serialize and check length
	data, err := json.Marshal(cfg)
	if err != nil || len(data) < 50 { // Arbitrary small size check
		return true
	}

	return false
}
