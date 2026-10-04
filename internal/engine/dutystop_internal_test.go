package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// A DUTY'S LOOP IS STOPPED BEFORE THE DUTY IS GIVEN BACK, so a node that stops
// never exits holding it.
//
// The loop claims the duty afresh on every turn, and a claim of a released
// lease simply succeeds. Stopped after the release, a turn in flight across it
// took the duty straight back under this incarnation, and the node exited
// holding the duty for the lease's whole TTL with nobody doing its work.
//
// DETERMINISTIC IN BOTH ORDERS. The loop here makes the duty's own claim from
// a turn that stays in flight: it polls the lease, claims it the moment it
// reads it given back, and looks for its stop only AFTER each read. A teardown
// that releases first is therefore always seen, however short the gap before
// the stop, and one that stops first always ends the turn with the duty still
// held, for the release to give back.
func TestADutyIsNotTakenBackAsTheNodeStops(t *testing.T) {
	t.Parallel()
	for duty, swap := range map[string]func(e *Engine, start func() *loop){
		objectCollectorDuty: func(e *Engine, start func() *loop) {
			o := e.objects
			o.collectorMu.Lock()
			defer o.collectorMu.Unlock()
			o.collector.stop()
			o.collector = start()
		},
	} {
		t.Run(duty, func(t *testing.T) {
			t.Parallel()
			e := newSandboxNode(t, parseCompany(t, companyWithoutSandboxDoc))
			leases := e.backends.Coord
			resource := coord.WorkerResource(duty)
			eventually(t, "the "+duty+" duty", func() bool {
				lease := held(t, leases, resource)
				return lease != nil && lease.Owner == e.incarnation
			})

			claim := e.workerDuty(duty, time.Minute)
			claimed := make(chan error, 1)
			swap(e, func() *loop {
				return startLoop(t.Context(), sleep, func(ctx context.Context) time.Duration {
					// THE READ AND THE CLAIM OUTLIVE THE STOP: they are
					// the turn already in flight, which a stop waits out
					// rather than aborts.
					inFlight := context.WithoutCancel(ctx)
					for {
						lease, err := leases.Get(inFlight, resource)
						if err == nil && lease == nil {
							_, err := claim(inFlight)
							select {
							case claimed <- err:
							default:
							}
							return time.Millisecond
						}
						if ctx.Err() != nil {
							return 0
						}
						time.Sleep(time.Millisecond)
					}
				})
			})

			e.Stop(context.Background())
			select {
			case err := <-claimed:
				if err != nil {
					t.Fatalf("the turn in flight could not ask for the duty: %v", err)
				}
			default:
			}
			if lease := held(t, leases, resource); lease != nil && lease.Owner == e.incarnation {
				t.Errorf("the %s duty is held by %s after it stopped: nobody does "+
					"its work until the lease lapses", duty, lease.Owner)
			}
		})
	}
}

// held is the lease on resource, nil while nobody holds it.
func held(t *testing.T, leases coord.Backend, resource string) *coord.Lease {
	t.Helper()
	lease, err := leases.Get(t.Context(), resource)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}
