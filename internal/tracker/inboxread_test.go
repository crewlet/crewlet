package tracker_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// asBob is the writer for the person whose inbox these cases read. An inbox
// is written ONLY on behalf of the person whose it is, so the harness's own
// actor cannot mark it — which is the rule, not the harness's shortcoming.
func asBob(r *roundTrip) *tracker.Writer {
	return r.writer.As("bob", tracker.AuthorHuman, tracker.Provenance{})
}

func (r *roundTrip) inbox(q tracker.InboxQuery) tracker.InboxAnswer {
	r.t.Helper()
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	answer, err := r.reader.Inbox(r.t.Context(), q, wednesday)
	if err != nil {
		r.t.Fatalf("Inbox(%+v): %v", q, err)
	}
	return answer
}

// routeTo files work at somebody, so a case gets an inbox row without
// restating the create and its notification.
//
// THE NOTIFY IS THE CALLER'S, which is the design: a record states who it
// concerns, and the applier routes from that rather than re-deriving it from
// the task — see [tracker.Candidates].
func routeTo(t *testing.T, r *roundTrip, id, key, assignee string) {
	routeAs(t, r, r.writer, id, key, assignee)
}

// routeAs is the same with a stated actor, for the one case that is about who
// made the change rather than who heard about it.
func routeAs(t *testing.T, r *roundTrip, w *tracker.Writer, id, key,
	assignee string) {

	t.Helper()
	task := newTask(id)
	task.Key = key
	task.Assignee = assignee
	task.Title = "work for " + assignee
	if _, err := w.CreateTask(t.Context(), "op-"+id, task, &tracker.Notify{
		Kind: tracker.ChangeCreated,
		Snapshot: tracker.Snapshot{
			Key: key, Project: "ENG", Title: task.Title, Assignee: assignee,
		},
		Excerpt: task.Title,
	}); err != nil {

		t.Fatalf("create %s: %v", id, err)
	}
	r.drain()
}

// TestAnInboxIsReadableAtAll is the finding this reader answers. The applier
// has written `tracker_notifications` since the domain landed — one row per
// (commit, recipient), with the reason, the subject and the excerpt — and the
// table shipped two indexes naming a reader that was never written. A company
// routed every change to the people it concerned, wrote them all down,
// replicated them, and offered no way to ask what was in them.
func TestAnInboxIsReadableAtAll(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")

	got := r.inbox(tracker.InboxQuery{Handle: "bob"})
	if len(got.Notices) == 0 {
		t.Fatal("bob was assigned work and his inbox is empty")
	}
	notice := got.Notices[0]
	switch {
	case notice.Reason != tracker.ReasonAssignee:
		t.Fatalf("the reason is %q, want assignee", notice.Reason)
	case notice.SubjectID != "t-1":
		t.Fatalf("the subject is %q, want t-1", notice.SubjectID)
	case notice.SubjectKey != "ENG-1":
		t.Fatalf("the key is %q — an inbox of uuids is an inbox nobody reads",
			notice.SubjectKey)
	case notice.RecordID == "":
		t.Fatal("the notice names no record, so nothing can mark it read")
	case notice.LogStream == "":
		t.Fatal("the notice carries no stream, so its position cannot be " +
			"compared against a seen-through one")
	case notice.At.IsZero():
		t.Fatal("the notice has no instant")
	}

	// AND NOBODY ELSE'S INBOX HAS IT, which is the recipient predicate
	// doing its job rather than the table being read whole.
	if other := r.inbox(tracker.InboxQuery{Handle: "cy"}); len(other.Notices) != 0 {
		t.Fatalf("cy's inbox holds %d notices about bob's work",
			len(other.Notices))
	}
}

