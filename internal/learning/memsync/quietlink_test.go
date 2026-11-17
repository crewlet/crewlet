package memsync

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
	seathost "github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/store"
)

// A HYDRATION ACROSS A METERED LINK, PLAYED ON A BUBBLE'S CLOCK.
//
// The cases here run the model changelog (copyfake_test.go) behind a link a
// case scripts: moving or still, and how many bytes it read, each read off
// closures the case writes inside its bubble, on the bubble's clock.

// fakeLink is a node's link as a case plays it.
type fakeLink struct {
	moving func() bool
	read   func() uint64
}

func (l fakeLink) Moving(time.Duration) bool { return l.moving() }

func (l fakeLink) Carried() (acked, read uint64) { return 0, l.read() }

// meteredSyncer is a syncer of the seat over db whose changelog is the model
// copies is played over, across a link at the shipped lease that link meters,
// and the model's client, for a case to script.
func meteredSyncer(t *testing.T, db *store.DB, copies *fakeCopies, link Link) (*Syncer, *fakeJS) {
	t.Helper()
	js := &fakeJS{log: copies.log, created: time.Unix(1_700_000_000, 0).UTC()}
	s := mustNew(db, js, func(string) string { return seat.AgentID },
		Config{AcrossLink: true, LeaseHeartbeat: seathost.HeartbeatInterval, Link: link})
	s.copies = copies
	return s, js
}

