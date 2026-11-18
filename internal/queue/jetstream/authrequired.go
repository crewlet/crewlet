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

// requiresAuthentication refuses an external server whose INFO line (info,
// nil where the connection read none) says it admits a client presenting
// nothing; server is [serverOf]'s name for it. Tier A refuses a node that
// presents nothing (config's Stream.presentsCredential); only the server can
// say whether it admits everybody else, to every subject the stream carries.
//
// Admitted: `auth_required` (any authorization; cleared by a no_auth_user), or
// `tls_required` AND `tls_verify`. TLS alone is not (measured, v2.15.1):
// `tls { }` without verify sets neither, and tls_verify without tls_required
// is allow_non_tls, admitting a plaintext client. An INFO [infoDialer] did not
// see is a refusal, never a pass. Only the server this dial reached is judged
// ([carriesTheContract]), so the remedy names them all.
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

// heldToAuthentication hands nc back where the INFO of the server it is
// connected to admits it, and closes it and answers the refusal where it does
// not ([heldToTheContract]'s shape). recorded is the dialer nc dialled
// through, and url the stream.url it dialled.
//
// The INFO judged is the one whose server_id the client says it is on, not the
// dialer's newest: nats.go redials the next member at once when a server drops
// (a rolling restart, lame-duck), so the newest may be one whose INFO is
// unread. A client on no server at all is refused as that, not as a server
// that sent no INFO.
func heldToAuthentication(nc *nats.Conn, recorded *infoDialer, url string) (*nats.Conn, error) {
	id := nc.ConnectedServerId()
	info := recorded.sealedInfo(id)
	var err error
	if id == "" {
		err = fmt.Errorf("jetstream: this node's connection to %s was lost before it "+
			"could tell whether that server authenticates its clients, and a "+
			"connection nobody checked is not handed on: the dial fails rather than "+
			"wait out the client's reconnect, so dial again once that cluster is serving",
			serverAddresses(url))
	} else {
		err = requiresAuthentication(serverOf(nc, false), info)
	}
	if err != nil {
		nc.Close()
		return nil, err
	}
	return nc, nil
}

// infoLineMax bounds the INFO line a recorded connection keeps; nats.go reads
// the line with no bound of its own.
//
// 64 KiB: the fixed fields stay under a kilobyte (419 bytes measured on a
// standalone nats-server v2.15.1 with JetStream and a token), and each member
// advertised adds a connect_urls (and, with websocket, ws_connect_urls) entry
// of ~20 bytes by pod IP, at most 262 by the longest DNS name: over a hundred
// members by longest names, thousands by IP. A longer line is refused as an
// INFO not read, never judged on a prefix.
const infoLineMax = 64 << 10

// memberDialer dials one member of a broker named by host and port, within a
// budget for the whole attempt; the external dial ([infoDialer]) and a leaf's
// tunnel ([tunnel]) both reach a member through it.
//
// It resolves the name itself: nats.go hands a custom dialer each of a name's
// addresses with the whole budget (N times it per pass, against the lease TTL
// [externalHandshake] guards, when a headless service resolves to every pod).
// So the client skips its lookup (nats.SkipHostLookup); this resolves within
// the budget, shuffles (spreading connections; Go's serial dial does not) and
// dials each address with an equal share. A leaf's tunnel needs the same:
// through a proxy nats-server names only the host (v2.15.2, leafnode.go), so
// Go's first address would send every leaf behind one name to one member.
type memberDialer struct {
	// budget bounds the lookup of a member's name, and then the dials to
	// its addresses together.
	budget time.Duration
	// lookup and connect are the resolver and the TCP dial: a seam, so a
	// test can stand a name with several silent addresses in for DNS and a
	// partition.
	lookup  func(ctx context.Context, host string) ([]string, error)
	connect func(ctx context.Context, network, address string) (net.Conn, error)
}

// newMemberDialer dials a member within budget an attempt, through the
// system's resolver and Go's TCP dial.
func newMemberDialer(budget time.Duration) memberDialer {
	var tcp net.Dialer
	return memberDialer{
		budget:  budget,
		lookup:  net.DefaultResolver.LookupHost,
		connect: tcp.DialContext,
	}
}

