package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// AN ANSWER NAMED BY OBJECT IDS IS SENT AS ONE THAT NEVER CHANGES. A question
// answering [queries.Immutable] — one whose bytes are a function of what its
// request named, as a tree, a file's lines or a diff read by whole commit ids
// can be — is sent over REST with a strong entity tag of the bytes it
// answered, kept by the caller's own cache and never revalidated while kept
// (`immutable`), under a cache key holding who the answer is for (the bearer
// in `Authorization` and the dashboard's reader) and never the session cookie,
// whose bytes every re-issue moves (see
// [TestAKeptOidAnswerIsServedPastItsCookiesReissue]); a request
// that already holds the tag is answered `304` with no body and the same
// headers, by the weak comparison `If-None-Match` takes, in a list or as `*`.
// The socket, which caches nothing, carries the answer itself and never the
// wrapper. And an answer that is not wrapped carries none of it, since the
// same question asked by a branch moves when the branch does.
//
// Mutation: drop httpjson.Immutable's If-None-Match arm → every repeat is a
// full 200, red; drop Immutable.MarshalJSON → the socket's frame carries
// {"Answer": …}, red; add Cookie to the Vary → red; drop the reader from the
// Vary → red.
func TestAnOidAnswerIsImmutable(t *testing.T) {
	t.Parallel()
	tree := map[string]any{"oid": "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "entries": []any{}}
	a := newApp(t, api.Options{})
	a.Queries().Register("test_tree_by_oid", queries.Everybody(),
		func(context.Context, queries.Params) (any, error) { return queries.Immutable{Answer: tree}, nil })
	a.Queries().Register("test_tree_by_branch", queries.Everybody(),
		func(context.Context, queries.Params) (any, error) { return tree, nil })
	want, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	get := func(what, noneMatch string) *httptest.ResponseRecorder {
		t.Helper()
		r := authed(httptest.NewRequest(http.MethodGet, "/query/"+what, nil))
		if noneMatch != "" {
			r.Header.Set("If-None-Match", noneMatch)
		}
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, r)
		return rec
	}

	first := get("test_tree_by_oid", "")
	tag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || strings.TrimSpace(first.Body.String()) != string(want) {
		t.Fatalf("an oid answer was %d %q, want 200 with the answer itself, %s", first.Code,
			first.Body.String(), want)
	}
	if !strings.HasPrefix(tag, `"`) || !strings.HasSuffix(tag, `"`) || len(tag) != 66 {
		t.Errorf("an oid answer's ETag is %q, want a strong tag of its bytes' SHA-256", tag)
	}
	headers := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if got := rec.Header().Get("Cache-Control"); got != httpjson.ImmutableCacheControl {
			t.Errorf("Cache-Control = %q, want %q", got, httpjson.ImmutableCacheControl)
		}
		if got := rec.Header().Get("ETag"); got != tag {
			t.Errorf("ETag = %q, want the first answer's %q", got, tag)
		}
		vary := varied(rec.Header())
		if !slices.Contains(vary, "authorization") {
			t.Errorf("Vary = %q, which leaves Authorization out of the cache key: one token's "+
				"answer would be another's in the same client", vary)
		}
		if !slices.Contains(vary, strings.ToLower(httpjson.ReaderHeader)) {
			t.Errorf("Vary = %q, which leaves the dashboard's reader out of the cache key: "+
				"one person's answer would be the next one's in the same browser", vary)
		}
		if slices.Contains(vary, "cookie") {
			t.Errorf("Vary = %q, which keys the answer on the session cookie's bytes: the next "+
				"re-issue misses everything the browser kept", vary)
		}
	}
	headers(t, first)

	for _, held := range []string{tag, "W/" + tag, `"elsewhere", ` + tag, "*"} {
		rec := get("test_tree_by_oid", held)
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Errorf("If-None-Match: %s answered %d with %d bytes, want 304 and none", held, rec.Code,
				rec.Body.Len())
		}
		headers(t, rec)
	}
	for _, other := range []string{`"elsewhere"`, strings.Trim(tag, `"`), `"` + strings.Trim(tag, `"`) + `,x"`} {
		if rec := get("test_tree_by_oid", other); rec.Code != http.StatusOK ||
			strings.TrimSpace(rec.Body.String()) != string(want) {
			t.Errorf("If-None-Match: %s answered %d %q, want the whole answer: it names no tag of it",
				other, rec.Code, rec.Body.String())
		}
	}

	// A BRANCH MOVES, so an answer read by one is never marked immutable.
	moving := get("test_tree_by_branch", "")
	if moving.Code != http.StatusOK || moving.Header().Get("ETag") != "" ||
		moving.Header().Get("Cache-Control") == httpjson.ImmutableCacheControl {
		t.Errorf("an answer read by a branch was %d with ETag %q and Cache-Control %q, want neither",
			moving.Code, moving.Header().Get("ETag"), moving.Header().Get("Cache-Control"))
	}

	// THE SOCKET CARRIES THE ANSWER ITSELF: what a frame's data encodes is
	// what the registry answered, encoded as JSON.
	asker := iam.WithPrincipal(t.Context(), iam.Principal{ID: uuid.New(), Login: "token:fixture",
		Kind: iam.KindMachine, Stage: iam.StageActive})
	answered, err := a.Queries().AnswerWith(asker, "test_tree_by_oid", queries.FromMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	frame, err := json.Marshal(answered)
	if err != nil || string(frame) != string(want) {
		t.Errorf("the socket would carry %s (%v), want the answer itself, %s", frame, err, want)
	}
}

