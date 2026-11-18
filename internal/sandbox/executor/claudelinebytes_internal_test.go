package executor

import (
	"bytes"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE ORACLE: a line's byte facts read off the whole line, by the functions
// each fact was read by where its reader read the line whole. [lineBytes]
// reads them as the line streams past, and is held to these.

// wholeLineBytes is what a reader of its bytes reads of raw, a whole line, its
// break dropped.
func wholeLineBytes(raw []byte) threadBytes {
	b := threadBytes{n: int64(len(raw)), headLen: uint8(min(len(raw), lineHeadBytes)), slotKeyed: slotKeyed(raw)}
	copy(b.head[:], raw)
	if at := slotKeyAt(raw); at >= 0 {
		copy(b.key[:], raw[at:])
		b.keyed = true
	}
	for _, m := range byteMarks {
		if bytes.Contains(raw, m.bytes) {
			b.marks |= m.mark
		}
	}
	return b
}

// slotKeyAt is where, in raw, a record's line, the 36 bytes every large-file
// reader keys it by begin, -1 where none ([threadBytes.KeyedBy] has the rule;
// [depthOneOf] the depth). Matches are read as bytes.Index finds them, each
// search resuming past the last match's key: of `"uuid":"uuid":"` the second
// is never one.
func slotKeyAt(raw []byte) int {
	first, good := -1, -1
	var more []int
	for from := 0; ; {
		i := bytes.Index(raw[from:], slotUUIDKey)
		if i < 0 {
			break
		}
		i += from
		if first < 0 {
			first = i
		}
		if after := i + len(slotUUIDKey) + slotKeyLen; after+len(slotTimeAfter) <= len(raw) &&
			bytes.Equal(raw[after:after+len(slotTimeAfter)], slotTimeAfter) {
			if good < 0 {
				good = i
			} else {
				more = append(more, i)
			}
		}
		from = i + len(slotUUIDKey)
	}
	at := first
	switch {
	case more != nil:
		at = depthOneOf(raw, append([]int{good}, more...))
	case good >= 0:
		at = good
	}
	if at < 0 || at+len(slotUUIDKey)+slotKeyLen > len(raw) {
		return -1
	}
	return at + len(slotUUIDKey)
}

// slotKeyed reports whether raw, a whole line, carries `"parentUuid":` at its
// object's depth one, outside every string (2.1.322's N1t @293787634, 2.1.310's
// Ipo @247171334), as a line that begins `{"parentUuid":`, the prune's slot,
// does too. A line whose JSON fails is scanned by its bytes as the readers
// scan it, so its depth is theirs.
func slotKeyed(raw []byte) bool {
	if !bytes.Contains(raw, slotParentKey) {
		return false
	}
	var s jsonDepth
	for i, b := range raw {
		if s.one() && b == '"' && bytes.HasPrefix(raw[i:], slotParentKey) {
			return true
		}
		s.step(b)
	}
	return false
}

// depthOneOf is the first of at, offsets into raw in their order, that lies
// at the line's object depth one outside every string, else the last of them
// (2.1.322's dzt @293787942, 2.1.310's $Mt @247171638).
func depthOneOf(raw []byte, at []int) int {
	var s jsonDepth
	for i, k := 0, 0; k < len(at); i++ {
		if i == at[k] {
			if s.one() {
				return at[k]
			}
			k++
		}
		s.step(raw[i])
	}
	return at[len(at)-1]
}

// A LINE'S BYTES ARE READ AS THE WHOLE LINE READS THEM, in pieces of any size
// ([lineBytes]): every fact equals [wholeLineBytes]'s, and each reader of the
// head answers on the head as on the whole line ([sidechainHeaded]; [slotHead]
// for null and for the 38 bytes after the prefix, named or not; a subagent's
// head at every end); KeyedBy answers alike for the 36 bytes after every
// `"uuid":"`, for an id of no key and for none. The lines are every Claude Code
// thread's under testdata, the layer's own records and [byteFactLines]' hard
// cases, in pieces of 1, 2, 7, 8, 13, 51 and 65,536 bytes and of random sizes.
//
// Mutation: take a `"uuid":"` 7 bytes past the last taken rather than 8; read
// the depth after stepping a quote; drop the comparisons a piece ends inside;
// skip an escaped quote; or hold a head a byte short of the longest read.
func TestALinesBytesAreReadAsTheWholeLineReadsThem(t *testing.T) {
	t.Parallel()
	if lineHeadBytes != len(slotPrefix)+slotKeyLen+2+len(slotSidechainHead) {
		t.Fatalf("a line's byte facts hold %d bytes of its head; the longest head a reader reads is %d", lineHeadBytes,
			len(slotPrefix)+slotKeyLen+2+len(slotSidechainHead))
	}
	lines := byteFactLines(t)
	random := rand.New(rand.NewPCG(1170, 4))
	for _, line := range lines {
		for _, size := range []int{1, 2, 7, 8, 13, 51, 64 << 10} {
			checkLineBytes(t, line, func(int) int { return size })
		}
		for range 3 {
			checkLineBytes(t, line, func(left int) int { return 1 + random.IntN(min(left, 97)) })
		}
	}
}

// FuzzALinesBytesAreReadAsTheWholeLineReadsThem holds a line of any bytes,
// written in pieces of sizes its seed draws, to what the whole-line functions
// read of it, as [TestALinesBytesAreReadAsTheWholeLineReadsThem] does.
func FuzzALinesBytesAreReadAsTheWholeLineReadsThem(f *testing.F) {
	for i, line := range byteFactHardLines() {
		f.Add([]byte(line), uint64(i))
	}
	f.Fuzz(func(t *testing.T, line []byte, seed uint64) {
		random := rand.New(rand.NewPCG(seed, seed>>32))
		checkLineBytes(t, line, func(left int) int { return 1 + random.IntN(min(left, 80)) })
	})
}

// checkLineBytes writes line to a [lineBytes] in pieces whose sizes next
// draws (handed what is left, at least 1), and holds what it reads to
// [wholeLineBytes] and its head's readers to the whole line's.
func checkLineBytes(t *testing.T, line []byte, next func(left int) int) {
	t.Helper()
	var l lineBytes
	var sizes []int
	for rest := line; len(rest) > 0; {
		n := min(next(len(rest)), len(rest))
		sizes = append(sizes, n)
		l.write(rest[:n])
		rest = rest[n:]
	}
	got, want := l.facts(), wholeLineBytes(line)
	at := func() string {
		if len(sizes) == 0 {
			return "a line of no bytes"
		}
		return fmt.Sprintf("%.60q of %d bytes, in %d pieces, the first of %d", line, len(line), len(sizes),
			sizes[0])
	}
	if got != want {
		t.Errorf("%s: read as it streams: %d bytes, head %q, slot keyed %t, keyed %t by %q, marks %b; "+
			"read whole: %d bytes, head %q, slot keyed %t, keyed %t by %q, marks %b", at(), got.Len(), got.Head(),
			got.slotKeyed, got.keyed, got.key[:], got.marks, want.Len(), want.Head(), want.slotKeyed, want.keyed,
			want.key[:], want.marks)
		return
	}
	head := got.Head()
	if sidechainHeaded(head) != sidechainHeaded(line) {
		t.Errorf("%s: its head says it is a sidechain's right after its parent %t, the whole line %t", at(),
			sidechainHeaded(head), sidechainHeaded(line))
	}
	parents := [][]byte{jsonNull}
	if rest, ok := bytes.CutPrefix(line, slotPrefix); ok && len(rest) >= slotKeyLen+2 {
		parents = append(parents, rest[:slotKeyLen+2])
	}
	for _, parent := range parents {
		for _, named := range []bool{false, true} {
			if a, b := slotHead(head, parent, named), slotHead(line, parent, named); a != b {
				t.Errorf("%s: its head ends the parent %q (named %t) at %d, the whole line at %d", at(), parent,
					named, a, b)
			}
		}
	}
	for end := 0; end <= min(len(line), len(slotPrefix)+slotKeyLen+2); end++ {
		if a, b := bytes.HasPrefix(head[end:], slotSidechainHead), bytes.HasPrefix(line[end:], slotSidechainHead); a != b {
			t.Errorf("%s: its head says a sidechain's at %d %t, the whole line %t", at(), end, a, b)
		}
	}
	ids := []string{"", ctUUID(99)}
	for from := 0; ; {
		i := bytes.Index(line[from:], slotUUIDKey)
		if i < 0 {
			break
		}
		if key := from + i + len(slotUUIDKey); key+slotKeyLen <= len(line) {
			ids = append(ids, string(line[key:key+slotKeyLen]))
		}
		from += i + 1
	}
	for _, id := range ids {
		if got.KeyedBy(id) != keyedBy(line, id) {
			t.Errorf("%s: keyed by %q %t, read whole %t", at(), id, got.KeyedBy(id), keyedBy(line, id))
		}
	}
}

// keyedBy reports whether the uuid every large-file reader keys raw, a
// record's line, by is id ([slotKeyAt]).
func keyedBy(raw []byte, id string) bool {
	at := slotKeyAt(raw)
	return at >= 0 && string(raw[at:at+slotKeyLen]) == id
}

// byteFactLines is every line of every Claude Code thread under testdata, the
// layer's own records, and the hard cases ([byteFactHardLines]).
func byteFactLines(t *testing.T) [][]byte {
	t.Helper()
	var lines [][]byte
	err := filepath.WalkDir(filepath.Join("testdata", "claude"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for line := range bytes.Lines(b) {
			lines = append(lines, bytes.TrimSuffix(line, []byte("\n")))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) < 100 {
		t.Fatalf("testdata holds %d thread lines; the claim reads every one", len(lines))
	}
	for _, line := range []string{ctEnqueue, ctBrief(ctUUID(1)), ctAssistant(ctUUID(1), ctUUID(2), ctUseContent),
		ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn), ctLastPrompt, ctMeta} {
		lines = append(lines, []byte(line))
	}
	for _, line := range byteFactHardLines() {
		lines = append(lines, []byte(line))
	}
	return lines
}

// byteFactHardLines is the lines whose bytes a reader of them is hardest to
// read alike to the whole line: a pattern begun at a quote a backslash
// escapes, keys a later key begins inside, every pattern, strings holding
// braces, backslashes and escaped quotes, a depth below zero, two and three
// keys `","timestamp":"` follows at mixed depths, a key the line ends within
// 36 or 51 bytes of, and lines shorter than the head.
func byteFactHardLines() []string {
	a, b, c := ctUUID(1), ctUUID(2), ctUUID(3)
	return []string{
		"",
		"{",
		`{"parentUuid":null}`,
		`{"parentUuid":null,"isSidechain":true,"uuid":"` + a + `","timestamp":"t"}`,
		`{"parentUuid":"` + a + `","isSidechain":true,"uuid":"` + b + `","timestamp":"t"}`,
		`{"parentUuid":"` + a + `","isSidechain":false,"uuid":"` + b + `","timestamp":"t"}`,
		`{"parentUuid":"` + strings.ToUpper(a) + `","isSidechain":true}`,
		`{"parent\u0055uid":null,"uuid":"` + a + `","timestamp":"t"}`,
		`{"parentUuid" :null}`,
		`[{"parentUuid":null,"uuid":"` + a + `"}]`,
		`{"a":"parentUuid":1}`,
		`{"message":{"content":[{"type":"tool_use","input":{"x\"isSidechain":true}}]}}`,
		`{"input":{"x\"type":"image"}}`,
		`{"input":{"x\"uuid":"` + a + `","timestamp":"t"},"uuid":"` + b + `","timestamp":"t"}`,
		`{"x":"\"parentUuid\":","parentUuid":null}`,
		`{"x":"\\","parentUuid":null}`,
		`{"x":"\\\"parentUuid\":1","y":{"parentUuid":2}}`,
		`{"x":"{{{","parentUuid":null}`,
		`{"x":"}}}","parentUuid":null}`,
		`}}}{"parentUuid":null}`,
		`{{"parentUuid":null}}`,
		`{"uuid":"uuid":"` + a + `","timestamp":"t"}`,
		`{"uuid":"uuid":"uuid":"` + a + `","timestamp":"t"}`,
		`{"m":{"uuid":"` + a + `","timestamp":"1"},"uuid":"` + b + `","timestamp":"2"}`,
		`{"m":{"uuid":"` + a + `","timestamp":"1"},"n":{"uuid":"` + b + `","timestamp":"2"}}`,
		`{"m":{"uuid":"` + a + `","timestamp":"1"},"uuid":"` + b + `","timestamp":"2","n":{"uuid":"` + c +
			`","timestamp":"3"}}`,
		`{"m":{"uuid":"` + a + `","timestamp":"1"},"n":{"uuid":"` + b + `","timestamp":"2"},"o":[{"uuid":"` + c +
			`","timestamp":"3"}]}`,
		`{"uuid":"` + a + `"}`,
		`{"uuid":"` + a,
		`{"uuid":"` + a[:20],
		`{"uuid":"` + a + `","timestamp`,
		`{"uuid":"` + a + `","timestamp":"`,
		`{"type":"message_pdf_reads"}`,
		`{"type":"document","isUnmetered":true}`,
		`{"edits":[{"type":"` + editClearToolUses + `"}]}`,
		`{"a":"\"type\":\"image\""}`,
		`{"a":"x"}` + "\r",
	}
}
