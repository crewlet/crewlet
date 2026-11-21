package queries

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/codesearch"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CODE SEARCH, AS A QUESTION: `code_search` (GET /gitlet/search), the lines
// of every repository the caller may read that match, answered by one scatter
// over the fleet's Gitlet nodes (internal/codesearch, through the engine's
// [CodeSearcher]), every answer with the coverage naming what was not
// searched. Decided per repository by each answering node's grant as every
// code question is (code.go): a listing leaves out what the caller may not
// read, a `repo:` naming one is refused [NotPermitted]; registered
// [Requires] the gitlet app.

// The search is codeindex's one grammar (codeindex.ParseQuery): `q` holds the
// filters (repo:, path:, ref:) then the text. `mode`, `ignore_case` and one
// `repo`, `path` and `ref` may ride beside it as a screen's controls, each
// held to the grammar's rule: a repo or path widens the search as one more
// filter does, and a ref beside a different ref: is refused.

// Code search is deliberately NOT read back at a session floor (the
// dashboard's SESSION_QUERIES) and takes no floor: it ANSWERS AT THE TIP IT
// NAMES. Each repository is searched at the commit its answering node's copy
// holds for the ref (the answer's `tips`) and a hit's file is read at that
// commit by object id, so every copy is read as it stands and the caller's
// repositories are listed at their level, `stale` by default. `read_level=
// session` and `min_position` are refused: a floor would tell a screen it
// waited for its write though the lines come from whatever each node held,
// and a scatter after every code write is a search nobody asked for.

// CodeSearcher is a code search over the fleet's Gitlet nodes, as the engine asks
// it ([engine.Engine.CodeSearch], over codesearch.Searcher). Declared here by
// its consumer.
type CodeSearcher interface {
	CodeSearch(ctx context.Context, q codesearch.Query) (codesearch.Answer, error)
}

// codeSearchFloorless is why `code_search` refuses a floor.
const codeSearchFloorless = "code_search takes no floor: it answers at the tip of each repository it names " +
	"(`tips`), and a hit's file is read at that commit; read_level is linearizable, stale or " +
	"consistent_prefix, the level the repositories it searches are listed at"

// codeSearch answers `code_search`: `q`, REQUIRED, in codeindex's grammar;
// `mode` (literal unset), `ignore_case`, one `repo`, `path` and `ref` beside
// its filters; `limit` (1 to gitlet.MaxSearchHits, unset or 0 the most);
// `context` (0 to gitlet.MaxSearchContext); `per_file`, each then naming its
// file's total; and the level the caller's repositories are listed at.
//
// THE SOURCE FIRST, so a node that does not serve Gitlet says why whatever the
// request named; then the request, refused [ErrBadParams] naming what the
// grammar refuses; then the search, asked AS THE CALLER and answered in
// [codeRefusal]'s vocabulary, a refusal over one repository naming it
// ([codesearch.RepoRefusal]).
func (s Sources) codeSearch(ctx context.Context, p Params) (any, error) {
	const what = "code_search"
	searcher, err := s.Code.CodeSearcher()
	if err != nil {
		return nil, err
	}
	principal, err := caller(ctx, what)
	if err != nil {
		return nil, err
	}
	q, err := codeSearchQuery(p)
	if err != nil {
		return nil, err
	}
	q.Asker = codeAsker(principal)
	answer, err := searcher.CodeSearch(ctx, q)
	if err != nil {
		var refused *codesearch.RepoRefusal
		repo := ""
		if errors.As(err, &refused) {
			repo = refused.Repo
		}
		return nil, codeRefusal(repo, err)
	}
	return answer, nil
}

// codeSearchQuery is p's search, its asker left for the caller, refused
// [ErrBadParams] naming what the grammar or the level refuses.
func codeSearchQuery(p Params) (codesearch.Query, error) {
	// NO FLOOR, named before the grammar reads the level: a bare `session`
	// would otherwise be told to send the min_position refused next.
	if level := strings.TrimSpace(p.String("read_level")); level == string(statelog.ReadSession) {
		return codesearch.Query{}, fmt.Errorf("%w: read_level=session: %s", ErrBadParams, codeSearchFloorless)
	}
	if p.Has("min_position") {
		return codesearch.Query{}, fmt.Errorf("%w: min_position: %s", ErrBadParams, codeSearchFloorless)
	}
	rows, err := Freshness(p)
	if err != nil {
		return codesearch.Query{}, err
	}
	text := p.String("q")
	if strings.TrimSpace(text) == "" {
		return codesearch.Query{}, badParams("q", "", nil)
	}
	search, err := codeindex.ParseQuery(text)
	if err != nil {
		return codesearch.Query{}, fmt.Errorf("%w: q: %w", ErrBadParams, err)
	}
	if mode := strings.TrimSpace(p.String("mode")); mode != "" {
		search.Mode = codeindex.Mode(mode)
		if !search.Mode.Valid() {
			return codesearch.Query{}, badParams("mode", mode, names(codeindex.Modes))
		}
	}
	if search.IgnoreCase, err = boolParam(p, "ignore_case", false); err != nil {
		return codesearch.Query{}, err
	}
	// A FILTER BESIDE THE TEXT IS ONE MORE FILTER, by the grammar's own
	// rule (codeindex.Query.Beside), which the seat tool's arguments take
	// too: a repository or a path widens the search, and a ref beside a
	// different ref: is the two refs the grammar refuses.
	if err := search.Beside(p.String("repo"), p.String("path"), p.String("ref")); err != nil {
		return codesearch.Query{}, fmt.Errorf("%w: %w", ErrBadParams, err)
	}
	if err := search.Validate(); err != nil {
		return codesearch.Query{}, fmt.Errorf("%w: %w", ErrBadParams, err)
	}
	q := codesearch.Query{Search: search, Rows: rows}
	for _, n := range []struct {
		name string
		into *int
		most int
	}{{"limit", &q.Limit, gitlet.MaxSearchHits}, {"context", &q.Context, gitlet.MaxSearchContext},
		{"per_file", &q.PerFile, gitlet.MaxSearchHits}} {
		if !p.Has(n.name) {
			continue
		}
		v, whole := p.WholeInt(n.name)
		if !whole || v < 0 || v > int64(n.most) {
			return codesearch.Query{}, fmt.Errorf("%w: %s: %q is not a whole number from 0 to %d", ErrBadParams,
				n.name, p.String(n.name), n.most)
		}
		*n.into = int(v)
	}
	if err := (gitlet.SearchQuery{Limit: q.Limit, Context: q.Context, PerFile: q.PerFile}).ValidateBounds(); err != nil {
		return codesearch.Query{}, fmt.Errorf("%w: %w", ErrBadParams, err)
	}
	return q, nil
}
