package estate

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// GATHERS, exercised over an in-process divided layout: the tracker space in
// four partitions, the pages space in two, the company space in one
// ([dividedLayout]) — every partition served by named nodes, each of which a
// case can make silent, behind, not a holder, lagging, or failing per
// partition.

// opTestList is a paged gather over every partition carrying the tracker's
// log, read at a level: the shape the tracker's own lists take when they are
// gathered, declared here so the framework's level and paging rules are held
// to a real gather of the real wire.
var opTestList = defineGather("test.list", address[listArgs]{domain: trackerDomain, partitions: everyTrackerPartition},
	func(ctx context.Context, b Backend, _ statelog.PartitionID, a listArgs) (tracker.Answer, error) {
		if b.Tracker == nil {
			return tracker.Answer{}, errNoHalf
		}
		return b.Tracker.Tasks(ctx, tracker.Query{
			Level: a.Level, Session: a.Session, Cursor: a.Cursor, Limit: a.Limit,
		}, time.Time{})
	},
	func(a listArgs, g Gathered[tracker.Answer]) (tracker.Answer, error) {
		rows, next := mergePaged(
			pagedParts(g.Parts, func(p tracker.Answer) ([]tracker.TaskRow, string) {
				return p.Rows, p.NextCursor
			}),
			func(x, y tracker.TaskRow) int { return cmp.Compare(x.Key, y.Key) },
			func(r tracker.TaskRow) string { return r.Key }, a.Limit)
		return tracker.Answer{Rows: rows, NextCursor: g.Cursor(next)}, nil
	}).
	leveled(func(a listArgs) statelog.ReadLevel { return a.Level },
		func(a listArgs, level statelog.ReadLevel, floor statelog.Position) listArgs {
			a.Level, a.Session = level, floor
			return a
		}).
	paged(func(a listArgs) string { return a.Cursor },
		func(a listArgs, own string) listArgs {
			a.Cursor = own
			return a
		})

type listArgs struct {
	Level   statelog.ReadLevel
	Session statelog.Position
	Cursor  string
	Limit   int
}

// everyTrackerPartition is every partition carrying the tracker's log.
func everyTrackerPartition(_ context.Context, l statelog.Layout, _ Resolver, _ listArgs) (
	[]statelog.PartitionID, error) {
	var out []statelog.PartitionID
	for _, log := range l.LogsOf(trackerDomain) {
		out = append(out, log.Partition)
	}
	return out, nil
}

// trouble is what one node does with one partition.
type trouble int

const (
	answers trouble = iota
	failing
	behindFloor
	lags
	notServing
	cannotTell
	noHalves
	// lagsFailing is a copy that lags its logs and whose read, told to
	// answer anyway, fails.
	lagsFailing
	// stalls is a copy whose read never finishes within an attempt: it
	// runs until the batch is told to give up.
	stalls
)

// partNode is one data node holding some partitions of the divided layout.
type partNode struct {
	name string

	mu       sync.Mutex
	holds    map[statelog.PartitionID]trouble
	silent   bool
	requests []request
	barriers []string
	levels   map[statelog.PartitionID][]tracker.Query
	inFlight atomic.Int32
	peak     atomic.Int32

	// gate, when set, holds every read until it is closed — so a case can
	// see how many run at once.
	gate chan struct{}

	// floorGate, when set, holds every floor wait until it is closed, and
	// waiting counts the waits it holds — so a case can see how many wait
	// at once.
	floorGate chan struct{}
	waiting   atomic.Int32

	// delay is how long each read takes.
	delay time.Duration
}

func (n *partNode) set(change func(*partNode)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	change(n)
}

func (n *partNode) asked() []request {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.requests)
}

// For implements [LocalBackends] over the partitions the node holds.
func (n *partNode) For(_ context.Context, p statelog.PartitionID) (Backend, bool, error) {
	n.mu.Lock()
	t, held := n.holds[p]
	n.mu.Unlock()
	switch {
	case !held || t == notServing:
		return Backend{}, false, nil
	case t == cannotTell:
		return Backend{}, false, errors.New("the holding could not be read")
	}
	return n.backend(p, t), true, nil
}

