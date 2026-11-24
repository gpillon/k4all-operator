package hooks

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/metadata"
)

// The component and the objects of the metadata service (spec §11.2).
const (
	// MetadataComponent is the component of the metadata service.
	MetadataComponent = "k4all-metadata"
	// MetadataPolicy injects the redirect and the server into VMs' pods.
	MetadataPolicy = "k4all-metadata"
	// MetadataVMIPolicy defaults the chassis asset tag of VMIs it serves.
	MetadataVMIPolicy = "k4all-metadata-vmi"

	admissionAPIVersion = "admissionregistration.k8s.io/v1"
	// psaEnforceLabel is PodSecurity's enforced level of a namespace.
	psaEnforceLabel = "pod-security.kubernetes.io/enforce"
)

// Volumes the policy adds to a VM's pod.
const (
	volTokens = "k4all-md-tokens"
	volPod    = "k4all-md-pod"
	volVM     = "k4all-md-vm"
	volVendor = "k4all-md-vendor"
)

// MetadataSettings is spec.features.virt.metadata with the operator's
// defaults filled in.
type MetadataSettings struct {
	// Enabled: virt and metadata are both on.
	Enabled             bool
	Scope               string
	Audiences           []string
	ExpirationSeconds   int64
	VendorDataConfigMap string
}

// MetadataSettingsFrom reads the metadata settings of config.
func MetadataSettingsFrom(config k4allv1alpha1.ClusterConfigSpec) MetadataSettings {
	s := MetadataSettings{
		Scope:             k4allv1alpha1.MetadataScopeLabelled,
		Audiences:         []string{metadata.DefaultAudience},
		ExpirationSeconds: metadata.DefaultExpirationSeconds,
	}
	md := config.Features.Virt.Metadata
	if md == nil {
		return s
	}
	s.Enabled = config.Features.Virt.Enabled && md.Enabled
	if md.Scope != "" {
		s.Scope = md.Scope
	}
	if md.Tokens != nil {
		if len(md.Tokens.Audiences) > 0 {
			s.Audiences = md.Tokens.Audiences
		}
		if md.Tokens.ExpirationSeconds != nil {
			s.ExpirationSeconds = *md.Tokens.ExpirationSeconds
		}
	}
	s.VendorDataConfigMap = md.VendorDataConfigMap
	return s
}

// ServesAll says whether every VM is in scope, but those labelled
// metadata.OptInDisabled.
func (s MetadataSettings) ServesAll() bool {
	return s.Scope == k4allv1alpha1.MetadataScopeAll
}

// metadataObjects are the objects of the k4all-metadata component: the two
// MutatingAdmissionPolicies and their bindings, the pod's before the VMI's.
func metadataObjects(image string, s MetadataSettings) ([]*unstructured.Unstructured, error) {
	if image == "" {
		return nil, fmt.Errorf("the %s component has no image", MetadataComponent)
	}
	return []*unstructured.Unstructured{
		podPolicy(image, s), binding(MetadataPolicy),
		vmiPolicy(s), binding(MetadataVMIPolicy),
	}, nil
}

func k4allLabels() map[string]any {
	return map[string]any{
		"app.kubernetes.io/managed-by": "k4all-operator",
		"app.kubernetes.io/part-of":    "k4all",
		"app.kubernetes.io/component":  "metadata",
	}
}

func admissionObject(kind, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": admissionAPIVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "labels": k4allLabels()},
		"spec":       spec,
	}}
}

func binding(policy string) *unstructured.Unstructured {
	return admissionObject("MutatingAdmissionPolicyBinding", policy, map[string]any{"policyName": policy})
}

// objectSelector picks the objects of the scope: those labelled enabled
// (Labelled), or all but those labelled disabled (All); plus extra labels.
func objectSelector(s MetadataSettings, extra map[string]any) map[string]any {
	labels := map[string]any{}
	for k, v := range extra {
		labels[k] = v
	}
	sel := map[string]any{}
	if s.ServesAll() {
		sel["matchExpressions"] = []any{map[string]any{
			"key": metadata.LabelOptIn, "operator": "NotIn", "values": []any{metadata.OptInDisabled},
		}}
	} else {
		labels[metadata.LabelOptIn] = metadata.OptInEnabled
	}
	if len(labels) > 0 {
		sel["matchLabels"] = labels
	}
	return sel
}

