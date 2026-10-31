package builtin

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE CONVERSATION WRITES: what is said about a change — an ask for review,
// a review with its findings, a comment and a thread's resolution, and an
// answer to findings — each ONE write of the change's conversation, decided by
// the repository's committing data node in one snapshot
// (code.ConversationWrite, CodeWriter.Converse) on the change's own subject:
// its writer stamps who wrote it, a reviewer's kind, a comment's and a
// review's ids, from the write's asker, the chart and its snapshot. Every one
// is a change write ([ChangeWrites]) on the change writes' terms
// (codechangewrite.go): admitted for every principal and judged where it is
// decided — who may ask, review, answer or dismiss (the change domain's rule
// D21) — named by the same operation rule, a token bound to nobody writing
// nothing.
//
// # Each is decided at or past its caller's read of the change
//
// The call reads the change ONCE, at the seat's level, for the id a write
// names it by — and a review or an anchored comment for the revision it
// judges, the head unless the call names an earlier one — and the write is
// decided at or past that read (code.ConversationWrite.Seen): a revision the
// deciding node has not reached is refused unknown_revision rather than taken
// for one that does not exist. An ask for review and a comment are additive —
// the operation makes the same call made again the same write — so neither
// names a version; nor does a review, which judges the revision it names
// whatever has moved since.
//
// # A finding is anchored where its lines were read
//
// A finding, and a comment anchored to a line, names the commit and the blob
// its lines were read in — the revision's head on the new side of its diff,
// its recorded base on the old — so a later revision can carry it to where
// those lines went (code.Reanchor). The call reads every blob it names in ONE
// read by object id, the revision's diff kept to the files they are on, at the
// floor of the records that moved the revision's head and its target: a
// finding is about a file the revision CHANGES, and one about any other is
// refused, to be said in the review's summary or a comment. A revision whose
// base its home has not recorded yet is answered not yet readable, as
// code_get_change's files are.

// The conversation write tools' names: each is its authority row's action.
const (
	CodeRequestReviewTool   = string(authz.ActionCodeRequestReview)
	CodeReviewChangeTool    = string(authz.ActionCodeReviewChange)
	CodeCommentOnChangeTool = string(authz.ActionCodeCommentOnChange)
	CodeResolveFindingTool  = string(authz.ActionCodeResolveFinding)
)

// emptyTreeID is git's id of the tree with nothing in it, which git knows
// whether or not a repository stores it: the base a revision sharing no
// history with its target is read from, its change every file of its head.
const emptyTreeID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// ---- one write of a change's conversation ----------------------------------- //

// conversing is one call of a conversation write: who writes, the repository
// the turn resolved and the change by its number on it ([changeRead]), the
// call's arguments, and the call's one read of the change — its detail, its
// head revision and the position it was read at.
type conversing struct {
	g      changeRead
	args   map[string]any
	change *code.ChangeDetail
	head   code.RevisionRow
	at     statelog.Position
}

// conversation binds a call's operation by the rule every write takes,
// resolves the repository it names and reads its change, for a write of the
// change's conversation to name — or answers the refusal the call is answered
// with.
func (d CodeDeps) conversation(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	args map[string]any, name string, number int64) (conversing, *tools.Result) {

	actor, bound := d.bindOperation(c.actor, tool, args)
	if bound != "" {
		r := failed(bound)
		return conversing{}, &r
	}
	c.actor = actor
	repo, refusal := d.repo(ctx, turn, tool, c, name)
	if refusal != nil {
		return conversing{}, refusal
	}
	g := changeRead{deps: d, turn: turn, tool: tool, c: c, repo: repo, number: number}
	detail, head, at, refusal := g.head(ctx, statelog.Position{})
	if refusal != nil {
		return conversing{}, refusal
	}
	return conversing{g: g, args: args, change: detail, head: head, at: at}, nil
}

// op is the call's operation: the one every write it makes is named by.
func (cv conversing) op() string {
	return opIDFor(cv.g.c.actor, cv.g.tool, changeVerb, cv.g.repo.ID, cv.args)
}

// send asks the repository's committing data node to decide w — under the
// call's operation, as its caller, at or past the call's read of the change —
// and answers what it came to, or the refusal the call is answered with.
func (cv conversing) send(ctx context.Context, w code.ConversationWrite) (code.ConversationResult,
	*tools.Result) {

	w.OpID, w.Seen = cv.op(), cv.at
	return cv.decided(ctx, w)
}

