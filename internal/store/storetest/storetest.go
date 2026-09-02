// Package storetest is the store's contract suite.
//
// It was written when there were two drivers and one dialect, to keep every
// statement inside their intersection by running the same assertions against
// both. There is one driver now and the suite is still the
// contract: what it pins is the BEHAVIOUR the packages above the store depend
// on — keyset paging that does not skip a row, a read floor, an idempotent
// append, a retention sweep that stops where it is told — none of which is a
// property of a driver name. A driver pin bump runs it unchanged, which is
// exactly the day it earns its keep.
//
// Run takes a constructor rather than a *store.DB so each subtest gets its own
// file: the store owns its file exclusively, and sharing one across parallel
// subtests would test a configuration the engine never runs in.
package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// Run executes the contract suite against databases produced by newDB.
func Run(t *testing.T, newDB func(t *testing.T) *store.DB) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(t *testing.T, db *store.DB)
	}{
		{"Schema", testSchema},
		{"SchemaIsIdempotent", testSchemaIdempotent},
		{"Capabilities", testCapabilities},
		{"AppendIsIdempotent", testAppendIdempotent},
		{"AppendRejectsAnIncompleteIdentity", testAppendIncomplete},
		{"KeysetPaging", testKeysetPaging},
		{"CursorIsExclusive", testCursorExclusive},
		{"IdenticalTimestampsBreakByID", testIdenticalTimestamps},
		{"OrderIsByTimeNotInsertion", testOrderByTime},
		{"ShortPageIsTheEnd", testShortPage},
		{"Filters", testFilters},
		{"RelatedAgent", testRelatedAgent},
		{"FailureFlag", testFailureFlag},
		{"TraceIsOldestFirst", testTrace},
		{"TraceKeepsTheOldestRowsAtTheCap", testTraceCap},
		{"ByID", testByID},
		{"ReadFloor", testReadFloor},
		{"RetentionSweep", testRetention},
		{"RetentionSweepDrainsABacklogWiderThanOneBatch", testRetentionBacklog},
		{"RelatedAgentIsAnIndexSeekNotAScan", testRelatedIndexed},
		{"RelatedAgentIndexIsSweptWithTheLog", testRelatedSwept},
		{"SpendFoldsTheWholeWindowNotACappedPrefix", testSpendUncapped},
		{"SpendSurvivesARecordBuiltByHand", testSpendDerived},
		{"BackupIsAReadableCopyOfTheData", testBackup},
		{"BackupIsOneSelfContainedFile", testBackupSelfContained},
		{"BackupRefusesAnOccupiedDestination", testBackupOccupied},
		{"BackupSurvivesConcurrentWrites", testBackupUnderWrites},
		{"RecordSkipsUntrackedTypes", testRecordUntracked},
		{"NullUnconstrainedWorkKey", testWorkKeyNull},
		{"FollowRoundTrips", testFollowRoundTrips},
		{"FollowUpdatesTheReasonAndTheStamp", testFollowUpdatesTheReasonAndTheStamp},
		{"FollowsAreScopedByBackend", testFollowsAreScopedByBackend},
		{"UnfollowRemovesExactlyOne", testUnfollowRemovesExactlyOne},
		{"FollowPurgeKeepsTheLiveOnes", testFollowPurgeKeepsTheLiveOnes},
		{"FollowRefusesAnIncompleteIdentity", testFollowRefusesAnIncompleteIdentity},
		{"OnlyOneRevisionIsActive", testOnlyOneRevisionIsActive},
		{"ActivatingAMissingRevisionChangesNothing", testActivatingAMissingRevisionChangesNothing},
		{"PayloadRoundTrips", testPayloadRoundTrips},
		{"RevisionsListInInsertionOrder", testRevisionsListInInsertionOrder},
		// NOT HERE: the token counter. It is fleet state now, certified
		// by coordtest against both coordination backends — a counter
		// this node kept privately was the whole defect (migration
		// 0011), so a suite for one would be certifying the bug.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newDB(t))
		})
	}
}

// base is the instant the seeded fixtures hang off: a day ago, truncated to
// the minute so a failure prints round numbers.
//
// Relative to now, not a calendar date, because the log has a 30-day read
// floor. A literal date is correct on the day it is written and then silently
// ages out of every query — the suite would keep compiling and start
// asserting that an empty page equals a full one.
var base = time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Minute)

func testSchema(t *testing.T, db *store.DB) {
	applied, err := db.AppliedMigrations(t.Context())
	if err != nil {
		t.Fatalf("AppliedMigrations: %v", err)
	}
	want := store.SchemaVersions()
	if len(want) == 0 {
		t.Fatal("no schema files embedded")
	}
	if !slices.Equal(applied, want) {
		t.Fatalf("applied %v, want %v", applied, want)
	}
}

