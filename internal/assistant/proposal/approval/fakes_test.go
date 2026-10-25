package approval_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

var noon = time.Date(2031, 4, 7, 12, 0, 0, 0, time.UTC)

// store is one fleet's proposals: an arbitrated move per subject, exactly the
// domain's rule (a move lands only from the state it names, and an operation
// already recorded is answered as a retry and decides nothing).
type store struct {
	mu        sync.Mutex
	proposals map[uuid.UUID]assistantdomain.Proposal
	ops       map[string]bool
	moves     []assistantdomain.ProposalMove
	now       func() time.Time
	// unknown makes every move answer unknown, for a broker nobody hears.
	unknown bool
}

func newStore() *store {
	return &store{proposals: map[uuid.UUID]assistantdomain.Proposal{}, ops: map[string]bool{},
		now: func() time.Time { return noon }}
}

func (s *store) Proposal(_ context.Context, owner, id uuid.UUID) (assistantdomain.Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proposals[id]
	if !ok || p.Owner != owner {
		return assistantdomain.Proposal{}, assistantdomain.ErrNotFound
	}
	return p, nil
}

func (s *store) TurnProposals(_ context.Context, owner, conversation,
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

func (s *store) Move(_ context.Context, c assistantdomain.Call, id uuid.UUID,
	m assistantdomain.ProposalMove) (approval.Written, error) {

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unknown {
		return approval.Written{Outcome: "unknown"}, nil
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
	return approval.Written{Outcome: "applied"}, nil
}

// state is a proposal's stored state.
func (s *store) state(id uuid.UUID) wire.ProposalState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proposals[id].State
}

// recorded is how many moves were recorded.
func (s *store) recorded() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.moves)
}

// grant is a grant the fake grants minted.
type grant struct {
	id      uuid.UUID
	scope   string
	request *exec.Request
	expires time.Time
}

func (g *grant) ID() uuid.UUID      { return g.id }
func (g *grant) Expires() time.Time { return g.expires }

// grants derives grants for the person on ctx, as the API's do.
type grants struct {
	mu      sync.Mutex
	execs   []approval.Exec
	revoked map[uuid.UUID]bool
	refuse  bool
}

func newGrants() *grants { return &grants{revoked: map[uuid.UUID]bool{}} }

func (g *grants) derive(ctx context.Context, scope string, r *exec.Request) (exec.Grant, error) {
	if g.refuse {
		return nil, fmt.Errorf("the session ended")
	}
	if p, how := iam.From(ctx); how != iam.Resolved || p.Kind != iam.KindPerson {
		return nil, fmt.Errorf("no person")
	}
	return &grant{id: uuid.New(), scope: scope, request: r, expires: time.Now().Add(time.Minute)}, nil
}

func (g *grants) Read(ctx context.Context, _, _ uuid.UUID) (exec.Grant, error) {
	return g.derive(ctx, "read", nil)
}

func (g *grants) Exec(ctx context.Context, x approval.Exec) (exec.Grant, error) {
	g.mu.Lock()
	g.execs = append(g.execs, x)
	g.mu.Unlock()
	r := x.Request
	return g.derive(ctx, "exec", &r)
}

func (g *grants) Revoke(id uuid.UUID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.revoked[id] = true
}

// api is the handler chain as the dispatcher reaches it: a write is admitted
// once per exec grant and only for the request the grant binds, and an
// operation key the ledger already holds is answered with its first write.
type api struct {
	mu      sync.Mutex
	writes  map[string]int // landed writes by operation key
	sent    []exec.Request
	answers []exec.Response // answers to give writes, in order; then applied
	err     error
	spent   map[uuid.UUID]bool
}

func newAPI() *api { return &api{writes: map[string]int{}, spent: map[uuid.UUID]bool{}} }

func (a *api) Do(_ context.Context, g exec.Grant, r exec.Request) (exec.Response, error) {
	held := g.(*grant)
	a.mu.Lock()
	defer a.mu.Unlock()
	if held.scope != "exec" {
		return exec.Response{Status: http.StatusForbidden}, nil
	}
	if a.spent[held.id] || held.request == nil || held.request.Path != r.Path ||
		string(held.request.Body) != string(r.Body) {
		return exec.Response{Status: http.StatusUnauthorized}, nil
	}
	a.spent[held.id] = true
	a.sent = append(a.sent, r)
	if a.err != nil {
		// THE WRITE LANDED AND ITS ANSWER WAS LOST, the case unknown is for.
		err := a.err
		a.err = nil
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
		"position": "CREWLET_TRACKER_LOG@1:42", "op_id": r.IdempotencyKey,
		"receipt": map[string]string{"key": "ENG-156"}})
	return exec.Response{Status: http.StatusOK, Body: body}, nil
}