// decided completes w with its asker and asks it, held first to the rule every
// change writer holds a write to (code.ConversationWrite's Validate), so one no
// node would decide is refused naming its field before it is routed.
func (cv conversing) decided(ctx context.Context, w code.ConversationWrite) (code.ConversationResult,
	*tools.Result) {

	g := cv.g
	w.Asker, w.Ceiling = g.c.asker, g.c.write
	if err := w.Validate(); err != nil {
		r := codeWriteFailure(ctx, g.tool, g.c.actor, g.repo.Name, err)
		return code.ConversationResult{}, &r
	}
	res, err := g.deps.Writer.Converse(ctx, w)
	if err != nil {
		r := g.deps.failedWrite(ctx, g.turn, g.tool, g.c, g.repo, w.OpID, err)
		return code.ConversationResult{}, &r
	}
	if res.Refusal != "" {
		r := conversationRefused(g.tool, res)
		return code.ConversationResult{}, &r
	}
	switch res.Outcome {
	case statelog.OutcomeApplied, statelog.OutcomePending:
		return res, nil
	case statelog.OutcomeUnknown:
		r := unknownWrite(g.c.actor, g.tool, fmt.Sprintf("the write of %s's conversation landed", g.ref()), w.OpID,
			false, unknownNext(false, sameCall(g.c.actor, g.tool), fmt.Sprintf("Read %s with %s", g.ref(),
				CodeGetChangeTool), "the same write would be made twice"))
		return code.ConversationResult{}, &r
	}
	r := faulted(ctx, g.tool, fmt.Errorf("builtin: the code host answered a conversation write with outcome %q "+
		"and no refusal", res.Outcome), fmt.Sprintf("%s could not tell what became of the write: %s.", g.tool,
		faultSaid))
	return code.ConversationResult{}, &r
}

// codeConversationAnswer is a conversation write's answer: the change, what
// became of the write, what it made — a comment's or a review's id, a review's
// findings' ids in the order it raised them — and, for a comment that also
// set its thread's state, what became of that.
type codeConversationAnswer struct {
	Repo          string           `json:"repo"`
	RenamedTo     string           `json:"renamed_to,omitempty"`
	RedirectUntil time.Time        `json:"redirect_until,omitzero"`
	Change        string           `json:"change"`
	Outcome       statelog.Outcome `json:"outcome"`
	Unchanged     bool             `json:"unchanged,omitempty"`
	ID            string           `json:"id,omitempty"`
	Findings      []string         `json:"findings,omitempty"`
	Position      string           `json:"position,omitempty"`
	// ThreadResolved is the state a comment's call left its thread in, and
	// ThreadNotSet why it left the thread as it was: the comment stands all
	// the same.
	ThreadResolved *bool  `json:"thread_resolved,omitempty"`
	ThreadNotSet   string `json:"thread_not_set,omitempty"`
	OpID           string `json:"op_id,omitempty"`
}

// answerOf is res as the call answers it.
func (cv conversing) answerOf(res code.ConversationResult) codeConversationAnswer {
	g := cv.g
	return codeConversationAnswer{Repo: g.repo.Name, RenamedTo: g.repo.Redirect, RedirectUntil: g.repo.RedirectUntil,
		Change: g.ref(), Outcome: res.Outcome, Unchanged: res.Unchanged, ID: res.ID, Findings: res.FindingIDs,
		Position: positionText(res.Position), OpID: g.c.actor.Operation}
}

// page renders an answer, every string defused as every code answer's is.
func (cv conversing) page(ctx context.Context, out codeConversationAnswer) (tools.Result, error) {
	return newForeignRead(cv.g.turn, cv.g.c.asker.Seat).page(ctx, cv.g.tool, out, "")
}

