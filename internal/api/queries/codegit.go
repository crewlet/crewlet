package queries

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REPOSITORY'S FILES, HISTORY AND DIFFS, AS QUESTIONS: `code_refs`, its live
// refs under a prefix (GET /code/repos/{name}/refs, the reader's `refs`);
// `code_tree` and `code_blob`, a directory and a file at a revision (…/tree,
// …/blob); `code_log` and `code_blame`, a path's history and who last changed
// each line of a file (…/log, …/blame); `code_diff` and `code_compare`, what
// changed between two revisions and how they relate, with what merging the
// one into the other would do (…/diff, …/compare); and `code_commit`, one
// commit with the push that introduced it and the changes it belongs to
// (…/commits/{sha}). Each is decided per repository by the grant the serving
// node derives, as every code question is (code.go), and read at the
// caller's freshness.
//
// # Read by object id, of the repository's home
//
// A tree, a file, a history, a blame and a diff are the repository's CONTENT,
// answered by its home git node (estate.CodeSeam.GitRead, ADR-0082) and never
// by the rows — the code tools' own reads, one implementation. A revision a
// request names by a ref — a branch, a tag, a whole ref, or `HEAD` for the
// default branch — is resolved first, at the caller's freshness, in ONE read
// however many it names (code.resolve_ref), and the git node is asked at the
// floor of the records that last moved those refs (code.RefsFloor), so the
// copy that answers holds what the rows named. An object id passes with no
// read of a ref.
//
// # An answer by whole object ids never changes
//
// A question every revision of which is a WHOLE object id is answered
// [Immutable] — a strong entity tag and `Cache-Control: immutable` over REST
// — so a screen paging back through a file or a diff costs the node nothing
// to send twice. That holds only of a page that is a function of what the
// request named, so such a read asks its git node for the page alone
// (code.GitQuery.Unstamped): cut against a room no stamp of the answering
// node takes, and answered with none — no repository id, no copy position, no
// push behind each commit — which would each move between two reads of one
// request. A revision named by a ref moves with the ref, and an abbreviation
// with whichever object it finds now, so a request naming either is answered
// bare, and carries what the read stamped: the refs it resolved, the copy's
// position and the push behind each commit, where the node's index holds it.
// A commit's provenance is `code_commit`'s, which is never immutable: the
// changes a commit belongs to move.

// ReasonNoRef is the reason a git question's `not_found` names beside the
// code questions' own and [ReasonNoObject]: a ref the request named is no
// live ref of the repository — every one named in the sentence.
const ReasonNoRef = "no_ref"

// headRev names the repository's default branch, as the code tools take it.
const headRev = "HEAD"

// CodePage is a read by object id as a code question answers it: the
// repository by the name the request asked by, the kind, the object a tree or
// a file was read at as its whole id, the cursor of the next page and the
// kind's own part — and, on a read naming a ref or an abbreviation, which is
// never immutable, what the read stamped: every ref it resolved by the
// parameter that named it, the answering copy's position, and the push that
// introduced each commit the page names with what the node's index covers.
type CodePage struct {
	Repo  string            `json:"repo"`
	Kind  code.GitReadKind  `json:"kind"`
	Rev   string            `json:"rev,omitempty"`
	Next  string            `json:"next,omitempty"`
	Tree  *code.TreeAnswer  `json:"tree,omitempty"`
	Blob  *code.BlobAnswer  `json:"blob,omitempty"`
	Diff  *code.DiffAnswer  `json:"diff,omitempty"`
	Log   *code.LogAnswer   `json:"log,omitempty"`
	Blame *code.BlameAnswer `json:"blame,omitempty"`
	// Compare is a comparison's own part, beside its merge preview
	// ([CodeComparison]).
	Compare *code.CompareAnswer `json:"compare,omitempty"`

	Resolved map[string]CodeResolved  `json:"resolved,omitempty"`
	Position statelog.Position        `json:"position,omitzero"`
	PushedBy map[string]code.PushedBy `json:"pushed_by,omitempty"`
	Coverage *code.Coverage           `json:"coverage,omitempty"`
}

