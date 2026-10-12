package eventfan_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// custodyTurn is a stateless node's turn as a custody batch carries it: a
// phase, a failure and a completion of turn `t-stateless`, all on one trace.
func custodyTurn(at time.Time) store.CustodyBatch {
	tags := map[string]string{"turn_id": "t-stateless", "agent_role": "Lead"}
	return store.CustodyBatch{ID: "batch-1", Origin: "seats-1", Records: []store.EventRecord{
		{ID: "x-phase", Type: "agent_phase_completed", Category: "task", TraceID: "tr-1",
			Time: at.Add(-3 * time.Minute), Tags: tags, Payload: json.RawMessage(`{}`),
			Spend: &store.Spend{Phase: "execute", Model: "m", TurnID: "t-stateless",
				InputTokens: 40, OutputTokens: 7, TotalTokens: 47}},
		{ID: "x-down", Type: "llm_unavailable", Category: "system", TraceID: "tr-1",
			Time: at.Add(-2 * time.Minute), Tags: tags, Payload: json.RawMessage(`{}`)},
		{ID: "x-done", Type: "turn_completed", Category: "task", TraceID: "tr-1",
			Time: at.Add(-time.Minute), Tags: tags,
			Payload: json.RawMessage(`{"turn_id":"t-stateless","duration_ms":25}`)},
	}}
}

// EVERY COUNT HOLDS A CUSTODY ROW TWO DATA NODES HOLD ONCE, at every step of
// its settling — the axis's bars, totals, failed share and facets, a trace's
// and a turn's total, a page of turns' sums and the spend window's records.
//
// A node without `data` hands its events to the data nodes in custody batches,
// and a batch whose claim failed is written by a second keeper before the first
// has learned it is not its own: until the first settles it, both logs hold
// the same rows. Added up as two parts, every one of those counts held each
// such row once per node holding it. So each node counts what it keeps and
// names what it holds unsettled, and the asker asks every node which named
// rows it keeps and counts each once — while both copies are unsettled, once
// the keeper has settled and the other has not, and once both have — asked
// from either node. Each node also holds rows of its own, of the same trace
// and turn, which are counted once each by being written once.
//
// Mutation: count a node's unsettled rows in its own counts as well as naming
// them ([store.EventHistogram.Count] at +0, the store's -1 dropped), or skip
// the second question in the fleet's settle, and each count is one batch too
// many in some state; add a named row once per node naming it, and the state
// with both copies unsettled counts it twice.
func TestEveryCountHoldsACustodyRowTwoDataNodesHoldOnce(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, b := newNode(t, broker, "node-a"), newNode(t, broker, "node-b")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour).Add(30 * time.Minute)
	// EACH NODE'S OWN HALF of the stateless node's turn: a phase on the trace.
	for i, n := range []node{a, b} {
		appendTo(t, n, store.EventRecord{
			ID: n.id + "-phase", Type: "agent_phase_completed", Category: "task", TraceID: "tr-1",
			Time: at.Add(-time.Duration(10+i) * time.Minute),
			Tags: map[string]string{"turn_id": "t-stateless", "agent_role": "Lead"},
			Spend: &store.Spend{Phase: "execute", Model: "m", TurnID: "t-stateless",
				InputTokens: 1, TotalTokens: 1},
		})
	}
	batch := custodyTurn(at)
	for _, n := range []node{a, b} {
		if err := n.log.WriteCustody(t.Context(), batch, at); err != nil {
			t.Fatal(err)
		}
	}

	check := func(state string) {
		t.Helper()
		for _, self := range []node{a, b} {
			fan := fanFrom(self, "node-a", "node-b")
			fan.Clock = func() time.Time { return at }
			complete := func(what string, c eventfan.Coverage) {
				t.Helper()
				if !c.Complete {
					t.Fatalf("%s, %s asked from %s: coverage %+v", state, what, self.id, c)
				}
			}

			axis, c, err := fan.Histogram(t.Context(), store.HistogramQuery{Bucket: store.BucketHour,
				ListQuery: store.ListQuery{Since: at.Add(-2 * time.Hour)}})
			if err != nil {
				t.Fatal(err)
			}
			complete("the axis", c)
			bars := 0
			for _, bar := range axis.Bars {
				bars += bar.Count
			}
			if want := map[string]int{"task": 4, "system": 1}; axis.Total != 5 || bars != 5 ||
				axis.Failed != 1 || !maps.Equal(axis.ByCategory, want) || len(axis.Unsettled) != 0 {
				t.Errorf("%s, asked from %s: the axis counts %d (bars %d), %d failed, facets %v, "+
					"naming %d — want each node's phase and the batch's three rows once: 5, one "+
					"failed, facets %v, naming none", state, self.id, axis.Total, bars, axis.Failed,
					axis.ByCategory, len(axis.Unsettled), want)
			}
			// NARROWED TO A CATEGORY, the bars count that category alone and
			// the facets still count every one.
			tasks, c, err := fan.Histogram(t.Context(), store.HistogramQuery{Bucket: store.BucketHour,
				ListQuery: store.ListQuery{Since: at.Add(-2 * time.Hour), Category: "task"}})
			if err != nil {
				t.Fatal(err)
			}
			complete("the task axis", c)
			if tasks.Total != 4 || tasks.Failed != 0 || tasks.ByCategory["system"] != 1 {
				t.Errorf("%s, asked from %s: the task axis counts %d, %d failed, facets %v — want "+
					"4, none failed, and the system facet's one row", state, self.id, tasks.Total,
					tasks.Failed, tasks.ByCategory)
			}

			trace, c, err := fan.Trace(t.Context(), "tr-1")
			if err != nil {
				t.Fatal(err)
			}
			complete("the trace", c)
			if trace.Total != 5 || len(trace.Rows) != 5 {
				t.Errorf("%s, asked from %s: the trace shows %d rows of %d, want 5 of 5 — a total "+
					"past its rows reads as a view cut short", state, self.id, len(trace.Rows), trace.Total)
			}

			turn, c, err := fan.Turn(t.Context(), "t-stateless")
			if err != nil {
				t.Fatal(err)
			}
			complete("the turn", c)
			if turn.Total != 5 || len(turn.Rows) != 5 {
				t.Errorf("%s, asked from %s: the turn shows %d rows of %d, want 5 of 5", state,
					self.id, len(turn.Rows), turn.Total)
			}

			page, c, err := fan.Turns(t.Context(), store.TurnQuery{SinceDays: 1})
			if err != nil {
				t.Fatal(err)
			}
			complete("the page of turns", c)
			if len(page.Turns) != 1 {
				t.Fatalf("%s, asked from %s: the page lists %d turns, want 1", state, self.id, len(page.Turns))
			}
			if got := page.Turns[0]; got.TotalTokens != 49 || got.InputTokens != 42 ||
				got.OutputTokens != 7 || got.Phases != 3 || got.DurationMS != 25 || !got.Failed {
				t.Errorf("%s, asked from %s: the turn sums %d tokens (%d in, %d out), %d phases, "+
					"%d ms — want 49 (42, 7), 3 and 25: each node's phase and the batch's once",
					state, self.id, got.TotalTokens, got.InputTokens, got.OutputTokens, got.Phases,
					got.DurationMS)
			}

			spend, c, err := fan.PhaseTokens(t.Context(), store.PhaseTokenQuery{SinceDays: 1})
			if err != nil {
				t.Fatal(err)
			}
			complete("the spend window", c)
			total := 0
			for _, r := range spend {
				total += r.TotalTokens
			}
			if len(spend) != 3 || total != 49 {
				t.Errorf("%s, asked from %s: the spend window holds %d records of %d tokens, want "+
					"3 of 49", state, self.id, len(spend), total)
			}
		}
	}
	check("both copies unsettled")
	if err := b.log.SettleCustody(t.Context(), batch.ID, true); err != nil {
		t.Fatal(err)
	}
	check("node-b kept it, node-a not yet settled")
	if err := a.log.SettleCustody(t.Context(), batch.ID, false); err != nil {
		t.Fatal(err)
	}
	check("node-b kept it, node-a let it go")
}

