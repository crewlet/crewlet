package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/host"
)

// A CHECK'S BOX READS ITS COMMIT, through the API rather than a clone:
// GET /box/gitlet/{repo}/tree, /blob and /diff ([auth.BoxGitletPrefix]) — the
// listing of a directory of the commit its check measures, a file's lines of
// it, and the diff from the base its attempt was queued with to it — so a
// check whose checks file says `checkout: none` (an API-first check: a linter
// of a change's diff, a policy over its files) reads what it needs and clones
// nothing.
//
// # Admitted as its git is, and held to its commit
//
// Each read is ADMITTED BY GITLET'S CHECK ADMISSION
// (host.Server.AdmitCheck): the check's token as git's Basic password, the
// attempt's row read on every request — a box whose attempt has ended reads
// nothing more, whatever its token's expiry — and the repository its token
// names, under the name its box was given. A refusal there is the one line of
// text the box's git is answered in, since it is the one admission. Then the
// read is asked of the code domain's git reads — the repository's home Gitlet
// node first — as THE ENGINE'S OWN, made for the check (gitlet.GitQuery.Check):
// held to no grant, recorded as nobody's read, and answered at the check's
// session, the position its attempt was queued at, so the node answering
// holds its commit.
//
// AND ONLY THAT COMMIT. No read takes a revision: a tree and a file are read
// at the attempt's commit, and a diff from the attempt's base to it, so a box
// cannot name another commit of the repository — a later head of its change,
// a branch's tip, an object it learnt the id of — and the Gitlet node holds the
// read to that commit again as it validates it, whatever asked. A parameter no
// read takes is REFUSED, never ignored: a box that asked for `rev=` and was
// answered its own commit would read the wrong code with nothing saying so.
//
// # What it answers
//
// The read's page in the read surface's JSON — a tree's listing, a file's
// lines, a diff's stat or one file's patch, each the Gitlet node's own — with the
// repository's name, the commit, the base a diff was read from and the next
// page's cursor: the commit's content alone, and none of the push provenance
// a seat's read carries (gitlet.GitAnswer.PushedBy), since a check measuring a
// commit reads nothing of who pushed what. A read's own refusal is the read
// surface's envelope: `not_found` for a path the commit does not hold,
// `bad_params` for a parameter the read cannot take (its sentence names it),
// `unavailable` for a moment no node holding the repository could answer.

// boxCodeParams are the parameters each read takes, every one optional, and
// the one that may repeat. NO REVISION AMONG THEM: the commit, and a diff's
// base, are the attempt's.
var boxCodeParams = map[gitlet.GitReadKind][]string{
	gitlet.GitTree: {"path", "path_b64", "recursive", "cursor"},
	gitlet.GitBlob: {"path", "path_b64", "start_line", "end_line", "cursor"},
	gitlet.GitDiff: {"paths", "file", "file_b64", "renames", "cursor"},
}

// boxCodeRepeated is the one parameter a request may name more than once:
// a diff's paths, each a path its stat is narrowed to.
const boxCodeRepeated = "paths"

// boxCodeAnswer is a check's box's read of its commit as the API answers it.
type boxCodeAnswer struct {
	// Repo is the repository's name, Commit the commit read — the attempt's
	// — and Base, on a diff, the commit it was read from.
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
	Base   string `json:"base,omitempty"`
	// Next is the next page's cursor, handed back as `cursor`; empty on the
	// last page.
	Next string             `json:"next,omitempty"`
	Tree *gitlet.TreeAnswer `json:"tree,omitempty"`
	Blob *gitlet.BlobAnswer `json:"blob,omitempty"`
	Diff *gitlet.DiffAnswer `json:"diff,omitempty"`
}

