package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The change writes: a change opened, edited (title, body, work item, draft
// state, target), closed and reopened, each ONE write the repository's
// committer decides from its own chart (gitlet.ChangeProposal) beside its
// pushes, delivered on Gitlet's surface (gitlet.Source). The gate admits
// everybody (`authz.ClassEverybody`), as a push's does: who may open or steer
// a change (rule D21) is the committer's to decide, which no verb-keyed row
// can answer before the change is read. [changeToolNeeds] names each call's
// least access; in a woken turn the Ceiling and WrittenFor every write carries
// hold it to its waker's too (ADR-0109), the seat's own work excepted. Each
// call's operation follows the package doc ("How a write is made once"), and
// an open's change id derives from it (gitlet.OpenChangeID), so a repeated
// open opens no second change.

// The change write tools' names: each is its authority row's action.
const (
	CodeOpenChangeTool   = string(authz.ActionCodeOpenChange)
	CodeUpdateChangeTool = string(authz.ActionCodeUpdateChange)
	CodeCloseChangeTool  = string(authz.ActionCodeCloseChange)
)

// ChangeWrites are the tools that write a repository's changes, in the order a
// catalogue lists them: the last of [CodeWrites], and with [ChangeToolNames]
// every change tool [changeToolNeeds] names an access for.
func ChangeWrites() []string {
	return []string{CodeOpenChangeTool, CodeUpdateChangeTool, CodeCloseChangeTool, CodeRequestReviewTool,
		CodeReviewChangeTool, CodeCommentOnChangeTool, CodeDeleteCommentTool, CodeResolveFindingTool,
		CodeApplySuggestionsTool, CodeRebaseChangeTool, CodeMergeChangeTool, CodeDequeueChangeTool,
		CodeRevertChangeTool}
}

// changeWriteTools are the change writes over deps, in [ChangeWrites]' order:
// a change's lifecycle, then its conversation (codechangeconverse.go), then
// its findings' suggestions applied (codechangesuggest.go), the change
// rebased onto its target (codechangerebase.go), merged into it or taken
// out of its queue (codechangemerge.go), and reverted by a change of its own
// (codechangerevert.go).
func changeWriteTools(deps CodeDeps) []tools.Callable {
	return []tools.Callable{&codeOpenChange{deps: deps}, &codeUpdateChange{deps: deps},
		&codeCloseChange{deps: deps}, &codeRequestReview{deps: deps}, &codeReviewChange{deps: deps},
		&codeCommentOnChange{deps: deps}, &codeDeleteComment{deps: deps}, &codeResolveFinding{deps: deps},
		&codeApplySuggestions{deps: deps},
		&codeRebaseChange{deps: deps}, &codeMergeChange{deps: deps}, &codeDequeueChange{deps: deps},
		&codeRevertChange{deps: deps}}
}

// changeVerb names every change write's operation ([opIDFor]), its object the
// repository's id, so no branch or title pushes an id past
// statelog.MaxCallerOpIDBytes. One verb serves all: the tool is already in the
// arguments a turn's id digests and in an operator's operation.
const changeVerb = "code-change"

// stepTwo names the second write of a call that makes two (an open's ask of
// its reviewers) as a STEP of the first's operation (statelog.StepOpID), so a
// repeat makes the same second write, which the ledger collapses.
//
// ONE BYTE: the operator's step of a change write's operation is already 125
// bytes at the longest change tool's name, leaving the step's name two bytes
// and a separator under statelog.MaxCallerOpIDBytes; a word would push every
// id past it (TestEveryChangeWriteStaysInsideTheOpIDBound).
const stepTwo = "2"

// ---- the change a call names ------------------------------------------------ //

// changeWritten is the change a write of an existing change is about, as the
// call's own read of it found it: its row in detail, and the position it was
// read at, which the write is decided at or past.
type changeWritten struct {
	detail *gitlet.ChangeDetail
	at     statelog.Position
}

// changeOf reads the change g names: ONE seat-level read of its detail alone
// (gitlet.ReadQuery.DetailOnly: no write reads a revision), held to the
// caller's grant and resolving the repository where the turn has not
// ([changeRead.ask]), for the id a write names and the position it is decided
// at or past. A detail naming another repository than the turn resolved is
// refused as the name taken again between reads ([changeRead.read]).
func (d CodeDeps) changeOf(ctx context.Context, g *changeRead) (changeWritten, *tools.Result) {
	answer, refusal := g.read(ctx, gitlet.ReadQuery{DetailOnly: true}, statelog.Position{})
	if refusal != nil {
		return changeWritten{}, refusal
	}
	if answer.Change == nil {
		r := faulted(ctx, g.tool, fmt.Errorf("builtin: Gitlet answered %s with no detail", g.ref()),
			fmt.Sprintf("%s could not read %s: %s.", g.tool, g.ref(), faultSaid))
		return changeWritten{}, &r
	}
	return changeWritten{detail: answer.Change, at: answer.Position}, nil
}

// openSeen is the position an open is decided at or past: what its caller has
// read of the repository — for a seat, this node's own writes to it and the
// turn's resolution of it, whichever is later (as a seat's engine write is
// built at); for a person, the resolution, read at the log's head.
func (d CodeDeps) openSeen(c codeCaller, repo gitlet.RepoRef) statelog.Position {
	if c.asker.Kind != gitlet.PrincipalAgent {
		return repo.Position
	}
	return furthestOnLog(d.Code.FloorOf(repo.ID), repo.Position)
}

// mentionsOf is the company's seat handles texts name ([CodeDeps.Mentions]),
// each once in first-named order, for a change write's Mentions (rule D38:
// resolved at the write by its asker, held to the deciding node's chart), or
// the refusal, before anything is read, of more than gitlet.MaxMentions: the
// deciding node refuses that list anyway, and this names the bound rather
// than letting a mention go unheard.
func (d CodeDeps) mentionsOf(tool string, texts ...string) ([]string, *tools.Result) {
	if d.Mentions == nil {
		return nil, nil
	}
	var out []string
	for _, text := range texts {
		for _, handle := range d.Mentions.Mentions(text) {
			if !slices.Contains(out, handle) {
				out = append(out, handle)
			}
		}
	}
	if len(out) > gitlet.MaxMentions {
		r := failed(fmt.Sprintf("%s: the text names %d colleagues, past the %d one write of a change mentions — "+
			"mention fewer, and say the rest without an @.", tool, len(out), gitlet.MaxMentions))
		return nil, &r
	}
	return out, nil
}

// expectedVersionArg is a write's REQUIRED `expected_version`: the change's
// code_changes.version the caller read, so a change moved since (an edit, a
// push moving its head) refuses the write stale. Or the refusal of none.
func expectedVersionArg(tool string, args map[string]any) (int64, *tools.Result) {
	v := int64(argInt(args, "expected_version", 0))
	if v < 1 {
		r := failed(fmt.Sprintf("%s needs `expected_version`: the change's `version` as %s answered it — a "+
			"write is held to the change you read, so one that moved since is not changed under you.", tool,
			CodeGetChangeTool))
		return 0, &r
	}
	return v, nil
}

// branchArg is a branch argument as a change names it — in full, under
// refs/heads/ — from a name with or without that root, or the refusal of one
// git does not take.
func branchArg(tool, arg string, args map[string]any) (string, *tools.Result) {
	branch := strings.TrimPrefix(strings.TrimSpace(argString(args, arg)), branchRoot)
	if !gitref.ValidBranch(branch) {
		r := failed(fmt.Sprintf("%s needs `%s`: a branch's name as refs/heads/ holds it (`main`, "+
			"`seats/<handle>/fix`) — %q is not one git takes.", tool, arg, clip(branch)))
		return "", &r
	}
	return branchRoot + branch, nil
}

// ---- the work a change carries ---------------------------------------------- //

// noWorkItem is what `work_item` says to carry none.
const noWorkItem = "none"

// workItemArg is the `work_item` a call names, an item [CodeDeps.workItemOf]
// resolves or `none`, read before anything of the change is: given false where
// the call names none (an open then carries the turn's item), clear for
// `none`. A change carries no spend of its own.
func (d CodeDeps) workItemArg(ctx context.Context, tool string, args map[string]any) (item *types.WorkItem,
	given, clear bool, refusal *tools.Result) {

	if !workItemAsked(args) {
		return nil, false, false, nil
	}
	named := strings.TrimSpace(argString(args, "work_item"))
	switch {
	case named == "":
		r := failed(fmt.Sprintf("%s: `work_item` names an item — its key (ENG-42), `<backend>:<id>`, or "+
			"`%s` — or is left out.", tool, noWorkItem))
		return nil, false, false, &r
	case named == noWorkItem:
		return nil, true, true, nil
	}
	if item, refusal = d.workItemOf(ctx, tool, "work_item", named); refusal != nil {
		return nil, false, false, refusal
	}
	return item, true, false, nil
}

// workItemAsked is whether a call names a `work_item`, from presence alone, so
// a call is judged before anything is read to resolve it. A null one is no
// ask: the entry leaves it out (toolargs.Of).
func workItemAsked(args map[string]any) bool { return args["work_item"] != nil }

// workItemOf is the work item a change write names: a Worklet key or id, bare
// or under `native:`, resolved by [CodeDeps.workletItem], or another
// tracker's `<backend>:<id>`; or the refusal, naming arg, of a name that is
// none. A WORKLET ID IS READ TOO: a change carrying an item that does not
// exist names nothing it implements. code_list_changes names `work` the same
// way but resolves less ([CodeDeps.listedWorkItem]).
func (d CodeDeps) workItemOf(ctx context.Context, tool, arg, named string) (*types.WorkItem, *tools.Result) {
	item, local, refusal := namedWorkItem(tool, arg, named)
	if item != nil || refusal != nil {
		return item, refusal
	}
	return d.workletItem(ctx, tool, arg, local,
		"a change carrying an item that does not exist names nothing it implements")
}

// listedWorkItem is the work item code_list_changes' `work` names, spelled as
// a change's row holds it and named as a change write names one
// ([CodeDeps.workItemOf]), so the key a seat was woken on lists the changes
// filed under it. ONLY A KEY IS RESOLVED; an identity (`<backend>:<id>`, or a
// Worklet id per [worklet.IsTaskID]) is listed as named. Proving its task
// exists would cost every listing a second linearizable barrier (and an
// estate-router hop off a data node) for nothing it uses, and would refuse a
// true answer: Gitlet does not follow a tracker purge, so a change still
// carries an item whose task is gone.
func (d CodeDeps) listedWorkItem(ctx context.Context, tool, arg, named string) (*types.WorkItem, *tools.Result) {
	item, local, refusal := namedWorkItem(tool, arg, named)
	switch {
	case item != nil || refusal != nil:
		return item, refusal
	case worklet.IsTaskID(local):
		return fittedItem(types.WorkItem{Backend: types.WorkWorklet, ID: local}), nil
	}
	return d.workletItem(ctx, tool, arg, local,
		"a key is listed through the task that holds it, and an item named as a change prints it, "+
			"`native:<id>`, is listed as named")
}

// namedWorkItem parses a work item's name, reading nothing: another tracker's
// `<backend>:<id>`, fitted; a Worklet key or id, bare or under `native:`, left
// as local for the caller to resolve; or the refusal, naming arg, of a tracker
// named with no item. Every field is FITTED (gitlet.FitProvenance): an item
// is another system's label, never a reason to refuse a write, and a listing
// asks for it as rows hold it.
func namedWorkItem(tool, arg, named string) (item *types.WorkItem, local string, refusal *tools.Result) {
	backend, id, ok := strings.Cut(named, ":")
	if !ok || !types.WorkBackend(backend).Valid() {
		return nil, named, nil
	}
	if id = strings.TrimSpace(id); id == "" {
		r := failed(fmt.Sprintf("%s: `%s` %q names a tracker and no item in it.", tool, arg, clip(named)))
		return nil, "", &r
	}
	if types.WorkBackend(backend) != types.WorkWorklet {
		return fittedItem(types.WorkItem{Backend: types.WorkBackend(backend), ID: id}), "", nil
	}
	return nil, id, nil
}

// workletItem is the Worklet item named, a key or an id, read through
// [CodeDeps.Tasks] so it carries its id (which renaming or moving a key does
// not change) beside its key; or the refusal, naming arg and ending with why,
// of a name no task holds.
func (d CodeDeps) workletItem(ctx context.Context, tool, arg, named, why string) (*types.WorkItem, *tools.Result) {
	if d.Tasks == nil {
		r := failed(fmt.Sprintf("%s: %q is no item this company's tracker can name here — Worklet "+
			"is not served on this surface. Name an item as `<backend>:<id>`.", tool, clip(named)))
		return nil, &r
	}
	detail, err := d.Tasks.Task(ctx, named, worklet.DetailWants{}, seatRead)
	switch {
	case errors.Is(err, worklet.ErrNoTask):
		r := failed(fmt.Sprintf("%s: `%s` %q names no work item. Check its key with %s rather than guessing — %s.",
			tool, arg, clip(named), worklet.ListWorkItemsTool, why))
		return nil, &r
	case err != nil:
		r := readFailure(ctx, tool, err)
		return nil, &r
	}
	t := detail.Task
	return fittedItem(types.WorkItem{Backend: types.WorkWorklet, ID: t.ID, Key: t.Key, Project: t.Project}), nil
}

// turnItem is the work item a seat's turn is charged to, fitted as every item
// a change carries is — nil for none, and for a call with no turn.
func turnItem(turn *turnctx.Turn) *types.WorkItem {
	if turn == nil || turn.WorkItem == nil {
		return nil
	}
	return fittedItem(*turn.WorkItem)
}

// fittedItem is w with each of its labels fitted to what a change's row holds
// (gitlet.FitProvenance).
func fittedItem(w types.WorkItem) *types.WorkItem {
	return &types.WorkItem{Backend: types.WorkBackend(gitlet.FitProvenance(string(w.Backend))),
		ID: gitlet.FitProvenance(w.ID), Key: gitlet.FitProvenance(w.Key), Project: gitlet.FitProvenance(w.Project)}
}

// ---- code_open_change ------------------------------------------------------- //

type codeOpenChange struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeOpenChange)(nil)

