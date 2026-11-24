package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gpillon/k4all-operator/internal/metadata"
)

// vmUID is the UID of the test VM: the instance-id of every boot.
const vmUID = "7f570bd6-4deb-4be7-b03b-cfad6320d6b0"

// fixture is the files the admission policy mounts in a VM's pod.
type fixture struct {
	t                                 *testing.T
	pod, vm, vendor, tokens, arp, dir string
	now                               time.Time
}

// newFixture is the pod of VM "web" in namespace "shop", with its per-VM
// Secret rendered: hostname "web-1", two SSH keys, user-data.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		t: t, dir: dir,
		pod:    filepath.Join(dir, "pod"),
		vm:     filepath.Join(dir, "vm"),
		vendor: filepath.Join(dir, "vendor"),
		tokens: filepath.Join(dir, "tokens"),
		arp:    filepath.Join(dir, "arp"),
		now:    time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC),
	}
	for _, d := range []string{f.pod, f.vm, f.vendor, f.tokens} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.write(f.pod, metadata.PodLabels, strings.Join([]string{
		`kubevirt.io="virt-launcher"`,
		`kubevirt.io/created-by="0fde2733-7537-483a-9d44-c212db9b510c"`,
		`vm.kubevirt.io/name="web"`,
		`vmi.kubevirt.io/id="web"`,
	}, "\n")+"\n")
	f.write(f.pod, metadata.PodAnnotations, `kubevirt.io/domain="web"`+"\n")
	f.write(f.pod, metadata.PodNamespace, "shop")
	f.write(f.vm, metadata.KeyInstanceID, vmUID)
	f.write(f.vm, metadata.KeyName, "web")
	f.write(f.vm, metadata.KeyHostname, "web-1")
	f.write(f.vm, metadata.KeySSHKeys, "ssh-ed25519 AAAAone alice@laptop\nssh-rsa AAAAtwo bob\n")
	f.write(f.vm, metadata.KeyUserData, "#cloud-config\nruncmd: [[touch, /ran]]\n")
	return f
}

func (f *fixture) write(dir, name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) remove(dir, name string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) server() *Server {
	return New(Config{
		PodDir: f.pod, VMDir: f.vm, VendorDir: f.vendor, TokenDir: f.tokens, ARPFile: f.arp,
		Now: func() time.Time { return f.now },
		Key: []byte("test-key-test-key-test-key-12345"),
	})
}

// get asks the server as the guest (10.0.2.2) does.
func get(t *testing.T, s http.Handler, path string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, s, http.MethodGet, path, headers...)
}

