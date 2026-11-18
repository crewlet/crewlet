package httpjson

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// ImmutableCacheControl is how an answer that never changes is marked: kept
// by the caller's own cache alone (`private`), for a year at most, and never
// asked again while it is kept (`immutable`, RFC 8246), not even on a
// reload. The cache key names who it was answered for ([Immutable]).
//
// PRIVATE, because the answer was decided for the credential that asked it,
// and a shared cache would hand it to whoever asked the same path next. A
// YEAR, because such an answer never expires (its bytes are a function of what
// its request named, the contract internal/api/queries' Immutable holds a
// question to): the longest a cache is asked to keep anything, not a property
// of the answer. `immutable`, because without it a browser revalidates a fresh
// entry on every reload, a round trip per page of a diff for nothing.
const ImmutableCacheControl = "private, max-age=31536000, immutable"

// Immutable answers 200 with body as JSON, marked as never changing: a STRONG
// entity tag of its bytes, [ImmutableCacheControl], and a cache key that holds
// who the answer is for (`Vary: Authorization, X-Crewlet-Reader`). Where the
// request's `If-None-Match` already names that tag, it answers `304` with no
// body and the same three headers (RFC 9110 §15.4.5: a 304 carries what the
// 200 would have).
//
// THE TAG IS THE BYTES' OWN DIGEST, never a name for the request: a newer
// build answering the same request with another field is a new tag, not a 304
// confirming a body it never sent. It saves the transfer only, and only while
// the bytes hold still: an answer carrying anything its read stamps is a new
// tag on every read, so queries' Immutable wraps nothing that moves.
func Immutable(w http.ResponseWriter, r *http.Request, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		encodeFailed(w, err)
		return
	}
	sum := sha256.Sum256(raw)
	tag := `"` + hex.EncodeToString(sum[:]) + `"`
	h := w.Header()
	h.Set("ETag", tag)
	h.Set("Cache-Control", ImmutableCacheControl)
	// ADDED, never set: a route's own Vary (CORS's Origin) stays beside it.
	//
	// The key names who the answer is for, never the session cookie:
	// `private` does not separate people sharing a browser, so it holds the
	// bearer and the dashboard's reader ([ReaderHeader]), neither of which
	// moves while they stay signed in. The cookie is re-issued every five
	// minutes (session.ReissueAfter), so a key holding it would stop matching;
	// `Clear-Site-Data` made Chrome scan its whole cache. A request naming
	// nobody would share one key, so internal/api's writeAnswer answers it
	// `no-store` (ADR-0110); a sign-out still empties the cache.
	h.Add("Vary", "Authorization, "+ReaderHeader)
	if NoneMatch(r.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeEncoded(w, http.StatusOK, raw)
}

// NoneMatch reports whether an `If-None-Match` header names tag, by the WEAK
// comparison RFC 9110 §13.1.2 holds If-None-Match to — `W/"x"` names `"x"` —
// and `*`, which names any current representation: the one an answer being
// sent is.
//
// PARSED, NEVER SPLIT ON COMMAS: an entity tag's characters include the comma
// (RFC 9110 §8.8.3, `etagc`), so a split would read one tag as two and answer
// a full body where the caller already holds it — or, worse, match half of a
// tag that is not this one. A header that is not a list of entity tags names
// nothing, and the caller is sent the body.
func NoneMatch(header, tag string) bool {
	header = strings.TrimSpace(header)
	if header == "*" {
		return true
	}
	for header != "" {
		header = strings.TrimLeft(header, " \t,")
		if header == "" {
			break
		}
		header = strings.TrimPrefix(header, "W/")
		if !strings.HasPrefix(header, `"`) {
			return false
		}
		end := strings.IndexByte(header[1:], '"')
		if end < 0 {
			return false
		}
		if header[:end+2] == tag {
			return true
		}
		header = header[end+2:]
	}
	return false
}
