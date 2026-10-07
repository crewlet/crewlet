package eventfan

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// seriesPeer answers the fleet's time axis from a store of its own and counts
// the questions it is asked. As an OLDER build it does not know `feed_only`:
// it refuses anything past v4, ignores the field and counts every row it
// holds — accounting rows a newer build wrote before a rollback among them —
// and sends no list of what it left out. As a current one it answers through
// the answerer every node runs. refuseNarrowed makes it refuse the second read
// a narrowing asks, the one naming a type.
type seriesPeer struct {
	older, refuseNarrowed bool
	mu                    sync.Mutex
	asked                 []seriesParams
}

func (s *seriesPeer) serve(t *testing.T, q queue.EventQueue, node string, log *store.EventLog) {
	t.Helper()
	stop, err := q.Serve(t.Context(), Subject, func(ctx context.Context, raw []byte) ([]byte, error) {
		var req request
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.Asker == node {
			return nil, errAsker
		}
		if s.older && req.Version > 4 {
			return encodeError(node, "this node speaks history protocol up to v4")
		}
		if req.Question != QuestionSeries {
			return encodeError(node, "this peer answers only the time axis")
		}
		var p seriesParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.asked = append(s.asked, p)
		s.mu.Unlock()
		if s.refuseNarrowed && p.List.Type != "" {
			return encodeError(node, "the read failed")
		}
		if !s.older {
			part, err := answer(ctx, log, req.Question, req.Params, nil)
			if err != nil {
				return encodeError(node, err.Error())
			}
			return fit(node, part, queue.MaxPayloadBytes, req.Version)
		}
		list := p.List
		list.FeedOnly = false
		h, err := log.Histogram(ctx, store.HistogramQuery{ListQuery: list.query(), Bucket: p.Bucket, At: p.At})
		if err != nil {
			return encodeError(node, err.Error())
		}
		return fit(node, h, queue.MaxPayloadBytes, req.Version)
	})
	if err != nil {
		t.Fatalf("serve %s: %v", node, err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
}

func (s *seriesPeer) questions() []seriesParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked)
}

// storeWith is a node's own store holding rows.
func storeWith(t *testing.T, rows ...store.EventRecord) *store.EventLog {
	t.Helper()
	db, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, r := range rows {
		if r.Payload == nil {
			r.Payload = json.RawMessage(`{}`)
		}
		if err := db.Events().Append(t.Context(), r); err != nil {
			t.Fatalf("append %s: %v", r.ID, err)
		}
	}
	return db.Events()
}

// A FEED-ONLY AXIS IS ANSWERED BY AN OLDER PEER, AND NARROWED BY THE ASKER.
//
// The Live strip and the event log's axis ask for the feed's rows alone. Asked
// at a version only the newer build speaks, every peer on the build before
// refused and its bars vanished for the length of an upgrade — to keep out
// rows such a peer can hold only after a rollback from the newer build, since
// the build before never writes them. So the axis goes out at its other
// filters' version; a current peer narrows it in its own read and says which
// types it left out, and is asked nothing more; an older peer counts what it
// holds, says nothing, and is asked once more for the axis of the type it
// should have left out, which the asker takes back out bar by bar. Every node
// answers, and no bar counts a row the feed does not hold.
//
// Mutations: ask the axis at a version of its own and the older peer is
// refused; sum its bars as they came and they count its accounting rows;
// ignore what the current peer says it left out and its row is taken out
// twice.
func TestAnOlderPeersFeedAxisIsNarrowedNotRefused(t *testing.T) {
	t.Parallel()
	unfed := events.Unfed()
	if len(unfed) == 0 {
		t.Fatal("no type is kept out of the feed — the case asserts nothing")
	}
	spend, _ := events.Category(unfed[0])
	at := time.Now().UTC().Add(-time.Minute)
	fed := func(id string) store.EventRecord {
		return store.EventRecord{ID: id, Type: "agent_turn_completed", Category: "lifecycle", Time: at}
	}
	kept := func(id string, failed bool) store.EventRecord {
		r := store.EventRecord{ID: id, Type: unfed[0], Category: spend, Time: at}
		if failed {
			r.Tags = map[string]string{"failed": "true"}
		}
		return r
	}
	mine := storeWith(t, fed("mine"), kept("mine-spend", false))
	broker := memory.NewBroker()
	older := &seriesPeer{older: true}
	older.serve(t, memberQueue(t, broker), "old", storeWith(t,
		fed("old-turn"), kept("old-spend-1", true), kept("old-spend-2", false)))
	current := &seriesPeer{}
	current.serve(t, memberQueue(t, broker), "cur", storeWith(t, fed("cur-turn"), kept("cur-spend", false)))
	fleet := &Fleet{
		Self: "new", Local: mine, Queue: memberQueue(t, broker),
		Roster: func(context.Context) ([]string, error) { return []string{"cur", "new", "old"}, nil },
		Budget: 5 * time.Second,
	}

	got, coverage, err := fleet.Histogram(t.Context(), store.HistogramQuery{
		ListQuery: store.ListQuery{FeedOnly: true}, Bucket: store.BucketHour,
	})
	if err != nil {
		t.Fatalf("Histogram: %v", err)
	}
	if !coverage.Complete {
		t.Fatalf("coverage = %+v — every node holds an answer to a feed-only axis", coverage)
	}
	if got.Total != 3 || got.Failed != 0 {
		t.Fatalf("total %d with %d failed, want 3 with none: one turn from each node and no "+
			"accounting row", got.Total, got.Failed)
	}
	hour := at.Truncate(time.Hour).Format(time.RFC3339)
	for _, bar := range got.Bars {
		if bar.At == hour && (bar.Count != 3 || bar.Failed != 0) {
			t.Errorf("the rows' bar is %d with %d failed, want 3 with none", bar.Count, bar.Failed)
		}
	}
	if n, ok := got.ByCategory[spend]; ok {
		t.Errorf("the %s facet counts %d rows the feed does not hold", spend, n)
	}
	if got.ByCategory["lifecycle"] != 3 {
		t.Errorf("the lifecycle facet counts %d, want 3", got.ByCategory["lifecycle"])
	}
	// THE SECOND READ REACHES EVERY NODE — the subject is the fleet's — and
	// the current peer's answer to it is not taken out of anything: its total
	// above is its turn, not its turn less its own accounting row twice.
	if asked := current.questions(); len(asked) == 0 || !asked[0].List.FeedOnly {
		t.Errorf("the current peer was asked %+v, want the feed-only axis first", asked)
	}
	asked := older.questions()
	if len(asked) != 2 || asked[1].List.Type != unfed[0] || asked[1].List.FeedOnly {
		t.Fatalf("the older peer was asked %+v, want the axis and then the axis of %s alone", asked, unfed[0])
	}
	if !asked[1].At.Equal(asked[0].At) || asked[1].Bucket != asked[0].Bucket {
		t.Errorf("the second read cut another window: %+v after %+v", asked[1], asked[0])
	}
}

