package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/rendezvous"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
	"github.com/crewlet/crewlet/internal/usage"
)

// The streams of the tracker's, the knowledge base's and the code host's logs,
// as a floor names them.
var (
	trackerStream = floorStreams[trackerDomain]
	pagesStream   = floorStreams[pagesDomain]
	codeStream    = floorStreams[codeDomain]
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
	created   []tracker.Task
	patches   []tracker.TaskPatch
	turns     []tracker.TurnRecord
	keys      []string
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

	// outOfService is a node whose copy of every domain is wrong, so it
	// serves nothing; outOfServiceOn names one domain's copy (or the seat
	// copy, [SeatCopy]) that is.
	outOfService   bool
	outOfServiceOn map[string]bool

	// notAdmitting is a copy that serves requests and admits no seat.
	notAdmitting bool

	// behindConfig is a node the control plane has confirmed behind the
	// company's configuration — shed or stuck ([Backend.Current] false).
	behindConfig bool

	// spend is the usage spend this node pages out, 64 rows a page at any
	// budget and every row at none, where it is set; spendAfters is every
	// group a page of it was asked after, and spendBudgets the budget each
	// page was asked at; and stuckPages answers every page ending where it
	// began.
	spend        []usage.SpendRow
	spendAfters  []usage.Group
	spendBudgets []int
	stuckPages   bool

	// repoBudgets is the budget every page of a repository's readers was
	// asked at, and repoElided how many times its elided count was.
	repoBudgets []int
	repoElided  int

	// unvouched answers every create `unknown`, unvouched by its ledger,
	// and refusal answers every create with itself.
	unvouched bool
	refusal   error

	// release, when set, holds a floor wait until it is closed.
	release chan struct{}

	// levelRead, when set, is what a task read runs before it answers: a
	// read through a REAL statelog reader, whose own waits — the barrier, the
	// level — are the operation's rather than the serving node's
	// ([laggingLinearizableRead]).
	levelRead func(context.Context) error

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

	// requests is every request that reached this node over the broker,
	// whatever it answered — run, refused or silent — and anyway is how
	// many of them its asker sent as the last resort, told to answer
	// anyway ([request.AcceptLagging]).
	requests int
	anyway   int

	// indexBuilding is a node whose index has not finished its first lap.
	indexBuilding bool

	// proposals is every push proposed to this node's committer,
	// compactions every compaction, expunges every expunge record, changes
	// every write of a change, conversations every write of a change's
	// conversation, codeSeq the code log's last position it committed at, and
	// codeErr what its code half refuses every operation with — a proposal and
	// every read — where it is set (codeops_test.go).
	proposals     []code.Proposal
	compactions   []code.CompactionProposal
	expunges      []code.ExpungeProposal
	changes       []code.ChangeProposal
	conversations []code.ConversationWrite
	codeSeq       uint64
	codeErr       error

	// codeHalf, where it is set, is this node's code half in place of the
	// fake's own answers: a REAL reader, for a case about what the code
	// domain itself answers on one node and the walk makes of it.
	codeHalf CodeServer

	// holdProposals, when set, is a committer whose batch is held: every
	// proposal waits inside it until the channel is closed, counted in
	// proposing as it arrives — a push queued behind its repository's
	// earlier ones, holding whatever slot it was answered through.
	holdProposals chan struct{}
	proposing     atomic.Int32
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

func (f *fakeNode) Tasks(ctx context.Context, q tracker.Query, _ time.Time) (tracker.Answer, error) {
	f.note("tasks")
	if q.Units != f.units {
		return tracker.Answer{}, errors.New("the serving node's chart was not attached")
	}
	if f.failWith != nil {
		return tracker.Answer{}, f.failWith
	}
	f.mu.Lock()
	read := f.levelRead
	f.mu.Unlock()
	if read != nil {
		if err := read(ctx); err != nil {
			return tracker.Answer{}, err
		}
	}
	return tracker.Answer{TotalHint: 7}, nil
}

func (f *fakeNode) CreateTask(_ context.Context, opID string, task tracker.Task,
	_ *tracker.Notify) (tracker.WriteResult, error) {
	f.note("create")
	f.mu.Lock()
	f.opIDs = append(f.opIDs, opID)
	f.created = append(f.created, task)
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

func (f *fakeNode) Comment(_ context.Context, actor pages.Actor, _ string,
	in pages.NewComment) (pages.Comment, pages.Written, error) {
	f.note("comment")
	// THE TURN'S KEY FIRST, the order the pages store derives a comment's
	// id in.
	if in.TurnKey != "" {
		f.keyed(in.TurnKey)
	} else {
		f.keyed(actor.OpKey)
	}
	return pages.Comment{Body: in.Body}, pages.Written{}, nil
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

// LinkedFrom answers one page link naming this node, or that its index is
// still building.
func (f *fakeNode) LinkedFrom(_ context.Context, pageID string) (search.Backlinks, error) {
	f.note("linked_from")
	f.mu.Lock()
	building := f.indexBuilding
	f.mu.Unlock()
	if building {
		return search.Backlinks{}, search.ErrIndexBuilding
	}
	return search.Backlinks{Pages: []search.PageLink{{ID: pageID, Title: "linked on " + f.name}},
		Tasks: []search.TaskLink{}, PagesTotal: 1}, nil
}

// fakeUsage is a node's usage half: every read answers one page of one row
// naming the node, so a case can tell which copy answered — except the spend
// of a node given rows of it to page ([fakeNode.spend]).
type fakeUsage struct{ *fakeNode }

func (f fakeUsage) SpendPage(_ context.Context, q usage.SpendQuery, after usage.Group,
	budget int) (usage.Page[usage.SpendRow], error) {

	f.note("usage.spend")
	f.mu.Lock()
	f.spendAfters = append(f.spendAfters, after)
	f.spendBudgets = append(f.spendBudgets, budget)
	rows, stuck := f.spend, f.stuckPages
	f.mu.Unlock()
	switch {
	case stuck:
		return usage.Page[usage.SpendRow]{Rows: []usage.SpendRow{{Day: q.From, Node: f.name}},
			Next: after}, nil
	case rows == nil:
		return onePage(usage.SpendRow{Day: q.From, Node: f.name}), nil
	}
	// SIXTY-FOUR ROWS A PAGE whatever the budget, every row its own group:
	// the asker must follow the pages it is handed, not compute them. No
	// budget is the whole read, as usage.Fill has it.
	var page usage.Page[usage.SpendRow]
	for _, r := range rows {
		if !after.Before(r.Group()) {
			continue
		}
		if budget > 0 && len(page.Rows) == 64 {
			page.Next = page.Rows[63].Group()
			return page, nil
		}
		page.Rows = append(page.Rows, r)
	}
	page.Done = true
	return page, nil
}

// onePage is a paged read's only page.
func onePage[R any](rows ...R) usage.Page[R] { return usage.Page[R]{Rows: rows, Done: true} }

func (f fakeUsage) SeatTurnsPage(context.Context, usage.SpendQuery, usage.Group, int) (usage.Page[usage.TurnsRow], error) {
	return onePage(usage.TurnsRow{Node: f.name}), nil
}

func (f fakeUsage) PersonSpendPage(context.Context, usage.PersonQuery, usage.Group, int) (usage.Page[usage.PersonRow], error) {
	return onePage(usage.PersonRow{Node: f.name}), nil
}

func (f fakeUsage) SeatDaysPage(context.Context, usage.SpendQuery, usage.Group, int) (usage.Page[usage.SeatDay], error) {
	return onePage(usage.SeatDay{Node: f.name}), nil
}

func (f fakeUsage) ScheduleRuns(context.Context, usage.ScheduleRunsQuery) ([]usage.ScheduleRun, bool, error) {
	return []usage.ScheduleRun{{Node: f.name}}, true, nil
}

func (f fakeUsage) PageReadsPage(context.Context, usage.PageReadsQuery, usage.Group, int) (usage.Page[usage.PageRead], error) {
	return onePage(usage.PageRead{Node: f.name}), nil
}

func (f fakeUsage) ReadsElided(context.Context, string, string) (int64, error) { return 3, nil }

// fakeReaders are the readers every node's fake usage half holds of a
// repository: three, in reader order.
var fakeReaders = []usage.RepoReader{
	{PrincipalKind: "agent", Principal: "dev", Seat: "dev", Via: "tool", Reads: 4},
	{PrincipalKind: "operator", Principal: "pat:1", Seat: "maya", Via: "git", Reads: 2},
	{PrincipalKind: "operator", Principal: "token:ci", Via: "git", Reads: 9},
}

// RepoReadsPage answers ONE READER A PAGE whatever the budget, after the
// reader after, noting the budget the page was asked at.
func (f fakeUsage) RepoReadsPage(_ context.Context, _ usage.RepoReadsQuery, after usage.Group,
	budget int) (usage.Page[usage.RepoReader], error) {

	f.note("usage.repo_reads")
	f.mu.Lock()
	f.repoBudgets = append(f.repoBudgets, budget)
	f.mu.Unlock()
	var page usage.Page[usage.RepoReader]
	for _, r := range fakeReaders {
		if !after.Before(r.Group()) {
			continue
		}
		if len(page.Rows) == 1 {
			page.Next = page.Rows[0].Group()
			return page, nil
		}
		page.Rows = append(page.Rows, r)
	}
	page.Done = true
	return page, nil
}

func (f fakeUsage) RepoReadsElided(context.Context, usage.RepoReadsQuery) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repoElided++
	return 5, nil
}

// backend is this node as a [Backend] over its copy of the estate.
func (f *fakeNode) backend() Backend {
	var half CodeServer = f
	f.mu.Lock()
	if f.codeHalf != nil {
		half = f.codeHalf
	}
	f.mu.Unlock()
	return Backend{
		Tracker: f, PageWriter: f, Knowledge: f, Backlinks: f, Usage: fakeUsage{f}, Code: half,
		Identity: f.directory,
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
		Current: func() bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return !f.behindConfig
		},
	}
}

// For implements [LocalBackends]: this node's copy of domain serves unless it
// was told that copy, or every copy, is out of service. Its SEAT copy is out
// whenever its tracker's or its knowledge base's is, which is what the engine's
// own domains declare: both gate seat admission.
func (f *fakeNode) For(_ context.Context, domain string) (Backend, bool) {
	f.mu.Lock()
	out := f.outOfService || f.outOfServiceOn[domain]
	if domain == SeatCopy {
		out = out || f.outOfServiceOn[trackerDomain] || f.outOfServiceOn[pagesDomain]
	}
	f.mu.Unlock()
	if out {
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

// fakePlacement is the nodes a case names, or an error where it says the
// placement cannot tell — and, as a git placement, what each git node it names
// says it can give its background work: a node said names says its budget
// (zero for one that says it has nothing to give), and every other node it
// names is NOT SAYING, so a router over a placement that says none places
// every repository's maintenance on its home.
type fakePlacement struct {
	mu         sync.Mutex
	nodes      []string
	said       map[string]int64
	err        error
	unanswered []string
	// reads counts the Holders and the Listings it answered: how often a
	// router read it.
	reads int
}

func (p *fakePlacement) Holders() ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	if p.err != nil {
		return nil, p.err
	}
	return slices.Clone(p.nodes), nil
}

// readsSoFar is how many times Holders or Listing has been asked.
func (p *fakePlacement) readsSoFar() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reads
}

// Listing is the nodes it names and what each of them says, as one listing.
func (p *fakePlacement) Listing() (GitListing, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	if p.err != nil {
		return GitListing{}, p.err
	}
	listing := GitListing{Nodes: slices.Clone(p.nodes), Budgets: map[string]int64{}}
	for _, node := range p.nodes {
		if budget, said := p.said[node]; said {
			listing.Budgets[node] = budget
		}
	}
	return listing, nil
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

	// git is who serves git as the routers ask a git read of: no node,
	// unless a case names some — and what each git node advertises it can
	// give its background work, as the routers weigh a repository's
	// maintainer by: nobody says anything, unless a case writes it.
	git *fakePlacement

	client *Router
}

func newFleet(t *testing.T, names ...string) *fleet {
	t.Helper()
	sorted := slices.Sorted(slices.Values(names))
	f := &fleet{broker: memory.NewBroker(), nodes: map[string]*fakeNode{},
		placement: &fakePlacement{nodes: sorted}, git: &fakePlacement{}}
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
// local where it is given one, waiting PRODUCTION'S TIMING.
//
// Its read attempt is production's, which holds every wait a holder makes at
// its whole budget ([ReadAttempt]), rather than anything nearer the shortest a
// router takes ([minReadAttempt]): a holder ends its waits inside whatever
// attempt it is given, but a node that answered must still never be judged
// silent because the machine running the suite was busy delivering its reply,
// and a cut-short wait would turn a case's slow-but-healthy holder into a
// `behind` it did not mean to test. Its write attempt cannot be shorter than
// [minWriteAttempt] at all.
// And a node that is gone costs none of either: the twin's ask ends as soon as
// every answerer has answered or failed, so only a node that HANGS waits an
// attempt out, and the case that hangs one gives its router a timing of its
// own ([fleet.routerTimed]).
func (f *fleet) router(t *testing.T, self string, local *fakeNode) *Router {
	t.Helper()
	return f.routerTimed(t, self, local, Production())
}

// routerTimed is [fleet.router] waiting timing rather than the fleet's.
func (f *fleet) routerTimed(t *testing.T, self string, local *fakeNode, timing Timing) *Router {
	t.Helper()
	opts := RouterOptions{Queue: f.start(t), Self: self, Placement: f.placement, Git: f.git,
		Session: NewSession(), Timing: timing, Envelopes: fleetEnvelopes}
	if local != nil {
		opts.Local, opts.Seams = local, local.seams()
	}
	r, err := NewRouter(opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// silencer registers an answerer that answers nothing while its node is
// silent — a node that died after the placement last saw it.
type silencer struct {
	q    *memory.Queue
	node *fakeNode
}

func (s silencer) Serve(ctx context.Context, subject string, answers int,
	h queue.AnswerFunc) (queue.Unsubscribe, error) {

	return s.q.Serve(ctx, subject, answers, func(ctx context.Context, raw []byte) ([]byte, error) {
		// OPENED AS A FLEET MEMBER READS IT, since what arrives is sealed:
		// the zero request where it is not the fleet's.
		req := opened(raw)
		s.node.mu.Lock()
		s.node.requests++
		if req.AcceptLagging {
			s.node.anyway++
		}
		silent, hang, refusesLagging := s.node.silent, s.node.hang, s.node.refusesLagging
		s.node.mu.Unlock()
		if refusesLagging {
			// A REFUSAL THE FLEET SIGNED, bound to the request it
			// answers, as a node of this fleet sends one.
			s.node.note("lagging")
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

type namedChart struct{ tracker.Units }

// chartOf is a chart value distinguishable per node, compared by identity.
func chartOf(string) tracker.Units { return &namedChart{} }

// first is the node r asks first, which is the rendezvous winner among the
// nodes the placement names.
func (f *fleet) first(t *testing.T, r *Router) (string, *fakeNode) {
	t.Helper()
	nodes, _ := f.placement.Holders()
	ordered := r.order(opTasks.spec, nodes)
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

// A READ IS ANSWERED BY A DATA NODE, with the serving node's own chart
// attached — the asking node's chart cannot cross a wire, and
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

// A READ A COPY REFUSES ABOUT ITSELF IS ASKED OF THE NEXT NODE: a node whose
// applier is behind the barrier its linearizable read appended ran nothing,
// and a peer whose applier keeps up serves the read — the refusal is the
// answer only where no node does better, and keeps its identity then. A
// refusal about the whole fleet is final at once, since no node does better.
func TestAReadACopyRefusesAboutItselfIsAskedOfTheNextNode(t *testing.T) {
	t.Parallel()
	behind := &statelog.Refused{Code: statelog.RefuseBehind, Level: statelog.ReadLinearizable,
		Detail: "this node has not reached its barrier within 2s", RetryAfter: 3 * time.Second}
	t.Run("a peer serves it", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t, "data-a", "data-b")
		_, first := f.first(t, f.client)
		first.set(func(n *fakeNode) { n.failWith = behind })
		answer, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
		if err != nil || answer.TotalHint != 7 {
			t.Fatalf("a read the first node refused as behind = (%+v, %v), want the peer's answer", answer, err)
		}
		if peer := f.other(first); !peer.askedFor("tasks") {
			t.Errorf("%s was never asked once %s refused the read about itself", peer.name, first.name)
		}
	})
	t.Run("nobody does better", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t, "data-a", "data-b")
		for _, n := range f.nodes {
			n.set(func(n *fakeNode) { n.failWith = behind })
		}
		_, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
		var refused *statelog.Refused
		if !errors.As(err, &refused) || refused.Code != statelog.RefuseBehind || refused.RetryAfter != behind.RetryAfter {
			t.Fatalf("a read every node refused as behind = %v, want that refusal with its hint", err)
		}
	})
	t.Run("a refusal about the whole fleet is final", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t, "data-a", "data-b")
		_, first := f.first(t, f.client)
		first.set(func(n *fakeNode) {
			n.failWith = &statelog.Refused{Code: statelog.RefuseLogFull, Level: statelog.ReadLinearizable}
		})
		_, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
		var refused *statelog.Refused
		if !errors.As(err, &refused) || refused.Code != statelog.RefuseLogFull {
			t.Fatalf("a read refused log_full = %v, want that refusal", err)
		}
		if peer := f.other(first); peer.askedFor("tasks") {
			t.Errorf("%s was asked a read %s refused for a full log, which every node refuses alike",
				peer.name, first.name)
		}
	})
	t.Run("this node's own copy", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t, "data-b")
		local := &fakeNode{name: "data-a", units: chartOf("data-a"), failWith: behind}
		answer, err := f.router(t, "data-a", local).Work().Tasks(t.Context(), tracker.Query{}, time.Now())
		if err != nil || answer.TotalHint != 7 {
			t.Fatalf("a read this node's own copy refused as behind = (%+v, %v), want data-b's answer",
				answer, err)
		}
	})
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

// A CALLER THAT STOPS WAITING SUSPECTS NO NODE: silence is evidence that a
// node is gone only through a wait the ROUTER chose for it, and a caller that
// hung up, or gave the request less than a read attempt, has said nothing about
// the node it was asking. Suspected for it, a healthy node would be asked last
// by every request this router makes for suspectFor — and for a keyed
// operation, every repository it commits sent to the runner-up — and dropped
// from the placement's next answer.
func TestACallerThatStopsWaitingSuspectsNoNode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// stop ends the caller's wait once the node has the request.
		stop func(cancel context.CancelFunc)
		// within is the caller's own deadline, zero for none.
		within time.Duration
	}{
		{name: "the caller hangs up", stop: func(cancel context.CancelFunc) { cancel() }},
		{name: "the caller's deadline is shorter than an attempt", stop: func(context.CancelFunc) {},
			within: 300 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t, "data-a", "data-b")
			hang := make(chan struct{})
			t.Cleanup(func() { close(hang) })
			firstName, first := f.first(t, f.client)
			first.set(func(n *fakeNode) { n.hang = hang })

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.within > 0 {
				ctx, cancel = context.WithTimeout(ctx, tc.within)
				defer cancel()
			}
			done := make(chan error, 1)
			go func() {
				_, err := f.client.Work().Tasks(ctx, tracker.Query{}, time.Now())
				done <- err
			}()
			if err := whileHeld(t, first, done, func() { tc.stop(cancel) }); err == nil {
				t.Fatal("a read whose caller stopped waiting answered, want the caller's own ending")
			}
			f.placement.mu.Lock()
			told := slices.Clone(f.placement.unanswered)
			f.placement.mu.Unlock()
			if len(told) != 0 {
				t.Errorf("the placement was told %v went unanswered — the caller stopped "+
					"waiting, the node did not stop answering", told)
			}
			if next, _ := f.first(t, f.client); next != firstName {
				t.Errorf("%s is asked after %s now: a node its caller stopped waiting for was "+
					"suspected", firstName, next)
			}
		})
	}
}

