package estate

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// layoutZero is the one layout this build runs: one partition, three logs.
var layoutZero = statelog.EstateLayout("tracker", "vectors", "pages")

// The stream of layout 0's tracker log, as a floor names it.
var trackerStream = func() string {
	name, _ := layoutZero.Stream(statelog.LogID{Domain: "tracker", Partition: statelog.EstatePartition})
	return name
}()

// fakeNode is one data node's backend, recording what it was asked.
type fakeNode struct {
	TrackerReader
	TrackerWriter
	PageWriter

	mu        sync.Mutex
	name      string
	asked     []string
	floors    []statelog.Position
	actors    []Actor
	opIDs     []string
	patches   []tracker.TaskPatch
	turns     []tracker.TurnRecord
	queries   []knowledge.Query
	units     tracker.Units
	notReady  bool
	behind    bool
	abandoned bool
	silent    bool
	failWith  error
	written   statelog.Position

	// notHolder is a node that does not serve the partition right now,
	// and holdingUnknown one that cannot tell whether it does.
	notHolder, holdingUnknown bool

	// notAdmitting is a copy that serves requests and admits no seat.
	notAdmitting bool

	// unvouched answers every create `unknown`, unvouched by its ledger,
	// and refusal answers every create with itself.
	unvouched bool
	refusal   error

	// release, when set, holds a floor wait until it is closed.
	release chan struct{}

	// hang, when set, is a node that takes a request and never answers it
	// until hang is closed — a wedged node, as the broker's own ask sees
	// one: waited for until the asker's deadline, where a node that is
	// simply gone is known to have answered nothing.
	hang chan struct{}
}

func (f *fakeNode) note(op string) {
	f.mu.Lock()
	f.asked = append(f.asked, op)
	f.mu.Unlock()
}

func (f *fakeNode) askedFor(op string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.asked, op)
}

func (f *fakeNode) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.opIDs)
}

func (f *fakeNode) set(change func(*fakeNode)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *fakeNode) Tasks(_ context.Context, q tracker.Query, _ time.Time) (tracker.Answer, error) {
	f.note("tasks")
	if q.Units != f.units {
		return tracker.Answer{}, errors.New("the serving node's chart was not attached")
	}
	if f.failWith != nil {
		return tracker.Answer{}, f.failWith
	}
	return tracker.Answer{TotalHint: 7}, nil
}

func (f *fakeNode) CreateTask(_ context.Context, opID string, task tracker.Task,
	_ *tracker.Notify) (tracker.WriteResult, error) {
	f.note("create")
	f.mu.Lock()
	f.opIDs = append(f.opIDs, opID)
	unvouched, refusal := f.unvouched, f.refusal
	f.mu.Unlock()
	if refusal != nil {
		return tracker.WriteResult{}, refusal
	}
	out := tracker.WriteResult{Key: task.Project + "-1"}
	out.Result = statelog.Result{Outcome: statelog.OutcomeApplied, Position: f.written, OpID: opID}
	if unvouched {
		out.Result = statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: opID, Unvouched: true}
	}
	return out, nil
}

func (f *fakeNode) RecordTurn(_ context.Context, opID string,
	turn tracker.TurnRecord) (tracker.WriteResult, error) {
	f.note("record_turn")
	f.mu.Lock()
	f.opIDs = append(f.opIDs, opID)
	f.turns = append(f.turns, turn)
	f.mu.Unlock()
	return tracker.WriteResult{}, nil
}

func (f *fakeNode) UpdateTask(_ context.Context, _, _, _ string, _ uint64,
	patch tracker.TaskPatch, _ tracker.ChangeKind, _ *tracker.Notify) (tracker.WriteResult, error) {
	f.note("update")
	f.mu.Lock()
	f.patches = append(f.patches, patch)
	f.mu.Unlock()
	return tracker.WriteResult{}, nil
}

func (f *fakeNode) Comment(_ context.Context, _ pages.Actor, _ string,
	in pages.NewComment) (pages.Comment, pages.Written, error) {
	f.note("comment")
	return pages.Comment{Body: in.Body}, pages.Written{}, nil
}

func (f *fakeNode) Search(_ context.Context, q knowledge.Query) []knowledge.Hit {
	f.note("search")
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	return []knowledge.Hit{{Title: "found on " + f.name}}
}

func (f *fakeNode) Building(context.Context) bool { return false }

// backend is this node as a [Backend] over estate.000.
func (f *fakeNode) backend() Backend {
	return Backend{
		Tracker: f, PageWriter: f, Knowledge: f,
		Writer: func(a Actor) TrackerWriter {
			f.mu.Lock()
			f.actors = append(f.actors, a)
			f.mu.Unlock()
			return f
		},
		Committed: func(ctx context.Context, at statelog.Position) error {
			f.mu.Lock()
			f.floors = append(f.floors, at)
			behind, abandoned, release := f.behind, f.abandoned, f.release
			f.mu.Unlock()
			switch {
			case abandoned:
				return statelog.ErrWaitAbandoned
			case behind:
				<-ctx.Done()
				return ctx.Err()
			case release != nil:
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		},
		Answers: func(context.Context) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return !f.notReady
		},
		Admits: func(context.Context) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.asked = append(f.asked, "admits")
			return !f.notAdmitting
		},
	}
}

