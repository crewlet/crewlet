package queries

import "encoding/json"

// Immutable is an answer that is a function of the OBJECT IDS its question
// named and of nothing that moves: a tree, a file's lines or a diff read by
// commit id names the same bytes on every node at every position, so the
// answer to one such request never changes. An answer returns it in place of
// the value it wraps exactly when it was asked by id — the same question asked
// by a branch is a function of where the branch points now, and returns the
// value bare — so whether an answer may be cached is decided per request, by
// the only code that knows what the request named.
//
// A REST route sends it with a strong entity tag and as cacheable for as long
// as a cache keeps anything, and answers a request that names that tag `304`
// with no body (httpjson.Immutable): a screen paging back through a diff
// costs the node the read and none of the transfer, or — from the browser's
// own cache — not even the request. A browser keeps it for as long as it stays
// signed in as the person who asked: the cache key holds no cookie, so the
// session cookie's re-issue every five minutes moves nothing, and every
// sign-in and sign-out empties the browser's cache (session.Begin,
// session.End), so the next person in that browser asks for themselves.
//
// SO AN ANSWER IS IMMUTABLE ONLY WHERE IT IS THE SAME FOR EVERY CALLER WHO MAY
// ASK IT — as a repository's content is: every person reads every repository
// (ADR-0058). The cache key does not say who asked, and the emptying is the
// browser's to do: it honours `Clear-Site-Data` only in a secure context
// (https, or a loopback address) and only from a release that implements the
// directive. Where it does not, a kept answer outlives the session that asked
// for it, and the next person to sign in there is served it from the cache —
// harmless exactly when it is what they would be answered themselves. An
// answer that differs by who asks therefore names the asker in what is asked —
// a parameter the question holds equal to the caller the guard resolved — so
// the request, and with it the cache key, differs too.
//
// A TRANSPORT THAT CACHES NOTHING SENDS THE ANSWER ITSELF: Immutable encodes
// as the value it wraps ([Immutable.MarshalJSON]), so the socket's frame and a
// dataset's rows carry what was answered, never the wrapper, without either
// knowing the type exists.
type Immutable struct {
	// Answer is what was answered.
	Answer any
}

// MarshalJSON is the answer itself.
func (i Immutable) MarshalJSON() ([]byte, error) { return json.Marshal(i.Answer) }
