package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CONTAINER DEEP-WORK RUN GAINS NOTHING A DIRECT ONE ON LOOPBACK LACKS.
//
// A container box handed the runtime's name for the engine host in place of a
// loopback bridge base is held to loopback as a direct box is, so its token is
// worth nothing elsewhere. A remote box (it cannot dial loopback), or any box
// on a non-loopback base, is never held. Every loopback spelling the rehosting
// moves (a `.localhost` name, an IPv6 address) is one the hold covers, or a
// container run on it is held to nothing. That the hold refuses other callers
// is the bridge's suite's (caller_test.go, host_test.go).
//
// Mutation: read the base as loopback only for `localhost` or a loopback
// address, and the `.localhost` base goes red for both local cells.
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
