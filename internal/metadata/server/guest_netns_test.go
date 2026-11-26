//go:build linux

package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/nftables"
	"golang.org/x/sys/unix"

	"github.com/gpillon/k4all-operator/internal/metadata"
	"github.com/gpillon/k4all-operator/internal/metadata/redirect"
)

// inNS runs fn on a thread in the network namespace fd: sockets fn opens
// stay there. The thread goes back to the test's namespace, or, if it
// cannot, ends with its goroutine.
func inNS(t *testing.T, fd int, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		orig, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			runtime.UnlockOSThread()
			t.Error(err)
			return
		}
		defer func() { _ = unix.Close(orig) }()
		if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			t.Error(err)
			return
		}
		fn()
		if unix.Setns(orig, unix.CLONE_NEWNET) == nil {
			runtime.UnlockOSThread()
		}
	}()
	<-done
}

// netns is a named network namespace (ip netns), deleted after the test.
func netns(t *testing.T, name string) int {
	t.Helper()
	ip(t, "netns", "add", name)
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", name).Run() })
	fd, err := unix.Open(filepath.Join("/run/netns", name), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

func ip(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
		t.Fatalf("ip %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// guestPod is a VM's pod and its guest as KubeVirt's masquerade binding
// wires them, with the VM network cidr: the pod holds the bridge k6t-eth0
// with the network's first address, the guest the second, its default
// route through the pod. A veth stands for the bridge and the tap.
func guestPod(t *testing.T, cidr string) (pod, guest int, guestIP, guestMAC string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: network namespaces")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("needs iproute2")
	}
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatal(err)
	}
	base := n.IP.To4()
	gw := fmt.Sprintf("%d.%d.%d.%d", base[0], base[1], base[2], base[3]+1)
	guestIP = fmt.Sprintf("%d.%d.%d.%d", base[0], base[1], base[2], base[3]+2)
	ones, _ := n.Mask.Size()
	id := fmt.Sprintf("%d", os.Getpid()%100000)
	podNS, guestNS := "k4md-pod-"+id, "k4md-guest-"+id
	pod, guest = netns(t, podNS), netns(t, guestNS)
	ip(t, "link", "add", "k4mdb"+id, "type", "veth", "peer", "name", "k4mdg"+id)
	ip(t, "link", "set", "k4mdb"+id, "netns", podNS)
	ip(t, "-n", podNS, "link", "set", "k4mdb"+id, "name", metadata.GuestInterface)
	ip(t, "link", "set", "k4mdg"+id, "netns", guestNS)
	ip(t, "-n", podNS, "addr", "add", fmt.Sprintf("%s/%d", gw, ones), "dev", metadata.GuestInterface)
	ip(t, "-n", guestNS, "addr", "add", fmt.Sprintf("%s/%d", guestIP, ones), "dev", "k4mdg"+id)
	for _, args := range [][]string{
		{"-n", podNS, "link", "set", metadata.GuestInterface, "up"}, {"-n", podNS, "link", "set", "lo", "up"},
		{"-n", guestNS, "link", "set", "k4mdg" + id, "up"}, {"-n", guestNS, "link", "set", "lo", "up"},
		{"-n", guestNS, "route", "add", "default", "via", gw},
	} {
		ip(t, args...)
	}
	inNS(t, guest, func() {
		iface, err := net.InterfaceByName("k4mdg" + id)
		if err == nil {
			guestMAC = iface.HardwareAddr.String()
		}
	})
	return pod, guest, guestIP, guestMAC
}

// The redirect matches the guest's interface, not KubeVirt's default VM
// network (10.0.2.0/24): Hangar gives VMs another vmNetworkCIDR where pods
// use 10.0.2.0/24 (Cilium, #72), and those VMs are served the same.
func TestGuest_AVMNetworkOtherThanKubeVirtsDefaultIsServed(t *testing.T) {
	pod, guest, guestIP, guestMAC := guestPod(t, "172.31.254.0/24")
	c, err := nftables.New(nftables.WithNetNSFd(pod))
	if err != nil {
		t.Fatal(err)
	}
	if err := redirect.Install(c, metadata.Port); err != nil {
		t.Skipf("no nftables in this kernel: %v", err)
	}
	var l net.Listener
	inNS(t, pod, func() { l, err = net.Listen("tcp", fmt.Sprintf(":%d", metadata.Port)) })
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	srv := &http.Server{Handler: f.server(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(RedirectedOnly(l, OriginalDestination, t.Logf)) }()
	t.Cleanup(func() { _ = srv.Close() })

	dial := func(ctx context.Context, network, addr string) (conn net.Conn, err error) {
		inNS(t, guest, func() { conn, err = (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, addr) })
		return conn, err
	}
	guestClient := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: dial}}
	ask := func(path string) string {
		t.Helper()
		resp, err := guestClient.Get("http://" + metadata.Address + path)
		if err != nil {
			t.Fatalf("GET %s from the guest: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %s %s", path, resp.Status, body)
		}
		return string(body)
	}

	if got := ask("/latest/meta-data/local-ipv4"); got != guestIP {
		t.Fatalf("local-ipv4 %q, want the guest's %s", got, guestIP)
	}
	if got := ask("/latest/meta-data/instance-id"); got != "i-7f570bd64deb4be7b" {
		t.Fatalf("instance-id %q", got)
	}
	// network_data.json finds the guest's NIC in the pod's neighbour table
	// (the server reads /proc/net/arp in its own namespace in a pod).
	var arp []byte
	inNS(t, pod, func() { arp, err = os.ReadFile("/proc/thread-self/net/arp") })
	if err != nil {
		t.Fatal(err)
	}
	f.write(f.dir, "arp", string(arp))
	if got := ask("/openstack/latest/network_data.json"); !strings.Contains(got, `"ethernet_mac_address":"`+guestMAC+`"`) {
		t.Fatalf("network_data.json %s, want the guest's MAC %s", got, guestMAC)
	}

	// The server's own port, asked directly from the guest, is closed.
	conn, err := dial(context.Background(), "tcp", fmt.Sprintf("172.31.254.1:%d", metadata.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write([]byte("GET /latest/meta-data/instance-id HTTP/1.0\r\n\r\n"))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := conn.Read(make([]byte, 64)); err == nil {
		t.Fatalf("the server answered a direct connection (%d bytes)", n)
	}
}
