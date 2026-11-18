package kv

import (
	"strings"

	"github.com/crewlet/crewlet/internal/coord"
)

// How a name becomes a NATS KV key, and why there are two shapes of it.
//
// A key is a subject token path under $KV.<bucket> ([-/_=.A-Za-z0-9]+), and
// names carry bytes outside that set (a colon in a resource, a pipe in a
// ledger key) and dots that would split one name into two tokens, so every
// name goes through [coord.DocumentKey] and [coord.DocumentSegments], the ONE
// grammar the fleet buckets share. [encodeKey] is for a bucket whose keys are
// one opaque name: the bucket IS the class. [encodeResource] is for the lease
// and epoch buckets: a resource's class becomes a token, so the broker matches
// a whole class (the nodes, the seat hints) without reading every key. Both
// are INJECTIVE: two resources on one key would share one lease, which is two
// nodes holding one seat.

// encodeKey maps one opaque name onto a key.
func encodeKey(name string) string { return coord.DocumentKey(name) }

// decodeKey recovers an opaque name, reporting false for a key this backend
// did not write — including a MULTI-SEGMENT one, which is a resource key and
// belongs to another bucket.
//
// A malformed key is skipped rather than guessed at: a listing that invented
// a name would hand its caller a record nobody wrote under that name.
func decodeKey(key string) (string, bool) {
	segments, ok := coord.DocumentSegments(key)
	if !ok || len(segments) != 1 {
		return "", false
	}
	return segments[0], true
}

// encodeResource maps a resource name onto a key, one segment per part.
//
// THE CLASS BECOMES A SUBJECT TOKEN, which is the whole point — see the file
// doc. The parts are [coord.ResourceSegments]', so `seat:alice` is the two
// segments `seat` and `alice` and lands on the key `seat.alice`, which
// `seat.>` matches and `node.>` does not.
func encodeResource(resource string) string {
	return coord.DocumentKey(coord.ResourceSegments(resource)...)
}

// decodeResource recovers a resource name, reporting false for a key this
// backend did not write.
func decodeResource(key string) (string, bool) {
	segments, ok := coord.DocumentSegments(key)
	if !ok {
		return "", false
	}
	return strings.Join(segments, coord.ResourceSeparator), true
}
