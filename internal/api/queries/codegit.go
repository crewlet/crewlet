package queries

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REPOSITORY'S FILES, HISTORY AND DIFFS, AS QUESTIONS: `code_refs` (live
// refs under a prefix), `code_tree`, `code_blob` and `code_find` (a revision's
// directory, file, and a file found by name), `code_log`, `code_blame` and
// `code_tree_commits` (history, who last changed each line, the commit that
// last changed each entry), `code_diff` and `code_compare`, `code_tips` (many
// heads and tags with their distance from a base), `code_tags` (newest first
// by their own date) and `code_commit` (one commit with its push and changes).
// Each is decided per repository by the serving node's grant, as every code
// question is (code.go), and read at the caller's freshness.

// Content is read by object id, of the repository's home: a tree, file,
// history, blame and diff are answered by its home Gitlet node
// (estate.CodeSeam.GitRead, ADR-0089), never by the rows. A revision named by
// a ref (branch, tag, whole ref, or `HEAD`) is resolved first in ONE read at
// the caller's freshness (code.resolve_ref), and the node is asked at the
// floor of the records that last moved those refs (gitlet.RefsFloor), so the
// answering copy holds what the rows named. An object id needs no ref read.

// An answer by whole object ids never changes: a question every revision of
// which is a WHOLE object id is answered [Immutable] (strong entity tag,
// `Cache-Control: immutable`), valid only of a page that is a function of what
// the request named, so such a read asks its node for the page alone
// (gitlet.GitQuery.Unstamped), carrying no repository id, copy position or
// push per commit, which would move between reads. A ref moves, and an
// abbreviation resolves to whatever it finds now, so a request naming either
// is answered bare with what the read stamped. A commit's provenance is
// `code_commit`'s, never immutable: its changes move.

// ReasonNoRef is the reason a git question's `not_found` names beside the
// code questions' own and [ReasonNoObject]: a ref the request named is no
// live ref of the repository — every one named in the sentence.
const ReasonNoRef = "no_ref"

// headRev names the repository's default branch, as the code tools take it
// and as a resolve of refs answers it (gitlet.HeadRef).
const headRev = gitlet.HeadRef

// CodePage is a read by object id as a code question answers it: the
// repository by the name the request asked by, the kind, the object a tree or
// a file was read at as its whole id, the cursor of the next page and the
// kind's own part — and, on a read naming a ref or an abbreviation, which is
// never immutable, what the read stamped: every ref it resolved by the
// parameter that named it, the answering copy's position, and the push that
// introduced each commit the page names with what the node's index covers,
// and the tags naming each commit a history's page names.
type CodePage struct {
	Repo  string              `json:"repo"`
	Kind  gitlet.GitReadKind  `json:"kind"`
	Rev   string              `json:"rev,omitempty"`
	Next  string              `json:"next,omitempty"`
	Tree  *gitlet.TreeAnswer  `json:"tree,omitempty"`
	Blob  *gitlet.BlobAnswer  `json:"blob,omitempty"`
	Diff  *gitlet.DiffAnswer  `json:"diff,omitempty"`
	Log   *gitlet.LogAnswer   `json:"log,omitempty"`
	Blame *gitlet.BlameAnswer `json:"blame,omitempty"`
	Find  *gitlet.FindAnswer  `json:"find,omitempty"`
	// Compare is a comparison's own part, beside its merge preview
	// ([CodeComparison]).
	Compare *gitlet.CompareAnswer `json:"compare,omitempty"`
	// Tips is a tips read's part (code_tips).
	Tips *gitlet.TipsAnswer `json:"tips,omitempty"`
	// Tags is a tags read's part (code_tags).
	Tags *gitlet.TagsAnswer `json:"tags,omitempty"`

	// TreeCommits is a directory's last commits' part (code_tree_commits).
	TreeCommits *gitlet.TreeCommitsAnswer `json:"tree_commits,omitempty"`

	Resolved map[string]CodeResolved    `json:"resolved,omitempty"`
	Position statelog.Position          `json:"position,omitzero"`
	PushedBy map[string]gitlet.PushedBy `json:"pushed_by,omitempty"`
	Coverage *gitlet.Coverage           `json:"coverage,omitempty"`
	// Tagged is the tags naming each commit a history's page names, where the
	// answering node's tag index holds them.
	Tagged *gitlet.TagMarks `json:"tagged,omitempty"`
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
	State            gitlet.MergeState `json:"state"`
	Conflicts        []string          `json:"conflicts,omitempty"`
	ConflictsOmitted int64             `json:"conflicts_omitted,omitempty"`
}