// TestTheActorJoinsFromTheHistory keeps the one column this read cannot get
// from the notification row: who made the change. A notice saying something
// happened without saying who did it is a notice its reader has to open the
// task to understand.
func TestTheActorJoinsFromTheHistory(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	bob := r.writer.As("bob", tracker.AuthorHuman, tracker.Provenance{})
	routeAs(t, r, bob, "t-1", "ENG-1", "ana")

	got := r.inbox(tracker.InboxQuery{Handle: "ana"})
	if len(got.Notices) == 0 {
		t.Fatal("ana's inbox is empty")
	}
	if got.Notices[0].Actor != "bob" {
		t.Fatalf("the actor is %q, want bob", got.Notices[0].Actor)
	}
	if got.Notices[0].ActorKind != tracker.AuthorHuman {
		t.Fatalf("the actor kind is %q, want human", got.Notices[0].ActorKind)
	}
}

// TestThePrimarySplitDefaultsRatherThanEmptying is the whole reason
// [tracker.Person.PrimaryReasons] could be read as a preference at all: an
// empty list is "I have not said", and reading it as "nothing is primary"
// would give every person in a fresh company an inbox whose primary half is
// blank — which is the state the field was in before it had a reader.
func TestThePrimarySplitDefaultsRatherThanEmptying(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")

	got := r.inbox(tracker.InboxQuery{Handle: "bob"})
	if !slices.Equal(got.PrimaryReasons, tracker.DefaultPrimaryReasons) {
		t.Fatalf("a person who has said nothing gets %v, want the shipped "+
			"default", got.PrimaryReasons)
	}
	if got.Primary != 1 || !got.Notices[0].Primary {
		t.Fatalf("work assigned to bob is not primary in his own inbox: "+
			"primary=%d notice=%+v", got.Primary, got.Notices[0])
	}

	// AND A DECLARED LIST REPLACES IT, so the preference has an effect
	// rather than being storage for a rule nobody wrote.
	if _, err := asBob(r).WriteInbox(t.Context(), "op-prefs", "bob", nil, nil,
		nil, []tracker.Reason{tracker.ReasonMention},
		tracker.Position{}); err != nil {

		t.Fatalf("declare the split: %v", err)
	}
	r.drain()

	got = r.inbox(tracker.InboxQuery{Handle: "bob"})
	if !slices.Equal(got.PrimaryReasons, []tracker.Reason{tracker.ReasonMention}) {
		t.Fatalf("the declared split is %v, want [mention]", got.PrimaryReasons)
	}
	if got.Primary != 0 || got.Notices[0].Primary {
		t.Fatal("an assignment is primary for somebody who said only " +
			"mentions are, so the declared list has no effect")
	}

	// AND PRIMARY_ONLY DROPS THE REST rather than labelling it.
	if only := r.inbox(tracker.InboxQuery{
		Handle: "bob", PrimaryOnly: true,
	}); len(only.Notices) != 0 {

		t.Fatalf("primary_only returned %d context notices", len(only.Notices))
	}
}

