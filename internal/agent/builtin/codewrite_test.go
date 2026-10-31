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
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// ---- a code host that takes writes ----------------------------------------- //

// writtenOID is the commit every write the fake lands answers with.
var writtenOID = strings.Repeat("d", 40)

// codeWrites is the code host's write half as the write tools see it: every
// engine write, lifecycle request and write of a change it is asked, in order,
// and the answer each is given — a write landed and a request applied unless a
// case says otherwise. before, set, is called with what each request names
// before it is answered, so a case can read what the asking turn held then.
type codeWrites struct {
	mu         sync.Mutex
	writes     []code.EngineWrite
	lifecycles []code.LifecycleRequest
	changes    []code.ChangeProposal
	converses  []code.ConversationWrite
	marks      []code.MarkWrite
	write      func(w code.EngineWrite) (code.EngineWriteResult, error)
	life       func(r code.LifecycleRequest) (code.LifecycleResult, error)
	change     func(p code.ChangeProposal) (code.ChangeResult, error)
	converse   func(w code.ConversationWrite) (code.ConversationResult, error)
	before     func(what, repoID string)
}

var (
	_ builtin.CodeWriter = (*codeWrites)(nil)
	_ builtin.CodeHolder = (*codeWrites)(nil)
)

func (c *codeWrites) GitWrite(_ context.Context, w code.EngineWrite) (code.EngineWriteResult, error) {
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
	return code.EngineWriteResult{Outcome: statelog.OutcomeApplied, Commit: writtenOID,
		PushID: code.WritePushID(w.OpID), Position: codeAt(13),
		Command: code.Command{Ref: w.Ref, Old: w.ExpectedHead, New: writtenOID, Result: code.ResultOK}}, nil
}

func (c *codeWrites) Lifecycle(_ context.Context, r code.LifecycleRequest) (code.LifecycleResult, error) {
	c.mu.Lock()
	c.lifecycles = append(c.lifecycles, r)
	answer, before := c.life, c.before
	c.mu.Unlock()
	if before != nil {
		id := r.RepoID
		if r.Kind == code.LifecycleCreate {
			id, _ = code.RepoIDOf(r.OpID)
		}
		before("lifecycle:"+string(r.Kind), id)
	}
	if answer != nil {
		return answer(r)
	}
	repo := siteDetail().RepoRef
	if r.Kind == code.LifecycleCreate {
		id, _ := code.RepoIDOf(r.OpID)
		repo = code.RepoRef{ID: id, Name: r.Name, Status: code.RepoStatusActive, Kind: r.Create.Kind,
			Owner: r.Create.Owner, Visibility: r.Create.Visibility, Search: r.Create.Search,
			DefaultBranch: r.Create.DefaultBranch}
	}
	return code.LifecycleResult{Outcome: statelog.OutcomeApplied, Repo: repo, Position: codeAt(14)}, nil
}

// Change answers a change's lifecycle as applied — an open as change site!7 at
// version 1, opened under the change id its operation derives; any other write
// as the change it names, one version past the one it expected — unless a case
// answers otherwise.
func (c *codeWrites) Change(_ context.Context, p code.ChangeProposal) (code.ChangeResult, error) {
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
	return code.ChangeResult{Outcome: statelog.OutcomeApplied, ChangeID: p.ChangeID(), Number: 7,
		Version: p.ExpectedVersion + 1, Position: codeAt(15)}, nil
}

// Mark answers a person's mark as applied.
func (c *codeWrites) Mark(_ context.Context, w code.MarkWrite) (code.MarkResult, error) {
	c.mu.Lock()
	c.marks = append(c.marks, w)
	c.mu.Unlock()
	return code.MarkResult{Outcome: statelog.OutcomeApplied, Position: codeAt(17)}, nil
}

// Converse answers a write of a change's conversation as applied, unless a
// case answers otherwise.
func (c *codeWrites) Converse(_ context.Context, w code.ConversationWrite) (code.ConversationResult, error) {
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
	return code.ConversationResult{Outcome: statelog.OutcomeApplied, Position: codeAt(16)}, nil
}

// asked is what the fake was asked, copied under its lock.
func (c *codeWrites) asked() ([]code.EngineWrite, []code.LifecycleRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.writes), slices.Clone(c.lifecycles)
}

// changesAsked is the writes of changes the fake was asked, copied under its
// lock: their lifecycles, then their conversations.
func (c *codeWrites) changesAsked() ([]code.ChangeProposal, []code.ConversationWrite) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.changes), slices.Clone(c.converses)
}

// writeDeps is codeDeps over host, with the code host's writes and the turn's
// holds wired to writer.
func writeDeps(host builtin.CodeReader, writer *codeWrites) builtin.CodeDeps {
	deps := codeDeps(host)
	deps.Writer, deps.Holder = writer, writer
	return deps
}

// writerTurn is agent-cto's turn as the engine builds one: its lookups, its
// holds, its call log, a time-ordered run id and the instant its work began —
// what every operation it derives carries.
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

