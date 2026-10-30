package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE CHANGE WRITES: a seat's and a person's assistant's writes of a
// repository's changes, made through a tool — a change opened, edited (its
// title, description or work item, a draft's state, its target), closed and
// reopened — each ONE write of the code host's change domain, decided where
// the change is decided: a change's lifecycle by the repository's committer,
// from its own chart (code.ChangeProposal), on the repository's subject beside
// its pushes. Every one is in [CodeTools] beside the reads and the other code
// writes ([CodeWrites]), so a seat's registry and the operator catalogue take
// one list, and each delivers to the code host's surface (code.Source): a turn
// woken about a change that answered by writing it delivered on the surface
// the work came from.
//
// # Who may is the change's to say
//
// The gate admits every principal (`authz.ClassEverybody`), as it admits a
// push: whether a caller may open a change from a branch — whether they may
// push it — and whether they may steer one — its owner, a principal that
// maintains the repository, or a person (the domain's rule D21) — is decided
// by the repository's committer from its own chart, which no row keyed on the
// verb can answer before the change is read. A person writes as the human seat
// they hold; a credential bound to nobody writes nothing, refused before
// anything is read or routed ([CodeDeps.writeCaller]). [changeToolNeeds] names
// the least access each one's call may be admitted at.
//
// # One operation, so a retry is the same write
//
// Every call names its operation by the one rule every family of write tools
// takes ([operations], [opIDFor]): verb `code-change`, object the repository's
// id — bounded, so no branch or title carries an id past
// statelog.MaxCallerOpIDBytes. An open's change is derived from it
// (code.OpenChangeID), so the same call made again — after an `unknown`, by a
// re-run turn — opens the change the first opened, which the ledger collapses,
// and never a second. A second write the call makes is a STEP of the first's
// operation ([stepTwo]): an open's ask of its reviewers, made once the change
// is open, so a repeat of the call asks them as the first did.
//
// # Each write is held to what its caller read
//
// An edit, a draft's state, a retarget, a close and a reopen name the version
// of the change they were formed against (`expected_version`, the code host's
// code_changes.version, which every read of the change answers and every write
// of it answers anew): a change moved since — an edit, a push that moved its
// head — refuses the write stale, and the caller reads it again. A write
// asking for what the change already says is answered unchanged, with no
// record. And each is decided at or past where its caller read: the
// repository's resolution for an open, the call's own read of the change for
// the rest (code.ChangeProposal.Seen).
//
// # The work a change carries
//
// An open carries a work item: the one the turn is charged to, unless the call
// names another — a native item by its key, resolved by the tracker's own
// read ([CodeDeps.Tasks]), or any tracker's by its identity, `<backend>:<id>`
// — or `none`. A change implements one work item and carries no spend of its
// own (the turn that opened it is charged as ever).

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
		CodeReviewChangeTool, CodeCommentOnChangeTool, CodeResolveFindingTool, CodeApplySuggestionsTool}
}

// changeWriteTools are the change writes over deps, in [ChangeWrites]' order:
// a change's lifecycle, then its conversation (codechangeconverse.go), then
// its findings' suggestions applied (codechangesuggest.go).
func changeWriteTools(deps CodeDeps) []tools.Callable {
	return []tools.Callable{&codeOpenChange{deps: deps}, &codeUpdateChange{deps: deps},
		&codeCloseChange{deps: deps}, &codeRequestReview{deps: deps}, &codeReviewChange{deps: deps},
		&codeCommentOnChange{deps: deps}, &codeResolveFinding{deps: deps}, &codeApplySuggestions{deps: deps}}
}

// changeVerb is the verb a change write's operation is named by (m3-w1's op
// rule): one for every change write, since the tool is in the arguments a
// turn's id digests and in the name an operator's operation carries.
const changeVerb = "code-change"

// stepTwo names the second write of a gesture that makes two — an open's ask
// of its reviewers — as a STEP of the first's operation
// (statelog.StepOpID), so the same call made again makes the same second
// write, which the ledger collapses.
//
// ONE BYTE, BY ITS PLACE: the step's name rides an id of at most
// statelog.MaxCallerOpIDBytes, and the operator's step of a change write's
// operation is already 125 bytes at the longest change tool's name — its
// operation (a UUID, the tool and a digest of its call) and the step that
// names the verb and the repository — so the step's own name has room for two
// bytes and its separator, and a word naming it would push every id past
// what every party holds one to (TestEveryChangeWriteStaysInsideTheOpIDBound).
const stepTwo = "2"

