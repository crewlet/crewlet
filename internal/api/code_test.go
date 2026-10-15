package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fixedCode is a code surface answering what it holds for good: a git host,
// and the code domain's reads — each absent where it holds nil, for the reason
// why (the company hosts no code where why is nil).
type fixedCode struct {
	git   api.GitRoutes
	reads api.CodeReads
	why   error
}

func (f fixedCode) unserved() error {
	if f.why != nil {
		return f.why
	}
	return engine.ErrCodeNotHosted
}

func (f fixedCode) GitRoutes() (api.GitRoutes, error) {
	if f.git == nil {
		return nil, f.unserved()
	}
	return f.git, nil
}

func (f fixedCode) CodeReads() (api.CodeReads, error) {
	if f.reads == nil {
		return nil, f.unserved()
	}
	return f.reads, nil
}

// switchedCode is a code surface whose answer a case moves: not hosted until
// present is set, then the fixed one.
type switchedCode struct {
	present atomic.Bool
	fixedCode
}

func (s *switchedCode) GitRoutes() (api.GitRoutes, error) {
	if !s.present.Load() {
		return nil, engine.ErrCodeNotHosted
	}
	return s.fixedCode.GitRoutes()
}

func (s *switchedCode) CodeReads() (api.CodeReads, error) {
	if !s.present.Load() {
		return nil, engine.ErrCodeNotHosted
	}
	return s.fixedCode.CodeReads()
}

// fakeGitRoutes is a git host whose handlers answer `served <what>`, whose
// start of a verify answers what start does — a running verification of the
// name where it is nil — and whose verifications are what kept holds.
type fakeGitRoutes struct {
	start func(ctx context.Context, name string, repair bool, floor statelog.Position) (host.Verification, error)
	kept  map[uuid.UUID]host.Verification
}

// served is a handler writing 200 and what it stands for.
func served(what string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "served "+what)
	})
}

func (fakeGitRoutes) InfoRefs() http.Handler    { return served("info/refs") }
func (fakeGitRoutes) UploadPack() http.Handler  { return served("upload-pack") }
func (fakeGitRoutes) ReceivePack() http.Handler { return served("receive-pack") }

func (f fakeGitRoutes) StartVerify(ctx context.Context, name string, repair bool, floor statelog.Position) (
	host.Verification, error) {
	if f.start == nil {
		return host.Verification{ID: uuid.Must(uuid.NewV7()), Repo: name, Repair: repair,
			State: host.VerifyRunning, Started: time.Now()}, nil
	}
	return f.start(ctx, name, repair, floor)
}

func (f fakeGitRoutes) Verification(name string, id uuid.UUID) (host.Verification, bool) {
	v, ok := f.kept[id]
	if !ok || v.Repo != name {
		return host.Verification{}, false
	}
	return v, true
}

// AwaitVerification answers a kept verification that has ended at once, and
// otherwise waits for ctx — the check never ends while the request waits —
// answering what is kept then.
func (f fakeGitRoutes) AwaitVerification(ctx context.Context, name string, id uuid.UUID) (host.Verification, bool) {
	if v, ok := f.Verification(name, id); ok && v.State != host.VerifyRunning {
		return v, true
	}
	<-ctx.Done()
	return f.Verification(name, id)
}

// fakeCodeReads records every query it is asked and answers what answer does
// — an empty answer at the level asked where it is nil.
type fakeCodeReads struct {
	answer func(code.ReadQuery) (code.ReadAnswer, error)

	mu    sync.Mutex
	asked []code.ReadQuery
}

func (f *fakeCodeReads) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	f.mu.Lock()
	f.asked = append(f.asked, q)
	f.mu.Unlock()
	if f.answer == nil {
		return code.ReadAnswer{Level: q.Fresh.Level}, nil
	}
	return f.answer(q)
}

// queries is every query asked so far.
func (f *fakeCodeReads) queries() []code.ReadQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// codeApp is an authenticated node of roles (nil: undeclared, every role)
// serving surface.
func codeApp(t *testing.T, roles []string, surface api.CodeSurface) *api.App {
	t.Helper()
	b := closedPosture()
	b.Node.Roles = roles
	return newApp(t, api.Options{Bootstrap: &b, Code: surface})
}