// For implements [LocalBackends]: this node serves estate.000 unless it was
// told it does not, or that it cannot tell.
func (f *fakeNode) For(_ context.Context, p statelog.PartitionID) (Backend, bool, error) {
	f.mu.Lock()
	notHolder, holdingUnknown := f.notHolder, f.holdingUnknown
	f.mu.Unlock()
	switch {
	case holdingUnknown:
		return Backend{}, false, errors.New("the executor could not say")
	case notHolder || p != statelog.EstatePartition:
		return Backend{}, false, nil
	}
	return f.backend(), true, nil
}

// seams is the serving node's own chart and scope.
func (f *fakeNode) seams() ServerSeams {
	return ServerSeams{
		Units: f.units,
		Seat: func(handle string) (*org.Role, *org.Organization) {
			return &org.Role{Name: handle, DeclaredHandle: handle},
				&org.Organization{Name: "Acme", KnowledgeScope: []string{"ENG"}}
		},
	}
}

// fakePlacement is layout 0 served by whichever nodes a case names, at the
// epoch it names.
type fakePlacement struct {
	mu         sync.Mutex
	layout     statelog.Layout
	nodes      []string
	epoch      uint64
	err        error
	refreshes  int
	onRefresh  func(*fakePlacement)
	unanswered []string
}

func (p *fakePlacement) Layout() (statelog.Layout, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return statelog.Layout{}, p.err
	}
	return p.layout, nil
}

func (p *fakePlacement) Serving(statelog.PartitionID) ([]string, uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, 0, p.err
	}
	return slices.Clone(p.nodes), p.epoch, nil
}

func (p *fakePlacement) Refresh(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshes++
	if p.onRefresh != nil {
		p.onRefresh(p)
	}
	return nil
}

func (p *fakePlacement) Unanswered(node string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unanswered = append(p.unanswered, node)
}

func (p *fakePlacement) set(change func(*fakePlacement)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	change(p)
}

func (p *fakePlacement) refreshed() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refreshes
}

// fleet is some data nodes serving on one broker, and a router on a node
// that holds nothing.
type fleet struct {
	broker *memory.Broker
	nodes  map[string]*fakeNode

	// placement is what the routers route by, and servers what the
	// serving nodes' own views say — two views, so a case can put them at
	// different map epochs.
	placement, servers *fakePlacement

	client *Router
}

func newFleet(t *testing.T, names ...string) *fleet {
	t.Helper()
	sorted := slices.Sorted(slices.Values(names))
	f := &fleet{broker: memory.NewBroker(), nodes: map[string]*fakeNode{},
		placement: &fakePlacement{layout: layoutZero, nodes: sorted},
		servers:   &fakePlacement{layout: layoutZero, nodes: slices.Clone(sorted)}}
	for _, name := range names {
		node := &fakeNode{name: name, units: chartOf(name)}
		f.nodes[name] = node
		stop, err := Serve(t.Context(), silencer{q: f.start(t), node: node}, name, node,
			f.servers, node.seams())
		if err != nil {
			t.Fatalf("serve %s: %v", name, err)
		}
		t.Cleanup(func() { _ = stop(context.Background()) })
	}
	f.client = f.router(t, "agent-1", nil)
	return f
}

// start is one more client of the fleet's broker.
func (f *fleet) start(t *testing.T) *memory.Queue {
	t.Helper()
	q := f.broker.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	return q
}

// router is a router on node self over the fleet, answering in-process from
// local where it is given one.
func (f *fleet) router(t *testing.T, self string, local *fakeNode) *Router {
	t.Helper()
	opts := RouterOptions{Queue: f.start(t), Self: self, Placement: f.placement,
		Session: NewSession()}
	if local != nil {
		opts.Local, opts.Seams = local, local.seams()
	}
	r, err := NewRouter(opts)
	if err != nil {
		t.Fatal(err)
	}
	r.readBudget, r.writeBudget = 300*time.Millisecond, 300*time.Millisecond
	return r
}

// silencer registers an answerer that answers nothing while its node is
// silent — a node that died after the placement last saw it.
type silencer struct {
	q    *memory.Queue
	node *fakeNode
}

func (s silencer) Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error) {
	return s.q.Serve(ctx, subject, func(ctx context.Context, raw []byte) ([]byte, error) {
		s.node.mu.Lock()
		silent, hang := s.node.silent, s.node.hang
		s.node.mu.Unlock()
		if hang != nil {
			s.node.note("hung")
			select {
			case <-hang:
			case <-ctx.Done():
			}
			return nil, errors.New("this node is wedged")
		}
		if silent {
			s.node.note("silent")
			return nil, errors.New("this node is gone")
		}
		return h(ctx, raw)
	})
}

type namedChart struct{ tracker.Units }

// chartOf is a chart value distinguishable per node, compared by identity.
func chartOf(string) tracker.Units { return &namedChart{} }

// first is the node r asks first for estate.000, which is the rendezvous
// winner among the nodes the placement names.
func (f *fleet) first(t *testing.T, r *Router) (string, *fakeNode) {
	t.Helper()
	nodes, _, _ := f.placement.Serving(statelog.EstatePartition)
	ordered := r.order(statelog.EstatePartition, nodes)
	if len(ordered) == 0 {
		t.Fatalf("no node to ask among %v", nodes)
	}
	return ordered[0], f.nodes[ordered[0]]
}

// other is the one node of a two-node fleet that is not n.
func (f *fleet) other(n *fakeNode) *fakeNode {
	for _, node := range f.nodes {
		if node != n {
			return node
		}
	}
	return nil
}

var swe = Actor{Handle: "swe", Kind: tracker.AuthorAgent}

