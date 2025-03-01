package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/gpillon/k4all-operator/internal/config"
	manifests "github.com/gpillon/k4all-operator/internal/downloader"
	"github.com/gpillon/k4all-operator/internal/jobrunner"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/utils/ptr"
)

// VirtReconciler reconciles the Virt feature
type VirtReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	ConfigMapName string
	ConfigMapNs   string
	ToolsImage    string
	// Add values-related fields similar to ArgoCD
	ValuesConfigMap string
}

// GetValues retrieves the current values for Virt
func (r *VirtReconciler) GetValues(ctx context.Context) (map[string]interface{}, error) {
	cm := &corev1.ConfigMap{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Name:      r.ValuesConfigMap,
		Namespace: r.ConfigMapNs,
	}, cm)

	if errors.IsNotFound(err) {
		return map[string]interface{}{}, nil
	} else if err != nil {
		return nil, err
	}

	if cm.Data == nil || cm.Data["values.json"] == "" {
		return map[string]interface{}{}, nil
	}

	var values map[string]interface{}
	if err := json.Unmarshal([]byte(cm.Data["values.json"]), &values); err != nil {
		return nil, err
	}

	return values, nil
}

// SaveValues stores the current values
func (r *VirtReconciler) SaveValues(ctx context.Context, values map[string]interface{}) error {
	valuesJSON, err := json.Marshal(values)
	if err != nil {
		return err
	}

	cm := &corev1.ConfigMap{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Name:      r.ValuesConfigMap,
		Namespace: r.ConfigMapNs,
	}, cm)

	if errors.IsNotFound(err) {
		// Create the ConfigMap
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      r.ValuesConfigMap,
				Namespace: r.ConfigMapNs,
			},
			Data: map[string]string{
				"values.json": string(valuesJSON),
			},
		}
		return r.Client.Create(ctx, cm)
	} else if err != nil {
		return err
	}

	// Update the ConfigMap
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data["values.json"] = string(valuesJSON)
	return r.Client.Update(ctx, cm)
}

// GetHostInfo retrieves IP and FQDN from the host
func (r *VirtReconciler) GetHostInfo(ctx context.Context) (string, string, error) {
	log := log.FromContext(ctx)

	// Get cluster IP
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

	// Get FQDN
	log.Info("Getting host FQDN")
	fqdn, err := jobrunner.RunHostCommand(
		ctx,
		r.Client,
		[]string{"echo $(get_fqdn)"},
		r.ConfigMapNs,
		r.ToolsImage,
	)
	if err != nil {
		log.Error(err, "Failed to get host FQDN")
		return "", "", fmt.Errorf("failed to get host FQDN: %w", err)
	}

	return strings.TrimSpace(ip), strings.TrimSpace(fqdn), nil
}

// RunVirtSetupScript executes the Virt setup script
func (r *VirtReconciler) RunVirtSetupScript(ctx context.Context) error {
	featureConfig := FeatureScriptConfig{
		FeatureName:         "virt",
		ScriptName:          "setup-feature-virt.sh",
		AdditionalUtilities: []string{"k4all-utils"},
		Env:                 map[string]string{},
	}

	return RunFeatureScript(ctx, r.Client, featureConfig, r.ToolsImage)
}

// CheckEmulationNeeded determines if emulation should be enabled
func (r *VirtReconciler) CheckEmulationNeeded(ctx context.Context, emulationSetting string) (bool, error) {
	log := log.FromContext(ctx)

	if emulationSetting == "true" {
		return true, nil
	} else if emulationSetting == "false" {
		return false, nil
	} else if emulationSetting == "auto" {
		// Run a DaemonSet to check for KVM on all nodes and label them
		if err := r.runKVMCheckAndLabelNodes(ctx); err != nil {
			log.Error(err, "Failed to check nodes for KVM capability")
			return true, err // Default to true (emulation) on error
		}

		// Now check if any node has the KVM capability
		nodes := &corev1.NodeList{}
		if err := r.Client.List(ctx, nodes); err != nil {
			log.Error(err, "Failed to list nodes")
			return true, err // Default to true on error
		}

		// Check node labels for virtualization capabilities
		for _, node := range nodes.Items {
			if val, exists := node.Labels["feature.node.kubernetes.io/kvm"]; exists && val == "true" {
				log.Info("Found node with KVM capability, emulation not needed", "node", node.Name)
				return false, nil
			}
		}

		log.Info("No nodes with KVM capability found, emulation needed")
		return true, nil
	}

	// Default to false for unknown settings
	log.Info("Unknown emulation setting, defaulting to no emulation", "setting", emulationSetting)
	return false, nil
}

