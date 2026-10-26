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
