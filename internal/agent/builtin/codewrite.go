package builtin

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE CODE WRITE TOOLS: a seat's and a person's assistant's changes to the
// company's own repositories, made through a tool rather than a clone — five
// that change a branch (code_commit_files, code_revert, code_cherry_pick,
// code_create_branch, code_delete_branch), each an ENGINE WRITE the code host
// builds on the repository's git node and lands as one push (ADR-0062), and
// five that change a repository (code_create_repo, code_update_repo,
// code_delete_repo, code_restore_repo, code_protect_branch), each one
// LIFECYCLE request a data node decides — and a repository's CHANGES' writes,
// each decided where the change is (codechangewrite.go). Every one is in
// [CodeTools], beside the reads, so a seat's registry and the operator
// catalogue take one list.
//
// # Who may is the repository's to say
//
// The gate admits every principal (`authz.ClassEverybody`), as it admits a
// push at the git transport: whether a caller may move a branch or change a
// repository is its GRANT on that repository (ADR-0058), which the node that
// decides judges from its own chart — the git node and the repository's
// committer for an engine write, the data node for a lifecycle request — and
// which no row keyed on the verb can answer before the repository is read. A
// person writes as the human seat they hold, through their own credential; a
// credential bound to nobody writes nothing, and is told so before anything
// is routed ([code.UnboundDetail]).
//
// # One operation, so a retry is the same write
//
// Every call names its operation by the one rule every family of write tools
// takes ([operations], [opIDFor]): a seat's turn derives it, a request
// carrying its own key derives it from the key, and the operator's assistant
// over MCP is minted one or brings back the one it was answered. An engine
// write's push and commit are pure functions of that operation
// (code.WritePushID), so the same call made again — after an `unknown`, by a
// re-run turn — is the same push, which the repository's committer collapses.
// THE OPERATION'S OBJECT IS BOUNDED: the repository's id for every tool that
// names a repository and the fixed word `new` for a create, never a ref or a
// name, which could carry an id past the 128 bytes of visible ASCII
// (statelog.MaxCallerOpIDBytes) every party to a write holds it to; what the
// call names beyond the repository is in the arguments a turn's id digests
// and an `op_id` is bound to.
//
// # What a seat's write says of itself
//
// A seat's write names the run behind it on its push (code.RunProvenance):
// the turn, the work item it is charged to and the model the call was made on
// ([toolloop.CallModel]) — a native round's, or the model an agent-mode run's
// launch pointed its coding CLI at, so one run's pushes name one model
// whichever path made them.
//
// # A scratch repository a turn names is held first
//
// Every write reaches its repository through [CodeDeps.repo], the reads' one
// resolution, which takes the turn's hold on a scratch repository before
// anything else about it is sent ([HoldRepo]); a create holds the id its
// decide will derive before the create is sent.
//
// # Every answer is the read's kind of answer
//
// Rendered through a [foreignRead], so no string a repository holds — a path,
// a branch, a conflicted file's text — can open or close a fence; a
// conflicted file's lines, its committers' words, go out fenced at trust
// `external`. Every refusal is one of the classes every surface already
// renders (mcp.Refusals), so a person's act transport answers each with the
// status and the sentence it gives every other write.

// The code write tools' names: each is its authority row's action.
const (
	CodeCommitFilesTool   = string(authz.ActionCodeCommitFiles)
	CodeRevertTool        = string(authz.ActionCodeRevert)
	CodeCherryPickTool    = string(authz.ActionCodeCherryPick)
	CodeCreateBranchTool  = string(authz.ActionCodeCreateBranch)
	CodeDeleteBranchTool  = string(authz.ActionCodeDeleteBranch)
	CodeCreateRepoTool    = string(authz.ActionCodeCreateRepo)
	CodeUpdateRepoTool    = string(authz.ActionCodeUpdateRepo)
	CodeDeleteRepoTool    = string(authz.ActionCodeDeleteRepo)
	CodeRestoreRepoTool   = string(authz.ActionCodeRestoreRepo)
	CodeProtectBranchTool = string(authz.ActionCodeProtectBranch)
)

// CodeWrites are the write tools, in the order a catalogue lists them: the
// branch's five, then the repository's five, then a repository's changes'
// ([ChangeWrites], codechangewrite.go). Read by [Register]'s delivery switch,
// [codeAnnotations] and every test that holds the writes apart from the reads.
func CodeWrites() []string {
	return append([]string{CodeCommitFilesTool, CodeRevertTool, CodeCherryPickTool, CodeCreateBranchTool,
		CodeDeleteBranchTool, CodeCreateRepoTool, CodeUpdateRepoTool, CodeDeleteRepoTool, CodeRestoreRepoTool,
		CodeProtectBranchTool}, ChangeWrites()...)
}

// CodeWritesWithheld is the code writes a seat whose role.code.access reads
// tier is never offered: all ten where the tier caps the seat below write
// ([code.Tier.Cap]) — read_only, which is also an unset tier, review, and a
// value no build writes, which caps at nothing — and none at full_access.
//
// NO REPOSITORY'S GRANT COULD ADMIT ONE, whatever the chart relates the seat
// to it by. A seat holds the least of its tier and its relation
// ([code.GrantOf]), so below write it moves no ref anywhere — a push needs
// write, or the full_access tier for the seat's own namespace
// ([code.Grant.MayPush]) — and every lifecycle request a tool sends needs
// maintain, a scratch repository's create full_access itself. Offered, each is
// a tool such a seat reaches for and is refused `not_permitted` on every
// repository, round after round; and each is paid for on every round besides,
// its definition in a catalogue nothing caches ([codeCatalogueBudget] in the
// tests) and the executor's guidance on changing code gated on holding them —
// by most of a company's seats, read_only being the default.
//
// THE TIER, NOT THE CHART, because only the tier caps a seat on every
// repository at once: a relation is one repository's, and a seat that writes
// one repository and reads the rest is offered the writes for the one. Asked
// per turn beside the authority table's refusals, over the epoch the turn is
// pinned to (internal/engine's Company.withheldFrom), so a seat raised to
// full_access is offered them from its next turn — and NOT on the authority
// table: a role grant there is one only an administrator sets
// (authz.SeatGrant), while a unit's lead sets its seats' tier.
func CodeWritesWithheld(tier code.Tier) []string {
	if tier.Cap() >= code.AccessWrite {
		return nil
	}
	return CodeWrites()
}

// CodeWriter is the code host's writes as these tools ask them —
// estate.CodeSeam, declared here by its consumer: an engine write asked of
// the repository's home git node, a lifecycle request asked of a data node,
// and a write of a change — its lifecycle asked of the repository's
// committer, its conversation of the change writer beside it — and a person's
// marks over their own code notices, asked of any data node.
type CodeWriter interface {
	GitWrite(ctx context.Context, w code.EngineWrite) (code.EngineWriteResult, error)
	Lifecycle(ctx context.Context, r code.LifecycleRequest) (code.LifecycleResult, error)
	Change(ctx context.Context, p code.ChangeProposal) (code.ChangeResult, error)
	Converse(ctx context.Context, w code.ConversationWrite) (code.ConversationResult, error)
	Mark(ctx context.Context, w code.MarkWrite) (code.MarkResult, error)
}