// A REPLAY HEARING NOTHING ON A MOVING LINK IS PLACED AGAIN PAST ONE TRANSPORT
// PULL, AND REFUSED AT ITS TIME CAP.
//
// Across a link at the shipped lease, the diary's copies say nothing at all —
// no delivery, no heartbeat, no answer to their consumer's info — while the
// link moves:
//
//   - on a link reading ten megabytes a second, past one message at the
//     transport's bound and a window at each stall, the replay is placed
//     again at each stall, four times, on copies that say nothing, and the
//     fifth hands the notes over: the hydration completes, a minute and more
//     after it last heard anything, since a placement for the link's
//     movement arms no behind clock;
//   - on a link reading a kilobyte a second, never past that, the replay is
//     neither placed again nor refused for silence: it is refused at the stall
//     of one pull at the transport's bound since it was placed, and not a
//     nanosecond before, naming the link's movement;
//   - on a still link, the replay is refused at its pull's stall, as it is
//     where no link is metered, naming the link's stillness;
//   - on a link that falls still a second and a half past the stall, the
//     replay is refused for silence at the first judgment after, two
//     seconds past the stall, judged every second while the link moved.
//
// Mutant: no byte cap — the fast link's replay is never placed again, and is
// refused at its time cap. Mutant: a placement for the link's movement arms
// the behind clock — the fast link's replay is refused a minute after its
// first. Mutant: no time cap — the creeping link's replay runs on past it.
// Mutant: silence judged by words alone — every link's replay is refused at
// its stall. Mutant: a held replay judged again a stall later — the link that
// falls still runs on to twice the stall.
func TestAReplayHearingNothingOnAMovingLinkIsPlacedAgainPastOneTransportPull(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		moving bool
		// stillFrom is when a moving link falls still, nil for never.
		stillFrom func(s *Syncer) time.Duration
		rate      float64
		scripts   []copyScript
		// carried is the notes the hydration carries, or -1 for a refusal.
		carried int
		// ends is when the hydration ends, read off the syncer.
		ends func(s *Syncer) time.Duration
		// says is what a refusal says.
		says []string
	}{
		{
			name: "past what one message and a window carry", moving: true, rate: 10 << 20,
			scripts: []copyScript{{mute: true}, {mute: true}, {mute: true}, {mute: true}, {}},
			carried: 5,
			ends:    func(s *Syncer) time.Duration { return 4 * s.stall(s.pullBound()) },
		},
		{
			name: "creeping under it", moving: true, rate: 1 << 10,
			scripts: []copyScript{{mute: true}},
			carried: -1,
			ends:    func(s *Syncer) time.Duration { return s.stall(s.pullBytes()) },
			says:    []string{"while the link kept moving", "no copy said anything to it"},
		},
		{
			name: "still", moving: false, rate: 0,
			scripts: []copyScript{{mute: true}},
			carried: -1,
			ends:    func(s *Syncer) time.Duration { return s.stall(s.pullBound()) },
			says:    []string{"the link carried nothing at the floor", "(0 rows carried)"},
		},
		{
			name: "falling still past its stall", moving: true, rate: 1 << 10,
			stillFrom: func(s *Syncer) time.Duration { return s.stall(s.pullBound()) + 1500*time.Millisecond },
			scripts:   []copyScript{{mute: true}},
			carried:   -1,
			ends:      func(s *Syncer) time.Duration { return s.stall(s.pullBound()) + 2*quietWait },
			says:      []string{"the link carried nothing at the floor"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := openStore(t)
			synctest.Test(t, func(t *testing.T) {
				log := newFakeChangelog()
				ids := notesOnto(t, log, "n", 5, 512)
				copies := newFakeCopies(log)
				copies.scripts["agent_diary"] = c.scripts
				start := time.Now()
				var s *Syncer
				link := fakeLink{
					moving: func() bool {
						return c.moving && (c.stillFrom == nil || time.Since(start) < c.stillFrom(s))
					},
					read: func() uint64 { return uint64(c.rate * time.Since(start).Seconds()) },
				}
				s, _ = meteredSyncer(t, db, copies, link)
				ends := c.ends(s)
				h := hydrating(t.Context(), s)
				time.Sleep(ends - time.Nanosecond)
				synctest.Wait()
				if h.returned() {
					t.Fatalf("the hydration returned (%d, %v) a nanosecond before %v", h.carried, h.err, ends)
				}
				if c.carried < 0 {
					time.Sleep(time.Nanosecond)
					synctest.Wait()
					if !h.returned() {
						t.Fatalf("the hydration was still running at %v, want it refused there", ends)
					}
					if h.err == nil || !says(h.err, c.says...) {
						t.Errorf("the hydration answered (%d, %v), want it refused saying %q", h.carried,
							h.err, c.says)
					}
					return
				}
				<-h.done
				if h.err != nil || h.carried != len(ids) {
					t.Fatalf("Hydrate = (%d, %v), want all %d notes from the copy placed last", h.carried,
						h.err, len(ids))
				}
				opened := copies.openedFor("agent_diary")
				if len(opened) != len(c.scripts) {
					t.Fatalf("the diary's replay was placed %d times, want %d", len(opened), len(c.scripts))
				}
				stall := s.stall(s.pullBound())
				for i := 1; i < len(opened); i++ {
					if gap := opened[i].at.Sub(opened[i-1].at); gap != stall {
						t.Errorf("placement %d came %v after the one before, want its pull's stall of %v",
							i, gap, stall)
					}
				}
			})
		})
	}
}

