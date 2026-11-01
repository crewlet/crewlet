package httpx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"syscall"
	"time"
)

// ErrNotPublic is a connection [Guarded] refused because the address it would
// reach is not on the public internet ([Public]). A client hands it back
// wrapped in the url.Error and net.OpError of the request, and errors.Is finds
// it through both, so a caller tells "this address is not allowed" from "this
// address did not answer".
var ErrNotPublic = errors.New("httpx: the address is not on the public internet")

// The guarded dialer's bounds. They are the shared transport's own, the ones
// net/http's default dialer carries, because the guard changes WHICH addresses
// are dialled and nothing about how long a dial may take: the whole request is
// bounded by the timeout its caller hands [Guarded], which is the patience the
// caller chose for what it is asking.
const (
	guardDialTimeout = 30 * time.Second
	guardKeepAlive   = 30 * time.Second
)

// Guarded builds a client whose every connection reaches only a public
// address: a clone of [Transport] whose dialer refuses, at the moment it
// connects, any address [Public] refuses. It is the one client a request to a
// URL a REMOTE party named is sent through (ADR-0072), whatever named it: a
// page an agent wrote, a server's metadata, a redirect.
//
// # The address is judged where the socket is opened
//
// The dialer resolves a name as every client does, and the check runs in its
// Control hook, on the address the socket is about to connect to. That is
// what makes a resolver that answers twice harmless: a check of the NAME,
// followed by a dial of the name, resolves it again, and an answer that was
// public the first time and private the second is dialled unchecked. Judging
// the socket's own address leaves no second answer to slip through, however
// many lookups a name gets. A name with several answers connects only to the
// public ones; the dialer moves past a refused address as it moves past one
// that does not answer, and when it refuses them all the request fails with
// [ErrNotPublic].
//
// # A proxy the operator configured is dialled as itself
//
// The clone keeps the process's proxy settings (the rule [Transport] states),
// and the proxy's own address is dialled without the check, since a proxy on a
// private address is the operator's choice rather than the remote party's. The
// target the proxy is asked for is judged where this process can judge it: an
// address written into the URL must be public, and is refused before the proxy
// hears of it. A NAME the proxy resolves itself, so no answer this process got
// would be the address the proxy connects to, and a check here would be a
// guess a rebinding answer defeats. An operator whose proxy reaches internal
// addresses holds the proxy to this rule; [ErrNotPublic] cannot be its wall.
//
// # What it is not for
//
// An address the OPERATOR configured (an MCP server's url, a webhook sink, a
// self-hosted vendor) is dialled through [Client]: a private or loopback
// address there is a deployment the operator chose, and refusing it would
// break the deployments the docs describe.
func Guarded(timeout time.Duration) *http.Client {
	return Customized(timeout, newGuard(&net.Dialer{Timeout: guardDialTimeout, KeepAlive: guardKeepAlive}, Public).install)
}

// guard is the dialer behind [Guarded].
type guard struct {
	// checked dials every address but a proxy's, with [guard.control] as its
	// Control hook; plain dials the proxies the transport chose.
	checked, plain *net.Dialer

	// admit is the rule an address is judged by: [Public], always, outside
	// this package's own tests, which stand a listener on loopback and need
	// an admitted address to reach it.
	admit func(netip.Addr) bool

	mu      sync.Mutex
	proxies map[string]bool
}

// newGuard builds the dialer, d's settings shared by both of its halves.
func newGuard(d *net.Dialer, admit func(netip.Addr) bool) *guard {
	plain, checked := *d, *d
	g := &guard{plain: &plain, checked: &checked, admit: admit, proxies: map[string]bool{}}
	checked.Control, checked.ControlContext = nil, g.control
	return g
}

// install sets the guard on a transport: its dialer, and the judgement of
// what a proxy is asked for.
func (g *guard) install(t *http.Transport) {
	base := t.Proxy
	t.Proxy = func(r *http.Request) (*url.URL, error) {
		if base == nil {
			return nil, nil
		}
		u, err := base(r)
		if err != nil || u == nil {
			return u, err
		}
		if err := g.judgeTarget(r.URL); err != nil {
			return nil, err
		}
		g.trust(u)
		return u, nil
	}
	t.DialContext = g.dial
}

