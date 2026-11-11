package queries_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/codesearch"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
)

// stubCode is Gitlet's rows as the code questions find them: a node
// serving them, whose read records every query it is asked — the WHOLE query,
// so a question that carried the level and dropped the floors or the bound
// beside it is visible — and answers what answer does, or an empty answer at
// the level asked where answer is nil; and the usage domain's readers of a
// repository. Where unserved is set, the node serves none of them, and says
// that.
type stubCode struct {
	answer   func(gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	asked    []gitlet.ReadQuery
	unserved error
	// git answers a read by object id, each recorded in read; an empty
	// answer of the kind asked where git is nil. floor is this node's own
	// floor on every repository.
	git     func(gitlet.GitQuery) (gitlet.GitAnswer, error)
	read    []gitlet.GitQuery
	floor   statelog.Position
	readers stubReaders
	// refs answers a resolve of refs by name, each recorded in resolved —
	// every name a ref of its own at stubOID where refs is nil — and
	// preview a merge preview, each recorded in previewed: clean where it is
	// nil.
	refs      func(gitlet.RefQuery) (gitlet.RefAnswer, error)
	resolved  []gitlet.RefQuery
	preview   func(gitlet.MergePreviewQuery) (gitlet.MergePreviewAnswer, error)
	previewed []gitlet.MergePreviewQuery
	// logs is the object store a check's log is read from: its bytes stand
	// for every object, each page asked recorded in paged; stubLog where it
	// is nil.
	logs  []byte
	paged []logPageAsk
	// search answers a code search, each recorded in searched: an empty
	// answer where it is nil.
	search   func(codesearch.Query) (codesearch.Answer, error)
	searched []codesearch.Query
	// sourced counts the asks of the source itself: CodeReads, RepoReaders,
	// CheckLogs and CodeSearcher.
	sourced int
	// mu guards asked and read, which a question's reads asked at once
	// append to from goroutines of their own (gitlet.AtOnce).
	mu sync.Mutex
}

// CodeSearcher is the stub itself, on CodeReads' terms.
func (s *stubCode) CodeSearcher() (queries.CodeSearcher, error) {
	s.sourced++
	if s.unserved != nil {
		return nil, s.unserved
	}
	return s, nil
}

// CodeSearch records q and answers what search does.
func (s *stubCode) CodeSearch(_ context.Context, q codesearch.Query) (codesearch.Answer, error) {
	s.searched = append(s.searched, q)
	if s.search != nil {
		return s.search(q)
	}
	return codesearch.Answer{}, nil
}

// logPageAsk is one page of a log the stub's object store was asked for.
type logPageAsk struct {
	object    objstore.Object
	off, size int64
}

// stubLog is the log the stub keeps where a case names none.
const stubLog = "--- FAIL: TestUpload (0.01s)\n    upload_test.go:12: no\n"

// stubLogKey is the object every attempt the stub answers keeps its log as.
var stubLogKey = objstore.KeyAt(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))

// CheckLogs is the stub's object store, on CodeReads' terms.
func (s *stubCode) CheckLogs() (queries.CheckLogs, error) {
	s.sourced++
	if s.unserved != nil {
		return nil, s.unserved
	}
	return s, nil
}

// ReadAt is a page of the stub's log, whatever object is named.
func (s *stubCode) ReadAt(_ context.Context, o objstore.Object, off, n int64) ([]byte, error) {
	s.paged = append(s.paged, logPageAsk{object: o, off: off, size: n})
	log := s.logs
	if log == nil {
		log = []byte(stubLog)
	}
	end := min(off+n, int64(len(log)))
	return append([]byte(nil), log[off:end]...), nil
}

// stubCheck is the attempt the stub answers a check read with: a failed run
// of the attempt q names, keeping a log of size bytes as stubLogKey.
func stubCheck(q gitlet.ReadQuery, size int64) *gitlet.CheckDetail {
	c := q.Checks
	return &gitlet.CheckDetail{CheckRow: gitlet.CheckRow{CheckKey: gitlet.CheckKey{RepoID: stubRepoID, OID: c.OID,
		Target: c.Target, Name: c.Name, Attempt: c.Attempt}, State: gitlet.CheckStateFailed},
		Log: &gitlet.CheckLog{Object: stubLogKey, Hash: objstore.Hash("sha256:stub"), Size: size}}
}