// conversationClass is the class a refusal of a change's conversation is
// answered in, on [changeClass]'s reading: a rule of the change refusing the
// caller — who may ask, review, answer or dismiss — FORBIDDEN; a revision the
// deciding node has not reached STALE_VERSION; a write only a live change
// takes, of one that is not, CONFLICT; a repository, a change, a thread or a
// finding gone NOT_FOUND; a reviewer no seat holds and an ask or a review past
// its bound INVALID — the fix is another call; and what waiting clears
// UNAVAILABLE. superseded is the home's own refusal, never a caller's, and
// has none.
func conversationClass(r code.ConversationRefusal) (tools.Refusal, bool) {
	switch r {
	case code.ConversationRefusalNotPermitted, code.ConversationRefusalNotAuthor,
		code.ConversationRefusalNotRaiser, code.ConversationRefusalRepoArchived:
		return tools.RefusalForbidden, true
	case code.ConversationRefusalUnknownRevision:
		return tools.RefusalStaleVersion, true
	case code.ConversationRefusalChangeClosed, code.ConversationRefusalChangeMerged:
		return tools.RefusalConflict, true
	case code.ConversationRefusalRepoDeleted, code.ConversationRefusalChangeNotFound,
		code.ConversationRefusalThreadGone, code.ConversationRefusalFindingGone:
		return tools.RefusalNotFound, true
	case code.ConversationRefusalNoReviewer, code.ConversationRefusalTooManyReviewers,
		code.ConversationRefusalTooManyFindings:
		return tools.RefusalInvalid, true
	case code.ConversationRefusalBusy, code.ConversationRefusalNotApplied, code.ConversationRefusalUnavailable:
		return tools.RefusalUnavailable, true
	}
	return "", false
}

// conversationRefused is a conversation write the change's writer refused, in
// its class, with the decide's own sentence and what to do about it.
func conversationRefused(tool string, res code.ConversationResult) tools.Result {
	cause := writeRefusalError(string(res.Refusal), res.Detail)
	said := fmt.Sprintf("%s wrote nothing: %s (%s).", tool, res.Detail, res.Refusal)
	class, known := conversationClass(res.Refusal)
	if !known {
		return refusedBy(tools.RefusalInternalError, cause, said+" "+faultSaid+".")
	}
	switch class {
	case tools.RefusalStaleVersion:
		said += fmt.Sprintf(" Read the change again with %s: the node that decided has not reached what you read.",
			CodeGetChangeTool)
	case tools.RefusalUnavailable:
		said += " Nothing was written; make the same call again — it is the same write."
	case tools.RefusalForbidden:
		said += " That is a rule of the change, so the same call is refused again."
	default:
		said += " The same call is refused again; change what it asks for."
	}
	return refusedBy(class, cause, said)
}

// ---- anchoring lines --------------------------------------------------------- //

// anchorAt is one place a call anchors lines: a file of the revision, on one
// side of its diff.
type anchorAt struct {
	path string
	side code.Side
}

// anchored is where a revision's lines were read: the commit each side's
// lines are in, and the blob each anchored file is on its side.
type anchored struct {
	commit map[code.Side]string
	blob   map[anchorAt]string
}

