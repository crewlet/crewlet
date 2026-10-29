package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fixedCode is a code surface answering what it holds for good: a git host,
// the code domain's reads, its reads by object id and who read a unit's
// repository — each absent where it holds nil, for the reason why (the company
// hosts no code where why is nil) — and the tokens a coding box's run token
// and a check's box's token verify by, none where it holds nil.
type fixedCode struct {
	git      api.GitRoutes
	reads    queries.CodeReads
	gitReads api.GitReader
	readers  builtin.ReadersDeps
	why      error
	boxes    *host.BoxTokens
	checks   *host.CheckTokens
}

// VerifyBox is whether boxes or checks signed token for user, by the code
// host's own rule: nothing is, where it holds neither.
func (f fixedCode) VerifyBox(user, token string) bool {
	if f.boxes == nil || f.checks == nil {
		return false
	}
	return host.VerifyBox(f.boxes, f.checks, user, token)
}

func (f fixedCode) GitReads() (api.GitReader, error) {
	if f.gitReads == nil {
		return nil, f.unserved()
	}
	return f.gitReads, nil
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

func (f fixedCode) CodeReads() (queries.CodeReads, error) {
	if f.reads == nil {
		return nil, f.unserved()
	}
	return f.reads, nil
}

func (f fixedCode) RepoReaders() (builtin.ReadersDeps, error) {
	if f.readers.Readers == nil {
		return builtin.ReadersDeps{}, f.unserved()
	}
	return f.readers, nil
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

func (s *switchedCode) CodeReads() (queries.CodeReads, error) {
	if !s.present.Load() {
		return nil, engine.ErrCodeNotHosted
	}
	return s.fixedCode.CodeReads()
}

func (s *switchedCode) RepoReaders() (builtin.ReadersDeps, error) {
	if !s.present.Load() {
		return builtin.ReadersDeps{}, engine.ErrCodeNotHosted
	}
	return s.fixedCode.RepoReaders()
}

func (s *switchedCode) GitReads() (api.GitReader, error) {
	if !s.present.Load() {
		return nil, engine.ErrCodeNotHosted
	}
	return s.fixedCode.GitReads()
}

// fakeGitRoutes is a git host whose handlers answer `served <what>`, whose
// start of a verify answers what start does — a running verification of the
// name where it is nil — whose verifications are what kept holds, and whose
// check admission admits every request as admitted holds it — or, holding
// none, answers it `served admit-check`, as the code host answers one it
// refuses.
type fakeGitRoutes struct {
	start    func(ctx context.Context, name string, repair bool, floor statelog.Position) (host.Verification, error)
	kept     map[uuid.UUID]host.Verification
	admitted *host.CheckAdmitted
}

func (f fakeGitRoutes) AdmitCheck(_ context.Context, w http.ResponseWriter, r *http.Request) (host.CheckAdmitted,
	bool) {

	if f.admitted == nil {
		served("admit-check").ServeHTTP(w, r)
		return host.CheckAdmitted{}, false
	}
	return *f.admitted, true
}

// served is a handler writing 200 and what it stands for.
func served(what string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "served "+what)
	})
}

func (fakeGitRoutes) InfoRefs() http.Handler       { return served("info/refs") }
func (fakeGitRoutes) UploadPack() http.Handler     { return served("upload-pack") }
func (fakeGitRoutes) ReceivePack() http.Handler    { return served("receive-pack") }
func (fakeGitRoutes) BoxInfoRefs() http.Handler    { return served("box-info/refs") }
func (fakeGitRoutes) BoxUploadPack() http.Handler  { return served("box-upload-pack") }
func (fakeGitRoutes) BoxReceivePack() http.Handler { return served("box-receive-pack") }

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

// boxRequests is one request of each coding box's git route, as git sends
// them, and what fakeGitRoutes' handler of each answers.
var boxRequests = []struct{ method, path, served string }{
	{http.MethodGet, "/box/git/web.git/info/refs?service=git-upload-pack", "served box-info/refs"},
	{http.MethodPost, "/box/git/web.git/git-upload-pack", "served box-upload-pack"},
	{http.MethodPost, "/box/git/web.git/git-receive-pack", "served box-receive-pack"},
}

// A BOX ROUTE IS ADMITTED BY ITS RUN, NOT THE GUARD: a coding box's three
// routes are mounted on the route table itself, as /mcp/ and /otlp/ are, and
// published on the public listener, where each request — with no credential,
// or with git's Basic carrying a run's token the guard never reads — reaches
// the code host, whose box admission is the check; never the guard's 401, nor
// the refusal of a router that decides on the request's iam principal, which
// a box's request never has. On api.port, beside a public listener, it is the
// partition's 404.
//
// Mutation: mount the box routes through the authz.Router the person's git
// routes take (ActionCodeGit) — every anonymous box request refused by the
// router — red.
func TestABoxRouteIsAdmittedByItsRunNotTheGuard(t *testing.T) {
	t.Parallel()
	a := listenerApp(t, true, false)
	for _, req := range boxRequests {
		for _, credential := range []string{"", "Basic " + basicAuth("run", "x")} {
			r := httptest.NewRequest(req.method, req.path, nil)
			if credential != "" {
				r.Header.Set("Authorization", credential)
			}
			rec := httptest.NewRecorder()
			a.Public().ServeHTTP(rec, r)
			if rec.Code != http.StatusOK || rec.Body.String() != req.served {
				t.Errorf("%s %s with %q answered %d %q on the public listener, want the code host's own answer",
					req.method, req.path, credential, rec.Code, rec.Body.String())
			}
		}
		if rec := ask(t, a, req.method, req.path, false); !absent(t, rec) {
			t.Errorf("%s %s answered %d %q on api.port beside a public listener, want the partition's 404",
				req.method, req.path, rec.Code, rec.Body.String())
		}
	}
}

