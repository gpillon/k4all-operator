package manifests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// GetLatestKubeVirtVersion fetches the latest stable KubeVirt version
func GetLatestKubeVirtVersion(ctx context.Context) (string, error) {
	resp, err := http.Get("https://storage.googleapis.com/kubevirt-prow/release/kubevirt/kubevirt/stable.txt")
	if err != nil {
		return "", fmt.Errorf("failed to get latest KubeVirt version: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	versionBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read version response: %w", err)
	}

	version := string(versionBytes)
	return version, nil
}

// ApplyYAMLFromURL downloads and applies a YAML manifest from a URL
func ApplyYAMLFromURL(ctx context.Context, c client.Client, url string) error {
	logger := log.FromContext(ctx)
	logger.Info("Applying YAML from URL", "url", url)

	// Download the YAML file
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download YAML from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d from URL: %s", resp.StatusCode, url)
	}

	// Create a YAML decoder
	decoder := yaml.NewYAMLOrJSONDecoder(resp.Body, 4096)

	// Loop through all resources in the YAML file
	for {
		// Create an unstructured object to hold the resource
		obj := &unstructured.Unstructured{}
		err := decoder.Decode(obj)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("error decoding YAML: %w", err)
		}

		// Skip empty objects (separators in YAML)
		if len(obj.Object) == 0 {
			continue
		}

		// Apply the resource
		err = createOrUpdateResource(ctx, c, obj)
		if err != nil {
			return fmt.Errorf("failed to apply resource %s/%s: %w",
				obj.GetKind(), obj.GetName(), err)
		}

		logger.Info("Applied resource",
			"kind", obj.GetKind(),
			"name", obj.GetName(),
			"namespace", obj.GetNamespace())
	}

	return nil
}

// createOrUpdateResource creates or updates a Kubernetes resource
func createOrUpdateResource(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	// Check if the resource already exists
	currentObj := obj.DeepCopy()
	key := types.NamespacedName{
		Name:      obj.GetName(),
		Namespace: obj.GetNamespace(),
	}

	err := c.Get(ctx, key, currentObj)
	if err != nil {
		if errors.IsNotFound(err) {
			// Create new resource
			return c.Create(ctx, obj)
		}
		return err
	}

	// Update existing resource
	// First ensure we have the right resource version
	obj.SetResourceVersion(currentObj.GetResourceVersion())
	return c.Update(ctx, obj)
}

// WaitForKubeVirtReady waits for the KubeVirt deployment to be ready
func WaitForKubeVirtReady(ctx context.Context, c client.Client, timeout time.Duration) error {
	logger := log.FromContext(ctx)
	logger.Info("Waiting for KubeVirt to be ready")

	// Create a context with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Check every 5 seconds
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timeout waiting for KubeVirt to be ready")
		case <-ticker.C:
			// Check the KubeVirt CR status
			kubevirt := &unstructured.Unstructured{}
			kubevirt.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "kubevirt.io",
				Version: "v1",
				Kind:    "KubeVirt",
			})

			err := c.Get(ctx, types.NamespacedName{
				Name:      "kubevirt",
				Namespace: "kubevirt",
			}, kubevirt)

			if errors.IsNotFound(err) {
				logger.Info("KubeVirt CR not found yet, continuing to wait")
				continue
			}
			if err != nil {
				logger.Error(err, "Failed to get KubeVirt CR")
				continue
			}

			// Check the phase in the status
			phase, found, err := unstructured.NestedString(kubevirt.Object, "status", "phase")
			if err != nil {
				logger.Error(err, "Failed to get KubeVirt phase")
				continue
			}

			if !found {
				logger.Info("KubeVirt status phase not found, continuing to wait")
				continue
			}

			logger.Info("KubeVirt phase", "phase", phase)
			if phase == "Deployed" {
				logger.Info("KubeVirt is ready")
				return nil
			}
		}
	}
}

// EnsureNamespace ensures a namespace exists
func EnsureNamespace(ctx context.Context, c client.Client, name string) error {
	ns := &corev1.Namespace{}
	err := c.Get(ctx, types.NamespacedName{Name: name}, ns)
	if err == nil {
		// Namespace exists
		return nil
	}

	if !errors.IsNotFound(err) {
		return err
	}

	// Create the namespace
	ns = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	return c.Create(ctx, ns)
}

// GetLatestCDIVersion fetches the latest stable CDI version
func GetLatestCDIVersion(ctx context.Context) (string, error) {
	// Use GitHub API to get the latest release
	url := "https://api.github.com/repos/kubevirt/containerized-data-importer/releases/latest"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// Add User-Agent header to avoid GitHub API limitations
	req.Header.Set("User-Agent", "k4all-operator")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to get latest CDI version: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Parse JSON response
	var release struct {
		TagName string `json:"tag_name"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("failed to decode GitHub API response: %w", err)
	}

	// Remove 'v' prefix if present
	version := release.TagName
	if len(version) > 0 && version[0] == 'v' {
		version = version[1:]
	}

	return version, nil
}

// PatchResource updates a specific resource with a JSON patch
func PatchResource(ctx context.Context, c client.Client,
	gvk schema.GroupVersionKind, name, namespace string, patch []byte) error {

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(name)
	obj.SetNamespace(namespace)

	return c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch))
}

// DeleteYAMLFromURL downloads YAML from a URL and deletes the resources
func DeleteYAMLFromURL(ctx context.Context, c client.Client, url string) error {
	logger := log.FromContext(ctx)
	logger.Info("Deleting resources from URL", "url", url)

	// Get the YAML content
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to get YAML from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to get YAML from %s: status code %d", url, resp.StatusCode)
	}

	// Parse the YAML documents
	decoder := yaml.NewYAMLOrJSONDecoder(resp.Body, 4096)

	count := 0
	for {
		// Create a new unstructured object for each resource
		obj := &unstructured.Unstructured{}
		err := decoder.Decode(obj)

		// If we've reached the end of the input, break
		if err == io.EOF {
			break
		}
		if err != nil {
			logger.Error(err, "Failed to decode YAML document")
			continue // Try the next document
		}

		// Skip empty objects
		if obj.GetAPIVersion() == "" || obj.GetKind() == "" {
			continue
		}

		// Try to delete the resource
		err = c.Delete(ctx, obj, &client.DeleteOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				logger.Info("Resource not found, skipping",
					"kind", obj.GetKind(),
					"name", obj.GetName(),
					"namespace", obj.GetNamespace())
			} else {
				logger.Error(err, "Failed to delete resource",
					"kind", obj.GetKind(),
					"name", obj.GetName(),
					"namespace", obj.GetNamespace())
				// Continue with other resources
			}
		} else {
			logger.Info("Deleted resource",
				"kind", obj.GetKind(),
				"name", obj.GetName(),
				"namespace", obj.GetNamespace())
			count++
		}
	}

	logger.Info("Finished deleting resources", "count", count)
	return nil
}
