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
// asked again while it is kept (`immutable`, RFC 8246) — not even on a
// reload. A browser keeps it for as long as it stays signed in as the person
// who asked: every sign-in and sign-out empties its cache ([Immutable] says
// why), and a re-issue of the session's cookie does not.
//
// PRIVATE, because the answer was decided for the credential that asked it,
// and a shared cache in front of the node would hand it to whoever asked the
// same path next. A YEAR, because such an answer never expires — its bytes
// are a function of what its request named, the contract internal/api/queries'
// Immutable holds a question to — so the bound is the longest a cache is asked
// to keep anything (RFC 9111's caches may hold less; RFC 8246's own example
// asks the same year), not a property of the answer. And `immutable`, because
// without it a browser revalidates a fresh entry on every reload, a round trip
// per page of a diff for nothing.
const ImmutableCacheControl = "private, max-age=31536000, immutable"

// Immutable answers 200 with body as JSON, marked as never changing: a STRONG
// entity tag of its bytes, [ImmutableCacheControl], and a cache key that holds
// the bearer a request presents (`Vary: Authorization`) — or, where the
// request's `If-None-Match` already names that tag, `304` with no body and the
// same three headers (RFC 9110 §15.4.5: a 304 carries what the 200 would have).
//
// THE TAG IS THE BYTES' OWN DIGEST, never a name for the request: it is right
// whatever the answer was a function of, including the build that encoded it,
// so a newer build answering the same request with another field is a new tag
// rather than a 304 confirming a body it never sent. What it saves is the
// transfer; the answer is still read to compute it. And it saves that only
// while the bytes hold still: an answer carrying anything the read that
// answered it stamps — the answering copy's position, whether a memo answered
// — is a new tag on every read, which is one reason queries' Immutable wraps
// nothing that moves.
//
// THE CACHE KEY HOLDS THE BEARER A CLIENT SETS, AND NEVER THE SESSION COOKIE.
// `private` keeps the answer out of shared caches and not out of the next
// credential in the same client, so the key keeps apart the credentials a
// client sets itself: an `Authorization` header is put there by its client,
// and an answer kept for one token is never served for another. The session
// cookie cannot be in it: a served session's cookie is re-issued every five
// minutes of use (session.ReissueAfter), so a key holding its bytes stopped
// matching minutes after an answer was kept, and every page a person went back
// to was read again on its repository's home git node — the read the cache is
// for. Who a browser is signed in as is kept apart from the other side
// instead: every response that may change it — a sign-in, a sign-out, a
// session the guard refused — empties the browser's cache (session.Begin and
// session.End's `Clear-Site-Data`), so the next person in a browser is never
// served what the last one was answered.
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
