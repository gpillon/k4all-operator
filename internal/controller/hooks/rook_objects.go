package hooks

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The Rook objects k4all creates, as the kubone.lab runs them
// (fcos-k8s ai/hangar/lab/06-rook).
const (
	rookNamespace      = "rook-ceph"
	cephClusterName    = "rook-ceph"
	cephAPIVersion     = "ceph.rook.io/v1"
	cephClusterKind    = "CephCluster"
	cephBlockPoolKind  = "CephBlockPool"
	cephDataDirOnHosts = "/var/lib/rook"
)

// cephClusterObject is the CephCluster k4all creates: the counts of the
// plan, the OSD devices of the settings, only aes256k cephx keys
// (CVE-2025-30156; the lab's 12 keys all moved to aes256k with HEALTH_OK),
// and no mgr module list: Rook 1.21 disables the "rook" module on Ceph 20
// whatever the list says.
func cephClusterObject(plan rookPlan, settings RookSettings, image string) *unstructured.Unstructured {
	storage := map[string]interface{}{
		"useAllNodes":   true,
		"useAllDevices": settings.DeviceFilter == "",
	}
	if settings.DeviceFilter != "" {
		storage["deviceFilter"] = settings.DeviceFilter
	}
	resources := func(cpu, memory, limit string) map[string]interface{} {
		return map[string]interface{}{
			"requests": map[string]interface{}{"cpu": cpu, "memory": memory},
			"limits":   map[string]interface{}{"memory": limit},
		}
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": cephAPIVersion,
		"kind":       cephClusterKind,
		"metadata":   map[string]interface{}{"name": cephClusterName, "namespace": rookNamespace},
		"spec": map[string]interface{}{
			"cephVersion":     map[string]interface{}{"image": image},
			"dataDirHostPath": cephDataDirOnHosts,
			"mon": map[string]interface{}{
				"count":                int64(plan.Mons),
				"allowMultiplePerNode": false,
			},
			"mgr": map[string]interface{}{"count": int64(plan.Mgrs)},
			"security": map[string]interface{}{
				"cephx": map[string]interface{}{"allowedCiphers": []interface{}{"aes256k"}},
			},
			"dashboard":      map[string]interface{}{"enabled": true, "ssl": false},
			"crashCollector": map[string]interface{}{"disable": true},
			"monitoring":     map[string]interface{}{"enabled": false},
			"storage":        storage,
			"resources": map[string]interface{}{
				"mon": resources("100m", "512Mi", "1Gi"),
				"mgr": resources("100m", "512Mi", "1Gi"),
				"osd": resources("200m", "1Gi", "2Gi"),
			},
			"priorityClassNames": map[string]interface{}{
				"mon": "system-node-critical",
				"osd": "system-node-critical",
				"mgr": "system-cluster-critical",
			},
			"disruptionManagement": map[string]interface{}{"managePodBudgets": true},
		},
	}}
}

// The pool and classes VMs use on Ceph.
const (
	cephPoolName          = "vm-pool"
	cephStorageClassName  = "ceph-rbd"
	cephSnapshotClassName = "ceph-rbd-snapshot"
	cephRBDDriver         = "rook-ceph.rbd.csi.ceph.com"

	snapshotAPIVersion = "snapshot.storage.k8s.io/v1"
	snapshotClassKind  = "VolumeSnapshotClass"

	// The CSI secrets Rook creates in its namespace for the RBD driver.
	rbdProvisionerSecret = "rook-csi-rbd-provisioner"
	rbdNodeSecret        = "rook-csi-rbd-node"
)

// cephBlockPoolObject is vm-pool: the plan's replicas, one per host. Rook
// refuses a single replica unless requireSafeReplicaSize is off.
func cephBlockPoolObject(plan rookPlan) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": cephAPIVersion,
		"kind":       cephBlockPoolKind,
		"metadata":   map[string]interface{}{"name": cephPoolName, "namespace": rookNamespace},
		"spec": map[string]interface{}{
			"failureDomain": "host",
			"replicated": map[string]interface{}{
				"size":                   int64(plan.PoolSize),
				"requireSafeReplicaSize": plan.PoolSize > 1,
			},
			"parameters": map[string]interface{}{"compression_mode": "none"},
		},
	}}
}