// TestReadIsTheSeenThroughPositionAndTheEntries protects the half of the mark
// that is easy to leave out. The entry lists are PRUNED at every write to what
// sits above the seen-through position, so reading only the lists reports
// every pruned notice unread — which is every notice older than the person's
// last visit, the exact set an inbox must not resurface.
func TestReadIsTheSeenThroughPositionAndTheEntries(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")
	routeTo(t, r, "t-2", "ENG-2", "bob")

	got := r.inbox(tracker.InboxQuery{Handle: "bob"})
	if len(got.Notices) != 2 {
		t.Fatalf("bob's inbox holds %d notices, want 2", len(got.Notices))
	}
	// NEWEST FIRST, which is what the cursor pages backwards through.
	newest, oldest := got.Notices[0], got.Notices[1]
	if newest.LogSeq <= oldest.LogSeq {
		t.Fatalf("the inbox is not newest-first: %d then %d",
			newest.LogSeq, oldest.LogSeq)
	}
	if got.Unread != 2 {
		t.Fatalf("%d of bob's notices read as unread, want 2 — she has "+
			"marked nothing", got.Unread)
	}

	// THE POSITION MARKS EVERYTHING AT OR BELOW IT, with no entry rows
	// at all, which is the pruned state.
	if _, err := asBob(r).WriteInbox(t.Context(), "op-seen", "bob", nil, nil,
		nil, nil, tracker.Position{
			Stream: oldest.LogStream, Generation: oldest.LogGeneration,
			Seq: oldest.LogSeq,
		}); err != nil {

		t.Fatalf("mark the seen-through position: %v", err)
	}
	r.drain()

	got = r.inbox(tracker.InboxQuery{Handle: "bob"})
	if got.Unread != 1 {
		t.Fatalf("%d unread after reading past the older notice, want 1",
			got.Unread)
	}
	for _, notice := range got.Notices {
		if notice.RecordID == oldest.RecordID && !notice.Read {
			t.Fatal("a notice at the seen-through position reads as unread, " +
				"so every pruned entry resurfaces on the next visit")
		}
	}

	// AND AN ENTRY ABOVE IT IS MARKED TOO, which is what a person working
	// their queue out of order does.
	if _, err := asBob(r).WriteInbox(t.Context(), "op-read", "bob",
		[]tracker.InboxEntry{{RecordID: newest.RecordID, Position: newest.LogSeq}},
		nil, nil, nil, tracker.Position{
			Stream: oldest.LogStream, Generation: oldest.LogGeneration,
			Seq: oldest.LogSeq,
		}); err != nil {

		t.Fatalf("mark the newer notice read: %v", err)
	}
	r.drain()

	if got := r.inbox(tracker.InboxQuery{Handle: "bob", Unread: true}); len(
		got.Notices) != 0 {

		t.Fatalf("%d notices are still unread after both were marked",
			len(got.Notices))
	}
}

// TestAStreamMismatchIsNotReadPast is what the position TRIPLE is for. A
// sequence from a dead stream compares as current against a live one, so a
// founder whose stream was recreated would open an inbox in which everything
// below their old position reads as already seen — the whole backlog, silently.
func TestAStreamMismatchIsNotReadPast(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")

	got := r.inbox(tracker.InboxQuery{Handle: "bob"})
	if len(got.Notices) != 1 {
		t.Fatalf("bob's inbox holds %d notices, want 1", len(got.Notices))
	}
	notice := got.Notices[0]

	if _, err := asBob(r).WriteInbox(t.Context(), "op-dead", "bob", nil, nil,
		nil, nil, tracker.Position{
			Stream:     "CREWLET_TRACKER_LOG_FROM_A_PREVIOUS_LIFE",
			Generation: notice.LogGeneration, Seq: notice.LogSeq + 1_000,
		}); err != nil {

		t.Fatalf("mark a position in a dead stream: %v", err)
	}
	r.drain()

	if got := r.inbox(tracker.InboxQuery{Handle: "bob"}); got.Unread != 1 {
		t.Fatal("a position from another stream marked the live stream's " +
			"notices read, so a recreated stream empties everybody's inbox")
	}
}

// TestASnoozeMeansNotNow. An inbox that returned a snoozed notice anyway would
// make the gesture do nothing; one that dropped a snooze whose time has come
// would lose it, which is why [splitSnoozes] reports due rather than promoting.
func TestASnoozeMeansNotNow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")

	notice := r.inbox(tracker.InboxQuery{Handle: "bob"}).Notices[0]
	asleep := wednesday.Add(48 * time.Hour)
	if _, err := asBob(r).WriteInbox(t.Context(), "op-snooze", "bob", nil, nil,
		[]tracker.InboxEntry{{
			RecordID: notice.RecordID, Position: notice.LogSeq, Until: &asleep,
		}}, nil, tracker.Position{}); err != nil {

		t.Fatalf("snooze: %v", err)
	}
	r.drain()

	if got := r.inbox(tracker.InboxQuery{Handle: "bob"}); len(got.Notices) != 0 {
		t.Fatalf("a snoozed notice is still in the inbox: %+v", got.Notices)
	}
	got := r.inbox(tracker.InboxQuery{Handle: "bob", IncludeSnoozed: true})
	if len(got.Notices) != 1 || !got.Notices[0].Snoozed {
		t.Fatalf("include_snoozed did not return the snoozed notice: %+v",
			got.Notices)
	}

	// AND ONE WHOSE TIME HAS COME IS BACK, without a write to promote it.
	past := wednesday.Add(-time.Hour)
	if _, err := asBob(r).WriteInbox(t.Context(), "op-due", "bob", nil, nil,
		[]tracker.InboxEntry{{
			RecordID: notice.RecordID, Position: notice.LogSeq, Until: &past,
		}}, nil, tracker.Position{}); err != nil {

		t.Fatalf("snooze into the past: %v", err)
	}
	r.drain()
	if got := r.inbox(tracker.InboxQuery{Handle: "bob"}); len(got.Notices) != 1 {
		t.Fatal("a snooze whose time has come did not come back, so `not " +
			"now` is a delete that does not say so")
	}
}

