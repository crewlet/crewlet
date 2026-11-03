package httpx

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// AN ANSWER THAT CHANGES BETWEEN LOOKUPS IS JUDGED AT EACH CONNECTION, AND THE
// CONNECTION GOES TO THE ADDRESS THAT WAS JUDGED.
//
// The resolver here answers a name with the listener's address the first time
// and another loopback address every time after, which is what a rebinding
// resolver does to a guard that checks a name and then dials it: the check
// sees the first answer and the dial resolves again and gets the second. The
// guard under test admits the listener's address alone, so the first request
// must reach the listener (a dial of the name would have connected to the
// second answer, where nothing listens) and the second, on a connection of its
// own, must be refused without reaching anything.
func TestTheGuardJudgesTheAddressItConnectsToNotTheName(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "listener")
	}))
	t.Cleanup(srv.Close)
	listener := netip.MustParseAddrPort(srv.Listener.Addr().String())

	var lookups atomic.Int64
	dns := dnsServer(func(q dnsmessage.Question) ([]netip.Addr, dnsmessage.RCode) {
		if q.Type != dnsmessage.TypeA {
			return nil, dnsmessage.RCodeSuccess
		}
		if lookups.Add(1) == 1 {
			return []netip.Addr{listener.Addr()}, dnsmessage.RCodeSuccess
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.2")}, dnsmessage.RCodeSuccess
	})
	d := &net.Dialer{Timeout: 5 * time.Second, Resolver: dns.resolver()}
	g := newGuard(d, admitting(func(ip netip.Addr) bool { return ip == listener.Addr() }))
	client := guardedClient(t, g, nil)

	target := "http://rebind.test:" + strconv.Itoa(int(listener.Port())) + "/"
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("the first request, whose answer the guard admits, failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "listener" {
		t.Fatalf("the first request reached %q, want the listener the guard admitted", body)
	}

	resp, err = client.Get(target)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("the second request, whose answer the guard refuses, answered %s", resp.Status)
	}
	if !errors.Is(err, ErrNotPublic) {
		t.Fatalf("the second request failed with %v, want ErrNotPublic", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the listener answered %d requests, want the first alone", n)
	}
}

