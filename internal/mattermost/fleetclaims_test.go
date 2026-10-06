package mattermost_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/mattermost"
)

// postAt is the instant every fixture post is written at: the create_at
// [frame] stamps, on the server's millisecond clock.
var postAt = time.UnixMilli(1718003000000).UTC()

// replayServer is a Mattermost stand-in whose clock reads serverNow and whose
// channel C1 holds the posts `posts` answers with at the moment of the read —
// those created AFTER the read's `since`, as the real endpoint answers, so a
// cursor that moved past a post is a post the replay never sees.
func replayServer(t *testing.T, serverNow time.Time, posts func() []map[string]any) *server {
	t.Helper()
	s := newServer(t)
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Date", serverNow.Format(http.TimeFormat))
		// MOST SPECIFIC FIRST, as in TestAReconnectReplaysTheGapInOrder.
		switch {
		case strings.Contains(r.URL.Path, "/posts"):
			since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
			list := posts()
			order := make([]string, 0, len(list))
			byID := map[string]any{}
			for _, p := range list {
				if at, _ := p["create_at"].(float64); int64(at) <= since {
					continue
				}
				id, _ := p["id"].(string)
				order = append([]string{id}, order...)
				byID[id] = p
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"order": order, "posts": byID})
		case strings.HasSuffix(r.URL.Path, "/channels"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "C1", "name": "eng", "type": "O"}})
		case strings.HasSuffix(r.URL.Path, "/teams"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "t1", "name": "eng"}})
		default:
			_, _ = w.Write([]byte(`{"id":"bot-1","username":"agent-swe"}`))
		}
		return true
	})
	return s
}

// storedPost is a post as the REST read returns it.
func storedPost(id string, at time.Time) map[string]any {
	return map[string]any{
		"id": id, "channel_id": "C1", "user_id": "u-ana",
		"message": "said " + id, "create_at": float64(at.UnixMilli()),
	}
}

// dialer hands out one socket per connect, the last one for every connect
// after the list runs out.
func dialer(sockets ...*fakeSocket) (mattermost.Connector, *atomic.Int32) {
	var dials atomic.Int32
	return func(context.Context, mattermost.Seat, *mattermost.Client) (mattermost.Socket, error) {
		n := int(dials.Add(1)) - 1
		return sockets[min(n, len(sockets)-1)], nil
	}, &dials
}

// drained waits until a socket has handed out every frame it was scripted
// with, then a moment longer for the last one to be delivered.
func drained(t *testing.T, sockets ...*fakeSocket) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		empty := true
		for _, s := range sockets {
			if len(s.frames) > 0 {
				empty = false
			}
		}
		if empty {
			time.Sleep(50 * time.Millisecond)
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("a socket never handed out its frames")
}

// TWO NODES READING ONE SEAT'S SOCKET DELIVER EACH POST ONCE.
//
// Every node opens every seat's socket, so every post arrives once per node.
// Deduplicated only within a process, a two-node fleet published each post
// twice and woke its seat twice, under two random wake ids nothing could
// pair. The fleet-wide claim is what makes it once — and the delivery is
// recorded once with it, so the integration count is one post to one seat.
//
// Mutation: deliver without claiming, and both nodes publish both posts.
func TestTwoNodesDeliverEachPostOnce(t *testing.T) {
	claims := coordmemory.NewFleet()
	s := newServer(t)
	var recs []*recorder
	var sockets []*fakeSocket
	for range 2 {
		rec := &recorder{}
		sock := newSocket(frame("p1", "hello", nil), frame("p2", "again", nil))
		connect, _ := dialer(sock)
		f, err := mattermost.NewFleet(mattermost.FleetOptions{
			Publisher: rec, Claims: claims, Backoff: fastBackoff, Connect: connect,
		})
		if err != nil {
			t.Fatalf("NewFleet: %v", err)
		}
		if err := f.Add(t.Context(), seat, client(t, s)); err != nil {
			t.Fatalf("Add: %v", err)
		}
		defer f.Stop()
		recs, sockets = append(recs, rec), append(sockets, sock)
	}
	drained(t, sockets...)

	var ids []string
	var records int
	for _, rec := range recs {
		ids = append(ids, rec.ids()...)
		for _, d := range rec.records() {
			records++
			if d.Recipient != "swe" || d.Route != mattermost.Backend || d.Label != "socket:posted" {
				t.Errorf("record = %+v, want the seat, the socket route and the socket label", d)
			}
			var arrived map[string]any
			if err := json.Unmarshal(d.Body, &arrived); err != nil {
				t.Fatalf("the record's body is not the post as it arrived: %v", err)
			}
			if _, annotated := arrived["bot_user_id"]; annotated {
				t.Error("the record's body carries the engine's own annotation, not what arrived")
			}
		}
	}
	slices.Sort(ids)
	if got := strings.Join(ids, ","); got != "p1,p2" {
		t.Fatalf("two nodes published %q, want each post exactly once", got)
	}
	if records != 2 {
		t.Fatalf("%d deliveries recorded, want 2 — one per post presented to the seat", records)
	}
}

