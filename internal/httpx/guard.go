package httpx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrNotPublic is an address the rule refuses: one that is not on the public
// internet. A connection [Guarded] refused hands it back wrapped in the
// url.Error and net.OpError of the request, naming the address it refused, and
// errors.Is finds it through both, so a caller tells "this address is not
// allowed" from "this address did not answer". [ResolvePublic] answers it for
// a host that is, or resolves to, such an address. An address the rule could
// not judge, because the network's resolver failed the lookup it needed
// ([Rule.Public]), is refused with that failure instead, never with this.
var ErrNotPublic = errors.New("httpx: the address is not on the public internet")

// The guarded dialer's bounds. They are the shared transport's own, the ones
// net/http's default dialer carries, because the guard changes WHICH addresses
// are dialled and nothing about how long a dial may take: the whole request is
// bounded by the timeout its caller hands [Guarded], which is the patience the
// caller chose for what it is asking. The one lookup the rule may make for a
// dial (the network's NAT64 prefixes, [Rule.Public]) runs inside the share of
// the dial's bound net.Dialer gives the address that needed it, so it adds no
// bound of its own.
const (
	guardDialTimeout = 30 * time.Second
	guardKeepAlive   = 30 * time.Second
)

// Guarded builds a client whose every connection reaches only a public
// address: a clone of [Transport] whose dialer refuses, at the moment it
// connects, any address the rule refuses on the network the node is on
// ([Rule]). It is the one client a request to a
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
// Judged there, the dial stays net.Dialer's own: it resolves the name, races
// its IPv6 addresses against its IPv4 ones (RFC 8305: the second family starts
// 300 ms after the first, at once if the first fails) and splits its bound
// across a family's addresses. A dialer that resolved the name itself and
// handed net.Dialer one judged address at a time would forgo all of it: each
// address would take the whole bound, so on a node whose IPv6 path drops
// connections, the IPv6 addresses the resolver orders first (RFC 6724) would
// spend the request's time before IPv4 was tried at all.
//
// # The network's NAT64 prefixes
//
// Each dial makes a [Rule] of its own on the resolver its dialer resolves the
// name with, the process's own ([NewRule]): on a network that reaches IPv4
// through a translator, a host with no IPv6 address of its own is dialled at
// an address under the network's prefix, judged by the IPv4 address it
// carries. The rule asks for the prefixes at most once a dial, at the socket of
// the first address whose verdict they can change ([Rule.Public] says which),
// so an IPv4 address's connect waits on no lookup, and a dial that tries none
// costs none. When the resolver cannot answer for them, each address that
// needed the answer is refused at its socket with the resolver's failure,
// never connected to on a guess, and the dial goes on to the name's addresses
// that need none.
//
// # A proxy the operator configured is dialled as itself
//
// The clone keeps the process's proxy settings (the rule [Transport] states),
// and the proxy's own address is dialled without the check, since a proxy on a
// private address is the operator's choice rather than the remote party's. The
// target the proxy is asked for is judged where this process can judge it: an
// address written into the URL must be public by the same rule on this node's
// network, and is refused before the proxy hears of it. A NAME the proxy
// resolves itself, so no answer this process got would be the address the
// proxy connects to, and a check here would be a guess a rebinding answer
// defeats. An operator whose proxy reaches internal
// addresses holds the proxy to this rule; [ErrNotPublic] cannot be its wall.
//
// THE PROXY IS TRUSTED FOR THE REQUEST THAT CHOSE IT, never by its address.
// Each request is marked on its way into the transport, and only a dial for a
// request the transport sent through a proxy, to that proxy's address, skips
// the check. A URL that names the proxy's own address on a host the process
// sends direct (`NO_PROXY`) is dialled through the check like any other, and
// no request's choice can widen another's.
//
// # It fails closed
//
// A process whose default transport is not an [http.Transport] (a test harness
// or an instrumented build) has nothing the guard can be installed on, and
// [Customized] answers the shared client there. A guarded client never does:
// it refuses every request with [ErrUnguarded] instead.
//
// # What it is not for
//
// An address the OPERATOR configured (an MCP server's url, a webhook sink, a
// self-hosted vendor) is dialled through [Client]: a private or loopback
// address there is a deployment the operator chose, and refusing it would
// break the deployments the docs describe.
func Guarded(timeout time.Duration) *http.Client {
	g := newGuard(&net.Dialer{Timeout: guardDialTimeout, KeepAlive: guardKeepAlive}, onNetwork)
	return g.wrap(Customized(timeout, g.install))
}