// A NODE THAT ANSWERS ONLY THE SECOND QUESTION SAYS NOTHING ABOUT WHAT WAS
// COUNTED.
//
// The second question asks which named rows each node keeps, and a row a node
// keeps and did not name is one its count already holds — of a node whose
// count the merge holds. A node that refused the first question counted
// nothing the answer holds, so its keeping a row is no evidence the row was
// counted: read as such, a row only it keeps and some answering node named is
// in no count at all.
//
// Mutation: read the second question's answer from every node that gave one,
// and the batch's rows are in neither the turn's sums nor the outcome counts.
func TestANodeThatAnswersOnlyTheSecondQuestionCountsNothing(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a := newNode(t, broker, "node-a")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour).Add(30 * time.Minute)
	batch := custodyTurn(at)
	batch.Records = append(batch.Records, store.EventRecord{ID: "x-skip",
		Type: "notification_skipped", Category: "notification", Time: at.Add(-time.Minute),
		Payload: json.RawMessage(`{}`), Tags: map[string]string{"notification_source": "gitlab"}})
	if err := a.log.WriteCustody(t.Context(), batch, at); err != nil {
		t.Fatal(err)
	}
	// node-x refuses every count and keeps every row it is asked about.
	q := client(t, broker)
	stop, err := q.Serve(t.Context(), eventfan.Subject, func(_ context.Context, raw []byte) ([]byte, error) {
		var req struct {
			Version  int             `json:"version"`
			Question string          `json:"question"`
			Params   json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.Question != string(eventfan.QuestionKept) {
			return json.Marshal(map[string]any{"version": req.Version, "node": "node-x",
				"error": fmt.Sprintf("node-x does not answer %s", req.Question)})
		}
		return json.Marshal(map[string]any{"version": req.Version, "node": "node-x", "answer": req.Params})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })

	fan := fanFrom(a, "node-a", "node-x")
	fan.Clock = func() time.Time { return at }
	page, c, err := fan.Turns(t.Context(), store.TurnQuery{SinceDays: 1})
	if err != nil {
		t.Fatal(err)
	}
	if c.Complete || !missing(c, "node-x", "does not answer") {
		t.Errorf("the page's coverage %+v, want node-x named for the question it refused", c)
	}
	if len(page.Turns) != 1 || page.Turns[0].TotalTokens != 47 || page.Turns[0].Phases != 1 {
		t.Errorf("the page is %+v, want the turn node-a holds with its 47 tokens and its phase — "+
			"node-x keeping its rows says nothing about a count the answer does not hold", page.Turns)
	}
	outcomes, c, err := fan.NotificationOutcomes(t.Context(), store.OutcomeQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Complete || outcomes.Skipped["gitlab"] != 1 {
		t.Errorf("the outcome counts are %v (coverage %+v), want node-a's one drop and node-x named",
			outcomes.Skipped, c)
	}
}