// A POST A PEER ALREADY CLAIMED IS DROPPED, including one a reconnect replays.
//
// The replay is where a duplicate comes from: a node whose socket dropped
// re-reads the gap, and a post a peer delivered meanwhile is in it.
func TestAPostAPeerClaimedIsNotReplayedAgain(t *testing.T) {
	claims := coordmemory.NewFleet()
	if won, err := claims.Claim(t.Context(), mattermost.ClaimKey("swe", "g1"),
		mattermost.ClaimTTL, time.Now()); err != nil || !won {
		t.Fatalf("the peer's claim: (%v, %v)", won, err)
	}
	s := replayServer(t, postAt.Add(-time.Minute), func() []map[string]any {
		return []map[string]any{storedPost("g1", postAt.Add(time.Second)),
			storedPost("g2", postAt.Add(2*time.Second))}
	})
	rec := &recorder{}
	first := newSocket(frame("p1", "before the drop", nil))
	connect, _ := dialer(first, newSocket())
	f, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: rec, Claims: claims, Backoff: fastBackoff, Connect: connect,
	})
	f.Add(t.Context(), seat, client(t, s))
	defer f.Stop()

	waitFor(t, 1, func() int { return len(rec.posts()) })
	first.Close()
	waitFor(t, 2, func() int { return len(rec.posts()) })
	time.Sleep(50 * time.Millisecond)
	if got := strings.Join(rec.ids(), ","); got != "p1,g2" {
		t.Fatalf("published %q, want p1 and g2 — g1 is the peer's to deliver", got)
	}
}

// failingClaims is a claim store that cannot be reached.
type failingClaims struct{ releases atomic.Int32 }

func (f *failingClaims) Claim(context.Context, string, time.Duration, time.Time) (bool, error) {
	return false, coord.ErrUnavailable
}

func (f *failingClaims) Release(context.Context, string) error {
	f.releases.Add(1)
	return coord.ErrUnavailable
}

// A CLAIM STORE THAT CANNOT ANSWER FAILS OPEN.
//
// A post suppressed because the store blinked is a message nobody answers,
// and nothing else would notice; a post two nodes both deliver is collapsed by
// the wake's derived id. So an error delivers.
//
// Mutation: treat a claim error as a lost claim, and nothing is published.
func TestAClaimStoreThatCannotAnswerFailsOpen(t *testing.T) {
	rec := &recorder{}
	connect, _ := dialer(newSocket(frame("p1", "hello", nil)))
	f, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: rec, Claims: &failingClaims{}, Backoff: fastBackoff, Connect: connect,
	})
	f.Add(t.Context(), seat, client(t, newServer(t)))
	defer f.Stop()

	waitFor(t, 1, func() int { return len(rec.posts()) })
	waitFor(t, 1, func() int { return len(rec.records()) })
}