// GIT IS SERVED WHERE A NODE'S ROLES NAME IT, AND READ EVERYWHERE. A node whose
// Tier A lists its roles without `git` keeps no cache and runs no git, so its
// git routes — a person's and a coding box's alike — and the verify of a cache
// it does not have are not mounted at all: the mux's own `no_route`, as a route
// this node's configuration does not enable has always answered. The code
// reads are a data node's rows or the estate router's, served on every node.
// An undeclared node has every role.
//
// Mutations: mount the git routes on every API node, and the first half goes
// red; mount the box routes on every node, and their rows go red.
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
			requests := append(gitRequests[:len(gitRequests):len(gitRequests)],
				struct{ method, path string }{http.MethodPost, "/code/repos/web/verify"})
			for _, box := range boxRequests {
				requests = append(requests, struct{ method, path string }{box.method, box.path})
			}
			requests = append(requests, boxCodeRequests...)
			for _, req := range requests {
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

// EACH CODE READ HAS ONE PATH, AND IT IS A QUESTION'S. The code host's rows —
// the repositories, one repository in detail, one repository's push history —
// and who read a unit's repository were routes of the code host's own beside
// git, with a parameter reading and a refusal mapping of their own beside the
// registry's. They are registry questions, served on the paths those routes
// had through the named routes, so a person, a script, the CLI and a screen
// reading its own write back are answered by one implementation: each code
// question is named by exactly one route, at its path; every route an app
// mounts under /code/ — on a node of every role and on one without git — is
// one of those or, where git is served, the verify and its read, operations on
// this node's copy rather than reads of the rows; and a request on each path
// is the question's own answer, asking the reader what the generic form of the
// same question asks it.
//
// Mutation: mount a code read beside the questions again (mountCode serving
// `GET /code/repos/{name}/readers` from a handler of its own, as it did) → the
// walk names it, red; drop a question's named route → its question has no
// path, red.
func TestTheCodeReadsHaveOnePathEach(t *testing.T) {
	t.Parallel()
	held := func(what string) bool {
		return slices.ContainsFunc(codeQuestions, func(q codeQuestion) bool { return q.what == what })
	}
	named := map[string][]string{}
	for _, r := range api.NamedRoutes() {
		if strings.HasPrefix(r.What, "code_") || strings.HasPrefix(r.Pattern, "/code/") {
			named[r.What] = append(named[r.What], r.Method+" "+r.Pattern)
		}
	}
	for _, q := range codeQuestions {
		if got := named[q.what]; !slices.Equal(got, []string{q.route}) {
			t.Errorf("%s is named by the routes %v, want exactly %s", q.what, got, q.route)
		}
	}
	for what, routes := range named {
		if !held(what) {
			t.Errorf("the named routes give %q the code paths %v, which this case does not hold", what, routes)
		}
	}
	verify := []string{"POST /code/repos/{name}/verify", "GET /code/repos/{name}/verify/{id}"}
	for _, tc := range []struct {
		name  string
		roles []string
		git   bool
	}{
		{"a node of every role", nil, true},
		{"a node without the git role", []string{"data", "ingress", "seats", "workers"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// EVERY REPOSITORY A DETAIL, the company's, so who read one is
			// answered from its row — not recorded — without the readers.
			reads := &fakeCodeReads{answer: func(q code.ReadQuery) (code.ReadAnswer, error) {
				return code.ReadAnswer{Level: q.Fresh.Level, Repo: &code.RepoDetail{RepoSummary: code.RepoSummary{
					RepoRef: code.RepoRef{Name: q.Repo, Visibility: code.VisibilityCompany}}}}, nil
			}}
			a := codeApp(t, tc.roles, fixedCode{git: fakeGitRoutes{}, reads: reads,
				readers: readersOn(t, &fakeRepoReaders{})})
			for _, what := range a.Queries().Names() {
				if strings.HasPrefix(what, "code_") && !held(what) {
					t.Errorf("this node answers the code question %q, which no path here holds", what)
				}
			}
			var want, mounted []string
			for _, q := range codeQuestions {
				want = append(want, q.route)
			}
			if tc.git {
				want = append(want, verify...)
			}
			for _, pattern := range a.Routes() {
				if _, path, _ := strings.Cut(pattern, " "); strings.HasPrefix(path, "/code/") {
					mounted = append(mounted, pattern)
				}
			}
			slices.Sort(want)
			slices.Sort(mounted)
			if !slices.Equal(mounted, want) {
				t.Errorf("the routes mounted under /code/ are\n  %v\nwant\n  %v", mounted, want)
			}
			// AND EACH PATH IS THE QUESTION'S OWN ANSWER: its kind and its
			// repository reach the reader as the generic form's do.
			for _, q := range codeQuestions {
				path := strings.Replace(strings.TrimPrefix(q.route, "GET "), "{name}", "web", 1)
				for _, at := range []string{path, "/query/" + q.what + "?name=web"} {
					before := len(reads.queries())
					if rec := ask(t, a, http.MethodGet, at, true); rec.Code != http.StatusOK {
						t.Errorf("GET %s answered %d %q", at, rec.Code, rec.Body.String())
						continue
					}
					asked := reads.queries()
					if len(asked) != before+1 {
						t.Errorf("GET %s asked the reader %d times, want once", at, len(asked)-before)
						continue
					}
					if got := asked[len(asked)-1]; got.Kind != q.kind || got.Repo != q.repo {
						t.Errorf("GET %s asked a %s read of %q, want a %s read of %q", at, got.Kind, got.Repo,
							q.kind, q.repo)
					}
				}
			}
		})
	}
}

// codeQuestion is one of the code host's questions: its name, its route, the
// kind of read it asks, and the repository a request naming "web" on its path
// reads — none for the listing, whose route names none.
type codeQuestion struct {
	what, route string
	kind        code.ReadKind
	repo        string
}

// codeQuestions are the four: the three rows, and who read a unit's
// repository, which reads the repository's row by name before its readers.
var codeQuestions = []codeQuestion{
	{"code_repos", "GET /code/repos", code.ReadRepos, ""},
	{"code_repo", "GET /code/repos/{name}", code.ReadRepo, "web"},
	{"code_pushes", "GET /code/repos/{name}/pushes", code.ReadPushes, "web"},
	{"code_repo_readers", "GET /code/repos/{name}/readers", code.ReadRepo, "web"},
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
		detail != httpjson.Sentence(strings.TrimSuffix(notHosted, "\n")) {
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
// form on a git route, its code in brackets, and in the refusal envelope under
// that code on a /code/ route — and only the one waiting is sure to clear, an
// apply in flight, carries
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
// failed apply's code or sentence, or an apply in flight with the code host's
// promise, and the unread case and the applying one go red.
func TestANodeServingNoCodeHostSaysWhy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		why    error
		status int
		// code is the envelope's `error` on a /code/ route, and the code
		// the git route's one line names in brackets after its sentence
		// for a 503; readReason the `reason` a read names beside it, which
		// for a company hosting no code is a reason the git line, read by
		// nobody but a person, says in words alone.
		code, readReason string
		detail           string
		retryAfter       string
		// claims is what the answer must NOT say: a fact about the
		// company's configuration the node did not read.
		claims []string
	}{
		{"not hosted", engine.ErrCodeNotHosted, http.StatusNotFound, "not_found", "not_hosted",
			"code.native is not set", "", nil},
		{"applying", fmt.Errorf("%w (epoch 4)", engine.ErrCodeApplying), http.StatusServiceUnavailable,
			"applying", "", "retry in 5 seconds", "5", []string{"hosts code", "serves the code host"}},
		{"apply failed", fmt.Errorf("%w (epoch 4)", engine.ErrCodeApplyFailed), http.StatusServiceUnavailable,
			"config_apply_failed", "", "which hosts code", "", nil},
		{"unknown", fmt.Errorf("%w (epoch 4)", engine.ErrCodeUnknown), http.StatusServiceUnavailable,
			"code_unknown", "", "cannot say whether this company hosts code", "",
			[]string{"which hosts code", "does not host code", "serves the code host", "config_apply_failed"}},
		{"needs restart", engine.ErrCodeNeedsRestart, http.StatusServiceUnavailable,
			"native_backend_needs_restart", "", "once restarted", "", nil},
		{"unexplained", errors.New("a seam that says something else"), http.StatusInternalServerError,
			"query_failed", "", "", "", nil},
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
			// The git line names a 503's code in brackets; a 404 and a 500
			// carry their words alone.
			names := ""
			if tc.status == http.StatusServiceUnavailable {
				names = "(" + tc.code + ")"
			}
			rec := ask(t, a, http.MethodGet, "/git/web.git/info/refs?service=git-upload-pack", true)
			if rec.Code != tc.status || rec.Header().Get("Retry-After") != tc.retryAfter ||
				!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") ||
				!strings.Contains(rec.Body.String(), tc.detail) || !strings.Contains(rec.Body.String(), names) {
				t.Errorf("a git route answered %d %q (Retry-After %q), want %d text/plain naming %q and %q "+
					"(Retry-After %q)", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"), tc.status,
					tc.detail, names, tc.retryAfter)
			}
			unclaimed("a git route", rec.Body.String())
			// ONCE, AT THE END: the sentence names no code of its own, so
			// the brackets the line ends with are its one name for it — a
			// sentence naming a code as well says two where they differ, as
			// an unread apply's once did.
			if names != "" {
				sentence, ends := strings.CutSuffix(rec.Body.String(), " "+names+"\n")
				for _, code := range []string{"config_apply_failed", "code_unknown", "native_backend_needs_restart"} {
					if !ends || strings.Contains(sentence, code) {
						t.Errorf("a git route answered %q, want a sentence naming no code, then %s", rec.Body.String(),
							names)
						break
					}
				}
			}
			for _, req := range []struct{ method, path string }{
				{http.MethodGet, "/code/repos"},
				{http.MethodGet, "/code/repos/web/readers"},
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
				if rec.Code != tc.status || body.Error != tc.code || body.Reason != tc.readReason ||
					!strings.Contains(body.Detail, tc.detail) || rec.Header().Get("Retry-After") != tc.retryAfter {
					t.Errorf("%s %s answered %d %+v (Retry-After %q), want %d %s, reason %q, naming %q "+
						"(Retry-After %q)", req.method, req.path, rec.Code, body, rec.Header().Get("Retry-After"),
						tc.status, tc.code, tc.readReason, tc.detail, tc.retryAfter)
				}
				// THE ENVELOPE, so a client reads the code's sentence beside
				// the detail rather than a code alone.
				if message := httpjson.Code(body.Error).Message(); message == "" ||
					!strings.Contains(rec.Body.String(), `"message"`) {
					t.Errorf("%s %s answered %q, which carries no message from the code's table",
						req.method, req.path, rec.Body.String())
				}
				unclaimed(req.method+" "+req.path, body.Detail)
			}
		})
	}
}

