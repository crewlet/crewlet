package jetstream

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/logging"
)

// tunnelDial bounds the tunnel's own dial to a member, its lookup included
// ([memberDialer]): a request's term ([jsapi.RequestTerm]), because the dial
// crosses the link, whose bottleneck every request across it is sized to
// wait behind — up to four seconds of its rate across its two directions
// together, and a round trip (crewlet/crewlet#1035) — and a connection's SYN
// and SYN-ACK queue there like any other packet. The handshake behind it is
// given a term too ([leafHandshakeTerm]).
const tunnelDial = jsapi.RequestTerm

// tunnelConnectTerm bounds a leaf's whole CONNECT exchange with its tunnel —
// the dial to it, the request, and the tunnel's answer, its own dial to the
// member inside it (nats-server's Proxy.Timeout): the tunnel's dial
// ([tunnelDial]) and a read's term ([jsprovision.ReadTerm]) beside it, so a
// dial that failed reaches the leaf as the tunnel's 502 naming why, inside
// the leaf's term, where a term no longer than the dial would cut the leaf
// first and leave it only a timeout to log. Left to nats-server it is the
// leaf's dial timeout, one second, and a leaf whose link dropped could not
// dial again while other flows kept the link's queue deep.
const tunnelConnectTerm = tunnelDial + jsprovision.ReadTerm

// tunnelCredentialTerm bounds how long the tunnel waits for a connection's
// CONNECT, up to its credential: a read's term ([jsprovision.ReadTerm]). The
// leaf writes its request the moment its dial to the tunnel returns, on
// loopback, so it is read within microseconds; a caller that sends nothing
// is closed a second later rather than at the leaf's whole connect term.
const tunnelCredentialTerm = jsprovision.ReadTerm

// tunnelHeaderMax bounds a CONNECT request the tunnel reads, request line and
// headers together: nats-server's is a request line naming a host and port,
// a Host header and the Proxy-Authorization header, under 300 bytes for the
// longest DNS name, so 4 KiB is room for any it sends and refuses a stream of
// anything else at once.
const tunnelHeaderMax = 4 << 10

// tunnelHandshakes is how many connections the tunnel holds before they have
// shown its credential. At that many, the next connection the tunnel takes
// closes the OLDEST of them, never the new one: a caller that opens
// connections and sends nothing — anything that reaches the loopback port,
// a box sharing it among them — holds no place a leaf's attempt then needs.
// The leaf's connection carries its CONNECT as it is taken, and leaves the
// places once its credential is read ([tunnel.handshake]), so it is closed
// only where the tunnel takes this many more connections between taking it
// and reading the request already waiting in its socket. 64 makes that a
// flood of connections at a pace that already starves any loopback
// listener's backlog, and holds 64 descriptors and goroutines, for a second
// each ([tunnelCredentialTerm]), at most: nothing a node can measure, where
// the leaf itself needs two (an attempt beside the one it abandoned).
const tunnelHandshakes = 64

// tunnelBuffer is what each direction of a tunnelled link is copied through:
// one buffer of 32 KiB, which reads no more ahead of what its far side has
// taken.
const tunnelBuffer = 32 << 10

// tunnelAcceptBackoff and tunnelAcceptBackoffMax bound the wait before the
// tunnel accepts again after a failed accept — a process out of descriptors,
// a connection reset before it was taken — doubling from the first to the
// second, as net/http's server does.
const (
	tunnelAcceptBackoff    = 5 * time.Millisecond
	tunnelAcceptBackoffMax = time.Second
)

