package queries_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/codesearch"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// CODE SEARCH IS NOT A SESSION QUERY: it answers at the tip it names. Each
// repository is searched at the commit its answering node's copy holds — the
// answer's `tips` — and a hit's file is read at that commit, so the search
// takes no floor: `read_level=session`, bare or beside a `min_position`, and
// a `min_position` at any level are refused `bad_params` before anything is
// searched, and the search reads every copy as it stands (no floor on the
// code log). What the caller names is the level its repositories are listed
// at: the REST read path's `stale` by default, `linearizable` asked. And it is
// no row of [sessionQuestions], the one table the dashboard's SESSION_QUERIES
// is held to both ways ([TestEverySessionQueryTakesAFreshnessFloor]) — so a
// client listing it there is refused by that gate, as a screen that believes
// it waited for its write.
//
// Mutation: take the caller's freshness, floor and all, as the search's
// listing and git floor, and the floored asks are searched, red; hand the
// search the caller's floor on the code log as its git floor, red.
func TestCodeSearchIsNotASessionQuery(t *testing.T) {
	t.Parallel()
	if slices.ContainsFunc(sessionQuestions, func(q sessionQuestion) bool { return q.what == "code_search" }) {
		t.Fatal("code_search is a row of sessionQuestions: it answers at the tip it names and takes no floor")
	}
	floor := topics.CodeLogStream + "@1:7"
	for _, params := range []map[string]any{
		{"q": "needle", "read_level": "session"},
		{"q": "needle", "read_level": "session", "min_position": floor},
		{"q": "needle", "read_level": "linearizable", "min_position": floor},
		{"q": "needle", "min_position": floor},
	} {
		s := &stubCode{}
		_, err := askNative(t, queries.Sources{Code: s}, "code_search", params)
		if !errors.Is(err, queries.ErrBadParams) || !strings.Contains(err.Error(), "takes no floor") {
			t.Errorf("code_search %v answered %v, want bad_params saying it takes no floor", params, err)
		}
		if len(s.searched) != 0 {
			t.Errorf("code_search %v searched %d times, want none", params, len(s.searched))
		}
	}
	for _, tc := range []struct {
		level string
		want  statelog.ReadLevel
	}{
		{"", statelog.ReadStale},
		{"linearizable", statelog.ReadLinearizable},
	} {
		s := &stubCode{}
		params := map[string]any{"q": "needle"}
		if tc.level != "" {
			params["read_level"] = tc.level
		}
		got, err := askNative(t, queries.Sources{Code: s}, "code_search", params)
		if err != nil {
			t.Fatalf("code_search at %q: %v", tc.level, err)
		}
		if _, immutable := got.(queries.Immutable); immutable {
			t.Errorf("code_search at %q was answered immutable: its tips move with their refs", tc.level)
		}
		if len(s.searched) != 1 {
			t.Fatalf("code_search at %q searched %d times, want once", tc.level, len(s.searched))
		}
		q := s.searched[0]
		if q.Rows.Level != tc.want || len(q.Rows.Floors) != 0 {
			t.Errorf("code_search at %q listed the repositories at %+v, want %s with no floor", tc.level,
				q.Rows, tc.want)
		}
		if !q.Git.Floor(topics.CodeLogStream).IsZero() || q.Git.Level != "" {
			t.Errorf("code_search at %q read the copies at %+v, want as each stands", tc.level, q.Git)
		}
	}
}

// THE DASHBOARD READS WHAT A CODE SEARCH ANSWERS: `contract/code.ts`'s
// `CodeSearchAnswer` and the rows it composes — a hit, the coverage, a set of
// repositories and a node that did not answer — declare exactly the fields
// `code_search` sends, each optional one where the answer may leave it out,
// read off an answer carrying every field — and every field the engine's JSON
// leaves out when empty declared optional there. A field the engine grew that
// the client lacks is one no screen can read; one the client keeps that the
// engine dropped, or declares required where the engine may leave it out, is
// read as undefined.
//
// Mutation: drop or rename a field on either side (`tips` from the
// interface, `path_b64` from the hit), red; declare `tips` required, red.
func TestTheDashboardReadsWhatACodeSearchAnswers(t *testing.T) {
	t.Parallel()
	set := codesearch.RepoSet{Count: 1, Names: []string{"web"}}
	s := &stubCode{search: func(codesearch.Query) (codesearch.Answer, error) {
		return codesearch.Answer{
			Hits: []codesearch.Hit{{Repo: "web", SearchHit: code.SearchHit{Path: "go.mod", PathB64: "Z28ubW9k",
				Line: 1, Text: "module web", Blob: stubOID}}},
			Tips: map[string]string{"web": stubOID},
			Coverage: codesearch.Coverage{Searched: 1, NotIndexed: set, Building: set,
				NotAnswering: []codesearch.NodeGap{{Node: "git-b", Why: "silent", Repos: set}},
				NotPermitted: set, GrepNotNamed: set, Stale: set, NoRef: set, SkippedFiles: 2, Truncated: true},
		}, nil
	}}
	raw, err := askNative(t, queries.Sources{Code: s}, "code_search", map[string]any{"q": "module"})
	if err != nil {
		t.Fatal(err)
	}
	body := asMap(t, raw)
	holdShape(t, "CodeSearchAnswer", []map[string]any{body}, false)
	holdShape(t, "CodeSearchHit", rowsOf(t, body["hits"]), false)
	coverage, ok := body["coverage"].(map[string]any)
	if !ok {
		t.Fatalf("the answer's coverage is %v, not an object", body["coverage"])
	}
	holdShape(t, "CodeSearchCoverage", []map[string]any{coverage}, false)
	gaps := rowsOf(t, coverage["not_answering"])
	holdShape(t, "CodeSearchNodeGap", gaps, false)
	var sets []map[string]any
	for _, key := range []string{"not_indexed", "building", "not_permitted", "grep_not_named", "stale", "no_ref"} {
		sets = append(sets, asMap(t, coverage[key]))
	}
	for _, gap := range gaps {
		sets = append(sets, asMap(t, gap["repos"]))
	}
	holdShape(t, "CodeSearchRepoSet", sets, false)

	// AND WHAT THE ENGINE MAY LEAVE OUT IS OPTIONAL THERE: a field its JSON
	// omits when empty (`tips` of a search that matched nothing, `path_b64`
	// of a UTF-8 path) declared required is a screen reading undefined as
	// a value on the answers that carry none — which an answer carrying
	// every field, as above, cannot show.
	tree := clientsource.Tree(t)
	for iface, typ := range map[string]reflect.Type{
		"CodeSearchAnswer": reflect.TypeFor[codesearch.Answer](),
		"CodeSearchHit":    reflect.TypeFor[codesearch.Hit](),
	} {
		members, err := clientsource.Interface(tree, iface)
		if err != nil {
			t.Fatal(err)
		}
		optional := map[string]bool{}
		for _, m := range members {
			optional[m.Name] = m.Optional
		}
		omitted := omittedWhenEmpty(typ)
		if len(omitted) == 0 {
			t.Fatalf("%s omits no field when empty, so this check asserts about nothing", typ)
		}
		for _, field := range omitted {
			if !optional[field] {
				t.Errorf("%s declares %s REQUIRED, and the engine leaves it out when empty", iface, field)
			}
		}
	}
}

