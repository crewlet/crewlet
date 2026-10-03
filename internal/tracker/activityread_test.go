package tracker_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

func (r *roundTrip) activity(q tracker.ActivityQuery) tracker.ActivityAnswer {
	r.t.Helper()
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	answer, err := r.reader.Activity(r.t.Context(), q, wednesday)
	if err != nil {
		r.t.Fatalf("Activity(%+v): %v", q, err)
	}
	return answer
}

// A QUIET COMMIT IS IN THE FEED. "Quiet" means it woke nobody, not that it did
// not happen — and a feed assembled from the notifications would be an account
// of what was ANNOUNCED rather than of what was done.
func TestTheActivityFeedCarriesQuietCommits(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	done := tracker.StatusDone
	// NO NOTIFY: this change tells nobody.
	if _, err := r.writer.UpdateTask(t.Context(), "op-quiet", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()

	answer := r.activity(tracker.ActivityQuery{Task: "t-1"})
	if len(answer.Records) < 2 {
		t.Fatalf("the feed carries %d records for a task that was created and "+
			"moved, want both — a quiet commit is still something that "+
			"happened", len(answer.Records))
	}
	// NEWEST FIRST, and the order is the LOG's rather than any clock's:
	// two nodes' authored instants can tie and can run backwards, and
	// neither says which commit the broker accepted first.
	if answer.Records[0].Kind != tracker.ChangeStatus {
		t.Fatalf("the newest record is %q, want the status change — the feed "+
			"is newest first", answer.Records[0].Kind)
	}
	first := answer.Records[0]
	if first.Notified {
		t.Error("a commit that carried no notification is reported as notified " +
			"— `notified` is how a reader tells `nothing was announced` from " +
			"`nothing happened`")
	}
	if first.Fields["status"].To != string(tracker.StatusDone) {
		t.Errorf("the record's deltas are %+v, want the status it moved to",
			first.Fields)
	}
	if first.SubjectKey == "" {
		t.Error("the record names no task key — a feed of uuids is a feed " +
			"nobody reads")
	}
	if first.LogStream == "" || first.LogSeq == 0 {
		t.Errorf("the record carries position %s@%d:%d, want the whole triple "+
			"— a bare sequence names no stream and no generation, so a cursor "+
			"built from one cannot survive a reanchor",
			first.LogStream, first.LogGeneration, first.LogSeq)
	}
	// BOTH INSTANTS. The authored one is what a person typed and what a
	// card renders; the effective one is what every duration is measured
	// on. A surface carrying one of them silently answers a different
	// question than it looks like.
	if first.At.IsZero() || first.EffectiveAt.IsZero() {
		t.Errorf("the record carries at=%v effective_at=%v, want both",
			first.At, first.EffectiveAt)
	}
}

// A TEXT SEARCH IS GATED ON WHAT IT WOULD SCAN, never on which keys were
// named.
//
// Written as "requires a task, a container or a since bound" the gate was one
// a caller satisfied in one attempt and learned nothing from:
// `container=workspace&q=` names a container and an unbounded `since:` names a
// bound, and both run the full scan the gate exists to stop.
func TestAnActivitySearchIsRefusedByWhatItWouldScan(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// (a) COMPANY SCOPE with a text search: refused naming BOTH keys.
	_, err := r.reader.Activity(t.Context(), tracker.ActivityQuery{
		Q: "deploy", Workspace: true, Level: statelog.ReadStale,
	}, wednesday)
	if err == nil {
		t.Fatal("a company-wide text search over the feed answered — it reads " +
			"every commit this company has ever made")
	}
	for _, want := range []string{"task", "container", "since"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is %q and does not name %q — a gate that "+
				"names one key is a gate a caller satisfies without narrowing "+
				"anything", err, want)
		}
	}

	// (b) A PROJECT WITH NO WINDOW is refused too, which is the half the
	// name-shaped gate let through.
	if _, err := r.reader.Activity(t.Context(), tracker.ActivityQuery{
		Q: "deploy", Project: "ENG", Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Error("a project-wide text search with no window answered — it " +
			"reads the project's whole history")
	}

	// (c) AND A WINDOW WIDER THAN THE SPAN, which is the other half: a
	// five-year `since:` satisfies "a since bound" and narrows nothing.
	_, err = r.reader.Activity(t.Context(), tracker.ActivityQuery{
		Q: "deploy", Project: "ENG", Level: statelog.ReadStale,
		SinceAt: wednesday.AddDate(-5, 0, 0),
	}, wednesday)
	if err == nil {
		t.Fatal("a five-year text search over a project answered")
	}
	if !strings.Contains(err.Error(), "90") {
		t.Errorf("the refusal is %q and does not name the span", err)
	}

	// (d) INSIDE THE SPAN IT ANSWERS, and so does a search scoped to one
	// task at any age — one subject is an index range however old.
	r.activity(tracker.ActivityQuery{
		Q: "deploy", Project: "ENG", SinceAt: wednesday.AddDate(0, 0, -30),
	})
	filedTask(t, r, "t-1")
	r.activity(tracker.ActivityQuery{Q: "deploy", Task: "t-1"})

	// AND A QUERY WITH NO `q` IS NEVER GATED: `kinds`, `actor` and the
	// position cursor are all indexed, so a company-wide feed is the
	// ordinary case rather than the dangerous one.
	r.activity(tracker.ActivityQuery{Workspace: true})
}