// A READ IS ANSWERED BY A NODE THAT SERVES THE PARTITION, with the serving
// node's own chart attached — the asking node's chart cannot cross a wire, and
// a read that arrived without one would render every unit unresolved.
func TestAReadIsAnsweredWithTheServingNodesChart(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	answer, err := f.client.Work().Tasks(t.Context(), tracker.Query{Units: chartOf("mine")}, time.Now())
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if answer.TotalHint != 7 {
		t.Errorf("total = %d, want the serving node's 7", answer.TotalHint)
	}
}

// AN ERROR KEEPS ITS IDENTITY: the tool that asks "is this no such task" of a
// remote answer is told yes.
func TestAServingNodesErrorKeepsItsIdentity(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.nodes["data-a"].failWith = errors.Join(errors.New("while reading"), tracker.ErrNoTask)
	_, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	if !errors.Is(err, tracker.ErrNoTask) {
		t.Fatalf("err = %v, want tracker.ErrNoTask through the wire", err)
	}
}

// A NODE THAT DOES NOT ANSWER IS PASSED OVER FOR A READ, and asked last until
// its suspicion lapses — so a node that died costs one attempt per router
// rather than one per request.
func TestAnUnansweredReadMovesOnAndTheSilentNodeIsAskedLast(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	firstName, first := f.first(t, f.client)
	first.set(func(n *fakeNode) { n.silent = true })
	if _, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("a read with a live second node failed: %v", err)
	}
	if next, _ := f.first(t, f.client); next == firstName {
		t.Errorf("the silent node %s is still asked first", firstName)
	}
	// AND THE PLACEMENT IS TOLD, so a node that left on a clean stop is
	// gone from its next answer rather than from the one a heartbeat later.
	f.placement.mu.Lock()
	told := slices.Clone(f.placement.unanswered)
	f.placement.mu.Unlock()
	if !slices.Contains(told, firstName) {
		t.Errorf("the placement was told %v went unanswered, want %s among them",
			told, firstName)
	}
}

// A PLACEMENT THAT CANNOT SAY WHO SERVES IS AN ERROR THAT SAYS SO, never an
// empty fleet: "nobody serves it" sends an operator to give a node the data
// role, which is the wrong remedy for a fleet whose coordination blinked.
func TestAnUnknownPlacementIsNotAnEmptyFleet(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.placement.set(func(p *fakePlacement) { p.err = errors.New("the leases could not be listed") })
	_, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	var unserved *ErrPartitionUnserved
	if err == nil || errors.As(err, &unserved) || errors.Is(err, ErrNoDataNode) {
		t.Fatalf("an unknown placement answered %v, want the placement's own error", err)
	}
	if !strings.Contains(err.Error(), "the leases could not be listed") {
		t.Fatalf("the error %q does not carry the placement's reason", err)
	}
}

// A NODE THAT IS NOT SERVING, OR BEHIND THE CALLER'S FLOOR, RAN NOTHING, so
// even a write that is never repeated moves on from it.
func TestANodeThatRanNothingIsPassedOverForEveryClass(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t, f.client)
	first.set(func(n *fakeNode) { n.notReady = true })
	if _, _, err := f.client.Pages().Comment(t.Context(), pages.Actor{}, "p1",
		pages.NewComment{Body: "hi"}); err != nil {
		t.Fatalf("a page write with a serving second node failed: %v", err)
	}
	if first.askedFor("comment") {
		t.Error("the node that was not serving ran the write")
	}
}

// A PAGE WRITE NOBODY ANSWERED IS NEVER ASKED AGAIN: it has no operation id a
// repeat could be collapsed on, so a second node would write a second comment.
func TestAnUnansweredPageWriteIsUnknownAndNotRepeated(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t, f.client)
	first.set(func(n *fakeNode) { n.silent = true })
	_, _, err := f.client.Pages().Comment(t.Context(), pages.Actor{}, "p1", pages.NewComment{Body: "hi"})
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("err = %v, want ErrOutcomeUnknown", err)
	}
	if second := f.other(first); second.askedFor("comment") {
		t.Errorf("%s was asked after an unanswered page write", second.name)
	}
}

// A TRACKER WRITE NOBODY ANSWERED IS ASKED AGAIN, UNDER THE SAME OPERATION ID:
// the ledger answers a repeat with the first copy's result, which is exactly
// the lost acknowledgement the id exists for.
func TestAnUnansweredTrackerWriteRepeatsUnderTheSameOperation(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t, f.client)
	first.set(func(n *fakeNode) { n.silent = true })
	written, err := f.client.WriterAs(swe).CreateTask(t.Context(), "op-7",
		tracker.Task{Project: "ENG"}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if written.Key != "ENG-1" {
		t.Errorf("key = %q", written.Key)
	}
	second := f.other(first)
	if got := second.ops(); !slices.Equal(got, []string{"op-7"}) {
		t.Errorf("the second node ran op ids %v, want [op-7]", got)
	}
	if len(second.actors) != 1 || second.actors[0].Handle != "swe" {
		t.Errorf("the write acted as %+v, want the seat that asked", second.actors)
	}
}

