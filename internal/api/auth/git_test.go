package auth_test

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
)

// THE ONE CLASSIFIER, ROUTE BY ROUTE. Every request git's smart HTTP protocol
// makes, and the shapes around it that must not be taken for one: a fetch is
// what may be served while a node drains and by a token bound to nobody, so
// every row that answers fetch is a row a mistake here would open to a push.
func TestGitAccessClassifiesEveryGitRoute(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		method, target string
		want           auth.GitOp
	}{
		// The advertisement, asked for each service.
		{http.MethodGet, "/git/r.git/info/refs?service=git-upload-pack", auth.GitFetch},
		{http.MethodGet, "/git/r.git/info/refs?service=git-receive-pack", auth.GitPush},
		// No service is the dumb protocol's request: a read, which the host
		// answers by refusing it.
		{http.MethodGet, "/git/r.git/info/refs", auth.GitFetch},
		{http.MethodHead, "/git/r.git/info/refs?service=git-upload-pack", auth.GitFetch},
		{http.MethodHead, "/git/r.git/info/refs?service=git-receive-pack", auth.GitPush},
		// The two RPCs.
		{http.MethodPost, "/git/r.git/git-upload-pack", auth.GitFetch},
		{http.MethodPost, "/git/r.git/git-receive-pack", auth.GitPush},
		// Everything else under the prefix is a push: the more restrictive
		// answer, and the mux then answers it 404.
		{http.MethodGet, "/git/r.git/HEAD", auth.GitPush},
		{http.MethodGet, "/git/r.git", auth.GitPush},
		{http.MethodGet, "/git/", auth.GitPush},
		{http.MethodGet, "/git/r.git/objects/info/packs", auth.GitPush},
		{http.MethodGet, "/git/r.git/git-upload-pack", auth.GitPush},
		{http.MethodPost, "/git/r.git/info/refs?service=git-upload-pack", auth.GitPush},
		{http.MethodGet, "/git/r.git/info/refs?service=git-upload-archive", auth.GitPush},
		{http.MethodGet, "/git/r.git/info/refs?service=", auth.GitFetch},
		// A service another reader could take for receive-pack is a push:
		// two values, in either order, and a query that does not parse —
		// url.Values drops the pair it cannot read, which would leave the
		// first reading "no service" and another reading what it kept.
		{http.MethodGet, "/git/r.git/info/refs?service=git-upload-pack&service=git-receive-pack", auth.GitPush},
		{http.MethodGet, "/git/r.git/info/refs?service=git-receive-pack&service=git-upload-pack", auth.GitPush},
		{http.MethodGet, "/git/r.git/info/refs?service=git-receive-pack;x=1", auth.GitPush},
		{http.MethodGet, "/git/r.git/info/refs?service=git-upload-pack&x=%zz", auth.GitPush},
		// A path that only resembles the RPC's: the repository segment
		// ends at the first slash, so its tail is not the upload RPC.
		{http.MethodPost, "/git/a/b/git-upload-pack", auth.GitPush},
		{http.MethodPost, "/git/r.git//git-upload-pack", auth.GitPush},
		// Not a git route at all.
		{http.MethodGet, "/github/x", auth.GitNone},
		{http.MethodGet, "/gitx", auth.GitNone},
		{http.MethodGet, "/git", auth.GitNone},
		{http.MethodGet, "/code/repos", auth.GitNone},
		{http.MethodPost, "/webhooks/github", auth.GitNone},
	} {
		r := httptest.NewRequest(tc.method, tc.target, nil)
		if got := auth.GitAccess(r); got != tc.want {
			t.Errorf("%s %s = %q, want %q", tc.method, tc.target, got, tc.want)
		}
		if got := auth.GitAccess(r); !got.Valid() {
			t.Errorf("%s %s answered %q, which is not one of the three", tc.method, tc.target, got)
		}
	}
	for _, op := range []auth.GitOp{"pull", "FETCH", "unknown"} {
		if op.Valid() {
			t.Errorf("%q is valid, and the set is closed", op)
		}
	}
}

