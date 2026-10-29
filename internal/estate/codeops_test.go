package estate

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
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

// ProposeCompaction is a committer that commits every compaction it is given
// at the next position on the code log — or refuses every one with codeErr.
func (f *fakeNode) ProposeCompaction(_ context.Context, p code.CompactionProposal) (code.CompactResult, error) {
	f.note("code.compact")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.compactions = append(f.compactions, p)
	if f.codeErr != nil {
		return code.CompactResult{}, f.codeErr
	}
	f.codeSeq++
	return code.CompactResult{Outcome: statelog.OutcomeApplied,
		Position: statelog.Position{Stream: codeStream, Generation: 1, Seq: f.codeSeq}}, nil
}

// ProposeExpunge is a committer that commits every expunge record it is given
// at the next position on the code log — or refuses every one with codeErr.
func (f *fakeNode) ProposeExpunge(_ context.Context, p code.ExpungeProposal) (code.ExpungeResult, error) {
	f.note("code.expunge")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expunges = append(f.expunges, p)
	if f.codeErr != nil {
		return code.ExpungeResult{}, f.codeErr
	}
	f.codeSeq++
	return code.ExpungeResult{Outcome: statelog.OutcomeApplied,
		Position: statelog.Position{Stream: codeStream, Generation: 1, Seq: f.codeSeq}}, nil
}

