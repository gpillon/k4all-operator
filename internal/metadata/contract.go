// Package metadata is what the parts of the k4all metadata service agree
// on: the operator's admission policy mounts files into each VM's
// virt-launcher pod where the server reads them, and the operator's
// metadata controller writes the per-VM Secret with the keys the server
// knows.
package metadata

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	// Address is the address guests ask, as on OpenStack and EC2.
	Address = "169.254.169.254"
	// GuestPort is the port guests ask on Address.
	GuestPort = 80
	// Port is where the server listens in the pod; the redirect sends the
	// guest's Address:GuestPort there.
	Port = 8775
	// GuestInterface is the bridge KubeVirt's masquerade binding puts
	// between the pod and the guest: guest traffic comes in on it.
	GuestInterface = "k6t-eth0"
	// NFTTable is the nftables table (family ip) of the redirect, in the
	// pod's network namespace.
	NFTTable = "k4all_metadata"

	// LabelOptIn on a VM template asks for the service (scope Labelled).
	LabelOptIn = "k4all.magesgate.com/metadata"
	// OptInEnabled is the value of LabelOptIn that asks for it.
	OptInEnabled = "enabled"
	// OptInDisabled is the value of LabelOptIn that keeps a VM out of
	// scope All.
	OptInDisabled = "disabled"
	// LabelServiceAccount on a VM template names the ServiceAccount whose
	// tokens the guest gets (default: the namespace's default).
	LabelServiceAccount = "k4all.magesgate.com/service-account"
	// ChassisAsset is the SMBIOS chassis asset tag cloud-init's ds-identify
	// needs to look for an OpenStack metadata service.
	ChassisAsset = "OpenStack Nova"

	// DefaultAudience is the audience of the token when the ClusterConfig
	// names none.
	DefaultAudience = "k4all-metadata"
	// DefaultExpirationSeconds is the token lifetime when the ClusterConfig
	// sets none.
	DefaultExpirationSeconds int64 = 3600

	// ServerContainer is the server's container (a native sidecar).
	ServerContainer = "k4all-metadata"
	// RedirectContainer is the init container that installs the redirect.
	RedirectContainer = "k4all-metadata-redirect"
	// ServerBinary is the server's path in its image.
	ServerBinary = "/metadata-server"
	// ServerUID runs the server: qemu's uid, as virt-launcher.
	ServerUID int64 = 107

	// TokenDir holds one projected token per audience, named TokenFile.
	TokenDir = "/var/run/k4all/metadata/tokens"
	// PodDir holds the pod's downward API files.
	PodDir = "/etc/k4all-md/pod"
	// VMDir holds the per-VM Secret.
	VMDir = "/etc/k4all-md/vm"
	// VendorDir holds the vendor-data ConfigMap.
	VendorDir = "/etc/k4all-md/vendor"

	// Files of PodDir.
	PodLabels      = "labels"
	PodAnnotations = "annotations"
	PodNamespace   = "namespace"

	// SecretPrefix starts the name of the per-VM Secret.
	SecretPrefix = "k4all-md-"
	// SecretType is the type of the per-VM Secret.
	SecretType = "k4all.magesgate.com/metadata"
	// Keys of the per-VM Secret.
	KeyInstanceID = "instance-id"
	KeyName       = "name"
	KeyHostname   = "hostname"
	KeyUserData   = "user-data"
	KeySSHKeys    = "ssh-authorized-keys"
	// KeyVendorData is the key of the vendor-data ConfigMap: cloud-init
	// vendor-data (a #cloud-config or a script).
	KeyVendorData = "vendor-data"

	// FlavorHeader must be FlavorValue on the k4all paths, so a browser or
	// a proxy in the guest cannot be made to fetch a token.
	FlavorHeader = "Metadata-Flavor"
	FlavorValue  = "k4all"
)

// TokenFile is the file of TokenDir with the token of audience: a hash, as
// an audience may hold any character (a URL).
func TokenFile(audience string) string {
	sum := sha256.Sum256([]byte(audience))
	return hex.EncodeToString(sum[:])
}

// SecretName is the name of the per-VM Secret of the VM vm.
func SecretName(vm string) string {
	return SecretPrefix + vm
}