// anchor reads where lines of revision rev are, for each place a call anchors
// them: its head on the new side, its recorded base on the old — refused, as
// code_get_change refuses its files, where the base is not recorded yet, and
// for the old side of a revision sharing no history with its target, whose
// base holds nothing — and the blob each file is on its side, from ONE read of
// the revision's diff by object id kept to those files, at the floor of the
// records that moved its head and its target. A file the revision does not
// change, or one absent on the side named, is refused: a finding and an
// anchored comment are about the change's own lines.
func (cv conversing) anchor(ctx context.Context, rev code.RevisionRow, at []anchorAt) (anchored, *tools.Result) {
	g := cv.g
	old := slices.ContainsFunc(at, func(a anchorAt) bool { return a.side == code.SideOld })
	if refusal := g.recorded(cv.change, rev, old); refusal != nil {
		return anchored{}, refusal
	}
	base := rev.Facts.BaseOID
	out := anchored{commit: map[code.Side]string{code.SideNew: rev.Head, code.SideOld: base},
		blob: map[anchorAt]string{}}
	if base == "" {
		base = emptyTreeID
	}
	var paths []string
	for _, a := range at {
		if !slices.Contains(paths, a.path) {
			paths = append(paths, a.path)
		}
	}
	slices.Sort(paths)
	files := map[string]code.DiffFile{}
	q := code.GitQuery{Kind: code.GitDiff, RepoID: g.repo.ID, Fresh: g.floor(rev),
		Diff: &code.DiffQuery{Base: base, Head: rev.Head, Paths: paths}}
	// THE DIFF'S PAGES, each its files in git's order: a page of at most
	// code.MaxDiffPaths files is one page but where their paths fill one, so
	// a page names where the next begins and the read goes on.
	for {
		page, refusal := g.deps.gitRead(ctx, g.turn, g.tool, g.c, g.repo, q)
		if refusal != nil {
			return anchored{}, refusal
		}
		if page.Diff == nil {
			r := faulted(ctx, g.tool, fmt.Errorf("builtin: the code host answered %s's diff with no diff", g.ref()),
				fmt.Sprintf("%s could not read revision %d of %s: %s.", g.tool, rev.N, g.ref(), faultSaid))
			return anchored{}, &r
		}
		for _, f := range page.Diff.Files {
			files[f.Path] = f
		}
		if page.Next == "" || len(page.Diff.Files) == 0 {
			break
		}
		q.Cursor = page.Next
	}
	for _, a := range at {
		f, changed := files[a.path]
		blob := f.NewOID
		if a.side == code.SideOld {
			blob = f.OldOID
		}
		switch {
		case !changed:
			r := failed(fmt.Sprintf("%s: revision %d of %s does not change %s: a finding and an anchored comment "+
				"are about the change's own lines — say what concerns another file in the review's summary or a "+
				"comment of its own.", g.tool, rev.N, g.ref(), a.path))
			return anchored{}, &r
		case blob == "" && a.side == code.SideNew:
			r := failed(fmt.Sprintf("%s: %s is deleted at revision %d of %s, so its lines are on side %s.",
				g.tool, a.path, rev.N, g.ref(), code.SideOld))
			return anchored{}, &r
		case blob == "":
			r := failed(fmt.Sprintf("%s: %s is new in revision %d of %s, so its lines are on side %s.", g.tool,
				a.path, rev.N, g.ref(), code.SideNew))
			return anchored{}, &r
		}
		out.blob[a] = blob
	}
	return out, nil
}

// revisionOf is the revision a call judges: the change's head, or the earlier
// one `revision` names, read at session floored at the call's read of the
// change.
func (cv conversing) revisionOf(ctx context.Context, n int64) (code.RevisionRow, *tools.Result) {
	if n == 0 || n == cv.head.N {
		return cv.head, nil
	}
	return cv.g.revision(ctx, n, cv.change.Revision, cv.at)
}

// sideArg is a side a call names, new where it names none.
func sideArg(tool, field string, raw string) (code.Side, *tools.Result) {
	side := code.Side(strings.TrimSpace(raw))
	if side == "" {
		return code.SideNew, nil
	}
	if !side.Valid() {
		r := failed(fmt.Sprintf("%s: %s is %v — new (the revision's head) or old (its base) — not %q.", tool, field,
			code.Sides(), clip(string(side))))
		return "", &r
	}
	return side, nil
}

// revisionArg is a call's `revision`, 0 for the change's head.
func revisionArg(tool string, args map[string]any) (int64, *tools.Result) {
	n := int64(argInt(args, "revision", 0))
	if n < 0 {
		r := failed(fmt.Sprintf("%s: `revision` counts from 1; leave it out for the change's latest.", tool))
		return 0, &r
	}
	return n, nil
}

// handlesArg is a list of handles a call names — refused where one is no
// seat's handle or the list is past code.MaxReviewersPerChange.
func handlesArg(tool, arg string, args map[string]any) ([]string, *tools.Result) {
	handles := argStrings(args, arg)
	if len(handles) > code.MaxReviewersPerChange {
		r := failed(fmt.Sprintf("%s: `%s` names %d, past the %d a change asks at once.", tool, arg, len(handles),
			code.MaxReviewersPerChange))
		return nil, &r
	}
	for _, h := range handles {
		if !org.ValidHandle(h) {
			r := failed(fmt.Sprintf("%s: `%s` names %q, which is no seat's handle.", tool, arg, clip(h)))
			return nil, &r
		}
	}
	return handles, nil
}

// ---- code_request_review ------------------------------------------------------ //

type codeRequestReview struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeRequestReview)(nil)

func (t *codeRequestReview) Name() string { return CodeRequestReviewTool }

func (t *codeRequestReview) Description() string {
	return "Ask `reviewers` to review a change, or `withdraw` an ask. A seat asked must review the repository and " +
		"not have written the change; a person may always be asked."
}

func (t *codeRequestReview) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":      changeRepoParam(),
			"change":    changeParam(),
			"reviewers": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"withdraw":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"note":      map[string]any{"type": "string"},
		},
		"required": []string{"change"},
	})
}