// CodeCommit is `code_commit`'s answer: one commit as the repository's history
// lists it, the push that introduced it — by the Gitlet node's index, and its row
// whole where the push history still holds it — and the changes it belongs to,
// read from the rows at the caller's freshness.
type CodeCommit struct {
	Repo   string           `json:"repo"`
	Commit gitlet.LogCommit `json:"commit"`
	// PushedBy is the push that introduced the commit, where the answering
	// node's index holds it (Coverage says what it covers), and Push that
	// push's row — who pushed it, the run behind a seat's push and every ref
	// it moved — where the repository's push history still holds it.
	PushedBy *gitlet.PushedBy `json:"pushed_by,omitempty"`
	Push     *gitlet.PushRow  `json:"push,omitempty"`
	Coverage *gitlet.Coverage `json:"coverage,omitempty"`
	// Merged, Revisions and More are the changes it belongs to, as the
	// reader's `commit` answers them (gitlet.CommitChanges): each with how
	// it landed once merged and the gate it merged through.
	Merged    []gitlet.ChangeLanding  `json:"merged,omitempty"`
	Revisions []gitlet.CommitRevision `json:"revisions,omitempty"`
	More      bool                    `json:"more,omitempty"`
	Position  statelog.Position       `json:"position"`
	Level     statelog.ReadLevel      `json:"level"`
	// Tagged is the tags naming the commit, from the answering node's tag
	// index, read at the repository's last record or later.
	Tagged *gitlet.TagMarks `json:"tagged,omitempty"`
}

// gitRev is one revision a request names: the parameter that named it, its
// spelling, and — once resolved — its object id and the ref it resolved
// through, none for an object id.
type gitRev struct {
	param, spelled string
	oid            string
	ref            *gitlet.ResolvedRef
}

// whole reports a revision named by a whole object id: the one spelling whose
// object no later push or object can change.
func (r *gitRev) whole() bool { return gitref.ValidOID(r.spelled) && r.spelled != gitlet.ZeroOID }

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
			gitlet.MinAbbrevDigits, headRev)
	}
	return &gitRev{param: param, spelled: rev}, nil
}

// oidsParam is the whole object ids a request names in key: every value of
// it, a list off a socket frame or a query string's one value, split at commas.
// How many, and a repeat, are the Gitlet node's to refuse.
func oidsParam(p Params, key string) ([]string, error) {
	values, ok := p.List(key)
	var oids []string
	for _, v := range values {
		for id := range strings.SplitSeq(v, ",") {
			if id = strings.TrimSpace(id); id != "" {
				oids = append(oids, id)
			}
		}
	}
	if !ok || len(oids) == 0 {
		return nil, fmt.Errorf("%w: %s is required: whole object ids, comma-separated", ErrBadParams, key)
	}
	for _, id := range oids {
		if !gitref.ValidOID(id) || id == gitlet.ZeroOID {
			return nil, fmt.Errorf("%w: %s names %q, which is not a whole object id", ErrBadParams, key, id)
		}
	}
	return oids, nil
}

// gitAsk is one git question as its caller asks it: the code reads, who asks,
// the repository by name, and the caller's freshness.
type gitAsk struct {
	reads CodeReads
	asker gitlet.Principal
	name  string
	fresh statelog.Freshness
	// listed is the repository's own last record, where a read lists what its
	// refs hold.
	listed statelog.Position
}