func (t *codeOpenChange) Name() string { return CodeOpenChangeTool }

func (t *codeOpenChange) Description() string {
	return "Open a change: a branch proposed into another — the default branch unless `target` names one — " +
		"carrying the turn's work item unless `work_item` names another, and asking `reviewers` once it is open."
}

// workItemParam is the change writes' `work_item`: a Worklet key or id (the id
// where the row read carries `key_collision`, whose key opens the task that
// claimed it first: workaddress.go's itemRef), any tracker's identity, or none.
func workItemParam() map[string]any {
	return map[string]any{"type": "string",
		"description": "Its key (ENG-42) or id — its id where you read it with `key_collision` — " +
			"`<backend>:<id>`, or `" + noWorkItem + "`."}
}

func (t *codeOpenChange) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":      codeRepoArg(),
			"branch":    map[string]any{"type": "string", "description": "What it proposes: a branch's name."},
			"target":    map[string]any{"type": "string"},
			"title":     map[string]any{"type": "string"},
			"body":      map[string]any{"type": "string"},
			"draft":     map[string]any{"type": "boolean"},
			"reviewers": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"work_item": workItemParam(),
		},
		"required": []string{"repo", "branch", "title"},
	})
}

func (t *codeOpenChange) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeOpenChange) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	tool := t.Name()
	c, refusal := t.deps.writeCaller(ctx, turn, tool)
	if refusal != nil {
		return *refusal, nil
	}
	// EVERY ARGUMENT FIRST, so a malformed call costs no read and mints no
	// operation; the reviewers too, since a call whose second write could never
	// be made opens nothing.
	name, refusal := repoArg(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	source, refusal := branchArg(tool, "branch", args)
	if refusal != nil {
		return *refusal, nil
	}
	target := ""
	if strings.TrimSpace(argString(args, "target")) != "" {
		if target, refusal = branchArg(tool, "target", args); refusal != nil {
			return *refusal, nil
		}
	}
	reviewers, refusal := reviewersArg(tool, "reviewers", args, c.asker)
	if refusal != nil {
		return *refusal, nil
	}
	open := gitlet.ChangeOpen{SourceRef: source, TargetRef: target, Title: argString(args, "title"),
		Body: argString(args, "body"), Draft: argBool(args, "draft")}
	if open.Mentions, refusal = t.deps.mentionsOf(tool, open.Title, open.Body); refusal != nil {
		return *refusal, nil
	}
	item, given, clear, refusal := t.deps.workItemArg(ctx, tool, args)
	switch {
	case refusal != nil:
		return *refusal, nil
	case given && !clear:
		open.WorkItem = item
	case !given:
		open.WorkItem = turnItem(turn)
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
	if open.TargetRef == "" {
		open.TargetRef = branchRoot + repo.DefaultBranch
	}
	// THE BRANCHES' HOLDS before the open is sent ([CodeDeps.branch]): source
	// and target, where a seat's branch, are held as a read is, and a source
	// the resolve finds missing as a write's is, in one read and one Take.
	if refusal = t.deps.branch(ctx, turn, tool, c, repo, true, open.SourceRef, open.TargetRef); refusal != nil {
		return *refusal, nil
	}
	if refusal = t.deps.heldByTarget(ctx, turn, tool, c, repo, open.SourceRef, open.TargetRef); refusal != nil {
		return *refusal, nil
	}
	op := opIDFor(actor, tool, changeVerb, repo.ID, args)
	open.RepoID, open.ChangeID = repo.ID, gitlet.OpenChangeID(op)
	if turn != nil && c.asker.Kind == gitlet.PrincipalAgent {
		open.TurnID = turn.RunID
	}
	p := gitlet.ChangeProposal{OpID: op, Seen: t.deps.openSeen(c, repo), Open: &open}
	res, refusal := t.deps.proposeChange(ctx, turn, tool, c, repo, p)
	if refusal != nil {
		return *refusal, nil
	}
	out := codeChangeWriteAnswer{Repo: repo.Name, RenamedTo: repo.Redirect, RedirectUntil: repo.RedirectUntil,
		Outcome: res.Outcome, Version: res.Version, Position: positionText(res.Position), OpID: c.actor.Operation}
	if res.Number > 0 {
		// A PENDING OPEN NAMES NO NUMBER: the apply that opens the change
		// numbers it, and none has run where the answer was formed.
		out.Change = gitlet.ChangeRef(repo.Name, res.Number)
	}
	if len(reviewers) > 0 {
		t.deps.askOpened(ctx, c, repo, op, res, reviewers, &out)
	}
	return newForeignRead(turn, c.asker.Seat).page(ctx, tool, out, "")
}

// heldByTarget refuses an open whose source its target already holds, as the
// committer refuses one whose source is AT its target (no_difference): such a
// change proposes nothing, and its home would mark it merged at once. The
// committer's decide reads rows, which see two tips that differ, so the
// histories are read here, one landed_in of the source's tip in the target,
// before anything is written. A branch missing, or both at one commit, is
// left to the decide, which refuses either in its own words.
func (d CodeDeps) heldByTarget(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	repo gitlet.RepoRef, source, target string) *tools.Result {

	answer, refusal := d.refsOf(ctx, turn, tool, c, repo, source, target)
	if refusal != nil {
		return refusal
	}
	if len(answer.Refs) != 2 {
		return nil
	}
	src, tgt := answer.Refs[0], answer.Refs[1]
	if src.Missing != "" || tgt.Missing != "" || src.OID == tgt.OID {
		return nil
	}
	page, refusal := d.gitRead(ctx, turn, tool, c, repo, gitlet.GitQuery{Kind: gitlet.GitLandedIn,
		RepoID: repo.ID, Fresh: gitlet.RefsFloor(d.Code.FloorOf(repo.ID), src, tgt),
		LandedIn: &gitlet.LandedInQuery{Commit: src.OID, Target: tgt.Ref, TargetOID: tgt.OID, At: tgt.Moved}})
	if refusal != nil {
		return refusal
	}
	if page.LandedIn == nil || !page.LandedIn.Landed {
		return nil
	}
	r := changeRefused(tool, gitlet.ChangeResult{Refusal: gitlet.ChangeRefusalNoDifference,
		Detail: fmt.Sprintf("%s already holds every commit of %s, whose head %s is in its history: the source "+
			"holds nothing its target does not", tgt.Ref, src.Ref, abbrevOID(src.OID))})
	return &r
}

// reviewersArg is a call's reviewers to ask (seat handles, a person's being
// their human seat's), or the refusal, before anything is written, of one no
// ask could name: a handle no seat may have, past
// gitlet.MaxReviewersPerChange, or the caller, whose review of their own
// change counts for nothing.
func reviewersArg(tool, arg string, args map[string]any, asker gitlet.Principal) ([]string, *tools.Result) {
	handles := argStrings(args, arg)
	if len(handles) > gitlet.MaxReviewersPerChange {
		r := failed(fmt.Sprintf("%s: `%s` names %d reviewers, past the %d a change asks at once.", tool, arg,
			len(handles), gitlet.MaxReviewersPerChange))
		return nil, &r
	}
	for _, h := range handles {
		switch {
		case !org.ValidHandle(h):
			r := failed(fmt.Sprintf("%s: `%s` names %q, which is no seat's handle.", tool, arg, clip(h)))
			return nil, &r
		case h == asker.Seat:
			r := failed(fmt.Sprintf("%s: `%s` names %s, who writes the change: an author's review of their own "+
				"change counts for nothing. Name others.", tool, arg, h))
			return nil, &r
		}
	}
	return handles, nil
}

// askOpened makes an open's second write, the ask of its reviewers, once the
// change is open (applied, or pending: on the log, so the ask is decided at or
// past it), as a STEP of the open's operation. An open that landed nothing
// asks nobody. The ask's fate is said beside the change, never as the open's
// failure: the change WAS opened, and the same call again opens nothing twice.
func (d CodeDeps) askOpened(ctx context.Context, c codeCaller, repo gitlet.RepoRef,
	openOp string, opened gitlet.ChangeResult, reviewers []string, out *codeChangeWriteAnswer) {

	if opened.Outcome != statelog.OutcomeApplied && opened.Outcome != statelog.OutcomePending {
		return
	}
	ask := gitlet.ReviewerRequest{RepoID: repo.ID, ChangeID: opened.ChangeID}
	for _, h := range reviewers {
		ask.Request = append(ask.Request, gitlet.Reviewer{Handle: h})
	}
	w := gitlet.ConversationWrite{OpID: statelog.StepOpID(openOp, stepTwo), Asker: c.asker, Ceiling: c.write,
		WrittenFor: c.actor.WrittenFor, Seen: opened.Position, Reviewers: &ask}
	named := out.Change
	if named == "" {
		named = "a change of " + repo.Name + " its apply has yet to number"
	}
	res, err := d.converse(ctx, w)
	switch {
	case err != nil:
		out.ReviewersNotAsked = notAsked(named, writeErrWords(err), sameCall(c.actor, CodeOpenChangeTool))
	case res.Refusal != "":
		// A REFUSAL IS REFUSED AGAIN by the same call, which asks the same
		// reviewers: asking others is a call of its own.
		out.ReviewersNotAsked = notAsked(named, fmt.Sprintf("%s (%s)", res.Detail, res.Refusal), "") +
			fmt.Sprintf(" Ask others with %s.", CodeRequestReviewTool)
	case res.Outcome == statelog.OutcomeApplied || res.Outcome == statelog.OutcomePending:
		out.Reviewers = reviewers
	default:
		out.ReviewersNotAsked = notAsked(named, "whether the ask landed is unknown",
			sameCall(c.actor, CodeOpenChangeTool))
	}
}

// notAsked is the sentence of an open whose ask did not finish: the change WAS
// opened, so the way on is the same call, never another open (again; "" where
// this caller has no such repeat).
func notAsked(change, why, again string) string {
	said := fmt.Sprintf("The change WAS opened, as %s, but asking its reviewers did not finish: %s.", change, why)
	if again != "" {
		return said + " To finish it, " + again + ": it opens nothing twice."
	}
	return said + " The change stands without them."
}

// writeErrWords is a write's error as a sentence names it: its domain's own
// words past the sentinel where it carries one, or the error.
func writeErrWords(err error) string {
	if errors.Is(err, estate.ErrOutcomeUnknown) {
		return "whether it landed is unknown"
	}
	return writeWords(err)
}

// ---- code_update_change ----------------------------------------------------- //

type codeUpdateChange struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeUpdateChange)(nil)

