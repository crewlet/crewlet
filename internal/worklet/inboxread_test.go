package worklet_test

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// asBob is the writer for the person whose inbox these cases read: an inbox is
// written ONLY on behalf of its owner, so the harness's own actor cannot mark it.
func asBob(r *roundTrip) *worklet.Writer {
	return r.writer.As("bob", worklet.AuthorHuman, worklet.Provenance{})
}

func (r *roundTrip) inbox(q worklet.InboxQuery) worklet.InboxAnswer {
	r.t.Helper()
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	if q.Snoozed == "" {
		// What every surface defaults to; the reader refuses the zero value.
		q.Snoozed = inbox.SnoozeExclude
	}
	answer, err := r.reader.Inbox(r.t.Context(), q, wednesday)
	if err != nil {
		r.t.Fatalf("Inbox(%+v): %v", q, err)
	}
	return answer
}

// routeTo files work at somebody, so a case gets an inbox row without
// restating the create and its notification. The notify is the caller's: the
// applier routes from what a record says it concerns (see [worklet.Candidates]).
func routeTo(t *testing.T, r *roundTrip, id, key, assignee string) {
	routeAs(t, r, r.writer, id, key, assignee)
}

// routeAs is the same with a stated actor, for the one case that is about who
// made the change rather than who heard about it.
func routeAs(t *testing.T, r *roundTrip, w *worklet.Writer, id, key,
	assignee string) {

	t.Helper()
	task := newTask(id)
	task.Key = key
	task.Assignee = assignee
	task.Title = "work for " + assignee
	if _, err := w.CreateTask(t.Context(), "op-"+id, task, &worklet.Notify{
		Kind: worklet.ChangeCreated,
		Snapshot: worklet.Snapshot{
			Key: key, Project: "ENG", Title: task.Title, Assignee: assignee,
		},
		Excerpt: task.Title,
	}); err != nil {

		t.Fatalf("create %s: %v", id, err)
	}
	r.drain()
}

