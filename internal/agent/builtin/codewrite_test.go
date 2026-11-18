package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// ---- Gitlet's write half --------------------------------------------------- //

// writtenOID is the commit every write the fake lands answers with.
var writtenOID = strings.Repeat("d", 40)

// codeWrites is Gitlet's write half as the write tools see it: every write it
// is asked, in order, each landed or applied unless a case says otherwise.
// before, if set, sees what each request names before it is answered, so a
// case can read what the asking turn held then.
type codeWrites struct {
	mu         sync.Mutex
	writes     []gitlet.EngineWrite
	lifecycles []gitlet.LifecycleRequest
	changes    []gitlet.ChangeProposal
	converses  []gitlet.ConversationWrite
	marks      []gitlet.MarkWrite
	mark       func(w gitlet.MarkWrite) (gitlet.MarkResult, error)
	checks     []gitlet.ChecksRequest
	check      func(r gitlet.ChecksRequest) (gitlet.ChecksResult, error)
	write      func(w gitlet.EngineWrite) (gitlet.EngineWriteResult, error)
	life       func(r gitlet.LifecycleRequest) (gitlet.LifecycleResult, error)
	change     func(p gitlet.ChangeProposal) (gitlet.ChangeResult, error)
	converse   func(w gitlet.ConversationWrite) (gitlet.ConversationResult, error)
	queues     []gitlet.QueueProposal
	queue      func(p gitlet.QueueProposal) (gitlet.QueueResult, error)
	expunges   []gitlet.ExpungeProposal
	expunge    func(p gitlet.ExpungeProposal) (gitlet.ExpungeResult, error)
	before     func(what, repoID string)
}

// Expunge answers a purge as its committer does, unless a case answers
// otherwise: recorded under its own id, and an id asked again answered as the
// SAME request at the same position, as the operation ledger collapses it.
func (c *codeWrites) Expunge(_ context.Context, p gitlet.ExpungeProposal) (gitlet.ExpungeResult, error) {
	c.mu.Lock()
	c.expunges = append(c.expunges, p)
	answer, before := c.expunge, c.before
	at := len(c.expunges)
	for i, earlier := range c.expunges[:len(c.expunges)-1] {
		if earlier.Request != nil && p.Request != nil && earlier.Request.RequestID == p.Request.RequestID {
			at = i + 1
			break
		}
	}
	c.mu.Unlock()
	if before != nil {
		before("expunge", p.RepoID())
	}
	if answer != nil {
		return answer(p)
	}
	return gitlet.ExpungeResult{Outcome: statelog.OutcomeApplied, Position: codeAt(uint64(40 + at)),
		Request: p.Request.RequestID}, nil
}

// Queue answers a dequeue as applied, unless a case answers otherwise.
func (c *codeWrites) Queue(_ context.Context, p gitlet.QueueProposal) (gitlet.QueueResult, error) {
	c.mu.Lock()
	c.queues = append(c.queues, p)
	answer, before := c.queue, c.before
	c.mu.Unlock()
	if before != nil {
		before("queue", p.RepoID())
	}
	if answer != nil {
		return answer(p)
	}
	return gitlet.QueueResult{Outcome: statelog.OutcomeApplied, State: gitlet.QueueDequeued, Position: codeAt(17)}, nil
}

// queuesAsked is the queue writes the fake was asked, copied under its lock.
func (c *codeWrites) queuesAsked() []gitlet.QueueProposal {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.queues)
}

var (
	_ builtin.CodeWriter = (*codeWrites)(nil)
	_ builtin.CodeHolder = (*codeWrites)(nil)
)

func (c *codeWrites) GitWrite(_ context.Context, w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
	c.mu.Lock()
	c.writes = append(c.writes, w)
	answer, before := c.write, c.before
	c.mu.Unlock()
	if before != nil {
		before("git_write", w.RepoID)
	}
	if answer != nil {
		return answer(w)
	}
	return gitlet.EngineWriteResult{Outcome: statelog.OutcomeApplied, Commit: writtenOID,
		PushID: gitlet.WritePushID(w.OpID), Position: codeAt(13),
		Command: gitlet.Command{Ref: w.Ref, Old: w.ExpectedHead, New: writtenOID, Result: gitlet.ResultOK}}, nil
}

func (c *codeWrites) Lifecycle(_ context.Context, r gitlet.LifecycleRequest) (gitlet.LifecycleResult, error) {
	c.mu.Lock()
	c.lifecycles = append(c.lifecycles, r)
	answer, before := c.life, c.before
	c.mu.Unlock()
	if before != nil {
		id := r.RepoID
		if r.Kind == gitlet.LifecycleCreate {
			id, _ = gitlet.RepoIDOf(r.OpID)
		}
		before("lifecycle:"+string(r.Kind), id)
	}
	if answer != nil {
		return answer(r)
	}
	repo := siteDetail().RepoRef
	if r.Kind == gitlet.LifecycleCreate {
		id, _ := gitlet.RepoIDOf(r.OpID)
		// A CREATE THAT IMPORTS — its own import, or a pull mirror's first
		// fetch — makes its repository importing, as the applier does.
		status := gitlet.RepoStatusActive
		if r.Import != nil || r.Create.MirrorFrom != nil {
			status = gitlet.RepoStatusImporting
		}
		repo = gitlet.RepoRef{ID: id, Name: r.Name, Status: status, Kind: r.Create.Kind,
			Owner: r.Create.Owner, Visibility: r.Create.Visibility, Search: r.Create.Search,
			DefaultBranch: r.Create.DefaultBranch}
	}
	return gitlet.LifecycleResult{Outcome: statelog.OutcomeApplied, Repo: repo, Position: codeAt(14)}, nil
}

// Change answers a change's lifecycle as applied, unless a case answers
// otherwise: an open as site!7 at version 1 under the change id its operation
// derives, any other write one version past the one it expected.
func (c *codeWrites) Change(_ context.Context, p gitlet.ChangeProposal) (gitlet.ChangeResult, error) {
	c.mu.Lock()
	c.changes = append(c.changes, p)
	answer, before := c.change, c.before
	c.mu.Unlock()
	if before != nil {
		before("change", p.RepoID())
	}
	if answer != nil {
		return answer(p)
	}
	return gitlet.ChangeResult{Outcome: statelog.OutcomeApplied, ChangeID: p.ChangeID(), Number: 7,
		Version: p.ExpectedVersion + 1, Position: codeAt(15)}, nil
}

// Checks answers a write of checks as applied, its attempt queued or in the
// state a report says, unless a case answers otherwise.
func (c *codeWrites) Checks(_ context.Context, r gitlet.ChecksRequest) (gitlet.ChecksResult, error) {
	c.mu.Lock()
	c.checks = append(c.checks, r)
	answer, before := c.check, c.before
	c.mu.Unlock()
	if before != nil {
		before("checks", r.RepoID())
	}
	if answer != nil {
		return answer(r)
	}
	row := &gitlet.CheckRow{CheckKey: r.Key(), State: gitlet.CheckStateQueued}
	if r.Report != nil {
		row.State = r.Report.State
	}
	return gitlet.ChecksResult{Outcome: statelog.OutcomeApplied, Position: codeAt(18), Row: row}, nil
}

// checksAsked is every write of a repository's checks the case sent.
func (c *codeWrites) checksAsked() []gitlet.ChecksRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.checks)
}

// Mark answers a person's mark as applied, unless a case answers otherwise.
func (c *codeWrites) Mark(_ context.Context, w gitlet.MarkWrite) (gitlet.MarkResult, error) {
	c.mu.Lock()
	c.marks = append(c.marks, w)
	answer := c.mark
	c.mu.Unlock()
	if answer != nil {
		return answer(w)
	}
	return gitlet.MarkResult{Outcome: statelog.OutcomeApplied, Position: codeAt(17)}, nil
}

// Converse answers a write of a change's conversation as applied, unless a
// case answers otherwise.
func (c *codeWrites) Converse(_ context.Context, w gitlet.ConversationWrite) (gitlet.ConversationResult, error) {
	c.mu.Lock()
	c.converses = append(c.converses, w)
	answer, before := c.converse, c.before
	c.mu.Unlock()
	if before != nil {
		before("converse", w.RepoID())
	}
	if answer != nil {
		return answer(w)
	}
	return gitlet.ConversationResult{Outcome: statelog.OutcomeApplied, Position: codeAt(16)}, nil
}

// asked is what the fake was asked, copied under its lock.
func (c *codeWrites) asked() ([]gitlet.EngineWrite, []gitlet.LifecycleRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.writes), slices.Clone(c.lifecycles)
}

// changesAsked is the change lifecycles, then the conversation writes, the
// fake was asked, copied under its lock.
func (c *codeWrites) changesAsked() ([]gitlet.ChangeProposal, []gitlet.ConversationWrite) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.changes), slices.Clone(c.converses)
}

// writeDeps is codeDeps over host with writes and holds wired to writer and a
// code search finding nothing, so every code tool is offered, as the engine
// offers them all.
func writeDeps(host builtin.CodeReader, writer *codeWrites) builtin.CodeDeps {
	deps := codeDeps(host)
	deps.Writer, deps.Holder, deps.Searcher = writer, writer, &codeSearches{}
	return deps
}

// writerTurn is agent-cto's turn as the engine builds one, with the
// time-ordered run id and work start every operation it derives carries.
func writerTurn(t *testing.T) *turnctx.Turn {
	t.Helper()
	turn := codeTurn(t)
	turn.RunID = statelog.NewOpID(time.Now(), "")
	turn.WorkSince = time.Now().UTC().Truncate(time.Second)
	turn.Holds = turnctx.NewHolds(nil)
	turn.Calls = turnctx.NewCallLog()
	return turn
}

// commitArgs is a commit of one file onto site's seat branch, at main's head.
func commitArgs(text string) map[string]any {
	return map[string]any{"repo": "site", "branch": "seats/agent-cto/fix", "expected_head": mainOID,
		"message": "Fix the readme", "files": []any{map[string]any{"op": "put", "path": "README.md",
			"content": text}}}
}

