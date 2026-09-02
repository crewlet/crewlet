package mattermost_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// recorder collects what the fleet republished.
type recorder struct {
	mu   sync.Mutex
	sent []*events.Event
	fail error
}

func (r *recorder) Publish(_ context.Context, topic string, ev *events.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	if topic != topics.NotificationsInbound {
		return errors.New("published onto " + topic)
	}
	r.sent = append(r.sent, ev)
	return nil
}

func (r *recorder) posts() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, ev := range r.sent {
		w, ok := events.DataAs[*types.RawWebhook](ev)
		if !ok {
			continue
		}
		out = append(out, w.Body)
	}
	return out
}

func (r *recorder) ids() []string {
	var out []string
	for _, body := range r.posts() {
		if p, ok := body["post"].(map[string]any); ok {
			out = append(out, str(p, "id"))
		}
	}
	return out
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// fakeSocket hands out a scripted sequence, then blocks until closed.
type fakeSocket struct {
	frames chan map[string]any
	closed chan struct{}
	once   sync.Once

	// pingErr, when set, makes every heartbeat fail — the L7 half-open a
	// TCP-level check cannot see.
	pingErr error
	pings   atomic.Int64
}

func newSocket(frames ...map[string]any) *fakeSocket {
	s := &fakeSocket{frames: make(chan map[string]any, len(frames)+8), closed: make(chan struct{})}
	for _, f := range frames {
		s.frames <- f
	}
	return s
}

func (s *fakeSocket) Read(ctx context.Context) (map[string]any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.closed:
		return nil, errors.New("socket closed")
	case f := <-s.frames:
		return f, nil
	}
}

func (s *fakeSocket) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *fakeSocket) Ping(ctx context.Context) error {
	s.pings.Add(1)
	if s.pingErr != nil {
		return s.pingErr
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.closed:
		return errors.New("socket closed")
	default:
		return nil
	}
}

// fastBackoff keeps the reconnect PATH under test rather than the sleep:
// the shipped schedule's floor is a whole second, which is right against a
// real server and far too long to exercise a reconnect against a fake one.
var fastBackoff = []time.Duration{time.Millisecond, 2 * time.Millisecond}

func frame(id, message string, mutate func(map[string]any)) map[string]any {
	body := map[string]any{
		"event":        "posted",
		"channel_type": "O",
		"channel_name": "eng",
		"post": map[string]any{
			"id": id, "channel_id": "C1", "user_id": "u-ana",
			"message": message, "create_at": float64(1718003000000),
			"delete_at": float64(0),
		},
	}
	if mutate != nil {
		mutate(body)
	}
	return body
}

func waitFor(t *testing.T, want int, got func() int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for got() < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := got(); n < want {
		t.Fatalf("saw %d of an expected %d", n, want)
	}
}

func TestEachPostIsRepublishedAsAWebhook(t *testing.T) {
	rec := &recorder{}
	sock := newSocket(frame("p1", "hello", nil))
	f, err := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: rec,
		Backoff:   fastBackoff,
		Connect: func(context.Context, mattermost.Seat, *mattermost.Client) (mattermost.Socket, error) {
			return sock, nil
		},
	})
	if err != nil {
		t.Fatalf("NewFleet: %v", err)
	}
	s := newServer(t)
	if err := f.Add(t.Context(), seat, client(t, s)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	defer f.Stop()

	waitFor(t, 1, func() int { return len(rec.posts()) })
	body := rec.posts()[0]
	// The seat's own identity rides along: the parser needs it to
	// suppress this seat's own posts, and the prompt needs it to teach
	// the agent how it is addressed. Neither is in the payload.
	if body["bot_user_id"] != seat.UserID || body["bot_username"] != seat.Username {
		t.Fatalf("the seat's identity did not ride along: %v", body)
	}
	// And the envelope names the seat, since every post here is already
	// addressed to whichever socket saw it.
	w, _ := events.DataAs[*types.RawWebhook](rec.sent[0])
	if w.Handle != "swe" {
		t.Fatalf("the envelope names %q", w.Handle)
	}
	if rec.sent[0].Source != mattermost.Backend {
		t.Fatalf("the envelope's source is %q", rec.sent[0].Source)
	}
	// A live post says nothing about being replayed.
	if _, replayed := body["replayed"]; replayed {
		t.Fatal("a live post claims to be replayed")
	}
}

