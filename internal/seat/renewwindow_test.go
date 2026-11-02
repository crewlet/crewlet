package seat

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
)

// renewRoundTrip is how long the stand-in store takes to answer a renew: a
// few store round trips' worth, which is what a healthy node's renew costs.
const renewRoundTrip = 20 * time.Millisecond

// probedRenews is a lease store whose renews take renewRoundTrip to answer
// and which, at the last instant before each one is answered, asks the host
// whether it admits every seat it holds — the instant in a cycle where the
// proof the host admits on is oldest.
type probedRenews struct {
	coord.Backend
	host  atomic.Pointer[Host]
	start time.Time

	mu      sync.Mutex
	probes  int
	refused []string
}

func (p *probedRenews) Renew(ctx context.Context, resource, owner string, epoch int64,
	ttl time.Duration) (bool, error) {

	time.Sleep(renewRoundTrip)
	if h := p.host.Load(); h != nil {
		for _, handle := range h.Held() {
			_, admitted := h.MayStart(handle)
			p.mu.Lock()
			p.probes++
			if !admitted {
				p.refused = append(p.refused, fmt.Sprintf("%s at %v", handle, time.Since(p.start)))
			}
			p.mu.Unlock()
		}
	}
	return p.Backend.Renew(ctx, resource, owner, epoch, ttl)
}

// statusTaking is a status hook that takes d to answer, or gives up when the
// beat's budget for it runs out.
func statusTaking(d time.Duration) func(context.Context) (coord.NodeStatus, error) {
	return func(ctx context.Context) (coord.NodeStatus, error) {
		select {
		case <-time.After(d):
			return coord.NodeStatus{}, nil
		case <-ctx.Done():
			return coord.NodeStatus{}, ctx.Err()
		}
	}
}

// A HEALTHY NODE ADMITS ITS SEAT THROUGH EVERY RENEW.
//
// The loop itself, on a synctest bubble's clock, against a store whose renews
// take [renewRoundTrip] and which asks [Host.MayStart] about every held seat at
// the last instant before each renew is answered — the oldest the proof it
// admits on ever gets on a node renewing on time. Five heartbeats per case.
//
// TWO FAILURES THE CASES ARE CHOSEN FOR, both measured here before the fix:
//
//   - The pass ran every heartbeat wherever the beat divided it, so a renew
//     was sent at the very instant the proof it replaced stopped admitting:
//     at 15 s, 30 s, 45 s and 90 s every renew was refused for its own round
//     trip, five in five. 1 s, 3 s, 10 s and 20 s, where the rounding left
//     slack, passed then and are kept as the controls.
//   - The pass ran every so many RECEIVED beats, and a ticker drops the beats
//     that come due while a pass is running, so a pass longer than two beats
//     delayed the next. The status hook alone may take a fifth of the
//     heartbeat: at 45 s a 2.5 s hook ran the renews 16 s apart, at 90 s a
//     5 s one 34 s apart against a 30 s window.
//
// Two seats, so the probe also covers a seat renewed after another in the
// same pass, whose send instant moves with the one before it.
func TestAHealthyNodeAdmitsItsSeatThroughEveryRenew(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ttl    time.Duration
		status time.Duration
	}{
		{ttl: SeatLeaseTTL},
		{ttl: 15 * time.Second},
		{ttl: 30 * time.Second},
		{ttl: 90 * time.Second},
		{ttl: time.Second},
		{ttl: 3 * time.Second},
		{ttl: 10 * time.Second},
		{ttl: 20 * time.Second},
		{ttl: SeatLeaseTTL, status: 2500 * time.Millisecond},
		{ttl: 90 * time.Second, status: 5 * time.Second},
	} {
		t.Run(fmt.Sprintf("ttl=%v,status=%v", tc.ttl, tc.status), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				store := &probedRenews{Backend: memory.New(), start: time.Now()}
				h, err := New(Config{
					Backend: store, Owner: "node-a:1", NodeID: "node-a",
					Seats: seatsNamed("ceo", "eng"), TTL: tc.ttl, Status: statusTaking(tc.status),
				})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				store.host.Store(h)
				h.Start(t.Context())
				time.Sleep(5 * h.heartbeat)
				h.Stop(context.Background())

				store.mu.Lock()
				defer store.mu.Unlock()
				if held := len(h.Held()); store.probes < 2*4 || held != 0 {
					t.Fatalf("the premise: %d seat admissions were probed in five "+
						"heartbeats, with %d seat(s) still held after the stop", store.probes, held)
				}
				if len(store.refused) > 0 {
					t.Errorf("a healthy node, renewing on time against a store "+
						"answering in %v, refused its own seat %d time(s) in %d probes: %v",
						renewRoundTrip, len(store.refused), store.probes, store.refused)
				}
			})
		})
	}
}