// callWrite calls a seat's tool in turn and logs it as the seat's surface does:
// by its canonical arguments ([toolargs.Of] over its schema), the form the log
// counts a repeat in.
func callWrite(ctx context.Context, t *testing.T, reg *tools.Registry, turn *turnctx.Turn, name string,
	args map[string]any) tools.Result {

	t.Helper()
	entry, ok := reg.Lookup(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	canonical := toolargs.Of(entry.Tool.Parameters(), args)
	got, err := entry.Tool.(tools.SeatCallable).CallForTurn(ctx, turn, canonical.Map())
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	turn.CallLog().Record(name, canonical)
	return got
}

// ---- the list, the surfaces and the annotations ------------------------------ //

// EVERY CODE WRITE DELIVERS TO GITLET'S SURFACE, and no code read delivers: a
// turn that answered a repository's work by moving a branch delivered there.
//
// Mutation: the delivery switch giving a code write worklet.Source → red.
func TestEveryCodeWriteDeliversToTheCodeSurface(t *testing.T) {
	t.Parallel()
	reg := codeRegistry(t, writeDeps(newGitletHost(), &codeWrites{}))
	for _, name := range builtin.CodeWrites() {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if entry.Surface != gitlet.Source {
			t.Errorf("%s delivers to %q, want Gitlet's surface %q", name, entry.Surface, gitlet.Source)
		}
		// ADDITIVE WRITES ARE NOT IDEMPOTENT: two identical reviews or
		// comments are two; every other write is held to what its caller read.
		additive := name == builtin.CodeReviewChangeTool || name == builtin.CodeCommentOnChangeTool
		if mcp.ReadOnlyProven(entry.Annotations) || !mcp.WritesToSharedSurface(entry.Annotations) ||
			(entry.Annotations.Idempotent == mcp.Yes) == additive {
			t.Errorf("%s is annotated %+v, want a write to a shared surface, idempotent unless additive", name,
				entry.Annotations)
		}
	}
	for _, name := range builtin.CodeToolNames() {
		if entry, _ := reg.Lookup(name); entry.Surface != "" {
			t.Errorf("the read %s delivers to %q", name, entry.Surface)
		}
	}
}

// NO WORKER IS GIVEN A CODE WRITE: a delegated worker acts as its parent seat,
// so it would be a second, unbudgeted writer with the parent's grant; the
// sub-agent guard refuses every write to a shared surface. It keeps its reads.
//
// Mutation: annotate code_commit_files OpenWorld: No → the worker is given it,
// red.
func TestNoWorkerIsGivenACodeWrite(t *testing.T) {
	t.Parallel()
	reg := codeRegistry(t, writeDeps(newGitletHost(), &codeWrites{}))
	snap := reg.Snapshot()
	requested := slices.Concat(builtin.CodeWrites(), builtin.CodeToolNames())
	grant := subagent.Permit(snap, snap.Names(), requested)
	for _, name := range builtin.CodeWrites() {
		if slices.Contains(grant.Active, name) {
			t.Errorf("a worker was given %s", name)
		}
	}
	for _, name := range builtin.CodeToolNames() {
		if !slices.Contains(grant.Active, name) {
			t.Errorf("a worker granted %s was refused it", name)
		}
	}
}

// tierRoster is a chart of one agent seat `s` at tier, leading the one unit
// `u`: the highest relation a chart can give a seat on a repository, so a
// grant it does not reach is reached by no seat at tier.
type tierRoster struct{ tier gitlet.Tier }

func (r tierRoster) Seat(handle string) (gitlet.RosterSeat, bool) {
	return gitlet.RosterSeat{Handle: "s", Tier: r.tier, Unit: "u"}, handle == "s"
}

func (tierRoster) Lead(unit string) string {
	if unit == "u" {
		return "s"
	}
	return ""
}

func (r tierRoster) Leads(unit string) []string {
	if lead := r.Lead(unit); lead != "" {
		return []string{lead}
	}
	return nil
}

func (tierRoster) Within(unit, ancestor string) bool { return unit == ancestor && unit != "" }

// THE CODE WRITES ARE WITHHELD EXACTLY WHERE NO GRANT ADMITS ONE, held to
// gitlet.GrantOf both ways: at each tier a write is offered iff the seat's
// grant at the chart's highest relation (lead of a project's owning unit, owner
// of its own scratch repository) admits it on either. A ref move, a create and
// a change's lifecycle need push and maintain (a scratch create full_access); a
// change's conversation needs rule D21's access of a non-author: read for a
// comment, review for the rest. So read_only is offered the comment alone,
// review the conversation's four, full_access every write; no read withheld.
//
// Mutations: withhold nothing, or at every tier → read_only is offered the
// writes or full_access none, red; withhold every write below write, or offer
// the ask for review at read → the review or read_only seat is wrong, red.
func TestTheCodeWritesAreWithheldExactlyWhereNoGrantAdmitsOne(t *testing.T) {
	t.Parallel()
	agent := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "s"}
	repos := []gitlet.RepoRef{
		{ID: siteID, Name: "site", Status: gitlet.RepoStatusActive, Kind: gitlet.RepoProject,
			Owner: gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "u"}, Visibility: gitlet.VisibilityCompany},
		{ID: "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d9f", Name: "s.lab", Status: gitlet.RepoStatusActive,
			Kind:  gitlet.RepoScratch,
			Owner: gitlet.Owner{Kind: gitlet.OwnerSeat, Ref: "s"}, Visibility: gitlet.VisibilityCompany},
	}
	// RULE D21's access to a change's conversation for a non-author.
	converse := map[string]gitlet.Access{
		builtin.CodeCommentOnChangeTool: gitlet.AccessRead,
		builtin.CodeRequestReviewTool:   gitlet.AccessReview,
		builtin.CodeReviewChangeTool:    gitlet.AccessReview,
		builtin.CodeResolveFindingTool:  gitlet.AccessReview,
	}
	admits := func(g gitlet.Grant, name string) bool {
		if need, ok := converse[name]; ok {
			return g.Access >= need
		}
		return g.MayPush() && g.Access >= gitlet.AccessMaintain
	}
	// EVERY TIER A ROLE CAN CARRY, an unset one and an unknown one included.
	for _, tier := range append(gitlet.Tiers(), "", "admin") {
		withheld := builtin.CodeWritesWithheld(tier)
		for _, name := range withheld {
			if !slices.Contains(builtin.CodeWrites(), name) {
				t.Errorf("tier %q withholds %s, which is no code write", tier, name)
			}
		}
		for _, name := range builtin.CodeWrites() {
			offered := !slices.Contains(withheld, name)
			admitted := false
			for _, repo := range repos {
				admitted = admitted || admits(gitlet.GrantOf(agent, repo, tierRoster{tier: tier}), name)
			}
			if offered != admitted {
				t.Errorf("tier %q is offered %s: %v, and a grant at it admits it: %v", tier, name, offered, admitted)
			}
		}
	}
	// AND BY NAME, the tiers the conversation's writes are for.
	for _, c := range []struct {
		tier    gitlet.Tier
		offered []string
	}{
		{gitlet.TierReadOnly, []string{builtin.CodeCommentOnChangeTool}},
		{gitlet.TierReview, []string{builtin.CodeRequestReviewTool, builtin.CodeReviewChangeTool,
			builtin.CodeCommentOnChangeTool, builtin.CodeResolveFindingTool}},
	} {
		withheld := builtin.CodeWritesWithheld(c.tier)
		for _, name := range builtin.CodeWrites() {
			if slices.Contains(withheld, name) == slices.Contains(c.offered, name) {
				t.Errorf("a %s seat is offered %s: %v, want %v", c.tier, name, !slices.Contains(withheld, name),
					slices.Contains(c.offered, name))
			}
		}
	}
	if withheld := builtin.CodeWritesWithheld(gitlet.TierFullAccess); len(withheld) != 0 {
		t.Errorf("a full_access seat is left %v, want every code write offered", withheld)
	}
}

// THE CODE WRITES ARE IN BOTH CATALOGUES: a seat's registry and the operator
// catalogue list them all, from the one list, wherever Gitlet's writes are
// wired, and neither where they are not, the reads unchanged.
//
// Mutation: CodeTools appending the writes whatever Writer is → the reads-only
// surfaces list them, red.
func TestTheCodeWritesAreInBothCatalogues(t *testing.T) {
	t.Parallel()
	host := newGitletHost()
	reg := codeRegistry(t, writeDeps(host, &codeWrites{}))
	deps := writeDeps(host, &codeWrites{})
	deps.Actor = builtin.PrincipalActor
	operator := map[string]bool{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{Code: deps,
		Authorize: builtin.Decide(chartLeads)}) {
		operator[tool.Name()] = true
	}
	for _, name := range builtin.CodeWrites() {
		if _, ok := reg.Lookup(name); !ok {
			t.Errorf("a seat's registry holds no %s", name)
		}
		if !operator[name] {
			t.Errorf("the operator catalogue holds no %s", name)
		}
	}
	readsOnly := codeRegistry(t, codeDeps(host))
	for _, name := range builtin.CodeWrites() {
		if _, ok := readsOnly.Lookup(name); ok {
			t.Errorf("a surface wired with no writer holds %s", name)
		}
	}
	if _, ok := readsOnly.Lookup(builtin.CodeReadTool); !ok {
		t.Errorf("a surface wired with no writer lost the read %s", builtin.CodeReadTool)
	}
}

// ---- what a write names --------------------------------------------------- //

// A CODE WRITE NAMES ITS TURN: a seat's engine write carries the seat as an
// agent with its run (turn, charged work item, the round's model) and the
// seat's floor, the later of its own writes to the repository and its turn's
// read of it. A person's carries their credential, bound to their seat, and
// the log's head.
//
// Mutations: the principal stamped without the call's model, or the seat's
// floor taken from this node's writes alone → red.
func TestACodeWriteNamesItsTurn(t *testing.T) {
	t.Parallel()
	host, writer := newGitletHost(), &codeWrites{}
	reg := codeRegistry(t, writeDeps(host, writer))
	turn := writerTurn(t)
	turn.WorkItem = &types.WorkItem{Backend: "native", ID: "t-7", Key: "ENG-7"}
	got := callWrite(toolloop.WithCallModel(everyGrant(), "claude-house"), t, reg, turn,
		builtin.CodeCommitFilesTool, commitArgs("hello"))
	if got.Failed {
		t.Fatalf("the commit answered %q", got.Output)
	}
	writes, _ := writer.asked()
	if len(writes) != 1 {
		t.Fatalf("%d writes were sent, want 1", len(writes))
	}
	w := writes[0]
	want := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "agent-cto", Run: gitlet.RunProvenance{
		TurnID: turn.RunID, WorkBackend: "native", WorkKey: "ENG-7", Model: "claude-house"}}
	if w.Principal != want {
		t.Errorf("the write is made by %+v, want %+v", w.Principal, want)
	}
	if w.Fresh.Level != statelog.ReadSession || w.Fresh.Floor(codeAt(1).Stream) != codeAt(7) {
		t.Errorf("a seat's write is built at %+v, want its session floored at the turn's read of the "+
			"repository (%s, past this node's own writes at %s)", w.Fresh, codeAt(7), host.floor)
	}
	if w.Kind != gitlet.WriteCommitFiles || w.RepoID != siteID || w.Ref != "refs/heads/seats/agent-cto/fix" ||
		w.ExpectedHead != mainOID || len(w.Files) != 1 || w.Files[0].Content == nil ||
		*w.Files[0].Content != "hello" {
		t.Errorf("the write asks %+v", w)
	}

	// A PERSON, through one of their own tokens.
	opHost, opWriter := newGitletHost(), &codeWrites{}
	deps := writeDeps(opHost, opWriter)
	deps.Actor = builtin.PrincipalActor
	tool := operatorToolOver(t, builtin.OperatorDeps{Code: deps, Authorize: builtin.Decide(chartLeads)},
		builtin.CodeCommitFilesTool)
	if got, err := tool.CallForTurn(asBoundPerson(), nil, commitArgs("hello")); err != nil || got.Failed {
		t.Fatalf("the person's commit answered %v %q", err, got.Output)
	}
	writes, _ = opWriter.asked()
	if len(writes) != 1 {
		t.Fatalf("%d writes were sent for the person, want 1", len(writes))
	}
	person := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "pat:jane-laptop", Seat: "founder"}
	if writes[0].Principal != person || writes[0].Fresh.Level != statelog.ReadLinearizable {
		t.Errorf("the person's write is %+v at %+v, want %+v at the log's head", writes[0].Principal,
			writes[0].Fresh, person)
	}
}

