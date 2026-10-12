package store_test

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// unsettledBatch is a stateless node's rows as a custody batch carries them:
// a phase of turn `t-x` with tokens, a failure, a completion with its
// duration and an unrelated row of another category — all on trace `tr-x`.
func unsettledBatch(at time.Time) store.CustodyBatch {
	tags := map[string]string{"turn_id": "t-x", "agent_role": "Lead"}
	return store.CustodyBatch{ID: "batch-x", Origin: "seats-1", Records: []store.EventRecord{
		{ID: "x-phase", Type: "agent_phase_completed", Category: "task", TraceID: "tr-x",
			Time: at.Add(-3 * time.Minute), Tags: tags, Payload: json.RawMessage(`{}`),
			Spend: &store.Spend{Phase: "execute", Model: "m", TurnID: "t-x", InputTokens: 40,
				OutputTokens: 7, TotalTokens: 47, CacheReadTokens: 5, CacheWriteTokens: 3}},
		{ID: "x-down", Type: "llm_unavailable", Category: "system", TraceID: "tr-x",
			Time: at.Add(-2 * time.Minute), Tags: tags, Payload: json.RawMessage(`{}`)},
		{ID: "x-done", Type: "turn_completed", Category: "task", TraceID: "tr-x",
			Time: at.Add(-time.Minute), Tags: tags,
			Payload: json.RawMessage(`{"turn_id":"t-x","duration_ms":25}`)},
		{ID: "x-other", Type: "inbound_webhook", Category: "integration",
			Time: at.Add(-90 * time.Second), Payload: json.RawMessage(`{}`)},
	}}
}

// heldAndNamed opens a log holding rows of its own and the batch unsettled.
func heldAndNamed(t *testing.T, at time.Time) *store.EventLog {
	t.Helper()
	log := open(t).Events()
	for _, rec := range []store.EventRecord{
		{ID: "own-phase", Type: "agent_phase_completed", Category: "task", TraceID: "tr-x",
			Time: at.Add(-10 * time.Minute), Tags: map[string]string{"turn_id": "t-x", "agent_role": "Lead"},
			Payload: json.RawMessage(`{}`),
			Spend:   &store.Spend{Phase: "execute", Model: "m", TurnID: "t-x", InputTokens: 1, TotalTokens: 1}},
		{ID: "own-other", Type: "inbound_webhook", Category: "integration",
			Time: at.Add(-20 * time.Minute), Payload: json.RawMessage(`{}`)},
	} {
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.WriteCustody(t.Context(), unsettledBatch(at), at); err != nil {
		t.Fatal(err)
	}
	return log
}

// THE AXIS TAKES OUT WHAT IT HOLDS UNSETTLED, AND ADDED BACK IT IS THE AXIS OF
// EVERY ROW HELD — bar for bar, failed share for failed share, facet for facet.
//
// A node's part of the axis counts the rows it keeps and names the rows of a
// custody batch it has written and not settled, for whoever sums the fleet to
// count once. The two counts it holds differ in exactly what a named row has to
// say for itself: the bars are cut over the snapped window and narrowed by the
// category, and the facets over the window that was asked for, with the
// category lifted. So each named row, counted back in by
// [store.EventHistogram.Count], must land where each statement put it — and the
// part with every name added back is the part the same log answers once the
// batch is settled as its own. Asked over a window whose bottom edge cuts
// inside a bucket, so a row the bars count lies outside the facets' window,
// and narrowed to a category, so a row the facets count is in no bar.
//
// Mutation: count a named row in its facet whatever its instant, or in a bar
// whatever its category, or count it failed whatever it carries, and one of the
// three axes is off by that row once added back.
func TestTheAxisNamesWhatItHoldsUnsettled(t *testing.T) {
	t.Parallel()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour).Add(30 * time.Minute)
	for name, q := range map[string]store.HistogramQuery{
		"the whole window": {Bucket: store.BucketHour,
			ListQuery: store.ListQuery{Since: at.Add(-2 * time.Hour), At: at}},
		"a bottom edge inside the bucket": {Bucket: store.BucketHour,
			ListQuery: store.ListQuery{Since: at.Add(-150 * time.Second), At: at}},
		"one category": {Bucket: store.BucketMinute,
			ListQuery: store.ListQuery{Since: at.Add(-time.Hour), Until: at, Category: "task", At: at}},
		"failures only": {Bucket: store.BucketHour,
			ListQuery: store.ListQuery{Failed: new(true), At: at}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log := heldAndNamed(t, at)
			kept, err := log.Histogram(t.Context(), q)
			if err != nil {
				t.Fatal(err)
			}
			if err := log.SettleCustody(t.Context(), "batch-x", true); err != nil {
				t.Fatal(err)
			}
			held, err := log.Histogram(t.Context(), q)
			if err != nil {
				t.Fatal(err)
			}
			if len(held.Unsettled) != 0 {
				t.Fatalf("once settled the axis names %d rows, want none", len(held.Unsettled))
			}
			// WHAT IT KEEPS IS WHAT A LOG THAT LET THE BATCH GO COUNTS — the
			// statements' own answer, which no rule for a named row shares.
			released := heldAndNamed(t, at)
			if err := released.SettleCustody(t.Context(), "batch-x", false); err != nil {
				t.Fatal(err)
			}
			without, err := released.Histogram(t.Context(), q)
			if err != nil {
				t.Fatal(err)
			}
			named := kept.Unsettled
			kept.Unsettled = nil
			if !reflect.DeepEqual(kept, without) {
				t.Errorf("unsettled, the axis is\n  %+v\nwant the axis of the rows it keeps\n  %+v",
					kept, without)
			}
			kept.Unsettled = named
			for _, r := range kept.Unsettled {
				kept.Count(q, r, 1)
			}
			kept.Unsettled = nil
			if !reflect.DeepEqual(kept, held) {
				t.Errorf("counted back in, the axis is\n  %+v\nwant the axis of every row held\n  %+v",
					kept, held)
			}
		})
	}
}