// TestTheInboxPagesAndRefuses covers the cursor and the two gates. A page that
// returned its own evidence row would overrun the caller's limit, and an
// unknown reason silently narrowing to nothing is worse than a refusal — the
// caller reads an empty inbox as an empty inbox.
func TestTheInboxPagesAndRefuses(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")
	routeTo(t, r, "t-2", "ENG-2", "bob")
	routeTo(t, r, "t-3", "ENG-3", "bob")

	first := r.inbox(tracker.InboxQuery{Handle: "bob", Limit: 2})
	if len(first.Notices) != 2 {
		t.Fatalf("the first page holds %d, want 2", len(first.Notices))
	}
	if first.NextCursor == "" {
		t.Fatal("a page with more behind it carries no cursor")
	}
	second := r.inbox(tracker.InboxQuery{
		Handle: "bob", Limit: 2, Cursor: first.NextCursor,
	})
	if len(second.Notices) != 1 {
		t.Fatalf("the second page holds %d, want 1", len(second.Notices))
	}
	if second.NextCursor != "" {
		t.Fatal("the last page carries a cursor, so a caller pages for ever")
	}
	for _, was := range first.Notices {
		for _, is := range second.Notices {
			if was.RecordID == is.RecordID {
				t.Fatalf("%s is on both pages", was.RecordID)
			}
		}
	}

	// AND `since` IS THE CHEAP FORM of the same question.
	oldest := second.Notices[0]
	after := r.inbox(tracker.InboxQuery{Handle: "bob", Since: statelog.Position{
		Stream: oldest.LogStream, Generation: oldest.LogGeneration,
		Seq: oldest.LogSeq,
	}})
	if len(after.Notices) != 2 {
		t.Fatalf("`since` the oldest returned %d, want the 2 above it",
			len(after.Notices))
	}

	for name, q := range map[string]tracker.InboxQuery{
		"no handle": {Level: statelog.ReadStale},
		"no level":  {Handle: "bob"},
		"an unknown reason": {Handle: "bob", Level: statelog.ReadStale,
			Reasons: []tracker.Reason{"because-i-said-so"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.reader.Inbox(t.Context(), q, wednesday); err == nil {
				t.Fatalf("an inbox read with %s was accepted", name)
			}
		})
	}
}

// TestReasonsFilterRatherThanClassify keeps the two narrowings apart. The
// split labels every row and returns them all; `reasons` returns fewer rows.
// One verb doing both would make "show me only mentions" and "mentions are
// what I act on" the same gesture, and they are not.
func TestReasonsFilterRatherThanClassify(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")

	if got := r.inbox(tracker.InboxQuery{
		Handle: "bob", Reasons: []tracker.Reason{tracker.ReasonMention},
	}); len(got.Notices) != 0 {

		t.Fatalf("filtering to mentions returned %d assignments",
			len(got.Notices))
	}
	if got := r.inbox(tracker.InboxQuery{
		Handle: "bob", Reasons: []tracker.Reason{tracker.ReasonAssignee},
	}); len(got.Notices) != 1 {

		t.Fatal("filtering to assignments returned nothing, so the filter " +
			"refuses everything rather than narrowing")
	}
}
