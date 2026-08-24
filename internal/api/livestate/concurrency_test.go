package livestate_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

func TestTheProjectionIsSafeToReadWhileItIsWritten(t *testing.T) {
	t.Parallel()
	// The Python this replaces took no lock and said so, on the grounds
	// that a single-threaded event loop made every mutation atomic. That
	// reasoning does not survive the port: here the stream feeds the
	// projection from its own goroutine while HTTP handlers and WebSocket
	// sends read it. Under -race this fails immediately without the lock;
	// without -race it fails as a torn read on a loaded box, which is
	// worse because it is rare.
	s := livestate.New()
	var wg sync.WaitGroup

	for w := range 4 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			role := fmt.Sprintf("Seat-%d", w)
			base := map[string]any{
				"role": role, "turn_id": "tn-1", "phase": "plan", "iteration": 0,
			}
			for i := range 50 {
				s.Apply(env("agent_phase_started", base, id(fmt.Sprint(w, i))))
				s.Apply(env("agent_turn_progress",
					with(base, map[string]any{"round_num": i, "response": "r"}),
					streamOnly, id(fmt.Sprint("p", w, i))))
				s.Apply(env("agent_turn_completed",
					with(base, map[string]any{"total_tokens": 1}), id(fmt.Sprint("t", w, i))))
				s.Apply(env("sandbox_run_started",
					map[string]any{"turn_id": fmt.Sprint("sb", w, i), "role": role}))
				s.Apply(env("budget_reported",
					meterReport("m-1", w*100+i, seatMeter(role, i, 100)), streamOnly))
			}
		}(w)
	}

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				s.RecentEvents(50)
				s.ActiveSandboxes()
				s.SpendRecords()
				s.Budget()
				s.AgentOverlay("Seat-0")
				s.RuntimeIDFor("Seat-1")
				s.MergeAgents([]map[string]any{{"role": "Seat-2"}, {"role": "Seat-3"}})
			}
		}()
	}
	wg.Wait()

	// Not an assertion about the values — concurrent writers make those
	// non-deterministic by design — only that the projection is still
	// coherent and answering.
	if len(s.RecentEvents(0)) == 0 {
		t.Error("the projection recorded nothing")
	}
}