// A WRITE'S POSITION IS THE NEXT REQUEST'S FLOOR, on whichever node answers
// it — on EVERY log of the partition, since a holder applies them all and a
// seat's next read of the partition must include what it wrote to any of
// them. A floor on a log the partition does not carry is never waited for.
func TestAWritesPositionIsTheNextReadsFloor(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	node.written = statelog.Position{Stream: trackerStream, Generation: 1, Seq: 99}
	if _, err := f.client.WriterAs(swe).CreateTask(t.Context(), "op-1",
		tracker.Task{Project: "ENG"}, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	node.set(func(n *fakeNode) { n.floors = nil })
	if _, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if len(node.floors) != 1 || node.floors[0] != node.written {
		t.Fatalf("the read waited for %v, want the write's %v", node.floors, node.written)
	}
	node.set(func(n *fakeNode) { n.floors = nil })
	if _, _, err := f.client.Pages().Comment(t.Context(), pages.Actor{}, "p", pages.NewComment{}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if len(node.floors) != 1 || node.floors[0] != node.written {
		t.Errorf("a write to the partition's pages log waited on %v, want the "+
			"tracker log's floor on the same partition", node.floors)
	}
	// ANOTHER PARTITION'S LOG: a position on a stream estate.000 does not
	// carry is not a floor any holder of it could reach.
	f.client.Observe(statelog.Position{Stream: "CREWLET_OTHER_LOG", Generation: 1, Seq: 3})
	node.set(func(n *fakeNode) { n.floors = nil })
	if _, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	for _, at := range node.floors {
		if at.Stream != trackerStream {
			t.Errorf("a read of estate.000 waited on %v, a log it does not carry", at)
		}
	}
}

// A NODE BEHIND THE FLOOR SAYS SO RATHER THAN ANSWER FROM BEFORE THE WRITE,
// and when every holder is behind the caller is told the partition is
// unserved — naming it.
func TestEveryHolderBehindTheFloorIsAnUnservedPartition(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.client.Observe(statelog.Position{Stream: trackerStream, Generation: 1, Seq: 5})
	f.nodes["data-a"].set(func(n *fakeNode) { n.behind = true })
	_, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	var unserved *ErrPartitionUnserved
	if !errors.As(err, &unserved) || unserved.Partition != "estate.000" {
		t.Fatalf("err = %v, want ErrPartitionUnserved naming estate.000", err)
	}
	if f.nodes["data-a"].askedFor("tasks") {
		t.Error("a node behind the floor answered anyway")
	}
}

// A FLOOR ON AN ABANDONED GENERATION IS DROPPED rather than carried for ever:
// no node will reach it, and a router that kept it would be refused every
// read until it restarted.
func TestAFloorNoNodeCanReachIsForgotten(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.client.Observe(statelog.Position{Stream: trackerStream, Generation: 1, Seq: 5})
	f.nodes["data-a"].set(func(n *fakeNode) { n.abandoned = true })
	if _, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if floors := f.client.Session().Floors([]string{trackerStream}); len(floors) != 0 {
		t.Errorf("the router still carries %v", floors)
	}
}

// A FLOOR IS FORGOTTEN BY POSITION, NEVER BY STREAM: a write observed after a
// request went out raised the floor past the one a holder reported
// abandoned — onto the log's new generation — and dropping the stream would
// lose that write from the next read.
func TestTheSessionForgetsOnlyTheFloorItWasTold(t *testing.T) {
	t.Parallel()
	s := NewSession()
	old := statelog.Position{Stream: trackerStream, Generation: 1, Seq: 5}
	s.Observe(old)
	newer := statelog.Position{Stream: trackerStream, Generation: 2, Seq: 1}
	s.Observe(newer)
	s.Forget(old)
	if got := s.Floors([]string{trackerStream}); len(got) != 1 || got[0] != newer {
		t.Fatalf("after forgetting the abandoned floor the session holds %v, want %v", got, newer)
	}
	s.Forget(newer)
	if got := s.Floors([]string{trackerStream}); len(got) != 0 {
		t.Fatalf("the floor that was reported is still carried: %v", got)
	}
}

// A PATCH'S GESTURES CROSS: they are resolved inside the decide and never
// encoded into a record, so the patch type does not serialise them — and a
// watch or a relation dropped on the way would be a write that did less than
// it said, reported as done.
func TestAPatchsGesturesCross(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	patch := tracker.TaskPatch{
		Watch:  &tracker.WatchIntent{Handle: "ana", Watch: true},
		Relate: &tracker.RelationIntent{},
	}
	if _, err := f.client.WriterAs(swe).
		UpdateTask(t.Context(), "op", "ENG-1", "ENG", 0, patch, "", nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := f.nodes["data-a"].patches
	if len(got) != 1 || got[0].Watch == nil || got[0].Watch.Handle != "ana" || !got[0].Watch.Watch ||
		got[0].Relate == nil {
		t.Fatalf("the serving node saw %+v", got)
	}
}

// A WRITE NAMES WHO IT ACTS AS, or the serving node refuses it — a history
// row nobody can attribute is not an audit trail. In-process too: the check
// is the operation's, not the wire's.
func TestAWriteThatNamesNobodyIsRefused(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	local := &fakeNode{name: "data-self", units: chartOf("self")}
	for _, r := range []*Router{f.client, f.router(t, "data-self", local)} {
		if _, err := r.WriterAs(Actor{}).CreateTask(t.Context(), "op", tracker.Task{}, nil); err == nil {
			t.Fatal("a write naming nobody was run")
		}
	}
	if f.nodes["data-a"].askedFor("create") || local.askedFor("create") {
		t.Error("a serving node ran it")
	}
}

// A SEARCH KEEPS "NO EXCLUSION" APART FROM "THE DEFAULT EXCLUSION", which the
// query tells apart on purpose, and is scoped by the serving node's chart.
func TestAKnowledgeSearchKeepsItsExclusionAndTakesTheServersScope(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	searcher := f.client.Knowledge()
	searcher.Search(t.Context(), knowledge.Query{Text: "deploys", Seat: seat,
		Org: &org.Organization{}, ExcludeAncestors: []string{}})
	searcher.Search(t.Context(), knowledge.Query{Text: "deploys"})
	got := f.nodes["data-a"].queries
	if len(got) != 2 {
		t.Fatalf("searches = %d", len(got))
	}
	if got[0].ExcludeAncestors == nil || len(got[0].ExcludeAncestors) != 0 {
		t.Errorf("an empty exclusion arrived as %#v", got[0].ExcludeAncestors)
	}
	if got[1].ExcludeAncestors != nil {
		t.Errorf("no exclusion arrived as %#v", got[1].ExcludeAncestors)
	}
	if got[0].Seat == nil || got[0].Seat.Handle() != "swe" || got[0].Org == nil ||
		!slices.Equal(got[0].Org.KnowledgeScope, []string{"ENG"}) {
		t.Errorf("the search was not scoped by the serving node's chart: %+v", got[0])
	}
	if got[1].Org != nil || got[1].Seat != nil {
		t.Errorf("an unscoped search arrived scoped: %+v", got[1])
	}
}

// A PARTITION NOBODY SERVES IS AN ANSWER NAMING IT, and an operation that
// addresses no partition says there is no data node — never an empty result.
func TestNoServingNodeSaysSo(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	_, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	var unserved *ErrPartitionUnserved
	if !errors.As(err, &unserved) || unserved.Partition != "estate.000" {
		t.Fatalf("err = %v, want ErrPartitionUnserved naming estate.000", err)
	}
	if _, err := f.client.AppendEvents(t.Context(), nil); !errors.Is(err, ErrNoDataNode) {
		t.Fatalf("custody with no data node answered %v, want ErrNoDataNode", err)
	}
}

// A TURN'S SPEND CROSSES, AS THE SEAT, AND AN UNANSWERED ONE IS ASKED AGAIN
// UNDER THE SAME OPERATION.
//
// A node without `data` has no applier, so the spend of a turn its seat ran
// reaches the task only through a data node. It carries its operation id,
// which is what makes a repeat on the next node safe: the applier adds a
// turn's spend only for an operation it has not applied.
func TestATurnsSpendCrossesAndRepeatsUnderItsOperation(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t, f.client)
	first.set(func(n *fakeNode) { n.silent = true })
	turn := tracker.TurnRecord{
		Task: "task-1", Seat: "swe", TurnID: "run-1", Outcome: "delivered",
		Phases: []string{"execute"},
		Spend: tracker.TurnSpend{Turns: 1, Rounds: 2, Input: 900, Output: 100,
			CacheRead: 400, WallMs: 3100},
	}
	if _, err := f.client.WriterAs(swe).RecordTurn(t.Context(), "op-turn", turn); err != nil {
		t.Fatalf("record the turn: %v", err)
	}
	second := f.other(first)
	second.mu.Lock()
	defer second.mu.Unlock()
	if !slices.Equal(second.opIDs, []string{"op-turn"}) {
		t.Fatalf("the next node was asked under %v, want the same operation", second.opIDs)
	}
	if len(second.turns) != 1 || !reflect.DeepEqual(second.turns[0], turn) {
		t.Fatalf("the turn arrived as %+v, want %+v", second.turns, turn)
	}
	if got := second.actors[len(second.actors)-1]; got.Handle != "swe" {
		t.Fatalf("the spend was written as %q, want the seat", got.Handle)
	}
}

// A NODE THAT SERVES THE PARTITION ANSWERS ITS OWN SEATS IN-PROCESS, and asks
// nobody: under layout 0 a data node holds everything, and a request over the
// broker to itself would be a round trip for nothing.
func TestANodeThatServesThePartitionAnswersItself(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	local := &fakeNode{name: "data-self", units: chartOf("self")}
	r := f.router(t, "data-self", local)
	answer, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	if err != nil || answer.TotalHint != 7 {
		t.Fatalf("tasks = (%+v, %v)", answer, err)
	}
	if !local.askedFor("tasks") || f.nodes["data-a"].askedFor("tasks") {
		t.Fatalf("the read went to %v here and %v on the peer, want it answered here",
			local.asked, f.nodes["data-a"].asked)
	}
}

// A LOCAL READ WAITS FOR THIS NODE'S OWN FLOOR — design B, the join direction.
//
// A data node's seats may write a partition through ANOTHER holder — this node
// was not serving it yet, or had stopped on a fault — and then read it here:
// the write is on the log and not yet in this node's rows. A node that trusted
// its own copy because "it applies the write itself" answers its own seat from
// before a write it was told landed. So the local path waits for the floor
// exactly as a remote holder would.
func TestALocalReadWaitsForTheNodesOwnFloor(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	release := make(chan struct{})
	local := &fakeNode{name: "data-self", units: chartOf("self"), release: release}
	r := f.router(t, "data-self", local)
	floor := statelog.Position{Stream: trackerStream, Generation: 1, Seq: 42}
	r.Observe(floor)

	done := make(chan error, 1)
	go func() {
		_, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the local read answered (%v) before this node applied its own floor", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("tasks: %v", err)
	}
	local.mu.Lock()
	defer local.mu.Unlock()
	if !slices.Contains(local.floors, floor) || !slices.Contains(local.asked, "tasks") {
		t.Fatalf("the local read waited for %v and ran %v, want the floor %v first",
			local.floors, local.asked, floor)
	}
}

// A LOCAL COPY THAT CANNOT REACH THE FLOOR IN TIME ANSWERS `behind` TO ITSELF,
// and the router asks the partition's next holder, which has.
func TestALocalCopyBehindItsFloorAsksAnotherHolder(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	local := &fakeNode{name: "data-self", units: chartOf("self"), behind: true}
	r := f.router(t, "data-self", local)
	floor := statelog.Position{Stream: trackerStream, Generation: 1, Seq: 42}
	r.Observe(floor)
	answer, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	if err != nil || answer.TotalHint != 7 {
		t.Fatalf("tasks = (%+v, %v)", answer, err)
	}
	if local.askedFor("tasks") {
		t.Error("the local copy answered from before its own floor")
	}
	peer := f.nodes["data-a"]
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if !slices.Contains(peer.asked, "tasks") || !slices.Contains(peer.floors, floor) {
		t.Fatalf("the peer ran %v and waited for %v, want the read held to %v",
			peer.asked, peer.floors, floor)
	}
}

// A NODE THAT DOES NOT SERVE THE PARTITION RIGHT NOW is never asked
// in-process — a copy that stopped serving on a fault is read from the
// partition's other holders — and answers another node's request
// `not_holder`, having run nothing.
func TestANodeThatDoesNotServeThePartitionRunsNothing(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	local := &fakeNode{name: "data-self", units: chartOf("self"), notHolder: true}
	r := f.router(t, "data-self", local)
	_, first := f.first(t, r)
	first.set(func(n *fakeNode) { n.notHolder = true })
	written, err := r.WriterAs(swe).CreateTask(t.Context(), "op-9", tracker.Task{Project: "ENG"}, nil)
	if err != nil || written.Key != "ENG-1" {
		t.Fatalf("create = (%+v, %v)", written, err)
	}
	if local.askedFor("create") || first.askedFor("create") {
		t.Fatal("a node that does not serve the partition ran the write")
	}
	if got := f.other(first).ops(); !slices.Equal(got, []string{"op-9"}) {
		t.Fatalf("the serving holder ran %v, want [op-9]", got)
	}
}

// NOT_HOLDER FROM A SERVER WITH A NEWER MAP REFRESHES THE VIEW, ONCE, and the
// request walks the holders the fresh view names.
//
// The server is the authority: a router routing by an older map costs one
// `not_holder` and one direct read of the map, never a wrong answer. And only
// once per request, because a view just read that a server still outruns is
// not one this request can wait for.
func TestANotHolderFromANewerMapRefreshesTheViewOnce(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	a, b := f.nodes["data-a"], f.nodes["data-b"]
	a.set(func(n *fakeNode) { n.notHolder = true })
	f.servers.set(func(p *fakePlacement) { p.epoch = 7 })
	f.placement.set(func(p *fakePlacement) {
		p.nodes, p.epoch = []string{"data-a"}, 3
		p.onRefresh = func(p *fakePlacement) { p.nodes, p.epoch = []string{"data-a", "data-b"}, 7 }
	})
	if _, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks after a newer map: %v", err)
	}
	if got := f.placement.refreshed(); got != 1 {
		t.Fatalf("the view was refreshed %d times, want once", got)
	}
	if !b.askedFor("tasks") {
		t.Fatal("the holder the fresh view names was not asked")
	}

	// ONCE: a server whose map keeps outrunning a view just read is not
	// chased again — the request moves on, and fails naming the partition.
	b.set(func(n *fakeNode) { n.notHolder = true })
	f.placement.set(func(p *fakePlacement) {
		p.nodes, p.epoch, p.refreshes = []string{"data-a", "data-b"}, 3, 0
		p.onRefresh = func(p *fakePlacement) { p.epoch = 5 }
	})
	_, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	var unserved *ErrPartitionUnserved
	if !errors.As(err, &unserved) {
		t.Fatalf("every holder refusing answered %v, want ErrPartitionUnserved", err)
	}
	if got := f.placement.refreshed(); got != 1 {
		t.Fatalf("the view was refreshed %d times for one request, want once", got)
	}
}

// NOT_HOLDER FROM A SERVER NO NEWER THAN THE VIEW IS THE SERVER'S OWN STATE —
// a joiner not serving yet, a leaver — so the router moves to the next holder
// and reads nothing again.
func TestANotHolderAtTheViewsEpochMovesToTheNextHolder(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t, f.client)
	first.set(func(n *fakeNode) { n.notHolder = true })
	if _, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if got := f.placement.refreshed(); got != 0 {
		t.Fatalf("a not_holder at the view's own epoch refreshed the view %d times", got)
	}
	if !f.other(first).askedFor("tasks") {
		t.Fatal("the next holder was not asked")
	}
}

