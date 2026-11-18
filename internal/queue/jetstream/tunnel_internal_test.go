package jetstream

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
)

// dialRecord is what a case's tunnel dialled: each address it connected to,
// and each name it looked up.
type dialRecord struct {
	mu      sync.Mutex
	dialled []string
	looked  []string
}

func (r *dialRecord) dials() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dialled...)
}

func (r *dialRecord) lookups() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.looked...)
}

// recordingDialer is a member dialer that records what it is asked and then
// dials loopback with Go's own dialer, or answers fail where it is set.
func recordingDialer(r *dialRecord, lookup func(string) ([]string, error), fail error) memberDialer {
	var tcp net.Dialer
	return memberDialer{
		budget: tunnelDial,
		lookup: func(_ context.Context, host string) ([]string, error) {
			r.mu.Lock()
			r.looked = append(r.looked, host)
			r.mu.Unlock()
			if lookup == nil {
				return nil, fmt.Errorf("no name %s in this case", host)
			}
			return lookup(host)
		},
		connect: func(ctx context.Context, network, address string) (net.Conn, error) {
			r.mu.Lock()
			r.dialled = append(r.dialled, address)
			r.mu.Unlock()
			if fail != nil {
				return nil, fail
			}
			return tcp.DialContext(ctx, network, address)
		},
	}
}

// openTunnel is a tunnel on loopback dialling through dialer, closed when
// the case ends, and the death it declares.
func openTunnel(t *testing.T, dialer memberDialer) (*tunnel, *Death) {
	t.Helper()
	return openTunnelWith(t, dialer, tunnelCredentialTerm)
}

// openTunnelWith is [openTunnel] that waits term for a connection's CONNECT.
func openTunnelWith(t *testing.T, dialer memberDialer, term time.Duration) (*tunnel, *Death) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	death := newDeath()
	tun := newTunnel(t.Context(), ln, dialer, newLinkMeter(), death)
	tun.credentialTerm = term
	tun.start()
	t.Cleanup(tun.close)
	return tun, death
}

// echoMember is a stand-in member on loopback that sends hello on each
// connection it takes and then echoes what it reads, until the case ends, and
// hands the first sixteen it takes on accepted.
func echoMember(t *testing.T, hello string) (addr string, accepted <-chan net.Conn) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	taken := make(chan net.Conn, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
			// Handed to a case that takes them, the first sixteen; a case
			// that does not is not waited on.
			select {
			case taken <- c:
			default:
			}
			go func() {
				_, _ = io.WriteString(c, hello)
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String(), taken
}

// connect sends a CONNECT for target to tun, with auth as its
// Proxy-Authorization unless empty, and answers the connection, a reader of
// what follows the answer, and the answer's status line.
func connect(t *testing.T, tun *tunnel, target, auth string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	c, err := net.Dial("tcp", tun.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if auth != "" {
		req += "Proxy-Authorization: " + auth + "\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read the tunnel's answer: %v", err)
	}
	_ = resp.Body.Close()
	return c, br, resp.Status
}

// basic is a Basic credential for user and pass.
func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// closedWithin fails the case unless a read of r ends — the far side closed —
// before a bound far past any a loopback close takes.
func closedWithin(t *testing.T, r io.Reader, what string) {
	t.Helper()
	ended := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, r)
		ended <- err
	}()
	bound := time.NewTimer(10 * time.Second) //fixedwait:liveness
	defer bound.Stop()
	select {
	case <-ended:
	case <-bound.C:
		t.Fatalf("%s: still open", what)
	}
}

