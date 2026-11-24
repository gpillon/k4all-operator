package server

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
)

// A connection the redirect did not bring (from the pod network, from
// another container of the pod, after a failed redirect) is closed unread:
// only the guest, asking 169.254.169.254:80, is served.
func TestRedirectedOnly_ClosesWhatTheRedirectDidNotBring(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dsts := make(chan netip.AddrPort, 3)
	dsts <- netip.MustParseAddrPort("10.100.40.38:8775") // pod IP
	dsts <- netip.AddrPort{}                             // no conntrack entry: error below
	dsts <- netip.MustParseAddrPort("169.254.169.254:80")
	l := RedirectedOnly(inner, func(net.Conn) (netip.AddrPort, error) {
		d := <-dsts
		if !d.IsValid() {
			return d, errors.New("no original destination")
		}
		return d, nil
	}, func(string, ...any) {})
	defer func() { _ = l.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	var clients []net.Conn
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
		defer func() { _ = c.Close() }()
	}
	for i, c := range clients[:2] {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("connection %d: read = %v, want EOF (closed)", i, err)
		}
	}
	select {
	case c := <-accepted:
		_ = c.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the redirected connection was not accepted")
	}
}

// Without NAT the kernel has no other destination than the real one: a
// plain connection to the server's port is never taken for the guest's.
func TestOriginalDestination_OfAPlainConnectionIsNotTheGuests(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		c, err := net.Dial("tcp", l.Addr().String())
		if err == nil {
			time.Sleep(time.Second)
			_ = c.Close()
		}
	}()
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	dst, err := OriginalDestination(c)
	if err == nil && dst == guestAddrPort {
		t.Fatalf("plain connection taken for the guest's: %v", dst)
	}
}

func TestProbe_AsksTheHealthSocket(t *testing.T) {
	name := fmt.Sprintf("@k4all-metadata-test-%d", time.Now().UnixNano())
	if err := Probe(name, time.Second); err == nil {
		t.Fatal("probe without a server succeeded")
	}
	l, err := ServeHealth(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := Probe(name, time.Second); err != nil {
		t.Fatalf("probe: %v", err)
	}
	_ = l.Close()
	if err := Probe(name, time.Second); err == nil {
		t.Fatal("probe after the server stopped succeeded")
	}
}