// CodeResolved is a ref a request's revision named, as it was resolved: its
// whole name, the object it pointed at, and the position of the record that
// last moved it — what the read by object id was floored at.
type CodeResolved struct {
	Ref   string            `json:"ref"`
	OID   string            `json:"oid"`
	Moved statelog.Position `json:"moved,omitzero"`
}

// CodeComparison is `code_compare`'s answer: how two revisions relate, and
// what merging head into base would do, as the repository's home merges it —
// the merge a change's mergeability is recorded by (code.merge_preview).
type CodeComparison struct {
	CodePage
	Preview CodeMergePreview `json:"preview"`
}

// CodeMergePreview is a comparison's merge preview: its state (`clean`,
// `conflict`, or `unknown` for a merge its home could not decide within a
// read's time), the conflicted paths and how many more there were.
type CodeMergePreview struct {
	State            code.MergeState `json:"state"`
	Conflicts        []string        `json:"conflicts,omitempty"`
	ConflictsOmitted int64           `json:"conflicts_omitted,omitempty"`
}

// CodeCommit is `code_commit`'s answer: one commit as the repository's history
// lists it, the push that introduced it — by the git node's index, and its row
// whole where the push history still holds it — and the changes it belongs to,
// read from the rows at the caller's freshness.
type CodeCommit struct {
	Repo   string         `json:"repo"`
	Commit code.LogCommit `json:"commit"`
	// PushedBy is the push that introduced the commit, where the answering
	// node's index holds it (Coverage says what it covers), and Push that
	// push's row — who pushed it, the run behind a seat's push and every ref
	// it moved — where the repository's push history still holds it.
	PushedBy *code.PushedBy `json:"pushed_by,omitempty"`
	Push     *code.PushRow  `json:"push,omitempty"`
	Coverage *code.Coverage `json:"coverage,omitempty"`
	// Merged, Revisions and More are the changes it belongs to, as the
	// reader's `commit` answers them (code.CommitChanges).
	Merged    []code.ChangeName     `json:"merged,omitempty"`
	Revisions []code.CommitRevision `json:"revisions,omitempty"`
	More      bool                  `json:"more,omitempty"`
	Position  statelog.Position     `json:"position"`
	Level     statelog.ReadLevel    `json:"level"`
}

// gitRev is one revision a request names: the parameter that named it, its
// spelling, and — once resolved — its object id and the ref it resolved
// through, none for an object id.
type gitRev struct {
	param, spelled string
	oid            string
	ref            *code.ResolvedRef
}

// whole reports a revision named by a whole object id: the one spelling whose
// object no later push or object can change.
func (r *gitRev) whole() bool { return gitref.ValidOID(r.spelled) && r.spelled != code.ZeroOID }

// revParam is the revision a request names in param, `HEAD` — the default
// branch — where it names none and dflt says so, and a refusal naming param
// where it must name one.
func revParam(p Params, param string, dflt bool) (*gitRev, error) {
	rev := strings.TrimSpace(p.String(param))
	switch {
	case rev == "" && dflt:
		rev = headRev
	case rev == "":
		return nil, fmt.Errorf("%w: %s is required: a commit id (whole, or its first %d or more hex digits), "+
			"or a branch, a tag, a whole ref, or %s for the default branch", ErrBadParams, param,
			code.MinAbbrevDigits, headRev)
	}
	return &gitRev{param: param, spelled: rev}, nil
}

// gitAsk is one git question as its caller asks it: the code reads, who asks,
// the repository by name, and the caller's freshness.
type gitAsk struct {
	reads CodeReads
	asker code.Principal
	name  string
	fresh statelog.Freshness
}