// testSchemaIdempotent reopens the same file. A forward-only migrator that
// re-ran an applied file would fail on the second CREATE TABLE, so a clean
// reopen is the whole assertion.
func testSchemaIdempotent(t *testing.T, db *store.DB) {
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	again, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = again.Close() }()

	applied, err := again.AppliedMigrations(t.Context())
	if err != nil {
		t.Fatalf("AppliedMigrations: %v", err)
	}
	if !slices.Equal(applied, store.SchemaVersions()) {
		t.Fatalf("reopen changed the applied set: %v", applied)
	}
}

// testCapabilities asserts the probe is self-consistent: a capability reported
// present must actually work. Which capabilities SHOULD be present on which
// driver is the subject of capability_test.go — that is a claim about a pinned
// driver version, not a contract every driver must meet.
func testCapabilities(t *testing.T, db *store.DB) {
	caps := db.Caps()
	if !caps.VectorFunctions {
		t.Skip("driver reports no vector functions; there is nothing to hold it to")
	}
	var d float64
	err := db.SQL().QueryRowContext(t.Context(),
		`SELECT vector_distance_cos(vector32('[1,0,0,0]'), vector32('[1,0,0,0]'))`,
	).Scan(&d)
	if err != nil {
		t.Fatalf("probe reported VectorFunctions but the query fails: %v", err)
	}
	if d > 1e-6 {
		t.Fatalf("cosine distance of a vector to itself = %v, want ~0", d)
	}
}

func testAppendIdempotent(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	rec := store.EventRecord{
		ID: "dup", Type: "task_created", Source: "pm", Time: base,
		Category: "task", Summary: "first",
	}
	if err := log.Append(ctx, rec); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// A replay presents the same identity carrying a different summary.
	// First writer wins; the second is silently dropped, because a publish
	// retry is not an error to report.
	rec.Summary = "second"
	if err := log.Append(ctx, rec); err != nil {
		t.Fatalf("replayed append: %v", err)
	}

	page, err := log.List(ctx, store.ListQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("got %d rows, want 1 (replay must not duplicate)", len(page))
	}
	if page[0].Summary != "first" {
		t.Fatalf("summary %q, want the first writer's", page[0].Summary)
	}
}

// testAppendIncomplete: a zero identity is the one write SQL cannot refuse.
// NOT NULL catches a missing column, not a zero one, so both of these store
// happily and then read as nothing — a zero timestamp lands in year 1,
// permanently below the read floor, and an empty id collides with every other
// record that forgot the same field.
func testAppendIncomplete(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	for _, tc := range []struct {
		name string
		rec  store.EventRecord
	}{
		{"no id", store.EventRecord{Type: "task_created", Time: base}},
		{"no timestamp", store.EventRecord{ID: "x", Type: "task_created"}},
	} {
		if err := log.Append(ctx, tc.rec); !errors.Is(err, store.ErrIncompleteRecord) {
			t.Errorf("%s: got %v, want ErrIncompleteRecord", tc.name, err)
		}
	}
	page, err := log.List(ctx, store.ListQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page) != 0 {
		t.Fatalf("a refused write landed anyway: %v", ids(page))
	}
}

func testKeysetPaging(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	seed(t, log, 6)

	page, err := log.List(ctx, store.ListQuery{Limit: 3})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := ids(page); !slices.Equal(got, []string{"e05", "e04", "e03"}) {
		t.Fatalf("first page %v", got)
	}

	oldest := page[len(page)-1]
	next, err := log.List(ctx, store.ListQuery{
		Limit:  3,
		Before: &store.Cursor{Time: oldest.Time, ID: oldest.ID},
	})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if got := ids(next); !slices.Equal(got, []string{"e02", "e01", "e00"}) {
		t.Fatalf("second page %v", got)
	}
}

func testCursorExclusive(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	seed(t, log, 3)

	page, err := log.List(ctx, store.ListQuery{Limit: 3})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	oldest := page[len(page)-1]
	next, err := log.List(ctx, store.ListQuery{
		Before: &store.Cursor{Time: oldest.Time, ID: oldest.ID},
	})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if slices.Contains(ids(next), oldest.ID) {
		t.Fatalf("cursor row %q came back; the row a caller holds must not repeat", oldest.ID)
	}
}

// testIdenticalTimestamps is the reason the cursor carries an id at all. Burst
// writes share a timestamp at microsecond resolution, and a cursor over a
// non-unique key skips or repeats whatever collided with it — silently.
func testIdenticalTimestamps(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	for _, id := range []string{"a", "b", "c"} {
		write(t, log, store.EventRecord{
			ID: id, Type: "task_created", Source: "pm", Time: base,
			Category: "task", Summary: id,
		})
	}
	first, err := log.List(ctx, store.ListQuery{Limit: 1})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if first[0].ID != "c" {
		t.Fatalf("head is %q, want the highest id among equal timestamps", first[0].ID)
	}
	rest, err := log.List(ctx, store.ListQuery{
		Before: &store.Cursor{Time: base, ID: "c"},
	})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if got := ids(rest); !slices.Equal(got, []string{"b", "a"}) {
		t.Fatalf("page after the tie %v, want [b a]", got)
	}
}