// whileHeld runs stop once node holds the request its caller sent — a node
// that hangs ([fakeNode.hang]) — and answers what the caller's wait ended
// with. A caller whose wait ended before the node held its request is the
// case's premise failing, never a pass.
func whileHeld(t *testing.T, node *fakeNode, done <-chan error, stop func()) error {
	t.Helper()
	for !node.askedFor("hung") {
		select {
		case err := <-done:
			t.Fatalf("the premise: the caller's wait ended (%v) before %s held its request",
				err, node.name)
		case <-time.After(time.Millisecond):
		}
	}
	stop()
	return <-done
}

// A WRITE'S SILENCE THROUGH A WHOLE WRITE ATTEMPT IS EVIDENCE, under a caller's
// own longer deadline too: such a write waits the caller's deadline rather than
// the router's attempt (budgetFor), and a node silent for a whole attempt of
// the router's own is one it judges gone rather than slow, whoever stopped
// waiting first — and one silent for less is not. The router's clock is moved
// on while the node holds the request, so the case waits no write attempt out.
func TestAWritesSilenceThroughAWholeAttemptIsEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		silent  time.Duration
		suspect bool
	}{
		{name: "a whole write attempt", silent: Production().WriteAttempt, suspect: true},
		{name: "less than a write attempt", silent: Production().WriteAttempt - time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t, "data-a", "data-b")
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			opts := RouterOptions{Queue: f.start(t), Self: "agent-2", Placement: f.placement, Git: f.git,
				Session: NewSession(), Timing: Production(), Envelopes: fleetEnvelopes,
				Now: func() time.Time { return time.Unix(0, clock.Load()) }}
			r, err := NewRouter(opts)
			if err != nil {
				t.Fatal(err)
			}
			hang := make(chan struct{})
			t.Cleanup(func() { close(hang) })
			firstName, first := f.first(t, r)
			first.set(func(n *fakeNode) { n.hang = hang })

			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := r.WriterAs(swe).CreateTask(ctx, "op-silent", tracker.Task{Project: "ENG"}, nil)
				done <- err
			}()
			if err := whileHeld(t, first, done, func() { clock.Add(int64(tc.silent)) }); err == nil {
				t.Fatal("a write whose caller's deadline passed answered, want its ending")
			}
			f.placement.mu.Lock()
			told := slices.Contains(f.placement.unanswered, firstName)
			f.placement.mu.Unlock()
			if told != tc.suspect {
				t.Errorf("silent for %v of the router's %v write attempt, %s was reported "+
					"unanswered: %v, want %v", tc.silent, Production().WriteAttempt, firstName,
					told, tc.suspect)
			}
		})
	}
}

// A ROUTER REFUSES A TIMING WITH ANY FIELD UNSET, BY THE FIELD IT LACKS, and
// takes Production whole.
//
// Read as a setting, a zero attempt is no bound of the router's own — a
// request from a caller with no deadline waits on a wedged node for ever —
// and a zero admission budget, like any negative field, is a deadline already
// past, which withholds every seat's claim: neither is a setting anybody
// chose. Each field is taken away from production's timing in turn, as zero
// and as negative.
func TestARouterRefusesAZeroTiming(t *testing.T) {
	t.Parallel()
	build := func(timing Timing) error {
		_, err := NewRouter(RouterOptions{Self: "agent-1", Queue: &recordingAsker{},
			Placement: &fakePlacement{}, Git: &fakePlacement{},
			Session: NewSession(), Timing: timing, Envelopes: fleetEnvelopes})
		return err
	}
	if err := build(Production()); err != nil {
		t.Fatalf("a router refused the timing every node runs: %v", err)
	}
	unset := map[string]func(*Timing, time.Duration){
		"ReadAttempt":    func(tm *Timing, d time.Duration) { tm.ReadAttempt = d },
		"WriteAttempt":   func(tm *Timing, d time.Duration) { tm.WriteAttempt = d },
		"AdmissionAsk":   func(tm *Timing, d time.Duration) { tm.AdmissionAsk = d },
		"GitReadAttempt": func(tm *Timing, d time.Duration) { tm.GitReadAttempt = d },
	}
	// EVERY FIELD, so a field added to Timing and to nothing here fails
	// rather than going unchecked.
	if fields := reflect.TypeFor[Timing]().NumField(); len(unset) != fields {
		t.Fatalf("Timing has %d fields and this case unsets %d", fields, len(unset))
	}
	for field, set := range unset {
		for _, d := range []time.Duration{0, -time.Second} {
			timing := Production()
			set(&timing, d)
			if err := build(timing); err == nil || !strings.Contains(err.Error(), "Timing."+field) {
				t.Errorf("a router built with Timing.%s = %v answered %v, want a refusal "+
					"naming Timing.%s", field, d, err, field)
			}
		}
	}
	if err := build(Timing{}); err == nil || !strings.Contains(err.Error(), "Timing.") {
		t.Errorf("a router built with no timing answered %v, want a refusal naming a field", err)
	}
}

// A READ ATTEMPT IS LONGER THAN THE REPLY'S MARGIN, or no router is built with
// it — and at the shortest one taken, a node behind its asker's floor is HEARD
// saying so. A serving node ends every wait a reply's margin short of its
// asker's deadline (statelog.WithReplyMargin), so an attempt no longer than the
// margin leaves it no time for anything, and any attempt past it is one a
// lagging node answers inside: here the node has no time left to wait for the
// floor at all and says it is behind at once. Waiting out its floor budget
// instead, it answered after its asker had stopped listening, which asked an
// HONEST node last for suspectFor and told the placement it was gone. The
// bound is spelled from code.ReplyMargin itself, never from the router's own
// constant, so a floor that drifted from the margin fails.
func TestAReadAttemptLongerThanTheReplysMarginIsAnsweredInside(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	timing := Production()
	timing.ReadAttempt = code.ReplyMargin
	if _, err := NewRouter(RouterOptions{Self: "agent-2", Queue: f.start(t),
		Placement: f.placement, Git: f.git, Session: NewSession(), Timing: timing,
		Envelopes: fleetEnvelopes}); err == nil ||
		!strings.Contains(err.Error(), "Timing.ReadAttempt") {
		t.Fatalf("a router whose read attempt is the reply's margin (%v) answered %v, want a "+
			"refusal naming Timing.ReadAttempt", timing.ReadAttempt, err)
	}
	timing.ReadAttempt = code.ReplyMargin + time.Nanosecond
	r := f.routerTimed(t, "agent-2", nil, timing)

	r.Observe(statelog.Position{Stream: trackerStream, Generation: 1, Seq: 5})
	f.nodes["data-a"].set(func(n *fakeNode) { n.behind = true })
	_, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "data-a: data-a has not applied") {
		t.Fatalf("a read of a node behind its floor, at the shortest read attempt a router "+
			"takes (%v), answered %v — want the node heard saying it is behind", timing.ReadAttempt, err)
	}
	f.placement.mu.Lock()
	told := slices.Clone(f.placement.unanswered)
	f.placement.mu.Unlock()
	if slices.Contains(told, "data-a") {
		t.Errorf("a node that answered it was behind was reported unanswered (%v): its asker "+
			"had stopped listening when it said so", told)
	}
}

