package httpjson

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// ImmutableCacheControl is how an answer that never changes is marked: kept
// by the caller's own cache alone (`private`), for a year, and never asked
// again while it is kept (`immutable`, RFC 8246) — not even on a reload.
//
// PRIVATE, because the answer was decided for the credential that asked it,
// and a shared cache in front of the node would hand it to whoever asked the
// same path next. A YEAR, because the answer itself never expires — it is a
// function of object ids, which name the same bytes for ever — so the bound is
// the longest a cache is asked to keep anything (RFC 9111's caches may hold
// less; RFC 8246's own example asks the same year), not a property of the
// answer. And `immutable`, because without it a browser revalidates a fresh
// entry on every reload, a round trip per page of a diff for nothing.
const ImmutableCacheControl = "private, max-age=31536000, immutable"

// Immutable answers 200 with body as JSON, marked as never changing: a STRONG
// entity tag of its bytes, [ImmutableCacheControl], and a cache key that holds
// the credential (`Vary: Authorization, Cookie`) — or, where the request's
// `If-None-Match` already names that tag, `304` with no body and the same
// three headers (RFC 9110 §15.4.5: a 304 carries what the 200 would have).
//
// THE TAG IS THE BYTES' OWN DIGEST, never a name for the request: it is right
// whatever the answer was a function of, including the build that encoded it,
// so a newer build answering the same request with another field is a new tag
// rather than a 304 confirming a body it never sent. What it saves is the
// transfer; the answer is still read to compute it.
//
// THE CACHE KEY HOLDS THE CREDENTIAL because `private` keeps the answer out of
// shared caches and not out of a second person's session in the same
// browser: the guard resolves the caller from `Authorization` first and the
// session cookie after it, so a browser's cache reuses an entry only for the
// same pair.
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
	// ADDED, never set: a route's own Vary — CORS's Origin — stays beside it.
	h.Add("Vary", "Authorization")
	h.Add("Vary", "Cookie")
	if noneMatch(r.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeEncoded(w, http.StatusOK, raw)
}

// noneMatch reports whether an `If-None-Match` header names tag, by the WEAK
// comparison RFC 9110 §13.1.2 holds If-None-Match to — `W/"x"` names `"x"` —
// and `*`, which names any current representation: the one an answer being
// sent is.
//
// PARSED, NEVER SPLIT ON COMMAS: an entity tag's characters include the comma
// (RFC 9110 §8.8.3, `etagc`), so a split would read one tag as two and answer
// a full body where the caller already holds it — or, worse, match half of a
// tag that is not this one. A header that is not a list of entity tags names
// nothing, and the caller is sent the body.
func noneMatch(header, tag string) bool {
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