// tunnel is a leaf's way to the members: an HTTP CONNECT proxy on loopback,
// in the engine's own process, that the leaf's link to the members dials
// through (nats-server's leaf remote Proxy option), so the link's upstream
// socket is the engine's to read ([LinkMeter]).
//
// # Why a tunnel
//
// A leaf's link is dialled by nats-server, which gives an embedding process
// no way to reach the socket — its dialer is unexported — and no counter it
// keeps moves before a whole message has crossed. Its Proxy option is the one
// supported way to put the engine on the path: every attempt the leaf makes,
// at a configured URL or one a member advertised, goes through the proxy, and
// the bytes a member sends with the proxy's answer are kept. The leaf still
// presents its nkey and signs the member's nonce, through the tunnel, so the
// keyed link (ADR-0094) is unchanged; the link carries no TLS for the tunnel
// to see.
//
// # What it does
//
// It listens on 127.0.0.1, takes one CONNECT a connection, and admits only
// one that presents its credential: a user and password minted at each start
// that only the in-process leaf server holds. It FORWARDS WHEREVER ITS
// CREDENTIAL'S HOLDER ASKS, which is why nothing else holds it — a box
// sharing the host's loopback reaches the port, and is answered 407. A
// caller that connects and says nothing is closed a second later, and the
// oldest such is closed at once when [tunnelHandshakes] are held, so no
// number of idle connections keeps the leaf's own from being served. It
// dials the host the CONNECT names as the external dial does
// ([memberDialer]): resolved, its addresses shuffled, the budget shared. A
// dial that fails is answered 502 with the dial's own error, so the leaf's
// log says why. Then it copies each direction through one 32 KiB buffer,
// and the upstream socket is the meter's.
//
// A CLOSE AT EITHER END CLOSES BOTH, at once: a member that went away is
// seen by the leaf as its link closing, and it dials again, rather than
// believing the link up until its own pings give it up four minutes later.
//
// IT LIVES AND DIES WITH THE LEAF SERVER: started before it, closed after
// it, and a listener that stops accepting in between is the broker's death
// ([Death]), so the node stops rather than run a leaf that can never link
// again. A failed accept is otherwise tried again after a backoff.
//
// WHAT IT CHANGES ON THE PATH: one loopback hop, and the back-pressure a
// direct socket's would give is spread over two more sockets' buffers, which
// the kernel autotunes. Nothing the engine bounds counts those buffers: a
// window ends at a stream's acknowledgement or a piece's arrival, both past
// them. Its upstream socket keeps no TCP keepalive, as nats-server's own
// dial of a leaf link keeps none (v2.15.2, util.go natsDialTimeout, which
// leaves liveness to the protocol's pings, for links that pay per packet):
// the leaf's pings find a member that vanished, as they do without the
// tunnel.
//
// WHAT IT CANNOT CARRY: a link that upgrades to TLS. nats-server closes a
// proxied remote whose member requires TLS (v2.15.2, leafnode.go), so a
// leaf link that ever takes TLS takes it through an https proxy, or not
// through this one.
type tunnel struct {
	ln          net.Listener
	user, pass  string
	dialer      memberDialer
	meter       *LinkMeter
	death       *Death
	log         *slog.Logger
	connectTerm time.Duration
	// credentialTerm bounds the read of a connection's CONNECT
	// ([tunnelCredentialTerm]).
	credentialTerm time.Duration

	// stop is closed when the tunnel is, and ctx, which every dial runs
	// under, ended by cancel.
	stop   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	closing bool
	// conns is every connection the tunnel holds: each side of every link it
	// carries, and every one still in its handshake.
	conns map[net.Conn]struct{}
	// unproven is every connection that has not yet shown the credential,
	// oldest first: [tunnelHandshakes] at most.
	unproven []net.Conn
	wg       sync.WaitGroup
}

// startTunnel listens on loopback for a leaf's CONNECTs, metering every link
// it carries with a meter of its own ([tunnel.meter]), closed with it, and
// declaring death if it stops accepting before it is closed.
//
// ctx bounds the listen alone: the tunnel lives until it is closed, and its
// dials end with it.
func startTunnel(ctx context.Context, death *Death) (*tunnel, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("jetstream: the leaf's link tunnel cannot listen on loopback (127.0.0.1): %w; "+
			"a leaf needs a loopback interface it may bind a free port on — check the host's or "+
			"the pod's lo is up and the process is not out of file descriptors", err)
	}
	dialer := newMemberDialer(tunnelDial)
	// NO KEEPALIVE, as nats-server dials a leaf link: see [tunnel]'s "What
	// it changes on the path".
	dialer.connect = (&net.Dialer{KeepAlive: -1}).DialContext
	t := newTunnel(context.WithoutCancel(ctx), ln, dialer, newLinkMeter(), death)
	t.start()
	return t, nil
}

// newTunnel is a tunnel accepting on ln, dialling through dialer, with a
// fresh credential; nothing is accepted until start. Its dials run under a
// context made from ctx and ended at its close.
func newTunnel(ctx context.Context, ln net.Listener, dialer memberDialer, meter *LinkMeter,
	death *Death) *tunnel {
	ctx, cancel := context.WithCancel(ctx)
	return &tunnel{
		ctx: ctx, cancel: cancel,
		ln: ln, user: "leaf", pass: rand.Text(),
		dialer: dialer, meter: meter, death: death,
		log:            logging.Get("queue.jetstream"),
		connectTerm:    tunnelConnectTerm,
		credentialTerm: tunnelCredentialTerm,
		stop:           make(chan struct{}),
		conns:          map[net.Conn]struct{}{},
	}
}