// A POST THAT COULD NOT BE QUEUED IS READ AGAIN.
//
// The cursor used to move past a post BEFORE it was published, so a broker
// that blinked lost the post for good: "it will not be re-read", said the log
// line. Now nothing is kept — the claim is released, the id forgotten, the
// cursor held before it — and the seat reconnects and replays it.
//
// Mutation: keep the claim on a failed publish, and the replay is refused its
// own post; or advance the cursor first, and the replay starts past it.
func TestAPostThatCouldNotBeQueuedIsReadAgain(t *testing.T) {
	claims := coordmemory.NewFleet()
	var mu sync.Mutex
	stored := []map[string]any{}
	s := replayServer(t, postAt.Add(-time.Minute), func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(stored)
	})
	// The server holds p1 from the start, as a real one would: it was
	// posted, and only the engine's queue failed to take it.
	stored = append(stored, storedPost("p1", postAt))

	rec := &recorder{failures: 1}
	connect, dials := dialer(newSocket(frame("p1", "hello", nil)), newSocket())
	f, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: rec, Claims: claims, Backoff: fastBackoff, Connect: connect,
	})
	f.Add(t.Context(), seat, client(t, s))
	defer f.Stop()

	waitFor(t, 1, func() int { return len(rec.posts()) })
	if got := strings.Join(rec.ids(), ","); got != "p1" {
		t.Fatalf("published %q, want p1 once, read again after the failure", got)
	}
	if dials.Load() < 2 {
		t.Fatalf("%d connects, want a reconnect after the failed publish", dials.Load())
	}
	if body := rec.posts()[0]; body["replayed"] != true {
		t.Errorf("p1 arrived as %v, want it read back by the replay", body)
	}
	// And the delivery that finally happened holds the claim, so a peer
	// reading the post now drops it.
	if won, err := claims.Claim(t.Context(), mattermost.ClaimKey("swe", "p1"),
		mattermost.ClaimTTL, time.Now()); err != nil || won {
		t.Fatalf("a peer could claim the delivered post: (%v, %v)", won, err)
	}
}

// A REPLAY SIX MINUTES LATER IS STILL DEDUPLICATED.
//
// The claim must outlive the replay horizon: a peer that reconnects re-reads
// up to MaxBackfill behind, so a post can come round again well after the
// coordination store's five-minute webhook claim would have lapsed. Six
// minutes is past that and inside the window.
//
// Mutation: claim for coord.ClaimTTL instead of mattermost.ClaimTTL, and the
// peer delivers the post a second time.
func TestAReplaySixMinutesLaterIsStillDeduplicated(t *testing.T) {
	claims := coordmemory.NewFleet()
	t0 := time.Now()
	s := replayServer(t, postAt.Add(-time.Minute), func() []map[string]any {
		return []map[string]any{storedPost("p1", postAt)}
	})

	// Node A hears p1 live, at t0.
	recA := &recorder{}
	connectA, _ := dialer(newSocket(frame("p1", "hello", nil)))
	a, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: recA, Claims: claims, Backoff: fastBackoff, Connect: connectA,
		Now: func() time.Time { return t0 },
	})
	a.Add(t.Context(), seat, client(t, s))
	defer a.Stop()
	waitFor(t, 1, func() int { return len(recA.posts()) })

	// Node B's socket dropped; six minutes on, it reconnects and replays
	// the gap, which holds p1.
	recB := &recorder{}
	firstB := newSocket(frame("x0", "before the drop", func(body map[string]any) {
		body["post"].(map[string]any)["create_at"] = float64(postAt.Add(-2 * time.Second).UnixMilli())
	}))
	connectB, _ := dialer(firstB, newSocket())
	b, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: recB, Claims: claims, Backoff: fastBackoff, Connect: connectB,
		Now: func() time.Time { return t0.Add(6 * time.Minute) },
	})
	b.Add(t.Context(), seat, client(t, s))
	defer b.Stop()
	waitFor(t, 1, func() int { return len(recB.posts()) })
	firstB.Close()
	time.Sleep(200 * time.Millisecond)

	if got := strings.Join(recB.ids(), ","); got != "x0" {
		t.Fatalf("node B published %q six minutes on, want only x0 — p1 is node A's", got)
	}
}