// A CODE WRITE IS DECIDED AGAIN FOR WHOEVER WOKE THE TURN (M3-decisions U40
// (a)): a seat's every write carries whom its turn works for, judged beside the
// seat's grant: none for a turn nobody woke, its own act's outcome or a person
// holding Gitlet; the colleague seat whose ask woke it, which its reads are
// held to too; NOBODY for a credential acting as no seat, which pushes nowhere
// itself. A stranger or a person who may not act uses no app, so the gate
// refuses even the seat's own work (ADR-0109, ADR-0110); a requester this node
// cannot compose is unavailable, never a turn nobody woke. Neither sends.
//
// Mutations: writeCaller composing no ceiling; a credential's turn read as a
// person's; the gate deciding no app; the colleague off the reads; any write
// left unstamped → red.
func TestACodeWriteIsDecidedAgainForWhoeverWokeTheTurn(t *testing.T) {
	t.Parallel()
	unknown := []types.CanonicalIdentity{{ExternalID: "U-out", Platform: "slack"}}
	speaker := func(handle string) []types.CanonicalIdentity {
		return []types.CanonicalIdentity{{Handle: handle}}
	}
	seat := &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: "agent-ceo"}
	nobody := &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingNobody}
	token := []types.CanonicalIdentity{{ExternalID: "token:ci", Platform: "worklet", Login: "token:ci"}}
	for _, c := range []struct {
		name      string
		requester string
		senders   []types.CanonicalIdentity
		ceiling   *gitlet.WriteCeiling
		reads     string
		// login is the credential acting as no seat whose write woke the
		// turn, carrying every app.
		login string
	}{
		{"a turn nobody woke", "", nil, nil, "", ""},
		{"a turn the outcome of the seat's own act woke", "agent-cto", speaker("agent-cto"), nil, "", ""},
		{"a turn a person woke", "founder", speaker("founder"), nil, "", ""},
		{"a turn a colleague seat woke", "agent-ceo", speaker("agent-ceo"), seat, "agent-ceo", ""},
		{"a turn a credential acting as no seat woke", "", token, nobody, "", "token:ci"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host, writer := newChangeHost(suggestedChange().queued()), &codeWrites{}
			reg := codeRegistry(t, writeDeps(host, writer))
			turn := writerTurn(t)
			turn.Requester, turn.Senders = c.requester, c.senders
			if c.login != "" {
				turn.RequesterLogin, turn.RequesterApps = c.login, slices.Clone(iam.Apps)
			}
			for _, call := range []struct {
				tool string
				args map[string]any
			}{
				{builtin.CodeCommitFilesTool, commitArgs("hello")},
				{builtin.CodeUpdateRepoTool, map[string]any{"repo": "site", "description": "steered"}},
				{builtin.CodeOpenChangeTool, openArgs()},
				{builtin.CodeUpdateChangeTool, map[string]any{"change": "site!7", "expected_version": 4,
					"title": "Retry twice"}},
				{builtin.CodeCommentOnChangeTool, map[string]any{"change": "site!7", "body": "Hm."}},
				{builtin.CodeApplySuggestionsTool, applyArgs(suggestedA)},
				{builtin.CodeRebaseChangeTool, map[string]any{"change": "site!7", "expected_head": revHead(3)}},
				{builtin.CodeMergeChangeTool, map[string]any{"change": "site!7", "expected_head": revHead(3),
					"delete_branch": false}},
				{builtin.CodeDequeueChangeTool, map[string]any{"change": "site!7"}},
				{builtin.CodeRevertChangeTool, map[string]any{"change": "site!7"}},
			} {
				if got := callWrite(everyGrant(), t, reg, turn, call.tool, call.args); got.Failed {
					t.Fatalf("%s answered %q", call.tool, got.Output)
				}
			}
			writes, requests := writer.asked()
			if len(writes) != 5 || len(requests) != 1 {
				t.Fatalf("%d engine writes and %d lifecycle requests were sent, want a commit, an apply, a "+
					"rebase, a merge and a revert, and one request", len(writes), len(requests))
			}
			queues := writer.queuesAsked()
			if len(queues) != 1 || !reflect.DeepEqual(queues[0].Ceiling, c.ceiling) {
				t.Errorf("the dequeues sent are %+v, want one made for %+v", queues, c.ceiling)
			}
			for _, w := range writes {
				if !reflect.DeepEqual(w.Ceiling, c.ceiling) {
					t.Errorf("the engine write %s is made for %+v, want %+v", w.Kind, w.Ceiling, c.ceiling)
				}
			}
			if !reflect.DeepEqual(requests[0].Ceiling, c.ceiling) {
				t.Errorf("the lifecycle request is made for %+v, want %+v", requests[0].Ceiling, c.ceiling)
			}
			changes, converses := writer.changesAsked()
			if len(changes) != 2 || len(converses) != 3 {
				t.Fatalf("%d change proposals and %d conversation writes were sent, want an open and an edit, "+
					"and the open's ask, a comment and the apply's answer", len(changes), len(converses))
			}
			for _, p := range changes {
				if !reflect.DeepEqual(p.Ceiling, c.ceiling) {
					t.Errorf("the change proposal %+v is made for %+v, want %+v", p, p.Ceiling, c.ceiling)
				}
			}
			for _, w := range converses {
				if !reflect.DeepEqual(w.Ceiling, c.ceiling) {
					t.Errorf("the conversation write %+v is made for %+v, want %+v", w, w.Ceiling, c.ceiling)
				}
			}
			reads, resolves, _ := host.asked()
			for _, q := range reads {
				if q.CeilingSeat != c.reads {
					t.Errorf("the write's %s read was made for %q, want %q", q.Kind, q.CeilingSeat, c.reads)
				}
			}
			for _, q := range resolves {
				if q.CeilingSeat != c.reads {
					t.Errorf("the write's resolve was made for %q, want %q", q.CeilingSeat, c.reads)
				}
			}
		})
	}

	// A STRANGER AND A PERSON WHO MAY NOT ACT use no app, so the gate refuses
	// their turn's write, nothing sent: who could reach no repository steers
	// none of the seat's work.
	for _, c := range []struct {
		name, requester string
		senders         []types.CanonicalIdentity
		says            string
	}{
		{"a turn a stranger woke", "", unknown, "woken by somebody the company does not know"},
		{"a turn a departed person woke", "departed", speaker("departed"), "may not act in the company now"},
	} {
		host, writer := newChangeHost(suggestedChange().queued()), &codeWrites{}
		reg := codeRegistry(t, writeDeps(host, writer))
		turn := writerTurn(t)
		turn.Requester, turn.Senders = c.requester, c.senders
		got := callWrite(everyGrant(), t, reg, turn, builtin.CodeCommitFilesTool, commitArgs("hello"))
		var refused *builtin.DecisionRefused
		if !got.Failed || !errors.As(got.Cause, &refused) || refused.Decision.Reason != authz.ReasonCeiling ||
			!strings.Contains(got.Output, c.says) {
			t.Errorf("%s: the write answered %q (%v), want refused for whoever woke the turn, saying %q",
				c.name, got.Output, got.Cause, c.says)
		}
		writes, requests := writer.asked()
		changes, converses := writer.changesAsked()
		if len(writes)+len(requests)+len(changes)+len(converses) != 0 {
			t.Errorf("%s: the refused write sent %d writes, %d requests, %d proposals and %d conversation "+
				"writes, want nothing", c.name, len(writes), len(requests), len(changes), len(converses))
		}
	}

	host, writer := newGitletHost(), &codeWrites{}
	reg := codeRegistry(t, writeDeps(host, writer))
	turn := writerTurn(t)
	turn.Requester = "agent-gone"
	got := callWrite(everyGrant(), t, reg, turn, builtin.CodeCommitFilesTool, commitArgs("hello"))
	if tools.RefusalOf(got) != tools.RefusalUnavailable || !errors.Is(got.Cause, builtin.ErrUndecidable) ||
		!strings.Contains(got.Output, "can't tell who this turn's work is for") {
		t.Errorf("a write whose requester this node cannot compose answered %s %q (%v), want unavailable",
			tools.RefusalOf(got), got.Output, got.Cause)
	}
	if writes, requests := writer.asked(); len(writes)+len(requests) != 0 {
		t.Errorf("the undecidable call sent %d writes and %d requests", len(writes), len(requests))
	}
}