// The socket carries typing indicators, presence changes and status updates
// too, and none of them is something to wake a seat for.
func TestNonPostFramesAreIgnored(t *testing.T) {
	rec := &recorder{}
	sock := newSocket(
		map[string]any{"event": "typing", "user_id": "u-ana"},
		map[string]any{"event": "status_change"},
		frame("p1", "hello", nil),
	)
	f, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: rec,
		Backoff:   fastBackoff,
		Connect: func(context.Context, mattermost.Seat, *mattermost.Client) (mattermost.Socket, error) {
			return sock, nil
		},
	})
	s := newServer(t)
	f.Add(t.Context(), seat, client(t, s))
	defer f.Stop()

	waitFor(t, 1, func() int { return len(rec.posts()) })
	time.Sleep(30 * time.Millisecond)
	if got := rec.ids(); len(got) != 1 || got[0] != "p1" {
		t.Fatalf("republished %v", got)
	}
	// Counted over EVERY envelope, not just the ones carrying a post: a
	// typing indicator republished as a webhook still reaches the parser
	// and still costs a delivery, even though it names nothing.
	if got := len(rec.posts()); got != 1 {
		t.Fatalf("%d envelopes were published, want only the post", got)
	}
}

// The window is BOUNDED because the purpose is to cover a blip, not to catch
// up after an outage: every replayed message costs a full agent turn, so an
// hour-long gap replayed in full is both expensive and wrong — those
// conversations have been resolved by people since.
func TestTheBackfillWindowIsBounded(t *testing.T) {
	s := newServer(t)
	connectedAt := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	// THE OUTAGE: the server's clock moves on by an hour while the seat
	// is away, which is what makes the gap wider than the window. A
	// fixture where the cursor is merely old relative to a frozen clock
	// cannot express this at all.
	var away atomic.Bool
	serverNow := func() time.Time {
		if away.Load() {
			return connectedAt.Add(time.Hour)
		}
		return connectedAt
	}

	var mu sync.Mutex
	var cursors []string
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Date", serverNow().Format(http.TimeFormat))
		switch {
		case strings.Contains(r.URL.Path, "/posts"):
			mu.Lock()
			cursors = append(cursors, r.URL.Query().Get("since"))
			mu.Unlock()
			w.Write([]byte(`{"order":[],"posts":{}}`))
		case strings.HasSuffix(r.URL.Path, "/channels"):
			json.NewEncoder(w).Encode([]map[string]any{{"id": "C1", "name": "eng", "type": "O"}})
		case strings.HasSuffix(r.URL.Path, "/teams"):
			json.NewEncoder(w).Encode([]map[string]any{{"id": "t1", "name": "eng"}})
		default:
			w.Write([]byte(`{"id":"bot-1"}`))
		}
		return true
	})

	rec := &recorder{}
	sockets := []*fakeSocket{
		newSocket(frame("p1", "live", func(b map[string]any) {
			b["post"].(map[string]any)["create_at"] =
				float64(connectedAt.Add(time.Second).UnixMilli())
		})),
		newSocket(),
	}
	var dials atomic.Int32
	f, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: rec, Backoff: fastBackoff,
		// A window far shorter than the outage.
		Backfill: time.Minute,
		Connect: func(context.Context, mattermost.Seat, *mattermost.Client) (mattermost.Socket, error) {
			return sockets[min(int(dials.Add(1))-1, len(sockets)-1)], nil
		},
	})
	f.Add(t.Context(), seat, client(t, s))
	defer f.Stop()

	waitFor(t, 1, func() int { return len(rec.posts()) })
	away.Store(true)
	sockets[0].Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(cursors)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	got := append([]string(nil), cursors...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("no backfill happened")
	}
	// The floor is measured from NOW minus the window — measuring it from
	// the cursor makes it unreachable, since the cursor is by definition
	// the newest thing seen.
	want := strconv.FormatInt(connectedAt.Add(time.Hour-time.Minute).UnixMilli(), 10)
	if got[0] != want {
		t.Fatalf("the backfill resumed from %s, want the window floor %s", got[0], want)
	}
}

