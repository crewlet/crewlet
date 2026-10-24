package api_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DASHBOARD UNPACKS A POSITION WITH THE ENGINE'S OWN STRIDE, held here —
// see internal/clientsource for why a copy exists at all and why this side
// checks it.
//
// Every coverage answer carries `applied_through` and `log_seq` packed, and the
// dashboard splits them into a generation and a sequence to print them
// (dashboard/src/components/work.tsx), with the stride its contract declares
// (dashboard/src/contract/positions.ts). A stride that drifted from the engine's
// would still compare correctly — the packed values are compared whole — and
// would print every re-anchored position as a wrong generation and a wrong
// sequence, which nothing else would notice.
func TestTheDashboardUnpacksAPositionWithTheEnginesStride(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "GENERATION_STRIDE")
	if err != nil {
		t.Fatal(err)
	}
	// Base 0 reads the literal as TypeScript spells it, digit separators and
	// all.
	stride, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		t.Fatalf("GENERATION_STRIDE = %q is not a number: %v", raw, err)
	}
	if stride != statelog.GenerationStride {
		t.Errorf("the dashboard unpacks a position with a stride of %d, where the "+
			"engine packs one with %d: a re-anchored log's position would print as "+
			"the wrong generation and sequence", stride, int64(statelog.GenerationStride))
	}
}
