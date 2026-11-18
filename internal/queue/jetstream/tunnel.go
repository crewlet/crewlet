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

// tunnelDial bounds the tunnel's dial to a member, lookup included
// ([memberDialer]), to a request's term ([jsapi.RequestTerm]): the dial
// crosses the link, and its SYN and SYN-ACK queue at the bottleneck every
// request across it is sized to wait behind. The handshake behind it has its
// own term ([leafHandshakeTerm]).
const tunnelDial = jsapi.RequestTerm

// tunnelConnectTerm bounds a leaf's whole CONNECT exchange with its tunnel
// (nats-server's Proxy.Timeout), the tunnel's dial to the member inside it:
// that dial ([tunnelDial]) plus a read's term ([jsprovision.ReadTerm]), so a
// failed dial reaches the leaf as the tunnel's 502 naming why. A term no
// longer than the dial cuts the leaf first, leaving it only a timeout to log;
// nats-server's default, a one-second dial timeout, leaves a leaf whose link
// dropped unable to dial again while other flows keep the link's queue deep.
const tunnelConnectTerm = tunnelDial + jsprovision.ReadTerm

// tunnelCredentialTerm bounds the wait for a connection's CONNECT: a read's
// term ([jsprovision.ReadTerm]). The leaf writes its request as its loopback
// dial returns, so a caller that sends nothing is closed a second later
// rather than at the leaf's whole connect term.
const tunnelCredentialTerm = jsprovision.ReadTerm

// tunnelHeaderMax bounds a CONNECT request, request line and headers
// together: nats-server's is under 300 bytes for the longest DNS name, so
// 4 KiB fits any it sends and refuses a stream of anything else at once.
const tunnelHeaderMax = 4 << 10

// tunnelHandshakes is how many connections the tunnel holds before they have
// shown its credential. At that many, the next one taken closes the OLDEST,
// never the new one, so callers on the loopback port that send nothing hold
// no place a leaf's attempt needs: the leaf's carries its CONNECT as it is
// taken and leaves its place once read ([tunnel.handshake]), so it is closed
// only if this many more arrive first. 64 makes that a flood that already
// starves any loopback listener's backlog, and costs at most 64 descriptors
// and goroutines for a second each ([tunnelCredentialTerm]); the leaf needs
// two (an attempt beside the one it abandoned).
const tunnelHandshakes = 64

// tunnelBuffer is what each direction of a tunnelled link is copied through:
// one 32 KiB buffer, reading no further ahead of what its far side took. The
// back-pressure a direct socket gives is spread over two more sockets' kernel
// buffers, which nothing the engine bounds counts: a window ends at a
// stream's acknowledgement or a piece's arrival, both past them.
const tunnelBuffer = 32 << 10

// tunnelAcceptBackoff and tunnelAcceptBackoffMax bound the wait before
// accepting again after a failed accept (a process out of descriptors, a
// connection reset before it was taken), doubling as net/http's server does.
const (
	tunnelAcceptBackoff    = 5 * time.Millisecond
	tunnelAcceptBackoffMax = time.Second
)

// tunnel is a leaf's way to the members: an HTTP CONNECT proxy on loopback,
// in the engine's process, that the leaf's link dials through (nats-server's
// leaf remote Proxy option, the one supported way onto a path whose dialer is
// unexported), so the upstream socket is the meter's (package doc, "What a
// link carries is read at its socket"). The leaf still signs the member's
// nonce through it, so the keyed link (ADR-0094) is unchanged.
//
// It FORWARDS WHEREVER ITS CREDENTIAL'S HOLDER ASKS, so only the in-process
// leaf server holds that credential, minted at each start; anything else on
// loopback is answered 407. IT LIVES AND DIES WITH THE LEAF SERVER: started
// before it, closed after it. A link needing TLS takes an https proxy, since
// nats-server refuses TLS on a proxied remote (v2.15.2, leafnode.go).
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
	// NO KEEPALIVE, as nats-server's own leaf dial keeps none (v2.15.2,
	// util.go natsDialTimeout): the leaf's pings find a member that vanished.
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
// backoff on a failed accept, and the broker's death ([Death]) where the
// listener closed before the tunnel did, so the node stops rather than run a
// leaf that can never link again.
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
	// A CLOSE AT EITHER END CLOSES BOTH, so the leaf sees a member that went
	// away as its link closing and dials again, rather than trust the link
	// until its pings give it up four minutes later.
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
// dials the member it names as the external dial does ([memberDialer]): up is
// the metered upstream socket, and pending what down sent after its request
// that the read took in with it.
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