func (t *codeRequestReview) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeRequestReview) CallForTurn(ctx context.Context, turn *turnctx.Turn,
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
	reviewers, refusal := reviewersArg(tool, "reviewers", args, c.asker)
	if refusal != nil {
		return *refusal, nil
	}
	withdraw, refusal := handlesArg(tool, "withdraw", args)
	if refusal != nil {
		return *refusal, nil
	}
	if len(reviewers)+len(withdraw) == 0 {
		return failed(fmt.Sprintf("%s needs `reviewers` to ask, or `withdraw` to take an ask back.", tool)), nil
	}
	cv, refusal := t.deps.conversation(ctx, turn, tool, c, args, name, number)
	if refusal != nil {
		return *refusal, nil
	}
	ask := code.ReviewerRequest{RepoID: cv.g.repo.ID, ChangeID: cv.change.ID, Withdraw: withdraw,
		Note: argString(args, "note")}
	for _, h := range reviewers {
		ask.Request = append(ask.Request, code.Reviewer{Handle: h})
	}
	res, refusal := cv.send(ctx, code.ConversationWrite{Reviewers: &ask})
	if refusal != nil {
		return *refusal, nil
	}
	return cv.page(ctx, cv.answerOf(res))
}

// ---- code_review_change ------------------------------------------------------- //

type codeReviewChange struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeReviewChange)(nil)

func (t *codeReviewChange) Name() string { return CodeReviewChangeTool }

func (t *codeReviewChange) Description() string {
	return "Review a change's revision — its latest unless `revision` names one — in ONE record: a `verdict`, a " +
		"`summary`, `findings` on lines the revision changes, and `carries`, your own standing findings that still " +
		"stand; reviewing a later revision supersedes every other one of yours. A blocker holds the change until " +
		"dismissed or not carried."
}

func (t *codeReviewChange) Parameters() map[string]any {
	enum := func(values []string) map[string]any { return map[string]any{"type": "string", "enum": values} }
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":     changeRepoParam(),
			"change":   changeParam(),
			"revision": map[string]any{"type": "integer"},
			"verdict":  enum(spelled(code.Verdicts())),
			"summary":  map[string]any{"type": "string"},
			"findings": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":       map[string]any{"type": "string"},
						"side":       sideParam(),
						"line_start": map[string]any{"type": "integer"},
						"line_end":   map[string]any{"type": "integer"},
						"severity":   enum(spelled(code.Severities())),
						"category":   enum(spelled(code.Categories())),
						"title":      map[string]any{"type": "string"},
						"body":       map[string]any{"type": "string"},
						"suggestion": map[string]any{"type": "string",
							"description": "The lines to put in place of line_start..line_end."},
					},
					"required": []string{"path", "line_start", "severity", "category", "title"},
				},
			},
			"carries": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []string{"change", "verdict"},
	})
}

// sideParam is a side argument as the conversation writes describe it.
func sideParam() map[string]any {
	return map[string]any{"type": "string", "enum": spelled(code.Sides()),
		"description": "new (the revision's head, the default) or old (its base)."}
}