// namespaceSelector leaves out the namespaces whose PodSecurity forbids
// NET_ADMIN (baseline, restricted): the redirect's init container would
// keep the VM from starting there (decision D8: those VMs keep a cloud-init
// disk; the metadata controller puts an Event on them). A selector, not a
// matchCondition: matchConditions get no namespaceObject.
func namespaceSelector() map[string]any {
	return map[string]any{"matchExpressions": []any{map[string]any{
		"key": psaEnforceLabel, "operator": "NotIn", "values": []any{"baseline", "restricted"},
	}}}
}

func matchConstraints(group, resource string, objSel map[string]any) map[string]any {
	return map[string]any{
		"resourceRules": []any{map[string]any{
			"apiGroups": []any{group}, "apiVersions": []any{"v1"},
			"operations": []any{"CREATE"}, "resources": []any{resource},
		}},
		"namespaceSelector": namespaceSelector(),
		"objectSelector":    objSel,
	}
}

func condition(name, expression string) map[string]any {
	return map[string]any{"name": name, "expression": expression}
}

func jsonPatch(expression string) []any {
	return []any{map[string]any{
		"patchType": "JSONPatch",
		"jsonPatch": map[string]any{"expression": expression},
	}}
}

// celString is s as a CEL string literal: Go's quoting is valid CEL.
func celString(s string) string {
	return strconv.Quote(s)
}

// podPolicy injects into each VM pod of the scope, at its CREATE:
//   - an init container that installs the redirect of 169.254.169.254:80
//     (NET_ADMIN only, container_t; it ends well when it fails, so a VM
//     never fails to start because of it);
//   - the server as a native sidecar (uid 107, no capabilities, read-only),
//     with an exec liveness probe only: KubeVirt hands every TCP port of the
//     pod to the guest, and a readiness probe would make the VM's Services
//     follow the server;
//   - the volumes it reads: a projected token per audience, the downward
//     API, the per-VM Secret (optional: the controller renders it) and the
//     vendor-data ConfigMap (optional);
//   - the ServiceAccount named by the VM's label (automount stays off: no
//     API token in the pod or the guest).
//
// Only pods with KubeVirt's masquerade binding (the annotation KubeVirt
// sets for Istio names k6t-eth0), and never twice or over names of its own.
func podPolicy(image string, s MetadataSettings) *unstructured.Unstructured {
	spec := map[string]any{
		"failurePolicy":      "Ignore",
		"reinvocationPolicy": "Never",
		"matchConstraints":   matchConstraints("", "pods", objectSelector(s, map[string]any{"kubevirt.io": "virt-launcher"})),
		"variables": []any{
			map[string]any{"name": "vm", "expression": labelOrEmpty("vm.kubevirt.io/name")},
			map[string]any{"name": "serviceAccount", "expression": labelOrEmpty(metadata.LabelServiceAccount)},
		},
		"matchConditions": []any{
			condition("masquerade-binding", masqueradeAnnotation),
			condition("not-injected-yet",
				"!has(object.spec.initContainers) || !object.spec.initContainers.exists(c, c.name.startsWith('k4all-metadata'))"),
			condition("no-volume-name-clash",
				"!has(object.spec.volumes) || !object.spec.volumes.exists(v, v.name.startsWith('k4all-md-'))"),
		},
		"mutations": jsonPatch(podPatch(image, s)),
	}
	return admissionObject("MutatingAdmissionPolicy", MetadataPolicy, spec)
}

func labelOrEmpty(label string) string {
	return fmt.Sprintf("has(object.metadata.labels) && %[1]s in object.metadata.labels ? object.metadata.labels[%[1]s] : ''",
		celString(label))
}

// masqueradeAnnotation: KubeVirt names the masquerade binding's interface
// in these annotations of the launcher pod (for Istio), and only then.
const masqueradeAnnotation = `has(object.metadata.annotations) &&
(['istio.io/reroute-virtual-interfaces', 'traffic.sidecar.istio.io/kubevirtInterfaces'].exists(a,
  a in object.metadata.annotations && object.metadata.annotations[a].contains('k6t-eth0')))`

// Resources of the injected containers: limits equal to requests, so a
// guaranteed-QoS VM (dedicated CPUs) stays guaranteed. The redirect gets
// more CPU: it runs once, and a tight quota would delay the VM's start.
const (
	redirectResources = `Object.spec.initContainers.resources{
      requests: {"cpu": "100m", "memory": "32Mi"},
      limits: {"cpu": "100m", "memory": "32Mi"}
    }`
	serverResources = `Object.spec.initContainers.resources{
      requests: {"cpu": "10m", "memory": "32Mi"},
      limits: {"cpu": "10m", "memory": "32Mi"}
    }`
)