// A KEPT OID ANSWER IS SERVED PAST ITS SESSION COOKIE'S RE-ISSUE.
//
// A signed-in browser presents its session cookie on every request, and the
// guard re-issues it once it is [session.ReissueAfter] old: new bytes, the same
// person. A browser reuses a kept answer for a later request only while every
// header the answer's `Vary` names is the same in both (RFC 9111 §4.1), so an
// answer keyed on `Cookie` stopped matching at the first re-issue, and every
// page a person went back to was asked of the node again — a read on the
// repository's home Gitlet node per page, per person, every five minutes: the
// read a year-long cache exists to save. So the key holds who the answer is
// for, the bearer a client sets (`Authorization`) and the reader a dashboard
// tab names (httpjson.ReaderHeader), and never the cookie; the re-issue that
// fills the cache here empties nothing.
//
// Through the whole app, the way a browser arrives: the request that fills the
// cache is one the guard re-issues on, and the browser's next request for the
// page — presenting the re-issued cookie and the same reader — is answered from
// what it kept.
//
// A REQUEST THAT NAMES NOBODY IS KEPT BY NO CACHE: the cookie alone, with no
// reader and no bearer, is the key every person who signs in on that browser
// shares, and a read of the company's code is not the same for every person
// (ADR-0110), so it is answered the value itself, `no-store`, with no tag.
//
// Mutation: add `Vary: Cookie` back to httpjson.Immutable → the request after
// the re-issue misses the kept answer, red; empty the cache on a re-issue
// (session.Issue sending Clear-Site-Data) → red; drop the reader from the Vary
// → the next person's tab is answered from the last one's, red; mark a request
// naming nobody immutable again → the cookie's own request is kept, red.
func TestAKeptOidAnswerIsServedPastItsCookiesReissue(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	authorize(&b, config.APIToken{ID: "fixture", Token: fixtureToken})
	b.API.ExternalURL = "http://127.0.0.1:8080"
	cookies := newCookieArm(t, &b)
	// A COOKIE AS OLD AS A RE-ISSUE WANTS, so the request that fills the
	// browser's cache is the one the guard re-issues on — as one request
	// every five minutes of a person's use is.
	signedIn := cookies.personIssued(t, []iam.Grant{}, clock, clock.Add(-session.ReissueAfter))
	held := signedIn.cookie
	a := newApp(t, api.Options{Bootstrap: &b, Sessions: cookies.arm})
	tree := map[string]any{"oid": "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "entries": []any{}}
	a.Queries().Register("test_tree_by_oid", queries.Everybody(),
		func(context.Context, queries.Params) (any, error) { return queries.Immutable{Answer: tree}, nil })
	ask := func(cookie *http.Cookie) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/query/test_tree_by_oid", nil)
		r.AddCookie(cookie)
		return r
	}

	filled := ask(held)
	kept := httptest.NewRecorder()
	a.ServeHTTP(kept, filled)
	if kept.Code != http.StatusOK || kept.Header().Get("Cache-Control") != "no-store" ||
		kept.Header().Get("ETag") != "" {
		t.Fatalf("an oid answer to a request naming nobody was %d with Cache-Control %q and ETag %q, want "+
			"200, no-store and no tag: it would be kept for whoever signs in on the browser next",
			kept.Code, kept.Header().Get("Cache-Control"), kept.Header().Get("ETag"))
	}
	var reissued *http.Cookie
	for _, c := range kept.Result().Cookies() {
		if c.Name == held.Name && c.Value != "" && c.Value != held.Value {
			reissued = c
		}
	}
	if reissued == nil {
		t.Fatalf("a cookie %s old was not re-issued (cookies %v): this case holds nothing",
			session.ReissueAfter, kept.Result().Cookies())
	}
	if v := kept.Header().Values("Clear-Site-Data"); len(v) != 0 {
		t.Errorf("the re-issue emptied the browser's cache (Clear-Site-Data %v), dropping every "+
			"answer it kept once every five minutes of use", v)
	}

	// A DASHBOARD TAB NAMES ITS READER, and keeps what it was answered past
	// the re-issue: the reader does not move while the person stays signed
	// in, and the browser's next request for the page — presenting the
	// cookie it was just re-issued — is answered from what it kept.
	read := func(cookie *http.Cookie, reader string) *http.Request {
		r := ask(cookie)
		r.Header.Set(httpjson.ReaderHeader, reader)
		return r
	}
	tabFilled := read(held, signedIn.person)
	tabKept := httptest.NewRecorder()
	a.ServeHTTP(tabKept, tabFilled)
	if tabKept.Code != http.StatusOK || tabKept.Header().Get("Cache-Control") != httpjson.ImmutableCacheControl {
		t.Fatalf("the tab's oid answer was %d with Cache-Control %q, want 200 and %q",
			tabKept.Code, tabKept.Header().Get("Cache-Control"), httpjson.ImmutableCacheControl)
	}
	if !reusable(tabKept.Header(), tabFilled, read(reissued, signedIn.person)) {
		t.Errorf("Vary %q keeps the tab's answer from its request after the re-issue",
			varied(tabKept.Header()))
	}
	// AND A BEARER IS STILL ITS OWN KEY: a request presenting a token is
	// never answered from what the session's tab kept.
	tokened := read(reissued, signedIn.person)
	tokened.Header.Set("Authorization", "Bearer "+fixtureToken)
	if reusable(tabKept.Header(), tabFilled, tokened) {
		t.Errorf("Vary %q lets a request presenting a token be answered from what a session "+
			"kept", varied(tabKept.Header()))
	}
	// AND THE NEXT PERSON IN THE BROWSER IS A KEY OF THEIR OWN: a tab read by
	// somebody else is never answered from what this one kept, so nothing
	// has to empty the browser's cache when who it is signed in as changes.
	if reusable(tabKept.Header(), tabFilled, read(reissued, uuid.NewString())) {
		t.Errorf("Vary %q lets a tab read by somebody else be answered from what this "+
			"person's tab kept", varied(tabKept.Header()))
	}
}

// reusable reports whether a cache holding a response with header — answered
// to stored — may answer next with it, by RFC 9111 §4.1's rule for the
// selecting header fields: every field the response's Vary names holds the
// same values in both requests, and a Vary of `*` matches nothing. The two
// requests name one URL, which is the rest of the cache key.
func reusable(header http.Header, stored, next *http.Request) bool {
	for _, field := range varied(header) {
		if field == "*" || !slices.Equal(stored.Header.Values(field), next.Header.Values(field)) {
			return false
		}
	}
	return true
}

// varied is every field a response's Vary names, lowercased, across every
// Vary line it carries.
func varied(header http.Header) []string {
	var out []string
	for _, line := range header.Values("Vary") {
		for field := range strings.SplitSeq(line, ",") {
			if field = strings.ToLower(strings.TrimSpace(field)); field != "" {
				out = append(out, field)
			}
		}
	}
	return out
}
