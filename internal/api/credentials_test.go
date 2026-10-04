package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// fakeCredentials is the engine's half of the credential feed: it keeps what
// the app registered, and a case fires it the way a committed identity batch
// would.
type fakeCredentials struct {
	mu sync.Mutex
	fn func(iamdomain.Moved)
}

func (f *fakeCredentials) SetOnIdentityMoved(fn func(iamdomain.Moved)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fn = fn
}

// fire hands one committed batch's move to whatever the app registered.
func (f *fakeCredentials) fire(t *testing.T, moved iamdomain.Moved) {
	t.Helper()
	f.mu.Lock()
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		t.Fatal("the app registered nothing on its credential feed, so a " +
			"revocation would reach no open socket")
	}
	fn(moved)
}

// liveRows is the identity estate as a node holds it for a few signed-in
// people, changeable while their sockets are open, counting how often each
// person's rows were read.
type liveRows struct {
	mu    sync.Mutex
	rows  map[string]session.Identity
	reads map[string]int
}

func (l *liveRows) Resolve(_ context.Context, _, person string) (session.Identity, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads[person]++
	if identity, ok := l.rows[person]; ok {
		return identity, nil
	}
	return session.Identity{Applied: cookieApplied}, nil
}

// AwaitApplied arrives at once: these rows cover every start a case mints at.
func (*liveRows) AwaitApplied(context.Context, uint64) error { return nil }

// end ends a person's session in the rows, as an applied sign-out leaves them.
func (l *liveRows) end(person string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	identity := l.rows[person]
	identity.Session.Ended = true
	l.rows[person] = identity
}

// deferPerson marks a person's rows as a node holding a record it retained
// leaves them: the read succeeds and cannot vouch for what it returned.
func (l *liveRows) deferPerson(person string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	identity := l.rows[person]
	identity.Deferred = true
	l.rows[person] = identity
}

// readsOf is how many times a person's rows have been read.
func (l *liveRows) readsOf(person string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reads[person]
}