// codeWriteTools are the writes over deps, in [CodeWrites]' order.
func codeWriteTools(deps CodeDeps) []tools.Callable {
	return append([]tools.Callable{
		&codeBranchWrite{deps: deps, kind: code.WriteCommitFiles},
		&codeBranchWrite{deps: deps, kind: code.WriteRevert},
		&codeBranchWrite{deps: deps, kind: code.WriteCherryPick},
		&codeBranchWrite{deps: deps, kind: code.WriteCreateBranch},
		&codeBranchWrite{deps: deps, kind: code.WriteDeleteBranch},
		&codeRepoWrite{deps: deps, tool: CodeCreateRepoTool},
		&codeRepoWrite{deps: deps, tool: CodeUpdateRepoTool},
		&codeRepoWrite{deps: deps, tool: CodeDeleteRepoTool},
		&codeRepoWrite{deps: deps, tool: CodeRestoreRepoTool},
		&codeRepoWrite{deps: deps, tool: CodeProtectBranchTool},
	}, changeWriteTools(deps)...)
}

// ---- who writes ------------------------------------------------------------ //

// writeCaller is who this call writes as, or the refusal it is answered with:
// [CodeDeps.asker]'s caller, held to the one rule every write is — a token
// bound to nobody writes nothing, refused before anything is read or routed.
//
// NO REQUESTER CEILING HERE: a seat's write is decided by its own grant on the
// repository, at the nodes that decide it. Whether a seat's code writes are
// decided again for whoever woke its turn is a hardening the user decides
// (M3 U40), built apart from these tools.
func (d CodeDeps) writeCaller(ctx context.Context, turn *turnctx.Turn, tool string) (codeCaller, *tools.Result) {
	actor, asker, refusal := d.asker(ctx, turn, tool)
	if refusal != nil {
		return codeCaller{}, refusal
	}
	if asker.Kind == code.PrincipalOperator && asker.Seat == "" {
		r := refusedBy(tools.RefusalForbidden, code.ErrNotPermitted, fmt.Sprintf("%s wrote nothing: %s.",
			tool, code.UnboundDetail))
		return codeCaller{}, &r
	}
	return codeCaller{actor: actor, asker: asker}, nil
}

// writer is the principal a write is made by: the caller as a repository's
// grant judges them and, for a seat, the run behind the call — its turn, the
// work item it is charged to and the model the call was made on — each label
// copied from outside the code domain fitted rather than refused
// ([code.FitProvenance]), so a vendor's long key never refuses a seat's write.
func writer(ctx context.Context, turn *turnctx.Turn, c codeCaller) code.Principal {
	p := c.asker
	if p.Kind != code.PrincipalAgent || turn == nil {
		return p
	}
	p.Run = code.RunProvenance{TurnID: turn.RunID, Model: code.FitProvenance(toolloop.CallModel(ctx))}
	if item := turn.WorkItem; item != nil {
		p.Run.WorkBackend = code.FitProvenance(string(item.Backend))
		p.Run.WorkKey = code.FitProvenance(item.Key)
	}
	return p
}

// writeFresh is the floor an engine write of repo is built at: a seat's
// session at the furthest of this node's own writes to the repository, the
// turn's own read of it (the resolution's position), and where every ref the
// turn has resolved on it last moved ([refsRead]) — this call's own revisions
// among them, resolved before the floor is read, as [CodeDeps.gitFresh] floors
// a read at them — so the git node's copy holds what the turn has seen: the
// head it writes onto, read by code_resolve after the head moved, and every
// commit it names by a ref. Floored short of them, the git node answers a
// session sync from a copy that lacks them, finds the head stale or the commit
// absent, and pays the write's re-judgement at the log's head — a barrier and
// a second sync, and before it, for a stale head, a data node's read of the
// write's own outcome — where one sync would have done. A person's is the
// log's head.
func (d CodeDeps) writeFresh(turn *turnctx.Turn, c codeCaller, repo code.RepoRef) statelog.Freshness {
	if c.asker.Kind != code.PrincipalAgent {
		return statelog.Freshness{Level: statelog.ReadLinearizable}
	}
	floor := furthestOnLog(furthestOnLog(d.Code.FloorOf(repo.ID), repo.Position), refsReadOn(turn, repo.ID))
	return statelog.FloorAt(statelog.ReadSession, floor)
}

// bindOperation settles which operation this call is, by the rule every family
// of write tools takes ([operations.bindActor]).
func (d CodeDeps) bindOperation(actor Actor, tool string, args map[string]any) (Actor, string) {
	return d.ops.bindActor(actor, tool, args)
}

// The verbs an operation's id names a write by: one for every engine write —
// its kind is in the arguments the id digests — and one per lifecycle kind.
const codeWriteVerb = "code-write"

// lifecycleVerb is the verb a lifecycle request's operation is named by.
func lifecycleVerb(kind code.LifecycleKind) string { return "code-" + string(kind) }

// newRepoObject is the object a create's operation is named for: the fixed
// word, never the name it asks for, which a turn's id digests with the rest of
// the arguments ([createObjectKey]'s rule).
const newRepoObject = "new"

// ---- the engine writes ------------------------------------------------------ //

// codeBranchWrite is one of the five engine writes, by its kind.
type codeBranchWrite struct {
	deps CodeDeps
	kind code.EngineWriteKind
}

var _ tools.SeatCallable = (*codeBranchWrite)(nil)

func (t *codeBranchWrite) Name() string {
	switch t.kind {
	case code.WriteCommitFiles:
		return CodeCommitFilesTool
	case code.WriteRevert:
		return CodeRevertTool
	case code.WriteCherryPick:
		return CodeCherryPickTool
	case code.WriteCreateBranch:
		return CodeCreateBranchTool
	}
	return CodeDeleteBranchTool
}

func (t *codeBranchWrite) Description() string {
	switch t.kind {
	case code.WriteCommitFiles:
		return "Commit a small change to a branch's files, built on the server as one push — anything to build " +
			"or test belongs in a checkout. `files`, one operation a path: `put` (`content`, or `blob_oid` of a " +
			"file the repository holds; `mode` 100755 executable, 120000 a link to `content`), `edit` (`old`, " +
			"which must occur exactly once, and `new`), `delete`, `move` (`to`), `mode`."
	case code.WriteRevert:
		return "Commit the revert of one commit onto a branch. On a conflict nothing is committed, and each " +
			"path's stages are answered by id for " + CodeReadTool + "."
	case code.WriteCherryPick:
		return "Commit one commit's change onto a branch. On a conflict nothing is committed, and each path's " +
			"stages are answered by id for " + CodeReadTool + "."
	case code.WriteCreateBranch:
		return "Create a branch at a commit."
	}
	return "Delete a branch at the head you read; its commits stay in the history."
}