// A PROXY THE OPERATOR CONFIGURED IS DIALLED AS ITSELF, AND AN ADDRESS IT IS
// ASKED FOR IS STILL JUDGED.
//
// The proxy stands on loopback, which the guard refuses for anything else, and
// a request for a name goes through it. A request whose URL writes an address
// that is not public is refused before the proxy hears of it, since the proxy
// would connect there on the engine's behalf.
func TestTheOperatorsProxyIsDialledAsItselfAndAnAddressItIsAskedForIsJudged(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var asked []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.String())
		mu.Unlock()
		_, _ = io.WriteString(w, "proxied")
	}))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}

	g := newGuard(&net.Dialer{Timeout: 5 * time.Second}, onNetwork)
	client := guardedClient(t, g, http.ProxyURL(proxyURL))

	resp, err := client.Get("http://cdn.example.com/a.js")
	if err != nil {
		t.Fatalf("a request through the operator's proxy on loopback was refused: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "proxied" {
		t.Fatalf("the request answered %q, want the proxy's answer", body)
	}

	for _, target := range []string{"http://169.254.169.254/latest/meta-data/", "http://[::1]:8200/", "http://10.0.0.5/"} {
		resp, err := client.Get(target)
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("GET %s through the proxy answered %s, want a refusal", target, resp.Status)
			continue
		}
		if !errors.Is(err, ErrNotPublic) {
			t.Errorf("GET %s through the proxy failed with %v, want ErrNotPublic", target, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != "http://cdn.example.com/a.js" {
		t.Fatalf("the proxy was asked for %q, want the name alone", asked)
	}
}

// A URL NAMING THE PROXY'S OWN ADDRESS, SENT DIRECT, IS JUDGED.
//
// The process sends the proxy's own host direct (as a `NO_PROXY` covering it
// would), and a request through the proxy has already been dialled. A request
// whose URL names the proxy's address is then dialled through the check and
// refused, because the proxy is trusted for the request that chose it and
// never by its address. The requests run side by side, so no order between
// one request's choice and another's dial can widen the second.
func TestAURLNamingTheProxysAddressDirectlyIsJudged(t *testing.T) {
	t.Parallel()
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.IsAbs() {
			proxied.Add(1)
			_, _ = io.WriteString(w, "proxied")
			return
		}
		_, _ = io.WriteString(w, "reached the proxy directly")
	}))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	direct := func(r *http.Request) (*url.URL, error) {
		if r.URL.Host == proxyURL.Host {
			return nil, nil
		}
		return proxyURL, nil
	}

	g := newGuard(&net.Dialer{Timeout: 5 * time.Second}, onNetwork)
	client := guardedClient(t, g, direct)

	const rounds = 8
	var wg sync.WaitGroup
	errs := make(chan error, 2*rounds)
	for range rounds {
		wg.Add(2)
		go func() {
			defer wg.Done()
			resp, err := client.Get("http://cdn.example.com/a.js")
			if err != nil {
				errs <- err
				return
			}
			_ = resp.Body.Close()
		}()
		go func() {
			defer wg.Done()
			resp, err := client.Get(proxy.URL + "/status")
			if err == nil {
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				errs <- errors.New("GET of the proxy's own address answered " + strconv.Quote(string(body)) + ", want a refusal")
				return
			}
			if !errors.Is(err, ErrNotPublic) {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := proxied.Load(); n != rounds {
		t.Fatalf("the proxy carried %d requests, want %d", n, rounds)
	}
}

// A GUARDED CLIENT REFUSES EVERYTHING WHERE THE GUARD CANNOT BE INSTALLED.
//
// [Customized] answers the shared client when the process's default transport
// is not an [http.Transport]; a guarded client built there must not reach
// anything, since nothing would judge where it connects.
func TestAGuardedClientRefusesEverythingWhereTheGuardCannotBeInstalled(t *testing.T) {
	t.Parallel()
	var reached atomic.Bool
	g := newGuard(&net.Dialer{Timeout: 5 * time.Second}, onNetwork)
	client := g.wrap(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		reached.Store(true)
		return nil, errors.New("the uninstallable transport was used")
	})})
	resp, err := client.Get("http://cdn.example.com/a.js")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a client with no guard installed answered a request")
	}
	if !errors.Is(err, ErrUnguarded) {
		t.Fatalf("the request failed with %v, want ErrUnguarded", err)
	}
	if reached.Load() {
		t.Fatal("the request reached a transport the guard is not installed on")
	}
}

// roundTripFunc is a transport the guard cannot be installed on.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// guardedClient is a client on a clone of the shared transport with g
// installed over proxy, opening a connection per request so each one dials.
func guardedClient(t *testing.T, g *guard, proxy func(*http.Request) (*url.URL, error)) *http.Client {
	t.Helper()
	base, ok := Transport().(*http.Transport)
	if !ok {
		t.Fatal("the shared transport is not an *http.Transport")
	}
	tr := base.Clone()
	tr.Proxy = proxy
	tr.DisableKeepAlives = true
	g.install(tr)
	t.Cleanup(tr.CloseIdleConnections)
	return g.wrap(&http.Client{Transport: tr, Timeout: 10 * time.Second})
}

// admitting is a guard's rule that admits the addresses admit does and asks
// no resolver: a case standing a listener on loopback, which the rule refuses,
// judges by it.
func admitting(admit func(netip.Addr) bool) func(Resolver) judgement {
	return func(Resolver) judgement {
		return func(_ context.Context, ip netip.Addr) (bool, error) { return admit(ip), nil }
	}
}

// dnsServer is a DNS server inside the test, so no query leaves the process:
// it answers each query with the addresses of the question's type it returns
// for it, under the response code it returns.
type dnsServer func(q dnsmessage.Question) ([]netip.Addr, dnsmessage.RCode)

// resolver is a resolver that asks s and nothing else.
func (s dnsServer) resolver() *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go s.serve(server)
		return client, nil
	}}
}