func (t *codeReviewChange) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeReviewChange) CallForTurn(ctx context.Context, turn *turnctx.Turn,
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
	n, refusal := revisionArg(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	review, places, refusal := reviewArgs(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	cv, refusal := t.deps.conversation(ctx, turn, tool, c, args, name, number)
	if refusal != nil {
		return *refusal, nil
	}
	rev, refusal := cv.revisionOf(ctx, n)
	if refusal != nil {
		return *refusal, nil
	}
	if len(review.Findings) > 0 {
		lines, refusal := cv.anchor(ctx, rev, places)
		if refusal != nil {
			return *refusal, nil
		}
		for i := range review.Findings {
			f := &review.Findings[i]
			f.CommitOID, f.BlobOID = lines.commit[f.Side], lines.blob[anchorAt{path: f.Path, side: f.Side}]
		}
	}
	review.RepoID, review.ChangeID, review.Revision = cv.g.repo.ID, cv.change.ID, rev.N
	if turn != nil && c.asker.Kind == code.PrincipalAgent {
		review.TurnID, review.Model = turn.RunID, code.FitProvenance(toolloop.CallModel(ctx))
	}
	res, refusal := cv.send(ctx, code.ConversationWrite{Review: &review})
	if refusal != nil {
		return *refusal, nil
	}
	return cv.page(ctx, cv.answerOf(res))
}

// reviewArgs is the review a call's arguments ask for — its verdict, summary,
// carried findings and findings, each finding's commit and blob left for the
// call's one read of the revision to fill — and the places its findings
// anchor lines, or the refusal of one no review could carry, before anything
// is read. What each field may hold beyond its shape — a title's bytes, a
// body's — is the change writer's rule, which the write is held to before it
// is routed.
func reviewArgs(tool string, args map[string]any) (code.Review, []anchorAt, *tools.Result) {
	review := code.Review{Verdict: code.Verdict(strings.TrimSpace(argString(args, "verdict"))),
		Summary: argString(args, "summary"), Carries: argStrings(args, "carries")}
	if !review.Verdict.Valid() {
		r := failed(fmt.Sprintf("%s needs `verdict`: one of %v.", tool, code.Verdicts()))
		return code.Review{}, nil, &r
	}
	raw, _ := args["findings"].([]any)
	if len(raw) > code.MaxFindingsPerReview {
		r := failed(fmt.Sprintf("%s: %d findings, past the %d one review raises: split them into two reviews of "+
			"the same revision, which supersede nothing of each other.", tool, len(raw), code.MaxFindingsPerReview))
		return code.Review{}, nil, &r
	}
	var places []anchorAt
	for i, entry := range raw {
		m, ok := entry.(map[string]any)
		if !ok {
			r := failed(fmt.Sprintf("%s: findings[%d] is not an object of `path`, `line_start`, `severity`, "+
				"`category` and `title`.", tool, i))
			return code.Review{}, nil, &r
		}
		at := fmt.Sprintf("findings[%d]", i)
		side, refusal := sideArg(tool, at+".side", argString(m, "side"))
		if refusal != nil {
			return code.Review{}, nil, refusal
		}
		f := code.Finding{Path: strings.TrimSpace(argString(m, "path")), Side: side,
			LineStart: argInt(m, "line_start", 0), LineEnd: argInt(m, "line_end", 0),
			Severity: code.Severity(strings.TrimSpace(argString(m, "severity"))),
			Category: code.Category(strings.TrimSpace(argString(m, "category"))),
			Title:    argString(m, "title"), Body: argString(m, "body")}
		if f.LineEnd == 0 {
			f.LineEnd = f.LineStart
		}
		if s, present := m["suggestion"]; present {
			text, ok := s.(string)
			if !ok {
				r := failed(fmt.Sprintf("%s: %s.suggestion is the lines to put in place of the finding's, as text.",
					tool, at))
				return code.Review{}, nil, &r
			}
			f.Suggestion = &text
		}
		switch {
		case f.Path == "":
			r := failed(fmt.Sprintf("%s: %s names no `path`: the file its lines are in.", tool, at))
			return code.Review{}, nil, &r
		case f.LineStart < 1 || f.LineEnd < f.LineStart:
			r := failed(fmt.Sprintf("%s: %s's lines %d to %d: a finding's lines count from 1, the last at or past "+
				"the first.", tool, at, f.LineStart, f.LineEnd))
			return code.Review{}, nil, &r
		case !f.Severity.Valid():
			r := failed(fmt.Sprintf("%s: %s.severity is one of %v.", tool, at, code.Severities()))
			return code.Review{}, nil, &r
		case !f.Category.Valid():
			r := failed(fmt.Sprintf("%s: %s.category is one of %v.", tool, at, code.Categories()))
			return code.Review{}, nil, &r
		}
		review.Findings = append(review.Findings, f)
		places = append(places, anchorAt{path: f.Path, side: f.Side})
	}
	return review, places, nil
}

// ---- code_comment_on_change --------------------------------------------------- //

type codeCommentOnChange struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeCommentOnChange)(nil)

func (t *codeCommentOnChange) Name() string { return CodeCommentOnChangeTool }

func (t *codeCommentOnChange) Description() string {
	return "Comment on a change: a thread of its own — on a line, with `anchor` — or a reply to a `thread` or a " +
		"`finding`. With `thread`, `resolve` marks the thread resolved (true) or open again (false), after the " +
		"reply where there is one."
}

func (t *codeCommentOnChange) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":    changeRepoParam(),
			"change":  changeParam(),
			"body":    map[string]any{"type": "string"},
			"thread":  map[string]any{"type": "string", "description": "A thread's first comment's id."},
			"finding": map[string]any{"type": "string", "description": "A finding's id."},
			"anchor": map[string]any{"type": "object", "properties": map[string]any{
				"revision": map[string]any{"type": "integer"}, "path": map[string]any{"type": "string"},
				"side": sideParam(), "line": map[string]any{"type": "integer"}},
				"required": []string{"path", "line"}},
			"resolve": map[string]any{"type": "boolean"},
		},
		"required": []string{"change"},
	})
}

