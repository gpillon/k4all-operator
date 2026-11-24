//go:build !linux

package server

import (
	"errors"
	"net"
	"net/netip"
)

// OriginalDestination needs Linux's conntrack: elsewhere no connection is
// taken for the guest's.
func OriginalDestination(net.Conn) (netip.AddrPort, error) {
	return netip.AddrPort{}, errors.New("original destinations are known on Linux only")
}