// TestAnInboxIsReadableAtAll: the rows the applier writes to
// `tracker_notifications`, one per (commit, recipient), can be read back.
func TestAnInboxIsReadableAtAll(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")

	got := r.inbox(worklet.InboxQuery{Handle: "bob"})
	if len(got.Notices) == 0 {
		t.Fatal("bob was assigned work and his inbox is empty")
	}
	notice := got.Notices[0]
	switch {
	case notice.Reason != worklet.ReasonAssignee:
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

	// AND NOBODY ELSE'S INBOX HAS IT: the recipient predicate filters, rather
	// than the table being read whole.
	if other := r.inbox(worklet.InboxQuery{Handle: "cy"}); len(other.Notices) != 0 {
		t.Fatalf("cy's inbox holds %d notices about bob's work",
			len(other.Notices))
	}
}

// TestTheActorJoinsFromTheHistory keeps the one column the notification row
// lacks: who made the change, joined from the history.
func TestTheActorJoinsFromTheHistory(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	bob := r.writer.As("bob", worklet.AuthorHuman, worklet.Provenance{})
	routeAs(t, r, bob, "t-1", "ENG-1", "ana")

	got := r.inbox(worklet.InboxQuery{Handle: "ana"})
	if len(got.Notices) == 0 {
		t.Fatal("ana's inbox is empty")
	}
	if got.Notices[0].Actor != "bob" {
		t.Fatalf("the actor is %q, want bob", got.Notices[0].Actor)
	}
	if got.Notices[0].ActorKind != worklet.AuthorHuman {
		t.Fatalf("the actor kind is %q, want human", got.Notices[0].ActorKind)
	}
}

// TestThePrimarySplitDefaultsRatherThanEmptying: an empty
// [worklet.Person.PrimaryReasons] is "I have not said", so it takes the
// default; read as "nothing is primary" it would blank every fresh person's
// primary half.
func TestThePrimarySplitDefaultsRatherThanEmptying(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")

	got := r.inbox(worklet.InboxQuery{Handle: "bob"})
	if !slices.Equal(got.PrimaryReasons, worklet.DefaultPrimaryReasons) {
		t.Fatalf("a person who has said nothing gets %v, want the shipped "+
			"default", got.PrimaryReasons)
	}
	if got.Primary != 1 || !got.Notices[0].Primary {
		t.Fatalf("work assigned to bob is not primary in his own inbox: "+
			"primary=%d notice=%+v", got.Primary, got.Notices[0])
	}

	// AND A DECLARED LIST REPLACES IT, so the preference has an effect.
	if _, err := asBob(r).MarkInbox(t.Context(), "op-prefs", "bob", statelog.Position{}, inbox.Gesture[worklet.Reason]{
		PrimaryReasons: &[]worklet.Reason{worklet.ReasonMention},
	}, worklet.PersonAuthority{}); err != nil {

		t.Fatalf("declare the split: %v", err)
	}
	r.drain()

	got = r.inbox(worklet.InboxQuery{Handle: "bob"})
	if !slices.Equal(got.PrimaryReasons, []worklet.Reason{worklet.ReasonMention}) {
		t.Fatalf("the declared split is %v, want [mention]", got.PrimaryReasons)
	}
	if got.Primary != 0 || got.Notices[0].Primary {
		t.Fatal("an assignment is primary for somebody who said only " +
			"mentions are, so the declared list has no effect")
	}

	// AND PRIMARY_ONLY DROPS THE REST rather than labelling it.
	if only := r.inbox(worklet.InboxQuery{
		Handle: "bob", PrimaryOnly: true,
	}); len(only.Notices) != 0 {

		t.Fatalf("primary_only returned %d context notices", len(only.Notices))
	}
}

// TestReadIsTheSeenThroughPositionAndTheEntries: the read list is PRUNED at
// every write to what sits above the seen-through position, so reading only
// the lists would resurface every notice older than the person's last visit.
func TestReadIsTheSeenThroughPositionAndTheEntries(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")
	routeTo(t, r, "t-2", "ENG-2", "bob")

	got := r.inbox(worklet.InboxQuery{Handle: "bob"})
	if len(got.Notices) != 2 {
		t.Fatalf("bob's inbox holds %d notices, want 2", len(got.Notices))
	}
	// NEWEST FIRST, which the cursor pages backwards through.
	newest, oldest := got.Notices[0], got.Notices[1]
	if newest.LogSeq <= oldest.LogSeq {
		t.Fatalf("the inbox is not newest-first: %d then %d",
			newest.LogSeq, oldest.LogSeq)
	}
	if got.Unread != 2 {
		t.Fatalf("%d of bob's notices read as unread, want 2 — she has "+
			"marked nothing", got.Unread)
	}

	// THE POSITION MARKS EVERYTHING AT OR BELOW IT, with no entry rows: the
	// pruned state.
	if _, err := asBob(r).MarkInbox(t.Context(), "op-seen", "bob", statelog.Position{}, inbox.Gesture[worklet.Reason]{
		ReadThrough: &statelog.Position{
			Stream: oldest.LogStream, Generation: oldest.LogGeneration,
			Seq: oldest.LogSeq,
		},
	}, worklet.PersonAuthority{}); err != nil {

		t.Fatalf("mark the seen-through position: %v", err)
	}
	r.drain()

	got = r.inbox(worklet.InboxQuery{Handle: "bob"})
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

	// AND AN ENTRY ABOVE IT IS MARKED TOO, as when a queue is worked out of
	// order.
	if _, err := asBob(r).MarkInbox(t.Context(), "op-read", "bob", statelog.Position{},
		inbox.Gesture[worklet.Reason]{Read: []string{newest.RecordID}}, worklet.PersonAuthority{}); err != nil {

		t.Fatalf("mark the newer notice read: %v", err)
	}
	r.drain()

	if got := r.inbox(worklet.InboxQuery{Handle: "bob", Unread: true}); len(
		got.Notices) != 0 {

		t.Fatalf("%d notices are still unread after both were marked",
			len(got.Notices))
	}
}

// TestAStreamMismatchIsNotReadPast is what the position TRIPLE is for: a
// sequence from a dead stream compares as current against a live one, so after
// a stream is recreated the whole backlog would silently read as seen.
func TestAStreamMismatchIsNotReadPast(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")

	got := r.inbox(worklet.InboxQuery{Handle: "bob"})
	if len(got.Notices) != 1 {
		t.Fatalf("bob's inbox holds %d notices, want 1", len(got.Notices))
	}
	notice := got.Notices[0]

	// A writer refuses such a position, so it is planted: the state a person's
	// record is in after the stream was recreated, which the reader must survive.
	if _, err := asBob(r).MarkInbox(t.Context(), "op-prefs", "bob", statelog.Position{}, inbox.Gesture[worklet.Reason]{
		PrimaryReasons: &[]worklet.Reason{},
	}, worklet.PersonAuthority{}); err != nil {
		t.Fatalf("write bob's record: %v", err)
	}
	r.drain()
	if err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE tracker_persons
			SET seen_through = ?, seen_through_stream = ? WHERE handle = 'bob'`,
			int64(notice.LogSeq+1_000), "CREWLET_TRACKER_LOG_FROM_A_PREVIOUS_LIFE")
		return err
	}); err != nil {
		t.Fatalf("plant a position in a dead stream: %v", err)
	}

	if got := r.inbox(worklet.InboxQuery{Handle: "bob"}); got.Unread != 1 {
		t.Fatal("a position from another stream marked the live stream's " +
			"notices read, so a recreated stream empties everybody's inbox")
	}
}

// TestASnoozeMeansNotNow: a snoozed notice is withheld until its time, and
// then returns without a write; dropping a due snooze would lose it, which is
// why [splitSnoozes] reports due rather than promoting.
func TestASnoozeMeansNotNow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")

	notice := r.inbox(worklet.InboxQuery{Handle: "bob"}).Notices[0]
	asleep := wednesday.Add(48 * time.Hour)
	if _, err := asBob(r).MarkInbox(t.Context(), "op-snooze", "bob", statelog.Position{}, inbox.Gesture[worklet.Reason]{
		Snooze: []inbox.Snooze{{RecordID: notice.RecordID, Until: asleep}},
	}, worklet.PersonAuthority{}); err != nil {

		t.Fatalf("snooze: %v", err)
	}
	r.drain()

	if got := r.inbox(worklet.InboxQuery{Handle: "bob"}); len(got.Notices) != 0 {
		t.Fatalf("a snoozed notice is still in the inbox: %+v", got.Notices)
	}
	got := r.inbox(worklet.InboxQuery{Handle: "bob",
		Snoozed: inbox.SnoozeInclude})
	if len(got.Notices) != 1 || !got.Notices[0].Snoozed {
		t.Fatalf("snoozed=include did not return the snoozed notice: %+v",
			got.Notices)
	}

	// AND ONE WHOSE TIME HAS COME IS BACK, from the same rows read after the
	// instant.
	back, err := r.reader.Inbox(t.Context(), worklet.InboxQuery{
		Handle: "bob", Level: statelog.ReadStale,
		Snoozed: inbox.SnoozeExclude,
	}, asleep.Add(time.Minute))
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(back.Notices) != 1 {
		t.Fatal("a snooze whose time has come did not come back, so `not " +
			"now` is a delete that does not say so")
	}
}