// A NODE THAT HAS ITS ASKER'S FLOOR AND LAGS ITS READ'S BARRIER IS HEARD SAYING
// SO, at an attempt a fraction of what a linearizable read's waits take at
// their whole budgets. The node reaches the floor at once; its read, through a
// real statelog reader, appends a barrier that commits at once and never
// reaches the barrier's position — a node that lags the log's end. The
// reader's wait ends a reply's margin short of the attempt
// (statelog.WithReplyMargin), so the read is refused `behind` while the router
// still listens, and the node is not reported unanswered. Waited out at its own
// budget (statelog.ReadBudget) — on a seat's read, every tool's — that refusal
// reached nobody, and an honest node was suspected.
func TestANodeThatLagsItsReadsBarrierIsHeardSayingSo(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	timing := Production()
	timing.ReadAttempt = minReadAttempt + time.Second
	if timing.ReadAttempt >= statelog.ReadBudget+code.ReplyMargin {
		t.Fatalf("the premise: a %v attempt must end before the read's own wait would",
			timing.ReadAttempt)
	}
	r := f.routerTimed(t, "agent-2", nil, timing)
	r.Observe(statelog.Position{Stream: trackerStream, Generation: 1, Seq: 5})
	f.nodes["data-a"].set(func(n *fakeNode) { n.levelRead = laggingLinearizableRead(t) })

	begun := time.Now()
	_, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	waited := time.Since(begun)
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseBehind {
		t.Fatalf("a read of a node lagging its barrier, at a %v attempt, answered %v — want "+
			"the node heard refusing it %q", timing.ReadAttempt, err, statelog.RefuseBehind)
	}
	if waited >= timing.ReadAttempt {
		t.Errorf("the refusal took %v, the router's whole %v attempt", waited, timing.ReadAttempt)
	}
	f.placement.mu.Lock()
	told := slices.Clone(f.placement.unanswered)
	f.placement.mu.Unlock()
	if slices.Contains(told, "data-a") {
		t.Errorf("a node that answered it was behind its barrier was reported unanswered (%v)", told)
	}
}

// PRODUCTION'S READ ATTEMPT HOLDS A LINEARIZABLE READ'S WAITS at their whole
// budgets — the asker's floor, the barrier, the barrier's position — and the
// reply's margin, so production cuts a healthy node's waits short only where a
// barrier first waits out one already in flight. Raising any of those budgets
// without the attempt fails here rather than in a fleet.
func TestProductionsReadAttemptHoldsALinearizableReadsWaits(t *testing.T) {
	t.Parallel()
	waits := 2*statelog.ReadBudget + statelog.DefaultBarrierBudget + code.ReplyMargin
	if read := Production().ReadAttempt; read < waits {
		t.Errorf("production's read attempt is %v, under the %v a linearizable read's waits "+
			"take at their whole budgets with the reply's margin", read, waits)
	}
}

// A WEDGED NODE COSTS A READ THE ROUTER'S OWN READ ATTEMPT, never
// production's: the timing a router is built with is the one it waits, which
// is what lets a suite in another package — where no unexported budget can be
// reached — drive the production router over a fleet whose node hangs without
// waiting out a whole production attempt per case. The first node this router
// asks takes the read and never answers; the next answers it, a whole attempt
// of this router's later and well inside production's.
//
// AND THE WEDGED NODE IS SUSPECTED, because the wait that ran out on it was the
// router's own. The router's clock is held still, so what marks it is the
// attempt's own end and never how long the read took by that clock.
func TestAWedgedNodeCostsAReadTheRoutersOwnReadAttempt(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	timing := Production()
	timing.ReadAttempt = minReadAttempt + time.Second
	still := time.Now()
	r, err := NewRouter(RouterOptions{Queue: f.start(t), Self: "agent-2", Placement: f.placement,
		Git: f.git, Session: NewSession(), Timing: timing, Envelopes: fleetEnvelopes,
		Now: func() time.Time { return still }})
	if err != nil {
		t.Fatal(err)
	}
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	firstName, first := f.first(t, r)
	first.set(func(n *fakeNode) { n.hang = hang })

	begun := time.Now()
	if _, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("a read with a live second node failed: %v", err)
	}
	waited := time.Since(begun)
	if !first.askedFor("hung") || waited < timing.ReadAttempt {
		t.Fatalf("the premise: %s, the node asked first, was asked (%v) and held the read "+
			"for %s, want it to have held its router's whole %s attempt", firstName,
			first.askedFor("hung"), waited, timing.ReadAttempt)
	}
	// HALFWAY TO PRODUCTION'S, so a loaded machine's slack is on both sides.
	if bound := (timing.ReadAttempt + Production().ReadAttempt) / 2; waited >= bound {
		t.Fatalf("a read waited %s on wedged %s, want about its router's %s read attempt "+
			"and under %s — the router waited a timing it was not built with",
			waited, firstName, timing.ReadAttempt, bound)
	}
	// SUSPECTED, which a caller's own ending never makes it
	// (TestACallerThatStopsWaitingSuspectsNoNode).
	f.placement.mu.Lock()
	told := slices.Contains(f.placement.unanswered, firstName)
	f.placement.mu.Unlock()
	if !told {
		t.Errorf("%s held a read through its router's whole attempt and was not reported "+
			"unanswered", firstName)
	}
}