// runKVMCheckAndLabelNodes creates a DaemonSet to check and label all nodes for KVM capability
func (r *VirtReconciler) runKVMCheckAndLabelNodes(ctx context.Context) error {
	log := log.FromContext(ctx)
	log.Info("Checking all nodes for KVM capability")

	// Create or update the DaemonSet
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kvm-capability-check",
			Namespace: r.ConfigMapNs,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "kvm-capability-check",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "kvm-capability-check",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "k4all-operator-controller-manager", // Make sure this service account has node labeling privileges
					Containers: []corev1.Container{
						{
							Name:  "kvm-check",
							Image: r.ToolsImage, // Using the operator's tools image
							Command: []string{
								"sh",
								"-c",
								`
								# We can use the Kubernetes-injected environment variable directly
								NODE_NAME=$HOSTNAME
								echo "Checking for KVM device on node ${NODE_NAME}"
								
								if [ -e /dev/kvm ]; then
									echo "KVM device found on node ${NODE_NAME}, labeling node"
									kubectl label node ${NODE_NAME} feature.node.kubernetes.io/kvm=true --overwrite
								else
									echo "KVM device not found on node ${NODE_NAME}, labeling node"
									kubectl label node ${NODE_NAME} feature.node.kubernetes.io/kvm=false --overwrite
								fi
								
								# Keep the pod running for a while to allow time for the label to be applied
								sleep 60
								`,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "dev",
									MountPath: "/dev",
								},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(true),
							},
							Env: []corev1.EnvVar{
								{
									Name: "HOSTNAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "dev",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/dev",
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyAlways,
					Tolerations: []corev1.Toleration{
						{
							Operator: corev1.TolerationOpExists, // This allows running on all nodes
						},
					},
				},
			},
		},
	}

	// Check if the DaemonSet already exists
	existing := &appsv1.DaemonSet{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Name:      daemonSet.Name,
		Namespace: daemonSet.Namespace,
	}, existing)

	if err != nil {
		if errors.IsNotFound(err) {
			// DaemonSet doesn't exist, create it
			log.Info("Creating KVM check DaemonSet")
			if err := r.Client.Create(ctx, daemonSet); err != nil {
				log.Error(err, "Failed to create KVM check DaemonSet", "error", err.Error())
				return err
			}
		} else {
			log.Error(err, "Failed to check for existing KVM DaemonSet")
			return err
		}
	} else {
		// DaemonSet exists, update it
		log.Info("Updating existing KVM check DaemonSet")
		existing.Spec = daemonSet.Spec
		if err := r.Client.Update(ctx, existing); err != nil {
			log.Error(err, "Failed to update KVM check DaemonSet", "error", err.Error())
			return err
		}
	}

	// Wait for the DaemonSet to deploy on all nodes
	log.Info("Waiting for KVM check DaemonSet to deploy on all nodes")
	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 2*time.Minute, false, func(ctx context.Context) (bool, error) {
		ds := &appsv1.DaemonSet{}
		if err := r.Client.Get(ctx, types.NamespacedName{
			Name:      daemonSet.Name,
			Namespace: daemonSet.Namespace,
		}, ds); err != nil {
			if errors.IsNotFound(err) {
				return false, nil // Still waiting for creation
			}
			return false, err // Real error
		}

		// Check if DaemonSet is fully deployed
		if ds.Status.NumberReady == ds.Status.DesiredNumberScheduled &&
			ds.Status.DesiredNumberScheduled > 0 {
			log.Info("KVM check DaemonSet ready",
				"ready", ds.Status.NumberReady,
				"desired", ds.Status.DesiredNumberScheduled)
			return true, nil
		}

		log.Info("Waiting for KVM check DaemonSet to be ready",
			"ready", ds.Status.NumberReady,
			"desired", ds.Status.DesiredNumberScheduled)
		return false, nil
	})

	if err != nil {
		log.Error(err, "Timeout waiting for KVM check DaemonSet")
		return err
	}

	// Give some extra time for the labels to propagate
	time.Sleep(5 * time.Second)

	// Verify that all nodes have the KVM label
	allNodesLabeled := false
	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 1*time.Minute, false, func(ctx context.Context) (bool, error) {
		// Get list of all nodes
		nodes := &corev1.NodeList{}
		if err := r.Client.List(ctx, nodes); err != nil {
			log.Error(err, "Failed to list nodes for label verification")
			return false, err
		}

		allLabeled := true
		totalNodes := len(nodes.Items)
		labeledNodes := 0

		for _, node := range nodes.Items {
			if _, exists := node.Labels["feature.node.kubernetes.io/kvm"]; !exists {
				allLabeled = false
				log.Info("Waiting for node to be labeled", "node", node.Name)
			} else {
				labeledNodes++
			}
		}

		log.Info("Checking node labeling progress", "labeled", labeledNodes, "total", totalNodes)
		return allLabeled, nil
	})

	if err != nil {
		log.Error(err, "Timeout waiting for all nodes to be labeled")
		// Don't return error here, we'll try to clean up anyway
	} else {
		allNodesLabeled = true
	}

	// Clean up the DaemonSet now that we're done with it
	log.Info("Cleaning up KVM check DaemonSet")
	if err := r.Client.Delete(ctx, daemonSet); err != nil && !errors.IsNotFound(err) {
		log.Error(err, "Failed to delete KVM check DaemonSet")
		// If we successfully labeled all nodes, don't return an error
		if !allNodesLabeled {
			return err
		}
	}

	log.Info("KVM capability checking complete, DaemonSet deleted")
	return nil
}

// IsVirtInstalled checks if KubeVirt, CDI, and KubeVirt Manager are properly installed
func (r *VirtReconciler) IsVirtInstalled(ctx context.Context) (bool, error) {

	// 1. Check for the KubeVirt CR
	kubevirtInstalled, err := r.isKubeVirtInstalled(ctx)
	if err != nil {
		return false, err
	}
	if !kubevirtInstalled {
		return false, nil
	}

	// 2. Check for the CDI CR
	cdiInstalled, err := r.isCDIInstalled(ctx)
	if err != nil {
		return false, err
	}
	if !cdiInstalled {
		return false, nil
	}

	// 3. Check for KubeVirt Manager deployment
	kvmInstalled, err := r.isKubeVirtManagerInstalled(ctx)
	if err != nil {
		return false, err
	}

	// All components must be installed
	return kubevirtInstalled && cdiInstalled && kvmInstalled, nil
}

