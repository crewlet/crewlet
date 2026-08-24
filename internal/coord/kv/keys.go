package kv

import "strings"

// Resource names carry a colon (seat:alice, node:node-0, worker:scheduler)
// and NATS KV keys do not allow one: a key must match [-/_=.A-Za-z0-9]+,
// because it becomes a subject token under $KV.<bucket>. So a resource is
// escaped on the way in and unescaped on the way out, and the mapping has to
// be INJECTIVE in both directions — two resources that collided on one key
// would share one lease, which is two nodes holding one seat.
//
// The scheme is percent-encoding with '=' as the escape byte, which is the
// one punctuation character NATS allows that is also vanishingly rare in a
// handle. Everything outside [A-Za-z0-9_-] travels as =HH, '=' itself
// included — escaping the escape is what makes the mapping injective, and
// leaving '.' out of the literal set is deliberate: a dot in a key is a
// SUBJECT SEPARATOR, so an unescaped one would split a key into two tokens
// and quietly change what a filtered watch matches.
//
// The result stays legible to an operator running `nats kv ls`:
// seat:alice -> seat=3Aalice.
const escapeByte = '='

const hexDigits = "0123456789ABCDEF"

// literalKeyByte reports whether b may appear in a key as itself.
func literalKeyByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_' || b == '-':
		return true
	}
	return false
}

// encodeKey maps a resource name onto a NATS KV key.
func encodeKey(resource string) string {
	needsEscape := false
	for i := 0; i < len(resource); i++ {
		if !literalKeyByte(resource[i]) {
			needsEscape = true
			break
		}
	}
	if !needsEscape {
		return resource
	}
	var b strings.Builder
	// Worst case every byte escapes; sizing for it costs one allocation
	// and this runs on every lease operation.
	b.Grow(len(resource) * 3)
	for i := 0; i < len(resource); i++ {
		c := resource[i]
		if literalKeyByte(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte(escapeByte)
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

// decodeKey recovers the resource name from a key, reporting false for a key
// this backend did not write.
//
// A malformed key is skipped rather than guessed at: a listing that invented
// a resource name would put a seat nobody owns into a capacity calculation.
func decodeKey(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	if !strings.ContainsRune(key, escapeByte) {
		for i := 0; i < len(key); i++ {
			if !literalKeyByte(key[i]) {
				return "", false
			}
		}
		return key, true
	}
	var b strings.Builder
	b.Grow(len(key))
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c != escapeByte {
			if !literalKeyByte(c) {
				return "", false
			}
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(key) {
			return "", false
		}
		hi, ok := unhex(key[i+1])
		if !ok {
			return "", false
		}
		lo, ok := unhex(key[i+2])
		if !ok {
			return "", false
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String(), true
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	// Lower-case hex is rejected on purpose. encodeKey only ever emits
	// upper case, so accepting both would make two different keys decode to
	// one resource — the collision this mapping exists to rule out.
	return 0, false
}
