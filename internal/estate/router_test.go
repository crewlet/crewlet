package estate

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/search"
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

	cpus CPUs

	mu        sync.Mutex
	name      string
	asked     []string
	floors    []statelog.Position
	actors    []Actor
	opIDs     []string
	patches   []tracker.TaskPatch
	turns     []tracker.TurnRecord
	keys      []pages.CallKey
	queries   []knowledge.Query
	units     tracker.Units
	notReady  bool
	behind    bool
	abandoned bool
	// behindOn is a node whose applier of one log is stuck: a floor on
	// that stream is never reached, every other is.
	behindOn string
	silent   bool
	failWith error
	written  statelog.Position

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

	// refusesLagging is a build from before a request could say "answer
	// anyway": it answers every request that its copy lags.
	refusesLagging bool

	// hang, when set, is a node that takes a request and never answers it
	// until hang is closed — a wedged node, as the broker's own ask sees
	// one: waited for until the asker's deadline, where a node that is
	// simply gone is known to have answered nothing.
	hang chan struct{}

	// gates is every gate record this node published for another, and
	// noGateLog a node that runs no log of the partition right now.
	gates     []GateArgs
	noGateLog bool
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
	f.keyed(in.CallKey)
	return pages.Comment{Body: in.Body}, pages.Written{}, nil
}

func (f *fakeNode) Slice(_ context.Context, q knowledge.Query) (pages.SearchSlice, error) {
	f.note("search")
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	return pages.SearchSlice{
		Candidates: search.Candidates{Lexical: []search.Scored{{Key: "page:" + f.name, Score: 1}}},
		Hits:       map[string]knowledge.Hit{"page:" + f.name: {Title: "found on " + f.name}},
	}, nil
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
			return reportingWriter{fakeNode: f, log: a.Provenance.Written}
		},
		Committed: func(ctx context.Context, at statelog.Position) error {
			f.mu.Lock()
			f.floors = append(f.floors, at)
			behind, abandoned, release := f.behind, f.abandoned, f.release
			behind = behind || (f.behindOn != "" && at.Stream == f.behindOn)
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
		Applied: func(stream string) statelog.Position {
			return statelog.Position{Stream: stream, Generation: 1, Seq: 7}
		},
		Gates: func(string) GateWriter {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.noGateLog {
				return nil
			}
			return f.gate
		},
		ReadmissionBounds: func(domain string) BoundReader {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.noGateLog {
				return nil
			}
			return func(context.Context) (statelog.ReadmissionBound, error) {
				f.note("readmission_bound")
				return f.bound(domain), nil
			}
		},
	}
}

// bound is the readmission bound this node's copy reads on domain's log: a
// floor that names the node, so an answer says whose fence it is.
func (f *fakeNode) bound(domain string) statelog.ReadmissionBound {
	return statelog.ReadmissionBound{Domain: domain + "@" + f.name, Generation: 1,
		Floor: uint64(len(f.name)), First: 1}
}

// gate publishes a gate record as this node's write authority would: applied,
// or `unknown` unvouched by its ledger where the node says so.
func (f *fakeNode) gate(_ context.Context, a GateArgs) (statelog.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gates = append(f.gates, a)
	if f.unvouched {
		return statelog.Result{Outcome: statelog.OutcomeUnknown, Unvouched: true}, nil
	}
	return statelog.Result{Outcome: statelog.OutcomeApplied,
		Position: statelog.Position{Stream: trackerStream, Generation: 1, Seq: 11}}, nil
}

// gated is every gate record this node published, in order.
func (f *fakeNode) gated() []GateArgs {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.gates)
}