// readAtLeast waits until a person's rows have been read n times.
func (l *liveRows) readAtLeast(t *testing.T, person string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		l.mu.Lock()
		got := l.reads[person]
		l.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's rows were read %d times, want %d", person, got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// signedInTab is one person's dashboard tab: their session and its socket.
type signedInTab struct {
	person, lineage string
	cookie          *http.Cookie
	conn            *websocket.Conn
}

// socketNode is one node serving the dashboard's socket through the REAL app
// and guard, over a session arm whose rows a case changes while sockets are
// open, and the credential feed a committed identity batch reaches it on.
type socketNode struct {
	b      config.Bootstrap
	signer *session.Signer
	rows   *liveRows
	feed   *fakeCredentials
	srv    *httptest.Server
	app    *api.App
	now    func() time.Time
}

// newSocketNode builds a node whose signer and session arm read now — the
// fixed clock, or a moving one for a case about a deadline.
func newSocketNode(t *testing.T, now func() time.Time) *socketNode {
	t.Helper()
	b := closedPosture()
	b.API.ExternalURL = "http://127.0.0.1:8080"
	signer, err := session.New(session.Options{
		Material: runtoken.Material{ActiveID: "k1",
			Keys: []runtoken.KeyMaterial{{ID: "k1", Material: "the-socket-signing-key"}}},
		Now: now,
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	rows := &liveRows{rows: map[string]session.Identity{}, reads: map[string]int{}}
	arm, err := auth.NewSessions(auth.SessionsDeps{
		Signer: signer, Directory: rows, Applier: rows, Chart: postureNoSeats{},
		External: b.API.ExternalBase(),
		Now:      now,
	})
	if err != nil {
		t.Fatalf("auth.NewSessions: %v", err)
	}
	feed := &fakeCredentials{}
	app := newApp(t, api.Options{Bootstrap: &b, Sessions: arm, Credentials: feed})
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	return &socketNode{b: b, signer: signer, rows: rows, feed: feed, srv: srv,
		app: app, now: now}
}

// tab signs a person in, at the node's clock, with a session whose absolute
// deadline is absolute, and opens their tab's socket with its cookie.
func (n *socketNode) tab(t *testing.T, login string, absolute time.Time) signedInTab {
	t.Helper()
	at := n.now()
	lineage := uuid.Must(uuid.NewV7())
	millis := at.UnixMilli()
	for i := range 6 {
		lineage[i] = byte(millis >> (8 * (5 - i)))
	}
	person := uuid.Must(uuid.NewV7()).String()
	bearer, err := n.signer.Mint(session.Mint{
		Lineage: lineage, Person: person, Epoch: 1, Generation: 1,
		StartPosition: 1, AbsoluteExpiresAt: absolute,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	n.rows.mu.Lock()
	n.rows.rows[person] = session.Identity{
		Applied: cookieApplied, Generation: 1,
		Session: session.LineageRow{Found: true, Epoch: 1, ProvedAt: at},
		Person: session.PersonRow{Found: true, Epoch: 1, Stage: iam.StageActive,
			Login: login, Grants: []iam.Grant{iam.GrantStateRead}},
	}
	n.rows.mu.Unlock()
	cookie := &http.Cookie{Name: session.CookieName(n.b.API.ExternalBase()), Value: bearer}
	conn, _, err := websocket.Dial(t.Context(),
		"ws"+strings.TrimPrefix(n.srv.URL, "http")+"/ws/stream",
		&websocket.DialOptions{HTTPHeader: http.Header{
			"Cookie": {cookie.String()},
			"Origin": {n.b.API.ExternalURL},
		}})
	if err != nil {
		t.Fatalf("a live session's handshake was refused: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	// THE HANDSHAKE AND THE DECISION THE SOCKET TAKES AS IT STARTS
	// LISTENING: both have read the rows before the case moves them.
	n.rows.readAtLeast(t, person, 2)
	return signedInTab{person: person, lineage: lineage.String(), cookie: cookie,
		conn: conn}
}

// probe is the plain GET a dashboard re-asks the socket's path with, presenting
// cookie: a REST request through the guard's ordinary resolution, answering
// 426 for a credential it serves, 401 for one it refuses and 503 for one this
// node cannot decide.
func (n *socketNode) probe(t *testing.T, cookie *http.Cookie) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/ws/stream", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	n.app.ServeHTTP(rec, req)
	return rec.Code
}

// A COMMITTED IDENTITY BATCH CLOSES THE OPEN TABS IT ENDED, and only those.
//
// A socket is authenticated at its handshake and then held for as long as the
// tab is, so a sign-out, a revocation or a suspension reaches an open tab only
// through the identity applier's word that it happened — which this node hears
// on the feed the app registers. Two failures are invisible from the writing
// side and are what this is for: a feed the app never registered on leaves
// every revoked session's tab receiving the company's state until it closes on
// its own, and a move delivered to every socket would close tabs whose
// sessions are fine. So two people are signed in, BOTH their sessions are
// ended in the rows, and the moves name them one at a time: the tab a move
// names closes `4401`, and the other stays open until its own move arrives.
func TestAnIdentityMoveClosesTheTabsItEnded(t *testing.T) {
	t.Parallel()
	node := newSocketNode(t, func() time.Time { return clock })
	rows, feed := node.rows, node.feed
	tab := func(login string) signedInTab {
		t.Helper()
		return node.tab(t, login, clock.Add(8*time.Hour))
	}
	jane, omar := tab("jane.doe"), tab("omar.haddad")
	rows.end(jane.person)
	rows.end(omar.person)

	feed.fire(t, iamdomain.Moved{Sessions: []string{jane.lineage}})
	if got := closeOf(t, jane.conn); got != stream.CloseUnauthenticated {
		t.Fatalf("the tab whose session ended closed %d, want %d", got,
			stream.CloseUnauthenticated)
	}
	stillOpen(t, omar.conn)

	feed.fire(t, iamdomain.Moved{People: []string{omar.person}})
	if got := closeOf(t, omar.conn); got != stream.CloseUnauthenticated {
		t.Fatalf("the tab whose person moved closed %d, want %d", got,
			stream.CloseUnauthenticated)
	}
}

// A MOVE THAT NAMES NOBODY CLOSES EVERY TAB, AND THE HANDSHAKE DECIDES.
//
// The fleet-wide session generation, a record this node retained (a newer
// build's, or one signed under a key it was not restarted with), a replaced
// estate and a node that stopped vouching for its identity rows (its applier
// halted on a record it cannot read, or frozen past the stall grace) each name
// nobody: whose it is sits where this node cannot read. Every open tab closes
// 1013 at once and nothing is read on its behalf; the reconnect's handshake is
// the decision, through the guard's ordinary resolution. Here one person's
// rows are what such a record leaves them — unvouched — and the other's are
// fine: both tabs close, neither's rows are read for it, and the two
// reconnects are answered differently, 503 for the person this node cannot
// vouch for and 426 (the upgrade the GET did not ask for) for the other.
//
// Mutation: decide every socket by the guard on such a move, and the tab whose
// rows are fine stays open.
func TestAMoveNamingNobodyClosesEveryTabAndTheHandshakeDecides(t *testing.T) {
	t.Parallel()
	node := newSocketNode(t, func() time.Time { return clock })
	jane := node.tab(t, "jane.doe", clock.Add(8*time.Hour))
	omar := node.tab(t, "omar.haddad", clock.Add(8*time.Hour))
	node.rows.deferPerson(jane.person)
	before := map[string]int{jane.person: node.rows.readsOf(jane.person),
		omar.person: node.rows.readsOf(omar.person)}

	node.feed.fire(t, iamdomain.Moved{Everyone: true})
	for _, tab := range []signedInTab{jane, omar} {
		if got := closeOf(t, tab.conn); got != stream.CloseUndecided {
			t.Fatalf("a tab a move naming nobody reached closed %d, want %d", got,
				stream.CloseUndecided)
		}
		if got := node.rows.readsOf(tab.person); got != before[tab.person] {
			t.Fatalf("a move naming nobody read the rows of a tab it closed "+
				"(%d reads, want %d)", got, before[tab.person])
		}
	}
	if got := node.probe(t, jane.cookie); got != http.StatusServiceUnavailable {
		t.Fatalf("the reconnect of a tab whose rows this node cannot vouch for "+
			"answered %d, want 503 — the handshake is where the client learns why",
			got)
	}
	if got := node.probe(t, omar.cookie); got != http.StatusUpgradeRequired {
		t.Fatalf("the reconnect of a tab whose rows are fine answered %d, want 426",
			got)
	}
}

// A TAB CLOSES AT ITS SESSION'S ABSOLUTE DEADLINE, with nothing else said.
//
// No record is written at a deadline, so no identity move will ever say it
// passed: what ends the socket is the timer its HANDLER arms from the
// handshake's own reading of when the credential ends (auth.Lifetime). The
// stream's own suite hands that instant straight to the socket, so only a
// case through the real app and guard holds the handler to passing it on.
// The control is a second tab whose session lasts hours, still open after.
//
// Mutation: drop the handshake's end from the socket's credential, and the
// first tab stays open past its deadline.
func TestATabClosesAtItsSessionsAbsoluteDeadline(t *testing.T) {
	t.Parallel()
	node := newSocketNode(t, func() time.Time { return time.Now().UTC() })
	ending := node.tab(t, "jane.doe", time.Now().Add(3*time.Second))
	lasting := node.tab(t, "omar.haddad", time.Now().Add(8*time.Hour))

	if got := closeOf(t, ending.conn); got != stream.CloseUnauthenticated {
		t.Fatalf("the tab whose session reached its absolute deadline closed %d, "+
			"want %d", got, stream.CloseUnauthenticated)
	}
	stillOpen(t, lasting.conn)
}

// AN OPEN TAB OUTLIVES ITS SESSION'S IDLE DEADLINE, and a request does not.
//
// A session's idle deadline is moved by a re-issue on a REST response, and a
// socket can never receive one, so the bearer it holds carries the
// handshake's deadline however busy the tab has been. Decided on that
// deadline, every tab open past session.Idle would close on the first
// identity move anywhere near it while its person was at the screen; so the
// HANDLER decides an open socket through the guard's ResolveOpen, which sets
// that deadline aside and asks the rows. A REST request presenting the same
// cookie at the same instant is held to it, which is the control: the cookie
// is past its idle deadline, and only the socket may outlive it.
//
// Two moves, and the socket's rows are read for each: the socket acts on one
// signal at a time, so the second read is proof the first decision left it
// open.
//
// Mutation: decide an open socket through the guard's ordinary Resolve, and
// the first move closes the tab.
func TestAnOpenTabOutlivesItsSessionsIdleDeadline(t *testing.T) {
	t.Parallel()
	var at atomic.Int64
	at.Store(clock.UnixNano())
	node := newSocketNode(t, func() time.Time { return time.Unix(0, at.Load()).UTC() })
	jane := node.tab(t, "jane.doe", clock.Add(session.Idle+time.Hour))
	if got := node.probe(t, jane.cookie); got != http.StatusUpgradeRequired {
		t.Fatalf("the cookie at its handshake's instant answered %d, want 426", got)
	}

	at.Store(clock.Add(session.Idle + time.Minute).UnixNano())
	before := node.rows.readsOf(jane.person)
	node.feed.fire(t, iamdomain.Moved{People: []string{jane.person}})
	node.rows.readAtLeast(t, jane.person, before+1)
	node.feed.fire(t, iamdomain.Moved{People: []string{jane.person}})
	node.rows.readAtLeast(t, jane.person, before+2)
	stillOpen(t, jane.conn)

	if got := node.probe(t, jane.cookie); got != http.StatusUnauthorized {
		t.Fatalf("a request presenting the cookie past its idle deadline answered "+
			"%d, want 401", got)
	}
}

// closeOf reads until conn closes and reports how.
func closeOf(t *testing.T, conn *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

// stillOpen proves conn is open: a ping answered with a pong, every frame in
// between skipped.
func stillOpen(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ping, err := json.Marshal(map[string]any{"kind": "ping"})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, ping); err != nil {
		t.Fatalf("a socket that should be open is gone: %v", err)
	}
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("a socket that should be open closed (%v): something "+
				"that did not end its credential ended it", err)
		}
		var frame map[string]any
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if frame["kind"] == string(stream.KindPong) {
			return
		}
	}
}
