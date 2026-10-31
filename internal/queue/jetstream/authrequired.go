package jetstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

// ErrUnauthenticatedBroker is the refusal of an external NATS server that
// authenticates nobody — see [requiresAuthentication].
var ErrUnauthenticatedBroker = errors.New(
	"jetstream: an external NATS server must authenticate its clients")

// requiresAuthentication refuses an external server whose INFO line — info,
// the first the connection read, nil where it read none — says it admits a
// client that presents nothing. server is [serverOf]'s name for it.
//
// # Why the engine refuses it at all
//
// The stream is this company's write-ahead log and its coordination store:
// every event, every seat's mail, every state-log record and every lease.
// Signing (ADR-0031) keeps a forged record from being applied, but anything a
// server admits reads all of it and writes every subject an unsigned message
// rides. An external cluster's admission is its operator's — and a server
// that admits anybody makes everything that can reach its port a reader and
// a writer of this company. Tier A refuses a node that presents nothing
// (config's Stream.presentsCredential); this is the half only the server can
// answer, since a node presenting a token to a server that does not ask for
// one is admitted all the same, and so is everybody else.
//
// # What admits it
//
// `auth_required`, which nats-server sets wherever it holds any authorization
// — a token, users, nkeys, an operator, a custom authenticator — and clears
// where it binds an anonymous client to a no_auth_user. Or `tls_required`
// AND `tls_verify` together: every client must then present a certificate
// the server verifies before it says anything.
//
// NOT A PRESENTED CERTIFICATE. A server holding `tls { }` without `verify`
// announces neither auth_required nor tls_verify, and admits a client that
// presents nothing — measured against v2.15.1 — whatever this node presents.
// TLS settings never set auth_required (configureAuthorization counts none of
// them), which is why tls_verify is read beside it.
//
// NOT tls_verify ON ITS OWN. `tls_verify` with `tls_required` false is the
// mixed mode `allow_non_tls`: a plaintext client presents no certificate and
// is admitted.
//
// # Where the INFO comes from
//
// nats.go parses no tls_verify, so the line is read off the wire by the dial's
// recording dialer ([infoDialer]): the NATS protocol sends INFO in plaintext,
// before any TLS upgrade, as the first line of every connection. An INFO the
// dialer did not see is a refusal saying so, never a pass: a check that
// passed on what it could not read would admit exactly the server it exists
// to refuse.
//
// # What it cannot see
//
// The server this dial reached, as [carriesTheContract] judges it: a client
// dialling a cluster reconnects to whichever member answers, so the remedy
// names every server of the cluster.
func requiresAuthentication(server string, info []byte) error {
	if info == nil {
		return fmt.Errorf("%w: this node read no INFO from %s before its connection "+
			"completed, so whether that server authenticates its clients cannot be "+
			"told. The engine reads it on the plaintext NATS handshake over TCP, "+
			"which a server that starts with TLS (handshake_first) or a proxy in "+
			"front of it does not send: dial the NATS servers themselves",
			ErrUnauthenticatedBroker, server)
	}
	var announced struct {
		AuthRequired bool `json:"auth_required"`
		TLSRequired  bool `json:"tls_required"`
		TLSVerify    bool `json:"tls_verify"`
	}
	body, ok := bytes.CutPrefix(info, []byte("INFO "))
	if !ok || json.Unmarshal(bytes.TrimSpace(body), &announced) != nil {
		return fmt.Errorf("%w: the first line %s sent is not an INFO this node can "+
			"read, so whether that server authenticates its clients cannot be told: "+
			"dial a NATS server's client port",
			ErrUnauthenticatedBroker, server)
	}
	if announced.AuthRequired || (announced.TLSRequired && announced.TLSVerify) {
		return nil
	}
	return fmt.Errorf("%w: %s requires no authentication (it admits anonymous "+
		"clients, or binds them to a no_auth_user), so anything that reaches it "+
		"reads and writes this company: configure authorization, or "+
		"tls { verify: true } without allow_non_tls, on every server of that cluster",
		ErrUnauthenticatedBroker, server)
}

// heldToAuthentication hands nc back where info admits it, and closes it and
// answers the refusal where it does not — [heldToTheContract]'s shape.
func heldToAuthentication(nc *nats.Conn, info []byte) (*nats.Conn, error) {
	if err := requiresAuthentication(serverOf(nc, false), info); err != nil {
		nc.Close()
		return nil, err
	}
	return nc, nil
}

// infoLineMax bounds the INFO line a recorded connection keeps.
//
// 64 KiB, against what a server sends: an INFO is its fixed fields plus one
// connect_urls entry — and, with websocket enabled, one ws_connect_urls entry
// — per member it advertises. The fixed fields measured 419 bytes on a
// standalone nats-server v2.15.1 with JetStream and a token (externaltest's
// server, 2026-10-31), and stay under a kilobyte with a cluster's name, a
// domain, a nonce, a git commit and the TLS flags beside them; an entry is
// `"host:port",`, about twenty bytes for the pod IP a member advertises by
// default and at most 262 for the longest DNS name (253 bytes) a
// client_advertise can give, so 64 KiB holds a cluster of more than a hundred
// members advertising both lists by their longest names, and of thousands by
// IP. nats.go reads the line with no bound of its own. A line past this is not
// kept, and its connection is refused as one whose INFO was not read rather
// than judged on a prefix.
const infoLineMax = 64 << 10

