package seat

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/clocktest"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
)

// at sets the fleet's clock, the host's and the store's, to an instant whose
// readings a case chose.
func (f *fleet) at(now time.Time) {
	f.clock.mu.Lock()
	defer f.clock.mu.Unlock()
	f.clock.now = now
}

// A HOST THAT SLEPT PAST ITS TTL PROVES NO LEASE ON WAKING, and one whose wall
// clock was stepped back proves none past its TTL either.
//
// A renew's age was measured on the monotonic clock alone, which stops while
// the host sleeps while the store's TTL runs on. So a node that slept past its
// TTL woke still provably holding every seat and its presence, and went on
// admitting turns as the seat until its next heartbeat failed to renew, while
// the store had expired the lease and a peer could hold it. The wall clock
// alone would have the opposite fault: a step back makes a renew look younger
// than it is. The age is the larger of the two readings, which neither fault
// can hold back.
//
// Mutation: measure the age on the monotonic clock alone, and the host that
// slept past its TTL still proves its seat and admits a turn on it; on the
// wall clock alone, and the step back proves it a TTL past its renew.
func TestAHostThatSleptPastItsTTLProvesNoLeaseOnWaking(t *testing.T) {
	t.Parallel()
	const ttl, beat = 30 * time.Second, 5 * time.Second
	for name, tc := range map[string]struct {
		monotonic, wall  time.Duration
		provable, admits bool
	}{
		"a sleep past the TTL":                         {0, ttl + time.Second, false, false},
		"a wall clock stepped back as the TTL ran out": {ttl + time.Second, -time.Hour, false, false},
		"a sleep inside one heartbeat":                 {0, 2 * time.Second, true, true},
		"a sleep past the heartbeat, inside the TTL":   {0, 2 * beat, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t)
			f.at(time.Now())
			h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), TTL: ttl,
				HeartbeatInterval: beat})
			h.renewNodePresence(f.ctx)
			h.Sweep(f.ctx)
			wantHeld(t, h, "ceo")
			renewed := f.clock.Now()
			wantAdmits(t, h, "ceo", true)

			f.at(clocktest.Parted(t, renewed, tc.monotonic, tc.wall))
			if got := h.Provable("ceo"); got != tc.provable {
				t.Errorf("the seat's lease is provable = %v, want %v", got, tc.provable)
			}
			if got := h.HoldsPresence(); got != tc.provable {
				t.Errorf("the node holds its presence = %v, want %v", got, tc.provable)
			}
			wantAdmits(t, h, "ceo", tc.admits)
		})
	}
}

// A STORE THAT CANNOT BE REACHED ACROSS A SLEEP PAST THE TTL DROPS THE SEAT. The
// heartbeat keeps a seat through an unreachable store only until its last
// renew is a TTL old, since past that the lease has lapsed whatever this node
// can see; measured on the monotonic clock alone, a sleep never aged it, and
// the node kept a seat the store had expired on the strength of a renew made
// before it slept.
//
// Mutation: measure the age on the monotonic clock alone, and the seat is kept.
func TestAnUnreachableStoreAcrossASleepPastTheTTLDropsTheSeat(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.at(time.Now())
	faulty := coordtest.NewFaulty(f.store)
	h := f.newHost("node-a", Config{Backend: faulty, Seats: seatsNamed("ceo"),
		TTL: 30 * time.Second})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantHeld(t, h, "ceo")

	faulty.Break(nil)
	f.at(clocktest.AfterSleep(t, f.clock.Now(), 31*time.Second))
	h.Heartbeat(f.ctx)
	wantHeld(t, h)
}