// isKubeVirtInstalled checks if KubeVirt is properly installed
func (r *VirtReconciler) isKubeVirtInstalled(ctx context.Context) (bool, error) {
	log := log.FromContext(ctx)

	// Check for the KubeVirt CR
	kubevirt := &unstructured.Unstructured{}
	kubevirt.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "kubevirt.io",
		Version: "v1",
		Kind:    "KubeVirt",
	})

	err := r.Client.Get(ctx, types.NamespacedName{
		Name:      "kubevirt",
		Namespace: "kubevirt",
	}, kubevirt)

	if errors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		log.Error(err, "Failed to check for KubeVirt CR")
		return false, err
	}

	// Check KubeVirt status
	phase, found, err := unstructured.NestedString(kubevirt.Object, "status", "phase")
	if err != nil {
		log.Error(err, "Failed to get KubeVirt phase")
		return false, err
	}

	if !found {
		log.Info("KubeVirt status phase not found, installation may be in progress")
		return false, nil
	}

	return phase == "Deployed", nil
}

// isCDIInstalled checks if CDI is properly installed
func (r *VirtReconciler) isCDIInstalled(ctx context.Context) (bool, error) {
	log := log.FromContext(ctx)

	// Check for the CDI CR
	cdi := &unstructured.Unstructured{}
	cdi.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cdi.kubevirt.io",
		Version: "v1beta1",
		Kind:    "CDI",
	})

	err := r.Client.Get(ctx, types.NamespacedName{Name: "cdi"}, cdi)
	if err != nil {
		// If the error is because the CRD doesn't exist yet, that's not an error
		// it just means CDI is not installed
		if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
			log.Info("CDI CRD not found in the cluster, CDI is not installed")
			return false, nil
		}

		// If it's a NotFound error, it means the CDI CR doesn't exist
		if errors.IsNotFound(err) {
			return false, nil
		}

		// For any other error, return it
		log.Error(err, "Failed to check for CDI CR")
		return false, err
	}

	// Check the phase
	phase, found, err := unstructured.NestedString(cdi.Object, "status", "phase")
	if err != nil {
		log.Error(err, "Failed to get CDI phase")
		return false, err
	}

	if !found {
		log.Info("CDI status phase not found, may still be installing")
		return false, nil
	}

	return phase == "Deployed", nil
}

// isKubeVirtManagerInstalled checks if KubeVirt Manager is properly installed
func (r *VirtReconciler) isKubeVirtManagerInstalled(ctx context.Context) (bool, error) {
	log := log.FromContext(ctx)

	// Check for KubeVirt Manager deployment
	// KubeVirt Manager typically installs in its own namespace
	deployment := &unstructured.Unstructured{}
	deployment.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apps",
		Version: "v1",
		Kind:    "Deployment",
	})

	err := r.Client.Get(ctx, types.NamespacedName{
		Name:      "kubevirt-manager",
		Namespace: "kubevirt-manager",
	}, deployment)

	if errors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		log.Error(err, "Failed to check for KubeVirt Manager deployment")
		return false, err
	}

	// Check deployment status
	availableReplicas, found, err := unstructured.NestedInt64(deployment.Object, "status", "availableReplicas")
	if err != nil {
		log.Error(err, "Failed to get KubeVirt Manager available replicas")
		return false, err
	}

	if !found {
		log.Info("KubeVirt Manager status not found, installation may be in progress")
		return false, nil
	}

	return availableReplicas > 0, nil
}