func (t *codeUpdateChange) Name() string { return CodeUpdateChangeTool }

func (t *codeUpdateChange) Description() string {
	return "Change ONE thing about a change a call: what it says (any of `title`, `body`, `work_item`), " +
		"`draft` (a draft, or ready for review), or `target`."
}

func (t *codeUpdateChange) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":             changeRepoParam(),
			"change":           changeParam(),
			"expected_version": expectedVersionParam(),
			"title":            map[string]any{"type": "string"},
			"body":             map[string]any{"type": "string"},
			"work_item":        workItemParam(),
			"draft":            map[string]any{"type": "boolean"},
			"target":           map[string]any{"type": "string"},
		},
		"required": []string{"change", "expected_version"},
	})
}

// changeRepoParam, changeParam and expectedVersionParam are the arguments a
// write of an existing change names it by, each a fresh map for every schema
// it is put in.
func changeRepoParam() map[string]any {
	return map[string]any{"type": "string", "description": "Not needed where `change` names it."}
}

func changeParam() map[string]any {
	return map[string]any{"type": "string", "description": "Its number on `repo`, or its reference (web!42)."}
}

func expectedVersionParam() map[string]any {
	return map[string]any{"type": "integer", "description": "The change's `version`, as you read it."}
}

func (t *codeUpdateChange) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeUpdateChange) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	tool := t.Name()
	c, refusal := t.deps.writeCaller(ctx, turn, tool)
	if refusal != nil {
		return *refusal, nil
	}
	name, number, refusal := changeArg(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	version, refusal := expectedVersionArg(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	p, refusal := t.update(ctx, args)
	if refusal != nil {
		return *refusal, nil
	}
	return t.deps.steer(ctx, turn, tool, c, args, name, number, version, p)
}

// update is the one write a call asks for (an edit, a draft's state or a
// retarget) with every field but the change's ids, filled once it is read; or
// the refusal of a call asking for none or more than one.
func (t *codeUpdateChange) update(ctx context.Context, args map[string]any) (gitlet.ChangeProposal, *tools.Result) {
	tool := t.Name()
	var edit gitlet.ChangeEdit
	// PRESENCE IS THE ASK: a null `title` never reaches a tool (toolargs.Of),
	// and an empty one is an edit the change writer refuses by its own rule.
	if _, held := args["title"]; held {
		title := argString(args, "title")
		edit.Title = &title
	}
	if _, held := args["body"]; held {
		body := argString(args, "body")
		edit.Body = &body
	}
	// COUNTED FROM PRESENCE, BEFORE THE ITEM IS RESOLVED: a Worklet key costs
	// a tracker read, which a call asking for more than one thing never makes.
	edits := edit.Title != nil || edit.Body != nil || workItemAsked(args)
	_, draft := argval.Bool(args["draft"])
	retarget := strings.TrimSpace(argString(args, "target")) != ""
	asked := 0
	for _, set := range []bool{edits, draft, retarget} {
		if set {
			asked++
		}
	}
	if asked != 1 {
		r := failed(fmt.Sprintf("%s changes ONE thing a call: what the change says (any of `title`, `body`, "+
			"`work_item`), `draft`, or `target` — each is a record of its own.", tool))
		return gitlet.ChangeProposal{}, &r
	}
	var refusal *tools.Result
	switch {
	case draft:
		return gitlet.ChangeProposal{Draft: &gitlet.ChangeDraft{Draft: argBool(args, "draft")}}, nil
	case retarget:
		var target string
		if target, refusal = branchArg(tool, "target", args); refusal != nil {
			return gitlet.ChangeProposal{}, refusal
		}
		return gitlet.ChangeProposal{Retarget: &gitlet.ChangeRetarget{TargetRef: target}}, nil
	}
	item, _, clear, refusal := t.deps.workItemArg(ctx, tool, args)
	if refusal != nil {
		return gitlet.ChangeProposal{}, refusal
	}
	edit.WorkItem, edit.ClearWorkItem = item, clear
	// WHAT THE EDIT SETS NAMES ITS MENTIONS, and nothing it leaves standing:
	// whoever the old title or body named was told when it was written.
	var said []string
	for _, text := range []*string{edit.Title, edit.Body} {
		if text != nil {
			said = append(said, *text)
		}
	}
	if edit.Mentions, refusal = t.deps.mentionsOf(tool, said...); refusal != nil {
		return gitlet.ChangeProposal{}, refusal
	}
	return gitlet.ChangeProposal{Edit: &edit}, nil
}

// ---- code_close_change ------------------------------------------------------ //

type codeCloseChange struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeCloseChange)(nil)

