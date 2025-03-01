package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/gpillon/k4all-operator/internal/config"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

const (
	FileSyncAnnotation   = "k4all.magesgate.com/file-sync"
	LastUpdateAnnotation = "k4all.magesgate.com/last-update"
)

// ClusterConfigReconciler reconciles a ClusterConfig object
type ClusterConfigReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	ConfigFilePath string
	ToolsImage     string

	// New fields for feature management
	configSyncer *K4allConfigSyncer
}

// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=clusterconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=clusterconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=clusterconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups=*,resources=*,verbs=*
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ClusterConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Get the ClusterConfig resource
	clusterConfig := &k4allv1alpha1.ClusterConfig{}
	err := r.Get(ctx, req.NamespacedName, clusterConfig)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Reconciling ClusterConfig", "name", clusterConfig.Name)

	// Check if the namespace exists, create if not
	ns := &corev1.Namespace{}
	err = r.Get(ctx, client.ObjectKey{Name: "k4all-operator-system"}, ns)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Namespace k4all-operator-system not found, creating it.")
			ns.Name = "k4all-operator-system"
			if err := r.Create(ctx, ns); err != nil {
				return ctrl.Result{}, fmt.Errorf("unable to create namespace k4all-operator-system: %w", err)
			}
		} else {
			return ctrl.Result{}, fmt.Errorf("unable to check if namespace k4all-operator-system exists: %w", err)
		}
	}

	// Check if we need to sync from file to k8s resource
	syncFromFile := true
	if val, ok := clusterConfig.Annotations[FileSyncAnnotation]; ok && val == "false" {
		syncFromFile = false
	}

	if syncFromFile {
		logger.Info("Synchronizing from config file to Kubernetes resource")
		// Use our config package to read and update
		if err := config.ReadConfigAndUpdateK8s(r.ConfigFilePath, clusterConfig); err != nil {
			logger.Error(err, "Failed to read config file or update k8s resource")
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}

		// Update annotation to prevent infinite sync
		if clusterConfig.Annotations == nil {
			clusterConfig.Annotations = make(map[string]string)
		}
		clusterConfig.Annotations[FileSyncAnnotation] = "false"
		clusterConfig.Annotations[LastUpdateAnnotation] = strconv.FormatInt(time.Now().Unix(), 10)

		if err := r.Update(ctx, clusterConfig); err != nil {
			logger.Error(err, "Failed to update ClusterConfig annotation")
			return ctrl.Result{}, err
		}
	} else {
		// We need to sync from k8s resource to file
		logger.Info("Synchronizing from Kubernetes resource to config file")

		// Use our config package to write
		if err := config.WriteClusterConfigToFile(clusterConfig, r.ConfigFilePath); err != nil {
			logger.Error(err, "Failed to write config file")
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}
	}

	// NEW: Ensure config is synced to ConfigMap for feature controllers
	if r.configSyncer != nil {
		if err := r.configSyncer.SyncConfig(ctx); err != nil {
			logger.Error(err, "Failed to sync config to ConfigMap")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.ConfigFilePath = "/host/etc/k4all-config.json" // Using /host prefix to access host filesystem
	if r.ToolsImage == "" {
		r.ToolsImage = "ghcr.io/gpillon/k4all-tools:latest" // Default if not set
	}

	// Just initialize the configSyncer without starting it
	if r.configSyncer == nil {
		r.configSyncer = &K4allConfigSyncer{
			Client:         mgr.GetClient(),
			ConfigPath:     r.ConfigFilePath,
			ConfigMapName:  "k4all-config",
			ConfigMapNs:    "k4all-operator-system",
			ResyncInterval: 5 * time.Minute,
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&k4allv1alpha1.ClusterConfig{}).
		Complete(r)
}
