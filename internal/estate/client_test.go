package estate

import (
	"context"
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
}

func (f *fakeNode) note(op string) {
	f.mu.Lock()
	f.asked = append(f.asked, op)
	f.mu.Unlock()
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
	f.mu.Unlock()
	out := tracker.WriteResult{Key: task.Project + "-1"}
	out.Position = f.written
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

// backend is this node as a [Backend].
func (f *fakeNode) backend() Backend {
	return Backend{
		Tracker: f, PageWriter: f, Knowledge: f, Units: f.units,
		Writer: func(a Actor) TrackerWriter {
			f.mu.Lock()
			f.actors = append(f.actors, a)
			f.mu.Unlock()
			return f
		},
		Seat: func(handle string) (*org.Role, *org.Organization) {
			return &org.Role{Name: handle, DeclaredHandle: handle},
				&org.Organization{Name: "Acme", KnowledgeScope: []string{"ENG"}}
		},
		Committed: func(ctx context.Context, at statelog.Position) error {
			f.mu.Lock()
			f.floors = append(f.floors, at)
			behind, abandoned := f.behind, f.abandoned
			f.mu.Unlock()
			switch {
			case abandoned:
				return statelog.ErrWaitAbandoned
			case behind:
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		},
		Established: func(context.Context) bool { return !f.notReady },
	}
}

// fleet is some data nodes serving on one broker and one stateless client.
type fleet struct {
	nodes  map[string]*fakeNode
	client *Client

	// unanswered is every node the client told the roster did not answer.
	mu         sync.Mutex
	unanswered []string
}

func newFleet(t *testing.T, names ...string) *fleet {
	t.Helper()
	broker := memory.NewBroker()
	start := func() *memory.Queue {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.Background()) })
		return q
	}
	f := &fleet{nodes: map[string]*fakeNode{}}
	for _, name := range names {
		node := &fakeNode{name: name, units: chartOf(name)}
		f.nodes[name] = node
		q := start()
		stop, err := Serve(t.Context(), silencer{q: q, node: node}, name, func() (Backend, bool) {
			return node.backend(), true
		})
		if err != nil {
			t.Fatalf("serve %s: %v", name, err)
		}
		t.Cleanup(func() { _ = stop(context.Background()) })
	}
	client, err := NewClient(ClientOptions{Queue: start(), Self: "agent-1",
		Roster: fleetRoster{f: f}})
	if err != nil {
		t.Fatal(err)
	}
	client.readBudget, client.writeBudget = 300*time.Millisecond, 300*time.Millisecond
	f.client = client
	return f
}

// fleetRoster is every node the fleet started, and a record of every one the
// client reported unanswered.
type fleetRoster struct{ f *fleet }

func (r fleetRoster) DataNodes(context.Context) ([]string, error) {
	return slices.Sorted(keys(r.f.nodes)), nil
}

func (r fleetRoster) Unanswered(node string) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	r.f.unanswered = append(r.f.unanswered, node)
}

// silencer registers an answerer that answers nothing while its node is
// silent — a node that died after the roster last saw it.
type silencer struct {
	q    *memory.Queue
	node *fakeNode
}

func (s silencer) Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error) {
	return s.q.Serve(ctx, subject, func(ctx context.Context, raw []byte) ([]byte, error) {
		s.node.mu.Lock()
		silent := s.node.silent
		s.node.mu.Unlock()
		if silent {
			s.node.note("silent")
			return nil, errors.New("this node is gone")
		}
		return h(ctx, raw)
	})
}

type namedChart struct{ tracker.Units }

// chartOf is a chart value distinguishable per node, compared by identity.
func chartOf(name string) tracker.Units { return &namedChart{} }

func keys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// the node the client asks first, which is the rendezvous winner.
func (f *fleet) first(t *testing.T) (string, *fakeNode) {
	t.Helper()
	nodes, err := f.client.candidates(t.Context())
	if err != nil || len(nodes) == 0 {
		t.Fatalf("candidates: %v %v", nodes, err)
	}
	return nodes[0], f.nodes[nodes[0]]
}