// backend is the node's copy of p.
func (n *partNode) backend(p statelog.PartitionID, t trouble) Backend {
	if t == noHalves {
		return Backend{}
	}
	read := partRead{node: n, p: p, t: t}
	return Backend{
		Tracker: read, Knowledge: read, WorkSearch: workSearcherOf{read},
		Committed: func(ctx context.Context, _ statelog.Position) error {
			if t == behindFloor {
				<-ctx.Done()
				return ctx.Err()
			}
			n.mu.Lock()
			gate := n.floorGate
			n.mu.Unlock()
			if gate != nil {
				n.waiting.Add(1)
				defer n.waiting.Add(-1)
				select {
				case <-gate:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		},
		Answers: func(context.Context) bool { return t != lags && t != lagsFailing },
		Applied: func(stream string) statelog.Position {
			return statelog.Position{Stream: stream, Generation: 1, Seq: 10}
		},
		Barrier: func(_ context.Context, stream string) (statelog.Position, bool, error) {
			if !trackerStreams[stream] {
				// ONLY THE TRACKER'S LOGS KEEP A READ INDEX here, as
				// the vectors' derived log keeps none in the engine.
				return statelog.Position{}, false, nil
			}
			n.mu.Lock()
			n.barriers = append(n.barriers, stream)
			n.mu.Unlock()
			return statelog.Position{Stream: stream, Generation: 1, Seq: 99}, true, nil
		},
	}
}

// trackerStreams is every tracker log's stream, in either layout a case runs.
var trackerStreams = func() map[string]bool {
	out := map[string]bool{}
	for _, l := range []statelog.Layout{layoutZero, dividedLayout} {
		for _, log := range l.LogsOf(trackerDomain) {
			stream, _ := l.Stream(log)
			out[stream] = true
		}
	}
	return out
}()

// partRead is one partition's reads on one node.
type partRead struct {
	TrackerReader
	node *partNode
	p    statelog.PartitionID
	t    trouble
}

func (r partRead) enter() func() {
	now := r.node.inFlight.Add(1)
	for peak := r.node.peak.Load(); now > peak && !r.node.peak.CompareAndSwap(peak, now); {
		peak = r.node.peak.Load()
	}
	r.node.mu.Lock()
	gate := r.node.gate
	r.node.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return func() { r.node.inFlight.Add(-1) }
}

// rowsOf is the partition's rows: every key K<n> with n in this partition's
// residue, so the partitions' keys interleave.
func rowsOf(p statelog.PartitionID) []tracker.TaskRow {
	from, step := int(p.Index), 4
	switch p.Space {
	case statelog.SpaceTracker:
	case statelog.SpaceEstate:
		// THE WHOLE ESTATE, every row in its one partition.
		from, step = 0, 1
	default:
		return nil
	}
	var out []tracker.TaskRow
	for n := from; n < 24; n += step {
		out = append(out, tracker.TaskRow{Key: fmt.Sprintf("K%02d", n), Title: p.String()})
	}
	return out
}

func (r partRead) Tasks(ctx context.Context, q tracker.Query, _ time.Time) (tracker.Answer, error) {
	defer r.enter()()
	r.node.mu.Lock()
	if r.node.levels == nil {
		r.node.levels = map[statelog.PartitionID][]tracker.Query{}
	}
	r.node.levels[r.p] = append(r.node.levels[r.p], q)
	delay := r.node.delay
	r.node.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return tracker.Answer{}, ctx.Err()
		}
	}
	if r.t == stalls {
		<-ctx.Done()
		return tracker.Answer{}, ctx.Err()
	}
	if r.t == failing || r.t == lagsFailing {
		return tracker.Answer{}, fmt.Errorf("the read of %s failed on %s: %w", r.p, r.node.name,
			tracker.ErrNoProject)
	}
	var out tracker.Answer
	for _, row := range rowsOf(r.p) {
		if q.Cursor != "" && row.Key <= q.Cursor {
			continue
		}
		if q.Limit > 0 && len(out.Rows) == q.Limit {
			out.NextCursor = out.Rows[len(out.Rows)-1].Key
			break
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

func (r partRead) Slice(_ context.Context, q knowledge.Query) (pages.SearchSlice, error) {
	defer r.enter()()
	if r.t == failing {
		return pages.SearchSlice{}, fmt.Errorf("the index of %s failed on %s", r.p, r.node.name)
	}
	key := "page:" + r.p.String()
	return pages.SearchSlice{
		Candidates: search.Candidates{Lexical: []search.Scored{{Key: key, Score: float64(r.p.Index + 1)}}},
		Hits:       map[string]knowledge.Hit{key: {Title: r.p.String(), Snippet: q.Text}},
	}, nil
}

func (r partRead) Building(context.Context) bool { return r.p.Index == 1 }

// workSlice is the tracker's half of a ranked search on this partition —
// reached through [workSearcherOf], since its knowledge half is also Slice.
func (r partRead) workSlice(text string) (tracker.SearchSlice, error) {
	defer r.enter()()
	if r.t == failing {
		return tracker.SearchSlice{}, fmt.Errorf("the index of %s failed on %s", r.p, r.node.name)
	}
	key := "task:" + r.p.String()
	return tracker.SearchSlice{
		Candidates: search.Candidates{Lexical: []search.Scored{{Key: key, Score: float64(r.p.Index + 1)}}},
		Items:      map[string]tracker.Ranked{key: {ID: r.p.String(), Title: text}},
	}, nil
}

// partPlacement is the divided layout, its partitions' holders named by case.
type partPlacement struct {
	mu        sync.Mutex
	layout    statelog.Layout
	holders   map[statelog.PartitionID][]string
	epoch     uint64
	refreshes int
	onRefresh func(*partPlacement)
}

func (p *partPlacement) Layout() (statelog.Layout, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.layout, nil
}

func (p *partPlacement) Serving(part statelog.PartitionID) ([]string, uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.holders[part]), p.epoch, nil
}

func (p *partPlacement) Refresh(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshes++
	if p.onRefresh != nil {
		p.onRefresh(p)
	}
	return nil
}

func (p *partPlacement) Unanswered(string) {}

// partFleet is nodes serving the divided layout on one broker.
type partFleet struct {
	broker *memory.Broker
	nodes  map[string]*partNode

	// placement is what the routers route by, and servers what the serving
	// nodes' own views say — two views, so a case can put them at
	// different map epochs.
	placement, servers *partPlacement
}

// newPartFleet stands up a node per name, each holding the partitions holds
// gives it, and a placement naming them as each partition's holders in the
// order given.
func newPartFleet(t *testing.T, holds map[string][]statelog.PartitionID) *partFleet {
	t.Helper()
	f := &partFleet{broker: memory.NewBroker(), nodes: map[string]*partNode{},
		placement: &partPlacement{layout: dividedLayout,
			holders: map[statelog.PartitionID][]string{}},
		servers: &partPlacement{layout: dividedLayout,
			holders: map[statelog.PartitionID][]string{}}}
	for _, name := range slices.Sorted(maps.Keys(holds)) {
		node := &partNode{name: name, holds: map[statelog.PartitionID]trouble{}}
		for _, p := range holds[name] {
			node.holds[p] = answers
			f.placement.holders[p] = append(f.placement.holders[p], name)
			f.servers.holders[p] = append(f.servers.holders[p], name)
		}
		f.nodes[name] = node
		q := f.client(t)
		stop, err := Serve(t.Context(), recorder{q: q, node: node}, name, node, f.servers,
			ServerSeams{})
		if err != nil {
			t.Fatalf("serve %s: %v", name, err)
		}
		t.Cleanup(func() { _ = stop(context.Background()) })
	}
	return f
}

func (f *partFleet) client(t *testing.T) *memory.Queue {
	t.Helper()
	q := f.broker.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	return q
}

// router is a router on self, serving in-process what local holds.
func (f *partFleet) router(t *testing.T, self string, local *partNode) *Router {
	t.Helper()
	opts := RouterOptions{Queue: f.client(t), Self: self, Placement: f.placement,
		Session: NewSession()}
	if local != nil {
		opts.Local = local
	}
	r, err := NewRouter(opts)
	if err != nil {
		t.Fatal(err)
	}
	r.readBudget = 300 * time.Millisecond
	return r
}

// recorder records every request a node is asked and answers nothing while
// the node is silent.
type recorder struct {
	q    *memory.Queue
	node *partNode
}

func (r recorder) Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error) {
	return r.q.Serve(ctx, subject, func(ctx context.Context, raw []byte) ([]byte, error) {
		var req request
		_ = json.Unmarshal(raw, &req)
		r.node.mu.Lock()
		r.node.requests = append(r.node.requests, req)
		silent := r.node.silent
		r.node.mu.Unlock()
		if silent {
			return nil, errors.New("this node is gone")
		}
		return h(ctx, raw)
	})
}

// workSearcherOf makes a partRead a [WorkSearcher].
type workSearcherOf struct{ partRead }

func (w workSearcherOf) Slice(_ context.Context, text string) (tracker.SearchSlice, error) {
	return w.workSlice(text)
}

func tp(i uint16) statelog.PartitionID {
	return statelog.PartitionID{Space: statelog.SpaceTracker, Index: i}
}

func pp(i uint16) statelog.PartitionID {
	return statelog.PartitionID{Space: statelog.SpacePages, Index: i}
}

var company = statelog.PartitionID{Space: statelog.SpaceCompany}

// listAll is a gathered list of every row, read from r as an operator reads.
func listAll(t *testing.T, r *Router, limit int, cursor string) (tracker.Answer, statelog.Coverage, error) {
	t.Helper()
	return gather(t.Context(), r, opTestList, statelog.SurfaceOperator,
		listArgs{Level: statelog.ReadLinearizable, Limit: limit, Cursor: cursor})
}

// ONE REQUEST PER HOLDER, carrying every partition it is asked for, and every
// partition answered — merged in the list's own order across partitions.
func TestAGatherAsksEachHolderOnceForAllItsPartitions(t *testing.T) {
	t.Parallel()
	f := newPartFleet(t, map[string][]statelog.PartitionID{
		"data-a": {tp(0), tp(1), company},
		"data-b": {tp(2), tp(3)},
	})
	r := f.router(t, "agent-1", nil)
	answer, cov, err := listAll(t, r, 0, "")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if got := len(answer.Rows); got != 24 {
		t.Fatalf("the gather answered %d rows, want all 24 of the four partitions", got)
	}
	if !slices.IsSortedFunc(answer.Rows, func(x, y tracker.TaskRow) int { return cmp.Compare(x.Key, y.Key) }) {
		t.Errorf("the rows are not in the list's own order across partitions: %v", answer.Rows)
	}
	if !cov.Complete() || cov.Addressed != 5 || len(cov.Answered) != 5 {
		t.Errorf("coverage = %+v, want all five tracker partitions answered", cov)
	}
	for name, want := range map[string][]string{
		"data-a": {"company.000", "tracker.000", "tracker.001"},
		"data-b": {"tracker.002", "tracker.003"},
	} {
		asked := f.nodes[name].asked()
		if len(asked) != 1 {
			t.Fatalf("%s was asked %d times, want once for all its partitions", name, len(asked))
		}
		got := slices.Sorted(slices.Values(asked[0].Partitions))
		if !slices.Equal(got, want) || !asked[0].Slices {
			t.Errorf("%s was asked %v (slices %v), want %v as slices", name, got, asked[0].Slices, want)
		}
	}
}

// A PARTITION THAT DID NOT ANSWER IS NAMED, WITH WHY — never a short list.
//
// Each of the five reasons a holder can give is held to its name, and in each
// case the partitions that did answer are in the answer and the one that did
// not is on its coverage, so a reader is told the list may be incomplete.
func TestAPartitionThatDidNotAnswerIsNamedWithItsReason(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		arrange func(f *partFleet)
		want    statelog.MissingReason
	}{
		"nobody serves it": {
			arrange: func(f *partFleet) {
				f.placement.mu.Lock()
				f.placement.holders[tp(2)] = nil
				f.placement.mu.Unlock()
			},
			want: statelog.MissingUnserved,
		},
		"its holder does not answer": {
			arrange: func(f *partFleet) { f.nodes["data-b"].set(func(n *partNode) { n.silent = true }) },
			want:    statelog.MissingUnreachable,
		},
		"its holder is behind the asker's floor": {
			arrange: func(f *partFleet) {
				f.nodes["data-b"].set(func(n *partNode) { n.holds[tp(2)] = behindFloor })
			},
			want: statelog.MissingBehind,
		},
		"its holder no longer serves it": {
			arrange: func(f *partFleet) {
				f.nodes["data-b"].set(func(n *partNode) { n.holds[tp(2)] = notServing })
			},
			want: statelog.MissingNotHolder,
		},
		"the read failed there": {
			arrange: func(f *partFleet) {
				f.nodes["data-b"].set(func(n *partNode) { n.holds[tp(2)] = failing })
			},
			want: statelog.MissingError,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newPartFleet(t, map[string][]statelog.PartitionID{
				"data-a": {tp(0), tp(1), tp(3), company},
				"data-b": {tp(2)},
			})
			tc.arrange(f)
			r := f.router(t, "agent-1", nil)
			// LONGER THAN THE HOLDER'S FLOOR WAIT, so a holder that is
			// behind says so before the asker stops listening.
			r.readBudget = statelog.ReadBudget + time.Second
			// A FLOOR ON EVERY TRACKER LOG, which is what a holder that is
			// behind is behind.
			for _, p := range []statelog.PartitionID{tp(0), tp(1), tp(2), tp(3), company} {
				stream, _ := dividedLayout.Stream(statelog.LogID{Domain: trackerDomain, Partition: p})
				r.Observe(statelog.Position{Stream: stream, Generation: 1, Seq: 5})
			}
			answer, cov, err := listAll(t, r, 0, "")
			if err != nil {
				t.Fatalf("a gather with four partitions answering failed: %v", err)
			}
			if len(answer.Rows) != 18 {
				t.Errorf("answered %d rows, want the 18 of the partitions that answered",
					len(answer.Rows))
			}
			if len(cov.Missing) != 1 || cov.Missing[0].Partition != "tracker.002" ||
				cov.Missing[0].Reason != tc.want {
				t.Fatalf("missing = %+v, want tracker.002 named %q", cov.Missing, tc.want)
			}
			const notice = "1 of 5 partitions did not answer; this list may be incomplete"
			if cov.Notice() != notice || len(cov.Answered) != 4 {
				t.Errorf("coverage %+v renders %q, want %q", cov, cov.Notice(), notice)
			}
		})
	}
}

