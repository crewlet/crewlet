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

	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The streams of the tracker's and the knowledge base's logs, as a floor names
// them.
var (
	trackerStream = floorStreams[trackerDomain]
	pagesStream   = floorStreams[pagesDomain]
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
	patches   []worklet.TaskPatch
	turns     []worklet.TurnRecord
	keys      []string
	queries   []knowledge.Query
	units     worklet.Units
	notReady  bool
	behind    bool
	abandoned bool
	// behindOn is a node whose applier of one log is stuck: a floor on
	// that stream is never reached, every other is.
	behindOn string
	silent   bool
	failWith error
	written  statelog.Position

	// outOfService is a node whose copy is wrong, so it serves nothing.
	outOfService bool

	// notAdmitting is a copy that serves requests and admits no seat.
	notAdmitting bool

	// unvouched answers every create `unknown`, unvouched by its ledger,
	// and refusal answers every create with itself.
	unvouched bool
	refusal   error

	// release, when set, holds a floor wait until it is closed.
	release chan struct{}

	// refusesLagging refuses every request while its copy lags, one that
	// says "answer anyway" included: a server that does not honour the
	// request's say-so, which the router must still not circle.
	refusesLagging bool

	// directory is the node's identity directory, nil for a node whose core
	// runtime is not up.
	directory IdentityReader

	// hang, when set, is a node that takes a request and never answers it
	// until hang is closed — a wedged node, as the broker's own ask sees
	// one: waited for until the asker's deadline, where a node that is
	// simply gone is known to have answered nothing.
	hang chan struct{}

	// rooms is this node's chat, nil for a company whose chat is not the
	// engine's own — see chat_test.go.
	rooms *chatDesk
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

func (f *fakeNode) Tasks(_ context.Context, q worklet.Query, _ time.Time) (worklet.Answer, error) {
	f.note("tasks")
	if q.Units != f.units {
		return worklet.Answer{}, errors.New("the serving node's chart was not attached")
	}
	if f.failWith != nil {
		return worklet.Answer{}, f.failWith
	}
	return worklet.Answer{TotalHint: 7}, nil
}

func (f *fakeNode) CreateTask(_ context.Context, opID string, task worklet.Task,
	_ *worklet.Notify) (worklet.WriteResult, error) {
	f.note("create")
	f.mu.Lock()
	f.opIDs = append(f.opIDs, opID)
	unvouched, refusal := f.unvouched, f.refusal
	f.mu.Unlock()
	if refusal != nil {
		return worklet.WriteResult{}, refusal
	}
	out := worklet.WriteResult{Key: task.Project + "-1"}
	out.Result = statelog.Result{Outcome: statelog.OutcomeApplied, Position: f.written, OpID: opID}
	if unvouched {
		out.Result = statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: opID, Unvouched: true}
	}
	return out, nil
}

func (f *fakeNode) RecordTurn(_ context.Context, opID string,
	turn worklet.TurnRecord) (worklet.WriteResult, error) {
	f.note("record_turn")
	f.mu.Lock()
	f.opIDs = append(f.opIDs, opID)
	f.turns = append(f.turns, turn)
	f.mu.Unlock()
	return worklet.WriteResult{}, nil
}

func (f *fakeNode) UpdateTask(_ context.Context, _, _, _ string, _ uint64,
	patch worklet.TaskPatch, _ worklet.ChangeKind, _ *worklet.Notify) (worklet.WriteResult, error) {
	f.note("update")
	f.mu.Lock()
	f.patches = append(f.patches, patch)
	f.mu.Unlock()
	return worklet.WriteResult{}, nil
}

func (f *fakeNode) Comment(_ context.Context, actor doclet.Actor, _ string,
	in doclet.NewComment) (doclet.Comment, doclet.Written, error) {
	f.note("comment")
	f.keyed(actor.OpKey)
	return doclet.Comment{Body: in.Body}, doclet.Written{}, nil
}

func (f *fakeNode) Answer(_ context.Context, q knowledge.Query) (knowledge.Result, error) {
	f.note("search")
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	return knowledge.Result{
		Hits: []knowledge.Hit{{Title: "found on " + f.name}},
		Outcome: knowledge.Outcome{ServedMode: knowledge.ModeKeyword,
			Coverage: knowledge.Coverage{Nodes: []knowledge.NodeCoverage{}, Complete: true}},
	}, nil
}

func (f *fakeNode) Building(context.Context) bool { return false }

// backend is this node as a [Backend] over its copy of the estate.
func (f *fakeNode) backend() Backend {
	b := f.halves()
	f.mu.Lock()
	rooms := f.rooms
	f.mu.Unlock()
	if rooms != nil {
		b.Chat, b.ChatWriter, b.ChatSearch = rooms, rooms, rooms
	}
	return b
}