// AN ADVERTISEMENT OPENS AN EXCHANGE, AND NOTHING ELSE DOES. A draining node
// refuses a fetch's advertisement — a fetch it would have to begin — and serves
// the upload-pack RPC that continues one already begun, here or on a peer. So
// a row answering true here that is not git's first request would refuse a
// fetch half done, and an advertisement answering false would begin a new one
// on a node that is going away.
func TestGitAdvertisementOpensAnExchange(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		method, target string
		want           bool
	}{
		// The advertisement, whatever it asks for: a fetch's and a push's
		// alike, and the dumb protocol's with no service at all.
		{http.MethodGet, "/git/r.git/info/refs?service=git-upload-pack", true},
		{http.MethodGet, "/git/r.git/info/refs?service=git-receive-pack", true},
		{http.MethodGet, "/git/r.git/info/refs", true},
		{http.MethodHead, "/git/r.git/info/refs?service=git-upload-pack", true},
		{http.MethodGet, "/git/r.git/info/refs?service=git-upload-pack&x=%zz", true},
		// The RPCs continue an exchange.
		{http.MethodPost, "/git/r.git/git-upload-pack", false},
		{http.MethodPost, "/git/r.git/git-receive-pack", false},
		// A POST to info/refs is nothing git sends, and the rest of the
		// prefix is not an exchange at all.
		{http.MethodPost, "/git/r.git/info/refs?service=git-upload-pack", false},
		{http.MethodGet, "/git/r.git/HEAD", false},
		{http.MethodGet, "/git/r.git", false},
		{http.MethodGet, "/git/a/b/info/refs", false},
		{http.MethodGet, "/git/r.git/info/refs/", false},
		// Not a git route.
		{http.MethodGet, "/github/info/refs", false},
		{http.MethodGet, "/code/repos", false},
	} {
		r := httptest.NewRequest(tc.method, tc.target, nil)
		if got := auth.GitAdvertisement(r); got != tc.want {
			t.Errorf("%s %s: advertisement = %v, want %v", tc.method, tc.target, got, tc.want)
		}
		// Every advertisement is a git request: the two answers are of
		// one path, so neither can name a request the other does not see.
		if auth.GitAdvertisement(r) && auth.GitAccess(r) == auth.GitNone {
			t.Errorf("%s %s is an advertisement that GitAccess calls no git request",
				tc.method, tc.target)
		}
	}
}

// basic is an Authorization header carrying user:password.
func basic(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

// BASIC IS A CREDENTIAL ON A GIT ROUTE AND NOWHERE ELSE. A git client presents a
// token no other way without configuration; a browser that cached a Basic
// credential replays it to every route of the origin, so accepting it beyond
// git would turn one prompt answered for a clone into a session on the whole
// API.
func TestBasicIsAcceptedOnlyOnGitRoutes(t *testing.T) {
	t.Parallel()
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))

	for _, tc := range []struct{ method, path, header string }{
		{http.MethodGet, "/git/r.git/info/refs?service=git-upload-pack", basic("crewlet", "secret")},
		{http.MethodPost, "/git/r.git/git-receive-pack", basic("crewlet", "secret")},
		// The user name is ignored: the password is the token.
		{http.MethodGet, "/git/r.git/info/refs?service=git-receive-pack", basic("anybody", "secret")},
		{http.MethodPost, "/git/r.git/git-upload-pack", basic("", "secret")},
		// The bearer header still works on a git route.
		{http.MethodPost, "/git/r.git/git-receive-pack", "Bearer secret"},
	} {
		res, seen := serve(t, g, tc.method, tc.path, tc.header)
		if res.StatusCode != http.StatusOK || seen != "founder" {
			t.Errorf("%s %s with %q = %d as %q, want 200 as founder",
				tc.method, tc.path, tc.header, res.StatusCode, seen)
		}
	}

	// Everywhere else a Basic credential is no credential: refused where a
	// token is required, in the API's own JSON …
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/config"},
		{http.MethodPost, "/backup"},
		{http.MethodGet, "/code/repos"},
		{http.MethodPost, "/agents"},
	} {
		res, seen := serve(t, g, tc.method, tc.path, basic("crewlet", "secret"))
		if res.StatusCode != http.StatusUnauthorized || seen != "" {
			t.Errorf("%s %s with Basic = %d as %q, want 401 as nobody",
				tc.method, tc.path, res.StatusCode, seen)
		}
		if ct := res.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s answered %q, want the API's JSON refusal", tc.method, tc.path, ct)
		}
		if ch := res.Header.Get("WWW-Authenticate"); ch != "" {
			t.Errorf("%s %s challenged %q: a browser that answered it would replay "+
				"the credential to every route", tc.method, tc.path, ch)
		}
	}
	// … and attributing nobody where a token is not required.
	if res, seen := serve(t, g, http.MethodGet, "/events", basic("crewlet", "secret")); res.StatusCode != http.StatusOK || seen != "" {
		t.Errorf("GET /events with Basic = %d as %q, want 200 as nobody", res.StatusCode, seen)
	}
}