// A PARTITION ITS HOLDER FAILED IS ASKED OF ITS NEXT HOLDER, and never again of
// the one that failed it — in the same gather, whatever else that holder is
// asked.
func TestAFailedPartitionMovesOnAndNeverBack(t *testing.T) {
	t.Parallel()
	f := newPartFleet(t, map[string][]statelog.PartitionID{
		"data-a": {tp(0), tp(1), tp(2), tp(3), company},
		"data-b": {tp(0), tp(1), tp(2), tp(3), company},
	})
	r := f.router(t, "agent-1", nil)
	firstName := r.order(tp(2), []string{"data-a", "data-b"})[0]
	first := f.nodes[firstName]
	second := "data-a"
	if firstName == "data-a" {
		second = "data-b"
	}
	first.set(func(n *partNode) { n.holds[tp(2)] = notServing })

	answer, cov, err := listAll(t, r, 0, "")
	if err != nil || !cov.Complete() || len(answer.Rows) != 24 {
		t.Fatalf("gather = (%d rows, %+v, %v), want every row from the next holder",
			len(answer.Rows), cov, err)
	}
	timesAsked := 0
	for _, req := range first.asked() {
		if slices.Contains(req.Partitions, "tracker.002") {
			timesAsked++
		}
	}
	if timesAsked != 1 {
		t.Errorf("%s was asked tracker.002 %d times, want once — it failed it", firstName, timesAsked)
	}
	tookIt := false
	for _, req := range f.nodes[second].asked() {
		tookIt = tookIt || slices.Contains(req.Partitions, "tracker.002")
	}
	if !tookIt {
		t.Errorf("%s, the partition's next holder, was never asked for it", second)
	}
}

// NOTHING ANSWERED IS AN ERROR, never an empty list — the partitions' own
// errors, each keeping its identity — and a gather of ONE partition fails as a
// single-partition read does, with that partition's own error.
func TestAGatherNothingAnsweredIsAnErrorNamingEveryPartition(t *testing.T) {
	t.Parallel()
	f := newPartFleet(t, map[string][]statelog.PartitionID{
		"data-a": {tp(0), tp(1), tp(2), tp(3), company},
	})
	f.nodes["data-a"].set(func(n *partNode) { n.silent = true })
	r := f.router(t, "agent-1", nil)
	_, cov, err := listAll(t, r, 0, "")
	var unserved *ErrPartitionUnserved
	if err == nil || !errors.As(err, &unserved) {
		t.Fatalf("a gather nobody answered = %v, want an error naming the partitions", err)
	}
	for _, p := range []string{"tracker.000", "tracker.003", "company.000"} {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("the error %q does not name %s", err, p)
		}
	}
	if len(cov.Missing) != 5 || len(cov.Answered) != 0 {
		t.Errorf("coverage = %+v, want all five missing", cov)
	}

	// ONE PARTITION: its own error, with its identity.
	one := newPartFleet(t, map[string][]statelog.PartitionID{"data-a": {statelog.EstatePartition}})
	one.placement.layout, one.servers.layout = layoutZero, layoutZero
	one.nodes["data-a"].set(func(n *partNode) { n.holds[statelog.EstatePartition] = failing })
	_, _, err = gather(t.Context(), one.router(t, "agent-1", nil), opTestList,
		statelog.SurfaceSeat, listArgs{Level: statelog.ReadLinearizable})
	if !errors.Is(err, tracker.ErrNoProject) {
		t.Fatalf("a one-partition gather that failed = %v, want its read's own error", err)
	}
}

// A GATHER OF ONE PARTITION IS A SINGLE-PARTITION READ: it asks for the whole
// answer, at the level the read itself names — the request a build before
// gathers sends and answers — so nothing a layout-0 fleet reads changes.
func TestAGatherOfOnePartitionIsASingleRead(t *testing.T) {
	t.Parallel()
	f := newPartFleet(t, map[string][]statelog.PartitionID{"data-a": {statelog.EstatePartition}})
	f.placement.layout, f.servers.layout = layoutZero, layoutZero
	r := f.router(t, "agent-1", nil)
	answer, cov, err := gather(t.Context(), r, opTestList, statelog.SurfaceSeat,
		listArgs{Level: statelog.ReadLinearizable, Limit: 4})
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	asked := f.nodes["data-a"].asked()
	if len(asked) != 1 || asked[0].Slices || asked[0].Level != "" {
		t.Fatalf("asked %+v, want one whole request at the read's own level", asked)
	}
	reads := f.nodes["data-a"].levels[statelog.EstatePartition]
	if len(reads) != 1 || reads[0].Level != statelog.ReadLinearizable {
		t.Errorf("the read ran at %+v, want the seat's own linearizable — a single-"+
			"partition read is not a gather", reads)
	}
	if len(answer.Rows) != 4 || cov.Addressed != 1 ||
		!slices.Equal(cov.Answered, []string{"estate.000"}) || len(cov.At) == 0 {
		t.Errorf("answer %d rows, coverage %+v; want 4 rows, estate.000 answered with its cut",
			len(answer.Rows), cov)
	}

	// AND A REQUEST FROM A BUILD BEFORE GATHERS — no slices, no partitions —
	// is answered whole, as it always was.
	raw, _ := json.Marshal(request{Op: "knowledge.search",
		Args: json.RawMessage(`{"Text":"deploys","Limit":3}`)})
	replies, err := f.client(t).Ask(t.Context(), Subject("data-a"), raw, 1)
	if err != nil || len(replies) != 1 {
		t.Fatalf("ask = (%d, %v)", len(replies), err)
	}
	var rep reply
	if err := json.Unmarshal(replies[0], &rep); err != nil {
		t.Fatal(err)
	}
	var hits []knowledge.Hit
	if err := json.Unmarshal(rep.Result, &hits); err != nil || len(hits) != 1 || rep.Parts != nil {
		t.Fatalf("an older asker's search was answered %+v (%v), want its whole list", rep, err)
	}
}

// A PAGED LIST OF ONE PARTITION IS A SINGLE-PARTITION READ TO ITS LAST PAGE:
// the cursor it answers is the partition's own, unwrapped; the cursor it is
// given is passed through untouched, never decoded as a gathered one; and an
// older build's request carrying its own cursor is answered from it — so a
// rolling upgrade under layout 0 neither repeats page one nor refuses page two
// in either direction.
func TestAPagedListOfOnePartitionKeepsItsOwnCursor(t *testing.T) {
	t.Parallel()
	f := newPartFleet(t, map[string][]statelog.PartitionID{"data-a": {statelog.EstatePartition}})
	f.placement.layout, f.servers.layout = layoutZero, layoutZero
	r := f.router(t, "agent-1", nil)
	list := func(cursor string) tracker.Answer {
		t.Helper()
		answer, _, err := gather(t.Context(), r, opTestList, statelog.SurfaceOperator,
			listArgs{Level: statelog.ReadLinearizable, Limit: 4, Cursor: cursor})
		if err != nil {
			t.Fatalf("page from %q: %v", cursor, err)
		}
		return answer
	}
	first := list("")
	if first.NextCursor != "K03" {
		t.Fatalf("page one's cursor = %q, want the partition's own K03", first.NextCursor)
	}
	// THE CURSOR A BUILD BEFORE GATHERS MINTED, handed to this one.
	second := list("K03")
	if len(second.Rows) != 4 || second.Rows[0].Key != "K04" || second.NextCursor != "K07" {
		t.Fatalf("page two = %v next %q, want K04..K07 next K07", second.Rows, second.NextCursor)
	}
	for _, req := range f.nodes["data-a"].asked() {
		if req.Slices || len(req.Cursors) != 0 {
			t.Errorf("a one-partition list was asked as slices (%+v)", req)
		}
	}

	// AN OLDER ASKER'S REQUEST: its own cursor in its arguments, no
	// partitions, no cursors map.
	raw, _ := json.Marshal(request{Op: opTestList.spec.name,
		Args: json.RawMessage(`{"Level":"stale","Cursor":"K03","Limit":4}`)})
	replies, err := f.client(t).Ask(t.Context(), Subject("data-a"), raw, 1)
	if err != nil || len(replies) != 1 {
		t.Fatalf("ask = (%d, %v)", len(replies), err)
	}
	var rep reply
	var page tracker.Answer
	if err := json.Unmarshal(replies[0], &rep); err != nil || json.Unmarshal(rep.Result, &page) != nil {
		t.Fatalf("decode %s: %v", replies[0], err)
	}
	if len(page.Rows) != 4 || page.Rows[0].Key != "K04" || page.NextCursor != "K07" {
		t.Fatalf("an older asker's page two = %v next %q, want K04..K07 next K07",
			page.Rows, page.NextCursor)
	}
}