// Reconcile handles the reconciliation of the Virt feature
func (r *VirtReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Only process the k4all config ConfigMap
	if req.Name != r.ConfigMapName || req.Namespace != r.ConfigMapNs {
		return ctrl.Result{}, nil
	}

	log.Info("Reconciling Virt based on config")

	// Get the ConfigMap
	cm := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, req.NamespacedName, cm); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get ConfigMap")
		return ctrl.Result{}, err
	}

	// Parse the config
	configJson, exists := cm.Data["config.json"]
	if !exists {
		log.Info("ConfigMap does not contain config.json")
		return ctrl.Result{}, nil
	}

	fileConfig := &config.Config{}
	if err := json.Unmarshal([]byte(configJson), fileConfig); err != nil {
		log.Error(err, "Failed to unmarshal config JSON")
		return ctrl.Result{}, err
	}

	// Check if Virt is managed
	isManaged := fileConfig.Features.Virt.Managed == "true"
	if !isManaged {
		log.Info("Virt is not managed by the operator")
		return ctrl.Result{}, nil
	}

	// Get the current values
	values, err := r.GetValues(ctx)
	if err != nil {
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	// Check if Virt is enabled
	isEnabled := fileConfig.Features.Virt.Enabled == "true"
	emulationSetting := fileConfig.Features.Virt.Emulation

	// Update values map
	values["enabled"] = isEnabled
	values["managed"] = isManaged
	values["emulation"] = emulationSetting

	// Determine if we need to install or remove
	if isEnabled && isManaged {
		// Check if virt is installed by looking for KubeVirt resources
		isInstalled, err := r.IsVirtInstalled(ctx)
		if err != nil {
			log.Error(err, "Failed to check if Virt is installed")
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}

		log.Info("Virt installation status", "isInstalled", isInstalled)

		if !isInstalled {
			log.Info("Virt is not installed, installing")
			// Get host information for ingress setup (if needed)
			// ip, fqdn, err := r.GetHostInfo(ctx)
			// if err != nil {
			// 	log.Error(err, "Failed to get host information")
			// 	return ctrl.Result{RequeueAfter: time.Minute}, err
			// }
			// log.Info("Host information", "IP", ip, "FQDN", fqdn)

			// Install components directly through the client
			if err := r.InstallVirtComponents(ctx); err != nil {
				log.Error(err, "Failed to install Virt components")
				return ctrl.Result{RequeueAfter: time.Minute}, err
			}

			log.Info("Successfully initiated Virt installation")
			// Schedule a follow-up check as installation may take time
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		} else {
			// Virt is already installed, check if we need to update emulation setting
			useEmulation, err := r.CheckEmulationNeeded(ctx, emulationSetting)
			if err != nil {
				log.Error(err, "Failed to determine emulation setting")
				return ctrl.Result{RequeueAfter: time.Minute}, err
			}

			// Update emulation setting if needed
			if err := r.UpdateEmulationSetting(ctx, useEmulation); err != nil {
				log.Error(err, "Failed to update emulation setting")
				return ctrl.Result{RequeueAfter: time.Minute}, err
			}
		}

		// Save the values
		if err := r.SaveValues(ctx, values); err != nil {
			log.Error(err, "Failed to save values")
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}
	} else {
		// Check if Virt is installed
		isInstalled, err := r.IsVirtInstalled(ctx)
		if err != nil {
			log.Error(err, "Failed to check if Virt is installed")
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}

		if isInstalled {

			// Save the values for potential future reinstall
			if err := r.SaveValues(ctx, values); err != nil {
				log.Error(err, "Failed to save values")
				return ctrl.Result{RequeueAfter: time.Minute}, err
			}

			log.Info("Uninstalling Virt")

			// Uninstall components
			if err := r.UninstallVirtComponents(ctx); err != nil {
				log.Error(err, "Failed to uninstall Virt components")
				return ctrl.Result{RequeueAfter: time.Minute}, err
			}

			log.Info("Successfully initiated Virt uninstallation")
			// Schedule a follow-up check
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		isNamespacePresent, err := r.IsNamespacePresent(ctx)
		if err != nil {
			log.Error(err, "Failed to check if namespace is present")
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}

		if isNamespacePresent {
			log.Info("Namespace is present, deleting it")
			if err := r.DeleteNamespace(ctx); err != nil {
				log.Error(err, "Failed to delete namespace")
				return ctrl.Result{RequeueAfter: time.Minute}, err
			}
		}
	}

	return ctrl.Result{}, nil
}

// IsNamespacePresent checks if the namespace is present
func (r *VirtReconciler) IsNamespacePresent(ctx context.Context) (bool, error) {
	ns := &corev1.Namespace{}
	err := r.Client.Get(ctx, types.NamespacedName{Name: "kubevirt"}, ns)
	return err == nil, nil
}

// DeleteNamespace deletes the namespace
func (r *VirtReconciler) DeleteNamespace(ctx context.Context) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kubevirt",
		},
	}
	return r.Client.Delete(ctx, ns)
}

// UpdateEmulationSetting updates the KubeVirt CR to enable/disable emulation
func (r *VirtReconciler) UpdateEmulationSetting(ctx context.Context, useEmulation bool) error {
	kubevirt := &unstructured.Unstructured{}
	kubevirt.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "kubevirt.io",
		Version: "v1",
		Kind:    "KubeVirt",
	})

	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      "kubevirt",
		Namespace: "kubevirt",
	}, kubevirt); err != nil {
		return err
	}

	// Check if the current setting matches what we want
	currentValue, found, err := unstructured.NestedBool(
		kubevirt.Object,
		"spec", "configuration", "developerConfiguration", "useEmulation",
	)

	if err != nil {
		return err
	}

	// If the setting is already correct or not found (and we're disabling), skip update
	if (found && currentValue == useEmulation) || (!found && !useEmulation) {
		return nil
	}

	// Update the configuration
	if err := unstructured.SetNestedField(kubevirt.Object, useEmulation,
		"spec", "configuration", "developerConfiguration", "useEmulation"); err != nil {
		return err
	}

	if err := r.Client.Update(ctx, kubevirt); err != nil {
		return err
	}

	log.FromContext(ctx).Info("Updated emulation setting", "useEmulation", useEmulation)
	return nil
}

