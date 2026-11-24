// Package server is the metadata server of one VM: the OpenStack and EC2
// metadata contracts on 169.254.169.254, plus k4all's workload-identity
// tokens. It runs in the VM's virt-launcher pod and serves only that VM, so
// the VM's identity never comes from the client's address. It has no API
// access: everything it serves comes from files the admission policy mounts
// (the downward API, the per-VM Secret, the vendor-data ConfigMap, the
// projected tokens), read again at each request, so a change or a token
// rotation reaches the guest without a restart.
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gpillon/k4all-operator/internal/metadata"
)

// Config says where the server finds its files.
type Config struct {
	// PodDir holds the downward API files (metadata.PodLabels, ...).
	PodDir string
	// VMDir holds the per-VM Secret (metadata.KeyInstanceID, ...).
	VMDir string
	// VendorDir holds the vendor-data ConfigMap (metadata.KeyVendorData).
	VendorDir string
	// TokenDir holds a projected token per audience (metadata.TokenFile).
	TokenDir string
	// ARPFile is the pod's IPv4 neighbour table (/proc/net/arp): the MAC
	// address of the guest NIC that asks.
	ARPFile string
	// Now is the clock of the IMDSv2 sessions (time.Now when nil).
	Now func() time.Time
	// Key signs the IMDSv2 sessions; random when nil.
	Key []byte
	// Logf logs each request; nil logs nothing.
	Logf func(format string, args ...any)
}

// Server serves one VM's metadata.
type Server struct {
	cfg      Config
	sessions *sessions
}

// New returns the server of cfg.
func New(cfg Config) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Server{cfg: cfg, sessions: newSessions(cfg.Key, cfg.Now)}
}

const (
	sessionPath = "/latest/api/token"
	tokenHeader = "X-aws-ec2-metadata-token"
	ttlHeader   = "X-aws-ec2-metadata-token-ttl-seconds"
	// maxSessionTTL is EC2's: 6 hours.
	maxSessionTTL = 21600
)

// openstackVersions are the metadata versions of Nova up to the newest
// cloud-init knows; cloud-init picks the newest it supports.
var openstackVersions = []string{
	"2012-08-10", "2013-04-04", "2013-10-17", "2015-10-15", "2016-06-30",
	"2016-10-06", "2017-02-22", "2018-08-27", "latest",
}

var version = regexp.MustCompile(`^(latest|\d{4}-\d{2}-\d{2})$`)

// networkEveryBoot lets cloud-init write the network config again at every
// boot, not only at the first: KubeVirt gives a VM's NIC a new MAC at each
// start (unless the VM sets one, or KubeMacPool runs), and the config of
// the first boot names the first MAC, so the NIC stays down at the next
// one. The new config comes from network_data.json, which describes the
// NIC that asks. A user-data "updates" replaces it.
const networkEveryBoot = "#cloud-config\nupdates:\n  network:\n    when: [boot-new-instance, boot]\n"

