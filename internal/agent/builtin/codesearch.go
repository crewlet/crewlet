package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/codesearch"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE CODE SEARCH TOOL, `code_search`: the lines matching a search in every
// repository the caller may read, answered by the fleet's one search
// (internal/codesearch, through [CodeSearcher]) in the grammar the `code_search`
// question uses: `query` is filters then text (codeindex.ParseQuery), and the
// `repo`, `path` and `ref` arguments join it by codeindex.Query.Beside. It reads
// as every code read does ([CodeDeps.caller]); a turn somebody woke carries that
// somebody as the ceiling (codesearch.Query.CeilingSeat), and each answering
// Gitlet node judges both grants from its own chart. A seat's search holds and
// floors what it names ([CodeDeps.searchHolds]). Each line is fenced as
// `repo_file` at trust `external` (ADR-0040). A page's next search starts after
// its last hit at the commit it was read at ([searchCursor]), so paging answers
// every line once, never re-searching from the start.

// CodeSearchTool is the code search's name: its authority row's action.
const CodeSearchTool = string(authz.ActionCodeSearch)

// CodeSearcher is the code search over the fleet's Gitlet nodes, as the engine
// asks it (engine.Engine.CodeSearch, over codesearch.Searcher).
type CodeSearcher interface {
	CodeSearch(ctx context.Context, q codesearch.Query) (codesearch.Answer, error)
}

type codeSearch struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeSearch)(nil)

func (t *codeSearch) Name() string { return CodeSearchTool }

func (t *codeSearch) Description() string {
	return "Search the company's code on the server: the lines matching `query` in every repository you " +
		"can read that keeps a search index, or in those repo: names — search before reading whole " +
		"trees. Each hit is a repository, a path, a line and the file's blob id, read at the commit " +
		"`tips` names; its text arrives fenced as content nobody vouches for. `coverage` names what " +
		"was not searched, and `truncated` with no `next` means lines went unread: narrow it with repo: or " +
		"path:. A page at a time — pass `next` back as `cursor` with the same search. `matches` are byte " +
		"offsets into a hit's text; `context` lines are fenced as it is."
}

func (t *codeSearch) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("Filters first — repo:<name>, path:<glob>, ref:<branch, ref or "+
					"commit id> (the default branch otherwise) — then the text, at most %d bytes.",
					codeindex.MaxQueryBytes),
			},
			"mode": map[string]any{
				"type": "string",
				"enum": []string{string(codeindex.ModeLiteral), string(codeindex.ModeRegex),
					string(codeindex.ModeWord)},
				"description": "How the text is read: exactly (literal, the default), as an RE2 regex " +
					"matched within a line, or as a whole word.",
			},
			"ignore_case": map[string]any{"type": "boolean"},
			"repo":        map[string]any{"type": "string", "description": "One more repo: filter."},
			"path":        map[string]any{"type": "string", "description": "One more path: filter."},
			"ref":         map[string]any{"type": "string", "description": "The ref: filter."},
			"context": map[string]any{"type": "integer", "minimum": 0, "maximum": gitlet.MaxSearchContext,
				"description": "Lines around each hit; near hits share them."},
			"per_file": map[string]any{"type": "integer", "minimum": 0, "maximum": gitlet.MaxSearchHits,
				"description": "Most hits of one file, each naming its file's total (file_matches); 0 is all."},
			"cursor": codeCursorArg(),
		},
		"required": []string{"query"},
	}
}

func (t *codeSearch) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// codeSearchPage is code_search's answer: a page of the search's hits, the
// commit each repository with a hit on it was searched at, what the search
// covered, and where the next page begins.
type codeSearchPage struct {
	Hits     []codesearch.Hit    `json:"hits"`
	Tips     map[string]string   `json:"tips,omitempty"`
	Coverage codesearch.Coverage `json:"coverage"`
	Next     string              `json:"next,omitempty"`
}