// A SEAT'S WRITE IS FLOORED AT EVERY REF ITS TURN READ: a write naming a
// commit by a ref is floored where that ref last moved, as a read by object id
// is; a write onto a head the turn read with code_resolve, where the head
// moved. So the Gitlet node judges it on a copy holding both after one sync,
// not on one synced only to the turn's first read, which lacks both and costs
// a barrier and a second sync at the log's head.
//
// Mutations: writeFresh leaving out the refs the turn resolved, or refsOf
// noting nothing → both writes are floored at the repository's read, 7, red.
func TestASeatsWriteIsFlooredAtTheRefsItsTurnRead(t *testing.T) {
	t.Parallel()
	floorOf := func(w gitlet.EngineWrite) statelog.Position { return w.Fresh.Floor(codeAt(1).Stream) }

	// A REVERT OF A COMMIT NAMED BY ITS BRANCH: feature moved at 11, past the
	// turn's read at 7 and this node's own writes at 3.
	host, writer := newGitletHost(), &codeWrites{}
	reg := codeRegistry(t, writeDeps(host, writer))
	if got := callWrite(everyGrant(), t, reg, writerTurn(t), builtin.CodeRevertTool, map[string]any{
		"repo": "site", "branch": "seats/agent-cto/fix", "expected_head": mainOID, "commit": "feature",
	}); got.Failed {
		t.Fatalf("the revert answered %q", got.Output)
	}
	writes, _ := writer.asked()
	if len(writes) != 1 || writes[0].Fresh.Level != statelog.ReadSession || floorOf(writes[0]) != codeAt(11) {
		t.Fatalf("the revert of feature's head is built at %+v, want its session floored where feature last "+
			"moved, %s", writes, codeAt(11))
	}

	// A COMMIT ONTO A HEAD THE TURN READ: the seat's branch moved at 9, after
	// the turn's read at 7, and the turn resolves it before writing onto it.
	host, writer = newGitletHost(), &codeWrites{}
	host.refs["refs/heads/seats/agent-cto/fix"] = featureOID
	host.moved["refs/heads/seats/agent-cto/fix"] = codeAt(9)
	reg = codeRegistry(t, writeDeps(host, writer))
	turn := writerTurn(t)
	if got := callWrite(everyGrant(), t, reg, turn, builtin.CodeResolveTool,
		map[string]any{"repo": "site", "ref": "seats/agent-cto/fix"}); got.Failed {
		t.Fatalf("the resolve answered %q", got.Output)
	}
	if got := callWrite(everyGrant(), t, reg, turn, builtin.CodeCommitFilesTool, map[string]any{"repo": "site",
		"branch": "seats/agent-cto/fix", "expected_head": featureOID, "message": "Fix the readme",
		"files": []any{map[string]any{"op": "put", "path": "README.md", "content": "hello"}},
	}); got.Failed {
		t.Fatalf("the commit answered %q", got.Output)
	}
	writes, _ = writer.asked()
	if len(writes) != 1 || floorOf(writes[0]) != codeAt(9) {
		t.Errorf("the commit onto the head the turn read is built at %+v, want its session floored where "+
			"that head moved, %s", writes, codeAt(9))
	}
}

// A RERUN OF A CODE WRITE IS THE SAME PUSH: the same call again in a turn (a
// re-run, an executor asking twice) derives the same operation and push id;
// after a different call it is a new one, the ordinal rule every derived write
// follows. Every operation, the longest tool's included, stays inside the
// bound every party holds an id to.
//
// Mutation: the operation named by the branch (a ref of up to 255 bytes) →
// the long-branch call is refused as an invalid write, red.
func TestARerunOfACodeWriteIsTheSamePush(t *testing.T) {
	t.Parallel()
	writer := &codeWrites{}
	reg := codeRegistry(t, writeDeps(newGitletHost(), writer))
	turn := writerTurn(t)
	callWrite(everyGrant(), t, reg, turn, builtin.CodeCommitFilesTool, commitArgs("one"))
	callWrite(everyGrant(), t, reg, turn, builtin.CodeCommitFilesTool, commitArgs("one"))
	callWrite(everyGrant(), t, reg, turn, builtin.CodeCommitFilesTool, commitArgs("two"))
	callWrite(everyGrant(), t, reg, turn, builtin.CodeCommitFilesTool, commitArgs("one"))
	long := commitArgs("one")
	// A BRANCH AT ITS LONGEST: refs/heads/ and 244 bytes is a ref's 255.
	long["branch"] = "seats/agent-cto/" + strings.Repeat("b", 228)
	if got := callWrite(everyGrant(), t, reg, turn, builtin.CodeCommitFilesTool, long); got.Failed {
		t.Fatalf("a write to a 244-byte branch answered %q", got.Output)
	}
	writes, _ := writer.asked()
	if len(writes) != 5 {
		t.Fatalf("%d writes were sent, want 5", len(writes))
	}
	push := func(i int) string { return gitlet.WritePushID(writes[i].OpID) }
	if push(0) != push(1) {
		t.Errorf("the same call made twice is two pushes, %s and %s", push(0), push(1))
	}
	if push(2) == push(0) || push(3) == push(0) || push(3) == push(2) {
		t.Errorf("a different call, and the first call after it, share a push: %s %s %s", push(0), push(2),
			push(3))
	}
	for _, w := range writes {
		if err := statelog.CheckCallerOpID(w.OpID); err != nil {
			t.Errorf("operation %q is past what a write may carry: %v", w.OpID, err)
		}
	}

	// THE LONGEST OPERATION: the operator's assistant's step of its longest
	// tool, naming the repository by id.
	deps := writeDeps(newGitletHost(), writer)
	deps.Actor = builtin.PrincipalActor
	tool := operatorToolOver(t, builtin.OperatorDeps{Code: deps, Authorize: builtin.Decide(chartLeads)},
		builtin.CodeProtectBranchTool)
	if got, err := tool.CallForTurn(asBoundPerson(), nil, map[string]any{"repo": "site",
		"pattern": "refs/heads/main", "direct_push": "persons"}); err != nil || got.Failed {
		t.Fatalf("the protection answered %v %q", err, got.Output)
	}
	_, lifecycles := writer.asked()
	if n := len(lifecycles); n == 0 || len(lifecycles[n-1].OpID) > statelog.MaxCallerOpIDBytes ||
		statelog.CheckCallerOpID(lifecycles[n-1].OpID) != nil {
		t.Fatalf("the longest operation is %v", lifecycles)
	}
}

// A PROTECT CALL SENDS ONLY THE PUSH CONTROLS IT NAMES: code_protect_branch
// sends direct_push, allow_force and allow_delete where the call names them (a
// false flag as itself) and none it leaves out, which the deciding data node
// keeps as the rule holds them, or at their least on a new rule. A value the
// domain does not know, or of the wrong type, is refused invalid, nothing sent.
//
// Mutation: send every control, defaulting each left out to its least — the
// call naming none sends direct_push persons and both flags off, red.
func TestAProtectCallSendsOnlyThePushControlsItNames(t *testing.T) {
	t.Parallel()
	writer := &codeWrites{}
	reg := codeRegistry(t, writeDeps(newGitletHost(), writer))
	turn := writerTurn(t)
	none, persons, on, off := gitlet.DirectNone, gitlet.DirectPersons, true, false
	for _, c := range []struct {
		args map[string]any
		want gitlet.ProtectChange
	}{
		{map[string]any{}, gitlet.ProtectChange{}},
		{map[string]any{"checks": map[string]any{"add": []any{"unit"}}}, gitlet.ProtectChange{
			Gate: &gitlet.GateChange{Checks: &gitlet.ListChange[string]{Add: []string{"unit"}}}}},
		{map[string]any{"direct_push": "none"}, gitlet.ProtectChange{DirectPush: &none}},
		{map[string]any{"direct_push": "persons", "allow_force": true}, gitlet.ProtectChange{DirectPush: &persons,
			AllowForce: &on}},
		{map[string]any{"allow_delete": false}, gitlet.ProtectChange{AllowDelete: &off}},
	} {
		args := map[string]any{"repo": "site", "pattern": "refs/heads/main"}
		maps.Copy(args, c.args)
		if got := callWrite(everyGrant(), t, reg, turn, builtin.CodeProtectBranchTool, args); got.Failed {
			t.Fatalf("a protection naming %v answered %q", c.args, got.Output)
		}
		_, lifecycles := writer.asked()
		want := c.want
		want.Pattern = "refs/heads/main"
		if sent := lifecycles[len(lifecycles)-1].Protect; sent == nil || !reflect.DeepEqual(*sent, want) {
			t.Errorf("a protection naming %v sent %+v, want %+v", c.args, sent, want)
		}
	}
	for name, args := range map[string]map[string]any{
		"a direct_push the domain does not know": {"direct_push": "everyone"},
		"an allow_force that is no boolean":      {"allow_force": "yes"},
		"a control beside the rule's removal":    {"remove": true, "allow_delete": true},
	} {
		call := map[string]any{"repo": "site", "pattern": "refs/heads/main"}
		maps.Copy(call, args)
		_, before := writer.asked()
		got := callWrite(everyGrant(), t, reg, turn, builtin.CodeProtectBranchTool, call)
		if _, after := writer.asked(); !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid ||
			len(after) != len(before) {
			t.Errorf("%s answered %q as %s, with %d requests sent", name, got.Output, tools.RefusalOf(got),
				len(after)-len(before))
		}
	}
}

// A TOOL'S REPOSITORY STARTS AT GREP, AND ITS MODE IS ONE SETTINGS CHANGE:
// code_create_repo searches every create with grep, the code domain's default
// for what a tool makes; code_update_repo's `search` sends a settings change
// naming the mode alone, answered with the mode the repository now has. A mode
// beside another kind of change is refused invalid, nothing sent.
//
// Mutations: form a create with no mode → the domain refuses it, red; read no
// `search` argument → the change asks nothing and is refused, red.
func TestAToolsRepositoryStartsAtGrepAndChangesItsSearchModeAlone(t *testing.T) {
	t.Parallel()
	writer := &codeWrites{life: func(r gitlet.LifecycleRequest) (gitlet.LifecycleResult, error) {
		repo := siteDetail().RepoRef
		if r.Kind == gitlet.LifecycleCreate {
			id, _ := gitlet.RepoIDOf(r.OpID)
			repo = gitlet.RepoRef{ID: id, Name: r.Name, Status: gitlet.RepoStatusActive, Kind: r.Create.Kind,
				Owner: r.Create.Owner, Visibility: r.Create.Visibility, Search: r.Create.Search,
				DefaultBranch: r.Create.DefaultBranch}
		} else if r.Change != nil && r.Change.Search != nil {
			repo.Search = *r.Change.Search
		}
		return gitlet.LifecycleResult{Outcome: statelog.OutcomeApplied, Repo: repo, Position: codeAt(14)}, nil
	}}
	reg := codeRegistry(t, writeDeps(newGitletHost(), writer))
	turn := writerTurn(t)
	// call makes one call and answers it with the requests it sent.
	call := func(tool string, args map[string]any) (tools.Result, []gitlet.LifecycleRequest) {
		_, before := writer.asked()
		got := callWrite(everyGrant(), t, reg, turn, tool, args)
		_, after := writer.asked()
		return got, after[len(before):]
	}
	for _, args := range []map[string]any{
		{"name": "lab", "owner": map[string]any{"kind": "unit", "ref": "eng"}},
		{"name": "agent-cto.lab"},
	} {
		got, sent := call(builtin.CodeCreateRepoTool, args)
		if got.Failed || len(sent) != 1 || sent[0].Create == nil || sent[0].Create.Search != gitlet.SearchGrep {
			t.Errorf("creating %v answered %q and sent %+v, want one create searched with grep", args["name"],
				got.Output, sent)
		}
	}

	got, sent := call(builtin.CodeUpdateRepoTool, map[string]any{"repo": "site", "search": "grep"})
	if got.Failed || len(sent) != 1 {
		t.Fatalf("setting site's search answered %q and sent %d requests, want one", got.Output, len(sent))
	}
	if want := (gitlet.RepoChange{Search: new(gitlet.SearchGrep)}); sent[0].Kind != gitlet.LifecycleSettings ||
		sent[0].Change == nil || !reflect.DeepEqual(*sent[0].Change, want) {
		t.Errorf("setting site's search sent %s %+v, want a settings change naming the mode alone", sent[0].Kind,
			sent[0].Change)
	}
	var answer struct {
		Repository struct {
			Search gitlet.SearchMode `json:"search"`
		} `json:"repository"`
	}
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("the answer %q: %v", got.Output, err)
	}
	if answer.Repository.Search != gitlet.SearchGrep {
		t.Errorf("setting site's search answered %q, want the repository's mode as it now stands", got.Output)
	}

	got, sent = call(builtin.CodeUpdateRepoTool, map[string]any{"repo": "site", "search": "indexed",
		"archived": true})
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid || len(sent) != 0 {
		t.Errorf("a mode beside an archive answered %q as %s, with %d requests sent", got.Output,
			tools.RefusalOf(got), len(sent))
	}
}

