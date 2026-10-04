package queries

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/usage"
)

// A SEAT CARRIES ONE NAME IN BOTH FOLDS. When a seat's rows name it under two
// handles and two of them share a last read, the page's
// `skill_loaded_by` and its `page_reads` must still agree on which name the
// reader sees — so both folds break the tie the same way, through one rule.
func TestBothFoldsNameASeatAlikeOnATiedLastRead(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
	rows := []usage.PageRead{
		{PageID: "p-1", Day: "2026-09-24", Node: "node-a", AgentID: "seat-1", Handle: "swe",
			Via: "skill_loaded", Count: 1, LastAt: at},
		{PageID: "p-1", Day: "2026-09-24", Node: "node-b", AgentID: "seat-1", Handle: "engineer",
			Via: "skill_loaded", Count: 1, LastAt: at},
	}
	loads := foldSkillLoads(rows, nil)
	reads := foldPageReads(rows, pageReadsFold{today: "2026-09-24"})
	if len(loads) != 1 || len(reads.Readers) != 1 {
		t.Fatalf("folded %d loads and %d readers, want one seat each", len(loads), len(reads.Readers))
	}
	if loads[0].Handle != reads.Readers[0].Handle {
		t.Errorf("skill_loaded_by names the seat %q and page_reads %q, want one name",
			loads[0].Handle, reads.Readers[0].Handle)
	}
}