// A NODE THAT CANNOT TELL WHETHER IT SERVES THE PARTITION SAYS SO, and is not
// `not_holder`: it names no map epoch, because nothing about the asker's map is
// in question, so the asker moves to the next holder without reading its map
// again — however new the server's own map is. The same holds for this node's
// own copy: a holding it cannot read is passed over for a peer.
func TestAHoldingNobodyCanTellMovesOnWithoutARefresh(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t, f.client)
	first.set(func(n *fakeNode) { n.holdingUnknown = true })
	f.servers.set(func(p *fakePlacement) { p.epoch = 7 })
	f.placement.set(func(p *fakePlacement) { p.epoch = 3 })
	if _, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if got := f.placement.refreshed(); got != 0 {
		t.Fatalf("a holding the server could not tell refreshed the asker's view %d times", got)
	}
	if first.askedFor("tasks") || !f.other(first).askedFor("tasks") {
		t.Fatal("the read was not taken from the node that could not tell to the next holder")
	}

	raw, err := json.Marshal(request{Op: "tracker.tasks", Args: json.RawMessage(`{}`),
		Partitions: []string{statelog.EstatePartition.String()}, MapEpoch: 3})
	if err != nil {
		t.Fatal(err)
	}
	replies, err := f.start(t).Ask(t.Context(), Subject(first.name), raw, 1)
	if err != nil || len(replies) != 1 {
		t.Fatalf("ask = (%d replies, %v)", len(replies), err)
	}
	var rep reply
	if err := json.Unmarshal(replies[0], &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Unserved != unservedHoldingUnknown || rep.Epoch != 0 {
		t.Fatalf("the server answered %q at epoch %d, want %q with no epoch",
			rep.Unserved, rep.Epoch, unservedHoldingUnknown)
	}

	local := &fakeNode{name: "data-self", units: chartOf("self"), holdingUnknown: true}
	r := f.router(t, "data-self", local)
	first.set(func(n *fakeNode) { n.holdingUnknown = false })
	if _, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks from a node that cannot tell: %v", err)
	}
	if local.askedFor("tasks") {
		t.Fatal("a node that cannot tell whether it serves the partition answered it itself")
	}
}