// start accepts until the tunnel is closed.
func (t *tunnel) start() {
	t.wg.Add(1)
	go t.accept()
}

// proxy sends the leaf's link to the members in the global account — its
// data link, which the queue's and the coordination store's connections ride
// — through the tunnel, within its connect term ([tunnelConnectTerm]). A
// remote binding another account is left to dial for itself.
func (t *tunnel) proxy(remotes []*server.RemoteLeafOpts) {
	for _, r := range remotes {
		if r.LocalAccount != "" && r.LocalAccount != server.DEFAULT_GLOBAL_ACCOUNT {
			continue
		}
		r.Proxy.URL = "http://" + t.ln.Addr().String()
		r.Proxy.Username, r.Proxy.Password = t.user, t.pass
		r.Proxy.Timeout = t.connectTerm
	}
}

// close stops accepting, closes every link it carries, waits for its
// goroutines to end and stops its meter.
func (t *tunnel) close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if !t.closing {
		t.closing = true
		close(t.stop)
		t.cancel()
	}
	conns := make([]net.Conn, 0, len(t.conns))
	for c := range t.conns {
		conns = append(conns, c)
	}
	t.mu.Unlock()
	_ = t.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	t.wg.Wait()
	t.meter.close()
}

// accept takes connections until the listener closes: tried again after a
// backoff on a failed accept, and the broker's death where the listener
// closed before the tunnel was.
func (t *tunnel) accept() {
	defer t.wg.Done()
	backoff := time.Duration(0)
	for {
		c, err := t.ln.Accept()
		if err != nil {
			if t.isClosing() {
				return
			}
			if errors.Is(err, net.ErrClosed) {
				t.death.declare("the leaf's link tunnel stopped accepting before the broker "+
					"stopped, so this leaf can never link to the members again: %v", err)
				return
			}
			backoff = min(max(2*backoff, tunnelAcceptBackoff), tunnelAcceptBackoffMax)
			t.log.Warn("leaf_tunnel_accept_failed", "error", err.Error(), "retry_in", backoff.String(),
				"detail", "the leaf's link tunnel could not take a connection, and tries again",
				"hint", "a process out of file descriptors fails this way; raise its limit")
			select {
			case <-time.After(backoff):
			case <-t.stop:
				return
			}
			continue
		}
		backoff = 0
		if !t.take(c) {
			_ = c.Close()
			return
		}
		t.wg.Add(1)
		go t.serve(c)
	}
}

func (t *tunnel) isClosing() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closing
}

// hold records c as one the tunnel holds, and answers false once it is
// closing, when c is not recorded.
func (t *tunnel) hold(c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing {
		return false
	}
	t.conns[c] = struct{}{}
	return true
}

// take holds c, a connection just accepted, as one that has not yet shown
// the credential, closing the oldest such where [tunnelHandshakes] are held
// already; and answers false once the tunnel is closing, when c is not held.
func (t *tunnel) take(c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing {
		return false
	}
	if len(t.unproven) == tunnelHandshakes {
		// ITS SERVER'S READ ENDS ON THE CLOSE, and drops it from conns.
		_ = t.unproven[0].Close()
		t.unproven = t.unproven[1:]
	}
	t.unproven = append(t.unproven, c)
	t.conns[c] = struct{}{}
	return true
}

// proven gives up c's place among the connections that have not shown the
// credential, where it holds one.
func (t *tunnel) proven(c net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.unprovenLocked(c)
}

// unprovenLocked removes c from t.unproven; t.mu held.
func (t *tunnel) unprovenLocked(c net.Conn) {
	for i, u := range t.unproven {
		if u == c {
			t.unproven = append(t.unproven[:i], t.unproven[i+1:]...)
			return
		}
	}
}

// drop closes c and forgets it.
func (t *tunnel) drop(c net.Conn) {
	_ = c.Close()
	t.mu.Lock()
	delete(t.conns, c)
	t.unprovenLocked(c)
	t.mu.Unlock()
}

