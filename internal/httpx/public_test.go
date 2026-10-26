package httpx_test

import (
	"net/netip"
	"testing"

	"github.com/crewlet/crewlet/internal/httpx"
)

// AN ADDRESS THAT IS NOT PUBLIC IS NEVER JUDGED PUBLIC, so no dialer holding
// the rule dials it: the cloud metadata address and an IPv4 address written as
// IPv6 among them.
func TestOnlyPublicAddressesArePublic(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fd00::1", "fe80::1",
		"::ffff:127.0.0.1", "224.0.0.1", "255.255.255.255", "198.18.0.1"} {
		if httpx.Public(netip.MustParseAddr(raw)) {
			t.Errorf("%s is judged public", raw)
		}
	}
	for _, raw := range []string{"104.16.0.1", "151.101.1.1", "2606:4700::1"} {
		if !httpx.Public(netip.MustParseAddr(raw)) {
			t.Errorf("%s is judged not public", raw)
		}
	}
}

// EVERY RANGE THE RULE LISTS REFUSES AN ADDRESS INSIDE IT, and so does an
// IPv4 one written in IPv6 form. netip's own predicates answer for loopback,
// private, link-local and multicast; these ranges are the rule's alone, so a
// range dropped from its list, or an address judged before it is unmapped
// (an IPv4 range never contains a mapped address), fails a case here whether
// or not the table above happens to name an address inside it.
func TestEveryReservedRangeIsNotPublic(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ addr, what string }{
		{"0.1.2.3", "this network"},
		{"100.127.255.255", "carrier-grade NAT"},
		{"192.0.0.8", "IETF protocol assignments"},
		{"192.0.2.1", "documentation"},
		{"198.19.255.255", "benchmarking"},
		{"198.51.100.1", "documentation"},
		{"203.0.113.1", "documentation"},
		{"240.0.0.1", "reserved"},
		{"64:ff9b:1::1", "local-use IPv4/IPv6 translation"},
		{"100::1", "discard"},
		{"2001:db8::1", "documentation"},
		{"::ffff:100.64.0.1", "carrier-grade NAT, in IPv6 form"},
		{"::ffff:192.0.2.1", "documentation, in IPv6 form"},
	} {
		if httpx.Public(netip.MustParseAddr(c.addr)) {
			t.Errorf("%s (%s) is judged public", c.addr, c.what)
		}
	}
}