// AN UNVOUCHED ANSWER IS NOT FINAL FOR AN IDEMPOTENT WRITE: the answering
// holder's ledger cannot say whether the operation landed, and another
// holder's may. So the next holder is asked under the same operation id, and
// only when none can vouch is the unknown what the caller is told.
func TestAnUnvouchedWriteIsAskedOfTheNextHolderUnderTheSameOperation(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	local := &fakeNode{name: "data-self", units: chartOf("self"), unvouched: true}
	r := f.router(t, "data-self", local)
	_, first := f.first(t, r)
	first.set(func(n *fakeNode) { n.unvouched = true })
	second := f.other(first)

	written, err := r.WriterAs(swe).CreateTask(t.Context(), "op-u", tracker.Task{Project: "ENG"}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if written.Outcome != statelog.OutcomeApplied || written.Unvouched {
		t.Fatalf("the write answered %+v, want the vouching holder's applied answer", written.Result)
	}
	for _, n := range []*fakeNode{local, first, second} {
		if got := n.ops(); !slices.Equal(got, []string{"op-u"}) {
			t.Errorf("%s ran %v, want [op-u] — every holder asked under the same operation",
				n.name, got)
		}
	}

	// NOBODY VOUCHES: the answer is the unknown, never an unserved partition.
	second.set(func(n *fakeNode) { n.unvouched = true })
	written, err = r.WriterAs(swe).CreateTask(t.Context(), "op-v", tracker.Task{Project: "ENG"}, nil)
	if err != nil || written.Outcome != statelog.OutcomeUnknown || !written.Unvouched {
		t.Fatalf("with no holder vouching the write answered (%+v, %v), want the unvouched unknown",
			written.Result, err)
	}
}

// A GATE-3 REFUSAL APPENDED NOTHING, so every class moves on from it under the
// same operation id: the write authority refused because this node does not
// serve the log's partition, or could not tell — and another holder can take
// the write. A refusal about another node's COPY is final, and reaches the
// caller whole: the node that refused passed its own fences, and the remedy —
// the same operation once the duplicate window lets go — is the caller's.
func TestAGateThreeRefusalMovesOnUnderTheSameOperation(t *testing.T) {
	t.Parallel()
	for _, reason := range []statelog.Reason{statelog.ReasonNotHolder, statelog.ReasonHoldingUnknown} {
		f := newFleet(t, "data-a", "data-b")
		_, first := f.first(t, f.client)
		first.set(func(n *fakeNode) {
			n.refusal = &statelog.Unavailable{Reason: reason, OpID: "op-g", Cause: statelog.ErrNotHolder}
		})
		written, err := f.client.WriterAs(swe).CreateTask(t.Context(), "op-g",
			tracker.Task{Project: "ENG"}, nil)
		if err != nil || written.Key != "ENG-1" {
			t.Fatalf("%s: create = (%+v, %v), want the next holder's answer", reason, written, err)
		}
		if got := f.other(first).ops(); !slices.Equal(got, []string{"op-g"}) {
			t.Fatalf("%s: the next holder ran %v, want [op-g]", reason, got)
		}
	}

	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t, f.client)
	first.set(func(n *fakeNode) {
		n.refusal = &statelog.Unavailable{Reason: statelog.ReasonReleased, OpID: "op-c",
			CopyWriter: "data-z"}
	})
	_, err := f.client.WriterAs(swe).CreateTask(t.Context(), "op-c", tracker.Task{Project: "ENG"}, nil)
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonReleased ||
		refusal.CopyWriter != "data-z" {
		t.Fatalf("a refusal naming another node's copy answered %v, want it whole", err)
	}
	if f.other(first).askedFor("create") {
		t.Error("a refusal that is final was taken to another holder")
	}
}

