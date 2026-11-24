package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gpillon/k4all-operator/internal/metadata"
)

const (
	// metadataVMLabel names, on a per-VM Secret, the VM it is for.
	metadataVMLabel = "k4all.magesgate.com/metadata-vm"
	// metadataMessageAnnotation says, on a per-VM Secret, what the VM asks
	// for that is missing (a user-data Secret that is not there), for
	// Hangar's VM page. Absent when nothing is missing.
	metadataMessageAnnotation = "k4all.magesgate.com/metadata-message"
)

// KubeVirt reads the user-data of a cloud-init Secret from either key.
var userDataKeys = []string{"userdata", "userData"}

// renderVMMetadata is what the server serves to vm, from the VM's own spec:
// its UID (the same at every boot), name and hostname, the user-data of its
// cloud-init volume (cloudInitNoCloud or cloudInitConfigDrive: userData,
// userDataBase64 or the Secret of secretRef), the SSH keys of its
// accessCredentials. Secrets it names are read from secrets; one that is
// missing leaves its data out and is said in problems.
func renderVMMetadata(ctx context.Context, secrets client.Reader, vm *unstructured.Unstructured) (map[string][]byte, []string, error) {
	data := map[string][]byte{
		metadata.KeyInstanceID: []byte(vm.GetUID()),
		metadata.KeyName:       []byte(vm.GetName()),
	}
	hostname, _, _ := unstructured.NestedString(vm.Object, "spec", "template", "spec", "hostname")
	if hostname == "" {
		hostname = vm.GetName()
	}
	data[metadata.KeyHostname] = []byte(hostname)

	var problems []string
	userData, problem, err := vmUserData(ctx, secrets, vm)
	if err != nil {
		return nil, nil, err
	}
	if problem != "" {
		problems = append(problems, problem)
	}
	if userData != nil {
		data[metadata.KeyUserData] = userData
	}
	keys, keyProblems, err := vmSSHKeys(ctx, secrets, vm)
	if err != nil {
		return nil, nil, err
	}
	problems = append(problems, keyProblems...)
	if len(keys) > 0 {
		data[metadata.KeySSHKeys] = []byte(strings.Join(keys, "\n") + "\n")
	}
	return data, problems, nil
}

// vmUserData is the user-data of the VM's cloud-init volume (KubeVirt
// allows one), nil without one; as KubeVirt, a secretRef wins over
// userDataBase64, which wins over userData.
func vmUserData(ctx context.Context, secrets client.Reader, vm *unstructured.Unstructured) ([]byte, string, error) {
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	for _, v := range volumes {
		vol, _ := v.(map[string]any)
		for _, kind := range []string{"cloudInitNoCloud", "cloudInitConfigDrive"} {
			src, ok := vol[kind].(map[string]any)
			if !ok {
				continue
			}
			if ref, _, _ := unstructured.NestedString(src, "secretRef", "name"); ref != "" {
				s, err := getSecret(ctx, secrets, vm.GetNamespace(), ref)
				if err != nil || s == nil {
					return nil, fmt.Sprintf("user-data Secret %q not found", ref), err
				}
				for _, key := range userDataKeys {
					if ud, ok := s.Data[key]; ok {
						return ud, "", nil
					}
				}
				return nil, fmt.Sprintf("user-data Secret %q has no userdata key", ref), nil
			}
			if b64, ok := src["userDataBase64"].(string); ok && b64 != "" {
				ud, err := base64.StdEncoding.DecodeString(b64)
				if err != nil {
					return nil, fmt.Sprintf("userDataBase64 of volume %v is not base64", vol["name"]), nil
				}
				return ud, "", nil
			}
			if ud, ok := src["userData"].(string); ok && ud != "" {
				return []byte(ud), "", nil
			}
			return nil, "", nil
		}
	}
	return nil, "", nil
}

// vmSSHKeys are the public keys of the VM's accessCredentials sshPublicKey
// Secrets, whatever their propagation method: every line of every value.
func vmSSHKeys(ctx context.Context, secrets client.Reader, vm *unstructured.Unstructured) ([]string, []string, error) {
	creds, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "accessCredentials")
	var keys, problems []string
	for _, c := range creds {
		cred, _ := c.(map[string]any)
		name, _, _ := unstructured.NestedString(cred, "sshPublicKey", "source", "secret", "secretName")
		if name == "" {
			continue
		}
		s, err := getSecret(ctx, secrets, vm.GetNamespace(), name)
		if err != nil {
			return nil, nil, err
		}
		if s == nil {
			problems = append(problems, fmt.Sprintf("SSH key Secret %q not found", name))
			continue
		}
		names := make([]string, 0, len(s.Data))
		for k := range s.Data {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			for _, line := range strings.Split(string(s.Data[k]), "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "#") {
					keys = append(keys, line)
				}
			}
		}
	}
	return keys, problems, nil
}

// getSecret is the Secret, nil when it is not there.
func getSecret(ctx context.Context, secrets client.Reader, ns, name string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	err := secrets.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, s)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get Secret %s/%s: %w", ns, name, err)
	}
	return s, nil
}

// metadataSecret is the per-VM Secret k4all-md-<vm>, owned by the VM: the
// garbage collector deletes it with the VM.
func metadataSecret(vm *unstructured.Unstructured, data map[string][]byte, problems []string) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      metadata.SecretName(vm.GetName()),
			Namespace: vm.GetNamespace(),
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "k4all-operator",
				"app.kubernetes.io/component":  "metadata",
				metadataVMLabel:                vm.GetName(),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: vmGVK.GroupVersion().String(), Kind: vmGVK.Kind,
				Name: vm.GetName(), UID: vm.GetUID(), Controller: ptr.To(true),
			}},
		},
		Type: metadata.SecretType,
		Data: data,
	}
	if len(problems) > 0 {
		s.Annotations = map[string]string{metadataMessageAnnotation: strings.Join(problems, "; ")}
	}
	return s
}

// isK4allMetadataSecret says whether s is a per-VM Secret k4all made.
func isK4allMetadataSecret(s *corev1.Secret) bool {
	return s.Labels["app.kubernetes.io/managed-by"] == "k4all-operator" && s.Labels[metadataVMLabel] != "" &&
		s.Type == metadata.SecretType
}