// sends is how many writes reached the handler chain.
func (a *api) sends() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sent)
}

// turns records what the approval handed the turns.
type turns struct {
	mu       sync.Mutex
	resumed  []approval.Resumption
	emitted  []approval.Emission
	stream   string
	resumeOK bool
}

func newTurns() *turns { return &turns{stream: "/assistant/conversations/x/stream", resumeOK: true} }

func (t *turns) Resume(_ context.Context, r approval.Resumption) (wire.TurnAccepted, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resumed = append(t.resumed, r)
	return wire.TurnAccepted{TurnID: r.Turn, StreamURL: t.stream, AfterSeq: 7}, nil
}

func (t *turns) Emit(_ context.Context, _, _, turn uuid.UUID, e approval.Emission) (wire.TurnAccepted, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.emitted = append(t.emitted, e)
	return wire.TurnAccepted{TurnID: turn, StreamURL: t.stream, AfterSeq: 9}, nil
}

// fleet is one company's approval path: a store, a handler chain and the
// turns every node shares, and an approver per node.
type fleet struct {
	store  *store
	api    *api
	grants *grants
	turns  *turns
	// revision is what a re-read of a proposal's object answers, keyed by
	// the revision it was proposed at.
	revision func(string) (string, error)
	stepUp   map[string]bool
	clock    time.Time
}

func newFleet() *fleet {
	return &fleet{store: newStore(), api: newAPI(), grants: newGrants(), turns: newTurns(),
		revision: func(r string) (string, error) { return r, nil }, stepUp: map[string]bool{},
		clock: noon}
}

// node is one node's approver over the fleet.
func (f *fleet) node(t *testing.T) *approval.Approver {
	t.Helper()
	a, err := approval.New(approval.Deps{
		Store: f.store, Grants: f.grants, Dispatcher: f.api, Turns: f.turns,
		Revisions: func(_ context.Context, _ proposal.Read, r string) (string, error) {
			return f.revision(r)
		},
		StepUp: func(act string) bool { return f.stepUp[act] },
		StepOp: func(op string, steps ...string) string {
			for _, s := range steps {
				op += "." + s
			}
			return op
		},
		NewOp: func() string { return uuid.NewString() },
		Now:   func() time.Time { return f.clock },
	})
	if err != nil {
		t.Fatalf("approval.New: %v", err)
	}
	return a
}

// person is a person signed in through a browser session, proved at reauth.
func person(reauth time.Time) iam.Principal {
	return iam.Principal{ID: uuid.New(), Kind: iam.KindPerson, Stage: iam.StageActive,
		Login: "ada", Seat: "ada", Via: "session:" + uuid.NewString(), ReauthAt: reauth}
}

// as is ctx resolved to p.
func as(ctx context.Context, p iam.Principal) context.Context { return iam.WithPrincipal(ctx, p) }

// propose records a proposal of p's, drawn from a real preview, and answers it.
func (f *fleet) propose(t *testing.T, owner iam.Principal, mutate func(*proposal.Draft)) assistantdomain.Proposal {
	t.Helper()
	d := proposal.Draft{
		Owner: owner.ID, Conversation: uuid.New(), Turn: uuid.New(),
		Tool: "create_task", Act: "create_work_item", Scope: wire.ScopeServer, Now: f.clock,
		Preview: proposal.Preview{
			Title: "Create task", ApproveLabel: "Approve and create",
			Changes:        []wire.Change{proposal.Added("title", "Title", "Payroll review", false)},
			SourceRevision: "task:ENG-1@3",
			Request: exec.Request{Method: "POST", Path: "/operator/act/create_work_item",
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
		State: wire.ProposalProposed, ExpiresAt: n.ExpiresAt, CreatedAt: f.clock,
	}
	f.store.mu.Lock()
	f.store.proposals[p.ID] = p
	f.store.mu.Unlock()
	return p
}

// approve is one approval request of p's card under a fresh key.
func approve(p assistantdomain.Proposal) approval.Approval {
	return approval.Approval{Proposal: p.ID, Digest: p.Digest, Key: uuid.NewString()}
}
