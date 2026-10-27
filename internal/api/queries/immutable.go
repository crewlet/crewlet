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
// own cache — not even the request.
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
