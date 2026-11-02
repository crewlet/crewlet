package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// THE BOOT ROW AND THE FLEET'S ROW ARE READ THROUGH THEIR INDEXES, and
// cleared through them, rather than by testing every revision the node keeps.
//
// The boot row is read at every start and on every converged reconcile tick,
// the fleet's row whenever the API has no pointer to answer from, and each row
// of company_config is a whole sealed company document kept for 400 days, so a
// scan reads the company's whole configuration history each time. The plan is
// read back for the EXACT statements the reads and the clears run, as the event
// log's filters are.
//
// Mutation: drop the `= 1` term from bootRow or fleetsRow, and its plans scan.
func TestTheMarkedRevisionsSeekTheirIndexes(t *testing.T) {
	t.Parallel()
	db, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for name, statement := range map[string]struct{ query, index string }{
		"Boot":            {bootSQL, "company_config_one_active_idx"},
		"clearing a boot": {unbootSQL, "company_config_one_active_idx"},
		"Fleets":          {fleetsSQL, "company_config_one_fleets_idx"},
		"clearing fleets": {unfleetsSQL, "company_config_one_fleets_idx"},
	} {
		plan := planOf(t, db, statement.query, nil)
		if !strings.Contains(plan, statement.index) || strings.Contains(plan, "SCAN company_config") {
			t.Errorf("%s plans as %q — it does not seek %s, so it tests every revision "+
				"the node keeps", name, plan, statement.index)
		}
	}
}
