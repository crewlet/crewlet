package eventfan_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
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
	db, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), id+".db"), store.Options{})
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

// A PAGE OF TURNS IS HELD TO ITS WINDOW WHOLE, across nodes. A turn that began
// on one node before the window and resumed on another inside it is listed by
// the second node — its half starts in the window — and only the fold of both
// halves says it did not. And a window in the past is bounded above: a bar
// three days ago is answered with that bar's turns, not the fleet's newest.
//
// Mutation: drop the fleet's check of the merged start against the window, and
// the resumed turn is listed; stop carrying `until` on the wire, and the bar's
// page is today's.
func TestAPageOfTurnsIsHeldToItsWindowAcrossNodes(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, b := newNode(t, broker, "node-a"), newNode(t, broker, "node-b")
	now := time.Now().UTC()
	since := now.Add(-time.Hour)
	phaseOn(t, a, "early", "resumed", since.Add(-time.Minute), 10, "m")
	phaseOn(t, b, "late", "resumed", since.Add(time.Minute), 10, "m")
	completionOn(t, b, "late-done", "resumed", since.Add(2*time.Minute))
	phaseOn(t, b, "own", "inside", since.Add(5*time.Minute), 10, "m")
	bar := now.Add(-72 * time.Hour).Truncate(time.Hour)
	phaseOn(t, a, "past", "in-the-bar", bar.Add(10*time.Minute), 10, "m")

	fan := fanFrom(a, "node-a", "node-b")
	page, coverage, err := fan.Turns(t.Context(), store.TurnQuery{Since: since})
	if err != nil {
		t.Fatal(err)
	}
	if !coverage.Complete {
		t.Fatalf("coverage %+v", coverage)
	}
	if ids := turnIDs(page); !slices.Equal(ids, []string{"inside"}) {
		t.Errorf("the last hour listed %v, want only the turn that began in it", ids)
	}
	page, _, err = fan.Turns(t.Context(), store.TurnQuery{Since: bar, Until: bar.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if ids := turnIDs(page); !slices.Equal(ids, []string{"in-the-bar"}) {
		t.Errorf("a bar three days ago listed %v, want its own turn", ids)
	}
}

func turnIDs(page eventfan.TurnPage) []string {
	out := []string{}
	for _, t := range page.Turns {
		out = append(out, t.TurnID)
	}
	return out
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
		fmt.Sprintf(`{"version":%d,"node":"node-garbled","answer":"not a page"}`, eventfan.Protocol),
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
		Sort: store.TurnSortTokens, Before: &store.TurnCursor{Start: at, TurnID: "t"},
	}); err == nil {
		t.Error("a cursor on a ranking was accepted")
	}
}