// AN UNSERVED BOX ROUTE TELLS WHY ONLY TO A TOKEN THIS FLEET SIGNED.
// /box/git/ is public — on api.public, reached from wherever a sandbox runs —
// so a node that serves none of the code host now tells a coding box its
// reason (a company hosting no code, an apply in flight, an apply that
// failed, a configuration it could not read, a restart owed, a reason the
// seam does not explain) only once the request presents a run token this
// fleet signed, and then in exactly the words a person is told on /git/. Any
// other request — no credential, a person's bearer, another user name than
// the box's, an operator token as the password, a token another fleet signed
// or another of this fleet's per-run edges did — is answered the code host's
// own challenge, the box admission's credential step, in the same bytes
// whatever the reason: it learns nothing of the deployment's state.
//
// Mutations: answer the reason before the credential (boxGitRoute calling
// unservedGit first, as gitRoute does) — the first row's request without a
// credential is told the 404, red; challenge in a person's words
// (auth.GitChallenge) — red.
func TestAnUnservedBoxRouteAnswersOnlyASignedToken(t *testing.T) {
	t.Parallel()
	keyring := runtoken.OneKey("k", "this fleet's key")
	boxes, checks := boxTokensOf(t, keyring)
	otherFleet, _ := boxTokensOf(t, runtoken.OneKey("k", "another fleet's key"))
	bridge, err := runtoken.New(runtoken.Options{Domain: mcpbridge.KeyDomain, Material: keyring})
	if err != nil {
		t.Fatal(err)
	}
	run := host.BoxRunRef{TurnID: uuid.Must(uuid.NewV7()).String(), LaunchID: uuid.NewString()}
	basic := func(user, password string) string { return "Basic " + basicAuth(user, password) }
	signed := basic(host.BoxUser, boxes.Mint(run))
	unsigned := []struct{ name, authorization string }{
		{"no credential", ""},
		{"a person's bearer", "Bearer secret"},
		{"another user name, with a signed token", basic("crewlet", boxes.Mint(run))},
		{"an operator token as the password", basic(host.BoxUser, "secret")},
		{"another fleet's token", basic(host.BoxUser, otherFleet.Mint(run))},
		{"the tool bridge's token, over this keyring", basic(host.BoxUser,
			bridge.Mint(run.LaunchID+"~"+run.TurnID, time.Hour))},
	}
	verify := func(user, token string) bool { return host.VerifyBox(boxes, checks, user, token) }
	for _, reason := range unservedReasons {
		t.Run(reason.name, func(t *testing.T) {
			t.Parallel()
			a := codeApp(t, nil, fixedCode{why: reason.why, boxes: boxes, checks: checks})
			// WHAT A PERSON IS TOLD, which a signed box is told too.
			told := gitAnswer(ask(t, a, http.MethodGet, "/git/web.git/info/refs?service=git-upload-pack", true))
			for _, req := range boxRequests {
				for _, row := range unsigned {
					// THE ADMISSION'S OWN CHALLENGE of the same request.
					want := httptest.NewRecorder()
					if !host.ChallengeBox(want, boxRequest(req.method, req.path, row.authorization), verify,
						slog.New(slog.DiscardHandler)) {
						t.Fatalf("%s: the box admission admits it, so it is no unsigned request", row.name)
					}
					rec := httptest.NewRecorder()
					a.ServeHTTP(rec, boxRequest(req.method, req.path, row.authorization))
					switch got := gitAnswer(rec); {
					case rec.Code != http.StatusUnauthorized ||
						rec.Header().Get("WWW-Authenticate") != `Basic realm="crewlet", charset="UTF-8"`:
						t.Errorf("%s %s with %s answered %s, want git's 401 challenge", req.method, req.path,
							row.name, got)
					case got != gitAnswer(want):
						t.Errorf("%s %s with %s answered\n  %s\nwhere the box admission answers\n  %s",
							req.method, req.path, row.name, got, gitAnswer(want))
					case strings.Contains(rec.Body.String(), "API token"):
						t.Errorf("%s %s with %s was challenged in a person's words: %s", req.method, req.path,
							row.name, got)
					}
				}
				rec := httptest.NewRecorder()
				a.ServeHTTP(rec, boxRequest(req.method, req.path, signed))
				if got := gitAnswer(rec); got != told {
					t.Errorf("%s %s with a signed token answered\n  %s\nwhere a person is told\n  %s",
						req.method, req.path, got, told)
				}
			}
		})
	}
}