// judgeTarget refuses a target a proxy would be asked for when the URL writes
// its address and that address is not public. A host that does not parse as
// an address is a name, which the proxy resolves.
func (g *guard) judgeTarget(u *url.URL) error {
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !g.admit(ip) {
		return fmt.Errorf("%w: %s", ErrNotPublic, ip)
	}
	return nil
}

// trust records a proxy the transport chose, by the address it will dial.
func (g *guard) trust(u *url.URL) {
	host := u.Host
	if u.Port() == "" {
		port := "80"
		switch u.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		}
		host = net.JoinHostPort(u.Hostname(), port)
	}
	g.mu.Lock()
	g.proxies[host] = true
	g.mu.Unlock()
}

// dial dials a proxy the transport chose as itself and everything else
// through the checked dialer.
func (g *guard) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	g.mu.Lock()
	proxied := g.proxies[addr]
	g.mu.Unlock()
	if proxied {
		return g.plain.DialContext(ctx, network, addr)
	}
	return g.checked.DialContext(ctx, network, addr)
}

// control is the checked dialer's hook: address is the resolved address the
// socket is about to connect to, never a name.
func (g *guard) control(_ context.Context, _, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s is not an address this guard can judge", ErrNotPublic, address)
	}
	if !g.admit(ap.Addr()) {
		return fmt.Errorf("%w: %s", ErrNotPublic, ap.Addr())
	}
	return nil
}

// The ranges [Public] refuses beyond what [netip.Addr]'s own predicates name.
var (
	// reserved4 are the IPv4 ranges the IANA IPv4 Special-Purpose Address
	// Registry marks as not globally reachable, less the ones IsPrivate,
	// IsLoopback and IsLinkLocalUnicast already cover.
	reserved4 = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),       // this network
		netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT, a cloud's metadata address among it
		netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
		netip.MustParsePrefix("192.0.2.0/24"),    // documentation
		netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
		netip.MustParsePrefix("198.51.100.0/24"), // documentation
		netip.MustParsePrefix("203.0.113.0/24"),  // documentation
		netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and the broadcast address
	}

	// globalUnicast6 is the one IPv6 block IANA allocates global unicast
	// addresses from (RFC 4291, 2.4). Everything outside it is loopback,
	// link-local, unique local, multicast, discard-only, reserved or a
	// translation prefix, and none of it is a public address.
	globalUnicast6 = netip.MustParsePrefix("2000::/3")

	// reserved6 are the ranges inside 2000::/3 that are still not public.
	reserved6 = []netip.Prefix{
		netip.MustParsePrefix("2001::/23"),     // IETF protocol assignments: Teredo, benchmarking, ORCHID
		netip.MustParsePrefix("2001:db8::/32"), // documentation
		netip.MustParsePrefix("2002::/16"),     // 6to4, which embeds an IPv4 address and is deprecated (RFC 7526)
		netip.MustParsePrefix("3fff::/20"),     // documentation (RFC 9637)
	}

	// nat64 is the well-known NAT64 prefix (RFC 6052). An address in it IS
	// an IPv4 address, reached through the network's translator, so it is
	// judged as the IPv4 address it carries: refusing the prefix outright
	// would cut an IPv6-only host off from every IPv4-only site, and
	// admitting it outright would let 64:ff9b::a9fe:a9fe reach the metadata
	// address the translator forwards it to.
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
)

// Public reports whether ip is an address on the public internet: not
// loopback, private, link-local (the cloud metadata address among them),
// multicast, unspecified, carrier-grade NAT, documentation or any other range
// the IANA special-purpose registries mark as not globally reachable. An IPv4
// address written in IPv6 form, or behind the NAT64 prefix, is judged as the
// IPv4 address it is. An address carrying an IPv6 zone is never public: a zone
// names one of this host's interfaces, which only a scoped address needs.
func Public(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if nat64.Contains(ip) {
		ip = netip.AddrFrom4([4]byte(ip.AsSlice()[12:]))
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return false
	}
	reserved := reserved4
	if ip.Is6() {
		if !globalUnicast6.Contains(ip) {
			return false
		}
		reserved = reserved6
	}
	for _, p := range reserved {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
