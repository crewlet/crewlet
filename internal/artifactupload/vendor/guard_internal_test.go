package vendor

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"syscall"
	"testing"

	"github.com/crewlet/crewlet/internal/httpx"
)

// resolver answers from a table and reaches nothing. A name it holds no
// address for, ipv4only.arpa among them unless a case gives it some, answers
// as Go's resolver answers NODATA: a not-found error. A lookup of
// ipv4only.arpa fails with discovery instead, where a case sets it.
type resolver struct {
	names     map[string][]string
	discovery error
}

func (r resolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	if host == "ipv4only.arpa." && r.discovery != nil {
		return nil, r.discovery
	}
	var addrs []netip.Addr
	for _, raw := range r.names[host] {
		if addr := netip.MustParseAddr(raw); network != "ip6" || addr.Is6() {
			addrs = append(addrs, addr)
		}
	}
	if len(addrs) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return addrs, nil
}

// THE GUARD DIALS NO ADDRESS THAT IS NOT PUBLIC, by name or by literal, and
// dials a proxy the operator configured as itself.
func TestTheGuardRefusesAnAddressThatIsNotPublic(t *testing.T) {
	t.Parallel()
	g := &guard{dialer: &net.Dialer{}, proxies: map[string]bool{},
		resolver: resolver{names: map[string][]string{"localhost": {"127.0.0.1", "::1"}}}}
	for _, addr := range []string{"127.0.0.1:443", "[::1]:443", "localhost:443", "169.254.169.254:80"} {
		if _, err := g.dial(t.Context(), "tcp", addr); !errors.Is(err, httpx.ErrNotPublic) {
			t.Errorf("a dial of %s answered %v, want httpx.ErrNotPublic", addr, err)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	g.trust(&url.URL{Scheme: "http", Host: ln.Addr().String()})
	conn, err := g.dial(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("the operator's proxy was refused: %v", err)
	}
	_ = conn.Close()
}

// THE GUARD ASKS ITS OWN RESOLVER FOR THE NETWORK'S NAT64 PREFIXES, and judges
// a CDN that resolves under one by the IPv4 address it carries there. A
// resolver that cannot answer for them fails the dial, which is never made on
// a guess.
func TestTheGuardJudgesANAT64AddressByTheIPv4AddressItCarries(t *testing.T) {
	t.Parallel()
	dns64 := resolver{names: map[string][]string{
		"ipv4only.arpa.": {"3ffe:64:64:96::c000:aa", "3ffe:64:64:96::c000:ab"},
		"cdn.example":    {"3ffe:64:64:96::a9fe:a9fe"},
	}}
	// The dialer opens no connection, so a judgment gone wrong fails the case
	// rather than sending a packet.
	nowhere := &net.Dialer{Control: func(string, string, syscall.RawConn) error {
		return errors.New("this case dials nothing")
	}}
	g := &guard{dialer: nowhere, resolver: dns64, proxies: map[string]bool{}}
	if _, err := g.dial(t.Context(), "tcp", "cdn.example:443"); !errors.Is(err, httpx.ErrNotPublic) {
		t.Errorf("a CDN resolving to the cloud metadata address under the network's prefix answered %v", err)
	}

	misbehaving := &net.DNSError{Err: "server misbehaving", Name: "ipv4only.arpa.", IsTemporary: true}
	g.resolver = resolver{names: map[string][]string{"cdn.example": {"10.0.0.5"}}, discovery: misbehaving}
	if _, err := g.dial(t.Context(), "tcp", "cdn.example:443"); !errors.Is(err, misbehaving) ||
		errors.Is(err, httpx.ErrNotPublic) {
		t.Errorf("a dial whose resolver failed ipv4only.arpa answered %v, want that failure", err)
	}
}