// THE SCHEMAS ARE TERSE ON PURPOSE: a seat of a company hosting code carries
// every one of them on every round of every turn, uncached, so each argument
// says only what its name and type do not ([codeCatalogueBudget] in the
// tests holds the bytes), and a revision is described once, in code_resolve's
// description ([codeRevArg]).
func (t *codeBranchWrite) Parameters() map[string]any {
	props := map[string]any{
		"repo":   codeRepoArg(),
		"branch": map[string]any{"type": "string", "description": "Its name under refs/heads/."},
	}
	required := []string{"repo", "branch"}
	head := map[string]any{"type": "string", "description": "The branch's head, as " + CodeResolveTool + " answered it."}
	switch t.kind {
	case code.WriteCommitFiles:
		head["description"] = "The branch's head, as " + CodeResolveTool + " answered it; left out, the call " +
			"creates the branch from `base`."
		props["expected_head"] = head
		props["base"] = codeRevArg("A new branch's start")
		props["message"] = map[string]any{"type": "string"}
		props["files"] = map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"op":       map[string]any{"type": "string", "enum": fileOpNames()},
					"path":     map[string]any{"type": "string"},
					"content":  map[string]any{"type": "string"},
					"blob_oid": map[string]any{"type": "string"},
					"old":      map[string]any{"type": "string"},
					"new":      map[string]any{"type": "string"},
					"to":       map[string]any{"type": "string"},
					"mode": map[string]any{"type": "string",
						"enum": []string{code.ModeFile, code.ModeExecutable, code.ModeLink}},
				},
				"required": []string{"op", "path"},
			},
		}
		required = append(required, "message", "files")
	case code.WriteRevert, code.WriteCherryPick:
		props["expected_head"] = head
		props["commit"] = codeRevArg("The commit")
		props["message"] = map[string]any{"type": "string", "description": "In place of git's own."}
		required = append(required, "expected_head", "commit")
	case code.WriteCreateBranch:
		props["at"] = codeRevArg("Where it starts")
		required = append(required, "at")
	case code.WriteDeleteBranch:
		props["expected_head"] = head
		required = append(required, "expected_head")
	}
	return t.deps.ops.param(map[string]any{"type": "object", "properties": props, "required": required})
}

// fileOpNames is the file operations' names, as the schema's enum lists them.
func fileOpNames() []string {
	var out []string
	for _, k := range code.FileOpKinds() {
		out = append(out, string(k))
	}
	return out
}

func (t *codeBranchWrite) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeBranchWrite) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	tool := t.Name()
	c, refusal := t.deps.writeCaller(ctx, turn, tool)
	if refusal != nil {
		return *refusal, nil
	}
	// EVERY ARGUMENT FIRST, so a malformed call costs no read and mints no
	// operation.
	name, refusal := repoArg(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	w, revs, refusal := t.args(args)
	if refusal != nil {
		return *refusal, nil
	}
	actor, bound := t.deps.bindOperation(c.actor, tool, args)
	if bound != "" {
		return failed(bound), nil
	}
	c.actor = actor
	repo, refusal := t.deps.repo(ctx, turn, tool, c, name)
	if refusal != nil {
		return *refusal, nil
	}
	if refusal := t.resolve(ctx, turn, c, repo, &w, revs); refusal != nil {
		return *refusal, nil
	}
	// THE BRANCH'S HOLD, before the write is sent: a write naming its head
	// is held as a read is, and a branch it creates with no read.
	if refusal := t.deps.branch(ctx, turn, tool, c, repo, w.Ref, w.ExpectedHead != ""); refusal != nil {
		return *refusal, nil
	}
	w.RepoID, w.Principal, w.Fresh = repo.ID, writer(ctx, turn, c), t.deps.writeFresh(turn, c, repo)
	w.OpID = opIDFor(actor, tool, codeWriteVerb, repo.ID, args)
	if err := w.Validate(); err != nil {
		return codeWriteFailure(ctx, tool, actor, repo.Name, err), nil
	}
	res, err := t.deps.Writer.GitWrite(ctx, w)
	if err != nil {
		return t.deps.failedWrite(ctx, turn, tool, c, repo, w.OpID, err), nil
	}
	return t.answer(ctx, turn, c, repo, w, res)
}

// revArgs are a write's revision arguments the git node takes by whole id: a
// commit_files create's base, a revert's or a cherry-pick's commit, and a
// create_branch's target — each resolved where it names a ref.
type revArgs struct{ base, commit string }

// args is the write a call's arguments ask for — every field but the
// repository's and the operation's, which the call resolves and binds — or
// the refusal of an argument no write could take.
func (t *codeBranchWrite) args(args map[string]any) (code.EngineWrite, revArgs, *tools.Result) {
	tool := t.Name()
	branch := strings.TrimPrefix(strings.TrimSpace(argString(args, "branch")), branchRoot)
	if !gitref.ValidBranch(branch) {
		r := failed(fmt.Sprintf("%s needs `branch`: a branch's name as refs/heads/ holds it (`main`, "+
			"`seats/<handle>/fix`) — %q is not one git takes.", tool, clip(branch)))
		return code.EngineWrite{}, revArgs{}, &r
	}
	w := code.EngineWrite{Kind: t.kind, Ref: branchRoot + branch,
		ExpectedHead: strings.TrimSpace(argString(args, "expected_head")),
		Message:      argString(args, "message")}
	var revs revArgs
	switch t.kind {
	case code.WriteCommitFiles:
		revs.base = strings.TrimSpace(argString(args, "base"))
		files, refusal := fileOpsArg(tool, args)
		if refusal != nil {
			return code.EngineWrite{}, revArgs{}, refusal
		}
		w.Files = files
	case code.WriteRevert, code.WriteCherryPick, code.WriteCreateBranch:
		arg := "commit"
		if t.kind == code.WriteCreateBranch {
			arg = "at"
		}
		rev, refusal := revArg(tool, arg, args)
		if refusal != nil {
			return code.EngineWrite{}, revArgs{}, refusal
		}
		revs.commit = rev
	}
	return w, revs, nil
}

// fileOpsArg is a commit_files' operations, each read field by field, or the
// refusal of one no write could take. A `content` present and empty is an
// empty file, so presence is read apart from the value.
func fileOpsArg(tool string, args map[string]any) ([]code.FileOp, *tools.Result) {
	raw, _ := args["files"].([]any)
	if len(raw) == 0 {
		r := failed(fmt.Sprintf("%s needs `files`: one operation a path — put, edit, delete, move or mode.", tool))
		return nil, &r
	}
	out := make([]code.FileOp, 0, len(raw))
	for i, entry := range raw {
		m, ok := entry.(map[string]any)
		if !ok {
			r := failed(fmt.Sprintf("%s: files[%d] is not an object of `op`, `path` and the fields its op "+
				"takes.", tool, i))
			return nil, &r
		}
		op := code.FileOp{Op: code.FileOpKind(argString(m, "op")), Path: argString(m, "path"),
			BlobOID: argString(m, "blob_oid"), Old: argString(m, "old"), New: argString(m, "new"),
			To: argString(m, "to"), Mode: argString(m, "mode")}
		if content, present := m["content"]; present {
			text, ok := content.(string)
			if !ok {
				r := failed(fmt.Sprintf("%s: files[%d].content is the file's text; a binary file is put by "+
					"`blob_oid` or made in run_sandbox.", tool, i))
				return nil, &r
			}
			op.Content = &text
		}
		out = append(out, op)
	}
	return out, nil
}

