package host

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// AN EDIT MATCHES ITS OLD TEXT EXACTLY ONCE, as the bytes stream: none is
// edit_not_found, two — overlapping ones included — edit_ambiguous naming both
// lines, a NUL in the first 8 000 bytes edit_binary, a carriage return beside
// an edit spanning a line edit_line_endings; and the result is the same however
// the file is cut into reads, a match straddling any of their boundaries.
//
// Mutations: count occurrences without overlap — "aa" in "aaa" edits, red;
// drop the carriage-return rule — the CRLF row edits, red; emit before the
// scan — a straddling match is written unreplaced, red.
func TestAnEditMatchesExactlyOnce(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		file, old, new string
		want           string
		refusal        code.WriteRefusal
		says           []string
	}{
		"once":                 {file: "alpha\nbeta\ngamma\n", old: "beta", new: "BETA", want: "alpha\nBETA\ngamma\n"},
		"deleted":              {file: "keep drop keep", old: " drop", new: "", want: "keep keep"},
		"at the start":         {file: "abc", old: "a", new: "x", want: "xbc"},
		"at the end":           {file: "abc", old: "c", new: "xyz", want: "abxyz"},
		"the whole file":       {file: "abc", old: "abc", new: "", want: ""},
		"spanning lines":       {file: "a\nb\nc\n", old: "a\nb", new: "z", want: "z\nc\n"},
		"a CR on its own line": {file: "a\r\nb\r\n", old: "b", new: "B", want: "a\r\nB\r\n"},
		"nowhere": {file: "alpha\n", old: "beta", new: "x", refusal: code.WriteEditNotFound,
			says: []string{"nowhere"}},
		"twice": {file: "x\nbeta\ny\nbeta\n", old: "beta", new: "B", refusal: code.WriteEditAmbiguous,
			says: []string{"line 2", "line 4"}},
		"overlapping": {file: "aaa", old: "aa", new: "b", refusal: code.WriteEditAmbiguous,
			says: []string{"line 1 and at line 1"}},
		"binary": {file: "a\x00b old", old: "old", new: "new", refusal: code.WriteEditBinary},
		"a NUL past the binary head": {file: strings.Repeat("x", editBinaryHead) + "\x00 old", old: "old",
			new: "new", want: strings.Repeat("x", editBinaryHead) + "\x00 new"},
		"a multi-line edit of a CRLF file": {file: "a\r\nb\r\n", old: "a\r\nb", new: "x",
			refusal: code.WriteEditLineEndings},
		"a multi-line replacement in a CRLF file": {file: "a\r\nb\r\n", old: "b", new: "b\nc",
			refusal: code.WriteEditLineEndings},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for step := 1; step <= len(tc.file)+1; step++ {
				var out bytes.Buffer
				e := newEditStream(tc.old, tc.new, &out)
				for rest := tc.file; rest != ""; {
					n := min(step, len(rest))
					if _, err := e.Write([]byte(rest[:n])); err != nil {
						t.Fatalf("reads of %d: %v", step, err)
					}
					rest = rest[n:]
				}
				refusal, detail, err := e.Close("f.txt")
				if err != nil {
					t.Fatalf("reads of %d: %v", step, err)
				}
				if refusal != tc.refusal {
					t.Fatalf("reads of %d: refused %q (%s), want %q", step, refusal, detail, tc.refusal)
				}
				for _, want := range tc.says {
					if !strings.Contains(detail, want) {
						t.Errorf("reads of %d: %q does not name %q", step, detail, want)
					}
				}
				if refusal == "" && out.String() != tc.want {
					t.Fatalf("reads of %d: wrote %q, want %q", step, out.String(), tc.want)
				}
			}
		})
	}
}

// AN EDIT HOLDS ITS TEXT AND A READ, NEVER THE FILE: a 64 MiB file streamed
// through it a 64 KiB read at a time leaves its buffer bounded by the read and
// the old text — whatever the file's size — and the edit made at its end.
//
// Mutation: keep every byte until Close — the buffer holds the file, red.
func TestAnEditHoldsNoFile(t *testing.T) {
	t.Parallel()
	const (
		size = 64 << 20
		read = 64 << 10
	)
	old, new := "the line to change", "the line, changed"
	var out countingWriter
	e := newEditStream(old, new, &out)
	chunk := bytes.Repeat([]byte("a quiet line of filler text\n"), read/28+1)[:read]
	held := 0
	for written := 0; written < size; written += read {
		if _, err := e.Write(chunk); err != nil {
			t.Fatal(err)
		}
		held = max(held, cap(e.buf))
	}
	if _, err := e.Write([]byte(old)); err != nil {
		t.Fatal(err)
	}
	if refusal, detail, err := e.Close("big.txt"); err != nil || refusal != "" {
		t.Fatalf("the edit of a large file: %q %s %v", refusal, detail, err)
	}
	if bound := 2 * (read + len(old) + len(new)); held > bound {
		t.Errorf("the edit held %d bytes of a %d-byte file, past %d: it buffers the file", held, size, bound)
	}
	if want := int64(size + len(new)); out.n != want {
		t.Errorf("the edit wrote %d bytes, want %d", out.n, want)
	}
}

