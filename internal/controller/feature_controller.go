package controller

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gpillon/k4all-operator/internal/controller/config"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// K4allConfigSyncer syncs /etc/k4all-config.json to a ConfigMap
type K4allConfigSyncer struct {
	Client         client.Client
	ConfigPath     string // Path to /etc/k4all-config.json
	ConfigMapName  string // Name of the config map
	ConfigMapNs    string // Namespace of the config map
	ResyncInterval time.Duration
}

// K4allConfig represents the structure of /etc/k4all-config.json
type K4allConfig struct {
	Features map[string]FeatureConfig `json:"features"`
	// Add other config sections as needed
}

// FeatureConfig contains configuration for a specific feature
type FeatureConfig struct {
	Enabled bool   `json:"enabled"`
	Managed bool   `json:"managed"`
	Version string `json:"version,omitempty"`
	// Other feature-specific config
}

// SyncConfig reads the config file and syncs it to the ConfigMap
func (s *K4allConfigSyncer) SyncConfig(ctx context.Context) error {
	log := log.FromContext(ctx)

	// Read the config file using our parser
	fileConfig, err := config.ParseConfigFile(s.ConfigPath)
	if err != nil {
		log.Error(err, "Failed to read config file")
		return err
	}

	// Convert to JSON
	data, err := json.MarshalIndent(fileConfig, "", "  ")
	if err != nil {
		log.Error(err, "Failed to marshal config to JSON")
		return err
	}

	// Get or create the ConfigMap
	cm := &corev1.ConfigMap{}
	err = s.Client.Get(ctx, types.NamespacedName{
		Name:      s.ConfigMapName,
		Namespace: s.ConfigMapNs,
	}, cm)

	if errors.IsNotFound(err) {
		// Create new ConfigMap
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      s.ConfigMapName,
				Namespace: s.ConfigMapNs,
			},
			Data: map[string]string{
				"config.json": string(data),
			},
		}
		if err := s.Client.Create(ctx, cm); err != nil {
			log.Error(err, "Failed to create ConfigMap")
			return err
		}
		log.Info("Created ConfigMap with k4all config", "configMap", s.ConfigMapName)
		return nil
	} else if err != nil {
		log.Error(err, "Failed to get ConfigMap")
		return err
	}

	// Initialize Data map if nil
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}

	// Update the ConfigMap if changed
	if cm.Data["config.json"] != string(data) {
		cm.Data["config.json"] = string(data)
		if err := s.Client.Update(ctx, cm); err != nil {
			log.Error(err, "Failed to update ConfigMap")
			return err
		}
		log.Info("Updated ConfigMap with k4all config", "configMap", s.ConfigMapName)
	}

	return nil
}

// Start implements the Runnable interface
func (s *K4allConfigSyncer) Start(ctx context.Context) error {
	log := log.FromContext(ctx)
	log.Info("Starting k4all config syncer", "interval", s.ResyncInterval)

	// Do an initial sync
	if err := s.SyncConfig(ctx); err != nil {
		log.Error(err, "Initial config sync failed")
		// Don't return error here to not prevent startup
	}

	// Start periodic sync
	ticker := time.NewTicker(s.ResyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("Stopping k4all config syncer")
			return nil
		case <-ticker.C:
			if err := s.SyncConfig(ctx); err != nil {
				log.Error(err, "Config sync failed")
				// Continue running despite errors
			}
		}
	}
}

// NeedLeaderElection implements the LeaderElectionRunnable interface
func (s *K4allConfigSyncer) NeedLeaderElection() bool {
	return true // Only run on the leader
}
