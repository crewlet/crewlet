package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE CHANGE READS: code_list_changes, the company's changes a page at a time,
// and code_get_change, one change a PART at a time — what it is, whether it
// may merge, its files and a file's patch at a revision or since an earlier
// one, its findings placed in the revision asked about, its reviews and its
// comments. Two more of the code reads ([CodeTools]), offered and judged as
// every one of them is: a seat's and a person's assistant's alike, on the one
// list both surfaces take; each call decided again for whoever woke the turn
// ([CodeDeps.caller]); the repository a call names resolved once a turn and
// held ([CodeDeps.repo]); and every question held, where it is answered, to
// the grant on the change's repository — of the caller and of the colleague
// a turn reads for ([code.JudgeRead]). [changeToolNeeds] names, one table for
// every change tool, the access each one's call needs.
//
// # Rows at the seat's level, once a call
//
// A change, its revisions and its conversation are ROWS, read at the seat's
// level — linearizable on both surfaces ([seatReadLevel]) — as a
// repository's are, so an answer holds every record the code log committed
// before the call. A call that reads the change more than once — the
// findings beside the revisions they are placed in, two revisions to compare
// — makes its first read at that level and every later one at SESSION,
// floored at the first's position: one barrier a call, and every later read
// at or past what the first saw.
//
// # Objects at the revisions' own floors
//
// A revision's files, a file's patch, what changed between two revisions and
// where a finding's lines are now are reads BY OBJECT ID of the heads and the
// bases the rows recorded, asked of the repository's home git node at a SYNC
// level: the furthest of this node's own floor on the repository, the
// record that moved each revision's head and, where its facts are recorded,
// the record that moved its target to the tip its base was read from. Never
// at a row read's checkpoint, which a linearizable read takes past its own
// barrier: a copy current to every push still sits below it, and a floor
// there would make the git node sync for nothing (codetools.go says the same
// of a ref). A finding is placed in a revision floored at the change's HEAD
// revision too: its findings were raised on revisions up to it, so the copy
// holds every blob their lines were read in.
//
// # Text somebody wrote, fenced by who wrote it
//
// A change's description, a review's summary, a finding's body and its
// suggestion, and a comment reach the model fenced — `change_description`,
// `change_review`, `change_comment` — and, unlike a repository's own text,
// each NAMES ITS WRITER, whom the change's rows record: the owner who opened
// it, the reviewer, the raiser, the commenter, each a seat or the human seat a
// person holds, at the trust that writer earns from the reader
// ([provenance.TrustFor]). A description is the owner's at the open and may
// have been rewritten since by a maintainer or a person the row does not
// name, so one whose change has moved since its open is read at
// [provenance.TrustForPartial]. A patch is a repository's text, read as
// code_diff reads one (`repo_patch`, trust `external`). Every other string is
// defused ([foreignRead]).
//
// # One page, one tool answer
//
// A row read pages by the estate's bound, four mebibytes; a tool answer is 64
// KiB. So every page these tools answer is cut WHOLE-ROW at one tool answer
// as it is sent, fences and all ([fitLabelled]), and resumes after the last
// row it kept by that row's own cursor — [code.ReadQuery.ChangesCursor] for a
// listing, each part's row's Cursor for a change's — never by a spelling of
// the reader's cursors written again here. A git node cuts its page at
// code.MaxGitAnswerBytes, which leaves these tools' wrapping room in the
// answer, as it does code_diff's.

// The change read tools' names: each is its authority row's action.
const (
	CodeListChangesTool = string(authz.ActionCodeListChanges)
	CodeGetChangeTool   = string(authz.ActionCodeGetChange)
)

// ChangeToolNames are the tools that read a repository's changes, in the
// order a catalogue lists them; [changeToolNeeds] names the access each needs.
func ChangeToolNames() []string { return []string{CodeListChangesTool, CodeGetChangeTool} }

// changeToolNeeds is the ACCESS each change tool's call needs on the change's
// repository — of its caller and, in a turn a colleague seat woke, of that
// seat too, whom the call reads for (the requester ceiling, [CodeDeps.caller]):
// ONE TABLE, which TestEveryChangeToolNamesTheAccessItNeeds holds to the
// change tools, [ChangeToolNames] and [ChangeWrites], both ways.
//
// A READ NEEDS READ, the one access the node that answers it judges both
// principals at: every question a read makes carries the colleague as its
// ceiling ([code.ReadQuery.CeilingSeat], [code.GitQuery.CeilingSeat]), and
// [code.JudgeRead] serves only what the caller and the colleague may each
// read. A read tool's need above read would be one no read holds a colleague
// to, so a read's row is read and nothing else.
//
// A WRITE NEEDS THE LEAST ACCESS AT WHICH ITS DECIDE MAY ADMIT IT, and the
// rest is the RELATION the deciding node judges from its own chart (the
// change domain's rule D21), which no access alone names: an open needs the
// push of the branch it proposes — a full_access seat's own namespace on a
// repository it reads, so read — and a change's lifecycle is steered by its
// owner, whatever their access, by a principal that maintains the repository
// or by a person — so read too. A comment and an ask for review need read (an
// author asks whatever their access); a review needs review, the one write
// whose access alone admits it; and an answer to findings is an author's, or a
// dismissal by the raiser, a maintainer or a person — read.
//
// A WRITE IN A TURN A COLLEAGUE SEAT WOKE holds that colleague at the same
// access, on the data node that decides it (code.ChangeProposal.Ceiling,
// code.ConversationWrite.Ceiling, [CodeDeps.writeCaller]): the relation beyond
// it is the seat's own to meet, so an author may ask a seat to review the
// change it could never review itself. The committer reads that access from
// the write's kind — review for a review, read for every other — which
// TestEveryChangeToolNamesTheAccessItNeeds holds to these rows. A REBASE is a
// branch move and nothing else, so the git node that builds it holds the
// colleague as it holds every branch write (code.EngineWrite.Ceiling,
// code.Grant.Under on the source it moves, the seat's own work spared), and no
// decide reads an access for it.
var changeToolNeeds = map[string]code.Access{
	CodeListChangesTool:     code.AccessRead,
	CodeGetChangeTool:       code.AccessRead,
	CodeOpenChangeTool:      code.AccessRead,
	CodeUpdateChangeTool:    code.AccessRead,
	CodeCloseChangeTool:     code.AccessRead,
	CodeRequestReviewTool:   code.AccessRead,
	CodeReviewChangeTool:    code.AccessReview,
	CodeCommentOnChangeTool: code.AccessRead,
	CodeResolveFindingTool:  code.AccessRead,
	// An apply pushes the change's source branch — a full_access seat's own
	// namespace on a repository it reads among them — so read, the push's
	// rule the git node's and the committer's.
	CodeApplySuggestionsTool: code.AccessRead,
	// A rebase force-pushes the change's source branch, its rule likewise the
	// git node's and the committer's — and, in a turn a colleague woke, the
	// colleague's too, on that git node (code.EngineWrite.Ceiling).
	CodeRebaseChangeTool: code.AccessRead,
	// A merge and a dequeue are decided from the deciding node's own chart —
	// who may merge into the target is its gate's merge_by (M5-D2), who may
	// take an entry out its enqueuer, its change's owner, a maintainer or a
	// person — so read, the least access at which it may admit the call. In
	// a turn somebody woke each carries the turn's ceiling, held to that
	// same relation for the requester where it is decided (the git node for
	// a merge, the committer for a dequeue), never to an access alone.
	CodeMergeChangeTool:   code.AccessRead,
	CodeDequeueChangeTool: code.AccessRead,
	// A revert creates a branch — a full_access seat's own namespace — and
	// opens a change in that push's record, so its rule is the push's, the
	// git node's and the committer's, as a rebase's: read.
	CodeRevertChangeTool: code.AccessRead,
}