// gitAskOf is the git question p asks as the caller, the source asked first
// so a node that serves no code host says why whatever the request named.
func (s Sources) gitAskOf(ctx context.Context, p Params, what string) (gitAsk, error) {
	reads, err := s.Code.CodeReads()
	if err != nil {
		return gitAsk{}, err
	}
	principal, err := caller(ctx, what)
	if err != nil {
		return gitAsk{}, err
	}
	fresh, err := Freshness(p)
	if err != nil {
		return gitAsk{}, err
	}
	return gitAsk{reads: reads, asker: codeAsker(principal), name: p.String("name"), fresh: fresh}, nil
}

// resolve is the repository the question names and every revision of revs as
// an object id: an object id passes as it is; every other is a ref, and all
// of them are resolved in ONE read of their refs at the caller's freshness —
// `HEAD` as the default branch, which a read of the repository names first,
// every later read at `session` floored at that read's position. Where no
// revision names a ref the repository is read alone, for its id. A name no
// live ref holds is [NotFound] (`no_ref`), every one of them named.
func (g gitAsk) resolve(ctx context.Context, revs ...*gitRev) (code.RepoRef, error) {
	var (
		repo  code.RepoRef
		names []string
		head  bool
	)
	for _, r := range revs {
		switch {
		case code.ValidRev(r.spelled):
			r.oid = r.spelled
		case r.spelled == headRev:
			head = true
		case !slices.Contains(names, r.spelled):
			names = append(names, r.spelled)
		}
	}
	fresh := g.fresh
	if head || len(names) == 0 {
		a, err := g.reads.Read(ctx, code.ReadQuery{Kind: code.ReadRepo, Repo: g.name, Asker: g.asker, Fresh: fresh})
		if err != nil {
			return code.RepoRef{}, codeRefusal(g.name, err)
		}
		if a.Repo == nil {
			return code.RepoRef{}, fmt.Errorf("queries: the code host answered repository %s's read with no "+
				"detail", g.name)
		}
		repo, fresh = a.Repo.RepoRef, statelog.FloorAt(statelog.ReadSession, a.Position)
		if head && !slices.Contains(names, repo.DefaultBranch) {
			names = append(names, repo.DefaultBranch)
		}
	}
	if len(names) == 0 {
		return repo, nil
	}
	a, err := g.reads.ResolveRef(ctx, code.RefQuery{Repo: g.name, Refs: names, Fresh: fresh, Asker: g.asker})
	if err != nil {
		return code.RepoRef{}, codeRefusal(g.name, err)
	}
	if err := a.Err(); err != nil {
		return code.RepoRef{}, codeRefusal(g.name, err)
	}
	if len(a.Refs) != len(names) {
		return code.RepoRef{}, fmt.Errorf("queries: the code host answered %d of the %d refs asked of %s",
			len(a.Refs), len(names), g.name)
	}
	// A NAME RELEASED AND TAKEN AGAIN between the read of the repository and
	// the read of its refs names two repositories: nothing read of the one is
	// an answer about the other, and the same question asked again reads one.
	if repo.ID != "" && a.Repo.ID != repo.ID {
		return code.RepoRef{}, fmt.Errorf("%w: repository %q was deleted and declared again while it was read: "+
			"ask again", ErrUnavailable, g.name)
	}
	for _, r := range revs {
		if r.oid != "" {
			continue
		}
		name := r.spelled
		if name == headRev {
			name = a.Repo.DefaultBranch
		}
		ref := a.Refs[slices.Index(names, name)]
		r.oid, r.ref = ref.OID, &ref
	}
	return a.Repo, nil
}

