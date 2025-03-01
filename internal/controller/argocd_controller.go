package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gpillon/k4all-operator/internal/config"
	"github.com/gpillon/k4all-operator/internal/jobrunner"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ArgocdReconciler reconciles argocd installation via Helm
type ArgocdReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	ConfigMapName   string
	ConfigMapNs     string
	ValuesConfigMap string
	HelmRepoURL     string
	HelmChartName   string
	HelmReleaseName string
	HelmReleaseNs   string
	ToolsImage      string
}

// HelmCommand executes a helm command and returns the output
func (r *ArgocdReconciler) HelmCommand(ctx context.Context, args ...string) (string, error) {
	log := log.FromContext(ctx)

	log.Info("Executing helm command in container", "args", args)

	// Use the container-based implementation from jobrunner
	return jobrunner.RunHelmCommand(
		ctx,
		r.Client,
		args,
		r.ConfigMapNs, // Use the same namespace as the config map
		r.ToolsImage,
	)
}

// SaveValues saves the helm values to a ConfigMap for later use
func (r *ArgocdReconciler) SaveValues(ctx context.Context, values map[string]interface{}) error {
	log := log.FromContext(ctx)

	// Convert values to JSON
	valuesJson, err := json.Marshal(values)
	if err != nil {
		log.Error(err, "Failed to marshal helm values")
		return err
	}

	// Get or create the ConfigMap
	cm := &corev1.ConfigMap{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Name:      r.ValuesConfigMap,
		Namespace: r.ConfigMapNs,
	}, cm)

	if errors.IsNotFound(err) {
		// Create new ConfigMap
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      r.ValuesConfigMap,
				Namespace: r.ConfigMapNs,
			},
			Data: map[string]string{
				"values.json": string(valuesJson),
			},
		}
		if err := r.Client.Create(ctx, cm); err != nil {
			log.Error(err, "Failed to create values ConfigMap")
			return err
		}
	} else if err != nil {
		log.Error(err, "Failed to get values ConfigMap")
		return err
	} else {
		// Update existing ConfigMap
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data["values.json"] = string(valuesJson)
		if err := r.Client.Update(ctx, cm); err != nil {
			log.Error(err, "Failed to update values ConfigMap")
			return err
		}
	}

	return nil
}

// GetValues gets the helm values from the ConfigMap
func (r *ArgocdReconciler) GetValues(ctx context.Context) (map[string]interface{}, error) {
	// Get the ConfigMap
	cm := &corev1.ConfigMap{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Name:      r.ValuesConfigMap,
		Namespace: r.ConfigMapNs,
	}, cm)

	if errors.IsNotFound(err) {
		// ConfigMap doesn't exist, return empty values
		return map[string]interface{}{}, nil
	} else if err != nil {
		return nil, err
	}

	// Parse values from ConfigMap
	if cm.Data == nil || cm.Data["values.json"] == "" {
		// No values stored yet
		return map[string]interface{}{}, nil
	}

	var values map[string]interface{}
	if err := json.Unmarshal([]byte(cm.Data["values.json"]), &values); err != nil {
		return nil, err
	}

	return values, nil
}

// GetHostInfo retrieves IP and FQDN from the host using k4all-utils
func (r *ArgocdReconciler) GetHostInfo(ctx context.Context) (string, string, error) {
	log := log.FromContext(ctx)

	// Run the get_ip function from k4all-utils
	log.Info("Getting host IP")
	ip, err := jobrunner.RunHostCommand(
		ctx,
		r.Client,
		[]string{"echo", "$(get_cluster_ip)"},
		r.ConfigMapNs,
		r.ToolsImage,
	)
	if err != nil {
		log.Error(err, "Failed to get host IP")
		return "", "", fmt.Errorf("failed to get host IP: %w", err)
	}

	// // Run the get_fqdn function from k4all-utils
	// log.Info("Getting host FQDN")
	// fqdn, err := jobrunner.RunHostCommand(
	// 	ctx,
	// 	r.Client,
	// 	[]string{"source", "/usr/local/bin/control-plane-utils", "&&", "echo", "$(get_fqdn)"},
	// 	r.ConfigMapNs,
	// 	r.ToolsImage,
	// )
	// if err != nil {
	// 	log.Error(err, "Failed to get host FQDN")
	// 	return "", "", fmt.Errorf("failed to get host FQDN: %w", err)
	// }

	// return strings.TrimSpace(ip), strings.TrimSpace(fqdn), nil

	return strings.TrimSpace(ip), "example.com", nil

}

