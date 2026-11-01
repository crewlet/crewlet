package queries

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/codesearch"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CODE SEARCH, AS A QUESTION: `code_search` (GET /code/search), the lines of
// every repository the caller may read that match a search — answered by the
// one scatter over the fleet's git nodes (internal/codesearch, through the
// engine's [CodeSearcher]): one repository
// read through its keyed walk, more in one signed request every git node
// hears, and every answer with the coverage that names what was not searched.
// Decided per repository by the grant each answering node derives, as every
// code question is (code.go): a listing of the caller's repositories leaves
// out what they may not read, and a repo: naming one is refused
// [NotPermitted] — so it is registered [DecidedBy] `code.read`, as the rest.
//
// # One grammar, in the text or beside it
//
// The search is codeindex's one grammar (codeindex.ParseQuery): `q` is what a
// person writes, its filters first — repo:, path: and ref: — then the text.
// `mode`, `ignore_case` and one `repo`, `path` and `ref` may ride beside it as
// parameters, a screen's controls, each held to the rule the grammar reads
// its filter by: a repo or a path widens the search as one more filter does,
// and a ref beside a different ref: is refused, as two ref: filters are.
//
// # Not read back at a session floor
//
// It is deliberately NOT one of the questions a screen reads its own write
// back through (the dashboard's SESSION_QUERIES), and it takes no floor: it
// ANSWERS AT THE TIP IT NAMES. Each repository is searched at the commit its
// answering node's copy holds for the ref — named in the answer's `tips` —
// and a hit's file is read at that commit by object id, the same at every
// position. So the search reads every copy as it stands (no floor on the code
// log) and lists the caller's repositories at the caller's level — `stale` by
// default, `linearizable` where a repository declared a moment ago must not be
// missed — and `read_level=session` and `min_position` are refused, naming
// why: a floor it took would be a screen told it waited for its write where
// the lines it was answered come from whatever each node held, and a scatter
// to every git node asked again after every code write a screen makes is a
// search nobody asked for.

// CodeSearcher is a code search over the fleet's git nodes, as the engine asks
// it ([engine.Engine.CodeSearch], over codesearch.Searcher). Declared here by
// its consumer.
type CodeSearcher interface {
	CodeSearch(ctx context.Context, q codesearch.Query) (codesearch.Answer, error)
}

// codeSearchFloorless is why `code_search` refuses a floor.
const codeSearchFloorless = "code_search takes no floor: it answers at the tip of each repository it names " +
	"(`tips`), and a hit's file is read at that commit — read_level is linearizable, stale or " +
	"consistent_prefix, the level the repositories it searches are listed at"

// codeSearch answers `code_search`: `q`, the search in codeindex's grammar,
// REQUIRED; `mode` (literal, regex or word; literal unset), `ignore_case`
// (true or false), and one `repo`, `path` and `ref` beside its filters;
// `limit`, the most lines answered (1 to code.MaxSearchHits, unset or 0 the
// most); and the level the caller's repositories are listed at.
//
// THE SOURCE FIRST, so a node that serves no code host says why whatever the
// request named; then the request, refused [ErrBadParams] naming what the
// grammar refuses; then the search, asked AS THE CALLER and answered in this
// surface's vocabulary ([codeRefusal]), a refusal over one repository naming
// it ([codesearch.RepoRefusal]).
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
	// A FILTER BESIDE THE TEXT IS ONE MORE FILTER: a repository or a path
	// widens the search, named once however often it is named, and one ref
	// is read — a ref beside a different ref: is the two refs the grammar
	// refuses.
	if repo := strings.TrimSpace(p.String("repo")); repo != "" && !slices.Contains(search.Repos, repo) {
		search.Repos = append(search.Repos, repo)
	}
	if path := strings.TrimSpace(p.String("path")); path != "" && !slices.Contains(search.Paths, path) {
		search.Paths = append(search.Paths, path)
	}
	if ref := strings.TrimSpace(p.String("ref")); ref != "" {
		if search.Ref != "" && search.Ref != ref {
			return codesearch.Query{}, fmt.Errorf("%w: ref %q beside ref:%s: a search reads one ref",
				ErrBadParams, ref, search.Ref)
		}
		search.Ref = ref
	}
	if err := search.Validate(); err != nil {
		return codesearch.Query{}, fmt.Errorf("%w: %w", ErrBadParams, err)
	}
	q := codesearch.Query{Search: search, Rows: rows}
	if p.Has("limit") {
		n, whole := p.WholeInt("limit")
		if !whole || n < 0 || n > code.MaxSearchHits {
			return codesearch.Query{}, fmt.Errorf("%w: the limit %q is not a whole number from 0 to %d "+
				"(0 is the most)", ErrBadParams, p.String("limit"), code.MaxSearchHits)
		}
		q.Limit = int(n)
	}
	return q, nil
}
