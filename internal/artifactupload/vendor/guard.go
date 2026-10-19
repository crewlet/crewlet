package vendor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// Guarded is the client a cache fetches through by default: the process's one
// transport (internal/httpx), cloned with a dialer that resolves a host itself,
// refuses every address that is not public, and dials the address it checked.
//
// # Why, when the hosts are a fixed list
//
// The hosts are public CDNs reached over verified TLS, so a hostile DNS answer
// pointing one at an internal service would already fail the certificate
// check. The dialer is the second wall: the fetch is the one request the
// engine makes to a URL a page wrote, and nothing about that request may ever
// reach the network the engine runs in, whatever a resolver answers. Checking
// the address it dials, rather than the name, is what keeps a resolver that
// answers twice (once public, once internal) from slipping past the check.
//
// A PROXY THE OPERATOR CONFIGURED IS DIALED AS ITSELF: the transport keeps the
// process's proxy settings (httpx's rule), the proxy resolves the CDN's name,
// and a proxy on a private address is the operator's own choice rather than a
// page's.
func Guarded() *http.Client {
	g := &guard{dialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
		proxies: map[string]bool{}}
	return httpx.Customized(FetchTimeout, func(t *http.Transport) {
		base := t.Proxy
		t.Proxy = func(r *http.Request) (*url.URL, error) {
			if base == nil {
				return nil, nil
			}
			u, err := base(r)
			if u != nil {
				g.trust(u)
			}
			return u, err
		}
		t.DialContext = g.dial
	})
}

// guard is the dialer behind [Guarded].
type guard struct {
	dialer *net.Dialer

	mu      sync.Mutex
	proxies map[string]bool
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

// errPrivateAddress is a dial the guard refused.
var errPrivateAddress = errors.New("vendor: the CDN resolved to an address that is not public")

// dial resolves addr, refuses it unless every address it names is public, and
// dials the first that answers.
func (g *guard) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	g.mu.Lock()
	proxied := g.proxies[addr]
	g.mu.Unlock()
	if proxied {
		return g.dialer.DialContext(ctx, network, addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else {
		resolved, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		addrs = resolved
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("vendor: %s resolved to no address", host)
	}
	for _, ip := range addrs {
		if !Public(ip) {
			return nil, fmt.Errorf("%w: %s is %s", errPrivateAddress, host, ip)
		}
	}
	var last error
	for _, ip := range addrs {
		conn, err := g.dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

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
// multicast, unspecified, carrier-grade NAT or any other reserved range. An
// IPv4 address written in IPv6 form is judged as the IPv4 address it is.
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
