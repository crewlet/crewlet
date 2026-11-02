// Package approvaltest is a whole approval path in memory, for the suites that
// drive internal/assistant/proposal/approval: a store that arbitrates every
// move exactly as the assistant domain does, grants derived only for a person
// on the context, a handler chain that admits a write once per exec grant and
// only for the request it binds and answers a repeated operation key with its
// first write, and turns that record what they were handed.
//
// One fleet ([Fleet]) is shared by every approver built from it ([Fleet.Node]),
// so two approvers are two nodes of one company.
package approvaltest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
)

// Noon is every fleet's clock at the start.
var Noon = time.Date(2031, 4, 7, 12, 0, 0, 0, time.UTC)

// Landed is the position every applied write lands at.
const Landed = "CREWLET_TRACKER_LOG@1:42"

// Store is one fleet's proposals: an arbitrated move per proposal, exactly the
// domain's rule (a move lands only from the state it names, and an operation
// already recorded is answered as a retry and decides nothing).
type Store struct {
	mu        sync.Mutex
	proposals map[uuid.UUID]assistantdomain.Proposal
	ops       map[string]bool
	moves     []assistantdomain.ProposalMove
	bounds    []time.Time
	unknown   bool
	fail      error
	// failAfter is how many more moves land before every later one fails
	// with failLater, when failLater is set.
	failAfter int
	failLater error
	// unread, when set, is what every later read of a proposal answers.
	unread error
	now    func() time.Time
}

var _ approval.Store = (*Store)(nil)

// Proposal is one of the owner's proposals, and not found for anybody else.
func (s *Store) Proposal(_ context.Context, owner, id uuid.UUID) (assistantdomain.Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unread != nil {
		return assistantdomain.Proposal{}, s.unread
	}
	p, ok := s.proposals[id]
	if !ok || p.Owner != owner {
		return assistantdomain.Proposal{}, assistantdomain.ErrNotFound
	}
	return p, nil
}

// TurnProposals are the owner's proposals of one turn.
func (s *Store) TurnProposals(_ context.Context, owner, conversation,
	turn uuid.UUID) ([]assistantdomain.Proposal, error) {

	s.mu.Lock()
	defer s.mu.Unlock()
	var out []assistantdomain.Proposal
	for _, p := range s.proposals {
		if p.Owner == owner && p.Conversation == conversation && p.Turn == turn {
			out = append(out, p)
		}
	}
	return out, nil
}

// Move records one move, arbitrated.
func (s *Store) Move(ctx context.Context, c assistantdomain.Call, id uuid.UUID,
	m assistantdomain.ProposalMove) (approval.Written, error) {

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unknown {
		return approval.Written{Outcome: "unknown"}, nil
	}
	if s.fail != nil {
		return approval.Written{}, s.fail
	}
	if s.failLater != nil {
		if s.failAfter == 0 {
			return approval.Written{}, s.failLater
		}
		s.failAfter--
	}
	if s.ops[c.OpID] {
		return approval.Written{Outcome: "applied", Collapsed: true}, nil
	}
	p, ok := s.proposals[id]
	if !ok || p.Owner != c.Owner {
		return approval.Written{}, assistantdomain.ErrNotFound
	}
	if p.State != m.From {
		return approval.Written{}, &assistantdomain.AlreadyDecided{State: p.State}
	}
	s.ops[c.OpID] = true
	p.State = m.To
	p.DecidedAt = s.now()
	if m.Receipt != nil || m.Failure != nil {
		p.Receipt, p.Failure = m.Receipt, m.Failure
	}
	p.RefusalCode = m.RefusalCode
	s.proposals[id] = p
	s.moves = append(s.moves, m)
	bound, _ := ctx.Deadline()
	s.bounds = append(s.bounds, bound)
	return approval.Written{Outcome: "applied"}, nil
}

// Put stores p as it is.
func (s *Store) Put(p assistantdomain.Proposal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proposals[p.ID] = p
}

// State is a proposal's stored state.
func (s *Store) State(id uuid.UUID) wire.ProposalState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proposals[id].State
}