// THE TWO GIT PREFIXES ARE ALWAYS GUARDED, reads included and whatever
// allow_anonymous_read opens: a repository is the company's source, and the
// code reads name every repository and the people who pushed to it.
func TestTheGitPrefixesAreGuardedEvenForReads(t *testing.T) {
	t.Parallel()
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))
	for _, path := range []string{
		"/git/r.git/info/refs", "/git/r.git/git-upload-pack", "/code/repos", "/code/repos/r/pushes",
	} {
		if !g.Requires(path, http.MethodGet) {
			t.Errorf("GET %s served without a token under anonymous read", path)
		}
	}
	// A slash-ended prefix covers no sibling.
	for _, path := range []string{"/github/hook", "/codes", "/gitx"} {
		if auth.AlwaysGuarded(path) {
			t.Errorf("%s is always guarded as if it were under a git prefix", path)
		}
	}
}

// logLine is one record the JSON handler wrote.
type logLine struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
	Route string `json:"route"`
}

// captureLogs points the process's logging at a buffer, at debug, for one
// test, and puts the default back after. NOT PARALLEL: the logger is the
// process's, and a parallel test would write its own lines into this buffer.
// Every parallel test of this package is held until the sequential ones have
// run, so nothing else logs while it is installed.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	logging.Configure(slog.LevelDebug, logging.FormatJSON, &buf)
	// The package's own boot default (logging's init).
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	return &buf
}

// lines decodes every record written to buf.
func lines(t *testing.T, buf *bytes.Buffer) []logLine {
	t.Helper()
	var out []logLine
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var l logLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("a log line is not JSON: %v (%q)", err, sc.Text())
		}
		out = append(out, l)
	}
	return out
}