// gitAskOf is the git question p asks as the caller, the source asked first
// so a node that does not serve Gitlet says why whatever the request named.
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
// an object id: an object id passes; every other is a ref (`HEAD` is the
// default branch, resolved from the repository's row, gitlet.HeadRef), all
// resolved in ONE read at the caller's freshness, which answers the
// repository too; with no ref named the repository is read alone, for its
// id. A name no live ref holds is [NotFound] (`no_ref`), each one named.
//
// ONE READ FOR EVERY SPELLING: `ref` defaults to the default branch, so a
// read of the row ahead of the refs would cost the commonest question a second
// snapshot (a second router hop on a node without the estate) and let the
// name be released and retaken between the two.
func (g gitAsk) resolve(ctx context.Context, revs ...*gitRev) (gitlet.RepoRef, error) {
	var names []string
	for _, r := range revs {
		switch {
		case gitlet.ValidRev(r.spelled):
			r.oid = r.spelled
		case !slices.Contains(names, r.spelled):
			names = append(names, r.spelled)
		}
	}
	if len(names) == 0 {
		d, err := g.detail(ctx)
		if err != nil {
			return gitlet.RepoRef{}, err
		}
		return d.RepoRef, nil
	}
	a, err := g.reads.ResolveRef(ctx, gitlet.RefQuery{Repo: g.name, Refs: names, Fresh: g.fresh, Asker: g.asker})
	if err != nil {
		return gitlet.RepoRef{}, codeRefusal(g.name, err)
	}
	if err := a.Err(); err != nil {
		return gitlet.RepoRef{}, codeRefusal(g.name, err)
	}
	if len(a.Refs) != len(names) {
		return gitlet.RepoRef{}, fmt.Errorf("queries: Gitlet answered %d of the %d refs asked of %s",
			len(a.Refs), len(names), g.name)
	}
	for _, r := range revs {
		if r.oid != "" {
			continue
		}
		ref := a.Refs[slices.Index(names, r.spelled)]
		r.oid, r.ref = ref.OID, &ref
	}
	return a.Repo, nil
}

// detail is the repository the question names, read alone at the caller's
// freshness.
func (g gitAsk) detail(ctx context.Context) (*gitlet.RepoDetail, error) {
	a, err := g.reads.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadRepo, Repo: g.name, Asker: g.asker,
		Fresh: g.fresh})
	if err != nil {
		return nil, codeRefusal(g.name, err)
	}
	if a.Repo == nil {
		return nil, fmt.Errorf("queries: Gitlet answered repository %s's read with no detail", g.name)
	}
	return a.Repo, nil
}

// floor is what a read by object id of repo's revs is asked at: the furthest
// of this node's own writes to the repository, the caller's own floor on the
// code log, and, for a listing of the refs, the repository's own last record,
// and the record that last moved each ref a revision resolved through
// (gitlet.RefsFloor) — so the copy that answers holds what the caller wrote
// and what the refs named — or none, which reads the copy as it stands: an
// object id names one object at every position.
func (g gitAsk) floor(repo gitlet.RepoRef, revs ...*gitRev) statelog.Freshness {
	var refs []gitlet.ResolvedRef
	for _, r := range revs {
		if r.ref != nil {
			refs = append(refs, *r.ref)
		}
	}
	own := statelog.Later(statelog.Later(g.reads.FloorOf(repo.ID), g.fresh.Floor(topics.CodeLogStream)), g.listed)
	return gitlet.RefsFloor(own, refs...)
}

// read asks q of repo's home Gitlet node as the caller, at the floor of revs:
// UNSTAMPED where every revision is a whole object id, and then answered
// [Immutable], unless q reads the node's index of pushes
// (gitlet.GitQuery.ReadsIndex), which is asked stamped and answered bare. It is
// refused here first where q is one no Gitlet node would take ([ErrBadParams]
// in the domain's own words), and answered in this surface's vocabulary
// ([codeRefusal]).
func (g gitAsk) read(ctx context.Context, repo gitlet.RepoRef, q gitlet.GitQuery, cursor string,
	revs ...*gitRev) (CodePage, bool, error) {

	immutable := !q.ReadsIndex()
	for _, r := range revs {
		immutable = immutable && r.whole()
	}
	// A kind no page of which is cut unstamped, the tags, is asked stamped.
	immutable = immutable && q.Kind.Unstampable()
	q.RepoID, q.Asker, q.Fresh, q.Cursor, q.Unstamped = repo.ID, g.asker, g.floor(repo, revs...), cursor, immutable
	if err := q.Validate(); err != nil {
		return CodePage{}, false, fmt.Errorf("%w: %w", ErrBadParams, err)
	}
	a, err := g.reads.GitRead(ctx, q)
	if err != nil {
		return CodePage{}, false, codeRefusal(g.name, err)
	}
	page := CodePage{Repo: g.name, Kind: a.Kind, Rev: a.Rev, Next: a.Next, Tree: a.Tree, Blob: a.Blob, Diff: a.Diff,
		Log: a.Log, Blame: a.Blame, Find: a.Find, Compare: a.Compare, Tips: a.Tips, Tags: a.Tags,
		TreeCommits: a.TreeCommits}
	if immutable {
		return page, true, nil
	}
	page.Position, page.PushedBy, page.Coverage = a.Position, a.PushedBy, a.Coverage
	page.Tagged = a.Tagged
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
// is not valid UTF-8 — refused where it names both, as the Gitlet node would.
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
	page, immutable, err := g.read(ctx, repo, gitlet.GitQuery{Kind: gitlet.GitTree, Tree: &gitlet.TreeQuery{Rev: rev.oid,
		Path: path, PathB64: b64, Recursive: recursive}}, p.String("cursor"), rev)
	if err != nil {
		return nil, err
	}
	return answered(page, immutable), nil
}