// Recorded is how many moves were recorded.
func (s *Store) Recorded() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.moves)
}

// MoveDeadlines are the deadlines each recorded move was handed, in order, the
// zero time for one handed none.
func (s *Store) MoveDeadlines() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.bounds)
}

// FailMoves makes every later move fail with err, as a store nobody can reach
// does.
func (s *Store) FailMoves(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = err
}

// FailMovesAfter lets the next n moves land and makes every later one fail
// with err, as a store that stops answering partway through a decision does.
func (s *Store) FailMovesAfter(n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failAfter, s.failLater = n, err
}

// FailReads makes every later read of a proposal fail with err, as a copy
// behind its log does.
func (s *Store) FailReads(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unread = err
}

// AnswerUnknown makes every later move answer unknown, as a broker nobody
// hears does.
func (s *Store) AnswerUnknown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unknown = true
}

// grant is a grant [Grants] minted.
type grant struct {
	id      uuid.UUID
	scope   iam.AssistScope
	request *exec.Request
	expires time.Time
}

func (g *grant) ID() uuid.UUID      { return g.id }
func (g *grant) Expires() time.Time { return g.expires }

// Grants derives grants for the person on the context, as the API's do.
type Grants struct {
	mu      sync.Mutex
	execs   []approval.Exec
	revoked map[uuid.UUID]bool
	refuse  bool
}

var _ approval.Grants = (*Grants)(nil)

func (g *Grants) derive(ctx context.Context, scope iam.AssistScope, r *exec.Request) (exec.Grant, error) {
	g.mu.Lock()
	refuse := g.refuse
	g.mu.Unlock()
	if refuse {
		return nil, fmt.Errorf("the session ended")
	}
	if p, how := iam.From(ctx); how != iam.Resolved || p.Kind != iam.KindPerson {
		return nil, fmt.Errorf("no person")
	}
	return &grant{id: uuid.New(), scope: scope, request: r, expires: time.Now().Add(time.Minute)}, nil
}

// Read derives a read grant.
func (g *Grants) Read(ctx context.Context, _, _ uuid.UUID) (exec.Grant, error) {
	return g.derive(ctx, iam.AssistScopeRead, nil)
}

// Exec derives an exec grant bound to x's request.
func (g *Grants) Exec(ctx context.Context, x approval.Exec) (exec.Grant, error) {
	g.mu.Lock()
	g.execs = append(g.execs, x)
	g.mu.Unlock()
	r := x.Request
	return g.derive(ctx, iam.AssistScopeExec, &r)
}

// Revoke ends a grant.
func (g *Grants) Revoke(id uuid.UUID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.revoked[id] = true
}

// Execs are the exec grants derived, in order.
func (g *Grants) Execs() []approval.Exec {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.execs)
}

// Revoked reports whether a grant was ended.
func (g *Grants) Revoked(id uuid.UUID) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.revoked[id]
}

// RefuseAll makes every later derivation refused, as a session that ended.
func (g *Grants) RefuseAll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refuse = true
}

// API is the handler chain as the dispatcher reaches it.
type API struct {
	mu      sync.Mutex
	writes  map[string]int
	sent    []exec.Request
	answers []exec.Response
	lose    error
	spent   map[uuid.UUID]bool
}

var _ exec.Dispatcher = (*API)(nil)

// Do serves one request.
func (a *API) Do(_ context.Context, g exec.Grant, r exec.Request) (exec.Response, error) {
	held := g.(*grant)
	a.mu.Lock()
	defer a.mu.Unlock()
	if held.scope != iam.AssistScopeExec {
		return exec.Response{Status: http.StatusForbidden}, nil
	}
	if a.spent[held.id] || held.request == nil || held.request.Path != r.Path ||
		string(held.request.Body) != string(r.Body) {
		return exec.Response{Status: http.StatusUnauthorized}, nil
	}
	a.spent[held.id] = true
	a.sent = append(a.sent, r)
	if a.lose != nil {
		// THE WRITE LANDED AND ITS ANSWER WAS LOST, the case unknown is for.
		err := a.lose
		a.lose = nil
		a.writes[r.IdempotencyKey]++
		return exec.Response{}, err
	}
	if len(a.answers) > 0 {
		resp := a.answers[0]
		a.answers = a.answers[1:]
		if resp.Status < 400 {
			a.writes[r.IdempotencyKey]++
		}
		return resp, nil
	}
	if a.writes[r.IdempotencyKey] == 0 {
		a.writes[r.IdempotencyKey]++
	}
	body, _ := json.Marshal(map[string]any{"tool": "create_work_item", "outcome": "applied",
		"position": Landed, "op_id": r.IdempotencyKey,
		"receipt": map[string]string{"key": "ENG-156"}})
	return exec.Response{Status: http.StatusOK, Body: body}, nil
}