// AN OPERATOR'S GATHER IS LINEARIZABLE: each holder appends one barrier on each
// of its partitions' logs that keeps a read index, applies through it, and
// answers at or after it — a `session` read floored at the barrier, never a
// second barrier — with the barriers as the cut it answered at.
func TestAnOperatorsGatherBarriersEachPartitionsLog(t *testing.T) {
	t.Parallel()
	f := newPartFleet(t, map[string][]statelog.PartitionID{
		"data-a": {tp(0), tp(1), tp(2), tp(3), company},
	})
	r := f.router(t, "agent-1", nil)
	_, cov, err := listAll(t, r, 0, "")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	node := f.nodes["data-a"]
	node.mu.Lock()
	barriers := slices.Clone(node.barriers)
	levels := node.levels
	node.mu.Unlock()
	if len(barriers) != 5 {
		t.Fatalf("barriers on %v, want one on each of the five tracker logs", barriers)
	}
	for _, p := range []statelog.PartitionID{tp(0), tp(3), company} {
		stream, _ := dividedLayout.Stream(statelog.LogID{Domain: trackerDomain, Partition: p})
		reads := levels[p]
		if len(reads) != 1 || reads[0].Level != statelog.ReadSession || reads[0].Session.Seq != 99 ||
			reads[0].Session.Stream != stream {
			t.Errorf("%s was read %+v, want once at session floored at its barrier", p, reads)
		}
		if cov.At[stream].Seq != 99 {
			t.Errorf("the cut holds %v for %s, want its barrier", cov.At[stream], stream)
		}
	}
	// THE VECTORS' LOG KEEPS NO READ INDEX, so the cut holds where it was.
	vectors, _ := dividedLayout.Stream(statelog.LogID{Domain: vectorsDomain, Partition: tp(0)})
	if cov.At[vectors].Seq != 10 {
		t.Errorf("the cut holds %v for the vectors' log, want where it was applied", cov.At[vectors])
	}
}

// A SEAT'S GATHER IS SESSION, floored at what the node has observed on each
// partition's log — its own writes, and the trigger that woke its turn — and
// appends no barrier at all.
func TestASeatsGatherReadsEachPartitionAtItsOwnFloor(t *testing.T) {
	t.Parallel()
	f := newPartFleet(t, map[string][]statelog.PartitionID{
		"data-a": {tp(0), tp(1), tp(2), tp(3), company},
	})
	r := f.router(t, "agent-1", nil)
	stream, _ := dividedLayout.Stream(statelog.LogID{Domain: trackerDomain, Partition: tp(1)})
	trigger := statelog.Position{Stream: stream, Generation: 1, Seq: 42}
	r.Observe(trigger)
	if _, _, err := gather(t.Context(), r, opTestList, statelog.SurfaceSeat,
		listArgs{Level: statelog.LevelFor(statelog.SurfaceSeat, "")}); err != nil {
		t.Fatalf("gather: %v", err)
	}
	node := f.nodes["data-a"]
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.barriers) != 0 {
		t.Errorf("a seat's gather appended barriers on %v", node.barriers)
	}
	if got := node.levels[tp(1)]; len(got) != 1 || got[0].Level != statelog.ReadSession ||
		got[0].Session != trigger {
		t.Errorf("tracker.001 was read %+v, want session floored at the trigger %v", got, trigger)
	}
	if got := node.levels[tp(2)]; len(got) != 1 || got[0].Level != statelog.ReadSession ||
		!got[0].Session.IsZero() {
		t.Errorf("tracker.002 was read %+v, want session with no floor of another log's", got)
	}
	for _, req := range node.requests {
		if req.Level != statelog.ReadSession {
			t.Errorf("a slice was asked at %q, want session", req.Level)
		}
	}
}

// THIS NODE'S OWN PARTITIONS ARE ANSWERED IN-PROCESS, at most GOMAXPROCS at a
// time, and only the rest are asked of the fleet.
func TestThisNodesPartitionsAreAnsweredInProcess(t *testing.T) {
	t.Parallel()
	f := newPartFleet(t, map[string][]statelog.PartitionID{
		"data-a": {tp(0), tp(1)},
		"data-b": {tp(2), tp(3), company},
	})
	local := f.nodes["data-a"]
	r := f.router(t, "data-a", local)
	answer, cov, err := listAll(t, r, 0, "")
	if err != nil || !cov.Complete() || len(answer.Rows) != 24 {
		t.Fatalf("gather = (%d rows, %+v, %v)", len(answer.Rows), cov, err)
	}
	if asked := local.asked(); len(asked) != 0 {
		t.Errorf("this node was asked over the broker for its own partitions: %+v", asked)
	}
	if asked := f.nodes["data-b"].asked(); len(asked) != 1 {
		t.Errorf("data-b was asked %d times, want once for its three partitions", len(asked))
	}
}

// THE IN-PROCESS PARTITIONS RUN CONCURRENTLY, AND NO MORE THAN THE CPUS AT
// ONCE: each is a read of a local file, so more at once only queues on the CPU.
func TestLocalPartitionsRunConcurrentlyWithinTheCPUs(t *testing.T) {
	t.Parallel()
	wide := statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: 32, Domains: []string{"tracker"}},
	}}
	var all []statelog.PartitionID
	for i := range uint16(32) {
		all = append(all, tp(i))
	}
	f := newPartFleet(t, map[string][]statelog.PartitionID{"data-a": all})
	f.placement.layout, f.servers.layout = wide, wide
	local := f.nodes["data-a"]
	gate := make(chan struct{})
	local.set(func(n *partNode) { n.gate = gate })
	r := f.router(t, "data-a", local)
	done := make(chan error, 1)
	go func() {
		_, _, err := listAll(t, r, 0, "")
		done <- err
	}()
	cpus := int32(runtime.GOMAXPROCS(0))
	deadline := time.Now().Add(5 * time.Second)
	for local.inFlight.Load() < min(cpus, 32) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("gather: %v", err)
	}
	if peak := local.peak.Load(); peak != min(cpus, 32) {
		t.Errorf("%d partitions read at once, want %d — every CPU and no more", peak, min(cpus, 32))
	}
}

