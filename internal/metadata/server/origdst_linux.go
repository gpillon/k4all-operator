package server

import (
	"errors"
	"net"
	"net/netip"

	"golang.org/x/sys/unix"
)

// OriginalDestination is the destination of the TCP connection c before
// the NAT (SO_ORIGINAL_DST), as conntrack recorded it.
func OriginalDestination(c net.Conn) (netip.AddrPort, error) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return netip.AddrPort{}, errors.New("not a TCP connection")
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var dst netip.AddrPort
	var sockErr error
	err = raw.Control(func(fd uintptr) {
		// A sockaddr_in in the first bytes: family, port, address. The
		// IPv6Mreq getter is the usual way to read it without cgo.
		mreq, err := unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, unix.SO_ORIGINAL_DST)
		if err != nil {
			sockErr = err
			return
		}
		a := mreq.Multiaddr
		dst = netip.AddrPortFrom(netip.AddrFrom4([4]byte{a[4], a[5], a[6], a[7]}), uint16(a[2])<<8|uint16(a[3]))
	})
	if err != nil {
		return netip.AddrPort{}, err
	}
	return dst, sockErr
}