func TestADuplicatePostIsPublishedOnce(t *testing.T) {
	rec := &recorder{}
	sock := newSocket(
		frame("p1", "hello", nil),
		frame("p1", "hello", nil),
		frame("p2", "again", nil),
	)
	f, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: rec,
		Backoff:   fastBackoff,
		Connect: func(context.Context, mattermost.Seat, *mattermost.Client) (mattermost.Socket, error) {
			return sock, nil
		},
	})
	s := newServer(t)
	f.Add(t.Context(), seat, client(t, s))
	defer f.Stop()

	waitFor(t, 2, func() int { return len(rec.posts()) })
	time.Sleep(30 * time.Millisecond)
	if got := strings.Join(rec.ids(), ","); got != "p1,p2" {
		t.Fatalf("republished %q", got)
	}
}

// Mattermost replays nothing on reconnect, so a seat re-reads the gap
// itself — and the cursor it resumes from is the SERVER's clock.
func TestAReconnectReplaysTheGapInOrder(t *testing.T) {
	s := newServer(t)
	serverNow := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	var backfilled atomic.Int32
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Date", serverNow.Format(http.TimeFormat))
		// MOST SPECIFIC FIRST: a channels path is
		// /users/{id}/teams/{id}/channels, so matching /teams before it
		// answers the channel read with a team list.
		switch {
		case strings.Contains(r.URL.Path, "/posts"):
			backfilled.Add(1)
			json.NewEncoder(w).Encode(map[string]any{
				"order": []string{"g2", "g1"},
				"posts": map[string]any{
					"g1": map[string]any{
						"id": "g1", "channel_id": "C1", "user_id": "u-ana",
						"message": "missed one", "create_at": float64(1718003001000),
					},
					"g2": map[string]any{
						"id": "g2", "channel_id": "C1", "user_id": "u-ana",
						"message": "missed two", "create_at": float64(1718003002000),
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/channels"):
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": "C1", "name": "eng", "type": "O"},
			})
		case strings.HasSuffix(r.URL.Path, "/teams"):
			json.NewEncoder(w).Encode([]map[string]any{{"id": "t1", "name": "eng"}})
		default:
			w.Write([]byte(`{"id":"bot-1","username":"agent-swe"}`))
		}
		return true
	})

	rec := &recorder{}
	first := newSocket(frame("p1", "before the drop", nil))
	second := newSocket()
	var dials atomic.Int32
	f, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: rec,
		Backoff:   fastBackoff,
		Connect: func(context.Context, mattermost.Seat, *mattermost.Client) (mattermost.Socket, error) {
			if dials.Add(1) == 1 {
				return first, nil
			}
			return second, nil
		},
	})
	f.Add(t.Context(), seat, client(t, s))
	defer f.Stop()

	waitFor(t, 1, func() int { return len(rec.posts()) })
	first.Close() // the drop

	waitFor(t, 3, func() int { return len(rec.posts()) })
	got := rec.ids()
	if strings.Join(got, ",") != "p1,g1,g2" {
		t.Fatalf("the gap replayed as %v, want it in order after p1", got)
	}
	// A replayed message says so: "this arrived while I was disconnected"
	// changes how stale the seat should assume the conversation is.
	for _, body := range rec.posts()[1:] {
		if body["replayed"] != true {
			t.Fatalf("a replayed post does not say so: %v", body)
		}
	}
	if backfilled.Load() == 0 {
		t.Fatal("no backfill was attempted")
	}
}

// A first connect is starting, not resuming: there is no gap, and replaying
// from the epoch would wake the seat with a channel's whole history.
func TestAFirstConnectReplaysNothing(t *testing.T) {
	s := newServer(t)
	var backfilled atomic.Int32
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		if strings.Contains(r.URL.Path, "/posts") {
			backfilled.Add(1)
		}
		w.Write([]byte(`{"id":"bot-1"}`))
		return true
	})

	rec := &recorder{}
	sock := newSocket(frame("p1", "hello", nil))
	f, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: rec,
		Backoff:   fastBackoff,
		Connect: func(context.Context, mattermost.Seat, *mattermost.Client) (mattermost.Socket, error) {
			return sock, nil
		},
	})
	f.Add(t.Context(), seat, client(t, s))
	defer f.Stop()

	waitFor(t, 1, func() int { return len(rec.posts()) })
	if backfilled.Load() != 0 {
		t.Fatalf("a first connect backfilled %d times", backfilled.Load())
	}
}