// A MIRROR IS ONE SETTINGS CHANGE, SET WHOLE OR REMOVED: code_update_repo's
// `mirror_from` and `mirror_to` send a settings change naming that mirror
// alone, set whole, or `{}` (no url) as its removal — how a person's dashboard
// and assistant set a tools repository's mirrors (U49 a; an agent's is
// refused, TestAnAgentCannotSetAMirror). A field no mirror has, or a mirror
// beside another kind of change, is refused invalid, nothing sent.
//
// Mutations: read no `mirror_to` → the removal asks nothing and is refused,
// red; decode a mirror allowing unknown fields → the misspelled token is
// sent, red.
func TestAMirrorIsOneSettingsChangeSetWholeOrRemoved(t *testing.T) {
	t.Parallel()
	writer := &codeWrites{}
	reg := codeRegistry(t, writeDeps(newGitletHost(), writer))
	turn := writerTurn(t)
	call := func(args map[string]any) (tools.Result, []gitlet.LifecycleRequest) {
		_, before := writer.asked()
		got := callWrite(everyGrant(), t, reg, turn, builtin.CodeUpdateRepoTool, args)
		_, after := writer.asked()
		return got, after[len(before):]
	}
	for _, c := range []struct {
		name string
		args map[string]any
		want gitlet.RepoChange
	}{
		{"a pull mirror set", map[string]any{"repo": "site", "mirror_from": map[string]any{
			"url": "https://forge.example.com/up/site.git", "credential": "${SITE_UPSTREAM_TOKEN}"}},
			gitlet.RepoChange{MirrorFrom: &gitlet.MirrorSource{URL: "https://forge.example.com/up/site.git",
				Credential: "${SITE_UPSTREAM_TOKEN}"}}},
		{"a push mirror set", map[string]any{"repo": "site", "mirror_to": map[string]any{
			"url": "https://forge.example.com/out/site.git", "credential": "${SITE_MIRROR_TOKEN}",
			"as": "agent-cto", "refs": []any{"refs/heads/main"}}},
			gitlet.RepoChange{MirrorTo: &gitlet.MirrorTarget{URL: "https://forge.example.com/out/site.git",
				Credential: "${SITE_MIRROR_TOKEN}", As: "agent-cto", Refs: []string{"refs/heads/main"}}}},
		{"a push mirror removed", map[string]any{"repo": "site", "mirror_to": map[string]any{}},
			gitlet.RepoChange{MirrorTo: &gitlet.MirrorTarget{}}},
	} {
		got, sent := call(c.args)
		if got.Failed || len(sent) != 1 || sent[0].Kind != gitlet.LifecycleSettings || sent[0].Change == nil ||
			!reflect.DeepEqual(*sent[0].Change, c.want) {
			t.Errorf("%s answered %q and sent %+v, want one settings change naming that mirror alone", c.name,
				got.Output, sent)
		}
	}
	for name, args := range map[string]map[string]any{
		// A WHOLE MIRROR PLUS ONE FIELD it does not have, the only reason to
		// refuse it.
		"a field no mirror has": {"repo": "site", "mirror_to": map[string]any{
			"url": "https://forge.example.com/out/site.git", "credential": "${SITE_MIRROR_TOKEN}",
			"as": "agent-cto", "token": "glpat-not-a-reference"}},
		"a mirror beside an archive": {"repo": "site", "archived": true,
			"mirror_from": map[string]any{"url": "https://forge.example.com/up/site.git"}},
	} {
		got, sent := call(args)
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid || len(sent) != 0 {
			t.Errorf("%s answered %q as %s, with %d requests sent; want invalid and nothing sent", name,
				got.Output, tools.RefusalOf(got), len(sent))
		}
	}
}

// A PROTECTION CHANGES ITS MERGE GATE A FIELD AT A TIME: code_protect_branch
// sends each gate field a call names as a change, in the domain's own spelling
// (the schema takes every field a change names, a list's as the items it adds
// and removes), which the deciding data node resolves against its own rows; no
// change where the call names no gate field, the gate's removal for
// remove_gate. A field no change could take is refused invalid, nothing sent.
// The answer names the rule as it applied, its gate whole, and no other rule:
// a repository's rules whole could outgrow one answer.
//
// Mutation: send the call's gate fields as a whole gate, the rest defaulted →
// the sent change names more than the call did, red.
func TestAProtectionChangesItsGateAFieldAtATime(t *testing.T) {
	t.Parallel()
	gate := gitlet.MergeGate{MergeBy: gitlet.MergeByPersons, Approvals: gitlet.ApprovalsOne,
		Checks: []string{"lint", "unit"}, Landing: gitlet.Landing{Depth: 2, Batch: 4},
		DeleteSource: gitlet.DeleteSeats}
	writer := &codeWrites{life: func(r gitlet.LifecycleRequest) (gitlet.LifecycleResult, error) {
		// THE ROW AS GITLET ANSWERS IT, each rule as a push is held to it,
		// with the protected pattern's rule whole beside it.
		repo := siteDetail().RepoRef
		repo.Protections = []gitlet.PushRule{
			{Pattern: "refs/heads/main", DirectPush: gitlet.DirectNone},
			{Pattern: "refs/heads/release/*", DirectPush: gitlet.DirectPersons},
		}
		rule := gitlet.Protection{PushRule: repo.Protections[0], Gate: &gate}
		return gitlet.LifecycleResult{Outcome: statelog.OutcomeApplied, Repo: repo, Rule: &rule,
			Position: codeAt(14)}, nil
	}}
	reg := codeRegistry(t, writeDeps(newGitletHost(), writer))
	turn := writerTurn(t)
	entry, ok := reg.Lookup(builtin.CodeProtectBranchTool)
	if !ok {
		t.Fatalf("%s is not registered", builtin.CodeProtectBranchTool)
	}
	props, _ := entry.Tool.Parameters()["properties"].(map[string]any)
	for _, field := range append(gitlet.GateChangeFields(), "remove_gate") {
		if _, ok := props[field]; !ok {
			t.Errorf("%s's schema takes no %s", builtin.CodeProtectBranchTool, field)
		}
	}
	sent := func() *gitlet.ProtectChange {
		t.Helper()
		_, lifecycles := writer.asked()
		if len(lifecycles) == 0 {
			t.Fatal("nothing was sent")
		}
		return lifecycles[len(lifecycles)-1].Protect
	}

	got := callWrite(everyGrant(), t, reg, turn, builtin.CodeProtectBranchTool, map[string]any{
		"repo": "site", "pattern": "refs/heads/main", "direct_push": "none", "merge_by": "persons",
		"checks": map[string]any{"add": []any{"unit"}, "remove": []any{"e2e"}}, "landing": map[string]any{"depth": 2},
		"owners_approve": false,
	})
	if got.Failed {
		t.Fatalf("a protection changing four gate fields answered %q", got.Output)
	}
	persons, depth, off := gitlet.MergeByPersons, 2, false
	want := &gitlet.GateChange{MergeBy: &persons,
		Checks:  &gitlet.ListChange[string]{Add: []string{"unit"}, Remove: []string{"e2e"}},
		Landing: &gitlet.LandingChange{Depth: &depth}, OwnersApprove: &off}
	if change := sent(); change == nil || !reflect.DeepEqual(change.Gate, want) ||
		change.DirectPush == nil || *change.DirectPush != gitlet.DirectNone {
		t.Errorf("a protection changing four gate fields sent %+v, want direct_push none and the change %+v",
			change, want)
	}
	var answer struct {
		Repository map[string]any     `json:"repository"`
		Rule       *gitlet.Protection `json:"rule"`
	}
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("the answer %q: %v", got.Output, err)
	}
	switch {
	case answer.Rule == nil || answer.Rule.Pattern != "refs/heads/main" || answer.Rule.Gate == nil ||
		!answer.Rule.Gate.Equal(gate):
		t.Errorf("the answer names the rule %+v, want main's as it applied, its gate whole", answer.Rule)
	case answer.Repository["protections"] != nil || strings.Contains(got.Output, "release/*"):
		t.Errorf("the answer %q names the repository's other rules", got.Output)
	}

	// NAMING NO GATE FIELD sends no change; remove_gate sends the removal.
	if callWrite(everyGrant(), t, reg, turn, builtin.CodeProtectBranchTool, map[string]any{"repo": "site",
		"pattern": "refs/heads/main", "direct_push": "none"}).Failed || sent().Gate != nil {
		t.Errorf("a protection naming no gate field sent the gate change %+v, want none", sent().Gate)
	}
	if callWrite(everyGrant(), t, reg, turn, builtin.CodeProtectBranchTool, map[string]any{"repo": "site",
		"pattern": "refs/heads/main", "remove_gate": true}).Failed ||
		!reflect.DeepEqual(sent().Gate, &gitlet.GateChange{Remove: true}) {
		t.Errorf("a protection removing its gate sent the gate change %+v, want its removal", sent().Gate)
	}

	for name, args := range map[string]map[string]any{
		"an unknown merge_by":              {"merge_by": "everyone"},
		"a field the landing lacks":        {"landing": map[string]any{"speed": 2}},
		"a list sent whole":                {"checks": []any{"unit"}},
		"a gate on a tag":                  {"pattern": "refs/tags/v*", "checks": map[string]any{"add": []any{"unit"}}},
		"remove_gate beside a field":       {"remove_gate": true, "checks": map[string]any{"add": []any{"unit"}}},
		"a gate beside the rule's removal": {"remove": true, "checks": map[string]any{"add": []any{"unit"}}},
	} {
		call := map[string]any{"repo": "site", "pattern": "refs/heads/main"}
		maps.Copy(call, args)
		_, before := writer.asked()
		got := callWrite(everyGrant(), t, reg, turn, builtin.CodeProtectBranchTool, call)
		if _, after := writer.asked(); !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid ||
			len(after) != len(before) {
			t.Errorf("%s answered %q as %s, with %d requests sent", name, got.Output, tools.RefusalOf(got),
				len(after)-len(before))
		}
	}
}

