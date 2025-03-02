package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// Config represents the structure of the /etc/k4all configuration file.
type Config struct {
	Version    string             `json:"version"`
	Networking NetworkingConfig   `json:"networking"`
	Features   FeaturesConfig     `json:"features"`
	Cluster    ControlPlaneConfig `json:"cluster"`
}

// NetworkingConfig represents the networking section of the configuration.
type NetworkingConfig struct {
	CNI       CNIConfig       `json:"cni"`
	Firewalld FirewalldConfig `json:"firewalld"`
	Interface InterfaceConfig `json:"iface"`
}

// CNIConfig represents the CNI configuration.
type CNIConfig struct {
	Type string `json:"type"`
}

// FirewalldConfig represents the Firewalld configuration.
type FirewalldConfig struct {
	Enabled string `json:"enabled"`
}

// InterfaceConfig represents the interface configuration.
type InterfaceConfig struct {
	Dev        string `json:"dev"`
	IpConfig   string `json:"ipconfig"`
	IpAddr     string `json:"ipaddr,omitempty"`
	Gateway    string `json:"gateway,omitempty"`
	SubnetMask string `json:"subnet_mask,omitempty"`
	DNS        string `json:"dns,omitempty"`
	DNSSearch  string `json:"dns_search,omitempty"`
}

// FeaturesConfig represents the features section of the configuration.
type FeaturesConfig struct {
	Virt   VirtConfig   `json:"virt"`
	ArgoCD ArgoCDConfig `json:"argocd"`
}

// VirtConfig defines the virtualization configuration
type VirtConfig struct {
	// Enabled indicates if virtualization is enabled
	Enabled string `json:"enabled"`
	// Managed indicates if virtualization is managed by the operator
	Managed string `json:"managed,omitempty"`
	// Version for virtualization components
	Version string `json:"version,omitempty"`
	// Emulation indicates if emulation is enabled (true, false, or auto)
	Emulation string `json:"emulation"`
}

// ArgoCDConfig defines the ArgoCD configuration
type ArgoCDConfig struct {
	// Enabled indicates if ArgoCD is enabled
	Enabled string `json:"enabled"`
	// Managed indicates if ArgoCD is managed by the operator
	Managed string `json:"managed,omitempty"`
	// Version of ArgoCD to install
	Version string `json:"version,omitempty"`
}

// ControlPlaneConfig represents the cluster configuration.
type ControlPlaneConfig struct {
	ApiEndPointUseHostName string   `json:"apiEndPointUseHostName"`
	CustomApiEndPoint      string   `json:"customApiEndPoint,omitempty"`
	HA                     HAConfig `json:"ha"`
}

// HAConfig represents the high availability configuration.
type HAConfig struct {
	Interface                    string `json:"interface"`
	Type                         string `json:"type"`
	ApiControlEndpoint           string `json:"apiControlEndpoint,omitempty"`
	ApiControlEndpointSubnetSize string `json:"apiControlEndpointSubnetSize,omitempty"`
}

// ParseConfigFile reads the k4all config file and parses it.
func ParseConfigFile(filePath string) (*Config, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := json.Unmarshal(content, &config); err != nil {
		return nil, fmt.Errorf("failed to parse JSON config: %w", err)
	}

	return &config, nil
}

// MapToClusterConfig maps the parsed config to a ClusterConfig spec.
func MapToClusterConfig(config *Config) (k4allv1alpha1.ClusterConfigSpec, error) {
	if config == nil {
		return k4allv1alpha1.ClusterConfigSpec{}, fmt.Errorf("config is nil")
	}

	var enabled bool
	if strings.ToLower(config.Features.ArgoCD.Enabled) == "true" {
		enabled = true
	}

	clusterConfigSpec := k4allv1alpha1.ClusterConfigSpec{
		Version: config.Version,
		Networking: k4allv1alpha1.NetworkingConfig{
			CNI: k4allv1alpha1.CNIConfig{
				Type: config.Networking.CNI.Type,
			},
			Firewalld: k4allv1alpha1.FirewalldConfig{
				Enabled: config.Networking.Firewalld.Enabled,
			},
			Interface: k4allv1alpha1.InterfaceConfig{
				Dev:        config.Networking.Interface.Dev,
				IpConfig:   config.Networking.Interface.IpConfig,
				IpAddr:     config.Networking.Interface.IpAddr,
				Gateway:    config.Networking.Interface.Gateway,
				SubnetMask: config.Networking.Interface.SubnetMask,
				DNS:        config.Networking.Interface.DNS,
				DNSSearch:  config.Networking.Interface.DNSSearch,
			},
		},
		Features: k4allv1alpha1.FeaturesConfig{
			Virt: k4allv1alpha1.VirtConfig{
				Enabled:   strings.ToLower(config.Features.Virt.Enabled) == "true",
				Emulation: config.Features.Virt.Emulation,
			},
			ArgoCD: k4allv1alpha1.ArgoCDConfig{
				Enabled: enabled,
			},
		},
		Cluster: k4allv1alpha1.ControlPlaneConfig{
			ApiEndPointUseHostName: config.Cluster.ApiEndPointUseHostName,
			CustomApiEndPoint:      config.Cluster.CustomApiEndPoint,
			HA: k4allv1alpha1.HAConfig{
				Interface:                    config.Cluster.HA.Interface,
				Type:                         config.Cluster.HA.Type,
				ApiControlEndpoint:           config.Cluster.HA.ApiControlEndpoint,
				ApiControlEndpointSubnetSize: config.Cluster.HA.ApiControlEndpointSubnetSize,
			},
		},
	}

	// Set managed fields
	clusterConfigSpec.Features.ArgoCD.Managed = strings.ToLower(config.Features.ArgoCD.Managed) == "true"
	clusterConfigSpec.Features.Virt.Managed = strings.ToLower(config.Features.Virt.Managed) == "true"

	return clusterConfigSpec, nil
}