// A READ IS ANSWERED BY A DATA NODE, with the serving node's own chart
// attached — the stateless node's chart cannot cross a wire, and a read that
// arrived without one would render every unit unresolved.
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
	f.nodes["data-a"].failWith = errorsJoin(tracker.ErrNoTask)
	_, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	if !errors.Is(err, tracker.ErrNoTask) {
		t.Fatalf("err = %v, want tracker.ErrNoTask through the wire", err)
	}
}

func errorsJoin(err error) error { return errors.Join(errors.New("while reading"), err) }

// A NODE THAT DOES NOT ANSWER IS PASSED OVER FOR A READ, and asked last until
// its suspicion lapses — so a node that died costs one attempt per client
// rather than one per request.
func TestAnUnansweredReadMovesOnAndTheSilentNodeIsAskedLast(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	firstName, first := f.first(t)
	first.silent = true
	if _, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("a read with a live second node failed: %v", err)
	}
	next, _ := f.first(t)
	if next == firstName {
		t.Errorf("the silent node %s is still asked first", firstName)
	}
	// AND THE ROSTER IS TOLD, so a node that left on a clean stop is gone
	// from its next answer rather than from the one a heartbeat later.
	f.mu.Lock()
	told := slices.Clone(f.unanswered)
	f.mu.Unlock()
	if !slices.Contains(told, firstName) {
		t.Errorf("the roster was told %v went unanswered, want %s among them",
			told, firstName)
	}
}

