package httpx_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/httpx"
)

// network is a resolver answering from a table, and reaching nothing: a name
// it holds no address for (of the family asked) answers as Go's resolver
// answers NODATA and NXDOMAIN, a not-found error. So ipv4only.arpa has no AAAA
// record unless a case gives it some, as on a network with no DNS64.
type network struct {
	answers map[string][]string
	fail    map[string]error

	mu    sync.Mutex
	asked []string
}

func (n *network) LookupNetIP(_ context.Context, family, host string) ([]netip.Addr, error) {
	n.mu.Lock()
	n.asked = append(n.asked, family+" "+host)
	n.mu.Unlock()
	if err := n.fail[host]; err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	for _, raw := range n.answers[host] {
		addr := netip.MustParseAddr(raw)
		if family == "ip" || (family == "ip6") == (addr.Is6() && !addr.Is4In6()) {
			addrs = append(addrs, addr)
		}
	}
	if len(addrs) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return addrs, nil
}

// dns64 is a network whose resolver synthesizes AAAA records, answering
// ipv4only.arpa with answers: each an address a DNS64 resolver writes for one
// of the name's A records (192.0.0.170, 192.0.0.171) under a prefix it
// translates through. Every case spells them out by hand, at the position RFC
// 6052 section 2.2 fixes, rather than building them with the code they test.
func dns64(answers ...string) *network {
	return &network{answers: map[string][]string{"ipv4only.arpa.": answers}}
}

// public is the rule's verdict on one address, written as a host, on a
// network with no DNS64: the one call the cases holding the blocks make.
func public(t *testing.T, addr netip.Addr) bool {
	t.Helper()
	return publicOn(t, &network{}, addr)
}

// publicOn is the rule's verdict on addr, written as a host, on network n.
func publicOn(t *testing.T, n *network, addr netip.Addr) bool {
	t.Helper()
	judged, err := httpx.ResolvePublic(t.Context(), n, addr.String())
	switch {
	case errors.Is(err, httpx.ErrNotPublic):
		return false
	case err != nil:
		t.Fatalf("judging %s: %v", addr, err)
	case len(judged) != 1 || judged[0] != addr.Unmap():
		t.Fatalf("judging %s answered %v, want the address unmapped", addr, judged)
	}
	return true
}

// ends is the first and the last address of a block, each judged by a case
// that holds the whole block: a row whose prefix is cut too short, or names
// the wrong block, leaves one of the two outside it.
func ends(p netip.Prefix) []netip.Addr {
	first := p.Masked().Addr()
	if p.IsSingleIP() {
		return []netip.Addr{first}
	}
	last := first.AsSlice()
	for bit := p.Bits(); bit < len(last)*8; bit++ {
		last[bit/8] |= 1 << (7 - bit%8)
	}
	end, _ := netip.AddrFromSlice(last)
	return []netip.Addr{first, end}
}

// AN ADDRESS THAT IS NOT PUBLIC IS NEVER JUDGED PUBLIC, so no dialer holding
// the rule dials it: the cloud metadata address and an IPv4 address written as
// IPv6 among them. And an IPv4 address written as IPv6 is judged as itself
// both ways: judged before it is unmapped, a public one would fall outside
// global unicast and be refused, which only its row here shows.
func TestOnlyPublicAddressesArePublic(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fd00::1", "fe80::1",
		"::ffff:127.0.0.1", "224.0.0.1", "255.255.255.255", "198.18.0.1"} {
		if public(t, netip.MustParseAddr(raw)) {
			t.Errorf("%s is judged public", raw)
		}
	}
	for _, raw := range []string{"104.16.0.1", "151.101.1.1", "2606:4700::1", "::ffff:104.16.0.1"} {
		if !public(t, netip.MustParseAddr(raw)) {
			t.Errorf("%s is judged not public", raw)
		}
	}
}