// A POST WHOSE CLAIM FAILED OPEN IS STILL READ AGAIN when its publish fails —
// and this node gives back no claim it never took, since releasing a key a
// peer may hold would hand that peer's post to a third node.
func TestAnUnclaimedPostThatCouldNotBeQueuedIsReadAgain(t *testing.T) {
	claims := &failingClaims{}
	s := replayServer(t, postAt.Add(-time.Minute), func() []map[string]any {
		return []map[string]any{storedPost("p1", postAt)}
	})
	rec := &recorder{failures: 1}
	connect, _ := dialer(newSocket(frame("p1", "hello", nil)), newSocket())
	f, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: rec, Claims: claims, Backoff: fastBackoff, Connect: connect,
	})
	f.Add(t.Context(), seat, client(t, s))
	defer f.Stop()

	waitFor(t, 1, func() int { return len(rec.posts()) })
	if n := claims.releases.Load(); n != 0 {
		t.Errorf("released %d claims this node never held", n)
	}
}

// pickyPublisher fails the first publish of one post, and passes everything
// else to a recorder.
type pickyPublisher struct {
	*recorder
	post   string
	failed atomic.Bool
}

func (p *pickyPublisher) Publish(ctx context.Context, topic string, ev *events.Event) error {
	if w, ok := events.DataAs[*types.RawWebhook](ev); ok {
		if post, _ := w.Body["post"].(map[string]any); str(post, "id") == p.post &&
			p.failed.CompareAndSwap(false, true) {
			return errors.New("the broker blinked")
		}
	}
	return p.recorder.Publish(ctx, topic, ev)
}

// A REPLAY THAT STOPS PART WAY OWES EVERYTHING IT HAD NOT YET QUEUED.
//
// Channels are read one after another and each post moves the cursor, so a
// replay that stops at a post it could not queue may already have moved the
// cursor past posts in channels it never reached — older ones included. Held
// at the failed post alone, the next replay started past those and they were
// lost; held where the replay BEGAN, it reads them.
//
// Mutation: hold only at the failed post, and c — older than it, in a channel
// the first replay never reached — is never published.
func TestAReplayThatStopsPartWayOwesWhatItHadNotQueued(t *testing.T) {
	channels := map[string][]map[string]any{
		"C1": {storedPost("a", postAt.Add(5*time.Second))},
		"C2": {storedPost("b", postAt.Add(4*time.Second))},
		"C3": {storedPost("c", postAt.Add(2*time.Second))},
	}
	s := newServer(t)
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Date", postAt.Add(-time.Minute).Format(http.TimeFormat))
		switch {
		case strings.Contains(r.URL.Path, "/posts"):
			since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
			order, byID := []string{}, map[string]any{}
			for id, list := range channels {
				if !strings.Contains(r.URL.Path, "/channels/"+id+"/") {
					continue
				}
				for _, p := range list {
					if at, _ := p["create_at"].(float64); int64(at) > since {
						order = append(order, p["id"].(string))
						byID[p["id"].(string)] = p
					}
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"order": order, "posts": byID})
		case strings.HasSuffix(r.URL.Path, "/channels"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "C1", "name": "one", "type": "O"},
				{"id": "C2", "name": "two", "type": "O"},
				{"id": "C3", "name": "three", "type": "O"},
			})
		case strings.HasSuffix(r.URL.Path, "/teams"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "t1", "name": "eng"}})
		default:
			_, _ = w.Write([]byte(`{"id":"bot-1","username":"agent-swe"}`))
		}
		return true
	})

	pub := &pickyPublisher{recorder: &recorder{}, post: "b"}
	first := newSocket(frame("x0", "before the drop", nil))
	connect, _ := dialer(first, newSocket())
	f, _ := mattermost.NewFleet(mattermost.FleetOptions{
		Publisher: pub, Claims: coordmemory.NewFleet(), Backoff: fastBackoff, Connect: connect,
	})
	f.Add(t.Context(), seat, client(t, s))
	defer f.Stop()

	waitFor(t, 1, func() int { return len(pub.posts()) })
	first.Close()
	waitFor(t, 4, func() int { return len(pub.posts()) })
	ids := pub.ids()
	slices.Sort(ids)
	if got := strings.Join(ids, ","); got != "a,b,c,x0" {
		t.Fatalf("published %q, want every post once — c included", got)
	}
}