// omittedWhenEmpty is the JSON names of typ's fields tagged omitempty, those
// of an embedded struct as its own, which JSON flattens.
func omittedWhenEmpty(typ reflect.Type) []string {
	var out []string
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, omittedWhenEmpty(f.Type)...)
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name != "" && name != "-" && slices.Contains(strings.Split(opts, ","), "omitempty") {
			out = append(out, name)
		}
	}
	return out
}

// A CODE SEARCH READS ONE GRAMMAR, in the text or beside it: `q` is parsed as
// a person writes it, filters first; `mode`, `ignore_case` and one `repo`,
// `path` and `ref` ride beside it, a repo or a path widening the search once
// however often it is named and a ref beside a different ref: refused, as two
// ref: filters are; `limit` is held to code.MaxSearchHits; and the search is
// asked as the caller. A refusal over one repository names it: a repo:
// nobody declared is `not_found` (`not_declared`) naming that repository.
//
// Mutation: drop the `repo` parameter, or add it twice, and the repositories
// asked differ, red; let a ref beside a different ref: replace it, red; name
// no repository in the not_declared sentence, red.
func TestCodeSearchReadsOneGrammar(t *testing.T) {
	t.Parallel()
	s := &stubCode{}
	_, err := askNative(t, queries.Sources{Code: s}, "code_search", map[string]any{
		"q":    "repo:web path:src/** ref:main retry budget",
		"mode": "word", "ignore_case": "true", "repo": "api", "path": "src/**", "ref": "main", "limit": "20"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.searched) != 1 {
		t.Fatalf("searched %d times, want once", len(s.searched))
	}
	q := s.searched[0]
	want := codeindex.Query{Text: "retry budget", Mode: codeindex.ModeWord, IgnoreCase: true,
		Repos: []string{"web", "api"}, Paths: []string{"src/**"}, Ref: "main"}
	if q.Search.Text != want.Text || q.Search.Mode != want.Mode || q.Search.IgnoreCase != want.IgnoreCase ||
		!slices.Equal(q.Search.Repos, want.Repos) || !slices.Equal(q.Search.Paths, want.Paths) ||
		q.Search.Ref != want.Ref || q.Limit != 20 {
		t.Errorf("searched %+v limit %d, want %+v limit 20", q.Search, q.Limit, want)
	}
	if q.Asker.OperatorID != "token:reader" {
		t.Errorf("searched as %+v, want the caller, token:reader", q.Asker)
	}

	for _, params := range []map[string]any{
		{},
		{"q": "   "},
		{"q": "needle", "mode": "glob"},
		{"q": "needle", "ignore_case": "yes"},
		{"q": "ref:main needle", "ref": "dev"},
		{"q": "needle", "limit": "501"},
		{"q": "needle", "limit": "-1"},
		{"q": "(", "mode": "regex"},
		{"q": "repo:Not_A_Name needle"},
	} {
		s := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: s}, "code_search", params); !errors.Is(err,
			queries.ErrBadParams) {
			t.Errorf("code_search %v answered %v, want bad_params", params, err)
		}
		if len(s.searched) != 0 {
			t.Errorf("code_search %v searched, want refused before", params)
		}
	}

	s = &stubCode{search: func(q codesearch.Query) (codesearch.Answer, error) {
		return codesearch.Answer{}, &codesearch.RepoRefusal{Repo: "nope", Err: code.ErrRepoNotFound}
	}}
	_, err = askNative(t, queries.Sources{Code: s}, "code_search", map[string]any{"q": "repo:web repo:nope needle"})
	var missing *queries.NotFound
	if !errors.As(err, &missing) || missing.Reason != queries.ReasonNotDeclared ||
		missing.Detail != queries.RepoNotDeclared("nope") {
		t.Errorf("a search naming an undeclared repository answered %v, want not_declared naming nope", err)
	}
}