// floor is what a read by object id of repo's revs is asked at: the furthest
// of this node's own writes to the repository, the caller's own floor on the
// code log, and the record that last moved each ref a revision resolved
// through (code.RefsFloor) — so the copy that answers holds what the caller
// wrote and what the refs named — or none, which reads the copy as it stands:
// an object id names one object at every position.
func (g gitAsk) floor(repo code.RepoRef, revs ...*gitRev) statelog.Freshness {
	var refs []code.ResolvedRef
	for _, r := range revs {
		if r.ref != nil {
			refs = append(refs, *r.ref)
		}
	}
	own := statelog.Later(g.reads.FloorOf(repo.ID), g.fresh.Floor(topics.CodeLogStream))
	return code.RefsFloor(own, refs...)
}

// read asks q of repo's home git node as the caller, at the floor of revs —
// UNSTAMPED where every revision is a whole object id, and then answered
// [Immutable] — refused here first where q is one no git node would take
// ([ErrBadParams] in the domain's own words), and answered in this surface's
// vocabulary ([codeRefusal]).
func (g gitAsk) read(ctx context.Context, repo code.RepoRef, q code.GitQuery, cursor string,
	revs ...*gitRev) (CodePage, bool, error) {

	immutable := true
	for _, r := range revs {
		immutable = immutable && r.whole()
	}
	q.RepoID, q.Asker, q.Fresh, q.Cursor, q.Unstamped = repo.ID, g.asker, g.floor(repo, revs...), cursor, immutable
	if err := q.Validate(); err != nil {
		return CodePage{}, false, fmt.Errorf("%w: %w", ErrBadParams, err)
	}
	a, err := g.reads.GitRead(ctx, q)
	if err != nil {
		return CodePage{}, false, codeRefusal(g.name, err)
	}
	page := CodePage{Repo: g.name, Kind: a.Kind, Rev: a.Rev, Next: a.Next, Tree: a.Tree, Blob: a.Blob, Diff: a.Diff,
		Log: a.Log, Blame: a.Blame, Compare: a.Compare}
	if immutable {
		return page, true, nil
	}
	page.Position, page.PushedBy, page.Coverage = a.Position, a.PushedBy, a.Coverage
	for _, r := range revs {
		if r.ref == nil {
			continue
		}
		if page.Resolved == nil {
			page.Resolved = map[string]CodeResolved{}
		}
		page.Resolved[r.param] = CodeResolved{Ref: r.ref.Ref, OID: r.ref.OID, Moved: r.ref.Moved}
	}
	return page, false, nil
}

// answered is page as a question answers it: [Immutable] where it was read by
// whole object ids alone, bare otherwise.
func answered(page any, immutable bool) any {
	if immutable {
		return Immutable{Answer: page}
	}
	return page
}

// pathParams is the path a request names — `path`, or `path_b64` for one that
// is not valid UTF-8 — refused where it names both, as the git node would.
func pathParams(p Params) (path, b64 string, err error) {
	path, b64 = p.String("path"), p.String("path_b64")
	if path != "" && b64 != "" {
		return "", "", fmt.Errorf("%w: path and path_b64 name one path two ways: name it once", ErrBadParams)
	}
	return path, b64, nil
}

// lineParams is a request's `start_line` and `end_line`, each a whole number
// of one or more where it names one.
func lineParams(p Params) (start, end int, err error) {
	for _, l := range []struct {
		key string
		to  *int
	}{{"start_line", &start}, {"end_line", &end}} {
		if !p.Has(l.key) {
			continue
		}
		n, whole := p.WholeInt(l.key)
		if !whole || n < 1 {
			return 0, 0, fmt.Errorf("%w: %s %q is not a line's number: a file numbers its lines from 1",
				ErrBadParams, l.key, p.String(l.key))
		}
		*l.to = int(n)
	}
	return start, end, nil
}

// boolParam is a request's flag key, def where it names none, refused where it
// names anything but `true` or `false`.
func boolParam(p Params, key string, def bool) (bool, error) {
	switch v := strings.TrimSpace(p.String(key)); v {
	case "":
		return def, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, badParams(key, v, []string{"true", "false"})
	}
}