// infoDialer is the external dial's TCP dialer: it dials one member of the
// pool within the attempt's budget, and keeps the first line every connection
// it opens reads — the server's INFO, which carries the tls_verify nats.go
// does not parse ([requiresAuthentication]).
//
// # Why it resolves the member's name itself
//
// A custom dialer replaces the client's, and the client applies its budget
// only to its own: without one, nats.go resolves a member's hostname to every
// address it has and shortens its dialer's timeout to the budget divided by
// their number, so one attempt at a member takes the budget IN TOTAL. Handed
// a custom dialer it hands that dialer each resolved address in turn, and
// each would take the whole budget: N × 2 s on the common Kubernetes shape,
// one stream.url name (a headless or round-robin service) resolving to every
// pod of the NATS cluster — 6 s an attempt for three members, 10 s for five,
// on every pass of the reconnect loop, against the lease TTL the coordination
// store riding this connection renews (see [externalHandshake]). So the dial
// asks the client to skip its lookup (nats.SkipHostLookup) and does what the
// client did: resolve the host, shuffle the addresses — the client's
// spreading of its connections across the members behind one name, which
// Go's own serial dial over a name's addresses does not do — and dial each
// with an equal share of the budget, stopping at the first that answers. The
// lookup is bounded by the same figure, where the client's was not bounded.
//
// # Which INFO it answers for
//
// The NEWEST connection it opened. A connect that tries a cluster's members
// in turn opens one per attempt, and the one it completed on is the last; a
// reconnect later opens more, and the dial asks before it hands the
// connection to anybody.
type infoDialer struct {
	// budget bounds the lookup of a member's name, and then the dials to
	// its addresses together.
	budget time.Duration
	// lookup and connect are the resolver and the TCP dial: a seam, so a
	// test can stand a name with several silent addresses in for DNS and a
	// partition.
	lookup  func(ctx context.Context, host string) ([]string, error)
	connect func(ctx context.Context, network, address string) (net.Conn, error)

	mu     sync.Mutex
	newest *infoConn
}

// newInfoDialer is the dialer an external dial goes through, each attempt at
// a member bounded by budget.
func newInfoDialer(budget time.Duration) *infoDialer {
	var tcp net.Dialer
	return &infoDialer{
		budget:  budget,
		lookup:  net.DefaultResolver.LookupHost,
		connect: tcp.DialContext,
	}
}

// Dial opens one connection to the member at address and makes it the one
// [infoDialer.info] answers for.
func (d *infoDialer) Dial(network, address string) (net.Conn, error) {
	c, err := d.dialMember(network, address)
	if err != nil {
		return nil, err
	}
	recorded := &infoConn{Conn: c}
	d.mu.Lock()
	d.newest = recorded
	d.mu.Unlock()
	return recorded, nil
}

// dialMember dials the member at address — host:port, as the client hands it
// with its own lookup skipped — within one attempt's budget: see
// [infoDialer].
func (d *infoDialer) dialMember(network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addrs := []string{host}
	if net.ParseIP(host) == nil {
		ctx, cancel := context.WithTimeout(context.Background(), d.budget)
		addrs, err = d.lookup(ctx, host)
		cancel()
		if err != nil {
			return nil, err
		}
		if len(addrs) == 0 {
			return nil, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
		}
		rand.Shuffle(len(addrs), func(i, j int) { addrs[i], addrs[j] = addrs[j], addrs[i] })
	}
	share := d.budget / time.Duration(len(addrs))
	for _, addr := range addrs {
		ctx, cancel := context.WithTimeout(context.Background(), share)
		var c net.Conn
		// Cancelled once the dial returns, which leaves a connection it
		// made be: the context bounds the dial, never the connection.
		c, err = d.connect(ctx, network, net.JoinHostPort(addr, port))
		cancel()
		if err == nil {
			return c, nil
		}
	}
	// The last address's failure, as the client answered for its own.
	return nil, err
}

// info is the INFO line the newest connection read, or nil where it read
// none (or none within [infoLineMax]).
func (d *infoDialer) info() []byte {
	d.mu.Lock()
	newest := d.newest
	d.mu.Unlock()
	if newest == nil {
		return nil
	}
	return newest.line()
}

// infoConn keeps the first line read through it, and passes every read
// through untouched — the TLS upgrade nats.go makes after INFO wraps this
// connection, so what follows the line is ciphertext it never keeps.
type infoConn struct {
	net.Conn

	// done is set once the line is complete or past [infoLineMax], so every
	// later read skips the lock: the client's read loop reads through here
	// for the connection's whole life.
	done atomic.Bool

	mu    sync.Mutex
	read  []byte
	first []byte
}

func (c *infoConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && !c.done.Load() {
		c.keep(p[:n])
	}
	return n, err
}

// keep adds b to the line read so far. The line is complete at its CRLF, and
// what follows that in the same read is never kept; a line that runs past
// [infoLineMax], whether its CRLF has arrived or not, is kept as none.
func (c *infoConn) keep(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.read = append(c.read, b...)
	if i := bytes.Index(c.read, []byte("\r\n")); i >= 0 && i <= infoLineMax {
		c.first = bytes.Clone(c.read[:i])
		c.read = nil
		c.done.Store(true)
		return
	}
	if len(c.read) > infoLineMax {
		c.read = nil
		c.done.Store(true)
	}
}

// line is the first complete line read, nil before one is or where it ran
// past [infoLineMax].
func (c *infoConn) line() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.first
}
