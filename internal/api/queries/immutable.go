package queries

import "encoding/json"

// Immutable is an answer whose BYTES are a function of what its request named
// — whole object ids, and the question's arguments — and of the build that
// encodes them, and of nothing a node, a moment or a caller holds: a tree, a
// file's lines or a diff read by whole commit ids can be one, since git names
// the same objects on every node at every position. An answer returns it in
// place of the value it wraps exactly when it was asked so — the same question
// asked by a branch is a function of where the branch points now, and one
// asked by an abbreviated id of whichever object the abbreviation finds now,
// which an object arriving can change; both return the value bare — so
// whether an answer may be cached is decided per request, by the only code
// that knows what the request named.
//
// # Nothing the answering read stamps
//
// IT WRAPS WHAT THE REQUEST NAMED, NEVER WHAT THE READ THAT ANSWERED IT ADDED.
// A git node's answer to a read by object id (code.GitAnswer) is not one as it
// stands. The read stamps it with the repository the name resolved to, the
// answering copy's position, whether the page was recalled from the memo or
// cut, and the push the node's index says introduced each commit with what
// that index covers so far — and each of those moves between two reads of one
// request: from one node to the next, from the read that cut a page to the one
// that recalled it, and as the index fills. The page's EXTENT follows them
// too: a git node cuts each page against the room its stamps leave
// (code/query's room), so the same page cut where the index holds more of its
// commits holds fewer items. Wrapped, any of them makes every read a new
// entity tag, so the `304` that saves the transfer never comes, and a browser
// keeps for a year whatever the field said once — a commit's pusher left out
// because the index had not reached it yet, read ever after as nobody. No type
// can see what a field means, so a question that answers Immutable carries the
// test that holds it to this: asked twice — by a read that cuts its page and
// one that recalls it, on copies at different positions — it answers the same
// bytes.
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