// ceilingFleet is one data node, data-a, holding every partition of the
// divided layout and answering under a reply ceiling of ceiling bytes, which
// the case may change through the server it is handed.
func ceilingFleet(t *testing.T, ceiling int) (*partFleet, *partNode, *server) {
	t.Helper()
	f := newPartFleet(t, nil)
	node := &partNode{name: "data-a", holds: map[statelog.PartitionID]trouble{}}
	for _, p := range []statelog.PartitionID{tp(0), tp(1), tp(2), tp(3), company} {
		node.holds[p] = answers
		f.placement.holders[p] = []string{"data-a"}
		f.servers.holders[p] = []string{"data-a"}
	}
	f.nodes["data-a"] = node
	srv := &server{self: "data-a", local: node, placement: f.servers, ceiling: ceiling}
	stop, err := recorder{q: f.client(t), node: node}.Serve(t.Context(), Subject("data-a"),
		func(ctx context.Context, raw []byte) ([]byte, error) { return srv.answer(ctx, raw), nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	return f, node, srv
}

// A HOLDER WAITS OUT EVERY PARTITION'S FLOOR AT ONCE, and only the queries
// take a CPU's place: a wait is not CPU work, and a place held through one
// queued a batch's every partition behind the slowest log's applier — on one
// CPU, five floors waited one after another.
func TestAHolderWaitsOutABatchsFloorsAtOnce(t *testing.T) {
	t.Parallel()
	f, node, srv := ceilingFleet(t, queue.MaxPayloadBytes)
	srv.cpus = 1
	gate := make(chan struct{})
	node.set(func(n *partNode) { n.floorGate = gate })
	r := f.router(t, "agent-1", nil)
	r.readBudget = 10 * time.Second
	observeEveryTrackerLog(r)
	done := make(chan error, 1)
	go func() {
		_, cov, err := listAll(t, r, 0, "")
		if err == nil && !cov.Complete() {
			err = fmt.Errorf("coverage %+v", cov)
		}
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for node.waiting.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	waited := node.waiting.Load()
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("gather: %v", err)
	}
	if waited != 5 {
		t.Errorf("%d of the batch's five floors were waited for at once, want all five", waited)
	}
}

// THIS NODE WAITS OUT ITS OWN PARTITIONS' FLOORS AT ONCE TOO, more of them
// than it has CPUs: only the in-process queries take a CPU's place.
func TestThisNodeWaitsOutItsOwnPartitionsFloorsAtOnce(t *testing.T) {
	t.Parallel()
	count := min(runtime.GOMAXPROCS(0)+2, statelog.MaxPartitions)
	wide := statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: count, Domains: []string{"tracker"}},
	}}
	var all []statelog.PartitionID
	for i := range count {
		all = append(all, tp(uint16(i)))
	}
	f := newPartFleet(t, map[string][]statelog.PartitionID{"data-a": all})
	f.placement.layout, f.servers.layout = wide, wide
	local := f.nodes["data-a"]
	gate := make(chan struct{})
	local.set(func(n *partNode) { n.floorGate = gate })
	r := f.router(t, "data-a", local)
	for _, p := range all {
		stream, _ := wide.Stream(statelog.LogID{Domain: trackerDomain, Partition: p})
		r.Observe(statelog.Position{Stream: stream, Generation: 1, Seq: 5})
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := listAll(t, r, 0, "")
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for local.waiting.Load() < int32(count) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	waited := local.waiting.Load()
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("gather: %v", err)
	}
	if waited != int32(count) {
		t.Errorf("%d of this node's %d floors were waited for at once, want all of them",
			waited, count)
	}
}

// A BATCH IS ANSWERED BEFORE ITS ASKER STOPS WAITING: the partitions behind
// the asker's floor are named behind, the ones that answered at once are in
// the answer, and the holder — which answered — is never suspected, all in one
// attempt. Answered at the asker's own deadline, the reply arrived after it,
// and every partition of the batch was lost with a healthy holder suspected.
func TestABatchIsAnsweredBeforeTheAskerStopsWaiting(t *testing.T) {
	t.Parallel()
	f, node, srv := ceilingFleet(t, queue.MaxPayloadBytes)
	srv.cpus = 1
	node.set(func(n *partNode) {
		for _, p := range []statelog.PartitionID{tp(1), tp(2), tp(3)} {
			n.holds[p] = behindFloor
		}
	})
	r := f.router(t, "agent-1", nil)
	// LONG ENOUGH FOR THE HEALTHY PARTITIONS, SHORT OF THE FLOOR WAIT: the
	// batch can only finish two of its five within the attempt.
	r.readBudget = statelog.ReadBudget / 2
	observeEveryTrackerLog(r)
	answer, cov, err := listAll(t, r, 0, "")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(answer.Rows) != 6 || !slices.Equal(cov.Answered, []string{"company.000", "tracker.000"}) {
		t.Errorf("answered %d rows from %v, want tracker.000's six and company.000",
			len(answer.Rows), cov.Answered)
	}
	for _, m := range cov.Missing {
		if m.Reason != statelog.MissingBehind {
			t.Errorf("%s is missing as %q (%s), want behind", m.Partition, m.Reason, m.Detail)
		}
	}
	if len(cov.Missing) != 3 {
		t.Errorf("missing = %+v, want the three partitions behind the floor", cov.Missing)
	}
	if asked := node.asked(); len(asked) != 1 {
		t.Errorf("the holder was asked %d times, want once", len(asked))
	}
	r.mu.Lock()
	_, suspected := r.suspect["data-a"]
	r.mu.Unlock()
	if suspected {
		t.Error("the holder that answered the batch was suspected")
	}
}

// WHAT A HOLDER DID NOT FINISH IS ASKED OF IT AGAIN: a batch whose queries
// take longer than one attempt is answered with what finished and the rest
// named unfinished, and the rest is asked again — of the same holder, which
// did nothing wrong — until every partition answered.
func TestWhatAHolderDidNotFinishIsAskedAgain(t *testing.T) {
	t.Parallel()
	f, node, srv := ceilingFleet(t, queue.MaxPayloadBytes)
	srv.cpus = 1
	node.set(func(n *partNode) { n.delay = 150 * time.Millisecond })
	r := f.router(t, "agent-1", nil)
	r.readBudget = 400 * time.Millisecond
	answer, cov, err := listAll(t, r, 0, "")
	if err != nil || !cov.Complete() || len(answer.Rows) != 24 {
		t.Fatalf("gather = (%d rows, %+v, %v), want every partition across attempts",
			len(answer.Rows), cov, err)
	}
	if asked := node.asked(); len(asked) < 2 {
		t.Errorf("the batch was answered in %d attempt(s), want it carried over several", len(asked))
	}
	r.mu.Lock()
	_, suspected := r.suspect["data-a"]
	r.mu.Unlock()
	if suspected {
		t.Error("the holder that answered every attempt was suspected")
	}
}

// A HOLDER RUNS A BATCH'S QUERIES AT MOST ITS CPUS AT A TIME: one request
// naming more partitions than the node has CPUs never has more of their reads
// in flight at once.
func TestAHolderRunsABatchsQueriesWithinItsCPUs(t *testing.T) {
	t.Parallel()
	cpus := runtime.GOMAXPROCS(0)
	count := min(2*cpus+2, statelog.MaxPartitions)
	wide := statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: count, Domains: []string{"tracker"}},
	}}
	var all []statelog.PartitionID
	for i := range count {
		all = append(all, tp(uint16(i)))
	}
	f := newPartFleet(t, map[string][]statelog.PartitionID{"data-a": all})
	f.placement.layout, f.servers.layout = wide, wide
	node := f.nodes["data-a"]
	gate := make(chan struct{})
	node.set(func(n *partNode) { n.gate = gate })
	r := f.router(t, "agent-1", nil)
	r.readBudget = 10 * time.Second
	done := make(chan error, 1)
	go func() {
		_, _, err := listAll(t, r, 0, "")
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for node.inFlight.Load() < int32(cpus) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("gather: %v", err)
	}
	if asked := node.asked(); len(asked) != 1 {
		t.Fatalf("the holder was asked %d times, want one batch", len(asked))
	}
	if peak := node.peak.Load(); peak != int32(cpus) {
		t.Errorf("%d of the batch's %d reads ran at once, want %d — every CPU and no more",
			peak, count, cpus)
	}
}

// observeEveryTrackerLog gives r a floor on every tracker log of the divided
// layout, which is what a holder behind is behind.
func observeEveryTrackerLog(r *Router) {
	for _, p := range []statelog.PartitionID{tp(0), tp(1), tp(2), tp(3), company} {
		stream, _ := dividedLayout.Stream(statelog.LogID{Domain: trackerDomain, Partition: p})
		r.Observe(statelog.Position{Stream: stream, Generation: 1, Seq: 5})
	}
}

// A BATCH THAT OUTGROWS ONE REPLY IS ANSWERED IN PAGES: what fits, then what
// did not, asked again of the same holder — and a slice too large to fit even alone is the
// error that says so, naming the partition, never a silence.
func TestABatchThatOutgrowsTheReplyIsAnsweredInPages(t *testing.T) {
	t.Parallel()
	// ROOM FOR TWO OF THE FOUR FULL SLICES (about 1.25 KiB each) beside the
	// envelope.
	f, node, srv := ceilingFleet(t, 2700)
	r := f.router(t, "agent-1", nil)
	answer, cov, err := listAll(t, r, 0, "")
	if err != nil || !cov.Complete() || len(answer.Rows) != 24 {
		t.Fatalf("gather = (%d rows, %+v, %v), want every row across pages", len(answer.Rows), cov, err)
	}
	if asked := node.asked(); len(asked) < 2 {
		t.Errorf("the batch was answered in %d request(s), want it paged over several", len(asked))
	}

	// A SLICE TOO LARGE EVEN ALONE: the four full partitions are each the
	// error naming the size, and the empty one still answers.
	srv.ceiling = 600
	_, cov, err = listAll(t, r, 0, "")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(cov.Missing) != 4 || !slices.Equal(cov.Answered, []string{"company.000"}) {
		t.Fatalf("coverage = %+v, want the four full partitions missing", cov)
	}
	for _, m := range cov.Missing {
		if m.Reason != statelog.MissingError || !strings.Contains(m.Detail, "narrow the read") {
			t.Errorf("%s is missing as %q (%s), want the error naming its size",
				m.Partition, m.Reason, m.Detail)
		}
	}
	// AND WHEN THAT IS EVERY PARTITION, the gather is that error.
	narrow := statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: 4, Domains: []string{"tracker"}},
	}}
	for _, view := range []*partPlacement{f.placement, f.servers} {
		view.mu.Lock()
		view.layout = narrow
		view.mu.Unlock()
	}
	if _, _, err := listAll(t, r, 0, ""); !errors.Is(err, queue.ErrTooLarge) {
		t.Errorf("err = %v, want queue.ErrTooLarge naming the size", err)
	}
}

