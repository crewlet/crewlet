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
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/codesearch"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE CODE SEARCH TOOL, `code_search`: the lines matching a search across every
// repository the caller may read, answered by the one search over the fleet's
// git nodes (internal/codesearch, which the engine asks: [CodeSearcher]) — the
// same search the `code_search` question answers a person's screen or script
// with, through the same grammar.
//
// # One grammar, in the text or beside it
//
// `query` is what a seat writes, filters first — repo:, path: and ref: — then
// the text (codeindex.ParseQuery); `mode`, `ignore_case` and one `repo`,
// `path` and `ref` ride beside it, each joined to the text's filters by the
// grammar's own rule (codeindex.Query.Beside), so a search reads one way
// whichever surface carries its filters and however they are spelled.
//
// # Judged where it is answered, for whoever woke the turn too
//
// It reads through [CodeDeps.caller], as every code read does: the asker is
// the caller ([CodeAskerOf]) — the seat in a turn, the person or the
// credential on the operator surface — and inside a turn somebody woke, that
// somebody is decided again by this tool's own authority row, a colleague
// seat riding on the search as its ceiling (codesearch.Query.CeilingSeat).
// Every git node that answers a repository judges both grants from its own
// chart, so a repository either may not read is never searched, and one a
// repo: filter names is refused in the code host's own sentence.
//
// # A turn holds what its search names
//
// A seat's turn resolves every repository its search names ONCE A TURN, through
// the one resolution every code tool's `repo` passes ([CodeDeps.repo]), which
// holds a scratch repository from the idle expiry before anything is asked
// about it; and resolves a ref it names in each of them through the one ref
// read ([CodeDeps.refsOf]), which holds a seat's branch the same way. A ref
// that could name a seat's branch is searched only in repositories the search
// names: one searched in every repository the seat may read is a branch held
// in none of them. Those reads also floor the search, as a read by object id
// is floored ([code.RefsFloor]): each copy searched holds this node's own
// pushes to the repositories named and every push that set the ref named, so
// a seat that pushed and then searched finds what it pushed. An unfiltered
// search is floored at nothing — each copy as it stands, the question's own
// rule — and its answer names the commit each repository was searched at.
//
// # Fenced, and paged by its own order
//
// A line is a file's text, which nobody a read by object id can name vouches
// for, so each hit's text goes out inside a fence of the turn's boundary at
// trust `external`, naming no writer (ADR-0040) — `repo_file`, as code_read's
// lines. A search answers at most code.MaxSearchHits lines in all, in the
// order of their repositories' names, their paths' bytes and their numbers,
// and those lines fenced are more than one tool answer, so the answer is cut,
// whole hits, at [ToolAnswerBytes] as it is sent ([fitLabelled]), and `next`
// names the last hit sent. The next page searches again and answers the hits
// AFTER it in that order — keyed by the hit, never by a count, so a page is
// never a line answered twice — and a cursor names the search it was cut
// from, so one handed to another search is refused rather than read as a
// place in it.

// CodeSearchTool is the code search's name: its authority row's action.
const CodeSearchTool = string(authz.ActionCodeSearch)

// CodeSearcher is the code search over the fleet's git nodes, as the engine
// asks it (engine.Engine.CodeSearch, over codesearch.Searcher). Declared here
// by its consumer.
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
		"was not searched, and `truncated` means narrow it with repo: or path:. A page at a time — " +
		"pass `next` back as `cursor` with the same search."
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
			"cursor":      codeCursorArg(),
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
	after, refusal := searchCursorArg(t.Name(), search, strings.TrimSpace(argString(args, "cursor")))
	if refusal != nil {
		return *refusal, nil
	}
	q := codesearch.Query{Search: search, Asker: c.asker, CeilingSeat: c.ceiling, Rows: seatRead}
	if turn != nil && c.asker.Kind == code.PrincipalAgent {
		// A SEAT'S SEARCH NAMES ITS TURN, whose bucket on each answering
		// git node pays the search's second look at the log's head, as a
		// read by object id's does ([CodeDeps.gitRead]).
		q.Turn = turn.RunID
		if q.Git, refusal = t.deps.searchHolds(ctx, turn, t.Name(), c, search); refusal != nil {
			return *refusal, nil
		}
	}
	answer, err := t.deps.Searcher.CodeSearch(ctx, q)
	if err != nil {
		return searchFailure(ctx, t.Name(), err), nil
	}
	hits := answer.Hits
	if after != nil {
		// THE HITS AFTER THE LAST ONE SENT, by the order the search answers
		// in: the search is asked again whole, and what an earlier page
		// answered is left out by its key, never by a count.
		hits = slices.DeleteFunc(slices.Clone(hits), func(h codesearch.Hit) bool {
			return codesearch.CompareHits(*after, h) >= 0
		})
	}
	build := func(k int) (*foreignRead, any) {
		read := newForeignRead(turn, c.asker.Seat)
		page := codeSearchPage{Hits: hits[:k], Coverage: answer.Coverage}
		for i, hit := range page.Hits {
			// A LINE IS A FILE'S TEXT, which nobody this read can name
			// vouches for: fenced, at the least trust.
			read.fence(fieldPath{}.key("hits").index(i).key("text"), provenance.SourceRepoFile, nil, false,
				hit.Text)
			if tip, ok := answer.Tips[hit.Repo]; ok {
				if page.Tips == nil {
					page.Tips = map[string]string{}
				}
				page.Tips[hit.Repo] = tip
			}
		}
		// A PROBE OF NO HITS names no place: it is the bare answer
		// fitLabelled measures first, never a page sent.
		if k > 0 && k < len(hits) {
			page.Next = searchCursorOf(search, hits[k-1])
		}
		return read, page
	}
	k, rendered, err := fitLabelled(len(hits), build)
	switch {
	case err != nil:
		return labelFault(ctx, t.Name(), err), nil
	case k == 0 && len(hits) > 0:
		// A HIT is a path, a line of at most code.MaxSearchLineBytes and a
		// blob's id, fenced; and the coverage lists at most
		// codesearch.MaxCoverageNames names a set — together far under one
		// answer, so this is a bound broken, never a search.
		return faulted(ctx, t.Name(), errors.New("builtin: one search hit beside its coverage is past one "+
			"tool answer"), fmt.Sprintf("%s could not fit one line in one answer: %s.", t.Name(), faultSaid)), nil
	}
	return compactAnswer(rendered, "Narrow the search with repo: or path:.")
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

