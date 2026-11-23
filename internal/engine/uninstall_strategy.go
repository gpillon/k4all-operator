package engine

// BlockUninstallIfWorkloadsExist is the uninstallStrategy of the KubeVirt and
// CDI CRs that k4all wants: their operators then refuse the deletion of the
// CR while VMs (KubeVirt) or DataVolumes (CDI) exist. Unset, as in
// upstream's kubevirt-cr.yaml and cdi-cr.yaml, the deletion is allowed and
// takes them with it: virt-operator v1.9.1 deletes its CRDs first, and the
// garbage collector deletes CDI v1.66.1's, owned by the CDI CR.
const BlockUninstallIfWorkloadsExist = "BlockUninstallIfWorkloadsExist"