// A FEED'S POSITIONS ARE THE CALLER'S TO GET RIGHT, AND A MISTAKE IN ONE IS
// REFUSED AS THE REQUEST'S.
//
// Both feeds take two positions from their caller — the cursor they resume
// from and a `since` bound — and compare each against `log_seq`, a packed
// column carrying no stream. A position from another domain's log answered a
// page of THIS feed chosen by a number from that one, with nothing to say so;
// a cursor that was not a position at all was parsed inside the read's
// transaction and failed the way a store does, which every surface answered as
// a fault of the node. Each is now refused before a row is read, with
// [tracker.ErrBadQuery] — the one sentinel a surface answers as a bad request —
// and a foreign one with [statelog.ErrForeignPosition] beside it. None of them
// is a state-log refusal. The activity gate and an unknown inbox reason are
// the same kind of refusal and carry the same sentinel. The control is the
// cursor the feed itself handed out, which pages.
func TestAFeedRefusesAPositionItCannotUseAsTheRequestsMistake(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")

	pages := statelog.Position{Stream: "CREWLET_PAGES_LOG", Generation: 1, Seq: 5}
	type ask struct {
		name    string
		run     func() error
		foreign bool
	}
	activity := func(q tracker.ActivityQuery) func() error {
		return func() error {
			q.Level = statelog.ReadStale
			_, err := r.reader.Activity(t.Context(), q, wednesday)
			return err
		}
	}
	inbox := func(q tracker.InboxQuery) func() error {
		return func() error {
			q.Handle, q.Level = "bob", statelog.ReadStale
			// THE SCOPE EVERY SURFACE DEFAULTS TO, so the case reaches
			// the position it is about rather than the absent scope.
			q.Snoozed = tracker.SnoozeExclude
			_, err := r.reader.Inbox(t.Context(), q, wednesday)
			return err
		}
	}
	for _, c := range []ask{
		{"an activity cursor from another log",
			activity(tracker.ActivityQuery{Workspace: true, Cursor: pages.String()}), true},
		{"an activity since from another log",
			activity(tracker.ActivityQuery{Workspace: true, Since: pages}), true},
		{"an activity cursor that is not a position",
			activity(tracker.ActivityQuery{Workspace: true, Cursor: "42"}), false},
		{"an activity search wider than the feed scans",
			activity(tracker.ActivityQuery{Workspace: true, Q: "deploy"}), false},
		{"an inbox cursor from another log",
			inbox(tracker.InboxQuery{Cursor: pages.String()}), true},
		{"an inbox since from another log",
			inbox(tracker.InboxQuery{Since: pages}), true},
		{"an inbox cursor that is not a position",
			inbox(tracker.InboxQuery{Cursor: "42"}), false},
		{"an unknown wake reason",
			inbox(tracker.InboxQuery{Reasons: []tracker.Reason{"because-i-said-so"}}), false},
	} {
		err := c.run()
		if !errors.Is(err, tracker.ErrBadQuery) {
			t.Errorf("%s: %v, want %v", c.name, err, tracker.ErrBadQuery)
			continue
		}
		if c.foreign && !errors.Is(err, statelog.ErrForeignPosition) {
			t.Errorf("%s: %v, want it to say the position is on another log", c.name, err)
		}
		if errors.Is(err, statelog.ErrUnavailable) {
			t.Errorf("%s: %v is a state-log refusal — the request is what is "+
				"wrong, on every node alike", c.name, err)
		}
	}

	// THE CONTROL: the feed's own cursor is on this log and pages.
	first := r.activity(tracker.ActivityQuery{Workspace: true, Limit: 1})
	if first.NextCursor == "" {
		t.Fatal("a one-row page over two commits carries no cursor")
	}
	if next := r.activity(tracker.ActivityQuery{
		Workspace: true, Limit: 1, Cursor: first.NextCursor,
	}); len(next.Records) != 1 {
		t.Errorf("the feed's own cursor paged to %d records, want 1", len(next.Records))
	}
}