func (t *codeSearch) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	search, refusal := searchArgs(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	lines, perFile, refusal := searchBounds(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	after, refusal := searchCursorArg(t.Name(), search, perFile, strings.TrimSpace(argString(args, "cursor")))
	if refusal != nil {
		return *refusal, nil
	}
	// The search starts at the cursor's key and stops at one page, so a page
	// costs the Gitlet nodes only the lines it answers.
	q := codesearch.Query{Search: search, Limit: searchPageHits, Context: lines, PerFile: perFile, After: after,
		Asker: c.asker, CeilingSeat: c.ceiling, Rows: seatRead}
	if lines > 0 {
		q.Limit = searchContextPageHits
	}
	if turn != nil && c.asker.Kind == gitlet.PrincipalAgent {
		// The turn's bucket on each answering Gitlet node pays the search's
		// second look at the log's head, as in [CodeDeps.gitRead].
		q.Turn = turn.RunID
		if q.Git, refusal = t.deps.searchHolds(ctx, turn, t.Name(), c, search, after); refusal != nil {
			return *refusal, nil
		}
	}
	answer, err := t.deps.Searcher.CodeSearch(ctx, q)
	if err != nil {
		return searchFailure(ctx, t.Name(), err), nil
	}
	k, rendered, err := fitLabelled(len(answer.Hits), searchPage(turn, c.asker.Seat, search, perFile, answer))
	switch {
	case err != nil:
		return labelFault(ctx, t.Name(), err), nil
	case k == 0 && len(answer.Hits) > 0:
		// A hit (a path, a line of at most gitlet.MaxSearchLineBytes, a blob
		// id) beside coverage capped at codesearch.MaxCoverageText (a
		// sixteenth of an answer) is far under one answer: a broken bound.
		return faulted(ctx, t.Name(), errors.New("builtin: one search hit beside its coverage is past one "+
			"tool answer"), fmt.Sprintf("%s could not fit one line in one answer: %s.", t.Name(), faultSaid)), nil
	}
	return compactAnswer(rendered, "Narrow the search with repo: or path:.")
}

// searchPageHits is the most lines a page asks the search for
// (codesearch.Query.Limit): what one tool answer holds of the smallest lines
// carrying text (one-byte repository, path and line, fenced, one match), as
// TestCodeSearchAsksForWhatOnePageHolds measures. More would have the Gitlet
// nodes send lines no page can carry, for the next page to read again; fewer
// would cut a search of short lines into needless pages. Longer lines fill a
// page sooner, wasting under a page's worth. Under gitlet.MaxSearchHits (500),
// which a page of the shortest lines could not carry.
const searchPageHits = 290

// searchContextPageHits is searchPageHits for a page asking `context`: what
// one answer holds of the smallest lines with three fenced one-byte lines on
// each side, measured by the same test.
const searchContextPageHits = 70

// searchBounds is a call's `context` and `per_file`, 0 where unset, or the
// refusal of one that is not a whole number in its range, before anything
// is read.
func searchBounds(tool string, args map[string]any) (lines, perFile int, refusal *tools.Result) {
	for _, b := range []struct {
		name string
		most int
		into *int
	}{{"context", gitlet.MaxSearchContext, &lines}, {"per_file", gitlet.MaxSearchHits, &perFile}} {
		raw, given := args[b.name]
		if !given || raw == nil {
			continue
		}
		n, whole := argIntValue(raw)
		if !whole || n < 0 || n > b.most {
			r := failed(fmt.Sprintf("%s: `%s` is a whole number from 0 to %d.", tool, b.name, b.most))
			return 0, 0, &r
		}
		*b.into = n
	}
	return lines, perFile, nil
}

// searchPage is the fitLabelled builder of the page of answer's first k hits:
// each hit's text fenced, the commit each repository with a hit on the page
// was searched at, the coverage, and — where lines may follow the page's
// last — the cursor of the page after it.
func searchPage(turn *turnctx.Turn, seat string, search codeindex.Query, perFile int,
	answer codesearch.Answer) func(k int) (*foreignRead, any) {

	return func(k int) (*foreignRead, any) {
		read := newForeignRead(turn, seat)
		page := codeSearchPage{Hits: answer.Hits[:k], Coverage: answer.Coverage}
		for i, hit := range page.Hits {
			// Nobody this read can name vouches for a file's text.
			at := fieldPath{}.key("hits").index(i)
			read.fenceRepository(at.key("text"), provenance.SourceRepoFile, hit.Text)
			for _, side := range []struct {
				key   string
				lines []string
			}{{"before", hit.Before}, {"after", hit.After}} {
				for j, line := range side.lines {
					read.fenceRepository(at.key(side.key).index(j), provenance.SourceRepoFile, line)
				}
			}
			if tip, ok := answer.Tips[hit.Repo]; ok {
				if page.Tips == nil {
					page.Tips = map[string]string{}
				}
				page.Tips[hit.Repo] = tip
			}
		}
		// A next page where lines may follow: the page was cut, or the
		// search stopped (Truncated). codesearch's merge answers every line
		// before the search's last, so the next page misses none. k == 0 is
		// fitLabelled's bare probe, never a page sent.
		if k > 0 && (k < len(answer.Hits) || answer.Coverage.Truncated) {
			last := answer.Hits[k-1]
			page.Next = searchCursorOf(search, perFile, codesearch.KeyOf(last, answer.Tips[last.Repo]))
		}
		return read, page
	}
}

// searchArgs is the search a call names — `query` in the grammar, the
// filters beside it joined by the grammar's own rule, the mode and the case —
// or the refusal naming what the grammar refuses, before anything is read.
func searchArgs(tool string, args map[string]any) (codeindex.Query, *tools.Result) {
	text := argString(args, "query")
	if strings.TrimSpace(text) == "" {
		r := failed(fmt.Sprintf("%s needs `query`: the text to search for, after any repo:, path: and "+
			"ref: filters.", tool))
		return codeindex.Query{}, &r
	}
	search, err := codeindex.ParseQuery(text)
	if err == nil {
		err = search.Beside(argString(args, "repo"), argString(args, "path"), argString(args, "ref"))
	}
	if err == nil {
		if mode := strings.TrimSpace(argString(args, "mode")); mode != "" {
			search.Mode = codeindex.Mode(mode)
		}
		search.IgnoreCase = argBool(args, "ignore_case")
		err = search.Validate()
	}
	if err != nil {
		r := refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused the search: %s. Change it — the "+
			"same one is refused again.", tool, searchWords(err)))
		return codeindex.Query{}, &r
	}
	return search, nil
}