// codeTree answers `code_tree`: a directory's entries at `ref` (the default
// branch where it names none) — its root, or `path` — or with `recursive`
// every file below it, a page at a time (`cursor`).
func (s Sources) codeTree(ctx context.Context, p Params) (any, error) {
	g, err := s.gitAskOf(ctx, p, "code_tree")
	if err != nil {
		return nil, err
	}
	rev, err := revParam(p, "ref", true)
	if err != nil {
		return nil, err
	}
	path, b64, err := pathParams(p)
	if err != nil {
		return nil, err
	}
	recursive, err := boolParam(p, "recursive", false)
	if err != nil {
		return nil, err
	}
	repo, err := g.resolve(ctx, rev)
	if err != nil {
		return nil, err
	}
	page, immutable, err := g.read(ctx, repo, code.GitQuery{Kind: code.GitTree, Tree: &code.TreeQuery{Rev: rev.oid,
		Path: path, PathB64: b64, Recursive: recursive}}, p.String("cursor"), rev)
	if err != nil {
		return nil, err
	}
	return answered(page, immutable), nil
}

// codeBlob answers `code_blob`: a file's lines at `ref` and `path` — or a
// blob by its own id with no path — `start_line` to `end_line`, a page at a
// time; a path naming a directory answers its listing.
func (s Sources) codeBlob(ctx context.Context, p Params) (any, error) {
	g, err := s.gitAskOf(ctx, p, "code_blob")
	if err != nil {
		return nil, err
	}
	rev, err := revParam(p, "ref", true)
	if err != nil {
		return nil, err
	}
	path, b64, err := pathParams(p)
	if err != nil {
		return nil, err
	}
	start, end, err := lineParams(p)
	if err != nil {
		return nil, err
	}
	repo, err := g.resolve(ctx, rev)
	if err != nil {
		return nil, err
	}
	page, immutable, err := g.read(ctx, repo, code.GitQuery{Kind: code.GitBlob, Blob: &code.BlobQuery{Rev: rev.oid,
		Path: path, PathB64: b64, StartLine: start, EndLine: end}}, p.String("cursor"), rev)
	if err != nil {
		return nil, err
	}
	return answered(page, immutable), nil
}

// codeLog answers `code_log`: the commits that changed `path` — or every
// commit — newest first from `ref`, `limit` a page (1 to code.MaxLogLimit,
// code.DefaultLogLimit where it names none), kept to those whose message
// holds `grep` (a literal) and committed at or after `since` (RFC 3339).
func (s Sources) codeLog(ctx context.Context, p Params) (any, error) {
	g, err := s.gitAskOf(ctx, p, "code_log")
	if err != nil {
		return nil, err
	}
	rev, err := revParam(p, "ref", true)
	if err != nil {
		return nil, err
	}
	path, b64, err := pathParams(p)
	if err != nil {
		return nil, err
	}
	l := code.LogQuery{Path: path, PathB64: b64, Grep: p.String("grep")}
	if since := strings.TrimSpace(p.String("since")); since != "" {
		if l.Since, err = time.Parse(time.RFC3339Nano, since); err != nil {
			return nil, fmt.Errorf("%w: since %q is not an RFC 3339 instant", ErrBadParams, since)
		}
	}
	if p.Has("limit") {
		n, whole := p.WholeInt("limit")
		if !whole || n < 0 {
			return nil, fmt.Errorf("%w: the limit %q is not a whole number of zero or more (0 is the history's "+
				"own page of %d)", ErrBadParams, p.String("limit"), code.DefaultLogLimit)
		}
		l.Limit = int(n)
	}
	repo, err := g.resolve(ctx, rev)
	if err != nil {
		return nil, err
	}
	l.Rev = rev.oid
	page, immutable, err := g.read(ctx, repo, code.GitQuery{Kind: code.GitLog, Log: &l}, p.String("cursor"), rev)
	if err != nil {
		return nil, err
	}
	return answered(page, immutable), nil
}