func (t *codeCommentOnChange) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// anchorArg is a comment's `anchor`, nil where the call names none.
type anchorArg struct {
	Revision int64  `json:"revision"`
	Path     string `json:"path"`
	Side     string `json:"side"`
	Line     int    `json:"line"`
}

func (t *codeCommentOnChange) CallForTurn(ctx context.Context, turn *turnctx.Turn,
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
	body := argString(args, "body")
	thread, finding := strings.TrimSpace(argString(args, "thread")), strings.TrimSpace(argString(args, "finding"))
	resolve, resolving := args["resolve"].(bool)
	var anchor *anchorArg
	if raw, ok := args["anchor"]; ok && raw != nil {
		anchor = &anchorArg{}
		if err := decodeArg(raw, anchor); err != nil {
			return failed(fmt.Sprintf("%s: `anchor` is {\"path\", \"line\", and optionally \"side\" and "+
				"\"revision\"}: %v.", tool, err)), nil
		}
	}
	saying := strings.TrimSpace(body) != ""
	switch {
	case !saying && !resolving:
		return failed(fmt.Sprintf("%s needs `body`, or `resolve` with a `thread`.", tool)), nil
	case resolving && thread == "":
		return failed(fmt.Sprintf("%s: `resolve` marks a thread, named by `thread`: a finding's is answered with "+
			"%s.", tool, CodeResolveFindingTool)), nil
	case anchor != nil && (!saying || thread != "" || finding != ""):
		return failed(fmt.Sprintf("%s: `anchor` places a thread's first comment on a line; a reply is about its "+
			"thread, and carries none.", tool)), nil
	}
	cv, refusal := t.deps.conversation(ctx, turn, tool, c, args, name, number)
	if refusal != nil {
		return *refusal, nil
	}
	if !saying {
		res, refusal := cv.send(ctx, code.ConversationWrite{ThreadResolve: &code.ThreadResolve{RepoID: cv.g.repo.ID,
			ChangeID: cv.change.ID, ThreadID: thread, Resolved: resolve}})
		if refusal != nil {
			return *refusal, nil
		}
		out := cv.answerOf(res)
		out.ThreadResolved = &resolve
		return cv.page(ctx, out)
	}
	comment := code.Comment{RepoID: cv.g.repo.ID, ChangeID: cv.change.ID, ParentID: thread, FindingID: finding,
		Body: body}
	if turn != nil && c.asker.Kind == code.PrincipalAgent {
		comment.TurnID = turn.RunID
	}
	if anchor != nil {
		if comment.Anchor, refusal = cv.anchorLine(ctx, tool, *anchor); refusal != nil {
			return *refusal, nil
		}
	}
	res, refusal := cv.send(ctx, code.ConversationWrite{Comment: &comment})
	if refusal != nil {
		return *refusal, nil
	}
	out := cv.answerOf(res)
	if resolving {
		cv.setThread(ctx, res, thread, resolve, &out)
	}
	return cv.page(ctx, out)
}

// anchorLine is a comment's anchor: the line of a file of a revision — the
// head unless it names one — on one side of its diff, with the blob it was
// read in.
func (cv conversing) anchorLine(ctx context.Context, tool string, a anchorArg) (*code.CommentAnchor,
	*tools.Result) {

	side, refusal := sideArg(tool, "anchor.side", a.Side)
	if refusal != nil {
		return nil, refusal
	}
	if a.Revision < 0 || a.Line < 1 || strings.TrimSpace(a.Path) == "" {
		r := failed(fmt.Sprintf("%s: `anchor` names a file's `path` and a `line` from 1, and a `revision` from 1 "+
			"where it names one.", tool))
		return nil, &r
	}
	rev, refusal := cv.revisionOf(ctx, a.Revision)
	if refusal != nil {
		return nil, refusal
	}
	at := anchorAt{path: strings.TrimSpace(a.Path), side: side}
	lines, refusal := cv.anchor(ctx, rev, []anchorAt{at})
	if refusal != nil {
		return nil, refusal
	}
	return &code.CommentAnchor{Revision: rev.N, Path: at.path, Side: side, Line: a.Line, BlobOID: lines.blob[at]},
		nil
}

