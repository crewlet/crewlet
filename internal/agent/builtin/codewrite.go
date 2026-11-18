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
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
)

// THE CODE WRITE TOOLS: five branch writes, each an engine write the
// repository's Gitlet node builds and lands as one push (ADR-0092); five
// repository lifecycle requests a data node decides; and a change's writes
// (codechangewrite.go), all in [CodeTools] beside the reads.
//
// The gate admits everybody (`authz.ClassEverybody`): who may write is the
// caller's grant on the repository (ADR-0088), judged by the deciding node
// from its own chart and, in a woken turn, again for the requester
// ([CodeDeps.writeCaller]). An engine write's push is a pure function of its
// operation (package doc, "How a write is made once"; gitlet.WritePushID), so
// a retried call is the same push. Answers render through a [foreignRead], so
// no string a repository holds can break a fence; refusals take [pushClass]'s.

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

// CodeWrites are the write tools in catalogue order: the five branch writes,
// the five lifecycle tools, then [ChangeWrites], [CheckWrites], [PurgeWrites]
// and [ImportWrites]. [Register]'s delivery switch and [codeAnnotations] read it.
func CodeWrites() []string {
	return append([]string{CodeCommitFilesTool, CodeRevertTool, CodeCherryPickTool, CodeCreateBranchTool,
		CodeDeleteBranchTool, CodeCreateRepoTool, CodeUpdateRepoTool, CodeDeleteRepoTool, CodeRestoreRepoTool,
		CodeProtectBranchTool}, append(append(append(ChangeWrites(), CheckWrites()...), PurgeWrites()...),
		ImportWrites()...)...)
}

// CodeWritesWithheld is the code writes never offered a seat whose
// role.code.access tier caps it below write ([gitlet.Tier.Cap]: read_only or
// unset, review, or a value no build writes): all but the change conversation
// writes its cap reaches ([belowWriteNeeds]). No grant could admit one, since
// a seat holds the least of tier and relation ([gitlet.GrantOf]); offered,
// each is refused `not_permitted` every round and paid for in a catalogue
// nothing caches ([codeCatalogueBudget] in the tests). Authorship (rule D21)
// could admit a review ask or a finding's answer to a lowered seat; both stay
// withheld at read_only, or every read_only seat would pay for them. The tier,
// not the chart, since only it caps every repository at once; asked per turn
// (internal/engine's Company.withheldFrom), not on the authority table: a
// unit's lead sets the tier, an administrator alone a role grant.
func CodeWritesWithheld(tier gitlet.Tier) []string {
	limit := tier.Cap()
	if limit >= gitlet.AccessWrite {
		return nil
	}
	var withheld []string
	for _, name := range CodeWrites() {
		if need, ok := belowWriteNeeds[name]; ok && limit >= need {
			continue
		}
		withheld = append(withheld, name)
	}
	return withheld
}

// belowWriteNeeds is the least access at which a change's decide admits each
// write a seat below write may make, for a principal that wrote none of the
// change (rule D21): a comment needs read; a review ask, a review and an
// answer to findings need review (a seat below write authors nothing, and may
// dismiss only the findings its own review raised). Every other code write
// needs write or more ([CodeWritesWithheld]).
//
// Each is at or above its row in [changeToolNeeds], the access a deciding node
// holds a COLLEAGUE to; they part where an author's relation admits a write
// below the access alone. TestEveryChangeToolNamesTheAccessItNeeds holds the
// two tables together, TestTheCodeWritesAreWithheldExactlyWhereNoGrantAdmitsOne
// this one to rule D21 and gitlet.GrantOf.
var belowWriteNeeds = map[string]gitlet.Access{
	CodeCommentOnChangeTool: gitlet.AccessRead,
	CodeRequestReviewTool:   gitlet.AccessReview,
	CodeReviewChangeTool:    gitlet.AccessReview,
	CodeResolveFindingTool:  gitlet.AccessReview,
}

// CodeWriter is Gitlet's writes as these tools ask them (estate.CodeSeam,
// declared by its consumer): an engine write goes to the repository's home
// Gitlet node; a lifecycle request and a person's marks (a change's viewed
// files among them) to any data node; a change's lifecycle to the committer,
// and its conversation and the repository's checks to the writers beside it.
type CodeWriter interface {
	GitWrite(ctx context.Context, w gitlet.EngineWrite) (gitlet.EngineWriteResult, error)
	Lifecycle(ctx context.Context, r gitlet.LifecycleRequest) (gitlet.LifecycleResult, error)
	Change(ctx context.Context, p gitlet.ChangeProposal) (gitlet.ChangeResult, error)
	Converse(ctx context.Context, w gitlet.ConversationWrite) (gitlet.ConversationResult, error)
	Mark(ctx context.Context, w gitlet.MarkWrite) (gitlet.MarkResult, error)
	Checks(ctx context.Context, r gitlet.ChecksRequest) (gitlet.ChecksResult, error)
	// Queue asks the repository's committer to decide one write of a
	// target's merge queue — a dequeue, here (gitlet.QueueProposal).
	Queue(ctx context.Context, p gitlet.QueueProposal) (gitlet.QueueResult, error)
	// Expunge asks the repository's committer to record a person's purge
	// request (gitlet.ExpungeProposal), which its maintainer then serves.
	Expunge(ctx context.Context, p gitlet.ExpungeProposal) (gitlet.ExpungeResult, error)
}

// codeWriteTools are the writes over deps, in [CodeWrites]' order.
func codeWriteTools(deps CodeDeps) []tools.Callable {
	return append([]tools.Callable{
		&codeBranchWrite{deps: deps, kind: gitlet.WriteCommitFiles},
		&codeBranchWrite{deps: deps, kind: gitlet.WriteRevert},
		&codeBranchWrite{deps: deps, kind: gitlet.WriteCherryPick},
		&codeBranchWrite{deps: deps, kind: gitlet.WriteCreateBranch},
		&codeBranchWrite{deps: deps, kind: gitlet.WriteDeleteBranch},
		&codeRepoWrite{deps: deps, tool: CodeCreateRepoTool},
		&codeRepoWrite{deps: deps, tool: CodeUpdateRepoTool},
		&codeRepoWrite{deps: deps, tool: CodeDeleteRepoTool},
		&codeRepoWrite{deps: deps, tool: CodeRestoreRepoTool},
		&codeRepoWrite{deps: deps, tool: CodeProtectBranchTool},
	}, append(append(changeWriteTools(deps), checkWriteTools(deps)...), &codePurgeUnreachable{deps: deps},
		&codeImportRepo{deps: deps})...)
}

// ---- who writes ------------------------------------------------------------ //