// ProposeChange is a committer that commits every write of a change it is
// given at the next position on the code log — or refuses every one with
// codeErr.
func (f *fakeNode) ProposeChange(_ context.Context, p code.ChangeProposal) (code.ChangeResult, error) {
	f.note("code.change")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changes = append(f.changes, p)
	if f.codeErr != nil {
		return code.ChangeResult{}, f.codeErr
	}
	f.codeSeq++
	return code.ChangeResult{Outcome: statelog.OutcomeApplied, ChangeID: p.ChangeID(), Number: 1, Version: 1,
		Position: statelog.Position{Stream: codeStream, Generation: 1, Seq: f.codeSeq}}, nil
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

// ResolveRef answers the ref named for the query at the next position past
// every one this node committed at — or refuses with codeErr.
func (f *fakeNode) ResolveRef(_ context.Context, q code.RefQuery) (code.RefAnswer, error) {
	f.note("code.resolve_ref")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.codeErr != nil {
		return code.RefAnswer{}, f.codeErr
	}
	at := statelog.Position{Stream: codeStream, Generation: 1, Seq: f.codeSeq + 1_000}
	out := code.RefAnswer{Repo: code.RepoRef{ID: "repo-on-" + f.name, Name: q.Repo}, Position: at}
	for _, name := range q.Refs {
		out.Refs = append(out.Refs, code.ResolvedRef{Name: name, Ref: name, Moved: at})
	}
	return out, nil
}

// Lifecycle decides every lifecycle request it is given at the next position
// on the code log, answering the repository the request names as applied — or
// refuses every one with codeErr.
func (f *fakeNode) Lifecycle(_ context.Context, r code.LifecycleRequest) (code.LifecycleResult, error) {
	f.note("code.lifecycle")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.codeErr != nil {
		return code.LifecycleResult{}, f.codeErr
	}
	f.codeSeq++
	at := statelog.Position{Stream: codeStream, Generation: 1, Seq: f.codeSeq}
	return code.LifecycleResult{Outcome: statelog.OutcomeApplied,
		Repo: code.RepoRef{ID: cmp.Or(r.RepoID, "repo-on-"+f.name), Name: r.Name, Position: at}, Position: at}, nil
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
		{opCodeResolveRef.spec.name, func() error {
			_, err := seam.ResolveRef(ctx, code.RefQuery{Repo: "app", Refs: []string{"main"},
				Asker: code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-founder", Seat: "founder"}})
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
		Via:       code.ViaGit,
		Commands: []code.Command{{Ref: "refs/heads/main", Old: code.ZeroOID,
			New: "1111111111111111111111111111111111111111"}},
		ReceivedAt: time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC),
	}
}

// richProposal is a push to repo carrying every field a proposal has: two
// commands, one the receiver already refused, a pack bundle, the objects it
// introduced, and the code log's position and the drop epoch the receiver
// checked it against.
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
	p.DropEpoch = 3
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
	read      code.ReadAnswer
	snap      code.RepoSnapshot
	ref       code.RepoRef
	out       code.PushOutcome
	pos       statelog.Position
	resolved  code.RefAnswer
	git       code.GitAnswer
	compacted code.CompactResult
	expunged  code.ExpungeResult
	changed   code.ChangeResult
	lifecycle code.LifecycleResult
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

func (d *codeDesk) ProposeCompaction(_ context.Context, p code.CompactionProposal) (code.CompactResult, error) {
	held, err := d.took(p)
	return held.compacted, err
}

func (d *codeDesk) ProposeExpunge(_ context.Context, p code.ExpungeProposal) (code.ExpungeResult, error) {
	held, err := d.took(p)
	return held.expunged, err
}

func (d *codeDesk) ProposeChange(_ context.Context, p code.ChangeProposal) (code.ChangeResult, error) {
	held, err := d.took(p)
	return held.changed, err
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

func (d *codeDesk) ResolveRef(_ context.Context, q code.RefQuery) (code.RefAnswer, error) {
	held, err := d.took(q)
	return held.resolved, err
}

func (d *codeDesk) Lifecycle(_ context.Context, r code.LifecycleRequest) (code.LifecycleResult, error) {
	held, err := d.took(r)
	return held.lifecycle, err
}

// deskGit is a desk as a git node's git half: what it reads by object id is
// recorded and answered as the desk's code half is, the desk's err included.
type deskGit struct{ d *codeDesk }

func (g deskGit) Git() (GitServer, bool) { return g, true }

func (g deskGit) Read(_ context.Context, q code.GitQuery) (code.GitAnswer, error) {
	held, err := g.d.took(q)
	return held.git, err
}

// serveDesk serves d as data-d, the one data node — and the one git node — the
// fleet's placements name.
func serveDesk(t *testing.T, f *fleet, d *codeDesk) {
	t.Helper()
	stop, err := Serve(t.Context(), f.start(t), ServerOptions{Self: "data-d", Local: d,
		Git: deskGit{d: d}, Envelopes: fleetEnvelopes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-d"} })
	f.git.set(func(p *fakePlacement) { p.nodes = []string{"data-d"} })
}

// EVERY CODE OPERATION CROSSES WHOLE: a push's proposal and a compaction's —
// a publication's, its layer whole — and each of an expunge's three records,
// the admission's resolve, a cache's snapshot page, the receiver's pending
// wait, the REST routes' listing, a seat's refs listing, a git node's
// introduced pages and its compactor's, publisher's and maintainer's due
// listings, and a seat's resolve of a ref reach a data node from a node that
// holds no data —
// and a read by object id the git node — with their arguments as they were
// sent (a seat's reads naming the colleague they are made for, whose grant
// the answering node judges too), their answers come back as they were
// given — a snapshot's layers, packs, refs, pushes, retirements and cursor, a
// repository's detail and its limits, the refs and the introducing pushes,
// the due listings, a resolved ref, a page of a diff — and a refusal keeps
// the identity the host and the cache branch on.
func TestEveryCodeOperationCrossesWhole(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	at := statelog.Position{Stream: codeStream, Generation: 2, Seq: 41}
	created := time.Date(2031, 3, 1, 9, 0, 0, 0, time.UTC)
	ref := code.RepoRef{
		ID: "019a0c2e-0000-7000-8000-0000000000a1", Name: "app", Status: code.RepoStatusActive,
		ManagedBy: code.ManagedTools, Kind: code.RepoProject, Owner: code.Owner{Kind: code.OwnerUnit, Ref: "eng"},
		DefaultBranch: "main", Description: "the app",
		Protections:    []code.Protection{{Pattern: "refs/heads/main", DirectPush: code.DirectNone}},
		LastActivityAt: created.Add(-time.Hour), Refs: 3, LivePacks: 2, LiveBytes: 9_000, LiveObjects: 1_200,
		DropEpoch: 3, Redirect: "app-2", RedirectUntil: created.Add(24 * time.Hour), Position: at,
	}
	pack := code.PackRow{
		PackID: "push-1", Part: "push", Object: objstore.KeyAt(created), Hash: objstore.HashOf([]byte("p1")),
		Size: 100, IndexBytes: 40, ReverseBytes: 20, GitChecksum: "5555555555555555555555555555555555555555",
		Objects: 2, Kind: "push", State: "live", Added: at,
	}
	cursor := &code.StateCursor{Phase: "pushes", Pushes: true, From: at, Since: at, Seen: at, Stream: created,
		RetiredFrom: at, DropEpoch: 3, After: "1000041/push-1"}
	retired := code.RetiredRow{PackID: "push-0", Part: "pack",
		GitChecksum: "7777777777777777777777777777777777777777", RetiredAt: created, Position: at}
	introduced := code.IntroducedRow{
		PushedBy: code.PushedBy{PushID: "push-1", Position: at, At: created, Via: code.ViaGit,
			Principal: code.Principal{Kind: code.PrincipalOperator, OperatorID: "pat:7", Seat: "founder"}},
		Moves: []code.RefMove{{Ref: "refs/heads/main", Old: code.ZeroOID,
			New: "6666666666666666666666666666666666666666", Forced: true}},
		Introduced: []string{"6666666666666666666666666666666666666666"}, Count: 2,
	}
	// A PUBLISHED LAYER, every field of it: what a publication carries and
	// what a snapshot's layers phase serves.
	layer := code.LayerRef{LayerID: "9999999999999999999999999999999999999999",
		BaseID:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Replaces: []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		GraphID:  "cccccccccccccccccccccccccccccccccccccccc",
		Packs:    []string{"5555555555555555555555555555555555555555", "8888888888888888888888888888888888888888"},
		Parts: []code.LayerPart{{Object: objstore.KeyAt(created), Hash: objstore.HashOf([]byte("l1")),
			Size: 300}},
		MidxBytes: 200, BitmapBytes: 60, GraphBytes: 40, Objects: 4, Commits: 2, Git: "2.56.0", TookMs: 1_200,
	}
	row := layer
	row.Replaces = nil
	d := &codeDesk{codeAnswers: codeAnswers{
		pos: at, ref: ref, out: code.PushOutcome{Found: true, Position: at,
			Push: code.PushRow{PushID: "push-1", RepoID: ref.ID, Position: at, At: created,
				Actor: "founder", ActorKind: "operator", Commands: []code.Command{{
					Ref: "refs/heads/main", Old: code.ZeroOID,
					New: "6666666666666666666666666666666666666666", Result: code.ResultOK}},
				ReceivedAt: created}},
		snap: code.RepoSnapshot{Repo: ref, Position: at, StreamCreated: created,
			Layers: []code.LayerRow{{Seq: 2, Layer: row, Added: at}}, LayersServed: true,
			Packs: []code.PackRow{pack},
			Refs: []code.RefRow{{Ref: "refs/heads/main", OID: "6666666666666666666666666666666666666666",
				Position: at}},
			Pushes:      []code.IntroducedRow{introduced},
			Retired:     []code.RetiredRow{retired},
			RetiredFrom: at,
			Next:        cursor},
		read: code.ReadAnswer{Repo: &code.RepoDetail{
			RepoSummary: code.RepoSummary{RepoRef: ref, Head: "6666666666666666666666666666666666666666",
				Pushes: 7, LastPushAt: created},
			Branches: 2, Tags: 1, Tombstones: 4, CreatedAt: created,
			Limits: code.Limits{MaxPushBytes: 1 << 20, MaxBlobBytes: 1 << 10, RepoMaxBytes: 1 << 30,
				PushesPerSeatDay: 40},
			PathOwners: []code.PathOwner{{Owner: code.Owner{Kind: code.OwnerUnit, Ref: "plat"},
				Paths: []string{"deploy/**", "web/**"}}},
		}, Refs: []code.RefRow{{Ref: "refs/heads/main", OID: "6666666666666666666666666666666666666666",
			Position: at}}, Introduced: []code.IntroducedRow{introduced},
			Due: []code.CompactionCandidate{{ID: ref.ID, Name: "app", LivePacks: 40, LiveObjects: 4_000,
				Version: at}},
			Publish: []code.PublicationCandidate{{ID: ref.ID, Name: "app", LivePacks: 40, PublishedPacks: 30,
				LiveObjects: 4_000, PublishedObjects: 3_000, LastPushAt: created, PublishedAt: created,
				PublishTook: 1200 * time.Millisecond, Version: at}},
			Expunge: []code.ExpungeCandidate{{ID: ref.ID, Name: "app", Open: "019a0c2e-0000-7000-8000-0000000000e1",
				Request: "019a0c2e-0000-7000-8000-0000000000e0", Requested: at, ExpungedAt: created, LivePacks: 40,
				LiveBytes: 90_000, Version: at}},
			Next: "c-2", Position: at, Level: statelog.ReadSession},
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

	// A COMPACTION'S PROPOSAL, every field of it and of its answer — a
	// publication's, so its layer crosses too.
	compaction := code.CompactionProposal{Compaction: code.Compaction{
		RepoID: ref.ID, CompactionID: "019a0c2e-0000-7000-8000-0000000000c1", Node: "git-a", Seen: at,
		Retire: []string{"push-0", "push-1"},
		Add: []code.PackRef{{PackID: "019a0c2e-0000-7000-8000-0000000000c2", Object: objstore.KeyAt(created),
			Hash: objstore.HashOf([]byte("c2")), Size: 1_300, IndexBytes: 1_100, ReverseBytes: 60,
			GitChecksum: "8888888888888888888888888888888888888888", Objects: 2}},
		Layer: &layer,
	}}
	d.mu.Lock()
	d.compacted = code.CompactResult{Outcome: statelog.OutcomePending, Refusal: code.RefusalStale,
		Detail: "pack push-0 is full; the next pass plans again", Position: at}
	d.mu.Unlock()
	if got, err := seam.Compact(ctx, compaction); err != nil || !reflect.DeepEqual(got, d.compacted) {
		t.Errorf("compact = (%+v, %v), want %+v", got, err, d.compacted)
	}
	if got := d.last(); !reflect.DeepEqual(got, compaction) {
		t.Errorf("the committer was proposed the compaction %+v, want %+v", got, compaction)
	}

	// AN EXPUNGE'S THREE RECORDS, every field of each and of the answer: a
	// person's request, and a maintainer's begin and commit.
	bundle := func(id, sum string) code.PackRef {
		return code.PackRef{PackID: id, Object: objstore.KeyAt(created), Hash: objstore.HashOf([]byte(id)),
			Size: 1_300, IndexBytes: 1_100, ReverseBytes: 60, GitChecksum: sum, Objects: 2}
	}
	for name, p := range map[string]code.ExpungeProposal{
		"a request": {Request: &code.ExpungeRequest{RepoID: ref.ID,
			RequestID: "019a0c2e-0000-7000-8000-0000000000e0.code_purge_unreachable",
			By:        code.Principal{Kind: code.PrincipalOperator, OperatorID: "pat:7", Seat: "founder"}}},
		"a begin": {Begin: &code.ExpungeBegin{RepoID: ref.ID, ExpungeID: "019a0c2e-0000-7000-8000-0000000000e1",
			RequestID: "019a0c2e-0000-7000-8000-0000000000e0.code_purge_unreachable", Node: "git-a", Seen: at,
			DropEpoch: 4, Retire: []string{"push-0", "push-1"},
			Add:        []code.PackRef{bundle("019a0c2e-0000-7000-8000-0000000000e2", "8888888888888888888888888888888888888888")},
			Dropping:   []code.PackRef{bundle("019a0c2e-0000-7000-8000-0000000000e3", "9999999999999999999999999999999999999999")},
			DropLayers: []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, Grace: code.PruneGrace}},
		"a commit": {Commit: &code.ExpungeCommit{RepoID: ref.ID, ExpungeID: "019a0c2e-0000-7000-8000-0000000000e1",
			CommitID: "019a0c2e-0000-7000-8000-0000000000e4", RequestID: "019a0c2e-0000-7000-8000-0000000000e0",
			Node: "git-a", Seen: at, DropEpoch: 4, Retire: []string{"019a0c2e-0000-7000-8000-0000000000e3"},
			Add: []code.PackRef{bundle("019a0c2e-0000-7000-8000-0000000000e5",
				"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")}}},
	} {
		d.mu.Lock()
		d.expunged = code.ExpungeResult{Outcome: statelog.OutcomePending, Refusal: code.RefusalStale,
			Detail: "the next pass plans again", Position: at, Request: "019a0c2e-0000-7000-8000-0000000000e0"}
		d.mu.Unlock()
		if got, err := seam.Expunge(ctx, p); err != nil || !reflect.DeepEqual(got, d.expunged) {
			t.Errorf("expunge %s = (%+v, %v), want %+v", name, got, err, d.expunged)
		}
		if got := d.last(); !reflect.DeepEqual(got, p) {
			t.Errorf("the committer was proposed %s %+v, want %+v", name, got, p)
		}
	}

	// A CHANGE'S SIX WRITES, every field of each — the fields the committer
	// stamps among them, which cross whole whoever set them — and every
	// field of the answer.
	title, body := "Retry the flaky upload", "It fails one run in ten.\n\nSee ENG-412."
	seat := code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}
	item := &code.ChangeOpen{RepoID: ref.ID, ChangeID: "019a0c2e-0000-7000-8000-0000000000d1",
		SourceRef: "refs/heads/fix", TargetRef: "refs/heads/main", Head: "6666666666666666666666666666666666666666",
		Title: title, Body: body, Draft: true, Owner: seat,
		WorkItem: &types.WorkItem{Backend: types.WorkNative, ID: "task-9", Key: "ENG-412", Project: "ENG"},
		TurnID:   "019a0c2e-0000-7000-8000-0000000000d2", RunID: "019a0c2e-0000-7000-8000-0000000000d3"}
	for name, p := range map[string]code.ChangeProposal{
		"an open": {Open: item},
		"an edit": {Edit: &code.ChangeEdit{RepoID: ref.ID, ChangeID: item.ChangeID, By: seat, Title: &title,
			Body: &body, WorkItem: item.WorkItem, ClearWorkItem: true}},
		"a draft's state": {Draft: &code.ChangeDraft{RepoID: ref.ID, ChangeID: item.ChangeID, By: seat,
			Draft: true}},
		"a retarget": {Retarget: &code.ChangeRetarget{RepoID: ref.ID, ChangeID: item.ChangeID, By: seat,
			TargetRef: "refs/heads/release"}},
		"a close": {Close: &code.ChangeClose{RepoID: ref.ID, ChangeID: item.ChangeID, By: seat,
			Reason: "superseded by web!43"}},
		"a reopen": {Reopen: &code.ChangeReopen{RepoID: ref.ID, ChangeID: item.ChangeID, By: seat,
			Head: "7777777777777777777777777777777777777777"}},
	} {
		p.OpID, p.Asker, p.Seen, p.ExpectedVersion = "019a0c2e-0000-7000-8000-0000000000d0.code_open_change",
			seat, at, 4
		d.mu.Lock()
		d.changed = code.ChangeResult{Outcome: statelog.OutcomeApplied, ChangeID: item.ChangeID, Number: 42,
			Version: 5, Position: at, Unchanged: true, Refusal: code.ChangeRefusalStale,
			Detail: "app!42 is at version 5, and this write was formed against version 4"}
		d.mu.Unlock()
		if got, err := seam.Change(ctx, p); err != nil || !reflect.DeepEqual(got, d.changed) {
			t.Errorf("change %s = (%+v, %v), want %+v", name, got, err, d.changed)
		}
		if got := d.last(); !reflect.DeepEqual(got, p) {
			t.Errorf("the committer was proposed %s %+v, want %+v", name, got, p)
		}
	}

	resolveQ := code.ResolveQuery{Name: "app", Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}}
	if got, err := seam.Resolve(ctx, resolveQ); err != nil || !reflect.DeepEqual(got, ref) {
		t.Errorf("resolve = (%+v, %v), want %+v", got, err, ref)
	}
	if got := d.last(); !reflect.DeepEqual(got, resolveQ) {
		t.Errorf("resolve asked %+v, want %+v", got, resolveQ)
	}

	stateQ := code.StateQuery{RepoID: ref.ID, Since: at, Cursor: cursor, Pushes: true, RetiredSince: at,
		Fresh: statelog.FloorAt(statelog.ReadSession, at)}
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
	for _, q := range []code.ReadQuery{
		{Asker: code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}, CeilingSeat: "lead", Kind: code.ReadRefs,
			Repo: "app", Prefix: "refs/heads/seats/dev/", Cursor: "refs/heads/seats/dev/a", Limit: 7,
			Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}},
		{Kind: code.ReadIntroduced, Introduced: &code.IntroducedQuery{RepoID: ref.ID, After: at,
			Through: statelog.Position{Stream: codeStream, Generation: 2, Seq: 90}}, Cursor: "1000041/push-1",
			Limit: 1, Fresh: statelog.FloorAt(statelog.ReadSession, at)},
		{Kind: code.ReadIntroduced, Introduced: &code.IntroducedQuery{RepoID: ref.ID, Descending: true,
			Before: at}, Fresh: statelog.Freshness{Level: statelog.ReadStale}},
		{Kind: code.ReadRunPushes, RunPushes: &code.RunPushesQuery{Run: "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b",
			Repos: []string{ref.ID, "0190f3c4-0000-7000-8000-00000000b0b0"}, After: at},
			Cursor: "1/1000041/push-1", Limit: 2, Fresh: statelog.FloorAt(statelog.ReadSession, at)},
		{Kind: code.ReadCompactionDue, MinPacks: 8, Cursor: "40/" + ref.ID,
			Fresh: statelog.Freshness{Level: statelog.ReadStale}},
		{Kind: code.ReadPublicationDue, Cursor: "1700000000000000/" + ref.ID,
			Fresh: statelog.Freshness{Level: statelog.ReadStale}},
		{Kind: code.ReadExpungeDue, Cursor: ref.ID, Fresh: statelog.Freshness{Level: statelog.ReadStale}},
	} {
		if got, err := seam.Read(ctx, q); err != nil || !reflect.DeepEqual(got, d.read) {
			t.Errorf("read %s = (%+v, %v), want %+v", q.Kind, got, err, d.read)
		}
		if got := d.last(); !reflect.DeepEqual(got, q) {
			t.Errorf("read %s asked %+v, want %+v", q.Kind, got, q)
		}
	}

	// A REF RESOLVED, and a READ BY OBJECT ID asked of the git node: each
	// query whole — the git read's floor, cursor, paths given as bytes and
	// its sub-query — and each answer whole, the git read's page with its
	// next cursor and the position its copy was read at. The walk stamps
	// the git read's attempt itself (home, asked of its one git node first),
	// so that is the one field it adds to what the caller sent.
	d.mu.Lock()
	d.resolved = code.RefAnswer{Repo: ref, Refs: []code.ResolvedRef{
		{Name: "main", Ref: "refs/heads/main", OID: "6666666666666666666666666666666666666666", Moved: at},
		{Name: "gone", Missing: "repository app holds no live refs/heads/gone or refs/tags/gone"},
	}, Position: at}
	d.git = code.GitAnswer{Kind: code.GitDiff, RepoID: ref.ID, Position: at, Next: "page-2",
		Diff: &code.DiffAnswer{Base: "6666666666666666666666666666666666666666",
			Head: "7777777777777777777777777777777777777777", Files: []code.DiffFile{{Path: "a.go",
				Status: "M", Added: 3, Deleted: 1}}, Total: 1, Added: 3, Deleted: 1}}
	d.mu.Unlock()
	refQ := code.RefQuery{Repo: "app", Refs: []string{"main", "gone"},
		Fresh: statelog.Freshness{Level: statelog.ReadLinearizable},
		Asker: code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}, CeilingSeat: "lead"}
	if got, err := seam.ResolveRef(ctx, refQ); err != nil || !reflect.DeepEqual(got, d.resolved) {
		t.Errorf("resolve ref = (%+v, %v), want %+v", got, err, d.resolved)
	}
	if got := d.last(); !reflect.DeepEqual(got, refQ) {
		t.Errorf("resolve ref asked %+v, want %+v", got, refQ)
	}
	gitQ := code.GitQuery{Kind: code.GitDiff, RepoID: ref.ID, Asker: code.Principal{Kind: code.PrincipalAgent,
		Seat: "dev"}, CeilingSeat: "lead", Fresh: statelog.FloorAt(statelog.ReadSession, at), Cursor: "page-1",
		Diff: &code.DiffQuery{Base: "6666666666", Head: "7777777777777777777777777777777777777777",
			FileB64: "YS5nbw==", Renames: true}}
	if got, err := seam.GitRead(ctx, gitQ); err != nil || !reflect.DeepEqual(got, d.git) {
		t.Errorf("git read = (%+v, %v), want %+v", got, err, d.git)
	}
	stamped := gitQ
	stamped.Home = true
	if got := d.last(); !reflect.DeepEqual(got, stamped) {
		t.Errorf("git read asked %+v, want %+v", got, stamped)
	}

	// A LIFECYCLE REQUEST of every kind's fields, and its answer whole: a
	// create's whole settings, a settings CHANGE and a protection's — the
	// changes the deciding node resolves — a delete's reason, and a hold's
	// turn and the read it is decided at.
	d.mu.Lock()
	d.lifecycle = code.LifecycleResult{Outcome: statelog.OutcomePending, Repo: ref,
		Refusal: code.LifecycleStale, Detail: "another rename of the repository landed first", Position: at}
	d.mu.Unlock()
	desc, branch, vis := "the app, renamed", "trunk", code.VisibilityUnit
	owner := code.Owner{Kind: code.OwnerUnit, Ref: "plat"}
	op := "019a0c2e-0000-7000-8000-0000000000f0.code_test"
	for name, r := range map[string]code.LifecycleRequest{
		"a create": {Kind: code.LifecycleCreate, OpID: op, Actor: seat, Name: "dev.app",
			Create: &code.RepoSettings{ManagedBy: code.ManagedTools, Kind: code.RepoScratch,
				Owner: code.Owner{Kind: code.OwnerSeat, Ref: "dev"}, DefaultBranch: "main",
				Description: "an experiment", Visibility: code.VisibilityUnit}},
		"a rename": {Kind: code.LifecycleRename, OpID: op, Actor: seat, RepoID: ref.ID, Name: "app-3"},
		"a settings change": {Kind: code.LifecycleSettings, OpID: op,
			Actor:  code.Principal{Kind: code.PrincipalOperator, OperatorID: "pat:7", Seat: "founder"},
			RepoID: ref.ID, Change: &code.RepoChange{Description: &desc, DefaultBranch: &branch,
				Visibility: &vis, Owner: &owner, PathOwners: &code.PathOwnersChange{
					Add:    []code.PathOwner{{Owner: owner, Paths: []string{"deploy/**", "web/**"}}},
					Remove: []code.PathOwner{{Owner: code.Owner{Kind: code.OwnerSeat, Ref: "dev"}, Paths: []string{"a"}}},
				}}},
		"a protection": {Kind: code.LifecycleProtect, OpID: op, Actor: seat, RepoID: ref.ID,
			Protect: &code.ProtectChange{Pattern: "refs/heads/release/*", Rule: &code.Protection{
				Pattern: "refs/heads/release/*", AllowForce: true, AllowDelete: true,
				DirectPush: code.DirectPersons}}},
		"a delete": {Kind: code.LifecycleDelete, OpID: op, Actor: seat, RepoID: ref.ID, Reason: "done with it"},
		"a hold": {Kind: code.LifecycleHold, OpID: op, Actor: seat, RepoID: ref.ID, TurnID: "turn-1",
			Seen: at},
	} {
		if got, err := seam.Lifecycle(ctx, r); err != nil || !reflect.DeepEqual(got, d.lifecycle) {
			t.Errorf("lifecycle %s = (%+v, %v), want %+v", name, got, err, d.lifecycle)
		}
		if got := d.last(); !reflect.DeepEqual(got, r) {
			t.Errorf("lifecycle %s asked %+v, want %+v", name, got, r)
		}
	}
	// AND A CONFIGURATION'S REPOSITORY'S REFUSAL KEEPS ITS SENTINEL.
	d.mu.Lock()
	d.err = fmt.Errorf("rename app: %w", code.ErrConfigManaged)
	d.mu.Unlock()
	if _, err := seam.Lifecycle(ctx, code.LifecycleRequest{Kind: code.LifecycleArchive, OpID: op,
		Actor: seat, RepoID: ref.ID}); !errors.Is(err, code.ErrConfigManaged) {
		t.Errorf("a served config_managed crossed as %v", err)
	}
	d.mu.Lock()
	d.err = nil
	d.mu.Unlock()

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
	// AND THE REPOSITORY'S OWN FLOOR, which the seam hands a read by object id
	// of THAT repository — a git operation carries it in its arguments rather
	// than the router's — and a read of any other repository is not handed.
	if got := f.client.Code().FloorOf("repo-floor"); got != pushed {
		t.Fatalf("after a push applied at %v the repository's floor is %v", pushed, got)
	}
	if got := f.client.Code().FloorOf("repo-elsewhere"); !got.IsZero() {
		t.Fatalf("a push to repo-floor floored repo-elsewhere at %v", got)
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
	if got := f.client.Code().FloorOf("repo-floor"); got != landed {
		t.Fatalf("after a push found landed at %v the repository's floor is %v", landed, got)
	}

	// AND A COMPACTION THIS NODE PROPOSED raises them to the compaction's
	// record, which its builder's next listing and sync must see — where it
	// landed, and never where it was refused with no record — the floor of
	// the repository it compacted and of no other.
	compacted := statelog.Position{Stream: codeStream, Generation: 1, Seq: 70}
	for _, tc := range []struct {
		answer code.CompactResult
		// want is the session's floor after it, and repo the compacted
		// repository's own.
		want, repo statelog.Position
	}{
		{code.CompactResult{Refusal: code.RefusalStale, Detail: "pack push-0 is full"}, landed, statelog.Position{}},
		{code.CompactResult{Outcome: statelog.OutcomeApplied, Position: compacted}, compacted, compacted},
	} {
		d.mu.Lock()
		d.compacted = tc.answer
		d.mu.Unlock()
		if _, err := f.client.Code().Compact(t.Context(), code.CompactionProposal{
			Compaction: code.Compaction{RepoID: "repo-compacted"}}); err != nil {
			t.Fatalf("compact: %v", err)
		}
		if got := f.client.Session().Floors([]string{codeStream}); !slices.Equal(got,
			[]statelog.Position{tc.want}) {
			t.Fatalf("after a compaction answered %+v the code floor is %v, want %v", tc.answer, got, tc.want)
		}
		if got := f.client.Code().FloorOf("repo-compacted"); got != tc.repo {
			t.Fatalf("after a compaction answered %+v the compacted repository's floor is %v, want %v",
				tc.answer, got, tc.repo)
		}
		if got := f.client.Code().FloorOf("repo-floor"); got != landed {
			t.Fatalf("a compaction of another repository moved repo-floor's floor to %v", got)
		}
	}

	// AND AN EXPUNGE RECORD THIS NODE PROPOSED raises them to the record
	// where it landed, and never where it was refused with no record: the
	// floor of the repository it is about and of no other.
	expunged := statelog.Position{Stream: codeStream, Generation: 1, Seq: 80}
	for _, tc := range []struct {
		answer code.ExpungeResult
		// want is the session's floor after it, and repo the expunged
		// repository's own.
		want, repo statelog.Position
	}{
		{code.ExpungeResult{Refusal: code.RefusalStale, Detail: "expunge e1 is open"}, compacted, statelog.Position{}},
		{code.ExpungeResult{Outcome: statelog.OutcomeApplied, Position: expunged}, expunged, expunged},
	} {
		d.mu.Lock()
		d.expunged = tc.answer
		d.mu.Unlock()
		if _, err := f.client.Code().Expunge(t.Context(), code.ExpungeProposal{
			Request: &code.ExpungeRequest{RepoID: "repo-expunged"}}); err != nil {
			t.Fatalf("expunge: %v", err)
		}
		if got := f.client.Session().Floors([]string{codeStream}); !slices.Equal(got,
			[]statelog.Position{tc.want}) {
			t.Fatalf("after an expunge answered %+v the code floor is %v, want %v", tc.answer, got, tc.want)
		}
		if got := f.client.Code().FloorOf("repo-expunged"); got != tc.repo {
			t.Fatalf("after an expunge answered %+v the expunged repository's floor is %v, want %v",
				tc.answer, got, tc.repo)
		}
		if got := f.client.Code().FloorOf("repo-compacted"); got != compacted {
			t.Fatalf("an expunge of another repository moved repo-compacted's floor to %v", got)
		}
	}

	// AND A CHANGE'S WRITE THIS NODE PROPOSED raises them to the record where
	// it landed — never where it was refused, and never where it was answered
	// UNCHANGED from the change's row, whose position is the change's last
	// record, somebody else's write: the floor of the repository it is about
	// and of no other.
	changed := statelog.Position{Stream: codeStream, Generation: 1, Seq: 85}
	for _, tc := range []struct {
		answer code.ChangeResult
		// want is the session's floor after it, and repo the change's
		// repository's own.
		want, repo statelog.Position
	}{
		{code.ChangeResult{Refusal: code.ChangeRefusalStale, Detail: "app!1 is at version 3"}, expunged,
			statelog.Position{}},
		{code.ChangeResult{Outcome: statelog.OutcomeApplied, Number: 1, Version: 3, Unchanged: true,
			Position: statelog.Position{Stream: codeStream, Generation: 1, Seq: 99}}, expunged, statelog.Position{}},
		{code.ChangeResult{Outcome: statelog.OutcomeApplied, Number: 1, Version: 4, Position: changed}, changed,
			changed},
	} {
		d.mu.Lock()
		d.changed = tc.answer
		d.mu.Unlock()
		if _, err := f.client.Code().Change(t.Context(), code.ChangeProposal{
			Close: &code.ChangeClose{RepoID: "repo-changed"}}); err != nil {
			t.Fatalf("change: %v", err)
		}
		if got := f.client.Session().Floors([]string{codeStream}); !slices.Equal(got,
			[]statelog.Position{tc.want}) {
			t.Fatalf("after a change answered %+v the code floor is %v, want %v", tc.answer, got, tc.want)
		}
		if got := f.client.Code().FloorOf("repo-changed"); got != tc.repo {
			t.Fatalf("after a change answered %+v its repository's floor is %v, want %v", tc.answer, got, tc.repo)
		}
		if got := f.client.Code().FloorOf("repo-expunged"); got != expunged {
			t.Fatalf("a change of another repository moved repo-expunged's floor to %v", got)
		}
	}

	// AND A LIFECYCLE REQUEST THIS NODE ASKED raises them to its record where
	// it landed — and never for a refusal, nor for an answer the rows gave with
	// no record (a no-op, a repeat of nothing), whose position is the rows
	// somebody else wrote: the floor of the repository it is about and of no
	// other.
	lived := statelog.Position{Stream: codeStream, Generation: 1, Seq: 95}
	for _, tc := range []struct {
		answer code.LifecycleResult
		// want is the session's floor after it, and repo the repository's own.
		want, repo statelog.Position
	}{
		{code.LifecycleResult{Refusal: code.LifecycleNotPermitted, Detail: "seat dev may not archive it",
			Repo: code.RepoRef{ID: "repo-lived", Position: past}}, changed, statelog.Position{}},
		{code.LifecycleResult{Outcome: statelog.OutcomeApplied,
			Repo: code.RepoRef{ID: "repo-lived", Position: past}}, changed, statelog.Position{}},
		{code.LifecycleResult{Outcome: statelog.OutcomeApplied,
			Repo: code.RepoRef{ID: "repo-lived", Position: lived}, Position: lived}, lived, lived},
	} {
		d.mu.Lock()
		d.lifecycle = tc.answer
		d.mu.Unlock()
		if _, err := f.client.Code().Lifecycle(t.Context(), code.LifecycleRequest{Kind: code.LifecycleArchive,
			RepoID: "repo-lived"}); err != nil {
			t.Fatalf("lifecycle: %v", err)
		}
		if got := f.client.Session().Floors([]string{codeStream}); !slices.Equal(got,
			[]statelog.Position{tc.want}) {
			t.Fatalf("after a lifecycle request answered %+v the code floor is %v, want %v", tc.answer, got,
				tc.want)
		}
		if got := f.client.Code().FloorOf("repo-lived"); got != tc.repo {
			t.Fatalf("after a lifecycle request answered %+v the repository's floor is %v, want %v",
				tc.answer, got, tc.repo)
		}
		if got := f.client.Code().FloorOf("repo-expunged"); got != expunged {
			t.Fatalf("a lifecycle request of another repository moved repo-expunged's floor to %v", got)
		}
	}
}

// A REPOSITORY'S FLOOR IS ITS OWN, AND THE NODE KEEPS A BOUNDED NUMBER: a
// write raises only its repository's floor, and never lowers it; a position
// that names nothing is ignored; and past maxRepoFloors repositories the one
// written at the lowest position is forgotten — floored at nothing again, which
// costs a read of it one sync at the log's head where its copy lacks what this
// node wrote, never a wrong answer — and no other.
//
// Mutation: observe keeping every repository, or evicting another than the
// lowest → red.
func TestARepositorysFloorIsItsOwnAndBounded(t *testing.T) {
	t.Parallel()
	at := func(seq uint64) statelog.Position {
		return statelog.Position{Stream: codeStream, Generation: 1, Seq: seq}
	}
	var f repoFloors
	f.observe("repo-a", at(5))
	f.observe("repo-a", at(3))
	f.observe("repo-b", statelog.Position{})
	f.observe("", at(9))
	if got := f.floor("repo-a"); got != at(5) {
		t.Fatalf("repo-a's floor is %v after writes at 5 and 3, want 5", got)
	}
	if got := f.floor("repo-b"); !got.IsZero() {
		t.Fatalf("a position naming nothing floored repo-b at %v", got)
	}

	// FULL: the oldest write is the one forgotten.
	for i := range maxRepoFloors - 1 {
		f.observe(fmt.Sprintf("repo-%05d", i), at(uint64(100+i)))
	}
	f.observe("repo-new", at(1_000_000))
	if got := f.floor("repo-a"); !got.IsZero() {
		t.Errorf("past %d repositories the lowest write, repo-a's at 5, is still held at %v", maxRepoFloors, got)
	}
	if got := f.floor("repo-new"); got != at(1_000_000) {
		t.Errorf("the repository written past the bound is floored at %v", got)
	}
	if got := f.floor("repo-00000"); got != at(100) {
		t.Errorf("a repository above the lowest write lost its floor: %v", got)
	}
	f.mu.Lock()
	held := len(f.at)
	f.mu.Unlock()
	if held != maxRepoFloors {
		t.Errorf("the node keeps %d repositories' floors, want its bound of %d", held, maxRepoFloors)
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
			Placement: &fakePlacement{}, Git: &fakePlacement{},
			Session: NewSession(), Timing: timing, Envelopes: fleetEnvelopes})
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

// platform is a chart of one unit, Platform, with one full_access seat, dev,
// in it: as the code domain's grant rules read a chart.
type platform struct{}

func (platform) Seat(handle string) (code.RosterSeat, bool) {
	return code.RosterSeat{Handle: "dev", Tier: code.TierFullAccess, Unit: "Platform"}, handle == "dev"
}
func (platform) Lead(string) string                { return "" }
func (platform) Leads(string) []string             { return nil }
func (platform) Within(unit, ancestor string) bool { return unit == ancestor }

// codeRows is a replicated estate whose code rows hold one repository, web —
// Platform's, `visibility: unit` — applied through the code log's position at,
// as a reader of it finds it.
func codeRows(t *testing.T, at statelog.Position) store.ReplicatedHandle {
	t.Helper()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = node.Close() })
	for _, row := range []struct {
		statement string
		args      []any
	}{
		{`INSERT INTO code_repos (id, name, status, managed_by, owner_kind, owner, visibility, default_branch,
			created_at) VALUES ('repo-web', 'web', 'active', 'config', 'unit', 'Platform', 'unit', 'main', 0)`, nil},
		{`INSERT INTO code_names (token, name, repo_id) VALUES (?, 'web', 'repo-web')`,
			[]any{code.NameToken("web")}},
		{`INSERT INTO statelog_cursor (stream, generation, seq, stream_created_at, updated_at, applied_version)
			VALUES (?, ?, ?, 1, 0, 0)`, []any{at.Stream, int64(at.Generation), int64(at.Seq)}},
	} {
		if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), row.statement, row.args...)
			return err
		}); err != nil {
			t.Fatalf("seed the code rows: %v", err)
		}
	}
	return db
}

