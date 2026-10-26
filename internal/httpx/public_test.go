package httpx_test

import (
	"net/netip"
	"testing"

	"github.com/crewlet/crewlet/internal/httpx"
)

// public is the rule's verdict on one address, the one call every case below
// makes.
func public(t *testing.T, addr netip.Addr) bool {
	t.Helper()
	return httpx.Public(addr)
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
// never the internet's own anycast services.
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
