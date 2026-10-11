package queries_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD KNOWS THE REACH OF EVERY QUESTION THIS BUILD ANSWERS.
//
// The person shell is held to asking only what a member is served — the
// dashboard's `app/shell.test.ts` walks every screen a person-shell chunk
// reaches and refuses a question above `member` — and the classification it
// judges by is the registry's, declared on the client as `QUESTION_REACH`
// (`contract/reach.ts`). So the table is held here against the registry every
// seam registers, BOTH WAYS:
//
//   - a question the engine answers that the table does not name is one the
//     shell gate cannot judge, so a person screen could ask it unseen;
//   - a reach that differs is the gate judging by a classification the engine
//     no longer makes — a question moved to `admin` would stay askable from a
//     person screen until a member's tab was refused it;
//   - a row naming a question nothing answers is a renamed question left
//     behind, reading as a decision somebody made.
func TestTheDashboardKnowsEveryQuestionsReach(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, everySeam(t))
	answered := r.Names()

	table, err := clientsource.Literal(clientsource.Tree(t), "QUESTION_REACH")
	if err != nil {
		t.Fatal(err)
	}
	named, err := clientsource.Keys(table)
	if err != nil {
		t.Fatal(err)
	}
	// A FLOOR: a table read as empty passes every comparison below.
	if len(named) < 20 {
		t.Fatalf("QUESTION_REACH names %d questions; a reader that stopped reading "+
			"it would certify nothing", len(named))
	}
	for _, what := range answered {
		if !slices.Contains(named, what) {
			t.Errorf("this build answers %q at %q and QUESTION_REACH does not name "+
				"it, so the person shell's gate cannot judge a screen that asks it",
				what, r.ReachOf(what))
			continue
		}
		reach := clientsource.Field(table, what)
		if len(reach) != 1 || reach[0] != string(r.ReachOf(what)) {
			t.Errorf("QUESTION_REACH says %q is served at %q and the registry "+
				"answers it at %q", what, reach, r.ReachOf(what))
		}
	}
	for _, what := range named {
		if !slices.Contains(answered, what) {
			t.Errorf("QUESTION_REACH names %q and nothing answers it — a renamed "+
				"question leaves exactly this behind", what)
		}
	}
}
