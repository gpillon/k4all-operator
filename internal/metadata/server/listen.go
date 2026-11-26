package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/gpillon/k4all-operator/internal/metadata"
)

// guestAddrPort is what the guest asks: a connection whose original
// destination it is came through the redirect from the guest's interface.
var guestAddrPort = netip.AddrPortFrom(netip.MustParseAddr(metadata.Address), metadata.GuestPort)

// RedirectedOnly accepts only the connections the redirect brought, those
// whose original destination (before the NAT, origDst) is
// 169.254.169.254:80: the redirect matches only packets from the guest's
// interface. Any other (the pod network, another container of the pod, the
// port reached after a failed redirect) is closed unread.
func RedirectedOnly(l net.Listener, origDst func(net.Conn) (netip.AddrPort, error),
	logf func(format string, args ...any)) net.Listener {
	return &redirectedOnly{Listener: l, origDst: origDst, logf: logf}
}

type redirectedOnly struct {
	net.Listener
	origDst func(net.Conn) (netip.AddrPort, error)
	logf    func(format string, args ...any)
}

func (l *redirectedOnly) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		dst, err := l.origDst(c)
		if err == nil && dst == guestAddrPort {
			return c, nil
		}
		l.logf("closed the connection from %s: not redirected from %s (original destination %v, %v)",
			c.RemoteAddr(), guestAddrPort, dst, err)
		_ = c.Close()
	}
}

// HealthSocket is the abstract unix socket the probe asks. KubeVirt's
// masquerade binding sends the pod's TCP ports, 127.0.0.1 included, to the
// guest: a TCP probe of the server would reach the guest instead.
const HealthSocket = "@k4all-metadata-health"

// ServeHealth answers /healthz on the abstract unix socket name, until the
// listener it returns is closed.
func ServeHealth(name string) (net.Listener, error) {
	l, err := net.Listen("unix", name)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		text(w, "ok\n")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(l) }()
	return l, nil
}

// Probe asks the health socket name: nil when the server answers.
func Probe(name string, timeout time.Duration) error {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", name)
			},
		},
	}
	resp, err := client.Get("http://k4all-metadata/healthz")
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: %s", resp.Status)
	}
	return nil
}