// InstallVirtComponents installs KubeVirt and related components using the official method
func (r *VirtReconciler) InstallVirtComponents(ctx context.Context) error {
	log := log.FromContext(ctx)
	log.Info("Installing KubeVirt components")

	// Ensure the kubevirt namespace exists
	if err := manifests.EnsureNamespace(ctx, r.Client, "kubevirt"); err != nil {
		log.Error(err, "Failed to create kubevirt namespace")
		return err
	}

	// Get the latest stable KubeVirt version
	version, err := manifests.GetLatestKubeVirtVersion(ctx)
	if err != nil {
		log.Error(err, "Failed to get latest KubeVirt version")
		return err
	}
	version = strings.TrimSpace(version)
	log.Info("Using KubeVirt version", "version", version)

	// Apply the KubeVirt operator
	operatorURL := fmt.Sprintf("https://github.com/kubevirt/kubevirt/releases/download/%s/kubevirt-operator.yaml", version)
	if err := manifests.ApplyYAMLFromURL(ctx, r.Client, operatorURL); err != nil {
		log.Error(err, "Failed to apply KubeVirt operator")
		return err
	}

	// Apply the KubeVirt CR
	crURL := fmt.Sprintf("https://github.com/kubevirt/kubevirt/releases/download/%s/kubevirt-cr.yaml", version)
	if err := manifests.ApplyYAMLFromURL(ctx, r.Client, crURL); err != nil {
		log.Error(err, "Failed to apply KubeVirt CR")
		return err
	}

	// Wait for KubeVirt to be ready (with a timeout of 10 minutes)
	if err := manifests.WaitForKubeVirtReady(ctx, r.Client, 10*time.Minute); err != nil {
		log.Error(err, "Failed waiting for KubeVirt to be ready")
		return err
	}

	// Apply emulation setting if needed
	emulationSetting, err := r.CheckEmulationNeeded(ctx, r.getEmulationSetting())
	if err != nil {
		log.Error(err, "Failed to determine emulation setting")
		return err
	}

	if emulationSetting {
		log.Info("Configuring KubeVirt to use emulation")
		if err := r.UpdateEmulationSetting(ctx, true); err != nil {
			log.Error(err, "Failed to update emulation setting")
			return err
		}
	}

	// Install CDI
	if err := r.InstallCDI(ctx); err != nil {
		log.Error(err, "Failed to install CDI")
		return err
	}

	// Install KubeVirt Manager
	if err := r.InstallKubeVirtManager(ctx); err != nil {
		log.Error(err, "Failed to install KubeVirt Manager")
		return err
	}

	log.Info("Successfully installed all KubeVirt components")
	return nil
}

// getEmulationSetting retrieves the current emulation setting from values
func (r *VirtReconciler) getEmulationSetting() string {
	// This could be fetched from the config object directly
	// For now, using a placeholder implementation
	return "auto"
}

// UninstallVirtComponents removes KubeVirt and related components
func (r *VirtReconciler) UninstallVirtComponents(ctx context.Context) error {
	log := log.FromContext(ctx)
	log.Info("Uninstalling KubeVirt components")

	// Uninstall in reverse order of installation

	// First, uninstall KubeVirt Manager
	if err := r.UninstallKubeVirtManager(ctx); err != nil {
		log.Error(err, "Failed to uninstall KubeVirt Manager")
		// Continue with other uninstallations
	}

	// Then, uninstall CDI
	if err := r.UninstallCDI(ctx); err != nil {
		log.Error(err, "Failed to uninstall CDI")
		// Continue with other uninstallations
	}

	// Delete the KubeVirt CR
	kubevirt := &unstructured.Unstructured{}
	kubevirt.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "kubevirt.io",
		Version: "v1",
		Kind:    "KubeVirt",
	})
	kubevirt.SetName("kubevirt")
	kubevirt.SetNamespace("kubevirt")

	log.Info("Deleting KubeVirt CR")
	if err := r.Client.Delete(ctx, kubevirt); err != nil && !errors.IsNotFound(err) {
		log.Error(err, "Failed to delete KubeVirt CR")
		// Continue with remaining uninstallations
	}

	// Wait for KubeVirt CR to be deleted (with timeout)
	log.Info("Waiting for KubeVirt CR to be deleted")
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	err := wait.PollUntilContextTimeout(waitCtx, 5*time.Second, 3*time.Minute, false, func(ctx context.Context) (bool, error) {
		tempKV := &unstructured.Unstructured{}
		tempKV.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "kubevirt.io",
			Version: "v1",
			Kind:    "KubeVirt",
		})

		err := r.Client.Get(ctx, types.NamespacedName{
			Name:      "kubevirt",
			Namespace: "kubevirt",
		}, tempKV)

		if errors.IsNotFound(err) {
			log.Info("KubeVirt CR has been deleted")
			return true, nil
		} else if err != nil {
			// Log but continue polling on API errors
			log.Error(err, "Error checking if KubeVirt CR is deleted")
			return false, nil
		}

		log.Info("Waiting for KubeVirt CR deletion to complete")
		return false, nil
	})

	if err != nil {
		log.Error(err, "Timeout waiting for KubeVirt CR deletion")
		// Continue anyway to try to clean up everything else
	}

	// Get the latest version to uninstall the operator from the correct URL
	version, err := manifests.GetLatestKubeVirtVersion(ctx)
	if err != nil {
		log.Error(err, "Failed to get KubeVirt version for operator uninstallation")
		// Use a fallback approach - just try to delete the namespace
	} else {
		version = strings.TrimSpace(version)
		operatorURL := fmt.Sprintf("https://github.com/kubevirt/kubevirt/releases/download/%s/kubevirt-operator.yaml", version)

		// Use our helper instead of kubectl
		if err := manifests.DeleteYAMLFromURL(ctx, r.Client, operatorURL); err != nil {
			log.Error(err, "Failed to delete KubeVirt operator resources")
			// Continue to cleanup namespace
		}
	}

	// As a final fallback, try to delete the namespace
	log.Info("Deleting kubevirt namespace")
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kubevirt",
		},
	}
	if err := r.Client.Delete(ctx, ns); err != nil && !errors.IsNotFound(err) {
		log.Error(err, "Failed to delete kubevirt namespace")
		return err
	}

	log.Info("Successfully initiated KubeVirt uninstallation")
	return nil
}