// unservedReasons is every reason a node serves none of the code host now, as
// the engine names it — and one the seam does not explain.
var unservedReasons = []struct {
	name string
	why  error
}{
	{"not hosted", engine.ErrCodeNotHosted},
	{"applying", fmt.Errorf("%w (epoch 4)", engine.ErrCodeApplying)},
	{"apply failed", fmt.Errorf("%w (epoch 4)", engine.ErrCodeApplyFailed)},
	{"unknown", fmt.Errorf("%w (epoch 4)", engine.ErrCodeUnknown)},
	{"needs restart", engine.ErrCodeNeedsRestart},
	{"unexplained", errors.New("a seam that says something else")},
}

// boxTokensOf is a coding box's token signer and a check's box's, both over
// keyring.
func boxTokensOf(t *testing.T, keyring runtoken.Material) (*host.BoxTokens, *host.CheckTokens) {
	t.Helper()
	boxes, err := host.NewBoxTokens(keyring, nil)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := host.NewCheckTokens(keyring, nil)
	if err != nil {
		t.Fatal(err)
	}
	return boxes, checks
}

// boxRequest is a box's request of path, carrying authorization where it is
// set.
func boxRequest(method, path, authorization string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	return r
}

// gitAnswer is what a client of a git route is shown: its status and the
// three headers a git route's answer can carry, then its words.
func gitAnswer(rec *httptest.ResponseRecorder) string {
	return fmt.Sprintf("%d [%s] [%s] [%s] %q", rec.Code, rec.Header().Get("Content-Type"),
		rec.Header().Get("WWW-Authenticate"), rec.Header().Get("Retry-After"), rec.Body.String())
}