// countingWriter counts what is written to it and keeps none of it.
type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// treeFixture is a repository whose root tree holds every mode a tree carries
// — a file, an executable, a link, a directory and a submodule — and names no
// filesystem would take beside ordinary ones: one with a line feed, one of 255
// bytes, one of a single byte. It answers the repository and its root tree.
func treeFixture(t testing.TB) (*gittest.Client, string, string) {
	t.Helper()
	c := gittest.NewClient(t)
	dir := c.Init(t)
	blob := strings.TrimSpace(c.Run(t, dir, "hash-object", "-w", "--stdin")) // the empty blob
	sub := strings.TrimSpace(c.Run(t, dir, "mktree"))                        // the empty tree
	var in strings.Builder
	for _, e := range []struct{ mode, kind, id, name string }{
		{"100644", "blob", blob, "README.md"},
		{"100755", "blob", blob, "run.sh"},
		{"120000", "blob", blob, "link"},
		{"040000", "tree", sub, "dir"},
		{"160000", "commit", strings.Repeat("ab", 20), "module"},
		{"100644", "blob", blob, "a\nb"},
		{"100644", "blob", blob, strings.Repeat("n", 255)},
		{"100644", "blob", blob, "x"},
	} {
		fmt.Fprintf(&in, "%s %s %s\t%s\x00", e.mode, e.kind, e.id, e.name)
	}
	out, stderr, err := c.Try(t, dir, strings.NewReader(in.String()), "mktree", "-z", "--missing")
	if err != nil {
		t.Fatalf("mktree: %v\n%s", err, stderr)
	}
	return c, dir, strings.TrimSpace(out)
}

