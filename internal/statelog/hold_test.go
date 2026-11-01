package statelog_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
)

// heldStream is the one log every case here pins.
const heldStream = "CREWLET_TRACKER_LOG"

// stepClock is a clock a case moves by hand, read by the holder on every put.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// countedHolds is a register that counts what a pin does to it, refuses
// renewals while refuse is set, and — as a real store's request does — fails
// under a context that has ended, which the in-memory twin does not.
type countedHolds struct {
	coord.HoldRegister
	puts, releases atomic.Int64
	refuse         atomic.Bool
}

func (c *countedHolds) PutHold(ctx context.Context, h coord.TrimHold) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.refuse.Load() {
		return errors.New("the coordination store cannot be reached")
	}
	c.puts.Add(1)
	return c.HoldRegister.PutHold(ctx, h)
}

func (c *countedHolds) ReleaseHold(ctx context.Context, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.releases.Add(1)
	return c.HoldRegister.ReleaseHold(ctx, owner)
}

// pinAt takes a pin at seq on generation 1 of heldStream, renewed every beat
// against clock, and releases it when the case ends.
func pinAt(ctx context.Context, t *testing.T, register statelog.HoldWriter, clock *stepClock,
	beat time.Duration, seq uint64, logger *slog.Logger) *statelog.TailPin {

	t.Helper()
	pin, err := statelog.HoldTail(ctx, statelog.TailHold{
		Register: register, Owner: coord.HoldOwner("node-join", "join"),
		Streams: map[string]coord.Position{
			heldStream: {Stream: heldStream, Generation: 1, Seq: seq},
		},
		Reason: "a case is pinning the tail",
		Every:  beat, Now: clock.Now, Logger: logger,
	})
	if err != nil {
		t.Fatalf("HoldTail: %v", err)
	}
	t.Cleanup(pin.Release)
	return pin
}

