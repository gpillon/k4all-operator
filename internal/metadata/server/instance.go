package server

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gpillon/k4all-operator/internal/metadata"
)

// errNotReady: the VM's pod runs before the operator rendered its Secret.
var errNotReady = errors.New("the VM's metadata is not rendered yet")

// Labels KubeVirt puts on a virt-launcher pod.
const (
	labelVMName   = "vm.kubevirt.io/name"
	labelVMIName  = "vmi.kubevirt.io/id"
	labelVMIUID   = "kubevirt.io/created-by"
	annotationDom = "kubevirt.io/domain"
)

// instance is what the server knows of its VM at one request.
type instance struct {
	// id is the VM's UID: the same at every boot of the VM, so cloud-init
	// runs its first-boot modules (SSH host keys) once.
	id, name, hostname, namespace string
	keys                          []string
	// userData and vendorData are nil when there are none.
	userData, vendorData []byte
}

// load reads the VM's files. The per-VM Secret is the identity of a VM; a
// VMI without a VM (no Secret: the controller renders VMs only) is its own
// instance, with the VMI's UID.
func (s *Server) load() (*instance, error) {
	labels, err := s.kv(s.cfg.PodDir, metadata.PodLabels)
	if err != nil {
		return nil, err
	}
	annotations, err := s.kv(s.cfg.PodDir, metadata.PodAnnotations)
	if err != nil {
		return nil, err
	}
	vm := &instance{}
	if vm.namespace, err = s.text(s.cfg.PodDir, metadata.PodNamespace); err != nil {
		return nil, err
	}
	if vm.id, err = s.text(s.cfg.VMDir, metadata.KeyInstanceID); err != nil {
		return nil, err
	}
	if vm.id == "" {
		// A VM's pod waits for the Secret: another id now would be a
		// second first boot later.
		if labels[labelVMName] != "" || labels[labelVMIUID] == "" {
			return nil, errNotReady
		}
		vm.id = labels[labelVMIUID]
	}
	if vm.name, err = s.text(s.cfg.VMDir, metadata.KeyName); err != nil {
		return nil, err
	}
	for _, name := range []string{labels[labelVMName], labels[labelVMIName], annotations[annotationDom]} {
		if vm.name == "" {
			vm.name = name
		}
	}
	if vm.hostname, err = s.text(s.cfg.VMDir, metadata.KeyHostname); err != nil {
		return nil, err
	}
	if vm.hostname == "" {
		vm.hostname = vm.name
	}
	keys, _, err := readFile(filepath.Join(s.cfg.VMDir, metadata.KeySSHKeys))
	if err != nil {
		return nil, err
	}
	vm.keys = authorizedKeys(keys)
	if vm.userData, err = s.blob(s.cfg.VMDir, metadata.KeyUserData); err != nil {
		return nil, err
	}
	if vm.vendorData, err = s.blob(s.cfg.VendorDir, metadata.KeyVendorData); err != nil {
		return nil, err
	}
	return vm, nil
}

func (s *Server) text(dir, name string) (string, error) {
	data, _, err := readFile(filepath.Join(dir, name))
	return strings.TrimSpace(string(data)), err
}

// blob is a file's content, nil when it is missing or empty.
func (s *Server) blob(dir, name string) ([]byte, error) {
	data, ok, err := readFile(filepath.Join(dir, name))
	if err != nil || !ok || len(data) == 0 {
		return nil, err
	}
	return data, nil
}

func (s *Server) kv(dir, name string) (map[string]string, error) {
	data, _, err := readFile(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	return parseKV(data), nil
}

// parseKV reads a downward API labels or annotations file: key="value" per
// line, the value quoted as a Go string.
func parseKV(data []byte) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		if u, err := strconv.Unquote(v); err == nil {
			v = u
		}
		m[k] = v
	}
	return m
}

// authorizedKeys are the keys of an authorized_keys text, one per line.
func authorizedKeys(data []byte) []string {
	var keys []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			keys = append(keys, line)
		}
	}
	return keys
}

func keyName(i int) string {
	return fmt.Sprintf("key-%d", i)
}

// ec2ID is uid as an EC2 instance id: "i-" and 17 hex digits (cirros
// refuses one that does not start with "i-").
func ec2ID(uid string) string {
	hex := strings.ReplaceAll(uid, "-", "")
	if len(hex) > 17 {
		hex = hex[:17]
	}
	return "i-" + hex
}

// openstackKey is an entry of meta_data.json's keys.
type openstackKey struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Data string `json:"data"`
}

// openstackMeta is OpenStack's meta_data.json, the fields cloud-init and
// cloudbase-init read.
type openstackMeta struct {
	UUID             string            `json:"uuid"`
	Name             string            `json:"name"`
	Hostname         string            `json:"hostname"`
	LaunchIndex      int               `json:"launch_index"`
	AvailabilityZone string            `json:"availability_zone"`
	ProjectID        string            `json:"project_id"`
	PublicKeys       map[string]string `json:"public_keys"`
	Keys             []openstackKey    `json:"keys"`
	Meta             map[string]string `json:"meta"`
	Devices          []any             `json:"devices"`
}

// zone is the availability zone of every instance: Nova's default.
const zone = "nova"

func (vm *instance) openstack() openstackMeta {
	md := openstackMeta{
		UUID: vm.id, Name: vm.name, Hostname: vm.hostname, AvailabilityZone: zone, ProjectID: vm.namespace,
		PublicKeys: map[string]string{}, Keys: []openstackKey{}, Meta: map[string]string{}, Devices: []any{},
	}
	for i, key := range vm.keys {
		md.PublicKeys[keyName(i)] = key
		md.Keys = append(md.Keys, openstackKey{Name: keyName(i), Type: "ssh", Data: key})
	}
	return md
}

// ec2 is the value of the meta-data key, for the guest at address ip.
func (vm *instance) ec2(key, ip string) (string, bool) {
	switch key {
	case "ami-id":
		return "ami-00000000", true
	case "ami-launch-index":
		return "0", true
	case "hostname", "local-hostname":
		return vm.hostname, true
	case "instance-id":
		return ec2ID(vm.id), true
	case "instance-type":
		return "kubevirt", true
	case "local-ipv4":
		return ip, true
	case "placement", "placement/":
		return "availability-zone\n", true
	case "placement/availability-zone":
		return zone, true
	case "reservation-id":
		return "r-" + vm.namespace, true
	}
	rest, ok := strings.CutPrefix(key, "public-keys")
	if !ok || len(vm.keys) == 0 {
		return "", false
	}
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		var b strings.Builder
		for i := range vm.keys {
			fmt.Fprintf(&b, "%d=%s\n", i, keyName(i))
		}
		return b.String(), true
	}
	n, file, _ := strings.Cut(rest, "/")
	i, err := strconv.Atoi(n)
	if err != nil || i < 0 || i >= len(vm.keys) {
		return "", false
	}
	switch file {
	case "":
		return "openssh-key\n", true
	case "openssh-key":
		return vm.keys[i] + "\n", true
	}
	return "", false
}
