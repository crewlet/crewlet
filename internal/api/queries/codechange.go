package queries

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHANGE'S ROWS AND ITS ROUTING, AS QUESTIONS: `code_changes`, a listing of
// changes across repositories or of one (GET /code/changes); `code_change`,
// one change a part at a time (GET /code/repos/{name}/changes/{n}) — its rows,
// a revision's files and one file's patch, and its findings each placed in the
// revision asked about; `code_change_routing`, whom its records reached
// (…/routing); and `code_reviews`, the reviews one reviewer gave
// (GET /code/reviews). Each is decided per repository by the grant the
// serving node derives (code.GrantOf, ADR-0081), as every code question is,
// and read at the caller's own freshness: a screen reads a change back at
// `session` beside the position its write answered with.
//
// # A revision's code is read by object id, by the code domain's one rule
//
// A change's files, a file's patch and where a finding sits now are reads of
// the repository's CONTENT, answered by its home git node, never by the rows.
// What they read between — the base a revision's facts record, the interdiff
// of two revisions, the re-anchoring of a page of findings — and the floor
// they are asked at are the code domain's rule (code.RevisionDiff,
// code.PlaceFindings, code.ObjectFloor), which the code tools' code_get_change
// reads by too: a seat and a screen are shown one diff of one revision and
// one finding on one line. The rows those reads name are read first, at the
// caller's freshness, and every later read of the same answer at `session`
// floored at the first's position, so the answer is never assembled from an
// older copy than its first read saw.

// The reasons a change question's `not_found` names beside the code
// questions' own ([ReasonNotDeclared], [ReasonNotHosted]), each remedied
// differently:
//
//   - ReasonNoChange — the repository holds no change of that number;
//   - ReasonNoRevision — the change holds no revision of that number;
//   - the files gaps (code.FilesUnrecorded, code.FilesUnrelated) — the
//     revision's files cannot be read from its facts, which no wait clears;
//   - ReasonNoObject — a read by object id names an object the repository
//     holds no object of, or a path its revision holds no such directory or
//     file at, in the git node's own sentence.
//
// A revision whose facts its home has not recorded YET (code.FilesPending)
// is no absence: it is [ErrUnavailable], which a wait clears — and so is a
// read by object id no git node could answer in time.
const (
	ReasonNoChange   = "no_change"
	ReasonNoRevision = "no_revision"
	ReasonNoObject   = "no_object"
)

// codeChangePart is a part of one change the `code_change` question answers:
// the reader's own parts (code.ChangeParts) — its revisions with its detail
// by default — and the three read by object id: `files`, `diff` and
// `findings`, its findings placed in the revision asked about.
type codeChangePart string

const (
	partFiles    codeChangePart = "files"
	partDiff     codeChangePart = "diff"
	partFindings codeChangePart = "findings"
)

// codeChangeParts is every part the question answers, the reader's first.
func codeChangeParts() []string {
	var out []string
	for _, p := range code.ChangeParts() {
		out = append(out, string(p))
	}
	return append(out, string(partFiles), string(partDiff))
}

// CodeChangeFiles is the `files` and `diff` parts' answer: a revision's files
// or one file's patch — from the base its facts record, or since an earlier
// revision the interdiff of the two — as the repository's home git node read
// it, beside the change and the revisions it names.
type CodeChangeFiles struct {
	Repo          string `json:"repo"`
	Change        string `json:"change"`
	Revision      int64  `json:"revision"`
	SinceRevision int64  `json:"since_revision,omitempty"`
	code.GitAnswer
}

// CodeChangeFindings is the `findings` part's answer: a page of the change's
// findings as the reader serves it, and where each is in the revision asked
// about (code.FindingPlace), by the finding's id — none for a finding on the
// old side of a revision whose base its home has not recorded yet.
type CodeChangeFindings struct {
	code.ReadAnswer
	Revision int64                        `json:"revision"`
	Placed   map[string]code.FindingPlace `json:"placed"`
}