// THE TUNNEL REFUSES A CONNECT WITHOUT ITS CREDENTIAL, answered 407 with
// nothing dialled: it forwards wherever its credential's holder asks, so
// anything else on the host's loopback — a box sharing it — is refused before
// it can name a member. A credential that is not the tunnel's is refused the
// same, and so is anything but a CONNECT that presents it.
//
// Mutant: the credential unchecked — the tunnel dials for every caller.
func TestTheTunnelRefusesAConnectWithoutItsCredential(t *testing.T) {
	t.Parallel()
	var rec dialRecord
	tun, _ := openTunnel(t, recordingDialer(&rec, nil, nil))
	member, _ := echoMember(t, "")
	for name, auth := range map[string]string{
		"no credential":       "",
		"another password":    basic(tun.user, tun.pass+"x"),
		"another user":        basic(tun.user+"x", tun.pass),
		"not Basic":           "Bearer " + tun.pass,
		"the password alone":  tun.pass,
		"an empty credential": basic("", ""),
	} {
		_, _, status := connect(t, tun, member, auth)
		if !strings.HasPrefix(status, "407") {
			t.Errorf("%s: answered %q, want 407", name, status)
		}
	}
	if d := rec.dials(); len(d) != 0 {
		t.Errorf("the tunnel dialled %v for callers without its credential", d)
	}

	// A GET presenting it is no CONNECT.
	c, err := net.Dial("tcp", tun.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "GET http://"+member+"/ HTTP/1.1\r\nHost: "+member+
		"\r\nProxy-Authorization: "+basic(tun.user, tun.pass)+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("a GET with the credential answered %q, want 405", resp.Status)
	}
	if d := rec.dials(); len(d) != 0 {
		t.Errorf("the tunnel dialled %v for a request that was no CONNECT", d)
	}
}

// THE TUNNEL CARRIES A LINK EACH WAY AND METERS IT: what the member sends with
// its first bytes and every echo come back to the leaf, the meter reads them
// off the upstream socket, and the far end's acknowledgements of what the leaf
// sent. Over many links opened and closed, the tunnel holds no connection and
// the meter no socket once each has closed, so neither goroutines nor
// descriptors grow with a leaf that dials again and again.
//
// Mutant: the upstream socket unmetered — nothing read. Mutant: a link's
// connections left in the tunnel's set at its end — red at the count.
func TestTheTunnelCarriesALinkEachWayAndMetersIt(t *testing.T) {
	t.Parallel()
	var rec dialRecord
	tun, _ := openTunnel(t, recordingDialer(&rec, nil, nil))
	const hello = "INFO {}\r\n"
	member, _ := echoMember(t, hello)
	const links = 40
	payload := strings.Repeat("x", 64<<10)
	for i := range links {
		c, br, status := connect(t, tun, member, basic(tun.user, tun.pass))
		if !strings.HasPrefix(status, "200") {
			t.Fatalf("link %d: answered %q, want 200", i, status)
		}
		got := make([]byte, len(hello)+len(payload))
		if _, err := io.WriteString(c, payload); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(br, got); err != nil {
			t.Fatalf("link %d: read the member's bytes back: %v", i, err)
		}
		if string(got) != hello+payload {
			t.Fatalf("link %d: carried back %d bytes not what the member sent", i, len(got))
		}
		_ = c.Close()
	}
	acked, read := tun.meter.Carried()
	if want := uint64(links * (len(hello) + len(payload))); read < want {
		t.Errorf("the meter read %d bytes off the links' upstream sockets, want at least the %d the "+
			"member sent", read, want)
	}
	// What the leaf sent is acknowledged by the member's kernel, but a
	// link's last acknowledgements may still be arriving as it closes.
	if acked == 0 {
		t.Error("the meter read nothing acknowledged of what the leaf sent")
	}
	until(t, "every link's connections to leave the tunnel and the meter", func() bool {
		tun.mu.Lock()
		held := len(tun.conns)
		tun.mu.Unlock()
		tun.meter.mu.Lock()
		metered := len(tun.meter.conns)
		opened := tun.meter.opened
		tun.meter.mu.Unlock()
		if opened != links {
			t.Fatalf("the meter opened %d sockets for %d links", opened, links)
		}
		return held == 0 && metered == 0
	})
}