// setThread makes a comment's second write — its thread marked resolved, or
// open again — once the comment is said (applied or pending), as a STEP of
// the comment's operation, decided at or past the comment's own record.
// Whatever became of it is said on the answer beside the comment, never as the
// comment's failure: the comment WAS said, and the call made again says it
// once.
func (cv conversing) setThread(ctx context.Context, said code.ConversationResult, thread string, resolved bool,
	out *codeConversationAnswer) {

	g := cv.g
	w := code.ConversationWrite{OpID: statelog.StepOpID(cv.op(), stepTwo), Seen: said.Position,
		ThreadResolve: &code.ThreadResolve{RepoID: g.repo.ID, ChangeID: cv.change.ID, ThreadID: thread,
			Resolved: resolved}}
	if _, refusal := cv.decided(ctx, w); refusal != nil {
		out.ThreadNotSet = fmt.Sprintf("The comment WAS said, as %s, but marking its thread did not finish: %s",
			said.ID, refusal.Output)
		return
	}
	out.ThreadResolved = &resolved
}

// ---- code_resolve_finding ----------------------------------------------------- //

type codeResolveFinding struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeResolveFinding)(nil)

func (t *codeResolveFinding) Name() string { return CodeResolveFindingTool }

func (t *codeResolveFinding) Description() string {
	return "Answer findings of a change: resolved, wont_fix or disputed (an author's answers, which leave a " +
		"blocker standing for its raiser to judge), or dismissed (its raiser's, a maintainer's or a person's, " +
		"which clears it)."
}

func (t *codeResolveFinding) Parameters() map[string]any {
	resolutions := []string{string(code.FindingResolved), string(code.FindingWontFix),
		string(code.FindingDisputed), string(code.FindingDismissed)}
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":       changeRepoParam(),
			"change":     changeParam(),
			"findings":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"resolution": map[string]any{"type": "string", "enum": resolutions},
			"note":       map[string]any{"type": "string"},
			"commit":     codeRevArg("With resolved, the commit that answers them"),
		},
		"required": []string{"change", "findings", "resolution"},
	})
}

func (t *codeResolveFinding) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeResolveFinding) CallForTurn(ctx context.Context, turn *turnctx.Turn,
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
	ids := argStrings(args, "findings")
	resolution := code.FindingState(strings.TrimSpace(argString(args, "resolution")))
	switch {
	case len(ids) == 0:
		return failed(fmt.Sprintf("%s needs `findings`: the ids of the findings it answers.", tool)), nil
	case len(ids) > code.MaxFindingsPerReview:
		return failed(fmt.Sprintf("%s: %d findings, past the %d one answer names.", tool, len(ids),
			code.MaxFindingsPerReview)), nil
	case !resolution.Resolution():
		return failed(fmt.Sprintf("%s needs `resolution`: resolved, wont_fix, disputed or dismissed.", tool)), nil
	}
	rev := strings.TrimSpace(argString(args, "commit"))
	cv, refusal := t.deps.conversation(ctx, turn, tool, c, args, name, number)
	if refusal != nil {
		return *refusal, nil
	}
	commit := ""
	if rev != "" {
		resolved, refusal := t.deps.resolveRevs(ctx, turn, tool, cv.g.c, cv.g.repo, rev)
		if refusal != nil {
			return *refusal, nil
		}
		commit = resolved[0].oid
	}
	answer := code.FindingResolve{RepoID: cv.g.repo.ID, ChangeID: cv.change.ID}
	for _, id := range ids {
		answer.Findings = append(answer.Findings, code.FindingResolution{FindingID: id, Resolution: resolution,
			Note: argString(args, "note"), Commit: commit})
	}
	res, refusal := cv.send(ctx, code.ConversationWrite{FindingResolve: &answer})
	if refusal != nil {
		return *refusal, nil
	}
	return cv.page(ctx, cv.answerOf(res))
}