// EVERY BLOCK IANA RESERVES IS NOT PUBLIC, from its first address to its last,
// and neither is any IPv6 address outside global unicast (2000::/3), the one
// block IANA allocates IPv6 unicast from.
//
// The IPv4 and IPv6 rows are IANA's two Special-Purpose Address Registries
// (both last updated 2025-10-09, read 2026-10-26), copied row by row: every
// block marked not globally reachable, and the globally reachable entries the
// rule refuses anyway, each with its reason. A block IANA reserves later is
// added here and to the rule together. Every IPv4 block is judged in IPv6 form
// too (::ffff:a.b.c.d), which a dial sends to the IPv4 address it carries.
func TestEveryReservedRangeIsNotPublic(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ block, what string }{
		{"0.0.0.0/8", `"this network" (RFC 791)`},
		{"0.0.0.0/32", `"this host on this network" (RFC 1122)`},
		{"10.0.0.0/8", "private use (RFC 1918)"},
		{"100.64.0.0/10", "shared address space, carrier-grade NAT (RFC 6598)"},
		{"127.0.0.0/8", "loopback (RFC 1122)"},
		{"169.254.0.0/16", "link local (RFC 3927), the cloud metadata address among it"},
		{"172.16.0.0/12", "private use (RFC 1918)"},
		{"192.0.0.0/24", "IETF protocol assignments (RFC 6890)"},
		{"192.0.0.0/29", "IPv4 service continuity prefix (RFC 7335)"},
		{"192.0.0.8/32", "IPv4 dummy address (RFC 7600)"},
		{"192.0.0.170/32", "NAT64/DNS64 discovery (RFC 8880)"},
		{"192.0.0.171/32", "NAT64/DNS64 discovery (RFC 8880)"},
		{"192.0.2.0/24", "documentation, TEST-NET-1 (RFC 5737)"},
		{"192.88.99.2/32", "6a44 relay anycast (RFC 6751)"},
		{"192.168.0.0/16", "private use (RFC 1918)"},
		{"198.18.0.0/15", "benchmarking (RFC 2544)"},
		{"198.51.100.0/24", "documentation, TEST-NET-2 (RFC 5737)"},
		{"203.0.113.0/24", "documentation, TEST-NET-3 (RFC 5737)"},
		{"240.0.0.0/4", "reserved (RFC 1112)"},
		{"255.255.255.255/32", "limited broadcast (RFC 919)"},
		// Globally reachable, and refused with the 192.0.0.0/24 they sit in:
		// each is an anycast service, never a host a dial here means to reach.
		{"192.0.0.9/32", "port control protocol anycast (RFC 7723)"},
		{"192.0.0.10/32", "TURN anycast (RFC 8155)"},
		// The registry's terminated row, which carries no flags any more: a
		// relay still answering it delivers to the IPv4 address a 6to4 packet
		// carries.
		{"192.88.99.0/24", "6to4 relay anycast, deprecated (RFC 7526)"},
		// Not in the special-purpose registry, which leaves multicast to a
		// registry of its own: no connection is made to a group.
		{"224.0.0.0/4", "multicast (RFC 5771)"},

		{"::1/128", "loopback (RFC 4291)"},
		{"::/128", "unspecified (RFC 4291)"},
		{"64:ff9b:1::/48", "local-use IPv4/IPv6 translation (RFC 8215)"},
		{"100::/64", "discard-only (RFC 6666)"},
		{"100:0:0:1::/64", "dummy prefix (RFC 9780)"},
		{"2001::/23", "IETF protocol assignments (RFC 2928)"},
		{"2001::/32", "Teredo (RFC 4380), which carries an IPv4 address"},
		{"2001:2::/48", "benchmarking (RFC 5180)"},
		{"2001:10::/28", "ORCHID, deprecated (RFC 4843)"},
		{"2001:db8::/32", "documentation (RFC 3849)"},
		{"2002::/16", "6to4 (RFC 3056), which carries an IPv4 address"},
		{"3fff::/20", "documentation (RFC 9637)"},
		{"5f00::/16", "segment routing SIDs (RFC 9602)"},
		{"fc00::/7", "unique local (RFC 4193)"},
		{"fe80::/10", "link-local unicast (RFC 4291)"},
		// Globally reachable, and refused with the 2001::/23 they sit in: each
		// is an anycast service or an identifier, never a host a dial here
		// means to reach.
		{"2001:1::1/128", "port control protocol anycast (RFC 7723)"},
		{"2001:1::2/128", "TURN anycast (RFC 8155)"},
		{"2001:1::3/128", "DNS-SD service registration anycast (RFC 9665)"},
		{"2001:3::/32", "AMT relay anycast (RFC 7450)"},
		{"2001:4:112::/48", "AS112-v6 anycast (RFC 7535)"},
		{"2001:20::/28", "ORCHIDv2 identifiers (RFC 7343)"},
		{"2001:30::/28", "drone remote ID entity tags (RFC 9374)"},
		// Outside global unicast, and in neither registry's rows.
		{"::/96", "IPv4-compatible, deprecated (RFC 4291 section 2.5.5.1)"},
		{"fec0::/10", "site-local, deprecated (RFC 3879)"},
		{"ff00::/8", "multicast (RFC 4291)"},
		{"4000::/3", "reserved by the IETF, never allocated"},
	} {
		block := netip.MustParsePrefix(c.block)
		for _, addr := range ends(block) {
			if public(t, addr) {
				t.Errorf("%s, in %s (%s), is judged public", addr, c.block, c.what)
			}
			if addr.Is4() {
				if mapped := netip.AddrFrom16(addr.As16()); public(t, mapped) {
					t.Errorf("%s, in %s (%s) written in IPv6 form, is judged public", mapped, c.block, c.what)
				}
			}
		}
	}
	// The addresses the probe that found the old rule's gaps called public,
	// verbatim, and the forms that carry an IPv4 address beside them.
	for _, c := range []struct{ addr, what string }{
		{"64:ff9b::a00:5", "NAT64 (RFC 6052) carrying 10.0.0.5"},
		{"64:ff9b::a9fe:a9fe", "NAT64 carrying 169.254.169.254"},
		{"64:ff9b::7f00:1", "NAT64 carrying 127.0.0.1"},
		{"64:ff9b:1::a00:5", "local-use NAT64 (RFC 8215) carrying 10.0.0.5"},
		{"2002:a00:5::1", "6to4 carrying 10.0.0.5"},
		{"2002:7f00:1::1", "6to4 carrying 127.0.0.1"},
		{"2001:0:4136:e378:8000:63bf:f5ff:fffa", "Teredo"},
		{"::a00:5", "IPv4-compatible carrying 10.0.0.5"},
		{"::7f00:1", "IPv4-compatible carrying 127.0.0.1"},
		{"2001:2::1", "benchmarking"},
		{"2001:10::1", "ORCHID"},
		{"3fff::1", "documentation"},
		{"5f00::1", "an SRv6 SID"},
		{"192.88.99.1", "6to4 relay anycast"},
		{"2606:4700::1%eth0", "a global address scoped to one link"},
	} {
		if public(t, netip.MustParseAddr(c.addr)) {
			t.Errorf("%s (%s) is judged public", c.addr, c.what)
		}
	}
}