// boxCodeRoute serves a check's box's read of kind: on a node not serving
// Gitlet now, a request presenting a token this fleet signed is told why and
// any other is challenged, as a box's git route answers ([App.boxGitRoute]);
// otherwise admitted (host.Server.AdmitCheck), its parameters read, and its
// read asked of the code domain's git reads.
func (a *App) boxCodeRoute(kind gitlet.GitReadKind) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g, why := a.code.GitRoutes()
		if why != nil {
			if host.ChallengeBox(w, r, a.code.VerifyBox, log) {
				return
			}
			unservedGit(w, why)
			return
		}
		adm, ok := g.AdmitCheck(r.Context(), w, r)
		if !ok {
			return
		}
		what := "box_code_" + string(kind)
		q, err := boxCodeQuery(kind, adm, r.URL.Query())
		if err != nil {
			writeQueryError(w, r, what, err)
			return
		}
		reads, why := a.code.GitReads()
		if why != nil {
			codeNotServed(w, r, what, why)
			return
		}
		page, err := reads.GitRead(r.Context(), q)
		if err != nil {
			writeQueryError(w, r, what, boxCodeRefusal(err))
			return
		}
		out := boxCodeAnswer{Repo: adm.Repo.Name, Commit: adm.Attempt.OID, Next: page.Next, Tree: page.Tree,
			Blob: page.Blob, Diff: page.Diff}
		if kind == gitlet.GitDiff {
			out.Base = adm.Attempt.Base
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// boxCodeQuery is the read of kind a check's box's request asks, at its
// attempt's commit and session, made for its attempt — or the parameter it
// cannot take, as [queries.ErrBadParams] naming it: one no read of kind
// takes, one named twice that may not be, a value that does not parse, or a
// read the domain refuses as asked (gitlet.GitQuery.Validate's own words).
func boxCodeQuery(kind gitlet.GitReadKind, adm host.CheckAdmitted, params url.Values) (gitlet.GitQuery, error) {
	takes := boxCodeParams[kind]
	for name, values := range params {
		switch {
		case !slices.Contains(takes, name):
			return gitlet.GitQuery{}, fmt.Errorf("%w: %s: a %s read of a check's commit takes %s, and never "+
				"a revision: it reads the commit its check measures", queries.ErrBadParams, name, kind,
				strings.Join(takes, ", "))
		case len(values) > 1 && name != boxCodeRepeated:
			return gitlet.GitQuery{}, fmt.Errorf("%w: %s: named %d times; it is one value", queries.ErrBadParams,
				name, len(values))
		}
	}
	one := params.Get
	flag := func(name string) (bool, error) {
		if one(name) == "" {
			return false, nil
		}
		v, err := strconv.ParseBool(one(name))
		if err != nil {
			return false, fmt.Errorf("%w: %s: %q is not true or false", queries.ErrBadParams, name, one(name))
		}
		return v, nil
	}
	line := func(name string) (int, error) {
		if one(name) == "" {
			return 0, nil
		}
		v, err := strconv.Atoi(one(name))
		if err != nil {
			return 0, fmt.Errorf("%w: %s: %q is not a line's number", queries.ErrBadParams, name, one(name))
		}
		return v, nil
	}
	attempt := adm.Attempt.CheckKey
	q := gitlet.GitQuery{Kind: kind, RepoID: adm.Repo.ID, Check: &attempt, Fresh: adm.Fresh,
		Cursor: one("cursor")}
	switch kind {
	case gitlet.GitTree:
		recursive, err := flag("recursive")
		if err != nil {
			return gitlet.GitQuery{}, err
		}
		q.Tree = &gitlet.TreeQuery{Rev: attempt.OID, Path: one("path"), PathB64: one("path_b64"),
			Recursive: recursive}
	case gitlet.GitBlob:
		start, err := line("start_line")
		if err != nil {
			return gitlet.GitQuery{}, err
		}
		end, err := line("end_line")
		if err != nil {
			return gitlet.GitQuery{}, err
		}
		q.Blob = &gitlet.BlobQuery{Rev: attempt.OID, Path: one("path"), PathB64: one("path_b64"),
			StartLine: start, EndLine: end}
	case gitlet.GitDiff:
		if adm.Attempt.Base == "" {
			return gitlet.GitQuery{}, fmt.Errorf("%w: diff: attempt %d of check %s of %s was queued with no "+
				"base to diff its commit from; read its tree and its files", queries.ErrBadParams,
				attempt.Attempt, attempt.Name, attempt.OID)
		}
		renames, err := flag("renames")
		if err != nil {
			return gitlet.GitQuery{}, err
		}
		q.Diff = &gitlet.DiffQuery{Base: adm.Attempt.Base, Head: attempt.OID, Paths: params[boxCodeRepeated],
			File: one("file"), FileB64: one("file_b64"), Renames: renames}
	}
	if err := q.Validate(); err != nil {
		return gitlet.GitQuery{}, fmt.Errorf("%w: %w", queries.ErrBadParams, err)
	}
	return q, nil
}

// boxCodeRefusal is a check's box's read the code domain's git reads did not
// answer, in the read surface's vocabulary: an object, a path or a repository
// the commit's copy does not hold is [queries.NotFound] in the domain's own
// words; a read the Gitlet node refused as asked is [queries.ErrBadParams]; a
// Gitlet node that could not answer now — unready, not serving, none of the
// repository's answering in time — is [queries.ErrUnavailable], as a seat's
// read of the same names it (builtin's code tools); anything else is
// classified as every question's failure is ([queries.ClassifyFailure]).
func boxCodeRefusal(err error) error {
	var unready *gitlet.ErrGitUnready
	for _, absent := range []error{gitlet.ErrObjectNotFound, gitlet.ErrNotATree, gitlet.ErrNotABlob,
		gitlet.ErrRepoNotFound} {
		if errors.Is(err, absent) {
			said, ok := builtin.RefusalSentence(err, absent)
			if !ok {
				said = absent.Error()
			}
			return &queries.NotFound{Detail: said}
		}
	}
	switch {
	case errors.Is(err, gitlet.ErrSliceTooLarge), errors.Is(err, gitlet.ErrInvalid),
		errors.Is(err, gitlet.ErrBadCursor), errors.Is(err, gitlet.ErrBadLevel):
		return fmt.Errorf("%w: %w", queries.ErrBadParams, err)
	case errors.As(err, &unready), errors.Is(err, gitlet.ErrNotServing):
		return fmt.Errorf("%w: %w", queries.ErrUnavailable, err)
	}
	return queries.ClassifyFailure(err)
}
