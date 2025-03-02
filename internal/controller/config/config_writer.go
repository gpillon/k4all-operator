package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// ConvertToConfig converts a ClusterConfigSpec to the internal Config format
func ConvertToConfig(clusterConfigSpec *k4allv1alpha1.ClusterConfigSpec) *Config {
	config := &Config{
		Version: clusterConfigSpec.Version,
		Networking: NetworkingConfig{
			CNI: CNIConfig{
				Type: clusterConfigSpec.Networking.CNI.Type,
			},
			Firewalld: FirewalldConfig{
				Enabled: clusterConfigSpec.Networking.Firewalld.Enabled,
			},
			Interface: InterfaceConfig{
				Dev:        clusterConfigSpec.Networking.Interface.Dev,
				IpConfig:   clusterConfigSpec.Networking.Interface.IpConfig,
				IpAddr:     clusterConfigSpec.Networking.Interface.IpAddr,
				Gateway:    clusterConfigSpec.Networking.Interface.Gateway,
				SubnetMask: clusterConfigSpec.Networking.Interface.SubnetMask,
				DNS:        clusterConfigSpec.Networking.Interface.DNS,
				DNSSearch:  clusterConfigSpec.Networking.Interface.DNSSearch,
			},
		},
		Features: FeaturesConfig{
			Virt: VirtConfig{
				Enabled:   fmt.Sprintf("%t", clusterConfigSpec.Features.Virt.Enabled),
				Managed:   fmt.Sprintf("%t", clusterConfigSpec.Features.Virt.Managed),
				Emulation: clusterConfigSpec.Features.Virt.Emulation,
			},
			ArgoCD: ArgoCDConfig{
				Enabled: fmt.Sprintf("%t", clusterConfigSpec.Features.ArgoCD.Enabled),
				Managed: fmt.Sprintf("%t", clusterConfigSpec.Features.ArgoCD.Managed),
			},
		},
		Cluster: ControlPlaneConfig{
			ApiEndPointUseHostName: clusterConfigSpec.Cluster.ApiEndPointUseHostName,
			CustomApiEndPoint:      clusterConfigSpec.Cluster.CustomApiEndPoint,
			HA: HAConfig{
				Interface:                    clusterConfigSpec.Cluster.HA.Interface,
				Type:                         clusterConfigSpec.Cluster.HA.Type,
				ApiControlEndpoint:           clusterConfigSpec.Cluster.HA.ApiControlEndpoint,
				ApiControlEndpointSubnetSize: clusterConfigSpec.Cluster.HA.ApiControlEndpointSubnetSize,
			},
		},
	}
	return config
}

// WriteConfigFile writes the configuration to a file.
func WriteConfigFile(filePath string, config *Config) error {
	// Marshal the Config to JSON with indentation for readability
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("error marshalling config: %w", err)
	}

	// Write to the file
	err = os.WriteFile(filePath, data, 0644)
	if err != nil {
		return fmt.Errorf("error writing config file: %w", err)
	}

	return nil
}

// WriteClusterConfigToFile writes the cluster config to the specified file
func WriteClusterConfigToFile(clusterConfig *k4allv1alpha1.ClusterConfig, filePath string) error {
	// Map from ClusterConfig to our Config structure
	config := mapFromClusterConfig(clusterConfig.Spec)

	// Marshal to JSON with pretty formatting
	jsonData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config to JSON: %w", err)
	}

	// Ensure directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory for config file: %w", err)
	}

	// Write to temporary file first
	tempFile := filePath + ".tmp"
	if err := os.WriteFile(tempFile, jsonData, 0644); err != nil {
		return fmt.Errorf("failed to write to temporary config file: %w", err)
	}

	// Atomically replace the original file
	if err := os.Rename(tempFile, filePath); err != nil {
		return fmt.Errorf("failed to replace config file: %w", err)
	}

	return nil
}