// codeFind answers `code_find`: the files of `ref`'s tree (the default branch where
// none) whose path holds `q`'s characters in order, case-folded, best first, up to
// `limit`, with how many match. No cursor: a person narrows it by typing on.
func (s Sources) codeFind(ctx context.Context, p Params) (any, error) {
	g, err := s.gitAskOf(ctx, p, "code_find")
	if err != nil {
		return nil, err
	}
	rev, err := revParam(p, "ref", true)
	if err != nil {
		return nil, err
	}
	f := gitlet.FindQuery{Query: p.String("q")}
	if f.Query == "" {
		return nil, fmt.Errorf("%w: q is required: the characters a file's path holds, in order", ErrBadParams)
	}
	if p.Has("limit") {
		n, whole := p.WholeInt("limit")
		if !whole || n < 0 {
			return nil, fmt.Errorf("%w: the limit %q is not a whole number of zero or more (0 is the find's "+
				"own %d)", ErrBadParams, p.String("limit"), gitlet.DefaultFindLimit)
		}
		f.Limit = int(n)
	}
	repo, err := g.resolve(ctx, rev)
	if err != nil {
		return nil, err
	}
	f.Rev = rev.oid
	page, immutable, err := g.read(ctx, repo, gitlet.GitQuery{Kind: gitlet.GitFind, Find: &f}, "", rev)
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
	page, immutable, err := g.read(ctx, repo, gitlet.GitQuery{Kind: gitlet.GitBlob, Blob: &gitlet.BlobQuery{Rev: rev.oid,
		Path: path, PathB64: b64, StartLine: start, EndLine: end}}, p.String("cursor"), rev)
	if err != nil {
		return nil, err
	}
	return answered(page, immutable), nil
}

// codeLog answers `code_log`: the commits that changed `path` — or every
// commit — newest first from `ref`, `limit` a page (1 to gitlet.MaxLogLimit,
// gitlet.DefaultLogLimit where it names none), kept to those whose message
// holds `grep` (a literal), committed at or after `since` and before `until`
// (each RFC 3339), and brought by a push of the seat `pushed_by` names (its
// handle), the commits the node's index cannot judge counted as `unjudged`.
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
	l := gitlet.LogQuery{Path: path, PathB64: b64, Grep: p.String("grep")}
	if l.Since, err = instantParam(p, "since"); err != nil {
		return nil, err
	}
	if l.Until, err = instantParam(p, "until"); err != nil {
		return nil, err
	}
	l.PushedBy = strings.TrimSpace(p.String("pushed_by"))
	if p.Has("limit") {
		n, whole := p.WholeInt("limit")
		if !whole || n < 0 {
			return nil, fmt.Errorf("%w: the limit %q is not a whole number of zero or more (0 is the history's "+
				"own page of %d)", ErrBadParams, p.String("limit"), gitlet.DefaultLogLimit)
		}
		l.Limit = int(n)
	}
	repo, err := g.resolve(ctx, rev)
	if err != nil {
		return nil, err
	}
	l.Rev = rev.oid
	page, immutable, err := g.read(ctx, repo, gitlet.GitQuery{Kind: gitlet.GitLog, Log: &l}, p.String("cursor"), rev)
	if err != nil {
		return nil, err
	}
	return answered(page, immutable), nil
}

// codeBlame answers `code_blame`: which commit last changed each line of the
// file `path` at `ref`, `start_line` to `end_line` — at most
// gitlet.MaxBlameLines lines — a page at a time.
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
	page, immutable, err := g.read(ctx, repo, gitlet.GitQuery{Kind: gitlet.GitBlame, Blame: &gitlet.BlameQuery{
		Rev: rev.oid, Path: path, PathB64: b64, StartLine: start, EndLine: end}}, p.String("cursor"), rev)
	if err != nil {
		return nil, err
	}
	return answered(page, immutable), nil
}

