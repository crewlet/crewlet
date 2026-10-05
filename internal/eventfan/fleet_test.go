package eventfan_test

import (
	"context"
	"encoding/json"
	"errors"
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
		rest, _, err := fan.Turns(t.Context(), store.TurnQuery{Limit: 10, Before: *cut.Next})
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
		exact, _, err := fan.Turns(t.Context(), store.TurnQuery{Limit: 2, Before: at.Add(2 * time.Minute)})
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

// servesAs stands a peer on the broker that behaves as a build speaking history
// protocol up to `version` does: it refuses a newer version by name, and answers
// any other listing with its own row, in the version it was asked — IGNORING
// every filter, which is exactly what an older build does with a field it
// cannot read.
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
		if req.Version > version {
			return json.Marshal(map[string]any{"version": version, "node": node, "error": fmt.Sprintf(
				"this node speaks history protocol up to v%d and was asked in v%d", version, req.Version)})
		}
		answer, _ := json.Marshal(map[string]any{"rows": []store.EventRecord{row}, "full": false})
		return json.Marshal(map[string]any{"version": req.Version, "node": node, "answer": json.RawMessage(answer)})
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
	servesAs(t, broker, "node-old", 1, store.EventRecord{ID: "old-unrelated", Type: "x",
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

// A v3 FILTER IS NOT ANSWERED AROUND BY A v2 PEER.
//
// The company's phases used to narrow by a role name; they narrow by the
// seat's own id now, and a v2 build reads only the role — so it would answer
// "this seat's phases" with every seat's. A listing and an axis narrowed by
// `suspended` are the same hazard: a v2 build counts the completion that
// parked a turn as one that ended it. Each goes out as v3, the v2 peer refuses
// by version, and the coverage names it; the unnarrowed question is still
// answered by the whole fleet.
//
// Mutation: ask the narrowed phases in v1, and the v2 peer's row is listed as
// this seat's; drop the Suspended case from [listParams.version], and the axis
// is summed over the v2 peer's unfiltered bars.
func TestAV3FilterIsNotAnsweredAroundByAV2Peer(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a := newNode(t, broker, "node-a")
	at := time.Now().UTC().Add(-time.Minute)
	appendTo(t, a, store.EventRecord{ID: "mine", Type: "agent_phase_completed", Category: "agent",
		Time: at, Tags: map[string]string{"agent_id": "agent-a", "agent_role": "Engineer"},
		Payload: []byte(`{"phase":"execute"}`)})
	servesAs(t, broker, "node-v2", 2, store.EventRecord{ID: "twin", Type: "agent_phase_completed",
		Category: "agent", Time: at.Add(time.Second)})
	fan := fanFrom(a, "node-a", "node-v2")

	all, coverage, err := fan.Phases(t.Context(), "", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !coverage.Complete || len(all.Rows) != 2 {
		t.Errorf("the company's phases: %v, coverage %+v — want both nodes' rows", idsOf(all.Rows), coverage)
	}
	seat, coverage, err := fan.Phases(t.Context(), "agent-a", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(seat.Rows); !slices.Equal(got, []string{"mine"}) {
		t.Errorf("one seat's phases = %v, want only its own — a v2 peer's unfiltered row is not a match", got)
	}
	if coverage.Complete || !missing(coverage, "node-v2", "v3") {
		t.Errorf("coverage %+v does not name node-v2 as unable to answer v3", coverage)
	}

	ended := false
	_, coverage, err = fan.List(t.Context(), store.ListQuery{Suspended: &ended, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Complete || !missing(coverage, "node-v2", "v3") {
		t.Errorf("a listing narrowed by suspended: coverage %+v does not name node-v2", coverage)
	}
	_, coverage, err = fan.Histogram(t.Context(), store.HistogramQuery{
		ListQuery: store.ListQuery{Type: "agent_turn_completed", Suspended: &ended}, Bucket: store.BucketHour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Complete || !missing(coverage, "node-v2", "v3") {
		t.Errorf("an axis narrowed by suspended: coverage %+v does not name node-v2", coverage)
	}
}

// A v4 FILTER IS NOT ANSWERED AROUND BY A v3 PEER.
//
// The event log's "Failures only" is a `failed` filter now, and a v3 build
// does not read it — it would answer "the failures" with every event it holds,
// merged in as though each one matched. The narrowed listing and its axis go
// out as v4, the v3 peer refuses by version and the coverage names it; the
// same listing without the filter is still answered by the whole fleet.
//
// Mutation: drop the Failed case from [listParams.version], and the v3 peer's
// clean row is listed as a failure.
func TestAV4FilterIsNotAnsweredAroundByAV3Peer(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a := newNode(t, broker, "node-a")
	at := time.Now().UTC().Add(-time.Minute)
	appendTo(t, a, store.EventRecord{ID: "broke", Type: "sandbox_run_failed", Category: "system", Time: at})
	servesAs(t, broker, "node-v3", 3, store.EventRecord{ID: "fine", Type: "thing_happened",
		Category: "system", Time: at.Add(time.Second)})
	fan := fanFrom(a, "node-a", "node-v3")

	all, coverage, err := fan.List(t.Context(), store.ListQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !coverage.Complete || len(all.Rows) != 2 {
		t.Errorf("the unnarrowed log: %v, coverage %+v — want both nodes' rows", idsOf(all.Rows), coverage)
	}
	failed := true
	only, coverage, err := fan.List(t.Context(), store.ListQuery{Failed: &failed, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(only.Rows); !slices.Equal(got, []string{"broke"}) {
		t.Errorf("the failures = %v, want only the one that failed — a v3 peer's clean row is not a match", got)
	}
	if coverage.Complete || !missing(coverage, "node-v3", "v4") {
		t.Errorf("a listing narrowed by failed: coverage %+v does not name node-v3", coverage)
	}
	_, coverage, err = fan.Histogram(t.Context(), store.HistogramQuery{
		ListQuery: store.ListQuery{Failed: &failed}, Bucket: store.BucketHour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Complete || !missing(coverage, "node-v3", "v4") {
		t.Errorf("an axis narrowed by failed: coverage %+v does not name node-v3", coverage)
	}
}

// servesUnfloored stands a peer on the broker that behaves as a build before
// the asker's instant does: it ignores `at`, and answers every row question
// with BOTH rows it holds — one past the asker's horizon, which its lookup by
// id never floored and its listings floored at a clock running behind the
// asker's, and one inside it — in the shape each question's part takes, in the
// version it was asked.
func servesUnfloored(t *testing.T, b *memory.Broker, node string, stale, fresh store.EventRecord) {
	t.Helper()
	q := client(t, b)
	stop, err := q.Serve(t.Context(), eventfan.Subject, func(_ context.Context, raw []byte) ([]byte, error) {
		var req struct {
			Version  int    `json:"version"`
			Question string `json:"question"`
			Params   struct {
				ID string `json:"id"`
			} `json:"params"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		newest := []store.EventRecord{fresh, stale}
		oldest := []store.EventRecord{stale, fresh}
		var answer any
		switch req.Question {
		case "event":
			var hit *store.EventRecord
			for _, r := range newest {
				if r.ID == req.Params.ID {
					hit = &r
				}
			}
			answer = map[string]any{"event": hit}
		case "trace":
			answer = map[string]any{"rows": oldest, "total": 2}
		case "turn":
			answer = map[string]any{"head": oldest, "total": 2,
				"traces": []store.TurnTrace{{TraceID: stale.TraceID, FirstAt: stale.Time}}}
		default:
			// A PAGE THAT FILLED, so the merge would stop at its last row
			// if that row were not cut away with the rest under the horizon.
			answer = map[string]any{"rows": newest, "full": true}
		}
		body, err := json.Marshal(answer)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"version": req.Version, "node": node, "answer": json.RawMessage(body)})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
}

// THE ASKER HOLDS EVERY ROW A PEER RETURNS TO ITS OWN HORIZON.
//
// The asker's instant raises no protocol version, so a node on an earlier
// build is still asked, ignores the instant and answers as of its own clock —
// and that build's lookup of one event by id was not floored at all, so a link
// to an event past the thirty-day horizon resolved from whichever such node
// still held a copy, during every upgrade. The asker owns the instant, so it
// cuts what comes back: the stale row here sits an hour under the asker's
// horizon on a peer that returns it to every question, and no answer carries
// it — the link is not found, with every node counted as answering, and a
// page whose last row was under the horizon no longer claims more behind it.
//
// Mutation: drop [heldTo] from any one question in fleet.go and that subtest
// gets the stale row back.
func TestAnOlderPeersRowsAreHeldToTheAskersHorizon(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a := newNode(t, broker, "node-a")
	at := time.Now().UTC().Add(-time.Hour)
	horizon := at.Add(-store.EventHistory)
	stale := store.EventRecord{ID: "stale", Type: "agent_phase_completed", Category: "agent",
		Time: horizon.Add(-time.Hour), TraceID: "tr-1", Actor: "Lead",
		Tags: map[string]string{"turn_id": "t-1", "agent_role": "Lead"}}
	fresh := stale
	fresh.ID, fresh.Time = "fresh", horizon.Add(time.Hour)
	servesUnfloored(t, broker, "node-old", stale, fresh)
	fan := fanFrom(a, "node-a", "node-old")
	fan.Clock = func() time.Time { return at }

	complete := func(t *testing.T, c eventfan.Coverage) {
		t.Helper()
		if !c.Complete {
			t.Fatalf("coverage %+v, want every node answering", c)
		}
	}
	t.Run("event", func(t *testing.T) {
		_, c, err := fan.ByID(t.Context(), "stale")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a link to an event an hour past the horizon answered %v, want not found", err)
		}
		complete(t, c)
		if rec, _, err := fan.ByID(t.Context(), "fresh"); err != nil || rec.ID != "fresh" {
			t.Errorf("the event inside the horizon: %q, %v — want it found", rec.ID, err)
		}
	})
	t.Run("events", func(t *testing.T) {
		got, c, err := fan.List(t.Context(), store.ListQuery{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		complete(t, c)
		if ids := idsOf(got.Rows); !slices.Equal(ids, []string{"fresh"}) || got.More {
			t.Errorf("the page is %v (more %v), want only the row inside the horizon and "+
				"nothing behind it", ids, got.More)
		}
	})
	t.Run("events by related agent", func(t *testing.T) {
		got, c, err := fan.List(t.Context(), store.ListQuery{RelatedAgent: "Lead", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		complete(t, c)
		if ids := idsOf(got.Rows); !slices.Equal(ids, []string{"fresh"}) {
			t.Errorf("the page and its trace siblings are %v, want only the row inside the horizon", ids)
		}
	})
	t.Run("trace", func(t *testing.T) {
		got, c, err := fan.Trace(t.Context(), "tr-1")
		if err != nil {
			t.Fatal(err)
		}
		complete(t, c)
		if ids := idsOf(got.Rows); !slices.Equal(ids, []string{"fresh"}) || got.Total != 1 {
			t.Errorf("the trace is %v (total %d), want the row inside the horizon, counted once",
				ids, got.Total)
		}
	})
	t.Run("turn", func(t *testing.T) {
		got, c, err := fan.Turn(t.Context(), "t-1")
		if err != nil {
			t.Fatal(err)
		}
		complete(t, c)
		if ids := idsOf(got.Rows); !slices.Equal(ids, []string{"fresh"}) || got.Total != 1 ||
			!slices.Equal(got.Nodes, []string{"node-old"}) {
			t.Errorf("the turn is %v (total %d) on %v, want node-old's row inside the horizon",
				ids, got.Total, got.Nodes)
		}
	})
	t.Run("phases", func(t *testing.T) {
		got, c, err := fan.Phases(t.Context(), "", 10, nil)
		if err != nil {
			t.Fatal(err)
		}
		complete(t, c)
		if ids := idsOf(got.Rows); !slices.Equal(ids, []string{"fresh"}) || got.More {
			t.Errorf("the company's phases are %v (more %v), want only the row inside the horizon",
				ids, got.More)
		}
	})
	t.Run("seat_phases", func(t *testing.T) {
		got, c, err := fan.SeatPhases(t.Context(), "", "Lead", nil)
		if err != nil {
			t.Fatal(err)
		}
		complete(t, c)
		if ids := idsOf(got.Rows); !slices.Equal(ids, []string{"fresh"}) {
			t.Errorf("the seat's phases are %v, want only the row inside the horizon", ids)
		}
	})
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
