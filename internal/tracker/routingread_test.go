package tracker_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

func (r *roundTrip) routing(q tracker.RoutingQuery) tracker.RoutingAnswer {
	r.t.Helper()
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	if q.Retention == 0 {
		// EVERY CASE STATES A HORIZON, because a query that does not
		// can only answer `unknown` — the one case that wants none says
		// so by calling the reader directly.
		q.Retention = 365 * 24 * time.Hour
	}
	answer, err := r.reader.Routing(r.t.Context(), q, wednesday)
	if err != nil {
		r.t.Fatalf("Routing(%+v): %v", q, err)
	}
	return answer
}

// recordOf is the change id a case reads the routing of, taken from the
// recipient's own inbox — which is where every real caller gets one too.
func recordOf(t *testing.T, r *roundTrip, handle string) string {
	t.Helper()
	inbox := r.inbox(tracker.InboxQuery{Handle: handle})
	if len(inbox.Notices) == 0 {
		t.Fatalf("%s has no notices, so there is no record to read", handle)
	}
	return inbox.Notices[0].RecordID
}

// THE OTHER AXIS OF THE SAME ROWS. `tracker_notifications` has always been
// readable by recipient and never by record, so "did my comment reach the
// person I meant" had exactly one answer in the whole product — the history
// row's `notified` boolean, which says that somebody, somewhere, was told.
func TestOneChangeSaysWhoItWokeAndWhy(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")

	got := r.routing(tracker.RoutingQuery{RecordID: recordOf(t, r, "bob")})
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
	case first.Reason != tracker.ReasonAssignee:
		t.Fatalf("the reason is %q, want assignee", first.Reason)
	case !first.Addressed:
		t.Fatal("an assignment asks something of its assignee and reads as " +
			"merely informing them")
	case got.SubjectKey != "ENG-1":
		t.Fatalf("the change names %q — a routing page of uuids is one "+
			"nobody can place", got.SubjectKey)
	case got.Addressed != 1:
		t.Fatalf("the addressed count is %d, want 1", got.Addressed)
	case got.Delivery != tracker.DeliveryReached:
		t.Fatalf("the delivery is %q, want reached", got.Delivery)
	}
}

// A RECORD ID THAT NAMES NOTHING IS NOT AN ERROR. A link from an old page, or
// a reanchor that has not replayed this far, is an ordinary thing for a caller
// to hold — and the two states have different remedies, so they are two
// answers rather than one empty list.
func TestAChangeNobodyWroteIsHeldFalseRatherThanAFailure(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	got := r.routing(tracker.RoutingQuery{RecordID: "no-such-record"})
	switch {
	case got.Held:
		t.Fatal("a record id naming nothing reports a change that exists")
	case len(got.Recipients) != 0:
		t.Fatalf("it carries %d recipients", len(got.Recipients))
	case got.Delivery != tracker.DeliveryQuiet:
		t.Fatalf("a change that never existed reports delivery %q, which "+
			"sends an operator to look for rows that never existed",
			got.Delivery)
	}
}

// AN ANNOUNCED CHANGE THAT REACHED NOBODY says so, and says it INSIDE the
// retention window — which is what makes it a statement about the routing
// rather than about the sweep. `notified` is set (the commit carried a
// notification) and no recipient row exists, and the horizon is what tells
// that from a set retention took.
func TestAnAnnouncedChangeInsideTheWindowThatReachedNobodySaysSo(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := newTask("t-9")
	task.Key = "ENG-9"
	task.Title = "nobody's work"
	if _, err := r.writer.CreateTask(t.Context(), "op-t-9", task,
		&tracker.Notify{Kind: tracker.ChangeCreated}); err != nil {

		t.Fatalf("create t-9: %v", err)
	}
	r.drain()

	feed := r.activity(tracker.ActivityQuery{Task: "t-9"})
	if len(feed.Records) == 0 {
		t.Fatal("the create wrote no history row")
	}
	got := r.routing(tracker.RoutingQuery{RecordID: feed.Records[0].ID})
	switch {
	case !got.Held:
		t.Fatal("the change is reported missing")
	case len(got.Recipients) != 0:
		t.Fatalf("a create naming nobody woke %d people", len(got.Recipients))
	case got.Delivery != tracker.DeliveryNobody:
		t.Fatalf("an announced change inside the retention window that "+
			"reached nobody reports delivery %q — the rows would still be "+
			"here if there were any", got.Delivery)
	}
}

