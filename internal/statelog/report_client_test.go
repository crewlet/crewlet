package statelog_test

import (
	"slices"
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
		{`export type RetentionTrimFloorState =([^;]*);`,
			stringsOf(statelog.TrimFloorStates())},
		{`export type RetentionGenerationState =([^;]*);`,
			stringsOf(statelog.GenerationStates())},
		{`export type RetentionIdentityCause =([^;]*);`,
			stringsOf(statelog.IdentityCauses())},
	} {
		body, err := clientsource.Declaration(clientsource.Tree(t), gate.declaration)
		if err != nil {
			t.Fatal(err)
		}
		client := clientsource.Strings(body)
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

// stringsOf is a named-string enum's values as plain strings.
func stringsOf[T ~string](values []T) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, string(v))
	}
	return out
}