// resolve fills the write's commits from its revision arguments: a whole id
// as given; a branch, a tag, a ref or HEAD through the call's one ref read
// ([CodeDeps.resolveRevs]); and an abbreviated id refused by the rule every
// party holds a write to, which names whole ids only. A ref it resolves is
// one the turn has read, and the write is floored where it moved
// ([CodeDeps.writeFresh]).
func (t *codeBranchWrite) resolve(ctx context.Context, turn *turnctx.Turn, c codeCaller, repo code.RepoRef,
	w *code.EngineWrite, revs revArgs) *tools.Result {

	var named []string
	for _, rev := range []string{revs.base, revs.commit} {
		if rev != "" {
			named = append(named, rev)
		}
	}
	if len(named) == 0 {
		return nil
	}
	resolved, refusal := t.deps.resolveRevs(ctx, turn, t.Name(), c, repo, named...)
	if refusal != nil {
		return refusal
	}
	i := 0
	if revs.base != "" {
		w.Base, i = resolved[i].oid, i+1
	}
	if revs.commit != "" {
		w.Commit = resolved[i].oid
	}
	return nil
}

// codeBranchAnswer is an engine write's answer: the change it made, or the
// refusal of it with — for a conflict — each conflicted path.
type codeBranchAnswer struct {
	Repo          string           `json:"repo"`
	RenamedTo     string           `json:"renamed_to,omitempty"`
	RedirectUntil time.Time        `json:"redirect_until,omitzero"`
	Branch        string           `json:"branch"`
	Outcome       statelog.Outcome `json:"outcome,omitempty"`
	Commit        string           `json:"commit,omitempty"`
	Previous      string           `json:"previous,omitempty"`
	Position      string           `json:"position,omitempty"`
	Refusal       string           `json:"refusal,omitempty"`
	Detail        string           `json:"detail,omitempty"`
	Conflicts     []code.Conflict  `json:"conflicts,omitempty"`
	// ConflictsOmitted counts the conflicted paths this answer leaves out:
	// those past the git node's page and those past one tool answer.
	ConflictsOmitted int    `json:"conflicts_omitted,omitempty"`
	OpID             string `json:"op_id,omitempty"`
}

// answer is what an engine write came to, as this tool says it: the change,
// or the refusal of the change or of its push in its class, or an outcome
// unknown, which the same call made again finds out.
func (t *codeBranchWrite) answer(ctx context.Context, turn *turnctx.Turn, c codeCaller, repo code.RepoRef,
	w code.EngineWrite, res code.EngineWriteResult) (tools.Result, error) {

	tool := t.Name()
	out := codeBranchAnswer{Repo: repo.Name, RenamedTo: repo.Redirect, RedirectUntil: repo.RedirectUntil,
		Branch: strings.TrimPrefix(w.Ref, branchRoot), OpID: c.actor.Operation}
	switch {
	case res.Refusal == code.WriteConflict:
		return t.conflicted(ctx, turn, c, out, res)
	case res.Refusal != "":
		return refusedBy(tools.RefusalInvalid, writeRefusalError(string(res.Refusal), res.Detail),
			fmt.Sprintf("%s changed nothing: %s (%s). The same call is refused again; change what it asks for.",
				tool, res.Detail, res.Refusal)), nil
	case res.Outcome == statelog.OutcomeApplied, res.Outcome == statelog.OutcomePending:
		out.Outcome, out.Commit, out.Position = res.Outcome, res.Commit, positionText(res.Position)
		out.Previous = res.Command.Old
		if out.Previous == code.ZeroOID {
			out.Previous = ""
		}
	case res.Outcome == statelog.OutcomeUnknown:
		return unknownWrite(c.actor, tool, fmt.Sprintf("the write to %s on %s landed", out.Branch, repo.Name),
			w.OpID, false, unknownNext(false, sameCall(c.actor, tool),
				fmt.Sprintf("Read the branch with %s", CodeResolveTool),
				"the same change would be made twice")), nil
	case res.Command.Result == code.ResultNoop:
		// THE BRANCH ALREADY STOOD WHERE THE CALL ASKED: nothing to land.
		out.Outcome, out.Commit = statelog.OutcomeApplied, res.Command.New
	case res.Command.Result == code.ResultNG:
		return pushRefused(ctx, tool, repo.Name, out.Branch, res.Command, c.actor), nil
	default:
		return faulted(ctx, tool, fmt.Errorf("builtin: the code host answered a write with no outcome and no "+
			"refusal: %+v", res), fmt.Sprintf("%s could not tell what became of the write: %s.", tool, faultSaid)), nil
	}
	return newForeignRead(turn, c.asker.Seat).page(ctx, tool, out, "")
}

// conflicted is a revert's or a cherry-pick's conflict: nothing was stored,
// and each conflicted path goes out with its stages' ids and the start of
// the file as git marked it, FENCED — a repository's text is its
// committers' — as many as fit one answer, the rest counted.
func (t *codeBranchWrite) conflicted(ctx context.Context, turn *turnctx.Turn, c codeCaller,
	out codeBranchAnswer, res code.EngineWriteResult) (tools.Result, error) {

	tool := t.Name()
	out.Refusal = string(code.WriteConflict)
	out.Detail = fmt.Sprintf("%s Nothing was committed. Read each path's stages with %s by the ids "+
		"below (base, ours — the branch — and theirs), make the change yourself, and commit it with %s.",
		strings.TrimSuffix(strings.TrimSpace(res.Detail), ".")+".", CodeReadTool, CodeCommitFilesTool)
	// EACH PAGE ITS OWN FENCES: a fence registered for a conflict a page
	// leaves out is one the render refuses (the fail-closed rule), so every
	// candidate page is rendered by a read that fences exactly what it holds.
	render := func(k int) (json.RawMessage, error) {
		read := newForeignRead(turn, c.asker.Seat)
		for i, conflict := range res.Conflicts[:k] {
			read.fence(fieldPath{}.key("conflicts").index(i).key("marked"), provenance.SourceRepoFile, nil, false,
				conflict.Marked)
		}
		paged := out
		paged.Conflicts = res.Conflicts[:k]
		paged.ConflictsOmitted = res.ConflictsOmitted + len(res.Conflicts) - k
		return read.render(paged)
	}
	rendered, err := fitPages(len(res.Conflicts), render)
	if err != nil {
		return labelFault(ctx, tool, err), nil
	}
	return tools.Result{Output: string(rendered), Failed: true, Cause: tools.Classify(tools.RefusalInvalid,
		writeRefusalError(string(code.WriteConflict), res.Detail))}, nil
}

// fitPages is the largest page of the first k of n entries whose rendering
// fits one tool answer — every entry where all of them fit, and otherwise a
// binary search over k — as [foreignRead.fit] cuts a listing, for a page whose
// every candidate renders on its own.
func fitPages(n int, render func(k int) (json.RawMessage, error)) (json.RawMessage, error) {
	whole, err := render(n)
	if err != nil || len(whole) <= ToolAnswerBytes {
		return whole, err
	}
	kept, err := render(0)
	if err != nil {
		return nil, err
	}
	for fits, over := 0, n; over-fits > 1; {
		mid := fits + (over-fits)/2
		rendered, err := render(mid)
		if err != nil {
			return nil, err
		}
		if len(rendered) <= ToolAnswerBytes {
			fits, kept = mid, rendered
		} else {
			over = mid
		}
	}
	return kept, nil
}

// ---- the lifecycle tools -------------------------------------------------- //

// codeRepoWrite is one of the five lifecycle tools, by its name.
type codeRepoWrite struct {
	deps CodeDeps
	tool string
}

var _ tools.SeatCallable = (*codeRepoWrite)(nil)

func (t *codeRepoWrite) Name() string { return t.tool }

