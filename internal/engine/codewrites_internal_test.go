package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE CODE WRITE TOOLS THROUGH THE ENGINE'S OWN PATHS: a bridged call of an
// agent-mode run, a native turn that suspends on a launch, and a native turn
// still running — each through the real tools, the real launchers and the
// reader the code-retention pass asks which repositories a live turn holds.

// codeWriteHost is the routed code host as the code tools see it: its
// repositories by name, the refs a case gives them, every engine write and
// lifecycle request it is asked, each landed or applied — and every question,
// in order (events).
type codeWriteHost struct {
	mu    sync.Mutex
	repos map[string]code.RepoRef
	// refs are a repository's refs by its name, each whole ref's oid: a
	// resolve answers one set an hour ago, by the seat whose namespace holds
	// it.
	refs       map[string]map[string]string
	writes     []code.EngineWrite
	lifecycles []code.LifecycleRequest
	// events is every question asked — resolve_ref, git_write, lifecycle —
	// in the order it came.
	events []string
}

func newCodeWriteHost(repos ...code.RepoRef) *codeWriteHost {
	h := &codeWriteHost{repos: map[string]code.RepoRef{}, refs: map[string]map[string]string{}}
	for _, r := range repos {
		h.repos[r.Name] = r
	}
	return h
}

// asked is every question the host was asked, in order.
func (h *codeWriteHost) asked() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.events)
}

func (h *codeWriteHost) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	repo, ok := h.repos[q.Repo]
	if !ok {
		return code.ReadAnswer{}, fmt.Errorf("%w: %s", code.ErrRepoNotFound, q.Repo)
	}
	return code.ReadAnswer{Repo: &code.RepoDetail{RepoSummary: code.RepoSummary{RepoRef: repo}},
		Position: repo.Position, Level: q.Fresh.Level}, nil
}

func (h *codeWriteHost) ResolveRef(_ context.Context, q code.RefQuery) (code.RefAnswer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "resolve_ref")
	repo, ok := h.repos[q.Repo]
	if !ok {
		return code.RefAnswer{}, fmt.Errorf("%w: %s", code.ErrRepoNotFound, q.Repo)
	}
	out := code.RefAnswer{Repo: repo, Position: codeLogAt(10)}
	for _, asked := range q.Refs {
		whole := asked
		if !strings.HasPrefix(whole, "refs/") {
			whole = "refs/heads/" + asked
		}
		oid, ok := h.refs[q.Repo][whole]
		if !ok {
			out.Refs = append(out.Refs, code.ResolvedRef{Name: asked, Missing: "no live " + whole})
			continue
		}
		seat, _, _ := strings.Cut(strings.TrimPrefix(whole, code.SeatNamespaceRoot), "/")
		out.Refs = append(out.Refs, code.ResolvedRef{Name: asked, Ref: whole, OID: oid, Moved: codeLogAt(8),
			At: time.Now().Add(-time.Hour), Seat: seat})
	}
	return out, nil
}

func (h *codeWriteHost) GitRead(context.Context, code.GitQuery) (code.GitAnswer, error) {
	return code.GitAnswer{}, code.ErrObjectNotFound
}

func (h *codeWriteHost) FloorOf(string) statelog.Position { return statelog.Position{} }

func (h *codeWriteHost) GitWrite(_ context.Context, w code.EngineWrite) (code.EngineWriteResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.writes = append(h.writes, w)
	h.events = append(h.events, "git_write")
	commit := strings.Repeat("d", 40)
	return code.EngineWriteResult{Outcome: statelog.OutcomeApplied, Commit: commit, PushID: code.WritePushID(w.OpID),
		Position: codeLogAt(20), Command: code.Command{Ref: w.Ref, Old: w.ExpectedHead, New: commit,
			Result: code.ResultOK}}, nil
}

func (h *codeWriteHost) Lifecycle(_ context.Context, r code.LifecycleRequest) (code.LifecycleResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lifecycles = append(h.lifecycles, r)
	h.events = append(h.events, "lifecycle:"+string(r.Kind))
	return code.LifecycleResult{Outcome: statelog.OutcomeApplied, Position: codeLogAt(21)}, nil
}