// The listing's page. A listed change is its row and its readiness in a line,
// about six hundred bytes at an ordinary title, so twenty are a fifth of one
// tool answer — a screen of them — and fifty at most; a page that outgrows
// the answer at the widest titles and branch names is cut whole-row at it.
const (
	defaultCodeChangeList = 20
	maxCodeChangeList     = 50
)

// The pages of one change's parts, each the rows a page READS: twenty-five
// revisions, findings, reviews or comments — about what a model reads down a
// list at once, and at an ordinary row (a revision about three hundred bytes,
// a finding or a review about a kilobyte, a comment about half of one) under
// half of one tool answer; a page of long rows is cut whole-row at the answer.
// The findings' page is also what one re-anchoring places, at most
// code.MaxReanchors, which the constant below holds at compile time.
const (
	changeRevisionsPage = 25
	changeFindingsPage  = 25
	changeReviewsPage   = 25
	changeCommentsPage  = 25
)

// A page of findings is placed in one read: its size may not pass what one
// re-anchoring places (a negative constant does not compile).
const _ = uint(code.MaxReanchors - changeFindingsPage)

// ---- a change as these tools name it ------------------------------------ //

// codeChangeRow is a change as the change tools name it: what a seat reads it
// by and decides about — its reference, its branches, its head, who owns it,
// the work it carries, its version, which a write of it names — never the
// ids and positions that are the code host's own bookkeeping.
type codeChangeRow struct {
	Ref         string           `json:"ref"`
	Title       string           `json:"title"`
	State       code.ChangeState `json:"state"`
	SourceRef   string           `json:"source_ref"`
	TargetRef   string           `json:"target_ref"`
	Head        string           `json:"head"`
	Revision    int64            `json:"revision"`
	Owner       string           `json:"owner"`
	OwnerKind   code.PartyKind   `json:"owner_kind"`
	WorkItem    *types.WorkItem  `json:"work_item,omitempty"`
	Access      string           `json:"access,omitempty"`
	OpenedAt    time.Time        `json:"opened_at"`
	ClosedAt    time.Time        `json:"closed_at,omitzero"`
	CloseReason string           `json:"close_reason,omitempty"`
	MergedAt    time.Time        `json:"merged_at,omitzero"`
	MergedVia   code.MergedVia   `json:"merged_via,omitempty"`
	MergeOID    string           `json:"merge_oid,omitempty"`
	Version     int64            `json:"version"`
	// Readiness is a live change's merge readiness in one line; the whole
	// judgment is code_get_change's merge_readiness.
	Readiness *codeReadyLine `json:"readiness,omitempty"`
}

// codeReadyLine is a live change's readiness in one line: how many of its
// preconditions are met, of how many, and the first one's sentence.
type codeReadyLine struct {
	Met   int    `json:"met"`
	Total int    `json:"total"`
	First string `json:"first"`
}

// changeRowOf is s as the change tools name it.
func changeRowOf(s code.ChangeSummary) codeChangeRow {
	row := codeChangeRow{Ref: s.Ref, Title: s.Title, State: s.State, SourceRef: s.SourceRef,
		TargetRef: s.TargetRef, Head: s.Head, Revision: s.Revision, Owner: s.Owner, OwnerKind: s.OwnerKind,
		WorkItem: s.WorkItem, Access: s.Access, OpenedAt: s.OpenedAt, ClosedAt: s.ClosedAt,
		CloseReason: s.CloseReason, MergedAt: s.MergedAt, MergedVia: s.MergedVia, MergeOID: s.MergeOID,
		Version: s.Version}
	if r := s.Readiness; r != nil {
		row.Readiness = &codeReadyLine{Met: r.Met, Total: r.Total, First: r.First}
	}
	return row
}

// party is one of a change's parties — its owner, a reviewer, a raiser, a
// commenter — as a fence names its writer: a seat by its handle, a person by
// the human seat they hold, and a kind this build does not name resolved
// against the turn's chart as an unkinded record's is.
func (r *foreignRead) party(handle string, kind code.PartyKind) provenance.Author {
	switch kind {
	case code.PartyAgent:
		return r.writer(handle, iam.ActorAgent)
	case code.PartyPerson:
		return r.writer(handle, iam.ActorHuman)
	}
	return r.writer(handle, "")
}

// ---- code_list_changes --------------------------------------------------- //

type codeListChanges struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeListChanges)(nil)

func (t *codeListChanges) Name() string { return CodeListChangesTool }

func (t *codeListChanges) Description() string {
	return "List changes — a branch proposed into another, named like web!42 — newest first, a page at a time, " +
		"a live one's merge readiness in a line. Name what to list — a `repo`, a `work` item, a `state`, a " +
		"`reviewer` or `needs` — and narrow it with the rest. A change of a repository you may not read — or, " +
		"in a turn a colleague woke, they may not — is not listed, so a page can hold fewer than `limit`, or " +
		"none, and still name a `next`."
}

func (t *codeListChanges) Parameters() map[string]any {
	states := make([]string, 0, len(code.ChangeStates()))
	for _, s := range code.ChangeStates() {
		states = append(states, string(s))
	}
	needs := make([]string, 0, len(code.Needs()))
	for _, n := range code.Needs() {
		needs = append(needs, string(n))
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":  map[string]any{"type": "string", "description": "Only this repository's changes."},
			"state": map[string]any{"type": "string", "enum": states},
			"owner": map[string]any{"type": "string", "description": "Only the changes this seat opened."},
			"work": map[string]any{"type": "string",
				"description": "Only the changes carrying this work item: `<backend>:<id>`."},
			"reviewer": map[string]any{"type": "string", "description": "Only the changes asking this seat's review."},
			"needs": map[string]any{"type": "string", "enum": needs,
				"description": "Only the live changes waiting on a `review` or a `fix` — yours, unless `reviewer` " +
					"or `owner` names another — on you (`me`, both), or on any `person`."},
			"cursor": codeCursorArg(),
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many changes a page reads, 1 to %d; %d by default.",
					maxCodeChangeList, defaultCodeChangeList),
			},
		},
	}
}

func (t *codeListChanges) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// codeChangeList is code_list_changes' answer.
type codeChangeList struct {
	Changes  []codeChangeRow   `json:"changes"`
	Next     string            `json:"next,omitempty"`
	Position statelog.Position `json:"position"`
}