// LoseNextAnswer makes the next write land and its answer never arrive.
func (a *API) LoseNextAnswer(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lose = err
}

// AnswerNext queues answers for the next writes.
func (a *API) AnswerNext(resp ...exec.Response) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answers = append(a.answers, resp...)
}

// Sent are the writes that reached the chain, in order.
func (a *API) Sent() []exec.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.sent)
}

// Writes is how many writes landed under an operation key.
func (a *API) Writes(key string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writes[key]
}

// Turns records what the approval handed the turns.
type Turns struct {
	mu      sync.Mutex
	resumed []approval.Resumption
	emitted []approval.Emission
	lapsed  []uuid.UUID
	// bounds are the deadlines each resumption was handed, zero for none.
	bounds []time.Time
	// withdrawn makes Carries answer false.
	withdrawn bool
	// unread, when set, is what every later Carries answers.
	unread error
}

var _ approval.Turns = (*Turns)(nil)

// StreamURL is where every handed turn streams from.
const StreamURL = "/assistant/conversations/c/stream"

// Resume records a resumption.
func (t *Turns) Resume(ctx context.Context, r approval.Resumption) (wire.TurnAccepted, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resumed = append(t.resumed, r)
	bound, _ := ctx.Deadline()
	t.bounds = append(t.bounds, bound)
	return wire.TurnAccepted{TurnID: r.Turn, StreamURL: StreamURL, AfterSeq: 7}, nil
}

// Emit records an emission.
func (t *Turns) Emit(_ context.Context, _, _, turn uuid.UUID, e approval.Emission) (wire.TurnAccepted, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.emitted = append(t.emitted, e)
	return wire.TurnAccepted{TurnID: turn, StreamURL: StreamURL, AfterSeq: 9}, nil
}

// Lapse records a turn the approval asked to end as expired.
func (t *Turns) Lapse(_ context.Context, _, _, turn uuid.UUID) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lapsed = append(t.lapsed, turn)
	return nil
}

// Withdraw makes every turn report that its approver no longer holds what it
// read ([Turns.Carries]).
func (t *Turns) Withdraw() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.withdrawn = true
}

// Carries reports whether the turn may carry on: false once [Turns.Withdraw].
func (t *Turns) Carries(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.unread != nil {
		return false, t.unread
	}
	return !t.withdrawn, nil
}

// FailCarries makes every later Carries fail with err, as a read of the
// turn's records that this node could not make does.
func (t *Turns) FailCarries(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.unread = err
}

// Lapsed are the turns asked to end as expired, in order.
func (t *Turns) Lapsed() []uuid.UUID {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.lapsed)
}

// Resumed are the resumptions, in order.
func (t *Turns) Resumed() []approval.Resumption {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.resumed)
}

// ResumeDeadlines are the deadlines each resumption was handed, in order, the
// zero time for one handed none.
func (t *Turns) ResumeDeadlines() []time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.bounds)
}

// Emitted are the emissions, in order.
func (t *Turns) Emitted() []approval.Emission {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.emitted)
}

// Fleet is one company's approval path.
type Fleet struct {
	Store  *Store
	API    *API
	Grants *Grants
	Turns  *Turns

	// Revision is what a re-read of a proposal's object answers, given the
	// revision it was proposed at; nil answers the same revision.
	Revision func(string) (string, error)
	// StepUp names the verbs whose row asks a recent proof.
	StepUp map[string]bool

	mu    sync.Mutex
	clock time.Time
}

