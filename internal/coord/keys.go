package coord

import "strings"

// THE KEY GRAMMAR: how a coordination key is built out of escaped segments.
// Every key class the fleet holds is built here (a node's positions, a trim
// floor, a hold, a backup point, a capacity operation), from ids, stream names
// and node ids that can carry a byte a subject token cannot ([DocumentKey]).

// KeySeparator is what joins a key's segments.
//
// A dot, because a key is a subject token path under its bucket, which makes
// a CLASS filterable: a listing of one class asks the broker for that class's
// keys ([DocumentFilter]) and reads nothing else. Every other byte is escaped,
// so no segment contains one and a key has exactly the segments it was built
// from.
const KeySeparator = "."

// DocumentKey builds a key from its segments, escaping each.
//
// SEGMENT BY SEGMENT: a raw dot would split a segment into two tokens and
// change what a filtered watch matches (a node id may carry one, a Slack
// thread's id always does), and the store refuses a space, a colon or a
// non-ASCII letter outright. Escaping also keeps `class.a.b` distinct from a
// single segment spelling it.
//
// AN EMPTY SEGMENT IS NOT A KEY: it is a caller that lost a value, and
// [DocumentSegments] refuses the key it builds, so every listing skips it
// rather than return a record naming nothing (a lease on no seat, a follow on
// no thread) for its reader to act on.
func DocumentKey(segments ...string) string {
	escaped := make([]string, len(segments))
	for i, seg := range segments {
		escaped[i] = escapeSegment(seg)
	}
	return strings.Join(escaped, KeySeparator)
}

// DocumentFilter builds the subject filter matching every key whose leading
// segments are these: a class prefix as a wildcard the BROKER can match. It
// lives beside [DocumentKey] so the two escape byte for byte alike.
//
// The wildcard is `>`, matching any remaining depth, so a class whose keys
// grow a segment stays selected; `*` would then silently match nothing, which
// a caller cannot tell from an empty class. An empty segment fails as
// silently, since nothing reads a filter back, so every filter is built from
// a constant, never from a value that arrived from elsewhere.
func DocumentFilter(segments ...string) string {
	return DocumentKey(segments...) + KeySeparator + ">"
}

// DocumentSegments recovers the segments of a key, reporting false for one
// this grammar did not write.
//
// A malformed key is skipped rather than guessed at: a listing that invented a
// segment would hand its caller a record under a name no writer used, and the
// caller would act on it as the record that name belongs to.
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
// The separator is NOT literal: a segment that could contain one would make
// the grammar ambiguous.
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