// A seat that cannot connect keeps trying, on a capped backoff — a
// configuration problem an operator has to see, without hammering a server
// that is down.
func TestAFailingConnectionIsRetried(t *testing.T) {
	rec := &recorder{}
	var dials atomic.Int32
	sock := newSocket(frame("p1", "at last", nil))
	f, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: rec,
		Backoff:   fastBackoff,
		Connect: func(context.Context, mattermost.Seat, *mattermost.Client) (mattermost.Socket, error) {
			if dials.Add(1) < 3 {
				return nil, errors.New("connection refused")
			}
			return sock, nil
		},
	})
	s := newServer(t)
	f.Add(t.Context(), seat, client(t, s))
	defer f.Stop()

	waitFor(t, 1, func() int { return len(rec.posts()) })
	if dials.Load() < 3 {
		t.Fatalf("the fleet gave up after %d attempts", dials.Load())
	}
}

// A live config apply legitimately changes a seat's token, and a fleet that
// kept the old socket would keep listening as an identity the operator has
// revoked.
func TestAddingASeatTwiceReplacesItsSocket(t *testing.T) {
	rec := &recorder{}
	first := newSocket(frame("p1", "on the old socket", nil))
	second := newSocket(frame("p2", "on the new socket", nil))
	var dials atomic.Int32
	f, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: rec,
		Backoff:   fastBackoff,
		Connect: func(context.Context, mattermost.Seat, *mattermost.Client) (mattermost.Socket, error) {
			if dials.Add(1) == 1 {
				return first, nil
			}
			return second, nil
		},
	})
	s := newServer(t)
	f.Add(t.Context(), seat, client(t, s))
	defer f.Stop()

	// WAIT for the first socket to be live before replacing it. Without
	// this the second Add can cancel the first loop before it has dialled
	// at all, and the test would then be asserting nothing.
	waitFor(t, 1, func() int { return len(rec.posts()) })
	if got := f.Handles(); len(got) != 1 {
		t.Fatalf("Handles = %v", got)
	}

	f.Add(t.Context(), seat, client(t, s))
	if got := f.Handles(); len(got) != 1 {
		t.Fatalf("re-adding a seat left %v", got)
	}
	// The replaced socket is CLOSED. A fleet that kept it would keep
	// listening as an identity the operator has just revoked.
	select {
	case <-first.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the replaced socket was left open")
	}
	waitFor(t, 2, func() int { return len(rec.posts()) })
	if got := strings.Join(rec.ids(), ","); got != "p1,p2" {
		t.Fatalf("republished %q, want the new socket's post after the old one's", got)
	}
}

func TestRemovingASeatStopsItsLoop(t *testing.T) {
	rec := &recorder{}
	sock := newSocket()
	f, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: rec,
		Backoff:   fastBackoff,
		Connect: func(context.Context, mattermost.Seat, *mattermost.Client) (mattermost.Socket, error) {
			return sock, nil
		},
	})
	s := newServer(t)
	f.Add(t.Context(), seat, client(t, s))

	f.Remove("swe")
	if got := f.Handles(); len(got) != 0 {
		t.Fatalf("Handles = %v after Remove", got)
	}
	// Removing an unknown seat is a no-op rather than a panic: a config
	// apply removing a seat this node never ran is ordinary.
	f.Remove("nobody")
	f.Stop()
}

func TestAFleetNeedsAPublisherAndItsSeatsNeedClients(t *testing.T) {
	if _, err := mattermost.NewFleet(mattermost.FleetOptions{}); err == nil {
		t.Fatal("a fleet was built with no publisher")
	}
	f, _ := mattermost.NewFleet(mattermost.FleetOptions{Publisher: &recorder{}})
	s := newServer(t)
	if err := f.Add(t.Context(), mattermost.Seat{Username: "x"}, client(t, s)); err == nil {
		t.Fatal("a seat with no handle was attached")
	}
	if err := f.Add(t.Context(), seat, nil); err == nil {
		t.Fatal("a seat with no client was attached")
	}
}