func podPatch(image string, s MetadataSettings) string {
	img := celString(image)
	vendorMount, vendorVolume := "", ""
	if s.VendorDataConfigMap != "" {
		vendorMount = fmt.Sprintf(` + [
      Object.spec.initContainers.volumeMounts{name: %q, mountPath: %q, readOnly: true}
    ]`, volVendor, metadata.VendorDir)
		vendorVolume = fmt.Sprintf(` + [
  Object.spec.volumes{name: %q, configMap: Object.spec.volumes.configMap{name: %s, optional: true}}
]`, volVendor, celString(s.VendorDataConfigMap))
	}
	tokens := make([]string, 0, len(s.Audiences))
	for _, aud := range s.Audiences {
		tokens = append(tokens, fmt.Sprintf(`
      Object.spec.volumes.projected.sources{serviceAccountToken: Object.spec.volumes.projected.sources.serviceAccountToken{
        audience: %s, expirationSeconds: %d, path: %q
      }}`, celString(aud), s.ExpirationSeconds, metadata.TokenFile(aud)))
	}

	return fmt.Sprintf(`(has(object.spec.initContainers) ? [] : [JSONPatch{op: "add", path: "/spec/initContainers", value: []}]) +
[
  JSONPatch{op: "add", path: "/spec/initContainers/-", value: Object.spec.initContainers{
    name: %[1]q,
    image: %[3]s,
    imagePullPolicy: "IfNotPresent",
    command: [%[4]q, "-redirect"],
    securityContext: Object.spec.initContainers.securityContext{
      runAsUser: 0,
      runAsNonRoot: false,
      allowPrivilegeEscalation: false,
      readOnlyRootFilesystem: true,
      capabilities: Object.spec.initContainers.securityContext.capabilities{add: ["NET_ADMIN"], drop: ["ALL"]},
      seccompProfile: Object.spec.initContainers.securityContext.seccompProfile{type: "RuntimeDefault"}
    },
    resources: %[5]s
  }},
  JSONPatch{op: "add", path: "/spec/initContainers/-", value: Object.spec.initContainers{
    name: %[2]q,
    image: %[3]s,
    imagePullPolicy: "IfNotPresent",
    command: [%[4]q],
    restartPolicy: "Always",
    securityContext: Object.spec.initContainers.securityContext{
      runAsUser: %[7]d,
      runAsGroup: %[7]d,
      runAsNonRoot: true,
      allowPrivilegeEscalation: false,
      readOnlyRootFilesystem: true,
      capabilities: Object.spec.initContainers.securityContext.capabilities{drop: ["ALL"]},
      seccompProfile: Object.spec.initContainers.securityContext.seccompProfile{type: "RuntimeDefault"}
    },
    resources: %[6]s,
    livenessProbe: Object.spec.initContainers.livenessProbe{
      exec: Object.spec.initContainers.livenessProbe.exec{command: [%[4]q, "-probe"]},
      periodSeconds: 60,
      timeoutSeconds: 5,
      failureThreshold: 3
    },
    volumeMounts: [
      Object.spec.initContainers.volumeMounts{name: %[8]q, mountPath: %[11]q, readOnly: true},
      Object.spec.initContainers.volumeMounts{name: %[9]q, mountPath: %[12]q, readOnly: true}
    ] + (variables.vm != "" ? [
      Object.spec.initContainers.volumeMounts{name: %[10]q, mountPath: %[13]q, readOnly: true}
    ] : [])%[14]s
  }}
] +
(has(object.spec.volumes) ? [] : [JSONPatch{op: "add", path: "/spec/volumes", value: []}]) +
([
  Object.spec.volumes{name: %[8]q, projected: Object.spec.volumes.projected{sources: [%[15]s
  ]}},
  Object.spec.volumes{name: %[9]q, downwardAPI: Object.spec.volumes.downwardAPI{items: [
    Object.spec.volumes.downwardAPI.items{path: %[17]q, fieldRef: Object.spec.volumes.downwardAPI.items.fieldRef{fieldPath: "metadata.labels"}},
    Object.spec.volumes.downwardAPI.items{path: %[18]q, fieldRef: Object.spec.volumes.downwardAPI.items.fieldRef{fieldPath: "metadata.annotations"}},
    Object.spec.volumes.downwardAPI.items{path: %[19]q, fieldRef: Object.spec.volumes.downwardAPI.items.fieldRef{fieldPath: "metadata.namespace"}}
  ]}}
] + (variables.vm != "" ? [
  Object.spec.volumes{name: %[10]q, secret: Object.spec.volumes.secret{secretName: %[20]q + variables.vm, optional: true}}
] : [])%[16]s).map(v, JSONPatch{op: "add", path: "/spec/volumes/-", value: v}) +
(variables.serviceAccount != "" ? [JSONPatch{op: "add", path: "/spec/serviceAccountName", value: variables.serviceAccount}] : [])`,
		metadata.RedirectContainer, metadata.ServerContainer, img, metadata.ServerBinary,
		redirectResources, serverResources, metadata.ServerUID,
		volTokens, volPod, volVM, metadata.TokenDir, metadata.PodDir, metadata.VMDir, vendorMount,
		strings.Join(tokens, ","), vendorVolume,
		metadata.PodLabels, metadata.PodAnnotations, metadata.PodNamespace, metadata.SecretPrefix)
}