func (t *codeListChanges) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	q := code.ReadQuery{Kind: code.ReadChanges, Owner: strings.TrimSpace(argString(args, "owner")),
		Work: strings.TrimSpace(argString(args, "work")), Reviewer: strings.TrimSpace(argString(args, "reviewer")),
		Cursor: strings.TrimSpace(argString(args, "cursor")), Fresh: seatRead}
	if s := code.ChangeState(strings.TrimSpace(argString(args, "state"))); s != "" {
		if !s.Valid() {
			return failed(fmt.Sprintf("%s: `state` is one of %v, not %q.", t.Name(), code.ChangeStates(),
				clip(string(s)))), nil
		}
		q.State = s
	}
	if n := code.Need(strings.TrimSpace(argString(args, "needs"))); n != "" {
		if !n.Valid() {
			return failed(fmt.Sprintf("%s: `needs` is one of %v, not %q.", t.Name(), code.Needs(),
				clip(string(n)))), nil
		}
		q.Needs = n
	}
	if q.Limit = argInt(args, "limit", defaultCodeChangeList); q.Limit < 1 || q.Limit > maxCodeChangeList {
		return failed(fmt.Sprintf("%s: `limit` is 1 to %d.", t.Name(), maxCodeChangeList)), nil
	}
	named := strings.TrimSpace(argString(args, "repo")) != ""
	if !named && q.Work == "" && q.State == "" && q.Reviewer == "" && q.Needs == "" {
		// REFUSED BEFORE ANYTHING IS READ, in the reader's own terms: no
		// index holds every change the company has, so a listing names what
		// it walks.
		return failed(fmt.Sprintf("%s needs to know what to list: a `repo`, a `work` item, a `state`, a "+
			"`reviewer` or `needs` (%v) — `needs: me` is what waits on you.", t.Name(), code.Needs())), nil
	}
	var repo code.RepoRef
	if named {
		name, refusal := repoArg(t.Name(), args)
		if refusal != nil {
			return *refusal, nil
		}
		if repo, refusal = t.deps.repo(ctx, turn, t.Name(), c, name); refusal != nil {
			return *refusal, nil
		}
		q.Repo = repo.Name
	}
	answer, err := t.deps.read(ctx, c, q)
	if err != nil {
		if named {
			return t.deps.failed(ctx, turn, t.Name(), c.asker, repo, err), nil
		}
		return codeFailure(ctx, t.Name(), "", err), nil
	}
	rows := make([]codeChangeRow, len(answer.Changes))
	for i, s := range answer.Changes {
		if named {
			if r := t.deps.sameRepo(turn, t.Name(), c.asker, repo, code.RepoRef{ID: s.RepoID}); r != nil {
				return *r, nil
			}
		}
		rows[i] = changeRowOf(s)
	}
	// A PAGE CUT WHOLE-ROW AT THE ANSWER, resuming after the last change
	// kept by the walk's own cursor ([code.ReadQuery.ChangesCursor]): a row
	// is bounded by the domain's own limits — a title, two branch names, a
	// close reason and a readiness sentence, each at its longest a few
	// kilobytes — so one row always fits, and a page of the widest is cut.
	page := func(k int) any {
		next := answer.Next
		if k > 0 && k < len(rows) {
			next = q.ChangesCursor(answer.Changes[k-1])
		}
		return codeChangeList{Changes: rows[:k], Next: next, Position: answer.Position}
	}
	read := newForeignRead(turn, c.asker.Seat)
	k, rendered, err := read.fit(len(rows), page)
	switch {
	case err != nil:
		return labelFault(ctx, t.Name(), err), nil
	case k == 0 && len(rows) > 0:
		return faulted(ctx, t.Name(), fmt.Errorf("builtin: change %s alone is past one tool answer",
			answer.Changes[0].Ref), fmt.Sprintf("%s could not list changes: %s.", t.Name(), faultSaid)), nil
	}
	return compactAnswer(rendered, "Ask for fewer with `limit`.")
}

// ---- code_get_change ------------------------------------------------------ //

// changePart is the part of one change code_get_change answers. A closed set
// with [changePart.Valid].
type changePart string

const (
	// partSummary is the change itself: its row, its description, its
	// authors, its conversation's counters and its revisions, newest first.
	partSummary changePart = "summary"
	// partReadiness is whether it may merge: each precondition judged, whom
	// an unmet one waits on, who clears each standing blocker, and its
	// head's recorded merge into its target.
	partReadiness changePart = "merge_readiness"
	// partFiles is the files a revision changes, from its base — or, since
	// an earlier revision, what changed between the two.
	partFiles changePart = "files"
	// partDiff is one of those files' patch.
	partDiff changePart = "diff"
	// partFindings is its findings, each placed in the revision asked about.
	partFindings changePart = "findings"
	// partReviews is its reviews, in the order they were given.
	partReviews changePart = "reviews"
	// partComments is its comments, a thread at a time.
	partComments changePart = "comments"
	// partChecks is its head's checks, checked for its target: each
	// attempt in a line, its summary fenced (codechecks.go).
	partChecks changePart = "checks"
)

// changeParts is the closed set, in the order a description names them.
func changeParts() []changePart {
	return []changePart{partSummary, partReadiness, partFiles, partDiff, partFindings, partReviews, partComments,
		partChecks}
}

// Valid reports a part this build answers.
func (p changePart) Valid() bool { return slices.Contains(changeParts(), p) }

type codeGetChange struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeGetChange)(nil)

func (t *codeGetChange) Name() string { return CodeGetChangeTool }

// Description says what each part answers and the one choice no part can
// carry: a change's diff, its findings and what changed since a review are
// read here, never by a clone's history.
func (t *codeGetChange) Description() string {
	return "One change, a `part` at a time: `summary` (its row, description, authors, counts, revisions), " +
		"`merge_readiness` (each precondition, whom it waits on, who clears each blocker), `files` and `diff` " +
		"(a revision's files and a file's patch from its base, or with `since_revision` what changed since), " +
		"`findings` (placed in the revision asked about), `reviews`, `comments`, `checks` (its head's). Its " +
		"writers' text arrives " +
		"fenced. A change's diff, findings and what changed since your review are read here — history in a " +
		"sandbox is for building, not for answering questions."
}

func (t *codeGetChange) Parameters() map[string]any {
	parts := make([]string, 0, len(changeParts()))
	for _, p := range changeParts() {
		parts = append(parts, string(p))
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": map[string]any{"type": "string",
				"description": "The change's repository; not needed where `change` names it."},
			"change": map[string]any{"type": "string",
				"description": "Its number on `repo`, or its reference (web!42)."},
			"part": map[string]any{"type": "string", "enum": parts, "description": "What to read; summary by default."},
			"revision": map[string]any{"type": "integer",
				"description": "With files, diff or findings: the revision read at; its latest by default."},
			"since_revision": map[string]any{"type": "integer",
				"description": "With files or diff: an earlier revision, to read what changed from it to `revision`."},
			"file": map[string]any{"type": "string", "description": "With diff: the file, as files names it."},
			"file_b64": map[string]any{"type": "string",
				"description": "The same file as its path's exact bytes in standard base64. Never beside `file`."},
			"cursor": codeCursorArg(),
		},
		"required": []string{"change"},
	}
}