// ---- the change a call names ------------------------------------------------ //

// changeWritten is the change a write of an existing change is about, as the
// call's own read of it found it: its row in detail, and the position it was
// read at, which the write is decided at or past.
type changeWritten struct {
	detail *code.ChangeDetail
	at     statelog.Position
}

// changeOf reads the change number names on repo, at the seat's level — ONE
// read a call, held to the caller's grant on the repository where it is
// answered — for the id a write names it by and the position the write is
// decided at or past. A detail naming another repository than the one the turn
// resolved the name to is refused as a name taken again between the two reads.
func (d CodeDeps) changeOf(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	repo code.RepoRef, number int64) (changeWritten, *tools.Result) {

	answer, err := d.read(ctx, c, code.ReadQuery{Kind: code.ReadChange, Repo: repo.Name, Change: number, Limit: 1,
		Fresh: seatRead})
	if err != nil {
		r := d.failed(ctx, turn, tool, c.asker, repo, err)
		return changeWritten{}, &r
	}
	if answer.Change == nil {
		r := faulted(ctx, tool, fmt.Errorf("builtin: the code host answered %s with no detail",
			code.ChangeRef(repo.Name, number)), fmt.Sprintf("%s could not read %s: %s.", tool,
			code.ChangeRef(repo.Name, number), faultSaid))
		return changeWritten{}, &r
	}
	if r := d.sameRepo(turn, tool, c.asker, repo, code.RepoRef{ID: answer.Change.RepoID}); r != nil {
		return changeWritten{}, r
	}
	return changeWritten{detail: answer.Change, at: answer.Position}, nil
}

// openSeen is the position an open is decided at or past: what its caller has
// read of the repository — for a seat, this node's own writes to it and the
// turn's resolution of it, whichever is later (as a seat's engine write is
// built at); for a person, the resolution, read at the log's head.
func (d CodeDeps) openSeen(c codeCaller, repo code.RepoRef) statelog.Position {
	if c.asker.Kind != code.PrincipalAgent {
		return repo.Position
	}
	return furthestOnLog(d.Code.FloorOf(repo.ID), repo.Position)
}

// expectedVersionArg is a write's `expected_version`, REQUIRED: the version of
// the change the caller read, which every read of it answers — or the refusal
// of a call that names none.
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