// writeCaller is who this call writes as, or its refusal: [CodeDeps.asker]'s
// caller, where a credential bound to nobody writes nothing. In a woken turn
// the write is decided again for the requester ([ceilingOf]): the tool's row
// first ([authz.DecideWithin] over no object; undecidable is unavailable,
// never a turn nobody woke), then the REQUESTER CEILING (U40 a) rides the
// request to the deciding node (gitlet.WriteCeiling), so a colleague moved
// out of the owning unit stops the next write. A colleague seat bounds it and
// every read the call makes ([codeCaller.ceiling]); a credential acting as no
// seat bounds it to the seat's own work; a person bounds nothing (ADR-0085).
// Whom it is written for (ADR-0109) is composed where the caller was resolved
// ([actorFor], [writtenForTurn]) and stamped beside the ceiling by every
// sender ([written], [CodeDeps.proposeChange]), which the decider holds together.
func (d CodeDeps) writeCaller(ctx context.Context, turn *turnctx.Turn, tool string) (codeCaller, *tools.Result) {
	actor, asker, refusal := d.asker(ctx, turn, tool)
	if refusal != nil {
		return codeCaller{}, refusal
	}
	if asker.Kind == gitlet.PrincipalOperator && asker.Seat == "" {
		r := refusedBy(tools.RefusalForbidden, gitlet.ErrNotPermitted, fmt.Sprintf("%s wrote nothing: %s.",
			tool, gitlet.UnboundDetail))
		return codeCaller{}, &r
	}
	c := codeCaller{actor: actor, asker: asker}
	if asker.Kind != gitlet.PrincipalAgent {
		return c, nil
	}
	ceiling, err := ceilingOf(ctx, turn, asker.Seat, d.Requesters)
	if err != nil {
		r := refusedBy(tools.RefusalUnavailable, fmt.Errorf("%w: %w", ErrUndecidable, err),
			fmt.Sprintf("%s could not establish who this turn's work is for, which every code write is "+
				"decided for as well as for you. Nothing was written. Try again shortly.", tool))
		return codeCaller{}, &r
	}
	if ceiling.Kind == authz.CeilingNone {
		return c, nil
	}
	action := authz.Action(tool)
	dec := authz.DecideWithin(ctx, turnctx.Principal(turn), ceiling, action, authz.Object{}, d.Chart,
		time.Now().UTC())
	switch {
	case dec.Unknown():
		r := refusedBy(tools.RefusalUnavailable, fmt.Errorf("%w: %w", ErrUndecidable, dec.Err),
			fmt.Sprintf("%s could not decide right now whether whoever this turn is working for may "+
				"change the company's code. Nothing was written. Try again shortly.", tool))
		return codeCaller{}, &r
	case !dec.Allowed:
		r := authorityRefusal(tool, DecisionError(action, dec))
		return codeCaller{}, &r
	case ceiling.Kind == authz.CeilingRequester && ceiling.Principal.Kind == iam.KindSeat:
		c.ceiling = ceiling.Principal.Seat
		c.write = &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: ceiling.Principal.Seat}
	case ceiling.Kind == authz.CeilingRequester && ceiling.Principal.Kind == iam.KindMachine:
		// A CREDENTIAL ACTING AS NO SEAT may only read (gitlet.GrantOf), so a
		// write made for it is held to the seat's own work, as a stranger's
		// is (ADR-0109); its reads are unbounded, since it reads everything.
		c.write = &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingNobody}
	}
	return c, nil
}

// writer is the principal a write is made by: the caller and, for a seat, the
// run behind the call (turn, work item, model, [toolloop.CallModel]), each
// label fitted rather than refused ([gitlet.FitProvenance]), so a vendor's
// long key never refuses a seat's write.
func writer(ctx context.Context, turn *turnctx.Turn, c codeCaller) gitlet.Principal {
	p := c.asker
	if p.Kind != gitlet.PrincipalAgent || turn == nil {
		return p
	}
	p.Run = gitlet.RunProvenance{TurnID: turn.RunID, Model: gitlet.FitProvenance(toolloop.CallModel(ctx))}
	if item := turn.WorkItem; item != nil {
		p.Run.WorkBackend = gitlet.FitProvenance(string(item.Backend))
		p.Run.WorkKey = gitlet.FitProvenance(item.Key)
	}
	return p
}

// written is engine write w as c makes it: principal ([writer]), ceiling
// ([CodeDeps.writeCaller]) and written-for carrier, never from a tool's
// arguments, STAMPED TOGETHER so the Gitlet node can hold the carrier to the
// ceiling it was composed beside (gitlet.EngineWrite.WrittenFor). Every code
// tool's engine write goes through it.
func written(ctx context.Context, turn *turnctx.Turn, c codeCaller, w gitlet.EngineWrite) gitlet.EngineWrite {
	w.Principal, w.Ceiling, w.WrittenFor = writer(ctx, turn, c), c.write, c.actor.WrittenFor
	return w
}