// The cursor never moves BACKWARDS. A backfill replays oldest-first while
// the live socket delivers newest, so the two interleave at a reconnect —
// and a cursor that regressed would re-read the same gap on the next drop,
// waking the seat again for messages it has already answered.
func TestTheCursorNeverRegresses(t *testing.T) {
	s := newServer(t)
	serverNow := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	// A live post is necessarily stamped AFTER the connect, and the
	// backfill then hands back something older — which is exactly the
	// interleave at a reconnect boundary.
	live := serverNow.Add(time.Minute)
	old := serverNow.Add(-5 * time.Minute)

	var mu sync.Mutex
	var cursors []string
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Date", serverNow.Format(http.TimeFormat))
		switch {
		case strings.Contains(r.URL.Path, "/posts"):
			mu.Lock()
			cursors = append(cursors, r.URL.Query().Get("since"))
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{
				"order": []string{"g1"},
				"posts": map[string]any{"g1": map[string]any{
					"id": "g1", "channel_id": "C1", "user_id": "u-ana",
					"message": "an older one", "create_at": float64(old.UnixMilli()),
				}},
			})
		case strings.HasSuffix(r.URL.Path, "/channels"):
			json.NewEncoder(w).Encode([]map[string]any{{"id": "C1", "name": "eng", "type": "O"}})
		case strings.HasSuffix(r.URL.Path, "/teams"):
			json.NewEncoder(w).Encode([]map[string]any{{"id": "t1", "name": "eng"}})
		default:
			w.Write([]byte(`{"id":"bot-1"}`))
		}
		return true
	})

	rec := &recorder{}
	sockets := []*fakeSocket{
		newSocket(frame("p1", "live", func(b map[string]any) {
			b["post"].(map[string]any)["create_at"] = float64(live.UnixMilli())
		})),
		newSocket(),
		newSocket(),
	}
	var dials atomic.Int32
	f, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: rec, Backoff: fastBackoff,
		Connect: func(context.Context, mattermost.Seat, *mattermost.Client) (mattermost.Socket, error) {
			return sockets[min(int(dials.Add(1))-1, len(sockets)-1)], nil
		},
	})
	f.Add(t.Context(), seat, client(t, s))
	defer f.Stop()

	waitFor(t, 1, func() int { return len(rec.posts()) })
	sockets[0].Close() // the first drop: backfill hands back an OLDER post

	waitFor(t, 2, func() int { return len(rec.posts()) })
	sockets[1].Close() // the second drop: the cursor must not have regressed

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(cursors)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	got := append([]string(nil), cursors...)
	mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("only %d backfills happened", len(got))
	}
	want := strconv.FormatInt(live.UnixMilli(), 10)
	for i, c := range got {
		if c != want {
			t.Fatalf("backfill %d resumed from %s, want the newest seen (%s)", i, c, want)
		}
	}
}