// boxCodeRequests is one request of each of a check's box's reads, and what
// fakeGitRoutes answers it where its admission holds none.
var boxCodeRequests = []struct{ method, path string }{
	{http.MethodGet, "/box/code/web/tree"},
	{http.MethodGet, "/box/code/web/blob?path=go.mod"},
	{http.MethodGet, "/box/code/web/diff"},
}

// aCheckRef is one attempt of a check, as its box is given repository web.
func aCheckRef() host.CheckRunRef {
	return host.CheckRunRef{Repo: "web", Attempt: code.CheckKey{RepoID: uuid.NewString(),
		OID: strings.Repeat("ab", 20), Name: "lint.go", Attempt: 2}}
}

// AN UNSERVED BOX ROUTE ANSWERS A CHECK'S TOKEN AS IT ANSWERS A RUN'S. A node
// serving none of the code host now tells a check's box its reason — on its
// git (/box/git/) and on its reads of its commit (/box/code/) alike — only once
// the request presents a check's token this fleet signed, as `check`, and then
// in exactly the words a person is told on /git/. A check's token presented as
// a coding run's (`run`), a run's presented as a check's, and a check's token
// another fleet signed are each answered the code host's own challenge, in the
// bytes the box admission's credential step answers — so the reason reaches
// nobody holding no credential this fleet signed for that user name.
//
// Mutations: CodeSurface.VerifyBox judging a check's token by the run tokens
// alone (no check token verifies) — the signed rows answer the challenge,
// red; VerifyBox accepting either token under either user name (no user in
// the rule) — the crossed rows are told the reason, red; boxCodeRoute
// answering the reason before the credential — red.
func TestAnUnservedBoxRouteAnswersACheckToken(t *testing.T) {
	t.Parallel()
	keyring := runtoken.OneKey("k", "this fleet's key")
	boxes, checks := boxTokensOf(t, keyring)
	_, otherFleet := boxTokensOf(t, runtoken.OneKey("k", "another fleet's key"))
	verify := func(user, token string) bool { return host.VerifyBox(boxes, checks, user, token) }
	ref := aCheckRef()
	run := host.BoxRunRef{TurnID: uuid.Must(uuid.NewV7()).String(), LaunchID: uuid.NewString()}
	basic := func(user, password string) string { return "Basic " + basicAuth(user, password) }
	signed := basic(host.CheckUser, checks.Mint(ref, time.Minute))
	unsigned := []struct{ name, authorization string }{
		{"a check's token as a run's", basic(host.BoxUser, checks.Mint(ref, time.Minute))},
		{"a run's token as a check's", basic(host.CheckUser, boxes.Mint(run))},
		{"another fleet's check token", basic(host.CheckUser, otherFleet.Mint(ref, time.Minute))},
		{"no credential", ""},
	}
	var requests []struct{ method, path string }
	for _, req := range boxRequests {
		requests = append(requests, struct{ method, path string }{req.method, req.path})
	}
	requests = append(requests, boxCodeRequests...)
	for _, reason := range unservedReasons {
		t.Run(reason.name, func(t *testing.T) {
			t.Parallel()
			a := codeApp(t, nil, fixedCode{why: reason.why, boxes: boxes, checks: checks})
			told := gitAnswer(ask(t, a, http.MethodGet, "/git/web.git/info/refs?service=git-upload-pack", true))
			for _, req := range requests {
				for _, row := range unsigned {
					want := httptest.NewRecorder()
					if !host.ChallengeBox(want, boxRequest(req.method, req.path, row.authorization), verify,
						slog.New(slog.DiscardHandler)) {
						t.Fatalf("%s: the box admission admits it, so it is no unsigned request", row.name)
					}
					rec := httptest.NewRecorder()
					a.ServeHTTP(rec, boxRequest(req.method, req.path, row.authorization))
					if got := gitAnswer(rec); rec.Code != http.StatusUnauthorized || got != gitAnswer(want) {
						t.Errorf("%s %s with %s answered\n  %s\nwhere the box admission answers\n  %s",
							req.method, req.path, row.name, got, gitAnswer(want))
					}
				}
				rec := httptest.NewRecorder()
				a.ServeHTTP(rec, boxRequest(req.method, req.path, signed))
				if got := gitAnswer(rec); got != told {
					t.Errorf("%s %s with a signed check token answered\n  %s\nwhere a person is told\n  %s",
						req.method, req.path, got, told)
				}
			}
		})
	}
}

// fakeGitReads records every read by object id it is asked, and answers each
// with a page naming what it read.
type fakeGitReads struct {
	mu    sync.Mutex
	asked []code.GitQuery
}

func (f *fakeGitReads) GitRead(_ context.Context, q code.GitQuery) (code.GitAnswer, error) {
	f.mu.Lock()
	f.asked = append(f.asked, q)
	f.mu.Unlock()
	a := code.GitAnswer{Kind: q.Kind, RepoID: q.RepoID, Next: "n",
		PushedBy: map[string]code.PushedBy{q.Check.OID: {}}}
	switch q.Kind {
	case code.GitTree:
		a.Tree = &code.TreeAnswer{}
	case code.GitBlob:
		a.Blob = &code.BlobAnswer{}
	case code.GitDiff:
		a.Diff = &code.DiffAnswer{}
	}
	return a, nil
}