// ADMISSION ASKS THE COPY THAT WILL SERVE THE SEAT: this node's own where it
// serves the partition — never passed over for a peer's, since its seats read
// here — and a remote holder's where it does not.
func TestAdmissionAsksTheCopyThatWillServeTheSeat(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	peer := f.nodes["data-a"]
	local := &fakeNode{name: "data-self", units: chartOf("self"), notAdmitting: true}
	r := f.router(t, "data-self", local)
	trackerServed, _, err := r.Serves(t.Context(), statelog.EstatePartition)
	if err != nil || trackerServed {
		t.Fatalf("a node whose own copy admits no seat was admitted (%v, %v)", trackerServed, err)
	}
	if peer.askedFor("admits") {
		t.Fatal("a peer was asked for a node that serves the partition itself")
	}

	// A COPY THIS NODE DOES NOT SERVE — faulted, or never held — is asked
	// of a holder that does.
	local.set(func(n *fakeNode) { n.notHolder = true })
	trackerServed, _, err = r.Serves(t.Context(), statelog.EstatePartition)
	if err != nil || !trackerServed {
		t.Fatalf("a node served by a peer was not admitted (%v, %v)", trackerServed, err)
	}

	// AND A REMOTE HOLDER THAT ADMITS NO SEAT IS PASSED OVER, like a node
	// that ran nothing.
	peer.set(func(n *fakeNode) { n.notAdmitting = true })
	if trackerServed, _, err = f.client.Serves(t.Context(), statelog.EstatePartition); trackerServed {
		t.Fatalf("a node was admitted on a holder that admits no seat (%v)", err)
	}
}