// THE ANSWER IS ORDERED FOR A READER, not for the primary key. A person
// scanning who a change reached is looking for who has to ACT, and the
// watchers — reached for context — are the ones they care about least.
//
// THE HANDLES ARE CHOSEN SO THAT ALPHABETICAL ORDER IS THE WRONG ANSWER: the
// assignee is `zoe` and the watcher is `bob`, so a query that sorted only by
// recipient would put the watcher first and pass a test written the other way
// round. It did — the first draft of this case used `bob` as the assignee and
// stayed green with the `addressed DESC` clause deleted.
func TestTheAddressedRecipientsComeFirst(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := newTask("t-2")
	task.Key = "ENG-2"
	task.Assignee = "zoe"
	task.Watchers = []string{"bob"}
	task.Title = "work for zoe"
	if _, err := r.writer.CreateTask(t.Context(), "op-t-2", task,
		&tracker.Notify{
			Kind: tracker.ChangeCreated,
			Snapshot: tracker.Snapshot{
				Key: "ENG-2", Project: "ENG", Title: task.Title,
				Assignee: "zoe", Watchers: []string{"bob"},
			},
			Excerpt: task.Title,
		}); err != nil {

		t.Fatalf("create t-2: %v", err)
	}
	r.drain()

	got := r.routing(tracker.RoutingQuery{RecordID: recordOf(t, r, "bob")})
	if len(got.Recipients) < 2 {
		t.Fatalf("the change named an assignee and a watcher and woke %d "+
			"people: %+v", len(got.Recipients), got.Recipients)
	}
	if got.Recipients[0].Handle != "zoe" {
		t.Fatalf("the watcher sorts above the assignee: %+v", got.Recipients)
	}
	// AND EVERY ADDRESSED ROW IS ABOVE EVERY UNADDRESSED ONE, which is the
	// ordering itself rather than this one pair.
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

// THE BOUND IS ARITHMETIC, and it has to stay that way: every source of a
// recipient is capped at the write, so a literal here would silently start
// truncating the day one of those caps moved.
func TestTheRoutingBoundIsTheSumOfTheWriteCaps(t *testing.T) {
	t.Parallel()
	if tracker.MaxRoutingRows <= tracker.MaxWatchers {
		t.Fatalf("MaxRoutingRows is %d and one change may name %d watchers "+
			"alone", tracker.MaxRoutingRows, tracker.MaxWatchers)
	}
	// The sum of every capped source, restated here so that raising a cap
	// without carrying it into the bound is a failure rather than a quiet
	// truncation.
	want := tracker.MaxWatchers + tracker.MaxWatchers + tracker.MaxCollaborators +
		tracker.MaxDependents + tracker.MaxDependents +
		tracker.MaxThreadParticipants + tracker.MaxChecklistAssignees +
		tracker.MaxGoalOwners + tracker.MaxGoalMembers +
		tracker.MaxMentions + 16
	if tracker.MaxRoutingRows != want {
		t.Fatalf("MaxRoutingRows is %d and the write caps sum to %d — a "+
			"recipient set the engine accepts would be truncated on the way "+
			"back out", tracker.MaxRoutingRows, want)
	}
}

// A READ NAMING NEITHER A RECORD NOR A LEVEL IS REFUSED, in the shape every
// other reader here refuses: the message names what to pass.
func TestARoutingReadStatesWhatItIsMissing(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.reader.Routing(t.Context(), tracker.RoutingQuery{
		Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Fatal("a routing read naming no record was answered")
	}
	if _, err := r.reader.Routing(t.Context(), tracker.RoutingQuery{
		RecordID: "r-1",
	}, wednesday); err == nil {
		t.Fatal("a routing read naming no level was answered")
	}
}

// A CALLER THAT STATES NO HORIZON GETS `unknown`, never a guess. It is the
// whole reason the horizon is on the query: a reader outlives the epoch, so
// one holding a retention nobody is running would date an absent set against
// a number from last week and report it gone.
func TestWithNoHorizonAnAbsentSetIsUnknownRatherThanAGuess(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := newTask("t-8")
	task.Key = "ENG-8"
	task.Title = "nobody's work"
	if _, err := r.writer.CreateTask(t.Context(), "op-t-8", task,
		&tracker.Notify{Kind: tracker.ChangeCreated}); err != nil {

		t.Fatalf("create t-8: %v", err)
	}
	r.drain()

	feed := r.activity(tracker.ActivityQuery{Task: "t-8"})
	if len(feed.Records) == 0 {
		t.Fatal("the create wrote no history row")
	}
	got, err := r.reader.Routing(t.Context(), tracker.RoutingQuery{
		RecordID: feed.Records[0].ID, Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		t.Fatalf("Routing: %v", err)
	}
	switch {
	case got.Delivery != tracker.DeliveryUnknown:
		t.Fatalf("with no horizon stated the delivery is %q", got.Delivery)
	case !got.RetainedFrom.IsZero():
		t.Fatalf("it dated the absence against %s", got.RetainedFrom)
	}
}