// vmiPolicy prepares, at their CREATE, the VMIs whose pod the pod policy
// serves (same scope and namespaces, a masquerade interface):
//   - the SMBIOS chassis asset tag "OpenStack Nova", when the VM's spec
//     has none: without it cloud-init never asks an OpenStack metadata
//     service, and with it but no server cloud-init would wait for one. A
//     tag set by the user stays (the metadata controller warns on the VM);
//   - a MAC on the masquerade interface, when it has none and a VM
//     controls the VMI: KubeVirt draws a new MAC at each start, and the
//     guest's network config of its first boot names the MAC, so on a
//     persistent disk the NIC would be down after a restart. The MAC comes
//     from the VM's UID (the VMI's changes at each start): 02 (locally
//     administered, unicast) and the UID's first 5 bytes, random in a
//     UUID. A masquerade NIC is seen by its own pod only: no other host
//     shares its segment. An explicit MAC (Hangar's, KubeMacPool's) is
//     the VM's and stays.
func vmiPolicy(s MetadataSettings) *unstructured.Unstructured {
	spec := map[string]any{
		"failurePolicy":      "Ignore",
		"reinvocationPolicy": "Never",
		"matchConstraints":   matchConstraints("kubevirt.io", "virtualmachineinstances", objectSelector(s, nil)),
		"variables": []any{
			map[string]any{"name": "masquerade",
				"expression": "object.spec.domain.devices.interfaces.filter(i, has(i.masquerade))"},
			map[string]any{"name": "vmUID", "expression": vmControllerUID},
		},
		"matchConditions": []any{
			condition("masquerade-interface", masqueradeInterface),
		},
		"mutations": append(jsonPatch(fmt.Sprintf(`!has(object.spec.domain.chassis)
  ? [JSONPatch{op: "add", path: "/spec/domain/chassis", value: {"asset": %[1]s}}]
  : !has(object.spec.domain.chassis.asset) || object.spec.domain.chassis.asset == ""
  ? [JSONPatch{op: "add", path: "/spec/domain/chassis/asset", value: %[1]s}]
  : []`, celString(metadata.ChassisAsset))), jsonPatch(stableMAC)...),
	}
	return admissionObject("MutatingAdmissionPolicy", MetadataVMIPolicy, spec)
}

// vmControllerUID is the UID of the VirtualMachine that controls the VMI,
// as hex digits ("" for a VMI without one).
const vmControllerUID = `has(object.metadata.ownerReferences) &&
object.metadata.ownerReferences.exists_one(r, r.kind == "VirtualMachine" && r.apiVersion.startsWith("kubevirt.io/") &&
  has(r.controller) && r.controller)
? object.metadata.ownerReferences.filter(r, r.kind == "VirtualMachine" && r.apiVersion.startsWith("kubevirt.io/") &&
    has(r.controller) && r.controller)[0].uid.replace("-", "").lowerAscii()
: ""`

// stableMAC sets the MAC of the VMI's (one) masquerade interface from its
// VM's UID, when the interface has none.
const stableMAC = `size(variables.vmUID) >= 10 &&
(!has(variables.masquerade[0].macAddress) || variables.masquerade[0].macAddress == "")
? [JSONPatch{
    op: "add",
    path: "/spec/domain/devices/interfaces/" +
      string(object.spec.domain.devices.interfaces.indexOf(variables.masquerade[0])) + "/macAddress",
    value: "02:" + [0, 2, 4, 6, 8].map(i, variables.vmUID.substring(i, i + 2)).join(":")
  }]
: []`

// masqueradeInterface: an interface of the VMI has the masquerade binding.
// A VMI that lists none gets KubeVirt's default interface (the binding of
// the KubeVirt CR's defaultNetworkInterface, bridge unless set) after this
// policy runs (webhooks follow policies): it is not tagged.
const masqueradeInterface = `has(object.spec.domain.devices) && has(object.spec.domain.devices.interfaces) &&
object.spec.domain.devices.interfaces.exists(i, has(i.masquerade))`