// cephStorageClass is ceph-rbd, on vm-pool. Block volumes of RBD can be
// shared (RWX Block), so VMs on it live-migrate. The image features are all
// those the kernel's RBD client maps since Linux 5.4; rxbounce avoids the
// CRC errors of guests that rewrite a buffer in flight (Windows). Its
// description carries the plan's warning, where the consoles show it.
func cephStorageClass(plan rookPlan) *storagev1.StorageClass {
	replicas := fmt.Sprintf("%d replicas across hosts", plan.PoolSize)
	if plan.PoolSize == 1 {
		replicas = "a single replica"
	}
	description := "Ceph RBD, " + replicas + " (shared: VMs can live-migrate)"
	if plan.Warning != "" {
		description += ". Warning: " + plan.Warning
	}
	expand := true
	reclaim := corev1.PersistentVolumeReclaimDelete
	binding := storagev1.VolumeBindingImmediate
	secrets := map[string]string{
		"provisioner":        rbdProvisionerSecret,
		"controller-expand":  rbdProvisionerSecret,
		"controller-publish": rbdProvisionerSecret,
		"controller-modify":  rbdProvisionerSecret,
		"node-stage":         rbdNodeSecret,
		"node-publish":       rbdNodeSecret,
	}
	params := map[string]string{
		"clusterID":                 rookNamespace,
		"pool":                      cephPoolName,
		"imageFormat":               "2",
		"imageFeatures":             "layering,deep-flatten,exclusive-lock,object-map,fast-diff",
		"mounter":                   "rbd",
		"mapOptions":                "krbd:rxbounce",
		"csi.storage.k8s.io/fstype": "ext4",
	}
	for use, secret := range secrets {
		params["csi.storage.k8s.io/"+use+"-secret-name"] = secret
		params["csi.storage.k8s.io/"+use+"-secret-namespace"] = rookNamespace
	}
	return &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        cephStorageClassName,
			Annotations: map[string]string{"description": description},
		},
		Provisioner:          cephRBDDriver,
		Parameters:           params,
		AllowVolumeExpansion: &expand,
		ReclaimPolicy:        &reclaim,
		VolumeBindingMode:    &binding,
	}
}

// cephSnapshotClass is the VolumeSnapshotClass of ceph-rbd.
func cephSnapshotClass() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": snapshotAPIVersion,
		"kind":       snapshotClassKind,
		"metadata":   map[string]interface{}{"name": cephSnapshotClassName},
		"driver":     cephRBDDriver,
		"parameters": map[string]interface{}{
			"clusterID": rookNamespace,
			"csi.storage.k8s.io/snapshotter-secret-name":      rbdProvisionerSecret,
			"csi.storage.k8s.io/snapshotter-secret-namespace": rookNamespace,
		},
		"deletionPolicy": "Delete",
	}}
}

// cephStorageProfile is CDI's StorageProfile of ceph-rbd: VM disks default
// to its first set, shared block volumes (RWX Block), which live migration
// needs. The second, RWO Filesystem, is for KubeVirt's persistent VM state
// (EFI, TPM) on a vmStateStorageClass ceph-rbd: KubeVirt creates it as a
// Filesystem volume, with an access mode of the profile's Filesystem sets,
// and without one the VM never starts. RBD has no RWX Filesystem; on RWO
// KubeVirt copies the state when the VM migrates.
func cephStorageProfile() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storageProfileAPIVersion,
		"kind":       storageProfileKind,
		"metadata":   map[string]interface{}{"name": cephStorageClassName},
		"spec": map[string]interface{}{
			"claimPropertySets": []interface{}{
				map[string]interface{}{
					"accessModes": []interface{}{"ReadWriteMany"},
					"volumeMode":  "Block",
				},
				map[string]interface{}{
					"accessModes": []interface{}{"ReadWriteOnce"},
					"volumeMode":  "Filesystem",
				},
			},
		},
	}}
}
