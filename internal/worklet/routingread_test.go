package worklet_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/routing"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

func (r *roundTrip) routing(q worklet.RoutingQuery) worklet.RoutingAnswer {
	r.t.Helper()
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	if q.Retention == 0 {
		// EVERY CASE STATES A HORIZON, or the answer can only be
		// `unknown`; the case that wants none calls the reader directly.
		q.Retention = 365 * 24 * time.Hour
	}
	answer, err := r.reader.Routing(r.t.Context(), q, wednesday)
	if err != nil {
		r.t.Fatalf("Routing(%+v): %v", q, err)
	}
	return answer
}

// recordOf is the change id a case reads the routing of, taken from the
// recipient's inbox, as a real caller gets one.
func recordOf(t *testing.T, r *roundTrip, handle string) string {
	t.Helper()
	inbox := r.inbox(worklet.InboxQuery{Handle: handle})
	if len(inbox.Notices) == 0 {
		t.Fatalf("%s has no notices, so there is no record to read", handle)
	}
	return inbox.Notices[0].RecordID
}

// THE OTHER AXIS OF THE SAME ROWS: `tracker_notifications` read by record
// answers "did my comment reach the person I meant" — who, under which
// reason, and whether it asked something of them.
func TestOneChangeSaysWhoItWokeAndWhy(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")

	got := r.routing(worklet.RoutingQuery{RecordID: recordOf(t, r, "bob")})
	switch {
	case !got.Held:
		t.Fatal("the change this record id came from is reported missing")
	case !got.Notified:
		t.Fatal("the history row says nobody was notified, and bob was")
	case len(got.Recipients) == 0:
		t.Fatal("the change woke bob and its routing is empty")
	}
	first := got.Recipients[0]
	switch {
	case first.Handle != "bob":
		t.Fatalf("the recipient is %q, want bob", first.Handle)
	case first.Reason != worklet.ReasonAssignee:
		t.Fatalf("the reason is %q, want assignee", first.Reason)
	case !first.Addressed:
		t.Fatal("an assignment asks something of its assignee and reads as " +
			"merely informing them")
	case got.SubjectKey != "ENG-1":
		t.Fatalf("the change names %q — a routing page of uuids is one "+
			"nobody can place", got.SubjectKey)
	case got.Addressed != 1:
		t.Fatalf("the addressed count is %d, want 1", got.Addressed)
	case got.Delivery != routing.DeliveryReached:
		t.Fatalf("the delivery is %q, want reached", got.Delivery)
	}
}

// A RECORD ID THAT NAMES NOTHING IS NOT AN ERROR (an old link, an unreplayed
// reanchor), and it is a different answer from a change that woke nobody,
// since the remedies differ.
func TestAChangeNobodyWroteIsHeldFalseRatherThanAFailure(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	got := r.routing(worklet.RoutingQuery{RecordID: "no-such-record"})
	switch {
	case got.Held:
		t.Fatal("a record id naming nothing reports a change that exists")
	case len(got.Recipients) != 0:
		t.Fatalf("it carries %d recipients", len(got.Recipients))
	case got.Delivery != routing.DeliveryQuiet:
		t.Fatalf("a change that never existed reports delivery %q, which "+
			"sends an operator to look for rows that never existed",
			got.Delivery)
	}
}

// AN ANNOUNCED CHANGE THAT REACHED NOBODY says so INSIDE the retention
// window: `notified` set and no recipient row, and the horizon is what tells
// that from a set the sweep took.
func TestAnAnnouncedChangeInsideTheWindowThatReachedNobodySaysSo(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := newTask("t-9")
	task.Key = "ENG-9"
	task.Title = "nobody's work"
	if _, err := r.writer.CreateTask(t.Context(), "op-t-9", task,
		&worklet.Notify{Kind: worklet.ChangeCreated}); err != nil {

		t.Fatalf("create t-9: %v", err)
	}
	r.drain()

	feed := r.activity(worklet.ActivityQuery{Task: "t-9"})
	if len(feed.Records) == 0 {
		t.Fatal("the create wrote no history row")
	}
	got := r.routing(worklet.RoutingQuery{RecordID: feed.Records[0].ID})
	switch {
	case !got.Held:
		t.Fatal("the change is reported missing")
	case len(got.Recipients) != 0:
		t.Fatalf("a create naming nobody woke %d people", len(got.Recipients))
	case got.Delivery != routing.DeliveryNobody:
		t.Fatalf("an announced change inside the retention window that "+
			"reached nobody reports delivery %q — the rows would still be "+
			"here if there were any", got.Delivery)
	}
}

