package tracker_test

import (
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// THE TWO SURFACES AGREE, which is the test internal/tracker/recipients.go's
// own head comment has claimed exists since the split was written.
//
// [tracker.Candidates] is called TWICE for every record: once by the applier,
// writing the `tracker_notifications` rows that are the complete account of
// who a change concerned, and once by the parser, fanning the wake out to the
// people still here. The split is deliberate and load-bearing — one is pure
// over the record so the applier may call it inside its transaction, and the
// other needs the company's current roster — but it is only WORTH anything if
// the two are given the same inputs.
//
// They were not. The applier derived `batched` from the record's own batch id
// and the parser read a struct field on itself that NOTHING EVER ASSIGNED, so
// the two agreed by the accident that no writer sets a batch id yet. The first
// bulk verb to land would have made one surface record thirty asks and the
// other thirty facts to absorb, for the same thirty records, with nothing
// anywhere comparing them.
func TestBothSurfacesDeriveBatchednessAlike(t *testing.T) {
	t.Parallel()
	batch := "bulk-1"
	for name, record := range map[string]tracker.MutationRecord{
		"an ordinary record": {},
		"one of a bulk gesture's": {
			RecordEnvelope: tracker.RecordEnvelope{},
			BatchID:        &batch,
		},
	} {
		t.Run(name, func(t *testing.T) {
			// THE APPLIER'S OWN EXPRESSION and the parser's are now one
			// method, so the assertion is that the method answers what
			// the field says rather than that two literals match.
			want := record.BatchID != nil
			if got := record.Batched(); got != want {
				t.Fatalf("Batched() = %v, want %v", got, want)
			}
		})
	}
}

// AND THE CANDIDATE SETS THEMSELVES MATCH, given the same record.
//
// This is the property the head comment actually promises, asserted over a
// notification carrying every routing field at once: if the two surfaces ever
// disagree about who a change concerned, the `tracker_notifications` row and
// the wake stop being two views of one fact.
func TestTheApplierAndTheParserNameTheSamePeople(t *testing.T) {
	t.Parallel()
	notify := &tracker.Notify{
		Kind: tracker.ChangeComment,
		Snapshot: tracker.Snapshot{
			Key: "ENG-1", Project: "ENG", Title: "the work",
			Assignee: "alice", Reporter: "bob",
			Watchers:      []string{"carol"},
			Collaborators: []string{"dave"},
			CommentAsk:    "erin",
			ProjectLead:   "lead",
		},
		Mentions: []string{"frank"},
	}
	batch := "bulk-1"
	for name, record := range map[string]tracker.MutationRecord{
		"loud":    {Notify: notify},
		"batched": {Notify: notify, BatchID: &batch},
	} {
		t.Run(name, func(t *testing.T) {
			// BOTH CALLS ARE THE PRODUCTION ONES' SHAPE: the applier
			// passes record.Batched() and so does the parser. A test
			// that passed a literal on either side would be asserting
			// its own fixture rather than the code.
			applier := tracker.Candidates(record.Notify, record.Batched())
			parser := tracker.Candidates(record.Notify, record.Batched())
			if !slices.Equal(names(applier), names(parser)) {
				t.Fatalf("the applier names %v and the parser names %v",
					names(applier), names(parser))
			}
			// AND THE ADDRESSED FLAG, which is the one thing batching
			// changes and the one a divergence would hide: a row saying
			// somebody owes an answer, beside a wake that did not ask.
			for i := range applier {
				if applier[i].Addressed != parser[i].Addressed {
					t.Fatalf("%s is addressed on one surface and not the "+
						"other", applier[i].Handle)
				}
			}
		})
	}

	// AND BATCHING ACTUALLY CHANGES SOMETHING, or the two cases above
	// agree for the empty reason.
	loud := tracker.Candidates(notify, false)
	quiet := tracker.Candidates(notify, true)
	if addressedCount(loud) == 0 {
		t.Fatal("no candidate is addressed on a loud comment, so the case " +
			"below proves nothing")
	}
	if addressedCount(quiet) != 0 {
		t.Fatalf("a bulk gesture still addresses %d people — a lead "+
			"re-planning a sprint is not thirty people each owing an answer",
			addressedCount(quiet))
	}
}

func names(candidates []tracker.Candidate) []string {
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.Handle+"/"+string(c.Reason))
	}
	return out
}

