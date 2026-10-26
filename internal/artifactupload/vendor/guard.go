package vendor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// Guarded is the client a cache fetches through by default: the process's one
// transport (internal/httpx), cloned with a dialer that resolves a host itself,
// refuses it unless every address it resolves to is public by the engine's one
// rule ([httpx.ResolvePublic]), and dials an address it judged.
//
// # Why, when the hosts are a fixed list
//
// The hosts are public CDNs reached over verified TLS, so a hostile DNS answer
// pointing one at an internal service would already fail the certificate
// check. The dialer is the second wall: the fetch is the one request the
// engine makes to a URL a page wrote, and nothing about that request may ever
// reach the network the engine runs in, whatever a CDN's DNS answers. Checking
// the address it dials, rather than the name, is what keeps a resolver that
// answers twice (once public, once internal) from slipping past the check.
//
// The rule's NAT64 prefixes are asked of the same resolver the host is
// resolved with, the process's own (net.DefaultResolver): on a network that
// reaches IPv4 through a translator, a CDN with no IPv6 address of its own is
// dialed at an address under the network's prefix, judged by the IPv4 address
// it carries. A resolver that cannot answer for them fails the fetch.
//
// A PROXY THE OPERATOR CONFIGURED IS DIALED AS ITSELF: the transport keeps the
// process's proxy settings (httpx's rule), the proxy resolves the CDN's name,
// and a proxy on a private address is the operator's own choice rather than a
// page's.
func Guarded() *http.Client {
	g := &guard{dialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
		resolver: net.DefaultResolver, proxies: map[string]bool{}}
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

	// resolver resolves a host and names the network's NAT64 prefixes, both
	// for the one judgment, so the two answers come from the same place.
	resolver httpx.Resolver

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
	addrs, err := httpx.ResolvePublic(ctx, g.resolver, host)
	if err != nil {
		return nil, fmt.Errorf("vendor: %w", err)
	}
	var last error
	for _, ip := range addrs {
		conn, err := g.dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}