func (r *ArgocdReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Only process the k4all config ConfigMap
	if req.Name != r.ConfigMapName || req.Namespace != r.ConfigMapNs {
		return ctrl.Result{}, nil
	}

	log.Info("Reconciling ArgoCD based on config")

	// Get the ConfigMap
	cm := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, req.NamespacedName, cm); err != nil {
		if errors.IsNotFound(err) {
			// ConfigMap was deleted, nothing to do
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get ConfigMap")
		return ctrl.Result{}, err
	}

	// Parse the config using our config parser
	configJson, exists := cm.Data["config.json"]
	if !exists {
		log.Info("ConfigMap does not contain config.json")
		return ctrl.Result{}, nil
	}

	// Parse the JSON into our config structure
	fileConfig := &config.Config{}
	if err := json.Unmarshal([]byte(configJson), fileConfig); err != nil {
		log.Error(err, "Failed to unmarshal config JSON")
		return ctrl.Result{}, err
	}

	// Check if ArgoCD is managed
	isManaged := fileConfig.Features.ArgoCD.Managed == "true"
	if !isManaged {
		log.Info("ArgoCD is not managed by the operator")
		return ctrl.Result{}, nil
	}

	// Get the current values
	values, err := r.GetValues(ctx)
	if err != nil {
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	// Check if ArgoCD is enabled
	isEnabled := fileConfig.Features.ArgoCD.Enabled == "true"

	// Get version
	version := fileConfig.Features.ArgoCD.Version
	if version == "" {
		version = "7.8.7" // Use latest if not specified
	}

	// Update values map with the current settings
	values["enabled"] = isEnabled
	values["managed"] = isManaged
	values["version"] = version

	// Reconcile based on current state
	if isEnabled && isManaged {
		// Check if ArgoCD is installed
		_, err := r.HelmCommand(ctx, "status", r.HelmReleaseName, "-n", r.HelmReleaseNs)
		isInstalled := err == nil

		log.Info("ArgoCD is installed", "isInstalled", isInstalled, "error", err)

		if !isInstalled {
			// Get host information
			ip, fqdn, err := r.GetHostInfo(ctx)
			if err != nil {
				log.Error(err, "Failed to get host information")
				return ctrl.Result{RequeueAfter: time.Minute}, err
			}

			// Install ArgoCD
			_, err = r.HelmCommand(ctx,
				"upgrade", "--install", r.HelmReleaseName, r.HelmChartName,
				"--repo", r.HelmRepoURL,
				"--version", version,
				"--namespace", r.HelmReleaseNs,
				"--create-namespace",
				"-f", "/host/usr/local/share/argocd-values.yaml",
				"--set", fmt.Sprintf("global.domain=argo.%s.nip.io", ip),
				"--set", fmt.Sprintf("server.ingress.extraHosts[0].name=argo.%s", fqdn),
				"--set", "server.ingress.extraHosts[0].path=/",
				"--wait")

			if err != nil {
				log.Error(err, "Failed to install ArgoCD")
				return ctrl.Result{RequeueAfter: time.Minute}, err
			}

			log.Info("Successfully installed ArgoCD", "version", version)
		}

		// Save the values
		if err := r.SaveValues(ctx, values); err != nil {
			log.Error(err, "Failed to save values")
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}

	} else if !isEnabled && isManaged {
		// Check if ArgoCD is installed
		_, err := r.HelmCommand(ctx, "status", r.HelmReleaseName, "-n", r.HelmReleaseNs)
		isInstalled := err == nil

		if isInstalled {
			// Uninstall ArgoCD
			log.Info("Uninstalling ArgoCD")

			_, err = r.HelmCommand(ctx,
				"uninstall", r.HelmReleaseName,
				"--namespace", r.HelmReleaseNs)

			if err != nil {
				log.Error(err, "Failed to uninstall ArgoCD")
				return ctrl.Result{RequeueAfter: time.Minute}, err
			}

			log.Info("Successfully uninstalled ArgoCD")

			// Delete the namespace
			if err := r.Client.Delete(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: r.HelmReleaseNs,
				},
			}); err != nil {
				log.Error(err, "Failed to delete namespace")
				return ctrl.Result{RequeueAfter: time.Minute}, err
			}

			log.Info("Successfully deleted namespace")
		}

		// Save the values for potential future reinstall
		if err := r.SaveValues(ctx, values); err != nil {
			log.Error(err, "Failed to save values")
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *ArgocdReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		Named("argocd-controller"). // Give a unique name to the controller
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return e.Object.GetName() == r.ConfigMapName &&
					e.Object.GetNamespace() == r.ConfigMapNs
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				return e.ObjectNew.GetName() == r.ConfigMapName &&
					e.ObjectNew.GetNamespace() == r.ConfigMapNs
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return false // Don't reconcile on delete
			},
		}).
		Complete(r)
}