// A BLOCK IANA MARKS GLOBALLY REACHABLE IS PUBLIC, from its first address to
// its last, unless it sits inside a block the rule refuses whole (the rows
// above give each one's reason): the rule refuses what is special-purpose,
// never the internet's own anycast services. The one other such block, the
// NAT64 well-known prefix, is judged address by address, by the IPv4 address
// each carries (TestANAT64AddressIsJudgedByTheIPv4AddressItCarries).
func TestAGloballyReachableBlockIsPublic(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ block, why string }{
		{"192.31.196.0/24", "AS112-v4 (RFC 7535): the internet's anycast sink for reverse lookups of private space"},
		{"192.52.193.0/24", "AMT (RFC 7450): multicast relays reached across the internet"},
		{"192.175.48.0/24", "AS112 direct delegation (RFC 7534): the same sink, delegated to directly"},
		{"2620:4f:8000::/48", "AS112 direct delegation (RFC 7534), in global unicast"},
	} {
		for _, addr := range ends(netip.MustParsePrefix(c.block)) {
			if !public(t, addr) {
				t.Errorf("%s, in %s, is judged not public: %s", addr, c.block, c.why)
			}
		}
	}
}

// AN ADDRESS IN A NAT64 PREFIX IS JUDGED BY THE IPv4 ADDRESS IT CARRIES, and by
// nothing else, read at the position RFC 6052 section 2.2 fixes for the
// prefix's length (past the u octet, for the lengths whose IPv4 address
// straddles it).
//
// The well-known prefix counts on every network, named or not. A
// network-specific one counts where the network's resolver names it, and
// each 3ffe row's prefix lies inside 2000::/3 (3ffe::/16 was the 6bone's,
// returned to IANA in 2006: global unicast that names nobody's network).
// Where such a prefix is not named the allowlist judges the same addresses
// and calls the one carrying 10.0.0.5 public, which is why discovery is part
// of the rule. The last row's prefix is in RFC 8215's local-use block, which
// the allowlist refuses whole where nothing names a prefix in it.
func TestANAT64AddressIsJudgedByTheIPv4AddressItCarries(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		addr string
		want bool
	}{
		{"64:ff9b::6810:1", true},     // 104.16.0.1
		{"64:ff9b::a00:5", false},     // 10.0.0.5
		{"64:ff9b::a9fe:a9fe", false}, // 169.254.169.254, the cloud metadata address
		{"64:ff9b::7f00:1", false},    // 127.0.0.1
		{"64:ff9b::", false},          // 0.0.0.0, the prefix's first address
		{"64:ff9b::ffff:ffff", false}, // 255.255.255.255, its last
	} {
		for _, n := range []*network{{}, dns64("64:ff9b::c000:aa", "64:ff9b::c000:ab")} {
			if got := publicOn(t, n, netip.MustParseAddr(c.addr)); got != c.want {
				t.Errorf("%s is judged public=%v, want %v (prefixes named: %v)", c.addr, got, c.want, n.answers)
			}
		}
	}
	for _, c := range []struct {
		prefix      string
		synthesized [2]string // what a DNS64 resolver answers ipv4only.arpa with under prefix
		public      string    // 104.16.0.1 under prefix
		private     string    // 10.0.0.5 under prefix
		unnamed     bool      // the allowlist's verdict on both where nothing names prefix
	}{
		{"3ffe:64::/32", [2]string{"3ffe:64:c000:aa::", "3ffe:64:c000:ab::"},
			"3ffe:64:6810:1::", "3ffe:64:a00:5::", true},
		{"3ffe:64:6400::/40", [2]string{"3ffe:64:64c0:0:aa::", "3ffe:64:64c0:0:ab::"},
			"3ffe:64:6468:1000:1::", "3ffe:64:640a:0:5::", true},
		{"3ffe:64:64::/48", [2]string{"3ffe:64:64:c000:0:aa00::", "3ffe:64:64:c000:0:ab00::"},
			"3ffe:64:64:6810:0:100::", "3ffe:64:64:a00:0:500::", true},
		{"3ffe:64:64:6400::/56", [2]string{"3ffe:64:64:64c0:0:aa::", "3ffe:64:64:64c0:0:ab::"},
			"3ffe:64:64:6468:10:1::", "3ffe:64:64:640a:0:5::", true},
		{"3ffe:64:64:64::/64", [2]string{"3ffe:64:64:64:c0:0:aa00:0", "3ffe:64:64:64:c0:0:ab00:0"},
			"3ffe:64:64:64:68:1000:100:0", "3ffe:64:64:64:a:0:500:0", true},
		{"3ffe:64:64:96::/96", [2]string{"3ffe:64:64:96::c000:aa", "3ffe:64:64:96::c000:ab"},
			"3ffe:64:64:96::6810:1", "3ffe:64:64:96::a00:5", true},
		{"64:ff9b:1::/96", [2]string{"64:ff9b:1::c000:aa", "64:ff9b:1::c000:ab"},
			"64:ff9b:1::6810:1", "64:ff9b:1::a00:5", false},
	} {
		named := dns64(c.synthesized[0], c.synthesized[1])
		pub, priv := netip.MustParseAddr(c.public), netip.MustParseAddr(c.private)
		if !publicOn(t, named, pub) {
			t.Errorf("%s, carrying 104.16.0.1 under the named %s, is judged not public", pub, c.prefix)
		}
		if publicOn(t, named, priv) {
			t.Errorf("%s, carrying 10.0.0.5 under the named %s, is judged public", priv, c.prefix)
		}
		for _, addr := range []netip.Addr{pub, priv} {
			if got := publicOn(t, &network{}, addr); got != c.unnamed {
				t.Errorf("%s, under %s with nothing naming it, is judged public=%v, want the allowlist's %v",
					addr, c.prefix, got, c.unnamed)
			}
		}
	}
	// An address two named prefixes hold is public only when the address it
	// carries is public read at both positions.
	both := dns64("3ffe:64:64:64:c0:0:aa00:0", "3ffe:64:64:64:c0:0:ab00:0",
		"3ffe:64:64:64::c000:aa", "3ffe:64:64:64::c000:ab")
	if addr := netip.MustParseAddr("3ffe:64:64:64::6810:1"); publicOn(t, both, addr) {
		t.Errorf("%s, carrying 104.16.0.1 under the /96 and 0.0.0.104 under the /64, is judged public", addr)
	}
	if addr := netip.MustParseAddr("3ffe:64:64:64:68:1000:100:0"); !publicOn(t, both, addr) {
		t.Errorf("%s, carrying 104.16.0.1 under the /64 alone, is judged not public", addr)
	}
}

