package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/org"
)

// A REMOVED SEAT'S LAST MEMORY LANDS UNDER THE ID ITS HANDLE DERIVES.
//
// The revision that removes a seat is already installed when the node lets the
// seat go, so the release flush asks for the id of a seat the company no
// longer holds. Looked up in the roster that was nobody: the flush published
// to a subject with an empty seat token, the broker refused every row, and the
// seat's last cycle never reached the changelog a seat re-added under the same
// handle hydrates from.
//
// The control is the seat while the company still holds it, which both the
// derivation and the roster answer alike.
func TestARemovedSeatsMemoryKeepsTheIDItsHandleDerives(t *testing.T) {
	t.Parallel()
	held := audienceChart(t)
	want, ok := held.AgentIDFor(held.AgentSeatByHandle("swe"))
	if !ok {
		t.Fatal("the fixture's swe is not an agent seat")
	}

	if got := memoryAgentID(&Company{Org: held}, "swe"); got != want.String() {
		t.Fatalf("a held seat's memory is addressed by %q, want its agent id %s", got, want)
	}
	removed := &Company{Org: &org.Organization{Name: held.Name}}
	if got := memoryAgentID(removed, "swe"); got != want.String() {
		t.Errorf("a seat the revision removed flushes under %q, want the id its "+
			"handle derives (%s), which is where a seat re-added under it hydrates from",
			got, want)
	}
	if got := memoryAgentID(nil, "swe"); got != "" {
		t.Errorf("a node with no company addressed a seat's memory by %q, want no address", got)
	}
}
