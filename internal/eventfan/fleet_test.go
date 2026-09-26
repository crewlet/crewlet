package eventfan_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/eventfan/eventfantest"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// THE SUITE, on the in-memory twin. Its own package runs it on a three-member
// cluster.
func TestTheScatterOnTheInMemoryBroker(t *testing.T) {
	t.Parallel()
	eventfantest.Run(t, func(t *testing.T, n int) []queue.EventQueue {
		broker := memory.NewBroker()
		out := make([]queue.EventQueue, 0, n)
		for range n {
			out = append(out, client(t, broker))
		}
		return out
	})
}

func client(t *testing.T, b *memory.Broker) queue.EventQueue {
	t.Helper()
	q := b.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start a queue client: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	return q
}

// node is one member with a store and an answerer.
type node struct {
	id  string
	q   queue.EventQueue
	log *store.EventLog
}

func newNode(t *testing.T, b *memory.Broker, id string) node {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), id+".db"), store.Options{})
	if err != nil {
		t.Fatalf("open %s: %v", id, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	n := node{id: id, q: client(t, b), log: db.Events()}
	stop, err := eventfan.Serve(t.Context(), n.q, id, n.log)
	if err != nil {
		t.Fatalf("serve %s: %v", id, err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
	return n
}

func fanFrom(self node, roster ...string) *eventfan.Fleet {
	return &eventfan.Fleet{
		Self: self.id, Local: self.log, Queue: self.q,
		Roster: func(context.Context) ([]string, error) { return roster, nil },
		Budget: 5 * time.Second,
	}
}

func appendTo(t *testing.T, n node, rec store.EventRecord) {
	t.Helper()
	if rec.Payload == nil {
		rec.Payload = json.RawMessage(`{}`)
	}
	if err := n.log.Append(t.Context(), rec); err != nil {
		t.Fatalf("%s: append %s: %v", n.id, rec.ID, err)
	}
}

func phaseOn(t *testing.T, n node, id, turn string, at time.Time, tokens int, model string) {
	t.Helper()
	appendTo(t, n, store.EventRecord{
		ID: id, Type: "agent_phase_completed", Category: "task", Time: at,
		Tags: map[string]string{"turn_id": turn, "agent_role": "Lead"},
		Spend: &store.Spend{Phase: "execute", Model: model, TurnID: turn,
			TotalTokens: tokens, InputTokens: tokens},
	})
}

func completionOn(t *testing.T, n node, id, turn string, at time.Time) {
	t.Helper()
	appendTo(t, n, store.EventRecord{
		ID: id, Type: "turn_completed", Category: "task", Time: at,
		Tags:    map[string]string{"turn_id": turn, "agent_role": "Lead"},
		Payload: json.RawMessage(fmt.Sprintf(`{"turn_id":%q,"duration_ms":10}`, turn)),
	})
}

// A SPLIT TURN IS WHOLE AFTER THE SECOND SCATTER.
//
// The node a turn is SELECTED on is not always the only node that holds it: a
// turn narrowed by its model is listed only by the node whose phase used the
// model, while its completion — the record that says it ended — sits on the
// node it resumed on after a move. The first scatter finds the turn; only the
// second, asking every node for its share of exactly the turns found, makes it
// whole.
//
// Mutation: fold the first scatter's partials and skip the second, and the turn
// comes back unfinished.
func TestASplitTurnIsWholeAfterTheSecondScatter(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, b := newNode(t, broker, "node-a"), newNode(t, broker, "node-b")
	at := time.Now().UTC().Add(-time.Hour)
	phaseOn(t, a, "p1", "t-1", at, 30, "model-cheap")
	completionOn(t, b, "c1", "t-1", at.Add(time.Minute))

	page, coverage, err := fanFrom(a, "node-a", "node-b").Turns(t.Context(),
		store.TurnQuery{Model: "model-cheap"})
	if err != nil {
		t.Fatal(err)
	}
	if !coverage.Complete {
		t.Fatalf("coverage %+v", coverage)
	}
	if len(page.Turns) != 1 {
		t.Fatalf("%d turns, want the one", len(page.Turns))
	}
	got := page.Turns[0]
	if !got.Complete || got.DurationMS != 10 || got.TotalTokens != 30 || got.Phases != 1 {
		t.Fatalf("the turn came back %+v — its completion lives on the node that did "+
			"not select it, and the second scatter is what brings it", got)
	}
}

// A SILENT NODE IS NAMED, and the rest of the fleet still answers.
func TestASilentNodeIsNamed(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, b := newNode(t, broker, "node-a"), newNode(t, broker, "node-b")
	at := time.Now().UTC().Add(-time.Minute)
	appendTo(t, a, store.EventRecord{ID: "on-a", Type: "x", Category: "task", Time: at})
	appendTo(t, b, store.EventRecord{ID: "on-b", Type: "x", Category: "task", Time: at.Add(time.Second)})

	fan := fanFrom(a, "node-a", "node-b", "node-gone")
	fan.Budget = 300 * time.Millisecond
	listing, coverage, err := fan.List(t.Context(), store.ListQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Complete {
		t.Fatal("an answer missing a live node called itself complete")
	}
	if missing := coverage.Missing(); !slices.Equal(missing, []string{"node-gone"}) {
		t.Fatalf("missing %v, want node-gone named — a short answer that does not say "+
			"so reads exactly like a quiet company", missing)
	}
	for _, n := range coverage.Nodes {
		if n.ID == "node-gone" && !strings.Contains(n.Error, "budget") {
			t.Errorf("node-gone's reason is %q, want the budget it did not answer inside", n.Error)
		}
	}
	if got := len(listing.Rows); got != 2 {
		t.Errorf("%d rows, want both answering nodes' rows", got)
	}
}

// AN UNREADABLE REPLY COUNTS AS MISSING — named when it names itself, and
// silent-and-therefore-missing when it names nobody.
func TestAnUnreadableReplyCountsAsMissing(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a := newNode(t, broker, "node-a")
	for _, answer := range []string{
		`{"version":99,"node":"node-newer"}`,
		`not json at all`,
		`{"version":1,"node":"node-garbled","answer":"not a page"}`,
	} {
		q := client(t, broker)
		stop, err := q.Serve(t.Context(), eventfan.Subject,
			func(context.Context, []byte) ([]byte, error) { return []byte(answer), nil })
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
	}
	appendTo(t, a, store.EventRecord{ID: "on-a", Type: "x", Category: "task",
		Time: time.Now().UTC().Add(-time.Minute)})

	fan := fanFrom(a, "node-a", "node-newer", "node-garbled", "node-mute")
	fan.Budget = 300 * time.Millisecond
	listing, coverage, err := fan.List(t.Context(), store.ListQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, n := range coverage.Nodes {
		if !n.Answered {
			reasons[n.ID] = n.Error
		}
	}
	if len(reasons) != 3 || coverage.Complete {
		t.Fatalf("not answered: %v — want node-newer, node-garbled and node-mute, and "+
			"an incomplete answer", reasons)
	}
	if !strings.Contains(reasons["node-newer"], "v99") {
		t.Errorf("node-newer's reason %q does not say it speaks another protocol", reasons["node-newer"])
	}
	if !strings.Contains(reasons["node-garbled"], "could not be read") {
		t.Errorf("node-garbled's reason %q does not say its answer was unreadable", reasons["node-garbled"])
	}
	if len(listing.Rows) != 1 {
		t.Errorf("%d rows, want the asker's own", len(listing.Rows))
	}
}

// A TOKEN-SORTED PAGE RANKS BY THE MERGED TOTAL.
//
// A turn split across two nodes is two small halves to either node's own
// ranking, and the costliest turn in the fleet to its sum. The page ranks each
// node's top candidates by what the whole turn spent, so the split turn is not
// out-ranked by a single-node turn smaller than it.
func TestATokenSortedPageRanksByTheMergedTotal(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, b := newNode(t, broker, "node-a"), newNode(t, broker, "node-b")
	at := time.Now().UTC().Add(-time.Hour)
	phaseOn(t, a, "u1", "whole", at, 100, "m")
	phaseOn(t, a, "s1", "split", at.Add(time.Minute), 60, "m")
	phaseOn(t, b, "s2", "split", at.Add(2*time.Minute), 50, "m")

	page, _, err := fanFrom(a, "node-a", "node-b").Turns(t.Context(),
		store.TurnQuery{Sort: store.TurnSortTokens, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 1 || page.Turns[0].TurnID != "split" || page.Turns[0].TotalTokens != 110 {
		t.Fatalf("the costliest turn is %+v, want `split` at 60+50 = 110 over `whole` at 100",
			page.Turns)
	}
	if page.Next != nil {
		t.Error("a ranking offered a cursor")
	}
	if _, _, err := fanFrom(a, "node-a").Turns(t.Context(), store.TurnQuery{
		Sort: store.TurnSortTokens, Before: at,
	}); err == nil {
		t.Error("a cursor on a ranking was accepted")
	}
}

// A NODE ALONE ANSWERS COMPLETE, and never touches the broker.
func TestASoloFleetIsItsOwnStoreAndComplete(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a := newNode(t, broker, "node-a")
	appendTo(t, a, store.EventRecord{ID: "e", Type: "x", Category: "task",
		Time: time.Now().UTC().Add(-time.Minute)})
	listing, coverage, err := eventfan.Solo("node-a", a.log).List(t.Context(), store.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !coverage.Complete || len(coverage.Nodes) != 1 || len(listing.Rows) != 1 {
		t.Fatalf("solo answered %d rows with coverage %+v", len(listing.Rows), coverage)
	}
}

// THE ALARM'S INPUT IS REPORTED for every answer, partial or not.
func TestEveryAnswerIsReportedWithItsCoverage(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a := newNode(t, broker, "node-a")
	var got []eventfan.Question
	var partial int
	fan := fanFrom(a, "node-a", "node-gone")
	fan.Budget = 100 * time.Millisecond
	fan.Report = func(q eventfan.Question, c eventfan.Coverage, _ time.Duration) {
		got = append(got, q)
		if !c.Complete {
			partial++
		}
	}
	if _, _, err := fan.List(t.Context(), store.ListQuery{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fan.Turns(t.Context(), store.TurnQuery{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []eventfan.Question{eventfan.QuestionEvents, eventfan.QuestionTurns}) || partial != 2 {
		t.Fatalf("reported %v with %d partial, want one report per answer, both partial", got, partial)
	}
}

// ONE NODE RANKS ITS OWN TURNS BY TOKENS IN SQL, before any merge: a node's
// candidates for a ranked page are its costliest, not its newest.
//
// Mutation: order the store's page by start alone and the older, costlier turn
// is not the one this node offers.
func TestAStoreRanksItsOwnCandidatesByTokens(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a := newNode(t, broker, "node-a")
	at := time.Now().UTC().Add(-time.Hour)
	phaseOn(t, a, "old", "costly", at, 100, "m")
	phaseOn(t, a, "new", "cheap", at.Add(time.Minute), 10, "m")
	page, _, err := eventfan.Solo("node-a", a.log).Turns(t.Context(),
		store.TurnQuery{Sort: store.TurnSortTokens, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 1 || page.Turns[0].TurnID != "costly" {
		t.Fatalf("the costliest turn is %+v, want `costly`", page.Turns)
	}
}

// THE SPEND A RESTARTED NODE SEEDS ITS ROLLUP FROM IS THE FLEET'S.
//
// Each phase is written to the store of the node that published it, so a
// rollup seeded from one store showed a restarted node only its own share of
// the company's day. The window is the asker's, and the cut is the newest
// `limit` across every node rather than each node's own.
//
// Mutation: answer PhaseTokens from the local store alone and node-b's newer
// phase is missing.
func TestPhaseTokensAreEveryNodesSpendNewestFirst(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a := newNode(t, broker, "node-a")
	b := newNode(t, broker, "node-b")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	phaseOn(t, a, "pa-1", "t-1", at, 10, "m")
	phaseOn(t, b, "pb-1", "t-2", at.Add(time.Minute), 20, "m")
	phaseOn(t, a, "pa-2", "t-3", at.Add(2*time.Minute), 30, "m")
	// Outside the window asked for: never answered, by anybody.
	phaseOn(t, b, "pb-old", "t-4", at.Add(-48*time.Hour), 99, "m")

	records, coverage, err := fanFrom(a, "node-a", "node-b").PhaseTokens(t.Context(),
		store.PhaseTokenQuery{Since: at.Add(-time.Hour), Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !coverage.Complete || len(coverage.Nodes) != 2 {
		t.Fatalf("coverage %+v, want both nodes", coverage)
	}
	var got []string
	for _, r := range records {
		got = append(got, r.EventID)
	}
	if !slices.Equal(got, []string{"pa-2", "pb-1"}) {
		t.Fatalf("records %v, want the fleet's newest two, pa-2 then pb-1", got)
	}
}

// servesAsV1 stands a peer on the broker that behaves as a build speaking only
// history protocol v1 does: it refuses any other version by name, and answers a
// v1 listing with its own row — IGNORING every filter it does not know, which is
// exactly what an older build does with a field it cannot read.
func servesAsV1(t *testing.T, b *memory.Broker, node string, row store.EventRecord) {
	t.Helper()
	q := client(t, b)
	stop, err := q.Serve(t.Context(), eventfan.Subject, func(_ context.Context, raw []byte) ([]byte, error) {
		var req struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.Version != 1 {
			return json.Marshal(map[string]any{"version": 1, "node": node, "error": fmt.Sprintf(
				"this node speaks history protocol v1 and was asked in v%d", req.Version)})
		}
		answer, _ := json.Marshal(map[string]any{"rows": []store.EventRecord{row}, "full": false})
		return json.Marshal(map[string]any{"version": 1, "node": node, "answer": json.RawMessage(answer)})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
}

// A FILTER AN OLDER PEER CANNOT APPLY IS NOT ANSWERED AROUND.
//
// An older build ignores a parameter it does not know, so a channel or seat
// filter scattered to it comes back as its UNFILTERED rows, merged in as though
// they matched. So a request is stamped with the lowest version that answers
// it: a listing narrowing by nothing new still goes out as v1 and the older
// peer's rows are part of it, while one narrowing by a v2 filter goes out as v2,
// the older peer refuses by version, and the coverage names it rather than the
// page carrying its unmatched row. The histogram is always v2, because its
// failed split is a field a v1 peer never sends and a sum would read as zero.
//
// Mutation: stamp every request with [eventfan.Protocol], and the plain listing
// loses the older peer; stamp them all v1, and its row lands on the channel's
// page.
func TestAFilterAnOlderPeerCannotApplyIsNotAnsweredAround(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a := newNode(t, broker, "node-a")
	at := time.Now().UTC().Add(-time.Minute)
	appendTo(t, a, store.EventRecord{ID: "on-channel", Type: "a2a_asked", Category: "task",
		Time: at, Tags: map[string]string{"channel_id": "ch-1"}})
	servesAsV1(t, broker, "node-old", store.EventRecord{ID: "old-unrelated", Type: "x",
		Category: "task", Time: at.Add(time.Second)})
	fan := fanFrom(a, "node-a", "node-old")

	plain, coverage, err := fan.List(t.Context(), store.ListQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !coverage.Complete || len(plain.Rows) != 2 {
		t.Errorf("a listing with no new filter: %v, coverage %+v — want both nodes' rows "+
			"and the older peer answering", idsOf(plain.Rows), coverage)
	}

	narrowed, coverage, err := fan.List(t.Context(), store.ListQuery{ChannelID: "ch-1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(narrowed.Rows); !slices.Equal(got, []string{"on-channel"}) {
		t.Errorf("the channel's page = %v, want only the row on it — an older peer's "+
			"unfiltered row must not be merged in as a match", got)
	}
	if coverage.Complete || !missing(coverage, "node-old", "v2") {
		t.Errorf("coverage %+v does not name node-old as unable to answer v2", coverage)
	}

	_, coverage, err = fan.Histogram(t.Context(), store.HistogramQuery{Bucket: store.BucketHour})
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Complete || !missing(coverage, "node-old", "v2") {
		t.Errorf("the axis's coverage %+v does not name node-old — its bars carry no "+
			"failed split and would under-count", coverage)
	}
}

// A HISTOGRAM'S FAILED SPLIT IS EVERY NODE'S, summed bar by bar.
func TestTheFailedSplitIsSummedAcrossNodes(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, b := newNode(t, broker, "node-a"), newNode(t, broker, "node-b")
	at := time.Now().UTC().Add(-time.Minute)
	appendTo(t, a, store.EventRecord{ID: "a-fail", Type: "x", Category: "task", Time: at,
		Tags: map[string]string{"failed": "true"}})
	appendTo(t, b, store.EventRecord{ID: "b-fail", Type: "budget_exhausted", Category: "task", Time: at})
	appendTo(t, b, store.EventRecord{ID: "b-ok", Type: "x", Category: "task", Time: at})

	got, coverage, err := fanFrom(a, "node-a", "node-b").Histogram(t.Context(),
		store.HistogramQuery{Bucket: store.BucketHour})
	if err != nil {
		t.Fatal(err)
	}
	if !coverage.Complete {
		t.Fatalf("coverage %+v", coverage)
	}
	if got.Total != 3 || got.Failed != 2 {
		t.Errorf("total %d with %d failed, want 3 with 2 — each node's failures summed", got.Total, got.Failed)
	}
	last := got.Bars[len(got.Bars)-1]
	if last.Count != 3 || last.Failed != 2 {
		t.Errorf("the current bar is %d with %d failed, want 3 with 2", last.Count, last.Failed)
	}
}

// missing reports whether a node is named as not answering, for a reason
// mentioning want.
func missing(c eventfan.Coverage, node, want string) bool {
	for _, n := range c.Nodes {
		if n.ID == node && !n.Answered && strings.Contains(n.Error, want) {
			return true
		}
	}
	return false
}

func idsOf(rows []store.EventRecord) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}
