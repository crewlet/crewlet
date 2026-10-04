package engine_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A NODE IN A MAINTENANCE MODE IS PRESENT AND RUNS NOTHING THAT WRITES.
//
// It keeps its presence lease, because the process IS running and three things
// read that during a window: the capacity operation's participant set, the
// estate's routing of a stateless node's reads, and the fleet views. The lease
// says what its broker is, which is the only record a broker member holding no
// data leaves for the participant set to find it by.
//
// It claims no seat however many sweeps run, and no surface is handed its
// writers — a seat's turns, the operator's MCP, the purge route and the chart's
// apply would each be a publish into a log whose usage a resize is being
// decided against. Its reads are still served.
//
// The normal-mode node beside it is built identically and does all of it,
// which is what makes each absence evidence rather than a fixture that could
// never have produced one.
func TestAMaintenanceModeNodeIsPresentAndRunsNothingThatWrites(t *testing.T) {
	t.Parallel()
	for _, mode := range statelog.MaintenanceModes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			e := newEngine(t, engine.Options{Mode: mode})
			if err := e.Start(t.Context()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			host := e.Node().Host()

			leases, err := e.Backends().Coord.ListLive(t.Context(), coord.ClassNode)
			if err != nil {
				t.Fatalf("list the live nodes: %v", err)
			}
			var present bool
			for _, lease := range leases {
				profile, ok := placement.FromLease(lease)
				if !ok || profile.ID != host.NodeID() {
					continue
				}
				present = true
				if profile.Broker != placement.BrokerMember {
					t.Errorf("the presence advertises broker %q, want the embedded "+
						"member this node is", profile.Broker)
				}
			}
			if !present {
				t.Fatalf("a %s node holds no presence lease (live: %d): a broker "+
					"member that holds no data would be invisible to the capacity "+
					"window's participant set", mode, len(leases))
			}
			if e.Tracker() == nil {
				t.Fatal("no tracker read side: the native runtime never ran, so the " +
					"writers' absence below would be evidence of nothing")
			}

			if mode.Publishes() {
				waitHeld(t, e, 2)
				if e.TrackerWriter() == nil || e.PagesStore() == nil {
					t.Fatal("a publishing node was handed no writer")
				}
				return
			}
			// EVERY SWEEP A WINDOW COULD RUN, driven here rather than
			// waited for: the first ran inside Start, and a gate that
			// opened on a later one would claim there.
			for range 3 {
				host.Sweep(t.Context())
			}
			if held := host.Held(); len(held) != 0 {
				t.Errorf("a %s node claimed %v: a seat's turns write the company's "+
					"records, which the mode exists to rule out", mode, held)
			}
			if e.TrackerWriter() != nil || e.PagesStore() != nil {
				t.Errorf("a %s node handed out a writer (tracker %v, pages %v): the "+
					"operator's MCP and the purge route would publish through it",
					mode, e.TrackerWriter() != nil, e.PagesStore() != nil)
			}
		})
	}
}

// waitHeld waits for the engine to hold at least n seats.
func waitHeld(t *testing.T, e *engine.Engine, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if len(e.Node().Host().Held()) >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("held %v after 30s, want %d seats", e.Node().Host().Held(), n)
}
