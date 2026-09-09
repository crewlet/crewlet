package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The engine's own tracker and knowledge base, end to end.
//
// What these assert is NOT that a store round-trips — the unit suites cover
// that against fakes. It is that a REAL node, with a real embedded broker, a
// real store and the real API in front of them, writes a record to the
// fleet, projects it onto itself, and answers a screen's question from that
// projection. Every one of those is a wire, and a company on the default
// backends has no other way to record anything.

// operator is the tracker's write authority acting as a person's own
// credential would — one writer per party, derived with [tracker.Writer.As]
// from an identity the caller cannot choose per call.
func operator(t *testing.T, n *node) *tracker.Writer {
	t.Helper()
	w := n.engine.TrackerWriter().As("e2e", tracker.AuthorOperator,
		tracker.Provenance{OperatorID: "e2e"})
	if w == nil {
		t.Fatal("the operator identity was refused")
	}
	return w
}

// newTask is one task as a caller files it: the fields a create names, and
// the derived ones the write path fills in.
func newTask(project, title string) tracker.Task {
	now := time.Now().UTC()
	return tracker.Task{
		V: tracker.DocumentVersion, ID: uuid.NewString(), Project: project,
		Type: tracker.DefaultType, Title: title, Status: tracker.StatusTodo,
		StatusGroup: tracker.GroupNotStarted, Priority: tracker.PriorityNormal,
		Rank: tracker.RankOrigin, Reporter: "e2e",
		CreatedAt: now, UpdatedAt: now,
	}
}

func pageOperator() pages.Actor {
	return pages.Actor{Kind: pages.AuthorOperator, OperatorID: "e2e"}
}

// A COMPANY THAT CONFIGURES NOTHING GETS A TRACKER AND A WIKI. That is the
// default, and it is the case a quickstart hits: no `tracker` block, no
// `knowledge` block, no integrations at all.
func TestADefaultCompanyHasATrackerAndAWiki(t *testing.T) {
	t.Parallel()
	n := start(t)
	waitFor(t, "the native backends to hydrate", n.engine.NativeHydrated)

	if n.engine.TrackerWriter() == nil || n.engine.Tracker() == nil {
		t.Fatal("a company that declares no tracker got no native one")
	}
	if n.engine.PagesStore() == nil || n.engine.Pages() == nil {
		t.Fatal("a company that declares no knowledge base got no native one")
	}
	// AND THE SEARCHER IS THE NATIVE ONE, which is what the turn-start
	// prefetch and every seat's search_knowledge read through.
	searcher := n.engine.Knowledge()
	if searcher == nil {
		t.Fatal("a default company got no knowledge searcher")
	}
	if got := searcher.Backend(); got != "native" {
		t.Errorf("the default searcher answers for %q", got)
	}
}