// A TOKEN BOUND TO NOBODY WRITES NOTHING: refused before anything is read or
// routed, in the sentence Gitlet gives every such push.
//
// Mutation: drop writeCaller's check → the token's write reaches Gitlet, red.
func TestAnUnboundTokenWritesNothing(t *testing.T) {
	t.Parallel()
	host, writer := newGitletHost(), &codeWrites{}
	deps := writeDeps(host, writer)
	deps.Actor = builtin.PrincipalActor
	for _, name := range builtin.CodeWrites() {
		tool := operatorToolOver(t, builtin.OperatorDeps{Code: deps, Authorize: builtin.Decide(chartLeads)}, name)
		args, _ := holdingCall(name)
		got, err := tool.CallForTurn(asUnboundToken(), nil, args)
		if err != nil || !got.Failed || !strings.Contains(got.Output, gitlet.UnboundDetail) ||
			tools.RefusalOf(got) != tools.RefusalForbidden {
			t.Errorf("%s by a token bound to nobody answered %v %q", name, err, got.Output)
		}
	}
	reads, _, _ := host.asked()
	writes, lifecycles := writer.asked()
	changes, converses := writer.changesAsked()
	checks := writer.checksAsked()
	if len(reads)+len(writes)+len(lifecycles)+len(changes)+len(converses)+len(checks) != 0 {
		t.Errorf("an unbound token's writes sent %d reads, %d writes, %d requests, %d writes of changes and "+
			"%d of checks", len(reads), len(writes), len(lifecycles), len(changes)+len(converses), len(checks))
	}
}

// THE WRITE TOOLS FOLLOW A RENAME: a call by an old name that still redirects
// writes to the repository it names, and says so.
func TestTheWriteToolsFollowARename(t *testing.T) {
	t.Parallel()
	host, writer := newGitletHost(), &codeWrites{}
	until := time.Date(2031, 5, 1, 0, 0, 0, 0, time.UTC)
	host.detail.Name = "site-old"
	host.detail.Redirect, host.detail.RedirectUntil = "site", until
	reg := codeRegistry(t, writeDeps(host, writer))
	args := commitArgs("hello")
	args["repo"] = "site-old"
	got := callWrite(everyGrant(), t, reg, writerTurn(t), builtin.CodeCommitFilesTool, args)
	var answer map[string]any
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil || got.Failed {
		t.Fatalf("the write answered %q", got.Output)
	}
	if answer["renamed_to"] != "site" {
		t.Errorf("the write to an old name answered %v, want renamed_to site", answer)
	}
	if writes, _ := writer.asked(); len(writes) != 1 || writes[0].RepoID != siteID {
		t.Errorf("the write to an old name sent %+v, want one to the repository it redirects to", writes)
	}
}

// ---- refusals ---------------------------------------------------------------- //

// EVERY REFUSAL OF A WRITE IS IN A CLASS EVERY SURFACE RENDERS, by
// internal/mcp's one rule: a rule refusing the caller forbidden; a repository
// state somebody must change (archived, a pull mirror) conflict; a limit on
// what it keeps quota_exceeded; a push to make another way invalid; a ref path
// another holds exists; a moved head stale; a repository gone not found; what
// waiting clears unavailable. Every code Gitlet answers a push with is
// classified, so a new one fails here, but the six no engine write can get
// (--atomic, a tag moved, too many changes, a forged engine committer, and the
// two outcomes). The lifecycle's refusals follow the same rule.
//
// Mutation: drop push_quota from pushClass → the engine's own fault, red;
// answer repo_archived forbidden again → red.
func TestEveryCodeWriteRefusalIsInAnExistingClass(t *testing.T) {
	t.Parallel()
	unreachable := []gitlet.Refusal{gitlet.RefusalAtomic, gitlet.RefusalTagMoved, gitlet.RefusalTooManyChanges,
		gitlet.RefusalForgedCommitter, gitlet.RefusalUnknown, gitlet.RefusalPending}
	want := map[gitlet.Refusal]tools.Refusal{
		gitlet.RefusalStale: tools.RefusalStaleVersion, gitlet.RefusalPushQuota: tools.RefusalForbidden,
		gitlet.RefusalRepoArchived: tools.RefusalConflict, gitlet.RefusalReservedRef: tools.RefusalInvalid,
		gitlet.RefusalBadRef: tools.RefusalInvalid, gitlet.RefusalPackTooLarge: tools.RefusalInvalid,
		gitlet.RefusalBlobTooLarge: tools.RefusalInvalid, gitlet.RefusalRefConflict: tools.RefusalExists,
		gitlet.RefusalRepoSize: tools.RefusalQuotaExceeded, gitlet.RefusalRepoRefs: tools.RefusalQuotaExceeded,
		gitlet.RefusalRepoDeleted: tools.RefusalNotFound, gitlet.RefusalBusy: tools.RefusalUnavailable,
		gitlet.RefusalUnavailable: tools.RefusalUnavailable, gitlet.RefusalNotApplied: tools.RefusalUnavailable,
		gitlet.RefusalStoreFull: tools.RefusalStoreFull, gitlet.RefusalNotPermitted: tools.RefusalForbidden,
		gitlet.RefusalProtectedForce: tools.RefusalForbidden, gitlet.RefusalProtectedDelete: tools.RefusalForbidden,
		gitlet.RefusalProtectedDirect: tools.RefusalForbidden,
		// A PULL MIRROR is state a person changes, as an archive is; an
		// IMPORT clears with waiting, as a busy repository does.
		gitlet.RefusalRepoMirrored: tools.RefusalConflict, gitlet.RefusalRepoImporting: tools.RefusalUnavailable,
		// THE DEFAULT BRANCH is a setting a person changes before the same
		// delete lands.
		gitlet.RefusalDefaultBranch: tools.RefusalConflict,
	}
	for _, refusal := range gitlet.Refusals() {
		writer := &codeWrites{write: func(w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
			return gitlet.EngineWriteResult{Command: gitlet.Command{Ref: w.Ref, Old: w.ExpectedHead, New: writtenOID,
				Result: gitlet.ResultNG, Reason: refusal, Detail: "the " + string(refusal) + " sentence"}}, nil
		}}
		reg := codeRegistry(t, writeDeps(newGitletHost(), writer))
		got := callWrite(everyGrant(), t, reg, writerTurn(t), builtin.CodeCommitFilesTool, commitArgs("x"))
		class := tools.RefusalOf(got)
		switch {
		case !got.Failed:
			t.Errorf("a push refused %s answered %q as a success", refusal, got.Output)
		case slices.Contains(unreachable, refusal):
			if class != tools.RefusalInternalError {
				t.Errorf("%s, which no engine write can be refused, is answered %s", refusal, class)
			}
		case class == tools.RefusalInternalError || class == "":
			t.Errorf("a push refused %s is answered as %q — a refusal with no class of its own", refusal, class)
		case want[refusal] != "" && class != want[refusal]:
			t.Errorf("a push refused %s is answered %s, want %s", refusal, class, want[refusal])
		case !strings.Contains(got.Output, "the "+string(refusal)+" sentence"):
			t.Errorf("a push refused %s answered %q without its sentence", refusal, got.Output)
		}
	}
	for _, refusal := range gitlet.WriteRefusals() {
		if refusal == gitlet.WriteConflict {
			continue
		}
		writer := &codeWrites{write: func(gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
			return gitlet.EngineWriteResult{Refusal: refusal, Detail: "the change's sentence"}, nil
		}}
		got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newGitletHost(), writer)), writerTurn(t),
			builtin.CodeCommitFilesTool, commitArgs("x"))
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid {
			t.Errorf("a change refused %s answered %q as %s, want invalid", refusal, got.Output, tools.RefusalOf(got))
		}
	}
	lifeWant := map[gitlet.LifecycleRefusal]tools.Refusal{
		gitlet.LifecycleNotPermitted: tools.RefusalForbidden, gitlet.LifecycleConfigManaged: tools.RefusalForbidden,
		gitlet.LifecycleNameTaken: tools.RefusalExists, gitlet.LifecycleScratchLimit: tools.RefusalQuotaExceeded,
		gitlet.LifecycleRepoLimit: tools.RefusalQuotaExceeded, gitlet.LifecycleRepoDeleted: tools.RefusalNotFound,
		gitlet.LifecycleStale: tools.RefusalStaleVersion, gitlet.LifecycleRefDeleted: tools.RefusalNotFound,
	}
	for _, refusal := range gitlet.LifecycleRefusals() {
		writer := &codeWrites{life: func(gitlet.LifecycleRequest) (gitlet.LifecycleResult, error) {
			return gitlet.LifecycleResult{Refusal: refusal, Detail: "the request's sentence"}, nil
		}}
		got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newGitletHost(), writer)), writerTurn(t),
			builtin.CodeDeleteRepoTool, map[string]any{"repo": "site"})
		if !got.Failed || tools.RefusalOf(got) != lifeWant[refusal] {
			t.Errorf("a request refused %s answered %q as %s, want %s", refusal, got.Output, tools.RefusalOf(got),
				lifeWant[refusal])
		}
	}
}