// THE TREE PARSER READS WHAT LS-TREE PRINTS: a raw tree as cat-file answers
// it, parsed, is every entry git's own ls-tree -z lists — its mode as git reads
// it, its name byte for byte, its id — in the same order.
//
// Mutation: read a name up to a line feed — the "a\nb" entry parses wrong, red.
func TestTheTreeParserReadsWhatLsTreePrints(t *testing.T) {
	t.Parallel()
	c, dir, tree := treeFixture(t)
	raw := c.Run(t, dir, "cat-file", "tree", tree)
	var got []string
	if err := parseTree(strings.NewReader(raw), func(e treeEntry) error {
		mode := e.canon()
		got = append(got, fmt.Sprintf("%s%s %s %s\t%s", strings.Repeat("0", 6-len(mode)), mode, typeOf(mode),
			e.idHex(), e.name))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := strings.Split(strings.TrimSuffix(c.Run(t, dir, "ls-tree", "-z", tree), "\x00"), "\x00")
	if !slices.Equal(got, want) {
		t.Errorf("the parser read\n%q\nls-tree prints\n%q", got, want)
	}
}

// A MODE IS READ AS GIT READS ONE: whatever octal an old writer left, a
// regular file is 100644 or 100755 by its owner's execute bit, a link
// 120000, a directory 40000, anything else a submodule — the modes a
// directory a write passes through is written in, as a commit of it from an
// index would be (git's canon_mode).
func TestAModeIsReadAsGitReadsIt(t *testing.T) {
	t.Parallel()
	for mode, want := range map[string]string{
		"100644": code.ModeFile, "100664": code.ModeFile, "100600": code.ModeFile, "100755": code.ModeExecutable,
		"100744": code.ModeExecutable, "120000": code.ModeLink, "40000": modeTree, "040000": modeTree,
		"160000": modeSubmodule, "0100644": code.ModeFile,
	} {
		if got := (treeEntry{mode: mode}).canon(); got != want {
			t.Errorf("mode %s reads as %s, want %s", mode, got, want)
		}
	}
	for _, bad := range []string{"", "1006a4", "1234567890123"} {
		raw := bad + " name\x00" + strings.Repeat("\x01", 20)
		err := parseTree(strings.NewReader(raw), func(treeEntry) error { return nil })
		if !errors.Is(err, errBadTree) {
			t.Errorf("an entry of mode %q parsed: %v", bad, err)
		}
	}
}

// FuzzParseTree holds the parser to never panic on any bytes, and to read
// back exactly what it was handed wherever it reads a tree at all: a tree
// parsed and encoded again is the input. Its seeds are real trees, as git
// writes them, every mode and an awkward name among them.
func FuzzParseTree(f *testing.F) {
	c, dir, tree := treeFixture(f)
	f.Add([]byte(c.Run(f, dir, "cat-file", "tree", tree)))
	f.Add([]byte{})
	f.Add([]byte("100644 a\x00" + strings.Repeat("\x00", 20) + "40000 b\x00" + strings.Repeat("\xff", 20)))
	f.Add([]byte("100644 truncated\x00abc"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var entries []treeEntry
		err := parseTree(bytes.NewReader(raw), func(e treeEntry) error {
			entries = append(entries, e)
			return nil
		})
		if err != nil {
			if !errors.Is(err, errBadTree) {
				t.Fatalf("a parse failed with %v, which is no errBadTree", err)
			}
			return
		}
		if back := encodeTree(entries); !bytes.Equal(back, raw) {
			t.Fatalf("a tree parsed and encoded again is %q, not the %q it was", back, raw)
		}
	})
}

// A BATCH IS READ AS IT STREAMS, -Z's records whatever its reads' cuts: an
// object's contents to its sink, a missing name to its own, an answer cut
// short refused.
func TestABatchIsReadAsItStreams(t *testing.T) {
	t.Parallel()
	answers := "ab12 blob 3\x00one\x00missing-name missing\x00cd34 tree 0\x00\x00"
	names := []string{"ab12", "missing-name", "cd34"}
	for step := 1; step <= len(answers); step++ {
		var got []string
		var bodies []*bytes.Buffer
		r := newBatchReader(names, batchSink{
			object: func(i int, oid, kind string, size int64) (io.Writer, error) {
				got = append(got, fmt.Sprintf("%d %s %s %d", i, oid, kind, size))
				b := &bytes.Buffer{}
				bodies = append(bodies, b)
				return b, nil
			},
			missing: func(i int) error {
				got = append(got, fmt.Sprintf("%d missing", i))
				return nil
			},
		})
		for rest := answers; rest != ""; {
			n := min(step, len(rest))
			if _, err := r.Write([]byte(rest[:n])); err != nil {
				t.Fatalf("reads of %d: %v", step, err)
			}
			rest = rest[n:]
		}
		if err := r.Close(); err != nil {
			t.Fatalf("reads of %d: %v", step, err)
		}
		if want := []string{"0 ab12 blob 3", "1 missing", "2 cd34 tree 0"}; !slices.Equal(got, want) {
			t.Fatalf("reads of %d: %v, want %v", step, got, want)
		}
		if bodies[0].String() != "one" || bodies[1].Len() != 0 {
			t.Fatalf("reads of %d: contents %q and %q", step, bodies[0], bodies[1])
		}
	}
	r := newBatchReader(names, batchSink{
		object:  func(int, string, string, int64) (io.Writer, error) { return io.Discard, nil },
		missing: func(int) error { return nil },
	})
	_, _ = r.Write([]byte("ab12 blob 3\x00on"))
	if err := r.Close(); err == nil {
		t.Error("an answer cut inside an object's contents closed clean")
	}
}

// A CONFLICTED FILE'S START IS TEXT OR NOTHING: what a conflict answers of its
// file is its first bytes, a character the cut split taken off, and nothing
// at all of a file git reads as binary.
func TestAConflictsStartIsTextOrNothing(t *testing.T) {
	t.Parallel()
	w := &prefixWriter{n: 5}
	_, _ = w.Write([]byte("abcdéfgh"))
	if got := w.text(); got != "abcd" || !w.truncated() {
		t.Errorf("a cut through é kept %q (truncated %t), want \"abcd\" and truncated", got, w.truncated())
	}
	bin := &prefixWriter{n: 8}
	_, _ = bin.Write([]byte("ab\x00cdef"))
	if got := bin.text(); got != "" {
		t.Errorf("a binary file's start is %q, want nothing", got)
	}
	whole := &prefixWriter{n: 8}
	_, _ = whole.Write([]byte("short\n"))
	if got := whole.text(); got != "short\n" || whole.truncated() {
		t.Errorf("a short file is %q (truncated %t)", got, whole.truncated())
	}
}
