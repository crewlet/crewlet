package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/iam"
)

// AN ANSWER NAMED BY OBJECT IDS IS SENT AS ONE THAT NEVER CHANGES. A question
// answering [queries.Immutable] — a tree, a file's lines or a diff read by
// commit id, the same bytes on every node at every position — is sent over
// REST with a strong entity tag of the bytes it answered, kept by the caller's
// own cache and never revalidated while kept (`immutable`), under a cache key
// holding the credential; a request that already holds the tag is answered
// `304` with no body and the same headers, by the weak comparison
// `If-None-Match` takes, in a list or as `*`. The socket, which caches
// nothing, carries the answer itself and never the wrapper. And an answer that
// is not wrapped carries none of it, since the same question asked by a branch
// moves when the branch does.
//
// Mutation: drop httpjson.Immutable's If-None-Match arm → every repeat is a
// full 200, red; drop Immutable.MarshalJSON → the socket's frame carries
// {"Answer": …}, red.
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
		vary := strings.Join(rec.Header().Values("Vary"), ",")
		for _, key := range []string{"Authorization", "Cookie"} {
			if !strings.Contains(vary, key) {
				t.Errorf("Vary = %q, which leaves %s out of the cache key: one person's answer "+
					"would be another's in the same browser", vary, key)
			}
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