// writeFresh is the floor an engine write of repo is built at. A seat's is the
// furthest of this node's own writes to it, the turn's read of it and where
// every ref the turn resolved on it last moved ([refsRead]), this call's own
// revisions included, so the Gitlet node's copy holds the head and commits the
// turn saw. Floored short, the node syncs a copy that lacks them, finds the
// head stale or the commit absent, and re-judges at the log's head: a barrier
// and a second sync where one would do. A person's is the log's head.
func (d CodeDeps) writeFresh(turn *turnctx.Turn, c codeCaller, repo gitlet.RepoRef) statelog.Freshness {
	if c.asker.Kind != gitlet.PrincipalAgent {
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
func lifecycleVerb(kind gitlet.LifecycleKind) string { return "code-" + string(kind) }

// newRepoObject is the object a create's operation is named for. An
// operation's object is the repository's id or this word, never a ref or a
// name, which could carry the id past statelog.MaxCallerOpIDBytes; the name is
// in the arguments a turn's id digests ([createObjectKey]'s rule).
const newRepoObject = "new"

// ---- the engine writes ------------------------------------------------------ //

// codeBranchWrite is one of the five engine writes, by its kind.
type codeBranchWrite struct {
	deps CodeDeps
	kind gitlet.EngineWriteKind
}

var _ tools.SeatCallable = (*codeBranchWrite)(nil)

func (t *codeBranchWrite) Name() string {
	switch t.kind {
	case gitlet.WriteCommitFiles:
		return CodeCommitFilesTool
	case gitlet.WriteRevert:
		return CodeRevertTool
	case gitlet.WriteCherryPick:
		return CodeCherryPickTool
	case gitlet.WriteCreateBranch:
		return CodeCreateBranchTool
	}
	return CodeDeleteBranchTool
}

func (t *codeBranchWrite) Description() string {
	switch t.kind {
	case gitlet.WriteCommitFiles:
		return "Commit a small change to a branch's files, built on the server as one push — anything to build " +
			"or test belongs in a checkout. `files`, one operation a path: `put` (`content`, or `blob_oid` of a " +
			"file the repository holds; `mode` 100755 executable, 120000 a link to `content`), `edit` (`old`, " +
			"which must occur exactly once, and `new`), `delete`, `move` (`to`), `mode`."
	case gitlet.WriteRevert:
		return "Commit the revert of one commit onto a branch. On a conflict nothing is committed, and each " +
			"path's stages are answered by id for " + CodeReadTool + "."
	case gitlet.WriteCherryPick:
		return "Commit one commit's change onto a branch. On a conflict nothing is committed, and each path's " +
			"stages are answered by id for " + CodeReadTool + "."
	case gitlet.WriteCreateBranch:
		return "Create a branch at a commit."
	}
	return "Delete a branch at the head you read; its commits stay in the history."
}

// THE SCHEMAS ARE TERSE ON PURPOSE: a seat carries every one on every round,
// uncached, so an argument says only what its name and type do not
// ([codeCatalogueBudget] in the tests), and a revision is described once, in
// code_resolve's description ([codeRevArg]).
func (t *codeBranchWrite) Parameters() map[string]any {
	props := map[string]any{
		"repo":   codeRepoArg(),
		"branch": map[string]any{"type": "string", "description": "Its name under refs/heads/."},
	}
	required := []string{"repo", "branch"}
	head := map[string]any{"type": "string", "description": "The branch's head, as " + CodeResolveTool + " answered it."}
	switch t.kind {
	case gitlet.WriteCommitFiles:
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
						"enum": []string{gitlet.ModeFile, gitlet.ModeExecutable, gitlet.ModeLink}},
				},
				"required": []string{"op", "path"},
			},
		}
		required = append(required, "message", "files")
	case gitlet.WriteRevert, gitlet.WriteCherryPick:
		props["expected_head"] = head
		props["commit"] = codeRevArg("The commit")
		props["message"] = map[string]any{"type": "string", "description": "In place of git's own."}
		required = append(required, "expected_head", "commit")
	case gitlet.WriteCreateBranch:
		props["at"] = codeRevArg("Where it starts")
		required = append(required, "at")
	case gitlet.WriteDeleteBranch:
		props["expected_head"] = head
		required = append(required, "expected_head")
	}
	return t.deps.ops.param(map[string]any{"type": "object", "properties": props, "required": required})
}

