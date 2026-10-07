package learning_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
)

// A LISTING READS THE ROW RECENT READS, BUT FOR ITS TWO OPENINGS AND ITS VECTOR.
//
// The listing's statement is derived from the row's own column list rather than
// spelled again, so every column the row holds is read — and this holds the
// derivation to it: a listing with an opening wider than every text is, column
// for column, what Recent returns, less the vector no listing draws; and with a
// narrow one the ask and the account are their openings, each whole size said.
func TestAListingReadsTheRowButForItsOpeningsAndItsVector(t *testing.T) {
	t.Parallel()
	store := episodes(t)
	long := ep("e1", "cto", base)
	long.Ask = strings.Repeat("what the turn was asked, ", 40)
	long.PlanSummary = strings.Repeat("what it did, ", 40)
	long.ToolSequence, long.SkillsUsed = []string{"read", "page"}, []string{"triage"}
	long.WorkKey, long.ConversationKey = "wk-e1", "slack:C1"
	long.Embedding = []float32{0.5, 0.5}
	folded := ep("c1", "cto", base.Add(time.Hour))
	folded.Kind, folded.Count, folded.SuccessRate = learning.KindCompacted, 4, 0.75
	folded.CommonTaskPattern, folded.NotablePatterns = "triaging pages", "one escalated"
	folded.ExemplarTurnIDs, folded.SubjectsInvolved = []string{"turn-x"}, []string{"sre"}
	folded.WorkKey, folded.Embedding = "wk-c1", nil
	for _, e := range []learning.Episode{long, folded} {
		if _, err := store.Append(t.Context(), e); err != nil {
			t.Fatalf("append %s: %v", e.ID, err)
		}
	}
	recent, err := store.Recent(t.Context(), "cto", 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	wide, err := store.Listing(t.Context(), "cto", 10, 1<<20)
	if err != nil || len(wide) != len(recent) {
		t.Fatalf("Listing = %d rows, %v; want Recent's %d", len(wide), err, len(recent))
	}
	for i, listed := range wide {
		want := recent[i]
		want.Embedding, want.EmbeddingModel = nil, ""
		if !reflect.DeepEqual(listed.Episode, want) {
			t.Errorf("listed %s = %+v, want Recent's row %+v less its vector", want.ID,
				listed.Episode, want)
		}
		if listed.AskBytes != len(want.Ask) || listed.PlanSummaryBytes != len(want.PlanSummary) {
			t.Errorf("listed %s names %d and %d bytes, want %d and %d", want.ID,
				listed.AskBytes, listed.PlanSummaryBytes, len(want.Ask), len(want.PlanSummary))
		}
	}

	narrow, err := store.Listing(t.Context(), "cto", 10, 32)
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	for _, listed := range narrow {
		if listed.ID != "e1" {
			continue
		}
		if listed.Ask != long.Ask[:32] || listed.AskBytes != len(long.Ask) ||
			listed.PlanSummary != long.PlanSummary[:32] ||
			listed.PlanSummaryBytes != len(long.PlanSummary) {
			t.Fatalf("a narrow listing read %q (%d) and %q (%d), want the 32-byte openings "+
				"and the whole sizes", listed.Ask, listed.AskBytes, listed.PlanSummary,
				listed.PlanSummaryBytes)
		}
	}
	if whole, found, err := store.Get(t.Context(), "cto", "e1"); err != nil || !found ||
		whole.Ask != long.Ask || whole.PlanSummary != long.PlanSummary {
		t.Fatalf("Get = found %v, %v; want the row whole", found, err)
	}
	if _, found, err := store.Get(t.Context(), "ceo", "e1"); err != nil || found {
		t.Fatalf("another seat's read of e1 found it (%v, %v)", found, err)
	}
}
