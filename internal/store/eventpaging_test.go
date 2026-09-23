package store_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// seedTraced writes one log row that belongs to a trace and names an agent in
// the tag the party index is built from.
func seedTraced(t *testing.T, log *store.EventLog, id, trace string, at time.Time,
	tags map[string]string,
) {
	t.Helper()
	if err := log.Append(t.Context(), store.EventRecord{
		ID: id, Type: "thing_happened", Time: at, Category: "task",
		TraceID: trace, Tags: tags, Payload: []byte(`{}`),
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

// walkRelated pages the related-agent listing the way a client does — the next
// cursor is the page's LAST row — and returns the ids in the order the walk
// handed them over.
//
// It stops at a zero-row page, which is the only end [store.ListQuery]'s
// RelatedAgent contract offers, and FAILS at a page budget rather than
// looping: a walk that cannot terminate is the defect under test, and a test
// that hangs reports it as a timeout in some other package's suite.
func walkRelated(t *testing.T, log *store.EventLog, q store.ListQuery, pages int) []string {
	t.Helper()
	var order []string
	for page := range pages {
		rows, err := log.List(t.Context(), q)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(rows) == 0 {
			return order
		}
		for _, rec := range rows {
			order = append(order, rec.ID)
		}
		last := rows[len(rows)-1]
		if q.Before != nil && !last.Time.Before(q.Before.Time) &&
			(!last.Time.Equal(q.Before.Time) || last.ID >= q.Before.ID) {
			t.Fatalf("page %d ends at %s/%s, which is not older than the cursor "+
				"%s/%s it was asked from — the walk cannot advance",
				page, last.ID, last.Time.Format(time.RFC3339Nano),
				q.Before.ID, q.Before.Time.Format(time.RFC3339Nano))
		}
		q.Before = &store.Cursor{Time: last.Time, ID: last.ID}
	}
	t.Fatalf("the walk was still returning rows after %d pages; it does not terminate", pages)
	return nil
}

// THE RELATED-AGENT WALK TERMINATES, AND EVERY ROW IS ON EXACTLY ONE PAGE.
//
// The sibling read is merged into the direct read and the result is cut to the
// page, so a sibling the cursor has already passed does not merely duplicate a
// row: the merge sorts it to the HEAD, the cut keeps it, and the page comes
// back as the one before it with the cursor unmoved. Measured before the fix
// on exactly this shape — 60 rows, one trace, all of them naming the agent,
// ten to a page — pages one through five each returned the same ten ids, the
// cursor never left the sixth row, and the other fifty rows were returned by
// no page at all. Since only a zero-row page ends this walk, a dashboard
// asking for "more" asks for ever.
func TestTheRelatedAgentWalkTerminatesWithEveryRowOnExactlyOnePage(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
	const rows = 60
	for i := range rows {
		seedTraced(t, log, eventID(i), "trace-1", base.Add(time.Duration(i)*time.Second),
			map[string]string{"agent_role": "PM"})
	}

	// A page budget well past the 6 pages this needs, so a walk that is
	// merely inefficient is not reported as a walk that cannot end.
	order := walkRelated(t, log, store.ListQuery{RelatedAgent: "PM", Limit: 10}, 20)

	seen := map[string]int{}
	for _, id := range order {
		seen[id]++
	}
	for id, count := range seen {
		if count > 1 {
			t.Errorf("%s was returned by %d pages; a keyset walk returns each row once",
				id, count)
		}
	}
	if len(seen) != rows {
		t.Errorf("the walk returned %d distinct rows of %d; %d are on no page at all",
			len(seen), rows, rows-len(seen))
	}
	for i := range rows {
		if _, on := seen[eventID(i)]; !on {
			t.Errorf("%s was returned by no page", eventID(i))
		}
	}
}

// eventID names a seeded row so its lexical order matches its recency, which
// is the tiebreak the keyset walks on.
func eventID(i int) string { return "e" + string(rune('A'+i/26)) + string(rune('a'+i%26)) }

// THE CAUSE IS STILL PULLED IN, which is the whole point of the second read.
//
// The cursor and the window now bound the sibling query, and a bound that also
// narrowed it by the CALLER'S OTHER FILTERS would delete the only row it
// exists to add: the webhook that woke the seat names the agent nowhere,
// carries another type and another category, and is a sibling precisely
// because it matches nothing the direct read asked for.
func TestTheTriggerThatNamesNoAgentIsStillOnThePage(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	// The cause, naming nobody, a minute before the work it caused.
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "cause", Type: "webhook_received", Time: base, Category: "integration",
		TraceID: "trace-1", Actor: "node-0", Payload: []byte(`{}`),
	}); err != nil {
		t.Fatalf("append the cause: %v", err)
	}
	seedTraced(t, log, "effect", "trace-1", base.Add(time.Minute),
		map[string]string{"agent_role": "PM"})

	rows, err := log.List(t.Context(), store.ListQuery{RelatedAgent: "PM", Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := map[string]bool{}
	for _, rec := range rows {
		ids[rec.ID] = true
	}
	if !ids["cause"] {
		t.Errorf("the page is %v; the trigger that caused the agent's work is "+
			"missing, which is the only reason the sibling read exists", ids)
	}
	if !ids["effect"] {
		t.Errorf("the page is %v; the direct match itself is missing", ids)
	}
}

// A WINDOW THE READER SCRUBBED TO BOUNDS THE SIBLINGS TOO.
//
// The merge sorts newest-first and cuts to the page, so a sibling from above
// `Until` does not appear harmlessly beside the matches — it displaces them.
// A reader who asked for one hour and got the events of the next one was told
// something untrue about the window they chose, and told it by rows that
// pushed the true ones off the page.
func TestASiblingFromOutsideTheWindowIsNotOnThePage(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	// Inside the window: the agent's own work.
	seedTraced(t, log, "inside", "trace-1", base.Add(10*time.Minute),
		map[string]string{"agent_role": "PM"})
	// Same trace, an hour later — outside the window, and NEWER, so an
	// unbounded sibling read sorts it to the head of the page.
	seedTraced(t, log, "after", "trace-1", base.Add(90*time.Minute), nil)
	// Same trace, before the window opens.
	seedTraced(t, log, "before", "trace-1", base.Add(-30*time.Minute), nil)

	rows, err := log.List(t.Context(), store.ListQuery{
		RelatedAgent: "PM",
		Since:        base,
		Until:        base.Add(time.Hour),
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, rec := range rows {
		if rec.Time.Before(base) || !rec.Time.Before(base.Add(time.Hour)) {
			t.Errorf("%s at %s is outside the half-open window [%s, %s) the "+
				"reader asked for", rec.ID, rec.Time.Format(time.RFC3339),
				base.Format(time.RFC3339), base.Add(time.Hour).Format(time.RFC3339))
		}
	}
	if len(rows) != 1 || rows[0].ID != "inside" {
		ids := make([]string, 0, len(rows))
		for _, rec := range rows {
			ids = append(ids, rec.ID)
		}
		t.Errorf("the page is %v, want just the in-window row", ids)
	}
}
