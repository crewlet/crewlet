package statelog_test

import (
	"slices"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The retention report's closed sets, which the dashboard copies, match the
// engine's both ways (internal/clientsource): an engine value the dashboard's
// union lacks falls to a renderer's default arm (an unknown trim floor state
// reads as "no conclusion yet"), and a dashboard value the engine never sends is
// a branch nothing reaches.
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

// The snapshot floor's donor count is the engine's: Settings › Retention tells
// an operator to reach "M donors", so a drifted copy names the wrong number.
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
