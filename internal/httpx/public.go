package httpx

import "net/netip"

// reserved are the ranges an address reachable from the public internet never
// falls in, beyond what [netip.Addr]'s own predicates name.
var reserved = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // this network
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and the broadcast address
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use IPv4/IPv6 translation
	netip.MustParsePrefix("100::/64"),        // discard
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
}

// Public reports whether ip is an address on the public internet: not
// loopback, private, link-local (the cloud metadata address among them),
// multicast, unspecified, or inside one of the ranges listed in reserved
// (carrier-grade NAT among them). An IPv4 address written in IPv6 form is
// judged as the IPv4 address it is.
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
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() {
		return false
	}
	for _, p := range reserved {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