// GITPROC'S READ BOUNDS ARE ONE READ ATTEMPT — production's, the one every
// node's router gives a read on one data node: a local git read
// (gitproc.ReadDeadline) is bounded like any read in the engine, and a run
// that has waited a whole read attempt for a slot (gitproc.SlotWait) is on a
// node a balancer should send the retry elsewhere from. gitproc imports no
// engine package, so the equality its deadlines claim is held here, where both
// sides are importable.
func TestGitprocsReadBoundsAreOneReadAttempt(t *testing.T) {
	t.Parallel()
	read := Production().ReadAttempt
	if gitproc.ReadDeadline != read {
		t.Errorf("gitproc.ReadDeadline is %v, and a node's router gives a read %v — a local git "+
			"read is one read attempt", gitproc.ReadDeadline, read)
	}
	if gitproc.SlotWait != read {
		t.Errorf("gitproc.SlotWait is %v, and a node's router gives a read %v — a run waits "+
			"one read attempt for a slot", gitproc.SlotWait, read)
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
// it — on the log of the operation's own domain, and on no other: a floor on
// another domain's log, or on a stream no domain logs to, is never waited for.
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
	// ANOTHER DOMAIN'S LOG: a page write does not depend on the tracker's
	// rows, so it carries no floor on its log.
	node.set(func(n *fakeNode) { n.floors = nil })
	if _, _, err := f.client.Pages().Comment(t.Context(), pages.Actor{}, "p", pages.NewComment{}); err != nil {
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
	if _, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	for _, at := range node.floors {
		if at.Stream != trackerStream {
			t.Errorf("a tracker read waited on %v, a log it does not read", at)
		}
	}
}

// A FLOOR ON ANOTHER DOMAIN'S LOG NEVER HOLDS AN OPERATION, though the estate
// carries both logs: no operation reads another domain's rows at a position,
// so a seat a
// page change woke — its trigger a floor on the pages log — reads and writes
// the tracker on a node whose pages applier is stuck exactly as before it was
// woken, and only the knowledge base's own operations wait for that applier.
func TestAFloorOnAnotherDomainsLogNeverHoldsAnOperation(t *testing.T) {
	t.Parallel()
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
	// AND THE SERVER HOLDS TO IT TOO, whatever an asker sends: a tracker
	// read carrying a floor on the pages log does not wait for it.
	rep := askSigned(t, f.start(t), "data-a", request{Op: opTasks.spec.name,
		Floors: []statelog.Position{{Stream: pagesStream, Generation: 1, Seq: 9}}})
	if rep.Unserved != "" || rep.Err != nil {
		t.Fatalf("a tracker read with a pages floor = %+v, want it answered", rep)
	}

	// The holder's floor wait ends a reply's margin inside its asker's
	// attempt (statelog.WithReplyMargin), so it says it is behind while the
	// asker is still listening.
	_, _, err := f.client.Pages().Comment(t.Context(), pages.Actor{}, "p", pages.NewComment{})
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
	_, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
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
// query tells apart on purpose, and is scoped by the serving node's chart. And
// a turn's prefetch arrives as one: the serving node coordinates the scan and
// times it, against the turn start's budget only if it is told whose it is.
//
// Mutation: drop Prefetch from knowledgeArgs or its query, and the prefetch
// arrives as a deliberate search.
func TestAKnowledgeSearchKeepsItsExclusionAndTakesTheServersScope(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	searcher := f.client.Knowledge()
	searcher.Search(t.Context(), knowledge.Query{Text: "deploys", Seat: seat,
		Org: &org.Organization{}, ExcludeAncestors: []string{}, Prefetch: true})
	searcher.Search(t.Context(), knowledge.Query{Text: "deploys"})
	got := f.nodes["data-a"].queries
	if len(got) != 2 {
		t.Fatalf("searches = %d", len(got))
	}
	if !got[0].Prefetch || got[1].Prefetch {
		t.Errorf("a prefetch arrived as Prefetch=%t and a deliberate search as Prefetch=%t", got[0].Prefetch,
			got[1].Prefetch)
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
	_, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) {
		t.Fatalf("err = %v, want ErrUnserved", err)
	}
}

// AN UNSERVED WALK SAYS WHETHER A NODE WENT SILENT. One in which every node
// answered that it would not run the operation is one in which NOTHING RAN it,
// and a caller with a write may say so definitively; one in which a node said
// nothing may have had it run there — a committer that appended and lost its
// reply — and the code host then tells the person the outcome is unknown,
// never that nothing took the push.
func TestAnUnservedWalkSaysWhetherANodeWentSilent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		silent bool
	}{{name: "every node refused"}, {name: "a node went silent", silent: true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t, "data-a", "data-b")
			f.nodes["data-a"].set(func(n *fakeNode) { n.outOfService = true })
			f.nodes["data-b"].set(func(n *fakeNode) { n.outOfService, n.silent = !tc.silent, tc.silent })
			_, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
			var unserved *ErrUnserved
			if !errors.As(err, &unserved) {
				t.Fatalf("err = %v, want ErrUnserved", err)
			}
			if unserved.Silent() != tc.silent {
				t.Fatalf("Silent() = %v for a walk where %s (%v), want %v", unserved.Silent(),
					tc.name, err, tc.silent)
			}
		})
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

// A DATA NODE ANSWERS ITS OWN SEATS IN-PROCESS, and asks nobody: it holds the
// whole estate, and a request over the broker to itself would be a round trip
// for nothing.
func TestADataNodeAnswersItself(t *testing.T) {
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
// and the router asks the next data node, which has.
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

	// THIS NODE'S OWN COPY, where it is the only data node: its seats are
	// answered, not refused as though nobody held the estate.
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

// A NODE BEHIND THE COMPANY'S CONFIGURATION IS ASKED LAST, NEVER REFUSED.
//
// A data node the control plane has confirmed behind the epoch the fleet
// activated — shed or stuck, never merely mid-rollout — gives up its seats, its
// schedules and its inbound work, and goes on serving the estate. What it
// supplies to an operation from its own epoch is the configuration the fleet
// replaced: the chart a tracker write resolves a people field by, the leads a
// wake falls back to, the unit a read renders. So it is a worse node to ask,
// exactly as a copy that lags its log is: every other data node is asked first,
// and this one is asked again, told to answer anyway, when none of them runs
// the operation — so a fleet whose only data node failed an apply keeps its
// tracker rather than losing it to a refusal. Asked first, a shed node decided
// a create's people field against the chart the fleet had replaced while a
// current peer could have decided it.
func TestANodeBehindTheConfigurationIsAskedLast(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	assigned := tracker.Task{Project: "ENG",
		Fields: map[string]json.RawMessage{"owner": json.RawMessage(`"swe"`)}}
	decided := func(n *fakeNode) bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return slices.ContainsFunc(n.created, func(task tracker.Task) bool {
			return string(task.Fields["owner"]) == `"swe"`
		})
	}

	// ASKED FIRST BY ANOTHER NODE, the node behind runs nothing and the
	// current one decides — a write and a read alike. Each through a
	// router that has asked nobody yet, whose first node is the one behind:
	// a router that had would stay on whichever node answered it
	// ([Router.markAnswered]), and the preference would never be what
	// moved it off the node behind.
	for _, tc := range []struct {
		name, op string
		run      func(*Router) error
		ran      func(*fakeNode) bool
	}{{
		name: "a write", op: "create",
		run: func(r *Router) error {
			_, err := r.WriterAs(swe).CreateTask(ctx, "op-first", assigned, nil)
			return err
		},
		ran: decided,
	}, {
		name: "a read", op: "tasks",
		run: func(r *Router) error {
			_, err := r.Work().Tasks(ctx, tracker.Query{}, time.Now())
			return err
		},
		ran: func(n *fakeNode) bool { return n.askedFor("tasks") },
	}} {
		f := newFleet(t, "data-a", "data-b")
		_, behind := f.first(t, f.client)
		current := f.other(behind)
		behind.set(func(n *fakeNode) { n.behindConfig = true })
		if err := tc.run(f.client); err != nil {
			t.Fatalf("%s with one data node behind the configuration: %v", tc.name, err)
		}
		if behind.askedFor(tc.op) || !tc.ran(current) {
			t.Fatalf("%s: the node behind, asked first, ran %v and the current one ran it %v: "+
				"want it run by the current node, and nothing run behind", tc.name,
				behind.asked, tc.ran(current))
		}
	}

	// AND IT SAYS WHY, naming the configuration rather than its log, which
	// is level — in a reason of its own, which the asker remembers: an
	// operator reading a walk's account must be sent to the control plane,
	// not to the applier.
	f := newFleet(t, "data-a", "data-b")
	_, behind := f.first(t, f.client)
	behind.set(func(n *fakeNode) { n.behindConfig = true })
	direct := f.start(t)
	ask := func() reply {
		t.Helper()
		return askSigned(t, direct, behind.name,
			request{Op: opTasks.spec.name, Args: json.RawMessage(`{"Query":{}}`)})
	}
	if rep := ask(); rep.Unserved != unservedBehindConfig ||
		!strings.Contains(rep.Detail, "configuration") || strings.Contains(rep.Detail, "lags its log") {
		t.Fatalf("the node behind answered %q: %q, want %q, naming the configuration and not "+
			"its log", rep.Unserved, rep.Detail, unservedBehindConfig)
	}
	// A NODE THAT IS BOTH answers the reason that lasts, so its asker
	// remembers it, and names both.
	behind.set(func(n *fakeNode) { n.notReady = true })
	if rep := ask(); rep.Unserved != unservedBehindConfig ||
		!strings.Contains(rep.Detail, "configuration") || !strings.Contains(rep.Detail, "lags its log") {
		t.Fatalf("the node behind and lagging answered %q: %q, want %q naming both",
			rep.Unserved, rep.Detail, unservedBehindConfig)
	}
	behind.set(func(n *fakeNode) { n.notReady = false })

	// WITH THE CURRENT NODE GONE, THE NODE BEHIND DECIDES IT: never refused,
	// or a fleet whose only data node failed an apply would answer none of
	// its tracker.
	f.placement.set(func(p *fakePlacement) { p.nodes = []string{behind.name} })
	if _, err := f.client.WriterAs(swe).CreateTask(ctx, "op-alone", assigned, nil); err != nil {
		t.Fatalf("the only data node, behind the configuration, refused a create: %v", err)
	}
	if !decided(behind) {
		t.Fatal("the only data node, behind the configuration, did not decide the create")
	}

	// THIS NODE'S OWN ROUTER asks a current peer before its own copy, and
	// its own copy when it is the only data node — in-process, as a remote
	// asker is answered.
	peered := newFleet(t, "data-b")
	own := &fakeNode{name: "data-a", units: chartOf("a"), behindConfig: true}
	if _, err := peered.router(t, "data-a", own).WriterAs(swe).CreateTask(ctx, "op-own",
		assigned, nil); err != nil {
		t.Fatalf("a create through a node behind the configuration: %v", err)
	}
	if own.askedFor("create") || !decided(peered.nodes["data-b"]) {
		t.Fatal("this node's own copy, behind the configuration, decided the create ahead " +
			"of a current peer")
	}
	alone := &fakeNode{name: "data-a", units: chartOf("a"), behindConfig: true}
	if _, err := newFleet(t).router(t, "data-a", alone).WriterAs(swe).CreateTask(ctx,
		"op-own-alone", assigned, nil); err != nil {
		t.Fatalf("this node's own copy, behind the configuration and the only one, refused "+
			"its own seat's create: %v", err)
	}
	if !decided(alone) {
		t.Fatal("this node's own copy, the only one, did not decide its own seat's create")
	}

	// ADMISSION'S PING IS NOT HELD BACK: it asks the copy's own seat
	// admission and routes no later operation, so a node behind the
	// configuration is asked it in turn like any other.
	pinged := newFleet(t, "data-a", "data-b")
	_, first := pinged.first(t, pinged.client)
	second := pinged.other(first)
	first.set(func(n *fakeNode) { n.behindConfig = true })
	second.set(func(n *fakeNode) { n.notAdmitting = true })
	if served, _, err := pinged.client.Serves(ctx); err != nil || !served {
		t.Fatalf("admission through a node behind the configuration = (%v, %v), want "+
			"admitted", served, err)
	}
	if second.askedFor("admits") {
		t.Fatal("admission's ping passed over a node behind the configuration")
	}
}

// A NODE BEHIND THE COMPANY'S CONFIGURATION IS REMEMBERED, SO A REQUEST ASKS
// IT ONCE RATHER THAN TWICE.
//
// Behind the configuration is no passing state, as a copy's lag is: a node
// stays there until its apply succeeds — a stuck one until a person acts — and
// says so to every request. Forgotten between requests, it cost every
// operation a refusal's round trip before the walk moved on, for as long as it
// stayed: on a stateless node beside one stuck data node every estate call was
// two requests and the data node served twice the traffic; with every data node
// behind, every call asked each of them twice; and a repository whose committer
// was behind sent it every push's whole proposal — encoded, decoded and refused
// — before the runner-up was asked. So the router remembers the answer for
// [suspectFor] and asks a remembered node only as the last resort, told at once
// to answer anyway: one ask a request once the router has heard the refusal,
// and none at all where a current node answers first. Nothing but the window
// ends the memory — the last resort's answer, given because it was told to
// answer anyway, says nothing about the node's configuration — and admission's
// ping, which the preference never holds back, is not held back by the memory
// either.
func TestANodeBehindTheConfigurationIsRemembered(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	behindConfig := func(n *fakeNode) { n.behindConfig = true }
	read := func(t *testing.T, r *Router) {
		t.Helper()
		if _, err := r.Work().Tasks(ctx, tracker.Query{}, time.Now()); err != nil {
			t.Fatalf("a read with the data nodes behind the configuration: %v", err)
		}
	}
	asked := func(nodes ...*fakeNode) (requests, anyway int) {
		for _, n := range nodes {
			n.mu.Lock()
			requests, anyway = requests+n.requests, anyway+n.anyway
			n.mu.Unlock()
		}
		return requests, anyway
	}
	const later = 4

	for _, tc := range []struct {
		name  string
		nodes []string
		// first is what the first read costs: a refusal from each data
		// node, all of them behind, and the last resort's answer.
		first int
	}{
		{name: "one data node", nodes: []string{"data-a"}, first: 2},
		{name: "every data node", nodes: []string{"data-a", "data-b"}, first: 3},
	} {
		f := newFleet(t, tc.nodes...)
		nodes := slices.Collect(maps.Values(f.nodes))
		for _, n := range nodes {
			n.set(behindConfig)
		}
		read(t, f.client)
		if requests, _ := asked(nodes...); requests != tc.first {
			t.Fatalf("%s behind: the first read asked %d times, want %d — each node's "+
				"refusal, then the last resort", tc.name, requests, tc.first)
		}
		for range later {
			read(t, f.client)
		}
		if requests, anyway := asked(nodes...); requests != tc.first+later || anyway != 1+later {
			t.Fatalf("%s behind: %d more reads asked %d times, %d of them told to answer "+
				"anyway — want one ask a read, the last resort's, once the router has "+
				"heard the refusal", tc.name, later, requests-tc.first, anyway-1)
		}
	}

	// A KEYED WRITE asks the runner-up instead of a committer behind: only
	// the first push is proposed to it.
	holders := []string{"data-a", "data-b", "data-c"}
	f := newFleet(t, holders...)
	const repo = "repo-behind"
	o := rendezvous.Order(repo, holders)
	f.nodes[o[0]].set(behindConfig)
	for i := range 1 + later {
		got, err := f.client.Code().Propose(ctx, proposalTo(repo, fmt.Sprintf("push-%d", i)))
		if err != nil || got.OpID != o[1] {
			t.Fatalf("push %d with the committer %s behind = (%+v, %v), want the runner-up "+
				"%s's answer", i, o[0], got, err, o[1])
		}
	}
	if requests, _ := asked(f.nodes[o[0]]); requests != 1 {
		t.Fatalf("the committer behind the configuration was sent %d of %d pushes, want only "+
			"the first", requests, 1+later)
	}

	// THE WINDOW ENDS IT: past it the node is asked as any other again —
	// refusing again while it is still behind, and answering at once once it
	// is current.
	alone := newFleet(t, "data-a")
	n := alone.nodes["data-a"]
	n.set(behindConfig)
	read(t, alone.client)
	alone.client.now = func() time.Time { return time.Now().Add(2 * suspectFor) }
	before, beforeAnyway := asked(n)
	read(t, alone.client)
	if requests, anyway := asked(n); requests-before != 2 || anyway-beforeAnyway != 1 {
		t.Fatalf("past the window, a read of a node still behind asked %d times, %d told to "+
			"answer anyway, want it refused once and answered by the last resort",
			requests-before, anyway-beforeAnyway)
	}
	n.set(func(n *fakeNode) { n.behindConfig = false })
	alone.client.now = func() time.Time { return time.Now().Add(4 * suspectFor) }
	before, beforeAnyway = asked(n)
	read(t, alone.client)
	if requests, anyway := asked(n); requests-before != 1 || anyway != beforeAnyway {
		t.Fatalf("past the window, a read of a node current again asked %d times, %d told "+
			"to answer anyway, want one ordinary ask", requests-before, anyway-beforeAnyway)
	}

	// ADMISSION'S PING IS NOT HELD BACK BY THE MEMORY EITHER: a router that
	// has heard the node is behind still asks it the ping in its turn.
	pinged := newFleet(t, "data-a", "data-b")
	_, first := pinged.first(t, pinged.client)
	second := pinged.other(first)
	first.set(behindConfig)
	read(t, pinged.client)
	second.set(func(n *fakeNode) { n.notAdmitting, n.asked = true, nil })
	if served, _, err := pinged.client.Serves(ctx); err != nil || !served {
		t.Fatalf("admission through a node the router remembers behind = (%v, %v), want "+
			"admitted", served, err)
	}
	if second.askedFor("admits") {
		t.Fatal("admission's ping passed over a node the router remembers behind the " +
			"configuration")
	}
}

// THE LAST RESORT ASKS A COPY THAT ONLY LAGS BEFORE A NODE BEHIND THE
// CONFIGURATION, AND ITS ORDER IS THE REASONS' — never when the router learned
// them.
//
// Both are worse nodes to ask, and they are not equally worse: a copy that
// only lags answers held to the caller's floors and to the CURRENT
// configuration, while a node behind the configuration decides under the
// revision the fleet replaced. So where no node in better standing runs an
// operation, the last resort asks every copy that only lags first and every
// node behind the configuration after them, each in the order the walk reached
// it. The order is the same on the request that learns a node is behind as on
// every one the router remembers it for, and the same in every asker: when the
// last resort asked in the order the walk REACHED the refusals, the request
// that learned a node was behind asked it first wherever rendezvous ranked it
// first — the read it answered was decided under the replaced chart, and a
// repository's pushes went to its committer behind the configuration on that
// request and to the lagging runner-up on every one after, two nodes
// committing one repository and contending on its one subject in every window.
// And a router that remembered one node behind and had not yet heard another
// asked them in another order than a router that had heard neither, so two
// askers sent one repository's pushes to two committers.
func TestTheLastResortAsksALaggingCopyBeforeANodeBehindTheConfiguration(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	behindConfig := func(n *fakeNode) { n.behindConfig = true }
	lagging := func(n *fakeNode) { n.notReady = true }
	ran := func(n *fakeNode, op string) int {
		n.mu.Lock()
		defer n.mu.Unlock()
		count := 0
		for _, asked := range n.asked {
			if asked == op {
				count++
			}
		}
		return count
	}
	read := func(t *testing.T, r *Router) {
		t.Helper()
		if _, err := r.Work().Tasks(ctx, tracker.Query{}, time.Now()); err != nil {
			t.Fatalf("a read with no data node in good standing: %v", err)
		}
	}
	// pastTheWindow moves r's clock past the memory of every refusal it
	// heard, each time it is called.
	pastTheWindow := func(r *Router) func() {
		windows := 0
		return func() {
			windows++
			offset := time.Duration(windows) * 2 * suspectFor
			r.now = func() time.Time { return time.Now().Add(offset) }
		}
	}
	const requests, window = 6, 3

	// AN OPERATION ANY COPY ANSWERS EQUALLY, through a router whose first
	// node is the one behind: the read that learns it is behind and every
	// read after — across a window's end — is answered by the copy that
	// only lags.
	f := newFleet(t, "data-a", "data-b")
	_, behind := f.first(t, f.client)
	lags := f.other(behind)
	behind.set(behindConfig)
	lags.set(lagging)
	next := pastTheWindow(f.client)
	for i := range requests {
		if i == window {
			next()
		}
		read(t, f.client)
		if got, want := ran(lags, "tasks"), i+1; got != want || ran(behind, "tasks") != 0 {
			t.Fatalf("read %d: the copy that only lags ran %d reads and the node behind the "+
				"configuration %d, want %d and none", i, got, ran(behind, "tasks"), want)
		}
	}

	// THIS NODE'S OWN COPY behind the configuration is asked after a peer
	// whose copy only lags, as a remote one is — in-process saves a round
	// trip, never a decide under the replaced configuration.
	peered := newFleet(t, "data-b")
	peered.nodes["data-b"].set(lagging)
	own := &fakeNode{name: "data-a", units: chartOf("a"), behindConfig: true}
	ownRouter := peered.router(t, "data-a", own)
	for i := range window {
		read(t, ownRouter)
		if got := ran(peered.nodes["data-b"], "tasks"); got != i+1 || own.askedFor("tasks") {
			t.Fatalf("read %d: the peer whose copy only lags ran %d reads and this node's own "+
				"copy behind the configuration ran %v, want %d and none", i, got,
				own.askedFor("tasks"), i+1)
		}
	}

	// A KEYED OPERATION: a committer behind the configuration and a runner-up
	// that only lags — every push across a window's end is committed by the
	// runner-up, the push that learns the committer is behind included, so a
	// repository's pushes meet at one node.
	holders := []string{"data-a", "data-b"}
	keyed := newFleet(t, holders...)
	const repo = "repo-worse"
	o := rendezvous.Order(repo, holders)
	keyed.nodes[o[0]].set(behindConfig)
	keyed.nodes[o[1]].set(lagging)
	next = pastTheWindow(keyed.client)
	for i := range requests {
		if i == window {
			next()
		}
		got, err := keyed.client.Code().Propose(ctx, proposalTo(repo, fmt.Sprintf("push-%d", i)))
		if err != nil || got.OpID != o[1] {
			t.Fatalf("push %d with the committer %s behind the configuration and the runner-up "+
				"%s lagging = (%+v, %v), want the runner-up's answer", i, o[0], o[1], got, err)
		}
	}
	if n := len(keyed.nodes[o[0]].proposalsTo(repo)); n != 0 {
		t.Fatalf("the committer behind the configuration decided %d pushes ahead of a runner-up "+
			"that only lags", n)
	}

	// THIS NODE AS THE COMMITTER, its own copy behind the configuration: the
	// runner-up that only lags commits the push, as it would for any asker.
	alone := newFleet(t, o[1])
	alone.nodes[o[1]].set(lagging)
	ownCommitter := &fakeNode{name: o[0], units: chartOf(o[0]), behindConfig: true}
	committerRouter := alone.router(t, o[0], ownCommitter)
	for i := range window {
		got, err := committerRouter.Code().Propose(ctx, proposalTo(repo, fmt.Sprintf("own-%d", i)))
		if err != nil || got.OpID != o[1] {
			t.Fatalf("push %d through the committer %s, its own copy behind the configuration, "+
				"with the runner-up %s lagging = (%+v, %v), want the runner-up's answer", i,
				o[0], o[1], got, err)
		}
	}
	if n := len(ownCommitter.proposalsTo(repo)); n != 0 {
		t.Fatalf("this node's own copy behind the configuration decided %d pushes ahead of a "+
			"runner-up that only lags", n)
	}

	// EVERY NODE BEHIND, ONE OF THEM REMEMBERED: the last resort asks them in
	// the key's order, as a router that has heard neither does — never the
	// one it has not heard yet first because the other is remembered.
	both := newFleet(t, holders...)
	both.nodes[o[0]].set(behindConfig)
	if got, err := both.client.Code().Propose(ctx, proposalTo(repo, "learn")); err != nil ||
		got.OpID != o[1] {
		t.Fatalf("a push with only the committer %s behind = (%+v, %v), want the current "+
			"runner-up %s's answer", o[0], got, err, o[1])
	}
	both.nodes[o[1]].set(behindConfig)
	fresh := both.router(t, "agent-2", nil)
	for _, r := range []*Router{both.client, fresh} {
		for i := range window {
			got, err := r.Code().Propose(ctx, proposalTo(repo, fmt.Sprintf("both-%d", i)))
			if err != nil || got.OpID != o[0] {
				t.Fatalf("push %d with every node behind the configuration = (%+v, %v), want "+
					"the committer %s's answer from every asker", i, got, err, o[0])
			}
		}
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
		tracker.SearchQuery{Text: "deploys", Limit: 5}); err == nil {
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
	_, err := f.client.Work().Search(t.Context(), tracker.SearchQuery{Text: "deploys", Limit: 5})
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

	// A COPY OUT OF SERVICE is no node to ask at all, and the answer is the
	// placement's own reason — never "nobody serves it".
	own.set(func(n *fakeNode) { n.outOfService = true })
	_, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
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
	if trackerServed, _, err := r.Serves(t.Context()); err != nil || trackerServed {
		t.Fatalf("a copy that admits no seat was admitted (%v, %v)", trackerServed, err)
	}
}

// THE ROUTER ASKS IN THE RENDEZVOUS ORDER OF ITS OWN ID: never itself or an
// empty id, the placement's listing untouched, and — once a node has answered
// and another gone silent — the answerer first and the silent node last with
// the rendezvous order kept between them. Keyed on the ASKER's id, so a
// fleet's askers make their first ask, and every failover, across the data
// nodes rather than all to one; where an asker then stays is the sticky
// node's business.
func TestTheRouterAsksInTheRendezvousOrderOfItsOwnID(t *testing.T) {
	t.Parallel()
	r := newFleet(t).router(t, "agent-2", nil)
	held := []string{"data-a", "data-b", "", "data-c", "agent-2", "data-d"}
	listed := slices.Clone(held)
	want := rendezvous.Order("agent-2", []string{"data-a", "data-b", "data-c", "data-d"})

	if got := r.order(opTasks.spec, held); !slices.Equal(got, want) {
		t.Fatalf("agent-2 asks %q, want the rendezvous order of its own id %q", got, want)
	}
	if !slices.Equal(held, listed) {
		t.Fatalf("ordering rewrote the placement's own listing %q to %q", listed, held)
	}

	r.markAnswered(want[3], trackerDomain, "")
	r.markSuspect(want[0])
	if got, w := r.order(opTasks.spec, held), []string{want[3], want[1], want[2], want[0]}; !slices.Equal(got, w) {
		t.Fatalf("with %s answered last and %s silent, agent-2 asks %q, want %q", want[3],
			want[0], got, w)
	}

	// PER DOMAIN: the tracker's sticky node is nobody's for the knowledge
	// base, and a node whose copy of the tracker said it is out of service
	// goes after every other live node — for the tracker alone.
	r.markOutOfService(want[1], trackerDomain)
	if got, w := r.order(opTasks.spec, held), []string{want[3], want[2], want[1], want[0]}; !slices.Equal(got, w) {
		t.Fatalf("with %s's tracker copy out of service, agent-2 asks %q for the tracker, "+
			"want %q", want[1], got, w)
	}
	if got, w := r.order(opListPages.spec, held), []string{want[1], want[2], want[3], want[0]}; !slices.Equal(got, w) {
		t.Fatalf("agent-2 asks %q for the knowledge base, want %q — the tracker's sticky "+
			"node and its copy out of service moved the knowledge base's order", got, w)
	}
	// AND THE PASS LAPSES: after the window the node is back in its place.
	r.now = func() time.Time { return time.Now().Add(2 * suspectFor) }
	if got, w := r.order(opTasks.spec, held), []string{want[3], want[0], want[1], want[2]}; !slices.Equal(got, w) {
		t.Fatalf("after the window agent-2 asks %q for the tracker, want %q", got, w)
	}
}

// A NODE OUT OF SERVICE FOR ONE DOMAIN IS NO REASON TO CHASE ANOTHER — ADR-0049.
//
// A router on a node without data, between a peer whose tracker copy is out
// of service and one whose knowledge base is, alternates tracker and
// knowledge-base calls. Each domain settles on a node whose copy of IT serves,
// so after each domain's first call every call reaches a serving node on its
// first ask. With one sticky node for every domain, the two calls made each
// other's server sticky in turn, and every call paid a round trip that ran
// nothing.
func TestEachDomainSettlesOnANodeWhoseCopyOfItServes(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b", "data-c")
	r := f.router(t, "agent-1", nil)
	o := rendezvous.Order("agent-1", []string{"data-a", "data-b", "data-c"})
	f.nodes[o[0]].set(func(n *fakeNode) { n.outOfServiceOn = map[string]bool{trackerDomain: true} })
	f.nodes[o[1]].set(func(n *fakeNode) { n.outOfServiceOn = map[string]bool{pagesDomain: true} })
	requests := func() int {
		total := 0
		for _, n := range f.nodes {
			n.mu.Lock()
			total += n.requests
			n.mu.Unlock()
		}
		return total
	}
	work := func() {
		t.Helper()
		if _, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
			t.Fatalf("tasks: %v", err)
		}
	}
	wiki := func() {
		t.Helper()
		if found := r.Knowledge().Search(t.Context(), knowledge.Query{Text: "runbook"}); len(found.Hits) != 1 {
			t.Fatalf("search = %+v", found)
		}
	}
	work()
	wiki()
	for i := range 6 {
		before := requests()
		if i%2 == 0 {
			work()
		} else {
			wiki()
		}
		if asked := requests() - before; asked != 1 {
			t.Fatalf("call %d asked %d nodes — it should reach a node whose copy of its "+
				"domain serves on its first ask", i, asked)
		}
	}

	// AND ONCE A DOMAIN'S STICKY NODE IS GONE, the node that said its copy is
	// out of service is asked after the rest rather than first again: the
	// tracker call that finds its sticky node silent goes on to the one node
	// left whose tracker copy serves, past the rendezvous winner whose copy
	// does not.
	f.nodes[o[1]].set(func(n *fakeNode) { n.silent = true })
	winner := f.nodes[o[0]]
	winner.mu.Lock()
	asked := winner.requests
	winner.mu.Unlock()
	work()
	winner.mu.Lock()
	asked = winner.requests - asked
	winner.mu.Unlock()
	if asked != 0 {
		t.Fatalf("the tracker call asked %s, whose tracker copy said it is out of service, "+
			"%d times once its sticky node went silent", o[0], asked)
	}
	if !f.nodes[o[2]].askedFor("tasks") {
		t.Fatal("the one node left whose tracker copy serves was never asked")
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
		_, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
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
	written, err := r.WriterAs(swe).CreateTask(t.Context(), "op-9", tracker.Task{Project: "ENG"}, nil)
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

// A FAULT IN ONE DOMAIN'S COPY ROUTES ONLY THAT DOMAIN'S OPERATIONS — ADR-0049.
//
// A node whose tracker copy is wrong sends its seats' tracker calls to a peer
// and still answers their knowledge-base calls from its own pages, and a peer
// asking it is told `out_of_service` for the tracker's operations alone,
// naming the domain. When one copy took every domain out of service, a
// knowledge-base applier halted on one page sent every tracker call the
// node's seats made to another node — and on a single node refused them all.
func TestAFaultInOneDomainRoutesOnlyThatDomainsOperations(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	local := &fakeNode{name: "data-self", units: chartOf("self"),
		outOfServiceOn: map[string]bool{trackerDomain: true}}
	r := f.router(t, "data-self", local)

	written, err := r.WriterAs(swe).CreateTask(t.Context(), "op-t", tracker.Task{Project: "ENG"}, nil)
	if err != nil || written.Key != "ENG-1" {
		t.Fatalf("create = (%+v, %v)", written, err)
	}
	if local.askedFor("create") {
		t.Fatal("a tracker copy out of service ran a tracker write")
	}
	ran := 0
	for _, n := range f.nodes {
		ran += len(n.ops())
	}
	if ran != 1 {
		t.Fatalf("%d peers ran the write, want exactly one", ran)
	}

	// THE KNOWLEDGE BASE'S OPERATIONS ARE ANSWERED HERE: its copy is sound.
	found := r.Knowledge().Search(t.Context(), knowledge.Query{Text: "runbook"})
	if len(found.Hits) != 1 || found.Hits[0].Title != "found on data-self" {
		t.Fatalf("a knowledge search on a node whose knowledge base serves = %+v, "+
			"want this node's own answer", found)
	}
	for _, n := range f.nodes {
		if n.askedFor("search") {
			t.Fatalf("%s was asked a knowledge search this node's own copy serves", n.name)
		}
	}

	// AND A PEER ASKING IS TOLD SO, PER DOMAIN.
	peer := f.nodes["data-a"]
	peer.set(func(n *fakeNode) { n.outOfServiceOn = map[string]bool{trackerDomain: true} })
	q := f.start(t)
	ask := func(op string) reply {
		t.Helper()
		return askSigned(t, q, peer.name, request{Op: op})
	}
	if rep := ask(opTasks.spec.name); rep.Unserved != unservedOutOfService ||
		rep.Result != nil || rep.Detail != outOfService(peer.name, trackerDomain) {
		t.Fatalf("a tracker read of a node whose tracker copy is wrong answered %+v, "+
			"want %q naming the tracker", rep, unservedOutOfService)
	}
	if rep := ask(opKnowledgeSearch.spec.name); rep.Unserved != "" || rep.Err != nil ||
		rep.Result == nil {
		t.Fatalf("a knowledge search of a node whose knowledge base is sound answered %+v, "+
			"want it run", rep)
	}

	// AND ON A SINGLE NODE, WHAT AN UNSERVED ESTATE SAYS NAMES THE COPY.
	solo := newFleet(t)
	alone := solo.router(t, "data-self", &fakeNode{name: "data-self", units: chartOf("self"),
		outOfServiceOn: map[string]bool{trackerDomain: true}})
	_, err = alone.Work().Tasks(t.Context(), tracker.Query{}, time.Now())
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) ||
		!strings.Contains(unserved.Detail, "data-self: "+outOfService("data-self", trackerDomain)) {
		t.Fatalf("a single node whose tracker copy is wrong answered %v — want an "+
			"unserved estate that names this node's own copy", err)
	}
}

// A PAGE'S BACKLINKS RUN ONLY WHERE BOTH COPIES THEY READ SERVE — ADR-0049.
// The links are derived from the pages' AND the tasks' bodies and named from
// both domains' rows, so a node whose tracker copy is out of service runs none
// of them though its knowledge base serves: its own router asks a peer, and a
// peer asking it is told `out_of_service` naming the tracker. Read straight
// off the node's own index, they merged a peer's page with links derived from
// rows the node had stopped serving. And an index still building says so
// across the wire, which is what the page answer's `building` status reads.
func TestAPagesBacklinksRunOnlyWhereBothCopiesServe(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	local := &fakeNode{name: "data-self", units: chartOf("self"),
		outOfServiceOn: map[string]bool{trackerDomain: true}}
	r := f.router(t, "data-self", local)

	links, err := r.Pages().LinkedFrom(t.Context(), "p1")
	if err != nil || len(links.Pages) != 1 || links.Pages[0].Title == "linked on data-self" {
		t.Fatalf("backlinks on a node whose tracker copy is wrong = (%+v, %v), want a peer's", links, err)
	}
	if local.askedFor("linked_from") {
		t.Fatal("a node whose tracker copy is out of service read its own backlinks")
	}
	found := r.Knowledge().Search(t.Context(), knowledge.Query{Text: "runbook"})
	if len(found.Hits) != 1 || found.Hits[0].Title != "found on data-self" {
		t.Fatalf("the knowledge base's own search left this node: %+v", found)
	}

	peer := f.nodes["data-a"]
	peer.set(func(n *fakeNode) { n.outOfServiceOn = map[string]bool{trackerDomain: true} })
	rep := askSigned(t, f.start(t), peer.name, request{Op: opLinkedFrom.spec.name})
	if rep.Unserved != unservedOutOfService || rep.Detail != outOfService(peer.name, trackerDomain) {
		t.Fatalf("a peer whose tracker copy is wrong answered backlinks %+v, want %q naming "+
			"the tracker", rep, unservedOutOfService)
	}

	other := f.nodes["data-b"]
	other.set(func(n *fakeNode) { n.indexBuilding = true })
	if _, err := r.Pages().LinkedFrom(t.Context(), "p1"); !errors.Is(err, search.ErrIndexBuilding) {
		t.Fatalf("backlinks from a node whose index is building = %v, want "+
			"search.ErrIndexBuilding across the wire", err)
	}
}

// A WRONG USAGE COPY ANSWERS NO SPEND SCREEN — ADR-0049. The usage domain's
// reads cross like every other domain's: a node whose usage copy is out of
// service answers its operator's spend and activity screens from a peer's
// copy, while its tracker reads are still its own; and every one of the
// domain's reads, its two-valued page of schedule runs included, arrives
// whole. Read straight off the node's own estate, they were answered from the
// very copy the node had stopped serving.
func TestAWrongUsageCopyAnswersNoSpendScreen(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	local := &fakeNode{name: "data-self", units: chartOf("self"),
		outOfServiceOn: map[string]bool{usageDomain: true}}
	r := f.router(t, "data-self", local)

	rows, err := r.Usage().Spend(t.Context(), usage.SpendQuery{From: "2026-10-01", To: "2026-10-01"})
	if err != nil || len(rows) != 1 || rows[0].Node == "data-self" || rows[0].Day != "2026-10-01" {
		t.Fatalf("spend on a node whose usage copy is wrong = (%+v, %v), want a peer's rows", rows, err)
	}
	if local.askedFor("usage.spend") {
		t.Fatal("a usage copy out of service answered a spend read")
	}
	if _, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil ||
		!local.askedFor("tasks") {
		t.Fatalf("a wrong usage copy moved the tracker's reads off this node (%v)", err)
	}

	u := r.Usage()
	ctx := t.Context()
	runs, more, err := u.ScheduleRuns(ctx, usage.ScheduleRunsQuery{Limit: 1})
	if err != nil || len(runs) != 1 || !more {
		t.Fatalf("schedule runs = (%+v, %v, %v), want a page and more", runs, more, err)
	}
	elided, err := u.ReadsElided(ctx, "2026-10-01", "2026-10-02")
	if err != nil || elided != 3 {
		t.Fatalf("reads elided = (%d, %v), want 3", elided, err)
	}
	for name, read := range map[string]func() (int, error){
		"seat turns": func() (int, error) {
			out, err := u.SeatTurns(ctx, usage.SpendQuery{})
			return len(out), err
		},
		"person spend": func() (int, error) {
			out, err := u.PersonSpend(ctx, usage.PersonQuery{})
			return len(out), err
		},
		"seat days": func() (int, error) {
			out, err := u.SeatDays(ctx, usage.SpendQuery{})
			return len(out), err
		},
		"page reads": func() (int, error) {
			out, err := u.PageReads(ctx, usage.PageReadsQuery{})
			return len(out), err
		},
	} {
		if n, err := read(); err != nil || n != 1 {
			t.Errorf("%s = (%d rows, %v), want one", name, n, err)
		}
	}
}

// A ROUTED USAGE READ ARRIVES WHOLE, IN PAGES. A window of spend is more than
// one message carries, so a peer answers it a page at a time and the asker
// asks again after the group each page ended on — here a thousand rows paged
// sixty-four at a time by a peer that chose that size itself: every row
// arrives, in order, each page asked after the one before it, and the node
// whose usage copy is out of service is never asked.
func TestARoutedUsageReadArrivesWholeInPages(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	var spend []usage.SpendRow
	for i := range 1000 {
		spend = append(spend, usage.SpendRow{Day: "2026-10-01", Node: "data-a",
			AgentID: fmt.Sprintf("seat-%04d", i)})
	}
	for _, name := range []string{"data-a", "data-b"} {
		f.nodes[name].set(func(n *fakeNode) { n.spend = spend })
	}
	local := &fakeNode{name: "data-self", units: chartOf("self"),
		outOfServiceOn: map[string]bool{usageDomain: true}}
	r := f.router(t, "data-self", local)

	rows, err := r.Usage().Spend(t.Context(), usage.SpendQuery{From: "2026-10-01", To: "2026-10-01"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(rows, spend, func(a, b usage.SpendRow) bool { return a.AgentID == b.AgentID }) {
		t.Fatalf("the read arrived as %d rows, want the %d the peer holds, in order", len(rows), len(spend))
	}
	var afters []usage.Group
	for _, name := range []string{"data-a", "data-b"} {
		n := f.nodes[name]
		n.mu.Lock()
		afters = append(afters, n.spendAfters...)
		n.mu.Unlock()
	}
	if len(afters) < 16 {
		t.Fatalf("the read took %d page(s) of a peer paging sixty-four rows at a time", len(afters))
	}
	slices.SortFunc(afters, func(a, b usage.Group) int {
		switch {
		case a.Before(b):
			return -1
		case b.Before(a):
			return 1
		}
		return 0
	})
	for i, after := range afters {
		if want := i * 64; (i == 0) != (len(after) == 0) || i > 0 && after[2] != spend[want-1].AgentID {
			t.Fatalf("page %d was asked after %v, want after the row the page before it ended on", i, after)
		}
	}
	if local.askedFor("usage.spend") {
		t.Fatal("a usage copy out of service answered a page")
	}
}

// A USAGE READ THIS NODE ANSWERS ITSELF IS ONE PAGE, and one a peer answers is
// paged at the wire's budget. Nothing carries an in-process answer, so paging
// it bought nothing and cost a transaction, a snapshot and an encoding of
// every row a page; a peer's answer crosses one message, so it is paged. Here
// the same thousand rows, read on a data node whose usage copy serves and
// then on one whose copy is out of service: the first asks its own copy once,
// with no budget, and never a peer; the second asks the peer page after page,
// each at the wire's budget, and never its own copy.
func TestAUsageReadThisNodeAnswersIsOnePageAndAPeersIsPaged(t *testing.T) {
	t.Parallel()
	var spend []usage.SpendRow
	for i := range 1000 {
		spend = append(spend, usage.SpendRow{Day: "2026-10-01", Node: "data-a",
			AgentID: fmt.Sprintf("seat-%04d", i)})
	}
	q := usage.SpendQuery{From: "2026-10-01", To: "2026-10-01"}

	f := newFleet(t, "data-a")
	f.nodes["data-a"].set(func(n *fakeNode) { n.spend = spend })
	local := &fakeNode{name: "data-self", units: chartOf("self"), spend: spend}
	rows, err := f.router(t, "data-self", local).Usage().Spend(t.Context(), q)
	if err != nil || len(rows) != len(spend) {
		t.Fatalf("this node's own read = (%d rows, %v), want all %d", len(rows), err, len(spend))
	}
	local.mu.Lock()
	budgets := slices.Clone(local.spendBudgets)
	local.mu.Unlock()
	if !slices.Equal(budgets, []int{0}) {
		t.Fatalf("this node answered itself at budgets %v, want one unbounded page", budgets)
	}
	if f.nodes["data-a"].askedFor("usage.spend") {
		t.Fatal("a read this node's copy answers was asked of a peer")
	}

	wrong := &fakeNode{name: "data-wrong", units: chartOf("self"), spend: spend,
		outOfServiceOn: map[string]bool{usageDomain: true}}
	rows, err = f.router(t, "data-wrong", wrong).Usage().Spend(t.Context(), q)
	if err != nil || len(rows) != len(spend) {
		t.Fatalf("a peer's read = (%d rows, %v), want all %d", len(rows), err, len(spend))
	}
	peer := f.nodes["data-a"]
	peer.mu.Lock()
	budgets = slices.Clone(peer.spendBudgets)
	peer.mu.Unlock()
	if len(budgets) < 16 || slices.ContainsFunc(budgets, func(b int) bool { return b != usagePageBytes }) {
		t.Fatalf("the peer answered at budgets %v, want every page at the wire's %d", budgets, usagePageBytes)
	}
	if wrong.askedFor("usage.spend") {
		t.Fatal("a usage copy out of service answered a page")
	}
}

// A REPOSITORY'S READERS ARE PAGED BY THEIR ASKER: each page is one routed
// call at the budget the asker names — the wire's own where that is the
// smaller — resumed from the cursor the page before handed back, so walked a
// page at a time every reader arrives once and in order; the count of readers
// the per-day cap dropped comes with the first page alone; and a cursor or a
// range the read cannot take is refused naming why before anything is asked.
//
// Mutation: ask the elided count on every page, and the walk asks it three
// times.
func TestARepositorysReadersArePagedByTheirAsker(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	wrong := &fakeNode{name: "data-wrong", units: chartOf("self"),
		outOfServiceOn: map[string]bool{usageDomain: true}}
	u := f.router(t, "data-wrong", wrong).Usage()
	q := usage.RepoReadsQuery{RepoID: "repo-1", From: "2026-10-01", To: "2026-10-30"}
	var walked []usage.RepoReader
	cursor, pages := "", 0
	for {
		a, err := u.RepoReaders(t.Context(), q, cursor, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if (pages == 0) != (a.Elided != nil) || a.Elided != nil && *a.Elided != 5 {
			t.Fatalf("page %d carries the elided count %v, want 5 on the first page alone", pages, a.Elided)
		}
		walked = append(walked, a.Readers...)
		pages++
		if a.Next == "" {
			break
		}
		cursor = a.Next
	}
	if !slices.Equal(walked, fakeReaders) || pages != len(fakeReaders) {
		t.Fatalf("walked in %d pages the readers are %+v, want %+v a page each", pages, walked, fakeReaders)
	}
	peer := f.nodes["data-a"]
	peer.mu.Lock()
	budgets, elided := slices.Clone(peer.repoBudgets), peer.repoElided
	peer.mu.Unlock()
	if !slices.Equal(budgets, []int{4096, 4096, 4096}) || elided != 1 {
		t.Fatalf("the peer was asked pages at %v and the elided count %d times, want the asker's 4096 "+
			"three times and the count once", budgets, elided)
	}
	if _, err := u.RepoReaders(t.Context(), q, "", 64<<20); err != nil {
		t.Fatal(err)
	}
	peer.mu.Lock()
	last := peer.repoBudgets[len(peer.repoBudgets)-1]
	peer.mu.Unlock()
	if last != usagePageBytes {
		t.Fatalf("a page asked past the wire's budget was asked at %d, want the wire's %d", last, usagePageBytes)
	}
	if wrong.askedFor("usage.repo_reads") {
		t.Fatal("a usage copy out of service answered a page of readers")
	}

	if _, err := u.RepoReaders(t.Context(), q, "not a cursor!", 4096); !errors.Is(err, usage.ErrBadCursor) {
		t.Errorf("a cursor no page issued = %v, want usage.ErrBadCursor", err)
	}
	long := q
	long.From = "2026-01-01"
	if _, err := u.RepoReaders(t.Context(), long, "", 4096); !errors.Is(err, usage.ErrBadRange) {
		t.Errorf("a range past the history = %v, want usage.ErrBadRange", err)
	}
}

// A USAGE PAGE THAT DOES NOT ADVANCE IS REFUSED rather than asked again: a node
// paging by another rule — or a fault that hands back where it began — would
// otherwise keep a screen's read looping for ever.
func TestAUsagePageThatDoesNotAdvanceIsRefused(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.nodes["data-a"].set(func(n *fakeNode) { n.stuckPages = true })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := f.client.Usage().Spend(ctx, usage.SpendQuery{From: "2026-10-01", To: "2026-10-01"})
	if err == nil || ctx.Err() != nil || !strings.Contains(err.Error(), "pages by another rule") {
		t.Fatalf("a page that ended where it began: %v (deadline: %v), want it refused naming the rule",
			err, ctx.Err())
	}
}

// A USAGE PAGE FITS ONE MESSAGE at its worst: a page of the skill readers'
// rows cut at [usagePageBytes], every string at its longest and every query
// the longest a search accepts spelled in the bytes JSON escapes to six, and
// every group padded to [usage.ReadsPerSeatDay] rows — more than any group of
// that read holds, since a group is one page's entries in one seat-day record
// and the record carries at most that many. Wrapped as a reply it is under
// the message the transport carries. A spend group is bounded the same way,
// by the record that wrote it being one message.
func TestAUsagePageFitsOneMessage(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 23, 23, 59, 59, 999_999_999, time.UTC)
	long := func(c string, n int) string { return strings.Repeat(c, n) }
	row := func(seat int) usage.PageRead {
		return usage.PageRead{PageID: long("p", 64), Day: "2026-09-23", Node: long("n", 64),
			AgentID: fmt.Sprintf("%036d", seat), Handle: long("h", 64), Role: long("r", 128),
			Backend: "confluence", Via: "skill_injected", Count: math.MaxInt64, LastAt: at,
			LastTurnID: long("t", 36), LastWorkKey: long("w", 36),
			LastQuery: long("<", knowledge.MaxQueryBytes)}
	}
	// EVERY GROUP FULL, so whichever one straddles the budget — the one a
	// page may run past it by — is as large as a group of this read gets.
	seq := func(yield func(usage.PageRead, error) bool) {
		for seat := 0; ; seat++ {
			for range usage.ReadsPerSeatDay {
				if !yield(row(seat), nil) {
					return
				}
			}
		}
	}
	page, err := usage.Fill(usagePageBytes, usage.PageRead.Group, seq)
	if err != nil {
		t.Fatal(err)
	}
	result, err := encodeResult(page)
	if err != nil {
		t.Fatal(err)
	}
	// SEALED, as the broker carries it: the signature's frame counts.
	raw := server{self: "data-a", env: fleetEnvelopes}.encodeReply(reply{Node: "data-a", Result: result})
	if decoded := rawReply(t, raw); decoded.Err != nil || len(decoded.Result) == 0 {
		t.Fatalf("a page at the budget did not cross as a result: %+v", decoded.Err)
	}
	t.Logf("a usage page at its worst is %d bytes of a %d-byte message", len(raw), queue.MaxPayloadBytes)
	if len(raw) >= queue.MaxPayloadBytes {
		t.Fatalf("a usage page at its worst is %d bytes, past the %d a message carries",
			len(raw), queue.MaxPayloadBytes)
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

	// NOBODY VOUCHES: the answer is the unknown, never an unserved estate.
	second.set(func(n *fakeNode) { n.unvouched = true })
	written, err = r.WriterAs(swe).CreateTask(t.Context(), "op-v", tracker.Task{Project: "ENG"}, nil)
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
	_, err := f.client.WriterAs(swe).CreateTask(t.Context(), "op-c", tracker.Task{Project: "ENG"}, nil)
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
	trackerServed, _, err := r.Serves(t.Context())
	if err != nil || trackerServed {
		t.Fatalf("a node whose own copy admits no seat was admitted (%v, %v)", trackerServed, err)
	}
	if peer.askedFor("admits") {
		t.Fatal("a peer was asked for a node whose own copy serves")
	}

	// A WRONG COPY OF A DOMAIN NO SEAT DEPENDS ON IS NOT THE SEAT'S COPY:
	// admission stays here, and a peer is not asked (ADR-0049).
	local.set(func(n *fakeNode) { n.outOfServiceOn = map[string]bool{"vectors": true} })
	if trackerServed, _, err = r.Serves(t.Context()); err != nil || trackerServed {
		t.Fatalf("a node whose own seat copy admits no seat was admitted on a peer's "+
			"word over a fault in a domain no seat depends on (%v, %v)", trackerServed, err)
	}
	if peer.askedFor("admits") {
		t.Fatal("a peer was asked for a node whose seat copy serves")
	}

	// A SEAT COPY OUT OF SERVICE is asked of a data node whose copy serves.
	local.set(func(n *fakeNode) { n.outOfServiceOn = map[string]bool{SeatCopy: true} })
	trackerServed, _, err = r.Serves(t.Context())
	if err != nil || !trackerServed {
		t.Fatalf("a node served by a peer was not admitted (%v, %v)", trackerServed, err)
	}

	// AND A REMOTE NODE THAT ADMITS NO SEAT IS PASSED OVER, like a node
	// that ran nothing.
	peer.set(func(n *fakeNode) { n.notAdmitting = true })
	if trackerServed, _, err = f.client.Serves(t.Context()); trackerServed {
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
		var answer served
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
	timing := Production()
	timing.AdmissionAsk = 200 * time.Millisecond
	r := f.routerTimed(t, "agent-2", nil, timing)
	type answer struct {
		served bool
		err    error
	}
	begun := time.Now()
	done := make(chan answer, 1)
	go func() {
		served, _, err := r.Serves(t.Context())
		done <- answer{served, err}
	}()
	select {
	case got := <-done:
		if got.served || got.err == nil {
			t.Fatalf("wedged data nodes admitted a seat (%v, %v)", got.served, got.err)
		}
		if waited := time.Since(begun); waited > 2*time.Second {
			t.Fatalf("admission waited %s on wedged data nodes, past its %s bound",
				waited, timing.AdmissionAsk)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admission waited on every wedged data node in turn, past its bound")
	}
	// AND THE NODE IT WAS ASKING IS SUSPECTED: the bound is the router's own,
	// so a node silent through it is evidence, and the next sweep — a
	// heartbeat later — asks the others first.
	f.placement.mu.Lock()
	told := slices.Clone(f.placement.unanswered)
	f.placement.mu.Unlock()
	if len(told) == 0 {
		t.Error("no wedged data node was reported unanswered: admission's own bound ran out " +
			"on one, and the next sweep would ask it first again")
	}
}

// A PUSH IS ASKED OF ITS REPOSITORY'S COMMITTER — the first node in the
// rendezvous order of the repository over every data node — whichever node
// asks and whichever node answered last, so a repository's pushes meet at one
// committer and its group commit holds them, while the repositories spread
// over the data nodes. A node that went silent is asked last for its keys and
// one that said its copy of the code log is out of service after the rest; a
// data node asks its OWN copy at its place in the order, in-process, and never
// ahead of the node the key prefers.
func TestAKeyedOpAsksTheKeysRendezvousOrder(t *testing.T) {
	t.Parallel()
	holders := []string{"data-a", "data-b", "data-c", "data-d"}
	f := newFleet(t, holders...)
	r := f.router(t, "agent-2", nil)
	committers := map[string]bool{}
	for i := range 32 {
		repo := fmt.Sprintf("repo-%02d", i)
		want := rendezvous.Order(repo, holders)[0]
		if _, err := r.Code().Propose(t.Context(), proposalTo(repo, "push-"+repo)); err != nil {
			t.Fatalf("propose to %s: %v", repo, err)
		}
		if got := f.proposedOn(repo); !slices.Equal(got, []string{want}) {
			t.Fatalf("a push to %s was proposed on %v, want its committer %s alone", repo, got, want)
		}
		committers[want] = true
	}
	if len(committers) < 3 {
		t.Fatalf("32 repositories met at %d committers %v, want them spread over the data nodes",
			len(committers), committers)
	}

	// NO STICKY NODE: a code read answered by one node leaves it the node
	// asked first for READS, and a push to a repository it is not the
	// committer of still goes to that repository's committer.
	if _, err := r.Code().Read(t.Context(), code.ReadQuery{Asker: code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-founder", Seat: "founder"}, Kind: code.ReadRepos}); err != nil {
		t.Fatalf("read: %v", err)
	}
	sticky := r.order(opCodeRead.spec, holders)[0]
	var repo string
	for i := 0; repo == ""; i++ {
		if candidate := fmt.Sprintf("repo-sticky-%d", i); rendezvous.Order(candidate, holders)[0] != sticky {
			repo = candidate
		}
	}
	o := rendezvous.Order(repo, holders)
	if _, err := r.Code().Propose(t.Context(), proposalTo(repo, "push-after-read")); err != nil {
		t.Fatalf("propose: %v", err)
	}
	if got := f.proposedOn(repo); !slices.Equal(got, []string{o[0]}) {
		t.Fatalf("after %s answered a read, a push to %s was proposed on %v, want its committer %s",
			sticky, repo, got, o[0])
	}

	// A NODE THAT WENT SILENT IS ASKED LAST for its keys, and one that said
	// its copy of the code log is out of service after the rest — the order
	// between them still the key's.
	r.markSuspect(o[0])
	r.markOutOfService(o[1], codeDomain)
	if got, want := r.orderFor(opCodePropose.spec, repo, holders),
		[]string{o[2], o[3], o[1], o[0]}; !slices.Equal(got, want) {
		t.Fatalf("with %s silent and %s's code copy out of service, %s is asked of %v, want %v",
			o[0], o[1], repo, got, want)
	}

	// A DATA NODE'S OWN COPY at its place in the order: run in-process where
	// the key prefers it, and asked only after the node the key prefers
	// otherwise — never over the wire.
	local := &fakeNode{name: "data-b", units: chartOf("data-b")}
	own := f.router(t, "data-b", local)
	first, second := "", ""
	for i := 0; first == "" || second == ""; i++ {
		candidate := fmt.Sprintf("repo-own-%d", i)
		order := rendezvous.Order(candidate, holders)
		switch {
		case order[0] == "data-b" && first == "":
			first = candidate
		case order[1] == "data-b" && second == "":
			second = candidate
		}
	}
	if _, err := own.Code().Propose(t.Context(), proposalTo(first, "push-own-first")); err != nil {
		t.Fatalf("propose: %v", err)
	}
	if got := f.proposedOn(first); len(got) != 0 || len(local.proposalsTo(first)) != 1 {
		t.Fatalf("a push to %s, whose committer is this node, was proposed over the wire on %v "+
			"and %d times in-process, want once in-process", first, got, len(local.proposalsTo(first)))
	}
	if _, err := own.Code().Propose(t.Context(), proposalTo(second, "push-own-second")); err != nil {
		t.Fatalf("propose: %v", err)
	}
	if got, want := f.proposedOn(second), rendezvous.Order(second, holders)[0]; !slices.Equal(got,
		[]string{want}) || len(local.proposalsTo(second)) != 0 {
		t.Fatalf("a push to %s was proposed on %v and %d times in-process, want its committer %s alone",
			second, got, len(local.proposalsTo(second)), want)
	}
}

// A KEYED WRITE THAT WENT UNANSWERED MOVES ON WITH THE SAME ARGUMENTS: the
// repository's runner-up is handed the very proposal its committer was — the
// same push id, which is what collapses the push should the first node have
// decided it after all — and its answer is the answer.
func TestAKeyedWriteMovesOnWithTheSameArguments(t *testing.T) {
	t.Parallel()
	holders := []string{"data-a", "data-b", "data-c"}
	f := newFleet(t, holders...)
	const repo = "repo-moves"
	o := rendezvous.Order(repo, holders)
	f.nodes[o[0]].set(func(n *fakeNode) { n.silent = true })
	sent := richProposal(repo)
	got, err := f.client.Code().Propose(t.Context(), sent)
	if err != nil {
		t.Fatalf("propose with the committer silent: %v", err)
	}
	if got.OpID != o[1] {
		t.Fatalf("the push was answered by %q, want the runner-up %s", got.OpID, o[1])
	}
	runnerUp := f.nodes[o[1]]
	runnerUp.mu.Lock()
	arrived := slices.Clone(runnerUp.proposals)
	runnerUp.mu.Unlock()
	if len(arrived) != 1 || !reflect.DeepEqual(arrived[0], sent) {
		t.Fatalf("the runner-up was proposed %+v, want exactly what was sent: %+v", arrived, sent)
	}
	silent := f.nodes[o[0]]
	silent.mu.Lock()
	asked, decided := silent.requests, len(silent.proposals)
	silent.mu.Unlock()
	if asked == 0 || decided != 0 {
		t.Fatalf("the committer was asked %d times and decided %d pushes, want it asked first "+
			"and silent", asked, decided)
	}
}

// A COMMITTER THAT RAN NOTHING HANDS THE REPOSITORY ON: a committer that
// refuses before it appends anything — stopping, a node that does not publish,
// a refusal about this node — is a node that ran nothing, so the runner-up is
// asked with the same push id, from a remote committer and from this node's
// own alike; and when every committer refuses, the refusal says why each did.
func TestACommitterThatRanNothingHandsTheRepositoryOn(t *testing.T) {
	t.Parallel()
	holders := []string{"data-a", "data-b"}
	f := newFleet(t, holders...)
	const repo = "repo-handed-on"
	o := rendezvous.Order(repo, holders)
	stopping := fmt.Errorf("%w: stopping", code.ErrNotServing)
	f.nodes[o[0]].set(func(n *fakeNode) { n.codeErr = stopping })
	got, err := f.client.Code().Propose(t.Context(), proposalTo(repo, "push-1"))
	if err != nil || got.OpID != o[1] {
		t.Fatalf("propose with the committer stopping = (%+v, %v), want the runner-up %s's answer",
			got, err, o[1])
	}
	if refused := len(f.nodes[o[0]].proposalsTo(repo)); refused != 1 {
		t.Fatalf("the stopping committer was proposed %d pushes, want it asked first", refused)
	}

	f.nodes[o[1]].set(func(n *fakeNode) { n.codeErr = stopping })
	_, err = f.client.Code().Propose(t.Context(), proposalTo(repo, "push-2"))
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) || strings.Count(unserved.Detail, "stopping") != 2 {
		t.Fatalf("every committer refusing = %v, want an unserved push naming each one's reason", err)
	}

	// THIS NODE'S OWN COMMITTER, refusing in-process, hands on alike.
	f.nodes[o[1]].set(func(n *fakeNode) { n.codeErr = nil })
	local := &fakeNode{name: o[0], units: chartOf(o[0]), codeErr: stopping}
	own := f.router(t, o[0], local)
	got, err = own.Code().Propose(t.Context(), proposalTo(repo, "push-3"))
	if err != nil || got.OpID != o[1] || len(local.proposalsTo(repo)) != 1 {
		t.Fatalf("propose with this node's own committer stopping = (%+v, %v), in-process %d, "+
			"want it asked first and the runner-up %s's answer", got, err,
			len(local.proposalsTo(repo)), o[1])
	}
}

// A NODE THAT RAN NOTHING FOR A PUSH IS ASKED LAST FOR THE NEXT ONE.
//
// A keyed order has no sticky node to move the walk off a node that refused,
// so a committer that answers `no_backend` — a node in a maintenance mode,
// whose mode decides no push for the whole window; a committer stopping; a
// refusal about the node itself — was asked FIRST again by every later push
// to each repository it ranks first for: the whole encoded proposal sent,
// decoded and refused, and only then the runner-up. Remembered as an
// `out_of_service` copy is, for [suspectFor], it is asked last for that
// operation instead — and only that one: its reads of the code log still
// serve, so a read's order is not moved — and the memory lapses with the
// window and clears the moment the node commits a push again.
func TestAKeyedWriteAsksANodeThatRanNothingLast(t *testing.T) {
	t.Parallel()
	holders := []string{"data-a", "data-b", "data-c"}
	f := newFleet(t, holders...)
	r := f.router(t, "agent-2", nil)
	const repo = "repo-declined"
	o := rendezvous.Order(repo, holders)
	publishesNothing := fmt.Errorf("%w: this node publishes nothing", code.ErrNotServing)
	f.nodes[o[0]].set(func(n *fakeNode) { n.codeErr = publishesNothing })

	if got, err := r.Code().Propose(t.Context(), proposalTo(repo, "push-1")); err != nil ||
		got.OpID != o[1] {
		t.Fatalf("the first push with %s declining = (%+v, %v), want the runner-up %s's "+
			"answer", o[0], got, err, o[1])
	}
	if got, err := r.Code().Propose(t.Context(), proposalTo(repo, "push-2")); err != nil ||
		got.OpID != o[1] {
		t.Fatalf("the second push = (%+v, %v), want the runner-up %s's answer", got, err, o[1])
	}
	if asked := len(f.nodes[o[0]].proposalsTo(repo)); asked != 1 {
		t.Fatalf("the node that ran nothing was proposed %d pushes, want only the first: "+
			"the second asks it last", asked)
	}
	if got, want := r.orderFor(opCodePropose.spec, repo, holders),
		[]string{o[1], o[2], o[0]}; !slices.Equal(got, want) {
		t.Fatalf("a push to %s is asked of %v, want %v — %s last", repo, got, want, o[0])
	}
	// ONLY FOR THE OPERATION IT DECLINED: its copy of the code log is not
	// out of service, so nothing moves it for a read.
	if class := r.classes(opCodeRead.spec, false)(o[0]); class != 1 {
		t.Fatalf("a read ranks %s in class %d, want 1 — declining a push says nothing "+
			"about its reads", o[0], class)
	}

	// THE WINDOW LAPSES: the node is back at its place in the key's order.
	r.now = func() time.Time { return time.Now().Add(2 * suspectFor) }
	if got := r.orderFor(opCodePropose.spec, repo, holders); !slices.Equal(got, o) {
		t.Fatalf("after the window a push to %s is asked of %v, want the key's order %v",
			repo, got, o)
	}
	r.now = time.Now

	// AND A NODE THAT COMMITS A PUSH AGAIN IS ITS PLACE AGAIN AT ONCE.
	f.nodes[o[0]].set(func(n *fakeNode) { n.codeErr = nil })
	r.markDeclined(o[0], opCodePropose.spec.name)
	f.nodes[o[1]].set(func(n *fakeNode) { n.codeErr = publishesNothing })
	f.nodes[o[2]].set(func(n *fakeNode) { n.codeErr = publishesNothing })
	if got, err := r.Code().Propose(t.Context(), proposalTo(repo, "push-3")); err != nil ||
		got.OpID != o[0] {
		t.Fatalf("with only %s committing = (%+v, %v), want its answer", o[0], got, err)
	}
	if class := r.classes(opCodePropose.spec, true)(o[0]); class != 1 {
		t.Fatalf("after %s committed a push it ranks in class %d for pushes, want 1 — "+
			"the decline it answered before is over", o[0], class)
	}
	for _, refused := range o[1:] {
		if class := r.classes(opCodePropose.spec, true)(refused); class != 2 {
			t.Fatalf("%s, which ran nothing for the push, ranks in class %d, want 2",
				refused, class)
		}
	}
}

// A CODE HALF THAT DECLINES A READ HANDS IT ON: a data node whose code half
// refuses a read code.ErrNotServing — a Served holding no reader of the code
// log, a half that is stopping — ran nothing, so every read the code host
// makes moves on to the next data node and is answered there, rather than
// failing the admission, the sync or the listing on the first node that
// asked; and when every data node declines, the unserved answer names each
// one's cause, so "nobody serves the code host" says why.
func TestACodeHalfThatDeclinesAReadHandsItOn(t *testing.T) {
	t.Parallel()
	holders := []string{"data-a", "data-b", "data-c"}
	// The client asks an unkeyed read in the rendezvous order of its own id;
	// every node but the LAST declines, so each declining node is asked
	// before the one that serves.
	o := rendezvous.Order("agent-1", holders)
	declined := fmt.Errorf("%w: this node holds no reader of the code log", code.ErrNotServing)
	covered := map[string]bool{}
	for _, r := range codeReads(t.Context(), CodeSeam{}) {
		covered[r.op] = true
	}
	requireEveryCodeRead(t, covered)
	for name := range covered {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t, holders...)
			read := func() error {
				for _, r := range codeReads(t.Context(), f.client.Code()) {
					if r.op == name {
						return r.read()
					}
				}
				t.Fatalf("no read named %s", name)
				return nil
			}
			for _, n := range o[:2] {
				f.nodes[n].set(func(n *fakeNode) { n.codeErr = declined })
			}
			if err := read(); err != nil {
				t.Fatalf("%s with %v declining = %v, want %s's answer", name, o[:2], err, o[2])
			}
			for _, n := range o {
				if !f.nodes[n].askedFor(name) {
					t.Fatalf("%s was never asked %s; want %v asked in turn", n, name, o)
				}
			}

			f.nodes[o[2]].set(func(n *fakeNode) { n.codeErr = declined })
			err := read()
			var unserved *ErrUnserved
			if !errors.As(err, &unserved) || strings.Count(unserved.Detail, "holds no reader") != len(o) {
				t.Fatalf("%s with every code half declining = %v, want an unserved read naming "+
					"each node's cause", name, err)
			}
		})
	}
}

// A KEYED OPERATION'S LAST RESORT KEEPS THE KEY'S ORDER: when every copy lags
// its log, the copies are asked again — told to take the request anyway — in
// the order the walk met them, so a data node whose own copy lags does not
// commit a push ahead of the repository's committer, which lags too. Asked
// first, this node's copy would split a burst between two committers exactly
// when the fleet is busiest.
func TestAKeyedLastResortKeepsTheKeysOrder(t *testing.T) {
	t.Parallel()
	holders := []string{"data-a", "data-b"}
	f := newFleet(t, holders...)
	const repo = "repo-lagging"
	o := rendezvous.Order(repo, holders)
	f.nodes[o[0]].set(func(n *fakeNode) { n.notReady = true })
	local := &fakeNode{name: o[1], units: chartOf(o[1]), notReady: true}
	own := f.router(t, o[1], local)
	got, err := own.Code().Propose(t.Context(), proposalTo(repo, "push-lagging"))
	if err != nil || got.OpID != o[0] {
		t.Fatalf("propose with every copy lagging = (%+v, %v), want the committer %s's answer",
			got, err, o[0])
	}
	if ran := len(local.proposalsTo(repo)); ran != 0 {
		t.Fatalf("this node's lagging copy decided %d pushes ahead of the committer %s", ran, o[0])
	}
}

// A KEYED OPERATION WHOSE ARGUMENTS NAME NO KEY IS REFUSED before anybody is
// asked: there is no node it prefers, and asking an arbitrary one would put a
// repository's pushes on as many committers as there are askers.
func TestAKeyedOpWithNoKeyIsRefused(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, err := f.client.Code().Propose(t.Context(), proposalTo("", "push-nowhere"))
	if err == nil || !strings.Contains(err.Error(), "code.propose") {
		t.Fatalf("a push naming no repository = %v, want a refusal naming the operation", err)
	}
	for name, n := range f.nodes {
		n.mu.Lock()
		asked := n.requests
		n.mu.Unlock()
		if asked != 0 {
			t.Errorf("%s was asked %d times for a push naming no repository", name, asked)
		}
	}
}

// THE CODE HOST'S OPERATIONS CARRY THE CODE LOG'S FLOOR — and only it: each
// names the code domain, so the code copy's own verdict decides whether a
// node answers it (ADR-0049), and a request carries this node's floor on the
// code log and never its floor on another domain's.
func TestTheCodeOpsCarryTheCodeFloor(t *testing.T) {
	t.Parallel()
	ops := []*opSpec{opCodePropose.spec, opCodeCompact.spec, opCodeExpunge.spec, opCodeChange.spec,
		opCodeConversation.spec, opCodeResolve.spec, opCodeRepoState.spec, opCodePushOutcome.spec,
		opCodeRead.spec, opCodeResolveRef.spec}
	for _, spec := range ops {
		if spec.domain != "code" || spec.floorStream != (code.Domain{}).Stream().Name ||
			spec.copies != "code" {
			t.Errorf("%s reads the %q copy with its floor on %q, want the code copy and %q",
				spec.name, spec.domain, spec.floorStream, (code.Domain{}).Stream().Name)
		}
	}
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	codeFloor := statelog.Position{Stream: codeStream, Generation: 1, Seq: 77}
	f.client.Observe(codeFloor)
	f.client.Observe(statelog.Position{Stream: trackerStream, Generation: 1, Seq: 12})
	ctx, seam := t.Context(), f.client.Code()
	if _, err := seam.Resolve(ctx, code.ResolveQuery{Name: "app"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := seam.RepoState(ctx, code.StateQuery{RepoID: "repo-1"}); err != nil {
		t.Fatalf("repo state: %v", err)
	}
	if _, err := seam.PushOutcome(ctx, code.OutcomeQuery{RepoID: "repo-1", PushID: "p"}); err != nil {
		t.Fatalf("push outcome: %v", err)
	}
	if _, err := seam.Read(ctx, code.ReadQuery{Asker: code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-founder", Seat: "founder"}, Kind: code.ReadRepos}); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := seam.ResolveRef(ctx, code.RefQuery{Repo: "app", Refs: []string{"main"},
		Asker: code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-founder", Seat: "founder"}}); err != nil {
		t.Fatalf("resolve ref: %v", err)
	}
	if _, err := seam.Propose(ctx, proposalTo("repo-1", "push-floor")); err != nil {
		t.Fatalf("propose: %v", err)
	}
	if _, err := seam.Compact(ctx, code.CompactionProposal{Compaction: code.Compaction{RepoID: "repo-1"}}); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if _, err := seam.Expunge(ctx, code.ExpungeProposal{Request: &code.ExpungeRequest{RepoID: "repo-1"}}); err != nil {
		t.Fatalf("expunge: %v", err)
	}
	if _, err := seam.Change(ctx, code.ChangeProposal{Close: &code.ChangeClose{RepoID: "repo-1"}}); err != nil {
		t.Fatalf("change: %v", err)
	}
	if _, err := seam.Converse(ctx, code.ConversationWrite{Comment: &code.Comment{RepoID: "repo-1"}}); err != nil {
		t.Fatalf("converse: %v", err)
	}
	node.mu.Lock()
	floors := slices.Clone(node.floors)
	node.mu.Unlock()
	if len(floors) != len(ops) {
		t.Fatalf("the code operations waited for %v, want one floor each", floors)
	}
	for _, at := range floors {
		if at != codeFloor {
			t.Errorf("a code operation waited for %v, want only this node's code floor %v", at, codeFloor)
		}
	}
}

// A KEYED OPERATION STILL ASKS ITS OWN COPY WHEN THE PLACEMENT CANNOT SAY WHO
// HOLDS THE ESTATE: the key orders the data nodes, and with none named this
// node's own copy is the one node left to ask — exactly the unknown-placement
// rule every other operation keeps.
func TestAKeyedOpAsksItsOwnCopyWhenThePlacementCannotSay(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.placement.set(func(p *fakePlacement) { p.err = errors.New("the presence view answers unknown") })
	local := &fakeNode{name: "data-self", units: chartOf("data-self")}
	r := f.router(t, "data-self", local)
	got, err := r.Code().Propose(t.Context(), proposalTo("repo-blind", "push-blind"))
	if err != nil || got.OpID != "data-self" || len(local.proposalsTo("repo-blind")) != 1 {
		t.Fatalf("propose with the placement unknown = (%+v, %v), want this node's own committer's "+
			"answer", got, err)
	}
}