// THE NETWORK'S OWN PREFIXES ARE READ FROM ITS RESOLVER'S ANSWER FOR
// ipv4only.arpa (RFC 7050), and nothing else names one: no record is no
// DNS64, an answer carrying neither of the name's two addresses was not
// synthesized, and a prefix whose own bits hold 192.0.0.170 is read from the
// answer for 192.0.0.171, in either order. A lookup that fails, rather than
// finding no record, fails the resolve with the resolver's own error.
func TestTheNetworksPrefixesAreReadFromIpv4onlyArpa(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		n     *network
		probe string // an address the answer's prefix holds, carrying 10.0.0.5
		want  bool   // the verdict on probe: public only where nothing names its prefix
	}{
		{"the well-known prefix names no other",
			dns64("64:ff9b::c000:aa", "64:ff9b::c000:ab"), "3ffe:64:64:96::a00:5", true},
		{"a /96", dns64("3ffe:64:64:96::c000:aa", "3ffe:64:64:96::c000:ab"), "3ffe:64:64:96::a00:5", false},
		{"a /64, read past the u octet",
			dns64("3ffe:64:64:64:c0:0:aa00:0", "3ffe:64:64:64:c0:0:ab00:0"), "3ffe:64:64:64:a:0:500:0", false},
		{"no record names none", &network{}, "3ffe:64:64:96::a00:5", true},
		{"an answer carrying neither address names none",
			dns64("3ffe:64:64:96::c000:ac", "3ffe:64:64:96::c0a8:aa"), "3ffe:64:64:96::a00:5", true},
		{"an answer carrying 192.0.0.171 alone names its prefix",
			dns64("3ffe:64:64:96::c000:ab"), "3ffe:64:64:96::a00:5", false},
		{"a prefix holding 192.0.0.170 is read from 192.0.0.171's answer",
			dns64("3ffe:64:c000:aa::c000:aa", "3ffe:64:c000:aa::c000:ab"), "3ffe:64:c000:aa::a00:5", false},
		{"and its own bits name no /32",
			dns64("3ffe:64:c000:aa::c000:aa", "3ffe:64:c000:aa::c000:ab"), "3ffe:64:a00:5::", true},
		{"in either order",
			dns64("3ffe:64:c000:aa::c000:ab", "3ffe:64:c000:aa::c000:aa"), "3ffe:64:a00:5::", true},
	} {
		if got := publicOn(t, c.n, netip.MustParseAddr(c.probe)); got != c.want {
			t.Errorf("%s: %s is judged public=%v, want %v", c.name, c.probe, got, c.want)
		}
	}

	misbehaving := &net.DNSError{Err: "server misbehaving", Name: "ipv4only.arpa.", Server: "192.0.2.53:53",
		IsTemporary: true}
	n := &network{answers: map[string][]string{"cdn.example": {"104.16.0.1"}},
		fail: map[string]error{"ipv4only.arpa.": misbehaving}}
	for _, host := range []string{"cdn.example", "104.16.0.1", "2606:4700::1"} {
		judged, err := httpx.ResolvePublic(t.Context(), n, host)
		if err == nil || errors.Is(err, httpx.ErrNotPublic) || !errors.Is(err, misbehaving) ||
			!strings.Contains(err.Error(), "ipv4only.arpa.") {
			t.Errorf("%s, with the resolver failing ipv4only.arpa, answered %v and %v, want the failure, "+
				"naming the lookup", host, judged, err)
		}
	}
}