// A REQUEST OF A HYDRATION BEHIND A MOVING LINK IS WAITED FOR.
//
// Each request a hydration hedges — the changelog's identity read, a table's
// end barrier, a replay's consumer create — is answered here only a hundred
// seconds after it is asked, past its ceiling, as one held behind a message of
// megabytes on the link is. Across a link that moves meanwhile the answer is
// taken; across a still link the request is refused at its ceiling, as it was,
// naming how long it was asked for.
//
// Mutant: any of the three asked at its plain ceiling — refused across the
// moving link.
func TestARequestOfAHydrationBehindAMovingLinkIsWaitedFor(t *testing.T) {
	t.Parallel()
	const late = 100 * time.Second
	for _, c := range []struct {
		name string
		// late is the request held past its ceiling; ask makes it, and the
		// hydration or the request is what answers.
		late lateRequest
		ask  func(ctx context.Context, s *Syncer) error
		says string
	}{
		{"the changelog's identity", lateIdentity, hydrated, "read the memory changelog's identity"},
		{"a table's end barrier", lateEndBarrier, hydrated, "the end barrier of eng's agent_diary"},
		{"a replay's consumer", lateConsumer, func(ctx context.Context, s *Syncer) error {
			_, err := s.createConsumer(ctx, jetstream.ConsumerConfig{Name: "replay",
				FilterSubjects: []string{tableFilter(seat.AgentID, "agent_diary")}})
			if err != nil && jsprovision.Unanswered(ctx, err) {
				return fmt.Errorf("the broker made no consumer in %s: %w",
					s.heldFor(s.waits(0), s.createBudget()), err)
			}
			return err
		}, "the broker made no consumer"},
	} {
		for _, moving := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, the link moving %t", c.name, moving), func(t *testing.T) {
				t.Parallel()
				db := openStore(t)
				synctest.Test(t, func(t *testing.T) {
					log := newFakeChangelog()
					notesOnto(t, log, "n", 3, 512)
					copies := newFakeCopies(log)
					link := fakeLink{moving: func() bool { return moving }, read: func() uint64 { return 0 }}
					s, js := meteredSyncer(t, db, copies, link)
					scriptLate(c.late, time.Now().Add(late), js)
					err := c.ask(t.Context(), s)
					switch {
					case moving && err != nil:
						t.Fatalf("%s answered past its ceiling across a moving link refused: %v", c.name, err)
					case !moving && (err == nil || !strings.Contains(err.Error(), c.says) ||
						!strings.Contains(err.Error(), "of this node's link carrying nothing at the floor")):
						t.Fatalf("%s answered past its ceiling across a still link = %v, want it refused "+
							"at its ceiling, saying %q and how long it was asked for", c.name, err, c.says)
					}
				})
			})
		}
	}
}

// lateRequest names the request [scriptLate] holds.
type lateRequest string

const (
	lateIdentity   lateRequest = "identity"
	lateEndBarrier lateRequest = "end barrier"
	lateConsumer   lateRequest = "consumer"
)

// scriptLate scripts js to answer the request late names at the instant at,
// and not before: every one of it asked before then is answered then, as
// replies held behind the link's other traffic are carried once it has
// drained.
//
//fixedwait:bubble
func scriptLate(late lateRequest, at time.Time, js *fakeJS) {
	switch late {
	case lateIdentity:
		js.identityAt = at
	case lateEndBarrier:
		js.ackAfter = func(n int) time.Duration {
			// THE FIRST TABLE'S END BARRIER AND ITS RE-ASKS: every
			// publish but the hydration's own barrier, the first.
			if wait := time.Until(at); n > 1 && wait > 0 {
				return wait
			}
			return 0
		}
	case lateConsumer:
		js.replay = func(ctx context.Context, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {
			answered := time.NewTimer(time.Until(at))
			defer answered.Stop()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-answered.C:
			}
			return countedConsumer{info: &jetstream.ConsumerInfo{Name: cfg.Name}}, nil
		}
	}
}

// hydrated is the seat's hydration as a request's answer.
func hydrated(ctx context.Context, s *Syncer) error {
	_, err := s.Hydrate(ctx, seat.Handle)
	return err
}

// A LINK IS READ ONLY ACROSS ONE.
//
// A node whose connection reaches the changelog in its own process crosses no
// link, so a meter handed with it is refused by name, rather than read as a
// link its hydrations wait on.
//
// Mutant: take the meter whatever the topology — the member's syncer is made.
func TestALinkIsReadOnlyAcrossOne(t *testing.T) {
	t.Parallel()
	link := fakeLink{moving: func() bool { return false }, read: func() uint64 { return 0 }}
	s, err := New(openStore(t), &fakeJS{}, func(string) string { return seat.AgentID }, Config{Link: link})
	if err == nil || s != nil || !strings.Contains(err.Error(), "Config.Link") {
		t.Errorf("a member's syncer handed a link = (%v, %v), want it refused naming Config.Link", s, err)
	}
}