// testOrderByTime: backfilled writes are real — a webhook replay, a gap
// re-read. Ordering by insertion puts them at the head, which under a cursor
// shows up as rows appearing above rows already scrolled past.
func testOrderByTime(t *testing.T, db *store.DB) {
	log := db.Events()
	for _, c := range []struct {
		id     string
		minute int
	}{{"late", 5}, {"early", 1}, {"middle", 3}} {
		write(t, log, store.EventRecord{
			ID: c.id, Type: "task_created", Source: "pm",
			Time:     base.Add(time.Duration(c.minute) * time.Minute),
			Category: "task", Summary: c.id,
		})
	}
	page, err := log.List(t.Context(), store.ListQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := ids(page); !slices.Equal(got, []string{"late", "middle", "early"}) {
		t.Fatalf("order %v", got)
	}
}

func testShortPage(t *testing.T, db *store.DB) {
	log := db.Events()
	seed(t, log, 2)
	page, err := log.List(t.Context(), store.ListQuery{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("got %d rows, want 2", len(page))
	}
}

func testFilters(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	write(t, log, store.EventRecord{
		ID: "t1", Type: "task_created", Source: "pm", Time: base,
		Category: "task", Actor: "alice", TraceID: "tr-1",
	})
	write(t, log, store.EventRecord{
		ID: "s1", Type: "llm_unavailable", Source: "engine",
		Time: base.Add(time.Minute), Category: "system", Actor: "bob",
		TraceID: "tr-2",
	})
	write(t, log, store.EventRecord{
		ID: "t2", Type: "task_created", Source: "pm",
		Time: base.Add(2 * time.Minute), Category: "task", Actor: "alice",
		TraceID: "tr-1",
	})

	for _, tc := range []struct {
		name string
		q    store.ListQuery
		want []string
	}{
		// The Activity view's category pills push their selection into
		// the query: filtering a page client-side silently excludes, and
		// a page holding 2 matches reads as "only 2 exist".
		{"category", store.ListQuery{Category: "task"}, []string{"t2", "t1"}},
		{"type", store.ListQuery{Type: "llm_unavailable"}, []string{"s1"}},
		{"source", store.ListQuery{Source: "engine"}, []string{"s1"}},
		{"actor", store.ListQuery{Actor: "alice"}, []string{"t2", "t1"}},
		{"trace", store.ListQuery{TraceID: "tr-1"}, []string{"t2", "t1"}},
	} {
		got, err := log.List(ctx, tc.q)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !slices.Equal(ids(got), tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, ids(got), tc.want)
		}
	}
}

// testRelatedAgent covers both halves: the direct match, and the trace sibling
// that carries the agent's name nowhere but caused its work.
func testRelatedAgent(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	write(t, log, store.EventRecord{
		ID: "trigger", Type: "external_notification", Source: "slack",
		Time: base, Category: "notification", Actor: "human",
		TraceID: "tr-1",
	})
	write(t, log, store.EventRecord{
		ID: "work", Type: "task_started", Source: "engine",
		Time: base.Add(time.Minute), Category: "task", Actor: "engineer",
		TraceID: "tr-1",
	})
	write(t, log, store.EventRecord{
		ID: "tagged", Type: "message_sent", Source: "engine",
		Time: base.Add(2 * time.Minute), Category: "communication",
		Actor: "someone-else", Tags: map[string]string{"recipient": "engineer"},
		TraceID: "tr-2",
	})
	write(t, log, store.EventRecord{
		ID: "unrelated", Type: "task_created", Source: "pm",
		Time: base.Add(3 * time.Minute), Category: "task", Actor: "other",
		TraceID: "tr-9",
	})

	got, err := log.List(ctx, store.ListQuery{RelatedAgent: "engineer"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	set := ids(got)
	for _, want := range []string{"work", "tagged", "trigger"} {
		if !slices.Contains(set, want) {
			t.Errorf("missing %q from %v", want, set)
		}
	}
	if slices.Contains(set, "unrelated") {
		t.Errorf("unrelated event leaked into %v", set)
	}
	if len(set) != len(slices.Compact(slices.Clone(set))) {
		t.Errorf("duplicate ids in %v", set)
	}
}

// testFailureFlag: one rule for failure, whichever surface asks.
func testFailureFlag(t *testing.T, db *store.DB) {
	log := db.Events()
	write(t, log, store.EventRecord{
		ID: "dead", Type: "agent_phase_completed", Source: "engine",
		Time: base, Category: "system", Tags: map[string]string{"failed": "true"},
	})
	write(t, log, store.EventRecord{
		ID: "typed", Type: "llm_unavailable", Source: "engine",
		Time: base.Add(time.Minute), Category: "system",
	})
	write(t, log, store.EventRecord{
		ID: "fine", Type: "agent_phase_completed", Source: "engine",
		Time: base.Add(2 * time.Minute), Category: "system",
	})

	page, err := log.List(t.Context(), store.ListQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := map[string]bool{"dead": true, "typed": true, "fine": false}
	for _, rec := range page {
		if rec.Failed != want[rec.ID] {
			t.Errorf("%s failed=%v, want %v", rec.ID, rec.Failed, want[rec.ID])
		}
	}
}

func testTrace(t *testing.T, db *store.DB) {
	log := db.Events()
	for _, c := range []struct {
		id     string
		minute int
	}{{"third", 3}, {"first", 1}, {"second", 2}} {
		write(t, log, store.EventRecord{
			ID: c.id, Type: "task_created", Source: "pm",
			Time:     base.Add(time.Duration(c.minute) * time.Minute),
			Category: "task", TraceID: "tr-1",
		})
	}
	got, err := log.Trace(t.Context(), "tr-1")
	if err != nil {
		t.Fatalf("trace: %v", err)
	}
	// A trace runs the other way from a feed: it is read as a causal
	// sequence, and the root is what explains it.
	if !slices.Equal(ids(got), []string{"first", "second", "third"}) {
		t.Fatalf("trace order %v", ids(got))
	}
}

// testTraceCap: a trace is unbounded in principle — a long turn with
// sub-agents accumulates thousands of spans — and the whole thing goes out in
// one WebSocket frame, so the read is capped. Which END the cap keeps is the
// part worth asserting: the root is what explains a trace, so a truncated tail
// is legible where a truncated head is not.
func testTraceCap(t *testing.T, db *store.DB) {
	log := db.Events()
	over := store.MaxTraceEvents + 10
	for i := range over {
		write(t, log, store.EventRecord{
			ID:       "s" + fourDigits(i),
			Type:     "task_created",
			Source:   "pm",
			Time:     base.Add(time.Duration(i) * time.Millisecond),
			Category: "task",
			TraceID:  "tr-long",
		})
	}
	got, err := log.Trace(t.Context(), "tr-long")
	if err != nil {
		t.Fatalf("trace: %v", err)
	}
	if len(got) != store.MaxTraceEvents {
		t.Fatalf("trace returned %d rows, want the cap %d", len(got), store.MaxTraceEvents)
	}
	if got[0].ID != "s"+fourDigits(0) {
		t.Fatalf("trace starts at %q; the cap must keep the ROOT, not the tail", got[0].ID)
	}
}

func testByID(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	write(t, log, store.EventRecord{
		ID: "one", Type: "task_created", Source: "pm", Time: base,
		Category: "task", Summary: "the one",
		Payload: json.RawMessage(`{"id":"one","detail":"kept"}`),
	})

	rec, err := log.ByID(ctx, "one")
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if rec.Summary != "the one" {
		t.Fatalf("summary %q", rec.Summary)
	}
	var body map[string]any
	if err = json.Unmarshal(rec.Payload, &body); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if body["detail"] != "kept" {
		t.Fatalf("payload lost its fields: %v", body)
	}

	// A listing never selects the payload column; that is what keeps the
	// feed cheap over thirty days of history.
	page, err := log.List(ctx, store.ListQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page[0].Payload) != 0 {
		t.Fatalf("listing carried a payload: %s", page[0].Payload)
	}

	if _, err := log.ByID(ctx, "absent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing id gave %v, want ErrNotFound", err)
	}
}

// testReadFloor: past the floor every page is empty forever. A UI that cannot
// name the floor draws that as "the org went quiet", which is why the floor is
// a named constant rather than a literal in two queries.
func testReadFloor(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	old := time.Now().UTC().Add(-store.EventHistory - time.Hour)
	write(t, log, store.EventRecord{
		ID: "ancient", Type: "task_created", Source: "pm", Time: old,
		Category: "task", TraceID: "tr-1",
	})
	write(t, log, store.EventRecord{
		ID: "recent", Type: "task_created", Source: "pm",
		Time: time.Now().UTC().Add(-time.Hour), Category: "task", TraceID: "tr-1",
	})

	page, err := log.List(ctx, store.ListQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !slices.Equal(ids(page), []string{"recent"}) {
		t.Fatalf("listing reached past the floor: %v", ids(page))
	}
	tr, err := log.Trace(ctx, "tr-1")
	if err != nil {
		t.Fatalf("trace: %v", err)
	}
	if !slices.Equal(ids(tr), []string{"recent"}) {
		t.Fatalf("trace reached past the floor: %v", ids(tr))
	}
	// The row is still THERE — unreadable, not deleted. That is exactly the
	// state the retention sweep exists to end.
	if _, err := log.ByID(ctx, "ancient"); err != nil {
		t.Fatalf("ByID on a row past the floor: %v", err)
	}
}

func testRetention(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	write(t, log, store.EventRecord{
		ID: "stale", Type: "task_created", Source: "pm",
		Time:     time.Now().UTC().Add(-store.EventRetention - time.Hour),
		Category: "task",
	})
	write(t, log, store.EventRecord{
		ID: "keep", Type: "task_created", Source: "pm",
		Time: time.Now().UTC().Add(-time.Hour), Category: "task",
	})

	n, err := log.Purge(ctx)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d rows, want 1", n)
	}
	if _, err := log.ByID(ctx, "stale"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("swept row still present: %v", err)
	}
	if _, err := log.ByID(ctx, "keep"); err != nil {
		t.Fatalf("sweep took a row inside retention: %v", err)
	}

	// The sweep must never reach a row a reader can still ask for.
	if store.EventRetention < store.EventHistory {
		t.Fatalf("retention %v is shorter than the read floor %v",
			store.EventRetention, store.EventHistory)
	}
}

// testRetentionBacklog: a purge over a backlog wider than one batch drains
// it completely and keeps what is inside retention. The batch bound exists
// so no single statement holds the writer for a whole overhang — but a loop
// that stopped after one batch would leave the tail in place and report a
// sweep that had not finished.
func testRetentionBacklog(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	stale := store.EventPurgeBatch + 3
	when := time.Now().UTC().Add(-store.EventRetention - time.Hour)
	for i := range stale {
		write(t, log, store.EventRecord{
			ID: fmt.Sprintf("stale-%04d", i), Type: "task_created", Source: "pm",
			Time: when.Add(time.Duration(i) * time.Millisecond), Category: "task",
		})
	}
	write(t, log, store.EventRecord{
		ID: "keep", Type: "task_created", Source: "pm",
		Time: time.Now().UTC().Add(-time.Hour), Category: "task",
	})

	n, err := log.Purge(ctx)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != int64(stale) {
		t.Fatalf("purged %d rows, want the whole %d-row backlog", n, stale)
	}
	if _, err := log.ByID(ctx, "keep"); err != nil {
		t.Fatalf("sweep took a row inside retention: %v", err)
	}
}

// testRelatedIndexed: the filter answers correctly, and the shape it answers
// through is one the engine can seek rather than scan.
//
// This is the one that used to hurt: matching lived in a JSON blob, so a
// QUIET seat in a busy org was the worst case — the reader walked the whole
// retention window in pages to conclude there was nothing to show.
//
// TWO ASSERTIONS OF DIFFERENT KINDS, and it is worth being straight about
// which is which. The List calls are a behavioural test of this package. The
// EXPLAIN is a TRIPWIRE on the schema and the engine's planner — it asks
// whether the party index is there and gets used, the way capability_test.go
// asks what the driver can do — and it re-states the join rather than
// borrowing List's own string, so a rewrite of List that quietly stopped
// joining would still pass here and be caught by the behavioural half plus
// the plan going unused. Timing is deliberately not asserted: on a fixture
// this small a scan is fast enough to pass.
func testRelatedIndexed(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	for i := range 200 {
		write(t, log, store.EventRecord{
			ID: fmt.Sprintf("noise-%03d", i), Type: "task_created", Source: "pm",
			Time: base.Add(time.Duration(i) * time.Second), Category: "task",
			Actor: "somebody-else",
		})
	}
	write(t, log, store.EventRecord{
		ID: "mine", Type: "task_created", Source: "pm",
		Time: base.Add(time.Hour), Category: "task",
		Tags: map[string]string{"recipient": "lead"},
	})

	got, err := log.List(ctx, store.ListQuery{RelatedAgent: "lead"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != "mine" {
		t.Fatalf("related page = %v, want just the matching event", ids(got))
	}

	// A seat with nothing to show must not read the log to find that out.
	quiet, err := log.List(ctx, store.ListQuery{RelatedAgent: "nobody"})
	if err != nil {
		t.Fatalf("quiet list: %v", err)
	}
	if len(quiet) != 0 {
		t.Fatalf("a seat with no events got %d rows", len(quiet))
	}

	var plan string
	rows, err := db.SQL().QueryContext(ctx, `EXPLAIN QUERY PLAN
		SELECT crewlet_events.event_id FROM crewlet_events
		JOIN crewlet_event_parties
		  ON crewlet_event_parties.event_time = crewlet_events.event_time
		 AND crewlet_event_parties.event_id = crewlet_events.event_id
		WHERE crewlet_event_parties.party = ?
		ORDER BY crewlet_events.event_time DESC LIMIT 50`, "lead")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("explain columns: %v", err)
	}
	for rows.Next() {
		cells := make([]any, len(cols))
		into := make([]any, len(cols))
		for i := range cells {
			into[i] = &cells[i]
		}
		if err := rows.Scan(into...); err != nil {
			t.Fatalf("explain scan: %v", err)
		}
		for _, cell := range cells {
			plan += fmt.Sprintf("%v ", cell)
		}
	}
	if strings.Contains(plan, "SCAN crewlet_events") {
		t.Fatalf("the related filter scans the log rather than seeking an index:\n%s", plan)
	}
	if !strings.Contains(plan, "crewlet_event_parties") {
		t.Fatalf("the party index is not used at all:\n%s", plan)
	}
}

// testRelatedSwept: the party index is purged with the log it indexes. Left
// unswept it grows for the life of the deployment while pointing at rows that
// no longer exist.
func testRelatedSwept(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	write(t, log, store.EventRecord{
		ID: "old", Type: "task_created", Source: "pm",
		Time:     time.Now().UTC().Add(-store.EventRetention - time.Hour),
		Category: "task", Actor: "lead",
	})
	write(t, log, store.EventRecord{
		ID: "new", Type: "task_created", Source: "pm",
		Time: time.Now().UTC().Add(-time.Hour), Category: "task", Actor: "lead",
	})

	if _, err := log.Purge(ctx); err != nil {
		t.Fatalf("purge: %v", err)
	}
	var orphans int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT count(*) FROM crewlet_event_parties p
		 WHERE NOT EXISTS (SELECT 1 FROM crewlet_events e
		   WHERE e.event_time = p.event_time AND e.event_id = p.event_id)`,
	).Scan(&orphans); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d party rows outlived their events", orphans)
	}
	// And the live one still resolves.
	got, err := log.List(ctx, store.ListQuery{RelatedAgent: "lead"})
	if err != nil {
		t.Fatalf("list after purge: %v", err)
	}
	if len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("after the sweep the filter returns %v", ids(got))
	}
}

// testSpendUncapped: the rollup folds EVERY phase completion in the window.
//
// It used to stop at twenty thousand rows, newest first, and say nothing —
// so a busy org's monthly spend was short by whatever fell past the cap, and
// an undercount reads exactly like an underspend. This writes more rows than
// that old ceiling and insists every one is counted.
func testSpendUncapped(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	const rows = 20001
	at := time.Now().UTC().Add(-time.Hour)
	for i := range rows {
		payload := []byte(`{"phase":"execute","model":"m","input_tokens":1,` +
			`"output_tokens":2,"total_tokens":3}`)
		write(t, log, store.EventRecord{
			ID:       fmt.Sprintf("spend-%05d", i),
			Type:     "agent_phase_completed",
			Source:   "agent",
			Time:     at.Add(time.Duration(i) * time.Millisecond),
			Category: "agent",
			Payload:  payload,
		})
	}

	got, err := log.PhaseTokens(ctx, store.PhaseTokenQuery{SinceDays: 1})
	if err != nil {
		t.Fatalf("phase tokens: %v", err)
	}
	if len(got) != rows {
		t.Fatalf("folded %d of %d phase completions — the window is being truncated",
			len(got), rows)
	}
	var total int
	for _, r := range got {
		total += r.TotalTokens
	}
	if want := rows * 3; total != want {
		t.Errorf("total tokens = %d, want %d", total, want)
	}
}

// testSpendDerived: a record built by hand, carrying only a payload, still
// records its spend. The write path derives it rather than trusting the
// caller — a phase completion stored with zero tokens is a company that
// reports having spent nothing.
func testSpendDerived(t *testing.T, db *store.DB) {
	log := db.Events()
	write(t, log, store.EventRecord{
		ID: "byhand", Type: "agent_phase_completed", Source: "agent",
		Time: time.Now().UTC().Add(-time.Minute), Category: "agent",
		Payload: []byte(`{"phase":"plan","provider_key":"anthropic",` +
			`"turn_id":"t1","iteration":2,"input_tokens":10,` +
			`"output_tokens":5,"total_tokens":15}`),
	})

	got, err := log.PhaseTokens(t.Context(), store.PhaseTokenQuery{SinceDays: 1})
	if err != nil {
		t.Fatalf("phase tokens: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	rec := got[0]
	if rec.TotalTokens != 15 || rec.InputTokens != 10 || rec.OutputTokens != 5 {
		t.Errorf("token counts lost: %+v", rec)
	}
	if rec.Phase != "plan" || rec.TurnID != "t1" || rec.Iteration != 2 {
		t.Errorf("call identity lost: %+v", rec)
	}
	// An entry naming no model is identified by the provider slot it ran
	// on, so a breakdown never groups real spend under an empty key.
	if rec.Model != "anthropic" {
		t.Errorf("model = %q, want the provider key as the fallback", rec.Model)
	}
}

// testBackup: the copy a backup produces is a database that OPENS and holds
// the rows the original held. The whole artifact is worthless if it cannot be
// read back, and this is the only place that is proven before the day it
// matters.
func testBackup(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	write(t, log, store.EventRecord{
		ID: "backed-up", Type: "task_created", Source: "pm",
		Time: base, Category: "task",
	})

	dest := filepath.Join(t.TempDir(), "copy.db")
	info, err := db.Backup(ctx, dest)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if info.Path != dest {
		t.Errorf("BackupInfo.Path = %q, want %q", info.Path, dest)
	}
	if info.Bytes <= 0 {
		t.Errorf("backup reports %d bytes", info.Bytes)
	}
	// The schema the copy carries is what a restore would bring back, so a
	// copy that claims a different one than this binary applied would
	// restore into a migration that runs from the wrong place.
	if !slices.Equal(info.Migrations, store.SchemaVersions()) {
		t.Errorf("copy carries schema %v, want %v", info.Migrations, store.SchemaVersions())
	}

	// Opened as a database in its own right — the restore path, exercised.
	restored, err := store.Open(ctx, dest, store.Options{})
	if err != nil {
		t.Fatalf("the copy will not open as a database: %v", err)
	}
	defer func() { _ = restored.Close() }()
	if _, err := restored.Events().ByID(ctx, "backed-up"); err != nil {
		t.Fatalf("the copy lost a row the original held: %v", err)
	}
}

// testBackupSelfContained: the copy is ONE file. A backup that silently
// depended on a -wal beside it would restore as an older database — or as a
// corrupt one — whenever an operator moved only the file they were told to.
func testBackupSelfContained(t *testing.T, db *store.DB) {
	write(t, db.Events(), store.EventRecord{
		ID: "solo", Type: "task_created", Source: "pm", Time: base, Category: "task",
	})
	dest := filepath.Join(t.TempDir(), "copy.db")
	if _, err := db.Backup(t.Context(), dest); err != nil {
		t.Fatalf("backup: %v", err)
	}
	for _, sidecar := range []string{"-wal", "-shm", "-tshm", ".part"} {
		if _, err := os.Stat(dest + sidecar); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the backup left %s beside it, so the artifact is not one file", dest+sidecar)
		}
	}
}

// testBackupOccupied: a destination that exists is refused, and the file in
// the way is left alone. It is by construction somebody's only copy of
// something.
func testBackupOccupied(t *testing.T, db *store.DB) {
	dest := filepath.Join(t.TempDir(), "taken.db")
	if err := os.WriteFile(dest, []byte("an earlier backup"), 0o600); err != nil {
		t.Fatalf("seed the destination: %v", err)
	}
	_, err := db.Backup(t.Context(), dest)
	if !errors.Is(err, store.ErrBackupExists) {
		t.Fatalf("backup onto an occupied path: %v, want ErrBackupExists", err)
	}
	body, readErr := os.ReadFile(dest)
	if readErr != nil || string(body) != "an earlier backup" {
		t.Fatalf("the refused backup disturbed what was already there: %q, %v", body, readErr)
	}
}

// testBackupUnderWrites: the copy is taken WITHOUT stopping the engine, so
// the case that matters is a database being written throughout. The copy must
// still open and verify — a point-in-time image, not a torn one.
func testBackupUnderWrites(t *testing.T, db *store.DB) {
	ctx := t.Context()
	log := db.Events()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			// Failures are not asserted on: the point is that the
			// backup does not break the writer's database, not that
			// every write in a hot loop commits.
			_ = log.Append(ctx, store.EventRecord{
				ID:   fmt.Sprintf("live-%04d", i),
				Type: "task_created", Source: "pm",
				Time: base.Add(time.Duration(i) * time.Millisecond), Category: "task",
			})
		}
	})

	dest := filepath.Join(t.TempDir(), "hot.db")
	_, err := db.Backup(ctx, dest)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("backup under concurrent writes: %v", err)
	}

	restored, err := store.Open(ctx, dest, store.Options{})
	if err != nil {
		t.Fatalf("a copy taken under writes will not open: %v", err)
	}
	if err := restored.Close(); err != nil {
		t.Fatalf("close the copy: %v", err)
	}
	// The original is still writable afterwards: a backup must not leave
	// the live database wedged behind a lock or a stale transaction.
	if err := log.Append(ctx, store.EventRecord{
		ID: "after", Type: "task_created", Source: "pm", Time: base, Category: "task",
	}); err != nil {
		t.Fatalf("the live database is unwritable after a backup: %v", err)
	}
}

// testRecordUntracked: a type absent from the category map is not stored. Two
// types are deliberately absent; every other absence is a bug, which is why
// the drop is a documented rule rather than a silent default.
func testRecordUntracked(t *testing.T, db *store.DB) {
	log := db.Events()
	if _, tracked := store.Category("agent_turn_progress"); tracked {
		t.Fatal("agent_turn_progress must stay out of the store: it is a live-only signal")
	}
	if _, tracked := store.Category("budget_reported"); tracked {
		t.Fatal("budget_reported must stay out of the store: it describes a dead process's meters")
	}
	if cat, tracked := store.Category("task_created"); !tracked || cat != "task" {
		t.Fatalf("task_created -> %q,%v", cat, tracked)
	}
	page, err := log.List(t.Context(), store.ListQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page) != 0 {
		t.Fatalf("empty log listed %d rows", len(page))
	}
}

// testWorkKeyNull proves the nullable-work_key design on the live driver: a
// PLAIN unique index over a nullable column gives the semantics a partial
// index (and an advisory lock) would otherwise be needed for, and a bare ON CONFLICT can target it.
func testWorkKeyNull(t *testing.T, db *store.DB) {
	ctx := t.Context()
	insert := func(id, handle, workKey string) error {
		_, err := db.SQL().ExecContext(ctx, `
			INSERT INTO episodes (
				id, agent_handle, agent_role, turn_id, started_at, ended_at,
				plan_summary, task_summary, review_outcome, duration_ms, work_key
			) VALUES (?, ?, 'engineer', ?, ?, ?, '', '', 'done', 0, ?)
			ON CONFLICT (agent_handle, work_key) DO NOTHING`,
			id, handle, id, store.EncodeTime(base), store.EncodeTime(base),
			store.NullText(workKey))
		return err
	}
	count := func(handle string) int {
		var n int
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM episodes WHERE agent_handle = ?`, handle,
		).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	// An empty work key means "legitimately unconstrained" — a scheduled
	// fire, a sub-agent, a sandbox resume. NULLs are distinct, so those
	// rows all land.
	for _, id := range []string{"u1", "u2", "u3"} {
		if err := insert(id, "engineer", ""); err != nil {
			t.Fatalf("unconstrained insert %s: %v", id, err)
		}
	}
	if got := count("engineer"); got != 3 {
		t.Fatalf("unconstrained episodes collapsed: %d rows, want 3", got)
	}

	// A real work key collapses the duplicate two nodes produce for one
	// trigger, which is the whole reason the key exists.
	if err := insert("k1", "engineer", "trigger-a"); err != nil {
		t.Fatalf("keyed insert: %v", err)
	}
	if err := insert("k2", "engineer", "trigger-a"); err != nil {
		t.Fatalf("duplicate keyed insert must not error: %v", err)
	}
	if got := count("engineer"); got != 4 {
		t.Fatalf("duplicate work key landed: %d rows, want 4", got)
	}

	// Per agent, not global: two seats legitimately act on one trigger.
	if err := insert("k3", "reviewer", "trigger-a"); err != nil {
		t.Fatalf("second seat, same trigger: %v", err)
	}
	if got := count("reviewer"); got != 1 {
		t.Fatalf("a second seat's episode was refused: %d rows, want 1", got)
	}
}

// --- helpers --------------------------------------------------------------

func seed(t *testing.T, log *store.EventLog, n int) {
	t.Helper()
	for i := range n {
		write(t, log, store.EventRecord{
			ID:       "e" + twoDigits(i),
			Type:     "task_created",
			Source:   "pm",
			Time:     base.Add(time.Duration(i) * time.Minute),
			Category: "task",
			Summary:  "event " + twoDigits(i),
		})
	}
}

func write(t *testing.T, log *store.EventLog, rec store.EventRecord) {
	t.Helper()
	if err := log.Append(t.Context(), rec); err != nil {
		t.Fatalf("append %s: %v", rec.ID, err)
	}
}

func ids(recs []store.EventRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	return out
}

// twoDigits and fourDigits keep seeded ids lexicographically ordered, so the
// id tiebreak agrees with the time ordering and a paging failure shows up as a
// wrong sequence rather than an arbitrary one.
func twoDigits(i int) string  { return zeroPad(i, 2) }
func fourDigits(i int) string { return zeroPad(i, 4) }

func zeroPad(i, width int) string {
	out := make([]byte, width)
	for p := width - 1; p >= 0; p-- {
		out[p] = byte('0' + i%10)
		i /= 10
	}
	return string(out)
}