// SetupWithManager sets up the controller with the Manager
func (r *VirtReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		Named("virt-controller"). // Give a unique name to the controller
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

// CleanupCDIResources removes any existing CDI resources that might interfere with installation
func (r *VirtReconciler) CleanupCDIResources(ctx context.Context) error {
	log := log.FromContext(ctx)
	log.Info("Cleaning up existing CDI resources")

	// Delete the CDI CR if it exists
	cdi := &unstructured.Unstructured{}
	cdi.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cdi.kubevirt.io",
		Version: "v1beta1",
		Kind:    "CDI",
	})
	cdi.SetName("cdi")

	if err := r.Client.Delete(ctx, cdi); err != nil && !errors.IsNotFound(err) {
		log.Error(err, "Failed to delete CDI CR")
		// Continue with installation anyway
	} else if err == nil {
		log.Info("Deleted CDI CR")

		// Wait for CDI CR to be deleted with a more reasonable timeout
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()

		err := wait.PollUntilContextTimeout(waitCtx, 5*time.Second, 10*time.Second, false, func(ctx context.Context) (bool, error) {
			tempCDI := &unstructured.Unstructured{}
			tempCDI.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "cdi.kubevirt.io",
				Version: "v1beta1",
				Kind:    "CDI",
			})

			err := r.Client.Get(ctx, types.NamespacedName{Name: "cdi"}, tempCDI)

			if errors.IsNotFound(err) {
				log.Info("CDI CR has been deleted")
				return true, nil
			} else if err != nil {
				// Log but continue polling on API errors
				log.Error(err, "Error checking if CDI CR is deleted")
				return false, nil
			}

			log.Info("Waiting for CDI CR deletion to complete")
			return false, nil
		})

		if err != nil {
			log.Error(err, "Timeout waiting for CDI CR deletion")
			// try to remove the cdi finalizer
			cdi.SetFinalizers([]string{})
			if err := r.Client.Update(ctx, cdi); err != nil {
				log.Error(err, "Failed to remove CDI finalizer")
			}
			log.Info("Removed CDI finalizer")
			// try again to delete the cdi
			if err := r.Client.Delete(ctx, cdi); err != nil && !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete CDI CR")
			} else if err == nil {
				log.Info("Deleted CDI CR")
			}
		}
	}

	// Get the latest CDI version to clean up operator resources
	version, err := manifests.GetLatestCDIVersion(ctx)
	if err != nil {
		log.Error(err, "Failed to get CDI version for cleanup")
		// Continue with installation anyway
	} else {
		// Fetch and delete resources from the operator YAML
		operatorURL := fmt.Sprintf("https://github.com/kubevirt/containerized-data-importer/releases/download/v%s/cdi-operator.yaml", version)

		// Use our helper instead of kubectl
		log.Info("Deleting CDI operator resources", "operatorURL", operatorURL)
		if err := manifests.DeleteYAMLFromURL(ctx, r.Client, operatorURL); err != nil {
			log.Error(err, "Failed to delete CDI operator resources")
			// Continue with installation anyway
		}
	}

	// delete the cdi namespace
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cdi",
		},
	}
	if err := r.Client.Delete(ctx, ns); err != nil && !errors.IsNotFound(err) {
		log.Error(err, "Failed to delete CDI namespace")
	}

	// wait for the cdi namespace to be deleted
	log.Info("Waiting for CDI namespace to be deleted")
	_ = wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Second, false, func(ctx context.Context) (bool, error) {
		ns := &corev1.Namespace{}
		ns.SetName("cdi")
		err := r.Client.Get(ctx, types.NamespacedName{Name: "cdi"}, ns)
		if errors.IsNotFound(err) {
			log.Info("CDI namespace has been deleted")
			return true, nil
		}
		return false, nil
	})

	log.Info("CDI cleanup completed")

	// // Delete specific ClusterRoles
	// clusterRoles := []string{
	// 	"cdi",
	// 	"cdi-apiserver",
	// 	"cdi-cronjob",
	// 	"cdi-uploadproxy",
	// 	"cdi.kubevirt.io:admin",
	// 	"cdi.kubevirt.io:config-reader",
	// }

	// for _, roleName := range clusterRoles {
	// 	role := &unstructured.Unstructured{}
	// 	role.SetGroupVersionKind(schema.GroupVersionKind{
	// 		Group:   "rbac.authorization.k8s.io",
	// 		Version: "v1",
	// 		Kind:    "ClusterRole",
	// 	})
	// 	role.SetName(roleName)

	// 	if err := r.Client.Delete(ctx, role); err != nil && !errors.IsNotFound(err) {
	// 		log.Error(err, "Failed to delete ClusterRole", "name", roleName)
	// 		// Continue with other deletions
	// 	} else if err == nil {
	// 		log.Info("Deleted ClusterRole", "name", roleName)
	// 	}
	// }

	// // Delete ServiceAccounts across different namespaces
	// serviceAccounts := []struct {
	// 	Name      string
	// 	Namespace string
	// }{
	// 	{"cdi-cronjob", "cdi"},
	// 	{"cdi-uploadproxy", "cdi"},
	// 	{"cdi-sa", "cdi"},
	// 	{"cdi-apiserver", "cdi"},
	// 	{"cdi-operator", "cdi"},
	// }

	// for _, sa := range serviceAccounts {
	// 	serviceAccount := &unstructured.Unstructured{}
	// 	serviceAccount.SetGroupVersionKind(schema.GroupVersionKind{
	// 		Group:   "",
	// 		Version: "v1",
	// 		Kind:    "ServiceAccount",
	// 	})
	// 	serviceAccount.SetName(sa.Name)
	// 	serviceAccount.SetNamespace(sa.Namespace)

	// 	if err := r.Client.Delete(ctx, serviceAccount); err != nil && !errors.IsNotFound(err) {
	// 		log.Error(err, "Failed to delete ServiceAccount", "name", sa.Name, "namespace", sa.Namespace)
	// 		// Continue with other deletions
	// 	} else if err == nil {
	// 		log.Info("Deleted ServiceAccount", "name", sa.Name, "namespace", sa.Namespace)
	// 	}
	// }

	return nil
}