// serve answers the one query on conn. A net.Pipe is no packet connection, so
// the resolver frames its query as it does over TCP, behind a two-byte length,
// and reads the answer so. A query this cannot read goes unanswered, which
// fails the lookup.
func (s dnsServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return
	}
	query := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(conn, query); err != nil {
		return
	}
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return
	}
	q, err := p.Question()
	if err != nil {
		return
	}
	addrs, rcode := s(q)
	msg, err := dnsAnswer(h, q, rcode, addrs)
	if err != nil {
		return
	}
	binary.BigEndian.PutUint16(msg, uint16(len(msg)-2))
	_, _ = conn.Write(msg)
}

// dnsAnswer is the response to q carrying addrs under rcode, behind the two
// bytes its length goes in.
func dnsAnswer(h dnsmessage.Header, q dnsmessage.Question, rcode dnsmessage.RCode, addrs []netip.Addr) ([]byte, error) {
	b := dnsmessage.NewBuilder(make([]byte, 2, 512), dnsmessage.Header{
		ID: h.ID, Response: true, Authoritative: true, RCode: rcode,
		RecursionDesired: h.RecursionDesired, RecursionAvailable: true,
	})
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(q); err != nil {
		return nil, err
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}
	for _, a := range addrs {
		var err error
		switch {
		case a.Is4() && q.Type == dnsmessage.TypeA:
			err = b.AResource(rh, dnsmessage.AResource{A: a.As4()})
		case a.Is6() && q.Type == dnsmessage.TypeAAAA:
			err = b.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: a.As16()})
		}
		if err != nil {
			return nil, err
		}
	}
	return b.Finish()
}

// network is a network's resolver: it answers ipv4only.arpa with the AAAA
// records a DNS64 resolver synthesizes for the network's NAT64 prefixes
// (ipv4only: none unless a case names some, as on a network with no DNS64) or
// with a server failure (ipv4onlyFails), and every other name with hosts.
type network struct {
	hosts         []string
	ipv4only      []string
	ipv4onlyFails bool
}

// answering is a resolver that answers every name but ipv4only.arpa with
// addrs, on a network with no DNS64.
func answering(addrs ...string) *net.Resolver {
	return network{hosts: addrs}.resolver()
}

// resolver is a resolver that asks n's DNS server and nothing else.
func (n network) resolver() *net.Resolver {
	return dnsServer(func(q dnsmessage.Question) ([]netip.Addr, dnsmessage.RCode) {
		raw := n.hosts
		if strings.EqualFold(q.Name.String(), "ipv4only.arpa.") {
			if n.ipv4onlyFails {
				return nil, dnsmessage.RCodeServerFailure
			}
			raw = n.ipv4only
		}
		addrs := make([]netip.Addr, 0, len(raw))
		for _, a := range raw {
			addrs = append(addrs, netip.MustParseAddr(a))
		}
		return addrs, dnsmessage.RCodeSuccess
	}).resolver()
}

// errDialedNothing is what a case's dialer answers in place of a connection
// to an address the guard let through.
var errDialedNothing = errors.New("this case dials nothing")

// stoppedAtConnect is a guard whose dials resolve with r, judge each address
// by the rule on r's network, and hand one the judgement lets through to
// reached where the connection would be made: no connection is ever made. A
// judgement gone wrong so fails the case rather than reaching the network, and
// a public address shows that it got as far as its connect without sending a
// packet.
func stoppedAtConnect(r *net.Resolver, reached func(ctx context.Context, address string) error) *guard {
	return newGuard(&net.Dialer{
		Timeout: guardDialTimeout, KeepAlive: guardKeepAlive, Resolver: r,
		ControlContext: func(ctx context.Context, _, address string, _ syscall.RawConn) error {
			return reached(ctx, address)
		},
	}, onNetwork)
}