// workItemArg is the `work_item` a call names — a native item's key, resolved
// by the tracker's own read ([CodeDeps.Tasks]); any tracker's item by its
// identity, `<backend>:<id>`; or `none` — read before anything of the change
// is: given false where the call names none, and clear where it names `none`.
// Every field is FITTED to the bound a change's row holds a label to
// (code.FitProvenance), as the run a seat's write names is: an item is another
// system's label, copied as it arrived, and never a reason to refuse a write.
func (d CodeDeps) workItemArg(ctx context.Context, tool string, args map[string]any) (item *types.WorkItem,
	given, clear bool, refusal *tools.Result) {

	raw, present := args["work_item"]
	if !present || raw == nil {
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
	if backend, id, ok := strings.Cut(named, ":"); ok && types.WorkBackend(backend).Valid() {
		if strings.TrimSpace(id) == "" {
			r := failed(fmt.Sprintf("%s: `work_item` %q names a tracker and no item in it.", tool, clip(named)))
			return nil, false, false, &r
		}
		if types.WorkBackend(backend) != types.WorkNative {
			return fittedItem(types.WorkItem{Backend: types.WorkBackend(backend), ID: strings.TrimSpace(id)}),
				true, false, nil
		}
		named = strings.TrimSpace(id)
	}
	// A NATIVE ITEM, by its key or its id: read, so the change carries the
	// item's identity — its id, which a key renamed or moved does not change
	// — beside the key a reader is shown.
	if d.Tasks == nil {
		r := failed(fmt.Sprintf("%s: %q is no item this company's tracker can name here — the engine's own "+
			"tracker is not served on this surface. Name an item as `<backend>:<id>`.", tool, clip(named)))
		return nil, false, false, &r
	}
	detail, err := d.Tasks.Task(ctx, named, tracker.DetailWants{}, seatRead)
	switch {
	case errors.Is(err, tracker.ErrNoTask):
		r := failed(fmt.Sprintf("%s: `work_item` %q names no work item. Check its key with list_work_items "+
			"rather than guessing — a change carrying an item that does not exist names nothing it implements.",
			tool, clip(named)))
		return nil, false, false, &r
	case err != nil:
		r := readFailure(ctx, tool, err)
		return nil, false, false, &r
	}
	t := detail.Task
	return fittedItem(types.WorkItem{Backend: types.WorkNative, ID: t.ID, Key: t.Key, Project: t.Project}), true,
		false, nil
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
// (code.FitProvenance).
func fittedItem(w types.WorkItem) *types.WorkItem {
	return &types.WorkItem{Backend: types.WorkBackend(code.FitProvenance(string(w.Backend))),
		ID: code.FitProvenance(w.ID), Key: code.FitProvenance(w.Key), Project: code.FitProvenance(w.Project)}
}

// ---- code_open_change ------------------------------------------------------- //

type codeOpenChange struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeOpenChange)(nil)

func (t *codeOpenChange) Name() string { return CodeOpenChangeTool }

func (t *codeOpenChange) Description() string {
	return "Open a change: a branch proposed into another — the default branch unless `target` names one — " +
		"carrying the turn's work item unless `work_item` names another, and asking `reviewers` once it is open."
}

// workItemParam is the `work_item` argument as the change writes describe it:
// a native item's key or id by the tracker's own rule — its id where the row
// a model read it from carries `key_collision`, whose key opens the task that
// claimed it first (workaddress.go's itemRef) — any tracker's identity, or
// none.
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
	// operation — the reviewers among them, since they are asked only once
	// the change is open, and a call whose second write could never be made
	// opens nothing.
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
	open := code.ChangeOpen{SourceRef: source, TargetRef: target, Title: argString(args, "title"),
		Body: argString(args, "body"), Draft: argBool(args, "draft")}
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
	// THE BRANCHES' HOLDS, before the open is sent ([CodeDeps.branch]): the
	// change proposes what its source names onto what its target names, so
	// each — where it is a seat's branch — is held as a read is, and a source
	// the resolve finds missing is held with no read, as a write's is.
	for _, ref := range []string{open.SourceRef, open.TargetRef} {
		if refusal := t.deps.branch(ctx, turn, tool, c, repo, ref, true); refusal != nil {
			return *refusal, nil
		}
	}
	op := opIDFor(actor, tool, changeVerb, repo.ID, args)
	open.RepoID, open.ChangeID = repo.ID, code.OpenChangeID(op)
	if turn != nil && c.asker.Kind == code.PrincipalAgent {
		open.TurnID = turn.RunID
	}
	p := code.ChangeProposal{OpID: op, Asker: c.asker, Seen: t.deps.openSeen(c, repo), Open: &open}
	res, refusal := t.deps.proposeChange(ctx, turn, tool, c, repo, p)
	if refusal != nil {
		return *refusal, nil
	}
	out := codeChangeWriteAnswer{Repo: repo.Name, RenamedTo: repo.Redirect, RedirectUntil: repo.RedirectUntil,
		Outcome: res.Outcome, Version: res.Version, Position: res.Position, OpID: c.actor.Operation}
	if res.Number > 0 {
		// A PENDING OPEN NAMES NO NUMBER: the apply that opens the change
		// numbers it, and none has run where the answer was formed.
		out.Change = code.ChangeRef(repo.Name, res.Number)
	}
	if len(reviewers) > 0 {
		t.deps.askOpened(ctx, turn, c, repo, op, res, reviewers, &out)
	}
	return newForeignRead(turn, c.asker.Seat).page(ctx, tool, out, "")
}