// A PEER WHOSE AXIS CANNOT BE NARROWED IS NAMED, NOT SUMMED. An older peer that
// answered the axis and then could not answer the read that takes the feed's
// left-out rows back out holds bars that count them — summed in, the strip
// would draw them; so it is left out and the coverage says why.
//
// Mutation: sum a peer whose second read failed, and its accounting row is a
// bar.
func TestAPeerWhoseAxisCannotBeNarrowedIsNamedNotSummed(t *testing.T) {
	t.Parallel()
	unfed := events.Unfed()
	at := time.Now().UTC().Add(-time.Minute)
	mine := storeWith(t, store.EventRecord{ID: "mine", Type: "agent_turn_completed", Category: "lifecycle", Time: at})
	broker := memory.NewBroker()
	older := &seriesPeer{older: true, refuseNarrowed: true}
	older.serve(t, memberQueue(t, broker), "old", storeWith(t,
		store.EventRecord{ID: "old-turn", Type: "agent_turn_completed", Category: "lifecycle", Time: at},
		store.EventRecord{ID: "old-spend", Type: unfed[0], Category: "system", Time: at}))
	fleet := &Fleet{
		Self: "new", Local: mine, Queue: memberQueue(t, broker),
		Roster: func(context.Context) ([]string, error) { return []string{"new", "old"}, nil },
		Budget: 5 * time.Second,
	}
	got, coverage, err := fleet.Histogram(t.Context(), store.HistogramQuery{
		ListQuery: store.ListQuery{FeedOnly: true}, Bucket: store.BucketHour,
	})
	if err != nil {
		t.Fatalf("Histogram: %v", err)
	}
	if got.Total != 1 {
		t.Errorf("total %d, want this node's 1 — the older peer's bars count a row the feed does not hold", got.Total)
	}
	if coverage.Complete || !slices.ContainsFunc(coverage.Nodes, func(n NodeCoverage) bool {
		return n.ID == "old" && !n.Answered && strings.Contains(n.Error, unfed[0])
	}) {
		t.Errorf("coverage %+v does not name the older peer and the type it could not leave out", coverage)
	}
}

// AN AXIS NARROWED TO A FED TYPE ASKS NOTHING MORE, and one narrowed to a type
// the feed leaves out is empty: the type filter already decides which rows of
// the class an axis can count, so only an axis that could count one is ever
// asked about it again.
func TestATypedFeedAxisAsksOnlyWhatItCouldCount(t *testing.T) {
	t.Parallel()
	unfed := events.Unfed()
	at := time.Now().UTC().Add(-time.Minute)
	mine := storeWith(t)
	broker := memory.NewBroker()
	older := &seriesPeer{older: true}
	older.serve(t, memberQueue(t, broker), "old", storeWith(t,
		store.EventRecord{ID: "old-turn", Type: "agent_turn_completed", Category: "lifecycle", Time: at},
		store.EventRecord{ID: "old-spend", Type: unfed[0], Category: "system", Time: at}))
	fleet := &Fleet{
		Self: "new", Local: mine, Queue: memberQueue(t, broker),
		Roster: func(context.Context) ([]string, error) { return []string{"new", "old"}, nil },
		Budget: 5 * time.Second,
	}
	turns, coverage, err := fleet.Histogram(t.Context(), store.HistogramQuery{
		ListQuery: store.ListQuery{Type: "agent_turn_completed", FeedOnly: true}, Bucket: store.BucketHour,
	})
	if err != nil || !coverage.Complete || turns.Total != 1 {
		t.Fatalf("an axis of turns = total %d, coverage %+v, %v; want the older peer's turn", turns.Total, coverage, err)
	}
	if asked := older.questions(); len(asked) != 1 {
		t.Errorf("an axis of a fed type asked the older peer %d times, want once", len(asked))
	}
	spend, coverage, err := fleet.Histogram(t.Context(), store.HistogramQuery{
		ListQuery: store.ListQuery{Type: unfed[0], FeedOnly: true}, Bucket: store.BucketHour,
	})
	if err != nil || !coverage.Complete || spend.Total != 0 {
		t.Fatalf("an axis of %s on the feed = total %d, coverage %+v, %v; want nothing", unfed[0],
			spend.Total, coverage, err)
	}
}