// fileOpNames is the file operations' names, as the schema's enum lists them.
func fileOpNames() []string {
	var out []string
	for _, k := range gitlet.FileOpKinds() {
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
	if refusal := t.deps.branch(ctx, turn, tool, c, repo, w.ExpectedHead != "", w.Ref); refusal != nil {
		return *refusal, nil
	}
	w = written(ctx, turn, c, w)
	w.RepoID, w.Fresh = repo.ID, t.deps.writeFresh(turn, c, repo)
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

// revArgs are a write's revision arguments the Gitlet node takes by whole id: a
// commit_files create's base, a revert's or a cherry-pick's commit, and a
// create_branch's target — each resolved where it names a ref.
type revArgs struct{ base, commit string }

// args is the write a call's arguments ask for — every field but the
// repository's and the operation's, which the call resolves and binds — or
// the refusal of an argument no write could take.
func (t *codeBranchWrite) args(args map[string]any) (gitlet.EngineWrite, revArgs, *tools.Result) {
	tool := t.Name()
	branch := strings.TrimPrefix(strings.TrimSpace(argString(args, "branch")), branchRoot)
	if !gitref.ValidBranch(branch) {
		r := failed(fmt.Sprintf("%s needs `branch`: a branch's name as refs/heads/ holds it (`main`, "+
			"`seats/<handle>/fix`) — %q is not one git takes.", tool, clip(branch)))
		return gitlet.EngineWrite{}, revArgs{}, &r
	}
	w := gitlet.EngineWrite{Kind: t.kind, Ref: branchRoot + branch,
		ExpectedHead: strings.TrimSpace(argString(args, "expected_head")),
		Message:      argString(args, "message")}
	var revs revArgs
	switch t.kind {
	case gitlet.WriteCommitFiles:
		revs.base = strings.TrimSpace(argString(args, "base"))
		files, refusal := fileOpsArg(tool, args)
		if refusal != nil {
			return gitlet.EngineWrite{}, revArgs{}, refusal
		}
		w.Files = files
	case gitlet.WriteRevert, gitlet.WriteCherryPick, gitlet.WriteCreateBranch:
		arg := "commit"
		if t.kind == gitlet.WriteCreateBranch {
			arg = "at"
		}
		rev, refusal := revArg(tool, arg, args)
		if refusal != nil {
			return gitlet.EngineWrite{}, revArgs{}, refusal
		}
		revs.commit = rev
	}
	return w, revs, nil
}

// fileOpsArg is a commit_files' operations, each read field by field, or the
// refusal of one no write could take. A `content` present and empty is an
// empty file, so presence is read apart from the value.
func fileOpsArg(tool string, args map[string]any) ([]gitlet.FileOp, *tools.Result) {
	raw, _ := argval.List(args["files"])
	if len(raw) == 0 {
		r := failed(fmt.Sprintf("%s needs `files`: one operation a path — put, edit, delete, move or mode.", tool))
		return nil, &r
	}
	out := make([]gitlet.FileOp, 0, len(raw))
	for i, entry := range raw {
		m, ok := argval.Object(entry)
		if !ok {
			r := failed(fmt.Sprintf("%s: files[%d] is not an object of `op`, `path` and the fields its op "+
				"takes.", tool, i))
			return nil, &r
		}
		op := gitlet.FileOp{Op: gitlet.FileOpKind(argString(m, "op")), Path: argString(m, "path"),
			BlobOID: argString(m, "blob_oid"), Old: argString(m, "old"), New: argString(m, "new"),
			To: argString(m, "to"), Mode: argString(m, "mode")}
		// A NULL `content` IS NO CONTENT (argval.Object drops nulls), left to
		// the write's own rule; a list or an object is no text.
		if content, present := m["content"]; present {
			text, ok := argval.String(content)
			if !ok {
				r := failed(fmt.Sprintf("%s: files[%d].content is the file's text; a binary file is put by "+
					"`blob_oid`, or committed in a clone and pushed.", tool, i))
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
// ([CodeDeps.resolveRevs]); an abbreviated id refused, since a write names
// whole ids only. A ref it resolves counts as read by the turn, so the write
// is floored where it moved ([CodeDeps.writeFresh]).
func (t *codeBranchWrite) resolve(ctx context.Context, turn *turnctx.Turn, c codeCaller, repo gitlet.RepoRef,
	w *gitlet.EngineWrite, revs revArgs) *tools.Result {

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
	Repo          string            `json:"repo"`
	RenamedTo     string            `json:"renamed_to,omitempty"`
	RedirectUntil time.Time         `json:"redirect_until,omitzero"`
	Branch        string            `json:"branch"`
	Outcome       statelog.Outcome  `json:"outcome,omitempty"`
	Commit        string            `json:"commit,omitempty"`
	Previous      string            `json:"previous,omitempty"`
	Position      string            `json:"position,omitempty"`
	Refusal       string            `json:"refusal,omitempty"`
	Detail        string            `json:"detail,omitempty"`
	Conflicts     []gitlet.Conflict `json:"conflicts,omitempty"`
	// ConflictsOmitted counts the conflicted paths this answer leaves out:
	// those past the Gitlet node's page and those past one tool answer.
	ConflictsOmitted int    `json:"conflicts_omitted,omitempty"`
	OpID             string `json:"op_id,omitempty"`
}

// answer is what an engine write came to, as this tool says it: the change,
// or the refusal of the change or of its push in its class, or an outcome
// unknown, which the same call made again finds out.
func (t *codeBranchWrite) answer(ctx context.Context, turn *turnctx.Turn, c codeCaller, repo gitlet.RepoRef,
	w gitlet.EngineWrite, res gitlet.EngineWriteResult) (tools.Result, error) {

	tool := t.Name()
	out := codeBranchAnswer{Repo: repo.Name, RenamedTo: repo.Redirect, RedirectUntil: repo.RedirectUntil,
		Branch: strings.TrimPrefix(w.Ref, branchRoot), OpID: c.actor.Operation}
	switch {
	case res.Refusal == gitlet.WriteConflict:
		return conflictedWrite(ctx, turn, c, tool, out, res)
	case res.Refusal != "":
		return refusedBy(tools.RefusalInvalid, writeRefusalError(string(res.Refusal), res.Detail),
			fmt.Sprintf("%s changed nothing: %s (%s). The same call is refused again; change what it asks for.",
				tool, res.Detail, res.Refusal)), nil
	case res.Outcome == statelog.OutcomeApplied, res.Outcome == statelog.OutcomePending:
		out.Outcome, out.Commit, out.Position = res.Outcome, res.Commit, positionText(res.Position)
		out.Previous = res.Command.Old
		if out.Previous == gitlet.ZeroOID {
			out.Previous = ""
		}
	case res.Outcome == statelog.OutcomeUnknown:
		return unknownWrite(c.actor, tool, fmt.Sprintf("the write to %s on %s landed", out.Branch, repo.Name),
			w.OpID, false, unknownNext(false, sameCall(c.actor, tool),
				fmt.Sprintf("Read the branch with %s", CodeResolveTool),
				"the same change would be made twice")), nil
	case res.Command.Result == gitlet.ResultNoop:
		// THE BRANCH ALREADY STOOD WHERE THE CALL ASKED: nothing to land.
		out.Outcome, out.Commit = statelog.OutcomeApplied, res.Command.New
	case res.Command.Result == gitlet.ResultNG:
		return pushRefused(ctx, tool, repo.Name, out.Branch, res.Command, c.actor), nil
	default:
		return faulted(ctx, tool, fmt.Errorf("builtin: Gitlet answered a write with no outcome and no "+
			"refusal: %+v", res), fmt.Sprintf("%s could not tell what became of the write: %s.", tool, faultSaid)), nil
	}
	return newForeignRead(turn, c.asker.Seat).page(ctx, tool, out, "")
}

// conflictedWrite is an engine write's conflict as tool answers it — a
// revert's, a cherry-pick's or a rebase's: nothing was stored, and each
// conflicted path goes out with its stages' ids and the start of the file as
// git marked it, FENCED — a repository's text is its committers' — as many as
// fit one answer, the rest counted.
func conflictedWrite(ctx context.Context, turn *turnctx.Turn, c codeCaller, tool string, out codeBranchAnswer,
	res gitlet.EngineWriteResult) (tools.Result, error) {

	out.Refusal = string(gitlet.WriteConflict)
	out.Detail = fmt.Sprintf("%s Nothing was committed. Read each path's stages with %s by the ids "+
		"below (base, ours — the branch — and theirs), make the change yourself, and commit it with %s.",
		strings.TrimSuffix(strings.TrimSpace(res.Detail), ".")+".", CodeReadTool, CodeCommitFilesTool)
	// EACH PAGE ITS OWN FENCES: a fence registered for a conflict a page
	// leaves out is one the render refuses (the fail-closed rule), so every
	// candidate page is rendered by a read that fences exactly what it holds.
	render := func(k int) (json.RawMessage, error) {
		read := newForeignRead(turn, c.asker.Seat)
		for i, conflict := range res.Conflicts[:k] {
			read.fenceRepository(fieldPath{}.key("conflicts").index(i).key("marked"), provenance.SourceRepoFile,
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
		writeRefusalError(string(gitlet.WriteConflict), res.Detail))}, nil
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
	return "Set the protection of the branches or tags a pattern names, or take it away (`remove`). Each " +
		"field changes that field alone, the rest of the rule kept (a new rule: direct_push persons, no " +
		"force or delete, no gate); `remove_gate` drops the merge gate."
}

// ownerShape is how an owner argument is spelled, as the read tools answer
// owners.
const ownerShape = "{\"kind\": \"unit\" or \"seat\", \"ref\": its key or handle}"

func (t *codeRepoWrite) Parameters() map[string]any {
	owner := map[string]any{"type": "object", "description": ownerShape}
	visibility := map[string]any{"type": "string",
		"enum": []string{string(gitlet.VisibilityCompany), string(gitlet.VisibilityUnit)}}
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
		props["search"] = map[string]any{"type": "string", "enum": spelled(gitlet.SearchModes())}
		props["owner"] = owner
		props["path_owners"] = map[string]any{"type": "object",
			"description": "Globs given to or taken from a project's owners: {\"add\": [{\"owner\": …, " +
				"\"paths\": [glob]}], \"remove\": [the same]}."}
		props["mirror_from"] = map[string]any{"type": "object",
			"description": "A person's: the pull mirror, {\"url\", \"credential\": \"${VAR}\"}; {} removes it."}
		props["mirror_to"] = map[string]any{"type": "object",
			"description": "A person's: the push mirror, {\"url\", \"credential\", \"as\": a handle, \"refs\"}; " +
				"{} removes it."}
		props["archived"] = map[string]any{"type": "boolean"}
		props["rename_to"] = map[string]any{"type": "string"}
	case CodeDeleteRepoTool:
		props["reason"] = map[string]any{"type": "string"}
	case CodeProtectBranchTool:
		props["pattern"] = map[string]any{"type": "string",
			"description": "refs/heads/<glob> or refs/tags/<glob>; `*` within a name, `**` across them."}
		props["direct_push"] = map[string]any{"type": "string",
			"enum":        []string{string(gitlet.DirectPersons), string(gitlet.DirectNone)},
			"description": "A person's alone, or nobody's."}
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
// schema by Gitlet's own names ([gitlet.GateChangeFields]), each saying only
// its type and values: what a field does is Gitlet's to document, and the
// schema rides every round ([codeCatalogueBudget] in the tests).
func gateParams(props map[string]any) {
	enum := func(values []string) map[string]any { return map[string]any{"type": "string", "enum": values} }
	list := func(item map[string]any) map[string]any { return map[string]any{"type": "array", "items": item} }
	object := func(fields map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": fields}
	}
	// A LIST CHANGES ITEM BY ITEM ([gitlet.ListChange], [gitlet.HumanPathsChange]):
	// what it adds and what it removes, never the list whole.
	change := func(item map[string]any) map[string]any {
		return object(map[string]any{"add": list(item), "remove": list(item)})
	}
	classes := list(enum(spelled(gitlet.PathClasses())))
	props["merge_by"] = enum(spelled(gitlet.MergeBys()))
	props["approvals"] = enum(spelled(gitlet.ApprovalCounts()))
	props["approvals_unless"] = change(enum(spelled(gitlet.PathClasses())))
	props["human_paths"] = object(map[string]any{
		"put": list(map[string]any{"type": "object", "properties": map[string]any{
			"glob": map[string]any{"type": "string"}, "unless": classes}, "required": []string{"glob"}}),
		"remove": list(map[string]any{"type": "string"})})
	props["checks"] = change(map[string]any{"type": "string"})
	props["landing"] = object(map[string]any{
		"depth": map[string]any{"type": "integer"}, "batch": map[string]any{"type": "integer"}})
	props["delete_source"] = enum(spelled(gitlet.DeleteSources()))
	for _, flag := range []string{"owners_approve", "agents_edit_checks"} {
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
	if req.Kind != gitlet.LifecycleCreate {
		if name, refusal = repoArg(t.tool, args); refusal != nil {
			return *refusal, nil
		}
	}
	actor, bound := t.deps.bindOperation(c.actor, t.tool, args)
	if bound != "" {
		return failed(bound), nil
	}
	c.actor = actor
	req.Actor, req.Ceiling = c.asker, c.write
	var repo gitlet.RepoRef
	if req.Kind == gitlet.LifecycleCreate {
		req.OpID = opIDFor(actor, t.tool, lifecycleVerb(req.Kind), newRepoObject, args)
		id, ok := gitlet.RepoIDOf(req.OpID)
		if !ok {
			return faulted(ctx, t.tool, fmt.Errorf("builtin: operation %s carries no instant", req.OpID),
				fmt.Sprintf("%s could not name the repository it would create: %s.", t.tool, faultSaid)), nil
		}
		// THE HOLD BEFORE THE CREATE, on the id its decide will derive, so a
		// scratch repository is held from before it exists.
		repo = gitlet.RepoRef{ID: id, Name: req.Name, Kind: gitlet.KindOf(req.Name)}
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
func (t *codeRepoWrite) request(args map[string]any) (gitlet.LifecycleRequest, *tools.Result) {
	tool := t.tool
	switch tool {
	case CodeCreateRepoTool:
		return createRequest(tool, args)
	case CodeUpdateRepoTool:
		return updateRequest(tool, args)
	case CodeDeleteRepoTool:
		return gitlet.LifecycleRequest{Kind: gitlet.LifecycleDelete, Reason: strings.TrimSpace(argString(args, "reason"))}, nil
	case CodeRestoreRepoTool:
		return gitlet.LifecycleRequest{Kind: gitlet.LifecycleRestore}, nil
	}
	pattern := strings.TrimSpace(argString(args, "pattern"))
	if !gitref.ValidPattern(pattern) {
		r := failed(fmt.Sprintf("%s needs `pattern`: refs/heads/<glob> or refs/tags/<glob> — %q is not one.",
			tool, clip(pattern)))
		return gitlet.LifecycleRequest{}, &r
	}
	// ONLY THE PUSH CONTROLS THE CALL NAMES: the deciding data node keeps the
	// rest as the rule holds them in its snapshot (or their least on a new
	// pattern, [gitlet.ProtectChange]), so requiring a check leaves a frozen
	// branch frozen. Sent beside a removal too, so the request's validation
	// refuses the pair rather than this tool dropping what its caller said.
	controls := map[string]any{}
	for _, field := range gitlet.ProtectControlFields() {
		if v, ok := args[field]; ok {
			controls[field] = v
		}
	}
	var change gitlet.ProtectChange
	if err := decodeArg(controls, &change); err != nil {
		r := failed(fmt.Sprintf("%s: a push control is not as the schema says: %v.", tool, err))
		return gitlet.LifecycleRequest{}, &r
	}
	change.Pattern, change.Remove = pattern, argBool(args, "remove")
	gate, refusal := gateChangeArg(tool, args)
	if refusal != nil {
		return gitlet.LifecycleRequest{}, refusal
	}
	change.Gate = gate
	return gitlet.LifecycleRequest{Kind: gitlet.LifecycleProtect, Protect: &change}, nil
}

// gateChangeArg is the change to a rule's merge gate a protect call names
// (nil where it names no gate field and keeps the gate), or the refusal of a
// field no change could take. Each field REPLACES that field alone
// ([gitlet.GateChange]): only the deciding data node reads the gate in the
// snapshot it decides in, and a whole gate formed here from an older read
// would undo whatever changed it meanwhile.
func gateChangeArg(tool string, args map[string]any) (*gitlet.GateChange, *tools.Result) {
	named := map[string]any{}
	for _, field := range gitlet.GateChangeFields() {
		if v, ok := args[field]; ok {
			named[field] = v
		}
	}
	remove := argBool(args, "remove_gate")
	if len(named) == 0 && !remove {
		return nil, nil
	}
	var change gitlet.GateChange
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
func createRequest(tool string, args map[string]any) (gitlet.LifecycleRequest, *tools.Result) {
	name := strings.TrimSpace(argString(args, "name"))
	if !gitlet.ValidName(name) {
		r := failed(fmt.Sprintf("%s needs `name`: a project's name — lowercase letters, digits and hyphens — "+
			"or `<your handle>.<name>` for a scratch repository; %q is neither.", tool, clip(name)))
		return gitlet.LifecycleRequest{}, &r
	}
	// A TOOL'S REPOSITORY IS SEARCHED WITH GREP until a maintainer asks for
	// an index (code_update_repo's `search`); a scratch repository for good.
	settings := gitlet.RepoSettings{ManagedBy: gitlet.ManagedTools, Kind: gitlet.KindOf(name),
		Description:   strings.TrimSpace(argString(args, "description")),
		DefaultBranch: cmp.Or(strings.TrimSpace(argString(args, "default_branch")), "main"),
		Visibility: gitlet.Visibility(cmp.Or(strings.TrimSpace(argString(args, "visibility")),
			string(gitlet.VisibilityCompany))),
		Search: gitlet.SearchGrep}
	owner, given, refusal := ownerArg(tool, "owner", args)
	if refusal != nil {
		return gitlet.LifecycleRequest{}, refusal
	}
	if handle, scratch := gitref.ScratchName(name); scratch {
		settings.Owner = gitlet.Owner{Kind: gitlet.OwnerSeat, Ref: handle}
		if given && owner != settings.Owner {
			r := failed(fmt.Sprintf("%s: %s is seat %s's scratch repository, which is its seat's own; leave "+
				"`owner` out.", tool, name, handle))
			return gitlet.LifecycleRequest{}, &r
		}
	} else if !given {
		r := failed(fmt.Sprintf("%s: the project %s needs an `owner` — a unit by its key ({\"kind\": \"unit\", "+
			"\"ref\": \"<key>\"}).", tool, name))
		return gitlet.LifecycleRequest{}, &r
	} else {
		settings.Owner = owner
	}
	return gitlet.LifecycleRequest{Kind: gitlet.LifecycleCreate, Name: name, Create: &settings}, nil
}

// updateRequest is code_update_repo's request: ONE of a settings change, an
// archive or unarchive, or a rename, since each is a record of its own.
func updateRequest(tool string, args map[string]any) (gitlet.LifecycleRequest, *tools.Result) {
	var change gitlet.RepoChange
	// PRESENCE IS THE ASK: a null never reaches a tool (toolargs.Of drops
	// it), and an empty description is one cleared.
	if _, held := args["description"]; held {
		description := argString(args, "description")
		change.Description = &description
	}
	if _, held := args["default_branch"]; held {
		branch := strings.TrimSpace(argString(args, "default_branch"))
		change.DefaultBranch = &branch
	}
	if _, held := args["visibility"]; held {
		visibility := gitlet.Visibility(strings.TrimSpace(argString(args, "visibility")))
		change.Visibility = &visibility
	}
	if _, held := args["search"]; held {
		search := gitlet.SearchMode(strings.TrimSpace(argString(args, "search")))
		change.Search = &search
	}
	owner, given, refusal := ownerArg(tool, "owner", args)
	if refusal != nil {
		return gitlet.LifecycleRequest{}, refusal
	}
	if given {
		change.Owner = &owner
	}
	if raw, ok := args["path_owners"]; ok {
		var owners gitlet.PathOwnersChange
		if err := decodeArg(raw, &owners); err != nil {
			r := failed(fmt.Sprintf("%s: `path_owners` is {\"add\": [...], \"remove\": [...]}, each entry an "+
				"`owner` and its `paths`: %v.", tool, err))
			return gitlet.LifecycleRequest{}, &r
		}
		change.PathOwners = &owners
	}
	// THE MIRRORS, each set whole or removed by {} ([gitlet.RepoChange.MirrorFrom]),
	// are a person's alone (U49 a): the deciding data node refuses an agent's
	// call as not_permitted, so this tool never filters it.
	if raw, ok := args["mirror_from"]; ok {
		var from gitlet.MirrorSource
		if err := decodeArg(raw, &from); err != nil {
			r := failed(fmt.Sprintf("%s: `mirror_from` is {\"url\": \"https://…\", \"credential\": "+
				"\"${VAR}\"}, or {} to remove the pull mirror: %v.", tool, err))
			return gitlet.LifecycleRequest{}, &r
		}
		change.MirrorFrom = &from
	}
	if raw, ok := args["mirror_to"]; ok {
		var to gitlet.MirrorTarget
		if err := decodeArg(raw, &to); err != nil {
			r := failed(fmt.Sprintf("%s: `mirror_to` is {\"url\": \"https://…\", \"credential\": \"${VAR}\", "+
				"\"as\": \"<handle>\", \"refs\": [pattern]}, or {} to remove the push mirror: %v.", tool, err))
			return gitlet.LifecycleRequest{}, &r
		}
		change.MirrorTo = &to
	}
	_, archive := argval.Bool(args["archived"])
	rename := strings.TrimSpace(argString(args, "rename_to"))
	settings := change != (gitlet.RepoChange{})
	asked := 0
	for _, set := range []bool{settings, archive, rename != ""} {
		if set {
			asked++
		}
	}
	if asked != 1 {
		r := failed(fmt.Sprintf("%s changes ONE thing a call: its settings (any of `description`, "+
			"`default_branch`, `visibility`, `search`, `owner`, `path_owners`, `mirror_from`, `mirror_to`), "+
			"`archived`, or `rename_to` — "+
			"each is a record of its own.", tool))
		return gitlet.LifecycleRequest{}, &r
	}
	switch {
	case archive && argBool(args, "archived"):
		return gitlet.LifecycleRequest{Kind: gitlet.LifecycleArchive}, nil
	case archive:
		return gitlet.LifecycleRequest{Kind: gitlet.LifecycleUnarchive}, nil
	case rename != "":
		return gitlet.LifecycleRequest{Kind: gitlet.LifecycleRename, Name: rename}, nil
	}
	return gitlet.LifecycleRequest{Kind: gitlet.LifecycleSettings, Change: &change}, nil
}

// ownerArg is an owner argument — {"kind": "unit"|"seat", "ref": "..."} — and
// whether the call gave one, or the refusal of one no owner is.
func ownerArg(tool, arg string, args map[string]any) (gitlet.Owner, bool, *tools.Result) {
	raw, given := args[arg]
	if !given {
		return gitlet.Owner{}, false, nil
	}
	var o gitlet.Owner
	if err := decodeArg(raw, &o); err != nil || !o.Kind.Valid() || strings.TrimSpace(o.Ref) == "" {
		r := failed(fmt.Sprintf("%s: `%s` is {\"kind\": \"unit\", \"ref\": \"<unit key>\"} or {\"kind\": "+
			"\"seat\", \"ref\": \"<handle>\"}.", tool, arg))
		return gitlet.Owner{}, false, &r
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

// codeRepoState is a repository as a lifecycle answer says it stands, never
// its protection rules: sixty-four rules with gates of up to
// gitlet.MaxGateBytes overrun one tool answer, and an answer refused for size
// AFTER its write landed would read as a failed write. A protect answer
// carries its one rule ([codeRepoWriteAnswer.Rule]); code_get_repo reads all.
type codeRepoState struct {
	Name          string            `json:"name"`
	Kind          gitlet.RepoKind   `json:"kind"`
	Status        gitlet.RepoStatus `json:"status"`
	Owner         gitlet.Owner      `json:"owner"`
	Visibility    gitlet.Visibility `json:"visibility"`
	Search        gitlet.SearchMode `json:"search"`
	DefaultBranch string            `json:"default_branch"`
	Description   string            `json:"description,omitempty" untrusted:"name"`
}

// codeRepoWriteAnswer is a lifecycle tool's answer.
type codeRepoWriteAnswer struct {
	Repo          string           `json:"repo"`
	RenamedTo     string           `json:"renamed_to,omitempty"`
	RedirectUntil time.Time        `json:"redirect_until,omitzero"`
	Outcome       statelog.Outcome `json:"outcome"`
	Repository    *codeRepoState   `json:"repository,omitempty"`
	// Rule is a protect request's rule as applied, its gate as the deciding
	// node RESOLVED it; absent where the rule was removed or the request is
	// pending, since a pending answer's row is the one read before the change.
	Rule     *gitlet.Protection `json:"rule,omitempty"`
	Position string             `json:"position,omitempty"`
	OpID     string             `json:"op_id,omitempty"`
}

// answer is what a lifecycle request came to, as this tool says it. One that
// applied ENDS THE TURN'S RESOLUTION of the repository's name: its row moved,
// and the kept resolution would serve the row as it was.
func (t *codeRepoWrite) answer(ctx context.Context, turn *turnctx.Turn, c codeCaller, repo gitlet.RepoRef,
	req gitlet.LifecycleRequest, res gitlet.LifecycleResult) (tools.Result, error) {

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
		return faulted(ctx, t.tool, fmt.Errorf("builtin: Gitlet answered a lifecycle request with "+
			"outcome %q and no refusal", res.Outcome), fmt.Sprintf("%s could not tell what became of the "+
			"request: %s.", t.tool, faultSaid)), nil
	}
	if req.Kind != gitlet.LifecycleCreate {
		forget(turn, c.asker, repo)
	}
	out := codeRepoWriteAnswer{Repo: name, RenamedTo: repo.Redirect, RedirectUntil: repo.RedirectUntil,
		Outcome: res.Outcome, Position: positionText(res.Position), OpID: c.actor.Operation}
	if r := res.Repo; r.ID != "" {
		out.Repository = &codeRepoState{Name: r.Name, Kind: r.Kind, Status: r.Status, Owner: r.Owner,
			Visibility: r.Visibility, Search: r.Search, DefaultBranch: r.DefaultBranch,
			Description: r.Description}
	}
	if req.Kind == gitlet.LifecycleProtect && res.Outcome == statelog.OutcomeApplied {
		// THE RULE WHOLE, from beside the row, whose rules carry no gate
		// (gitlet.LifecycleResult.Rule).
		out.Rule = res.Rule
	}
	read := newForeignRead(turn, c.asker.Seat)
	// THE REPOSITORY'S DESCRIPTION AS IT NOW STANDS, a name this call may not
	// have written ([codeListRepos]'s reason).
	if out.Repository != nil && out.Repository.Description != "" {
		namesBy(read)
	}
	return read.page(ctx, t.tool, out, "")
}

// ---- refusals, in the classes every surface renders ------------------------ //

// HostRefusal is a refusal Gitlet answered, as the cause beneath a tool's
// class: the host's own word for it and its sentence.
//
// THE WORD TRAVELS AS A VALUE because several host refusals share a class (a
// moved change head and a moved train base are both stale_version), and a
// person's surface that words each its own way reads it from the act route's
// `refusal` field (operator.Fail) rather than parsing a sentence for a model.
type HostRefusal struct {
	// Refusal is the host's word: a gitlet.MergeRefusal, a gitlet.WriteRefusal,
	// a gitlet.ChangeRefusal and their like.
	Refusal string
	// Detail is the host's sentence about it.
	Detail string
}

func (r *HostRefusal) Error() string { return fmt.Sprintf("gitlet: %s: %s", r.Refusal, r.Detail) }

// writeRefusalError is a refusal Gitlet answered, as the cause beneath
// a tool's class ([HostRefusal]).
func writeRefusalError(refusal, detail string) error {
	return &HostRefusal{Refusal: refusal, Detail: detail}
}

// pushClass is the class a refused push is answered in, by [tools.Refusal]'s
// rule: what would make the same request land. A rule (grant, protection, the
// daily push quota) is FORBIDDEN, never budget_exhausted, the token budget's;
// a repository state somebody must change (archived, pull-mirrored, deleting
// the default branch) CONFLICT; a keep limit (code.limits.repo_max_bytes,
// MaxRefsPerRepo) QUOTA_EXCEEDED, which no retry clears; a ref path another
// ref holds (feature beside feature/x) EXISTS; the push itself INVALID; what
// waiting clears UNAVAILABLE, the same call being the same write. What no
// engine write can draw (an --atomic push, a moved tag, forged_committer: an
// engine write's commits are the engine's by right) is the engine's fault.
// [changeClass], [conversationClass], [mergeRefusalClass] and
// [lifecycleRefused] follow the same rule, so a state is one class everywhere.
func pushClass(r gitlet.Refusal) (tools.Refusal, bool) {
	switch r {
	case gitlet.RefusalNotPermitted, gitlet.RefusalProtectedForce, gitlet.RefusalProtectedDelete,
		gitlet.RefusalProtectedDirect, gitlet.RefusalPushQuota:
		return tools.RefusalForbidden, true
	case gitlet.RefusalRepoArchived, gitlet.RefusalRepoMirrored, gitlet.RefusalDefaultBranch:
		return tools.RefusalConflict, true
	case gitlet.RefusalRepoSize, gitlet.RefusalRepoRefs:
		return tools.RefusalQuotaExceeded, true
	case gitlet.RefusalRefConflict:
		return tools.RefusalExists, true
	case gitlet.RefusalReservedRef, gitlet.RefusalBadRef, gitlet.RefusalPackTooLarge, gitlet.RefusalBlobTooLarge:
		return tools.RefusalInvalid, true
	case gitlet.RefusalStale:
		return tools.RefusalStaleVersion, true
	case gitlet.RefusalRepoDeleted:
		return tools.RefusalNotFound, true
	case gitlet.RefusalStoreFull:
		return tools.RefusalStoreFull, true
	case gitlet.RefusalBusy, gitlet.RefusalCompactionBehind, gitlet.RefusalUploadExpired, gitlet.RefusalExpunging,
		gitlet.RefusalRepoImporting, gitlet.RefusalUnavailable, gitlet.RefusalNotApplied:
		return tools.RefusalUnavailable, true
	}
	return "", false
}

// pushRefused is an engine write whose push the landing refused.
func pushRefused(ctx context.Context, tool, repo, branch string, cmd gitlet.Command, actor Actor) tools.Result {

	detail := cmp.Or(cmd.Detail, cmd.Reason.Sentence())
	cause := writeRefusalError(string(cmd.Reason), detail)
	class, known := pushClass(cmd.Reason)
	if !known {
		return faulted(ctx, tool, cause, fmt.Sprintf("%s could not land the write to %s on %s: Gitlet "+
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
	case tools.RefusalConflict:
		said += fmt.Sprintf(" That is the repository's state, which the sentence names: read it with %s "+
			"before you decide, since the same call does not land while it stands.", CodeGetRepoTool)
	case tools.RefusalQuotaExceeded:
		said += " Nothing was committed, and trying again does not clear it: something the repository keeps " +
			"has to go, or the limit has to be raised."
	case tools.RefusalExists:
		said += " Another ref holds that name's path: use another name, or delete that ref first."
	case tools.RefusalInvalid:
		said += " The same call is refused again; change what it names."
	}
	if cmd.Reason == gitlet.RefusalDefaultBranch {
		// THE DEFAULT BRANCH IS KEPT, which a person meets deleting from a
		// stale listing: told what lets it go, never the conflict class's
		// "collided with another change".
		return toPerson(refusedBy(class, cause, said), caveat.DefaultBranchKept())
	}
	return refusedBy(class, cause, said)
}

// lifecycleRefused is a lifecycle request the data node refused, in its class
// by [pushClass]'s rule; a rule of the repository or the company (the
// caller's grant, a repository the company document declares) is FORBIDDEN,
// and a repository that moved between decide and apply STALE_VERSION.
func lifecycleRefused(tool, repo string, res gitlet.LifecycleResult) tools.Result {
	cause := writeRefusalError(string(res.Refusal), res.Detail)
	said := fmt.Sprintf("%s changed nothing about %s: %s (%s).", tool, repo, res.Detail, res.Refusal)
	switch res.Refusal {
	case gitlet.LifecycleNameTaken:
		return refusedBy(tools.RefusalExists, cause, said)
	case gitlet.LifecycleScratchLimit, gitlet.LifecycleRepoLimit:
		return refusedBy(tools.RefusalQuotaExceeded, cause, said+" Trying again does not clear it: a "+
			"repository has to go, or the limit has to be raised.")
	case gitlet.LifecycleRepoDeleted, gitlet.LifecycleRefDeleted:
		// GONE: the repository, or — a hold's alone — the seat's branch
		// it names.
		return refusedBy(tools.RefusalNotFound, cause, said)
	case gitlet.LifecycleStale:
		return refusedBy(tools.RefusalStaleVersion, cause, said+fmt.Sprintf(" Read it again with %s.",
			CodeGetRepoTool))
	}
	return refusedBy(tools.RefusalForbidden, cause, said+" That is a rule of the repository or the company, so "+
		"the same call is refused again.")
}

// failedWrite explains a write Gitlet could not answer: a silent node may have
// landed it, so it is an unknown the same call made again finds out; a
// repository gone ends the turn's resolution of its name ([CodeDeps.failed]);
// the rest is [codeWriteFailure]'s.
func (d CodeDeps) failedWrite(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	repo gitlet.RepoRef, opID string, err error) tools.Result {

	var unserved *estate.ErrUnserved
	if errors.Is(err, estate.ErrOutcomeUnknown) || errors.As(err, &unserved) && unserved.Silent() {
		return unknownWrite(c.actor, tool, fmt.Sprintf("the write to %s landed", repo.Name), opID, false,
			unknownNext(false, sameCall(c.actor, tool), fmt.Sprintf("Read it with %s", CodeGetRepoTool),
				"the same change would be made twice"))
	}
	if errors.Is(err, gitlet.ErrRepoNotFound) {
		forget(turn, c.asker, repo)
	}
	return codeWriteFailure(ctx, tool, c.actor, repo.Name, err)
}

// codeWriteFailure explains a write refused or unanswered before it was
// decided, each sentinel in its class; anything unclassed is this node's fault.
func codeWriteFailure(ctx context.Context, tool string, actor Actor, repo string, err error) tools.Result {
	var (
		unready     *gitlet.ErrGitUnready
		unserved    *estate.ErrUnserved
		unknownKeys *estate.ErrUnknownKeys
	)
	again := sameCall(actor, tool)
	if again == "" {
		again = "make the same call again"
	}
	switch {
	case errors.Is(err, gitlet.ErrInvalid), errors.Is(err, gitlet.ErrObjectNotFound),
		errors.Is(err, gitlet.ErrInvalidProposal):
		// A PROPOSAL NO NODE WOULD DECIDE (a change write's, checked before
		// routing) means the call's arguments are wrong.
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused what it was asked: %s. Change the "+
			"arguments — the same ones are refused again.", tool, writeWords(err)))
	case errors.Is(err, gitlet.ErrNotPermitted), errors.Is(err, gitlet.ErrConfigManaged):
		return refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s: %s. That is a decision about who is "+
			"asking, so the same call is refused again.", tool, writeWords(err)))
	case errors.Is(err, gitlet.ErrRepoNotFound):
		return codeFailure(ctx, tool, repo, err)
	case errors.As(err, &unknownKeys):
		// THE FLEET RUNS TWO BUILDS, which no retry clears: matched AHEAD of
		// the unserved walk it wraps, which would tell the model to retry a
		// call refused the same way while the fleet stays mixed.
		why, _ := Condition(err)
		ran := "; nothing was written"
		if unknownKeys.Answer {
			ran = ""
		}
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not write to %s (%s)%s. %s",
			tool, repo, why, ran, stuckWrite))
	case errors.As(err, &unready), errors.Is(err, gitlet.ErrNotServing), errors.As(err, &unserved):
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s: Gitlet could not be reached "+
			"right now (no node that builds or decides it could take it in time); nothing was written — %s, "+
			"which is the same write.", tool, again))
	}
	if why, ok := Condition(err); ok {
		if said, _ := condition(err); said.stuck {
			// A CONDITION NO WAIT CLEARS: the same write again is refused
			// the same way ([conditionNext]).
			return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not write to %s (%s); "+
				"nothing is known to have been written. %s", tool, repo, why, stuckWrite))
		}
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not write to %s right now (%s); "+
			"nothing is known to have been written — %s, which is the same write.", tool, repo, why, again))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s could not write to %s: %s.", tool, repo, faultSaid))
}

// writeWords is a write refusal's own sentence: the words its domain wrote
// for the caller past the sentinel.
func writeWords(err error) string {
	for _, sentinel := range []error{gitlet.ErrInvalid, gitlet.ErrObjectNotFound, gitlet.ErrNotPermitted,
		gitlet.ErrConfigManaged, gitlet.ErrInvalidProposal} {
		if said, ok := RefusalSentence(err, sentinel); ok {
			return said
		}
	}
	return err.Error()
}
