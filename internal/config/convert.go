package config

import k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"

// ConvertToClusterConfigSpec converts Config to ClusterConfigSpec
func ConvertToClusterConfigSpec(cfg *Config) *k4allv1alpha1.ClusterConfigSpec {
	if cfg == nil {
		return nil
	}
	return &k4allv1alpha1.ClusterConfigSpec{
		Version:    cfg.Version,
		Features:   ConvertToV1alpha1FeaturesConfig(cfg.Features),
		Networking: ConvertToV1alpha1NetworkingConfig(cfg.Networking),
	}
}

// ConvertToV1alpha1FeaturesConfig converts config.FeaturesConfig to v1alpha1.FeaturesConfig
func ConvertToV1alpha1FeaturesConfig(features FeaturesConfig) k4allv1alpha1.FeaturesConfig {
	return k4allv1alpha1.FeaturesConfig{
		// Map all fields from features to v1alpha1.FeaturesConfig
		// For example:
		// EnableFeatureA: features.EnableFeatureA,
		// EnableFeatureB: features.EnableFeatureB,
		// ... and so on for all fields
	}
}

// ConvertToV1alpha1NetworkingConfig converts config.NetworkingConfig to v1alpha1.NetworkingConfig
func ConvertToV1alpha1NetworkingConfig(networking NetworkingConfig) k4allv1alpha1.NetworkingConfig {
	return k4allv1alpha1.NetworkingConfig{
		// Map all fields from networking to v1alpha1.NetworkingConfig
		// For example:
		// NetworkMode: networking.NetworkMode,
		// ExternalIP: networking.ExternalIP,
		// ... and so on for all fields
	}
}