// A NAME IS PUBLIC ONLY WHEN EVERY ADDRESS IT RESOLVES TO IS, and comes back as
// the addresses to dial, an IPv4 one written in IPv6 form unmapped. One that
// is not refuses the name, naming the address. A name that resolves to no
// address, or not at all, is refused by its own lookup, never judged. And the
// network's prefixes are asked of the resolver the name is resolved with.
func TestANameIsPublicOnlyWhenEveryAddressItResolvesToIs(t *testing.T) {
	t.Parallel()
	n := &network{answers: map[string][]string{
		"cdn.example":   {"104.16.0.1", "::ffff:151.101.1.1", "2606:4700::1"},
		"split.example": {"104.16.0.1", "64:ff9b::a00:5"},
	}}
	judged, err := httpx.ResolvePublic(t.Context(), n, "cdn.example")
	want := []netip.Addr{netip.MustParseAddr("104.16.0.1"), netip.MustParseAddr("151.101.1.1"),
		netip.MustParseAddr("2606:4700::1")}
	if err != nil || !slices.Equal(judged, want) {
		t.Errorf("cdn.example answered %v and %v, want %v", judged, err, want)
	}
	if !slices.Contains(n.asked, "ip6 ipv4only.arpa.") {
		t.Errorf("the network's prefixes were not asked of the name's own resolver: it was asked %v", n.asked)
	}
	if _, err := httpx.ResolvePublic(t.Context(), n, "split.example"); !errors.Is(err, httpx.ErrNotPublic) ||
		!strings.Contains(err.Error(), "split.example resolves to 64:ff9b::a00:5") {
		t.Errorf("split.example, one of whose addresses carries 10.0.0.5, answered %v", err)
	}
	if _, err := httpx.ResolvePublic(t.Context(), n, "missing.example"); err == nil ||
		errors.Is(err, httpx.ErrNotPublic) {
		t.Errorf("missing.example, which does not resolve, answered %v, want its lookup's error", err)
	}
	var none resolverFunc = func(context.Context, string, string) ([]netip.Addr, error) { return nil, nil }
	if judged, err := httpx.ResolvePublic(t.Context(), none, "empty.example"); err == nil {
		t.Errorf("empty.example, which resolves to no address, answered %v", judged)
	}
}

// resolverFunc is a resolver written as one function.
type resolverFunc func(ctx context.Context, network, host string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}