// renewedAt waits until the register holds the pin stamped at least at, which
// is a renewal that happened after the clock reached it, and returns what the
// trim reads of the register then.
func renewedAt(t *testing.T, register coord.HoldRegister, at time.Time) []statelog.Hold {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		holds, err := register.Holds(t.Context())
		if err != nil {
			t.Fatalf("read the holds: %v", err)
		}
		for _, h := range holds {
			if pos, pins := h.Streams[heldStream]; pins && !h.At.Before(at) {
				return []statelog.Hold{{
					Owner: h.Owner, Generation: pos.Generation, Seq: pos.Seq, At: h.At,
				}}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no renewal stamped at %s reached the register in ten seconds — "+
		"a pin nothing renews is read by the trim as a crashed holder once it "+
		"is %s old", at, statelog.TrimHoldStale)
	return nil
}

// A PIN ON THE REPLAY TAIL RENEWS ITSELF UNTIL IT IS RELEASED, AND NOT AFTER.
//
// The trim ignores a hold older than [statelog.TrimHoldStale], which is what
// stops a crashed holder pinning the log for ever — and reads a pin nobody
// renews exactly as a crashed holder. A join's pin was put once and never
// renewed, so a minute into any transfer the trim could pass the very tail the
// transfer was fetching. Here the holder's clock runs past the stale bound and
// the register must hold a renewal stamped after it; what the trim makes of
// such a pin is [TestTrimRacesATransfer]'s second arm. And once released it is
// gone and stays gone: a renewal still running after the release would put it
// back for a whole stale window.
func TestATailPinRenewsItselfUntilReleased(t *testing.T) {
	t.Parallel()
	clock := &stepClock{now: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	register := &countedHolds{HoldRegister: memory.NewFleet()}
	pin := pinAt(t.Context(), t, register, clock, time.Millisecond, 4_000, nil)

	held := renewedAt(t, register, clock.advance(statelog.TrimHoldStale+time.Second))
	if held[0].Seq != 4_000 || held[0].Generation != 1 {
		t.Fatalf("the renewed pin names %+v, want the position it was taken at", held[0])
	}

	pin.Release()
	puts := register.puts.Load()
	time.Sleep(20 * time.Millisecond)
	if holds, err := register.Holds(t.Context()); err != nil || len(holds) != 0 {
		t.Fatalf("%d hold(s) survive the release (%v)", len(holds), err)
	}
	if after := register.puts.Load(); after != puts {
		t.Fatalf("the pin was put %d more time(s) after its release — a renewal "+
			"that outlives the release pins the log until the stale bound", after-puts)
	}
}

// A RELEASE IS WRITTEN ONCE, AND OUTLIVES THE CONTEXT THE PIN WAS TAKEN UNDER.
//
// The failure a release undoes is often the cancellation itself, and a release
// under that dead context would leave the pin standing for its whole stale
// window. And a pin may be released from more than one place — a join's is
// released by the publish that counts it, or by the stop — so only the first
// call may do anything.
func TestATailPinIsReleasedOnceWhateverItsContextSays(t *testing.T) {
	t.Parallel()
	clock := &stepClock{now: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	register := &countedHolds{HoldRegister: memory.NewFleet()}
	ctx, cancel := context.WithCancel(t.Context())
	pin := pinAt(ctx, t, register, clock, time.Millisecond, 4_000, nil)
	cancel()

	pin.Release()
	pin.Release()
	if n := register.releases.Load(); n != 1 {
		t.Fatalf("the pin was released %d time(s), want once", n)
	}
	if holds, err := register.Holds(t.Context()); err != nil || len(holds) != 0 {
		t.Fatalf("%d hold(s) survive a release made after the pin's own context "+
			"ended (%v) — the release inherited the cancellation it was undoing",
			len(holds), err)
	}
}

// A RENEWAL THE REGISTER REFUSES IS SAID AND CARRIED; THE FIRST PUT IS NOT.
//
// A missed beat is survivable — the stale bound allows three — so the holder
// says so and keeps beating, and its next renewal lands. A holder that cannot
// pin the log at all holds nothing, and is told.
func TestATailPinSaysAMissedRenewalAndKeepsRenewing(t *testing.T) {
	t.Parallel()
	clock := &stepClock{now: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	register := &countedHolds{HoldRegister: memory.NewFleet()}
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	pinAt(t.Context(), t, register, clock, time.Millisecond, 4_000, logger)

	register.refuse.Store(true)
	deadline := time.Now().Add(10 * time.Second)
	for len(logRecords(t, logs.Bytes(), "statelog_hold_not_renewed")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no statelog_hold_not_renewed was written while the register " +
				"refused every renewal — the one line that says a pin is going stale")
		}
		time.Sleep(time.Millisecond)
	}
	register.refuse.Store(false)
	renewedAt(t, register, clock.advance(time.Second))

	register.refuse.Store(true)
	if _, err := statelog.HoldTail(t.Context(), statelog.TailHold{
		Register: register, Owner: coord.HoldOwner("node-other", "join"),
		Streams: map[string]coord.Position{heldStream: {Stream: heldStream, Generation: 1, Seq: 1}},
		Every:   statelog.TrimHoldHeartbeat,
	}); err == nil {
		t.Fatal("a pin the register refused outright was reported as held")
	}
}

// A PIN THAT WOULD NEVER RENEW IS REFUSED.
func TestATailPinThatWouldNeverRenewIsRefused(t *testing.T) {
	t.Parallel()
	for _, every := range []time.Duration{0, -time.Second} {
		if _, err := statelog.HoldTail(t.Context(), statelog.TailHold{
			Register: memory.NewFleet(), Owner: coord.HoldOwner("node-join", "join"),
			Streams: map[string]coord.Position{heldStream: {Stream: heldStream, Generation: 1, Seq: 1}},
			Every:   every,
		}); err == nil {
			t.Fatalf("a pin renewed every %s was taken", every)
		}
	}
	if statelog.TrimHoldHeartbeat*3 >= statelog.TrimHoldStale {
		t.Fatalf("a holder renewing every %s misses three beats inside the %s "+
			"stale bound only if four beats fit in it", statelog.TrimHoldHeartbeat,
			statelog.TrimHoldStale)
	}
}