func (t *codeRepoWrite) Description() string {
	switch t.tool {
	case CodeCreateRepoTool:
		return "Create a repository: a project a unit owns, or your own scratch repository for an experiment, " +
			"`<your handle>.<name>`, which expires once it sits idle."
	case CodeUpdateRepoTool:
		return "Change a repository, one thing a call: its settings, or `archived` (read-only, and back), or " +
			"`rename_to` (the old name redirects for a while)."
	case CodeDeleteRepoTool:
		return "Delete a repository; " + CodeRestoreRepoTool + " brings it back until its purge."
	case CodeRestoreRepoTool:
		return "Restore a deleted repository as it was."
	}
	return "Set the protection of the branches or tags a pattern names, or take it away (`remove`). A call " +
		"sets direct_push, allow_force and allow_delete whole; a merge gate field changes that field alone, " +
		"the rest of the branch's gate kept (`remove_gate` drops it)."
}

// ownerShape is how an owner argument is spelled, as the read tools answer
// owners.
const ownerShape = "{\"kind\": \"unit\" or \"seat\", \"ref\": its key or handle}"

func (t *codeRepoWrite) Parameters() map[string]any {
	owner := map[string]any{"type": "object", "description": ownerShape}
	visibility := map[string]any{"type": "string",
		"enum": []string{string(code.VisibilityCompany), string(code.VisibilityUnit)}}
	props := map[string]any{"repo": codeRepoArg()}
	required := []string{"repo"}
	switch t.tool {
	case CodeCreateRepoTool:
		props = map[string]any{
			"name":           map[string]any{"type": "string"},
			"owner":          map[string]any{"type": "object", "description": "A project's: " + ownerShape},
			"description":    map[string]any{"type": "string"},
			"default_branch": map[string]any{"type": "string", "description": "`main` when left out."},
			"visibility":     visibility,
		}
		required = []string{"name"}
	case CodeUpdateRepoTool:
		props["description"] = map[string]any{"type": "string"}
		props["default_branch"] = map[string]any{"type": "string"}
		props["visibility"] = visibility
		props["search"] = map[string]any{"type": "string", "enum": spelled(code.SearchModes())}
		props["owner"] = owner
		props["path_owners"] = map[string]any{"type": "object",
			"description": "Globs given to or taken from a project's owners: {\"add\": [{\"owner\": …, " +
				"\"paths\": [glob]}], \"remove\": [the same]}."}
		props["archived"] = map[string]any{"type": "boolean"}
		props["rename_to"] = map[string]any{"type": "string"}
	case CodeDeleteRepoTool:
		props["reason"] = map[string]any{"type": "string"}
	case CodeProtectBranchTool:
		props["pattern"] = map[string]any{"type": "string",
			"description": "refs/heads/<glob> or refs/tags/<glob>; `*` within a name, `**` across them."}
		props["direct_push"] = map[string]any{"type": "string",
			"enum":        []string{string(code.DirectPersons), string(code.DirectNone)},
			"description": "A person's alone (also when left out), or nobody's."}
		props["allow_force"] = map[string]any{"type": "boolean"}
		props["allow_delete"] = map[string]any{"type": "boolean"}
		props["remove"] = map[string]any{"type": "boolean"}
		gateParams(props)
		props["remove_gate"] = map[string]any{"type": "boolean"}
		required = []string{"repo", "pattern"}
	}
	return t.deps.ops.param(map[string]any{"type": "object", "properties": props, "required": required})
}

// gateParams puts a branch rule's merge gate fields in a protect call's
// schema, each by the name the code host's change spells it
// ([code.GateChangeFields]), so a call takes exactly the fields a change
// names, and each says nothing beyond its type and values: what a field does
// is the code host's to document, and every code tool's schema rides every
// round of a seat's turn ([codeCatalogueBudget] in the tests).
func gateParams(props map[string]any) {
	enum := func(values []string) map[string]any { return map[string]any{"type": "string", "enum": values} }
	list := func(item map[string]any) map[string]any { return map[string]any{"type": "array", "items": item} }
	classes := list(enum(spelled(code.PathClasses())))
	props["merge_by"] = enum(spelled(code.MergeBys()))
	props["approvals"] = enum(spelled(code.ApprovalCounts()))
	props["approvals_unless"] = classes
	props["human_paths"] = list(map[string]any{"type": "object", "properties": map[string]any{
		"glob": map[string]any{"type": "string"}, "unless": classes}, "required": []string{"glob"}})
	props["checks"] = list(map[string]any{"type": "string"})
	props["landing"] = map[string]any{"type": "object", "properties": map[string]any{
		"policy": enum(spelled(code.LandingPolicies())),
		"depth":  map[string]any{"type": "integer"}, "batch": map[string]any{"type": "integer"}}}
	props["delete_source"] = enum(spelled(code.DeleteSources()))
	for _, flag := range []string{"owners_approve", "auto_revert", "agents_edit_checks"} {
		props[flag] = map[string]any{"type": "boolean"}
	}
}

// spelled is a closed set's values as a schema's enum lists them.
func spelled[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return out
}

func (t *codeRepoWrite) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeRepoWrite) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.writeCaller(ctx, turn, t.tool)
	if refusal != nil {
		return *refusal, nil
	}
	// EVERY ARGUMENT FIRST, so a malformed call costs no read and mints no
	// operation.
	req, refusal := t.request(args)
	if refusal != nil {
		return *refusal, nil
	}
	var name string
	if req.Kind != code.LifecycleCreate {
		if name, refusal = repoArg(t.tool, args); refusal != nil {
			return *refusal, nil
		}
	}
	actor, bound := t.deps.bindOperation(c.actor, t.tool, args)
	if bound != "" {
		return failed(bound), nil
	}
	c.actor = actor
	req.Actor = c.asker
	var repo code.RepoRef
	if req.Kind == code.LifecycleCreate {
		req.OpID = opIDFor(actor, t.tool, lifecycleVerb(req.Kind), newRepoObject, args)
		id, ok := code.RepoIDOf(req.OpID)
		if !ok {
			return faulted(ctx, t.tool, fmt.Errorf("builtin: operation %s carries no instant", req.OpID),
				fmt.Sprintf("%s could not name the repository it would create: %s.", t.tool, faultSaid)), nil
		}
		// THE HOLD BEFORE THE CREATE, on the id its decide will derive: a
		// scratch repository a turn creates is held from before it exists.
		// A new repository is not idle, so nothing is recorded.
		repo = code.RepoRef{ID: id, Name: req.Name, Kind: code.KindOf(req.Name)}
		if err := HoldRepo(ctx, turn, t.deps.Holder, actor, repo, t.deps.Now); err != nil {
			return holdFailure(ctx, t.tool, repo, err), nil
		}
	} else {
		if repo, refusal = t.deps.repo(ctx, turn, t.tool, c, name); refusal != nil {
			return *refusal, nil
		}
		req.RepoID = repo.ID
		req.OpID = opIDFor(actor, t.tool, lifecycleVerb(req.Kind), repo.ID, args)
	}
	if err := req.Validate(); err != nil {
		return codeWriteFailure(ctx, t.tool, actor, cmp.Or(repo.Name, req.Name), err), nil
	}
	res, err := t.deps.Writer.Lifecycle(ctx, req)
	if err != nil {
		return t.deps.failedWrite(ctx, turn, t.tool, c, repo, req.OpID, err), nil
	}
	return t.answer(ctx, turn, c, repo, req, res)
}