// A CLOSE AT EITHER END OF A TUNNELLED LINK CLOSES BOTH: a member that goes
// away is a link the leaf sees close, so it dials again at once rather than
// believe the link up until its pings give it up four minutes later, losing
// what it carries meanwhile; and a leaf that closes its side leaves no socket
// open to the member.
//
// Mutant: the member's close not passed on — the leaf's side stays open.
func TestATunnelledLinkClosesAtBothEndsWhenEitherCloses(t *testing.T) {
	t.Parallel()
	var rec dialRecord
	tun, _ := openTunnel(t, recordingDialer(&rec, nil, nil))
	member, accepted := echoMember(t, "")

	t.Run("the_member_closes", func(t *testing.T) {
		_, br, status := connect(t, tun, member, basic(tun.user, tun.pass))
		if !strings.HasPrefix(status, "200") {
			t.Fatalf("answered %q, want 200", status)
		}
		up := <-accepted
		_ = up.Close()
		closedWithin(t, br, "the leaf's side of a link whose member closed")
	})
	t.Run("the_leaf_closes", func(t *testing.T) {
		c, _, status := connect(t, tun, member, basic(tun.user, tun.pass))
		if !strings.HasPrefix(status, "200") {
			t.Fatalf("answered %q, want 200", status)
		}
		up := <-accepted
		_ = c.Close()
		closedWithin(t, up, "the member's side of a link whose leaf closed")
	})
}

// THE TUNNEL DIALS A MEMBER AS THE EXTERNAL DIAL DOES: a name resolved by the
// tunnel and its addresses dialled in a shuffled order — nats-server names
// only the URL's host in its CONNECT, so a tunnel that let Go take a name's
// first address would send every leaf behind one name to one member — and a
// dial that fails answered 502 naming its error, so the leaf's log, which
// prints the status line, says why.
//
// Mutant: the tunnel dials the CONNECT's host with Go's dialer, no lookup.
// Mutant: a failed dial answered with a bare 502.
func TestTheTunnelDialsAMemberAsTheExternalDialDoes(t *testing.T) {
	t.Parallel()
	t.Run("a_name_resolved_and_shuffled", func(t *testing.T) {
		t.Parallel()
		addrs := []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}
		var rec dialRecord
		failed := errors.New("this case dials nobody")
		tun, _ := openTunnel(t, recordingDialer(&rec,
			func(string) ([]string, error) { return addrs, nil }, failed))
		const attempts = 30
		for range attempts {
			_, _, status := connect(t, tun, "members.example.com:7422", basic(tun.user, tun.pass))
			if !strings.HasPrefix(status, "502") {
				t.Fatalf("answered %q, want 502", status)
			}
		}
		if l := rec.lookups(); len(l) != attempts || l[0] != "members.example.com" {
			t.Fatalf("looked up %v, want the member's name once an attempt", l)
		}
		first := map[string]bool{}
		dials := rec.dials()
		for i := 0; i < len(dials); i += len(addrs) {
			first[dials[i]] = true
		}
		if len(dials) != attempts*len(addrs) || len(first) < 2 {
			t.Errorf("dialled %d addresses in %d attempts, first of them %v; want every address an "+
				"attempt, begun at more than one", len(dials), attempts, first)
		}
	})
	t.Run("a_failed_dial_answered_with_its_cause", func(t *testing.T) {
		t.Parallel()
		var rec dialRecord
		cause := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		tun, _ := openTunnel(t, recordingDialer(&rec, nil, cause))
		_, _, status := connect(t, tun, "127.0.0.1:7422", basic(tun.user, tun.pass))
		if !strings.HasPrefix(status, "502") || !strings.Contains(status, cause.Error()) {
			t.Errorf("a refused dial answered %q, want a 502 naming %q", status, cause.Error())
		}
	})
}

