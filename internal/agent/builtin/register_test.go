package builtin_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE DELIVERY SURFACE AND THE FEED SOURCE ARE ONE VOCABULARY, and nothing
// but this holds them together.
//
// The gate compares a delivery's declared surface against the
// NotificationSource an inbound wake carried. The native feeds publish under
// [tracker.Source] and [pages.Source]; registering their writes under any
// other spelling makes every native obligation unreachable, and the check then
// falls back to "any delivery counts" — silently, with a passing suite, which
// is exactly what `tracker`/`pages` did in place of `work`/`page`.
func TestTheNativeWritesDeliverToTheirOwnFeedSource(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	kb := newFakeKB()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Work:  builtin.WorkDeps{Reader: trk, Writer: trk.as, Merges: trk.merges, Moves: trk.moves},
		Pages: builtin.PageDeps{Reader: kb, Writer: kb},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	deliveries := reg.Deliveries()
	for _, c := range []struct {
		names []string
		want  string
	}{
		{builtin.WorkWrites(), tracker.Source},
		{builtin.PageWrites(), pages.Source},
	} {
		for _, name := range c.names {
			if got := deliveries[name]; got != c.want {
				t.Errorf("%s delivers to %q, want the feed's own source %q — a "+
					"second spelling makes the obligation unreachable and the gate "+
					"falls back to any delivery at all", name, got, c.want)
			}
		}
	}
}