func (t *codeGetChange) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// changeArg is the change a call names — `change`, by its number on `repo` or
// by its reference, web!42, which names the repository too — read with the
// rest of a call's arguments, before anything is.
func changeArg(tool string, args map[string]any) (string, int64, *tools.Result) {
	repo := strings.TrimSpace(argString(args, "repo"))
	var number int64
	switch v := args["change"].(type) {
	case string:
		s := strings.TrimSpace(v)
		if name, n, ok := strings.Cut(s, "!"); ok {
			if repo != "" && repo != name {
				r := failed(fmt.Sprintf("%s: `change` %q is a change of repository %q and `repo` names %q — "+
					"name one repository.", tool, clip(s), clip(name), clip(repo)))
				return "", 0, &r
			}
			repo, s = name, n
		}
		number, _ = strconv.ParseInt(s, 10, 64)
	case nil:
	default:
		number = int64(argInt(args, "change", 0))
	}
	if number < 1 {
		r := failed(fmt.Sprintf("%s needs `change`: a change's number on `repo`, or its reference as %s "+
			"names it (web!42).", tool, CodeListChangesTool))
		return "", 0, &r
	}
	name, refusal := repoArg(tool, map[string]any{"repo": repo})
	if refusal != nil {
		return "", 0, refusal
	}
	return name, number, nil
}

// changeCall is what one call of code_get_change asked for, read and
// checked before anything is: the part, the revision read at (0 for the
// latest), the one changes are read since, the file and the cursor.
type changeCall struct {
	part          changePart
	revision      int64
	since         int64
	file, fileB64 string
	cursor        string
}

// changeCallArgs is a call's part and the arguments that go with it, or the
// refusal of an argument its part does not take — refused before anything is
// read, so a call that misnames what it reads costs no read.
func changeCallArgs(tool string, args map[string]any) (changeCall, *tools.Result) {
	call := changeCall{part: changePart(strings.TrimSpace(argString(args, "part"))),
		revision: int64(argInt(args, "revision", 0)), since: int64(argInt(args, "since_revision", 0)),
		file: argString(args, "file"), fileB64: argString(args, "file_b64"),
		cursor: strings.TrimSpace(argString(args, "cursor"))}
	if call.part == "" {
		call.part = partSummary
	}
	refuse := func(why string) (changeCall, *tools.Result) {
		r := failed(tool + ": " + why)
		return changeCall{}, &r
	}
	takes := func(ps ...changePart) bool { return slices.Contains(ps, call.part) }
	switch {
	case !call.part.Valid():
		return refuse(fmt.Sprintf("`part` is one of %v, not %q.", changeParts(), clip(string(call.part))))
	case call.revision < 0 || call.since < 0:
		return refuse("a revision is numbered from 1.")
	case call.revision != 0 && !takes(partFiles, partDiff, partFindings):
		return refuse(fmt.Sprintf("`revision` chooses what %s, %s and %s read at; %s reads none.", partFiles,
			partDiff, partFindings, call.part))
	case call.since != 0 && !takes(partFiles, partDiff):
		return refuse(fmt.Sprintf("`since_revision` reads what %s and %s changed since a revision; %s reads "+
			"no change between two.", partFiles, partDiff, call.part))
	case call.since != 0 && call.revision != 0 && call.since >= call.revision:
		return refuse("`since_revision` is a revision before `revision`.")
	case (call.file != "" || call.fileB64 != "") && call.part != partDiff:
		return refuse(fmt.Sprintf("`file` names the file whose patch %s reads; %s lists every file it touches.",
			partDiff, partFiles))
	case call.part == partDiff && call.file == "" && call.fileB64 == "":
		return refuse(fmt.Sprintf("%s reads one file's patch: name it with `file`, as %s lists it.", partDiff,
			partFiles))
	case call.cursor != "" && call.part == partReadiness:
		return refuse(fmt.Sprintf("%s is one answer, with no next page.", partReadiness))
	}
	return call, nil
}

func (t *codeGetChange) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	name, number, refusal := changeArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	call, refusal := changeCallArgs(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	repo, refusal := t.deps.repo(ctx, turn, t.Name(), c, name)
	if refusal != nil {
		return *refusal, nil
	}
	g := changeRead{deps: t.deps, turn: turn, tool: t.Name(), c: c, repo: repo, number: number}
	switch call.part {
	case partReadiness:
		return g.readiness(ctx)
	case partFiles, partDiff:
		return g.diff(ctx, call)
	case partFindings:
		return g.findings(ctx, call)
	case partReviews:
		return g.reviews(ctx, call.cursor)
	case partComments:
		return g.comments(ctx, call.cursor)
	case partChecks:
		return g.checks(ctx, call.cursor)
	}
	return g.summary(ctx, call.cursor)
}

// changeRead is one call of code_get_change: who reads, the repository the
// turn resolved, and the change by its number on it.
type changeRead struct {
	deps   CodeDeps
	turn   *turnctx.Turn
	tool   string
	c      codeCaller
	repo   code.RepoRef
	number int64
}

// ref is the change as a person and a seat name it, web!42.
func (g changeRead) ref() string { return code.ChangeRef(g.repo.Name, g.number) }

// read is q — a `change` read of this call's change — asked as its caller:
// at the seat's level, or, where after is set, at session floored at it —
// a later read of the call, at or past what its first saw. A detail naming
// another repository than the one the turn resolved the name to is refused
// as a name taken again between the two ([CodeDeps.sameRepo]).
func (g changeRead) read(ctx context.Context, q code.ReadQuery, after statelog.Position) (code.ReadAnswer,
	*tools.Result) {

	q.Kind, q.Repo, q.Change, q.Fresh = code.ReadChange, g.repo.Name, g.number, seatRead
	if !after.IsZero() {
		q.Fresh = statelog.FloorAt(statelog.ReadSession, after)
	}
	answer, err := g.deps.read(ctx, g.c, q)
	if err != nil {
		r := g.deps.failed(ctx, g.turn, g.tool, g.c.asker, g.repo, err)
		return code.ReadAnswer{}, &r
	}
	if d := answer.Change; d != nil {
		if r := g.deps.sameRepo(g.turn, g.tool, g.c.asker, g.repo, code.RepoRef{ID: d.RepoID}); r != nil {
			return code.ReadAnswer{}, r
		}
	}
	return answer, nil
}

// head is the change in detail and its head revision — a first page of its
// revisions, one revision long — and the position it was read at.
func (g changeRead) head(ctx context.Context, after statelog.Position) (*code.ChangeDetail, code.RevisionRow,
	statelog.Position, *tools.Result) {

	answer, refusal := g.read(ctx, code.ReadQuery{Limit: 1}, after)
	if refusal != nil {
		return nil, code.RevisionRow{}, statelog.Position{}, refusal
	}
	if answer.Change == nil || len(answer.Revisions) != 1 {
		r := faulted(ctx, g.tool, fmt.Errorf("builtin: the code host answered %s's first page with %d revisions "+
			"and detail %v", g.ref(), len(answer.Revisions), answer.Change != nil),
			fmt.Sprintf("%s could not read %s: %s.", g.tool, g.ref(), faultSaid))
		return nil, code.RevisionRow{}, statelog.Position{}, &r
	}
	return answer.Change, answer.Revisions[0], answer.Position, nil
}

// revision is revision n of a change whose latest is latest, read at session
// floored at after: the page of its revisions that resumes after n+1, one
// revision long — refused where the change has fewer than n.
func (g changeRead) revision(ctx context.Context, n, latest int64, after statelog.Position) (code.RevisionRow,
	*tools.Result) {

	if n > latest {
		r := failed(fmt.Sprintf("%s: %s has %d %s, numbered from 1, and no revision %d.", g.tool, g.ref(), latest,
			plural(latest, "revision", "revisions"), n))
		return code.RevisionRow{}, &r
	}
	answer, refusal := g.read(ctx, code.ReadQuery{Cursor: code.RevisionRow{N: n + 1}.Cursor(), Limit: 1}, after)
	if refusal != nil {
		return code.RevisionRow{}, refusal
	}
	if len(answer.Revisions) != 1 || answer.Revisions[0].N != n {
		r := faulted(ctx, g.tool, fmt.Errorf("builtin: the code host answered %s's revision %d with %d revisions",
			g.ref(), n, len(answer.Revisions)), fmt.Sprintf("%s could not read revision %d of %s: %s.", g.tool, n,
			g.ref(), faultSaid))
		return code.RevisionRow{}, &r
	}
	return answer.Revisions[0], nil
}