// halves is this node's tracker, knowledge base and identity directory, and
// the gates every operation passes.
func (f *fakeNode) halves() Backend {
	return Backend{
		Tracker: f, PageWriter: f, Knowledge: f, Identity: f.directory,
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
	}
}

// For implements [LocalBackends]: this node's copy serves unless it was told
// it is out of service.
func (f *fakeNode) For(context.Context) (Backend, bool) {
	f.mu.Lock()
	outOfService := f.outOfService
	f.mu.Unlock()
	if outOfService {
		return Backend{}, false
	}
	return f.backend(), true
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

// fakePlacement is the data nodes a case names, or an error where it says the
// placement cannot tell.
type fakePlacement struct {
	mu         sync.Mutex
	nodes      []string
	err        error
	unanswered []string
}

func (p *fakePlacement) Holders() ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	return slices.Clone(p.nodes), nil
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

// fleet is some data nodes serving on one broker, and a router on a node
// that holds nothing.
type fleet struct {
	broker *memory.Broker
	nodes  map[string]*fakeNode

	// placement is what the routers route by.
	placement *fakePlacement

	client *Router
}

func newFleet(t *testing.T, names ...string) *fleet {
	t.Helper()
	sorted := slices.Sorted(slices.Values(names))
	f := &fleet{broker: memory.NewBroker(), nodes: map[string]*fakeNode{},
		placement: &fakePlacement{nodes: sorted}}
	for _, name := range names {
		node := &fakeNode{name: name, units: chartOf(name)}
		f.nodes[name] = node
		stop, err := Serve(t.Context(), silencer{q: f.start(t), node: node}, ServerOptions{
			Self: name, Local: node, Seams: node.seams(), Envelopes: fleetEnvelopes,
		})
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
		Session: NewSession(), Envelopes: fleetEnvelopes}
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
			// A REFUSAL THE FLEET SIGNED, bound to the request it
			// answers, as a node of this fleet sends one.
			s.node.note("lagging")
			req := opened(raw)
			if req.ID == "" {
				return nil, errors.New("a request that does not open as the fleet's")
			}
			return fleetEnvelopes.sealReply(reply{Node: s.node.name, Request: req.ID,
				Unserved: unservedLagging, Detail: s.node.name + "'s copy lags its logs"}), nil
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

type namedChart struct{ worklet.Units }

// chartOf is a chart value distinguishable per node, compared by identity.
func chartOf(string) worklet.Units { return &namedChart{} }

// first is the node r asks first, which is the rendezvous winner among the
// nodes the placement names.
func (f *fleet) first(t *testing.T, r *Router) (string, *fakeNode) {
	t.Helper()
	nodes, _ := f.placement.Holders()
	ordered := r.order(nodes)
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

var swe = Actor{Handle: "swe", Kind: worklet.AuthorAgent}

// A READ IS ANSWERED BY A DATA NODE, with the serving node's own chart
// attached — the asking node's chart cannot cross a wire, and
// a read that arrived without one would render every unit unresolved.
func TestAReadIsAnsweredWithTheServingNodesChart(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	answer, err := f.client.Work().Tasks(t.Context(), worklet.Query{Units: chartOf("mine")}, time.Now())
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
	f.nodes["data-a"].failWith = errors.Join(errors.New("while reading"), worklet.ErrNoTask)
	_, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now())
	if !errors.Is(err, worklet.ErrNoTask) {
		t.Fatalf("err = %v, want worklet.ErrNoTask through the wire", err)
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
	if _, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); err != nil {
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
	_, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now())
	var unserved *ErrUnserved
	if err == nil || errors.As(err, &unserved) {
		t.Fatalf("an unknown placement answered %v, want the placement's own error", err)
	}
	// AND IT NAMES ITS OPERATION, so the person reading it knows which call
	// to look at.
	const want = "estate: tracker.tasks: read who holds the estate: the leases could not be listed"
	if err.Error() != want {
		t.Fatalf("the error %q, want %q", err, want)
	}
}

// A NODE THAT IS NOT SERVING, OR BEHIND THE CALLER'S FLOOR, RAN NOTHING, so
// even a write that is never repeated moves on from it.
func TestANodeThatRanNothingIsPassedOverForEveryClass(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t, f.client)
	first.set(func(n *fakeNode) { n.notReady = true })
	if _, _, err := f.client.Pages().Comment(t.Context(), doclet.Actor{}, "p1",
		doclet.NewComment{Body: "hi"}); err != nil {
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
	_, _, err := f.client.Pages().Comment(t.Context(), doclet.Actor{}, "p1", doclet.NewComment{Body: "hi"})
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
		worklet.Task{Project: "ENG"}, nil)
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

// A REPEATABLE WRITE NO NODE ANSWERED IS UNKNOWN, NEVER UNSERVED.
//
// Every holder was asked under the same operation, none ran it, and at least
// one never answered — so that one may have run it. "No data node could serve it" is the claim that
// nothing ran, and a caller told that writes again, under a FRESH id where it
// minted none, which is a second write: a purge appended twice, a task filed
// twice. A READ nobody answered is still unserved, since a read ran nothing
// that matters; and a write every node REFUSED is unserved too, since a node
// that answered "I did not run it" did not.
func TestARepeatableWriteNoNodeAnsweredIsUnknown(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	for _, node := range f.nodes {
		node.set(func(n *fakeNode) { n.silent = true })
	}
	_, err := f.client.WriterAs(swe).CreateTask(t.Context(), "op-9",
		worklet.Task{Project: "ENG"}, nil)
	var unserved *ErrUnserved
	if !errors.Is(err, ErrOutcomeUnknown) || errors.As(err, &unserved) {
		t.Fatalf("a write no node answered = %v, want ErrOutcomeUnknown and not "+
			"ErrUnserved", err)
	}
	for _, name := range []string{"data-a", "data-b"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the unknown write %q does not name %s, which it was sent to", err, name)
		}
	}

	if _, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); !errors.As(err, &unserved) || errors.Is(err, ErrOutcomeUnknown) {
		t.Errorf("a read no node answered = %v, want ErrUnserved", err)
	}

	refusing := newFleet(t, "data-a", "data-b")
	for _, node := range refusing.nodes {
		node.set(func(n *fakeNode) { n.outOfService = true })
	}
	_, err = refusing.client.WriterAs(swe).CreateTask(t.Context(), "op-10",
		worklet.Task{Project: "ENG"}, nil)
	if !errors.As(err, &unserved) || errors.Is(err, ErrOutcomeUnknown) {
		t.Errorf("a write every node refused to run = %v, want ErrUnserved", err)
	}

	// ONE SILENT NODE IS ENOUGH: the other answering that it did not run
	// the write says nothing about the one that never answered.
	mixed := newFleet(t, "data-a", "data-b")
	silentName, silent := mixed.first(t, mixed.client)
	silent.set(func(n *fakeNode) { n.silent = true })
	for name, node := range mixed.nodes {
		if name != silentName {
			node.set(func(n *fakeNode) { n.outOfService = true })
		}
	}
	_, err = mixed.client.WriterAs(swe).CreateTask(t.Context(), "op-13",
		worklet.Task{Project: "ENG"}, nil)
	if !errors.Is(err, ErrOutcomeUnknown) || errors.As(err, &unserved) ||
		!strings.Contains(err.Error(), silentName) {
		t.Errorf("a write one node never answered and the other refused = %v, want "+
			"ErrOutcomeUnknown naming %s", err, silentName)
	}

	// AND A WRITE WHOSE CALLER STOPPED LISTENING: the request was sent, and
	// the caller's context ending says nothing about whether it ran.
	wedged := newFleet(t, "data-a")
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	wedged.nodes["data-a"].set(func(n *fakeNode) { n.hang = hang })
	wedged.client.writeBudget = 10 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err = wedged.client.WriterAs(swe).CreateTask(ctx, "op-12",
		worklet.Task{Project: "ENG"}, nil)
	if !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a write whose caller's deadline ended while a node held it = %v, "+
			"want ErrOutcomeUnknown carrying the deadline", err)
	}
}