// codeBlame answers `code_blame`: which commit last changed each line of the
// file `path` at `ref`, `start_line` to `end_line` — at most
// code.MaxBlameLines lines — a page at a time.
func (s Sources) codeBlame(ctx context.Context, p Params) (any, error) {
	g, err := s.gitAskOf(ctx, p, "code_blame")
	if err != nil {
		return nil, err
	}
	rev, err := revParam(p, "ref", true)
	if err != nil {
		return nil, err
	}
	path, b64, err := pathParams(p)
	if err != nil {
		return nil, err
	}
	if path == "" && b64 == "" {
		return nil, badParams("path", "", nil)
	}
	start, end, err := lineParams(p)
	if err != nil {
		return nil, err
	}
	repo, err := g.resolve(ctx, rev)
	if err != nil {
		return nil, err
	}
	page, immutable, err := g.read(ctx, repo, code.GitQuery{Kind: code.GitBlame, Blame: &code.BlameQuery{
		Rev: rev.oid, Path: path, PathB64: b64, StartLine: start, EndLine: end}}, p.String("cursor"), rev)
	if err != nil {
		return nil, err
	}
	return answered(page, immutable), nil
}

// codeDiff answers `code_diff`: what changed from `base` to `head` — the
// per-file stat, narrowed to `paths` (a file, or a directory it is under; at
// most code.MaxDiffPaths), or one `file`'s patch (`file_b64` for a path's
// exact bytes) — with renames detected unless `renames` is false, a page at a
// time.
func (s Sources) codeDiff(ctx context.Context, p Params) (any, error) {
	g, err := s.gitAskOf(ctx, p, "code_diff")
	if err != nil {
		return nil, err
	}
	base, err := revParam(p, "base", false)
	if err != nil {
		return nil, err
	}
	head, err := revParam(p, "head", false)
	if err != nil {
		return nil, err
	}
	d := code.DiffQuery{File: p.String("file"), FileB64: p.String("file_b64")}
	if d.Paths, _ = p.List("paths"); len(d.Paths) > code.MaxDiffPaths {
		return nil, fmt.Errorf("%w: paths names %d paths, past the %d one diff is narrowed to", ErrBadParams,
			len(d.Paths), code.MaxDiffPaths)
	}
	if d.Renames, err = boolParam(p, "renames", true); err != nil {
		return nil, err
	}
	repo, err := g.resolve(ctx, base, head)
	if err != nil {
		return nil, err
	}
	d.Base, d.Head = base.oid, head.oid
	page, immutable, err := g.read(ctx, repo, code.GitQuery{Kind: code.GitDiff, Diff: &d}, p.String("cursor"), base,
		head)
	if err != nil {
		return nil, err
	}
	return answered(page, immutable), nil
}

// codeCompare answers `code_compare`: how `base` and `head` relate — their
// merge base, which contains which, the commits each has that the other
// lacks, and the first page of what head changed since the merge base — and
// what merging head into base would do, asked of the repository's home git
// node by the two commits' whole ids the comparison answered
// (code.merge_preview), so the preview merges exactly the commits compared.
// Immutable only where both are whole ids and the merge was decided: a merge
// its home could not decide in a read's time answers `unknown`, which a later
// read may decide.
func (s Sources) codeCompare(ctx context.Context, p Params) (any, error) {
	g, err := s.gitAskOf(ctx, p, "code_compare")
	if err != nil {
		return nil, err
	}
	base, err := revParam(p, "base", false)
	if err != nil {
		return nil, err
	}
	head, err := revParam(p, "head", false)
	if err != nil {
		return nil, err
	}
	repo, err := g.resolve(ctx, base, head)
	if err != nil {
		return nil, err
	}
	page, immutable, err := g.read(ctx, repo, code.GitQuery{Kind: code.GitCompare, Compare: &code.CompareQuery{
		Base: base.oid, Head: head.oid}}, "", base, head)
	if err != nil {
		return nil, err
	}
	if page.Compare == nil {
		return nil, fmt.Errorf("queries: the code host answered %s's comparison with no comparison", g.name)
	}
	preview, err := g.reads.MergePreview(ctx, code.MergePreviewQuery{RepoID: repo.ID, Asker: g.asker,
		Base: page.Compare.Base, Head: page.Compare.Head, Fresh: g.floor(repo, base, head)})
	if err != nil {
		return nil, codeRefusal(g.name, err)
	}
	out := CodeComparison{CodePage: page, Preview: CodeMergePreview{State: preview.State,
		Conflicts: preview.Conflicts, ConflictsOmitted: preview.ConflictsOmitted}}
	return answered(out, immutable && preview.State != code.MergeUnknown), nil
}