// A SEAT'S READ ON A NODE WITH NO CHART IS ANSWERED BY A PEER (M3-O82). The
// node the walk asks first holds no chart — between its code reader's start
// and its first epoch's install — and its reader refuses the seat's read
// ABOUT ITSELF, code.ErrNotServing, which the walk reads as a node that ran
// nothing; the next node, holding a chart, answers it. Both read the same
// rows with REAL readers: what differs is the chart alone.
//
// Mutation: a reader with no chart answering AccessNone again → the first node
// answers not_permitted, a definite answer, and the walk ends there.
func TestASeatsReadOnANodeWithNoChartIsAnsweredByAPeer(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-1", "data-2")
	at := statelog.Position{Stream: codeStream, Generation: 1, Seq: 4}
	authority, err := statelogtest.LocalReader(code.Domain{}, codeRows(t, at).Reader(), at)
	if err != nil {
		t.Fatalf("the code log's read authority: %v", err)
	}
	half := func(chart code.Roster) CodeServer {
		r, err := code.NewReader(code.ReaderOptions{Log: authority,
			Roster: func() code.Roster { return chart }})
		if err != nil {
			t.Fatalf("NewReader: %v", err)
		}
		return code.Served{Reader: r}
	}
	nodes, _ := f.placement.Holders()
	order := f.client.order(opCodeRead.spec, nodes)
	chartless, charted := f.nodes[order[0]], f.nodes[order[1]]
	chartless.set(func(n *fakeNode) { n.codeHalf = half(nil) })
	charted.set(func(n *fakeNode) { n.codeHalf = half(platform{}) })

	answer, err := f.client.Code().Read(t.Context(), code.ReadQuery{
		Asker: code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}, Kind: code.ReadRepo, Repo: "web",
		Fresh: statelog.Freshness{Level: statelog.ReadStale}})
	if err != nil || answer.Repo == nil || answer.Repo.Access != code.AccessWrite.String() {
		t.Fatalf("dev's read of web through the walk = %+v, %v; want web at write, answered by "+
			"the node holding a chart", answer.Repo, err)
	}
	for _, n := range []*fakeNode{chartless, charted} {
		n.mu.Lock()
		asked := n.requests
		n.mu.Unlock()
		if asked != 1 {
			t.Errorf("%s was asked %d times, want once: the node with no chart first, then its peer",
				n.name, asked)
		}
	}
}