func (f *fakeGitReads) queries() []code.GitQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// A CHECK'S API READS ONLY ITS COMMIT. Each of a check's box's three reads,
// once admitted, is asked of the code domain's git reads AT THE ATTEMPT'S
// COMMIT — a tree and a file at it, a diff from the attempt's base to it — as
// a read made for the check: no asker, the attempt named, at the check's
// session. A revision the request names (`rev`, `base`, `head`, a blob's own
// id) is refused before anything is read, as is a parameter no read takes
// and one named twice, and the answer carries the commit's content alone —
// none of the push provenance a seat's read carries. And the git node holds
// a read made for a check to its commit itself (code.GitQuery.Validate), so a
// node that built one at another commit is refused there too.
//
// Mutations: boxCodeQuery reading a `rev` parameter into the tree read — the
// row naming another commit is read, red; dropping the query's Check (a read
// as nobody, held to nothing) — red; boxCodeAnswer carrying PushedBy — red;
// atItsCheck accepting any revision — the last half red.
func TestTheCheckAPIReadsOnlyItsSha(t *testing.T) {
	t.Parallel()
	ref := aCheckRef()
	base := strings.Repeat("cd", 20)
	session := statelog.FloorAt(statelog.ReadSession,
		statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 9})
	admitted := host.CheckAdmitted{Attempt: code.CheckRow{CheckKey: ref.Attempt, State: code.CheckStateRunning,
		Base: base}, Repo: code.RepoRef{ID: ref.Attempt.RepoID, Name: "web"}, Fresh: session}
	reads := &fakeGitReads{}
	a := codeApp(t, nil, fixedCode{git: fakeGitRoutes{admitted: &admitted}, reads: &fakeCodeReads{},
		gitReads: reads})
	other := strings.Repeat("ef", 20)
	for _, refused := range []string{
		"/box/code/web/tree?rev=" + other,
		"/box/code/web/blob?path=go.mod&rev=" + other,
		"/box/code/web/diff?base=" + other,
		"/box/code/web/diff?head=" + other,
		"/box/code/web/blob?path=go.mod&path=README.md",
		"/box/code/web/tree?start_line=2",
		"/box/code/web/blob?start_line=two",
	} {
		rec := ask(t, a, http.MethodGet, refused, false)
		if got, _ := errorBody(t, rec); rec.Code != http.StatusBadRequest || got != string(httpjson.CodeBadParams) {
			t.Errorf("GET %s answered %d %q, want 400 bad_params", refused, rec.Code, rec.Body.String())
		}
	}
	if asked := reads.queries(); len(asked) != 0 {
		t.Fatalf("refused requests asked the git reads %+v", asked)
	}
	for _, read := range []struct {
		path string
		kind code.GitReadKind
		rev  func(code.GitQuery) string
	}{
		{"/box/code/web/tree?path=cmd&recursive=true", code.GitTree,
			func(q code.GitQuery) string { return q.Tree.Rev }},
		{"/box/code/web/blob?path=go.mod&start_line=2&end_line=9", code.GitBlob,
			func(q code.GitQuery) string { return q.Blob.Rev }},
		{"/box/code/web/diff?paths=cmd&paths=go.mod&renames=true", code.GitDiff,
			func(q code.GitQuery) string { return q.Diff.Head }},
	} {
		before := len(reads.queries())
		rec := ask(t, a, http.MethodGet, read.path, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s answered %d %q", read.path, rec.Code, rec.Body.String())
		}
		asked := reads.queries()
		if len(asked) != before+1 {
			t.Fatalf("GET %s asked the git reads %d times", read.path, len(asked)-before)
		}
		q := asked[before]
		switch {
		case q.Kind != read.kind || read.rev(q) != ref.Attempt.OID || q.RepoID != ref.Attempt.RepoID:
			t.Errorf("GET %s asked %+v, want a %s read of %s at %s", read.path, q, read.kind,
				ref.Attempt.RepoID, ref.Attempt.OID)
		case q.Check == nil || *q.Check != ref.Attempt || q.Asker != (code.Principal{}) || q.CeilingSeat != "":
			t.Errorf("GET %s asked as %+v for %+v, want the engine's own read made for %+v", read.path,
				q.Asker, q.Check, ref.Attempt)
		case !reflect.DeepEqual(q.Fresh, session):
			t.Errorf("GET %s read at %+v, want the check's session %+v", read.path, q.Fresh, session)
		case q.Kind == code.GitDiff &&
			(q.Diff.Base != base || !slices.Equal(q.Diff.Paths, []string{"cmd", "go.mod"})):
			t.Errorf("GET %s asked the diff %+v, want from the attempt's base %s over its paths", read.path,
				q.Diff, base)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["commit"] != ref.Attempt.OID || body["repo"] != "web" || body["next"] != "n" {
			t.Errorf("GET %s answered %v, want the repository, the commit and the next page", read.path, body)
		}
		if _, carried := body["pushed_by"]; carried {
			t.Errorf("GET %s answered the push provenance a seat's read carries: %v", read.path, body)
		}
	}

	// THE GIT NODE'S OWN HOLD: a read made for the check at any other commit
	// is refused as it is validated, whoever built it.
	at := func(rev string) []code.GitQuery {
		check := ref.Attempt
		return []code.GitQuery{
			{Kind: code.GitTree, RepoID: check.RepoID, Check: &check, Tree: &code.TreeQuery{Rev: rev}},
			{Kind: code.GitBlob, RepoID: check.RepoID, Check: &check,
				Blob: &code.BlobQuery{Rev: rev, Path: "go.mod"}},
			{Kind: code.GitDiff, RepoID: check.RepoID, Check: &check,
				Diff: &code.DiffQuery{Base: base, Head: rev}},
		}
	}
	for _, q := range at(other) {
		if err := q.Validate(); !errors.Is(err, code.ErrInvalid) {
			t.Errorf("a %s read made for the check at another commit validated: %v", q.Kind, err)
		}
	}
	for _, q := range at(ref.Attempt.OID) {
		if err := q.Validate(); err != nil {
			t.Errorf("a %s read made for the check at its commit was refused: %v", q.Kind, err)
		}
	}
	asked := at(ref.Attempt.OID)[0]
	asked.Asker = code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ops", Seat: "ceo"}
	if err := asked.Validate(); !errors.Is(err, code.ErrInvalid) {
		t.Errorf("a read made for the check naming an asker validated: %v", err)
	}
}