// A TURN'S SHARE TAKES OUT WHAT IT HOLDS UNSETTLED, from its sums alone, and
// added back it is the share of every row held.
//
// The share's sums — phases, tokens, the cache's share of them, the duration —
// are what a fleet adds across nodes, so they count the rows the node keeps,
// and the rows of a custody batch in flight are named with what each sum adds;
// every other aggregate is a minimum, a maximum or a union, which a row held
// twice cannot move, and keeps every row held. Which turns the page lists is
// read in the same snapshot.
//
// Mutation: drop a sum from [store.TurnPartial.Add], or take the named rows out
// of the whole partial rather than its sums, and the share counted back in
// differs from the one the settled log answers.
func TestATurnsShareNamesWhatItHoldsUnsettled(t *testing.T) {
	t.Parallel()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	log := heldAndNamed(t, at)
	q := store.TurnQuery{IDs: []string{"t-x"}, SinceDays: 1, At: at}
	kept, err := log.TurnShares(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept.Partials) != 1 || kept.Partials[0].TotalTokens != 1 || kept.Partials[0].Phases != 1 ||
		kept.Partials[0].DurationMS != 0 {
		t.Fatalf("unsettled, the share is %+v — want this node's own phase alone in its sums", kept.Partials)
	}
	if !slices.Equal(kept.Listed, []string{"t-x"}) {
		t.Errorf("the share says its page lists %v, want t-x", kept.Listed)
	}
	if ids := rowIDs(kept.Unsettled); !slices.Equal(ids, []string{"x-done", "x-down", "x-phase"}) {
		t.Errorf("the share names %v, want the turn's three batch rows", ids)
	}
	if err := log.SettleCustody(t.Context(), "batch-x", true); err != nil {
		t.Fatal(err)
	}
	held, err := log.TurnShares(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	// THE SUMS ARE A LOG'S THAT LET THE BATCH GO, and every other aggregate
	// the settled log's.
	released := heldAndNamed(t, at)
	if err := released.SettleCustody(t.Context(), "batch-x", false); err != nil {
		t.Fatal(err)
	}
	without, err := released.TurnShares(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	sums := func(p store.TurnPartial) [7]int {
		return [7]int{p.Phases, p.InputTokens, p.OutputTokens, p.TotalTokens, p.CacheRead,
			p.CacheWrite, p.DurationMS}
	}
	if len(without.Partials) != 1 || sums(kept.Partials[0]) != sums(without.Partials[0]) {
		t.Errorf("unsettled, the share sums %v, want the sums of the rows it keeps %v",
			sums(kept.Partials[0]), without.Partials)
	}
	if a, b := kept.Partials[0], held.Partials[0]; !a.StartedAt.Equal(b.StartedAt) ||
		!a.EndedAt.Equal(b.EndedAt) || a.Failed != b.Failed || !reflect.DeepEqual(a.LastEnded, b.LastEnded) {
		t.Errorf("unsettled, the share's span and state are %+v, want every row held's %+v", a, b)
	}
	for _, r := range kept.Unsettled {
		kept.Partials[0].Add(r, 1)
	}
	if !reflect.DeepEqual(kept.Partials, held.Partials) || len(held.Unsettled) != 0 {
		t.Errorf("counted back in, the share is\n  %+v\nwant the share of every row held\n  %+v",
			kept.Partials, held.Partials)
	}
}

// A SNAPSHOT NAMES A TRACE'S AND A TURN'S UNSETTLED ROWS, inside the history
// at its instant and of that trace or turn alone — what a count of either's
// extent leaves to the asker.
//
// Mutation: drop the id's term from [store.EventSnapshot.Unsettled]'s
// predicate, and the trace names the batch's unrelated row.
func TestASnapshotNamesATracesAndATurnsUnsettledRows(t *testing.T) {
	t.Parallel()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	log := heldAndNamed(t, at)
	got := map[string][]string{}
	if err := log.Snapshot(t.Context(), func(s store.EventSnapshot) error {
		for column, id := range map[string]string{"trace_id": "tr-x", "turn_id": "t-x"} {
			rows, err := s.Unsettled(t.Context(), column, id, at)
			if err != nil {
				return err
			}
			got[column] = rowIDs(rows)
		}
		_, err := s.Unsettled(t.Context(), "agent_id", "a", at)
		if err == nil {
			t.Error("a column that is not an extent's was read")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"trace_id": {"x-done", "x-down", "x-phase"},
		"turn_id": {"x-done", "x-down", "x-phase"}}
	if !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("named %v, want %v", got, want)
	}
	// AND NOTHING UNDER THE HISTORY: asked a month and more later, the
	// batch's rows lie under the horizon.
	if err := log.Snapshot(t.Context(), func(s store.EventSnapshot) error {
		rows, err := s.Unsettled(t.Context(), "trace_id", "tr-x", at.Add(store.EventHistory+time.Hour))
		if err == nil && len(rows) != 0 {
			t.Errorf("past the horizon the trace names %v", rowIDs(rows))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