func do(t *testing.T, s http.Handler, method, path string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://169.254.169.254"+path, nil)
	r.RemoteAddr = "10.0.2.2:40000"
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// wantBody: 200 with body.
func wantBody(t *testing.T, w *httptest.ResponseRecorder, body string) {
	t.Helper()
	if w.Code != http.StatusOK || w.Body.String() != body {
		t.Fatalf("got %d %q, want 200 %q", w.Code, w.Body.String(), body)
	}
}

func wantCode(t *testing.T, w *httptest.ResponseRecorder, code int) {
	t.Helper()
	if w.Code != code {
		t.Fatalf("got %d %q, want %d", w.Code, w.Body.String(), code)
	}
}

// --- EC2 -------------------------------------------------------------------

// cirros (and anything EC2) refuses an instance-id that does not start with
// "i-"; it is the VM's UID, so every boot of the VM has the same.
func TestEC2_InstanceIDIsTheVMUIDInEC2Form(t *testing.T) {
	s := newFixture(t).server()
	for _, version := range []string{"latest", "2009-04-04", "2021-03-23"} {
		wantBody(t, get(t, s, "/"+version+"/meta-data/instance-id"), "i-7f570bd64deb4be7b")
	}
}

func TestEC2_Indexes(t *testing.T) {
	s := newFixture(t).server()
	wantBody(t, get(t, s, "/"), "2009-04-04\nlatest\nopenstack\n")
	wantBody(t, get(t, s, "/latest/"), "meta-data\nuser-data\n")
	for _, path := range []string{"/latest/meta-data", "/latest/meta-data/"} {
		wantBody(t, get(t, s, path), strings.Join([]string{
			"ami-id", "ami-launch-index", "hostname", "instance-id", "instance-type",
			"local-hostname", "local-ipv4", "placement/", "public-keys/", "reservation-id",
		}, "\n")+"\n")
	}
	wantBody(t, get(t, s, "/latest/meta-data/placement/"), "availability-zone\n")
	wantBody(t, get(t, s, "/latest/meta-data/placement/availability-zone"), "nova")
}

func TestEC2_NamesAndAddress(t *testing.T) {
	s := newFixture(t).server()
	wantBody(t, get(t, s, "/latest/meta-data/hostname"), "web-1")
	wantBody(t, get(t, s, "/latest/meta-data/local-hostname"), "web-1")
	// The guest's own address, as the redirect hands it over: the
	// masquerade binding's guest side (10.0.2.2 unless vmNetworkCIDR moves it).
	wantBody(t, get(t, s, "/latest/meta-data/local-ipv4"), "10.0.2.2")
	wantBody(t, get(t, s, "/latest/meta-data/reservation-id"), "r-shop")
	wantCode(t, get(t, s, "/latest/meta-data/nothing-here"), 404)
}

func TestEC2_PublicKeys(t *testing.T) {
	s := newFixture(t).server()
	wantBody(t, get(t, s, "/latest/meta-data/public-keys/"), "0=key-0\n1=key-1\n")
	wantBody(t, get(t, s, "/latest/meta-data/public-keys/1/"), "openssh-key\n")
	wantBody(t, get(t, s, "/2009-04-04/meta-data/public-keys/0/openssh-key"),
		"ssh-ed25519 AAAAone alice@laptop\n")
	wantBody(t, get(t, s, "/latest/meta-data/public-keys/1/openssh-key"), "ssh-rsa AAAAtwo bob\n")
	wantCode(t, get(t, s, "/latest/meta-data/public-keys/2/openssh-key"), 404)
}

func TestEC2_NoKeysNoKeyIndex(t *testing.T) {
	f := newFixture(t)
	f.remove(f.vm, metadata.KeySSHKeys)
	wantCode(t, get(t, f.server(), "/latest/meta-data/public-keys/"), 404)
}

func TestEC2_UserData(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	wantBody(t, get(t, s, "/latest/user-data"), "#cloud-config\nruncmd: [[touch, /ran]]\n")
	f.remove(f.vm, metadata.KeyUserData)
	wantCode(t, get(t, s, "/latest/user-data"), 404)
	wantBody(t, get(t, s, "/latest/"), "meta-data\n")
}

// --- OpenStack -------------------------------------------------------------

func TestOpenStack_Versions(t *testing.T) {
	s := newFixture(t).server()
	for _, path := range []string{"/openstack", "/openstack/"} {
		w := get(t, s, path)
		wantCode(t, w, 200)
		if !strings.HasSuffix(w.Body.String(), "2018-08-27\nlatest\n") {
			t.Fatalf("%s: %q", path, w.Body.String())
		}
	}
	wantBody(t, get(t, s, "/openstack/latest/"),
		"meta_data.json\nnetwork_data.json\nuser_data\nvendor_data.json\nvendor_data2.json\n")
}

func TestOpenStack_MetaData(t *testing.T) {
	s := newFixture(t).server()
	for _, version := range []string{"latest", "2018-08-27", "2012-08-10"} {
		w := get(t, s, "/openstack/"+version+"/meta_data.json")
		wantCode(t, w, 200)
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("content type %q", ct)
		}
		var md struct {
			UUID       string            `json:"uuid"`
			Name       string            `json:"name"`
			Hostname   string            `json:"hostname"`
			Project    string            `json:"project_id"`
			Zone       string            `json:"availability_zone"`
			PublicKeys map[string]string `json:"public_keys"`
			Keys       []struct {
				Name, Type, Data string
			} `json:"keys"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &md); err != nil {
			t.Fatal(err)
		}
		if md.UUID != vmUID || md.Name != "web" || md.Hostname != "web-1" || md.Project != "shop" || md.Zone != "nova" {
			t.Fatalf("%s: %+v", version, md)
		}
		if md.PublicKeys["key-0"] != "ssh-ed25519 AAAAone alice@laptop" || md.PublicKeys["key-1"] != "ssh-rsa AAAAtwo bob" ||
			len(md.Keys) != 2 || md.Keys[1].Name != "key-1" || md.Keys[1].Type != "ssh" || md.Keys[1].Data != "ssh-rsa AAAAtwo bob" {
			t.Fatalf("keys: %+v", md)
		}
	}
}

func TestOpenStack_UserData(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	wantBody(t, get(t, s, "/openstack/latest/user_data"), "#cloud-config\nruncmd: [[touch, /ran]]\n")
	f.remove(f.vm, metadata.KeyUserData)
	wantCode(t, get(t, s, "/openstack/latest/user_data"), 404)
}

// Vendor-data is cloud-init's: OpenStack hands it over in the "cloud-init"
// key of vendor_data.json.
func TestOpenStack_VendorData(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	wantBody(t, get(t, s, "/openstack/latest/vendor_data.json"), "{}\n")
	f.write(f.vendor, metadata.KeyVendorData, "#cloud-config\npackages: [htop]\n")
	wantBody(t, get(t, s, "/openstack/latest/vendor_data.json"),
		`{"cloud-init":"#cloud-config\npackages: [htop]\n"}`+"\n")
}

// KubeVirt gives a VM's NIC a new MAC at each start (unless the VM sets
// one), and cloud-init pins the MAC in the network config of the first
// boot: at the second boot the NIC is down (VM 970, Ubuntu 24.04 on a
// DataVolume). k4all's own vendor-data (vendor_data2.json, beside the
// admin's) lets cloud-init redo the network config at every boot, from
// network_data.json, which describes the NIC that asks. cloud-init reads
// the top-level "cloud-init" key of vendor_data2.json, not a provider's.
func TestOpenStack_VendorData2RedoesTheNetworkAtEachBoot(t *testing.T) {
	s := newFixture(t).server()
	wantBody(t, get(t, s, "/openstack/latest/vendor_data2.json"),
		`{"cloud-init":"#cloud-config\nupdates:\n  network:\n    when: [boot-new-instance, boot]\n"}`+"\n")
}

// network_data.json describes the NIC that asks (found by its address in
// the pod's neighbour table) as DHCP: what cloud-init does without it, now
// also said to cloudbase-init. Without the NIC it is not served, and
// cloud-init falls back to DHCP on the first NIC.
func TestOpenStack_NetworkDataIsDHCPOnTheAskingNIC(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	wantCode(t, get(t, s, "/openstack/latest/network_data.json"), 404)
	f.write(f.dir, "arp", strings.Join([]string{
		"IP address       HW type     Flags       HW address            Mask     Device",
		"10.100.40.1      0x1         0x2         ee:ee:ee:ee:ee:ee     *        eth0",
		"10.0.2.2         0x1         0x2         02:7a:1c:00:00:01     *        k6t-eth0",
	}, "\n")+"\n")
	w := get(t, s, "/openstack/latest/network_data.json")
	wantBody(t, w, `{"links":[{"id":"interface0","type":"phy","ethernet_mac_address":"02:7a:1c:00:00:01"}],`+
		`"networks":[{"id":"network0","type":"ipv4_dhcp","link":"interface0","network_id":"network0"}],"services":[]}`+"\n")
}

// --- identity sources ------------------------------------------------------

// Rotation and changes of the Secret reach the guest without a restart.
func TestFilesAreReadAtEachRequest(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	wantBody(t, get(t, s, "/latest/meta-data/hostname"), "web-1")
	f.write(f.vm, metadata.KeyHostname, "web-2")
	wantBody(t, get(t, s, "/latest/meta-data/hostname"), "web-2")
}

// The pod of a VM may start before the operator renders the VM's Secret:
// identity waits for it, rather than serving another instance-id (which
// would make cloud-init redo its first boot later).
func TestAVMsPodWithoutItsSecretIsNotReady(t *testing.T) {
	f := newFixture(t)
	f.remove(f.vm, metadata.KeyInstanceID)
	s := f.server()
	for _, path := range []string{"/latest/meta-data/instance-id", "/openstack/latest/meta_data.json", "/latest/user-data"} {
		wantCode(t, get(t, s, path), http.StatusServiceUnavailable)
	}
}

// A VirtualMachineInstance without a VM has no Secret: the VMI's UID is its
// identity for its whole life, its name the hostname.
func TestAStandaloneVMIIsItsOwnInstance(t *testing.T) {
	f := newFixture(t)
	for _, key := range []string{metadata.KeyInstanceID, metadata.KeyName, metadata.KeyHostname,
		metadata.KeySSHKeys, metadata.KeyUserData} {
		f.remove(f.vm, key)
	}
	f.write(f.pod, metadata.PodLabels, `kubevirt.io/created-by="0fde2733-7537-483a-9d44-c212db9b510c"`+"\n"+
		`vmi.kubevirt.io/id="lone"`+"\n")
	f.write(f.pod, metadata.PodAnnotations, `kubevirt.io/domain="lone"`+"\n")
	s := f.server()
	wantBody(t, get(t, s, "/latest/meta-data/instance-id"), "i-0fde27337537483a9")
	wantBody(t, get(t, s, "/latest/meta-data/hostname"), "lone")
	wantCode(t, get(t, s, "/latest/user-data"), 404)
}

func TestDownwardAPIValuesAreUnquoted(t *testing.T) {
	got := parseKV([]byte(`a="x"` + "\n" + `b="with \"quotes\" and \\ \n newline"` + "\n" + "broken\n"))
	if got["a"] != "x" || got["b"] != "with \"quotes\" and \\ \n newline" || len(got) != 2 {
		t.Fatalf("%q", got)
	}
}

// --- IMDSv2 ----------------------------------------------------------------

func session(t *testing.T, s http.Handler, ttl string) string {
	t.Helper()
	w := do(t, s, http.MethodPut, "/latest/api/token", ttlHeader, ttl)
	wantCode(t, w, 200)
	if w.Header().Get(ttlHeader) != ttl {
		t.Fatalf("ttl header %q", w.Header().Get(ttlHeader))
	}
	return w.Body.String()
}

func TestIMDSv2_ASessionNeedsAValidTTL(t *testing.T) {
	s := newFixture(t).server()
	wantCode(t, do(t, s, http.MethodPut, "/latest/api/token"), 400)
	for _, ttl := range []string{"0", "21601", "x"} {
		wantCode(t, do(t, s, http.MethodPut, "/latest/api/token", ttlHeader, ttl), 400)
	}
	session(t, s, "21600")
	wantCode(t, do(t, s, http.MethodGet, "/latest/api/token"), 405)
}

// As on EC2: a PUT that went through a proxy is refused.
func TestIMDSv2_NoSessionThroughAProxy(t *testing.T) {
	s := newFixture(t).server()
	wantCode(t, do(t, s, http.MethodPut, "/latest/api/token", ttlHeader, "60", "X-Forwarded-For", "10.0.2.9"), 403)
}

// The session is optional on the EC2 and OpenStack paths (cirros and old
// cloud-init have none), but one that is sent must be valid.
func TestIMDSv2_ASessionSentMustBeValid(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	wantCode(t, get(t, s, "/latest/meta-data/instance-id"), 200)
	token := session(t, s, "60")
	wantCode(t, get(t, s, "/latest/meta-data/instance-id", tokenHeader, token), 200)
	wantCode(t, get(t, s, "/openstack/latest/meta_data.json", tokenHeader, token), 200)
	wantCode(t, get(t, s, "/latest/meta-data/instance-id", tokenHeader, token+"x"), 401)
	wantCode(t, get(t, s, "/latest/meta-data/instance-id", tokenHeader, "garbage"), 401)
	f.now = f.now.Add(61 * time.Second)
	wantCode(t, get(t, s, "/latest/meta-data/instance-id", tokenHeader, token), 401)
}

// A session of another server (another pod, a restart) is not valid here.
func TestIMDSv2_ASessionIsThisServers(t *testing.T) {
	f := newFixture(t)
	token := session(t, f.server(), "60")
	other := New(Config{PodDir: f.pod, VMDir: f.vm, VendorDir: f.vendor, TokenDir: f.tokens, ARPFile: f.arp,
		Now: func() time.Time { return f.now }})
	wantCode(t, get(t, other, "/latest/meta-data/instance-id", tokenHeader, token), 401)
}

// --- k4all workload identity -------------------------------------------------

func TestK4allToken_ServesTheProjectedTokenOfTheAudience(t *testing.T) {
	f := newFixture(t)
	f.write(f.tokens, metadata.TokenFile("k4all-metadata"), "jwt-one")
	f.write(f.tokens, metadata.TokenFile("https://vault.example.com"), "jwt-vault")
	s := f.server()
	token := session(t, s, "60")
	flavor := []string{tokenHeader, token, metadata.FlavorHeader, metadata.FlavorValue}
	wantBody(t, get(t, s, "/k4all/v1/token?audience=k4all-metadata", flavor...), "jwt-one")
	wantBody(t, get(t, s, "/k4all/v1/token?audience=https%3A%2F%2Fvault.example.com", flavor...), "jwt-vault")
	// kubelet rotated it
	f.write(f.tokens, metadata.TokenFile("k4all-metadata"), "jwt-two")
	wantBody(t, get(t, s, "/k4all/v1/token?audience=k4all-metadata", flavor...), "jwt-two")
	// an audience the ClusterConfig does not list has no projected token
	wantCode(t, get(t, s, "/k4all/v1/token?audience=kubernetes", flavor...), 404)
	wantCode(t, get(t, s, "/k4all/v1/token", flavor...), 400)
	wantCode(t, do(t, s, http.MethodPost, "/k4all/v1/token?audience=k4all-metadata", flavor...), 405)
}

func TestK4allToken_NeedsASessionAndTheFlavorHeader(t *testing.T) {
	f := newFixture(t)
	f.write(f.tokens, metadata.TokenFile("k4all-metadata"), "jwt-one")
	s := f.server()
	path := "/k4all/v1/token?audience=k4all-metadata"
	wantCode(t, get(t, s, path), 401)
	wantCode(t, get(t, s, path, metadata.FlavorHeader, metadata.FlavorValue), 401)
	token := session(t, s, "60")
	wantCode(t, get(t, s, path, tokenHeader, token), 403)
	wantCode(t, get(t, s, path, tokenHeader, token, metadata.FlavorHeader, "Google"), 403)
	wantCode(t, get(t, s, path, tokenHeader, token, metadata.FlavorHeader, metadata.FlavorValue), 200)
}

func TestOnlyReads(t *testing.T) {
	s := newFixture(t).server()
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut} {
		wantCode(t, do(t, s, method, "/latest/meta-data/instance-id"), 405)
	}
	if w := do(t, s, http.MethodHead, "/latest/meta-data/instance-id"); w.Code != 200 {
		t.Fatalf("HEAD: %d", w.Code)
	}
}

func TestEC2IDIsShortAndHex(t *testing.T) {
	for uid, want := range map[string]string{
		vmUID: "i-7f570bd64deb4be7b",
		"abc": "i-abc",
	} {
		if got := ec2ID(uid); got != want {
			t.Errorf("ec2ID(%q) = %q, want %q", uid, got, want)
		}
	}
	if len(ec2ID(vmUID)) != 2+17 {
		t.Fatal(strconv.Itoa(len(ec2ID(vmUID))))
	}
}
