package queries_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
)

// stubCode is the code host's rows as the code questions find them: a node
// serving them, whose read records every query it is asked — the WHOLE query,
// so a question that carried the level and dropped the floors or the bound
// beside it is visible — and answers what answer does, or an empty answer at
// the level asked where answer is nil; and the usage domain's readers of a
// repository. Where unserved is set, the node serves none of them, and says
// that.
type stubCode struct {
	answer   func(code.ReadQuery) (code.ReadAnswer, error)
	asked    []code.ReadQuery
	unserved error
	// git answers a read by object id, each recorded in read; an empty
	// answer of the kind asked where git is nil. floor is this node's own
	// floor on every repository.
	git     func(code.GitQuery) (code.GitAnswer, error)
	read    []code.GitQuery
	floor   statelog.Position
	readers stubReaders
	// sourced counts the asks of the source itself: CodeReads and
	// RepoReaders.
	sourced int
}

// stubReaders is the usage domain's readers of a repository: an empty page,
// every ask counted.
type stubReaders struct{ paged int }

func (r *stubReaders) RepoReaders(context.Context, usage.RepoReadsQuery, string, int) (
	estate.RepoReadersAnswer, error) {

	r.paged++
	return estate.RepoReadersAnswer{}, nil
}

// CodeReads is the stub itself: this node serves the code host's rows, or
// why it does not.
func (s *stubCode) CodeReads() (queries.CodeReads, error) {
	s.sourced++
	if s.unserved != nil {
		return nil, s.unserved
	}
	return s, nil
}

// RepoReaders is the stub's readers, on CodeReads' terms.
func (s *stubCode) RepoReaders() (builtin.ReadersDeps, error) {
	s.sourced++
	if s.unserved != nil {
		return builtin.ReadersDeps{}, s.unserved
	}
	return builtin.ReadersDeps{Readers: &s.readers}, nil
}

func (s *stubCode) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	s.asked = append(s.asked, q)
	if s.answer != nil {
		return s.answer(q)
	}
	return code.ReadAnswer{Level: q.Fresh.Level}, nil
}

func (s *stubCode) GitRead(_ context.Context, q code.GitQuery) (code.GitAnswer, error) {
	s.read = append(s.read, q)
	if s.git != nil {
		return s.git(q)
	}
	return code.GitAnswer{Kind: q.Kind, RepoID: q.RepoID}, nil
}

func (s *stubCode) FloorOf(string) statelog.Position { return s.floor }

// last is the newest query asked, the zero query where none was.
func (s *stubCode) last() code.ReadQuery {
	if len(s.asked) == 0 {
		return code.ReadQuery{}
	}
	return s.asked[len(s.asked)-1]
}

// codeQuestions are the code host's rows as questions — the kind of read each
// asks, and the arguments each needs to answer at all.
var codeQuestions = []struct {
	what string
	kind code.ReadKind
	args map[string]any
}{
	{"code_repos", code.ReadRepos, map[string]any{}},
	{"code_repo", code.ReadRepo, map[string]any{"name": "web"}},
	{"code_pushes", code.ReadPushes, map[string]any{"name": "web"}},
	{"code_changes", code.ReadChanges, map[string]any{"repo": "web"}},
	{"code_change", code.ReadChange, map[string]any{"name": "web", "change": "1"}},
	{"code_change_routing", code.ReadChangeRouting, map[string]any{"name": "web", "change": "1"}},
	{"code_reviews", code.ReadReviews, map[string]any{"reviewer": "dev"}},
}

