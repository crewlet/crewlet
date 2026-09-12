package coord

import "strings"

// THE KEY GRAMMAR: how a coordination key is built out of segments, and why
// every one of them is escaped.
//
// It is what survived the document families. Those families were a KV estate
// with a change feed and a per-node projection in front of it, and both native
// backends left them for ordered logs — but every key class the fleet still
// holds is built here: a node's positions, a trim floor, a hold, a backup
// point, a capacity operation. Each one composes an id, a stream name or a
// node id into a key, and each of those can carry a byte a subject token
// cannot.
//
// A key IS a subject token path under its bucket, which is what makes the
// escaping load-bearing rather than tidy: a raw segment containing a dot
// splits into two tokens and changes what a filtered watch matches, and a
// segment containing a space, a colon or a non-ASCII letter is refused by the
// store outright.

// KeySeparator is what joins a key's segments.
//
// A dot, because a key becomes a subject token path under the bucket, and
// that is what makes a CLASS filterable: a consumer subscribed to the change
// class of a family selects every change key and nothing else. Every other
// byte in a segment is escaped, so a segment can never contain one of these
// and a key's shape is exactly the number of segments it was built from.
const KeySeparator = "."

// DocumentKey builds a key from its segments, escaping each.
//
// SEGMENT BY SEGMENT, and this is load-bearing rather than tidy. A key is a
// subject token path, so a raw segment containing a dot would split into two
// tokens and change what a filtered watch matches; a segment containing a
// space, a colon or a non-ASCII letter is refused by the store outright. Page
// titles and seat handles contain all four. Escaping each segment and joining
// with the separator is what lets a title be part of a key at all, and what
// keeps `class.a.b` distinct from a single segment that happened to spell it.
//
// AN EMPTY SEGMENT IS NOT A KEY. Every segment names something — a class, an
// id, a project, a title — so an empty one is a caller that lost a value on
// the way here, and the key it would build is one [DocumentSegments] refuses.
// That refusal is the design: a key nobody can decode is caught at the first
// listing that reads it back, where a key that silently decoded to an empty
// name would put a document belonging to nothing into a projection.
func DocumentKey(segments ...string) string {
	escaped := make([]string, len(segments))
	for i, seg := range segments {
		escaped[i] = escapeSegment(seg)
	}
	return strings.Join(escaped, KeySeparator)
}

// DocumentSegments recovers the segments of a key, reporting false for one
// this grammar did not write.
//
// A malformed key is skipped rather than guessed at, on the lease table's
// rule: a listing that invented a segment would put a document nobody wrote
// into a projection.
func DocumentSegments(key string) ([]string, bool) {
	if key == "" {
		return nil, false
	}
	parts := strings.Split(key, KeySeparator)
	out := make([]string, len(parts))
	for i, part := range parts {
		seg, ok := unescapeSegment(part)
		if !ok {
			return nil, false
		}
		out[i] = seg
	}
	return out, true
}

// KeyClass is the first segment of a key — the record kind.
func KeyClass(key string) (string, bool) {
	segs, ok := DocumentSegments(key)
	if !ok || len(segs) == 0 {
		return "", false
	}
	return segs[0], true
}

const (
	segmentEscape = '='
	segmentHex    = "0123456789ABCDEF"
)

// literalSegmentByte reports whether b may appear in a segment as itself.
//
// The separator is deliberately NOT literal: a segment that could contain one
// would make the grammar ambiguous, which is the whole thing it exists to
// prevent.
func literalSegmentByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_' || b == '-':
		return true
	}
	return false
}

func escapeSegment(seg string) string {
	needs := false
	for i := 0; i < len(seg); i++ {
		if !literalSegmentByte(seg[i]) {
			needs = true
			break
		}
	}
	if !needs {
		return seg
	}
	var b strings.Builder
	b.Grow(len(seg) * 3)
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if literalSegmentByte(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte(segmentEscape)
		b.WriteByte(segmentHex[c>>4])
		b.WriteByte(segmentHex[c&0x0f])
	}
	return b.String()
}

func unescapeSegment(seg string) (string, bool) {
	if seg == "" {
		return "", false
	}
	if !strings.ContainsRune(seg, segmentEscape) {
		for i := 0; i < len(seg); i++ {
			if !literalSegmentByte(seg[i]) {
				return "", false
			}
		}
		return seg, true
	}
	var b strings.Builder
	b.Grow(len(seg))
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if c != segmentEscape {
			if !literalSegmentByte(c) {
				return "", false
			}
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(seg) {
			return "", false
		}
		hi, ok := unhexSegment(seg[i+1])
		if !ok {
			return "", false
		}
		lo, ok := unhexSegment(seg[i+2])
		if !ok {
			return "", false
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String(), true
}

func unhexSegment(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	// Upper case only, so two keys can never decode to one segment set.
	return 0, false
}