// NewFleet is a fleet at [Noon].
func NewFleet() *Fleet {
	f := &Fleet{API: &API{writes: map[string]int{}, spent: map[uuid.UUID]bool{}},
		Grants: &Grants{revoked: map[uuid.UUID]bool{}}, Turns: &Turns{},
		StepUp: map[string]bool{}, clock: Noon}
	f.Store = &Store{proposals: map[uuid.UUID]assistantdomain.Proposal{}, ops: map[string]bool{},
		now: f.Now}
	return f
}

// Now is the fleet's clock.
func (f *Fleet) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

// Set moves the fleet's clock.
func (f *Fleet) Set(at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = at
}

// Node is one node's approver over the fleet.
func (f *Fleet) Node(t testing.TB) *approval.Approver {
	t.Helper()
	a, err := approval.New(approval.Deps{
		Store: f.Store, Grants: f.Grants, Dispatcher: f.API, Turns: f.Turns,
		Revisions: func(_ context.Context, _ proposal.Read, r string) (string, error) {
			if f.Revision == nil {
				return r, nil
			}
			return f.Revision(r)
		},
		StepUp: func(act string) bool { return f.StepUp[act] },
		StepOp: func(op string, steps ...string) string {
			for _, s := range steps {
				op += "." + s
			}
			return op
		},
		DomainOf: func(stream string) string {
			if stream == "CREWLET_TRACKER_LOG" {
				return "tracker"
			}
			return ""
		},
		Now: f.Now,
	})
	if err != nil {
		t.Fatalf("approval.New: %v", err)
	}
	return a
}

// Person is a person signed in through a browser session, proved until reauth.
func Person(reauth time.Time) iam.Principal {
	return iam.Principal{ID: uuid.New(), Kind: iam.KindPerson, Stage: iam.StageActive,
		Login: "ada", Seat: "ada", Via: "session:" + uuid.NewString(), ReauthAt: reauth}
}

// As is ctx resolved to p.
func As(ctx context.Context, p iam.Principal) context.Context { return iam.WithPrincipal(ctx, p) }

// Propose records a proposal of owner's, drawn from a real preview of a task
// creation, finished by mutate, and answers it.
func (f *Fleet) Propose(t testing.TB, owner iam.Principal, mutate func(*proposal.Draft)) assistantdomain.Proposal {
	t.Helper()
	d := proposal.Draft{
		Owner: owner.ID, Conversation: uuid.New(), Turn: uuid.New(),
		Tool: "create_task", Act: "create_work_item", Scope: wire.ScopeServer, Now: f.Now(),
		Preview: proposal.Preview{
			Title: "Create task", ApproveLabel: "Approve and create",
			Changes:        []wire.Change{proposal.Added("title", "Title", "Payroll review", false)},
			SourceRevision: "task:ENG-1@3",
			Request: exec.Request{Method: http.MethodPost, Path: "/operator/act/create_work_item",
				Body: []byte(`{"args":{"title":"Payroll review"}}`)},
		},
	}
	if mutate != nil {
		mutate(&d)
	}
	n, err := proposal.New(d)
	if err != nil {
		t.Fatalf("proposal.New: %v", err)
	}
	p := assistantdomain.Proposal{
		ID: n.Proposal, Conversation: d.Conversation, Turn: n.Turn, Owner: d.Owner,
		Tool: n.Tool, Act: n.Act, Scope: n.Scope, Method: n.Method, Path: n.Path,
		Query: n.Query, Body: n.Body, RequestDigest: n.RequestDigest, Digest: n.Digest,
		OpKey: n.OpKey, SourceRevision: n.SourceRevision, StepUp: n.StepUp, Card: n.Card,
		State: wire.ProposalProposed, ExpiresAt: n.ExpiresAt, CreatedAt: f.Now(),
	}
	f.Store.Put(p)
	return p
}

// Approve is one approval request of p's card under a fresh key.
func Approve(p assistantdomain.Proposal) approval.Approval {
	return approval.Approval{Proposal: p.ID, Digest: p.Digest, Key: uuid.NewString()}
}