// codeTreeCommits answers `code_tree_commits`: the commit that last changed
// each entry of a directory at `ref` (the default branch where it names
// none), its root or `path`, a page at a time from `cursor`, code_tree's own.
func (s Sources) codeTreeCommits(ctx context.Context, p Params) (any, error) {
	g, err := s.gitAskOf(ctx, p, "code_tree_commits")
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
	repo, err := g.resolve(ctx, rev)
	if err != nil {
		return nil, err
	}
	page, immutable, err := g.read(ctx, repo, gitlet.GitQuery{Kind: gitlet.GitTreeCommits,
		TreeCommits: &gitlet.TreeCommitsQuery{Rev: rev.oid, Path: path, PathB64: b64}}, p.String("cursor"), rev)
	if err != nil {
		return nil, err
	}
	// A page its read's budget cut is the machine's and the moment's.
	return answered(page, immutable && !page.TreeCommits.Cut()), nil
}

// codeDiff answers `code_diff`: what changed from `base` to `head` — the
// per-file stat, narrowed to `paths` (a file, or a directory it is under; at
// most gitlet.MaxDiffPaths), or one `file`'s patch (`file_b64` for a path's
// exact bytes), or with `patches` every file's patch — with renames detected
// unless `renames` is false, a page at a time.
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
	d := gitlet.DiffQuery{File: p.String("file"), FileB64: p.String("file_b64")}
	if d.Paths, _ = p.List("paths"); len(d.Paths) > gitlet.MaxDiffPaths {
		return nil, fmt.Errorf("%w: paths names %d paths, past the %d one diff is narrowed to", ErrBadParams,
			len(d.Paths), gitlet.MaxDiffPaths)
	}
	if d.Renames, err = boolParam(p, "renames", true); err != nil {
		return nil, err
	}
	if d.Patches, err = boolParam(p, "patches", false); err != nil {
		return nil, err
	}
	repo, err := g.resolve(ctx, base, head)
	if err != nil {
		return nil, err
	}
	d.Base, d.Head = base.oid, head.oid
	page, immutable, err := g.read(ctx, repo, gitlet.GitQuery{Kind: gitlet.GitDiff, Diff: &d}, p.String("cursor"), base,
		head)
	if err != nil {
		return nil, err
	}
	return answered(page, immutable), nil
}

// codeCompare answers `code_compare`: how `base` and `head` relate and what
// merging head into base would do, asked of the home by the two whole commit
// ids compared (code.merge_preview), so the preview merges exactly those.
// Immutable only where both are whole ids and the merge was decided; one the
// home could not decide in time answers `unknown`.
//
// A COMPARISON IN WHICH ONE COMMIT CONTAINS THE OTHER IS ITS OWN PREVIEW and
// asks the home nothing: base inside head is a fast-forward, head inside base
// merges nothing, both `clean`, decided here from the merge base already
// read. Asking would cost a call and a git-work unit of the seats' shared
// share on every view of the commonest comparison, a branch against its
// unmoved target.
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
	page, immutable, err := g.read(ctx, repo, gitlet.GitQuery{Kind: gitlet.GitCompare, Compare: &gitlet.CompareQuery{
		Base: base.oid, Head: head.oid}}, "", base, head)
	if err != nil {
		return nil, err
	}
	if page.Compare == nil {
		return nil, fmt.Errorf("queries: Gitlet answered %s's comparison with no comparison", g.name)
	}
	preview := gitlet.MergePreviewAnswer{State: gitlet.MergeClean}
	if !page.Compare.BaseIsAncestor && !page.Compare.HeadIsAncestor {
		preview, err = g.reads.MergePreview(ctx, gitlet.MergePreviewQuery{RepoID: repo.ID, Asker: g.asker,
			Base: page.Compare.Base, Head: page.Compare.Head, Fresh: g.floor(repo, base, head)})
		if err != nil {
			return nil, codeRefusal(g.name, err)
		}
	}
	out := CodeComparison{CodePage: page, Preview: CodeMergePreview{State: preview.State,
		Conflicts: preview.Conflicts, ConflictsOmitted: preview.ConflictsOmitted}}
	return answered(out, immutable && preview.State != gitlet.MergeUnknown), nil
}