// changeCall is one `code_change` question as its parameters ask it, read and
// checked before anything is: the part, the revision read at (0 for the
// change's head), the one changes are read since, and the file.
type changeCall struct {
	part          string
	revision      int64
	since         int64
	file, fileB64 string
}

// changeCallOf reads a call's part and the parameters that go with it, or
// refuses one its part does not take — [ErrBadParams] naming it, before
// anything is read.
func changeCallOf(p Params) (changeCall, error) {
	call := changeCall{part: strings.TrimSpace(p.String("part")), file: p.String("file"),
		fileB64: p.String("file_b64")}
	for _, n := range []struct {
		key string
		to  *int64
	}{{"revision", &call.revision}, {"since_revision", &call.since}} {
		if !p.Has(n.key) {
			continue
		}
		v, whole := p.WholeInt(n.key)
		if !whole || v < 1 {
			return changeCall{}, fmt.Errorf("%w: %s %q is not a revision's number: a change numbers its "+
				"revisions from 1", ErrBadParams, n.key, p.String(n.key))
		}
		*n.to = v
	}
	takes := func(ps ...codeChangePart) bool { return slices.Contains(ps, codeChangePart(call.part)) }
	switch {
	case call.part != "" && !slices.Contains(codeChangeParts(), call.part):
		return changeCall{}, badParams("part", call.part, codeChangeParts())
	case call.revision != 0 && !takes(partFiles, partDiff, partFindings):
		return changeCall{}, fmt.Errorf("%w: revision chooses what files, diff and findings read at", ErrBadParams)
	case call.since != 0 && !takes(partFiles, partDiff):
		return changeCall{}, fmt.Errorf("%w: since_revision reads what files and diff changed since a revision",
			ErrBadParams)
	case call.since != 0 && call.revision != 0 && call.since >= call.revision:
		return changeCall{}, fmt.Errorf("%w: since_revision %d is not before revision %d", ErrBadParams,
			call.since, call.revision)
	case (call.file != "" || call.fileB64 != "") && !takes(partDiff):
		return changeCall{}, fmt.Errorf("%w: file names the file whose patch diff reads; files lists every file",
			ErrBadParams)
	case call.file != "" && call.fileB64 != "":
		return changeCall{}, fmt.Errorf("%w: file and file_b64 name one file two ways: name it once",
			ErrBadParams)
	case takes(partDiff) && call.file == "" && call.fileB64 == "":
		return changeCall{}, badParams("file", "", nil)
	}
	return call, nil
}

// codeChange answers `code_change`: one change by its number on the path's
// repository, a part at a time ([codeChangePart]) — a part the reader serves
// read as the caller asked it, and a part read by object id assembled from the
// rows it names ([changeAsk.files], [changeAsk.findings]).
func (s Sources) codeChange(ctx context.Context, p Params) (any, error) {
	const what = "code_change"
	reads, err := s.Code.CodeReads()
	if err != nil {
		return nil, err
	}
	principal, err := caller(ctx, what)
	if err != nil {
		return nil, err
	}
	call, err := changeCallOf(p)
	if err != nil {
		return nil, err
	}
	q, err := codeReadQuery(code.ReadChange, codeAsker(principal), p)
	if err != nil {
		return nil, err
	}
	g := changeAsk{reads: reads, q: q}
	switch codeChangePart(call.part) {
	case partFiles, partDiff:
		return g.files(ctx, call)
	case partFindings:
		return g.findings(ctx, call)
	}
	q.Part = code.ChangePart(call.part)
	answer, err := reads.Read(ctx, q)
	if err != nil {
		return nil, codeRefusal(q.Repo, err)
	}
	return answer, nil
}

// changeAsk is one `code_change` question read by object id: the reads, and
// the caller's read of the change as its parameters ask it.
type changeAsk struct {
	reads CodeReads
	q     code.ReadQuery
}