// serve reads one CONNECT from down, the leaf's connection, and carries it to
// the member it names.
func (t *tunnel) serve(down net.Conn) {
	defer t.wg.Done()
	up, pending, ok := t.handshake(down)
	if !ok {
		t.drop(down)
		return
	}
	if !t.hold(up) {
		_ = up.Close()
		t.drop(down)
		return
	}
	// Each direction until either ends, and then both are closed: see
	// [tunnel]'s "A close at either end closes both".
	var once sync.Once
	end := func() {
		once.Do(func() {
			t.drop(down)
			t.drop(up)
		})
	}
	toMember := make(chan struct{})
	go func() {
		defer close(toMember)
		pipe(up, io.MultiReader(pending, down))
		end()
	}()
	pipe(down, up)
	end()
	<-toMember
}

// handshake reads the CONNECT on down and, where it presents the credential,
// dials the member it names: up is the metered upstream socket, and pending
// what down sent after its request that the read took in with it.
func (t *tunnel) handshake(down net.Conn) (up net.Conn, pending io.Reader, ok bool) {
	// THE REQUEST WITHIN A READ'S TERM, and held to its bound, so a
	// connection that sends nothing, or anything but a CONNECT, holds
	// nothing for long; the leaf's is in its socket as it is taken.
	_ = down.SetDeadline(time.Now().Add(t.credentialTerm))
	br := bufio.NewReader(io.LimitReader(down, tunnelHeaderMax))
	req, err := http.ReadRequest(br)
	if err != nil {
		tunnelAnswer(down, http.StatusBadRequest, "")
		return nil, nil, false
	}
	switch {
	case !t.admits(req.Header.Get("Proxy-Authorization")):
		tunnelAnswer(down, http.StatusProxyAuthRequired, `Proxy-Authenticate: Basic realm="crewlet"`+"\r\n")
		return nil, nil, false
	case req.Method != http.MethodConnect:
		tunnelAnswer(down, http.StatusMethodNotAllowed, "")
		return nil, nil, false
	}
	// PROVEN, so its place goes to the next caller before the dial, and
	// the rest of the exchange — the dial, the answer — is the leaf's own
	// term.
	t.proven(down)
	_ = down.SetDeadline(time.Now().Add(t.connectTerm))
	up, err = t.dialer.dialMember(t.ctx, "tcp", req.Host)
	if err != nil {
		// THE LEAF LOGS IT, not the tunnel: nats-server prints the status
		// line it was answered, the dial's error in it ([reason]), and
		// holds that line to its own pace for a remote that keeps failing,
		// where a line here would repeat at every attempt for as long as a
		// listed member is down.
		tunnelStatus(down, http.StatusBadGateway, reason(err), "")
		return nil, nil, false
	}
	if _, err := io.WriteString(down, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		_ = up.Close()
		return nil, nil, false
	}
	_ = down.SetDeadline(time.Time{})
	// What the leaf sent behind its request is the link's first bytes: the
	// limited reader holds nothing past the request's bound, so the buffer
	// holds the rest of what that one read took in.
	buffered, _ := br.Peek(br.Buffered())
	return t.meter.wrap(up), strings.NewReader(string(buffered)), true
}

// admits reports whether a Proxy-Authorization header presents the tunnel's
// credential, compared in constant time.
func (t *tunnel) admits(header string) bool {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(t.user+":"+t.pass))
	return subtle.ConstantTimeCompare([]byte(header), []byte(want)) == 1
}

// tunnelAnswer writes a bodiless response of status, with extra header lines.
func tunnelAnswer(c net.Conn, status int, extra string) {
	tunnelStatus(c, status, http.StatusText(status), extra)
}

// tunnelStatus writes a bodiless response of status with phrase as its reason.
func tunnelStatus(c net.Conn, status int, phrase, extra string) {
	_, _ = fmt.Fprintf(c, "HTTP/1.1 %d %s\r\n%sContent-Length: 0\r\n\r\n", status, phrase, extra)
}

// reason is err as a status line's reason phrase: its text on one line, so a
// leaf's log, which prints the status line it was answered, names why its dial
// failed (nats-server logs "proxy CONNECT failed: <status>").
func reason(err error) string {
	return "Bad Gateway: " + strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, err.Error())
}

// pipe copies from src to dst through one [tunnelBuffer] until src ends or dst
// fails.
func pipe(dst io.Writer, src io.Reader) {
	buf := make([]byte, tunnelBuffer)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
