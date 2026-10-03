package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/stream"
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
	conn            *websocket.Conn
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
	b := closedPosture()
	b.API.ExternalURL = "http://127.0.0.1:8080"
	signer, err := session.New(session.Options{
		Material: runtoken.Material{ActiveID: "k1",
			Keys: []runtoken.KeyMaterial{{ID: "k1", Material: "the-socket-signing-key"}}},
		Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	rows := &liveRows{rows: map[string]session.Identity{}, reads: map[string]int{}}
	arm, err := auth.NewSessions(auth.SessionsDeps{
		Signer: signer, Directory: rows, Applier: rows, Chart: postureNoSeats{},
		External: b.API.ExternalBase(), Audit: silentAudit{},
		Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("auth.NewSessions: %v", err)
	}
	feed := &fakeCredentials{}
	srv := httptest.NewServer(newApp(t, api.Options{
		Bootstrap: &b, Sessions: arm, Credentials: feed,
	}))
	t.Cleanup(srv.Close)

	tab := func(login string) signedInTab {
		t.Helper()
		lineage := uuid.Must(uuid.NewV7())
		millis := clock.UnixMilli()
		for i := range 6 {
			lineage[i] = byte(millis >> (8 * (5 - i)))
		}
		person := uuid.Must(uuid.NewV7()).String()
		bearer, err := signer.Mint(session.Mint{
			Lineage: lineage, Person: person, Epoch: 1, Generation: 1,
			StartPosition: 1, AbsoluteExpiresAt: clock.Add(8 * time.Hour),
		})
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		rows.mu.Lock()
		rows.rows[person] = session.Identity{
			Applied: cookieApplied, Generation: 1,
			Session: session.LineageRow{Found: true, Epoch: 1, ProvedAt: clock},
			Person: session.PersonRow{Found: true, Epoch: 1, Stage: iam.StageActive,
				Login: login, Grants: []iam.Grant{iam.GrantStateRead}},
		}
		rows.mu.Unlock()
		cookie := &http.Cookie{Name: session.CookieName(b.API.ExternalBase()), Value: bearer}
		conn, _, err := websocket.Dial(t.Context(),
			"ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/stream",
			&websocket.DialOptions{HTTPHeader: http.Header{
				"Cookie": {cookie.String()},
				"Origin": {b.API.ExternalURL},
			}})
		if err != nil {
			t.Fatalf("a live session's handshake was refused: %v", err)
		}
		t.Cleanup(func() { _ = conn.CloseNow() })
		// THE HANDSHAKE AND THE DECISION THE SOCKET TAKES AS IT STARTS
		// LISTENING: both have read the rows before the case moves them.
		rows.readAtLeast(t, person, 2)
		return signedInTab{person: person, lineage: lineage.String(), conn: conn}
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
		t.Fatalf("the other tab's socket is gone: %v", err)
	}
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("the other tab's socket closed (%v): a move naming one "+
				"session ended another", err)
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