// Change and Converse answer a change's writes as decided — no case here
// writes one: the code writes these cases make are engine writes and
// lifecycle requests, and the change tools ride the same list.
func (h *codeWriteHost) Change(context.Context, code.ChangeProposal) (code.ChangeResult, error) {
	return code.ChangeResult{Outcome: statelog.OutcomeApplied, Position: codeLogAt(22)}, nil
}

func (h *codeWriteHost) Converse(context.Context, code.ConversationWrite) (code.ConversationResult, error) {
	return code.ConversationResult{Outcome: statelog.OutcomeApplied, Position: codeLogAt(23)}, nil
}

// writesAsked is the engine writes the host was asked, copied under its lock.
func (h *codeWriteHost) writesAsked() []code.EngineWrite {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.writes)
}

// codeLogAt is a position on the code log.
func codeLogAt(seq uint64) statelog.Position {
	return statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: seq}
}

// noRequester composes nobody: these turns were woken by no one, so no code
// call of theirs is made for anybody but the seat.
type noRequester struct{}

func (noRequester) Requester(_ context.Context, handle string) (iam.Principal, error) {
	return iam.Principal{}, fmt.Errorf("no requester is composed here, and %q was asked for", handle)
}

// codeTool is the real code tool named name, over host's reads and writes.
func codeTool(t *testing.T, host *codeWriteHost, name string) tools.Callable {
	t.Helper()
	for _, tool := range builtin.CodeTools(builtin.CodeDeps{Code: host, Writer: host, Holder: host,
		Requesters: noRequester{}, Chart: authz.NoChart{}}) {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("the code tools serve no %s", name)
	return nil
}

// scratchOf is seat's scratch repository named name, last active an hour ago:
// held by a turn that names it, and recorded by none.
func scratchOf(seat *org.Role, name string) code.RepoRef {
	return code.RepoRef{ID: uuid.NewString(), Name: seat.Handle() + "." + name, Status: code.RepoStatusActive,
		ManagedBy: code.ManagedTools, Kind: code.RepoScratch, Owner: code.Owner{Kind: code.OwnerSeat,
			Ref: seat.Handle()}, Visibility: code.VisibilityCompany, DefaultBranch: "main",
		LastActivityAt: time.Now().Add(-time.Hour), Position: codeLogAt(9)}
}

// commitTo is a commit of one file onto repo's branch fix, which a call
// creates.
func commitTo(repo string) map[string]any {
	return map[string]any{"repo": repo, "branch": "fix", "message": "Fix the readme",
		"files": []any{map[string]any{"op": "put", "path": "README.md", "content": "hello"}}}
}

// bridgedCall makes one bridged call of name with args, as the box's coding
// agent does, failing the case unless it answered.
func (b bridgedRun) bridgedCall(t *testing.T, name string, args map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := b.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	if res.IsError {
		t.Fatalf("CallTool(%s) answered an error: %v", name, res.Content)
	}
}

// liveHolds is the code-retention pass's reader over runs — a run store — on a
// node whose running turns are turns: which of keys a live turn holds.
func liveHolds(t *testing.T, runs interface {
	ListRuns(context.Context) ([]sandbox.PendingRun, error)
}, turns *runningTurns, keys ...string) map[string]bool {
	t.Helper()
	got, err := holdsReader{runs: runs, leases: listedLeases{}, self: turns, budget: holdsAskBudget}.
		holds(t.Context(), keys)
	if err != nil {
		t.Fatalf("read the live turns' holds: %v", err)
	}
	return got
}

// modellessCompany is splitLoginCompany's one-seat epoch with its executor on
// an API-key entry that names no model: the launch leaves the coding CLI to
// choose its own.
func modellessCompany(t *testing.T) (*Company, *org.Role) {
	t.Helper()
	api, err := cliagent.New(cliagent.Config{
		Key: "api", Agent: "claude-code", StateDir: t.TempDir(), Timeout: time.Minute,
		Auth: cliagent.Auth{Mode: cliagent.AuthAPIKey, APIKey: "sk-ant-not-real"},
	})
	if err != nil {
		t.Fatalf("cliagent.New: %v", err)
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "api", Provider: api}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	seat := &org.Role{Name: "SWE", LLM: org.ProviderKeys{"api"}}
	return &Company{
		Models: models,
		Org:    &org.Organization{Name: "Acme", Roles: []*org.Role{seat}},
		Config: &config.Company{Providers: config.Providers{LLM: map[string]config.LLMProvider{
			"api": {Auth: config.CLIAgentAuth{Mode: config.AuthAPIKey, APIKey: "${ANTHROPIC_API_KEY}"}},
		}}},
	}, seat
}

// A BRIDGED CODE WRITE NAMES ITS LAUNCH'S MODEL: an agent-mode run's coding
// CLI makes its calls through the bridge, in rounds the engine never sees, so
// each bridged call is made on the model the run's launch pointed the CLI at —
// the one a seat's code write names on its push, as a native round's call
// names its round's — and a launch that named none names none.
//
// Mutation: the bridge leaving the call's context unset → the first write
// names no model, red.
func TestABridgedCodeWriteNamesItsLaunchsModel(t *testing.T) {
	t.Parallel()
	named, namedSeat := splitLoginCompany(t, "api", "codex")
	modelless, modellessSeat := modellessCompany(t)
	for _, c := range []struct {
		company *Company
		seat    *org.Role
		want    string
	}{
		{named, namedSeat, "claude-golden"},
		{modelless, modellessSeat, ""},
	} {
		host := newCodeWriteHost(code.RepoRef{ID: uuid.NewString(), Name: "site", Status: code.RepoStatusActive,
			ManagedBy: code.ManagedConfig, Kind: code.RepoProject, Owner: code.Owner{Kind: code.OwnerUnit,
				Ref: "eng"}, Visibility: code.VisibilityCompany, DefaultBranch: "main", Position: codeLogAt(9)})
		turn := &turnctx.Turn{RunID: statelog.NewOpID(time.Now(), ""), Seat: c.seat,
			Lookups: turnctx.NewLookups(), Holds: turnctx.NewHolds(nil)}
		run := launchBridgedOn(t, c.company, turn, codeTool(t, host, builtin.CodeCommitFilesTool))
		run.bridgedCall(t, builtin.CodeCommitFilesTool, commitTo("site"))
		writes := host.writesAsked()
		if len(writes) != 1 {
			t.Fatalf("%d writes reached the code host, want 1", len(writes))
		}
		if got := writes[0].Principal.Run.Model; got != c.want {
			t.Errorf("a bridged write of a run launched on %q names model %q", c.want, got)
		}
		if writes[0].Principal.Run.TurnID != turn.RunID {
			t.Errorf("a bridged write names turn %q, want the run's own %q", writes[0].Principal.Run.TurnID,
				turn.RunID)
		}
	}
}

// A BRIDGED RUN HOLDS THE SCRATCH REPOSITORY IT WROTE: an agent-mode run's
// bridged code_commit_files on a scratch repository takes the turn's hold, and
// the hold is on the run's row before the write is sent — so a node that
// restarts under the parked run, its bridge session and every hold in its
// memory gone, still answers the code-retention pass that the repository is
// held, from the row.
//
// Mutation: the launch attaching no writer to the turn's holds → the hold is
// in memory only, and the restarted node answers the repository unheld, red.
func TestABridgedRunHoldsTheScratchRepositoryItWrote(t *testing.T) {
	t.Parallel()
	c, seat := splitLoginCompany(t, "api", "codex")
	scratch := scratchOf(seat, "lab")
	host := newCodeWriteHost(scratch)
	turn := &turnctx.Turn{RunID: statelog.NewOpID(time.Now(), ""), Seat: seat,
		Lookups: turnctx.NewLookups(), Holds: turnctx.NewHolds(nil)}
	run := launchBridgedOn(t, c, turn, codeTool(t, host, builtin.CodeCommitFilesTool))
	run.bridgedCall(t, builtin.CodeCommitFilesTool, commitTo(scratch.Name))
	if row := run.row(t, turn.RunID); !slices.Contains(row.CodeHolds, scratch.ID) {
		t.Fatalf("the run's row holds %v, want the scratch repository %s its bridged write named", row.CodeHolds,
			scratch.ID)
	}
	// THE NODE RESTARTS: no running turn, no bridge session — the row alone.
	if held := liveHolds(t, run.pending, &runningTurns{}, scratch.ID); !held[scratch.ID] {
		t.Errorf("a restarted node answers scratch repository %s unheld under the parked run", scratch.ID)
	}
}

// webOf is the project repository web, the engineering unit's, its default
// branch main.
func webOf() code.RepoRef {
	return code.RepoRef{ID: uuid.NewString(), Name: "web", Status: code.RepoStatusActive,
		ManagedBy: code.ManagedConfig, Kind: code.RepoProject, Owner: code.Owner{Kind: code.OwnerUnit,
			Ref: "eng"}, Visibility: code.VisibilityCompany, DefaultBranch: "main", Position: codeLogAt(9)}
}

// commitOn is a commit of one file onto repo's branch — at head where head is
// given, and creating the branch where it is not.
func commitOn(repo, branch, head string) map[string]any {
	args := map[string]any{"repo": repo, "branch": branch, "message": "Take notes",
		"files": []any{map[string]any{"op": "put", "path": "NOTES.md", "content": "notes"}}}
	if head != "" {
		args["expected_head"] = head
	}
	return args
}

// A BRIDGED RUN'S BRANCH HOLD OUTLIVES ITS NODE (M3-decisions M3-O64): an
// agent-mode run's bridged code_commit_files creates a seat's branch on a
// project repository — a branch no launch granted — and the turn's hold on it
// is on the run's row before the write is sent, so a node that restarts under
// the parked run, its bridge session and every hold in its memory gone, still
// answers the branch held, from the row.
//
// Mutation: CodeDeps.branch skipping HoldRef → the branch's key is on no row,
// red.
func TestABridgedRunsBranchHoldOutlivesItsNode(t *testing.T) {
	t.Parallel()
	c, seat := splitLoginCompany(t, "api", "codex")
	web := webOf()
	host := newCodeWriteHost(web)
	turn := &turnctx.Turn{RunID: statelog.NewOpID(time.Now(), ""), Seat: seat,
		Lookups: turnctx.NewLookups(), Holds: turnctx.NewHolds(nil)}
	run := launchBridgedOn(t, c, turn, codeTool(t, host, builtin.CodeCommitFilesTool))
	branch := code.SeatNamespace(seat.Handle()) + "notes"
	run.bridgedCall(t, builtin.CodeCommitFilesTool, commitOn(web.Name, strings.TrimPrefix(branch, "refs/heads/"), ""))
	key := code.RefHoldKey(web.ID, branch)
	if row := run.row(t, turn.RunID); !slices.Contains(row.CodeHolds, key) {
		t.Fatalf("the run's row holds %v, want %s's key %s, which its bridged write created", row.CodeHolds, branch,
			key)
	}
	// THE NODE RESTARTS: no running turn, no bridge session — the row alone.
	if held := liveHolds(t, run.pending, &runningTurns{}, key); !held[key] {
		t.Errorf("a restarted node answers %s unheld under the parked run", branch)
	}
}

// A RESUMED TURN RESOLVES ITS BRANCH AGAIN (M3-decisions M3-O65): a native
// executor writes a seat's branch twice at its head — one resolve, before the
// first write, which confirms its hold — launches run_sandbox and suspends;
// the resumed executor, its holds built from the row, holds the branch's key
// and confirms none, so its write resolves the branch once more, before it.
//
// Mutation: NewHolds confirming its seed, or the mark riding the row → the
// resumed write resolves nothing, red.
func TestAResumedTurnResolvesItsBranchAgain(t *testing.T) {
	t.Parallel()
	c := specCompany(t, runEnvDoc)
	e, _ := codeWorkRig(t, c)
	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"SWE_GITLAB_TOKEN": "glpat-swe"})
	e.secretsMu.Unlock()
	seat := c.Org.AgentSeatByHandle("swe")
	web := webOf()
	host := newCodeWriteHost(web)
	branch := code.SeatNamespace(seat.Handle()) + "x"
	head := strings.Repeat("d", 40)
	host.refs[web.Name] = map[string]string{branch: head}
	commit := codeTool(t, host, builtin.CodeCommitFilesTool).(tools.SeatCallable)
	args := commitOn(web.Name, strings.TrimPrefix(branch, "refs/heads/"), head)

	turn := &turnctx.Turn{RunID: statelog.NewOpID(time.Now(), ""), Seat: seat, Org: c.Org,
		Lookups: turnctx.NewLookups(), Holds: turnctx.NewHolds(nil)}
	for range 2 {
		if got, err := commit.CallForTurn(t.Context(), turn, args); err != nil || got.Failed {
			t.Fatalf("the executor's write answered %v %q", err, got.Output)
		}
	}
	if got := host.asked(); !slices.Equal(got, []string{"resolve_ref", "git_write", "git_write"}) {
		t.Fatalf("the executor's two writes asked %v, want one resolve, before the first", got)
	}
	if _, err := (&launcher{engine: e}).Launch(t.Context(), turn,
		builtin.SandboxRequest{Brief: "fix the failing test"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	row, found, err := e.sandbox.Load().pending.Get(t.Context(), turn.RunID)
	if err != nil || !found || !slices.Contains(row.CodeHolds, code.RefHoldKey(web.ID, branch)) {
		t.Fatalf("the launched run's row holds %v (found %v, %v), want %s's key", row.CodeHolds, found, err, branch)
	}
	resumed := &turnctx.Turn{RunID: turn.RunID, Seat: seat, Org: c.Org, Lookups: turnctx.NewLookups(),
		Holds: e.rowHolds(row)}
	mark := len(host.asked())
	if got, err := commit.CallForTurn(t.Context(), resumed, args); err != nil || got.Failed {
		t.Fatalf("the resumed executor's write answered %v %q", err, got.Output)
	}
	if got := host.asked()[mark:]; !slices.Equal(got, []string{"resolve_ref", "git_write"}) {
		t.Errorf("the resumed executor's write asked %v, want the branch resolved again, before it", got)
	}
}

// suspendedRun is a native executor's run as the suspended-turn cases leave
// it: the engine, its run store, the run's turn id, the two scratch
// repositories its turn wrote — the first before its launch, the second from
// the resumed turn — and the node's running turns, the resumed turn's frame
// still among them until done is called.
type suspendedRun struct {
	pending       sandbox.PendingStore
	turnID        string
	first, second code.RepoRef
	turns         *runningTurns
	done          func()
}

// suspendRun drives a native executor through the code write tools and a
// run_sandbox launch: it writes a scratch repository — held in its memory, it
// has no row — launches and suspends, its frame returning; and the resumed
// executor, built from the row as the engine builds it, writes a second.
func suspendRun(t *testing.T) suspendedRun {
	t.Helper()
	c := specCompany(t, runEnvDoc)
	e, _ := codeWorkRig(t, c)
	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"SWE_GITLAB_TOKEN": "glpat-swe"})
	e.secretsMu.Unlock()
	seat := c.Org.AgentSeatByHandle("swe")
	first, second := scratchOf(seat, "lab"), scratchOf(seat, "lab2")
	host := newCodeWriteHost(first, second)
	commit := codeTool(t, host, builtin.CodeCommitFilesTool).(tools.SeatCallable)

	turn := &turnctx.Turn{RunID: statelog.NewOpID(time.Now(), ""), Seat: seat, Org: c.Org,
		Lookups: turnctx.NewLookups(), Holds: turnctx.NewHolds(nil)}
	turns := &runningTurns{}
	frame := turns.running(turn.Holds)
	if got, err := commit.CallForTurn(t.Context(), turn, commitTo(first.Name)); err != nil || got.Failed {
		t.Fatalf("the executor's write answered %v %q", err, got.Output)
	}
	if _, err := (&launcher{engine: e}).Launch(t.Context(), turn, builtin.SandboxRequest{Brief: "fix the failing test"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	frame() // the turn suspends, and its frame returns
	pending := e.sandbox.Load().pending
	row, found, err := pending.Get(t.Context(), turn.RunID)
	if err != nil || !found {
		t.Fatalf("the launched run's row: found %v, %v", found, err)
	}
	resumed := &turnctx.Turn{RunID: turn.RunID, Seat: seat, Org: c.Org, Lookups: turnctx.NewLookups(),
		Holds: e.rowHolds(row)}
	done := turns.running(resumed.Holds)
	if got, err := commit.CallForTurn(t.Context(), resumed, commitTo(second.Name)); err != nil || got.Failed {
		t.Fatalf("the resumed executor's write answered %v %q", err, got.Output)
	}
	return suspendedRun{pending: pending, turnID: turn.RunID, first: first, second: second, turns: turns,
		done: done}
}

// A SUSPENDED TURN HOLDS THE SCRATCH REPOSITORY IT WROTE: a native executor
// writes a scratch repository — held in its memory, it has no row — and then
// launches run_sandbox and suspends: the launch carries the turn's holds onto
// the row it opens, so once the turn's frame has returned the pass reads the
// repository held from the row. The resumed executor, built from the row,
// writes a second scratch repository, held on the same row before its write.
//
// Mutation: the launch handing the row no holds (LaunchRequest.CodeHolds
// empty) → the first repository is unheld once the turn suspends, red.
func TestASuspendedTurnHoldsTheScratchRepositoryItWrote(t *testing.T) {
	t.Parallel()
	run := suspendRun(t)
	run.done()
	row, _, err := run.pending.Get(t.Context(), run.turnID)
	if err != nil || !slices.Contains(row.CodeHolds, run.first.ID) || !slices.Contains(row.CodeHolds, run.second.ID) {
		t.Fatalf("the run's row holds %v (%v), want %s, which its turn wrote before the launch, and %s, which "+
			"its resumed turn wrote", row.CodeHolds, err, run.first.ID, run.second.ID)
	}
	held := liveHolds(t, run.pending, run.turns, run.first.ID, run.second.ID)
	if !held[run.first.ID] || !held[run.second.ID] {
		t.Errorf("the parked run's scratch repositories read %v, want both held", held)
	}
}

// A SCRATCH REPOSITORY IS EXPIRED ON TIME ONCE ITS RUN ENDS: the run of the
// suspended-turn case ends — its ending decided and its row deleted — and the
// resumed turn's frame returns, so nothing holds either repository any more,
// and the pass after it expires them.
//
// Mutation: the node's registry never forgetting a turn that returned → the
// second repository reads held after the run ended, red.
func TestAScratchRepositoryIsExpiredOnTimeOnceItsRunEnds(t *testing.T) {
	t.Parallel()
	run := suspendRun(t)
	ending, decided, err := run.pending.DecideEnding(t.Context(), run.turnID, sandbox.Decision{
		License: sandbox.License{WhileIn: sandbox.Active, Launch: sandbox.EveryLaunch}})
	if err != nil || !decided || ending.Ending == nil {
		t.Fatalf("decide the run's ending: %v, %v", decided, err)
	}
	if _, finished, err := run.pending.Finish(t.Context(), run.turnID, ending.Ending.ID); err != nil || !finished {
		t.Fatalf("finish the run: %v, %v", finished, err)
	}
	run.done()
	if held := liveHolds(t, run.pending, run.turns, run.first.ID, run.second.ID); len(held) != 0 {
		t.Errorf("after the run ended and its turn returned, %v read held", held)
	}
}

// A RUNNING TURN HOLDS WHAT IT READ, and only while it runs: a native turn
// that reads a scratch repository — last active under the threshold that
// would record the hold, so the hold lives in the turn's memory alone — is
// answered held by its node while its frame runs, and unheld once it returns,
// so the pass after the turn ends expires the repository on time.
//
// Mutations: the read taking no hold → unheld while the turn runs, red; the
// registry keeping a turn that returned → held after it, red.
func TestARunningTurnHoldsWhatItRead(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "SWE"}
	scratch := scratchOf(seat, "lab")
	scratch.LastActivityAt = time.Now().Add(-20 * time.Hour)
	host := newCodeWriteHost(scratch)
	read := codeTool(t, host, builtin.CodeGetRepoTool).(tools.SeatCallable)
	turn := &turnctx.Turn{RunID: statelog.NewOpID(time.Now(), ""), Seat: seat,
		Lookups: turnctx.NewLookups(), Holds: turnctx.NewHolds(nil)}
	turns := &runningTurns{}
	done := turns.running(turn.Holds)
	if got, err := read.CallForTurn(t.Context(), turn, map[string]any{"repo": scratch.Name}); err != nil || got.Failed {
		t.Fatalf("the read answered %v %q", err, got.Output)
	}
	host.mu.Lock()
	recorded := slices.Clone(host.lifecycles)
	host.mu.Unlock()
	if len(recorded) != 0 {
		t.Errorf("a repository last active 20 h ago was recorded: %+v", recorded)
	}
	if held := liveHolds(t, listedRuns(nil), turns, scratch.ID); !held[scratch.ID] {
		t.Errorf("a running turn's read scratch repository %s reads unheld", scratch.ID)
	}
	done()
	if held := liveHolds(t, listedRuns(nil), turns, scratch.ID); held[scratch.ID] {
		t.Errorf("scratch repository %s reads held after the turn that read it returned", scratch.ID)
	}
}