// A ROSTER THAT CANNOT SAY WHO HOLDS DATA IS AN ERROR THAT SAYS SO, never an
// empty fleet: "no data node is live" names a role to give a node, and giving
// one to a node on a fleet whose coordination blinked is the wrong remedy.
func TestAnUnknownRosterIsNotAnEmptyFleet(t *testing.T) {
	t.Parallel()
	client, err := NewClient(ClientOptions{Queue: memory.NewBroker().Client(),
		Self: "agent-1", Roster: unknownRoster{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	if err == nil || errors.Is(err, ErrNoDataNode) {
		t.Fatalf("an unknown roster answered %v, want the roster's own error "+
			"rather than ErrNoDataNode", err)
	}
	if !strings.Contains(err.Error(), "the leases could not be listed") {
		t.Fatalf("the error %q does not carry the roster's reason", err)
	}
}

type unknownRoster struct{}

func (unknownRoster) DataNodes(context.Context) ([]string, error) {
	return nil, errors.New("the leases could not be listed")
}

func (unknownRoster) Unanswered(string) {}

// A NODE THAT IS NOT ESTABLISHED, OR BEHIND THE CALLER'S FLOOR, RAN NOTHING,
// so even a write moves on from it.
func TestANodeThatRanNothingIsPassedOverForEveryClass(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t)
	first.notReady = true
	if _, _, err := f.client.Pages().Comment(t.Context(), pages.Actor{}, "p1",
		pages.NewComment{Body: "hi"}); err != nil {
		t.Fatalf("a page write with a ready second node failed: %v", err)
	}
	if slices.Contains(first.asked, "comment") {
		t.Error("the node that was not established ran the write")
	}
}

// A PAGE WRITE NOBODY ANSWERED IS NEVER ASKED AGAIN: it has no operation id a
// repeat could be collapsed on, so a second node would write a second comment.
func TestAnUnansweredPageWriteIsUnknownAndNotRepeated(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	firstName, first := f.first(t)
	first.silent = true
	_, _, err := f.client.Pages().Comment(t.Context(), pages.Actor{}, "p1", pages.NewComment{Body: "hi"})
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("err = %v, want ErrOutcomeUnknown", err)
	}
	for name, node := range f.nodes {
		if name != firstName && len(node.asked) > 0 {
			t.Errorf("%s was asked %v after an unanswered page write", name, node.asked)
		}
	}
}

// A TRACKER WRITE NOBODY ANSWERED IS ASKED AGAIN, UNDER THE SAME OPERATION ID:
// the ledger answers a repeat with the first copy's result, which is exactly
// the lost acknowledgement the id exists for.
func TestAnUnansweredTrackerWriteRepeatsUnderTheSameOperation(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t)
	first.silent = true
	actor := Actor{Handle: "swe", Kind: tracker.AuthorAgent}
	written, err := f.client.WriterAs(actor).CreateTask(t.Context(), "op-7",
		tracker.Task{Project: "ENG"}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if written.Key != "ENG-1" {
		t.Errorf("key = %q", written.Key)
	}
	var second *fakeNode
	for _, node := range f.nodes {
		if node != first {
			second = node
		}
	}
	if !slices.Equal(second.opIDs, []string{"op-7"}) {
		t.Errorf("the second node ran op ids %v, want [op-7]", second.opIDs)
	}
	if len(second.actors) != 1 || second.actors[0].Handle != "swe" {
		t.Errorf("the write acted as %+v, want the seat that asked", second.actors)
	}
}

// A WRITE'S POSITION IS THE NEXT REQUEST'S FLOOR, on whichever node answers
// it — the read-your-writes wait a stateless node has no applier to do.
func TestAWritesPositionIsTheNextReadsFloor(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	node.written = statelog.Position{Stream: trackerStream, Generation: 1, Seq: 99}
	if _, err := f.client.WriterAs(Actor{Handle: "swe", Kind: tracker.AuthorAgent}).
		CreateTask(t.Context(), "op-1", tracker.Task{Project: "ENG"}, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	node.floors = nil
	if _, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if len(node.floors) != 1 || node.floors[0] != node.written {
		t.Fatalf("the read waited for %v, want the write's %v", node.floors, node.written)
	}
	// AND ONLY ON ITS OWN STREAM: a page read is not held for a tracker write.
	node.floors = nil
	if _, _, err := f.client.Pages().Comment(t.Context(), pages.Actor{}, "p", pages.NewComment{}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if len(node.floors) != 0 {
		t.Errorf("a page write waited on %v, a tracker floor", node.floors)
	}
}

// A NODE BEHIND THE FLOOR SAYS SO RATHER THAN ANSWER FROM BEFORE THE WRITE,
// and when every node is behind the caller is told there was nobody.
func TestEveryNodeBehindTheFloorIsNoAnswer(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.client.Observe(statelog.Position{Stream: trackerStream, Generation: 1, Seq: 5})
	f.nodes["data-a"].behind = true
	_, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	if !errors.Is(err, ErrNoDataNode) {
		t.Fatalf("err = %v, want ErrNoDataNode", err)
	}
	if slices.Contains(f.nodes["data-a"].asked, "tasks") {
		t.Error("a node behind the floor answered anyway")
	}
}

// A FLOOR ON AN ABANDONED GENERATION IS DROPPED rather than carried for ever:
// no node will reach it, and a client that kept it would be refused every
// read until it restarted.
func TestAFloorNoNodeCanReachIsForgotten(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.client.Observe(statelog.Position{Stream: trackerStream, Generation: 1, Seq: 5})
	f.nodes["data-a"].abandoned = true
	if _, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if floors := f.client.floors(trackerStream); len(floors) != 0 {
		t.Errorf("the client still carries %v", floors)
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
	if _, err := f.client.WriterAs(Actor{Handle: "swe", Kind: tracker.AuthorAgent}).
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
// row nobody can attribute is not an audit trail.
func TestAWriteThatNamesNobodyIsRefused(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	_, err := f.client.WriterAs(Actor{}).CreateTask(t.Context(), "op", tracker.Task{}, nil)
	if err == nil {
		t.Fatal("a write naming nobody was run")
	}
	if slices.Contains(f.nodes["data-a"].asked, "create") {
		t.Error("the serving node ran it")
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

// NO DATA NODE IS AN ANSWER THAT SAYS SO, naming the role to give a node.
func TestNoDataNodeSaysSo(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	_, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	if !errors.Is(err, ErrNoDataNode) {
		t.Fatalf("err = %v, want ErrNoDataNode", err)
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
	_, first := f.first(t)
	first.silent = true
	turn := tracker.TurnRecord{
		Task: "task-1", Seat: "swe", TurnID: "run-1", Outcome: "delivered",
		Phases: []string{"execute"},
		Spend: tracker.TurnSpend{Turns: 1, Rounds: 2, Input: 900, Output: 100,
			CacheRead: 400, WallMs: 3100},
	}
	if _, err := f.client.WriterAs(Actor{Handle: "swe", Kind: tracker.AuthorAgent}).
		RecordTurn(t.Context(), "op-turn", turn); err != nil {
		t.Fatalf("record the turn: %v", err)
	}
	var second *fakeNode
	for _, node := range f.nodes {
		if node != first {
			second = node
		}
	}
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