// ref is the change as a person names it, web!42.
func (g changeAsk) ref() string { return code.ChangeRef(g.q.Repo, g.q.Change) }

// head is the change in detail and its head revision — its first page of
// revisions, one revision long — read at the caller's freshness, and the
// position the read was answered at, which every later read of the answer is
// floored at.
func (g changeAsk) head(ctx context.Context) (*code.ChangeDetail, code.RevisionRow, statelog.Position, error) {
	q := g.q
	q.Limit, q.Cursor = 1, ""
	answer, err := g.reads.Read(ctx, q)
	if err != nil {
		return nil, code.RevisionRow{}, statelog.Position{}, codeRefusal(q.Repo, err)
	}
	if answer.Change == nil || len(answer.Revisions) != 1 {
		return nil, code.RevisionRow{}, statelog.Position{}, fmt.Errorf("queries: the code host answered %s's "+
			"first page with %d revisions and detail %t", g.ref(), len(answer.Revisions), answer.Change != nil)
	}
	return answer.Change, answer.Revisions[0], answer.Position, nil
}

// revision is revision n of a change whose latest is latest, read at session
// floored at after — refused [NotFound] where the change has fewer.
func (g changeAsk) revision(ctx context.Context, n, latest int64, after statelog.Position) (code.RevisionRow,
	error) {

	if n > latest {
		return code.RevisionRow{}, &NotFound{Reason: ReasonNoRevision, Detail: fmt.Sprintf("%s has %d "+
			"revision(s), numbered from 1, and no revision %d", g.ref(), latest, n)}
	}
	q := g.q
	q.Cursor, q.Limit, q.Fresh = code.RevisionRow{N: n + 1}.Cursor(), 1, statelog.FloorAt(statelog.ReadSession,
		after)
	answer, err := g.reads.Read(ctx, q)
	if err != nil {
		return code.RevisionRow{}, codeRefusal(q.Repo, err)
	}
	if len(answer.Revisions) != 1 || answer.Revisions[0].N != n {
		return code.RevisionRow{}, fmt.Errorf("queries: the code host answered %s's revision %d with %d "+
			"revisions", g.ref(), n, len(answer.Revisions))
	}
	return answer.Revisions[0], nil
}

// gap refuses a revision whose files cannot be read from its facts
// (code.RevisionRow.FilesGap): one its home has not measured yet is
// [ErrUnavailable], which a wait clears, and the rest are absences that name
// their reason.
func (g changeAsk) gap(d *code.ChangeDetail, r code.RevisionRow, based bool) error {
	switch gap := r.FilesGap(d.State, based); gap {
	case code.FilesPending:
		return fmt.Errorf("%w: revision %d of %s has no recorded base yet: the repository's home git node "+
			"records each revision's facts against its target within a minute of its push", ErrUnavailable, r.N,
			g.ref())
	case code.FilesUnrecorded:
		return &NotFound{Reason: string(gap), Detail: fmt.Sprintf("%s was %s before its home recorded revision "+
			"%d's base against its target, so it has none to read the revision's files from: its commits are "+
			"read from its head %s", g.ref(), d.State, r.N, r.Head)}
	case code.FilesUnrelated:
		return &NotFound{Reason: string(gap), Detail: fmt.Sprintf("revision %d of %s shares no history with %s, "+
			"so its change is every file of its head %s", r.N, g.ref(), r.Facts.TargetRef, r.Head)}
	}
	return nil
}

