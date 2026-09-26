package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE TRIM AND THE GATE COUNT ONLY THE NODES THAT HOLD DATA. A node without it
// applies no log and publishes no position, so a trim that counted it at zero
// would never advance again — and a presence row with no roles at all is an
// older build's, which held data, so it still counts.
func TestTheTrimAndTheGateCountOnlyDataNodes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := coordmem.New()
	for id, meta := range map[string]map[string]any{
		"data-a":  {"roles": []string{"data", "seats"}},
		"agent-1": {"roles": []string{"seats"}},
		"older":   nil,
	} {
		if _, err := backend.TryAcquire(ctx, coord.NodeResource(id), coord.AcquireOptions{
			Owner: id + ":1", TTL: time.Minute, Meta: meta,
		}); err != nil {
			t.Fatal(err)
		}
	}
	presences, err := livePresences(ctx, backend)
	if err != nil {
		t.Fatalf("livePresences: %v", err)
	}
	var got []string
	for _, p := range presences {
		got = append(got, p.NodeID)
	}
	slices.Sort(got)
	if want := []string{"data-a", "older"}; !slices.Equal(got, want) {
		t.Fatalf("counted %v, want %v", got, want)
	}
}

// A DATA NODE IN A MAINTENANCE MODE TAKES NO WRITE FOR ANYBODY. The mode is
// the evidence a capacity operation is established from — no publisher on
// this node — and a write it took on a stateless node's behalf would be the
// publish the mode rules out. Its reads are still served.
func TestAMaintenanceModeDataNodeServesNoWriter(t *testing.T) {
	t.Parallel()
	for mode, wantWriters := range map[statelog.MaintenanceMode]bool{
		statelog.ModeNormal:      true,
		statelog.ModeMaintenance: false,
		statelog.ModeSeal:        false,
	} {
		e := &Engine{mode: mode, backends: &Backends{}}
		e.native.Store(&native{
			writer: &tracker.Writer{}, pages: &pages.Store{},
			trackerReader: &tracker.Reader{}, pageReader: &pages.Reader{},
		})
		b, ok := e.estateBackend()
		if !ok {
			t.Fatalf("%s: no backend", mode)
		}
		if got := b.Writer != nil && b.PageWriter != nil; got != wantWriters {
			t.Errorf("%s: writers offered = %v, want %v", mode, got, wantWriters)
		}
		if b.Tracker == nil || b.Pages == nil || b.Events == nil {
			t.Errorf("%s: a read half or the event log is missing", mode)
		}
	}
}