// THE GUARD CONNECTS TO NO ADDRESS THAT IS NOT PUBLIC, whether the host is a
// literal or a name: each address is refused at the socket net.Dialer opened
// for it, before the connection is made, so the refusal is a dial error that
// names the address judged. A zone reaches the rule with its address. A
// public address gets as far as its connect.
func TestTheGuardRefusesAnAddressThatIsNotPublic(t *testing.T) {
	t.Parallel()
	g := stoppedAtConnect(answering("10.0.0.5", "fd00::5"),
		func(context.Context, string) error { return errDialedNothing })
	for _, c := range []struct{ addr, judged string }{
		{"127.0.0.1:443", "127.0.0.1:443"},
		{"[::1]:443", "[::1]:443"},
		{"169.254.169.254:80", "169.254.169.254:80"},
		{"[::ffff:127.0.0.1]:443", "127.0.0.1:443"},
		{"[2606:4700::1%1]:443", "[2606:4700::1%1]:443"},
		{"localhost:443", ""},
		{"cdn.example:443", ""},
	} {
		_, err := g.dial(t.Context(), "tcp", c.addr)
		var op *net.OpError
		switch {
		case !errors.Is(err, ErrNotPublic):
			t.Errorf("a dial of %s answered %v, want ErrNotPublic", c.addr, err)
		case !errors.As(err, &op) || op.Op != "dial" || op.Addr == nil:
			t.Errorf("a dial of %s was refused as %v, not at the socket of an address", c.addr, err)
		case c.judged != "" && op.Addr.String() != c.judged:
			t.Errorf("a dial of %s was refused at %s, want %s", c.addr, op.Addr, c.judged)
		}
	}
	if _, err := g.dial(t.Context(), "tcp", "104.16.0.1:443"); !errors.Is(err, errDialedNothing) {
		t.Errorf("a public address answered %v, want it to reach its connect", err)
	}
}