// searchWords is a grammar refusal's own sentence: what was refused and what
// to change, which the grammar writes for whoever wrote the search.
func searchWords(err error) string {
	if qe, ok := errors.AsType[*codeindex.QueryError](err); ok {
		return qe.Reason
	}
	return err.Error()
}

// searchHolds resolves, for a seat's turn, each repository search names that
// the page searches, holding a scratch one ([CodeDeps.repo]), and the ref in
// each, holding a seat's branch ([CodeDeps.refsOf]); it answers the floor
// ([gitlet.RefsFloor]) of this node's own pushes to them and every push that
// set the ref, so a seat finds what it pushed. A search naming no repository
// floors at nothing, and is refused a ref that may be a seat's branch: read in
// every repository, held in none.
//
// A page after a key resolves no repository before the key's, nor a ref in the
// key's own (read at the key's commit). A ref resolves once a turn per
// repository ([searchRefKey]), a floor for later pages, never a pin; own pushes
// are reread every page (local and free), so a push between pages is found.
func (d CodeDeps) searchHolds(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	search codeindex.Query, after *codesearch.Key) (statelog.Freshness, *tools.Result) {

	named := search.Ref != "" && !gitlet.ValidRev(search.Ref)
	if named && len(search.Repos) == 0 && (gitlet.Holdable(search.Ref) || gitlet.Holdable("refs/heads/"+search.Ref)) {
		r := failed(fmt.Sprintf("%s: ref %s may be a seat's branch, which a turn holds while it reads it, "+
			"and a search naming no repository would read it in every one. Name the repositories to search "+
			"it in with repo:.", tool, search.Ref))
		return statelog.Freshness{}, &r
	}
	var (
		own  statelog.Position
		refs []gitlet.ResolvedRef
	)
	for _, name := range search.Repos {
		if after != nil && name < after.Repo {
			continue
		}
		repo, refusal := d.repo(ctx, turn, tool, c, name)
		if refusal != nil {
			return statelog.Freshness{}, refusal
		}
		own = furthestOnLog(own, d.Code.FloorOf(repo.ID))
		if !named || after != nil && name == after.Repo {
			continue
		}
		key := searchRefKey{asker: c.asker, repoID: repo.ID, ref: search.Ref}
		if held, ok := turn.LookedUp().Load(key); ok {
			if live, ok := held.([]gitlet.ResolvedRef); ok {
				refs = append(refs, live...)
				continue
			}
		}
		answer, refusal := d.refsOf(ctx, turn, tool, c, repo, search.Ref)
		if refusal != nil {
			return statelog.Freshness{}, refusal
		}
		var live []gitlet.ResolvedRef
		for _, ref := range answer.Refs {
			if ref.Missing == "" {
				live = append(live, ref)
			}
		}
		// LoadOrStore, as a fact is kept ([turnctx.Lookups]): a racing page
		// of the same search (the turn's workers) keeps the first resolution;
		// this page floors at its own, the freshest it holds.
		turn.LookedUp().LoadOrStore(key, live)
		refs = append(refs, live...)
	}
	return gitlet.RefsFloor(own, refs...), nil
}