// THE CODE READS ANSWER IN THE READ SURFACE'S ONE VOCABULARY. A repository that
// is not declared is `not_found`; one the asker's grant does not cover is
// `not_permitted`, 403, with the reader's own sentence and not the sentinel's
// Go text — answered by this node or wrapped by the router from another; a
// level, a cursor or a parameter the reader refuses is `bad_params` with the
// reader's own sentence; a read this node could not serve — a state-log
// refusal, no data node serving — is `unavailable`, with its Retry-After where
// waiting clears it and none where it cannot (an evicted node). The level is
// the tree's one freshness grammar, and a parameter no read takes is refused
// before the reader is asked.
//
// Mutation: drop the ErrNotPermitted case of the code questions' refusal mapping
// (queries' codeRefusal), and both refusals answer 500 query_failed with no sentence.
func TestCodeReadsAnswerInTheReadVocabulary(t *testing.T) {
	t.Parallel()
	shutOut := `seat "dev" holds no access to repository "secret": it is visibility: unit, read by ` +
		`the seats of unit "platform" and the leads above it and by persons`
	// said is the reader's sentence as the envelope writes a detail.
	said := httpjson.Sentence(shutOut)
	refused := map[string]error{
		"secret": fmt.Errorf("%w: %s", code.ErrNotPermitted, shutOut),
		"routed": fmt.Errorf("estate: data node n2 answered: %w",
			fmt.Errorf("%w: %s", code.ErrNotPermitted, shutOut)),
		"web":     code.ErrRepoNotFound,
		"level":   fmt.Errorf("%w: a session read names the position it must include", code.ErrBadLevel),
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
		reason      string
		detail      string
		retryAfter  string
		readerAsked bool
	}{
		{"/code/repos/web", http.StatusNotFound, "not_found", "not_declared", `"web" is declared in code.repos`, "",
			true},
		{"/code/repos/secret", http.StatusForbidden, "not_permitted", "", said, "", true},
		{"/code/repos/routed/pushes", http.StatusForbidden, "not_permitted", "", said, "", true},
		{"/code/repos/level", http.StatusBadRequest, "bad_params", "", "names the position it must include", "", true},
		{"/code/repos/cursor/pushes", http.StatusBadRequest, "bad_params", "", "not a cursor", "", true},
		{"/code/repos/behind", http.StatusServiceUnavailable, "unavailable", "", "", "7", true},
		{"/code/repos/nobody", http.StatusServiceUnavailable, "unavailable", "", "", "5", true},
		{"/code/repos/evicted", http.StatusServiceUnavailable, "unavailable", "", "", "", true},
		{"/code/repos?read_level=bogus", http.StatusBadRequest, "bad_params", "", "bogus", "", false},
		{"/code/repos?status=gone", http.StatusBadRequest, "bad_params", "", `"gone"`, "", false},
		{"/code/repos?limit=-1", http.StatusBadRequest, "bad_params", "", "limit", "", false},
		{"/code/repos?limit=ten", http.StatusBadRequest, "bad_params", "", "limit", "", false},
		{"/code/repos/site/pushes?brief=yes", http.StatusBadRequest, "bad_params", "", "brief", "", false},
	} {
		before := len(reads.queries())
		rec := ask(t, a, http.MethodGet, tc.path, true)
		code, detail := errorBody(t, rec)
		var reason struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &reason)
		if rec.Code != tc.status || code != tc.code || reason.Reason != tc.reason ||
			!strings.Contains(detail, tc.detail) || rec.Header().Get("Retry-After") != tc.retryAfter {
			t.Errorf("GET %s answered %d %s (reason %q) %q (Retry-After %q), want %d %s (reason %q) naming %q "+
				"(Retry-After %q)", tc.path, rec.Code, code, reason.Reason, detail, rec.Header().Get("Retry-After"),
				tc.status, tc.code, tc.reason, tc.detail, tc.retryAfter)
		}
		if asked := len(reads.queries()) > before; asked != tc.readerAsked {
			t.Errorf("GET %s asked the reader: %v, want %v", tc.path, asked, tc.readerAsked)
		}
		if tc.code == "not_permitted" && detail != said {
			t.Errorf("GET %s answered the detail %q, want the reader's sentence alone, %q", tc.path, detail, said)
		}
	}

	// AND WHAT IS ASKED IS WHAT ARRIVES: the kind, the repository, the
	// filter, the page, a history in brief or one push of it, and the level,
	// through the one grammar.
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
		{"/code/repos/site/pushes?brief=true", code.ReadQuery{Kind: code.ReadPushes, Repo: "site",
			Brief: true, Fresh: statelog.Freshness{Level: statelog.ReadStale}}},
		{"/code/repos/site/pushes?push=0192f4c8-0000-7000-8000-000000000001&brief=false",
			code.ReadQuery{Kind: code.ReadPushes, Repo: "site", Push: "0192f4c8-0000-7000-8000-000000000001",
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
			got.Limit != tc.want.Limit || got.Cursor != tc.want.Cursor || got.Brief != tc.want.Brief ||
			got.Push != tc.want.Push || !reflect.DeepEqual(got.Fresh, tc.want.Fresh) {
			t.Errorf("GET %s asked %+v, want %+v", tc.path, got, tc.want)
		}
		// A token the directory binds to no seat asks as itself, under
		// the login the guard composes it with.
		if want := (code.Principal{Kind: code.PrincipalOperator, OperatorID: iam.TokenLogin("founder")}); got.Asker != want {
			t.Errorf("GET %s asked as %+v, want %+v", tc.path, got.Asker, want)
		}
	}
}