// ErrUnguarded is a guarded client's answer to every request in a process
// whose default transport the guard cannot be installed on.
var ErrUnguarded = errors.New("httpx: the public-address guard could not be installed on this process's transport")

// guard is the dialer behind [Guarded].
type guard struct {
	// checked dials every address but a proxy's, each dial on a copy of it
	// whose ControlContext judges the address ([guard.dial]); plain dials the
	// proxies the transport chose.
	checked, plain *net.Dialer

	// then is the ControlContext the dialer handed to [newGuard] carried, run
	// after the judgement on an address it lets through: none, outside this
	// package's own tests, which stop a dial at its connect.
	then control

	// ruleOn makes the judgement one dial asks of each address, on the
	// network resolver resolves on: [onNetwork], always, outside this
	// package's own tests, which stand a listener on loopback and need an
	// admitted address to reach it.
	ruleOn func(Resolver) judgement
}

// control is the shape of net.Dialer's ControlContext.
type control = func(ctx context.Context, network, address string, c syscall.RawConn) error

// judgement is one dial's verdict on an address it is about to connect to, or
// the failure that kept the address from being judged: [Rule.Public]'s shape.
type judgement func(ctx context.Context, ip netip.Addr) (bool, error)

// onNetwork is the rule a guarded dial judges by: one [Rule] a dial, on the
// resolver the dial resolves its name with, so the network's own NAT64
// prefixes are asked of the network that translates through them.
func onNetwork(r Resolver) judgement { return NewRule(r).Public }

// newGuard builds the dialer, d's settings shared by both of its halves.
func newGuard(d *net.Dialer, ruleOn func(Resolver) judgement) *guard {
	plain, checked := *d, *d
	checked.Control, checked.ControlContext = nil, nil
	return &guard{plain: &plain, checked: &checked, then: d.ControlContext, ruleOn: ruleOn}
}

// resolver is the resolver the checked dialer resolves a name with: its own,
// or the process's where it names none.
func (g *guard) resolver() Resolver {
	if g.checked.Resolver != nil {
		return g.checked.Resolver
	}
	return net.DefaultResolver
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
		if err := g.judgeTarget(r.Context(), r.URL); err != nil {
			return nil, err
		}
		if rt, ok := r.Context().Value(routeKey{}).(*route); ok {
			rt.via(proxyAddr(u))
		}
		return u, nil
	}
	t.DialContext = g.dial
}

// wrap finishes a client [Customized] built with [guard.install]: each request
// is marked so a dial knows the proxy its own request chose, and a client
// whose transport the guard could not be installed on refuses everything.
func (g *guard) wrap(c *http.Client) *http.Client {
	t, ok := c.Transport.(*http.Transport)
	if !ok {
		c.Transport = refusing{}
		return c
	}
	c.Transport = marking{t}
	return c
}

// routeKey is the context key a request's [route] travels under.
type routeKey struct{}

// route is one request's way out: the address of the proxy the transport
// chose for it, empty when it goes direct.
//
// The transport asks for the proxy before it dials and dials under the
// request's own context values, so the dial reads what the same request's
// choice wrote, and nothing another request chose.
type route struct{ proxy atomic.Pointer[string] }

func (r *route) via(addr string) { r.proxy.Store(&addr) }

func (r *route) proxied(addr string) bool {
	p := r.proxy.Load()
	return p != nil && *p == addr
}

// marking is the guarded transport: every request enters it with a fresh
// [route].
type marking struct{ t *http.Transport }

func (m marking) RoundTrip(r *http.Request) (*http.Response, error) {
	return m.t.RoundTrip(r.WithContext(context.WithValue(r.Context(), routeKey{}, new(route))))
}

// CloseIdleConnections lets [http.Client.CloseIdleConnections] reach the pool.
func (m marking) CloseIdleConnections() { m.t.CloseIdleConnections() }