// A WRITE STOPPED BEFORE ITS NEXT ASK IS STILL ONE A SILENT NODE MAY HAVE RUN.
//
// A request that cannot be made at all ends the walk — no other node would
// fare better — but a node the write was already sent to, and never heard
// from, may have committed it. So the walk's answer is unknown, carrying what
// stopped it, rather than the bare failure of the ask that came after: told
// that, a caller sees a refusal and writes again under a fresh id.
func TestAWriteWhoseNextAskFailsAfterASilentNodeIsUnknown(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	firstName, first := f.first(t, f.client)
	first.set(func(n *fakeNode) { n.silent = true })
	broken := errors.New("the queue went down between two asks")
	r, err := NewRouter(RouterOptions{Self: "agent-1", Placement: f.placement,
		Session: NewSession(), Envelopes: fleetEnvelopes,
		Queue: &failingAfter{Asker: f.start(t), first: Subject(firstName), err: broken}})
	if err != nil {
		t.Fatal(err)
	}
	r.readBudget, r.writeBudget = 300*time.Millisecond, 300*time.Millisecond

	_, err = r.WriterAs(swe).CreateTask(t.Context(), "op-11", worklet.Task{Project: "ENG"}, nil)
	if !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, broken) {
		t.Fatalf("a write sent to a silent node and then stopped = %v, want "+
			"ErrOutcomeUnknown carrying what stopped it", err)
	}
	if !strings.Contains(err.Error(), firstName) {
		t.Errorf("the unknown write %q does not name %s, which it was sent to", err, firstName)
	}

	// A READ STOPPED THE SAME WAY RAN NOTHING THAT MATTERS: the failure as
	// it is.
	if _, err := r.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); !errors.Is(err, broken) ||
		errors.Is(err, ErrOutcomeUnknown) {
		t.Errorf("a read stopped by its next ask = %v, want the ask's own failure", err)
	}
}