// stubReaders is the usage domain's readers of a repository: an empty page,
// every ask counted.
type stubReaders struct{ paged int }

func (r *stubReaders) RepoReaders(context.Context, usage.RepoReadsQuery, string, int) (
	estate.RepoReadersAnswer, error) {

	r.paged++
	return estate.RepoReadersAnswer{}, nil
}

// CodeReads is the stub itself: this node serves Gitlet's rows, or
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

// stubRepoID is the one repository the stub serves, whatever its name.
const stubRepoID = "0e8bd7a4-5a04-4e17-8f3c-7e7d4b2c9a11"

// stubOID is the commit every ref the stub resolves names.
const stubOID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// stubRepo is the repository the stub answers a read of name with: its
// default branch main.
func stubRepo(name string) *gitlet.RepoDetail {
	return &gitlet.RepoDetail{RepoSummary: gitlet.RepoSummary{RepoRef: gitlet.RepoRef{ID: stubRepoID, Name: name,
		DefaultBranch: "main", Visibility: gitlet.VisibilityCompany}}}
}

func (s *stubCode) Read(_ context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	s.mu.Lock()
	s.asked = append(s.asked, q)
	s.mu.Unlock()
	if s.answer != nil {
		return s.answer(q)
	}
	a := gitlet.ReadAnswer{Level: q.Fresh.Level}
	switch q.Kind {
	case gitlet.ReadRepo, gitlet.ReadRefs:
		a.Repo = stubRepo(q.Repo)
	case gitlet.ReadCommit:
		a.Repo, a.Commit = stubRepo(q.Repo), &gitlet.CommitChanges{Commit: q.Commit}
	case gitlet.ReadInbox:
		a.Inbox = &gitlet.InboxPage{Handle: q.Inbox.Handle}
	case gitlet.ReadCheck:
		size := int64(len(stubLog))
		if s.logs != nil {
			size = int64(len(s.logs))
		}
		a.Check = stubCheck(q, size)
	}
	return a, nil
}

func (s *stubCode) ResolveRef(_ context.Context, q gitlet.RefQuery) (gitlet.RefAnswer, error) {
	s.resolved = append(s.resolved, q)
	if s.refs != nil {
		return s.refs(q)
	}
	a := gitlet.RefAnswer{Repo: stubRepo(q.Repo).RepoRef}
	for _, name := range q.Refs {
		a.Refs = append(a.Refs, gitlet.ResolvedRef{Name: name, Ref: "refs/heads/" + name, OID: stubOID})
	}
	return a, nil
}

func (s *stubCode) GitRead(_ context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	s.mu.Lock()
	s.read = append(s.read, q)
	s.mu.Unlock()
	if s.git != nil {
		return s.git(q)
	}
	a := gitlet.GitAnswer{Kind: q.Kind, RepoID: q.RepoID}
	switch q.Kind {
	case gitlet.GitCompare:
		a.Compare = &gitlet.CompareAnswer{Base: q.Compare.Base, Head: q.Compare.Head}
	case gitlet.GitLog:
		a.Log = &gitlet.LogAnswer{Commits: []gitlet.LogCommit{{OID: q.Log.Rev}}}
	}
	return a, nil
}

func (s *stubCode) MergePreview(_ context.Context, q gitlet.MergePreviewQuery) (gitlet.MergePreviewAnswer, error) {
	s.previewed = append(s.previewed, q)
	if s.preview != nil {
		return s.preview(q)
	}
	return gitlet.MergePreviewAnswer{RepoID: q.RepoID, Base: q.Base, Head: q.Head, State: gitlet.MergeClean}, nil
}

func (s *stubCode) FloorOf(string) statelog.Position { return s.floor }

// first is the first query asked — a question's read at the caller's own
// freshness, before any read it floors at that one's position — the zero
// query where none was.
func (s *stubCode) first() gitlet.ReadQuery {
	if len(s.asked) == 0 {
		return gitlet.ReadQuery{}
	}
	return s.asked[0]
}

// last is the newest query asked, the zero query where none was.
func (s *stubCode) last() gitlet.ReadQuery {
	if len(s.asked) == 0 {
		return gitlet.ReadQuery{}
	}
	return s.asked[len(s.asked)-1]
}