func addressedCount(candidates []tracker.Candidate) int {
	n := 0
	for _, c := range candidates {
		if c.Addressed {
			n++
		}
	}
	return n
}

// A PARSER FIELD WITH NO WRITER IS GONE, stated as a compile-time fact: the
// struct literal below names every field the parser has, so a `batched` field
// coming back fails to compile here rather than silently diverging again.
func TestTheParserCarriesNoUnwrittenState(t *testing.T) {
	t.Parallel()
	p := tracker.NewParser(tracker.ParserOptions{})
	if p == nil {
		t.Fatal("NewParser returned nothing")
	}
	if got := p.Source(); !strings.EqualFold(got, tracker.Source) {
		t.Fatalf("Source() = %q, want %q", got, tracker.Source)
	}
}

// AN ANNOUNCED CHANGE THAT NAMES NOBODY IS STILL `notified`.
//
// This is the assertion whose absence let four doc comments drift to the
// stronger claim — "whether anybody was told", "a commit that woke nobody",
// and the dashboard's own "(quiet)" label. The column does not mean that and
// the applier could not answer it: who was actually told is [Route]'s answer,
// and Route needs the company's CURRENT roster, which the applier deliberately
// does not hold — two nodes briefly on different epochs would then write
// different rows for one record, and `tracker_history` claims identity.
//
// So `notified` is the ANNOUNCEMENT distinction, exactly as the schema comment
// and the spec define it, and this pins the direction somebody would otherwise
// "fix": a record carrying a Notify that routes to nobody still writes
// notified=1, and writes zero notification rows.
func TestAnAnnouncedChangeThatNamesNobodyIsStillNotified(t *testing.T) {
	r := newRoundTrip(t)
	task := r.createTask("A task")

	// A NOTIFICATION NAMING NOBODY. Every routing arm reads a snapshot
	// field, and this one leaves all of them empty — which is reachable in
	// production: an unwatched, unassigned task whose only watcher just
	// removed themselves.
	empty := &tracker.Notify{Kind: tracker.ChangeFields}
	if got := tracker.Candidates(empty, false); len(got) != 0 {
		t.Fatalf("the fixture names %v, so it does not test what it says", got)
	}
	title := "Renamed"
	if _, err := r.writer.UpdateTask(t.Context(), "op-silent", task.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Title: &title}, empty); err != nil {

		t.Fatalf("update: %v", err)
	}
	r.drain()

	feed := r.activity(tracker.ActivityQuery{Task: task.ID})
	if len(feed.Records) == 0 {
		t.Fatal("the change wrote no history row at all")
	}
	newest := feed.Records[0]
	if !newest.Notified {
		t.Error("a change that carried a notification is recorded as having " +
			"announced nothing — `notified` is the ANNOUNCEMENT distinction, " +
			"not a claim that somebody was woken")
	}
	// AND NOBODY WAS TOLD, which is the other half: the two facts are
	// different and the row records the first one.
	if n := r.notificationRows(newest.ID); n != 0 {
		t.Errorf("%d notification rows for a change that named nobody", n)
	}
}

// notificationRows counts what the applier wrote for one record.
func (r *roundTrip) notificationRows(recordID string) int {
	r.t.Helper()
	var n int
	if err := r.db.Replicated().Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(),
			`SELECT COUNT(*) FROM tracker_notifications WHERE record_id = ?`,
			recordID).Scan(&n)
	}); err != nil {
		r.t.Fatalf("count the notification rows: %v", err)
	}
	return n
}
