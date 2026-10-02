package api_test

import (
	"strconv"
	"strings"
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
// (dashboard/src/components/work.tsx). A stride that drifted from the engine's
// would still compare correctly — the packed values are compared whole — and
// would print every re-anchored position as a wrong generation and a wrong
// sequence, which nothing else would notice.
func TestTheDashboardUnpacksAPositionWithTheEnginesStride(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Declaration(clientsource.Tree(t),
		`export const GENERATION_STRIDE = ([0-9_]+);`)
	if err != nil {
		t.Fatal(err)
	}
	stride, err := strconv.ParseInt(strings.ReplaceAll(body, "_", ""), 10, 64)
	if err != nil {
		t.Fatalf("GENERATION_STRIDE = %q is not a number: %v", body, err)
	}
	if stride != statelog.GenerationStride {
		t.Errorf("the dashboard unpacks a position with a stride of %d, where the "+
			"engine packs one with %d: a re-anchored log's position would print as "+
			"the wrong generation and sequence", stride, int64(statelog.GenerationStride))
	}
}