// callWrite calls a seat's write tool in turn under ctx, recording the call in
// the turn's log as the tool loop does.
func callWrite(t *testing.T, ctx context.Context, reg *tools.Registry, turn *turnctx.Turn, name string,
	args map[string]any) tools.Result {

	t.Helper()
	entry, ok := reg.Lookup(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	got, err := entry.Tool.(tools.SeatCallable).CallForTurn(ctx, turn, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	turn.CallLog().Record(name, args)
	return got
}

// ---- the list, the surfaces and the annotations ------------------------------ //

// EVERY CODE WRITE DELIVERS TO THE CODE HOST'S SURFACE, and no code read
// delivers anywhere: a turn woken about work on a repository that answered by
// moving a branch is a turn that delivered, on the surface the work came from.
//
// Mutation: the delivery switch giving a code write tracker.Source → red.
func TestEveryCodeWriteDeliversToTheCodeSurface(t *testing.T) {
	t.Parallel()
	reg := codeRegistry(t, writeDeps(newCodeHost(), &codeWrites{}))
	for _, name := range builtin.CodeWrites() {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if entry.Surface != code.Source {
			t.Errorf("%s delivers to %q, want the code host's surface %q", name, entry.Surface, code.Source)
		}
		// ADDITIVE WRITES ARE NOT IDEMPOTENT: two reviews or two comments
		// saying the same are two; every other write is held to what its
		// caller read.
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
// so a write tool on its surface would be a second, unbudgeted writer with the
// parent's grant — the sub-agent guard refuses every write to a shared
// surface, and the code writes are each one. The reads it was granted it
// keeps.
//
// Mutation: annotate code_commit_files OpenWorld: No → the worker is given it,
// red.
func TestNoWorkerIsGivenACodeWrite(t *testing.T) {
	t.Parallel()
	reg := codeRegistry(t, writeDeps(newCodeHost(), &codeWrites{}))
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

// tierRoster is a chart of one agent seat, `s`, at tier, leading the one unit
// `u`: the highest relation the chart can give a seat on a repository — the
// effective lead of the unit owning it, or the owner of its own scratch
// repository — so a grant it does not reach is reached by no seat at tier.
type tierRoster struct{ tier code.Tier }

func (r tierRoster) Seat(handle string) (code.RosterSeat, bool) {
	return code.RosterSeat{Handle: "s", Tier: r.tier, Unit: "u"}, handle == "s"
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

// THE CODE WRITES ARE WITHHELD EXACTLY WHERE NO GRANT ADMITS ONE: a seat is
// left the eighteen at every tier that caps it below write and offered them at
// full_access — held to code.GrantOf both ways at the highest relation the
// chart can give a seat, on a project its unit owns and leads and on its own
// scratch repository. A tier they are withheld at pushes to neither and
// maintains neither (every lifecycle request a tool sends needs maintain, a
// scratch repository's create full_access), so its every call of one would be
// refused; a tier they are offered at does both. The reads are never
// withheld.
//
// Mutations: withhold nothing → read_only's grant pushes nothing and is
// offered the writes, red; withhold at every tier → full_access's maintains
// and is left none, red.
func TestTheCodeWritesAreWithheldExactlyWhereNoGrantAdmitsOne(t *testing.T) {
	t.Parallel()
	agent := code.Principal{Kind: code.PrincipalAgent, Seat: "s"}
	repos := []code.RepoRef{
		{ID: siteID, Name: "site", Status: code.RepoStatusActive, Kind: code.RepoProject,
			Owner: code.Owner{Kind: code.OwnerUnit, Ref: "u"}, Visibility: code.VisibilityCompany},
		{ID: "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d9f", Name: "s.lab", Status: code.RepoStatusActive,
			Kind:  code.RepoScratch,
			Owner: code.Owner{Kind: code.OwnerSeat, Ref: "s"}, Visibility: code.VisibilityCompany},
	}
	// EVERY TIER A ROLE CAN CARRY, an unset one and one no build writes among
	// them.
	for _, tier := range append(code.Tiers(), "", "admin") {
		withheld := builtin.CodeWritesWithheld(tier)
		if len(withheld) != 0 && !slices.Equal(withheld, builtin.CodeWrites()) {
			t.Errorf("tier %q withholds %v, want all eighteen writes or none", tier, withheld)
		}
		for _, repo := range repos {
			g := code.GrantOf(agent, repo, tierRoster{tier: tier})
			admits := g.MayPush() && g.Access >= code.AccessMaintain
			if admits == (len(withheld) != 0) {
				t.Errorf("tier %q on %s: its grant pushes %v and holds %s, and the writes are withheld: %v",
					tier, repo.Name, g.MayPush(), g.Access, len(withheld) != 0)
			}
		}
		if len(withheld) == 0 && tier != code.TierFullAccess {
			t.Errorf("tier %q is offered the writes, and only full_access creates a scratch repository", tier)
		}
	}
}

// THE CODE WRITES ARE IN BOTH CATALOGUES: a seat's registry and the operator
// catalogue both list the ten, from the one list, wherever the code host's
// writes are wired — and neither lists one where they are not, the reads
// staying as they were.
//
// Mutation: CodeTools appending the writes whatever Writer is → the reads-only
// surfaces list them, red.
func TestTheCodeWritesAreInBothCatalogues(t *testing.T) {
	t.Parallel()
	host := newCodeHost()
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

// A CODE WRITE NAMES ITS TURN: the engine write a seat's call sends carries the
// seat as an agent with the run behind it — its turn, the work item it is
// charged to and the model its round ran on — and the seat's floor: its own
// writes to the repository and its turn's read of it, whichever is later. A
// person's carries the credential they came through, bound to their seat, and
// the log's head.
//
// Mutations: the principal stamped without the call's model, or the seat's
// floor taken from this node's writes alone → red.
func TestACodeWriteNamesItsTurn(t *testing.T) {
	t.Parallel()
	host, writer := newCodeHost(), &codeWrites{}
	reg := codeRegistry(t, writeDeps(host, writer))
	turn := writerTurn(t)
	turn.WorkItem = &types.WorkItem{Backend: "native", ID: "t-7", Key: "ENG-7"}
	got := callWrite(t, toolloop.WithCallModel(everyGrant(), "claude-house"), reg, turn,
		builtin.CodeCommitFilesTool, commitArgs("hello"))
	if got.Failed {
		t.Fatalf("the commit answered %q", got.Output)
	}
	writes, _ := writer.asked()
	if len(writes) != 1 {
		t.Fatalf("%d writes were sent, want 1", len(writes))
	}
	w := writes[0]
	want := code.Principal{Kind: code.PrincipalAgent, Seat: "agent-cto", Run: code.RunProvenance{
		TurnID: turn.RunID, WorkBackend: "native", WorkKey: "ENG-7", Model: "claude-house"}}
	if w.Principal != want {
		t.Errorf("the write is made by %+v, want %+v", w.Principal, want)
	}
	if w.Fresh.Level != statelog.ReadSession || w.Fresh.Floor(codeAt(1).Stream) != codeAt(7) {
		t.Errorf("a seat's write is built at %+v, want its session floored at the turn's read of the "+
			"repository (%s, past this node's own writes at %s)", w.Fresh, codeAt(7), host.floor)
	}
	if w.Kind != code.WriteCommitFiles || w.RepoID != siteID || w.Ref != "refs/heads/seats/agent-cto/fix" ||
		w.ExpectedHead != mainOID || len(w.Files) != 1 || w.Files[0].Content == nil ||
		*w.Files[0].Content != "hello" {
		t.Errorf("the write asks %+v", w)
	}

	// A PERSON, through one of their own tokens.
	opHost, opWriter := newCodeHost(), &codeWrites{}
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
	person := code.Principal{Kind: code.PrincipalOperator, OperatorID: "pat:jane-laptop", Seat: "founder"}
	if writes[0].Principal != person || writes[0].Fresh.Level != statelog.ReadLinearizable {
		t.Errorf("the person's write is %+v at %+v, want %+v at the log's head", writes[0].Principal,
			writes[0].Fresh, person)
	}
}

// A CODE WRITE IS DECIDED AGAIN FOR WHOEVER WOKE THE TURN (M3-decisions U40
// (a)): the engine writes, the lifecycle request and a change's writes — an
// open, its ask for review, an edit, a comment, a suggestions apply and its
// findings' answer, and a rebase — a seat's call sends carry
// whom its turn works for, for the git node and the data node that decide them
// to judge beside the seat's own grant — none for a turn nobody woke, the
// outcome of its own act or a person (who maintains every repository); the
// colleague seat whose ask woke it, which every read the call makes is held
// to as well; and NOBODY for a stranger or a person who may not act in the
// company now, whose turn is still sent, since the seat's own work stays
// theirs to steer and only the deciding node reads whether that is what it
// is. A requester this node cannot compose is answered unavailable with
// nothing sent, never as a turn nobody woke. A person's own write carries
// none.
//
// Mutations: writeCaller composing no ceiling → the colleague's and the
// stranger's writes carry none, red; the ceiling's refusal answered as a
// refusal rather than nobody's ceiling → the stranger's write is refused
// before it is sent, red; the colleague left off the call's reads → red; the
// lifecycle request, a change proposal, a conversation write or a rebase's
// engine write left unstamped → red.
func TestACodeWriteIsDecidedAgainForWhoeverWokeTheTurn(t *testing.T) {
	t.Parallel()
	unknown := []types.CanonicalIdentity{{ExternalID: "U-out", Platform: "slack"}}
	speaker := func(handle string) []types.CanonicalIdentity {
		return []types.CanonicalIdentity{{Handle: handle}}
	}
	seat := &code.WriteCeiling{Kind: code.WriteCeilingSeat, Seat: "agent-ceo"}
	nobody := &code.WriteCeiling{Kind: code.WriteCeilingNobody}
	for _, c := range []struct {
		name      string
		requester string
		senders   []types.CanonicalIdentity
		ceiling   *code.WriteCeiling
		reads     string
	}{
		{"a turn nobody woke", "", nil, nil, ""},
		{"a turn the outcome of the seat's own act woke", "agent-cto", speaker("agent-cto"), nil, ""},
		{"a turn a person woke", "founder", speaker("founder"), nil, ""},
		{"a turn a colleague seat woke", "agent-ceo", speaker("agent-ceo"), seat, "agent-ceo"},
		{"a turn a stranger woke", "", unknown, nobody, ""},
		{"a turn a departed person woke", "departed", speaker("departed"), nobody, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host, writer := newChangeHost(suggestedChange()), &codeWrites{}
			reg := codeRegistry(t, writeDeps(host, writer))
			turn := writerTurn(t)
			turn.Requester, turn.Senders = c.requester, c.senders
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
			} {
				if got := callWrite(t, everyGrant(), reg, turn, call.tool, call.args); got.Failed {
					t.Fatalf("%s answered %q", call.tool, got.Output)
				}
			}
			writes, requests := writer.asked()
			if len(writes) != 3 || len(requests) != 1 {
				t.Fatalf("%d engine writes and %d lifecycle requests were sent, want a commit, an apply and a "+
					"rebase, and one request", len(writes), len(requests))
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

	host, writer := newCodeHost(), &codeWrites{}
	reg := codeRegistry(t, writeDeps(host, writer))
	turn := writerTurn(t)
	turn.Requester = "agent-gone"
	got := callWrite(t, everyGrant(), reg, turn, builtin.CodeCommitFilesTool, commitArgs("hello"))
	if tools.RefusalOf(got) != tools.RefusalUnavailable || !errors.Is(got.Cause, builtin.ErrUndecidable) ||
		!strings.Contains(got.Output, "could not establish who this turn's work is for") {
		t.Errorf("a write whose requester this node cannot compose answered %s %q (%v), want unavailable",
			tools.RefusalOf(got), got.Output, got.Cause)
	}
	if writes, requests := writer.asked(); len(writes)+len(requests) != 0 {
		t.Errorf("the undecidable call sent %d writes and %d requests", len(writes), len(requests))
	}
}

// A SEAT'S WRITE IS FLOORED AT EVERY REF ITS TURN READ: a write naming a
// commit by a ref is floored at where that ref last moved, as a read by object
// id is; and a write onto a head the turn read with code_resolve — a branch
// that moved after the turn first read the repository — at where the head
// moved. So the git node judges it on a copy that holds the commit and the
// head, in one sync, rather than on one synced only to the turn's first read,
// which lacks both and is judged again at the log's head: a barrier and a
// second sync, and for the head a data node's read of the write's own outcome
// before them.
//
// Mutations: writeFresh leaving out the refs the turn resolved → both writes
// are floored at the repository's read, 7, red; refsOf noting nothing → the
// same, red.
func TestASeatsWriteIsFlooredAtTheRefsItsTurnRead(t *testing.T) {
	t.Parallel()
	floorOf := func(w code.EngineWrite) statelog.Position { return w.Fresh.Floor(codeAt(1).Stream) }

	// A REVERT OF A COMMIT NAMED BY ITS BRANCH: feature last moved at 11,
	// past the turn's read of the repository at 7 and this node's own writes
	// at 3.
	host, writer := newCodeHost(), &codeWrites{}
	reg := codeRegistry(t, writeDeps(host, writer))
	if got := callWrite(t, everyGrant(), reg, writerTurn(t), builtin.CodeRevertTool, map[string]any{
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
	// the turn's read of the repository at 7, and the turn reads its head
	// before it writes onto it.
	host, writer = newCodeHost(), &codeWrites{}
	host.refs["refs/heads/seats/agent-cto/fix"] = featureOID
	host.moved["refs/heads/seats/agent-cto/fix"] = codeAt(9)
	reg = codeRegistry(t, writeDeps(host, writer))
	turn := writerTurn(t)
	if got := callWrite(t, everyGrant(), reg, turn, builtin.CodeResolveTool,
		map[string]any{"repo": "site", "ref": "seats/agent-cto/fix"}); got.Failed {
		t.Fatalf("the resolve answered %q", got.Output)
	}
	if got := callWrite(t, everyGrant(), reg, turn, builtin.CodeCommitFilesTool, map[string]any{"repo": "site",
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

// A RERUN OF A CODE WRITE IS THE SAME PUSH: the same call made again in a turn
// — a re-run, an executor asking twice — derives the same operation and so
// the same push id; the same call after a different one is a new operation,
// the ordinal rule every derived write follows; and every operation stays
// inside the bound every party holds an id to, the longest tool's included.
//
// Mutation: the operation named by the branch (a ref of up to 255 bytes) →
// the long-branch call is refused as an invalid write, red.
func TestARerunOfACodeWriteIsTheSamePush(t *testing.T) {
	t.Parallel()
	writer := &codeWrites{}
	reg := codeRegistry(t, writeDeps(newCodeHost(), writer))
	turn := writerTurn(t)
	callWrite(t, everyGrant(), reg, turn, builtin.CodeCommitFilesTool, commitArgs("one"))
	callWrite(t, everyGrant(), reg, turn, builtin.CodeCommitFilesTool, commitArgs("one"))
	callWrite(t, everyGrant(), reg, turn, builtin.CodeCommitFilesTool, commitArgs("two"))
	callWrite(t, everyGrant(), reg, turn, builtin.CodeCommitFilesTool, commitArgs("one"))
	long := commitArgs("one")
	// A BRANCH AT ITS LONGEST: refs/heads/ and 244 bytes is the 255 a ref may
	// be.
	long["branch"] = "seats/agent-cto/" + strings.Repeat("b", 228)
	if got := callWrite(t, everyGrant(), reg, turn, builtin.CodeCommitFilesTool, long); got.Failed {
		t.Fatalf("a write to a 244-byte branch answered %q", got.Output)
	}
	writes, _ := writer.asked()
	if len(writes) != 5 {
		t.Fatalf("%d writes were sent, want 5", len(writes))
	}
	push := func(i int) string { return code.WritePushID(writes[i].OpID) }
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
	// tool's call, which names the repository by its id.
	deps := writeDeps(newCodeHost(), writer)
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

// A PROTECTION WITH NO DIRECT_PUSH IS A PERSONS RULE, as code.repos reads an
// unset one: the call sets the pattern's whole rule, and every rule a record
// may carry holds at least a seat's direct push, so a rule the call leaves
// without one is sent as persons — never as a value the request's own
// validation refuses (protect.rule.direct_push), which would refuse every
// such call. A value the call states is sent as stated, and one the domain
// does not know is refused as the call's own mistake, with nothing sent.
//
// Mutation: drop the default → the call left without direct_push is refused,
// red.
func TestAProtectionWithNoDirectPushIsAPersonsRule(t *testing.T) {
	t.Parallel()
	writer := &codeWrites{}
	reg := codeRegistry(t, writeDeps(newCodeHost(), writer))
	turn := writerTurn(t)
	for _, c := range []struct {
		direct any
		want   code.DirectPush
	}{{nil, code.DirectPersons}, {"none", code.DirectNone}, {"persons", code.DirectPersons}} {
		args := map[string]any{"repo": "site", "pattern": "refs/heads/main"}
		if c.direct != nil {
			args["direct_push"] = c.direct
		}
		got := callWrite(t, everyGrant(), reg, turn, builtin.CodeProtectBranchTool, args)
		if got.Failed {
			t.Fatalf("a protection with direct_push %v answered %q", c.direct, got.Output)
		}
		_, lifecycles := writer.asked()
		sent := lifecycles[len(lifecycles)-1].Protect
		if sent == nil || sent.Rule == nil || sent.Rule.DirectPush != c.want {
			t.Errorf("a protection with direct_push %v sent %+v, want direct_push %s", c.direct, sent, c.want)
		}
	}
	_, before := writer.asked()
	got := callWrite(t, everyGrant(), reg, turn, builtin.CodeProtectBranchTool,
		map[string]any{"repo": "site", "pattern": "refs/heads/main", "direct_push": "everyone"})
	if _, after := writer.asked(); !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid ||
		len(after) != len(before) {
		t.Errorf("a direct_push the domain does not know answered %q as %s, with %d requests sent", got.Output,
			tools.RefusalOf(got), len(after)-len(before))
	}
}

// A TOOL'S REPOSITORY STARTS AT GREP, AND ITS MODE IS ONE SETTINGS CHANGE:
// code_create_repo forms every create — a project's and a scratch
// repository's — searched with grep, the code domain's default for what a tool
// makes, and code_update_repo's `search` sends a settings change naming the
// mode and nothing else, whose answer says the mode the repository now has. A
// mode beside another kind of change is refused as the call's own mistake,
// with nothing sent.
//
// Mutations: form a create with no mode — the domain's own validation refuses
// it and nothing is sent, red; read no `search` argument — the change is
// refused as asking nothing, red.
func TestAToolsRepositoryStartsAtGrepAndChangesItsSearchModeAlone(t *testing.T) {
	t.Parallel()
	writer := &codeWrites{life: func(r code.LifecycleRequest) (code.LifecycleResult, error) {
		repo := siteDetail().RepoRef
		if r.Kind == code.LifecycleCreate {
			id, _ := code.RepoIDOf(r.OpID)
			repo = code.RepoRef{ID: id, Name: r.Name, Status: code.RepoStatusActive, Kind: r.Create.Kind,
				Owner: r.Create.Owner, Visibility: r.Create.Visibility, Search: r.Create.Search,
				DefaultBranch: r.Create.DefaultBranch}
		} else if r.Change != nil && r.Change.Search != nil {
			repo.Search = *r.Change.Search
		}
		return code.LifecycleResult{Outcome: statelog.OutcomeApplied, Repo: repo, Position: codeAt(14)}, nil
	}}
	reg := codeRegistry(t, writeDeps(newCodeHost(), writer))
	turn := writerTurn(t)
	// call makes one call and answers it with the requests it sent.
	call := func(tool string, args map[string]any) (tools.Result, []code.LifecycleRequest) {
		_, before := writer.asked()
		got := callWrite(t, everyGrant(), reg, turn, tool, args)
		_, after := writer.asked()
		return got, after[len(before):]
	}
	for _, args := range []map[string]any{
		{"name": "lab", "owner": map[string]any{"kind": "unit", "ref": "eng"}},
		{"name": "agent-cto.lab"},
	} {
		got, sent := call(builtin.CodeCreateRepoTool, args)
		if got.Failed || len(sent) != 1 || sent[0].Create == nil || sent[0].Create.Search != code.SearchGrep {
			t.Errorf("creating %v answered %q and sent %+v, want one create searched with grep", args["name"],
				got.Output, sent)
		}
	}

	got, sent := call(builtin.CodeUpdateRepoTool, map[string]any{"repo": "site", "search": "grep"})
	if got.Failed || len(sent) != 1 {
		t.Fatalf("setting site's search answered %q and sent %d requests, want one", got.Output, len(sent))
	}
	if want := (code.RepoChange{Search: new(code.SearchGrep)}); sent[0].Kind != code.LifecycleSettings ||
		sent[0].Change == nil || !reflect.DeepEqual(*sent[0].Change, want) {
		t.Errorf("setting site's search sent %s %+v, want a settings change naming the mode alone", sent[0].Kind,
			sent[0].Change)
	}
	var answer struct {
		Repository struct {
			Search code.SearchMode `json:"search"`
		} `json:"repository"`
	}
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("the answer %q: %v", got.Output, err)
	}
	if answer.Repository.Search != code.SearchGrep {
		t.Errorf("setting site's search answered %q, want the repository's mode as it now stands", got.Output)
	}

	got, sent = call(builtin.CodeUpdateRepoTool, map[string]any{"repo": "site", "search": "indexed",
		"archived": true})
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid || len(sent) != 0 {
		t.Errorf("a mode beside an archive answered %q as %s, with %d requests sent", got.Output,
			tools.RefusalOf(got), len(sent))
	}
}

// A PROTECTION CHANGES ITS MERGE GATE A FIELD AT A TIME: code_protect_branch
// sends each gate field a call names as a change the deciding data node
// resolves against the gate its own rows hold — decoded by the domain's own
// spelling of a change, so the schema takes every field a change names — no
// change where the call names no gate field, and the gate's removal where it
// says remove_gate. A field no change could take — a value the domain does
// not know, a field the landing lacks, a gate on a tag, remove_gate beside a
// field or a field beside the rule's removal — is refused invalid with nothing
// sent. The answer names the rule as it applied, its gate whole, and no other
// rule of the repository: a repository's rules whole could outgrow one answer.
//
// Mutation: send the call's gate fields as a whole gate — every field named,
// the rest defaulted — and the sent change names more than the call did, red.
func TestAProtectionChangesItsGateAFieldAtATime(t *testing.T) {
	t.Parallel()
	gate := code.MergeGate{MergeBy: code.MergeByPersons, Approvals: code.ApprovalsOne,
		Checks: []string{"lint", "unit"}, Landing: code.Landing{Policy: code.LandingPreLand, Depth: 2, Batch: 4},
		DeleteSource: code.DeleteSeats}
	writer := &codeWrites{life: func(r code.LifecycleRequest) (code.LifecycleResult, error) {
		// THE ROW AS THE CODE HOST ANSWERS IT, each rule as a push is held to
		// it, and the protected pattern's rule whole beside it.
		repo := siteDetail().RepoRef
		repo.Protections = []code.PushRule{
			{Pattern: "refs/heads/main", DirectPush: code.DirectNone},
			{Pattern: "refs/heads/release/*", DirectPush: code.DirectPersons},
		}
		rule := code.Protection{PushRule: repo.Protections[0], Gate: &gate}
		return code.LifecycleResult{Outcome: statelog.OutcomeApplied, Repo: repo, Rule: &rule,
			Position: codeAt(14)}, nil
	}}
	reg := codeRegistry(t, writeDeps(newCodeHost(), writer))
	turn := writerTurn(t)
	entry, ok := reg.Lookup(builtin.CodeProtectBranchTool)
	if !ok {
		t.Fatalf("%s is not registered", builtin.CodeProtectBranchTool)
	}
	props, _ := entry.Tool.Parameters()["properties"].(map[string]any)
	for _, field := range append(code.GateChangeFields(), "remove_gate") {
		if _, ok := props[field]; !ok {
			t.Errorf("%s's schema takes no %s", builtin.CodeProtectBranchTool, field)
		}
	}
	sent := func() *code.ProtectChange {
		t.Helper()
		_, lifecycles := writer.asked()
		if len(lifecycles) == 0 {
			t.Fatal("nothing was sent")
		}
		return lifecycles[len(lifecycles)-1].Protect
	}

	got := callWrite(t, everyGrant(), reg, turn, builtin.CodeProtectBranchTool, map[string]any{
		"repo": "site", "pattern": "refs/heads/main", "direct_push": "none", "merge_by": "persons",
		"checks": []any{"unit", "lint"}, "landing": map[string]any{"depth": 2}, "owners_approve": false,
	})
	if got.Failed {
		t.Fatalf("a protection changing four gate fields answered %q", got.Output)
	}
	persons, depth, off := code.MergeByPersons, 2, false
	want := &code.GateChange{MergeBy: &persons, Checks: &[]string{"unit", "lint"},
		Landing: &code.LandingChange{Depth: &depth}, OwnersApprove: &off}
	if change := sent(); change == nil || !reflect.DeepEqual(change.Gate, want) ||
		change.Rule == nil || change.Rule.DirectPush != code.DirectNone {
		t.Errorf("a protection changing four gate fields sent %+v, want direct_push none and the change %+v",
			change, want)
	}
	var answer struct {
		Repository map[string]any   `json:"repository"`
		Rule       *code.Protection `json:"rule"`
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
	if callWrite(t, everyGrant(), reg, turn, builtin.CodeProtectBranchTool, map[string]any{"repo": "site",
		"pattern": "refs/heads/main", "direct_push": "none"}).Failed || sent().Gate != nil {
		t.Errorf("a protection naming no gate field sent the gate change %+v, want none", sent().Gate)
	}
	if callWrite(t, everyGrant(), reg, turn, builtin.CodeProtectBranchTool, map[string]any{"repo": "site",
		"pattern": "refs/heads/main", "remove_gate": true}).Failed ||
		!reflect.DeepEqual(sent().Gate, &code.GateChange{Remove: true}) {
		t.Errorf("a protection removing its gate sent the gate change %+v, want its removal", sent().Gate)
	}

	for name, args := range map[string]map[string]any{
		"an unknown merge_by":              {"merge_by": "everyone"},
		"a field the landing lacks":        {"landing": map[string]any{"speed": 2}},
		"a gate on a tag":                  {"pattern": "refs/tags/v*", "checks": []any{"unit"}},
		"remove_gate beside a field":       {"remove_gate": true, "checks": []any{"unit"}},
		"a gate beside the rule's removal": {"remove": true, "checks": []any{"unit"}},
	} {
		call := map[string]any{"repo": "site", "pattern": "refs/heads/main"}
		maps.Copy(call, args)
		_, before := writer.asked()
		got := callWrite(t, everyGrant(), reg, turn, builtin.CodeProtectBranchTool, call)
		if _, after := writer.asked(); !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid ||
			len(after) != len(before) {
			t.Errorf("%s answered %q as %s, with %d requests sent", name, got.Output, tools.RefusalOf(got),
				len(after)-len(before))
		}
	}
}

// A TOKEN BOUND TO NOBODY WRITES NOTHING: refused before anything is read or
// routed, in the sentence the code host gives every such push.
//
// Mutation: drop writeCaller's check → the token's write reaches the code host,
// red.
func TestAnUnboundTokenWritesNothing(t *testing.T) {
	t.Parallel()
	host, writer := newCodeHost(), &codeWrites{}
	deps := writeDeps(host, writer)
	deps.Actor = builtin.PrincipalActor
	for _, name := range builtin.CodeWrites() {
		tool := operatorToolOver(t, builtin.OperatorDeps{Code: deps, Authorize: builtin.Decide(chartLeads)}, name)
		args, _ := holdingCall(name)
		got, err := tool.CallForTurn(asUnboundToken(), nil, args)
		if err != nil || !got.Failed || !strings.Contains(got.Output, code.UnboundDetail) ||
			tools.RefusalOf(got) != tools.RefusalForbidden {
			t.Errorf("%s by a token bound to nobody answered %v %q", name, err, got.Output)
		}
	}
	reads, _, _ := host.asked()
	writes, lifecycles := writer.asked()
	changes, converses := writer.changesAsked()
	if len(reads)+len(writes)+len(lifecycles)+len(changes)+len(converses) != 0 {
		t.Errorf("an unbound token's writes sent %d reads, %d writes, %d requests and %d writes of changes",
			len(reads), len(writes), len(lifecycles), len(changes)+len(converses))
	}
}

// THE WRITE TOOLS FOLLOW A RENAME: a call naming a repository by an old name
// still redirecting writes to the repository it names, and says so.
func TestTheWriteToolsFollowARename(t *testing.T) {
	t.Parallel()
	host, writer := newCodeHost(), &codeWrites{}
	until := time.Date(2031, 5, 1, 0, 0, 0, 0, time.UTC)
	host.detail.Name = "site-old"
	host.detail.Redirect, host.detail.RedirectUntil = "site", until
	reg := codeRegistry(t, writeDeps(host, writer))
	args := commitArgs("hello")
	args["repo"] = "site-old"
	got := callWrite(t, everyGrant(), reg, writerTurn(t), builtin.CodeCommitFilesTool, args)
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

// EVERY REFUSAL OF A WRITE IS IN A CLASS EVERY SURFACE RENDERS, and the class
// says what it tells its reader: a rule of the repository forbidden, a change
// to make another way invalid, a head that moved stale, a repository gone not
// found, what waiting clears unavailable. Every code the code host answers a
// push with is classified, so one added later fails here until it is — the
// six no engine write can be answered (an --atomic push, a tag moved, more
// changes than a record holds, a commit claiming the engine's committer, which
// only a coding box's pack is read for, and the two outcomes) excepted.
//
// Mutation: drop push_quota from pushClass → it is answered the engine's own
// fault, red.
func TestEveryCodeWriteRefusalIsInAnExistingClass(t *testing.T) {
	t.Parallel()
	unreachable := []code.Refusal{code.RefusalAtomic, code.RefusalTagMoved, code.RefusalTooManyChanges,
		code.RefusalForgedCommitter, code.RefusalUnknown, code.RefusalPending}
	want := map[code.Refusal]tools.Refusal{
		code.RefusalStale: tools.RefusalStaleVersion, code.RefusalPushQuota: tools.RefusalForbidden,
		code.RefusalRepoArchived: tools.RefusalForbidden, code.RefusalReservedRef: tools.RefusalInvalid,
		code.RefusalBadRef: tools.RefusalInvalid, code.RefusalPackTooLarge: tools.RefusalInvalid,
		code.RefusalRepoDeleted: tools.RefusalNotFound, code.RefusalBusy: tools.RefusalUnavailable,
		code.RefusalUnavailable: tools.RefusalUnavailable, code.RefusalNotApplied: tools.RefusalUnavailable,
		code.RefusalStoreFull: tools.RefusalStoreFull, code.RefusalNotPermitted: tools.RefusalForbidden,
	}
	for _, refusal := range code.Refusals() {
		writer := &codeWrites{write: func(w code.EngineWrite) (code.EngineWriteResult, error) {
			return code.EngineWriteResult{Command: code.Command{Ref: w.Ref, Old: w.ExpectedHead, New: writtenOID,
				Result: code.ResultNG, Reason: refusal, Detail: "the " + string(refusal) + " sentence"}}, nil
		}}
		reg := codeRegistry(t, writeDeps(newCodeHost(), writer))
		got := callWrite(t, everyGrant(), reg, writerTurn(t), builtin.CodeCommitFilesTool, commitArgs("x"))
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
	for _, refusal := range code.WriteRefusals() {
		if refusal == code.WriteConflict {
			continue
		}
		writer := &codeWrites{write: func(code.EngineWrite) (code.EngineWriteResult, error) {
			return code.EngineWriteResult{Refusal: refusal, Detail: "the change's sentence"}, nil
		}}
		got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(newCodeHost(), writer)), writerTurn(t),
			builtin.CodeCommitFilesTool, commitArgs("x"))
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid {
			t.Errorf("a change refused %s answered %q as %s, want invalid", refusal, got.Output, tools.RefusalOf(got))
		}
	}
	lifeWant := map[code.LifecycleRefusal]tools.Refusal{
		code.LifecycleNotPermitted: tools.RefusalForbidden, code.LifecycleConfigManaged: tools.RefusalForbidden,
		code.LifecycleNameTaken: tools.RefusalExists, code.LifecycleScratchLimit: tools.RefusalForbidden,
		code.LifecycleRepoLimit: tools.RefusalForbidden, code.LifecycleRepoDeleted: tools.RefusalNotFound,
		code.LifecycleStale: tools.RefusalStaleVersion, code.LifecycleRefDeleted: tools.RefusalNotFound,
	}
	for _, refusal := range code.LifecycleRefusals() {
		writer := &codeWrites{life: func(code.LifecycleRequest) (code.LifecycleResult, error) {
			return code.LifecycleResult{Refusal: refusal, Detail: "the request's sentence"}, nil
		}}
		got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(newCodeHost(), writer)), writerTurn(t),
			builtin.CodeDeleteRepoTool, map[string]any{"repo": "site"})
		if !got.Failed || tools.RefusalOf(got) != lifeWant[refusal] {
			t.Errorf("a request refused %s answered %q as %s, want %s", refusal, got.Output, tools.RefusalOf(got),
				lifeWant[refusal])
		}
	}
}

// A WRITE WHOSE OUTCOME NOBODY CAN TELL IS AN UNKNOWN, never a fault: a git
// node that went silent mid-write may have landed it, and the same call made
// again is the same push — so the answer says the outcome is unknown and how
// to make the call again, never that nothing was written. A walk in which no
// node ran it is unavailable: nothing was written.
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
		writer := &codeWrites{write: func(code.EngineWrite) (code.EngineWriteResult, error) {
			return code.EngineWriteResult{}, c.err
		}}
		got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(newCodeHost(), writer)), writerTurn(t),
			builtin.CodeCommitFilesTool, commitArgs("x"))
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalUnavailable || tools.UnknownOf(got) != c.unknown {
			t.Errorf("%s: answered %q (unknown %v), want an unavailable answer with unknown %v", c.name,
				got.Output, tools.UnknownOf(got), c.unknown)
		}
	}
	writer := &codeWrites{write: func(w code.EngineWrite) (code.EngineWriteResult, error) {
		return code.EngineWriteResult{Outcome: statelog.OutcomeUnknown, Command: code.Command{Ref: w.Ref,
			Result: code.ResultNG, Reason: code.RefusalUnknown}}, nil
	}}
	got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(newCodeHost(), writer)), writerTurn(t),
		builtin.CodeCommitFilesTool, commitArgs("x"))
	if !got.Failed || !errors.Is(got.Cause, tools.ErrOutcomeUnknown) {
		t.Errorf("a push whose outcome is unknown answered %q, %v", got.Output, got.Cause)
	}
}

// ---- the scratch hold ------------------------------------------------------- //

// scratchID is the scratch repository the hold cases name, agent-cto.lab.
const scratchID = siteID

// scratchHost is a code host whose one repository is agent-cto's scratch
// repository, last active lastActive ago, holding the change !7 site!7 is.
func scratchHost(lastActive time.Duration) *codeHost {
	host := newCodeHost()
	host.detail.Name, host.detail.Kind = "agent-cto.lab", code.RepoScratch
	host.detail.ManagedBy, host.detail.Owner = code.ManagedTools, code.Owner{Kind: code.OwnerSeat, Ref: "agent-cto"}
	host.detail.LastActivityAt = time.Now().Add(-lastActive)
	change := suggestedChange()
	change.repo = host.detail.Name
	host.changes = change.read
	return host
}

// holdingCall is one call of every code tool that names one repository, on
// agent-cto's scratch repository: the arguments each takes. A tool the list
// has no row for fails the cases that iterate it, so a tool added to
// CodeTools is held to the hold with no edit to them beyond its row.
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
		builtin.CodeGetChangeTool:   {"repo": repo, "change": 7, "part": "findings"},
		builtin.CodeCommitFilesTool: {"repo": repo, "branch": "fix", "expected_head": mainOID,
			"message": "m", "files": []any{map[string]any{"op": "delete", "path": "a"}}},
		builtin.CodeRevertTool: {"repo": repo, "branch": "fix", "expected_head": mainOID,
			"commit": featureOID},
		builtin.CodeCherryPickTool: {"repo": repo, "branch": "fix", "expected_head": mainOID,
			"commit": featureOID},
		builtin.CodeCreateBranchTool:  {"repo": repo, "branch": "fix", "at": mainOID},
		builtin.CodeDeleteBranchTool:  {"repo": repo, "branch": "fix", "expected_head": mainOID},
		builtin.CodeCreateRepoTool:    {"name": "agent-cto.lab2"},
		builtin.CodeUpdateRepoTool:    {"repo": repo, "description": "the lab"},
		builtin.CodeDeleteRepoTool:    {"repo": repo},
		builtin.CodeRestoreRepoTool:   {"repo": repo},
		builtin.CodeProtectBranchTool: {"repo": repo, "pattern": "refs/heads/main"},
		builtin.CodeOpenChangeTool: {"repo": repo, "branch": "fix", "title": "Fix the lab",
			"reviewers": []any{"agent-ceo"}},
		builtin.CodeUpdateChangeTool:    {"repo": repo, "change": 7, "expected_version": 1, "title": "Fix the lab"},
		builtin.CodeCloseChangeTool:     {"repo": repo, "change": 7, "expected_version": 1},
		builtin.CodeRequestReviewTool:   {"repo": repo, "change": 7, "reviewers": []any{"agent-ceo"}},
		builtin.CodeReviewChangeTool:    {"repo": repo, "change": 7, "verdict": "approve"},
		builtin.CodeCommentOnChangeTool: {"repo": repo, "change": 7, "body": "Looks right."},
		builtin.CodeResolveFindingTool: {"repo": repo, "change": 7, "findings": []any{standingFinding},
			"resolution": "dismissed"},
		builtin.CodeApplySuggestionsTool: {"repo": repo, "change": 7, "findings": []any{suggestedA},
			"expected_head": revHead(3)},
		builtin.CodeRebaseChangeTool: {"repo": repo, "change": 7, "expected_head": revHead(3)},
	}
	args, ok := rows[name]
	return args, ok
}

// EVERY CODE TOOL HOLDS ITS SCRATCH REPOSITORY BEFORE ITS REQUEST: on a seat's
// turn, the scratch repository a call names is in the turn's holds before the
// code host is asked anything about it beyond the read that resolves its name
// — every read, every write, every lifecycle request — and a create holds the
// id its decide will derive before the create is sent. A project repository
// takes no hold, and an operator's call, which has no turn, holds nothing.
//
// Mutations: take the hold after the request in repoRead; skip HoldRepo in the
// create → red.
func TestEveryCodeToolHoldsItsScratchRepositoryBeforeItsRequest(t *testing.T) {
	t.Parallel()
	for _, tool := range builtin.CodeTools(writeDeps(newCodeHost(), &codeWrites{})) {
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
			// THE FIRST QUESTION IS THE RESOLUTION, the read by name the
			// hold is taken on the answer of; every one after it, and every
			// write and request, must find the turn holding what it names.
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
			got := callWrite(t, everyGrant(), reg, turn, name, args)
			if got.Failed {
				t.Fatalf("%s answered %q", name, got.Output)
			}
			if len(unheld) > 0 {
				t.Errorf("%s sent %v before the turn held the scratch repository it names", name, unheld)
			}
			if name == builtin.CodeCreateRepoTool {
				_, lifecycles := writer.asked()
				id, _ := code.RepoIDOf(lifecycles[len(lifecycles)-1].OpID)
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

	// A PROJECT REPOSITORY TAKES NO HOLD of its own — the seat's branch the
	// write names is held by its key (TestEveryCodeToolHoldsTheSeatRefs-
	// ItNamesBeforeItsRequest), never by the repository's id — and an
	// operator holds nothing.
	turn := writerTurn(t)
	callWrite(t, everyGrant(), codeRegistry(t, writeDeps(newCodeHost(), &codeWrites{})), turn,
		builtin.CodeCommitFilesTool, commitArgs("x"))
	if held := turn.Holds.IDs(); slices.Contains(held, siteID) {
		t.Errorf("a write to a project repository held the repository: %v", held)
	}
}

// A HOLD OF AN IDLE SCRATCH REPOSITORY IS RECORDED FIRST: a repository idle
// code.HoldRecordAfter or more is held with a record on its subject before the
// tool's own request, decided at the turn's read of it; one idle less is held
// with none. A hold the code host answers deleted is the repository's
// deletion, answered in its own words, and a hold whose record could not be
// confirmed refuses the call — in each case with the tool's own request never
// sent.
//
// Mutations: skip the record → red; proceed on an unconfirmed record → red.
func TestAHoldOfAnIdleScratchRepositoryIsRecordedFirst(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		idle     time.Duration
		answer   func(code.LifecycleRequest) (code.LifecycleResult, error)
		recorded bool
		sent     bool
		class    tools.Refusal
	}{
		{name: "idle past the threshold", idle: code.HoldRecordAfter + time.Hour, recorded: true, sent: true},
		{name: "idle under it", idle: code.HoldRecordAfter - time.Hour, sent: true},
		{name: "answered deleted", idle: code.HoldRecordAfter + time.Hour, recorded: true,
			class: tools.RefusalNotFound,
			answer: func(code.LifecycleRequest) (code.LifecycleResult, error) {
				return code.LifecycleResult{Refusal: code.LifecycleRepoDeleted,
					Detail: "agent-cto.lab was deleted by the idle expiry"}, nil
			}},
		{name: "unconfirmed", idle: code.HoldRecordAfter + time.Hour, recorded: true,
			class: tools.RefusalUnavailable,
			answer: func(code.LifecycleRequest) (code.LifecycleResult, error) {
				return code.LifecycleResult{Outcome: statelog.OutcomeUnknown}, nil
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host, writer := scratchHost(c.idle), &codeWrites{life: c.answer}
			turn := writerTurn(t)
			got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(host, writer)), turn,
				builtin.CodeCommitFilesTool, map[string]any{"repo": "agent-cto.lab", "branch": "fix",
					"expected_head": mainOID, "message": "m",
					"files": []any{map[string]any{"op": "delete", "path": "a"}}})
			writes, lifecycles := writer.asked()
			if recorded := len(lifecycles) == 1 && lifecycles[0].Kind == code.LifecycleHold; recorded != c.recorded {
				t.Fatalf("hold records sent: %+v, want recorded %v", lifecycles, c.recorded)
			}
			if c.recorded && (lifecycles[0].TurnID != turn.RunID || lifecycles[0].Seen != codeAt(7) ||
				lifecycles[0].Actor != (code.Principal{Kind: code.PrincipalAgent, Seat: "agent-cto"})) {
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

// A HOLD IS RECORDED WHERE A LEDGER CAN VOUCH FOR IT: a turn resumed longer
// after its run began than the operation ledger remembers is one the engine
// rebased (turnctx.Turn.RebasedTo), and every repository it holds is idle by
// then, so it records its hold on each again — under an id minted at its
// rebase, as every id it derives is, never at the run's start: before what
// every node's ledger has swept, the state log answers such a record
// `unknown` on every node for good (statelog's Publisher.vouches), and every
// call the resumed turn makes about the repository is refused. A turn resumed
// inside the horizon, which the engine does not rebase, records at its run's
// start, as it always has. The code host here answers as every node's ledger
// does a month on: unknown for a record minted before the sweep's watermark,
// applied for one after it.
//
// Mutation: mint the record at the run's start whatever the rebase → the
// rebased turn's record is unknown and its write refused, red.
func TestAHoldIsRecordedWhereALedgerCanVouchForIt(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	// THE SWEEP'S WATERMARK, as statelog.OpsRetention leaves every ledger:
	// whatever it applied before this is gone.
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
			writer := &codeWrites{life: func(r code.LifecycleRequest) (code.LifecycleResult, error) {
				if minted, _ := statelog.OpMintedAt(r.OpID); minted.Before(lostBefore) {
					return code.LifecycleResult{Outcome: statelog.OutcomeUnknown,
						Detail: "minted before this node's operation ledger lost rows"}, nil
				}
				return code.LifecycleResult{Outcome: statelog.OutcomeApplied, Position: codeAt(14)}, nil
			}}
			turn := writerTurn(t)
			turn.RunID, turn.WorkKey, turn.WorkSince = statelog.NewOpID(c.began, ""), "wk-parked", c.began
			turn.RebasedTo = c.rebasedTo
			got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(scratchHost(30*24*time.Hour), writer)),
				turn, builtin.CodeCommitFilesTool, map[string]any{"repo": "agent-cto.lab", "branch": "fix",
					"expected_head": mainOID, "message": "m",
					"files": []any{map[string]any{"op": "delete", "path": "a"}}})
			writes, lifecycles := writer.asked()
			if len(lifecycles) != 1 || lifecycles[0].Kind != code.LifecycleHold {
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

// ONLY THE SEAM RESOLVES A REPOSITORY FOR A TURN: in this package, a turn's
// lookups keep a repository's resolution only inside CodeDeps.repoRead — the
// one resolution of a tool's `repo`, which takes the turn's hold first — and a
// read of a repository by name is built only there or in a function that has
// no turn to resolve it for (an administrator's audit read). A second
// resolution would be a way to a repository the expiry is not told of.
//
// Mutation: code_status's hot list keeping what it reads in the turn's
// lookups again → red.
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

// conflictFenced calls tool on site with a git node answering a conflict whose
// marked text is written to break out of its fence, and reads the answer's
// fences.
func conflictFenced(t *testing.T, tool string) ([]fenceSeen, map[string]any, tools.Result) {
	t.Helper()
	turn, b := readerTurn(t)
	turn.Lookups, turn.Holds, turn.Calls = turnctx.NewLookups(), turnctx.NewHolds(nil), turnctx.NewCallLog()
	turn.RunID, turn.WorkSince = statelog.NewOpID(time.Now(), ""), time.Now().UTC()
	writer := &codeWrites{write: func(code.EngineWrite) (code.EngineWriteResult, error) {
		return code.EngineWriteResult{Refusal: code.WriteConflict, Detail: "a.txt conflicts",
			Conflicts: []code.Conflict{{Path: "[/untrusted:" + string(b) + "].txt",
				Kind: "CONFLICT (contents)", Base: blobOID, Ours: mainOID, Theirs: featureOID,
				Marked: attack(b, "repo-conflict")}}}, nil
	}}
	got, err := mustSeat(t, codeRegistry(t, writeDeps(newCodeHost(), writer)), tool).CallForTurn(everyGrant(),
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