// ask runs one request on a with the operator's bearer token, or with no
// credential where bearer is false.
func ask(t *testing.T, a http.Handler, method, path string, bearer bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if bearer {
		r.Header.Set("Authorization", "Bearer secret")
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, r)
	return rec
}

// errorBody is a JSON refusal's code and detail, failing the case on a body
// that is not one.
func errorBody(t *testing.T, rec *httptest.ResponseRecorder) (code, detail string) {
	t.Helper()
	var body struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("an answer that is not JSON: %v (%d %q)", err, rec.Code, rec.Body.String())
	}
	return body.Error, body.Detail
}

// gitRequests is one request of each git route, as git sends them.
var gitRequests = []struct{ method, path string }{
	{http.MethodGet, "/git/web.git/info/refs?service=git-upload-pack"},
	{http.MethodPost, "/git/web.git/git-upload-pack"},
	{http.MethodPost, "/git/web.git/git-receive-pack"},
}

// GIT IS SERVED WHERE A NODE'S ROLES NAME IT, AND READ EVERYWHERE. A node whose
// Tier A lists its roles without `git` keeps no cache and runs no git, so its
// git routes — and the verify of a cache it does not have — are not mounted at
// all: the mux's own `no_route`, as a route this node's configuration does not
// enable has always answered. The code reads are a data node's rows or the
// estate router's, served on every node. An undeclared node has every role.
//
// Mutation: mount the git routes on every API node, and the first half goes
// red.
func TestGitRoutesAreMountedOnlyOnAGitNode(t *testing.T) {
	t.Parallel()
	surface := fixedCode{git: fakeGitRoutes{}, reads: &fakeCodeReads{}}
	for _, tc := range []struct {
		name  string
		roles []string
		git   bool
	}{
		{"roles without git", []string{"data", "ingress", "seats", "workers"}, false},
		{"undeclared roles", nil, true},
		{"roles naming git", []string{"data", "git", "ingress"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := codeApp(t, tc.roles, surface)
			for _, req := range append(gitRequests[:len(gitRequests):len(gitRequests)],
				struct{ method, path string }{http.MethodPost, "/code/repos/web/verify"}) {
				rec := ask(t, a, req.method, req.path, true)
				if mounted := !absent(t, rec); mounted != tc.git {
					t.Errorf("%s %s answered %d %q: mounted %v, want %v", req.method, req.path,
						rec.Code, rec.Body.String(), mounted, tc.git)
				}
			}
			for _, path := range []string{"/code/repos", "/code/repos/web", "/code/repos/web/pushes"} {
				if rec := ask(t, a, http.MethodGet, path, true); rec.Code != http.StatusOK {
					t.Errorf("GET %s answered %d %q on a node of every API: the reads are "+
						"everybody's", path, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

// THE CODE SURFACE IS ASKED ON EVERY REQUEST. The code host is built with the
// native runtime — at the first apply whose company sets code.native, which on
// a node that booted with no company is after the API was assembled — so one
// app answers "not hosted" before it and serves the moment it exists, with no
// restart between.
//
// Mutation: resolve the surface once in mountCode, and the second half goes
// red.
func TestTheCodeSurfaceIsAskedOnEveryRequest(t *testing.T) {
	t.Parallel()
	reads := &fakeCodeReads{}
	surface := &switchedCode{fixedCode: fixedCode{git: fakeGitRoutes{}, reads: reads}}
	a := codeApp(t, nil, surface)
	const notHosted = "this company does not host code (code.native is not set)\n"

	rec := ask(t, a, http.MethodGet, "/git/web.git/info/refs?service=git-upload-pack", true)
	if rec.Code != http.StatusNotFound || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") ||
		rec.Body.String() != notHosted {
		t.Errorf("a git route before the host answered %d %q %q, want 404 text/plain %q", rec.Code,
			rec.Header().Get("Content-Type"), rec.Body.String(), notHosted)
	}
	rec = ask(t, a, http.MethodGet, "/code/repos", true)
	if code, detail := errorBody(t, rec); rec.Code != http.StatusNotFound || code != "not_found" ||
		detail+"\n" != notHosted {
		t.Errorf("a code read before the host answered %d %s %q, want 404 not_found naming code.native",
			rec.Code, code, detail)
	}

	surface.present.Store(true)
	if rec := ask(t, a, http.MethodGet, "/git/web.git/info/refs?service=git-upload-pack", true); rec.Code !=
		http.StatusOK || rec.Body.String() != "served info/refs" {
		t.Errorf("a git route once the host exists answered %d %q, want the host's own", rec.Code,
			rec.Body.String())
	}
	if rec := ask(t, a, http.MethodGet, "/code/repos", true); rec.Code != http.StatusOK || len(reads.queries()) != 1 {
		t.Errorf("a code read once the host exists answered %d %q, reaching the reads %d times",
			rec.Code, rec.Body.String(), len(reads.queries()))
	}
}

// A NODE THAT SERVES NONE OF THE CODE HOST SAYS WHY, and a company that hosts
// code is never told it does not. Where the company hosts no code, a code
// route is a 404 naming code.native. Where the fleet activated a configuration
// that hosts code and this node serves none of it now — or this node has not
// read whether it does — it is a 503 naming which of four it is, in git's
// form on a git route, in the read vocabulary with a `reason` on a /code/
// read, and only the one waiting is sure to clear, an apply in flight, carries
// a Retry-After: an apply that failed and a runtime that needs its restart
// clear when somebody acts, and a client told to come back in five seconds
// would ask this node for ever when another node serves it. A reason the seam
// does not name breaks its contract: 500.
//
// WHAT THE NODE NEVER READ THE ANSWER NEVER CLAIMS: an apply in flight may not
// have decoded the revision yet, and an apply that failed before it could
// (ErrCodeUnknown) did not — so neither answer says the company hosts code or
// that the code host is coming, which a company that never set code.native
// would read as a fact about its own document.
//
// Mutation: answer every reason with the health tick's Retry-After, and the
// failed apply's and the restart's go red; answer the unread failure with the
// failed apply's sentence, or an apply in flight with the code host's
// promise, and the unread case and the applying one go red.
func TestANodeServingNoCodeHostSaysWhy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		why        error
		status     int
		reason     string
		detail     string
		retryAfter string
		// claims is what the answer must NOT say: a fact about the
		// company's configuration the node did not read.
		claims []string
	}{
		{"not hosted", engine.ErrCodeNotHosted, http.StatusNotFound, "", "code.native is not set", "", nil},
		{"applying", fmt.Errorf("%w (epoch 4)", engine.ErrCodeApplying), http.StatusServiceUnavailable,
			"applying", "retry in 5 seconds", "5", []string{"hosts code", "serves the code host"}},
		{"apply failed", fmt.Errorf("%w (epoch 4)", engine.ErrCodeApplyFailed), http.StatusServiceUnavailable,
			"config_apply_failed", "which hosts code", "", nil},
		{"unknown", fmt.Errorf("%w (epoch 4)", engine.ErrCodeUnknown), http.StatusServiceUnavailable,
			"config_apply_failed", "cannot say whether this company hosts code", "",
			[]string{"which hosts code", "does not host code", "serves the code host"}},
		{"needs restart", engine.ErrCodeNeedsRestart, http.StatusServiceUnavailable,
			"native_backend_needs_restart", "once restarted", "", nil},
		{"unexplained", errors.New("a seam that says something else"), http.StatusInternalServerError,
			"", "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := codeApp(t, nil, fixedCode{why: tc.why})
			unclaimed := func(surface, said string) {
				t.Helper()
				for _, claim := range tc.claims {
					if strings.Contains(said, claim) {
						t.Errorf("%s answered %q, which says %q of a configuration this node did not read",
							surface, said, claim)
					}
				}
			}
			rec := ask(t, a, http.MethodGet, "/git/web.git/info/refs?service=git-upload-pack", true)
			if rec.Code != tc.status || rec.Header().Get("Retry-After") != tc.retryAfter ||
				!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") ||
				!strings.Contains(rec.Body.String(), tc.detail) || !strings.Contains(rec.Body.String(), tc.reason) {
				t.Errorf("a git route answered %d %q (Retry-After %q), want %d text/plain naming %q and %q "+
					"(Retry-After %q)", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"), tc.status,
					tc.detail, tc.reason, tc.retryAfter)
			}
			unclaimed("a git route", rec.Body.String())
			for _, req := range []struct{ method, path string }{
				{http.MethodGet, "/code/repos"},
				{http.MethodPost, "/code/repos/web/verify"},
				{http.MethodGet, "/code/repos/web/verify/" + uuid.Must(uuid.NewV7()).String()},
			} {
				rec = ask(t, a, req.method, req.path, true)
				var body struct {
					Error  string `json:"error"`
					Reason string `json:"reason"`
					Detail string `json:"detail"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("%s %s answered %d %q: %v", req.method, req.path, rec.Code, rec.Body.String(), err)
				}
				want := map[int]string{http.StatusNotFound: "not_found", http.StatusServiceUnavailable: "unavailable",
					http.StatusInternalServerError: "query_failed"}[tc.status]
				if rec.Code != tc.status || body.Error != want || body.Reason != tc.reason ||
					!strings.Contains(body.Detail, tc.detail) || rec.Header().Get("Retry-After") != tc.retryAfter {
					t.Errorf("%s %s answered %d %+v (Retry-After %q), want %d %s, reason %q, naming %q "+
						"(Retry-After %q)", req.method, req.path, rec.Code, body, rec.Header().Get("Retry-After"),
						tc.status, want, tc.reason, tc.detail, tc.retryAfter)
				}
				unclaimed(req.method+" "+req.path, body.Detail)
			}
		})
	}
}

// THE CODE READS ANSWER IN THE READ SURFACE'S ONE VOCABULARY. A repository that
// is not declared is `not_found`; a level, a cursor or a parameter the reader
// refuses is `bad_params` with the reader's own sentence; what clears by
// waiting — a retryable read refusal, no data node serving — is `unavailable`
// with its Retry-After. The level is the tree's one freshness grammar, and a
// parameter no read takes is refused before the reader is asked.
func TestCodeReadsAnswerInTheReadVocabulary(t *testing.T) {
	t.Parallel()
	refused := map[string]error{
		"web":     code.ErrRepoNotFound,
		"level":   fmt.Errorf("%w: \"session\" is not a level a code read serves: it serves linearizable, stale and consistent_prefix", code.ErrBadLevel),
		"cursor":  fmt.Errorf("%w: phase %q", code.ErrBadCursor, "x"),
		"behind":  &statelog.Refused{Code: statelog.RefuseBehind, Level: statelog.ReadLinearizable, RetryAfter: 7 * time.Second},
		"nobody":  &estate.ErrUnserved{Detail: "no data node answered"},
		"evicted": &statelog.Refused{Code: statelog.RefuseEvicted, Level: statelog.ReadStale},
	}
	reads := &fakeCodeReads{answer: func(q code.ReadQuery) (code.ReadAnswer, error) {
		if err, ok := refused[q.Repo]; ok {
			return code.ReadAnswer{}, err
		}
		return code.ReadAnswer{Level: q.Fresh.Level}, nil
	}}
	a := codeApp(t, nil, fixedCode{reads: reads})
	for _, tc := range []struct {
		path        string
		status      int
		code        string
		detail      string
		retryAfter  string
		readerAsked bool
	}{
		{"/code/repos/web", http.StatusNotFound, "not_found", `"web" is declared in code.repos`, "", true},
		{"/code/repos/level", http.StatusBadRequest, "bad_params", "stale and consistent_prefix", "", true},
		{"/code/repos/cursor/pushes", http.StatusBadRequest, "bad_params", "not a cursor", "", true},
		{"/code/repos/behind", http.StatusServiceUnavailable, "unavailable", "", "7", true},
		{"/code/repos/nobody", http.StatusServiceUnavailable, "unavailable", "", "5", true},
		{"/code/repos/evicted", http.StatusInternalServerError, "query_failed", "", "", true},
		{"/code/repos?read_level=bogus", http.StatusBadRequest, "bad_params", "bogus", "", false},
		{"/code/repos?status=gone", http.StatusBadRequest, "bad_params", `"gone"`, "", false},
		{"/code/repos?limit=-1", http.StatusBadRequest, "bad_params", "limit", "", false},
		{"/code/repos?limit=ten", http.StatusBadRequest, "bad_params", "limit", "", false},
	} {
		before := len(reads.queries())
		rec := ask(t, a, http.MethodGet, tc.path, true)
		code, detail := errorBody(t, rec)
		if rec.Code != tc.status || code != tc.code || !strings.Contains(detail, tc.detail) ||
			rec.Header().Get("Retry-After") != tc.retryAfter {
			t.Errorf("GET %s answered %d %s %q (Retry-After %q), want %d %s naming %q (Retry-After %q)",
				tc.path, rec.Code, code, detail, rec.Header().Get("Retry-After"), tc.status, tc.code,
				tc.detail, tc.retryAfter)
		}
		if asked := len(reads.queries()) > before; asked != tc.readerAsked {
			t.Errorf("GET %s asked the reader: %v, want %v", tc.path, asked, tc.readerAsked)
		}
	}

	// AND WHAT IS ASKED IS WHAT ARRIVES: the kind, the repository, the
	// filter, the page and the level, through the one grammar.
	for _, tc := range []struct {
		path string
		want code.ReadQuery
	}{
		{"/code/repos?status=active,archived&limit=7&cursor=c1&read_level=linearizable",
			code.ReadQuery{Kind: code.ReadRepos, Status: []code.RepoStatus{code.RepoStatusActive,
				code.RepoStatusArchived}, Limit: 7, Cursor: "c1",
				Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}}},
		{"/code/repos/site", code.ReadQuery{Kind: code.ReadRepo, Repo: "site",
			Fresh: statelog.Freshness{Level: statelog.ReadStale}}},
		{"/code/repos/site/pushes?limit=0", code.ReadQuery{Kind: code.ReadPushes, Repo: "site",
			Fresh: statelog.Freshness{Level: statelog.ReadStale}}},
	} {
		before := len(reads.queries())
		if rec := ask(t, a, http.MethodGet, tc.path, true); rec.Code != http.StatusOK {
			t.Errorf("GET %s answered %d %q", tc.path, rec.Code, rec.Body.String())
			continue
		}
		asked := reads.queries()
		if len(asked) != before+1 {
			t.Errorf("GET %s asked the reader %d times", tc.path, len(asked)-before)
			continue
		}
		got := asked[len(asked)-1]
		if got.Kind != tc.want.Kind || got.Repo != tc.want.Repo || !slices.Equal(got.Status, tc.want.Status) ||
			got.Limit != tc.want.Limit || got.Cursor != tc.want.Cursor || got.Fresh != tc.want.Fresh {
			t.Errorf("GET %s asked %+v, want %+v", tc.path, got, tc.want)
		}
	}
}

// THE CODE ROUTES ARE ALWAYS GUARDED, reads included, whatever
// allow_anonymous_read opens: a repository is the company's source, and the
// reads name every repository and the people who pushed to it. A code read with
// no credential is refused in the API's JSON; a git route in git's own form,
// the Basic challenge git answers with its credential helper.
func TestCodeReadsAreAlwaysGuarded(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	b.API.Auth.AllowAnonymousRead = true
	a := newApp(t, api.Options{Bootstrap: &b,
		Code: fixedCode{git: fakeGitRoutes{}, reads: &fakeCodeReads{}}})
	for _, path := range []string{"/code/repos", "/code/repos/web", "/code/repos/web/pushes"} {
		rec := ask(t, a, http.MethodGet, path, false)
		if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("Content-Type"),
			"application/json") {
			t.Errorf("GET %s with no credential answered %d %q, want the guard's JSON 401", path,
				rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	rec := ask(t, a, http.MethodGet, "/git/web.git/info/refs?service=git-upload-pack", false)
	if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic") ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("a clone with no credential answered %d %v, want git's Basic challenge", rec.Code, rec.Header())
	}
}

// A VERIFY IS AN OPERATION: begun with 202 and read until it ends. The check
// reads every byte of a repository, which outlives a balancer's read timeout,
// so the POST answers the verification running — its id and its address in
// `Location` — and a GET there answers it running, done with the node's
// report, or failed with why and what the check found before it failed. What
// can be refused without reading the repository is refused at once: `repair`
// is true or false and nothing else; a repository not declared, or one this
// node holds no copy of, is `not_found` (a node holding no copy has nothing to
// check, and building one is a whole download); a check of the other repair
// setting running is a 409 naming it; a stopping host and a node with no slot
// or memory free are 503s with their own waits — refused before the check
// read anything, so the retry costs nothing; and anything else says what it
// was, the route being always guarded and its caller an operator.
//
// Mutation: drop the report from a failed verification's answer, and the
// stopped check's problems go missing.
func TestVerifyAnswersTheReport(t *testing.T) {
	t.Parallel()
	var (
		repairs []bool
		floors  []statelog.Position
		mu      sync.Mutex
	)
	running := host.Verification{ID: uuid.Must(uuid.NewV7()), Repo: "racing", State: host.VerifyRunning,
		Started: time.Now()}
	done := host.Verification{ID: uuid.Must(uuid.NewV7()), Repo: "web", State: host.VerifyDone,
		Started: time.Now().Add(-time.Minute), Ended: time.Now(),
		Report: cache.VerifyReport{Repo: "r0", Name: "web", Packs: 2, Refs: 5}}
	webRun := uuid.Must(uuid.NewV7())
	quick := host.Verification{ID: uuid.Must(uuid.NewV7()), Repo: "quick", State: host.VerifyDone,
		Started: time.Now(), Ended: time.Now(), Report: cache.VerifyReport{Repo: "r4", Name: "quick", Packs: 1}}
	found := []string{"pack p1 (pack-ab) hashes to 01; its row says 02"}
	stopped := host.Verification{ID: uuid.Must(uuid.NewV7()), Repo: "half", State: host.VerifyFailed,
		Started: time.Now().Add(-time.Minute), Ended: time.Now(),
		Report: cache.VerifyReport{Repo: "r1", Name: "half", Packs: 3, Problems: found},
		Err:    fmt.Errorf("%w: cache: verify half: fsck: %w", host.ErrStopping, context.Canceled)}
	failed := host.Verification{ID: uuid.Must(uuid.NewV7()), Repo: "lost", State: host.VerifyFailed,
		Started: time.Now().Add(-time.Minute), Ended: time.Now(),
		Report: cache.VerifyReport{Repo: "r2", Name: "lost", Packs: 1, Problems: found},
		Err:    errors.New("cache: verify lost: a rebuild could not repair it: pack p1 is missing")}
	ends := time.Now().Add(time.Minute).Truncate(time.Second)
	checking := host.Verification{ID: uuid.Must(uuid.NewV7()), Repo: "deep", State: host.VerifyRunning,
		Started: time.Now(), Deadline: ends}
	done.Deadline = ends // what an ended verification is never answered with
	g := fakeGitRoutes{
		kept: map[uuid.UUID]host.Verification{done.ID: done, stopped.ID: stopped, failed.ID: failed,
			checking.ID: checking, quick.ID: quick},
		start: func(_ context.Context, name string, repair bool, floor statelog.Position) (host.Verification, error) {
			mu.Lock()
			repairs = append(repairs, repair)
			floors = append(floors, floor)
			mu.Unlock()
			switch name {
			case "web":
				return host.Verification{ID: webRun, Repo: "web", Repair: repair, State: host.VerifyRunning,
					Started: done.Started}, nil
			case "quick":
				return host.Verification{ID: quick.ID, Repo: "quick", State: host.VerifyRunning,
					Started: quick.Started}, nil
			case "gone":
				return host.Verification{}, code.ErrRepoNotFound
			case "elsewhere":
				return host.Verification{}, fmt.Errorf("cache: verify r3: %w", cache.ErrNotHeld)
			case "racing":
				return host.Verification{}, &host.VerifyRunningError{Running: running}
			case "stopping":
				return host.Verification{}, host.ErrStopping
			case "astray":
				return host.Verification{}, fmt.Errorf("%w: a verify's floor is on another log", code.ErrInvalid)
			case "busy":
				return host.Verification{}, fmt.Errorf("cache: verify busy: %w — run it again", gitproc.ErrBusy)
			case "nobody":
				return host.Verification{}, &estate.ErrUnserved{Detail: "no data node answered"}
			default:
				return host.Verification{}, fmt.Errorf("cache: verify %s: %w: its connectivity check needs "+
					"about 9 bytes — run it on a node with more memory", name, cache.ErrNoMemory)
			}
		},
	}
	a := codeApp(t, nil, fixedCode{git: g})
	post := func(path string) *httptest.ResponseRecorder {
		return ask(t, a, http.MethodPost, "/code/repos/"+path, true)
	}

	for _, bad := range []string{"web/verify?repair=maybe", "web/verify?repair=1", "web/verify?repair=true&repair=false",
		"web/verify?min_position=yesterday"} {
		rec := post(bad)
		if code, _ := errorBody(t, rec); rec.Code != http.StatusBadRequest || code != "bad_params" {
			t.Errorf("POST %s answered %d %s, want 400 bad_params", bad, rec.Code, code)
		}
	}
	mu.Lock()
	asked := len(repairs)
	mu.Unlock()
	if asked != 0 {
		t.Errorf("a refused repair reached the host %d times", asked)
	}

	// BEGUN: 202, the verification still running once the request has
	// waited host.VerifyHold for it, and where to read it.
	began := time.Now()
	rec := post("web/verify")
	if waited := time.Since(began); waited < host.VerifyHold {
		t.Errorf("a verify still running was answered after %s, want the request held %s for it", waited,
			host.VerifyHold)
	}
	var begun struct {
		ID     string           `json:"id"`
		Repo   string           `json:"repo"`
		State  host.VerifyState `json:"state"`
		Report json.RawMessage  `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &begun); err != nil || rec.Code != http.StatusAccepted ||
		begun.ID != webRun.String() || begun.Repo != "web" || begun.State != host.VerifyRunning ||
		begun.Report != nil || rec.Header().Get("Location") != "/code/repos/web/verify/"+webRun.String() {
		t.Errorf("a verify answered %d %v %q (%v), want 202 running at its Location", rec.Code, rec.Header(),
			rec.Body.String(), err)
	}
	// ENDED WHILE THE REQUEST WAITED: 200, the verification done, its report
	// in the one answer.
	rec = post("quick/verify")
	var ended struct {
		State  host.VerifyState    `json:"state"`
		Report *cache.VerifyReport `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ended); err != nil || rec.Code != http.StatusOK ||
		ended.State != host.VerifyDone || ended.Report == nil || ended.Report.Packs != 1 {
		t.Errorf("a verify that ended while it was asked answered %d %q (%v), want 200 done with its report",
			rec.Code, rec.Body.String(), err)
	}
	post("web/verify?repair=true")
	post("web/verify?repair=false&min_position=CREWLET_CODE_LOG@1:4711")
	mu.Lock()
	if !slices.Equal(repairs, []bool{false, false, true, false}) {
		t.Errorf("the host was asked repair %v, want false, false, true, false", repairs)
	}
	// THE CALLER'S FLOOR reaches the host as the position it named, and a
	// verify naming none asks at the log's head.
	if want := (statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: 4711}); len(floors) != 4 ||
		!floors[0].IsZero() || floors[3] != want {
		t.Errorf("the host was asked at %v, want no floor and then %v", floors, want)
	}
	mu.Unlock()

	// REFUSED AT ONCE — and a repository this node holds no copy of is told
	// from one nobody declared by its reason, which a client acts on without
	// reading the sentence: a sweep of every repository on one node passes
	// over the ones that node does not keep, and fails on the ones nobody
	// declared.
	for _, tc := range []struct {
		name, code, reason, detail, retryAfter string
		status                                 int
	}{
		{"gone", "not_found", "", `no repository named "gone" is declared in code.repos`, "",
			http.StatusNotFound},
		{"elsewhere", "not_found", "not_held", "holds no copy", "", http.StatusNotFound},
		{"racing", "verify_running", "", running.ID.String(), "", http.StatusConflict},
		{"stopping", "unavailable", "", "stopping", "30", http.StatusServiceUnavailable},
		{"busy", "unavailable", "", "run it again", "10", http.StatusServiceUnavailable},
		{"astray", "bad_params", "", "another log", "", http.StatusBadRequest},
		{"nobody", "unavailable", "", "", "5", http.StatusServiceUnavailable},
		{"large", "query_failed", "", "a node with more memory", "", http.StatusInternalServerError},
	} {
		rec := post(tc.name + "/verify")
		code, detail := errorBody(t, rec)
		var reason struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &reason)
		if rec.Code != tc.status || code != tc.code || reason.Reason != tc.reason ||
			!strings.Contains(detail, tc.detail) || rec.Header().Get("Retry-After") != tc.retryAfter {
			t.Errorf("a verify of %s answered %d %s (reason %q) %q (Retry-After %q), want %d %s "+
				"(reason %q) naming %q (Retry-After %q)", tc.name, rec.Code, code, reason.Reason, detail,
				rec.Header().Get("Retry-After"), tc.status, tc.code, tc.reason, tc.detail, tc.retryAfter)
		}
	}

	// READ: done with its report; failed with why and WHAT IT FOUND BEFORE —
	// a stop the node can clear by restarting is `unavailable`, anything
	// else `query_failed` — and what this node does not keep is none.
	type view struct {
		ID       string              `json:"id"`
		State    host.VerifyState    `json:"state"`
		Deadline time.Time           `json:"deadline"`
		Ended    time.Time           `json:"ended"`
		Report   *cache.VerifyReport `json:"report"`
		Error    string              `json:"error"`
		Detail   string              `json:"detail"`
	}
	read := func(path string) (int, view) {
		rec := ask(t, a, http.MethodGet, "/code/repos/"+path, true)
		var v view
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatalf("GET %s answered %d %q: %v", path, rec.Code, rec.Body.String(), err)
		}
		return rec.Code, v
	}
	if status, v := read("web/verify/" + done.ID.String()); status != http.StatusOK || v.State != host.VerifyDone ||
		v.Ended.IsZero() || v.Report == nil || v.Report.Packs != 2 || v.Report.Refs != 5 || v.Error != "" ||
		!v.Deadline.IsZero() {
		t.Errorf("a done verification read %d %+v, want 200 done with its report and no deadline", status, v)
	}
	// CHECKING: running, with when the node ends its check.
	if status, v := read("deep/verify/" + checking.ID.String()); status != http.StatusOK ||
		v.State != host.VerifyRunning || !v.Deadline.Equal(ends) || v.Report != nil {
		t.Errorf("a verification checking its copy read %d %+v, want 200 running, ending its check by %s", status,
			v, ends)
	}
	for _, tc := range []struct {
		v      host.Verification
		code   string
		detail string
	}{
		{stopped, "unavailable", "stopping"},
		{failed, "query_failed", "pack p1 is missing"},
	} {
		status, v := read(tc.v.Repo + "/verify/" + tc.v.ID.String())
		if status != http.StatusOK || v.State != host.VerifyFailed || v.Error != tc.code ||
			!strings.Contains(v.Detail, tc.detail) {
			t.Errorf("a failed verification of %s read %d %+v, want 200 failed, %s, naming %q", tc.v.Repo,
				status, v, tc.code, tc.detail)
		}
		if v.Report == nil || !slices.Equal(v.Report.Problems, found) {
			t.Errorf("a failed verification of %s carried %+v, want what it found before it failed", tc.v.Repo,
				v.Report)
		}
	}
	for _, path := range []string{
		"web/verify/" + uuid.Must(uuid.NewV7()).String(),
		"web/verify/not-an-id",
		"half/verify/" + done.ID.String(),
	} {
		if status, v := read(path); status != http.StatusNotFound || v.Error != "not_found" ||
			!strings.Contains(v.Detail, "keeps no verify") {
			t.Errorf("GET %s read %d %+v, want 404 not_found saying this node keeps none", path, status, v)
		}
	}
}