// plural is one where n is 1, many otherwise.
func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// floor is what a read by object id of revs is asked with ([changeRead]'s
// doc): the code domain's one rule ([code.ObjectFloor]) over this node's own
// floor on the repository.
func (g changeRead) floor(revs ...code.RevisionRow) statelog.Freshness {
	return code.ObjectFloor(g.deps.Code.FloorOf(g.repo.ID), revs...)
}

// ---- summary --------------------------------------------------------------- //

// codeChangeDetail is one change in detail: its row, what its description
// says, who wrote it, and what its conversation counts.
type codeChangeDetail struct {
	codeChangeRow
	Description  string            `json:"description,omitempty"`
	Authors      []codeAuthor      `json:"authors"`
	Conversation *codeConversation `json:"conversation,omitempty"`
}

// codeAuthor is one author of a change: whoever opened it and everyone whose
// push moved its head, from the revision they first wrote — whose approval
// never counts toward its merge.
type codeAuthor struct {
	Author        string         `json:"author"`
	Kind          code.PartyKind `json:"kind"`
	FirstRevision int64          `json:"first_revision"`
}

// codeConversation is what a change's conversation counts.
type codeConversation struct {
	Comments         int64 `json:"comments"`
	Reviews          int64 `json:"reviews"`
	OpenFindings     int64 `json:"open_findings"`
	OpenBlockers     int64 `json:"open_blockers"`
	ChangesRequested int64 `json:"changes_requested"`
}

// codeRevision is one revision of a change: its head, who pushed it and when,
// and the facts its home recorded of it — its base and its target as they
// were read, its patch id, and how many files it touches.
type codeRevision struct {
	N          int64              `json:"n"`
	Head       string             `json:"head"`
	Pusher     string             `json:"pusher"`
	PusherKind code.PartyKind     `json:"pusher_kind"`
	At         time.Time          `json:"at"`
	Facts      *codeRevisionFacts `json:"facts,omitempty"`
}

// codeRevisionFacts is a revision's recorded facts as the tools name them:
// the touched files counted — `files` lists them.
type codeRevisionFacts struct {
	TargetRef      string `json:"target_ref"`
	TargetOID      string `json:"target_oid"`
	Base           string `json:"base,omitempty"`
	PatchID        string `json:"patch_id,omitempty"`
	PatchIDOmitted bool   `json:"patch_id_omitted,omitempty"`
	Touched        int    `json:"touched"`
	TouchesAll     bool   `json:"touches_all,omitempty"`
}

// codeChangeSummary is code_get_change's summary: the change in detail on a
// first page, and a page of its revisions, newest first.
type codeChangeSummary struct {
	Repo      string            `json:"repo"`
	Change    *codeChangeDetail `json:"change,omitempty"`
	Revisions []codeRevision    `json:"revisions"`
	Next      string            `json:"next,omitempty"`
	Position  statelog.Position `json:"position"`
}

// detailOf is d as the summary names it.
func detailOf(d *code.ChangeDetail) *codeChangeDetail {
	out := &codeChangeDetail{codeChangeRow: changeRowOf(d.ChangeSummary), Description: d.Body,
		Authors: make([]codeAuthor, len(d.Authors))}
	for i, a := range d.Authors {
		out.Authors[i] = codeAuthor{Author: a.Author, Kind: a.Kind, FirstRevision: a.FirstRevision}
	}
	out.Conversation = conversationOf(d.Conversation)
	return out
}

// conversationOf is what a change's conversation counts, nil for a change
// nobody has discussed.
func conversationOf(v *code.ConversationRow) *codeConversation {
	if v == nil {
		return nil
	}
	return &codeConversation{Comments: v.Comments, Reviews: v.Reviews, OpenFindings: v.OpenFindings,
		OpenBlockers: v.OpenBlockers, ChangesRequested: v.ChangesRequested}
}

// revisionOf is r as the summary names it.
func revisionOf(r code.RevisionRow) codeRevision {
	out := codeRevision{N: r.N, Head: r.Head, Pusher: r.Pusher, PusherKind: r.PusherKind, At: r.At}
	if f := r.Facts; f != nil {
		out.Facts = &codeRevisionFacts{TargetRef: f.TargetRef, TargetOID: f.TargetOID, Base: f.BaseOID,
			PatchID: f.PatchID, PatchIDOmitted: f.PatchIDOmitted, Touched: len(f.Touched), TouchesAll: f.TouchesAll}
	}
	return out
}

// summary answers the change in detail on a first page and its revisions a
// page at a time; a later page, after the cursor of the one before, carries
// its revisions alone.
func (g changeRead) summary(ctx context.Context, cursor string) (tools.Result, error) {
	answer, refusal := g.read(ctx, code.ReadQuery{Cursor: cursor, Limit: changeRevisionsPage}, statelog.Position{})
	if refusal != nil {
		return *refusal, nil
	}
	var detail *codeChangeDetail
	if d := answer.Change; d != nil {
		detail = detailOf(d)
	}
	revs := make([]codeRevision, len(answer.Revisions))
	for i, r := range answer.Revisions {
		revs[i] = revisionOf(r)
	}
	next := func(k int) string {
		switch {
		case k == len(revs):
			return answer.Next
		case k > 0:
			return answer.Revisions[k-1].Cursor()
		}
		// NONE KEPT BESIDE THE DETAIL: the next page starts at this one's
		// first revision, a page of revisions alone.
		return code.RevisionRow{N: answer.Revisions[0].N + 1}.Cursor()
	}
	build := func(k int) (*foreignRead, any) {
		read := newForeignRead(g.turn, g.c.asker.Seat)
		if d := answer.Change; d != nil {
			// THE OWNER'S WORDS AT THE OPEN, and since then perhaps an
			// editor's the row does not name: any record that moved the
			// change past its open may have rewritten them.
			read.fence(fieldPath{}.key("change").key("description"), provenance.SourceChangeDescription,
				[]provenance.Author{read.party(d.Owner, d.OwnerKind)}, d.Version > 1, d.Body)
		}
		return read, codeChangeSummary{Repo: g.repo.Name, Change: detail, Revisions: revs[:k], Next: next(k),
			Position: answer.Position}
	}
	k, rendered, err := fitLabelled(len(revs), build)
	switch {
	case err != nil:
		return labelFault(ctx, g.tool, err), nil
	case k == 0 && len(rendered) > ToolAnswerBytes:
		// THE DETAIL ALONE PAST ONE ANSWER: a description at its longest,
		// in characters JSON escapes, or a change with many authors. No part
		// narrows it.
		return failed(fmt.Sprintf("%s: %s's description and authors alone are past one answer, so its summary "+
			"cannot be read through %s; its other parts can.", g.tool, g.ref(), g.tool)), nil
	}
	return compactAnswer(rendered, "Read the change's revisions a page at a time with `cursor`.")
}