// THE CURSOR IS A LOG POSITION, and it pages with no gap and no repeat.
func TestTheActivityCursorPagesByPosition(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for i := range 5 {
		filedTask(t, r, "t-"+itoa(i))
	}

	first := r.activity(tracker.ActivityQuery{Workspace: true, Limit: 2})
	if len(first.Records) != 2 || first.NextCursor == "" {
		t.Fatalf("the first page has %d records and cursor %q, want 2 and a "+
			"cursor", len(first.Records), first.NextCursor)
	}
	second := r.activity(tracker.ActivityQuery{
		Workspace: true, Limit: 2, Cursor: first.NextCursor,
	})
	if len(second.Records) != 2 {
		t.Fatalf("the second page has %d records, want 2", len(second.Records))
	}
	// NO REPEAT, which a cursor comparing anything but the total order
	// cannot promise: two commits share an authored instant routinely.
	seen := map[string]bool{}
	for _, record := range append(first.Records, second.Records...) {
		if seen[record.ID] {
			t.Fatalf("record %s is on both pages — the cursor is not strictly "+
				"after the last row", record.ID)
		}
		seen[record.ID] = true
	}
	// AND STRICTLY DESCENDING: the page after is older, always.
	if second.Records[0].LogSeq >= first.Records[1].LogSeq {
		t.Errorf("the second page starts at %d and the first ended at %d, want "+
			"strictly older", second.Records[0].LogSeq, first.Records[1].LogSeq)
	}

	// A CURSOR THAT IS NOT A POSITION IS REFUSED naming the shape, never
	// silently ignored — a page that quietly restarted from the top would
	// loop for ever.
	if _, err := r.reader.Activity(t.Context(), tracker.ActivityQuery{
		Workspace: true, Cursor: "42", Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Error("a bare sequence was accepted as a cursor — it names no " +
			"stream and no generation")
	}
}

// THE FILTERS NARROW, and each one is a different question.
func TestTheActivityFiltersNarrow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")
	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-done", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()

	byKind := r.activity(tracker.ActivityQuery{
		Workspace: true, Kinds: []tracker.ChangeKind{tracker.ChangeStatus},
	})
	if len(byKind.Records) != 1 || byKind.Records[0].SubjectID != "t-1" {
		t.Fatalf("kinds=status answers %d records, want the one status change",
			len(byKind.Records))
	}
	byTask := r.activity(tracker.ActivityQuery{Task: "t-2"})
	for _, record := range byTask.Records {
		if record.SubjectID != "t-2" {
			t.Fatalf("task=t-2 answers a record about %s", record.SubjectID)
		}
	}
	// A KEY RESOLVES, AND IN ANY CASE. The history is the one place a
	// renamed task is most likely to be looked up from, and a key is what
	// somebody pastes out of a chat message rather than a uuid.
	key := byTask.Records[0].SubjectKey
	if key == "" {
		t.Fatal("the feed names no key for a task, so this case tests nothing")
	}
	if got := r.activity(tracker.ActivityQuery{
		Task: strings.ToLower(key),
	}); len(got.Records) == 0 {
		t.Errorf("the feed answered nothing for %q — a key is uppercased "+
			"before it is resolved, because `eng-9` is the same task as "+
			"`ENG-9`", strings.ToLower(key))
	}
	if _, err := r.reader.Activity(t.Context(), tracker.ActivityQuery{
		Task: "nope", Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Error("the feed answered for an unknown task — an empty history and " +
			"a typo are different facts")
	}

	// AND `actor` IS NOT `assignee`. One is who did it, the other whose
	// work it is, and they are routinely different people.
	byActor := r.activity(tracker.ActivityQuery{Workspace: true, Actor: "ana"})
	if len(byActor.Records) == 0 {
		t.Error("actor=ana answers nothing, and every commit here is hers")
	}
	if got := r.activity(tracker.ActivityQuery{
		Workspace: true, Actor: "nobody",
	}); len(got.Records) != 0 {
		t.Errorf("actor=nobody answers %d records", len(got.Records))
	}
}

// THE FEED NARROWS TO WHO WAS WRITING, which is a different question from
// which handle.
//
// `actor` is a name and the two name spaces are DISJOINT: an `operator` commit
// carries the token's own label and an `agent` one carries a seat handle. So
// "what did the operators of this company do to it" — the whole of the audit
// screen — cannot be asked as a set of handles. That set is the roster, it
// changes, and a commit by somebody who has left would silently drop out of an
// audit assembled from it.
//
// BOTH DIRECTIONS, because a filter that narrowed to nothing and one that
// narrowed to everything both look like a working screen: the first reads as a
// quiet company and the second as one with no seats.
func TestTheFeedNarrowsToWhoWasWriting(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")

	// The harness's own writer is a HUMAN; this one is the operator token,
	// which is what an audit is about.
	operator := r.writer.As("ops-1", tracker.AuthorOperator, tracker.Provenance{})
	done := tracker.StatusDone
	if _, err := operator.UpdateTask(t.Context(), "op-by-token", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus,
		nil); err != nil {
		t.Fatalf("UpdateTask as the operator: %v", err)
	}
	r.drain()

	all := r.activity(tracker.ActivityQuery{Workspace: true})
	if len(all.Records) < 2 {
		t.Fatalf("the feed carries %d records, want the human's create and the "+
			"operator's change — this case tests nothing without both",
			len(all.Records))
	}

	byOperator := r.activity(tracker.ActivityQuery{
		Workspace: true, ActorKinds: []tracker.AuthorKind{tracker.AuthorOperator},
	})
	if len(byOperator.Records) != 1 {
		t.Fatalf("actor_kinds=operator answers %d records, want the one commit "+
			"a token made", len(byOperator.Records))
	}
	if got := byOperator.Records[0]; got.ActorKind != tracker.AuthorOperator {
		t.Errorf("the record it answered was written by a %s", got.ActorKind)
	}

	// A SET IS A UNION, not an intersection: "everything a person or a
	// token did" is one question and it names two kinds.
	both := r.activity(tracker.ActivityQuery{
		Workspace: true,
		ActorKinds: []tracker.AuthorKind{
			tracker.AuthorOperator, tracker.AuthorHuman,
		},
	})
	if len(both.Records) != len(all.Records) {
		t.Errorf("actor_kinds=operator,human answers %d of %d records, and every "+
			"commit here was written by one or the other",
			len(both.Records), len(all.Records))
	}

	// AND A KIND NOBODY WROTE UNDER ANSWERS NOTHING rather than falling
	// through to every commit — which is what an ignored filter does, and
	// on an audit surface an ignored filter is a wider answer wearing the
	// shape of the narrow one.
	if got := r.activity(tracker.ActivityQuery{
		Workspace: true, ActorKinds: []tracker.AuthorKind{tracker.AuthorSystem},
	}); len(got.Records) != 0 {
		t.Errorf("actor_kinds=system answers %d records, and the engine wrote "+
			"none of them", len(got.Records))
	}
}

// A KIND THE FEED HAS NO ROWS OF IS REFUSED NAMING THE SET, never answered with
// an empty page.
//
// A filter matching nothing answers exactly like a quiet project. The query
// surface refused a mistyped kind by name while a seat's `task_activity` built
// a kind from any string it was handed, so a model asking for `status_changed`
// was told nothing had happened. The read itself refuses it now, as
// [tracker.ErrBadQuery], so every surface inherits one answer — and the
// control, a kind the feed does have, still pages.
//
// Mutation: drop activityFilters from Activity and both refusals read an empty
// page instead.
func TestAnActivityFilterOnAKindThisBuildDoesNotHaveIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	for _, c := range []struct {
		name string
		q    tracker.ActivityQuery
		want string
	}{
		{"a change kind", tracker.ActivityQuery{Workspace: true,
			Kinds: []tracker.ChangeKind{"status_changed"}}, "status_changed"},
		{"an author kind", tracker.ActivityQuery{Workspace: true,
			ActorKinds: []tracker.AuthorKind{"robot"}}, "robot"},
	} {
		c.q.Level = statelog.ReadStale
		_, err := r.reader.Activity(t.Context(), c.q, wednesday)
		if !errors.Is(err, tracker.ErrBadQuery) {
			t.Errorf("%s nobody has: %v, want %v", c.name, err, tracker.ErrBadQuery)
			continue
		}
		if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "created") &&
			!strings.Contains(err.Error(), "agent") {
			t.Errorf("%s: %q does not name what was asked and what is accepted",
				c.name, err)
		}
	}
	// THE CONTROL.
	if got := r.activity(tracker.ActivityQuery{Workspace: true,
		Kinds: []tracker.ChangeKind{tracker.ChangeCreated}}); len(got.Records) != 1 {
		t.Errorf("a kind the feed has answered %d records, want the one create",
			len(got.Records))
	}
}