// TestTheInboxPagesAndRefuses covers the cursor and the two gates: a page
// must not overrun the caller's limit with its own evidence row, and an
// unknown reason is refused, since narrowing to nothing reads as an empty inbox.
func TestTheInboxPagesAndRefuses(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")
	routeTo(t, r, "t-2", "ENG-2", "bob")
	routeTo(t, r, "t-3", "ENG-3", "bob")

	first := r.inbox(worklet.InboxQuery{Handle: "bob", Limit: 2})
	if len(first.Notices) != 2 {
		t.Fatalf("the first page holds %d, want 2", len(first.Notices))
	}
	if first.NextCursor == "" {
		t.Fatal("a page with more behind it carries no cursor")
	}
	second := r.inbox(worklet.InboxQuery{
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
	after := r.inbox(worklet.InboxQuery{Handle: "bob", Since: statelog.Position{
		Stream: oldest.LogStream, Generation: oldest.LogGeneration,
		Seq: oldest.LogSeq,
	}})
	if len(after.Notices) != 2 {
		t.Fatalf("`since` the oldest returned %d, want the 2 above it",
			len(after.Notices))
	}

	for name, q := range map[string]worklet.InboxQuery{
		"no handle": {Level: statelog.ReadStale, Snoozed: inbox.SnoozeExclude},
		"no level":  {Handle: "bob", Snoozed: inbox.SnoozeExclude},
		"an unknown reason": {Handle: "bob", Level: statelog.ReadStale,
			Snoozed: inbox.SnoozeExclude,
			Reasons: []worklet.Reason{"because-i-said-so"}},
		// THE ZERO SCOPE IS REFUSED: each of the three is some caller's right
		// default, so picking one would answer the others' question wrong silently.
		"no snoozed scope": {Handle: "bob", Level: statelog.ReadStale},
		"an unknown snoozed scope": {Handle: "bob",
			Level: statelog.ReadStale, Snoozed: "sometimes"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.reader.Inbox(t.Context(), q, wednesday); err == nil {
				t.Fatalf("an inbox read with %s was accepted", name)
			}
		})
	}
}

// TestReasonsFilterRatherThanClassify keeps the two narrowings apart: the
// split labels every row and returns them all, `reasons` returns fewer rows.
// "Show me only mentions" and "mentions are what I act on" differ.
func TestReasonsFilterRatherThanClassify(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")

	if got := r.inbox(worklet.InboxQuery{
		Handle: "bob", Reasons: []worklet.Reason{worklet.ReasonMention},
	}); len(got.Notices) != 0 {

		t.Fatalf("filtering to mentions returned %d assignments",
			len(got.Notices))
	}
	if got := r.inbox(worklet.InboxQuery{
		Handle: "bob", Reasons: []worklet.Reason{worklet.ReasonAssignee},
	}); len(got.Notices) != 1 {

		t.Fatal("filtering to assignments returned nothing, so the filter " +
			"refuses everything rather than narrowing")
	}
}

// A CREATION'S NOTICE CARRIES THE ITEM'S KEY. The key is MINTED BY THE WRITE
// (`ENG-1` from the project's counter), so the record's snapshot cannot state
// it and only the applier's row can supply it: "an inbox of uuids is an inbox
// nobody reads" (`SubjectKey`). The other cases state the key on the
// snapshot; this one deliberately does not.
func TestACreationsNoticeCarriesTheKeyTheWriteMinted(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	task := newTask("t-key")
	task.Assignee = "bob"
	task.Title = "work for bob"
	if _, err := r.writer.CreateTask(t.Context(), "op-t-key", task, &worklet.Notify{
		Kind: worklet.ChangeCreated,
		// NO Snapshot.Key, as in a real create: the write assigns it.
		Snapshot: worklet.Snapshot{Project: "ENG", Title: task.Title, Assignee: "bob"},
		Excerpt:  task.Title,
	}); err != nil {

		t.Fatalf("create t-key: %v", err)
	}
	r.drain()

	// Read back rather than a literal: the key comes from the project's
	// counter, so a literal would test this suite's bookkeeping, not the rule.
	detail := r.task(t, "t-key")
	if detail.Task.Key == "" {
		t.Fatal("the write minted no key at all")
	}
	got := r.inbox(worklet.InboxQuery{Handle: "bob"})
	if len(got.Notices) == 0 {
		t.Fatal("bob was assigned work and his inbox is empty")
	}
	if got.Notices[0].SubjectKey != detail.Task.Key {
		t.Fatalf("the notice names %q and the item is %q — the key is minted "+
			"by the write, so only the row the applier read can supply it",
			got.Notices[0].SubjectKey, detail.Task.Key)
	}
}

// AND THE WAKE THE SEAT IS DELIVERED NAMES IT TOO, though the wake is the
// record's own snapshot, built before the write minted the key; an empty `item_key`
// charges the turn to the task's uuid alone (`native:<uuid>` in turn lists).
func TestACreatesWakeCarriesTheKeyTheWriteMinted(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := newTask("t-wake")
	task.Assignee = "bob"
	if _, err := r.writer.CreateTask(t.Context(), "op-t-wake", task, &worklet.Notify{
		Kind:     worklet.ChangeCreated,
		Snapshot: worklet.Snapshot{Project: "ENG", Title: task.Title, Assignee: "bob"},
	}); err != nil {
		t.Fatalf("create t-wake: %v", err)
	}
	r.drain()
	detail := r.task(t, "t-wake")
	wake := r.lastWake()
	if wake == nil {
		t.Fatal("the create published no wake")
	}
	if wake.Snapshot.Key == "" || wake.Snapshot.Key != detail.Task.Key {
		t.Fatalf("the wake names %q and the item is %q", wake.Snapshot.Key, detail.Task.Key)
	}
}

// createOf is the history row id of the create that minted `key` — what the
// notification rows of that change are filed under.
func createOf(r *roundTrip, key string) string {
	r.t.Helper()
	ids := r.strings(`SELECT h.id FROM tracker_history h
		JOIN tracker_tasks t ON t.id = h.subject_id
		WHERE t.key = ? AND h.kind = 'created'`, key)
	if len(ids) != 1 {
		r.t.Fatalf("the create of %s left history rows %v, want one", key, ids)
	}
	return ids[0]
}

// A CHANGE IS NOT NEWS TO THE PERSON WHO MADE IT, whatever credential they
// made it through: a founder filing work on her own seat through her assistant
// must not find it in her inbox as unread "assigned to you". The wake already
// drops her ([worklet.Route]); a person bound to a seat writes AS it
// (iam.ActorFor), so her own write's actor IS the recipient.
func TestYourOwnChangeIsNotInYourInbox(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	jane := r.writer.As("jane-founder", worklet.AuthorHuman, worklet.Provenance{
		OperatorID: "pat:founder",
	})
	routeAs(t, r, jane, "t-own", "ENG-1", "jane-founder")
	// AND A COLLEAGUE'S CHANGE STILL REACHES HER, or the rule would just empty
	// the inbox.
	bob := r.writer.As("bob", worklet.AuthorHuman, worklet.Provenance{})
	routeAs(t, r, bob, "t-bob", "ENG-2", "jane-founder")

	got := r.inbox(worklet.InboxQuery{Handle: "jane-founder"})
	var keys []string
	for _, n := range got.Notices {
		keys = append(keys, n.SubjectKey)
	}
	if !slices.Equal(keys, []string{"ENG-2"}) {
		t.Fatalf("jane's inbox holds %v, want only the task bob filed on her", keys)
	}
	// AND NO ROW WAS WRITTEN FOR HER: `work_routing` reads an empty set for a
	// record as "every candidate was the actor".
	if rows := r.strings(`SELECT recipient FROM tracker_notifications
		WHERE record_id = ?`, createOf(r, "ENG-1")); len(rows) != 0 {
		t.Errorf("the change jane made wrote notification rows for %v", rows)
	}
	// AND THE COLLEAGUE'S DID, or the check above proves nothing.
	if rows := r.strings(`SELECT recipient FROM tracker_notifications
		WHERE record_id = ?`, createOf(r, "ENG-2")); !slices.Contains(rows, "jane-founder") {
		t.Errorf("bob's change wrote notification rows for %v, want jane among them", rows)
	}
}
