package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// THE ACTIVE REVISION IS READ THROUGH ITS INDEX, and cleared through it, rather
// than by testing every revision the node keeps.
//
// The read runs on every node every reconcile tick, on every GET /config and
// before every conditional write, and each row of company_config is a whole
// sealed company document kept for 400 days, so a scan reads the company's
// whole configuration history each time. The plan is read back for the EXACT
// statements Active and the three activations run, as the event log's
// filters are.
//
// Mutation: drop the `is_active = 1` term from activeRow, and both plans
// scan.
func TestTheActiveRevisionSeeksItsIndex(t *testing.T) {
	t.Parallel()
	db, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for name, query := range map[string]string{"Active": activeSQL, "the deactivation": deactivateSQL} {
		plan := planOf(t, db, query, nil)
		if !strings.Contains(plan, "company_config_one_active_idx") || strings.Contains(plan, "SCAN company_config") {
			t.Errorf("%s plans as %q — it does not seek company_config_one_active_idx, so it "+
				"tests every revision the node keeps", name, plan)
		}
	}
}