// files answers the `files` and `diff` parts: a revision's files or one file's
// patch from the base its facts record, or since an earlier revision the
// interdiff of the two (code.RevisionDiff), asked of the home git node at the
// floor of the records that moved the revisions (code.ObjectFloor).
func (g changeAsk) files(ctx context.Context, call changeCall) (any, error) {
	d, head, first, err := g.head(ctx)
	if err != nil {
		return nil, err
	}
	rev := head
	if call.revision != 0 && call.revision != head.N {
		if rev, err = g.revision(ctx, call.revision, d.Revision, first); err != nil {
			return nil, err
		}
	}
	revs := []code.RevisionRow{rev}
	var since *code.RevisionRow
	if call.since != 0 {
		if call.since >= rev.N {
			return nil, fmt.Errorf("%w: since_revision %d is not before revision %d, the one read", ErrBadParams,
				call.since, rev.N)
		}
		earlier, err := g.revision(ctx, call.since, d.Revision, first)
		if err != nil {
			return nil, err
		}
		revs, since = append(revs, earlier), &earlier
	}
	for _, r := range revs {
		if err := g.gap(d, r, since == nil); err != nil {
			return nil, err
		}
	}
	gq, err := code.RevisionDiff(d.RepoID, rev, since, call.file, call.fileB64)
	if err != nil {
		// UNREACHABLE past gap's refusals and changeCallOf's order of the two
		// revisions, so this node's own fault, never the caller's.
		return nil, err
	}
	gq.Asker, gq.Fresh, gq.Cursor = g.q.Asker, code.ObjectFloor(g.reads.FloorOf(d.RepoID), revs...), g.q.Cursor
	page, err := g.gitRead(ctx, gq)
	if err != nil {
		return nil, err
	}
	return CodeChangeFiles{Repo: g.q.Repo, Change: g.ref(), Revision: rev.N, SinceRevision: call.since,
		GitAnswer: page}, nil
}

// findings answers the `findings` part: a page of the change's findings as the
// reader serves it, read at session floored at the head's read, each placed in
// the revision asked about — the head by default — without a read where the
// rows say where it is, and by ONE re-anchoring of the revision otherwise
// (code.PlaceFindings), asked at the floor of the head revision and of the
// revision placed in.
func (g changeAsk) findings(ctx context.Context, call changeCall) (any, error) {
	d, head, first, err := g.head(ctx)
	if err != nil {
		return nil, err
	}
	rev := head
	if call.revision != 0 && call.revision != head.N {
		if rev, err = g.revision(ctx, call.revision, d.Revision, first); err != nil {
			return nil, err
		}
	}
	q := g.q
	q.Part, q.Fresh = code.PartFindings, statelog.FloorAt(statelog.ReadSession, first)
	page, err := g.reads.Read(ctx, q)
	if err != nil {
		return nil, codeRefusal(q.Repo, err)
	}
	placed, reanchor := code.PlaceFindings(rev, page.Findings)
	if reanchor != nil {
		answer, err := g.gitRead(ctx, code.GitQuery{Kind: code.GitReanchor, RepoID: d.RepoID, Asker: g.q.Asker,
			Fresh: code.ObjectFloor(g.reads.FloorOf(d.RepoID), head, rev), Reanchor: reanchor})
		if err != nil {
			return nil, err
		}
		// AN ANSWER PLACING ANOTHER SET THAN THE ANCHORS ASKED is no git
		// node's of this build: this node's fault, never a finding with
		// no place.
		if err := code.PlaceAnchors(placed, reanchor, answer.Reanchor); err != nil {
			return nil, fmt.Errorf("queries: %s: %w", g.ref(), err)
		}
	}
	return CodeChangeFindings{ReadAnswer: page, Revision: rev.N, Placed: placed}, nil
}

// gitRead asks q of the repository's home git node, refused here first where
// it is one no git node would take — the request's own mistake, in this
// surface's vocabulary — and answered in it ([codeRefusal]).
func (g changeAsk) gitRead(ctx context.Context, q code.GitQuery) (code.GitAnswer, error) {
	if err := q.Validate(); err != nil {
		return code.GitAnswer{}, fmt.Errorf("%w: %w", ErrBadParams, err)
	}
	answer, err := g.reads.GitRead(ctx, q)
	if err != nil {
		return code.GitAnswer{}, codeRefusal(g.q.Repo, err)
	}
	return answer, nil
}