// ---- merge_readiness -------------------------------------------------------- //

// codeChangeReadiness is code_get_change's merge_readiness: the change's
// state and head, its readiness judged in the read's own snapshot — nil for a
// change no longer live, which merges nothing — its head's recorded merge into
// its target, and its conversation's counts.
type codeChangeReadiness struct {
	Repo         string            `json:"repo"`
	Change       string            `json:"change"`
	State        code.ChangeState  `json:"state"`
	Revision     int64             `json:"revision"`
	Head         string            `json:"head"`
	TargetRef    string            `json:"target_ref"`
	Readiness    *code.Readiness   `json:"readiness,omitempty"`
	Mergeability *codeMergeability `json:"mergeability,omitempty"`
	Conversation *codeConversation `json:"conversation,omitempty"`
	Position     statelog.Position `json:"position"`
}

// codeMergeability is whether a revision merged cleanly into its target, as
// its home recorded it: the revision, the target tip it merged into, and the
// paths that conflict, the rest counted.
type codeMergeability struct {
	Revision         int64           `json:"revision"`
	TargetRef        string          `json:"target_ref"`
	TargetOID        string          `json:"target_oid"`
	State            code.MergeState `json:"state"`
	Conflicts        []string        `json:"conflicts,omitempty"`
	ConflictsOmitted int64           `json:"conflicts_omitted,omitempty"`
	At               time.Time       `json:"at"`
}

// readiness answers whether the change may merge.
func (g changeRead) readiness(ctx context.Context) (tools.Result, error) {
	d, _, at, refusal := g.head(ctx, statelog.Position{})
	if refusal != nil {
		return *refusal, nil
	}
	out := codeChangeReadiness{Repo: g.repo.Name, Change: g.ref(), State: d.State, Revision: d.Revision,
		Head: d.Head, TargetRef: d.TargetRef, Readiness: d.Readiness, Position: at}
	out.Conversation = conversationOf(d.Conversation)
	var conflicts []string
	if m := d.Mergeability; m != nil {
		conflicts = m.Conflicts
		out.Mergeability = &codeMergeability{Revision: m.Revision, TargetRef: m.TargetRef, TargetOID: m.TargetOID,
			State: m.State, ConflictsOmitted: m.ConflictsOmitted, At: m.At}
	}
	// THE CONFLICTING PATHS CUT AT THE ANSWER, the rest counted beside those
	// named: a merge records up to a git page of them, which a readiness
	// beside it can push past one answer.
	page := func(k int) any {
		cut := out
		if m := out.Mergeability; m != nil {
			mm := *m
			mm.Conflicts, mm.ConflictsOmitted = conflicts[:k], m.ConflictsOmitted+int64(len(conflicts)-k)
			cut.Mergeability = &mm
		}
		return cut
	}
	read := newForeignRead(g.turn, g.c.asker.Seat)
	_, rendered, err := read.fit(len(conflicts), page)
	if err != nil {
		return labelFault(ctx, g.tool, err), nil
	}
	return compactAnswer(rendered, "")
}

// ---- files and diff ------------------------------------------------------- //

// codeChangeDiff is code_get_change's files and diff: a revision's files or
// one file's patch, from its base or since an earlier revision, as the git
// node's page answers it.
type codeChangeDiff struct {
	Repo          string `json:"repo"`
	Change        string `json:"change"`
	Revision      int64  `json:"revision"`
	SinceRevision int64  `json:"since_revision,omitempty"`
	code.GitAnswer
}

// diff answers a revision's files or one file's patch: from the revision's
// recorded base to its head, or — since an earlier revision — the interdiff
// of the two, kept to the files either touches (code-host.md, "What changed
// between two revisions").
func (g changeRead) diff(ctx context.Context, call changeCall) (tools.Result, error) {
	d, head, first, refusal := g.head(ctx, statelog.Position{})
	if refusal != nil {
		return *refusal, nil
	}
	rev := head
	if call.revision != 0 && call.revision != head.N {
		if rev, refusal = g.revision(ctx, call.revision, d.Revision, first); refusal != nil {
			return *refusal, nil
		}
	}
	revs := []code.RevisionRow{rev}
	if call.since != 0 {
		if call.since >= rev.N {
			return failed(fmt.Sprintf("%s: `since_revision` is a revision before %d, the one read.", g.tool,
				rev.N)), nil
		}
		since, refusal := g.revision(ctx, call.since, d.Revision, first)
		if refusal != nil {
			return *refusal, nil
		}
		revs = append(revs, since)
	}
	for _, r := range revs {
		if refusal := g.recorded(d, r, call.since == 0); refusal != nil {
			return *refusal, nil
		}
	}
	var since *code.RevisionRow
	if call.since != 0 {
		since = &revs[1]
	}
	q, err := code.RevisionDiff(g.repo.ID, rev, since, call.file, call.fileB64)
	if err != nil {
		// UNREACHABLE past recorded's refusals and changeCallArgs' order of
		// the two revisions, so this node's own fault, never the caller's.
		return faulted(ctx, g.tool, err, fmt.Sprintf("%s could not read %s's files: %s.", g.tool, g.ref(),
			faultSaid)), nil
	}
	q.Fresh, q.Cursor = g.floor(revs...), call.cursor
	page, refusal := g.deps.gitRead(ctx, g.turn, g.tool, g.c, g.repo, q)
	if refusal != nil {
		return *refusal, nil
	}
	read := newForeignRead(g.turn, g.c.asker.Seat)
	narrow := "Read one file's patch with part diff and `file`."
	if df := page.Diff; df != nil && df.Patch != "" {
		// A PATCH IS THE REPOSITORY'S TEXT, both sides of it, which nobody
		// this read can name vouches for: fenced as code_diff fences one.
		read.fence(fieldPath{}.key("diff").key("patch"), provenance.SourceRepoPatch, nil, false, df.Patch)
		narrow = fmt.Sprintf("The patch spells fence markers, which grow when they are escaped: read the file's "+
			"lines at the revision's head (%s) with %s and a line range instead.", rev.Head, CodeReadTool)
	}
	return read.page(ctx, g.tool, codeChangeDiff{Repo: g.repo.Name, Change: g.ref(), Revision: rev.N,
		SinceRevision: call.since, GitAnswer: page}, narrow)
}

// recorded refuses a revision whose files cannot be read from its facts
// ([code.RevisionRow.FilesGap]): none recorded — a live change's home reads
// them within a minute of the push, and a change merged or closed before it
// did never has them — or, where based, none of a base to diff its head from:
// a history that shares no commit with its target, whose change is every file
// of its head.
func (g changeRead) recorded(d *code.ChangeDetail, r code.RevisionRow, based bool) *tools.Result {
	switch r.FilesGap(d.State, based) {
	case code.FilesPending:
		res := refused(tools.RefusalUnavailable, fmt.Sprintf("%s: revision %d of %s has no recorded base yet: "+
			"the repository's home git node records each revision's facts against its target within a minute "+
			"of its push. Try again shortly.", g.tool, r.N, g.ref()))
		return &res
	case code.FilesUnrecorded:
		res := refused(tools.RefusalNotFound, fmt.Sprintf("%s: %s was %s before its home recorded revision %d's "+
			"base against its target, so it has none to read the revision's files from: read the commits it "+
			"brought with %s from its head %s, or compare that head with a commit of your choosing with %s.",
			g.tool, g.ref(), d.State, r.N, CodeLogTool, r.Head, CodeCompareTool))
		return &res
	case code.FilesUnrelated:
		res := refused(tools.RefusalNotFound, fmt.Sprintf("%s: revision %d of %s shares no history with %s, so "+
			"its change is every file of its head: read them with %s at %s.", g.tool, r.N, g.ref(),
			r.Facts.TargetRef, CodeReadTool, r.Head))
		return &res
	}
	return nil
}

