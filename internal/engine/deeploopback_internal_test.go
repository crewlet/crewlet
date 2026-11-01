package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CONTAINER DEEP-WORK RUN GAINS NOTHING A DIRECT ONE ON LOOPBACK LACKS.
//
// A container box is handed the runtime's name for the engine host in place
// of a loopback bridge base, and that must not loosen who may call its
// session: it is held to loopback exactly as a direct box is, so its token is
// worth nothing from any address but this host's loopback. A remote box is
// never held (it cannot dial loopback), and nothing is held where the base is
// not loopback, since then no box dials it there. Every spelling of loopback
// the rehosting moves is one the hold covers — a name under `localhost` and
// an IPv6 address included — or a container run on it would be handed the
// runtime's name and held to nothing. That the hold refuses every other
// caller is the bridge's own suite's (caller_test.go, host_test.go).
//
// Mutation: read the base as loopback only for `localhost` or a loopback
// address, as the bridge once did, and the `.localhost` base goes red for
// both local cells.
func TestAContainerDeepRunIsHeldToLoopbackAsADirectOneIs(t *testing.T) {
	t.Parallel()
	bridge := func(base string) *mcpbridge.Bridge {
		b, err := mcpbridge.New(mcpbridge.Options{Material: runtoken.OneKey("k", "test-key"), BaseURL: base})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, base := range []struct {
		url      string
		loopback bool
	}{
		{"http://127.0.0.1:8000", true},
		{"http://engine.localhost:8000", true},
		{"http://[::1]:8000", true},
		{"https://engine.example.com", false},
	} {
		b := bridge(base.url)
		for _, tc := range []struct {
			placement sandbox.Placement
			want      bool
		}{
			{sandbox.Direct, base.loopback},
			{sandbox.Container, base.loopback},
			{sandbox.E2B, false},
		} {
			if got := deepHeldToLoopback(tc.placement, b); got != tc.want {
				t.Errorf("%s with the base %s: held to loopback = %v, want %v",
					tc.placement, base.url, got, tc.want)
			}
		}
	}
}
