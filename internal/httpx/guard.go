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

// Guarded builds the one client for a URL a REMOTE party named (ADR-0072): a
// clone of [Transport] whose dialer refuses any address [Rule] refuses, with
// [ErrNotPublic].
// The address is judged in net.Dialer's Control hook, on the socket's own
// address, so a resolver answering twice cannot slip a private answer past a
// name check; the dial keeps net.Dialer's racing. NAT64 prefixes are asked at
// most once a dial; unavailable, an address needing them is refused. The
// operator's proxy is dialled unchecked, only for the request that chose it;
// an IP in the URL is judged before the proxy hears of it, a NAME the proxy
// resolves is not. Not an [http.Transport]: fails closed ([ErrUnguarded]).
// An OPERATOR-configured address goes through [Client].
// address goes through [Client].
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
// which the proxy resolves: the parse failing is that verdict, not an error to
// pass on, so only a written address reaches [guard.judgeAddr].
func (g *guard) judgeTarget(ctx context.Context, u *url.URL) error {
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		return g.judgeAddr(ctx, ip)
	}
	return nil
}

// judgeAddr refuses ip when this node's rule, made on the resolver the guard
// dials with, finds it not public, or cannot judge it.
func (g *guard) judgeAddr(ctx context.Context, ip netip.Addr) error {
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
// carrier-grade NAT, documentation or any other block IANA marks not globally
// reachable. IPv4 in IPv6 form, or behind the well-known NAT64 prefix, is
// judged as the IPv4 address; an IPv6 zone is never public (public.go).
//
// IT IS THE VERDICT FOR A CALLER WITH AN ADDRESS AND NO DIAL (internal/config,
// internal/github), with no resolver for NAT64 prefixes. A dialer never asks
// it: [Guarded] and [ResolvePublic] use a [Rule] on their own network, since
// only it knows its prefixes; an address under one carrying 10.0.0.5 passes
// here though the translator delivers it to a private address.
func Public(ip netip.Addr) bool {
	return public(ip, nil)
}
