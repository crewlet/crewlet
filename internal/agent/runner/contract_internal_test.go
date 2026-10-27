package runner

import (
	"testing"

	"github.com/crewlet/crewlet/internal/tools"
)

// THE RUNNER'S OWN TOOLS ADVERTISE SCHEMAS THE SURFACE'S CHECK CAN KEEP
// (ADR-0059): the submissions and the discovery pair are first-party calls,
// held to their schemas like every builtin's.
//
// Mutation: add a keyword the check does not implement to workSchema and this
// names it.
func TestTheRunnersSchemasAreOnesTheCheckKeeps(t *testing.T) {
	t.Parallel()
	for name, schema := range map[string]map[string]any{
		SubmitWorkTool:   workSchema,
		SubmitReviewTool: reviewSchema,
		ListMCPToolsTool: (&listMCPTools{}).Parameters(),
		ActivateTool:     (&activateTool{}).Parameters(),
	} {
		if bad := tools.UnsupportedSchema(schema); len(bad) > 0 {
			t.Errorf("%s advertises what the check cannot keep: %v", name, bad)
		}
	}
}