// A GIT CHALLENGE IS NOT A WARNING. The protocol's first request carries no
// credential by design — git asks its credential helper only after a 401 with a
// Basic challenge — so a warning there would be written by every clone, and
// the warning a wrong credential deserves would drown in them.
func TestAGitChallengeIsNotAWarning(t *testing.T) {
	// Not parallel: it installs the process's logger (captureLogs).
	buf := captureLogs(t)
	g := guard(t, withTokens(config.APIToken{ID: "founder", Token: "secret"}))

	const challengePath = "/git/challenged.git/info/refs"
	res, _ := serve(t, g, http.MethodGet, challengePath+"?service=git-upload-pack", "")
	assertGitChallenge(t, res, "an unauthenticated git GET")
	for _, l := range lines(t, buf) {
		if l.Route == challengePath && l.Msg != "api_auth_challenge" {
			t.Errorf("an unauthenticated git GET logged %s at %s, want only api_auth_challenge", l.Msg, l.Level)
		}
	}
	if !logged(t, buf, "api_auth_challenge", "DEBUG", challengePath) {
		t.Errorf("an unauthenticated git GET logged no api_auth_challenge at DEBUG: %s", buf.String())
	}

	const wrongPath = "/git/wrong.git/git-receive-pack"
	buf.Reset()
	res, _ = serve(t, g, http.MethodPost, wrongPath, basic("crewlet", "wrong"))
	assertGitChallenge(t, res, "a wrong Basic password")
	if !logged(t, buf, "api_auth_failed", "WARN", wrongPath) {
		t.Errorf("a wrong Basic password logged no api_auth_failed at WARN: %s", buf.String())
	}

	// A non-git route's 401 is the API's own, with no challenge to answer.
	res, _ = serve(t, g, http.MethodPost, "/agents", "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST /agents = %d, want 401", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("POST /agents refused as %q, want JSON", ct)
	}
	if ch := res.Header.Get("WWW-Authenticate"); ch != "" {
		t.Errorf("POST /agents challenged %q", ch)
	}
}

// logged reports whether buf holds msg at level for route.
func logged(t *testing.T, buf *bytes.Buffer, msg, level, route string) bool {
	t.Helper()
	for _, l := range lines(t, buf) {
		if l.Msg == msg && l.Level == level && l.Route == route {
			return true
		}
	}
	return false
}

// assertGitChallenge holds a response to git's form of a 401: the challenge git
// answers with its credential helper, and a body git prints.
func assertGitChallenge(t *testing.T, res *http.Response, what string) {
	t.Helper()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("%s = %d, want 401", what, res.StatusCode)
	}
	if got := res.Header.Get("WWW-Authenticate"); got != `Basic realm="crewlet", charset="UTF-8"` {
		t.Errorf("%s challenged %q, want git's Basic challenge", what, got)
	}
	if got := res.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("%s answered %q: git prints a refusal's body only when it is text/plain", what, got)
	}
	var body bytes.Buffer
	if _, err := body.ReadFrom(res.Body); err != nil {
		t.Fatal(err)
	}
	if got := body.String(); got != "authentication required: use a Crewlet API token as the password (crewlet code credential)\n" {
		t.Errorf("%s said %q", what, got)
	}
}

// GIT'S FORM, AND ONLY GIT'S FORM: text/plain, so remote-curl prints it, never
// sniffed into anything else, never cached in front of the node, and the
// sentence ending in a newline.
func TestGitTextIsTheFormGitPrints(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	auth.GitText(rec, http.StatusServiceUnavailable, "this node is draining")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d", rec.Code)
	}
	for header, want := range map[string]string{
		"Content-Type":           "text/plain; charset=utf-8",
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "no-cache",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if got := rec.Body.String(); got != "this node is draining\n" {
		t.Errorf("body = %q", got)
	}
	// A sentence already ending in its newline gets no second one.
	rec = httptest.NewRecorder()
	auth.GitText(rec, http.StatusNotFound, "no such repository\n")
	if got := rec.Body.String(); !strings.HasSuffix(got, "repository\n") || strings.HasSuffix(got, "\n\n") {
		t.Errorf("body = %q", got)
	}
}

// A SENTENCE IS ONE LINE: git prints each line of a refusal's body as a
// `remote:` line of its own, so a sentence spanning two reaches a person as a
// refusal broken in two. Every caller passes a sentence of its own making, so
// one carrying a newline inside it is the caller's bug, and a test build —
// where every caller's sentence is exercised — panics on it rather than
// write it; a production build folds it onto one line rather than fail a
// refusal over its punctuation.
func TestGitTextRefusesASentenceOfTwoLines(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("a sentence of two lines was written rather than refused")
		}
	}()
	auth.GitText(httptest.NewRecorder(), http.StatusServiceUnavailable, "this node is draining\nretry in 30 s\n")
}
