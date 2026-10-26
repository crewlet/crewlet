package codeindex

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"

	"github.com/crewlet/crewlet/internal/gitref"
)

// Trigram is three consecutive bytes of a blob's Unicode lowering, packed
// big-endian into the low 24 bits: what the index files a blob under, and
// what a plan looks a literal up by.
type Trigram uint32

// trigramBytes is a trigram's length, and so the shortest literal the index
// can look up: a shorter one holds no trigram at all.
const trigramBytes = 3

// String is t's three bytes, quoted.
func (t Trigram) String() string {
	return strconv.Quote(string([]byte{byte(t >> 16), byte(t >> 8), byte(t)}))
}

// trigramAt is the trigram of b's three bytes from i.
func trigramAt(b []byte, i int) Trigram {
	return Trigram(uint32(b[i])<<16 | uint32(b[i+1])<<8 | uint32(b[i+2]))
}

// lower is the ONE lowering the index applies — to a blob before its
// trigrams are taken, and to a plain literal before it is looked up: Unicode's,
// rune by rune, which is bytes.ToLower and the comparison regexlit's
// prefilter makes for a folded literal. An ASCII-only lowering would keep É
// from meeting é, which (?i) treats as one letter.
//
// Lowering keeps every line where it was: '\n' lowers to itself and nothing
// else lowers to it — a byte that is not UTF-8 becomes U+FFFD, never a
// newline — so a blob and its lowering have the same lines, in order.
func lower(b []byte) []byte { return bytes.ToLower(b) }

// Trigrams is every distinct trigram of blob's Unicode lowering, ascending:
// what the index files blob under.
func Trigrams(blob []byte) []Trigram {
	return appendTrigrams(nil, lower(blob))
}

// appendTrigrams appends the distinct trigrams of lowered, ascending, to dst.
func appendTrigrams(dst []Trigram, lowered []byte) []Trigram {
	start := len(dst)
	for i := 0; i+trigramBytes <= len(lowered); i++ {
		dst = append(dst, trigramAt(lowered, i))
	}
	run := dst[start:]
	slices.Sort(run)
	return dst[:start+len(slices.Compact(run))]
}

// MaxFileBytes is the largest blob the index holds: 1 MiB. A larger one is
// SKIPPED — recorded, never read for its trigrams — and the grep path drops
// one past it by its size alone, so both paths search the same files.
//
// A file past a mebibyte in a source tree is generated, vendored or data — a
// minified bundle, a lockfile, a fixture — whose trigrams would join nearly
// every posting list and admit it to nearly every search, for hits nobody
// reads; and the bound is what one blob may cost a build: its lowering, and
// at most a million trigrams sorted (4 MiB).
const MaxFileBytes = 1 << 20

// BinaryProbeBytes is how much of a blob git reads to call it binary:
// FIRST_FEW_BYTES, 8 000, in git's xdiff-interface.c.
const BinaryProbeBytes = 8000

// Binary reports whether a blob whose opening is head is binary by git's own
// test (buffer_is_binary): a NUL in its first BinaryProbeBytes. head holds
// the blob's first BinaryProbeBytes, or all of a shorter blob; past them it
// is not read.
//
// The ONE copy of the rule: the index skips a binary blob by it, `git grep
// -I` skips the same one, and code/query answers a file read of one by its
// size alone. A gitattributes `binary` or `-diff` is git's other half of the
// rule, which a reader holding only the blob cannot see.
func Binary(head []byte) bool {
	return bytes.IndexByte(head[:min(len(head), BinaryProbeBytes)], 0) >= 0
}

// Skip reports whether a blob of size bytes, opening with head, is left out
// of the index — and so out of every search, whichever path answers it: past
// MaxFileBytes, or Binary.
func Skip(size int64, head []byte) bool {
	return size > MaxFileBytes || Binary(head)
}

// oidBytes is an object id's length: the engine's repositories are
// object-format sha1 (gitref.OIDHexLen), so twenty bytes.
const oidBytes = gitref.OIDHexLen / 2

// OID is an object id — a blob's, a commit's — as its twenty bytes.
type OID [oidBytes]byte

// ParseOID is the object id s spells: gitref's grammar, forty lowercase hex
// digits.
func ParseOID(s string) (OID, error) {
	var o OID
	if !gitref.ValidOID(s) {
		return o, fmt.Errorf("codeindex: %q is not an object id: one is %d lowercase hex digits", s,
			gitref.OIDHexLen)
	}
	if _, err := hex.Decode(o[:], []byte(s)); err != nil {
		return o, fmt.Errorf("codeindex: %q is not an object id: %w", s, err)
	}
	return o, nil
}

// String is o in hex, as git prints it.
func (o OID) String() string { return hex.EncodeToString(o[:]) }

// compare orders object ids by their bytes, which is their hex's order too.
func (o OID) compare(p OID) int { return bytes.Compare(o[:], p[:]) }