// codeCommit answers `code_commit`: the commit `sha` — a WHOLE id, which is
// what a commit's page is linked by; an abbreviation is expanded by asking
// `code_log` with it as `ref` and a `limit` of 1 — as the repository's history
// lists it, the push that introduced it, and every change it belongs to. The
// rows first, at the caller's freshness — the repository and the commit's
// changes, in one snapshot (the reader's `commit`) — then the commit, asked of
// the repository's home git node, and the push the node's index names,
// read whole at `session` floored at the later of the rows' read and the
// push's own record.
func (s Sources) codeCommit(ctx context.Context, p Params) (any, error) {
	g, err := s.gitAskOf(ctx, p, "code_commit")
	if err != nil {
		return nil, err
	}
	sha := strings.TrimSpace(p.String("sha"))
	if !gitref.ValidOID(sha) || sha == code.ZeroOID {
		return nil, fmt.Errorf("%w: sha %q is not a whole commit id: a commit's page names its commit whole, and "+
			"code_log with it as ref and a limit of 1 expands an abbreviation", ErrBadParams, sha)
	}
	rows, err := g.reads.Read(ctx, code.ReadQuery{Kind: code.ReadCommit, Repo: g.name, Commit: sha, Asker: g.asker,
		Fresh: g.fresh})
	if err != nil {
		return nil, codeRefusal(g.name, err)
	}
	if rows.Repo == nil || rows.Commit == nil {
		return nil, fmt.Errorf("queries: the code host answered commit %s of %s with no repository or changes",
			sha, g.name)
	}
	repo := rows.Repo.RepoRef
	q := code.GitQuery{Kind: code.GitLog, RepoID: repo.ID, Asker: g.asker, Fresh: g.floor(repo),
		Log: &code.LogQuery{Rev: sha, Limit: 1}}
	a, err := g.reads.GitRead(ctx, q)
	if err != nil {
		return nil, codeRefusal(g.name, err)
	}
	if a.Log == nil || len(a.Log.Commits) == 0 || a.Log.Commits[0].OID != sha {
		return nil, fmt.Errorf("%w: %s names no commit of repository %q: a tag's or a tree's id is not a "+
			"commit's", ErrBadParams, sha, g.name)
	}
	out := CodeCommit{Repo: g.name, Commit: a.Log.Commits[0], Coverage: a.Coverage, Merged: rows.Commit.Merged,
		Revisions: rows.Commit.Revisions, More: rows.Commit.More, Position: rows.Position, Level: rows.Level}
	by, pushed := a.PushedBy[sha]
	if !pushed {
		return out, nil
	}
	out.PushedBy = &by
	pushes, err := g.reads.Read(ctx, code.ReadQuery{Kind: code.ReadPushes, Repo: g.name, Push: by.PushID,
		Asker: g.asker, Fresh: statelog.FloorAt(statelog.ReadSession, statelog.Later(rows.Position, by.Position))})
	if err != nil {
		return nil, codeRefusal(g.name, err)
	}
	if len(pushes.Pushes) == 1 {
		out.Push = &pushes.Pushes[0]
	}
	return out, nil
}