// ---- findings ------------------------------------------------------------- //

// codeChangeFindings is code_get_change's findings: a page of them, each
// placed in the revision the answer is of.
type codeChangeFindings struct {
	Repo     string            `json:"repo"`
	Change   string            `json:"change"`
	Revision int64             `json:"revision"`
	Findings []codeFinding     `json:"findings"`
	Next     string            `json:"next,omitempty"`
	Position statelog.Position `json:"position"`
}

// codeFinding is one finding: who raised it on which revision and where — its
// path, its side of that revision's diff, its lines and the commit and blob
// they were read in — how much it weighs, what it says and suggests, where it
// stands, and AT, where its lines are in the revision asked about.
type codeFinding struct {
	ID             string            `json:"id"`
	ReviewID       string            `json:"review_id"`
	Raiser         string            `json:"raiser"`
	RaiserKind     code.PartyKind    `json:"raiser_kind"`
	Revision       int64             `json:"revision"`
	Path           string            `json:"path"`
	Side           code.Side         `json:"side"`
	LineStart      int               `json:"line_start"`
	LineEnd        int               `json:"line_end"`
	CommitOID      string            `json:"commit_oid"`
	BlobOID        string            `json:"blob_oid"`
	Severity       code.Severity     `json:"severity"`
	Category       code.Category     `json:"category"`
	Title          string            `json:"title"`
	Body           string            `json:"body,omitempty"`
	Suggestion     *string           `json:"suggestion,omitempty"`
	State          code.FindingState `json:"state"`
	ResolutionNote string            `json:"resolution_note,omitempty"`
	ResolvedBy     string            `json:"resolved_by,omitempty"`
	ResolvedCommit string            `json:"resolved_commit,omitempty"`
	// At is where its lines are in the revision the answer is of
	// ([code.FindingPlace]); none for a finding on the old side of a revision
	// whose base its home has not recorded yet.
	At *code.FindingPlace `json:"at,omitempty"`
}

// findings answers a page of the change's findings, each placed in the
// revision asked about: the page first, at the seat's level; then the change
// and its head revision — and the revision asked about where it is another —
// at session floored at it, so the head is every revision a finding of the
// page was raised on; then ONE re-anchoring of every finding raised on
// another revision than the one asked about.
func (g changeRead) findings(ctx context.Context, call changeCall) (tools.Result, error) {
	page, refusal := g.read(ctx, code.ReadQuery{Part: code.PartFindings, Cursor: call.cursor,
		Limit: changeFindingsPage}, statelog.Position{})
	if refusal != nil {
		return *refusal, nil
	}
	d, head, _, refusal := g.head(ctx, page.Position)
	if refusal != nil {
		return *refusal, nil
	}
	rev := head
	if call.revision != 0 && call.revision != head.N {
		if rev, refusal = g.revision(ctx, call.revision, d.Revision, page.Position); refusal != nil {
			return *refusal, nil
		}
	}
	placed, refusal := g.place(ctx, head, rev, page.Findings)
	if refusal != nil {
		return *refusal, nil
	}
	rows := make([]codeFinding, len(page.Findings))
	for i, f := range page.Findings {
		rows[i] = codeFinding{ID: f.ID, ReviewID: f.ReviewID, Raiser: f.Raiser, RaiserKind: f.RaiserKind,
			Revision: f.Revision, Path: f.Path, Side: f.Side, LineStart: f.LineStart, LineEnd: f.LineEnd,
			CommitOID: f.CommitOID, BlobOID: f.BlobOID, Severity: f.Severity, Category: f.Category, Title: f.Title,
			Body: f.Body, Suggestion: f.Suggestion, State: f.State, ResolutionNote: f.ResolutionNote,
			ResolvedBy: f.ResolvedBy, ResolvedCommit: f.ResolvedCommit}
		if at, ok := placed[f.ID]; ok {
			rows[i].At = &at
		}
	}
	build := func(k int) (*foreignRead, any) {
		read := newForeignRead(g.turn, g.c.asker.Seat)
		for i, f := range page.Findings[:k] {
			// A FINDING IS ITS RAISER'S WORDS: what it says, and the lines it
			// would put in place of its own.
			raiser := []provenance.Author{read.party(f.Raiser, f.RaiserKind)}
			at := fieldPath{}.key("findings").index(i)
			read.fence(at.key("body"), provenance.SourceChangeReview, raiser, false, f.Body)
			if f.Suggestion != nil {
				read.fence(at.key("suggestion"), provenance.SourceChangeReview, raiser, false, *f.Suggestion)
			}
		}
		next := page.Next
		if k > 0 && k < len(rows) {
			next = page.Findings[k-1].Cursor()
		}
		return read, codeChangeFindings{Repo: g.repo.Name, Change: g.ref(), Revision: rev.N, Findings: rows[:k],
			Next: next, Position: page.Position}
	}
	k, rendered, err := fitLabelled(len(rows), build)
	switch {
	case err != nil:
		return labelFault(ctx, g.tool, err), nil
	case k == 0 && len(rows) > 0:
		return failed(fmt.Sprintf("%s: finding %s of %s alone is past one answer — its body and its suggestion "+
			"spell that many characters an answer escapes — so it cannot be read through %s.", g.tool,
			rows[0].ID, g.ref(), g.tool)), nil
	}
	return compactAnswer(rendered, "Read the findings a page at a time with `cursor`.")
}

// place is where each finding of findings is in revision rev of a change whose
// head revision is head, by the finding's id: what the rows say without a read
// ([code.PlaceFindings]), and every other finding by ONE re-anchoring read,
// floored at the head revision — the latest any finding was raised on — and
// at rev's own head and base. A finding on the old side of a revision whose
// base is not yet recorded is in neither, unplaced.
func (g changeRead) place(ctx context.Context, head, rev code.RevisionRow,
	findings []code.FindingRow) (map[string]code.FindingPlace, *tools.Result) {

	placed, reanchor := code.PlaceFindings(rev, findings)
	if reanchor == nil {
		return placed, nil
	}
	answer, refusal := g.deps.gitRead(ctx, g.turn, g.tool, g.c, g.repo, code.GitQuery{Kind: code.GitReanchor,
		RepoID: g.repo.ID, Fresh: g.floor(head, rev), Reanchor: reanchor})
	if refusal != nil {
		return nil, refusal
	}
	if err := code.PlaceAnchors(placed, reanchor, answer.Reanchor); err != nil {
		r := faulted(ctx, g.tool, fmt.Errorf("builtin: %s: %w", g.ref(), err), fmt.Sprintf("%s could not place "+
			"%s's findings in revision %d: %s.", g.tool, g.ref(), rev.N, faultSaid))
		return nil, &r
	}
	return placed, nil
}