// A seat in three teams should still hear two of them: one team's channel
// read failing must not lose the others' backfill.
func TestOneUnreadableTeamDoesNotLoseTheRest(t *testing.T) {
	s := newServer(t)
	serverNow := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Date", serverNow.Format(http.TimeFormat))
		switch {
		case strings.Contains(r.URL.Path, "/posts"):
			json.NewEncoder(w).Encode(map[string]any{
				"order": []string{"g1"},
				"posts": map[string]any{"g1": map[string]any{
					"id": "g1", "channel_id": "C2", "user_id": "u-ana",
					"message":   "from the healthy team",
					"create_at": float64(serverNow.Add(-time.Minute).UnixMilli()),
				}},
			})
		case strings.Contains(r.URL.Path, "/teams/broken/channels"):
			w.WriteHeader(http.StatusForbidden)
		case strings.HasSuffix(r.URL.Path, "/channels"):
			json.NewEncoder(w).Encode([]map[string]any{{"id": "C2", "name": "ops", "type": "O"}})
		case strings.HasSuffix(r.URL.Path, "/teams"):
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": "broken", "name": "broken"}, {"id": "t2", "name": "ops"},
			})
		default:
			w.Write([]byte(`{"id":"bot-1"}`))
		}
		return true
	})

	rec := &recorder{}
	sockets := []*fakeSocket{newSocket(frame("p1", "live", nil)), newSocket()}
	var dials atomic.Int32
	f, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: rec, Backoff: fastBackoff,
		Connect: func(context.Context, mattermost.Seat, *mattermost.Client) (mattermost.Socket, error) {
			return sockets[min(int(dials.Add(1))-1, len(sockets)-1)], nil
		},
	})
	f.Add(t.Context(), seat, client(t, s))
	defer f.Stop()

	waitFor(t, 1, func() int { return len(rec.posts()) })
	sockets[0].Close()

	waitFor(t, 2, func() int { return len(rec.posts()) })
	if got := rec.ids(); got[len(got)-1] != "g1" {
		t.Fatalf("the healthy team's backfill was lost: %v", got)
	}
}

// A SEAT WHOSE SERVER STOPS ANSWERING RECONNECTS, and nothing in this package
// could previously tell that it had.
//
// coder/websocket answers a server's pings internally without returning from
// Read, so a quiet channel and a dead server look identical from this side.
// TCP keepalives rescue a genuinely dead path in about eleven minutes; an L7
// half-open — a proxy that tore down the upstream connection while still
// answering keepalives — never resolves at all, and the seat is deaf
// indefinitely with no log line to say so.
func TestASeatWhoseServerStopsAnsweringIsReconnected(t *testing.T) {
	rec := &recorder{}
	dead := newSocket()
	dead.pingErr = errors.New("no pong: the upstream connection is gone")
	live := newSocket(frame("p1", "back", nil))

	var dials atomic.Int64
	f, err := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher:    rec,
		Backoff:      fastBackoff,
		PingInterval: 5 * time.Millisecond,
		PongTimeout:  50 * time.Millisecond,
		Connect: func(context.Context, mattermost.Seat, *mattermost.Client) (mattermost.Socket, error) {
			if dials.Add(1) == 1 {
				return dead, nil
			}
			return live, nil
		},
	})
	if err != nil {
		t.Fatalf("NewFleet: %v", err)
	}
	s := newServer(t)
	if err := f.Add(t.Context(), seat, client(t, s)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	defer f.Stop()

	// The post only arrives on the SECOND socket, so receiving it is proof
	// the first was abandoned rather than merely pinged.
	waitFor(t, 1, func() int { return len(rec.posts()) })
	if dead.pings.Load() == 0 {
		t.Error("the socket was never pinged, so a dead server is undetectable")
	}
}

// AND A HEALTHY SOCKET IS LEFT ALONE. The heartbeat exists to tell GONE from
// QUIET, so a seat on an idle channel must not be reconnected for being idle
// — each reconnect costs a backfill and its duplicates.
func TestAQuietButHealthySocketIsNotReconnected(t *testing.T) {
	rec := &recorder{}
	sock := newSocket()

	var dials atomic.Int64
	f, err := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher:    rec,
		Backoff:      fastBackoff,
		PingInterval: 2 * time.Millisecond,
		PongTimeout:  time.Second,
		Connect: func(context.Context, mattermost.Seat, *mattermost.Client) (mattermost.Socket, error) {
			dials.Add(1)
			return sock, nil
		},
	})
	if err != nil {
		t.Fatalf("NewFleet: %v", err)
	}
	s := newServer(t)
	if err := f.Add(t.Context(), seat, client(t, s)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	defer f.Stop()

	// Long enough for many ping intervals to elapse on a silent channel.
	time.Sleep(200 * time.Millisecond)
	if got := dials.Load(); got != 1 {
		t.Errorf("dialled %d times on a healthy idle socket, want 1: an idle "+
			"seat is being reconnected and backfilled for being quiet", got)
	}
	if sock.pings.Load() == 0 {
		t.Error("the heartbeat never ran")
	}
}