// firstResolve is the first resolve of refs asked — a content question's one
// read of the rows where a revision names a ref, `HEAD` among them
// (gitlet.HeadRef) — the zero query where none was.
func (s *stubCode) firstResolve() gitlet.RefQuery {
	if len(s.resolved) == 0 {
		return gitlet.RefQuery{}
	}
	return s.resolved[0]
}

// codeQuestions are Gitlet's rows as questions — the kind of read each
// asks, and the arguments each needs to answer at all.
var codeQuestions = []struct {
	what string
	kind gitlet.ReadKind
	args map[string]any
}{
	{"code_repos", gitlet.ReadRepos, map[string]any{}},
	{"code_repo", gitlet.ReadRepo, map[string]any{"name": "web"}},
	{"code_pushes", gitlet.ReadPushes, map[string]any{"name": "web"}},
	{"code_changes", gitlet.ReadChanges, map[string]any{"repo": "web"}},
	{"code_change", gitlet.ReadChange, map[string]any{"name": "web", "change": "1"}},
	{"code_change_routing", gitlet.ReadChangeRouting, map[string]any{"name": "web", "change": "1"}},
	{"code_reviews", gitlet.ReadReviews, map[string]any{"reviewer": "dev"}},
	{"code_refs", gitlet.ReadRefs, map[string]any{"name": "web"}},
	{"code_queue", gitlet.ReadQueue, map[string]any{"repo": "web", "target": "refs/heads/main"}},
	{"code_checks", gitlet.ReadChecks, map[string]any{"name": "unit", "state": "failed"}},
	{"code_check", gitlet.ReadCheck, map[string]any{"name": "web", "oid": stubOID, "check": "unit",
		"attempt": "1", "target": "refs/heads/main"}},
	{"code_check_log", gitlet.ReadCheck, map[string]any{"name": "web", "oid": stubOID, "check": "unit",
		"attempt": "1", "target": "refs/heads/main"}},
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

// A QUESTION ASKS FOR A REPOSITORY'S PATH OWNERS AND ITS RULES' MERGE GATES
// ONLY WHERE ITS CALLER NAMES THEM — `path_owners=true`, `gates=true` — and
// each alone: at their bound they are a quarter and half a mebibyte of JSON,
// and the repository's question is mostly asked for its row, by a header a
// screen polls every minute. A spelling that is neither `true` nor `false`
// is refused before the reader is asked, never read as "not asked"
// (gitlet.ReadQuery.PathOwners, gitlet.ReadQuery.Gates).
//
// Mutations: ask the repository's question for path owners and gates
// whatever its caller named — the bare question asks for both, red; read
// `gates` from `path_owners` — the question naming one asks for the other,
// red; read an unknown spelling as false — it reaches the reader, red.
func TestAQuestionAsksForPathOwnersAndGatesOnlyWhereItsCallerNamesThem(t *testing.T) {
	t.Parallel()
	for _, q := range codeQuestions {
		stub := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: stub}, q.what, q.args); err != nil {
			t.Fatalf("%s: %v", q.what, err)
		}
		if got := stub.last(); got.PathOwners || got.Gates {
			t.Errorf("%s, naming neither, asked the reader for path owners %t and gates %t, want neither",
				q.what, got.PathOwners, got.Gates)
		}
	}
	for _, tc := range []struct {
		name          string
		args          map[string]any
		owners, gates bool
	}{
		{"path owners", map[string]any{"name": "web", "path_owners": "true"}, true, false},
		{"gates", map[string]any{"name": "web", "gates": "true"}, false, true},
		{"both", map[string]any{"name": "web", "path_owners": "true", "gates": "true"}, true, true},
		{"neither, spelled out", map[string]any{"name": "web", "path_owners": "false", "gates": "false"},
			false, false},
	} {
		stub := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_repo", tc.args); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := stub.last(); got.PathOwners != tc.owners || got.Gates != tc.gates {
			t.Errorf("code_repo naming %s asked for path owners %t and gates %t, want %t and %t",
				tc.name, got.PathOwners, got.Gates, tc.owners, tc.gates)
		}
	}
	for _, key := range []string{"path_owners", "gates"} {
		stub := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_repo",
			map[string]any{"name": "web", key: "yes"}); !errors.Is(err, queries.ErrBadParams) ||
			len(stub.asked) != 0 {
			t.Errorf("%s=yes: answered %v having asked the reader %d times, want bad_params before it",
				key, err, len(stub.asked))
		}
	}
}

