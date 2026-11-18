package memsync

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
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
// copies is played over, across a link that link meters, at the link's
// window, and the model's client, for a case to script.
func meteredSyncer(t *testing.T, db *store.DB, copies *fakeCopies, link Link) (*Syncer, *fakeJS) {
	t.Helper()
	js := &fakeJS{log: copies.log, created: time.Unix(1_700_000_000, 0).UTC()}
	s := mustNew(db, js, func(string) string { return seat.AgentID },
		Config{AcrossLink: true, Link: link, Window: jsapi.NewLinkWindow()})
	s.copies = copies
	return s, js
}

// mutes is n placements whose copies say nothing at all.
func mutes(n int) []copyScript {
	scripts := make([]copyScript, n)
	for i := range scripts {
		scripts[i].mute = true
	}
	return scripts
}

// oneMute is a replay's one placement, on copies that say nothing at all.
func oneMute(*Syncer) []copyScript { return mutes(1) }

// mutedPastBehind is how many placements, one each pull's stall, a quiet
// replay on a moving link must be placed on copies that say nothing for its
// last to come more than [behindWait] after its first: the fewest that would
// refuse it were its placements to arm the behind clock, so a case that
// expects it to complete across them holds them to arming none, whatever the
// window makes the stall.
func mutedPastBehind(s *Syncer) int {
	return int(behindWait/s.stall(s.pullBound())) + 2
}

// A REPLAY HEARING NOTHING ON A MOVING LINK IS PLACED AGAIN PAST ONE TRANSPORT
// PULL, AND REFUSED AT ITS TIME CAP.
//
// The diary's copies say nothing while the link moves. Moving past one
// transport message and a window a stall, the replay is placed again each
// stall (arming no behind clock) past [behindWait] until a copy hands the notes
// over; creeping under that, it is refused at one transport pull's stall; still,
// at its pull's stall; falling still 1.5 s past the stall, for silence at the
// next judgment, 2 s past it. Each refusal names the link's movement.
//
// Mutants, each turning a row red: no byte cap; a link's placement arming the
// behind clock; no time cap; silence by words alone; a held replay rejudged.
func TestAReplayHearingNothingOnAMovingLinkIsPlacedAgainPastOneTransportPull(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		moving bool
		// stillFrom is when a moving link falls still, nil for never.
		stillFrom func(s *Syncer) time.Duration
		rate      float64
		// scripts is how each placement of the diary's replay answers.
		scripts func(s *Syncer) []copyScript
		// carried is the notes the hydration carries, or -1 for a refusal.
		carried int
		// ends is when the hydration ends, read off the syncer.
		ends func(s *Syncer) time.Duration
		// says is what a refusal says.
		says []string
	}{
		{
			name: "past what one message and a window carry", moving: true, rate: 10 << 20,
			scripts: func(s *Syncer) []copyScript {
				return append(mutes(mutedPastBehind(s)), copyScript{})
			},
			carried: 5,
			ends: func(s *Syncer) time.Duration {
				return time.Duration(mutedPastBehind(s)) * s.stall(s.pullBound())
			},
		},
		{
			name: "creeping under it", moving: true, rate: 1 << 10,
			scripts: oneMute,
			carried: -1,
			ends:    func(s *Syncer) time.Duration { return s.stall(s.pullBytes()) },
			says:    []string{"while the link kept moving", "no copy said anything to it"},
		},
		{
			name: "still", moving: false, rate: 0,
			scripts: oneMute,
			carried: -1,
			ends:    func(s *Syncer) time.Duration { return s.stall(s.pullBound()) },
			says:    []string{"the link carried nothing at the floor", "(0 rows carried)"},
		},
		{
			name: "falling still past its stall", moving: true, rate: 1 << 10,
			stillFrom: func(s *Syncer) time.Duration { return s.stall(s.pullBound()) + 1500*time.Millisecond },
			scripts:   oneMute,
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
				start := time.Now()
				var s *Syncer
				link := fakeLink{
					moving: func() bool {
						return c.moving && (c.stillFrom == nil || time.Since(start) < c.stillFrom(s))
					},
					read: func() uint64 { return uint64(c.rate * time.Since(start).Seconds()) },
				}
				s, _ = meteredSyncer(t, db, copies, link)
				scripts := c.scripts(s)
				copies.scripts["agent_diary"] = scripts
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
				if len(opened) != len(scripts) {
					t.Fatalf("the diary's replay was placed %d times, want %d", len(opened), len(scripts))
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
// taken; across a still link the request is refused at its ceiling, naming
// how long it was asked for.
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
// link, so a meter or a window handed with it is refused by name, rather than
// read as a link its hydrations wait on; and a node across one pulls inside
// its link's window, so one handed none is refused.
//
// Mutant: take the meter whatever the topology — the member's syncer is made.
func TestALinkIsReadOnlyAcrossOne(t *testing.T) {
	t.Parallel()
	link := fakeLink{moving: func() bool { return false }, read: func() uint64 { return 0 }}
	s, err := New(openStore(t), &fakeJS{}, func(string) string { return seat.AgentID }, Config{Link: link})
	if err == nil || s != nil || !strings.Contains(err.Error(), "Config.Link") {
		t.Errorf("a member's syncer handed a link = (%v, %v), want it refused naming Config.Link", s, err)
	}
	// AND ITS WINDOW IS THE LINK'S, both ways: a member handed one is
	// refused, and so is a node across a link handed none, which would
	// otherwise pull beside its link's other transfers.
	for _, c := range []struct {
		name string
		cfg  Config
	}{
		{"a member handed a window", Config{Window: jsapi.NewLinkWindow()}},
		{"a node across a link handed none", Config{AcrossLink: true}},
	} {
		s, err := New(openStore(t), &fakeJS{}, func(string) string { return seat.AgentID }, c.cfg)
		if err == nil || s != nil || !strings.Contains(err.Error(), "Config.Window") {
			t.Errorf("%s = (%v, %v), want it refused naming Config.Window", c.name, s, err)
		}
	}
}