// CPUs implements [LocalBackends]: the node's one.
func (f *fakeNode) CPUs() *CPUs { return &f.cpus }

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
	servingErr error
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
	if p.servingErr != nil {
		return nil, 0, p.servingErr
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
		silent, hang, refusesLagging := s.node.silent, s.node.hang, s.node.refusesLagging
		s.node.mu.Unlock()
		if refusesLagging {
			s.node.note("lagging")
			return json.Marshal(reply{Node: s.node.name, Unserved: unservedLagging,
				Detail: s.node.name + "'s copy lags its logs"})
		}
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
	// ANOTHER DOMAIN'S LOG IN THE SAME PARTITION: a page write does not
	// depend on the tracker's rows, so it carries no floor on its log.
	node.set(func(n *fakeNode) { n.floors = nil })
	if _, _, err := f.client.Pages().Comment(t.Context(), pages.Actor{}, "p", pages.NewComment{}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if len(node.floors) != 0 {
		t.Errorf("a write to the partition's pages log waited on %v, the tracker "+
			"log's floor — a log it does not depend on", node.floors)
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

// A FLOOR ON ANOTHER DOMAIN'S LOG NEVER HOLDS AN OPERATION, though one
// partition carries both logs: no operation reads another domain's rows, so a
// seat a page change woke — its trigger a floor on the pages log — reads and
// writes the tracker on a node whose pages applier is stuck exactly as before
// it was woken, and only the knowledge base's own operations wait for that
// applier.
func TestAFloorOnAnotherDomainsLogNeverHoldsAnOperation(t *testing.T) {
	t.Parallel()
	pagesStream, _ := layoutZero.Stream(statelog.LogID{Domain: pagesDomain,
		Partition: statelog.EstatePartition})
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	node.set(func(n *fakeNode) { n.behindOn = pagesStream })
	f.client.Observe(statelog.Position{Stream: pagesStream, Generation: 1, Seq: 9})

	if _, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("a tracker read behind a pages floor = %v, want it answered", err)
	}
	if _, err := f.client.WriterAs(swe).CreateTask(t.Context(), "op-1",
		tracker.Task{Project: "ENG"}, nil); err != nil {
		t.Fatalf("a tracker write behind a pages floor = %v, want it written", err)
	}
	node.mu.Lock()
	carried := slices.Clone(node.floors)
	node.mu.Unlock()
	if len(carried) != 0 {
		t.Errorf("the tracker's operations waited on %v, the pages log's floor", carried)
	}
	// AND THE HOLDER HOLDS TO IT TOO: an older asker sends its floors on
	// every log of the partition, and the one on another domain's log is
	// not waited for.
	raw, _ := json.Marshal(request{Op: opTasks.spec.name, Partitions: []string{"estate.000"},
		Floors: []statelog.Position{{Stream: pagesStream, Generation: 1, Seq: 9}}})
	replies, err := f.start(t).Ask(t.Context(), Subject("data-a"), raw, 1)
	var rep reply
	if err != nil || len(replies) != 1 || json.Unmarshal(replies[0], &rep) != nil ||
		rep.Unserved != "" || rep.Err != nil {
		t.Fatalf("an older asker's tracker read with a pages floor = (%+v, %v), want it answered",
			rep, err)
	}

	// LONGER THAN THE HOLDER'S FLOOR WAIT, so it says it is behind before
	// the asker stops listening.
	f.client.writeBudget = statelog.ReadBudget + time.Second
	_, _, err = f.client.Pages().Comment(t.Context(), pages.Actor{}, "p", pages.NewComment{})
	var unserved *ErrPartitionUnserved
	if !errors.As(err, &unserved) || !strings.Contains(err.Error(), "has not applied") ||
		node.askedFor("comment") {
		t.Fatalf("a page write behind its own log's floor = %v, want the partition "+
			"unserved as behind, and nothing written", err)
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

// A COPY THAT LAGS ITS LOGS IS A WORSE HOLDER, NEVER NO HOLDER.
//
// Whether a copy answers requests is its DISTANCE from its logs — the floors
// and the read's own level hold what it answers to what the caller must see —
// so a lagging copy is passed over for one that does not lag, and asked again,
// told to take the request anyway, once no such holder is left. Refused
// outright, a single data node a burst put past the snapshot slack refused its
// own seats every call until it caught up, and a fleet the same burst put
// behind together refused everybody's.
func TestALaggingCopyIsAskedLastAndNeverRefusedForLagging(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t, f.client)
	second := f.other(first)
	first.set(func(n *fakeNode) { n.notReady = true })

	// A HOLDER WHOSE COPY DOES NOT LAG IS PREFERRED.
	if _, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks with one copy lagging: %v", err)
	}
	if first.askedFor("tasks") || !second.askedFor("tasks") {
		t.Fatal("the lagging copy ran the read while a holder whose copy does not lag was there")
	}

	// AND WHEN EVERY COPY LAGS, ONE OF THEM ANSWERS — a read, a tracker
	// write and a page write alike — held to the caller's floor.
	for _, n := range f.nodes {
		n.set(func(n *fakeNode) { n.notReady, n.asked, n.floors = true, nil, nil })
	}
	floor := statelog.Position{Stream: trackerStream, Generation: 1, Seq: 9}
	f.client.Observe(floor)
	if _, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks with every copy lagging: %v", err)
	}
	for _, n := range f.nodes {
		n.mu.Lock()
		ran, waited := slices.Contains(n.asked, "tasks"), slices.Contains(n.floors, floor)
		n.mu.Unlock()
		if ran && !waited {
			t.Fatalf("%s ran the read on a lagging copy without waiting for the caller's floor", n.name)
		}
	}
	if _, err := f.client.WriterAs(swe).CreateTask(t.Context(), "op-lag",
		tracker.Task{Project: "ENG"}, nil); err != nil {
		t.Fatalf("a tracker write with every copy lagging: %v", err)
	}
	if _, _, err := f.client.Pages().Comment(t.Context(), pages.Actor{}, "p1",
		pages.NewComment{Body: "hi"}); err != nil {
		t.Fatalf("a page write with every copy lagging: %v", err)
	}

	// THIS NODE'S OWN COPY, where it is the only holder: its seats are
	// answered, not refused naming a partition it serves.
	local := &fakeNode{name: "data-self", units: chartOf("self"), notReady: true}
	alone := newFleet(t).router(t, "data-self", local)
	if _, err := alone.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("the only copy, lagging, refused its own seat's read: %v", err)
	}
	if _, err := alone.WriterAs(swe).CreateTask(t.Context(), "op-own",
		tracker.Task{Project: "ENG"}, nil); err != nil {
		t.Fatalf("the only copy, lagging, refused its own seat's write: %v", err)
	}

	// AND STILL ASKED LAST: a peer whose copy does not lag answers first.
	peered := newFleet(t, "data-a")
	own := &fakeNode{name: "data-self", units: chartOf("self"), notReady: true}
	r := peered.router(t, "data-self", own)
	if _, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if own.askedFor("tasks") || !peered.nodes["data-a"].askedFor("tasks") {
		t.Fatal("this node's lagging copy ran the read ahead of a peer whose copy does not lag")
	}
}