// A LISTING ASKS FOR THE PROTECTED REPOSITORIES ALONE ONLY WHERE ITS CALLER
// NAMES THEM — `protected=true` — and otherwise for every repository, so
// Settings › Gitlet pages through the repositories a rule protects without the
// Gitlet workspace's listing losing any; a spelling that is neither `true` nor
// `false` is refused before the reader is asked (gitlet.ReadQuery.Protected).
//
// Mutations: read `protected` as nothing — the listing naming it asks for
// every repository, red; read it whatever the caller named — the bare listing
// asks for the protected alone, red; read an unknown spelling as false — it
// reaches the reader, red.
func TestAListingAsksForTheProtectedRepositoriesOnlyWhereItsCallerNamesThem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args map[string]any
		want bool
	}{
		{"nothing", nil, false},
		{"protected", map[string]any{"protected": "true"}, true},
		{"not protected, spelled out", map[string]any{"protected": "false"}, false},
	} {
		stub := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_repos", tc.args); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := stub.last(); got.Protected != tc.want {
			t.Errorf("code_repos naming %s asked for the protected repositories alone %t, want %t", tc.name,
				got.Protected, tc.want)
		}
	}
	stub := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_repos",
		map[string]any{"protected": "yes"}); !errors.Is(err, queries.ErrBadParams) || len(stub.asked) != 0 {
		t.Errorf("protected=yes: answered %v having asked the reader %d times, want bad_params before it", err,
			len(stub.asked))
	}
}

