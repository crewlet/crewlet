package codeindex_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/codeindex"
)

// A BLOB'S TRIGRAMS ARE THE THREE-BYTE RUNS OF ITS UNICODE LOWERING, each
// once, ascending — the lowering bytes.ToLower makes, so É meets é, the
// Kelvin sign meets k, and a byte that is not UTF-8 becomes U+FFFD's three.
func TestABlobsTrigramsAreItsLoweringsRuns(t *testing.T) {
	t.Parallel()
	runs := func(lowered string) []string {
		var out []string
		for i := 0; i+3 <= len(lowered); i++ {
			out = append(out, lowered[i:i+3])
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	for _, blob := range []string{"", "ab", "abc", "AbcD", "abcabc", "ÉTÉ", "KELVIN", "caf\xe9 x",
		"ſtatus", "line\nline\n"} {
		var got []string
		for _, tg := range codeindex.Trigrams([]byte(blob)) {
			got = append(got, string([]byte{byte(tg >> 16), byte(tg >> 8), byte(tg)}))
		}
		if want := runs(string(bytes.ToLower([]byte(blob)))); !slices.Equal(got, want) {
			t.Errorf("Trigrams(%q) = %q, want %q", blob, got, want)
		}
	}
	if got := codeindex.Trigrams([]byte("ÉTÉ")); !slices.Equal(got, codeindex.Trigrams([]byte("été"))) {
		t.Error("ÉTÉ and été have different trigrams, so (?i)été would miss ÉTÉ")
	}
	if got := codeindex.Trigrams([]byte("abc"))[0].String(); got != `"abc"` {
		t.Errorf("a trigram names itself %s, want \"abc\"", got)
	}
}

// A BLOB IS SKIPPED AS `git grep -I` SKIPS IT: past MaxFileBytes, or with a
// NUL among its first BinaryProbeBytes — git's buffer_is_binary — and never
// for one after them.
func TestABlobIsSkippedAsGitGrepSkipsIt(t *testing.T) {
	t.Parallel()
	nulAt := func(i, size int) []byte {
		b := bytes.Repeat([]byte("x"), size)
		b[i] = 0
		return b
	}
	for _, tc := range []struct {
		name string
		size int64
		head []byte
		skip bool
	}{
		{"text", 4, []byte("text"), false},
		{"empty", 0, nil, false},
		{"exactly the bound", codeindex.MaxFileBytes, []byte("text"), false},
		{"past the bound, unread", codeindex.MaxFileBytes + 1, nil, true},
		{"a NUL first", 4, []byte("\x00abc"), true},
		{"a NUL last of the probe", codeindex.BinaryProbeBytes, nulAt(codeindex.BinaryProbeBytes-1,
			codeindex.BinaryProbeBytes), true},
		{"a NUL just past the probe", codeindex.BinaryProbeBytes + 1, nulAt(codeindex.BinaryProbeBytes,
			codeindex.BinaryProbeBytes+1), false},
		{"not UTF-8", 3, []byte("\xff\xfe\xfd"), false},
	} {
		if got := codeindex.Skip(tc.size, tc.head); got != tc.skip {
			t.Errorf("Skip of %s = %v, want %v", tc.name, got, tc.skip)
		}
	}
}

// AN OBJECT ID IS GITREF'S GRAMMAR, forty lowercase hex digits.
func TestAnObjectIDIsGitrefsGrammar(t *testing.T) {
	t.Parallel()
	hex := strings.Repeat("0123456789", 4)
	oid, err := codeindex.ParseOID(hex)
	if err != nil || oid.String() != hex {
		t.Errorf("ParseOID(%s) = %s, %v", hex, oid, err)
	}
	for _, bad := range []string{"", hex[:39], strings.ToUpper(strings.Repeat("ab", 20)), hex + "0",
		strings.Repeat("g", 40)} {
		if _, err := codeindex.ParseOID(bad); err == nil {
			t.Errorf("ParseOID(%q) takes it", bad)
		}
	}
}
