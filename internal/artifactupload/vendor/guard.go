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
// refuses every address that is not public by the engine's one rule
// ([httpx.Public]), and dials the address it checked.
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
		if !httpx.Public(ip) {
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