// request is the lifecycle request a call's arguments ask for — every field
// but the operation, the actor and the repository's id, which the call binds
// and resolves — or the refusal of an argument no request could take.
func (t *codeRepoWrite) request(args map[string]any) (code.LifecycleRequest, *tools.Result) {
	tool := t.tool
	switch tool {
	case CodeCreateRepoTool:
		return createRequest(tool, args)
	case CodeUpdateRepoTool:
		return updateRequest(tool, args)
	case CodeDeleteRepoTool:
		return code.LifecycleRequest{Kind: code.LifecycleDelete, Reason: strings.TrimSpace(argString(args, "reason"))}, nil
	case CodeRestoreRepoTool:
		return code.LifecycleRequest{Kind: code.LifecycleRestore}, nil
	}
	pattern := strings.TrimSpace(argString(args, "pattern"))
	if !gitref.ValidPattern(pattern) {
		r := failed(fmt.Sprintf("%s needs `pattern`: refs/heads/<glob> or refs/tags/<glob> — %q is not one.",
			tool, clip(pattern)))
		return code.LifecycleRequest{}, &r
	}
	change := code.ProtectChange{Pattern: pattern}
	if !argBool(args, "remove") {
		// A DIRECT_PUSH LEFT OUT IS PERSONS, as code.repos reads an unset
		// one: every rule holds at least a seat's direct push, so no value
		// of it means "anyone who may write" — what a ref no rule matches
		// already means — and a rule carrying none is one no record may
		// carry, refused by the request's own validation. Like allow_force
		// and allow_delete left out, it is the rule's least: the call sets
		// the pattern's push controls whole.
		direct := cmp.Or(strings.TrimSpace(argString(args, "direct_push")), string(code.DirectPersons))
		change.Rule = &code.Protection{PushRule: code.PushRule{Pattern: pattern, DirectPush: code.DirectPush(direct),
			AllowForce: argBool(args, "allow_force"), AllowDelete: argBool(args, "allow_delete")}}
	}
	// THE MERGE GATE AS A CHANGE, never whole, sent beside a removal too, so
	// the request's own validation refuses the two together rather than this
	// tool dropping what its caller said.
	gate, refusal := gateChangeArg(tool, args)
	if refusal != nil {
		return code.LifecycleRequest{}, refusal
	}
	change.Gate = gate
	return code.LifecycleRequest{Kind: code.LifecycleProtect, Protect: &change}, nil
}

// gateChangeArg is the change to a rule's merge gate a protect call's
// arguments name — nil where they name no gate field and do not remove the
// gate — or the refusal of a field no change could take. Every field it
// names is decoded by the code host's own spelling of a change
// ([code.GateChange], [code.GateChangeFields]), so the call takes exactly the
// fields a change has, each to REPLACE that field alone: the deciding data
// node keeps every other field as the rule's gate holds it, since only it
// reads the gate in the snapshot it decides in — a whole gate this tool formed
// from a read made elsewhere would undo whatever changed the gate meanwhile.
func gateChangeArg(tool string, args map[string]any) (*code.GateChange, *tools.Result) {
	named := map[string]any{}
	for _, field := range code.GateChangeFields() {
		if v, ok := args[field]; ok {
			named[field] = v
		}
	}
	remove := argBool(args, "remove_gate")
	if len(named) == 0 && !remove {
		return nil, nil
	}
	var change code.GateChange
	if err := decodeArg(named, &change); err != nil {
		r := failed(fmt.Sprintf("%s: a merge gate field is not as the schema says: %v.", tool, err))
		return nil, &r
	}
	change.Remove = remove
	return &change, nil
}

// createRequest is a create's request from its arguments: a scratch
// repository is its seat's own, whatever `owner` says, and a project names
// its owner.
func createRequest(tool string, args map[string]any) (code.LifecycleRequest, *tools.Result) {
	name := strings.TrimSpace(argString(args, "name"))
	if !code.ValidName(name) {
		r := failed(fmt.Sprintf("%s needs `name`: a project's name — lowercase letters, digits and hyphens — "+
			"or `<your handle>.<name>` for a scratch repository; %q is neither.", tool, clip(name)))
		return code.LifecycleRequest{}, &r
	}
	// A TOOL'S REPOSITORY IS SEARCHED WITH GREP until somebody who maintains
	// it asks for an index (code_update_repo's `search`): the code domain's
	// default for what a tool makes, a scratch repository's for good.
	settings := code.RepoSettings{ManagedBy: code.ManagedTools, Kind: code.KindOf(name),
		Description:   strings.TrimSpace(argString(args, "description")),
		DefaultBranch: cmp.Or(strings.TrimSpace(argString(args, "default_branch")), "main"),
		Visibility: code.Visibility(cmp.Or(strings.TrimSpace(argString(args, "visibility")),
			string(code.VisibilityCompany))),
		Search: code.SearchGrep}
	owner, given, refusal := ownerArg(tool, "owner", args)
	if refusal != nil {
		return code.LifecycleRequest{}, refusal
	}
	if handle, scratch := gitref.ScratchName(name); scratch {
		settings.Owner = code.Owner{Kind: code.OwnerSeat, Ref: handle}
		if given && owner != settings.Owner {
			r := failed(fmt.Sprintf("%s: %s is seat %s's scratch repository, which is its seat's own; leave "+
				"`owner` out.", tool, name, handle))
			return code.LifecycleRequest{}, &r
		}
	} else if !given {
		r := failed(fmt.Sprintf("%s: the project %s needs an `owner` — a unit by its key ({\"kind\": \"unit\", "+
			"\"ref\": \"<key>\"}).", tool, name))
		return code.LifecycleRequest{}, &r
	} else {
		settings.Owner = owner
	}
	return code.LifecycleRequest{Kind: code.LifecycleCreate, Name: name, Create: &settings}, nil
}

// updateRequest is code_update_repo's request: ONE of a settings change, an
// archive or unarchive, or a rename, since each is a record of its own.
func updateRequest(tool string, args map[string]any) (code.LifecycleRequest, *tools.Result) {
	var change code.RepoChange
	if v, ok := args["description"].(string); ok {
		change.Description = &v
	}
	if v, ok := args["default_branch"].(string); ok {
		v = strings.TrimSpace(v)
		change.DefaultBranch = &v
	}
	if v, ok := args["visibility"].(string); ok {
		visibility := code.Visibility(strings.TrimSpace(v))
		change.Visibility = &visibility
	}
	if v, ok := args["search"].(string); ok {
		search := code.SearchMode(strings.TrimSpace(v))
		change.Search = &search
	}
	owner, given, refusal := ownerArg(tool, "owner", args)
	if refusal != nil {
		return code.LifecycleRequest{}, refusal
	}
	if given {
		change.Owner = &owner
	}
	if raw, ok := args["path_owners"]; ok {
		var owners code.PathOwnersChange
		if err := decodeArg(raw, &owners); err != nil {
			r := failed(fmt.Sprintf("%s: `path_owners` is {\"add\": [...], \"remove\": [...]}, each entry an "+
				"`owner` and its `paths`: %v.", tool, err))
			return code.LifecycleRequest{}, &r
		}
		change.PathOwners = &owners
	}
	_, archive := args["archived"].(bool)
	rename := strings.TrimSpace(argString(args, "rename_to"))
	settings := change != (code.RepoChange{})
	asked := 0
	for _, set := range []bool{settings, archive, rename != ""} {
		if set {
			asked++
		}
	}
	if asked != 1 {
		r := failed(fmt.Sprintf("%s changes ONE thing a call: its settings (any of `description`, "+
			"`default_branch`, `visibility`, `search`, `owner`, `path_owners`), `archived`, or `rename_to` — "+
			"each is a record of its own.", tool))
		return code.LifecycleRequest{}, &r
	}
	switch {
	case archive && argBool(args, "archived"):
		return code.LifecycleRequest{Kind: code.LifecycleArchive}, nil
	case archive:
		return code.LifecycleRequest{Kind: code.LifecycleUnarchive}, nil
	case rename != "":
		return code.LifecycleRequest{Kind: code.LifecycleRename, Name: rename}, nil
	}
	return code.LifecycleRequest{Kind: code.LifecycleSettings, Change: &change}, nil
}