// A WRITE REACHES THE FLEET'S RECORD AND THEN THIS NODE'S PROJECTION.
//
// The two are different estates and the second is what every read goes to, so
// a write that landed on one and not the other is a board that disagrees with
// the record — which is exactly the failure a projection has to be watched
// for rather than assumed.
func TestAnItemWrittenToTheFleetLandsOnTheBoard(t *testing.T) {
	t.Parallel()
	n := start(t)
	waitFor(t, "the native backends to hydrate", n.engine.NativeHydrated)

	task := newTask("ENG", "the deploy hangs on rollback")
	task.Body = "reproduces on every second run"
	task.Assignee = "ceo"
	written, err := operator(t, n).CreateTask(t.Context(), "e2e-create-1", task, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(written.Key, "ENG-") {
		t.Errorf("the task was keyed %q, not ENG-n", written.Key)
	}
	// THE THREE OUTCOMES ARE NOT A BOOL. A create that reported `pending`
	// is one the fleet holds and this node has not applied — the wait
	// below is what turns it into `applied`, and collapsing the two is
	// how a caller reports work that was never recorded.
	if written.Outcome == statelog.OutcomeUnknown {
		t.Fatalf("the create resolved to %q", written.Outcome)
	}

	// READ THROUGH THIS NODE'S OWN ROWS, not through the log: this is what
	// a board, a tool and the REST route all use, and it is the copy that
	// can be behind.
	if err := n.engine.WaitCommitted(t.Context(), written.Position); err != nil {
		t.Fatalf("wait for the applier: %v", err)
	}
	detail, err := n.engine.Tracker().Task(t.Context(), written.Key,
		tracker.DetailWants{History: true}, statelog.ReadSession)
	if err != nil {
		t.Fatalf("read %s back: %v", written.Key, err)
	}
	if detail.Task.Title != "the deploy hangs on rollback" {
		t.Errorf("the applied task reads %q", detail.Task.Title)
	}
	if detail.Task.Assignee != "ceo" {
		t.Errorf("the applied assignee is %q", detail.Task.Assignee)
	}
	// AND THE ANSWER SAYS HOW COMPLETE IT IS. A detail read that could not
	// distinguish a caught-up node from a lagging one would be the one
	// screen in the product where the difference is invisible.
	if !detail.Complete {
		t.Errorf("a read after its own write reports incomplete: %+v", detail.Incomplete)
	}

	// AND THE BOARD FINDS IT, which is a different query from the detail
	// read: a board filters, and a filter that reached no rows would draw
	// an empty board over a company that has work.
	answer, err := n.engine.Tracker().Tasks(t.Context(), tracker.Query{
		Scope: tracker.Scope{Project: "ENG"},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := slices.ContainsFunc(answer.Rows, func(r tracker.TaskRow) bool {
		return r.Key == written.Key
	})
	if !found {
		t.Errorf("the board lists %d tasks and not the one just filed", len(answer.Rows))
	}
}

// A PAGE IS FINDABLE BY SEARCH, which is the whole reason the knowledge base
// keeps an index rather than only rows.
//
// The index is built BEHIND the projection, asynchronously — so this also
// asserts the one thing that makes that safe: a node still indexing reports
// itself as building rather than answering an empty search, because an empty
// answer is one a seat acts on by writing a page that already exists.
func TestAPageBecomesSearchable(t *testing.T) {
	t.Parallel()
	n := start(t)
	waitFor(t, "the native backends to hydrate", n.engine.NativeHydrated)

	written, err := n.engine.PagesStore().Create(t.Context(), pageOperator(), pages.NewPage{
		Container: "ENG", Title: "Rollback runbook",
		Body: "When a deploy hangs on rollback, drain the node before retrying.",
	})
	if err != nil {
		t.Fatalf("create page: %v", err)
	}
	if err := n.engine.WaitApplied(t.Context(), coord.FamilyPages, written.Revision); err != nil {
		t.Fatalf("wait for the projection: %v", err)
	}

	searcher := n.engine.NativeSearcher()
	if searcher == nil {
		t.Fatal("no native searcher")
	}
	// THE INDEX CATCHES UP ON ITS OWN. Until it does the searcher says so
	// — the assertion below is that the state ENDS, not that it never
	// happened.
	waitFor(t, "the index to catch up", func() bool {
		return !searcher.Building(t.Context())
	})

	hits := searcher.Search(t.Context(), knowledge.Query{
		Text: "rollback drain node", Org: n.engine.Company().Org, Limit: 5,
	})
	if len(hits) == 0 {
		t.Fatal("a page written a moment ago is not findable by its own words")
	}
	if hits[0].Title != "Rollback runbook" {
		t.Errorf("the top hit is %q", hits[0].Title)
	}
}

// THE OPERATOR'S OWN WRITES ARE ATTRIBUTED TO THE OPERATOR, not to a seat.
//
// It is what lets an audit tell a person's edit from an agent's, and it has to
// survive the round trip through the fleet record and the projection —
// dropping the actor kind anywhere in between would make every operator edit
// read as a colleague's.
func TestAnOperatorWriteIsStillAnOperatorWriteOnTheBoard(t *testing.T) {
	t.Parallel()
	n := start(t)
	waitFor(t, "the native backends to hydrate", n.engine.NativeHydrated)

	written, err := operator(t, n).CreateTask(t.Context(), "e2e-create-2",
		newTask("OPS", "rotate the signing key"), nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := n.engine.WaitCommitted(t.Context(), written.Position); err != nil {
		t.Fatalf("wait for the applier: %v", err)
	}
	detail, err := n.engine.Tracker().Task(t.Context(), written.Key,
		tracker.DetailWants{History: true}, statelog.ReadSession)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(detail.History) == 0 {
		t.Fatal("the task has no history, so nothing records who filed it")
	}
	change := detail.History[len(detail.History)-1]
	// THE KIND IS THE DISCRIMINATOR, and it is what every renderer and
	// every recipient rule reads. An author string alone cannot say
	// whether `e2e` is a credential or a colleague.
	if change.ActorKind != tracker.AuthorOperator {
		t.Errorf("the filing is attributed as %q, not as an operator", change.ActorKind)
	}
	if change.Actor != "e2e" {
		t.Errorf("the change names its author %q", change.Actor)
	}
}