// refusing is a guarded client's transport where the guard has nothing to be
// installed on.
type refusing struct{}

func (refusing) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_ = r.Body.Close()
	}
	return nil, ErrUnguarded
}

// judgeTarget refuses a target a proxy would be asked for when the URL writes
// its address and that address is not public on this node's network, or
// cannot be judged there. A host that does not parse as an address is a name,
// which the proxy resolves.
func (g *guard) judgeTarget(ctx context.Context, u *url.URL) error {
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil {
		return nil
	}
	public, err := g.ruleOn(g.resolver())(ctx, ip)
	switch {
	case err != nil:
		return err
	case !public:
		return fmt.Errorf("%w: %s", ErrNotPublic, ip)
	}
	return nil
}

// proxyAddr is the address the transport dials for the proxy u, spelled as it
// spells it: the host and the scheme's port when u names none. A spelling that
// differs (a host outside ASCII, which the transport converts) only sends the
// proxy's dial through the check, so it cannot widen anything.
func proxyAddr(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	port := "80"
	switch u.Scheme {
	case "https":
		port = "443"
	case "socks5", "socks5h":
		port = "1080"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// dial dials the proxy a request's transport chose as itself, and everything
// else through a copy of the checked dialer whose ControlContext judges each
// address with a rule of the dial's own, made on the resolver the copy
// resolves with ([guard.judging]).
func (g *guard) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if rt, ok := ctx.Value(routeKey{}).(*route); ok && rt.proxied(addr) {
		return g.plain.DialContext(ctx, network, addr)
	}
	d := *g.checked
	d.ControlContext = g.judging(g.ruleOn(g.resolver()))
	return d.DialContext(ctx, network, addr)
}

// judging is one dial's ControlContext. net.Dialer calls it with each address
// it is about to connect to, once the socket for it exists and before the
// connection is made, so the address judged is the address connected to
// whatever a resolver answered, and an address refused here is sent nothing:
// one that is not public with [ErrNotPublic], and one the rule could not judge
// with the failure of the lookup that names the network's prefixes.
// net.Dialer then moves on to the name's next address, and when every address
// fails it answers with the first one's error, inside a *net.OpError naming
// that address.
func (g *guard) judging(judge judgement) control {
	return func(ctx context.Context, network, address string, c syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			// net.Dialer formats the address itself. One this cannot read is
			// refused rather than connected to unjudged.
			return fmt.Errorf("%w: %s is not an address this guard can judge", ErrNotPublic, address)
		}
		public, err := judge(ctx, ap.Addr())
		switch {
		case err != nil:
			return err
		case !public:
			return fmt.Errorf("%w: %s", ErrNotPublic, ap.Addr())
		case g.then == nil:
			return nil
		}
		return g.then(ctx, network, address, c)
	}
}

// Public reports whether ip is an address on the public internet on a network
// that names no NAT64 prefix of its own: not loopback, private, link-local
// (the cloud metadata address among them), multicast, unspecified,
// carrier-grade NAT, documentation or any other block the IANA special-purpose
// registries mark as not globally reachable. An IPv4 address written in IPv6
// form, or behind the well-known NAT64 prefix, is judged as the IPv4 address it
// is. An IPv6 address carrying a zone is never public: a zone names one of
// this host's interfaces, which only a scoped address needs. public.go holds
// the blocks and the reason for each.
//
// IT IS THE VERDICT FOR A CALLER WITH AN ADDRESS AND NO DIAL to judge it at:
// whether the public internet routes an MCP server's configured address
// (internal/config), whether github.com can deliver to a webhook's
// (internal/github). Neither asks on behalf of a network a translator serves,
// so neither has a resolver to ask for prefixes. A dialer never asks it:
// [Guarded] judges each socket's address by a [Rule] on the network it dials
// from, and [ResolvePublic] a host by one on the resolver it resolves with,
// because only that network can say which NAT64 prefixes it translates
// through. Judged without them, an address under a network-specific prefix
// inside global unicast that carries 10.0.0.5 is called public by the
// allowlist alone, though that network's translator delivers it to the
// private address.
func Public(ip netip.Addr) bool {
	return public(ip, nil)
}
