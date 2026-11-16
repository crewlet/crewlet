package httpjson_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// AN IMMUTABLE ANSWER'S TAG IS ITS OWN BYTES' DIGEST, never a name for the
// request: one answer is one tag on every node, and the same request answered
// with another field — by a newer build, say — is a new tag, so a `304` never
// confirms a body the caller was not sent. A `304` carries no body and no
// content type, only what names the representation the caller holds.
//
// Mutation: tag the answer by anything but its encoded bytes (a constant, or
// the request's path) → two answers to one path share a tag, red.
func TestAnImmutableAnswersTagIsItsOwnBytesDigest(t *testing.T) {
	t.Parallel()
	tagOf := func(body any) string {
		t.Helper()
		rec := httptest.NewRecorder()
		httpjson.Immutable(rec, httptest.NewRequest(http.MethodGet, "/gitlet/tree", nil), body)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("an immutable answer was %d %q", rec.Code, rec.Header().Get("Content-Type"))
		}
		sum := sha256.Sum256(rec.Body.Bytes())
		if want := `"` + hex.EncodeToString(sum[:]) + `"`; rec.Header().Get("ETag") != want {
			t.Errorf("ETag = %q, want the digest of the bytes sent, %q", rec.Header().Get("ETag"), want)
		}
		return rec.Header().Get("ETag")
	}
	tag := tagOf(map[string]string{"oid": "4b825dc642cb6eb9a060e54bf8d69288fbee4904"})
	if again := tagOf(map[string]string{"oid": "4b825dc642cb6eb9a060e54bf8d69288fbee4904"}); again != tag {
		t.Errorf("one answer was tagged %q and %q", tag, again)
	}
	if other := tagOf(map[string]string{"oid": "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
		"mode": "040000"}); other == tag {
		t.Errorf("two answers to one path share the tag %q", tag)
	}

	r := httptest.NewRequest(http.MethodGet, "/gitlet/tree", nil)
	r.Header.Set("If-None-Match", tag)
	rec := httptest.NewRecorder()
	httpjson.Immutable(rec, r, map[string]string{"oid": "4b825dc642cb6eb9a060e54bf8d69288fbee4904"})
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" ||
		rec.Header().Get("ETag") != tag {
		t.Errorf("a request holding the tag was answered %d, %d bytes, Content-Type %q, ETag %q; want 304 "+
			"with no body and no content type, naming the tag", rec.Code, rec.Body.Len(),
			rec.Header().Get("Content-Type"), rec.Header().Get("ETag"))
	}
}

// IF-NONE-MATCH IS PARSED, never split: a weak tag names its strong twin, `*`
// names any, a comma inside a tag is the tag's, and a header that is no list
// of tags names nothing.
//
// Mutation: split the header on commas, and the tag holding one never matches.
func TestNoneMatchReadsEntityTags(t *testing.T) {
	t.Parallel()
	const tag = `"a,b"`
	for header, want := range map[string]bool{
		`"a,b"`:          true,
		`W/"a,b"`:        true,
		`*`:              true,
		` "x", W/"a,b"`:  true,
		`"x"`:            false,
		`"a"`:            false,
		`a,b`:            false,
		`"a,b`:           false,
		``:               false,
		`"x", garbage`:   false,
		`garbage, "a,b"`: false,
	} {
		if got := httpjson.NoneMatch(header, tag); got != want {
			t.Errorf("NoneMatch(%q, %s) = %v, want %v", header, tag, got, want)
		}
	}
}
