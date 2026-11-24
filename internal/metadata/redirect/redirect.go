// Package redirect installs, in a VM's virt-launcher pod network namespace,
// the nftables rules that hand the guest's 169.254.169.254:80 to the
// metadata server: the init container of the admission policy runs it,
// with NET_ADMIN only (container_t is enough under SELinux).
package redirect

import (
	"encoding/binary"
	"net"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"

	"github.com/gpillon/k4all-operator/internal/metadata"
)

// Install replaces the table metadata.NFTTable (family ip) in c's network
// namespace with:
//
//	table ip k4all_metadata {
//	  chain prerouting { type nat hook prerouting priority dstnat - 10; policy accept;
//	    iifname "k6t-eth0" ip daddr 169.254.169.254 tcp dport 80 counter redirect to :<port> }
//	  chain input { type filter hook input priority filter - 10; policy accept;
//	    iifname != { "k6t-eth0", "lo" } tcp dport <port> counter drop }
//	}
//
// The prerouting chain runs ahead of KubeVirt's masquerade (priority
// dstnat), which would otherwise send the guest's packet out through the
// node: on a node that is itself a cloud VM, to the node's own metadata.
// The input chain keeps the pod network away from the server's port. One
// transaction: the table is never half there, and a second run replaces it.
func Install(c *nftables.Conn, port uint16) error {
	table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: metadata.NFTTable}
	// Adding a table that exists is a no-op: add, delete, add replaces it.
	c.AddTable(table)
	c.DelTable(table)
	c.AddTable(table)

	accept := nftables.ChainPolicyAccept
	prerouting := c.AddChain(&nftables.Chain{
		Name: "prerouting", Table: table, Type: nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityRef(*nftables.ChainPriorityNATDest - 10),
		Policy:   &accept,
	})
	c.AddRule(&nftables.Rule{Table: table, Chain: prerouting, Exprs: redirectRule(port)})

	input := c.AddChain(&nftables.Chain{
		Name: "input", Table: table, Type: nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookInput,
		Priority: nftables.ChainPriorityRef(*nftables.ChainPriorityFilter - 10),
		Policy:   &accept,
	})
	c.AddRule(&nftables.Rule{Table: table, Chain: input, Exprs: dropRule(port)})
	return c.Flush()
}

// redirectRule: iifname "k6t-eth0" ip daddr 169.254.169.254 tcp dport 80
// counter redirect to :port.
func redirectRule(port uint16) []expr.Any {
	exprs := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(metadata.GuestInterface)},
		// ip daddr: 4 bytes at offset 16 of the IPv4 header
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: net.ParseIP(metadata.Address).To4()},
	}
	exprs = append(exprs, tcpDport(metadata.GuestPort)...)
	return append(exprs,
		&expr.Counter{},
		&expr.Immediate{Register: 1, Data: be16(port)},
		&expr.Redir{RegisterProtoMin: 1},
	)
}

// dropRule: iifname != { "k6t-eth0", "lo" } tcp dport port counter drop.
func dropRule(port uint16) []expr.Any {
	exprs := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: ifname(metadata.GuestInterface)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: ifname("lo")},
	}
	exprs = append(exprs, tcpDport(port)...)
	return append(exprs, &expr.Counter{}, &expr.Verdict{Kind: expr.VerdictDrop})
}

// tcpDport matches TCP packets to port.
func tcpDport(port uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
		// tcp dport: 2 bytes at offset 2 of the TCP header
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: be16(port)},
	}
}

// ifname is an interface name as nftables compares it: IFNAMSIZ bytes.
func ifname(name string) []byte {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, name)
	return b
}

func be16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}