func (t *codeCloseChange) Name() string { return CodeCloseChangeTool }

func (t *codeCloseChange) Description() string {
	return "Close a change without merging it, or with `reopen` open a closed one again at its branch's head."
}

func (t *codeCloseChange) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":             changeRepoParam(),
			"change":           changeParam(),
			"expected_version": expectedVersionParam(),
			"reason":           map[string]any{"type": "string"},
			"reopen":           map[string]any{"type": "boolean"},
		},
		"required": []string{"change", "expected_version"},
	})
}

func (t *codeCloseChange) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeCloseChange) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	tool := t.Name()
	c, refusal := t.deps.writeCaller(ctx, turn, tool)
	if refusal != nil {
		return *refusal, nil
	}
	name, number, refusal := changeArg(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	version, refusal := expectedVersionArg(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	p := gitlet.ChangeProposal{Close: &gitlet.ChangeClose{Reason: strings.TrimSpace(argString(args, "reason"))}}
	if argBool(args, "reopen") {
		if p.Close.Reason != "" {
			return failed(fmt.Sprintf("%s: a reopen takes no `reason` — it is the close that says why.",
				tool)), nil
		}
		p = gitlet.ChangeProposal{Reopen: &gitlet.ChangeReopen{}}
	}
	return t.deps.steer(ctx, turn, tool, c, args, name, number, version, p)
}

// ---- a write of an existing change ------------------------------------------- //

// steer makes p, a write of change number on repository name with every field
// but its ids, held to the version its caller read: ONE seat-level read of the
// change for its id ([CodeDeps.changeOf]), then the write decided at or past
// that read.
func (d CodeDeps) steer(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	args map[string]any, name string, number, version int64, p gitlet.ChangeProposal) (tools.Result, error) {

	actor, bound := d.bindOperation(c.actor, tool, args)
	if bound != "" {
		return failed(bound), nil
	}
	c.actor = actor
	g := &changeRead{deps: d, turn: turn, tool: tool, c: c, name: name, number: number}
	change, refusal := d.changeOf(ctx, g)
	if refusal != nil {
		return *refusal, nil
	}
	repo := g.repo
	// A RETARGET OR A REOPEN DEPENDS ON A BRANCH (the new target, the source
	// it reopens at), so a seat's branch is held first, as a read is
	// ([CodeDeps.branch]).
	held := ""
	switch {
	case p.Retarget != nil:
		held = p.Retarget.TargetRef
	case p.Reopen != nil:
		held = change.detail.SourceRef
	}
	if held != "" {
		if refusal = d.branch(ctx, turn, tool, c, repo, true, held); refusal != nil {
			return *refusal, nil
		}
	}
	id := change.detail.ID
	switch {
	case p.Edit != nil:
		p.Edit.RepoID, p.Edit.ChangeID = repo.ID, id
	case p.Draft != nil:
		p.Draft.RepoID, p.Draft.ChangeID = repo.ID, id
	case p.Retarget != nil:
		p.Retarget.RepoID, p.Retarget.ChangeID = repo.ID, id
	case p.Close != nil:
		p.Close.RepoID, p.Close.ChangeID = repo.ID, id
	case p.Reopen != nil:
		p.Reopen.RepoID, p.Reopen.ChangeID = repo.ID, id
	}
	p.OpID, p.Seen, p.ExpectedVersion = opIDFor(actor, tool, changeVerb, repo.ID, args), change.at, version
	res, refusal := d.proposeChange(ctx, turn, tool, c, repo, p)
	if refusal != nil {
		return *refusal, nil
	}
	out := codeChangeWriteAnswer{Repo: repo.Name, RenamedTo: repo.Redirect, RedirectUntil: repo.RedirectUntil,
		Change: gitlet.ChangeRef(repo.Name, number), Outcome: res.Outcome, Unchanged: res.Unchanged,
		Version: res.Version, Position: positionText(res.Position), OpID: c.actor.Operation}
	return newForeignRead(turn, c.asker.Seat).page(ctx, tool, out, "")
}

// codeChangeWriteAnswer is a change write's answer: the change, the write's
// outcome (Unchanged, with no record, where it asked what the change already
// says), and the version the caller's next write names (0 where a later
// record moved the change, which is then read again).
type codeChangeWriteAnswer struct {
	Repo          string           `json:"repo"`
	RenamedTo     string           `json:"renamed_to,omitempty"`
	RedirectUntil time.Time        `json:"redirect_until,omitzero"`
	Change        string           `json:"change,omitempty"`
	Outcome       statelog.Outcome `json:"outcome"`
	Unchanged     bool             `json:"unchanged,omitempty"`
	Version       int64            `json:"version,omitempty"`
	Position      string           `json:"position,omitempty"`
	// Reviewers are those an open asked once the change was open, and
	// ReviewersNotAsked why it asked none: the change opened all the same.
	Reviewers         []string `json:"reviewers,omitempty"`
	ReviewersNotAsked string   `json:"reviewers_not_asked,omitempty"`
	OpID              string   `json:"op_id,omitempty"`
}

// proposeChange asks the repository's committer to decide p for c: its asker,
// ceiling and WrittenFor stamped together from the caller, never from a tool's
// arguments, and held first to gitlet.ChangeProposal.Validate, so a proposal
// no node would decide is refused naming its field before it is routed. It
// answers what p came to, or the call's refusal: the write refused in its
// class, an outcome unknown, or no committer reached.
func (d CodeDeps) proposeChange(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	repo gitlet.RepoRef, p gitlet.ChangeProposal) (gitlet.ChangeResult, *tools.Result) {

	p.Asker, p.Ceiling, p.WrittenFor = c.asker, c.write, c.actor.WrittenFor
	if err := p.Validate(); err != nil {
		r := codeWriteFailure(ctx, tool, c.actor, repo.Name, err)
		return gitlet.ChangeResult{}, &r
	}
	res, err := d.Writer.Change(ctx, p)
	if err != nil {
		r := d.failedWrite(ctx, turn, tool, c, repo, p.OpID, err)
		return gitlet.ChangeResult{}, &r
	}
	commit(turn, res.Outcome, types.CommittedCode)
	if res.Refusal != "" {
		r := changeRefused(tool, res)
		return gitlet.ChangeResult{}, &r
	}
	switch res.Outcome {
	case statelog.OutcomeApplied, statelog.OutcomePending:
		return res, nil
	case statelog.OutcomeUnknown:
		r := unknownWrite(c.actor, tool, fmt.Sprintf("the write of a change on %s landed", repo.Name), p.OpID, false,
			unknownNext(false, sameCall(c.actor, tool), fmt.Sprintf("Read the change with %s", CodeGetChangeTool),
				"the same write would be asked twice"))
		return gitlet.ChangeResult{}, &r
	}
	r := faulted(ctx, tool, fmt.Errorf("builtin: Gitlet answered a change write with outcome %q and no "+
		"refusal", res.Outcome), fmt.Sprintf("%s could not tell what became of the write: %s.", tool, faultSaid))
	return gitlet.ChangeResult{}, &r
}

// converse asks the repository's committing data node to decide w — held first
// to the rule every change writer holds a write to (gitlet.ConversationWrite's
// Validate) — and answers what it came to.
func (d CodeDeps) converse(ctx context.Context, w gitlet.ConversationWrite) (gitlet.ConversationResult, error) {
	if err := w.Validate(); err != nil {
		return gitlet.ConversationResult{}, err
	}
	return d.Writer.Converse(ctx, w)
}

// changeClass is the class a change lifecycle refusal is answered in, by
// [pushClass]'s rule: a rule refusing the caller FORBIDDEN; a change moved
// since read STALE_VERSION; a branch already heading a live change EXISTS;
// branches no change can be about INVALID (the fix is another call); a state
// refusing the write until somebody changes it CONFLICT (closed, merged, an
// archived or pull-mirrored repository); gone NOT_FOUND; what waiting clears
// UNAVAILABLE (an import finishing among it). The two only a push's receiver
// answers (ambiguous_source, bad_option) are INVALID too, though no tool's
// write draws them.
func changeClass(r gitlet.ChangeRefusal) (tools.Refusal, bool) {
	switch r {
	case gitlet.ChangeRefusalNotPermitted:
		return tools.RefusalForbidden, true
	case gitlet.ChangeRefusalRepoArchived, gitlet.ChangeRefusalRepoMirrored:
		return tools.RefusalConflict, true
	case gitlet.ChangeRefusalStale:
		return tools.RefusalStaleVersion, true
	case gitlet.ChangeRefusalChangeExists:
		return tools.RefusalExists, true
	case gitlet.ChangeRefusalNoSource, gitlet.ChangeRefusalNoTarget, gitlet.ChangeRefusalSameRef,
		gitlet.ChangeRefusalNoDifference, gitlet.ChangeRefusalAmbiguousSource, gitlet.ChangeRefusalBadOption:
		return tools.RefusalInvalid, true
	case gitlet.ChangeRefusalChangeClosed, gitlet.ChangeRefusalChangeMerged:
		return tools.RefusalConflict, true
	case gitlet.ChangeRefusalRepoDeleted, gitlet.ChangeRefusalChangeNotFound:
		return tools.RefusalNotFound, true
	case gitlet.ChangeRefusalBusy, gitlet.ChangeRefusalNotApplied, gitlet.ChangeRefusalUnavailable,
		gitlet.ChangeRefusalRepoImporting:
		return tools.RefusalUnavailable, true
	}
	return "", false
}

// changeRefused is a change write the committer refused, in its class, with
// the decide's own sentence and what to do about it.
func changeRefused(tool string, res gitlet.ChangeResult) tools.Result {
	cause := writeRefusalError(string(res.Refusal), res.Detail)
	said := fmt.Sprintf("%s changed nothing: %s (%s).", tool, res.Detail, res.Refusal)
	class, known := changeClass(res.Refusal)
	if !known {
		return refusedBy(tools.RefusalInternalError, cause, said+" "+faultSaid+".")
	}
	switch class {
	case tools.RefusalStaleVersion:
		said += fmt.Sprintf(" Read the change again with %s, and decide on what it says now.", CodeGetChangeTool)
	case tools.RefusalUnavailable:
		said += " Nothing was written; make the same call again — it is the same write."
	case tools.RefusalForbidden:
		said += " That is a rule of the repository or the change, so the same call is refused again."
	case tools.RefusalConflict:
		said += fmt.Sprintf(" That is the change's or the repository's state, which the sentence names: read "+
			"the change again with %s before you decide, since the same call does not land while it stands.",
			CodeGetChangeTool)
	default:
		said += " The same call is refused again; change what it asks for."
	}
	return refusedBy(class, cause, said)
}