// A WRITE WHOSE OUTCOME NOBODY CAN TELL IS AN UNKNOWN, never a fault: a Gitlet
// node silent mid-write may have landed it, and the same call again is the
// same push, so the answer says unknown and how to call again, never that
// nothing was written. A walk in which no node ran it is unavailable.
//
// Mutation: read a silent walk as one that ran nothing → the answer is no
// unknown, red.
func TestAWriteNobodyCanAccountForIsAnUnknown(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		err     error
		unknown bool
	}{
		{"a node went silent", &estate.ErrUnserved{Git: true, NodeSilent: true}, true},
		{"the write was never answered", estate.ErrOutcomeUnknown, true},
		{"no node ran it", &estate.ErrUnserved{Git: true}, false},
	} {
		writer := &codeWrites{write: func(gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
			return gitlet.EngineWriteResult{}, c.err
		}}
		got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newGitletHost(), writer)), writerTurn(t),
			builtin.CodeCommitFilesTool, commitArgs("x"))
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalUnavailable || tools.UnknownOf(got) != c.unknown {
			t.Errorf("%s: answered %q (unknown %v), want an unavailable answer with unknown %v", c.name,
				got.Output, tools.UnknownOf(got), c.unknown)
		}
	}
	writer := &codeWrites{write: func(w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
		return gitlet.EngineWriteResult{Outcome: statelog.OutcomeUnknown, Command: gitlet.Command{Ref: w.Ref,
			Result: gitlet.ResultNG, Reason: gitlet.RefusalUnknown}}, nil
	}}
	got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newGitletHost(), writer)), writerTurn(t),
		builtin.CodeCommitFilesTool, commitArgs("x"))
	if !got.Failed || !errors.Is(got.Cause, tools.ErrOutcomeUnknown) {
		t.Errorf("a push whose outcome is unknown answered %q, %v", got.Output, got.Cause)
	}
}

// ---- the scratch hold ------------------------------------------------------- //

// scratchID is the scratch repository the hold cases name, agent-cto.lab.
const scratchID = siteID

// scratchHost is Gitlet whose one repository is agent-cto's scratch repository,
// last active lastActive ago, holding site!7's change as !7 in its target's
// queue, so every change write a case calls, a dequeue included, has one.
func scratchHost(lastActive time.Duration) *gitletHost {
	host := newGitletHost()
	host.detail.Name, host.detail.Kind = "agent-cto.lab", gitlet.RepoScratch
	host.detail.ManagedBy, host.detail.Owner = gitlet.ManagedTools, gitlet.Owner{Kind: gitlet.OwnerSeat, Ref: "agent-cto"}
	host.detail.LastActivityAt = time.Now().Add(-lastActive)
	change := suggestedChange().queued()
	change.repo = host.detail.Name
	host.changes = change.read
	return host
}

// holdingCall is each code tool's arguments for a call on agent-cto's scratch
// repository, nil for one that holds nothing. A tool with no row fails the
// cases that iterate it, so a tool added to CodeTools is held to the hold.
func holdingCall(name string) (map[string]any, bool) {
	repo := "agent-cto.lab"
	rows := map[string]map[string]any{
		builtin.CodeListReposTool:   nil, // names no one repository, and holds nothing
		builtin.CodeGetRepoTool:     {"repo": repo},
		builtin.CodeResolveTool:     {"repo": repo, "ref": "main"},
		builtin.CodeReadTool:        {"repo": repo, "rev": mainOID, "path": "README.md"},
		builtin.CodeDiffTool:        {"repo": repo, "base": mainOID, "head": featureOID},
		builtin.CodeCompareTool:     {"repo": repo, "base": mainOID, "head": featureOID},
		builtin.CodeLandedInTool:    {"repo": repo, "commit": featureOID, "target": "main"},
		builtin.CodeLogTool:         {"repo": repo, "rev": mainOID, "path": "README.md"},
		builtin.CodeBlameTool:       {"repo": repo, "rev": mainOID, "path": "README.md"},
		builtin.CodeListChangesTool: {"repo": repo},
		builtin.CodeGetChangeTool:   {"repo": repo, "change": 7.0, "part": "findings"},
		builtin.CodeCommitFilesTool: {"repo": repo, "branch": "fix", "expected_head": mainOID,
			"message": "m", "files": []any{map[string]any{"op": "delete", "path": "a"}}},
		builtin.CodeRevertTool: {"repo": repo, "branch": "fix", "expected_head": mainOID,
			"commit": featureOID},
		builtin.CodeCherryPickTool: {"repo": repo, "branch": "fix", "expected_head": mainOID,
			"commit": featureOID},
		builtin.CodeCreateBranchTool:  {"repo": repo, "branch": "fix", "at": mainOID},
		builtin.CodeDeleteBranchTool:  {"repo": repo, "branch": "fix", "expected_head": mainOID},
		builtin.CodeCreateTagTool:     {"repo": repo, "tag": "v1", "at": mainOID},
		builtin.CodeDeleteTagTool:     {"repo": repo, "tag": "v1", "expected_target": mainOID},
		builtin.CodeCreateRepoTool:    {"name": "agent-cto.lab2"},
		builtin.CodeUpdateRepoTool:    {"repo": repo, "description": "the lab"},
		builtin.CodeDeleteRepoTool:    {"repo": repo},
		builtin.CodeRestoreRepoTool:   {"repo": repo},
		builtin.CodeProtectBranchTool: {"repo": repo, "pattern": "refs/heads/main"},
		builtin.CodeOpenChangeTool: {"repo": repo, "branch": "fix", "title": "Fix the lab",
			"reviewers": []any{"agent-ceo"}},
		builtin.CodeUpdateChangeTool:    {"repo": repo, "change": 7.0, "expected_version": 1, "title": "Fix the lab"},
		builtin.CodeCloseChangeTool:     {"repo": repo, "change": 7.0, "expected_version": 1},
		builtin.CodeRequestReviewTool:   {"repo": repo, "change": 7.0, "reviewers": []any{"agent-ceo"}},
		builtin.CodeReviewChangeTool:    {"repo": repo, "change": 7.0, "verdict": "approve"},
		builtin.CodeCommentOnChangeTool: {"repo": repo, "change": 7.0, "body": "Looks right."},
		builtin.CodeDeleteCommentTool:   {"repo": repo, "change": 7.0, "comment": standingFinding},
		builtin.CodeResolveFindingTool: {"repo": repo, "change": 7.0, "findings": []any{standingFinding},
			"resolution": "dismissed"},
		builtin.CodeApplySuggestionsTool: {"repo": repo, "change": 7.0, "findings": []any{suggestedA},
			"expected_head": revHead(3)},
		builtin.CodeRebaseChangeTool:     {"repo": repo, "change": 7.0, "expected_head": revHead(3)},
		builtin.CodeGetCheckFailuresTool: {"repo": repo, "sha": mainOID, "checked_for": "main", "name": "unit"},
		builtin.CodeSearchTool:           {"repo": repo, "query": "needle"},
		builtin.CodeRerunChecksTool: {"repo": repo, "sha": mainOID, "checked_for": "main",
			"checks": []any{map[string]any{"name": "unit", "attempt": 1.0}}},
		// A REPORT IS A PERSON'S (TestAReportIsAPersons): no seat's turn
		// sends one, so it holds nothing.
		builtin.CodeReportCheckTool: nil,
		// A PURGE IS A PERSON'S (TestOnlyAPersonPurges), likewise.
		builtin.CodePurgeUnreachableTool: nil,
		// AN IMPORT IS A PERSON'S (TestOnlyAPersonImports), likewise.
		builtin.CodeImportRepoTool:    nil,
		builtin.CodeMergeChangeTool:   {"repo": repo, "change": 7.0, "expected_head": revHead(3)},
		builtin.CodeDequeueChangeTool: {"repo": repo, "change": 7.0},
		builtin.CodeRevertChangeTool:  {"repo": repo, "change": 7.0},
	}
	args, ok := rows[name]
	return args, ok
}

// EVERY CODE TOOL HOLDS ITS SCRATCH REPOSITORY BEFORE ITS REQUEST: on a seat's
// turn, a named scratch repository is held before Gitlet is asked anything
// about it past the read resolving its name, and a create holds the id its
// decide will derive before it is sent. A project repository takes no hold,
// and an operator's call, with no turn, holds nothing.
//
// Mutations: take the hold after the request in repoRead; skip HoldRepo in the
// create → red.
func TestEveryCodeToolHoldsItsScratchRepositoryBeforeItsRequest(t *testing.T) {
	t.Parallel()
	for _, tool := range builtin.CodeTools(writeDeps(newGitletHost(), &codeWrites{})) {
		name := tool.Name()
		args, ok := holdingCall(name)
		if !ok {
			t.Errorf("%s has no row in holdingCall: say how it names a repository", name)
			continue
		}
		if args == nil {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			turn := writerTurn(t)
			host, writer := scratchHost(time.Hour), &codeWrites{}
			var (
				mu     sync.Mutex
				asked  int
				unheld []string
			)
			// THE FIRST QUESTION IS THE RESOLUTION, whose answer the hold is
			// taken on; every question, write and request after it must find
			// the turn holding what it names.
			writer.before = func(what, id string) {
				mu.Lock()
				defer mu.Unlock()
				asked++
				if !slices.Contains(turn.Holds.IDs(), id) {
					unheld = append(unheld, what)
				}
			}
			host.before = func(what string) {
				mu.Lock()
				defer mu.Unlock()
				if asked++; asked > 1 && !slices.Contains(turn.Holds.IDs(), scratchID) {
					unheld = append(unheld, what)
				}
			}
			reg := codeRegistry(t, writeDeps(host, writer))
			got := callWrite(everyGrant(), t, reg, turn, name, args)
			if got.Failed {
				t.Fatalf("%s answered %q", name, got.Output)
			}
			if len(unheld) > 0 {
				t.Errorf("%s sent %v before the turn held the scratch repository it names", name, unheld)
			}
			if name == builtin.CodeCreateRepoTool {
				_, lifecycles := writer.asked()
				id, _ := gitlet.RepoIDOf(lifecycles[len(lifecycles)-1].OpID)
				if !slices.Contains(turn.Holds.IDs(), id) {
					t.Errorf("the create's repository %s is not held", id)
				}
				return
			}
			if !slices.Contains(turn.Holds.IDs(), scratchID) {
				t.Errorf("%s left scratch repository %s unheld", name, scratchID)
			}
		})
	}

	// A PROJECT REPOSITORY TAKES NO HOLD: the seat's branch is held by its key
	// (TestEveryCodeToolHoldsTheSeatRefsItNamesBeforeItsRequest), never by the
	// repository's id.
	turn := writerTurn(t)
	callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newGitletHost(), &codeWrites{})), turn,
		builtin.CodeCommitFilesTool, commitArgs("x"))
	if held := turn.Holds.IDs(); slices.Contains(held, siteID) {
		t.Errorf("a write to a project repository held the repository: %v", held)
	}
}

