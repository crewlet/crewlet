package vendor

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/crewlet/crewlet/internal/httpx"
)

// errDialedNothing is what a case's CDN dialer answers in place of a
// connection to an address the guard let through.
var errDialedNothing = errors.New("this case dials nothing")

// reachConnect puts reached where g's CDN dialer would connect: the guard
// judges each address first, hands one its judgment lets through to reached,
// and no connection is ever made. A judgment gone wrong so fails the case
// rather than reaching the network, and a public address shows that it got as
// far as its connect without sending a packet.
func reachConnect(g *guard, reached func(ctx context.Context, address string) error) {
	g.cdn.ControlContext = func(ctx context.Context, _, address string, _ syscall.RawConn) error {
		return reached(ctx, address)
	}
}

// dnsServer is a DNS server inside the test, so no query leaves the process.
// It answers ipv4only.arpa with the AAAA records a DNS64 resolver synthesizes
// for the network's NAT64 prefixes (ipv4only: none unless a case names some,
// as on a network with no DNS64) or with a server failure (ipv4onlyFails),
// and every other name with hosts.
type dnsServer struct {
	hosts         []string
	ipv4only      []string
	ipv4onlyFails bool
}

// answering is a resolver that answers every name but ipv4only.arpa with
// addrs, on a network with no DNS64.
func answering(addrs ...string) *net.Resolver {
	return dnsServer{hosts: addrs}.resolver()
}

// resolver is a resolver that asks s and nothing else.
func (s dnsServer) resolver() *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go s.serve(server)
		return client, nil
	}}
}

// serve answers the one query on conn with those of its addresses the query's
// type asks for. A net.Pipe is no packet connection, so the resolver frames
// its query as it does over TCP, behind a two-byte length, and reads the
// answer so. A query this cannot read goes unanswered, which fails the
// lookup.
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
	addrs, rcode := s.hosts, dnsmessage.RCodeSuccess
	if strings.EqualFold(q.Name.String(), "ipv4only.arpa.") {
		addrs = s.ipv4only
		if s.ipv4onlyFails {
			addrs, rcode = nil, dnsmessage.RCodeServerFailure
		}
	}
	b := dnsmessage.NewBuilder(make([]byte, 2, 512),
		dnsmessage.Header{ID: h.ID, Response: true, RecursionAvailable: true, RCode: rcode})
	if b.StartQuestions() != nil || b.Question(q) != nil || b.StartAnswers() != nil {
		return
	}
	rh := dnsmessage.ResourceHeader{Name: q.Name, Type: q.Type, Class: dnsmessage.ClassINET, TTL: 60}
	for _, raw := range addrs {
		addr := netip.MustParseAddr(raw)
		switch {
		case q.Type == dnsmessage.TypeA && addr.Is4():
			err = b.AResource(rh, dnsmessage.AResource{A: addr.As4()})
		case q.Type == dnsmessage.TypeAAAA && addr.Is6():
			err = b.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: addr.As16()})
		}
		if err != nil {
			return
		}
	}
	msg, err := b.Finish()
	if err != nil {
		return
	}
	binary.BigEndian.PutUint16(msg, uint16(len(msg)-2))
	_, _ = conn.Write(msg)
}

