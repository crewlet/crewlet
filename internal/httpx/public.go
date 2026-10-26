package httpx

import "net/netip"

// notGlobal4 are the IPv4 blocks no address on the public internet is in.
//
// They are IANA's IPv4 Special-Purpose Address Registry, held whole: every
// block it marks not globally reachable is here or inside a block that is, and
// TestEveryReservedRangeIsNotPublic copies the registry row by row. Two more
// join them. 192.88.99.0/24, the 6to4 relay anycast RFC 7526 retired, whose
// terminated row carries no flags any more: a relay still answering it
// delivers to the IPv4 address a 6to4 packet carries. And multicast, which
// IANA keeps in a registry of its own: no connection is made to a group.
//
// 192.0.0.0/24 is refused whole, although the registry marks two addresses in
// it globally reachable (192.0.0.9 and 192.0.0.10): each is an anycast service
// (PCP, TURN), never a host a dial here means to reach.
//
// IPv4 is held to a list of what is reserved, where IPv6 is held to a list of
// what is allocated ([global6]), because IPv4 has no unallocated remainder: it
// is allocated whole, so the registry's list is the complete answer.
var notGlobal4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network" (RFC 791); 0.0.0.0 is in it
	netip.MustParsePrefix("10.0.0.0/8"),      // private use (RFC 1918)
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space, carrier-grade NAT (RFC 6598)
	netip.MustParsePrefix("127.0.0.0/8"),     // loopback (RFC 1122)
	netip.MustParsePrefix("169.254.0.0/16"),  // link local (RFC 3927): the cloud metadata address
	netip.MustParsePrefix("172.16.0.0/12"),   // private use (RFC 1918)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments (RFC 6890), whole
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation (RFC 5737)
	netip.MustParsePrefix("192.88.99.0/24"),  // 6to4 relay anycast, deprecated (RFC 7526)
	netip.MustParsePrefix("192.168.0.0/16"),  // private use (RFC 1918)
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking (RFC 2544)
	netip.MustParsePrefix("198.51.100.0/24"), // documentation (RFC 5737)
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation (RFC 5737)
	netip.MustParsePrefix("224.0.0.0/4"),     // multicast (RFC 5771)
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved (RFC 1112); 255.255.255.255 is in it
}

// global6 is global unicast, the one block IANA allocates IPv6 unicast
// addresses from (its IPv6 Address Space registry: "IANA unicast address
// assignments are currently limited to ... 2000::/3"). Every IPv6 address on
// the public internet is in it.
var global6 = netip.MustParsePrefix("2000::/3")

// notGlobal6 are the blocks inside [global6] that are not on the public
// internet: those IANA's IPv6 Special-Purpose Address Registry marks not
// globally reachable, and the two that carry an IPv4 address a relay delivers
// to.
//
// 2001::/23 is refused whole. It holds Teredo, benchmarking and ORCHID, and
// the entries the registry marks globally reachable inside it are anycast
// services (PCP, TURN, DNS-SD, AMT, AS112) and identifiers (ORCHIDv2, drone
// tags), never a host a dial here means to reach.
var notGlobal6 = []netip.Prefix{
	netip.MustParsePrefix("2001::/23"),     // IETF protocol assignments (RFC 2928), whole
	netip.MustParsePrefix("2001:db8::/32"), // documentation (RFC 3849)
	netip.MustParsePrefix("2002::/16"),     // 6to4 (RFC 3056), which carries an IPv4 address
	netip.MustParsePrefix("3fff::/20"),     // documentation (RFC 9637)
}

// Public reports whether ip is an address on the public internet.
//
// An IPv4 address is public unless it is in a block IANA reserves
// ([notGlobal4]); one written in IPv6 form (::ffff:a.b.c.d) is judged as the
// IPv4 address it is. An IPv6 address is public only inside global unicast,
// 2000::/3, and outside the four blocks there that are not on the internet
// ([notGlobal6]): 2001::/23, the two documentation blocks and 6to4. Every
// other IPv6 address is not public, whatever block it is in: unique-local,
// link-local, site-local, multicast, the forms that carry an IPv4 address
// (IPv4-compatible ::/96, the NAT64 prefixes 64:ff9b::/96 and 64:ff9b:1::/48),
// and every block the IETF still holds in reserve. So is an address scoped to
// one link by a zone.
//
// IPv6 IS AN ALLOWLIST because a list of what is reserved cannot be completed.
// IANA keeps carving blocks out of the unallocated space (5f00::/16 in 2024,
// 100:0:0:1::/64 in 2025), and a denylist calls each one public until somebody
// adds its row: the one this replaced called NAT64, 6to4, Teredo and
// IPv4-compatible addresses public, each a way to reach a private IPv4 address
// through an IPv6 one. Outside 2000::/3 nothing is public, whatever IANA
// reserves there next. A block it reserves inside 2000::/3, as it did
// 3fff::/20 in 2024, still needs its row here.
//
// Every dialer that refuses an address asks this rather than keeping a list
// of ranges of its own (the package doc says why). It judges ONE address, and
// what makes it a wall is how a dialer uses it: every address a name resolves
// to is held to it, and the dial goes to the address that was judged, never to
// the name again, so a resolver that answers twice (once public, once
// internal) cannot slip between the check and the dial. The CDN fetch's
// dialer (internal/artifactupload/vendor.Guarded) is the worked example.
func Public(ip netip.Addr) bool {
	ip = ip.Unmap()
	switch {
	case ip.Is4():
		return !inAny(notGlobal4, ip)
	case ip.Is6():
		// A zoned address is in no prefix, so it falls out here: a zone
		// scopes it to one link of this host.
		return global6.Contains(ip) && !inAny(notGlobal6, ip)
	}
	return false
}

// inAny reports whether ip is in one of blocks.
func inAny(blocks []netip.Prefix, ip netip.Addr) bool {
	for _, b := range blocks {
		if b.Contains(ip) {
			return true
		}
	}
	return false
}