// searchHolds resolves, for a seat's turn, every repository search names —
// holding a scratch one ([CodeDeps.repo]) — and the ref it names in each,
// holding a seat's branch ([CodeDeps.refsOf]), and answers the floor each
// copy searched is brought to: this node's own pushes to those repositories
// and every push that set the ref there ([code.RefsFloor]). A ref that could
// be a seat's branch, in a search naming no repository, is refused: it would
// be read in every repository the seat may read and held in none.
func (d CodeDeps) searchHolds(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller,
	search codeindex.Query) (statelog.Freshness, *tools.Result) {

	named := search.Ref != "" && !code.ValidRev(search.Ref)
	if named && len(search.Repos) == 0 && (code.Holdable(search.Ref) || code.Holdable("refs/heads/"+search.Ref)) {
		r := failed(fmt.Sprintf("%s: ref %s may be a seat's branch, which a turn holds while it reads it, "+
			"and a search naming no repository would read it in every one. Name the repositories to search "+
			"it in with repo:.", tool, search.Ref))
		return statelog.Freshness{}, &r
	}
	var (
		own  statelog.Position
		refs []code.ResolvedRef
	)
	for _, name := range search.Repos {
		repo, refusal := d.repo(ctx, turn, tool, c, name)
		if refusal != nil {
			return statelog.Freshness{}, refusal
		}
		own = furthestOnLog(own, d.Code.FloorOf(repo.ID))
		if !named {
			continue
		}
		answer, refusal := d.refsOf(ctx, turn, tool, c, repo, search.Ref)
		if refusal != nil {
			return statelog.Freshness{}, refusal
		}
		for _, ref := range answer.Refs {
			if ref.Missing == "" {
				refs = append(refs, ref)
			}
		}
	}
	return code.RefsFloor(own, refs...), nil
}

// searchFailure explains a search that did not answer: a refusal over one
// repository it names in the code host's own words ([codeFailure]), a search
// the grammar or the code host refused as asked, a condition of the node or
// the fleet as unavailable, and anything else as this node's fault.
func searchFailure(ctx context.Context, tool string, err error) tools.Result {
	var refusal *codesearch.RepoRefusal
	switch {
	case errors.As(err, &refusal):
		return codeFailure(ctx, tool, refusal.Repo, err)
	case errors.Is(err, code.ErrInvalid):
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
// from, by its fingerprint, and the last hit sent — its place, the
// repository, the path as the hit names it (its text, and its exact bytes
// where they are not UTF-8) and the line — which the next page compares by
// the search's own order ([codesearch.CompareHits]), never a copy of it.
type searchCursor struct {
	Search  string `json:"s"`
	Repo    string `json:"r"`
	Path    string `json:"p"`
	PathB64 string `json:"b,omitempty"`
	Line    int    `json:"l"`
}

// searchFingerprint names one search by its parts — its repo: and path:
// filters as the sets they are, sorted, since either order searches alike —
// so a cursor cut from it is told from one cut from any other: sixteen hex
// digits of its SHA-256, a name and not a secret.
func searchFingerprint(q codeindex.Query) string {
	repos, paths := slices.Sorted(slices.Values(q.Repos)), slices.Sorted(slices.Values(q.Paths))
	data, _ := json.Marshal([]any{q.Text, q.Mode, q.IgnoreCase, repos, paths, q.Ref})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// searchCursorOf is the cursor of the page after h, in search.
func searchCursorOf(search codeindex.Query, h codesearch.Hit) string {
	data, _ := json.Marshal(searchCursor{Search: searchFingerprint(search), Repo: h.Repo, Path: h.Path,
		PathB64: h.PathB64, Line: h.Line})
	return base64.RawURLEncoding.EncodeToString(data)
}

// searchCursorArg is the place a call's cursor names in search — the last
// hit sent, as far as the search's order reads one — nil for the first page,
// or the refusal of a cursor that is not one of search's.
func searchCursorArg(tool string, search codeindex.Query, cursor string) (*codesearch.Hit, *tools.Result) {
	if cursor == "" {
		return nil, nil
	}
	var c searchCursor
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err == nil {
		err = json.Unmarshal(data, &c)
	}
	if err == nil && c.PathB64 != "" {
		_, err = base64.StdEncoding.DecodeString(c.PathB64)
	}
	if err != nil || c.Line < 1 || c.Repo == "" || c.Path == "" {
		r := failed(fmt.Sprintf("%s: `cursor` is not a page of a search — hand back the `next` a "+
			"page answered, unchanged.", tool))
		return nil, &r
	}
	if c.Search != searchFingerprint(search) {
		r := failed(fmt.Sprintf("%s: `cursor` is a page of another search — hand it back with the same "+
			"`query`, `mode`, `ignore_case`, `repo`, `path` and `ref` it was answered for.", tool))
		return nil, &r
	}
	return &codesearch.Hit{Repo: c.Repo, SearchHit: code.SearchHit{Path: c.Path, PathB64: c.PathB64,
		Line: c.Line}}, nil
}