// AN OLDER BUILD'S ADMISSION IS ANSWERED. A stateless node from before
// partitions asks its ping with no arguments and names no partition; a data
// node of this build answers it as the question it always was — whether its
// copy of the estate admits a seat — rather than refusing it as malformed,
// which that node would take as final and withhold every seat claim until it
// was upgraded too.
func TestAnOlderBuildsAdmissionIsAnswered(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	raw, err := json.Marshal(map[string]any{"op": "estate.ping", "args": struct{}{}})
	if err != nil {
		t.Fatal(err)
	}
	replies, err := f.start(t).Ask(t.Context(), Subject("data-a"), raw, 1)
	if err != nil || len(replies) != 1 {
		t.Fatalf("ask = (%d replies, %v)", len(replies), err)
	}
	var rep reply
	if err := json.Unmarshal(replies[0], &rep); err != nil {
		t.Fatal(err)
	}
	var answer served
	if rep.Err != nil || rep.Unserved != "" || json.Unmarshal(rep.Result, &answer) != nil ||
		!answer.Tracker {
		t.Fatalf("an older build's ping was answered %+v, want the admission answer", rep)
	}
	if !f.nodes["data-a"].askedFor("admits") {
		t.Fatal("the older question was answered without asking the copy")
	}

	// UNDER A LAYOUT THAT DIVIDES THE ESTATE the older question has no
	// partition to be about — and no older build shares such a fleet.
	f.servers.set(func(p *fakePlacement) { p.layout = dividedLayout })
	replies, err = f.start(t).Ask(t.Context(), Subject("data-a"), raw, 1)
	if err != nil || len(replies) != 1 {
		t.Fatalf("ask = (%d replies, %v)", len(replies), err)
	}
	rep = reply{}
	if err := json.Unmarshal(replies[0], &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Err == nil || !errors.Is(decodeError(rep.Err), ErrUnaddressed) {
		t.Fatalf("a whole-estate ping under a divided layout answered %+v, want ErrUnaddressed", rep)
	}
}

// dividedLayout is a layout that divides every domain.
var dividedLayout = statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
	{Space: statelog.SpaceTracker, Partitions: 4, Domains: []string{"tracker", "vectors"}},
	{Space: statelog.SpacePages, Partitions: 2, Domains: []string{"pages", "vectors"}},
	{Space: statelog.SpaceCompany, Partitions: 1, Domains: []string{"tracker"}},
}}

// ADMISSION IS BOUNDED AS A WHOLE, not per holder: a sweep asks it on every
// pass, the first one at boot, and each listed holder that is wedged would
// otherwise cost the sweep a whole read attempt. A fleet whose holders do not
// answer withholds the claim within the bound, and the sweep goes on.
func TestWedgedHoldersCostAdmissionNoMoreThanItsBound(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b", "data-c")
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	for _, n := range f.nodes {
		n.set(func(n *fakeNode) { n.hang = hang })
	}
	r := f.router(t, "agent-2", nil)
	r.readBudget, r.admissionBudget = 10*time.Second, 200*time.Millisecond
	type answer struct {
		served bool
		err    error
	}
	begun := time.Now()
	done := make(chan answer, 1)
	go func() {
		served, _, err := r.Serves(t.Context(), statelog.EstatePartition)
		done <- answer{served, err}
	}()
	select {
	case got := <-done:
		if got.served || got.err == nil {
			t.Fatalf("wedged holders admitted a seat (%v, %v)", got.served, got.err)
		}
		if waited := time.Since(begun); waited > 2*time.Second {
			t.Fatalf("admission waited %s on wedged holders, past its %s bound",
				waited, r.admissionBudget)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admission waited on every wedged holder in turn, past its bound")
	}
}

// AN OPERATION THAT ADDRESSES ITS DOMAIN AS ONE PARTITION HAS NONE under a
// layout that divides the domain, and says so rather than guessing one — the
// same answer the domain's own partition function gives.
func TestAWholeDomainOperationUnderADividedLayoutIsUnaddressed(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.placement.set(func(p *fakePlacement) { p.layout = dividedLayout })
	_, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	if !errors.Is(err, ErrUnaddressed) {
		t.Fatalf("err = %v, want ErrUnaddressed", err)
	}
	if f.nodes["data-a"].askedFor("tasks") {
		t.Error("a node was asked for an operation with no partition")
	}
}
