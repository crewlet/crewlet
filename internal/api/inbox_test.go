package api_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/tracker"
)

// fakeInbox is the engine's half of the inbox feed: it keeps what the app
// registered, and a case fires it the way a committed tracker batch would.
type fakeInbox struct {
	mu sync.Mutex
	fn func([]tracker.InboxMovement)
}

func (f *fakeInbox) SetOnInboxMoved(fn func([]tracker.InboxMovement)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fn = fn
}

// fire hands one committed batch's movements to whatever the app registered.
func (f *fakeInbox) fire(t *testing.T, moved ...tracker.InboxMovement) {
	t.Helper()
	f.mu.Lock()
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		t.Fatal("the app registered nothing on its inbox feed, so a committed " +
			"batch would reach no socket")
	}
	fn(moved)
}

// AN INBOX MOVEMENT REACHES THE SOCKETS HOLDING THAT SEAT, AND NO OTHER.
//
// This is how a person learns they have work: the tracker's applier says whose
// inbox a committed batch moved, and the API pushes that to the sockets
// WATCHING that seat. Two failures are invisible from the sending side and are
// what the case is for. Pushed to every socket, one person's screen would learn
// when and why somebody else's inbox moved — the `work_inbox` question refuses
// exactly that. And pushed to nobody (a feed the app never registered on, or a
// frame built without its seat), every screen would learn a poll interval late,
// which reads as a quiet company rather than a broken wire.
//
// THE PAYLOAD IS IDENTIFIERS AND A COUNT, never content: the dashboard fetches
// the inbox through the question that decides who may read it, so the frame
// says only that it should.
func TestInboxChangedReachesOnlyTheSocketsHoldingThatSeat(t *testing.T) {
	t.Parallel()
	feed := &fakeInbox{}
	b := config.DefaultBootstrap()
	// state:read ALONE, the grant the inbox question takes: a holder of the
	// admin grant may watch any seat, so a case about the routing would
	// pass through a hole in the authority instead.
	reader := []iam.Grant{iam.GrantStateRead}
	authorize(&b,
		config.APIToken{ID: "alice", Token: "alice-token-long-enough-to-pass", Grants: reader},
		config.APIToken{ID: "bob", Token: "bob-token-long-enough-to-pass", Grants: reader},
		config.APIToken{ID: "carol", Token: "carol-token-long-enough-to-pass", Grants: reader},
	)
	seats := map[string]string{
		iam.TokenLogin("alice"): "alice-seat",
		iam.TokenLogin("bob"):   "bob-seat",
		iam.TokenLogin("carol"): "carol-seat",
	}
	a := newApp(t, api.Options{
		Bootstrap: &b,
		Inbox:     feed,
		SeatBindings: auth.SeatBindings{
			Directory: inboxBindings(seats), Chart: inboxChart(seats),
		},
	})
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)

	alice := dialInbox(t, srv.URL, "alice-token-long-enough-to-pass")
	bob := dialInbox(t, srv.URL, "bob-token-long-enough-to-pass")
	// carol watches NOTHING: a socket that never asked is the control for
	// "reaches every socket".
	carol := dialInbox(t, srv.URL, "carol-token-long-enough-to-pass")
	alice.send(t, map[string]any{"kind": "watch", "seat": "alice-seat"})
	bob.send(t, map[string]any{"kind": "watch", "seat": "bob-seat"})
	hub := a.Stream().Hub()
	deadline := time.Now().Add(10 * time.Second)
	for hub.Watchers("alice-seat") != 1 || hub.Watchers("bob-seat") != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the watches never reached the index (alice %d, bob %d)",
				hub.Watchers("alice-seat"), hub.Watchers("bob-seat"))
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Two batches IN ORDER, so a socket that received alice's frame would
	// have it queued in front of its own.
	feed.fire(t, tracker.InboxMovement{
		Handle: "alice-seat", UnreadDelta: 1, Subject: "task:1f0c", Reason: tracker.ReasonAssignee,
	})
	feed.fire(t, tracker.InboxMovement{
		Handle: "bob-seat", UnreadDelta: -2, Subject: "task:9a1e", Reason: tracker.ReasonMention,
	})

	got := alice.nextInbox(t)
	if got.Seat != "alice-seat" {
		t.Fatalf("alice's socket received a frame for %q", got.Seat)
	}
	want := map[string]any{
		"handle": "alice-seat", "unread_delta": float64(1),
		"subject": "task:1f0c", "reason": "assignee",
	}
	if !maps.Equal(got.Data, want) {
		t.Fatalf("the frame carried %v, want exactly %v — identifiers and a "+
			"count, and nothing a reader would need the inbox question for", got.Data, want)
	}
	if other := bob.nextInbox(t); other.Seat != "bob-seat" {
		t.Fatalf("bob's socket received the frame for %q before its own: one "+
			"person's screen learned about somebody else's inbox", other.Seat)
	}
	carol.drainUntilPong(t)
}

// inboxSocket is one dashboard tab on the live channel.
type inboxSocket struct {
	conn *websocket.Conn
}

// inboxFrame is the part of a pushed frame the case reads.
type inboxFrame struct {
	Kind string         `json:"kind"`
	Seat string         `json:"seat"`
	Data map[string]any `json:"data"`
}

func dialInbox(t *testing.T, base, token string) *inboxSocket {
	t.Helper()
	conn, _, err := websocket.Dial(t.Context(),
		"ws"+strings.TrimPrefix(base, "http")+"/ws/stream?token="+token, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return &inboxSocket{conn: conn}
}

func (s *inboxSocket) send(t *testing.T, frame map[string]any) {
	t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.conn.Write(t.Context(), websocket.MessageText, raw); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func (s *inboxSocket) read(t *testing.T) inboxFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, raw, err := s.conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f inboxFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return f
}

// nextInbox reads past every other push to the next inbox_changed frame.
func (s *inboxSocket) nextInbox(t *testing.T) inboxFrame {
	t.Helper()
	for {
		if f := s.read(t); f.Kind == "inbox_changed" {
			return f
		}
	}
}

// drainUntilPong asks the socket to answer a ping and fails on any
// inbox_changed frame queued in front of the answer — every movement was
// fanned out before the ping was written, so a frame this socket was sent
// would arrive first.
func (s *inboxSocket) drainUntilPong(t *testing.T) {
	t.Helper()
	s.send(t, map[string]any{"kind": "ping"})
	for {
		f := s.read(t)
		switch f.Kind {
		case "inbox_changed":
			t.Fatalf("a socket that watched nothing received the frame for %q", f.Seat)
		case "pong":
			return
		}
	}
}

// inboxBindings binds each Tier A token's login to its seat, as an active
// machine row decided at chart position 1.
type inboxBindings map[string]string

func (b inboxBindings) BoundSeat(_ context.Context, login string) (session.PersonRow, error) {
	seat, ok := b[login]
	if !ok {
		return session.PersonRow{}, nil
	}
	return session.PersonRow{Found: true, Stage: iam.StageActive, Login: login,
		Seat: seat, SeatAt: 1}, nil
}

// inboxChart holds each bound seat as a human seat, at a position that covers
// every binding.
type inboxChart map[string]string

func (c inboxChart) Seat(_ context.Context, ref string) (session.Seat, bool, error) {
	for _, seat := range c {
		if seat == ref {
			return session.Seat{Handle: seat, Kind: "human"}, true, nil
		}
	}
	return session.Seat{}, false, nil
}

func (inboxChart) Position(context.Context) (uint64, time.Duration, error) {
	return 1, 0, nil
}