// ---- reviews and comments ------------------------------------------------ //

// codeChangeReviews is code_get_change's reviews: a page of them, in the
// order they were given.
type codeChangeReviews struct {
	Repo     string            `json:"repo"`
	Change   string            `json:"change"`
	Reviews  []codeReview      `json:"reviews"`
	Next     string            `json:"next,omitempty"`
	Position statelog.Position `json:"position"`
}

// codeReview is one review: the revision it judged, its reviewer, its
// verdict and what it says, and how many findings it raised.
type codeReview struct {
	ID           string         `json:"id"`
	Revision     int64          `json:"revision"`
	Reviewer     string         `json:"reviewer"`
	ReviewerKind code.PartyKind `json:"reviewer_kind"`
	Verdict      code.Verdict   `json:"verdict"`
	Summary      string         `json:"summary,omitempty"`
	Findings     int64          `json:"findings"`
	At           time.Time      `json:"at"`
}

// reviews answers a page of the change's reviews, each summary its reviewer's
// words.
func (g changeRead) reviews(ctx context.Context, cursor string) (tools.Result, error) {
	page, refusal := g.read(ctx, code.ReadQuery{Part: code.PartReviews, Cursor: cursor, Limit: changeReviewsPage},
		statelog.Position{})
	if refusal != nil {
		return *refusal, nil
	}
	rows := make([]codeReview, len(page.Reviews))
	for i, r := range page.Reviews {
		rows[i] = codeReview{ID: r.ID, Revision: r.Revision, Reviewer: r.Reviewer, ReviewerKind: r.ReviewerKind,
			Verdict: r.Verdict, Summary: r.Summary, Findings: r.Findings, At: r.At}
	}
	build := func(k int) (*foreignRead, any) {
		read := newForeignRead(g.turn, g.c.asker.Seat)
		for i, r := range page.Reviews[:k] {
			read.fence(fieldPath{}.key("reviews").index(i).key("summary"), provenance.SourceChangeReview,
				[]provenance.Author{read.party(r.Reviewer, r.ReviewerKind)}, false, r.Summary)
		}
		next := page.Next
		if k > 0 && k < len(rows) {
			next = page.Reviews[k-1].Cursor()
		}
		return read, codeChangeReviews{Repo: g.repo.Name, Change: g.ref(), Reviews: rows[:k], Next: next,
			Position: page.Position}
	}
	return g.answerPage(ctx, len(rows), build, "review")
}

// codeChangeComments is code_get_change's comments: a page of them, a thread
// at a time.
type codeChangeComments struct {
	Repo     string            `json:"repo"`
	Change   string            `json:"change"`
	Comments []codeComment     `json:"comments"`
	Next     string            `json:"next,omitempty"`
	Position statelog.Position `json:"position"`
}

// codeComment is one comment: its thread — its own id for a thread's first
// comment, a finding's for a reply to one — what it replies to, who said it,
// the line a thread's first comment points at, whether its thread is
// resolved, and what it says.
type codeComment struct {
	ID         string              `json:"id"`
	ThreadID   string              `json:"thread_id"`
	ParentID   string              `json:"parent_id,omitempty"`
	FindingID  string              `json:"finding_id,omitempty"`
	Author     string              `json:"author"`
	AuthorKind code.PartyKind      `json:"author_kind"`
	Anchor     *code.CommentAnchor `json:"anchor,omitempty"`
	Resolved   bool                `json:"resolved,omitempty"`
	ResolvedBy string              `json:"resolved_by,omitempty"`
	Body       string              `json:"body"`
	At         time.Time           `json:"at"`
}

// comments answers a page of the change's comments, each its writer's words.
func (g changeRead) comments(ctx context.Context, cursor string) (tools.Result, error) {
	page, refusal := g.read(ctx, code.ReadQuery{Part: code.PartComments, Cursor: cursor,
		Limit: changeCommentsPage}, statelog.Position{})
	if refusal != nil {
		return *refusal, nil
	}
	rows := make([]codeComment, len(page.Comments))
	for i, c := range page.Comments {
		rows[i] = codeComment{ID: c.ID, ThreadID: c.ThreadID, ParentID: c.ParentID, FindingID: c.FindingID,
			Author: c.Author, AuthorKind: c.AuthorKind, Anchor: c.Anchor, Resolved: c.Resolved,
			ResolvedBy: c.ResolvedBy, Body: c.Body, At: c.At}
	}
	build := func(k int) (*foreignRead, any) {
		read := newForeignRead(g.turn, g.c.asker.Seat)
		for i, c := range page.Comments[:k] {
			read.fence(fieldPath{}.key("comments").index(i).key("body"), provenance.SourceChangeComment,
				[]provenance.Author{read.party(c.Author, c.AuthorKind)}, false, c.Body)
		}
		next := page.Next
		if k > 0 && k < len(rows) {
			next = page.Comments[k-1].Cursor()
		}
		return read, codeChangeComments{Repo: g.repo.Name, Change: g.ref(), Comments: rows[:k], Next: next,
			Position: page.Position}
	}
	return g.answerPage(ctx, len(rows), build, "comment")
}

// answerPage is a page of n rows of one kind — reviews, comments — cut
// whole-row at one tool answer, or refused where its first row alone is past
// one: what one row holds is its writer's, and no argument narrows it.
func (g changeRead) answerPage(ctx context.Context, n int, build func(k int) (*foreignRead, any),
	kind string) (tools.Result, error) {

	k, rendered, err := fitLabelled(n, build)
	switch {
	case err != nil:
		return labelFault(ctx, g.tool, err), nil
	case k == 0 && n > 0:
		return failed(fmt.Sprintf("%s: the next %s of %s alone is past one answer — it spells that many "+
			"characters an answer escapes — so it cannot be read through %s.", g.tool, kind, g.ref(), g.tool)), nil
	}
	return compactAnswer(rendered, fmt.Sprintf("Read the %ss a page at a time with `cursor`.", kind))
}

// fitLabelled is the longest prefix of n rows whose answer fits one tool
// answer, with that answer rendered as it is sent — n where the whole page
// does — for a page whose rows carry fenced text: build makes the answer
// holding the first k rows AND the read that labels it, afresh for each
// probe, so a probe's fences and its envelope are of the rows it holds alone.
// A fence registered for a row a probe leaves out would be one no field of it
// holds, which rendering refuses ([foreignRead.render]'s fail-closed rule).
//
// MEASURED AS SENT, as [foreignRead.fit] is — each probe rendered the way the
// answer goes out — by a search over the prefixes, since an answer only grows
// with its rows.
func fitLabelled(n int, build func(k int) (*foreignRead, any)) (int, json.RawMessage, error) {
	render := func(k int) (json.RawMessage, error) {
		read, v := build(k)
		return read.render(v)
	}
	whole, err := render(n)
	if err != nil || len(whole) <= ToolAnswerBytes {
		return n, whole, err
	}
	fits, kept := 0, json.RawMessage(nil)
	if kept, err = render(0); err != nil {
		return 0, nil, err
	}
	for over := n; over-fits > 1; {
		mid := fits + (over-fits)/2
		rendered, err := render(mid)
		if err != nil {
			return 0, nil, err
		}
		if len(rendered) <= ToolAnswerBytes {
			fits, kept = mid, rendered
		} else {
			over = mid
		}
	}
	return fits, kept, nil
}