// reviewersArg is a call's list of reviewers to ask — handles, each a seat's
// or the human seat a person holds — or the refusal of one no ask could name:
// a handle no seat may have, past code.MaxReviewersPerChange, or the caller
// themselves, the change's author, whose review of their own change counts
// for nothing. Refused before anything is written, so a call whose ask could
// never be made writes nothing.
func reviewersArg(tool, arg string, args map[string]any, asker code.Principal) ([]string, *tools.Result) {
	handles := argStrings(args, arg)
	if len(handles) > code.MaxReviewersPerChange {
		r := failed(fmt.Sprintf("%s: `%s` names %d reviewers, past the %d a change asks at once.", tool, arg,
			len(handles), code.MaxReviewersPerChange))
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

// askOpened makes an open's second write — the ask of the reviewers it names —
// once the change is open (applied or pending: a pending open's change is on
// the log, and the writer decides the ask at or past where it landed), as a
// STEP of the open's operation, decided at or past the open's own record. An
// open that landed nothing asks nobody. Whatever became of the ask is said on
// the answer beside the change it was made of, never as the open's failure:
// the change WAS opened, and a call made again to finish the ask opens nothing
// twice.
func (d CodeDeps) askOpened(ctx context.Context, turn *turnctx.Turn, c codeCaller, repo code.RepoRef,
	openOp string, opened code.ChangeResult, reviewers []string, out *codeChangeWriteAnswer) {

	if opened.Outcome != statelog.OutcomeApplied && opened.Outcome != statelog.OutcomePending {
		return
	}
	ask := code.ReviewerRequest{RepoID: repo.ID, ChangeID: opened.ChangeID}
	for _, h := range reviewers {
		ask.Request = append(ask.Request, code.Reviewer{Handle: h})
	}
	w := code.ConversationWrite{OpID: statelog.StepOpID(openOp, stepTwo), Asker: c.asker, Seen: opened.Position,
		Reviewers: &ask}
	named := out.Change
	if named == "" {
		named = "a change of " + repo.Name + " its apply has yet to number"
	}
	res, err := d.converse(ctx, w)
	switch {
	case err != nil:
		out.ReviewersNotAsked = notAsked(named, writeErrWords(err), sameCall(c.actor, CodeOpenChangeTool))
	case res.Refusal != "":
		// A REFUSAL IS REFUSED AGAIN by the same call, which opens nothing
		// twice and asks the same reviewers: asking others is a call of its
		// own, of the change the open made.
		out.ReviewersNotAsked = notAsked(named, fmt.Sprintf("%s (%s)", res.Detail, res.Refusal), "") +
			fmt.Sprintf(" Ask others with %s.", CodeRequestReviewTool)
	case res.Outcome == statelog.OutcomeApplied || res.Outcome == statelog.OutcomePending:
		out.Reviewers = reviewers
	default:
		out.ReviewersNotAsked = notAsked(named, "whether the ask landed is unknown",
			sameCall(c.actor, CodeOpenChangeTool))
	}
}

// notAsked is the sentence of an open whose ask of its reviewers did not
// finish: the change WAS opened, so opening it again is never the way — the
// same call is the same operation, and finishes the ask where waiting clears
// what stopped it (again, "" where this caller has no such repeat).
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

// update is the one write a call's arguments ask for — an edit of what the
// change says, a draft's state or a retarget — with every field but the
// change's own ids, which the call fills once it has read the change, or the
// refusal of a call that asks for none of them or for more than one.
func (t *codeUpdateChange) update(ctx context.Context, args map[string]any) (code.ChangeProposal, *tools.Result) {
	tool := t.Name()
	var edit code.ChangeEdit
	if v, ok := args["title"].(string); ok {
		edit.Title = &v
	}
	if v, ok := args["body"].(string); ok {
		edit.Body = &v
	}
	item, given, clear, refusal := t.deps.workItemArg(ctx, tool, args)
	if refusal != nil {
		return code.ChangeProposal{}, refusal
	}
	edit.WorkItem, edit.ClearWorkItem = item, clear
	edits := edit.Title != nil || edit.Body != nil || given
	_, draft := args["draft"].(bool)
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
		return code.ChangeProposal{}, &r
	}
	switch {
	case draft:
		return code.ChangeProposal{Draft: &code.ChangeDraft{Draft: argBool(args, "draft")}}, nil
	case retarget:
		target, refusal := branchArg(tool, "target", args)
		if refusal != nil {
			return code.ChangeProposal{}, refusal
		}
		return code.ChangeProposal{Retarget: &code.ChangeRetarget{TargetRef: target}}, nil
	}
	return code.ChangeProposal{Edit: &edit}, nil
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
	p := code.ChangeProposal{Close: &code.ChangeClose{Reason: strings.TrimSpace(argString(args, "reason"))}}
	if argBool(args, "reopen") {
		if p.Close.Reason != "" {
			return failed(fmt.Sprintf("%s: a reopen takes no `reason` — it is the close that says why.",
				tool)), nil
		}
		p = code.ChangeProposal{Reopen: &code.ChangeReopen{}}
	}
	return t.deps.steer(ctx, turn, tool, c, args, name, number, version, p)
}

// ---- a write of an existing change ------------------------------------------- //

// steer makes a write of an existing change — p, an edit, a draft's state, a
// retarget, a close or a reopen, with every field but its ids — named by its
// number on repository name, held to the version its caller read: the
// repository resolved and the change read, ONE read at the seat's level for
// its id, then the write decided at or past that read, and its answer.
func (d CodeDeps) steer(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	args map[string]any, name string, number, version int64, p code.ChangeProposal) (tools.Result, error) {

	actor, bound := d.bindOperation(c.actor, tool, args)
	if bound != "" {
		return failed(bound), nil
	}
	c.actor = actor
	repo, refusal := d.repo(ctx, turn, tool, c, name)
	if refusal != nil {
		return *refusal, nil
	}
	change, refusal := d.changeOf(ctx, turn, tool, c, repo, number)
	if refusal != nil {
		return *refusal, nil
	}
	// A RETARGET OR A REOPEN DEPENDS ON A BRANCH — the target it moves the
	// change onto, the source whose head it reopens at — so where that is a
	// seat's branch it is held before the proposal is sent, as a read is
	// ([CodeDeps.branch]).
	held := ""
	switch {
	case p.Retarget != nil:
		held = p.Retarget.TargetRef
	case p.Reopen != nil:
		held = change.detail.SourceRef
	}
	if held != "" {
		if refusal := d.branch(ctx, turn, tool, c, repo, held, true); refusal != nil {
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
	p.OpID, p.Asker, p.Seen, p.ExpectedVersion = opIDFor(actor, tool, changeVerb, repo.ID, args), c.asker,
		change.at, version
	res, refusal := d.proposeChange(ctx, turn, tool, c, repo, p)
	if refusal != nil {
		return *refusal, nil
	}
	out := codeChangeWriteAnswer{Repo: repo.Name, RenamedTo: repo.Redirect, RedirectUntil: repo.RedirectUntil,
		Change: code.ChangeRef(repo.Name, number), Outcome: res.Outcome, Unchanged: res.Unchanged,
		Version: res.Version, Position: res.Position, OpID: c.actor.Operation}
	return newForeignRead(turn, c.asker.Seat).page(ctx, tool, out, "")
}

// codeChangeWriteAnswer is a change write's answer: the change, what became of
// the write, and the version the caller's next write of it names — 0 where a
// later record moved the change since, which is read again before it is
// written.
type codeChangeWriteAnswer struct {
	Repo          string            `json:"repo"`
	RenamedTo     string            `json:"renamed_to,omitempty"`
	RedirectUntil time.Time         `json:"redirect_until,omitzero"`
	Change        string            `json:"change,omitempty"`
	Outcome       statelog.Outcome  `json:"outcome"`
	Unchanged     bool              `json:"unchanged,omitempty"`
	Version       int64             `json:"version,omitempty"`
	Position      statelog.Position `json:"position,omitzero"`
	// Reviewers are the reviewers an open asked once the change was open,
	// and ReviewersNotAsked why it asked none: the change opened all the
	// same.
	Reviewers         []string `json:"reviewers,omitempty"`
	ReviewersNotAsked string   `json:"reviewers_not_asked,omitempty"`
	OpID              string   `json:"op_id,omitempty"`
}

// proposeChange asks the repository's committer to decide p — held first to
// the rule every committer holds a proposal to (code.ChangeProposal.Validate),
// so one no node would decide is refused naming its field before it is routed
// — and answers what it came to, or the refusal the call is answered with: a
// refusal of the write in its class, an outcome unknown, or a failure to
// reach any committer.
func (d CodeDeps) proposeChange(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	repo code.RepoRef, p code.ChangeProposal) (code.ChangeResult, *tools.Result) {

	if err := p.Validate(); err != nil {
		r := codeWriteFailure(ctx, tool, c.actor, repo.Name, err)
		return code.ChangeResult{}, &r
	}
	res, err := d.Writer.Change(ctx, p)
	if err != nil {
		r := d.failedWrite(ctx, turn, tool, c, repo, p.OpID, err)
		return code.ChangeResult{}, &r
	}
	if res.Refusal != "" {
		r := changeRefused(tool, res)
		return code.ChangeResult{}, &r
	}
	switch res.Outcome {
	case statelog.OutcomeApplied, statelog.OutcomePending:
		return res, nil
	case statelog.OutcomeUnknown:
		r := unknownWrite(c.actor, tool, fmt.Sprintf("the write of a change on %s landed", repo.Name), p.OpID, false,
			unknownNext(false, sameCall(c.actor, tool), fmt.Sprintf("Read the change with %s", CodeGetChangeTool),
				"the same write would be asked twice"))
		return code.ChangeResult{}, &r
	}
	r := faulted(ctx, tool, fmt.Errorf("builtin: the code host answered a change write with outcome %q and no "+
		"refusal", res.Outcome), fmt.Sprintf("%s could not tell what became of the write: %s.", tool, faultSaid))
	return code.ChangeResult{}, &r
}

// converse asks the repository's committing data node to decide w — held first
// to the rule every change writer holds a write to (code.ConversationWrite's
// Validate) — and answers what it came to.
func (d CodeDeps) converse(ctx context.Context, w code.ConversationWrite) (code.ConversationResult, error) {
	if err := w.Validate(); err != nil {
		return code.ConversationResult{}, err
	}
	return d.Writer.Converse(ctx, w)
}

// changeClass is the class a refusal of a change's lifecycle is answered in —
// chosen by what each class tells its reader, as a push's are ([pushClass]):
// a rule of the repository or the change refusing the caller FORBIDDEN; a
// change that moved since the caller read it STALE_VERSION; a branch already
// heading a live change EXISTS; a write naming branches no change can be
// about INVALID — the fix is another call; a write only a live change takes,
// of one that is not, CONFLICT; a repository or a change gone NOT_FOUND; and
// what waiting clears UNAVAILABLE, the same call again being the same write.
// The two only a push's receiver answers (ambiguous_source, bad_option) are
// INVALID on the same reading, though no tool's write is ever refused either.
func changeClass(r code.ChangeRefusal) (tools.Refusal, bool) {
	switch r {
	case code.ChangeRefusalNotPermitted, code.ChangeRefusalRepoArchived:
		return tools.RefusalForbidden, true
	case code.ChangeRefusalStale:
		return tools.RefusalStaleVersion, true
	case code.ChangeRefusalChangeExists:
		return tools.RefusalExists, true
	case code.ChangeRefusalNoSource, code.ChangeRefusalNoTarget, code.ChangeRefusalSameRef,
		code.ChangeRefusalNoDifference, code.ChangeRefusalAmbiguousSource, code.ChangeRefusalBadOption:
		return tools.RefusalInvalid, true
	case code.ChangeRefusalChangeClosed, code.ChangeRefusalChangeMerged:
		return tools.RefusalConflict, true
	case code.ChangeRefusalRepoDeleted, code.ChangeRefusalChangeNotFound:
		return tools.RefusalNotFound, true
	case code.ChangeRefusalBusy, code.ChangeRefusalNotApplied, code.ChangeRefusalUnavailable:
		return tools.RefusalUnavailable, true
	}
	return "", false
}

// changeRefused is a change write the committer refused, in its class, with
// the decide's own sentence and what to do about it.
func changeRefused(tool string, res code.ChangeResult) tools.Result {
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
	default:
		said += " The same call is refused again; change what it asks for."
	}
	return refusedBy(class, cause, said)
}