// THE FEED SAYS WHERE ITS NODE IS ON THE LOG, and how much of it produced its
// rows. The audit names a tracker answer whose node is behind by these two
// numbers alone, so they are the whole of what it can say: a node holding a
// record it cannot decode moves its checkpoint past that record, reads exactly
// like one at the head of the log by its position, and only the applied prefix
// stopping below the record says otherwise. A retained record ABOVE the
// checkpoint holds nothing back that the checkpoint has passed, so the prefix
// is the checkpoint — never a position beyond it, which an applied prefix
// cannot be. It is the framework's one rule ([statelog.PrefixIn]), which
// this package once kept a copy of, and the copy answered that last case with
// a prefix past the checkpoint.
func TestTheFeedReportsItsPositionAndAppliedPrefix(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")

	caught := r.activity(tracker.ActivityQuery{})
	if len(caught.Records) < 2 {
		t.Fatalf("the feed carries %d records, want both tasks' — this case "+
			"tests nothing without them", len(caught.Records))
	}
	at := func(record tracker.ActivityRecord) uint64 {
		return uint64(statelog.Position{
			Generation: record.LogGeneration, Seq: record.LogSeq,
		}.Packed())
	}
	newest, oldest := at(caught.Records[0]), at(caught.Records[len(caught.Records)-1])
	if caught.LogSeq < newest {
		t.Errorf("the feed is at %d, below its own newest record at %d",
			caught.LogSeq, newest)
	}
	if caught.AppliedThrough != caught.LogSeq {
		t.Errorf("a node that retains nothing applied through %d of %d",
			caught.AppliedThrough, caught.LogSeq)
	}

	// A RECORD RETAINED AT THE OLDEST RECORD'S POSITION: the checkpoint is
	// past it, and the applied prefix stops just below it.
	r.retainAt(oldest)
	behind := r.activity(tracker.ActivityQuery{})
	if behind.LogSeq != caught.LogSeq {
		t.Errorf("retaining a record moved the position from %d to %d",
			caught.LogSeq, behind.LogSeq)
	}
	if behind.AppliedThrough != oldest-1 {
		t.Errorf("a node retaining the record at %d applied through %d, want %d",
			oldest, behind.AppliedThrough, oldest-1)
	}

	// AND ONE ABOVE THE CHECKPOINT holds back nothing the checkpoint passed.
	r.clearRetained()
	r.retainAt(caught.LogSeq + 1_000)
	ahead := r.activity(tracker.ActivityQuery{})
	if ahead.AppliedThrough != ahead.LogSeq {
		t.Errorf("a record retained above the checkpoint put the applied "+
			"prefix at %d against a checkpoint of %d — an applied prefix "+
			"past the checkpoint is a position no record reached",
			ahead.AppliedThrough, ahead.LogSeq)
	}
}

// retainAt files a record this node "could not decode" at a packed position,
// the rows the framework's own loop writes when it retains one.
func (r *roundTrip) retainAt(position uint64) {
	r.t.Helper()
	if err := r.db.Replicated().Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `
			INSERT INTO tracker_log_deferred
				(position, subject, subject_kind, subject_id, version, payload, stored_at)
			VALUES (?, 'task.x', 'task', 'x', ?, x'00', 0)`,
			int64(position), tracker.RecordVersion+1)
		return err
	}); err != nil {
		r.t.Fatalf("retain a record: %v", err)
	}
}

// clearRetained forgets every record [roundTrip.retainAt] filed.
func (r *roundTrip) clearRetained() {
	r.t.Helper()
	if err := r.db.Replicated().Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `DELETE FROM tracker_log_deferred`)
		return err
	}); err != nil {
		r.t.Fatalf("clear the retained records: %v", err)
	}
}
