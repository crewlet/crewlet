package kv

import (
	"strings"

	"github.com/crewlet/crewlet/internal/coord"
)

// How a name becomes a NATS KV key, and why there are two shapes of it.
//
// A key must match [-/_=.A-Za-z0-9]+ because it becomes a subject token path
// under $KV.<bucket>, and the names this engine keys on carry bytes that are
// not in that set — a colon in a resource, a pipe in a ledger key, a space or
// a non-ASCII letter in a page title. So a name is escaped on the way in and
// unescaped on the way out, by [coord.DocumentKey] and [coord.DocumentSegments]
// — the ONE grammar, shared with the fleet buckets, rather than a second copy
// of the same rule here. It was a second copy once, byte-identical to the
// first, which is the arrangement that ends with the two disagreeing.
//
// The two shapes differ only in how many segments the name has:
//
//   - [encodeKey] is for a bucket whose keys are ONE opaque name — a budget
//     scope, a node id, a secret name, a turn id. Nothing filters inside such
//     a bucket, because the bucket IS the class.
//
//   - [encodeResource] is for the LEASE and EPOCH buckets, whose keys name
//     resources of several classes at once. A resource's segments become the
//     key's segments, so its class is a subject token of its own and a whole
//     class is a wildcard the broker can match. That is what lets the
//     membership read ask for the nodes rather than reading every lease in the
//     fleet, and the sweep ask for the seat hints rather than reading every
//     epoch the deployment has ever minted.
//
// Both mappings are INJECTIVE in both directions, which is the property that
// matters rather than the encoding: two resources that collided on one key
// would share one lease, and that is two nodes holding one seat.

// encodeKey maps one opaque name onto a key.
func encodeKey(name string) string { return coord.DocumentKey(name) }

// decodeKey recovers an opaque name, reporting false for a key this backend
// did not write — including a MULTI-SEGMENT one, which is a resource key and
// belongs to another bucket.
//
// A malformed key is skipped rather than guessed at: a listing that invented
// a name would put a record nobody wrote into a projection.
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