// mapFromClusterConfig maps from ClusterConfig to our Config structure
func mapFromClusterConfig(spec k4allv1alpha1.ClusterConfigSpec) *Config {

	argocdEnabled := "false"
	if spec.Features.ArgoCD.Enabled {
		argocdEnabled = "true"
	}

	argocdManaged := "false"
	if spec.Features.ArgoCD.Managed {
		argocdManaged = "true"
	}

	virtEnabled := "false"
	if spec.Features.Virt.Enabled {
		virtEnabled = "true"
	}

	virtManaged := "false"
	if spec.Features.Virt.Managed {
		virtManaged = "true"
	}

	return &Config{
		Version: spec.Version,
		Networking: NetworkingConfig{
			CNI: CNIConfig{
				Type: spec.Networking.CNI.Type,
			},
			Firewalld: FirewalldConfig{
				Enabled: spec.Networking.Firewalld.Enabled,
			},
			Interface: InterfaceConfig{
				Dev:        spec.Networking.Interface.Dev,
				IpConfig:   spec.Networking.Interface.IpConfig,
				IpAddr:     spec.Networking.Interface.IpAddr,
				Gateway:    spec.Networking.Interface.Gateway,
				SubnetMask: spec.Networking.Interface.SubnetMask,
				DNS:        spec.Networking.Interface.DNS,
				DNSSearch:  spec.Networking.Interface.DNSSearch,
			},
		},
		Features: FeaturesConfig{
			Virt: VirtConfig{
				Enabled:   virtEnabled,
				Managed:   virtManaged,
				Emulation: spec.Features.Virt.Emulation,
			},
			ArgoCD: ArgoCDConfig{
				Enabled: argocdEnabled,
				Managed: argocdManaged,
			},
		},
		Cluster: ControlPlaneConfig{
			ApiEndPointUseHostName: spec.Cluster.ApiEndPointUseHostName,
			CustomApiEndPoint:      spec.Cluster.CustomApiEndPoint,
			HA: HAConfig{
				Interface:                    spec.Cluster.HA.Interface,
				Type:                         spec.Cluster.HA.Type,
				ApiControlEndpoint:           spec.Cluster.HA.ApiControlEndpoint,
				ApiControlEndpointSubnetSize: spec.Cluster.HA.ApiControlEndpointSubnetSize,
			},
		},
	}
}

// ReadConfigAndUpdateK8s reads the config file and updates the K8s resource
func ReadConfigAndUpdateK8s(configFilePath string, k8sConfig *k4allv1alpha1.ClusterConfig) error {
	config, err := ParseConfigFile(configFilePath)
	if err != nil {
		return err
	}

	spec, err := MapToClusterConfig(config)
	if err != nil {
		return err
	}

	// Update spec with our new values
	k8sConfig.Spec = spec
	return nil
}

// MergeAndWriteConfig merges K8s config with existing file config and writes back
func MergeAndWriteConfig(configFilePath string, k8sConfig *k4allv1alpha1.ClusterConfig) error {
	// Try to read existing config first
	existingConfig, err := ParseConfigFile(configFilePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// If file doesn't exist, create from scratch using K8s config
	if os.IsNotExist(err) {
		return WriteClusterConfigToFile(k8sConfig, configFilePath)
	}

	// Merge configs giving preference to K8s values
	mergedConfig := mergeConfigs(existingConfig, k8sConfig.Spec)

	// Create new k8s config with merged values
	mergedK8sConfig := k8sConfig.DeepCopy()
	mergedK8sConfig.Spec, err = MapToClusterConfig(mergedConfig)
	if err != nil {
		return err
	}

	// Write back to file
	return WriteClusterConfigToFile(mergedK8sConfig, configFilePath)
}

// mergeConfigs merges configs giving preference to K8s values
func mergeConfigs(fileConfig *Config, k8sSpec k4allv1alpha1.ClusterConfigSpec) *Config {
	// Start with file config
	merged := fileConfig

	// Update with K8s values
	merged.Version = k8sSpec.Version
	merged.Networking.CNI.Type = k8sSpec.Networking.CNI.Type
	merged.Networking.Firewalld.Enabled = k8sSpec.Networking.Firewalld.Enabled
	merged.Networking.Interface.Dev = k8sSpec.Networking.Interface.Dev
	merged.Networking.Interface.IpConfig = k8sSpec.Networking.Interface.IpConfig
	merged.Networking.Interface.IpAddr = k8sSpec.Networking.Interface.IpAddr
	merged.Networking.Interface.Gateway = k8sSpec.Networking.Interface.Gateway
	merged.Networking.Interface.SubnetMask = k8sSpec.Networking.Interface.SubnetMask
	merged.Networking.Interface.DNS = k8sSpec.Networking.Interface.DNS
	merged.Networking.Interface.DNSSearch = k8sSpec.Networking.Interface.DNSSearch

	// Update feature config
	merged.Features.Virt.Enabled = strconv.FormatBool(k8sSpec.Features.Virt.Enabled)
	merged.Features.Virt.Managed = strconv.FormatBool(k8sSpec.Features.Virt.Managed)
	merged.Features.Virt.Emulation = k8sSpec.Features.Virt.Emulation

	merged.Features.ArgoCD.Enabled = strconv.FormatBool(k8sSpec.Features.ArgoCD.Enabled)
	merged.Features.ArgoCD.Managed = strconv.FormatBool(k8sSpec.Features.ArgoCD.Managed)

	merged.Cluster.ApiEndPointUseHostName = k8sSpec.Cluster.ApiEndPointUseHostName
	merged.Cluster.CustomApiEndPoint = k8sSpec.Cluster.CustomApiEndPoint
	merged.Cluster.HA.Interface = k8sSpec.Cluster.HA.Interface
	merged.Cluster.HA.Type = k8sSpec.Cluster.HA.Type
	merged.Cluster.HA.ApiControlEndpoint = k8sSpec.Cluster.HA.ApiControlEndpoint
	merged.Cluster.HA.ApiControlEndpointSubnetSize = k8sSpec.Cluster.HA.ApiControlEndpointSubnetSize

	return merged
}
