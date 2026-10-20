package estate

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ---- a fake node's code half ------------------------------------------------ //

// Propose is a committer that commits every push it is given at the next
// position on the code log, answering as the operation its own node's name —
// so a case can tell which node decided — or refuses every push with codeErr;
// and, while holdProposals is set, decides nothing until it is closed.
func (f *fakeNode) Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error) {
	f.note("code.propose")
	f.mu.Lock()
	f.proposals = append(f.proposals, p)
	hold := f.holdProposals
	f.mu.Unlock()
	if hold != nil {
		f.proposing.Add(1)
		select {
		case <-hold:
		case <-ctx.Done():
			return code.ProposeResult{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.codeErr != nil {
		return code.ProposeResult{}, f.codeErr
	}
	f.codeSeq++
	return code.ProposeResult{
		Outcome:  statelog.OutcomeApplied,
		Position: statelog.Position{Stream: codeStream, Generation: 1, Seq: f.codeSeq},
		OpID:     f.name, Answer: code.PushAnswer{PushID: p.PushID},
	}, nil
}

// codeRefusal is what this node's code half refuses every operation with, or
// nil.
func (f *fakeNode) codeRefusal() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.codeErr
}

// Resolve answers a repository named for the query, on this node — or
// refuses with codeErr.
func (f *fakeNode) Resolve(_ context.Context, q code.ResolveQuery) (code.RepoRef, error) {
	f.note("code.resolve")
	if err := f.codeRefusal(); err != nil {
		return code.RepoRef{}, err
	}
	return code.RepoRef{ID: "repo-on-" + f.name, Name: q.Name}, nil
}

// Snapshot answers one empty full page — or refuses with codeErr.
func (f *fakeNode) Snapshot(_ context.Context, q code.StateQuery) (code.RepoSnapshot, error) {
	f.note("code.repo_state")
	if err := f.codeRefusal(); err != nil {
		return code.RepoSnapshot{}, err
	}
	return code.RepoSnapshot{Repo: code.RepoRef{ID: q.RepoID}, Full: true}, nil
}

// PushOutcome answers that the push was not found — or refuses with codeErr.
func (f *fakeNode) PushOutcome(context.Context, code.OutcomeQuery) (code.PushOutcome, error) {
	f.note("code.push_outcome")
	if err := f.codeRefusal(); err != nil {
		return code.PushOutcome{}, err
	}
	return code.PushOutcome{}, nil
}

// Read answers an empty listing, read at a position past every one this node
// committed at — or refuses with codeErr.
func (f *fakeNode) Read(context.Context, code.ReadQuery) (code.ReadAnswer, error) {
	f.note("code.read")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.codeErr != nil {
		return code.ReadAnswer{}, f.codeErr
	}
	return code.ReadAnswer{Position: statelog.Position{Stream: codeStream, Generation: 1,
		Seq: f.codeSeq + 1_000}}, nil
}

// codeReads is every read the code host makes through the seam, by the name
// of its operation — so a case that holds each read to a rule names the one a
// later commit adds as missing ([requireEveryCodeRead]).
func codeReads(ctx context.Context, seam CodeSeam) []struct {
	op   string
	read func() error
} {
	return []struct {
		op   string
		read func() error
	}{
		{opCodeResolve.spec.name, func() error {
			_, err := seam.Resolve(ctx, code.ResolveQuery{Name: "app"})
			return err
		}},
		{opCodeRepoState.spec.name, func() error {
			_, err := seam.RepoState(ctx, code.StateQuery{RepoID: "019a0c2e-0000-7000-8000-0000000000a1"})
			return err
		}},
		{opCodePushOutcome.spec.name, func() error {
			_, err := seam.PushOutcome(ctx, code.OutcomeQuery{RepoID: "019a0c2e-0000-7000-8000-0000000000a1",
				PushID: "019a0c2e-0000-7000-8000-0000000000e1"})
			return err
		}},
		{opCodeRead.spec.name, func() error {
			_, err := seam.Read(ctx, code.ReadQuery{Asker: code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-founder", Seat: "founder"}, Kind: code.ReadRepos})
			return err
		}},
	}
}

// requireEveryCodeRead fails t unless covered names every read registered on
// the code domain.
func requireEveryCodeRead(t *testing.T, covered map[string]bool) {
	t.Helper()
	for name, spec := range registry {
		if spec.domain == codeDomain && spec.class == opRead && !covered[name] {
			t.Errorf("%s reads the code log and codeReads does not ask it: add it there", name)
		}
	}
}

// proposalsTo is every push this node was proposed to repo.
func (f *fakeNode) proposalsTo(repo string) []code.Proposal {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []code.Proposal
	for _, p := range f.proposals {
		if p.RepoID == repo {
			out = append(out, p)
		}
	}
	return out
}

// proposedOn is every serving node a push to repo was proposed on, sorted.
func (f *fleet) proposedOn(repo string) []string {
	var out []string
	for name, n := range f.nodes {
		if len(n.proposalsTo(repo)) > 0 {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// proposalTo is a push of one branch to repo under pushID.
func proposalTo(repo, pushID string) code.Proposal {
	return code.Proposal{
		PushID: pushID, RepoID: repo, Node: "agent-1",
		Principal: code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-founder", Seat: "founder"},
		Commands: []code.Command{{Ref: "refs/heads/main", Old: code.ZeroOID,
			New: "1111111111111111111111111111111111111111"}},
		ReceivedAt: time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC),
	}
}

// richProposal is a push to repo carrying every field a proposal has: two
// commands, one the receiver already refused, a pack bundle, the objects it
// introduced and the code log's position the receiver checked it against.
func richProposal(repo string) code.Proposal {
	p := proposalTo(repo, "019a0c2e-0000-7000-8000-00000000a11d")
	p.Atomic = true
	p.Commands = append(p.Commands, code.Command{
		Ref: "refs/tags/v1", Old: code.ZeroOID, New: "2222222222222222222222222222222222222222",
		Result: code.ResultNG, Reason: code.RefusalTagMoved, Detail: "refs/tags/v1 already exists",
	})
	p.Pack = &code.PackRef{
		PackID: p.PushID, Object: objstore.KeyAt(p.ReceivedAt), Hash: objstore.HashOf([]byte("pack")),
		Size:       4_096 + code.IndexBytesFor(3) + code.ReverseBytesFor(3),
		IndexBytes: code.IndexBytesFor(3), ReverseBytes: code.ReverseBytesFor(3),
		GitChecksum: "3333333333333333333333333333333333333333", Objects: 3,
	}
	p.Introduced = []string{"1111111111111111111111111111111111111111",
		"4444444444444444444444444444444444444444"}
	p.IntroducedCount = 3
	p.Seen = statelog.Position{Stream: codeStream, Generation: 2, Seq: 40}
	return p
}

// ---- a data node's code half, as the code host's surfaces reach it ---------- //

// codeDesk is a data node's code half that records every query it was asked,
// whole, and the deadline each proposal was served under, and answers with the
// values it holds — or with err, where it is set.
type codeDesk struct {
	mu        sync.Mutex
	got       []any
	deadlines []time.Time // zero for a proposal served with none
	err       error
	codeAnswers
}

// codeAnswers is what a desk answers each operation with.
type codeAnswers struct {
	read code.ReadAnswer
	snap code.RepoSnapshot
	ref  code.RepoRef
	out  code.PushOutcome
	pos  statelog.Position
}

func (d *codeDesk) For(context.Context, string) (Backend, bool) { return Backend{Code: d}, true }

// took records one query and answers what the desk holds, read under its lock
// with the query: a case changes the answers between calls.
func (d *codeDesk) took(q any) (codeAnswers, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.got = append(d.got, q)
	return d.codeAnswers, d.err
}

func (d *codeDesk) last() any {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.got) == 0 {
		return nil
	}
	return d.got[len(d.got)-1]
}

func (d *codeDesk) Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error) {
	deadline, _ := ctx.Deadline()
	d.mu.Lock()
	d.deadlines = append(d.deadlines, deadline)
	d.mu.Unlock()
	held, err := d.took(p)
	return code.ProposeResult{Outcome: statelog.OutcomeApplied, Position: held.pos, OpID: "op-batch",
		Answer: code.PushAnswer{PushID: p.PushID, Commands: p.Commands}}, err
}

func (d *codeDesk) Resolve(_ context.Context, q code.ResolveQuery) (code.RepoRef, error) {
	held, err := d.took(q)
	return held.ref, err
}

func (d *codeDesk) Snapshot(_ context.Context, q code.StateQuery) (code.RepoSnapshot, error) {
	held, err := d.took(q)
	return held.snap, err
}

func (d *codeDesk) PushOutcome(_ context.Context, q code.OutcomeQuery) (code.PushOutcome, error) {
	held, err := d.took(q)
	return held.out, err
}

func (d *codeDesk) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	held, err := d.took(q)
	return held.read, err
}

// serveDesk serves d as data-d, the one data node the fleet's placement names.
func serveDesk(t *testing.T, f *fleet, d *codeDesk) {
	t.Helper()
	stop, err := Serve(t.Context(), f.start(t), "data-d", d, ServerSeams{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-d"} })
}

// EVERY CODE OPERATION CROSSES WHOLE: a push's proposal, the admission's
// resolve, a cache's snapshot page, the receiver's pending wait and the REST
// routes' listing reach a data node from a node that holds no data with their
// arguments as they were sent, their answers come back as they were given —
// a snapshot's packs, refs and cursor, a repository's detail and its limits —
// and a refusal keeps the identity the host and the cache branch on.
func TestEveryCodeOperationCrossesWhole(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	at := statelog.Position{Stream: codeStream, Generation: 2, Seq: 41}
	created := time.Date(2031, 3, 1, 9, 0, 0, 0, time.UTC)
	ref := code.RepoRef{
		ID: "019a0c2e-0000-7000-8000-0000000000a1", Name: "app", Status: code.RepoStatusActive,
		ManagedBy: code.ManagedConfig, Owner: code.Owner{Kind: code.OwnerUnit, Ref: "eng"},
		DefaultBranch: "main", Description: "the app",
		Protections: []code.Protection{{Pattern: "refs/heads/main", DirectPush: code.DirectNone}},
		Refs:        3, LivePacks: 2, LiveBytes: 9_000, LiveObjects: 1_200, Position: at,
	}
	pack := code.PackRow{
		PackID: "push-1", Part: "push", Object: objstore.KeyAt(created), Hash: objstore.HashOf([]byte("p1")),
		Size: 100, IndexBytes: 40, ReverseBytes: 20, GitChecksum: "5555555555555555555555555555555555555555",
		Objects: 2, Kind: "push", State: "live", Added: at,
	}
	cursor := &code.StateCursor{Full: true, Phase: "refs", From: at, Seen: at, Stream: created,
		After: "refs/heads/a"}
	d := &codeDesk{codeAnswers: codeAnswers{
		pos: at, ref: ref, out: code.PushOutcome{Found: true, Position: at,
			Push: code.PushRow{PushID: "push-1", RepoID: ref.ID, Position: at, At: created,
				Actor: "founder", ActorKind: "operator", Commands: []code.Command{{
					Ref: "refs/heads/main", Old: code.ZeroOID,
					New: "6666666666666666666666666666666666666666", Result: code.ResultOK}},
				ReceivedAt: created}},
		snap: code.RepoSnapshot{Repo: ref, Position: at, StreamCreated: created, Full: true,
			Packs: []code.PackRow{pack},
			Refs: []code.RefRow{{Ref: "refs/heads/main", OID: "6666666666666666666666666666666666666666",
				Position: at}},
			Next: cursor},
		read: code.ReadAnswer{Repo: &code.RepoDetail{
			RepoSummary: code.RepoSummary{RepoRef: ref, Head: "6666666666666666666666666666666666666666",
				Pushes: 7, LastPushAt: created},
			Branches: 2, Tags: 1, Tombstones: 4, CreatedAt: created,
			Limits: code.Limits{MaxPushBytes: 1 << 20, MaxBlobBytes: 1 << 10, RepoMaxBytes: 1 << 30},
		}, Next: "c-2", Position: at, Level: statelog.ReadSession},
	}}
	serveDesk(t, f, d)
	ctx, seam := t.Context(), f.client.Code()

	proposal := richProposal(ref.ID)
	proposed, err := seam.Propose(ctx, proposal)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if want := (code.ProposeResult{Outcome: statelog.OutcomeApplied, Position: at, OpID: "op-batch",
		Answer: code.PushAnswer{PushID: proposal.PushID, Commands: proposal.Commands}}); !reflect.DeepEqual(proposed, want) {
		t.Errorf("propose answered %+v, want %+v", proposed, want)
	}
	if got := d.last(); !reflect.DeepEqual(got, proposal) {
		t.Errorf("the committer was proposed %+v, want %+v", got, proposal)
	}

	resolveQ := code.ResolveQuery{Name: "app", Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}}
	if got, err := seam.Resolve(ctx, resolveQ); err != nil || !reflect.DeepEqual(got, ref) {
		t.Errorf("resolve = (%+v, %v), want %+v", got, err, ref)
	}
	if got := d.last(); !reflect.DeepEqual(got, resolveQ) {
		t.Errorf("resolve asked %+v, want %+v", got, resolveQ)
	}

	stateQ := code.StateQuery{RepoID: ref.ID, Since: at, Cursor: cursor,
		Fresh: statelog.Freshness{Level: statelog.ReadSession, MinPosition: at}}
	if got, err := seam.RepoState(ctx, stateQ); err != nil || !reflect.DeepEqual(got, d.snap) {
		t.Errorf("repo state = (%+v, %v), want %+v", got, err, d.snap)
	}
	if got := d.last(); !reflect.DeepEqual(got, stateQ) {
		t.Errorf("repo state asked %+v, want %+v", got, stateQ)
	}

	outcomeQ := code.OutcomeQuery{RepoID: ref.ID, PushID: "push-1", At: at}
	if got, err := seam.PushOutcome(ctx, outcomeQ); err != nil || !reflect.DeepEqual(got, d.out) {
		t.Errorf("push outcome = (%+v, %v), want %+v", got, err, d.out)
	}
	if got := d.last(); !reflect.DeepEqual(got, outcomeQ) {
		t.Errorf("push outcome asked %+v, want %+v", got, outcomeQ)
	}

	readQ := code.ReadQuery{Asker: code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-founder", Seat: "founder"}, Kind: code.ReadRepos, Status: []code.RepoStatus{code.RepoStatusArchived},
		Cursor: "c-1", Limit: 10, Fresh: statelog.Freshness{Level: statelog.ReadStale}}
	if got, err := seam.Read(ctx, readQ); err != nil || !reflect.DeepEqual(got, d.read) {
		t.Errorf("read = (%+v, %v), want %+v", got, err, d.read)
	}
	if got := d.last(); !reflect.DeepEqual(got, readQ) {
		t.Errorf("read asked %+v, want %+v", got, readQ)
	}

	// A REFUSAL KEEPS ITS IDENTITY: the admission answers a missing
	// repository 404, a cache reads a moved snapshot from its first page,
	// and a newer build's record is a version this one cannot read.
	for _, refusal := range []error{
		fmt.Errorf("resolve app: %w", code.ErrRepoNotFound),
		fmt.Errorf("page 3: %w", code.ErrSnapshotMoved),
	} {
		d.mu.Lock()
		d.err = refusal
		d.mu.Unlock()
		if _, err := seam.RepoState(ctx, stateQ); !errors.Is(err, errors.Unwrap(refusal)) ||
			err.Error() != refusal.Error() {
			t.Errorf("a served %v crossed as %v", refusal, err)
		}
	}
	future := &code.ErrFutureVersion{Got: 9, Want: 1, Subject: code.RepoSubject(ref.ID)}
	d.mu.Lock()
	d.err = fmt.Errorf("apply: %w", future)
	d.mu.Unlock()
	_, err = seam.Read(ctx, readQ)
	var crossed *code.ErrFutureVersion
	if !errors.As(err, &crossed) || !reflect.DeepEqual(crossed, future) {
		t.Errorf("a served future version crossed as %v (%+v), want %+v", err, crossed, future)
	}
}

// A PROPOSAL RAISES THIS NODE'S FLOOR AND A READ DOES NOT — departure D1: the
// node's session holds the positions of its OWN writes, which its next read of
// the repository must see on whichever node answers it; a read's position is
// somebody else's writes, and observing it would hold every later stale read
// this node routes behind the newest linearizable read anybody on it made.
func TestAProposalRaisesTheFloorAndAReadDoesNot(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	pushed := statelog.Position{Stream: codeStream, Generation: 1, Seq: 10}
	// EVERY READ ANSWERS PAST THE PUSH, in every position it carries — the
	// read's own, a repository's, a snapshot's — and the outcome read finds
	// a push this node did not propose there.
	past := statelog.Position{Stream: codeStream, Generation: 1, Seq: 90}
	repo := code.RepoRef{ID: "019a0c2e-0000-7000-8000-0000000000a1", Name: "app", Position: past}
	d := &codeDesk{codeAnswers: codeAnswers{pos: pushed, ref: repo,
		snap: code.RepoSnapshot{Repo: repo, Position: past, Full: true},
		out:  code.PushOutcome{Position: past},
		read: code.ReadAnswer{Position: past}}}
	serveDesk(t, f, d)
	if _, err := f.client.Code().Propose(t.Context(), proposalTo("repo-floor", "push-floor")); err != nil {
		t.Fatalf("propose: %v", err)
	}
	if got := f.client.Session().Floors([]string{codeStream}); !slices.Equal(got,
		[]statelog.Position{pushed}) {
		t.Fatalf("after a push applied at %v the code floor is %v", pushed, got)
	}
	covered := map[string]bool{}
	for _, r := range codeReads(t.Context(), f.client.Code()) {
		covered[r.op] = true
		if err := r.read(); err != nil {
			t.Fatalf("%s: %v", r.op, err)
		}
		if got := f.client.Session().Floors([]string{codeStream}); !slices.Equal(got,
			[]statelog.Position{pushed}) {
			t.Errorf("after %s answered at %v the code floor is %v, want still the push's %v",
				r.op, past, got, pushed)
		}
	}
	requireEveryCodeRead(t, covered)

	// AND A PUSH THIS NODE PROPOSED THAT IS FOUND LANDED raises it to the
	// push's own position, for the proposal's reason.
	landed := statelog.Position{Stream: codeStream, Generation: 1, Seq: 55}
	d.mu.Lock()
	d.out = code.PushOutcome{Found: true, Push: code.PushRow{PushID: "push-floor", Position: landed},
		Position: past}
	d.mu.Unlock()
	if _, err := f.client.Code().PushOutcome(t.Context(), code.OutcomeQuery{RepoID: "repo-floor",
		PushID: "push-floor", At: landed}); err != nil {
		t.Fatalf("push outcome: %v", err)
	}
	if got := f.client.Session().Floors([]string{codeStream}); !slices.Equal(got,
		[]statelog.Position{landed}) {
		t.Fatalf("after a push found landed at %v the code floor is %v", landed, got)
	}
}

// A PROPOSAL CARRIES ITS ATTEMPT TO THE COMMITTER: the committer answers a
// push inside its asker's attempt by judging it against the deadline it is
// served under — starting a batch only while a whole CommitBudget and the
// ReplyMargin are left, and answering it busy the moment they are not — so a
// routed proposal must reach the code half bounded by the attempt the asker
// gives it: the router's write attempt for a caller with no deadline (the
// host's receiver), and the caller's own where it has one. A proposal served
// with none would be judged by its wait in the queue alone, and answered up to
// the serving node's floor wait and the request's trip after its asker had
// handed the push id to the runner-up.
//
// THE ROUTER'S OWN WRITE ATTEMPT, read from the timing it was built with:
// halfway between the shortest a router takes and production's, so it is
// neither production's constant nor the router's read attempt, and a router
// that waited either for a write is caught.
func TestAProposalCarriesItsAttemptToTheCommitter(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	d := &codeDesk{}
	serveDesk(t, f, d)
	timing := Production()
	timing.WriteAttempt = (minWriteAttempt + Production().WriteAttempt) / 2
	if timing.WriteAttempt == timing.ReadAttempt || timing.WriteAttempt == Production().WriteAttempt {
		t.Fatalf("the premise: a write attempt of %v tells nothing apart from a read attempt of "+
			"%v and production's write attempt of %v", timing.WriteAttempt, timing.ReadAttempt,
			Production().WriteAttempt)
	}
	r := f.routerTimed(t, "agent-2", nil, timing)

	asked := time.Now()
	if _, err := r.Code().Propose(t.Context(),
		proposalTo("repo-deadline", "019a0c2e-0000-7000-8000-0000000000d1")); err != nil {
		t.Fatalf("propose: %v", err)
	}
	answered := time.Now()
	callerDeadline := answered.Add(5 * time.Minute)
	ctx, cancel := context.WithDeadline(t.Context(), callerDeadline)
	defer cancel()
	if _, err := r.Code().Propose(ctx,
		proposalTo("repo-deadline", "019a0c2e-0000-7000-8000-0000000000d2")); err != nil {
		t.Fatalf("propose with a deadline: %v", err)
	}

	d.mu.Lock()
	deadlines := slices.Clone(d.deadlines)
	d.mu.Unlock()
	if len(deadlines) != 2 {
		t.Fatalf("the committer was proposed %d pushes, want 2", len(deadlines))
	}
	attempt := timing.WriteAttempt
	if got := deadlines[0]; got.Before(asked.Add(attempt)) || got.After(answered.Add(attempt)) {
		t.Errorf("a push from a caller with no deadline was served under %v, want the router's write "+
			"attempt (%s) from the moment it was asked, between %v and %v", got, attempt,
			asked.Add(attempt), answered.Add(attempt))
	}
	if got := deadlines[1]; !got.Equal(callerDeadline) {
		t.Errorf("a push whose caller gave it until %v was served under %v, want the caller's own",
			callerDeadline, got)
	}
}

// A ROUTED PUSH CAN BE DECIDED INSIDE ITS ATTEMPT, or no router is built with
// it: the serving node may spend statelog.ReadBudget of the router's write
// attempt waiting for the asker's floor before the committer sees the push,
// and the committer starts a batch only while a CommitBudget and the
// ReplyMargin are left before the attempt ends — so the attempt, less that
// wait, must hold MORE than a batch and its answer, or every push asked of
// another node's committer by a caller with no deadline of its own is answered
// busy before anything decides it. A write attempt at that sum is refused,
// naming the field; the next is taken, and so is production's, whose excess is
// how long a push may queue behind its repository's earlier pushes.
//
// The sum is spelled here from the committer's bounds and the floor wait, never
// from the router's own constant, so a bound that lost a term fails; the two
// packages' constants meet here because estate imports code, never the
// reverse.
func TestARoutedPushCanBeDecidedInsideItsAttempt(t *testing.T) {
	t.Parallel()
	build := func(write time.Duration) error {
		timing := Production()
		timing.WriteAttempt = write
		_, err := NewRouter(RouterOptions{Self: "agent-1", Queue: &recordingAsker{},
			Placement: &fakePlacement{}, Session: NewSession(), Timing: timing})
		return err
	}
	bound := statelog.ReadBudget + code.CommitBudget + code.ReplyMargin
	if err := build(bound); err == nil || !strings.Contains(err.Error(), "Timing.WriteAttempt") {
		t.Errorf("a router whose write attempt is the serving node's floor wait (%v), a batch "+
			"(%v) and its answer (%v) answered %v, want a refusal naming Timing.WriteAttempt: no "+
			"push asked of another node's committer could be decided, only answered busy",
			statelog.ReadBudget, code.CommitBudget, code.ReplyMargin, err)
	}
	for _, write := range []time.Duration{bound + time.Nanosecond, Production().WriteAttempt} {
		if err := build(write); err != nil {
			t.Errorf("a router whose write attempt is %v, over the %v a routed push needs, "+
				"answered %v", write, bound, err)
		}
	}
}