// InstallCDI installs the Containerized Data Importer (CDI)
func (r *VirtReconciler) InstallCDI(ctx context.Context) error {
	log := log.FromContext(ctx)
	log.Info("Installing CDI components")

	// Clean up any existing CDI resources
	if err := r.CleanupCDIResources(ctx); err != nil {
		log.Error(err, "Failed to clean up existing CDI resources")
		// Continue with installation
	}

	// Get the latest CDI version
	version, err := manifests.GetLatestCDIVersion(ctx)
	if err != nil {
		log.Error(err, "Failed to get latest CDI version")
		return err
	}
	version = strings.TrimSpace(version)
	log.Info("Using CDI version", "version", version)

	// Apply the CDI operator
	operatorURL := fmt.Sprintf("https://github.com/kubevirt/containerized-data-importer/releases/download/v%s/cdi-operator.yaml", version)
	if err := manifests.ApplyYAMLFromURL(ctx, r.Client, operatorURL); err != nil {
		log.Error(err, "Failed to apply CDI operator")
		return err
	}

	// Wait for the CDI CRD to be registered by the operator
	log.Info("Waiting for CDI CRD to be registered")
	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 2*time.Minute, false, func(ctx context.Context) (bool, error) {
		// Check if the CRD exists by attempting to list CDI resources
		cdiList := &unstructured.UnstructuredList{}
		cdiList.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "cdi.kubevirt.io",
			Version: "v1beta1",
			Kind:    "CDIList",
		})

		err := r.Client.List(ctx, cdiList)
		if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
			log.Info("CDI CRD not yet registered, waiting...")
			return false, nil
		}

		if err != nil {
			log.Error(err, "Error checking for CDI CRD")
			return false, nil // Continue waiting
		}

		log.Info("CDI CRD is now registered")
		return true, nil
	})

	if err != nil {
		log.Error(err, "Timeout waiting for CDI CRD to be registered")
		return err
	}

	// Apply the CDI CR
	crURL := fmt.Sprintf("https://github.com/kubevirt/containerized-data-importer/releases/download/v%s/cdi-cr.yaml", version)
	if err := manifests.ApplyYAMLFromURL(ctx, r.Client, crURL); err != nil {
		log.Error(err, "Failed to apply CDI CR")
		return err
	}

	// Wait for CDI to be ready
	log.Info("Waiting for CDI to be ready")
	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, false, func(ctx context.Context) (bool, error) {
		cdi := &unstructured.Unstructured{}
		cdi.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "cdi.kubevirt.io",
			Version: "v1beta1",
			Kind:    "CDI",
		})

		err := r.Client.Get(ctx, types.NamespacedName{Name: "cdi"}, cdi)
		if err != nil {
			if errors.IsNotFound(err) {
				log.Info("CDI CR not found yet, still waiting")
				return false, nil
			}
			log.Error(err, "Error checking CDI status")
			return false, nil // Continue waiting
		}

		// Check CDI phase
		phase, found, err := unstructured.NestedString(cdi.Object, "status", "phase")
		if err != nil {
			log.Error(err, "Error getting CDI phase")
			return false, nil
		}

		if !found {
			log.Info("CDI phase not found yet, still waiting")
			return false, nil
		}

		log.Info("CDI current phase", "phase", phase)
		return phase == "Deployed", nil
	})

	if err != nil {
		log.Error(err, "Timeout waiting for CDI to be ready")
		return err
	}

	// Patch the CDI resource to set memory limits
	log.Info("Patching CDI resource with memory limits")
	patch := []byte(`{"spec": {"config": {"podResourceRequirements": {"limits": {"memory": "2G"}}}}}`)

	if err := manifests.PatchResource(ctx, r.Client,
		schema.GroupVersionKind{
			Group:   "cdi.kubevirt.io",
			Version: "v1beta1",
			Kind:    "CDI",
		},
		"cdi", "", patch); err != nil {
		log.Error(err, "Failed to patch CDI resource")
		return err
	}

	log.Info("Successfully installed CDI components")
	return nil
}