// THE TUNNEL HOLDS NOTHING FOR A CALLER THAT SENDS NO CONNECT: a request is
// read no further than its header bound, one that sends nothing is closed at
// a read's term, and at [tunnelHandshakes] connections waiting to show the
// credential the next one taken closes the oldest, so no number of idle
// callers holds the tunnel's descriptors or keeps the leaf's CONNECT unserved.
//
// Mutant: the request read without its bound. Mutant: no deadline on the
// request. Mutant: no cap on the unproven connections — the oldest idle one
// is held. Mutant: the newest closed at the cap — the leaf's CONNECT is never
// answered. Mutant: a place held through the dial — one more is held once the
// leaf is served.
func TestTheTunnelHoldsNothingForACallerThatSendsNoConnect(t *testing.T) {
	t.Parallel()
	member, _ := echoMember(t, "")

	t.Run("a_request_is_read_no_further_than_its_bound", func(t *testing.T) {
		t.Parallel()
		var rec dialRecord
		tun, _ := openTunnel(t, recordingDialer(&rec, nil, nil))
		c, err := net.Dial("tcp", tun.ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		// A HEADER THAT NEVER ENDS: a bounded tunnel answers once it has
		// read the bound, leaving the caller only what the sockets hold;
		// an unbounded one reads on for its whole term.
		var written atomic.Int64
		go func() {
			if _, err := io.WriteString(c, "CONNECT a:1 HTTP/1.1\r\nX: "); err != nil {
				return
			}
			chunk := []byte(strings.Repeat("y", 64<<10))
			// AT MOST TWICE THE CLAIM'S BOUND, so a tunnel that reads
			// without one is shown past it rather than run out of memory.
			for written.Load() < 128<<20 {
				n, err := c.Write(chunk)
				written.Add(int64(n))
				if err != nil {
					return
				}
			}
		}()
		// ITS END, whatever it reads as: the tunnel answers 400 and closes
		// a connection it has not read to its end, which the caller's
		// kernel may report as a reset before the answer.
		closedWithin(t, c, "a connection whose request ran past the header bound")
		if w := written.Load(); w > 64<<20 {
			t.Errorf("the tunnel read %d bytes of a request before answering it, past its %d-byte "+
				"bound and every socket buffer between", w, tunnelHeaderMax)
		}
	})
	t.Run("a_silent_caller_is_closed_at_a_reads_term", func(t *testing.T) {
		t.Parallel()
		var rec dialRecord
		tun, _ := openTunnelWith(t, recordingDialer(&rec, nil, nil), 200*time.Millisecond)
		c, err := net.Dial("tcp", tun.ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		closedWithin(t, c, "a connection that sent nothing, past its read's term")
	})
	t.Run("a_leafs_connect_is_served_while_idle_callers_hold_connections", func(t *testing.T) {
		t.Parallel()
		var rec dialRecord
		// NO TERM TO END AN IDLE CALLER, so only the cap closes one.
		tun, _ := openTunnelWith(t, recordingDialer(&rec, nil, nil), time.Hour)
		idle := make([]net.Conn, tunnelHandshakes)
		for i := range idle {
			c, err := net.Dial("tcp", tun.ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			idle[i] = c
		}
		until(t, "the tunnel to take every idle caller", func() bool {
			return tun.unprovenHeld() == tunnelHandshakes
		})
		c, br, status := connect(t, tun, member, basic(tun.user, tun.pass))
		if !strings.HasPrefix(status, "200") {
			t.Fatalf("the leaf's CONNECT beside %d idle callers was answered %q, want 200",
				tunnelHandshakes, status)
		}
		if _, err := io.WriteString(c, "ping\n"); err != nil {
			t.Fatal(err)
		}
		if line, err := br.ReadString('\n'); err != nil || line != "ping\n" {
			t.Fatalf("the leaf's link beside idle callers echoed (%q, %v), want ping", line, err)
		}
		closedWithin(t, idle[0], "the oldest idle caller, once the cap was reached")
		if n := tun.unprovenHeld(); n != tunnelHandshakes-1 {
			t.Errorf("the tunnel holds %d connections that showed no credential once the leaf's was "+
				"served, want %d: every idle caller but the oldest, and not the leaf's", n,
				tunnelHandshakes-1)
		}
	})
}

// unprovenHeld is how many connections the tunnel holds that have not shown
// its credential.
func (t *tunnel) unprovenHeld() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.unproven)
}

// fakeListener is a listener whose Accept answers what a case queues.
type fakeListener struct {
	next     chan acceptResult
	accepted atomic.Int64
	closed   chan struct{}
	once     sync.Once
}

type acceptResult struct {
	c   net.Conn
	err error
}

func newFakeListener() *fakeListener {
	return &fakeListener{next: make(chan acceptResult, 16), closed: make(chan struct{})}
}

func (l *fakeListener) Accept() (net.Conn, error) {
	select {
	case r := <-l.next:
		l.accepted.Add(1)
		return r.c, r.err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *fakeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// A TUNNEL THAT STOPS ACCEPTING IS THE BROKER'S DEATH, and one whose accept
// fails is not: a failed accept — a process out of descriptors — is tried
// again after a backoff and the next connection served, while a listener that
// closed before the tunnel did means the leaf can never link again, so the
// node stops ([Death]) rather than serve on a leaf with no way to the fleet.
//
// Mutant: any accept error ends the loop — the connection after the failures
// is never served. Mutant: a closed listener ends the loop and declares
// nothing.
func TestATunnelThatStopsAcceptingIsTheBrokersDeath(t *testing.T) {
	t.Parallel()
	ln := newFakeListener()
	death := newDeath()
	var rec dialRecord
	tun := newTunnel(t.Context(), ln, recordingDialer(&rec, nil, nil), newLinkMeter(), death)
	tun.start()
	t.Cleanup(tun.close)

	out := &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
	for range 3 {
		ln.next <- acceptResult{err: out}
	}
	leaf, server := net.Pipe()
	defer leaf.Close()
	ln.next <- acceptResult{c: server}
	go func() { _, _ = io.WriteString(leaf, "CONNECT a:1 HTTP/1.1\r\nHost: a:1\r\n\r\n") }()
	answered := make(chan string, 1)
	go func() {
		resp, err := http.ReadResponse(bufio.NewReader(leaf), nil)
		if err != nil {
			answered <- err.Error()
			return
		}
		_ = resp.Body.Close()
		answered <- resp.Status
	}()
	served := time.NewTimer(10 * time.Second) //fixedwait:liveness
	defer served.Stop()
	select {
	case status := <-answered:
		if !strings.HasPrefix(status, "407") {
			t.Errorf("the connection after three failed accepts was answered %q, want 407", status)
		}
	case <-served.C:
		t.Fatal("the connection after three failed accepts was never served")
	}
	select {
	case <-death.Done():
		t.Fatalf("a failed accept declared the broker dead: %v", death.Err())
	default:
	}

	ln.next <- acceptResult{err: fmt.Errorf("accept tcp: %w", net.ErrClosed)}
	bound := time.NewTimer(10 * time.Second) //fixedwait:liveness
	defer bound.Stop()
	select {
	case <-death.Done():
	case <-bound.C:
		t.Fatal("a tunnel whose listener closed under it declared no death")
	}
	if err := death.Err(); !errors.Is(err, ErrBrokerDied) || !strings.Contains(err.Error(), "tunnel") {
		t.Errorf("the death says %v, want ErrBrokerDied naming the tunnel", err)
	}
}

// until polls cond until it holds, failing the case once a bound far past any
// a loopback link takes has passed.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	bound := time.NewTimer(30 * time.Second) //fixedwait:liveness
	defer bound.Stop()
	poll := time.NewTicker(5 * time.Millisecond) //fixedwait:liveness
	defer poll.Stop()
	for !cond() {
		select {
		case <-bound.C:
			t.Fatalf("still waiting for %s", what)
		case <-poll.C:
		}
	}
}

// A LINK'S HANDSHAKE IS GIVEN A REQUEST'S TERM, on a leaf and on an external
// dial alike, since its packets queue at the link's bottleneck like any
// request's: the tunnel's dial of a member gets [jsapi.RequestTerm] and the
// leaf's CONNECT to its tunnel a read's term more, so the tunnel's 502
// reaches it; the leaf waits a term for the member's INFO, the member a term
// for the leaf's CONNECT ([leafHandshakeTerm]), and an external dial gets the
// term too, where one- and two-second defaults strand a link dropped behind a
// deep queue. The tunnel carries only the global account's (data) link.
//
// Mutant: the tunnel's connect term at one second, or no longer than its
// dial. Mutant: the external handshake at two seconds. Mutant: every remote
// proxied. Mutant: the leaf's wait for INFO, or the member's for CONNECT, unset.
func TestALinksHandshakeIsGivenARequestsTerm(t *testing.T) {
	t.Parallel()
	var rec dialRecord
	tun, _ := openTunnel(t, recordingDialer(&rec, nil, nil))
	data := &server.RemoteLeafOpts{}
	global := &server.RemoteLeafOpts{LocalAccount: server.DEFAULT_GLOBAL_ACCOUNT}
	other := &server.RemoteLeafOpts{LocalAccount: "leases"}
	tun.proxy([]*server.RemoteLeafOpts{data, global, other})
	want := jsapi.RequestTerm + jsprovision.ReadTerm
	for name, r := range map[string]*server.RemoteLeafOpts{"the default account's": data,
		"the global account's": global} {
		if r.Proxy.Timeout != want || r.Proxy.URL != "http://"+tun.ln.Addr().String() ||
			r.Proxy.Username != tun.user || r.Proxy.Password != tun.pass {
			t.Errorf("%s remote dials through %q as %q within %v, want the tunnel with its credential "+
				"within %v", name, r.Proxy.URL, r.Proxy.Username, r.Proxy.Timeout, want)
		}
	}
	if other.Proxy.URL != "" {
		t.Errorf("a remote in another account dials through %q, want directly", other.Proxy.URL)
	}
	if tun.dialer.budget != jsapi.RequestTerm || tun.connectTerm <= tun.dialer.budget {
		t.Errorf("the tunnel dials a member within %v and answers within %v, want the request term "+
			"%v, answered inside the leaf's term", tun.dialer.budget, tun.connectTerm, jsapi.RequestTerm)
	}

	leafCfg := Config{ServerName: "leaf", LeafURLs: []string{"nats-leaf://127.0.0.1:7422"},
		FleetAuth: testFleetAuth()}
	leaf, _, err := embeddedOptions(leafCfg, authFor(t, leafCfg))
	if err != nil {
		t.Fatalf("a leaf's options: %v", err)
	}
	for i, r := range leaf.LeafNode.Remotes {
		if r.FirstInfoTimeout != jsapi.RequestTerm {
			t.Errorf("the leaf's remote %d waits %v for a member's INFO, want the request term %v",
				i, r.FirstInfoTimeout, jsapi.RequestTerm)
		}
	}
	memberCfg := Config{ServerName: "a", LeafPort: 7422, StoreDir: t.TempDir(), FleetAuth: testFleetAuth()}
	member, scratch, err := embeddedOptions(memberCfg, authFor(t, memberCfg))
	if err != nil {
		t.Fatalf("a member's options: %v", err)
	}
	removeScratch(scratch)
	if got := member.LeafNode.AuthTimeout; got != jsapi.RequestTerm.Seconds() {
		t.Errorf("a member waits %vs for a leaf's CONNECT, want the request term %v", got,
			jsapi.RequestTerm)
	}

	opts, _, err := dialOptions(Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	applied := nats.GetDefaultOptions()
	for _, opt := range opts {
		if err := opt(&applied); err != nil {
			t.Fatal(err)
		}
	}
	if applied.Timeout != jsapi.RequestTerm {
		t.Errorf("an external dial's handshake is given %v, want the request term %v",
			applied.Timeout, jsapi.RequestTerm)
	}
}