// ServeHTTP answers the guest.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &recorder{ResponseWriter: w, code: http.StatusOK}
	if s.cfg.Logf != nil {
		defer func() {
			s.cfg.Logf("%s %s %d from %s", r.Method, r.URL.Path, rec.code, r.RemoteAddr)
		}()
	}
	s.serve(rec, r)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == sessionPath {
		if r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.newSession(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// The session is optional (IMDSv1: cirros, old cloud-init), but one
	// that is sent must be valid, as on EC2.
	hasSession := false
	if token := r.Header.Get(tokenHeader); token != "" {
		if !s.sessions.valid(token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		hasSession = true
	}
	switch {
	case path == "/":
		text(w, "2009-04-04\nlatest\nopenstack\n")
	case strings.HasPrefix(path, "/k4all/"):
		s.k4all(w, r, hasSession)
	case path == "/openstack" || strings.HasPrefix(path, "/openstack/"):
		s.openstack(w, r, strings.TrimPrefix(strings.TrimPrefix(path, "/openstack"), "/"))
	default:
		s.ec2(w, r, strings.TrimPrefix(path, "/"))
	}
}

// newSession starts an IMDSv2 session.
func (s *Server) newSession(w http.ResponseWriter, r *http.Request) {
	// EC2 refuses a session asked through a proxy: an SSRF in the guest
	// rarely controls the method and headers, a forwarding proxy adds this.
	if r.Header.Get("X-Forwarded-For") != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ttl, err := strconv.Atoi(r.Header.Get(ttlHeader))
	if err != nil || ttl < 1 || ttl > maxSessionTTL {
		http.Error(w, "bad request: "+ttlHeader+" must be 1 to 21600", http.StatusBadRequest)
		return
	}
	w.Header().Set(ttlHeader, strconv.Itoa(ttl))
	text(w, s.sessions.issue(time.Duration(ttl)*time.Second))
}

// k4all serves k4all's own paths: GET /k4all/v1/token?audience=<a>, the
// VM ServiceAccount's token for that audience. It needs an IMDSv2 session
// and the Metadata-Flavor header: two things a browser or an SSRF in the
// guest cannot send.
func (s *Server) k4all(w http.ResponseWriter, r *http.Request, hasSession bool) {
	if r.URL.Path != "/k4all/v1/token" {
		http.NotFound(w, r)
		return
	}
	if !hasSession {
		http.Error(w, "unauthorized: start an IMDSv2 session (PUT "+sessionPath+")", http.StatusUnauthorized)
		return
	}
	if r.Header.Get(metadata.FlavorHeader) != metadata.FlavorValue {
		http.Error(w, "forbidden: "+metadata.FlavorHeader+": "+metadata.FlavorValue+" missing", http.StatusForbidden)
		return
	}
	audience := r.URL.Query().Get("audience")
	if audience == "" {
		http.Error(w, "bad request: audience missing", http.StatusBadRequest)
		return
	}
	token, ok, err := readFile(filepath.Join(s.cfg.TokenDir, metadata.TokenFile(audience)))
	switch {
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
	case !ok:
		http.Error(w, "no token for this audience", http.StatusNotFound)
	default:
		w.Header().Set("Cache-Control", "no-store")
		text(w, string(token))
	}
}

func (s *Server) openstack(w http.ResponseWriter, r *http.Request, rest string) {
	if rest == "" {
		text(w, strings.Join(openstackVersions, "\n")+"\n")
		return
	}
	ver, file, _ := strings.Cut(rest, "/")
	if !version.MatchString(ver) {
		http.NotFound(w, r)
		return
	}
	switch file {
	case "":
		text(w, "meta_data.json\nnetwork_data.json\nuser_data\nvendor_data.json\nvendor_data2.json\n")
		return
	case "network_data.json":
		s.networkData(w, r)
		return
	case "vendor_data2.json":
		// k4all's own vendor-data, beside the admin's vendor_data.json.
		// cloud-init takes the top-level "cloud-init" key.
		jsonOut(w, map[string]string{"cloud-init": networkEveryBoot})
		return
	}
	vm, ok := s.instance(w)
	if !ok {
		return
	}
	switch file {
	case "meta_data.json":
		jsonOut(w, vm.openstack())
	case "user_data":
		if vm.userData == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(vm.userData)
	case "vendor_data.json":
		if vm.vendorData == nil {
			jsonOut(w, struct{}{})
			return
		}
		jsonOut(w, map[string]string{"cloud-init": string(vm.vendorData)})
	default:
		http.NotFound(w, r)
	}
}

// ec2Keys are the meta-data keys served, in the order of the index.
var ec2Keys = []string{
	"ami-id", "ami-launch-index", "hostname", "instance-id", "instance-type",
	"local-hostname", "local-ipv4", "placement/", "public-keys/", "reservation-id",
}

func (s *Server) ec2(w http.ResponseWriter, r *http.Request, path string) {
	ver, rest, _ := strings.Cut(path, "/")
	if !version.MatchString(ver) {
		http.NotFound(w, r)
		return
	}
	vm, ok := s.instance(w)
	if !ok {
		return
	}
	switch rest {
	case "":
		index := "meta-data\n"
		if vm.userData != nil {
			index += "user-data\n"
		}
		text(w, index)
		return
	case "user-data":
		if vm.userData == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(vm.userData)
		return
	case "meta-data", "meta-data/":
		text(w, strings.Join(ec2Keys, "\n")+"\n")
		return
	}
	key, ok := strings.CutPrefix(rest, "meta-data/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if value, ok := vm.ec2(key, clientIP(r)); ok {
		text(w, value)
		return
	}
	http.NotFound(w, r)
}

// networkData describes the NIC that asks as DHCP, from its MAC address in
// the pod's neighbour table (the guest just sent a packet from it). That is
// what cloud-init does without network_data.json; cloudbase-init needs it
// said. Without the MAC nothing is served: an empty network_data.json
// would turn off cloud-init's own DHCP fallback.
func (s *Server) networkData(w http.ResponseWriter, r *http.Request) {
	mac := neighbourMAC(s.cfg.ARPFile, clientIP(r), metadata.GuestInterface)
	if mac == "" {
		http.NotFound(w, r)
		return
	}
	type link struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		MAC  string `json:"ethernet_mac_address"`
	}
	type network struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		Link      string `json:"link"`
		NetworkID string `json:"network_id"`
	}
	jsonOut(w, struct {
		Links    []link    `json:"links"`
		Networks []network `json:"networks"`
		Services []any     `json:"services"`
	}{
		Links:    []link{{ID: "interface0", Type: "phy", MAC: mac}},
		Networks: []network{{ID: "network0", Type: "ipv4_dhcp", Link: "interface0", NetworkID: "network0"}},
		Services: []any{},
	})
}

// instance loads the VM's data, or answers why it cannot.
func (s *Server) instance(w http.ResponseWriter) (*instance, bool) {
	vm, err := s.load()
	switch {
	case errors.Is(err, errNotReady):
		http.Error(w, "the VM's metadata is not ready yet", http.StatusServiceUnavailable)
		return nil, false
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}
	return vm, true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// neighbourMAC finds ip on device in a /proc/net/arp table.
func neighbourMAC(file, ip, device string) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		// IP address, HW type, Flags, HW address, Mask, Device
		if len(f) == 6 && f[0] == ip && f[5] == device && f[3] != "00:00:00:00:00:00" {
			return f[3]
		}
	}
	return ""
}

// readFile reads a file that may be missing: ok false, no error.
func readFile(name string) (data []byte, ok bool, err error) {
	data, err = os.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", name, err)
	}
	return data, true, nil
}

func text(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(s))
}

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// recorder keeps the status code for the log.
type recorder struct {
	http.ResponseWriter
	code int
}

func (r *recorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}
