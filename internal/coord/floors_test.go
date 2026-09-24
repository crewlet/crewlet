package coord_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// A FLOOR MAY NOT LICENSE A PURGE PAST WHAT IT TELLS ITS READERS.
//
// The trim writes the row before the purge it licenses, and every write fence
// reads the row's Floor to decide what may be gone. A row whose TrimTo is
// above its Floor licenses removing records the fences are told are held, so
// a node the purge leaves below the log is cleared to publish at an
// expectation of zero over them. The writer computes the two together and the
// register refuses the combination, so neither half can drift alone.
func TestAFloorThatLicensesPastItselfIsRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		row     coord.TrimFloor
		refused bool
	}{
		"an advancing tick at its own floor": {
			row: coord.TrimFloor{Domain: "tracker", TrimTo: 900, Floor: 900}},
		"a blocked tick carrying an earlier floor": {
			row: coord.TrimFloor{Domain: "tracker", BlockedBy: "backup_floor", Floor: 900}},
		"a conclusion below the floor it carries": {
			row: coord.TrimFloor{Domain: "tracker", TrimTo: 300, Floor: 900}},
		"a licence past the floor": {
			row: coord.TrimFloor{Domain: "tracker", TrimTo: 901, Floor: 900}, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := tc.row.Validate(); (err != nil) != tc.refused {
				t.Fatalf("Validate = %v, want refused=%v", err, tc.refused)
			}
		})
	}
}

// THE FLOOR TRAVELS UNDER ITS OWN KEY beside the conclusion, so a peer reads
// both — the register is shared by every build in a rolling upgrade, and a
// field that did not survive the round trip would read as zero on the next
// node, which is the one value a write fence must never be handed by mistake.
func TestTheFloorAndTheConclusionRoundTripSeparately(t *testing.T) {
	t.Parallel()
	row := coord.TrimFloor{Domain: "tracker", Generation: 2, TrimTo: 300, Floor: 900}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"floor":900`) ||
		!strings.Contains(string(raw), `"trim_to":300`) {
		t.Fatalf("the row encodes as %s, want both `floor` and `trim_to`", raw)
	}
	var back coord.TrimFloor
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Floor != 900 || back.TrimTo != 300 {
		t.Fatalf("round trip = floor %d, trim_to %d, want 900 and 300",
			back.Floor, back.TrimTo)
	}
}
