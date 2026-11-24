package redirect

import (
	"bytes"
	"runtime"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"

	"github.com/gpillon/k4all-operator/internal/metadata"
)

// newNetNS is a network namespace of the test's own (it needs root): the
// redirect is installed there, never in the host's.
func newNetNS(t *testing.T) *nftables.Conn {
	t.Helper()
	runtime.LockOSThread()
	orig, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		runtime.UnlockOSThread()
		t.Skipf("no network namespaces here: %v", err)
	}
	defer func() { _ = unix.Close(orig) }()
	if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
		runtime.UnlockOSThread()
		t.Skipf("cannot create a network namespace (needs root): %v", err)
	}
	ns, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if setErr := unix.Setns(orig, unix.CLONE_NEWNET); setErr != nil {
		// The thread stays in the new namespace: leave it locked, Go
		// drops it with the goroutine.
		t.Fatalf("back to the test's network namespace: %v", setErr)
	}
	runtime.UnlockOSThread()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(ns) })
	c, err := nftables.New(nftables.WithNetNSFd(ns))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func chains(t *testing.T, c *nftables.Conn) map[string]*nftables.Chain {
	t.Helper()
	list, err := c.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]*nftables.Chain{}
	for _, ch := range list {
		m[ch.Table.Name+"/"+ch.Name] = ch
	}
	return m
}

func rules(t *testing.T, c *nftables.Conn, ch *nftables.Chain) []*nftables.Rule {
	t.Helper()
	r, err := c.GetRules(ch.Table, ch)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// hasCmp says whether the rule compares (op) with data.
func hasCmp(r *nftables.Rule, op expr.CmpOp, data []byte) bool {
	for _, e := range r.Exprs {
		if c, ok := e.(*expr.Cmp); ok && c.Op == op && bytes.Equal(c.Data, data) {
			return true
		}
	}
	return false
}

func has[T expr.Any](r *nftables.Rule) bool {
	for _, e := range r.Exprs {
		if _, ok := e.(T); ok {
			return true
		}
	}
	return false
}

// The table of the prototype (VM 970), in the pod's network namespace:
//
//	table ip k4all_metadata {
//	  chain prerouting { type nat hook prerouting priority dstnat - 10; policy accept;
//	    iifname "k6t-eth0" ip daddr 169.254.169.254 tcp dport 80 counter redirect to :8775 }
//	  chain input { type filter hook input priority filter - 10; policy accept;
//	    iifname != { "k6t-eth0", "lo" } tcp dport 8775 counter drop }
//	}
//
// Ahead of KubeVirt's own nat table (priority dstnat), which it leaves alone.
func TestInstall_TheRedirectTable(t *testing.T) {
	c := newNetNS(t)
	kubevirt := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: "nat"})
	if err := c.Flush(); err != nil {
		t.Skipf("no nftables in this kernel: %v", err)
	}
	if err := Install(c, metadata.Port); err != nil {
		t.Fatal(err)
	}
	got := chains(t, c)
	pre, in := got[metadata.NFTTable+"/prerouting"], got[metadata.NFTTable+"/input"]
	if pre == nil || in == nil {
		t.Fatalf("chains: %v", got)
	}
	if pre.Type != nftables.ChainTypeNAT || *pre.Hooknum != *nftables.ChainHookPrerouting || *pre.Priority != -110 ||
		*pre.Policy != nftables.ChainPolicyAccept {
		t.Fatalf("prerouting: %+v", pre)
	}
	if in.Type != nftables.ChainTypeFilter || *in.Hooknum != *nftables.ChainHookInput || *in.Priority != -10 ||
		*in.Policy != nftables.ChainPolicyAccept {
		t.Fatalf("input: %+v", in)
	}

	redirect := rules(t, c, pre)
	if len(redirect) != 1 {
		t.Fatalf("prerouting rules: %d", len(redirect))
	}
	wantRedirectRule(t, redirect[0])
	drop := rules(t, c, in)
	if len(drop) != 1 {
		t.Fatalf("input rules: %d", len(drop))
	}
	wantDropRule(t, drop[0])
	if _, ok := got[kubevirt.Name+"/prerouting"]; ok {
		t.Fatal("test setup: nat had no chains")
	}
	if _, err := c.ListTableOfFamily(kubevirt.Name, nftables.TableFamilyIPv4); err != nil {
		t.Fatalf("KubeVirt's table: %v", err)
	}
}

// An init container runs again when its pod's sandbox is recreated: the
// table is replaced, not doubled.
func TestInstall_Twice(t *testing.T) {
	c := newNetNS(t)
	for i := 0; i < 2; i++ {
		if err := Install(c, metadata.Port); err != nil {
			if i == 0 {
				t.Skipf("no nftables in this kernel: %v", err)
			}
			t.Fatal(err)
		}
	}
	got := chains(t, c)
	for _, name := range []string{"prerouting", "input"} {
		if n := len(rules(t, c, got[metadata.NFTTable+"/"+name])); n != 1 {
			t.Fatalf("%s: %d rules", name, n)
		}
	}
}

// iifname "k6t-eth0" ip daddr 169.254.169.254 tcp dport 80 counter redirect to :8775
func wantRedirectRule(t *testing.T, r *nftables.Rule) {
	t.Helper()
	if !hasCmp(r, expr.CmpOpEq, ifname("k6t-eth0")) || !hasCmp(r, expr.CmpOpEq, []byte{169, 254, 169, 254}) ||
		!hasCmp(r, expr.CmpOpEq, []byte{unix.IPPROTO_TCP}) || !hasCmp(r, expr.CmpOpEq, []byte{0, 80}) ||
		!has[*expr.Counter](r) || !has[*expr.Redir](r) {
		t.Fatalf("redirect rule: %#v", r.Exprs)
	}
	var port []byte
	for _, e := range r.Exprs {
		if i, ok := e.(*expr.Immediate); ok {
			port = i.Data
		}
	}
	if !bytes.Equal(port, []byte{0x22, 0x47}) { // 8775
		t.Fatalf("redirected to %v", port)
	}
}

// iifname != { "k6t-eth0", "lo" } tcp dport 8775 counter drop
func wantDropRule(t *testing.T, d *nftables.Rule) {
	t.Helper()
	if !hasCmp(d, expr.CmpOpNeq, ifname("k6t-eth0")) || !hasCmp(d, expr.CmpOpNeq, ifname("lo")) ||
		!hasCmp(d, expr.CmpOpEq, []byte{0x22, 0x47}) || !has[*expr.Counter](d) {
		t.Fatalf("drop rule: %#v", d.Exprs)
	}
	var verdict *expr.Verdict
	for _, e := range d.Exprs {
		if v, ok := e.(*expr.Verdict); ok {
			verdict = v
		}
	}
	if verdict == nil || verdict.Kind != expr.VerdictDrop {
		t.Fatalf("drop rule verdict: %#v", d.Exprs)
	}
}