// A CODE QUESTION HANDS THE READER THE CALLER'S WHOLE FRESHNESS: the level,
// every floor named — one per log, so the reader that waits on the code log's
// can refuse one on any other as the request's mistake, which a floor it was
// never handed it cannot — and a staleness bound beside a stale read, read
// through the grammar every question reads them by. That is what makes a
// screen's read of its own code write back honest: it names the position the
// write answered with, and the code log's reader waits for that position
// rather than answer from before it. What is not a freshness at all — a bare
// `session`, a floor that is not a position, a bound beside a level it does not
// bound — is refused as the request's mistake before the reader is asked.
//
// Mutation: hand the reader the level alone (codeReadQuery building its Fresh
// from the grammar's Level) → every floor and bound goes missing, red.
func TestACodeQuestionHandsTheReaderTheCallersWholeFreshness(t *testing.T) {
	t.Parallel()
	floor := statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: 4711}
	other := statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 99}
	for _, q := range codeQuestions {
		with := func(params map[string]any) map[string]any {
			all := map[string]any{}
			for k, v := range params {
				all[k] = v
			}
			for k, v := range q.args {
				all[k] = v
			}
			return all
		}
		for _, tc := range []struct {
			name   string
			params map[string]any
			want   statelog.Freshness
		}{
			{"nothing asked", nil, statelog.Freshness{Level: statelog.ReadStale}},
			{"session at the code log's floor",
				map[string]any{"read_level": "session", "min_position": floor.String()},
				statelog.Freshness{Level: statelog.ReadSession, Floors: []statelog.Position{floor}}},
			{"a floor on each of two logs",
				map[string]any{"read_level": "session", "min_position": []any{floor.String(), other.String()}},
				statelog.Freshness{Level: statelog.ReadSession, Floors: []statelog.Position{floor, other}}},
			{"linearizable", map[string]any{"read_level": "linearizable"},
				statelog.Freshness{Level: statelog.ReadLinearizable}},
			{"stale, floored", map[string]any{"read_level": "stale", "min_position": floor.String()},
				statelog.Freshness{Level: statelog.ReadStale, Floors: []statelog.Position{floor}}},
			{"stale, bounded", map[string]any{"max_lag_seconds": "30", "max_lag_seq": "250"},
				statelog.Freshness{Level: statelog.ReadStale, MaxLag: 30 * time.Second, MaxLagSeq: 250}},
		} {
			stub := &stubCode{}
			if _, err := askNative(t, queries.Sources{Code: stub}, q.what, with(tc.params)); err != nil {
				t.Errorf("%s, %s: %v", q.what, tc.name, err)
				continue
			}
			if got := stub.last(); len(stub.asked) != 1 || got.Kind != q.kind ||
				!reflect.DeepEqual(got.Fresh, tc.want) {
				t.Errorf("%s, %s: the reader was asked %d times, last %s at %+v; want once, %s at %+v",
					q.what, tc.name, len(stub.asked), got.Kind, got.Fresh, q.kind, tc.want)
			}
		}
		for name, params := range map[string]map[string]any{
			"a bare session":                 {"read_level": "session"},
			"a floor that is not a position": {"min_position": "4711"},
			"a bound beside linearizable":    {"read_level": "linearizable", "max_lag_seq": "250"},
			"a level that is not one":        {"read_level": "eventual"},
		} {
			stub := &stubCode{}
			if _, err := askNative(t, queries.Sources{Code: stub}, q.what, with(params)); !errors.Is(err,
				queries.ErrBadParams) || len(stub.asked) != 0 {
				t.Errorf("%s, %s: answered %v having asked the reader %d times, want bad_params before it",
					q.what, name, err, len(stub.asked))
			}
		}
	}
}

// ONE REPOSITORY'S QUESTION ASKS FOR ITS PATH OWNERS AND ITS RULES' MERGE
// GATES, and no other does: its detail is where an API reader learns who owns
// which of its paths and what a merge into each protected branch needs
// (code.ReadQuery.PathOwners, code.ReadQuery.Gates) — no listing answers a
// gate — and the reader refuses either ask on every other kind of read.
//
// Mutations: ask every question for path owners — the listing and the history
// ask a read that refuses it, red; ask the detail for no gates — it answers
// none of a gated repository's, red.
func TestOnlyTheRepositoryQuestionAsksForPathOwnersAndGates(t *testing.T) {
	t.Parallel()
	for _, q := range codeQuestions {
		stub := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: stub}, q.what, q.args); err != nil {
			t.Fatalf("%s: %v", q.what, err)
		}
		if got, want := stub.last().PathOwners, q.kind == code.ReadRepo; got != want {
			t.Errorf("%s asked the reader for path owners: %t, want %t", q.what, got, want)
		}
		if got, want := stub.last().Gates, q.kind == code.ReadRepo; got != want {
			t.Errorf("%s asked the reader for its rules' merge gates: %t, want %t", q.what, got, want)
		}
	}
}