// THE GUARD CONNECTS TO NO ADDRESS THAT IS NOT PUBLIC, whether the host is a
// literal or a name: each address is refused at the socket net.Dialer opened
// for it, before the connection is made, so the refusal is a dial error that
// names the address judged. A zone reaches the rule with its address. A
// public address gets as far as its connect, and a proxy the operator
// configured is dialed as itself.
func TestTheGuardRefusesAnAddressThatIsNotPublic(t *testing.T) {
	t.Parallel()
	g := newGuard()
	g.cdn.Resolver = answering("10.0.0.5", "fd00::5")
	reachConnect(g, func(context.Context, string) error { return errDialedNothing })
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
		case !errors.Is(err, httpx.ErrNotPublic):
			t.Errorf("a dial of %s answered %v, want httpx.ErrNotPublic", c.addr, err)
		case !errors.As(err, &op) || op.Op != "dial" || op.Addr == nil:
			t.Errorf("a dial of %s was refused as %v, not at the socket of an address", c.addr, err)
		case c.judged != "" && op.Addr.String() != c.judged:
			t.Errorf("a dial of %s was refused at %s, want %s", c.addr, op.Addr, c.judged)
		}
	}
	if _, err := g.dial(t.Context(), "tcp", "104.16.0.1:443"); !errors.Is(err, errDialedNothing) {
		t.Errorf("a public address answered %v, want it to reach its connect", err)
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

// A NAME WHOSE ANSWER MIXES PUBLIC AND PRIVATE ADDRESSES IS DIALED AT ITS
// PUBLIC ONES ALONE: each private address is refused at its own socket, and
// the dial moves on to the name's next address rather than giving the name
// up. No connection is made to the private ones, which is the wall.
func TestTheGuardDialsOnlyThePublicAddressesOfAName(t *testing.T) {
	t.Parallel()
	g := newGuard()
	g.cdn.Resolver = answering("10.0.0.5", "104.16.0.1", "169.254.169.254")
	var (
		mu      sync.Mutex
		reached []string
	)
	reachConnect(g, func(_ context.Context, address string) error {
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
// connection reaches a CDN over IPv4 within net.Dialer's fallback delay
// (300 ms), not after the dial's whole bound. Nothing is sent: the case holds
// the first address's connect until the second reaches its own, or until
// the dialer gives the first up.
func TestTheGuardRacesTheAddressFamiliesOfAName(t *testing.T) {
	t.Parallel()
	g := newGuard()
	g.cdn.Resolver = answering("104.16.0.1", "2606:4700::1")
	var (
		mu      sync.Mutex
		tried   []string
		raced   bool
		beside  = make(chan struct{})
		gaveUp  = make(chan struct{})
		release sync.Once
	)
	reachConnect(g, func(ctx context.Context, address string) error {
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
// there, at its socket like any other. Both of the CDN's addresses lie in
// 3ffe::/16, global unicast, where the allowlist alone would let the one
// carrying the cloud metadata address through, so only the network's own
// prefix, asked of that resolver, refuses it. A resolver that cannot answer
// for the prefixes fails the dial before any address is tried: a dial is
// never made on a guess.
func TestTheGuardJudgesANAT64AddressByTheIPv4AddressItCarries(t *testing.T) {
	t.Parallel()
	g := newGuard()
	g.cdn.Resolver = dnsServer{
		ipv4only: []string{"3ffe:64:64:96::c000:aa", "3ffe:64:64:96::c000:ab"},
		hosts:    []string{"3ffe:64:64:96::a9fe:a9fe", "3ffe:64:64:96::6810:1"},
	}.resolver()
	var (
		mu      sync.Mutex
		reached []string
	)
	reachConnect(g, func(_ context.Context, address string) error {
		mu.Lock()
		defer mu.Unlock()
		reached = append(reached, address)
		return errDialedNothing
	})
	if _, err := g.dial(t.Context(), "tcp", "cdn.example:443"); err == nil {
		t.Fatal("a dial that reaches no connect connected")
	}
	mu.Lock()
	got := slices.Clone(reached)
	mu.Unlock()
	if want := []string{"[3ffe:64:64:96::6810:1]:443"}; !slices.Equal(got, want) {
		t.Errorf("the dial reached the connects of %v, want %v: 104.16.0.1 under the network's prefix alone", got, want)
	}

	g.cdn.Resolver = dnsServer{hosts: []string{"104.16.0.1"}, ipv4onlyFails: true}.resolver()
	_, err := g.dial(t.Context(), "tcp", "cdn.example:443")
	if dnsErr, ok := errors.AsType[*net.DNSError](err); !ok || dnsErr.IsNotFound ||
		errors.Is(err, httpx.ErrNotPublic) || !strings.Contains(err.Error(), "ipv4only.arpa") {
		t.Errorf("a dial whose resolver failed ipv4only.arpa answered %v, want that failure, naming the lookup", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(reached, got) {
		t.Errorf("a dial whose resolver failed ipv4only.arpa reached the connects of %v", reached[len(got):])
	}
}