// A REPOSITORIES LISTING ASKS UNDER THE PREFIX ITS CALLER TYPED — a picker's
// text, trimmed as a refs listing's is — and under none where it names none,
// so the reader seeks the names the prefix begins (gitlet.ReadQuery.Prefix) and
// a picker finds a repository however many sort before it.
//
// Mutation: read `prefix` for a refs listing alone — the listing naming one
// asks for every repository, red; leave it untrimmed — " api- " reaches the
// reader, red.
func TestARepositoriesListingAsksUnderThePrefixItsCallerTyped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"nothing", nil, ""},
		{"a prefix", map[string]any{"prefix": "api"}, "api"},
		{"a prefix typed with spaces", map[string]any{"prefix": " api- "}, "api-"},
	} {
		stub := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_repos", tc.args); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := stub.last(); got.Prefix != tc.want {
			t.Errorf("code_repos naming %s asked under %q, want %q", tc.name, got.Prefix, tc.want)
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
		{"not declared", gitlet.ErrRepoNotFound, func(t *testing.T, repo string, err error) {
			t.Helper()
			var missing *queries.NotFound
			if !errors.As(err, &missing) || missing.Reason != queries.ReasonNotDeclared ||
				missing.Detail != queries.RepoNotDeclared(repo) || !errors.Is(err, queries.ErrNotFound) {
				t.Errorf("answered %v, want not_found (not_declared) naming the repository", err)
			}
		}},
		// AN ATTEMPT THE REPOSITORY HOLDS NO ROW OF is not_found
		// `no_check`, carrying the reader's sentence: the reason a screen
		// tells "never queued, or past its thirty days" from a repository
		// that is not declared.
		{"no such attempt", fmt.Errorf("%w: unit's attempt 3 of %s for refs/heads/main", gitlet.ErrCheckNotFound,
			stubOID), func(t *testing.T, _ string, err error) {
			t.Helper()
			var missing *queries.NotFound
			if !errors.As(err, &missing) || missing.Reason != queries.ReasonNoCheck ||
				!strings.Contains(missing.Detail, "unit's attempt 3") || !errors.Is(err, queries.ErrNotFound) {
				t.Errorf("answered %v, want not_found (no_check) carrying the reader's sentence", err)
			}
		}},
		{"not permitted here", fmt.Errorf("%w: %s", gitlet.ErrNotPermitted, shutOut), notPermitted},
		{"not permitted on another data node", fmt.Errorf("estate: data node n2 answered: %w",
			fmt.Errorf("%w: %s", gitlet.ErrNotPermitted, shutOut)), notPermitted},
		{"a level", fmt.Errorf("%w: a session read names the position it must include", gitlet.ErrBadLevel),
			is(queries.ErrBadParams)},
		{"a cursor", fmt.Errorf("%w: phase %q", gitlet.ErrBadCursor, "x"), is(queries.ErrBadParams)},
		{"a parameter", fmt.Errorf("%w: limit: 9000 is past the 200 one page of pushes carries", gitlet.ErrInvalid),
			is(queries.ErrBadParams)},
		{"a floor on another log", fmt.Errorf("code: this read's floor: %w", statelog.ErrForeignPosition),
			is(queries.ErrBadParams)},
		{"behind", &statelog.Refused{Code: statelog.RefuseBehind, Level: statelog.ReadSession,
			RetryAfter: 7 * time.Second}, is(queries.ErrUnavailable)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, q := range codeQuestions {
				stub := &stubCode{answer: func(gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
					return gitlet.ReadAnswer{}, tc.err
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
// that would admit them — alike by a node serving Gitlet and by one
// serving none of it, and neither the source, the repository's row nor its
// readers is asked; an admin is told why a node serves none, and answered by
// one that does.
//
// Mutation: ask the source before the decide (codeRepoReaders' first lines
// swapped) → the colleague on a node serving none is told `not_hosted` and
// the source is asked, red.
func TestWhoReadARepositoryIsDecidedBeforeAnythingIsRead(t *testing.T) {
	t.Parallel()
	company := func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		return gitlet.ReadAnswer{Level: q.Fresh.Level, Repo: &gitlet.RepoDetail{RepoSummary: gitlet.RepoSummary{
			RepoRef: gitlet.RepoRef{Name: q.Repo, Visibility: gitlet.VisibilityCompany}}}}, nil
	}
	hostsNone := &queries.NotFound{Reason: queries.ReasonNotHosted, Detail: "this company does not host code"}
	ask := func(t *testing.T, stub *stubCode, grants ...iam.Grant) (any, error) {
		t.Helper()
		r := queries.NewRegistry()
		queries.Register(r, queries.Sources{Code: stub})
		return r.Answer(asGrants(t, "", grants...), "code_repo_readers", map[string]any{"name": "web"})
	}
	for name, unserved := range map[string]error{"a node serving Gitlet": nil,
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

// A CODE QUESTION ASKS THE GITLET APP BEFORE ANYTHING IS READ (ADR-0110).
//
// Every code question but who read a repository (admin's) is registered
// behind the app, so a caller holding every other app is refused on authority,
// naming exactly gitlet, by the registry — on REST and the socket alike, which
// answer through it — and the node's source, the repository's rows, its
// content and its search are never asked: no answer, count or "not declared"
// tells somebody the company withheld code from what a repository holds. The
// questions are found by their names, so one added later is held here too, and
// the walk fails if it finds none. The CONTROL is the same caller holding
// gitlet alone, answered. Mutation: register code_repos DecidedBy code.read
// again, as it was, and it is answered to the caller without the app.
func TestACodeQuestionAsksTheGitletAppBeforeAnythingIsRead(t *testing.T) {
	t.Parallel()
	var others []iam.Grant
	for _, app := range iam.Apps {
		if app != iam.GrantGitlet {
			others = append(others, app)
		}
	}
	stub := &stubCode{}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Code: stub})
	walked := 0
	for _, what := range r.Names() {
		if !strings.HasPrefix(what, "code_") || what == "code_repo_readers" {
			continue
		}
		walked++
		_, err := r.Answer(asGrants(t, "", others...), what, map[string]any{"name": "web"})
		var refusal *queries.Refusal
		if !errors.As(err, &refusal) || !slices.Equal(refusal.Grants, []iam.Grant{iam.GrantGitlet}) {
			t.Errorf("%s: a caller holding every app but gitlet was answered %v, want refused naming gitlet",
				what, err)
		}
	}
	if walked == 0 {
		t.Fatal("no code question is registered, so this walk certifies nothing")
	}
	if stub.sourced != 0 || len(stub.asked) != 0 || len(stub.read) != 0 || len(stub.searched) != 0 {
		t.Errorf("a refused caller reached the source %d time(s), its rows %d, its content %d and its search %d",
			stub.sourced, len(stub.asked), len(stub.read), len(stub.searched))
	}
	if _, err := r.Answer(asGrants(t, "", iam.GrantGitlet), "code_repos", map[string]any{}); err != nil {
		t.Errorf("a caller holding gitlet alone was refused the repositories: %v", err)
	}
}
