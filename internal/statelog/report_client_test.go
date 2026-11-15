package statelog_test

import (
	"slices"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The retention report's closed sets the dashboard keeps its own copy of, held
// against the engine's in both directions — see internal/clientsource.
//
// A value the engine sends and the dashboard's union does not name falls to a
// renderer's default arm: a trim floor state the screen does not know reads as
// "no conclusion yet", a generation state as a distance it is not. A value the
// dashboard names and the engine never sends is a branch nothing reaches.
func TestTheDashboardKnowsEveryRetentionReportState(t *testing.T) {
	t.Parallel()
	for _, gate := range []struct {
		declaration string
		engine      []string
	}{
		{"RetentionTrimFloorState", stringsOf(statelog.TrimFloorStates())},
		{"RetentionGenerationState", stringsOf(statelog.GenerationStates())},
		{"RetentionIdentityCause", stringsOf(statelog.IdentityCauses())},
		{"RetentionTermName", stringsOf(statelog.TermNames)},
	} {
		client, err := clientsource.Union(clientsource.Tree(t), gate.declaration)
		if err != nil {
			t.Fatal(err)
		}
		if len(client) == 0 {
			t.Fatalf("%s names nothing, so this gate certifies nothing", gate.declaration)
		}
		for _, v := range gate.engine {
			if !slices.Contains(client, v) {
				t.Errorf("the engine sends %q and the dashboard's %s does not name it "+
					"(it names %v)", v, gate.declaration, client)
			}
		}
		for _, v := range client {
			if !slices.Contains(gate.engine, v) {
				t.Errorf("the dashboard names %q, which the engine never sends (%v)",
					v, gate.engine)
			}
		}
	}
}

// THE SNAPSHOT FLOOR'S DONOR COUNT IS THE ENGINE'S: Settings › Retention says
// "N of M donors" and tells an operator to reach M, so a copy that drifted
// would send them to fix the wrong number.
func TestTheDashboardCountsTheDonorsTheSnapshotFloorRequires(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "SNAPSHOT_DONORS_REQUIRED")
	if err != nil {
		t.Fatal(err)
	}
	if client, err := strconv.Atoi(raw); err != nil || client != statelog.SnapshotDonorsRequired {
		t.Errorf("the dashboard requires %q snapshot donors and the engine %d: change "+
			"SNAPSHOT_DONORS_REQUIRED in dashboard/src/contract/retention.ts to the engine's figure",
			raw, statelog.SnapshotDonorsRequired)
	}
}

// stringsOf is a named-string enum's values as plain strings.
func stringsOf[T ~string](values []T) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, string(v))
	}
	return out
}