// A SLICE TOO LARGE TO SEND BESIDE ONE ITS HOLDER DID NOT FINISH IS ITS SIZE
// ERROR, and the gather ends: a reply in which the holder decided anything
// carries a decided partition. The
// note of a partition the holder did not finish decides nothing — the asker
// asks it again — and counted as what made the reply progress, it kept an
// oversized slice from ever being answered as its error, so the same batch
// went to the same holder once an attempt until the caller stopped waiting,
// and the slice was named unreachable rather than too large.
func TestAnOversizedSliceBesideAnUnfinishedOneIsItsSizeError(t *testing.T) {
	t.Parallel()
	// EVERY TRACKER SLICE IS OVER THE CEILING (about 1.25 KiB each against
	// 600 bytes), the company's empty one fits, and an unfinished
	// partition's note fits too.
	f, node, srv := ceilingFleet(t, 600)
	srv.cpus = 2
	two := statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: 2, Domains: []string{"tracker"}},
		{Space: statelog.SpaceCompany, Partitions: 1, Domains: []string{"tracker"}},
	}}
	for _, view := range []*partPlacement{f.placement, f.servers} {
		view.mu.Lock()
		view.layout = two
		view.mu.Unlock()
	}
	node.set(func(n *partNode) { n.holds[tp(1)] = stalls })
	r := f.router(t, "agent-1", nil)
	r.readBudget = 400 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, cov, err := gather(ctx, r, opTestList, statelog.SurfaceOperator,
		listArgs{Level: statelog.ReadLinearizable})
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if !slices.Equal(cov.Answered, []string{"company.000"}) || len(cov.Missing) != 2 {
		t.Fatalf("coverage = %+v, want company.000 answered and both tracker partitions missing", cov)
	}
	for _, m := range cov.Missing {
		switch m.Partition {
		case "tracker.000":
			if m.Reason != statelog.MissingError || !strings.Contains(m.Detail, "narrow the read") {
				t.Errorf("tracker.000 is missing as %q (%s), want the error naming its size",
					m.Reason, m.Detail)
			}
		case "tracker.001":
			if m.Reason != statelog.MissingUnreachable || strings.Contains(m.Detail, "stopped waiting") {
				t.Errorf("tracker.001 is missing as %q (%s), want unfinished on its holder, "+
					"before the caller stopped waiting", m.Reason, m.Detail)
			}
		}
	}
	if asked := node.asked(); len(asked) != 2 {
		t.Errorf("the holder was asked %d times, want twice — the batch, then the "+
			"partition it did not finish", len(asked))
	}
}

// EVERY SLICE THAT CAN NEVER FIT IS ITS SIZE ERROR IN ONE REPLY, beside what
// does fit: asked again, it would fit in no smaller batch either, and one
// such error a reply re-ran every other oversized slice's read once a reply.
func TestEverySliceThatCanNeverFitIsItsErrorAtOnce(t *testing.T) {
	t.Parallel()
	const ceiling = 8 << 10
	big, _ := json.Marshal(strings.Repeat("r", 10<<10))
	parts := []partReply{{Partition: tp(0).String(), Result: big}}
	for i := 1; i < 4; i++ {
		parts = append(parts, partReply{Partition: tp(uint16(i)).String(), Result: big})
	}
	parts = append(parts, partReply{Partition: company.String(), Result: json.RawMessage(`{}`)})
	got := fitParts("data-a", ceiling, reply{Node: "data-a"}, parts)
	for _, part := range got[:4] {
		if part.Err == nil || !errors.Is(decodeError(part.Err), queue.ErrTooLarge) {
			t.Errorf("%s = %+v, want the error naming its size", part.Partition, part)
		}
	}
	if got[4].Result == nil {
		t.Errorf("%s = %+v, want its answer, which fits", got[4].Partition, got[4])
	}
	if size := len(encodeReply(reply{Node: "data-a", Parts: got})); size > ceiling {
		t.Errorf("the reply is %d bytes, over its %d ceiling", size, ceiling)
	}
}

// A REPLY DECIDES A PARTITION EVEN WHEN NO DECISION FITS BESIDE THE BATCH: a
// slice that would fit alone but not beside the other partitions' notes is
// answered as its size error, since a reply of notes alone decides nothing and
// is the same batch asked again — while what the holder did not finish is a
// note asking for it again.
func TestAReplyDecidesAPartitionWhenNoDecisionFits(t *testing.T) {
	t.Parallel()
	slice, _ := json.Marshal(strings.Repeat("r", 1<<10))
	whole := partReply{Partition: tp(0).String(), Result: slice}
	// ROOM FOR THE SLICE ALONE, and short of it beside the notes of the
	// partitions the holder did not finish.
	ceiling := len(encodeReply(reply{Node: "data-a", Parts: []partReply{whole}})) + 20
	parts := []partReply{whole}
	for i := 1; i < 4; i++ {
		parts = append(parts, partReply{Partition: tp(uint16(i)).String(), Unserved: unservedUnfinished,
			Detail: "data-a had not finished it when the batch had to be answered"})
	}
	got := fitParts("data-a", ceiling, reply{Node: "data-a"}, parts)
	if got[0].Err == nil || !errors.Is(decodeError(got[0].Err), queue.ErrTooLarge) {
		t.Fatalf("%s = %+v, want the error naming its size", got[0].Partition, got[0])
	}
	for _, part := range got[1:] {
		if part.decisive() {
			t.Errorf("%s = %+v, want it asked for again", part.Partition, part)
		}
	}
}