// A SEARCH THAT REACHED NOTHING STILL SAYS SO: at layout 0 one partition, its
// only holder gone, is a search that failed — answered with no hits, as the
// seam requires, and with that partition named missing, so the turn-start
// block and the tool say "1 of 1 partitions did not answer" rather than
// "nothing is written down".
func TestASearchThatReachedNothingNamesWhatItMissed(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.nodes["data-a"].set(func(n *fakeNode) { n.silent = true })
	got := f.client.Knowledge().Search(t.Context(), knowledge.Query{Text: "deploys", Limit: 5})
	if len(got.Hits) != 0 || got.Partitions.Addressed != 1 || len(got.Partitions.Missing) != 1 ||
		got.Partitions.Missing[0].Partition != "estate.000" ||
		got.Partitions.Missing[0].Reason != statelog.MissingUnreachable {
		t.Fatalf("a search nothing answered = %+v, want estate.000 named unreachable", got)
	}
	const notice = "1 of 1 partitions did not answer; this list may be incomplete"
	if got.Partitions.Notice() != notice {
		t.Errorf("it renders %q, want %q", got.Partitions.Notice(), notice)
	}
}

// A SEARCH ON A FLEET A BURST PUT BEHIND RUNS ON ONE NODE, as a read does:
// under layout 0 a search is a gather of the one partition, and its last
// resort asks the lagging copies one at a time, stopping at the first that
// answers — never every data node at once for one search.
func TestASearchWithEveryCopyLaggingRunsOnce(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b", "data-c")
	for _, n := range f.nodes {
		n.set(func(n *fakeNode) { n.notReady = true })
	}
	got := f.client.Knowledge().Search(t.Context(), knowledge.Query{Text: "deploys", Limit: 5})
	if len(got.Hits) != 1 || !got.Partitions.Complete() {
		t.Fatalf("search with every copy lagging = %+v, want one copy's answer", got)
	}
	var ran []string
	for name, n := range f.nodes {
		if n.askedFor("search") {
			ran = append(ran, name)
		}
	}
	if len(ran) != 1 {
		t.Errorf("the search ran on %v, want exactly one lagging copy", ran)
	}
}