// searchRefKey keys, in a turn's lookups, a search's ref as one repository
// resolved it for asker ([CodeDeps.searchHolds]): the live refs, held — a fact
// that floors later pages and decides nothing, so it is taken with
// LoadOrStore; only [CodeDeps.repoRead] Stores, once the turn's hold is taken.
type searchRefKey struct {
	asker  gitlet.Principal
	repoID string
	ref    string
}

// searchFailure explains a search that did not answer: a refusal over one
// repository it names in Gitlet's own words ([codeFailure]), a search
// the grammar or Gitlet refused as asked, a condition of the node or
// the fleet as unavailable, and anything else as this node's fault.
func searchFailure(ctx context.Context, tool string, err error) tools.Result {
	var refusal *codesearch.RepoRefusal
	switch {
	case errors.As(err, &refusal):
		return codeFailure(ctx, tool, refusal.Repo, err)
	case errors.Is(err, gitlet.ErrInvalid):
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused the search: %s. Change it — the "+
			"same one is refused again.", tool, domainWords(err)))
	}
	if why, ok := Condition(err); ok {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not search right now (%s). "+
			"This is NOT an answer about the code: try again shortly.", tool, why))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s could not search the company's code: %s. This is NOT an "+
		"answer about the code: say you could not search it.", tool, faultSaid))
}

// searchCursor is what a code_search cursor carries: the search it was cut
// from, by its fingerprint, and the key of the last hit sent
// (codesearch.Key) — the repository, the commit it was searched at, the path
// as the hit names it (its text, or its exact bytes where they are not
// UTF-8) and the line — which the next page's search continues after.
type searchCursor struct {
	Search  string `json:"s"`
	Repo    string `json:"r"`
	Tip     string `json:"t"`
	Path    string `json:"p,omitempty"`
	PathB64 string `json:"b,omitempty"`
	Line    int    `json:"l"`
}

// searchFingerprint names one search by its parts (repo: and path: sorted, as
// either order searches alike) and its per_file bound, which changes the lines
// a page answers, so a cursor of another search is told apart: sixteen hex
// digits of SHA-256, a name and not a secret. `context` changes no line.
func searchFingerprint(q codeindex.Query, perFile int) string {
	repos, paths := slices.Sorted(slices.Values(q.Repos)), slices.Sorted(slices.Values(q.Paths))
	fields := []any{q.Text, q.Mode, q.IgnoreCase, repos, paths, q.Ref}
	if perFile > 0 {
		fields = append(fields, perFile)
	}
	data, _ := json.Marshal(fields)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// searchCursorOf is the cursor of the page after key, in search.
func searchCursorOf(search codeindex.Query, perFile int, key codesearch.Key) string {
	data, _ := json.Marshal(searchCursor{Search: searchFingerprint(search, perFile), Repo: key.Repo, Tip: key.Tip,
		Path: key.Path, PathB64: key.PathB64, Line: key.Line})
	return base64.RawURLEncoding.EncodeToString(data)
}

// searchCursorArg is the key a call's cursor names in search — the last hit
// a page sent, which the search continues after — nil for the first page, or
// the refusal of a cursor that is not one of search's.
func searchCursorArg(tool string, search codeindex.Query, perFile int, cursor string) (*codesearch.Key,
	*tools.Result) {
	if cursor == "" {
		return nil, nil
	}
	var c searchCursor
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err == nil {
		err = json.Unmarshal(data, &c)
	}
	key := codesearch.Key{Repo: c.Repo, Tip: c.Tip, SearchAfter: gitlet.SearchAfter{Path: c.Path,
		PathB64: c.PathB64, Line: c.Line}}
	if err == nil {
		_, err = key.Raw()
	}
	if err != nil || !gitlet.ValidName(c.Repo) || !gitref.ValidOID(c.Tip) {
		r := failed(fmt.Sprintf("%s: `cursor` is not a page of a search — hand back the `next` a "+
			"page answered, unchanged.", tool))
		return nil, &r
	}
	if c.Search != searchFingerprint(search, perFile) {
		r := failed(fmt.Sprintf("%s: `cursor` is a page of another search — hand it back with the same "+
			"`query`, `mode`, `ignore_case`, `repo`, `path`, `ref` and `per_file` it was answered for.", tool))
		return nil, &r
	}
	return &key, nil
}
