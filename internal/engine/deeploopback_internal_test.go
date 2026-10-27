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
// not loopback, since then no box dials it there. That the hold refuses every
// other caller is the bridge's own suite's (fleet_test.go).
func TestAContainerDeepRunIsHeldToLoopbackAsADirectOneIs(t *testing.T) {
	t.Parallel()
	bridge := func(base string) *mcpbridge.Bridge {
		b, err := mcpbridge.New(mcpbridge.Options{Material: runtoken.OneKey("k", "test-key"), BaseURL: base})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	loopback, public := bridge("http://127.0.0.1:8000"), bridge("https://engine.example.com")
	for _, tc := range []struct {
		placement sandbox.Placement
		bridge    *mcpbridge.Bridge
		want      bool
	}{
		{sandbox.Direct, loopback, true},
		{sandbox.Container, loopback, true},
		{sandbox.E2B, loopback, false},
		{sandbox.Direct, public, false},
		{sandbox.Container, public, false},
	} {
		if got := deepHeldToLoopback(tc.placement, tc.bridge); got != tc.want {
			t.Errorf("%s with a %q base: held to loopback = %v, want %v",
				tc.placement, map[bool]string{true: "loopback", false: "public"}[tc.bridge == loopback], got, tc.want)
		}
	}
}