// ownerArg is an owner argument — {"kind": "unit"|"seat", "ref": "..."} — and
// whether the call gave one, or the refusal of one no owner is.
func ownerArg(tool, arg string, args map[string]any) (code.Owner, bool, *tools.Result) {
	raw, given := args[arg]
	if !given {
		return code.Owner{}, false, nil
	}
	var o code.Owner
	if err := decodeArg(raw, &o); err != nil || !o.Kind.Valid() || strings.TrimSpace(o.Ref) == "" {
		r := failed(fmt.Sprintf("%s: `%s` is {\"kind\": \"unit\", \"ref\": \"<unit key>\"} or {\"kind\": "+
			"\"seat\", \"ref\": \"<handle>\"}.", tool, arg))
		return code.Owner{}, false, &r
	}
	o.Ref = strings.TrimSpace(o.Ref)
	return o, true, nil
}

// decodeArg reads an argument a schema declares as an object into v, refusing
// a field v does not have.
func decodeArg(raw any, v any) error {
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// codeRepoState is a repository as a lifecycle answer says it stands — never
// its protection rules, which no lifecycle answer carries whole: sixty-four
// rules, each with a merge gate of up to code.MaxGateBytes, are past one tool
// answer, and an answer refused for its size AFTER its write landed would
// read as a write that failed. A protect request's answer carries the one
// rule it set ([codeRepoWriteAnswer.Rule]), and code_get_repo reads them all.
type codeRepoState struct {
	Name          string          `json:"name"`
	Kind          code.RepoKind   `json:"kind"`
	Status        code.RepoStatus `json:"status"`
	Owner         code.Owner      `json:"owner"`
	Visibility    code.Visibility `json:"visibility"`
	Search        code.SearchMode `json:"search"`
	DefaultBranch string          `json:"default_branch"`
	Description   string          `json:"description,omitempty"`
}

// codeRepoWriteAnswer is a lifecycle tool's answer.
type codeRepoWriteAnswer struct {
	Repo          string           `json:"repo"`
	RenamedTo     string           `json:"renamed_to,omitempty"`
	RedirectUntil time.Time        `json:"redirect_until,omitzero"`
	Outcome       statelog.Outcome `json:"outcome"`
	Repository    *codeRepoState   `json:"repository,omitempty"`
	// Rule is a protect request's pattern's rule as it applied — its merge
	// gate as the deciding node RESOLVED it, every field the call left out
	// as the rule's gate held it — and absent where the rule was removed, or
	// where the request is pending: the row a pending answer carries is the
	// one its decide read, before the change.
	Rule     *code.Protection `json:"rule,omitempty"`
	Position string           `json:"position,omitempty"`
	OpID     string           `json:"op_id,omitempty"`
}

// answer is what a lifecycle request came to, as this tool says it.
//
// A REQUEST THAT APPLIED ENDS THE TURN'S RESOLUTION of the repository's name:
// its row moved — a new name, a status, an owner — and the resolution the
// turn keeps would serve the row as it was.
func (t *codeRepoWrite) answer(ctx context.Context, turn *turnctx.Turn, c codeCaller, repo code.RepoRef,
	req code.LifecycleRequest, res code.LifecycleResult) (tools.Result, error) {

	name := cmp.Or(repo.Name, req.Name)
	if res.Refusal != "" {
		return lifecycleRefused(t.tool, name, res), nil
	}
	switch res.Outcome {
	case statelog.OutcomeUnknown:
		return unknownWrite(c.actor, t.tool, fmt.Sprintf("the %s of %s landed", req.Kind, name), req.OpID,
			false, unknownNext(false, sameCall(c.actor, t.tool),
				fmt.Sprintf("Read the repository with %s", CodeGetRepoTool),
				"the same change would be asked twice")), nil
	case statelog.OutcomeApplied, statelog.OutcomePending:
	default:
		return faulted(ctx, t.tool, fmt.Errorf("builtin: the code host answered a lifecycle request with "+
			"outcome %q and no refusal", res.Outcome), fmt.Sprintf("%s could not tell what became of the "+
			"request: %s.", t.tool, faultSaid)), nil
	}
	if req.Kind != code.LifecycleCreate {
		forget(turn, c.asker, repo)
	}
	out := codeRepoWriteAnswer{Repo: name, RenamedTo: repo.Redirect, RedirectUntil: repo.RedirectUntil,
		Outcome: res.Outcome, Position: positionText(res.Position), OpID: c.actor.Operation}
	if r := res.Repo; r.ID != "" {
		out.Repository = &codeRepoState{Name: r.Name, Kind: r.Kind, Status: r.Status, Owner: r.Owner,
			Visibility: r.Visibility, Search: r.Search, DefaultBranch: r.DefaultBranch,
			Description: r.Description}
	}
	if req.Kind == code.LifecycleProtect && res.Outcome == statelog.OutcomeApplied {
		// THE RULE WHOLE, its gate as the deciding node resolved it: the code
		// host answers it beside the row, which carries each rule as a push
		// is held to it and never a gate (code.LifecycleResult.Rule).
		out.Rule = res.Rule
	}
	return newForeignRead(turn, c.asker.Seat).page(ctx, t.tool, out, "")
}

// ---- refusals, in the classes every surface renders ------------------------ //

// writeRefusalError is a refusal the code host answered, as the cause beneath
// a tool's class: its code and its sentence.
func writeRefusalError(refusal, detail string) error {
	return fmt.Errorf("code: %s: %s", refusal, detail)
}

// pushClass is the class a refused push's code is answered in — chosen by
// what each class tells its reader, since a person's surface shows the tool's
// own sentence for `invalid` and `forbidden` alone:
//
//   - a rule of the repository refusing the caller — the grant, a protection,
//     the seat's daily quota, a read-only repository, its bounds — is
//     FORBIDDEN, the sentence naming the rule and, for the quota, when the
//     day turns; never budget_exhausted, which is the company's token budget;
//   - a branch the engine's names refuse, or a pack past what one push may
//     store, is INVALID: the fix is another call;
//   - a head that moved is STALE_VERSION, a repository gone NOT_FOUND, a store
//     with no room STORE_FULL;
//   - what waiting clears — a busy or compacting repository, an expunge that
//     began, a log or a store that refused, a push applied nowhere — is
//     UNAVAILABLE, the same call again being the same write;
//   - and what no engine write can be answered — an --atomic push, a tag moved,
//     a push of more changes than a record holds, a commit claiming the
//     engine's committer (forged_committer, which only a coding box's pack is
//     read for: an engine write's commits are the engine's by right) — is the
//     engine's own fault.
func pushClass(r code.Refusal) (tools.Refusal, bool) {
	switch r {
	case code.RefusalNotPermitted, code.RefusalProtectedForce, code.RefusalProtectedDelete,
		code.RefusalProtectedDirect, code.RefusalPushQuota, code.RefusalRepoArchived, code.RefusalRepoSize,
		code.RefusalRepoRefs, code.RefusalBlobTooLarge, code.RefusalRefConflict:
		return tools.RefusalForbidden, true
	case code.RefusalReservedRef, code.RefusalBadRef, code.RefusalPackTooLarge:
		return tools.RefusalInvalid, true
	case code.RefusalStale:
		return tools.RefusalStaleVersion, true
	case code.RefusalRepoDeleted:
		return tools.RefusalNotFound, true
	case code.RefusalStoreFull:
		return tools.RefusalStoreFull, true
	case code.RefusalBusy, code.RefusalCompactionBehind, code.RefusalUploadExpired, code.RefusalExpunging,
		code.RefusalUnavailable, code.RefusalNotApplied:
		return tools.RefusalUnavailable, true
	}
	return "", false
}

// pushRefused is an engine write whose push the landing refused.
func pushRefused(ctx context.Context, tool, repo, branch string, cmd code.Command, actor Actor) tools.Result {

	detail := cmp.Or(cmd.Detail, cmd.Reason.Sentence())
	cause := writeRefusalError(string(cmd.Reason), detail)
	class, known := pushClass(cmd.Reason)
	if !known {
		return faulted(ctx, tool, cause, fmt.Sprintf("%s could not land the write to %s on %s: the code host "+
			"refused it %s, which no write it builds can be — %s.", tool, branch, repo, cmd.Reason, faultSaid))
	}
	said := fmt.Sprintf("%s did not land the write to %s on %s: %s (%s).", tool, branch, repo, detail, cmd.Reason)
	switch class {
	case tools.RefusalStaleVersion:
		said += fmt.Sprintf(" Read the branch again with %s and make the change on the head it answers.",
			CodeResolveTool)
	case tools.RefusalUnavailable:
		if again := sameCall(actor, tool); again != "" {
			said += " Nothing was committed: " + again + " — it is the same write."
		} else {
			said += " Nothing was committed; try again shortly."
		}
	case tools.RefusalForbidden:
		said += " That is a rule of the repository, so the same call is refused again."
	case tools.RefusalInvalid:
		said += " The same call is refused again; change what it names."
	}
	return refusedBy(class, cause, said)
}

// lifecycleRefused is a lifecycle request the data node refused, in its class:
// a rule of the repository or the company FORBIDDEN, a name taken EXISTS, a
// repository gone NOT_FOUND, and a repository that moved between the decide
// and its apply STALE_VERSION.
func lifecycleRefused(tool, repo string, res code.LifecycleResult) tools.Result {
	cause := writeRefusalError(string(res.Refusal), res.Detail)
	said := fmt.Sprintf("%s changed nothing about %s: %s (%s).", tool, repo, res.Detail, res.Refusal)
	switch res.Refusal {
	case code.LifecycleNameTaken:
		return refusedBy(tools.RefusalExists, cause, said)
	case code.LifecycleRepoDeleted, code.LifecycleRefDeleted:
		// GONE: the repository, or — a hold's alone — the seat's branch
		// it names.
		return refusedBy(tools.RefusalNotFound, cause, said)
	case code.LifecycleStale:
		return refusedBy(tools.RefusalStaleVersion, cause, said+fmt.Sprintf(" Read it again with %s.",
			CodeGetRepoTool))
	}
	return refusedBy(tools.RefusalForbidden, cause, said+" That is a rule of the repository or the company, so "+
		"the same call is refused again.")
}

// failedWrite explains a write the code host could not answer: one whose
// outcome nobody can tell — a node that went silent, which may have landed it
// — is an unknown the same call made again finds out; a repository gone ends
// the turn's resolution of its name ([CodeDeps.failed]); every other answer is
// [codeWriteFailure]'s.
func (d CodeDeps) failedWrite(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	repo code.RepoRef, opID string, err error) tools.Result {

	var unserved *estate.ErrUnserved
	if errors.Is(err, estate.ErrOutcomeUnknown) || errors.As(err, &unserved) && unserved.Silent() {
		return unknownWrite(c.actor, tool, fmt.Sprintf("the write to %s landed", repo.Name), opID, false,
			unknownNext(false, sameCall(c.actor, tool), fmt.Sprintf("Read it with %s", CodeGetRepoTool),
				"the same change would be made twice"))
	}
	if errors.Is(err, code.ErrRepoNotFound) {
		forget(turn, c.asker, repo)
	}
	return codeWriteFailure(ctx, tool, c.actor, repo.Name, err)
}

// codeWriteFailure explains a write refused or unanswered before it was
// decided, each sentinel in its class: a request no node would take INVALID,
// the caller's grant FORBIDDEN, a repository gone NOT_FOUND, and a node or a
// fleet that could not serve it UNAVAILABLE — nothing was written, and the
// same call again is the same write — with anything else this node's fault.
func codeWriteFailure(ctx context.Context, tool string, actor Actor, repo string, err error) tools.Result {
	var (
		unready  *code.ErrGitUnready
		unserved *estate.ErrUnserved
	)
	again := sameCall(actor, tool)
	if again == "" {
		again = "make the same call again"
	}
	switch {
	case errors.Is(err, code.ErrInvalid), errors.Is(err, code.ErrObjectNotFound),
		errors.Is(err, code.ErrInvalidProposal):
		// A PROPOSAL NO NODE WOULD DECIDE is a change write's own: held to
		// the rule its deciding node holds it to before it is routed, it is
		// the call's arguments that are wrong — a title past its bound, a
		// branch no change may name.
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused what it was asked: %s. Change the "+
			"arguments — the same ones are refused again.", tool, writeWords(err)))
	case errors.Is(err, code.ErrNotPermitted), errors.Is(err, code.ErrConfigManaged):
		return refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s: %s. That is a decision about who is "+
			"asking, so the same call is refused again.", tool, writeWords(err)))
	case errors.Is(err, code.ErrRepoNotFound):
		return codeFailure(ctx, tool, repo, err)
	case errors.As(err, &unready), errors.Is(err, code.ErrNotServing), errors.As(err, &unserved):
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s: the code host could not be reached "+
			"right now (no node that builds or decides it could take it in time); nothing was written — %s, "+
			"which is the same write.", tool, again))
	}
	if why, ok := Condition(err); ok {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not write to %s right now (%s); "+
			"nothing is known to have been written — %s, which is the same write.", tool, repo, why, again))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s could not write to %s: %s.", tool, repo, faultSaid))
}

// writeWords is a write refusal's own sentence: the words its domain wrote
// for the caller past the sentinel.
func writeWords(err error) string {
	for _, sentinel := range []error{code.ErrInvalid, code.ErrObjectNotFound, code.ErrNotPermitted,
		code.ErrConfigManaged, code.ErrInvalidProposal} {
		if said, ok := RefusalSentence(err, sentinel); ok {
			return said
		}
	}
	return err.Error()
}
