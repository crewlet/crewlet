package queries

import "encoding/json"

// Immutable is an answer whose BYTES are a function of what its request named
// (whole object ids, the question's arguments) and the build that encodes
// them, and of nothing a node, a moment or a caller holds: a tree, a file's
// lines or a diff read by whole commit ids. A question asked by a branch or an
// abbreviated id returns the value bare. REST sends it with a strong entity
// tag and answers `304` to it (httpjson.Immutable), but ONLY WHERE THE ANSWER
// IS THE SAME FOR EVERY CALLER WHO MAY ASK IT (ADR-0088, ADR-0110) AND TO A
// REQUEST THAT NAMES ITS READER: one naming nobody would be kept under one key
// for everybody using that browser, so REST answers it the bare value (internal/
// api's writeAnswer); a sign-out empties what a named reader kept (session.SignOut).
// The key names the bearer or dashboard reader, never the session cookie.
type Immutable struct {
	// Answer is what the REQUEST NAMED, NEVER WHAT THE READ ADDED. A Gitlet
	// node's answer to a read by object id (gitlet.GitAnswer) carries stamps
	// (resolved repository, the copy's position, recalled or cut, the push the
	// index says introduced each commit) and a page EXTENT (gitlet/query's
	// room) that move between reads of one request. Wrapped, they make every
	// read a new entity tag, and a browser keeps for a year a field once said:
	// a pusher left out because the index had not caught up, read ever after as
	// nobody. A question answering Immutable carries a test asking twice, by a
	// read that cuts its page and one that recalls it, and getting equal bytes.
	Answer any
}

// MarshalJSON is the answer itself, so a transport that caches nothing carries
// the value and never the wrapper.
func (i Immutable) MarshalJSON() ([]byte, error) { return json.Marshal(i.Answer) }