// codeTips answers `code_tips`: up to gitlet.MaxTips objects by their whole
// `oids`, each peeled with its tag's facts and its commit, and with `base`
// (any revision) how far each is from it; immutable unless base names a ref.
func (s Sources) codeTips(ctx context.Context, p Params) (any, error) {
	g, err := s.gitAskOf(ctx, p, "code_tips")
	if err != nil {
		return nil, err
	}
	oids, err := oidsParam(p, "oids")
	if err != nil {
		return nil, err
	}
	var revs []*gitRev
	if strings.TrimSpace(p.String("base")) != "" {
		base, _ := revParam(p, "base", false) // named, so never refused
		revs = append(revs, base)
	}
	repo, err := g.resolve(ctx, revs...)
	if err != nil {
		return nil, err
	}
	q := gitlet.TipsQuery{OIDs: oids}
	if len(revs) > 0 {
		q.Base = revs[0].oid
	}
	page, immutable, err := g.read(ctx, repo, gitlet.GitQuery{Kind: gitlet.GitTips, Tips: &q}, p.String("cursor"),
		revs...)
	if err != nil {
		return nil, err
	}
	return answered(page, immutable), nil
}

// codeTags answers `code_tags`: the repository's tags newest first by their
// own date, `limit` a page (1 to gitlet.MaxTagsLimit, gitlet.DefaultTagsLimit
// where it names none) from `cursor`; never immutable, since the tags move.
func (s Sources) codeTags(ctx context.Context, p Params) (any, error) {
	g, err := s.gitAskOf(ctx, p, "code_tags")
	if err != nil {
		return nil, err
	}
	q := gitlet.TagsQuery{}
	if p.Has("limit") {
		n, whole := p.WholeInt("limit")
		if !whole || n < 0 {
			return nil, fmt.Errorf("%w: the limit %q is not a whole number of zero or more (0 is a page of %d)",
				ErrBadParams, p.String("limit"), gitlet.DefaultTagsLimit)
		}
		q.Limit = int(n)
	}
	d, err := g.detail(ctx)
	if err != nil {
		return nil, err
	}
	g.listed = d.Version
	page, _, err := g.read(ctx, d.RepoRef, gitlet.GitQuery{Kind: gitlet.GitTags, Tags: &q}, p.String("cursor"))
	if err != nil {
		return nil, err
	}
	return page, nil
}

// codeCommit answers `code_commit`: the commit `sha` — a WHOLE id, which is
// what a commit's page is linked by; an abbreviation is expanded by asking
// `code_log` with it as `ref` and a `limit` of 1 — as the repository's history
// lists it, the push that introduced it, and every change it belongs to. The
// rows first, at the caller's freshness — the repository and the commit's
// changes, in one snapshot (the reader's `commit`) — then the commit, asked of
// the repository's home Gitlet node at the repository's own last record, so
// the tags naming it are the rows' too, and the push the node's index names,
// read whole at `session` floored at the later of the rows' read and the
// push's own record.
func (s Sources) codeCommit(ctx context.Context, p Params) (any, error) {
	g, err := s.gitAskOf(ctx, p, "code_commit")
	if err != nil {
		return nil, err
	}
	sha := strings.TrimSpace(p.String("sha"))
	if !gitref.ValidOID(sha) || sha == gitlet.ZeroOID {
		return nil, fmt.Errorf("%w: sha %q is not a whole commit id: a commit's page names its commit whole, and "+
			"code_log with it as ref and a limit of 1 expands an abbreviation", ErrBadParams, sha)
	}
	rows, err := g.reads.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadCommit, Repo: g.name, Commit: sha, Asker: g.asker,
		Fresh: g.fresh})
	if err != nil {
		return nil, codeRefusal(g.name, err)
	}
	if rows.Repo == nil || rows.Commit == nil {
		return nil, fmt.Errorf("queries: Gitlet answered commit %s of %s with no repository or changes",
			sha, g.name)
	}
	repo := rows.Repo.RepoRef
	// At the repository's own last record, so the copy holds every tag the
	// rows' position does.
	g.listed = rows.Repo.Version
	q := gitlet.GitQuery{Kind: gitlet.GitLog, RepoID: repo.ID, Asker: g.asker, Fresh: g.floor(repo),
		Log: &gitlet.LogQuery{Rev: sha, Limit: 1}}
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
	out.Tagged = a.Tagged
	by, pushed := a.PushedBy[sha]
	if !pushed {
		return out, nil
	}
	out.PushedBy = &by
	pushes, err := g.reads.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadPushes, Repo: g.name, Push: by.PushID,
		Asker: g.asker, Fresh: statelog.FloorAt(statelog.ReadSession, statelog.Later(rows.Position, by.Position))})
	if err != nil {
		return nil, codeRefusal(g.name, err)
	}
	if len(pushes.Pushes) == 1 {
		out.Push = &pushes.Pushes[0]
	}
	return out, nil
}