// A CODE QUESTION REFUSES IN THIS SURFACE'S VOCABULARY, which every transport
// renders: a repository nobody declared is an absence that says which
// (`not_declared`, the sentence naming the setting that declares one); one the
// asker's grant does not cover is a refusal of the CALLER carrying the reader's
// own sentence rather than the sentinel's Go text — answered by this node or
// wrapped by the router from another; a level, a cursor, a parameter or a
// floor the reader refuses is the request's mistake; and a read this node
// could not serve is `unavailable`. Told apart by type and sentinel, so the
// REST envelope and the socket's frame each render one refusal one way.
//
// Mutation: drop codeRefusal's ErrRepoNotFound arm → a repository nobody
// declared reaches the caller as an unclassified failure, red.
func TestACodeQuestionRefusesInThisSurfacesVocabulary(t *testing.T) {
	t.Parallel()
	const shutOut = `seat "dev" holds no access to repository "secret": it is visibility: unit`
	notPermitted := func(t *testing.T, _ string, err error) {
		t.Helper()
		var denied *queries.NotPermitted
		if !errors.As(err, &denied) || denied.Detail != shutOut || !errors.Is(err, queries.ErrUnauthorized) {
			t.Errorf("answered %v, want a refusal of the caller carrying the reader's sentence alone", err)
		}
	}
	is := func(sentinel error) func(*testing.T, string, error) {
		return func(t *testing.T, _ string, err error) {
			t.Helper()
			if !errors.Is(err, sentinel) {
				t.Errorf("answered %v, want %v", err, sentinel)
			}
		}
	}
	for _, tc := range []struct {
		name string
		err  error
		// check judges the answer to a read of repo that the reader
		// refused with err.
		check func(t *testing.T, repo string, err error)
	}{
		{"not declared", code.ErrRepoNotFound, func(t *testing.T, repo string, err error) {
			t.Helper()
			var missing *queries.NotFound
			if !errors.As(err, &missing) || missing.Reason != queries.ReasonNotDeclared ||
				missing.Detail != queries.RepoNotDeclared(repo) || !errors.Is(err, queries.ErrNotFound) {
				t.Errorf("answered %v, want not_found (not_declared) naming the repository", err)
			}
		}},
		{"not permitted here", fmt.Errorf("%w: %s", code.ErrNotPermitted, shutOut), notPermitted},
		{"not permitted on another data node", fmt.Errorf("estate: data node n2 answered: %w",
			fmt.Errorf("%w: %s", code.ErrNotPermitted, shutOut)), notPermitted},
		{"a level", fmt.Errorf("%w: a session read names the position it must include", code.ErrBadLevel),
			is(queries.ErrBadParams)},
		{"a cursor", fmt.Errorf("%w: phase %q", code.ErrBadCursor, "x"), is(queries.ErrBadParams)},
		{"a parameter", fmt.Errorf("%w: limit: 9000 is past the 200 one page of pushes carries", code.ErrInvalid),
			is(queries.ErrBadParams)},
		{"a floor on another log", fmt.Errorf("code: this read's floor: %w", statelog.ErrForeignPosition),
			is(queries.ErrBadParams)},
		{"behind", &statelog.Refused{Code: statelog.RefuseBehind, Level: statelog.ReadSession,
			RetryAfter: 7 * time.Second}, is(queries.ErrUnavailable)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, q := range codeQuestions {
				stub := &stubCode{answer: func(code.ReadQuery) (code.ReadAnswer, error) {
					return code.ReadAnswer{}, tc.err
				}}
				_, err := askNative(t, queries.Sources{Code: stub}, q.what, q.args)
				tc.check(t, stub.last().Repo, err)
			}
		})
	}
}

// WHO READ A UNIT'S REPOSITORY IS DECIDED BEFORE ANYTHING IS READ, by its own
// verb (authz.ActionCodeRepoReaders) inside its answer: a caller holding every
// app and not admin is refused on authority — naming admin, the one permission
// that would admit them — alike by a node serving the code host and by one
// serving none of it, and neither the source, the repository's row nor its
// readers is asked; an admin is told why a node serves none, and answered by
// one that does.
//
// Mutation: ask the source before the decide (codeRepoReaders' first lines
// swapped) → the colleague on a node serving none is told `not_hosted` and
// the source is asked, red.
func TestWhoReadARepositoryIsDecidedBeforeAnythingIsRead(t *testing.T) {
	t.Parallel()
	company := func(q code.ReadQuery) (code.ReadAnswer, error) {
		return code.ReadAnswer{Level: q.Fresh.Level, Repo: &code.RepoDetail{RepoSummary: code.RepoSummary{
			RepoRef: code.RepoRef{Name: q.Repo, Visibility: code.VisibilityCompany}}}}, nil
	}
	hostsNone := &queries.NotFound{Reason: queries.ReasonNotHosted, Detail: "this company does not host code"}
	ask := func(t *testing.T, stub *stubCode, grants ...iam.Grant) (any, error) {
		t.Helper()
		r := queries.NewRegistry()
		queries.Register(r, queries.Sources{Code: stub})
		return r.Answer(asGrants(t, "", grants...), "code_repo_readers", map[string]any{"name": "web"})
	}
	for name, unserved := range map[string]error{"a node serving the code host": nil,
		"a node serving none of it": hostsNone} {
		stub := &stubCode{answer: company, unserved: unserved}
		_, err := ask(t, stub, iam.Apps...)
		var refusal *queries.Refusal
		if !errors.As(err, &refusal) || !slices.Equal(refusal.Grants, []iam.Grant{iam.GrantAdmin}) {
			t.Errorf("%s: a caller holding every app and not admin was answered %v, want refused naming admin",
				name, err)
		}
		if stub.sourced != 0 || len(stub.asked) != 0 || stub.readers.paged != 0 {
			t.Errorf("%s: a refused caller asked the source %d time(s), the repository %d and its readers %d",
				name, stub.sourced, len(stub.asked), stub.readers.paged)
		}
	}
	if _, err := ask(t, &stubCode{answer: company, unserved: hostsNone}, iam.GrantAdmin); !errors.Is(err,
		queries.ErrNotFound) {
		t.Errorf("an admin asking a node serving none was answered %v, want why: %v", err, hostsNone)
	}
	answer, err := ask(t, &stubCode{answer: company}, iam.GrantAdmin)
	if got, ok := answer.(builtin.ReadersAnswer); err != nil || !ok || got.Repo != "web" || got.Recorded {
		t.Errorf("an admin was answered %+v (%v), want the company repository's answer, not recorded", answer, err)
	}
}