// A COPY TOLD TO ANSWER ANYWAY THAT REFUSES AGAIN IS NOT ASKED A THIRD TIME —
// an older build, which ignores the request's say-so — so a gather whose only
// holder is one ends naming the partition rather than circling it until the
// caller's deadline.
func TestALaggingCopyThatRefusesTheLastResortIsNotAskedAgain(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	node.set(func(n *fakeNode) { n.refusesLagging = true })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got := f.client.Knowledge().Search(ctx, knowledge.Query{Text: "deploys", Limit: 5})
	if ctx.Err() != nil || len(got.Partitions.Missing) != 1 ||
		got.Partitions.Missing[0].Reason != statelog.MissingUnserved {
		t.Fatalf("search = %+v (deadline %v), want estate.000 missing as unserved before the "+
			"caller's deadline", got, ctx.Err())
	}
	node.mu.Lock()
	asked := slices.Clone(node.asked)
	node.mu.Unlock()
	if len(asked) != 2 {
		t.Errorf("the lagging copy was asked %d times (%v), want twice: once, and once told "+
			"to answer anyway", len(asked), asked)
	}
}

// A GATHER THAT FAILS NAMES ITS OPERATION, as a single-partition read does:
// a placement that cannot say who serves a partition fails a search and a
// board alike with "estate: <operation>: read who serves <partition>", so the
// person reading it knows which call to look at.
func TestAGatherThatFailsNamesItsOperation(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.placement.set(func(p *fakePlacement) {
		p.servingErr = errors.New("the presence view answers unknown")
	})
	_, err := f.client.Work().Search(t.Context(), tracker.SearchQuery{Text: "deploys", Limit: 5})
	const want = "estate: tracker.search: read who serves estate.000: the presence view answers unknown"
	if err == nil || err.Error() != want {
		t.Errorf("a gather the placement could not route = %v, want %q", err, want)
	}
	_, err = f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	if err == nil || !strings.HasPrefix(err.Error(), "estate: tracker.tasks: read who serves estate.000: ") {
		t.Errorf("a single-partition read the placement could not route = %v, want the same shape", err)
	}
}

// A LAGGING COPY OF ITS OWN STILL ANSWERS WHEN THE PLACEMENT CANNOT SAY WHO
// SERVES. This node knows it holds the partition without asking anybody, so a
// view that answers unknown names no peer to prefer and takes nothing from the
// copy the walk passed over for lagging. Answered with the placement's error
// instead, a single data node a burst put behind refused every call its own
// seats made — while it kept them — for as long as its view could not answer.
// A node whose own copy runs nothing still gets the placement's reason.
func TestALaggingOwnCopyAnswersWhileThePlacementCannot(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.placement.set(func(p *fakePlacement) {
		p.servingErr = errors.New("the presence view answers unknown")
	})
	own := &fakeNode{name: "data-self", units: chartOf("self"), notReady: true}
	r := f.router(t, "data-self", own)
	floor := statelog.Position{Stream: trackerStream, Generation: 1, Seq: 4}
	r.Observe(floor)
	if _, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("a lagging copy of its own refused this node's read: %v", err)
	}
	own.mu.Lock()
	waited := slices.Contains(own.floors, floor)
	own.mu.Unlock()
	if !own.askedFor("tasks") || !waited {
		t.Fatalf("the read ran %v on this node's copy, floors %v, want it run there and held "+
			"to the node's floor", own.asked, own.floors)
	}
	if _, err := r.WriterAs(swe).CreateTask(t.Context(), "op-blind",
		tracker.Task{Project: "ENG"}, nil); err != nil {
		t.Fatalf("a lagging copy of its own refused this node's write: %v", err)
	}

	// A COPY THAT DOES NOT SERVE THE PARTITION is no holder at all, and the
	// answer is the placement's own reason — never "nobody serves it".
	own.set(func(n *fakeNode) { n.notHolder = true })
	_, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	var unserved *ErrPartitionUnserved
	if err == nil || errors.As(err, &unserved) ||
		!strings.Contains(err.Error(), "the presence view answers unknown") {
		t.Fatalf("a node serving nothing while its placement cannot answer = %v, want "+
			"the placement's reason", err)
	}
}