// failingAfter asks the node on first through the fleet, and refuses every
// other ask as one that could not be made at all.
type failingAfter struct {
	Asker
	first string
	err   error
}

func (a *failingAfter) Ask(ctx context.Context, subject string, request []byte,
	want int) ([][]byte, error) {
	if subject != a.first {
		return nil, a.err
	}
	return a.Asker.Ask(ctx, subject, request, want)
}

// A WRITE'S POSITION IS THE NEXT REQUEST'S FLOOR, on whichever node answers
// it — on the log of the operation's own domain, and on no other: a floor on
// another domain's log, or on a stream no domain logs to, is never waited for.
func TestAWritesPositionIsTheNextReadsFloor(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	node.written = statelog.Position{Stream: trackerStream, Generation: 1, Seq: 99}
	if _, err := f.client.WriterAs(swe).CreateTask(t.Context(), "op-1",
		worklet.Task{Project: "ENG"}, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	node.set(func(n *fakeNode) { n.floors = nil })
	if _, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if len(node.floors) != 1 || node.floors[0] != node.written {
		t.Fatalf("the read waited for %v, want the write's %v", node.floors, node.written)
	}
	// ANOTHER DOMAIN'S LOG: a page write does not depend on the tracker's
	// rows, so it carries no floor on its log.
	node.set(func(n *fakeNode) { n.floors = nil })
	if _, _, err := f.client.Pages().Comment(t.Context(), doclet.Actor{}, "p", doclet.NewComment{}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if len(node.floors) != 0 {
		t.Errorf("a write to the pages log waited on %v, the tracker "+
			"log's floor — a log it does not depend on", node.floors)
	}
	// A STREAM NO DOMAIN LOGS TO: a position on it is a floor no
	// operation is about.
	f.client.Observe(statelog.Position{Stream: "CREWLET_OTHER_LOG", Generation: 1, Seq: 3})
	node.set(func(n *fakeNode) { n.floors = nil })
	if _, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	for _, at := range node.floors {
		if at.Stream != trackerStream {
			t.Errorf("a tracker read waited on %v, a log it does not read", at)
		}
	}
}

// A FLOOR ON ANOTHER DOMAIN'S LOG NEVER HOLDS AN OPERATION, though the estate
// carries both logs: no operation reads another domain's rows, so a seat a
// page change woke — its trigger a floor on the pages log — reads and writes
// the tracker on a node whose pages applier is stuck exactly as before it was
// woken, and only the knowledge base's own operations wait for that applier.
func TestAFloorOnAnotherDomainsLogNeverHoldsAnOperation(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	node.set(func(n *fakeNode) { n.behindOn = pagesStream })
	f.client.Observe(statelog.Position{Stream: pagesStream, Generation: 1, Seq: 9})

	if _, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); err != nil {
		t.Fatalf("a tracker read behind a pages floor = %v, want it answered", err)
	}
	if _, err := f.client.WriterAs(swe).CreateTask(t.Context(), "op-1",
		worklet.Task{Project: "ENG"}, nil); err != nil {
		t.Fatalf("a tracker write behind a pages floor = %v, want it written", err)
	}
	node.mu.Lock()
	carried := slices.Clone(node.floors)
	node.mu.Unlock()
	if len(carried) != 0 {
		t.Errorf("the tracker's operations waited on %v, the pages log's floor", carried)
	}
	// AND THE SERVER HOLDS TO IT TOO, whatever an asker sends: a tracker
	// read carrying a floor on the pages log does not wait for it.
	rep := askSigned(t, f.start(t), "data-a", request{Op: opTasks.spec.name,
		Floors: []statelog.Position{{Stream: pagesStream, Generation: 1, Seq: 9}}})
	if rep.Unserved != "" || rep.Err != nil {
		t.Fatalf("a tracker read with a pages floor = %+v, want it answered", rep)
	}

	// LONGER THAN THE HOLDER'S FLOOR WAIT, so it says it is behind before
	// the asker stops listening.
	f.client.writeBudget = statelog.ReadBudget + time.Second
	_, _, err := f.client.Pages().Comment(t.Context(), doclet.Actor{}, "p", doclet.NewComment{})
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) || !strings.Contains(err.Error(), "has not applied") ||
		node.askedFor("comment") {
		t.Fatalf("a page write behind its own log's floor = %v, want it unserved as "+
			"behind, and nothing written", err)
	}
}