// A NAME WHOSE ANSWER MIXES PUBLIC AND PRIVATE ADDRESSES IS DIALLED AT ITS
// PUBLIC ONES ALONE: each private address is refused at its own socket, and
// the dial moves on to the name's next address rather than giving the name
// up. No connection is made to the private ones, which is the wall.
func TestTheGuardDialsOnlyThePublicAddressesOfAName(t *testing.T) {
	t.Parallel()
	var (
		mu      sync.Mutex
		reached []string
	)
	g := stoppedAtConnect(answering("10.0.0.5", "104.16.0.1", "169.254.169.254"),
		func(_ context.Context, address string) error {
			mu.Lock()
			defer mu.Unlock()
			reached = append(reached, address)
			return errDialedNothing
		})
	if _, err := g.dial(t.Context(), "tcp", "cdn.example:443"); err == nil {
		t.Fatal("a dial that reaches no connect connected")
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(reached, []string{"104.16.0.1:443"}) {
		t.Errorf("the dial reached the connects of %v, want the public address alone", reached)
	}
}

// THE GUARD DIALS A NAME AS net.Dialer DOES, ITS ADDRESS FAMILIES RACING (RFC
// 8305): while the first address it tries has not answered, the other
// family's is tried beside it, so a node whose IPv6 path drops every
// connection reaches a host over IPv4 within net.Dialer's fallback delay
// (300 ms), not after the dial's whole bound. Nothing is sent: the case holds
// the first address's connect until the second reaches its own, or until
// the dialer gives the first up.
func TestTheGuardRacesTheAddressFamiliesOfAName(t *testing.T) {
	t.Parallel()
	var (
		mu      sync.Mutex
		tried   []string
		raced   bool
		beside  = make(chan struct{})
		gaveUp  = make(chan struct{})
		release sync.Once
	)
	g := stoppedAtConnect(answering("104.16.0.1", "2606:4700::1"), func(ctx context.Context, address string) error {
		mu.Lock()
		tried = append(tried, address)
		first := len(tried) == 1
		mu.Unlock()
		if first {
			defer close(gaveUp)
			select {
			case <-beside:
			case <-ctx.Done():
			}
			return errDialedNothing
		}
		select {
		case <-gaveUp:
		default:
			mu.Lock()
			raced = true
			mu.Unlock()
		}
		release.Do(func() { close(beside) })
		return errDialedNothing
	})
	if _, err := g.dial(t.Context(), "tcp", "cdn.example:443"); err == nil {
		t.Fatal("a dial that reaches no connect connected")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tried) != 2 || !raced {
		t.Errorf("the dial tried %v, the second beside the first: %t; want both families, raced", tried, raced)
	}
}

// THE GUARD ASKS THE RESOLVER ITS DIALER RESOLVES WITH FOR THE NETWORK'S NAT64
// PREFIXES, and judges an address under one by the IPv4 address it carries
// there, at its socket like any other. Both of the host's addresses lie in
// 3ffe::/16, global unicast, where the allowlist alone ([Public]) would let the
// one carrying the cloud metadata address through, so only the network's own
// prefix, asked of that resolver, refuses it.
func TestTheGuardJudgesANAT64AddressByTheIPv4AddressItCarries(t *testing.T) {
	t.Parallel()
	var (
		mu      sync.Mutex
		reached []string
	)
	g := stoppedAtConnect(network{
		ipv4only: []string{"3ffe:64:64:96::c000:aa", "3ffe:64:64:96::c000:ab"},
		hosts:    []string{"3ffe:64:64:96::a9fe:a9fe", "3ffe:64:64:96::6810:1"},
	}.resolver(), func(_ context.Context, address string) error {
		mu.Lock()
		defer mu.Unlock()
		reached = append(reached, address)
		return errDialedNothing
	})
	if _, err := g.dial(t.Context(), "tcp", "cdn.example:443"); err == nil {
		t.Fatal("a dial that reaches no connect connected")
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"[3ffe:64:64:96::6810:1]:443"}; !slices.Equal(reached, want) {
		t.Errorf("the dial reached the connects of %v, want %v: 104.16.0.1 under the network's prefix alone",
			reached, want)
	}
}

// THE GUARD ASKS FOR THE NETWORK'S PREFIXES ONLY AT THE SOCKET OF AN ADDRESS
// WHOSE VERDICT THEY CAN CHANGE: an IPv6 address in global unicast or in
// 64:ff9b:1::/48. So a resolver that fails ipv4only.arpa keeps the guard from
// no IPv4 address, nor from one under the well-known prefix, each of which
// reaches its connect; and an address that needs the answer is refused at its
// own socket with the resolver's failure, naming the lookup, never dialled on
// a guess. A name that has both is dialled at the address that needs no
// answer.
func TestTheGuardAsksForTheNetworksPrefixesOnlyWhereTheyCanChangeAVerdict(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		hosts   []string
		reached []string // the connects the dial reaches
		refused string   // the address refused with the resolver's failure, if any
	}{
		{[]string{"104.16.0.1"}, []string{"104.16.0.1:443"}, ""},
		{[]string{"64:ff9b::6810:1"}, []string{"[64:ff9b::6810:1]:443"}, ""},
		{[]string{"2606:4700::1"}, nil, "[2606:4700::1]:443"},
		{[]string{"2606:4700::1", "104.16.0.1"}, []string{"104.16.0.1:443"}, ""},
	} {
		var (
			mu      sync.Mutex
			reached []string
		)
		g := stoppedAtConnect(network{hosts: c.hosts, ipv4onlyFails: true}.resolver(),
			func(_ context.Context, address string) error {
				mu.Lock()
				defer mu.Unlock()
				reached = append(reached, address)
				return errDialedNothing
			})
		_, err := g.dial(t.Context(), "tcp", "cdn.example:443")
		mu.Lock()
		got := slices.Clone(reached)
		mu.Unlock()
		if !slices.Equal(got, c.reached) {
			t.Errorf("a dial of %v, its resolver failing ipv4only.arpa, reached the connects of %v, want %v",
				c.hosts, got, c.reached)
		}
		if c.refused == "" {
			continue
		}
		var op *net.OpError
		if dnsErr, ok := errors.AsType[*net.DNSError](err); !ok || dnsErr.IsNotFound ||
			errors.Is(err, ErrNotPublic) || !strings.Contains(err.Error(), "ipv4only.arpa") ||
			!errors.As(err, &op) || op.Addr == nil || op.Addr.String() != c.refused {
			t.Errorf("a dial of %v, its resolver failing ipv4only.arpa, answered %v, want that failure, naming "+
				"the lookup, at the socket of %s", c.hosts, err, c.refused)
		}
	}
}
