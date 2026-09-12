package tracker_test

import (
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
	inSprint(t, r, "t-1", nil)
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
	inSprint(t, r, "t-1", nil)
	r.activity(tracker.ActivityQuery{Q: "deploy", Task: "t-1"})

	// AND A QUERY WITH NO `q` IS NEVER GATED: `kinds`, `actor` and the
	// position cursor are all indexed, so a company-wide feed is the
	// ordinary case rather than the dangerous one.
	r.activity(tracker.ActivityQuery{Workspace: true})
}

// THE CURSOR IS A LOG POSITION, and it pages with no gap and no repeat.
func TestTheActivityCursorPagesByPosition(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for i := range 5 {
		inSprint(t, r, "t-"+itoa(i), nil)
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
	inSprint(t, r, "t-1", nil)
	inSprint(t, r, "t-2", nil)
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