// A NODE BEHIND THE FLOOR SAYS SO RATHER THAN ANSWER FROM BEFORE THE WRITE,
// and when every data node is behind the caller is told the estate is
// unserved — naming each node it asked.
func TestEveryDataNodeBehindTheFloorIsAnUnservedEstate(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.client.Observe(statelog.Position{Stream: trackerStream, Generation: 1, Seq: 5})
	f.nodes["data-a"].set(func(n *fakeNode) { n.behind = true })
	_, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now())
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) || !strings.HasPrefix(unserved.Detail, "data-a: ") {
		t.Fatalf("err = %v, want ErrUnserved naming data-a", err)
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
	if _, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); err != nil {
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
	patch := worklet.TaskPatch{
		Watch:  &worklet.WatchIntent{Handle: "ana", Watch: true},
		Relate: &worklet.RelationIntent{},
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
		if _, err := r.WriterAs(Actor{}).CreateTask(t.Context(), "op", worklet.Task{}, nil); err == nil {
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

// AN ESTATE NO DATA NODE SERVES IS AN ANSWER SAYING SO — never an empty
// result, which would read as a company with nothing in it.
func TestNoServingNodeSaysSo(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	_, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now())
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) {
		t.Fatalf("err = %v, want ErrUnserved", err)
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
	turn := worklet.TurnRecord{
		Task: "task-1", Seat: "swe", TurnID: "run-1", Outcome: "delivered",
		Phases: []string{"execute"},
		Spend: worklet.TurnSpend{Turns: 1, Rounds: 2, Input: 900, Output: 100,
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

// A DATA NODE ANSWERS ITS OWN SEATS IN-PROCESS, and asks nobody: it holds the
// whole estate, and a request over the broker to itself would be a round trip
// for nothing.
func TestADataNodeAnswersItself(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	local := &fakeNode{name: "data-self", units: chartOf("self")}
	r := f.router(t, "data-self", local)
	answer, err := r.Work().Tasks(t.Context(), worklet.Query{}, time.Now())
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
// A data node's seats may write through ANOTHER data node — this node's copy
// was out of service, or behind — and then read here: the write is on the log
// and not yet in this node's rows. A node that trusted its own copy because
// "it applies the write itself" answers its own seat from before a write it
// was told landed. So the local path waits for the floor exactly as a remote
// node would.
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
		_, err := r.Work().Tasks(t.Context(), worklet.Query{}, time.Now())
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
// and the router asks the next data node, which has.
func TestALocalCopyBehindItsFloorAsksAnotherHolder(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	local := &fakeNode{name: "data-self", units: chartOf("self"), behind: true}
	r := f.router(t, "data-self", local)
	floor := statelog.Position{Stream: trackerStream, Generation: 1, Seq: 42}
	r.Observe(floor)
	answer, err := r.Work().Tasks(t.Context(), worklet.Query{}, time.Now())
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
	if _, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); err != nil {
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
	if _, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); err != nil {
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
		worklet.Task{Project: "ENG"}, nil); err != nil {
		t.Fatalf("a tracker write with every copy lagging: %v", err)
	}
	if _, _, err := f.client.Pages().Comment(t.Context(), doclet.Actor{}, "p1",
		doclet.NewComment{Body: "hi"}); err != nil {
		t.Fatalf("a page write with every copy lagging: %v", err)
	}

	// THIS NODE'S OWN COPY, where it is the only data node: its seats are
	// answered, not refused as though nobody held the estate.
	local := &fakeNode{name: "data-self", units: chartOf("self"), notReady: true}
	alone := newFleet(t).router(t, "data-self", local)
	if _, err := alone.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); err != nil {
		t.Fatalf("the only copy, lagging, refused its own seat's read: %v", err)
	}
	if _, err := alone.WriterAs(swe).CreateTask(t.Context(), "op-own",
		worklet.Task{Project: "ENG"}, nil); err != nil {
		t.Fatalf("the only copy, lagging, refused its own seat's write: %v", err)
	}

	// AND STILL ASKED LAST: a peer whose copy does not lag answers first.
	peered := newFleet(t, "data-a")
	own := &fakeNode{name: "data-self", units: chartOf("self"), notReady: true}
	r := peered.router(t, "data-self", own)
	if _, err := r.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if own.askedFor("tasks") || !peered.nodes["data-a"].askedFor("tasks") {
		t.Fatal("this node's lagging copy ran the read ahead of a peer whose copy does not lag")
	}
}

// A SEARCH THAT REACHED NOTHING STILL SAYS SO: its only data node gone, a
// knowledge search is answered with no hits, as the seam requires — and with
// NO MODE SERVED and none of the corpus covered, so the turn-start block and
// the tool say the knowledge base could not be searched rather than "nothing
// is written down", which a seat acts on by writing a duplicate.
func TestASearchThatReachedNothingSaysItDidNotRun(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.nodes["data-a"].set(func(n *fakeNode) { n.silent = true })
	got := f.client.Knowledge().Search(t.Context(), knowledge.Query{Text: "deploys", Limit: 5})
	if len(got.Hits) != 0 || got.ServedMode != "" || got.Coverage.Complete ||
		got.Coverage.BucketsMissing != search.SearchShards || got.Coverage.Nodes == nil {
		t.Fatalf("a search nothing answered = %+v, want no mode served and every "+
			"bucket missing", got)
	}
	// AND THE ITEM SEARCH IS AN ERROR, never an empty list.
	if answer, err := f.client.Work().Search(t.Context(),
		worklet.SearchQuery{Text: "deploys", Limit: 5}); err == nil {
		t.Fatalf("an item search nothing answered = %+v, want its error", answer)
	}
}

// A SEARCH ON A FLEET A BURST PUT BEHIND RUNS ON ONE NODE, as a read does: its
// last resort asks the lagging copies one at a time, stopping at the first
// that answers — never every data node at once for one search.
func TestASearchWithEveryCopyLaggingRunsOnce(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b", "data-c")
	for _, n := range f.nodes {
		n.set(func(n *fakeNode) { n.notReady = true })
	}
	got := f.client.Knowledge().Search(t.Context(), knowledge.Query{Text: "deploys", Limit: 5})
	if len(got.Hits) != 1 || got.ServedMode == "" || !got.Coverage.Complete {
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

// A COPY TOLD TO ANSWER ANYWAY THAT REFUSES AGAIN IS NOT ASKED A THIRD TIME,
// whatever made it refuse, so a search whose only data node is one ends saying
// it did not run rather than circling it until the caller's deadline.
func TestALaggingCopyThatRefusesTheLastResortIsNotAskedAgain(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	node.set(func(n *fakeNode) { n.refusesLagging = true })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got := f.client.Knowledge().Search(ctx, knowledge.Query{Text: "deploys", Limit: 5})
	if ctx.Err() != nil || got.ServedMode != "" || got.Coverage.Complete {
		t.Fatalf("search = %+v (deadline %v), want it answered as not run before the "+
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

// A SEARCH THAT FAILS NAMES ITS OPERATION, as every read does: a placement
// that cannot say who holds the estate fails it with "estate: <operation>:
// read who holds the estate", so the person reading it knows which call to
// look at.
func TestASearchThatFailsNamesItsOperation(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.placement.set(func(p *fakePlacement) {
		p.err = errors.New("the presence view answers unknown")
	})
	_, err := f.client.Work().Search(t.Context(), worklet.SearchQuery{Text: "deploys", Limit: 5})
	const want = "estate: tracker.search: read who holds the estate: the presence view answers unknown"
	if err == nil || err.Error() != want {
		t.Errorf("a search the placement could not route = %v, want %q", err, want)
	}
}

// A LAGGING COPY OF ITS OWN STILL ANSWERS WHEN THE PLACEMENT CANNOT SAY WHO
// HOLDS THE ESTATE. This node knows it holds it without asking anybody, so a
// view that answers unknown names no peer to prefer and takes nothing from the
// copy the walk passed over for lagging. Answered with the placement's error
// instead, a single data node a burst put behind refused every call its own
// seats made — while it kept them — for as long as its view could not answer.
// A node whose own copy runs nothing still gets the placement's reason.
func TestALaggingOwnCopyAnswersWhileThePlacementCannot(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.placement.set(func(p *fakePlacement) {
		p.err = errors.New("the presence view answers unknown")
	})
	own := &fakeNode{name: "data-self", units: chartOf("self"), notReady: true}
	r := f.router(t, "data-self", own)
	floor := statelog.Position{Stream: trackerStream, Generation: 1, Seq: 4}
	r.Observe(floor)
	if _, err := r.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); err != nil {
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
		worklet.Task{Project: "ENG"}, nil); err != nil {
		t.Fatalf("a lagging copy of its own refused this node's write: %v", err)
	}

	// A COPY OUT OF SERVICE is no node to ask at all, and the answer is the
	// placement's own reason — never "nobody serves it".
	own.set(func(n *fakeNode) { n.outOfService = true })
	_, err := r.Work().Tasks(t.Context(), worklet.Query{}, time.Now())
	var unserved *ErrUnserved
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
	if _, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if !peer.askedFor("tasks") || peer.askedFor("admits") {
		t.Fatalf("the holder was asked %v, want the read served without admission's question",
			peer.asked)
	}

	local := &fakeNode{name: "data-self", units: chartOf("self"), notAdmitting: true}
	r := newFleet(t).router(t, "data-self", local)
	if _, err := r.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks on this node's own copy: %v", err)
	}
	if !local.askedFor("tasks") || local.askedFor("admits") {
		t.Fatalf("this node's copy was asked %v, want the read served without admission's "+
			"question", local.asked)
	}
	// AND ADMISSION STILL REFUSES IT: the two gates disagree on purpose.
	if served, err := r.Serves(t.Context()); err != nil || served.Tracker {
		t.Fatalf("a copy that admits no seat was admitted (%+v, %v)", served, err)
	}
}

// THE DATA NODE THAT ANSWERED LAST IS ASKED FIRST NEXT TIME, ahead of the
// rendezvous winner — its applier is the one most likely to hold this
// node's writes — and loses that place the moment it goes silent, not only for
// as long as it stays suspect.
func TestTheNodeThatAnsweredLastIsAskedFirstUntilItGoesSilent(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	r := f.router(t, "agent-2", nil)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	r.now = func() time.Time { return time.Unix(0, clock.Load()) }
	winnerName, winner := f.first(t, r)
	runnerUp := f.other(winner)
	read := func() error {
		_, err := r.Work().Tasks(t.Context(), worklet.Query{}, time.Now())
		return err
	}

	// THE RUNNER-UP ANSWERS ONCE, because the winner ran nothing...
	winner.set(func(n *fakeNode) { n.outOfService = true })
	if err := read(); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	winner.set(func(n *fakeNode) { n.outOfService = false })
	// ...AND IS ASKED FIRST FROM THEN ON.
	for range 3 {
		if err := read(); err != nil {
			t.Fatalf("tasks: %v", err)
		}
	}
	if winner.askedFor("tasks") {
		t.Fatal("the rendezvous winner was asked ahead of the node that answered last")
	}

	// A NODE THAT GOES SILENT LOSES ITS PLACE for good: once its suspicion
	// lapses it is back in the rendezvous order, not first.
	runnerUp.set(func(n *fakeNode) { n.silent = true })
	winner.set(func(n *fakeNode) { n.outOfService = true })
	if err := read(); err == nil {
		t.Fatal("a read nobody could answer was answered")
	}
	runnerUp.set(func(n *fakeNode) { n.silent = false })
	winner.set(func(n *fakeNode) { n.outOfService = false })
	clock.Add(int64(2 * suspectFor))
	if got, _ := f.first(t, r); got != winnerName {
		t.Fatalf("%s is asked first after the node that answered last went silent, want "+
			"the rendezvous winner %s", got, winnerName)
	}
}

// A COPY OUT OF SERVICE is never asked in-process — its seats are answered by
// the other data nodes — and answers another node's request `out_of_service`,
// having run nothing; the asker moves to the next data node.
func TestACopyOutOfServiceRunsNothing(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	local := &fakeNode{name: "data-self", units: chartOf("self"), outOfService: true}
	r := f.router(t, "data-self", local)
	_, first := f.first(t, r)
	first.set(func(n *fakeNode) { n.outOfService = true })
	written, err := r.WriterAs(swe).CreateTask(t.Context(), "op-9", worklet.Task{Project: "ENG"}, nil)
	if err != nil || written.Key != "ENG-1" {
		t.Fatalf("create = (%+v, %v)", written, err)
	}
	if local.askedFor("create") || first.askedFor("create") {
		t.Fatal("a copy out of service ran the write")
	}
	if got := f.other(first).ops(); !slices.Equal(got, []string{"op-9"}) {
		t.Fatalf("the serving node ran %v, want [op-9]", got)
	}

	rep := askSigned(t, f.start(t), first.name, request{Op: opTasks.spec.name})
	if rep.Unserved != unservedOutOfService || rep.Result != nil || rep.Err != nil {
		t.Fatalf("a copy out of service answered %+v, want %q and nothing run",
			rep, unservedOutOfService)
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

	written, err := r.WriterAs(swe).CreateTask(t.Context(), "op-u", worklet.Task{Project: "ENG"}, nil)
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

	// NOBODY VOUCHES: the answer is the unknown, never an unserved estate.
	second.set(func(n *fakeNode) { n.unvouched = true })
	written, err = r.WriterAs(swe).CreateTask(t.Context(), "op-v", worklet.Task{Project: "ENG"}, nil)
	if err != nil || written.Outcome != statelog.OutcomeUnknown || !written.Unvouched {
		t.Fatalf("with no holder vouching the write answered (%+v, %v), want the unvouched unknown",
			written.Result, err)
	}
}

// A REFUSAL ABOUT ANOTHER NODE'S COPY IS FINAL, and reaches the caller whole:
// the node that refused passed its own fences, and the remedy — the same
// operation once the duplicate window lets go — is the caller's. Taken to
// another data node, the same operation id would meet the same copy there.
func TestARefusalOfAnotherNodesCopyIsFinal(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t, f.client)
	first.set(func(n *fakeNode) {
		n.refusal = &statelog.Unavailable{Reason: statelog.ReasonEvicted, OpID: "op-c",
			CopyWriter: "data-z"}
	})
	_, err := f.client.WriterAs(swe).CreateTask(t.Context(), "op-c", worklet.Task{Project: "ENG"}, nil)
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonEvicted ||
		refusal.CopyWriter != "data-z" {
		t.Fatalf("a refusal naming another node's copy answered %v, want it whole", err)
	}
	if f.other(first).askedFor("create") {
		t.Error("a refusal that is final was taken to another node")
	}
}

// ADMISSION ASKS THE COPY THAT WILL SERVE THE SEAT: this node's own where it
// serves — never passed over for a peer's, since its seats read here — and a
// remote data node's where it does not.
func TestAdmissionAsksTheCopyThatWillServeTheSeat(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	peer := f.nodes["data-a"]
	local := &fakeNode{name: "data-self", units: chartOf("self"), notAdmitting: true}
	r := f.router(t, "data-self", local)
	served, err := r.Serves(t.Context())
	if err != nil || served.Tracker {
		t.Fatalf("a node whose own copy admits no seat was admitted (%+v, %v)", served, err)
	}
	if peer.askedFor("admits") {
		t.Fatal("a peer was asked for a node whose own copy serves")
	}

	// A COPY OUT OF SERVICE is asked of a data node whose copy serves.
	local.set(func(n *fakeNode) { n.outOfService = true })
	served, err = r.Serves(t.Context())
	if err != nil || !served.Tracker {
		t.Fatalf("a node served by a peer was not admitted (%+v, %v)", served, err)
	}

	// AND A REMOTE NODE THAT ADMITS NO SEAT IS PASSED OVER, like a node
	// that ran nothing.
	peer.set(func(n *fakeNode) { n.notAdmitting = true })
	if served, err = f.client.Serves(t.Context()); served.Tracker {
		t.Fatalf("a node was admitted on a data node that admits no seat (%v)", err)
	}
}

// ADMISSION'S PING IS ABOUT THE ONE ESTATE, so it carries no arguments, and a
// data node answers it with or without them: asked with none at all it is
// still the question whether its copy admits a seat — never refused as
// malformed, which the asker would take as final and withhold every seat
// claim on.
func TestAdmissionsPingNeedsNoArguments(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	for name, args := range map[string]json.RawMessage{
		"empty arguments": json.RawMessage(`{}`),
		"no arguments":    nil,
	} {
		rep := askSigned(t, f.start(t), "data-a", request{Op: "estate.ping", Args: args})
		var answer Served
		if rep.Err != nil || rep.Unserved != "" || json.Unmarshal(rep.Result, &answer) != nil ||
			!answer.Tracker {
			t.Fatalf("%s: the ping was answered %+v, want the admission answer", name, rep)
		}
	}
	if !f.nodes["data-a"].askedFor("admits") {
		t.Fatal("the ping was answered without asking the copy")
	}
}

// ADMISSION IS BOUNDED AS A WHOLE, not per data node: a sweep asks it on every
// pass, the first one at boot, and each listed node that is wedged would
// otherwise cost the sweep a whole read attempt. A fleet whose data nodes do
// not answer withholds the claim within the bound, and the sweep goes on.
func TestWedgedDataNodesCostAdmissionNoMoreThanItsBound(t *testing.T) {
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
		served, err := r.Serves(t.Context())
		done <- answer{served.Tracker, err}
	}()
	select {
	case got := <-done:
		if got.served || got.err == nil {
			t.Fatalf("wedged data nodes admitted a seat (%v, %v)", got.served, got.err)
		}
		if waited := time.Since(begun); waited > 2*time.Second {
			t.Fatalf("admission waited %s on wedged data nodes, past its %s bound",
				waited, r.admissionBudget)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admission waited on every wedged data node in turn, past its bound")
	}
}