// ONE REFUSAL HAS ONE SPELLING: a code read the asker's grant does not cover is
// refused with the same word a refused push prints on the ref git names, so a
// script reading either surface branches on one value. Declared twice by
// necessity — the envelope's table is the API's, the push's refusal class the
// domain's — so the two are held equal here.
func TestNotPermittedIsTheDomainsOwnWord(t *testing.T) {
	t.Parallel()
	if string(httpjson.CodeNotPermitted) != string(code.RefusalNotPermitted) {
		t.Errorf("the envelope says %q and a refused push %q", httpjson.CodeNotPermitted,
			code.RefusalNotPermitted)
	}
}

// A CODE READ ASKS AS THE REQUEST'S PERSON: the principal the guard resolved,
// under the credential it came through, and the human seat it holds — the one
// the identity directory binds a token to — or none. A REST caller is a person
// or a credential, never a seat, so the reader judges what it answers by a
// person's grant (ADR-0058); and the conversion is the code tools' own
// (builtin.CodeAskerOf), so a person's REST read and their assistant's code
// tool ask as one asker.
//
// Mutation: the asker's seat left empty → the bound token asks unbound; the
// credential replaced by the principal's display name → each token asks as
// another name than the one its writes are recorded under.
func TestACodeReadAsksAsTheRequestsPerson(t *testing.T) {
	t.Parallel()
	reads := &fakeCodeReads{}
	b := config.DefaultBootstrap()
	authorize(&b, config.APIToken{ID: "founder", Token: "secret"},
		config.APIToken{ID: "ci", Token: "ci-secret"})
	seats := map[string]string{iam.TokenLogin("founder"): "jane-founder"}
	a := newApp(t, api.Options{Bootstrap: &b, Code: fixedCode{reads: reads},
		SeatBindings: auth.SeatBindings{Directory: inboxBindings(seats), Chart: inboxChart(seats)}})
	asked := func(token string) code.Principal {
		t.Helper()
		rec := ask(t, bearing(a, token), http.MethodGet, "/code/repos", false)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /code/repos with the token %q answered %d %q", token, rec.Code, rec.Body.String())
		}
		q := reads.queries()
		return q[len(q)-1].Asker
	}
	for token, want := range map[string]code.Principal{
		"secret": {Kind: code.PrincipalOperator, OperatorID: iam.TokenLogin("founder"),
			Seat: "jane-founder"},
		"ci-secret": {Kind: code.PrincipalOperator, OperatorID: iam.TokenLogin("ci")},
	} {
		if got := asked(token); got != want {
			t.Errorf("the token %q asked as %+v, want %+v", token, got, want)
		}
	}
}

// bearing is h with every request it serves carrying token as its bearer.
func bearing(h http.Handler, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(w, r)
	})
}

// A CODE QUESTION ASKED FOR NOBODY IS NEVER JUDGED. The guard resolves every
// request it admits, so a question carrying no principal reached the registry
// past it — and each is refused as the registry refuses every question asked
// so, never read: a caller who presented nothing as unauthenticated, one this
// node could not tell as unavailable, and a context no resolver touched as the
// wiring fault it is. The reader holds every read to who asks, and is not
// asked one for nobody.
//
// Mutation: answer a code question before the registry decides who asks
// (register it with a posture that skips the check), and every one asks the
// reader with no asker.
func TestACodeReadForNobodyIsRefused(t *testing.T) {
	t.Parallel()
	reads, readers := &fakeCodeReads{}, &fakeRepoReaders{}
	a := codeApp(t, nil, fixedCode{reads: reads, readers: readersOn(t, readers)})
	for name, ctx := range map[string]context.Context{
		"nobody presented anything": iam.WithAnonymous(t.Context()),
		"this node could not tell":  iam.WithUnresolved(t.Context(), errors.New("the identity estate is behind")),
		"no resolver touched it":    t.Context(),
	} {
		for _, q := range codeQuestions {
			if _, err := a.Queries().AnswerWith(ctx, q.what,
				queries.FromMap(map[string]any{"name": "web"})); err == nil {
				t.Errorf("%s: %s was answered", name, q.what)
			}
		}
	}
	if asked := reads.queries(); len(asked) != 0 {
		t.Errorf("the reader was asked %d reads for nobody: %+v", len(asked), asked)
	}
	if asked := readers.asks(); len(asked) != 0 {
		t.Errorf("the readers were asked %d pages for nobody: %+v", len(asked), asked)
	}
}

// THE CODE ROUTES ARE GUARDED, reads included, as every route the exemption
// list does not name is: a repository is the company's source, and the reads
// name every repository and the people who pushed to it. A code read with no
// credential is refused in the API's JSON; a git route in git's own form, the
// Basic challenge git answers with its credential helper.
func TestCodeReadsAreAlwaysGuarded(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b,
		Code: fixedCode{git: fakeGitRoutes{}, reads: &fakeCodeReads{}}})
	for _, path := range []string{"/code/repos", "/code/repos/web", "/code/repos/web/pushes",
		"/code/repos/web/readers"} {
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

	// A FLOOR ON ANOTHER LOG, alone or beside the code log's own, is refused
	// before the host is asked: `min_position` names one floor per log, and a
	// verify reads the code log alone.
	for _, bad := range []string{"web/verify?repair=maybe", "web/verify?repair=1", "web/verify?repair=true&repair=false",
		"web/verify?min_position=yesterday", "web/verify?min_position=CREWLET_TRACKER_LOG@1:2",
		"web/verify?min_position=CREWLET_CODE_LOG@1:1&min_position=CREWLET_TRACKER_LOG@1:2",
		"web/verify?min_position=CREWLET_CODE_LOG@1:1&min_position=CREWLET_CODE_LOG@1:2"} {
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
		{"gone", "not_found", "not_declared", `repository named "gone" is declared in code.repos`, "",
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