// THE ANSWER IS ORDERED FOR A READER, not for the primary key: who has to ACT
// comes before the watchers.
//
// THE HANDLES MAKE ALPHABETICAL ORDER THE WRONG ANSWER (assignee `zoe`, watcher
// `bob`), so the case fails with the `addressed DESC` clause deleted.
func TestTheAddressedRecipientsComeFirst(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := newTask("t-2")
	task.Key = "ENG-2"
	task.Assignee = "zoe"
	task.Watchers = []string{"bob"}
	task.Title = "work for zoe"
	if _, err := r.writer.CreateTask(t.Context(), "op-t-2", task,
		&worklet.Notify{
			Kind: worklet.ChangeCreated,
			Snapshot: worklet.Snapshot{
				Key: "ENG-2", Project: "ENG", Title: task.Title,
				Assignee: "zoe", Watchers: []string{"bob"},
			},
			Excerpt: task.Title,
		}); err != nil {

		t.Fatalf("create t-2: %v", err)
	}
	r.drain()

	got := r.routing(worklet.RoutingQuery{RecordID: recordOf(t, r, "bob")})
	if len(got.Recipients) < 2 {
		t.Fatalf("the change named an assignee and a watcher and woke %d "+
			"people: %+v", len(got.Recipients), got.Recipients)
	}
	if got.Recipients[0].Handle != "zoe" {
		t.Fatalf("the watcher sorts above the assignee: %+v", got.Recipients)
	}
	// AND EVERY ADDRESSED ROW IS ABOVE EVERY UNADDRESSED ONE.
	seenUnaddressed := false
	for _, recipient := range got.Recipients {
		if !recipient.Addressed {
			seenUnaddressed = true
			continue
		}
		if seenUnaddressed {
			t.Fatalf("%s is addressed and sorts below one that is not: %+v",
				recipient.Handle, got.Recipients)
		}
	}
}

// THE BOUND IS THE SUM OF THE WRITE CAPS, so a cap raised without the bound
// fails here rather than silently truncating.
func TestTheRoutingBoundIsTheSumOfTheWriteCaps(t *testing.T) {
	t.Parallel()
	if worklet.MaxRoutingRows <= worklet.MaxWatchers {
		t.Fatalf("MaxRoutingRows is %d and one change may name %d watchers "+
			"alone", worklet.MaxRoutingRows, worklet.MaxWatchers)
	}
	want := worklet.MaxWatchers + worklet.MaxWatchers + worklet.MaxCollaborators +
		worklet.MaxDependents + worklet.MaxDependents +
		worklet.MaxThreadParticipants + worklet.MaxChecklists +
		worklet.MaxMentions + 16
	if worklet.MaxRoutingRows != want {
		t.Fatalf("MaxRoutingRows is %d and the write caps sum to %d — a "+
			"recipient set the engine accepts would be truncated on the way "+
			"back out", worklet.MaxRoutingRows, want)
	}
}

// A READ NAMING NO RECORD, OR NO LEVEL, IS REFUSED.
func TestARoutingReadStatesWhatItIsMissing(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.reader.Routing(t.Context(), worklet.RoutingQuery{
		Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Fatal("a routing read naming no record was answered")
	}
	if _, err := r.reader.Routing(t.Context(), worklet.RoutingQuery{
		RecordID: "r-1",
	}, wednesday); err == nil {
		t.Fatal("a routing read naming no level was answered")
	}
}

// A CALLER THAT STATES NO HORIZON GETS `unknown`, never an absent set dated
// against a retention nobody stated ([worklet.RoutingQuery.Retention]).
func TestWithNoHorizonAnAbsentSetIsUnknownRatherThanAGuess(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := newTask("t-8")
	task.Key = "ENG-8"
	task.Title = "nobody's work"
	if _, err := r.writer.CreateTask(t.Context(), "op-t-8", task,
		&worklet.Notify{Kind: worklet.ChangeCreated}); err != nil {

		t.Fatalf("create t-8: %v", err)
	}
	r.drain()

	feed := r.activity(worklet.ActivityQuery{Task: "t-8"})
	if len(feed.Records) == 0 {
		t.Fatal("the create wrote no history row")
	}
	got, err := r.reader.Routing(t.Context(), worklet.RoutingQuery{
		RecordID: feed.Records[0].ID, Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		t.Fatalf("Routing: %v", err)
	}
	switch {
	case got.Delivery != routing.DeliveryUnknown:
		t.Fatalf("with no horizon stated the delivery is %q", got.Delivery)
	case !got.RetainedFrom.IsZero():
		t.Fatalf("it dated the absence against %s", got.RetainedFrom)
	}
}