// dialMember dials the member at address — host:port, as the client hands it
// with its own lookup skipped, or as a leaf names it in its CONNECT — within
// one attempt's budget, or until ctx ends: see [memberDialer].
func (d *memberDialer) dialMember(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addrs := []string{host}
	if net.ParseIP(host) == nil {
		lookup, cancel := context.WithTimeout(ctx, d.budget)
		addrs, err = d.lookup(lookup, host)
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
		attempt, cancel := context.WithTimeout(ctx, share)
		var c net.Conn
		// Cancelled once the dial returns, which leaves a connection it
		// made be: the context bounds the dial, never the connection.
		c, err = d.connect(attempt, network, net.JoinHostPort(addr, port))
		cancel()
		if err == nil {
			return c, nil
		}
	}
	// The last address's failure, as the client answered for its own.
	return nil, err
}

// infoDialer is the external dial's TCP dialer: it dials a member within the
// attempt's budget ([memberDialer]), meters every connection, reconnects
// included, as a socket of the node's data link ([LinkMeter]; none for a dial
// that is not, a command's), and keeps each connection's first line: the INFO,
// sent in plaintext before any TLS upgrade, whose tls_verify nats.go does not
// parse ([requiresAuthentication]); for one dial only, as sealedInfo seals it.
//
// The client asks for a socket only once it has given up the last, so each
// Dial closes the one before: nats.go's reconnect loop (v1.55.0, doReconnect)
// leaves a failed handshake's socket to the finalizer, which the meter's hold
// keeps from ever running, leaking a descriptor per failed attempt.
type infoDialer struct {
	memberDialer
	// meter counts every connection the dialer opens; nil for none.
	meter *LinkMeter

	mu     sync.Mutex
	sealed bool
	opened []*infoConn
	// last is the socket the dialer handed its client last, closed when
	// the client asks for the next.
	last net.Conn
}

// newInfoDialer is the dialer an external dial goes through, each attempt at
// a member bounded by budget, and every connection it opens counted by meter
// (nil for none).
func newInfoDialer(budget time.Duration, meter *LinkMeter) *infoDialer {
	return &infoDialer{memberDialer: newMemberDialer(budget), meter: meter}
}

// Dial opens one connection to the member at address, metered, and recording
// the line it reads first until the dial seals the dialer.
func (d *infoDialer) Dial(network, address string) (net.Conn, error) {
	// THE CLIENT GAVE THE LAST ONE UP: see [infoDialer].
	d.mu.Lock()
	given := d.last
	d.last = nil
	d.mu.Unlock()
	if given != nil {
		_ = given.Close()
	}
	// NO CALLER'S CONTEXT reaches a dial the client makes for itself, a
	// reconnect's among them: the attempt's budget bounds it.
	c, err := d.dialMember(context.Background(), network, address)
	if err != nil {
		return nil, err
	}
	c = d.meter.wrap(c)
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.sealed {
		recorded := &infoConn{Conn: c}
		d.opened = append(d.opened, recorded)
		c = recorded
	}
	d.last = c
	return c, nil
}

// sealedInfo is the INFO line read by the connection whose server_id is
// serverID — the newest such, nil where none completed one — and stops the
// dialer recording: see [infoDialer].
func (d *infoDialer) sealedInfo(serverID string) []byte {
	d.mu.Lock()
	opened := d.opened
	d.sealed, d.opened = true, nil
	d.mu.Unlock()
	if serverID == "" {
		return nil
	}
	for i := len(opened) - 1; i >= 0; i-- {
		line := opened[i].line()
		var announced struct {
			ServerID string `json:"server_id"`
		}
		body, ok := bytes.CutPrefix(line, []byte("INFO "))
		if ok && json.Unmarshal(bytes.TrimSpace(body), &announced) == nil &&
			announced.ServerID == serverID {
			return line
		}
	}
	return nil
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