// InstallKubeVirtManager installs the KubeVirt Manager UI
func (r *VirtReconciler) InstallKubeVirtManager(ctx context.Context) error {
	log := log.FromContext(ctx)
	log.Info("Installing KubeVirt Manager")

	// First check if the deployment already exists
	deployment := &unstructured.Unstructured{}
	deployment.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apps",
		Version: "v1",
		Kind:    "Deployment",
	})

	err := r.Client.Get(ctx, types.NamespacedName{
		Name:      "kubevirt-manager",
		Namespace: "kubevirt-manager",
	}, deployment)

	// If deployment exists, we need to delete it first to avoid immutable field errors
	if err == nil {
		log.Info("KubeVirt Manager deployment already exists, deleting it before reinstalling")
		if err := r.Client.Delete(ctx, deployment, &client.DeleteOptions{}); err != nil {
			log.Error(err, "Failed to delete existing KubeVirt Manager deployment")
			return err
		}

		// Wait for deletion to complete
		log.Info("Waiting for existing deployment to be deleted")
		err = wait.PollUntilContextTimeout(ctx, 2*time.Second, 30*time.Second, false, func(ctx context.Context) (bool, error) {
			err := r.Client.Get(ctx, types.NamespacedName{
				Name:      "kubevirt-manager",
				Namespace: "kubevirt-manager",
			}, deployment)
			return errors.IsNotFound(err), nil
		})

		if err != nil {
			log.Error(err, "Timeout waiting for KubeVirt Manager deployment deletion")
			// Continue anyway, as the next step might still work
		}
	} else if !errors.IsNotFound(err) {
		log.Error(err, "Failed to check for existing KubeVirt Manager deployment")
		return err
	}

	// Ensure namespace exists
	if err := manifests.EnsureNamespace(ctx, r.Client, "kubevirt-manager"); err != nil {
		log.Error(err, "Failed to create kubevirt-manager namespace")
		return err
	}

	// Apply the bundled resources
	bundleURL := "https://raw.githubusercontent.com/kubevirt-manager/kubevirt-manager/main/kubernetes/bundled.yaml"
	if err := manifests.ApplyYAMLFromURL(ctx, r.Client, bundleURL); err != nil {
		log.Error(err, "Failed to apply KubeVirt Manager bundle")
		return err
	}

	// Apply the CRDs
	crdURL := "https://raw.githubusercontent.com/kubevirt-manager/kubevirt-manager/main/kubernetes/crd.yaml"
	if err := manifests.ApplyYAMLFromURL(ctx, r.Client, crdURL); err != nil {
		log.Error(err, "Failed to apply KubeVirt Manager CRDs")
		return err
	}

	// Wait for deployment to be ready
	log.Info("Waiting for KubeVirt Manager deployment to be ready")
	err = wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
		err := r.Client.Get(ctx, types.NamespacedName{
			Name:      "kubevirt-manager",
			Namespace: "kubevirt-manager",
		}, deployment)
		if err != nil {
			if errors.IsNotFound(err) {
				return false, nil // Not ready yet
			}
			return false, err // Real error
		}

		// Check deployment status
		availableReplicas, found, err := unstructured.NestedInt64(deployment.Object, "status", "availableReplicas")
		if err != nil || !found {
			return false, nil // Not ready yet
		}

		return availableReplicas > 0, nil
	})

	if err != nil {
		log.Error(err, "Failed to wait for KubeVirt Manager deployment to be ready")
		return err
	}

	log.Info("Successfully installed KubeVirt Manager")
	return nil
}

// UninstallCDI removes the CDI components
func (r *VirtReconciler) UninstallCDI(ctx context.Context) error {
	log := log.FromContext(ctx)
	log.Info("Uninstalling CDI components")

	// Delete the CDI CR first
	cdi := &unstructured.Unstructured{}
	cdi.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cdi.kubevirt.io",
		Version: "v1beta1",
		Kind:    "CDI",
	})
	cdi.SetName("cdi")

	if err := r.Client.Delete(ctx, cdi); err != nil && !errors.IsNotFound(err) {
		log.Error(err, "Failed to delete CDI CR")
		return err
	}

	// Get the latest version to uninstall the operator from the correct URL
	version, err := manifests.GetLatestCDIVersion(ctx)
	if err != nil {
		log.Error(err, "Failed to get CDI version for operator uninstallation")
		// Continue with deletion anyway
	} else {
		operatorURL := fmt.Sprintf("https://github.com/kubevirt/containerized-data-importer/releases/download/v%s/cdi-operator.yaml", version)

		// Use our helper instead of kubectl
		if err := manifests.DeleteYAMLFromURL(ctx, r.Client, operatorURL); err != nil {
			log.Error(err, "Failed to delete CDI operator resources")
		}
	}

	log.Info("Successfully initiated CDI uninstallation")
	return nil
}

// UninstallKubeVirtManager removes KubeVirt Manager components
func (r *VirtReconciler) UninstallKubeVirtManager(ctx context.Context) error {
	log := log.FromContext(ctx)
	log.Info("Uninstalling KubeVirt Manager")

	// Delete the bundled resources
	bundleURL := "https://raw.githubusercontent.com/kubevirt-manager/kubevirt-manager/main/kubernetes/bundled.yaml"
	if err := manifests.DeleteYAMLFromURL(ctx, r.Client, bundleURL); err != nil {
		log.Error(err, "Failed to delete KubeVirt Manager bundle resources")
		// Continue with CRD deletion anyway
	}

	// Delete the CRDs
	crdURL := "https://raw.githubusercontent.com/kubevirt-manager/kubevirt-manager/main/kubernetes/crd.yaml"
	if err := manifests.DeleteYAMLFromURL(ctx, r.Client, crdURL); err != nil {
		log.Error(err, "Failed to delete KubeVirt Manager CRD resources")
	}

	log.Info("Successfully initiated KubeVirt Manager uninstallation")
	return nil
}