// THE LAST PAGE CARRIES NO CURSOR. Every non-empty page used to carry one, so
// the walk never said it had ended: a seat's profile offered "older" under
// its last turn and wrote its count as a floor for ever. And a page that
// merely FILLED was read as one with more behind it, so a history exactly a
// page long still offered "older" onto nothing — each node's read now asks one
// row past its page and says.
//
// Mutation: read a node's part as full when its page filled (the old
// `len >= limit`), and the exactly-a-page cases get a cursor.
func TestOnlyAPageWithMoreBehindItCarriesACursor(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, b := newNode(t, broker, "node-a"), newNode(t, broker, "node-b")
	at := time.Now().UTC().Add(-time.Hour)
	phaseOn(t, a, "a1", "t1", at, 10, "m")
	phaseOn(t, a, "a2", "t2", at.Add(time.Minute), 10, "m")
	phaseOn(t, b, "b1", "t3", at.Add(2*time.Minute), 10, "m")

	for name, fan := range map[string]*eventfan.Fleet{
		"solo":   eventfan.Solo("node-a", a.log),
		"fanned": fanFrom(a, "node-a", "node-b"),
	} {
		whole, _, err := fan.Turns(t.Context(), store.TurnQuery{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(whole.Turns) == 0 || whole.Next != nil {
			t.Errorf("%s: %d turns and cursor %v on a page holding every turn, want no cursor",
				name, len(whole.Turns), whole.Next)
		}
		cut, _, err := fan.Turns(t.Context(), store.TurnQuery{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(cut.Turns) != 1 || cut.Next == nil {
			t.Fatalf("%s: a page cut at one turn of several offered no cursor", name)
		}
		rest, _, err := fan.Turns(t.Context(), store.TurnQuery{Limit: 10, Before: cut.Next})
		if err != nil {
			t.Fatal(err)
		}
		if len(rest.Turns) == 0 || rest.Next != nil {
			t.Errorf("%s: the page after the cursor holds %d turns and cursor %v, want the rest and none",
				name, len(rest.Turns), rest.Next)
		}
		// EXACTLY A PAGE IS NOT MORE THAN A PAGE. Older than node-b's turn,
		// node-a holds exactly two and nobody holds anything else: a page of
		// two FILLS, and a filled page was read as one with more behind it —
		// a cursor onto an empty page.
		exact, _, err := fan.Turns(t.Context(), store.TurnQuery{Limit: 2,
			Before: &store.TurnCursor{Start: at.Add(2 * time.Minute)}})
		if err != nil {
			t.Fatal(err)
		}
		if len(exact.Turns) != 2 || exact.Next != nil {
			t.Errorf("%s: a page of exactly the two turns there are holds %d with cursor %v, want 2 and none",
				name, len(exact.Turns), exact.Next)
		}
		// AND THE PHASE LISTINGS, which read their `more` the same way.
		below := &store.Cursor{Time: at.Add(2 * time.Minute), ID: "b1"}
		phases, _, err := fan.Phases(t.Context(), "", 2, below)
		if err != nil {
			t.Fatal(err)
		}
		if len(phases.Rows) != 2 || phases.More {
			t.Errorf("%s: a phase page of exactly the two there are holds %d with more=%v, want 2 and false",
				name, len(phases.Rows), phases.More)
		}
		if shorter, _, err := fan.Phases(t.Context(), "", 1, below); err != nil || !shorter.More {
			t.Errorf("%s: a phase page of one of two says more=%v (err %v), want true", name, shorter.More, err)
		}
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

// servesAs stands a peer on the broker that speaks history protocol `version`:
// it refuses a request in any other version by name, as every node does, and
// answers one in its own with its row — whatever the question, which is what a
// node that cannot read this protocol's fields would be doing if it answered.
func servesAs(t *testing.T, b *memory.Broker, node string, version int, row store.EventRecord) {
	t.Helper()
	q := client(t, b)
	stop, err := q.Serve(t.Context(), eventfan.Subject, func(_ context.Context, raw []byte) ([]byte, error) {
		var req struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.Version != version {
			return json.Marshal(map[string]any{"version": version, "node": node, "error": fmt.Sprintf(
				"this node speaks history protocol v%d and was asked in v%d", version, req.Version)})
		}
		answer, _ := json.Marshal(map[string]any{"rows": []store.EventRecord{row}, "full": false})
		return json.Marshal(map[string]any{"version": req.Version, "node": node, "answer": json.RawMessage(answer)})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
}

// A PEER ON ANOTHER PROTOCOL IS NAMED ON EVERY QUESTION, and nothing of it is
// merged.
//
// Every question carries fields only this protocol defines — the asker's
// instant above all, which every node floors the history at, and the filters
// and counts each version added — so a node speaking any other version is never
// asked to answer around them: every question goes out in [eventfan.Protocol],
// the peer refuses it by version, and the coverage names it in its own words
// while the rest of the fleet answers as usual. Its row is never merged in as
// though it matched a filter it could not read, or counted under a horizon it
// never floored at.
//
// Mutation: stamp any question with a version but [eventfan.Protocol], and the
// peer's row is merged into that answer with the coverage reading complete.
func TestAPeerOnAnotherProtocolIsNamedOnEveryQuestion(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a := newNode(t, broker, "node-a")
	at := time.Now().UTC().Add(-time.Minute)
	appendTo(t, a, store.EventRecord{ID: "mine", Type: "agent_phase_completed", Category: "agent",
		Time: at, TraceID: "tr-1", Actor: "Lead",
		Tags: map[string]string{"turn_id": "t-1", "agent_role": "Lead", "agent_id": "agent-a",
			"channel_id": "ch-1"}})
	servesAs(t, broker, "node-old", eventfan.Protocol-1, store.EventRecord{ID: "theirs",
		Type: "agent_phase_completed", Category: "agent", Time: at.Add(time.Second), TraceID: "tr-1",
		Tags: map[string]string{"turn_id": "t-1"}})
	fan := fanFrom(a, "node-a", "node-old")
	asked := fmt.Sprintf("v%d", eventfan.Protocol)
	named := func(t *testing.T, c eventfan.Coverage) {
		t.Helper()
		if c.Complete || !missing(c, "node-old", asked) {
			t.Errorf("coverage %+v does not name node-old as refusing %s", c, asked)
		}
	}
	mineOnly := func(t *testing.T, rows []store.EventRecord) {
		t.Helper()
		if got := idsOf(rows); !slices.Equal(got, []string{"mine"}) {
			t.Errorf("rows %v, want this node's alone — nothing of a peer that refused is merged", got)
		}
	}
	t.Run("events", func(t *testing.T) {
		for _, q := range []store.ListQuery{{Limit: 10}, {ChannelID: "ch-1", Limit: 10}} {
			got, c, err := fan.List(t.Context(), q)
			if err != nil {
				t.Fatal(err)
			}
			mineOnly(t, got.Rows)
			named(t, c)
		}
	})
	t.Run("event_series", func(t *testing.T) {
		got, c, err := fan.Histogram(t.Context(), store.HistogramQuery{Bucket: store.BucketHour})
		if err != nil {
			t.Fatal(err)
		}
		if got.Total != 1 {
			t.Errorf("the axis counts %d, want this node's one row", got.Total)
		}
		named(t, c)
	})
	t.Run("event", func(t *testing.T) {
		_, c, err := fan.ByID(t.Context(), "mine")
		if err != nil {
			t.Fatal(err)
		}
		named(t, c)
	})
	t.Run("trace", func(t *testing.T) {
		got, c, err := fan.Trace(t.Context(), "tr-1")
		if err != nil {
			t.Fatal(err)
		}
		mineOnly(t, got.Rows)
		named(t, c)
	})
	t.Run("turn", func(t *testing.T) {
		got, c, err := fan.Turn(t.Context(), "t-1")
		if err != nil {
			t.Fatal(err)
		}
		mineOnly(t, got.Rows)
		named(t, c)
	})
	t.Run("phases", func(t *testing.T) {
		for _, seat := range []string{"", "agent-a"} {
			got, c, err := fan.Phases(t.Context(), seat, 10, nil)
			if err != nil {
				t.Fatal(err)
			}
			mineOnly(t, got.Rows)
			named(t, c)
		}
		got, c, err := fan.SeatPhases(t.Context(), "", "Lead", nil)
		if err != nil {
			t.Fatal(err)
		}
		mineOnly(t, got.Rows)
		named(t, c)
	})
	t.Run("turns", func(t *testing.T) {
		_, c, err := fan.Turns(t.Context(), store.TurnQuery{SinceDays: 1})
		if err != nil {
			t.Fatal(err)
		}
		named(t, c)
	})
	t.Run("phase_tokens", func(t *testing.T) {
		_, c, err := fan.PhaseTokens(t.Context(), store.PhaseTokenQuery{SinceDays: 1})
		if err != nil {
			t.Fatal(err)
		}
		named(t, c)
	})
	t.Run("notification_outcomes", func(t *testing.T) {
		_, c, err := fan.NotificationOutcomes(t.Context(), store.OutcomeQuery{Since: at.Add(-time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		named(t, c)
	})
}

// A NODE ANSWERS ONLY ITS OWN PROTOCOL. An asker on another build sends a
// question without the fields this one floors and counts by, or with fields it
// does not know; answered, its merge would read rows this node floored where it
// never asked, or fields it cannot read. So the node refuses, naming the
// version it speaks and the one it was asked in — older and newer alike.
//
// Mutation: answer a request in an older version, and the asker on it is
// handed a page.
func TestANodeAnswersOnlyItsOwnProtocol(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	b := newNode(t, broker, "node-b")
	at := time.Now().UTC().Add(-time.Minute)
	appendTo(t, b, store.EventRecord{ID: "on-b", Type: "x", Category: "task", Time: at})
	params, err := json.Marshal(map[string]any{"limit": 10, "at": at})
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{eventfan.Protocol - 1, eventfan.Protocol + 1} {
		req, err := json.Marshal(map[string]any{"version": version, "asker": "node-x",
			"question": "events", "params": json.RawMessage(params)})
		if err != nil {
			t.Fatal(err)
		}
		replies, err := client(t, broker).Ask(t.Context(), eventfan.Subject, req, 1)
		if err != nil || len(replies) != 1 {
			t.Fatalf("asked node-b in v%d: %d replies, %v", version, len(replies), err)
		}
		var reply struct {
			Version int             `json:"version"`
			Error   string          `json:"error"`
			Answer  json.RawMessage `json:"answer"`
		}
		if err := json.Unmarshal(replies[0], &reply); err != nil {
			t.Fatal(err)
		}
		if len(reply.Answer) != 0 || reply.Version != eventfan.Protocol ||
			!strings.Contains(reply.Error, fmt.Sprintf("v%d", eventfan.Protocol)) ||
			!strings.Contains(reply.Error, fmt.Sprintf("v%d", version)) {
			t.Errorf("asked in v%d, node-b replied v%d with %q and answer %s — want a refusal "+
				"naming both versions", version, reply.Version, reply.Error, reply.Answer)
		}
	}
}

// A HISTOGRAM'S FAILED SPLIT IS EVERY NODE'S, summed bar by bar.
func TestTheFailedSplitIsSummedAcrossNodes(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, b := newNode(t, broker, "node-a"), newNode(t, broker, "node-b")
	// The window is cut at a PINNED instant half way through an hour that
	// has already ended, and the events sit a minute before it, so both are
	// in one bar whatever the wall clock says. Cut against now with the
	// events a minute back, the case failed whenever it ran in the first
	// minute of an hour: the events fell in the previous bar and the
	// current one was empty.
	cut := time.Now().UTC().Truncate(time.Hour).Add(-30 * time.Minute)
	at := cut.Add(-time.Minute)
	appendTo(t, a, store.EventRecord{ID: "a-fail", Type: "x", Category: "task", Time: at,
		Tags: map[string]string{"failed": "true"}})
	appendTo(t, b, store.EventRecord{ID: "b-fail", Type: "budget_exhausted", Category: "task", Time: at})
	appendTo(t, b, store.EventRecord{ID: "b-ok", Type: "x", Category: "task", Time: at})

	got, coverage, err := fanFrom(a, "node-a", "node-b").Histogram(t.Context(),
		store.HistogramQuery{Bucket: store.BucketHour, At: cut})
	if err != nil {
		t.Fatal(err)
	}
	if !coverage.Complete {
		t.Fatalf("coverage %+v", coverage)
	}
	if got.Total != 3 || got.Failed != 2 {
		t.Errorf("total %d with %d failed, want 3 with 2 — each node's failures summed", got.Total, got.Failed)
	}
	// THE BAR OF THE HOUR THEY WERE WRITTEN IN, never simply the newest:
	// written a minute ago, they are in the PREVIOUS hour's bar for the
	// first minute of every hour.
	hour := at.Truncate(time.Hour).Format(time.RFC3339)
	i := slices.IndexFunc(got.Bars, func(b store.EventBar) bool { return b.At == hour })
	if i < 0 {
		t.Fatalf("no bar starts at %s, the hour the events were written in", hour)
	}
	if bar := got.Bars[i]; bar.Count != 3 || bar.Failed != 2 {
		t.Errorf("the bar of %s is %d with %d failed, want 3 with 2", hour, bar.Count, bar.Failed)
	}
}

// THE AXIS IS SUMMED OVER EVERY NODE AND SHOWN FROM ITS FIRST WHOLE BAR.
//
// The default window, any ask that names no `since`, starts at the history
// floor, which lies mid-bucket whenever the instant does. Every node cuts it
// down to the bucket the floor falls in, partial first bar and all, so the
// parts sum bar for bar; the asker drops the partial bar from the sum, so the
// axis shown begins at the first whole bucket inside the history, and its chips
// still reach the floor on every node.
//
// Mutation: begin a clipped window at the first whole bucket on one node, and
// its part is refused and named; drop nothing after the sum, and the axis
// begins with the partial bar.
func TestTheAxisIsSummedOverEveryNodeAndShownFromItsFirstWholeBar(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, b := newNode(t, broker, "node-a"), newNode(t, broker, "node-b")
	// HALF PAST AN HOUR, so the floor is half past too.
	at := time.Now().UTC().Truncate(time.Hour).Add(-90 * time.Minute)
	floor := at.Add(-store.EventHistory)
	first := store.BucketHour.HistoryStart(at)
	if first.Equal(floor) {
		t.Fatalf("the floor %s is on an hour boundary; the case needs it inside one", floor)
	}
	appendTo(t, a, store.EventRecord{ID: "a-partial", Type: "x", Category: "task", Time: floor.Add(10 * time.Minute)})
	appendTo(t, a, store.EventRecord{ID: "a-first", Type: "x", Category: "task", Time: first.Add(5 * time.Minute)})
	appendTo(t, b, store.EventRecord{ID: "b-partial", Type: "x", Category: "task", Time: floor.Add(15 * time.Minute)})
	appendTo(t, b, store.EventRecord{ID: "b-first", Type: "x", Category: "task", Time: first.Add(20 * time.Minute)})
	appendTo(t, b, store.EventRecord{ID: "b-recent", Type: "x", Category: "system", Time: at.Add(-10 * time.Minute)})
	fan := fanFrom(a, "node-a", "node-b")
	fan.Clock = func() time.Time { return at }

	got, coverage, err := fan.Histogram(t.Context(), store.HistogramQuery{Bucket: store.BucketHour})
	if err != nil {
		t.Fatal(err)
	}
	if !coverage.Complete {
		t.Fatalf("coverage %+v — a node's part was not summed", coverage)
	}
	if got.Since != first.Format(time.RFC3339) {
		t.Fatalf("since = %s, want the first whole hour inside the history, %s", got.Since, first)
	}
	if bar := got.Bars[0]; bar.At != got.Since || bar.Count != 2 {
		t.Errorf("the first bar is %s counting %d, want both nodes' row in it", bar.At, bar.Count)
	}
	if got.Total != 3 {
		t.Errorf("total = %d, want the three rows inside whole bars, from both nodes", got.Total)
	}
	if got.ByCategory["task"] != 4 || got.ByCategory["system"] != 1 {
		t.Errorf("by_category = %v, want task 4 and system 1 — the chips reach the floor on "+
			"both nodes", got.ByCategory)
	}
}

// EVERY NODE FLOORS A PINNED WINDOW AT THE ASKER'S INSTANT.
//
// The axis, the page of turns and the spend window are each cut on the asker's
// clock before anybody is asked, and the instant travels with the question —
// so by the time a peer reads, it is in that peer's past. Each peer floored
// its rows at its OWN clock's history horizon instead, so what it held between
// the asker's horizon and its own was missing from bars, a turn and a window
// the merged answer said it covered. Node-b's rows here sit a minute above the
// pinned horizon: below its clock's, and still on disk, since retention keeps
// a day past the floor.
//
// Mutation: floor any of the three reads in the store at `now()` rather than
// the query's instant, or leave `at` off the turns or the spend question's
// wire parameters, and node-b's rows drop out of that answer.
func TestEveryNodeFloorsAPinnedWindowAtTheAskersInstant(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, b := newNode(t, broker, "node-a"), newNode(t, broker, "node-b")
	// AN HOUR BOUNDARY one to two hours back, so the horizon under it is one
	// too and the axis's first bar begins exactly there.
	at := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	horizon := at.Add(-store.EventHistory)
	phaseOn(t, b, "b-phase", "t-edge", horizon.Add(time.Minute), 30, "m")
	completionOn(t, b, "b-done", "t-edge", horizon.Add(2*time.Minute))
	appendTo(t, b, store.EventRecord{ID: "b-failed", Type: "x", Category: "system",
		Time: horizon.Add(3 * time.Minute), Tags: map[string]string{"failed": "true"}})
	fan := fanFrom(a, "node-a", "node-b")

	t.Run("event_series", func(t *testing.T) {
		got, coverage, err := fan.Histogram(t.Context(), store.HistogramQuery{Bucket: store.BucketHour, At: at})
		if err != nil {
			t.Fatal(err)
		}
		if !coverage.Complete {
			t.Fatalf("coverage %+v", coverage)
		}
		if got.Since != horizon.Format(time.RFC3339) {
			t.Fatalf("since = %s, want the horizon under the pinned instant, %s", got.Since, horizon)
		}
		if first := got.Bars[0]; first.Count != 3 || first.Failed != 1 {
			t.Errorf("the first bar is %d with %d failed, want node-b's 3 with 1", first.Count, first.Failed)
		}
		if got.Total != 3 || got.Failed != 1 || got.ByCategory["task"] != 2 || got.ByCategory["system"] != 1 {
			t.Errorf("total %d, failed %d, by_category %v — want node-b's three rows in every count",
				got.Total, got.Failed, got.ByCategory)
		}
	})
	t.Run("turns", func(t *testing.T) {
		page, coverage, err := fan.Turns(t.Context(), store.TurnQuery{SinceDays: store.MaxTurnDays, At: at})
		if err != nil {
			t.Fatal(err)
		}
		if !coverage.Complete {
			t.Fatalf("coverage %+v", coverage)
		}
		if len(page.Turns) != 1 || !page.Turns[0].Complete || page.Turns[0].TotalTokens != 30 {
			t.Errorf("the page is %+v, want node-b's whole turn", page.Turns)
		}
	})
	t.Run("phase_tokens", func(t *testing.T) {
		records, coverage, err := fan.PhaseTokens(t.Context(),
			store.PhaseTokenQuery{SinceDays: store.MaxPhaseTokenDays, At: at})
		if err != nil {
			t.Fatal(err)
		}
		if !coverage.Complete {
			t.Fatalf("coverage %+v", coverage)
		}
		if len(records) != 1 || records[0].EventID != "b-phase" {
			t.Errorf("records %+v, want node-b's phase", records)
		}
	})
}

// EVERY QUESTION IS ASKED AT THE ASKER'S INSTANT, on every node.
//
// A listing, a related-agent page and its siblings, one event, a trace, a turn
// and the phase histories carry no window, but each is floored at the history
// horizon — and each node floored at its OWN clock's, so a fleet's answer was a
// union of different horizons and a node answering late, or running a little
// ahead, dropped what it held at the edge. The asker's [eventfan.Fleet.Clock]
// is read once per question and its instant sent with it; node-b's rows here
// sit just above the horizon under that instant and below its own clock's.
//
// Mutation: leave `at` off any question's wire parameters — the listing's, the
// trace rows', an id question's, the phases' — and node-b's rows drop out of
// that answer; ignore the Clock and node-a's own row does too.
func TestEveryHistoryQuestionIsAskedAtTheAskersInstant(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, b := newNode(t, broker, "node-a"), newNode(t, broker, "node-b")
	at := time.Now().UTC().Add(-time.Hour)
	horizon := at.Add(-store.EventHistory)
	// The cause: names nobody, on the node the delivery reached.
	appendTo(t, b, store.EventRecord{ID: "b-cause", Type: "webhook_received", Category: "webhook",
		Time: horizon.Add(time.Minute), TraceID: "tr-edge"})
	appendTo(t, b, store.EventRecord{ID: "b-phase", Type: "agent_phase_completed", Category: "task",
		Time: horizon.Add(2 * time.Minute), TraceID: "tr-edge",
		Tags:  map[string]string{"turn_id": "t-edge", "agent_role": "Lead"},
		Spend: &store.Spend{Phase: "execute", Model: "m", TurnID: "t-edge", TotalTokens: 30}})
	completionOn(t, b, "b-done", "t-edge", horizon.Add(3*time.Minute))
	// The work, naming the seat, on the node that held it.
	appendTo(t, a, store.EventRecord{ID: "a-work", Type: "a2a_asked", Category: "task",
		Time: horizon.Add(4 * time.Minute), TraceID: "tr-edge", Actor: "PM"})
	fan := fanFrom(a, "node-a", "node-b")
	fan.Clock = func() time.Time { return at }

	answered := func(t *testing.T, c eventfan.Coverage) {
		t.Helper()
		if !c.Complete {
			t.Fatalf("coverage %+v", c)
		}
	}
	t.Run("events", func(t *testing.T) {
		got, c, err := fan.List(t.Context(), store.ListQuery{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		answered(t, c)
		if ids := idsOf(got.Rows); !slices.Equal(ids, []string{"a-work", "b-done", "b-phase", "b-cause"}) {
			t.Errorf("the page is %v, want every node's rows above the asker's horizon", ids)
		}
	})
	t.Run("events by related agent", func(t *testing.T) {
		got, c, err := fan.List(t.Context(), store.ListQuery{RelatedAgent: "PM", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		answered(t, c)
		if ids := idsOf(got.Rows); !slices.Equal(ids, []string{"a-work", "b-phase", "b-cause"}) {
			t.Errorf("the page is %v, want the work and its trace siblings from node-b", ids)
		}
	})
	t.Run("event", func(t *testing.T) {
		rec, c, err := fan.ByID(t.Context(), "b-cause")
		if err != nil {
			t.Fatal(err)
		}
		answered(t, c)
		if rec.ID != "b-cause" {
			t.Errorf("got %q, want node-b's event", rec.ID)
		}
	})
	t.Run("trace", func(t *testing.T) {
		got, c, err := fan.Trace(t.Context(), "tr-edge")
		if err != nil {
			t.Fatal(err)
		}
		answered(t, c)
		if ids := idsOf(got.Rows); len(ids) != 3 || got.Total != 3 {
			t.Errorf("the trace is %v (total %d), want its three rows on both nodes", ids, got.Total)
		}
	})
	t.Run("turn", func(t *testing.T) {
		got, c, err := fan.Turn(t.Context(), "t-edge")
		if err != nil {
			t.Fatal(err)
		}
		answered(t, c)
		if len(got.Rows) != 2 || got.Total != 2 || !slices.Equal(got.Nodes, []string{"node-b"}) ||
			!slices.Equal(got.Traces, []string{"tr-edge"}) {
			t.Errorf("the turn is %d rows (total %d) on %v touching %v, want node-b's two rows and its trace",
				len(got.Rows), got.Total, got.Nodes, got.Traces)
		}
	})
	t.Run("phases", func(t *testing.T) {
		got, c, err := fan.Phases(t.Context(), "", 10, nil)
		if err != nil {
			t.Fatal(err)
		}
		answered(t, c)
		if ids := idsOf(got.Rows); !slices.Equal(ids, []string{"b-phase"}) {
			t.Errorf("the company's phases are %v, want node-b's", ids)
		}
	})
	t.Run("seat_phases", func(t *testing.T) {
		got, c, err := fan.SeatPhases(t.Context(), "", "Lead", nil)
		if err != nil {
			t.Fatal(err)
		}
		answered(t, c)
		if ids := idsOf(got.Rows); !slices.Equal(ids, []string{"b-phase"}) {
			t.Errorf("the seat's phases are %v, want node-b's", ids)
		}
	})
}

// ONE NODE'S PART IS READ AT ONE INSTANT, so its count is never short.
//
// A turn's part is four reads — its oldest rows, how many it holds, its newest
// rows and the traces it touched — and a trace's is two. Each read floored the
// history at its own reading of the clock, so a row crossing the floor between
// the rows and the count made the count come back short of them: a long turn
// counted at the cap was reported whole, lost its ending, and named no trace.
// The turn here is one row past the cap, every row above the horizon under the
// asker's instant and below the clock's — so a read floored anywhere but that
// instant finds none of it.
//
// Mutation: hand any of the part's follow-up reads (the count, the ending, the
// traces) the zero instant rather than the part's own, and that half of the
// answer comes back empty.
func TestOneNodesPartIsReadAtOneInstant(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a := newNode(t, broker, "node-a")
	at := time.Now().UTC().Add(-time.Hour)
	start := at.Add(-store.EventHistory).Add(time.Minute)
	held := store.MaxTurnEvents + 1
	for i := range held {
		appendTo(t, a, store.EventRecord{ID: fmt.Sprintf("r%04d", i), Type: "x", Category: "task",
			Time: start.Add(time.Duration(i) * time.Millisecond), TraceID: "tr-long",
			Tags: map[string]string{"turn_id": "t-long"}})
	}
	fan := eventfan.Solo("node-a", a.log)
	fan.Clock = func() time.Time { return at }

	turn, _, err := fan.Turn(t.Context(), "t-long")
	if err != nil {
		t.Fatal(err)
	}
	last := fmt.Sprintf("r%04d", held-1)
	if turn.Total != held || len(turn.Rows) == 0 || turn.Rows[len(turn.Rows)-1].ID != last ||
		!slices.Equal(turn.Traces, []string{"tr-long"}) {
		got := ""
		if len(turn.Rows) > 0 {
			got = turn.Rows[len(turn.Rows)-1].ID
		}
		t.Errorf("the turn counts %d ending at %q touching %v, want %d ending at %s touching tr-long",
			turn.Total, got, turn.Traces, held, last)
	}
	trace, _, err := fan.Trace(t.Context(), "tr-long")
	if err != nil {
		t.Fatal(err)
	}
	if trace.Total != held {
		t.Errorf("the trace counts %d, want %d — counted where its rows were read", trace.Total, held)
	}
}

// outcomeOn writes one notification outcome for a third-party app to a node's
// own store.
func outcomeOn(t *testing.T, n node, id, kind, app string, at time.Time) {
	t.Helper()
	appendTo(t, n, store.EventRecord{ID: id, Type: kind, Source: "engine", Category: "notification",
		Time: at, Summary: kind, Tags: map[string]string{"notification_source": app}})
}

// THE OUTCOME COUNTS ARE EVERY NODE'S, SUMMED, OVER THE ASKER'S WINDOW — and a
// node that could not count is named rather than read as zero.
//
// An outcome event is written to the store of the node that decided it, so a
// count from one store is that node's share; and every node counts the window
// the ASKER names, both edges — the bottom one, and the asker's instant, which
// is the top edge and where the history is floored. Node-b's rows sit on every
// edge: at the bottom (in), a second under it (out), at the instant (out), and
// a second above the asker's horizon, which is under node-b's own clock's (in
// on the window that reaches it). A build before the question refuses it by
// version, and a node that never answers is named for the budget.
//
// Mutation: answer from the asker's store alone, or keep one node's count where
// two name the same app, and gitlab is short; leave either edge off the wire
// parameters, and node-b counts a row outside the window or loses the one at
// the horizon.
func TestNotificationOutcomesAreSummedAcrossNodes(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, b := newNode(t, broker, "node-a"), newNode(t, broker, "node-b")
	servesAs(t, broker, "node-old", 4, store.EventRecord{ID: "old", Type: "notification_skipped",
		Category: "notification", Time: time.Now().UTC(),
		Tags: map[string]string{"notification_source": "gitlab"}})
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	since := at.Add(-2 * time.Hour)
	horizon := at.Add(-store.EventHistory)

	outcomeOn(t, a, "a-skip", "notification_skipped", "gitlab", at.Add(-time.Minute))
	outcomeOn(t, a, "a-merge", "notifications_coalesced", "slack", at.Add(-time.Hour))
	outcomeOn(t, b, "b-skip", "notification_skipped", "gitlab", since)
	outcomeOn(t, b, "b-merge", "notifications_coalesced", "slack", at.Add(-time.Minute))
	outcomeOn(t, b, "b-early", "notification_skipped", "gitlab", since.Add(-time.Second))
	outcomeOn(t, b, "b-late", "notification_skipped", "gitlab", at)
	outcomeOn(t, b, "b-horizon", "notification_skipped", "jira", horizon.Add(time.Second))

	fan := fanFrom(a, "node-a", "node-b", "node-old", "node-gone")
	fan.Budget = 500 * time.Millisecond
	fan.Clock = func() time.Time { return at }

	t.Run("the window", func(t *testing.T) {
		got, coverage, err := fan.NotificationOutcomes(t.Context(), store.OutcomeQuery{Since: since})
		if err != nil {
			t.Fatal(err)
		}
		if want := map[string]int{"gitlab": 2}; !maps.Equal(got.Skipped, want) {
			t.Errorf("skipped = %v, want %v — one inside the window on each node", got.Skipped, want)
		}
		if want := map[string]int{"slack": 2}; !maps.Equal(got.Coalesced, want) {
			t.Errorf("coalesced = %v, want %v — one merge on each node", got.Coalesced, want)
		}
		if coverage.Complete {
			t.Error("an answer two nodes short called itself complete")
		}
		if !missing(coverage, "node-gone", "budget") {
			t.Errorf("coverage %+v does not name node-gone for the budget", coverage)
		}
		if !missing(coverage, "node-old", "v5") {
			t.Errorf("coverage %+v does not name node-old for the version it was asked in", coverage)
		}
		if missing(coverage, "node-b", "") {
			t.Errorf("coverage %+v names node-b, which answered", coverage)
		}
	})
	t.Run("the whole history under the asker's instant", func(t *testing.T) {
		got, _, err := fan.NotificationOutcomes(t.Context(), store.OutcomeQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Skipped["jira"] != 1 || got.Skipped["gitlab"] != 3 {
			t.Errorf("skipped = %v, want jira's row above the asker's horizon and gitlab's "+
				"three before the asker's instant — never the one at it", got.Skipped)
		}
	})
}

// THE ASKER'S INSTANT IS READ AT THE STORE'S RESOLUTION, so the history floor is
// one edge on both sides of an answer.
//
// Every statement floors at the ENCODED instant, the microsecond, while the
// asker holds what comes back to its horizon itself. Read off a clock to the
// nanosecond, the two disagreed about a row at the floor's own microsecond:
// inside the history to the count of notification outcomes, under it to the
// listing — the two reads one integrations row is made of, at one instant. A
// clock half a microsecond past a tick puts the floor there.
//
// Mutation: drop the truncation from the fleet's instant, and the listing loses
// the row the count keeps, alone and fanned alike.
func TestTheAskersInstantIsReadAtTheStoresResolution(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a := newNode(t, broker, "node-a")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond).Add(500 * time.Nanosecond)
	edge := at.Add(-store.EventHistory).Truncate(time.Microsecond)
	outcomeOn(t, a, "at-floor", "notification_skipped", "gitlab", edge)

	for name, fan := range map[string]*eventfan.Fleet{
		"solo":   eventfan.Solo("node-a", a.log),
		"fanned": fanFrom(a, "node-a"),
	} {
		fan.Clock = func() time.Time { return at }
		listing, _, err := fan.List(t.Context(), store.ListQuery{Category: "notification"})
		if err != nil {
			t.Fatal(err)
		}
		if listing.At.Nanosecond()%int(time.Microsecond) != 0 {
			t.Errorf("%s: the listing was asked at %s, finer than the store holds", name,
				listing.At.Format(time.RFC3339Nano))
		}
		counted, _, err := fan.NotificationOutcomes(t.Context(), store.OutcomeQuery{At: listing.At})
		if err != nil {
			t.Fatal(err)
		}
		if len(listing.Rows) != 1 || counted.Skipped["gitlab"] != 1 {
			t.Errorf("%s: the listing holds %v and the count %d — want the row at the floor's "+
				"microsecond in both, one instant being one floor", name, idsOf(listing.Rows),
				counted.Skipped["gitlab"])
		}
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

// A CUSTODY ROW TWO DATA NODES HOLD IS COUNTED ONCE, at every step of its
// settling.
//
// A node without `data` hands its events to the data nodes in custody batches,
// and a batch whose claim failed is written by a second keeper before the first
// has learned it is not its own: until the first settles it, both logs hold the
// same rows. Summed as two counts, one dropped notification was two. So each
// node counts what it keeps and names what it holds unsettled, and the asker
// asks every node which of the named rows it keeps — while both copies are
// unsettled, once the keeper has settled and the other has not, and once both
// have — and counts each row once, asked from either node.
//
// Mutation: sum each node's count with the rows it names, or skip the second
// question, and the batch's drop is counted twice in one of the three states.
func TestACustodyRowTwoDataNodesHoldIsCountedOnce(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, b := newNode(t, broker, "node-a"), newNode(t, broker, "node-b")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	outcomeOn(t, a, "a-own", "notification_skipped", "gitlab", at.Add(-time.Minute))
	outcomeOn(t, b, "b-own", "notification_skipped", "gitlab", at.Add(-time.Minute))
	batch := store.CustodyBatch{ID: "batch-1", Origin: "seats-1", Records: []store.EventRecord{
		{ID: "x-skip", Type: "notification_skipped", Source: "engine", Category: "notification",
			Time: at.Add(-2 * time.Minute), Summary: "skipped", Payload: json.RawMessage(`{}`),
			Tags: map[string]string{"notification_source": "gitlab"}},
		{ID: "x-merge", Type: "notifications_coalesced", Source: "engine", Category: "notification",
			Time: at.Add(-2 * time.Minute), Summary: "coalesced", Payload: json.RawMessage(`{}`),
			Tags: map[string]string{"notification_source": "slack"}},
	}}
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
			got, coverage, err := fan.NotificationOutcomes(t.Context(), store.OutcomeQuery{})
			if err != nil {
				t.Fatal(err)
			}
			if !coverage.Complete {
				t.Fatalf("%s, asked from %s: coverage %+v", state, self.id, coverage)
			}
			if got.Skipped["gitlab"] != 3 || got.Coalesced["slack"] != 1 || len(got.Unsettled) != 0 {
				t.Errorf("%s, asked from %s: skipped %v, coalesced %v, unsettled %v — want each "+
					"node's own drop and the batch's drop and merge once", state, self.id,
					got.Skipped, got.Coalesced, got.Unsettled)
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

// turnOn writes one node's half of a turn, failed or not — see [halfOn].
func turnOn(t *testing.T, n node, turn string, at time.Time, failed bool) {
	t.Helper()
	tags := map[string]string{}
	if failed {
		tags["failed"] = "true"
	}
	halfOn(t, n, turn, at, tags)
}

// walkTurns walks every page a query of turns answers, by the page's own
// cursors, and says how often each turn was on a page and how it last read.
func walkTurns(t *testing.T, fan *eventfan.Fleet, q store.TurnQuery) (map[string]int, map[string]store.Turn) {
	t.Helper()
	seen, rows := map[string]int{}, map[string]store.Turn{}
	for range 100 {
		page, coverage, err := fan.Turns(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		if !coverage.Complete {
			t.Fatalf("coverage %+v", coverage)
		}
		for _, turn := range page.Turns {
			seen[turn.TurnID]++
			rows[turn.TurnID] = turn
		}
		if page.Next == nil {
			return seen, rows
		}
		q.Before = page.Next
	}
	t.Fatal("the walk never ended")
	return nil, nil
}

// onceEach reports what is wrong with a walk that should have listed each of
// want exactly once and nothing else.
func onceEach(seen map[string]int, want []string) []string {
	var wrong []string
	for _, id := range want {
		if seen[id] != 1 {
			wrong = append(wrong, fmt.Sprintf("%s listed %d times", id, seen[id]))
		}
	}
	for id := range seen {
		if !slices.Contains(want, id) {
			wrong = append(wrong, id+" listed and not held")
		}
	}
	slices.Sort(wrong)
	return wrong
}

// A TURN IS PAGED WHERE IT IS LISTED, though it began earlier on a node that
// does not list it.
//
// A page narrowed by a turn-level filter is selected by each node on its own
// half of a turn, and the turn resumed here after a move passes the filter
// only in its second half — failed only there, or charged to the item only by
// the records written there. The node holding the first half, where the turn
// began, lists none of it, and the node holding the second lists it at that
// half's start. Paged by where it began, it sorted below the newest point that
// full node's page stopped at and was cut, and the cursor moved on past the
// half that listed it, page after page, until no page held it. It is paged at
// the start its listing reaches, which each node says of its own share, and
// shown from where it began.
//
// Mutation: page a turn by its merged start, and the turn is on no page of
// either walk; ignore what a node says it lists and judge its share by what
// the share carries, and the half naming no item reads as listing the turn.
func TestATurnIsPagedWhereItIsListed(t *testing.T) {
	t.Parallel()
	failed := true
	for name, c := range map[string]struct {
		q              store.TurnQuery
		early, matches map[string]string
	}{
		"the failures": {store.TurnQuery{Failed: &failed}, map[string]string{},
			map[string]string{"failed": "true"}},
		"one work item": {store.TurnQuery{WorkItem: "native:t-1"}, map[string]string{},
			map[string]string{"work_item": "native:t-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			broker := memory.NewBroker()
			a, b := newNode(t, broker, "node-a"), newNode(t, broker, "node-b")
			at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			began, resumed := at.Add(-20*24*time.Hour), at.Add(-50*time.Hour)
			halfOn(t, a, "t-resumed", began, c.early)
			halfOn(t, b, "t-resumed", resumed, c.matches)
			want := []string{"t-resumed"}
			for i := range 49 {
				turn := fmt.Sprintf("t-newer-%02d", i)
				halfOn(t, b, turn, resumed.Add(time.Duration(i+1)*time.Hour), c.matches)
				want = append(want, turn)
			}
			for i := range 60 {
				turn := fmt.Sprintf("t-older-%02d", i)
				halfOn(t, b, turn, resumed.Add(-time.Duration(i+1)*time.Hour), c.matches)
				want = append(want, turn)
			}
			fan := fanFrom(a, "node-a", "node-b")
			fan.Clock = func() time.Time { return at }

			q := c.q
			q.SinceDays = store.MaxTurnDays
			seen, rows := walkTurns(t, fan, q)
			if wrong := onceEach(seen, want); len(wrong) > 0 {
				t.Fatalf("the walk over every page: %v", wrong)
			}
			if got := rows["t-resumed"]; !got.StartedAt.Equal(began) {
				t.Errorf("the resumed turn starts %s, want it shown from where it began, %s",
					got.StartedAt, began)
			}
		})
	}
}

// halfOn writes one node's half of a turn: a phase carrying some tags, and the
// completion a minute later.
func halfOn(t *testing.T, n node, turn string, at time.Time, tags map[string]string) {
	t.Helper()
	all := map[string]string{"turn_id": turn, "agent_role": "Lead"}
	for k, v := range tags {
		all[k] = v
	}
	appendTo(t, n, store.EventRecord{
		ID: turn + "-" + n.id + "-phase", Type: "agent_phase_completed", Category: "task", Time: at,
		Tags:  all,
		Spend: &store.Spend{Phase: "execute", Model: "m", TurnID: turn, TotalTokens: 30, InputTokens: 30},
	})
	completionOn(t, n, turn+"-"+n.id+"-done", turn, at.Add(time.Minute))
}
