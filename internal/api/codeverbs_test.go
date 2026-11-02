package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CODE ROUTES ARE DECIDED BY THE AUTHORITY TABLE, each on the verb it is
// mounted with.
//
// A VERIFY IS ADMIN'S, as the deployment's other controls are: it reads every
// byte of THIS NODE's copy of a repository, holds a maintenance slot and the
// memory its check needs, and with `repair=true` rewrites the copy every fetch
// on the node is served from. Mounted on the plain mux it asked only whether
// somebody resolved, so a person holding nothing but the overview — or a
// machine token carrying one app — could start a check and its repair on
// every repository a git node keeps. So a caller holding every app and not
// admin is refused it `403` naming admin, before the code host is asked, and
// its read the same.
//
// THE READS AND THE GIT ROUTES ADMIT EVERYBODY THE GUARD RESOLVED: what each
// may read, fetch or push is the per-repository grant the serving node
// decides (ADR-0081), as a work item's thread is admitted for the comment's
// own decision. The same caller reaches every one of them — the control that
// shows the refusal above is the verb's and not the credential's.
//
// Mutation: mount the code routes on the plain mux again, and the colleague
// starts a check; mount the reads or the git routes on an admin verb, and the
// colleague is refused them.
func TestTheCodeRoutesAreDecidedByTheAuthorityTable(t *testing.T) {
	t.Parallel()
	const colleague = "a-token-that-carries-every-app-and-not-admin"
	b := closedPosture()
	authorize(&b, config.APIToken{ID: "founder", Token: "secret"},
		config.APIToken{ID: "colleague", Token: colleague, Grants: slices.Clone(iam.Apps)})
	var started atomic.Int32
	git := fakeGitRoutes{start: func(_ context.Context, name string, repair bool, _ statelog.Position) (
		host.Verification, error) {
		started.Add(1)
		return host.Verification{ID: uuid.Must(uuid.NewV7()), Repo: name, Repair: repair,
			State: host.VerifyRunning, Started: time.Now()}, nil
	}}
	a := newApp(t, api.Options{Bootstrap: &b, Code: fixedCode{git: git, reads: &fakeCodeReads{}}})
	as := func(token, method, path string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, r)
		return rec
	}

	verify := []struct{ method, path string }{
		{http.MethodPost, "/code/repos/web/verify"},
		{http.MethodGet, "/code/repos/web/verify/" + uuid.Must(uuid.NewV7()).String()},
	}
	for _, req := range verify {
		rec := as(colleague, req.method, req.path)
		var body struct {
			Error  string   `json:"error"`
			Grants []string `json:"grants"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s %s answered %d %q: %v", req.method, req.path, rec.Code, rec.Body.String(), err)
		}
		if rec.Code != http.StatusForbidden || body.Error != string(httpjson.CodeUnauthorized) ||
			!slices.Equal(body.Grants, []string{string(iam.GrantAdmin)}) {
			t.Errorf("%s %s by a caller holding every app answered %d %s, want 403 %s naming [%s]",
				req.method, req.path, rec.Code, rec.Body.String(), httpjson.CodeUnauthorized, iam.GrantAdmin)
		}
	}
	if n := started.Load(); n != 0 {
		t.Errorf("a refused verify reached the code host %d times", n)
	}
	// AND ADMIN TAKES IT: the refusal is the verb's, not a route that
	// refuses everybody.
	if rec := as("secret", http.MethodPost, "/code/repos/web/verify"); rec.Code != http.StatusAccepted ||
		started.Load() != 1 {
		t.Errorf("an admin's verify answered %d %q (%d started), want 202 and one check begun",
			rec.Code, rec.Body.String(), started.Load())
	}

	for _, path := range []string{"/code/repos", "/code/repos/web", "/code/repos/web/pushes"} {
		if rec := as(colleague, http.MethodGet, path); rec.Code != http.StatusOK {
			t.Errorf("GET %s by a caller holding every app answered %d %q, want the read",
				path, rec.Code, rec.Body.String())
		}
	}
	for _, req := range gitRequests {
		rec := as(colleague, req.method, req.path)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "served ") {
			t.Errorf("%s %s by a caller holding every app answered %d %q, want the git host's own answer",
				req.method, req.path, rec.Code, rec.Body.String())
		}
	}
}

// A GIT ROUTE'S ROUTER REFUSES IN GIT'S FORM: one line of text, never the JSON
// envelope a REST route answers, because git prints a refusal's text and
// nothing of a JSON body — under the status and the code a JSON route answers
// the same decision with, and a decision this node could not make saying when
// to come back. The guard in front of every route answers first whatever this
// could refuse on a path the app serves (item 1's rows), so the renderer is
// held here by itself.
//
// Mutation: render through authz.EnvelopeRefusal instead, and every row goes
// red on its content type.
func TestAGitRoutesRefusalIsOneLineGitPrints(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		d          authz.Decision
		status     int
		code       httpjson.Code
		retryAfter string
	}{
		{"a principal whose stage may not act", authz.Decision{Reason: authz.ReasonStage},
			http.StatusForbidden, httpjson.CodeUnauthorized, ""},
		{"a node that could not decide", authz.Decision{Err: errors.New("the directory is unreadable")},
			http.StatusServiceUnavailable, httpjson.CodeUnavailable, strconv.Itoa(authz.RetryUndecidedSeconds)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			api.GitRefusal(rec, httptest.NewRequest(http.MethodGet, "/git/web.git/info/refs", nil),
				authz.Policy{Action: authz.ActionCodeGit}, tc.d)
			body := rec.Body.String()
			if rec.Code != tc.status || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") ||
				strings.Count(body, "\n") != 1 || !strings.HasSuffix(body, "("+string(tc.code)+")\n") ||
				rec.Header().Get("Retry-After") != tc.retryAfter {
				t.Errorf("answered %d %q %q (Retry-After %q), want %d one text/plain line ending (%s) "+
					"(Retry-After %q)", rec.Code, rec.Header().Get("Content-Type"), body,
					rec.Header().Get("Retry-After"), tc.status, tc.code, tc.retryAfter)
			}
		})
	}
}