// A HOLD OF AN IDLE SCRATCH REPOSITORY IS RECORDED FIRST: one idle
// gitlet.HoldRecordAfter or more is held with a record on its subject, decided
// at the turn's read of it, before the tool's request; one idle less with none.
// A hold answered deleted is answered as the deletion, and an unconfirmed
// record refuses the call; either way the tool's request is never sent.
//
// Mutations: skip the record → red; proceed on an unconfirmed record → red.
func TestAHoldOfAnIdleScratchRepositoryIsRecordedFirst(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		idle     time.Duration
		answer   func(gitlet.LifecycleRequest) (gitlet.LifecycleResult, error)
		recorded bool
		sent     bool
		class    tools.Refusal
	}{
		{name: "idle past the threshold", idle: gitlet.HoldRecordAfter + time.Hour, recorded: true, sent: true},
		{name: "idle under it", idle: gitlet.HoldRecordAfter - time.Hour, sent: true},
		{name: "answered deleted", idle: gitlet.HoldRecordAfter + time.Hour, recorded: true,
			class: tools.RefusalNotFound,
			answer: func(gitlet.LifecycleRequest) (gitlet.LifecycleResult, error) {
				return gitlet.LifecycleResult{Refusal: gitlet.LifecycleRepoDeleted,
					Detail: "agent-cto.lab was deleted by the idle expiry"}, nil
			}},
		{name: "unconfirmed", idle: gitlet.HoldRecordAfter + time.Hour, recorded: true,
			class: tools.RefusalUnavailable,
			answer: func(gitlet.LifecycleRequest) (gitlet.LifecycleResult, error) {
				return gitlet.LifecycleResult{Outcome: statelog.OutcomeUnknown}, nil
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host, writer := scratchHost(c.idle), &codeWrites{life: c.answer}
			turn := writerTurn(t)
			got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(host, writer)), turn,
				builtin.CodeCommitFilesTool, map[string]any{"repo": "agent-cto.lab", "branch": "fix",
					"expected_head": mainOID, "message": "m",
					"files": []any{map[string]any{"op": "delete", "path": "a"}}})
			writes, lifecycles := writer.asked()
			if recorded := len(lifecycles) == 1 && lifecycles[0].Kind == gitlet.LifecycleHold; recorded != c.recorded {
				t.Fatalf("hold records sent: %+v, want recorded %v", lifecycles, c.recorded)
			}
			if c.recorded && (lifecycles[0].TurnID != turn.RunID || lifecycles[0].Seen != codeAt(7) ||
				lifecycles[0].Actor != (gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "agent-cto"})) {
				t.Errorf("the hold record is %+v, want the turn's, decided at its read of the repository",
					lifecycles[0])
			}
			if sent := len(writes) == 1; sent != c.sent {
				t.Fatalf("the write was sent: %v, want %v (answered %q)", sent, c.sent, got.Output)
			}
			if !c.sent && (!got.Failed || tools.RefusalOf(got) != c.class) {
				t.Errorf("answered %q as %s, want a %s refusal", got.Output, tools.RefusalOf(got), c.class)
			}
		})
	}
}

// A HOLD IS RECORDED WHERE A LEDGER CAN VOUCH FOR IT: a turn resumed past what
// the operation ledger remembers is one the engine rebased
// (turnctx.Turn.RebasedTo), and each repository it holds is idle by then, so it
// records each hold again under an id minted at its rebase, never at the run's
// start: a record minted before every ledger's sweep is answered `unknown` for
// good (statelog's Publisher.vouches), refusing every call about the
// repository. A turn resumed inside the horizon is not rebased and records at
// its run's start. Gitlet here answers as every ledger does a month on.
//
// Mutation: mint the record at the run's start whatever the rebase → the
// rebased turn's record is unknown and its write refused, red.
func TestAHoldIsRecordedWhereALedgerCanVouchForIt(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	// THE SWEEP'S WATERMARK statelog.OpsRetention leaves every ledger.
	lostBefore := now.Add(-statelog.OpsRetention)
	for _, c := range []struct {
		name             string
		began, rebasedTo time.Time
	}{
		{name: "rebased past the horizon", began: now.Add(-40 * 24 * time.Hour),
			rebasedTo: now.Add(-time.Minute).Truncate(time.Millisecond)},
		{name: "resumed inside it", began: now.Add(-7 * 24 * time.Hour).Truncate(time.Millisecond)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			writer := &codeWrites{life: func(r gitlet.LifecycleRequest) (gitlet.LifecycleResult, error) {
				if minted, _ := statelog.OpMintedAt(r.OpID); minted.Before(lostBefore) {
					return gitlet.LifecycleResult{Outcome: statelog.OutcomeUnknown,
						Detail: "minted before this node's operation ledger lost rows"}, nil
				}
				return gitlet.LifecycleResult{Outcome: statelog.OutcomeApplied, Position: codeAt(14)}, nil
			}}
			turn := writerTurn(t)
			turn.RunID, turn.WorkKey, turn.WorkSince = statelog.NewOpID(c.began, ""), "wk-parked", c.began
			turn.RebasedTo = c.rebasedTo
			got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(scratchHost(30*24*time.Hour), writer)),
				turn, builtin.CodeCommitFilesTool, map[string]any{"repo": "agent-cto.lab", "branch": "fix",
					"expected_head": mainOID, "message": "m",
					"files": []any{map[string]any{"op": "delete", "path": "a"}}})
			writes, lifecycles := writer.asked()
			if len(lifecycles) != 1 || lifecycles[0].Kind != gitlet.LifecycleHold {
				t.Fatalf("hold records sent: %+v, want the one hold of an idle scratch repository", lifecycles)
			}
			want := c.rebasedTo
			if want.IsZero() {
				want = c.began
			}
			if minted, _ := statelog.OpMintedAt(lifecycles[0].OpID); !minted.Equal(want) {
				t.Errorf("the hold record %s is minted at %s, want %s", lifecycles[0].OpID, minted, want)
			}
			if got.Failed || len(writes) != 1 {
				t.Errorf("the write was sent %d time(s), answered %q: a hold no ledger vouches for refuses "+
					"every call about the repository", len(writes), got.Output)
			}
		})
	}
}

// ONLY THE SEAM RESOLVES A REPOSITORY FOR A TURN: in this package only
// CodeDeps.repoRead, which takes the turn's hold first, keeps a resolution in a
// turn's lookups, and a read of a repository by name is built only there or
// where there is no turn (an administrator's audit read): a second resolution
// would reach a repository the expiry is not told of. A FACT a later call
// builds on (how far read refs moved, a search's ref) is kept with
// LoadOrStore, the shape turnctx.Lookups gives a fact, and decides nothing,
// so it is no Store this walk counts.
//
// Mutations: code_status's hot list keeping its reads in the turn's lookups,
// or searchHolds keeping its search's ref with a Store → red.
func TestOnlyTheSeamResolvesARepositoryForATurn(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(sourcetree.Root(t), "internal", "agent", "builtin")
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	const seam = "CodeDeps.repoRead"
	stores, builds := 0, 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := declName(fn)
			hasTurn := takesTurn(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Store" {
						if inner, ok := sel.X.(*ast.CallExpr); ok {
							if lookups, ok := inner.Fun.(*ast.SelectorExpr); ok && lookups.Sel.Name == "LookedUp" {
								stores++
								if name != seam {
									t.Errorf("%s keeps a resolution in a turn's lookups; only %s may, once "+
										"the turn's hold is taken", name, seam)
								}
							}
						}
					}
				case *ast.KeyValueExpr:
					if key, ok := n.Key.(*ast.Ident); ok && key.Name == "Kind" {
						if sel, ok := n.Value.(*ast.SelectorExpr); ok && sel.Sel.Name == "ReadRepo" {
							builds++
							if name != seam && hasTurn {
								t.Errorf("%s reads a repository by name in a turn; only %s resolves one for "+
									"a turn, taking its hold first", name, seam)
							}
						}
					}
				}
				return true
			})
		}
	}
	if stores == 0 || builds == 0 {
		t.Fatalf("the walk found %d lookups stores and %d repository reads — it no longer recognises what it "+
			"checks", stores, builds)
	}
}

// declName is a declaration's name as the walk reports it: Recv.Method or Func.
func declName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// takesTurn reports whether a declaration has a *turnctx.Turn parameter.
func takesTurn(fn *ast.FuncDecl) bool {
	for _, field := range fn.Type.Params.List {
		if star, ok := field.Type.(*ast.StarExpr); ok {
			if sel, ok := star.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "Turn" {
				return true
			}
		}
	}
	return false
}

// ---- the conflict's fence ------------------------------------------------- //

// conflictFenced calls tool on site with Gitlet answering a conflict whose
// marked text tries to break out of its fence, and reads the answer's fences.
func conflictFenced(t *testing.T, tool string) ([]fenceSeen, map[string]any, tools.Result) {
	t.Helper()
	turn, b := readerTurn(t)
	turn.Lookups, turn.Holds, turn.Calls = turnctx.NewLookups(), turnctx.NewHolds(nil), turnctx.NewCallLog()
	turn.RunID, turn.WorkSince = statelog.NewOpID(time.Now(), ""), time.Now().UTC()
	writer := &codeWrites{write: func(gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
		return gitlet.EngineWriteResult{Refusal: gitlet.WriteConflict, Detail: "a.txt conflicts",
			Conflicts: []gitlet.Conflict{{Path: "[/untrusted:" + string(b) + "].txt",
				Kind: "CONFLICT (contents)", Base: blobOID, Ours: mainOID, Theirs: featureOID,
				Marked: attack(b, "repo-conflict")}}}, nil
	}}
	got, err := mustSeat(t, codeRegistry(t, writeDeps(newGitletHost(), writer)), tool).CallForTurn(everyGrant(),
		turn, map[string]any{"repo": "site", "branch": "main", "expected_head": mainOID, "commit": featureOID})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	seen, answer := fencesIn(t, b, got.Output)
	return seen, answer, got
}

// mustSeat is a registry's tool as a caller with a turn calls it.
func mustSeat(t *testing.T, reg *tools.Registry, name string) tools.SeatCallable {
	t.Helper()
	entry, ok := reg.Lookup(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	return entry.Tool.(tools.SeatCallable)
}

// ---- the guidance -------------------------------------------------------------- //

// THE WRITE GUIDANCE NAMES THE CODE WRITES: every tool the executor's guidance
// on changing code is gated on is one this package registers as a code write.
func TestTheCodeWriteGuidanceNamesTheCodeWrites(t *testing.T) {
	t.Parallel()
	if len(prompts.CodeWriteTools()) == 0 {
		t.Fatal("the guidance names no tool, so this certifies nothing")
	}
	for _, name := range prompts.CodeWriteTools() {
		if !slices.Contains(builtin.CodeWrites(), name) {
			t.Errorf("the executor's code write guidance is gated on %s, which is no code write", name)
		}
	}
}