// A REPLY THAT DECIDED NOTHING MOVES EVERY PARTITION ON: what the holder could
// not finish or could not fit is asked of it again only beside a partition the
// reply decided. Beside none, the same batch would be answered the same way —
// once an attempt until the caller stopped waiting, and a caller with no
// deadline for ever.
func TestAReplyThatDecidedNothingMovesEveryPartitionOn(t *testing.T) {
	t.Parallel()
	f := newPartFleet(t, nil)
	node := &partNode{name: "data-x", holds: map[statelog.PartitionID]trouble{}}
	all := []statelog.PartitionID{tp(0), tp(1), tp(2), tp(3), company}
	for _, p := range all {
		f.placement.holders[p] = []string{"data-x"}
	}
	stop, err := recorder{q: f.client(t), node: node}.Serve(t.Context(), Subject("data-x"),
		func(_ context.Context, raw []byte) ([]byte, error) {
			var req request
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, err
			}
			// THE FIRST DID NOT FIT, AND THE REST DID NOT FINISH.
			out := reply{Node: "data-x"}
			for i, name := range req.Partitions {
				part := partReply{Partition: name, Unserved: unservedUnfinished,
					Detail: "data-x had not finished it"}
				if i == 0 {
					part = partReply{Partition: name, Unserved: unservedOverflow}
				}
				out.Parts = append(out.Parts, part)
			}
			return encodeReply(out), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, cov, err := gather(ctx, f.router(t, "agent-1", nil), opTestList, statelog.SurfaceOperator,
		listArgs{Level: statelog.ReadLinearizable})
	if err == nil {
		t.Fatalf("gather answered with %+v, want the error naming every partition", cov)
	}
	if len(cov.Missing) != len(all) {
		t.Fatalf("missing = %+v, want all %d partitions", cov.Missing, len(all))
	}
	for _, m := range cov.Missing {
		if m.Reason != statelog.MissingUnreachable || strings.Contains(m.Detail, "stopped waiting") {
			t.Errorf("%s is missing as %q (%s), want the holder's own answer, before the "+
				"caller stopped waiting", m.Partition, m.Reason, m.Detail)
		}
	}
	if asked := node.asked(); len(asked) != 1 {
		t.Errorf("the holder was asked %d times, want once", len(asked))
	}
}

// A READ THAT KEEPS GOING AFTER IT IS TOLD TO STOP IS NAMED, NOT ITS BATCH: a
// wait or a query that gives up when told to reports before the batch is
// answered, so a partition with no report by then is a read that ignored the
// stop — and alone in its batch, a detail blaming the batch's size points at a
// remedy, a smaller batch, that a batch of one has already taken.
func TestAReadThatKeepsGoingIsNamedRatherThanItsBatch(t *testing.T) {
	t.Parallel()
	f := newPartFleet(t, map[string][]statelog.PartitionID{
		"data-a": {tp(0), tp(2), tp(3), company},
		"data-b": {tp(1)},
	})
	// THE GATE IGNORES THE READ'S CONTEXT, holding it past the batch's
	// answer until the case ends.
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	f.nodes["data-b"].set(func(n *partNode) { n.gate = gate })
	r := f.router(t, "agent-1", nil)
	r.readBudget = time.Second
	_, cov, err := listAll(t, r, 0, "")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(cov.Missing) != 1 || cov.Missing[0].Partition != "tracker.001" {
		t.Fatalf("missing = %+v, want tracker.001 alone", cov.Missing)
	}
	m := cov.Missing[0]
	if m.Reason != statelog.MissingUnreachable ||
		!strings.Contains(m.Detail, "read of tracker.001 had not returned") ||
		!strings.Contains(m.Detail, "after it was told to stop") ||
		strings.Contains(m.Detail, "batch of") {
		t.Errorf("tracker.001 is missing as %q (%s), want unreachable, naming the read that "+
			"kept going after it was told to stop rather than its batch", m.Reason, m.Detail)
	}
	if asked := f.nodes["data-b"].asked(); len(asked) != 1 {
		t.Errorf("data-b was asked %d times, want once — a reply deciding nothing moves "+
			"its partition on", len(asked))
	}
}

// A REPLY FITS ITS CEILING WITH ITS WHOLE ENVELOPE: the slices are fitted
// beside what the envelope measures, never beside a fixed allowance for it —
// a batch whose floors a reanchor made obsolete names a stream for each, and a
// reply over the ceiling is one the broker refuses, every partition of the
// batch with it.
func TestAReplyFitsItsCeilingWithItsWholeEnvelope(t *testing.T) {
	t.Parallel()
	// A BATCH OF TWO HUNDRED SMALL SLICES, about half of which fit: the
	// envelope's obsolete floors are several kibibytes, and so are the
	// notes of the slices that do not fit.
	const ceiling = 40 << 10
	envelope := reply{Node: "data-a"}
	for i := range 200 {
		envelope.Obsolete = append(envelope.Obsolete, fmt.Sprintf("CREWLET_L1_TRACKER_%03d_TRACKER", i))
	}
	var parts []partReply
	for i := range 200 {
		rows := make([]string, 0, 4)
		for range cap(rows) {
			rows = append(rows, strings.Repeat("r", 64))
		}
		raw, _ := json.Marshal(rows)
		parts = append(parts, partReply{Partition: tp(uint16(i)).String(), Result: raw})
	}
	envelope.Parts = fitParts("data-a", ceiling, envelope, parts)
	encoded := encodeReply(envelope)
	if len(encoded) > ceiling {
		t.Fatalf("the reply is %d bytes, over its %d ceiling", len(encoded), ceiling)
	}
	overflowed := 0
	for _, part := range envelope.Parts {
		if part.Unserved == unservedOverflow {
			overflowed++
		}
	}
	if overflowed == 0 || overflowed == len(parts) {
		t.Errorf("%d of %d slices overflowed, want some fitted and some asked again",
			overflowed, len(parts))
	}
}

// A LIST PAGED ACROSS PARTITIONS RESUMES EACH PARTITION WHERE IT LEFT OFF:
// every row exactly once, in the list's own order — and a partition missing
// from one page is not a partition out of rows.
func TestAPagedGatherResumesEachPartitionWhereItLeftOff(t *testing.T) {
	t.Parallel()
	f := newPartFleet(t, map[string][]statelog.PartitionID{
		"data-a": {tp(0), tp(1), tp(2), tp(3), company},
	})
	r := f.router(t, "agent-1", nil)
	var seen []string
	cursor := ""
	for page := 0; ; page++ {
		if page == 2 {
			// ONE PARTITION GOES MISSING FOR A PAGE.
			f.nodes["data-a"].set(func(n *partNode) { n.holds[tp(2)] = failing })
		}
		if page == 3 {
			f.nodes["data-a"].set(func(n *partNode) { n.holds[tp(2)] = answers })
		}
		answer, _, err := listAll(t, r, 5, cursor)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, row := range answer.Rows {
			seen = append(seen, row.Key)
		}
		if answer.NextCursor == "" {
			break
		}
		cursor = answer.NextCursor
		if page > 20 {
			t.Fatal("the pages never ended")
		}
	}
	var want []string
	for n := range 24 {
		want = append(want, fmt.Sprintf("K%02d", n))
	}
	sorted := slices.Sorted(slices.Values(seen))
	if !slices.Equal(sorted, want) {
		t.Fatalf("paged through %v, want every row exactly once", seen)
	}
}

// A CURSOR THIS LIST DID NOT MINT IS REFUSED BY NAME, never read as the start
// and never as an empty last page: a partition's own cursor handed to a list
// across several, one another list minted, one naming a partition this list
// does not address (another layout's), and one naming none.
func TestAGatherCursorItDidNotMintIsRefused(t *testing.T) {
	t.Parallel()
	f := newPartFleet(t, map[string][]statelog.PartitionID{
		"data-a": {tp(0), tp(1), tp(2), tp(3), company},
	})
	r := f.router(t, "agent-1", nil)
	for name, cursor := range map[string]string{
		"a partition's own": "K07",
		"another list's": encodeGatherCursor("knowledge.search",
			map[statelog.PartitionID]string{tp(1): "K05"}),
		"another layout's": encodeGatherCursor(opTestList.spec.name,
			map[statelog.PartitionID]string{pp(0): "K05"}),
		"one naming none": gatherCursorVersion + base64.RawURLEncoding.EncodeToString(
			[]byte(`{"op":"test.list","parts":{}}`)),
	} {
		answer, _, err := listAll(t, r, 5, cursor)
		if !errors.Is(err, ErrBadCursor) {
			t.Errorf("%s cursor = (%d rows, %v), want ErrBadCursor", name, len(answer.Rows), err)
		}
	}
	next := map[statelog.PartitionID]string{tp(0): "", tp(3): "K11"}
	got, err := decodeGatherCursor(opTestList.spec.name, encodeGatherCursor(opTestList.spec.name, next))
	if err != nil || len(got) != 2 || got[tp(3)] != "K11" || got[tp(0)] != "" {
		t.Fatalf("the cursor read back as (%v, %v), want %v", got, err, next)
	}
}

// A NOT-HOLDER FROM A NEWER MAP REFRESHES THE GATHER'S VIEW ONCE, and the
// partition is asked of the holder the fresh map names.
func TestANotHolderFromANewerMapRefreshesTheGather(t *testing.T) {
	t.Parallel()
	f := newPartFleet(t, map[string][]statelog.PartitionID{
		"data-a": {tp(0), tp(1), tp(2), tp(3), company},
		"data-b": {tp(2)},
	})
	f.placement.mu.Lock()
	f.placement.holders[tp(2)] = []string{"data-a"}
	f.placement.epoch = 3
	f.placement.onRefresh = func(p *partPlacement) {
		p.holders[tp(2)] = []string{"data-b"}
		p.epoch = 4
	}
	f.placement.mu.Unlock()
	// THE SERVER'S OWN VIEW IS NEWER than the asker's: tracker.002 moved.
	f.servers.mu.Lock()
	f.servers.epoch = 4
	f.servers.mu.Unlock()
	f.nodes["data-a"].set(func(n *partNode) { n.holds[tp(2)] = notServing })
	r := f.router(t, "agent-1", nil)
	answer, cov, err := listAll(t, r, 0, "")
	if err != nil || len(answer.Rows) != 24 || !cov.Complete() {
		t.Fatalf("gather = (%d rows, %+v, %v), want tracker.002 from the fresh map's holder",
			len(answer.Rows), cov, err)
	}
	if f.placement.refreshes != 1 {
		t.Errorf("refreshed %d times, want once", f.placement.refreshes)
	}
}

// A COPY THAT LAGS IS ASKED LAST, never refused for lagging: the partition's
// only holder lags, and is asked again to answer anyway.
func TestAGatherAsksALaggingCopyLastRatherThanMissingIt(t *testing.T) {
	t.Parallel()
	f := newPartFleet(t, map[string][]statelog.PartitionID{
		"data-a": {tp(0), tp(1), tp(2), tp(3), company},
	})
	f.nodes["data-a"].set(func(n *partNode) { n.holds[tp(1)] = lags })
	answer, cov, err := listAll(t, f.router(t, "agent-1", nil), 0, "")
	if err != nil || !cov.Complete() || len(answer.Rows) != 24 {
		t.Fatalf("gather = (%d rows, %+v, %v), want the lagging copy's rows too",
			len(answer.Rows), cov, err)
	}
	var accepted bool
	for _, req := range f.nodes["data-a"].asked() {
		accepted = accepted || (req.AcceptLagging && slices.Equal(req.Partitions, []string{"tracker.001"}))
	}
	if !accepted {
		t.Error("the lagging copy was never asked to answer anyway")
	}
}

// THE LAST RESORT ASKS ONE LAGGING COPY AT A TIME, and the first answer that
// settles the partition is its answer: the next copy is never asked, so it
// can neither run the read for nothing nor overwrite the rows with its own
// failure.
func TestTheLastResortAsksOneLaggingCopyAtATime(t *testing.T) {
	t.Parallel()
	f := newPartFleet(t, map[string][]statelog.PartitionID{
		"data-a": {tp(0), tp(1), tp(2), tp(3), company},
		"data-b": {tp(1)},
	})
	r := f.router(t, "agent-1", nil)
	order := r.order(tp(1), []string{"data-a", "data-b"})
	first, second := f.nodes[order[0]], f.nodes[order[1]]
	first.set(func(n *partNode) { n.holds[tp(1)] = lags })
	second.set(func(n *partNode) { n.holds[tp(1)] = lagsFailing })

	answer, cov, err := listAll(t, r, 0, "")
	if err != nil || !cov.Complete() || len(answer.Rows) != 24 {
		t.Fatalf("gather = (%d rows, %+v, %v), want tracker.001 answered by %s, the first "+
			"lagging copy", len(answer.Rows), cov, err, first.name)
	}
	for _, req := range second.asked() {
		if req.AcceptLagging {
			t.Errorf("%s, the second lagging copy, was told to answer anyway (%v) after the "+
				"first had answered", second.name, req.Partitions)
		}
	}
}

// A PARTITION IS SETTLED ONCE: the first answer stands, and a later one — a
// failure above all — never discards the rows it settled with.
func TestAPartitionIsSettledOnce(t *testing.T) {
	t.Parallel()
	st := &partState{p: tp(1), tried: map[string]bool{}}
	st.settle(tracker.Answer{Rows: rowsOf(tp(1))}, nil, nil)
	st.settle(nil, nil, errors.New("the read of tracker.001 failed on data-b"))
	if m := st.missing(); m != nil {
		t.Fatalf("a settled partition is missing as %+v after a second answer", m)
	}
	if got, _ := st.value.(tracker.Answer); len(got.Rows) != 6 {
		t.Errorf("the settled rows were replaced: %+v", st.value)
	}
}

// A LAGGING COPY'S ANSWER THAT DID NOT FIT IS ASKED AGAIN, as any holder's
// is: the last resort is a round like the others, not one last batch whose
// overflow is lost.
func TestALaggingCopysOverflowIsAskedAgain(t *testing.T) {
	t.Parallel()
	f, node, _ := ceilingFleet(t, 2700)
	for p := range node.holds {
		node.holds[p] = lags
	}
	answer, cov, err := listAll(t, f.router(t, "agent-1", nil), 0, "")
	if err != nil || !cov.Complete() || len(answer.Rows) != 24 {
		t.Fatalf("gather = (%d rows, %+v, %v), want every row of the lagging copy across pages",
			len(answer.Rows), cov, err)
	}
}

// THE SEARCHES ARE GATHERS over their corpus — the knowledge base's over the
// pages space, the work items' over the tracker space's partitions that index
// them — fused across partitions, with what did not answer named.
func TestTheSearchesAreGathersOverTheirCorpus(t *testing.T) {
	t.Parallel()
	f := newPartFleet(t, map[string][]statelog.PartitionID{
		"data-a": {tp(0), tp(1), pp(0), company},
		"data-b": {tp(2), tp(3), pp(1)},
	})
	r := f.router(t, "agent-1", nil)
	got := r.Knowledge().Search(t.Context(), knowledge.Query{Text: "deploys", Limit: 5})
	if !got.Coverage.Complete() || got.Coverage.Addressed != 2 || len(got.Hits) != 2 {
		t.Fatalf("knowledge search = %+v, want both pages partitions' hits", got)
	}
	// FUSED BY SCORE, so pages.001's stronger candidate ranks first.
	if got.Hits[0].Title != "pages.001" {
		t.Errorf("the fused order is %v, want pages.001 first", got.Hits)
	}
	if !r.Knowledge().Building(t.Context()) {
		t.Error("pages.001's index is building and the search said it was not")
	}
	// THE WORK ITEMS' CORPUS IS THE TRACKER SPACE'S, and not the company
	// space's catalogue, which carries the tracker's log and indexes nothing.
	work, err := r.Work().Search(t.Context(), "retry backoff", 10)
	if err != nil || work.Coverage.Addressed != 4 || len(work.Hits) != 4 ||
		slices.Contains(work.Coverage.Answered, "company.000") {
		t.Fatalf("work search = (%+v, %v), want the four tracker partitions' hits", work, err)
	}
	if work.Hits[0].ID != "tracker.003" || work.Hits[3].Rank != 4 {
		t.Errorf("the fused order is %v, want tracker.003 first and ranks counted 1..4", work.Hits)
	}

	f.nodes["data-b"].set(func(n *partNode) { n.silent = true })
	got = r.Knowledge().Search(t.Context(), knowledge.Query{Text: "deploys", Limit: 5})
	if len(got.Hits) != 1 || len(got.Coverage.Missing) != 1 ||
		got.Coverage.Missing[0].Partition != "pages.001" ||
		got.Coverage.Missing[0].Reason != statelog.MissingUnreachable {
		t.Fatalf("with pages.001's holder gone the search answered %+v, want it named", got)
	}

	// NOTHING ANSWERED: still no "nothing is written down" — the answer
	// names every partition it did not reach, though the search failed.
	f.nodes["data-a"].set(func(n *partNode) { n.silent = true })
	got = r.Knowledge().Search(t.Context(), knowledge.Query{Text: "deploys", Limit: 5})
	if len(got.Hits) != 0 || got.Coverage.Addressed != 2 || len(got.Coverage.Missing) != 2 {
		t.Fatalf("with every holder gone the search answered %+v, want both pages "+
			"partitions named missing", got)
	}
	for _, m := range got.Coverage.Missing {
		if m.Reason != statelog.MissingUnreachable {
			t.Errorf("%s is missing as %q, want unreachable", m.Partition, m.Reason)
		}
	}
}

// A NOT-HOLDER AT THE ASKER'S OWN EPOCH, OR AN OLDER ONE, MOVES ON WITHOUT A
// REFRESH: the holder is the one behind the map — a joiner not serving yet, or
// a node on its way out — so the asker's view is right, and the partition is
// asked of its next holder.
func TestANotHolderFromAnOlderMapMovesOnWithoutARefresh(t *testing.T) {
	t.Parallel()
	for name, serverEpoch := range map[string]uint64{"the same epoch": 3, "an older epoch": 2} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newPartFleet(t, map[string][]statelog.PartitionID{
				"data-a": {tp(0), tp(1), tp(2), tp(3), company},
				"data-b": {tp(2)},
			})
			for _, view := range []*partPlacement{f.placement, f.servers} {
				view.mu.Lock()
				view.epoch = 3
				view.mu.Unlock()
			}
			f.servers.mu.Lock()
			f.servers.epoch = serverEpoch
			f.servers.mu.Unlock()
			r := f.router(t, "agent-1", nil)
			first := r.order(tp(2), []string{"data-a", "data-b"})[0]
			f.nodes[first].set(func(n *partNode) { n.holds[tp(2)] = notServing })
			answer, cov, err := listAll(t, r, 0, "")
			if err != nil || !cov.Complete() || len(answer.Rows) != 24 {
				t.Fatalf("gather = (%d rows, %+v, %v), want tracker.002 from its next holder",
					len(answer.Rows), cov, err)
			}
			f.placement.mu.Lock()
			refreshes := f.placement.refreshes
			f.placement.mu.Unlock()
			if refreshes != 0 {
				t.Errorf("refreshed %d times on a not-holder at %s, want none", refreshes, name)
			}
		})
	}
}

// A SINGLE-PARTITION READ WHOSE ANSWER A GATHER WILL ONE DAY ASSEMBLE SAYS WHAT
// IT COVERED: the one partition it was read from and the cut its holder
// measured before the read began — answered in-process or by a peer alike.
func TestASingleReadSaysWhatItCovered(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	for name, r := range map[string]*Router{
		"by a peer":    f.client,
		"by this node": f.router(t, "data-a", f.nodes["data-a"]),
	} {
		answer, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
		if err != nil {
			t.Fatalf("%s: tasks: %v", name, err)
		}
		cov := answer.Coverage
		if cov.Addressed != 1 || !slices.Equal(cov.Answered, []string{"estate.000"}) ||
			!cov.Complete() || cov.At[trackerStream].Seq != 7 {
			t.Errorf("%s: coverage = %+v, want estate.000 answered at its holder's cut", name, cov)
		}
	}
}
