package vendor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"syscall"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// The bounds of the guarded client's dials.
const (
	// dialTimeout bounds one dial: the network's NAT64 prefixes asked of the
	// resolver, and every address a name resolves to. net.Dialer races the
	// name's address families inside it and splits what is left of it across
	// a family's addresses, at least two seconds each. It is FetchTimeout
	// because a connection is dialed for a fetch that gives up at
	// FetchTimeout, and it is the only bound a dial has: net/http dials apart
	// from the request's own deadline, so that the connection can serve a
	// later request, and a dial with no bound of its own would wait out the
	// kernel's connect retries on an address that never answers (about two
	// minutes at Linux's default net.ipv4.tcp_syn_retries).
	dialTimeout = FetchTimeout

	// dialKeepAlive is how often an idle connection to a CDN or a proxy is
	// probed: net/http's default dialer's period, which every client on the
	// process's shared transport (httpx.Transport, its clone) is dialed
	// with. The guard changes which addresses a connection may reach, not how
	// a connection is kept.
	dialKeepAlive = 30 * time.Second
)

// Guarded is the client a cache fetches through by default: the process's one
// transport (internal/httpx), cloned with a dialer that connects only to an
// address the engine's one rule calls public on the network the node is on
// ([httpx.Rule]), judging each at the socket net.Dialer opens for it, before
// the connection is made.
//
// # Why, when the hosts are a fixed list
//
// The hosts are public CDNs reached over verified TLS, so a hostile DNS answer
// pointing one at an internal service would already fail the certificate
// check. The dialer is the second wall: the fetch is the one request the
// engine makes to a URL a page wrote, and nothing about that request may ever
// reach the network the engine runs in, whatever a CDN's DNS answers. Judging
// the address each connection is made to, rather than the name, is what keeps
// a resolver that answers twice (once public, once internal) from slipping
// past the judgment.
//
// # Why at the socket
//
// Judged there, the dial is net.Dialer's own: it resolves the name, races its
// IPv6 addresses against its IPv4 ones (RFC 8305: the second family starts
// 300 ms after the first, at once if the first fails) and splits its bound
// across a family's addresses. A dialer that resolved the name itself and
// handed net.Dialer one judged address at a time would forgo all of it: each
// address would take the whole bound, so on a node whose IPv6 path drops
// connections, the IPv6 addresses the resolver orders first (RFC 6724) would
// spend the fetch's time before IPv4 was tried at all. A name whose answer
// mixes public and private addresses is dialed at its public ones alone: no
// connection is made to the others, which is all the wall asks.
//
// # The network's NAT64 prefixes
//
// The rule's NAT64 prefixes are asked of the resolver the CDN's name is
// resolved with, the process's own (net.DefaultResolver), once a dial and
// before it: on a network that reaches IPv4 through a translator, a CDN with
// no IPv6 address of its own is dialed at an address under the network's
// prefix, judged by the IPv4 address it carries. A resolver that cannot
// answer for them fails the dial, which is never made on a guess.
//
// A PROXY THE OPERATOR CONFIGURED IS DIALED AS ITSELF: the transport keeps the
// process's proxy settings (httpx's rule), the proxy resolves the CDN's name,
// and a proxy on a private address is the operator's own choice rather than a
// page's.
func Guarded() *http.Client {
	g := newGuard()
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
	// proxy dials a proxy the operator configured, as itself.
	proxy *net.Dialer

	// cdn is what every other host is dialed with: net.Dialer resolves the
	// host through its Resolver (the process's own when nil) and races its
	// addresses. Each dial copies it and puts the network's rule in front
	// of its ControlContext (judging), so an address that is not public is
	// refused before the connection to it is made.
	cdn *net.Dialer

	mu      sync.Mutex
	proxies map[string]bool
}

// newGuard builds the dialer behind [Guarded].
func newGuard() *guard {
	return &guard{
		proxy:   &net.Dialer{Timeout: dialTimeout, KeepAlive: dialKeepAlive},
		cdn:     &net.Dialer{KeepAlive: dialKeepAlive},
		proxies: map[string]bool{},
	}
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

// dial dials a proxy the transport chose as itself. Every other host it dials
// through a copy of the CDN dialer that connects only to an address the
// network's rule, asked of the resolver the dialer resolves with, calls
// public. One bound, dialTimeout, covers the rule's discovery and the dial.
func (g *guard) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	g.mu.Lock()
	proxied := g.proxies[addr]
	g.mu.Unlock()
	if proxied {
		return g.proxy.DialContext(ctx, network, addr)
	}
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	resolver := g.cdn.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	rule, err := httpx.DiscoverRule(ctx, resolver)
	if err != nil {
		return nil, fmt.Errorf("vendor: %w", err)
	}
	cdn := *g.cdn
	cdn.ControlContext = judging(rule, g.cdn.ControlContext)
	return cdn.DialContext(ctx, network, addr)
}

// control is the shape of net.Dialer's ControlContext.
type control = func(ctx context.Context, network, address string, c syscall.RawConn) error

// judging is the CDN dialer's ControlContext for one dial. net.Dialer calls it
// with each address it is about to connect to, once the socket for it exists
// and before the connection is made, so the address judged is the address
// connected to whatever a resolver answered, and an address refused here is
// sent nothing. net.Dialer then moves on to the name's next address, and when
// every address fails it answers with the first one's error, inside a
// *net.OpError naming that address. An address rule lets through goes on to
// then, the ControlContext the CDN dialer already carried (none, outside a
// test that stops each dial at its connect).
func judging(rule httpx.Rule, then control) control {
	return func(ctx context.Context, network, address string, c syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			// net.Dialer formats the address itself. One this cannot read is
			// refused rather than connected to unjudged.
			return fmt.Errorf("%w: %q cannot be read as an address: %w", httpx.ErrNotPublic, address, err)
		}
		if !rule.Public(ap.Addr()) {
			return fmt.Errorf("%w: %s", httpx.ErrNotPublic, ap.Addr())
		}
		if then == nil {
			return nil
		}
		return then(ctx, network, address, c)
	}
}