// A COPY THAT ANSWERS REQUESTS SERVES THEM AT ONCE, EVEN WHILE IT ADMITS NO
// SEAT. Admission's gate is strict — a lag of zero this instant — and a busy
// company's copy fails it on most instants, with a record in flight; a request
// gate that asked it sent every request to whichever holder happened to be
// level at that moment. Only admission's own ping asks it.
func TestACopyThatAdmitsNoSeatStillServesRequests(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	peer := f.nodes["data-a"]
	peer.set(func(n *fakeNode) { n.notAdmitting = true })
	if _, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if !peer.askedFor("tasks") || peer.askedFor("admits") {
		t.Fatalf("the holder was asked %v, want the read served without admission's question",
			peer.asked)
	}

	local := &fakeNode{name: "data-self", units: chartOf("self"), notAdmitting: true}
	r := newFleet(t).router(t, "data-self", local)
	if _, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks on this node's own copy: %v", err)
	}
	if !local.askedFor("tasks") || local.askedFor("admits") {
		t.Fatalf("this node's copy was asked %v, want the read served without admission's "+
			"question", local.asked)
	}
	// AND ADMISSION STILL REFUSES IT: the two gates disagree on purpose.
	if trackerServed, _, err := r.Serves(t.Context(), statelog.EstatePartition); err != nil ||
		trackerServed {
		t.Fatalf("a copy that admits no seat was admitted (%v, %v)", trackerServed, err)
	}
}

// THE HOLDER THAT ANSWERED LAST FOR A PARTITION IS ASKED FIRST NEXT TIME, ahead
// of the rendezvous winner — its applier is the one most likely to hold this
// node's writes — and loses that place the moment it goes silent, not only for
// as long as it stays suspect.
func TestTheHolderThatAnsweredLastIsAskedFirstUntilItGoesSilent(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	r := f.router(t, "agent-2", nil)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	r.now = func() time.Time { return time.Unix(0, clock.Load()) }
	winnerName, winner := f.first(t, r)
	runnerUp := f.other(winner)
	read := func() error {
		_, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
		return err
	}

	// THE RUNNER-UP ANSWERS ONCE, because the winner ran nothing...
	winner.set(func(n *fakeNode) { n.notHolder = true })
	if err := read(); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	winner.set(func(n *fakeNode) { n.notHolder = false })
	// ...AND IS ASKED FIRST FROM THEN ON.
	for range 3 {
		if err := read(); err != nil {
			t.Fatalf("tasks: %v", err)
		}
	}
	if winner.askedFor("tasks") {
		t.Fatal("the rendezvous winner was asked ahead of the holder that answered last")
	}

	// A HOLDER THAT GOES SILENT LOSES ITS PLACE for good: once its
	// suspicion lapses it is back in the rendezvous order, not first.
	runnerUp.set(func(n *fakeNode) { n.silent = true })
	winner.set(func(n *fakeNode) { n.notHolder = true })
	if err := read(); err == nil {
		t.Fatal("a read nobody could answer was answered")
	}
	runnerUp.set(func(n *fakeNode) { n.silent = false })
	winner.set(func(n *fakeNode) { n.notHolder = false })
	clock.Add(int64(2 * suspectFor))
	if got, _ := f.first(t, r); got != winnerName {
		t.Fatalf("%s is asked first after the holder that answered last went silent, want "+
			"the rendezvous winner %s", got, winnerName)
	}
}

// A WRITE THIS NODE'S OWN WRITE AUTHORITY REFUSED AT GATE 3 is taken to a peer
// under the same operation id, exactly as a remote holder's refusal is: it
// appended nothing, and a node that serves the partition can take it.
func TestALocalGateThreeRefusalMovesOnUnderTheSameOperation(t *testing.T) {
	t.Parallel()
	for _, reason := range []statelog.Reason{statelog.ReasonNotHolder, statelog.ReasonHoldingUnknown} {
		f := newFleet(t, "data-a")
		local := &fakeNode{name: "data-self", units: chartOf("self"),
			refusal: &statelog.Unavailable{Reason: reason, OpID: "op-l", Cause: statelog.ErrNotHolder}}
		r := f.router(t, "data-self", local)
		written, err := r.WriterAs(swe).CreateTask(t.Context(), "op-l", tracker.Task{Project: "ENG"}, nil)
		if err != nil || written.Key != "ENG-1" {
			t.Fatalf("%s: create = (%+v, %v), want the peer's answer", reason, written, err)
		}
		if got := f.nodes["data-a"].ops(); !slices.Equal(got, []string{"op-l"}) {
			t.Fatalf("%s: the peer ran %v, want [op-l]", reason, got)
		}
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
