package query

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/code"
)

// A LITERAL IS MATCHED FOLDED, WHEREVER A WRITE CUTS IT. Every message is fed
// whole and in pieces of every size — so a rune, and the literal, are cut at
// every byte — and found exactly where strings.EqualFold finds the literal in
// one of the message's windows: across the cases of the Kelvin sign, of a
// long s and of a Greek sigma; never across a byte that is not UTF-8, which
// stands for itself; and with a match that begins inside a failed one (aaab).
//
// Mutation: fold left out (the literal matched as written) — the cases' arms
// red; a rune cut by a write read as bytes that are not UTF-8 — the pieces'
// arms red.
func TestALiteralIsMatchedFoldedWhereverAWriteCutsIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		literal, message string
		found            bool
	}{
		{"a.b", "fix the a.b parser", true},
		{"a.b", "fix the axb parser", false},
		{"abc", "the AbC of it", true},
		{"kelvin", "KELVIN", true},
		{"k", "K", true}, // the Kelvin sign folds to k
		{"strasse", "STRASSE", true},
		{"ſ", "S", true}, // a long s is an s
		{"σ", "Σ and ς", true},
		{"straße", "STRASSE", false}, // simple folding maps no rune to two
		{"aab", "aaab", true},
		{"abab", "abaabab", true},
		{"é", "\xc3", false}, // half a rune is a byte of its own
		{"x", "\xffx", true},
		{"needle", strings.Repeat("hay ", 1<<18) + "Needle", true},
		{"needle", strings.Repeat("hay ", 1<<18) + "needl", false},
	} {
		whole := newLiteral(tc.literal)
		whole.write([]byte(tc.message))
		if got := whole.end(); got != tc.found {
			t.Errorf("%q in %.40q, whole: %t, want %t", tc.literal, tc.message, got, tc.found)
		}
		if len(tc.message) > 64 {
			continue
		}
		for size := 1; size <= len(tc.message); size++ {
			l := newLiteral(tc.literal)
			for p := []byte(tc.message); len(p) > 0; p = p[min(size, len(p)):] {
				l.write(p[:min(size, len(p))])
			}
			if got := l.end(); got != tc.found {
				t.Errorf("%q in %q, in pieces of %d: %t, want %t", tc.literal, tc.message, size, got, tc.found)
			}
		}
	}
	// AND THE MATCHER IS READY FOR THE NEXT MESSAGE once one ends, a rune
	// the last ended inside included.
	l := newLiteral("ab")
	l.write([]byte("a\xc3"))
	l.end()
	l.write([]byte("b"))
	if l.end() {
		t.Error("a message's tail carried into the next")
	}
}

// logRecordText is one record of a log's walk as git writes it, its fields in
// the format's order, every one NUL-terminated, and the -z terminator.
func logRecordText(fields ...string) string {
	return strings.Join(fields, "\x00") + "\x00\x00"
}

// A LOG'S RECORD IS READ EXACTLY: its declared fields, each held to its
// grammar, and the empty field that ends it — fed whole and a byte at a time —
// and any record that is not that fails the page as output that does not
// parse, never answered as a commit: a field too many, a field too few, an id
// that is no id, a parent list that is no list, a date that is no number, and
// a stream that ends inside a record.
//
// Mutation: the terminator's emptiness unchecked (a field too many read as the
// next record's start) → red.
func TestALogRecordIsReadExactly(t *testing.T) {
	t.Parallel()
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	good := logRecordText(a, b+" "+a, "Ana", "ana@example.com", "1700000000", "1700000100", "fix it") +
		logRecordText(b, "", "Bo", "bo@example.com", "1600000000", "1600000100", "")
	for _, size := range []int{len(good), 1} {
		var got []code.LogCommit
		s := newLogStream(nil, func(rec *logRecord) error {
			c, _ := rec.commit()
			got = append(got, c)
			return nil
		})
		for p := []byte(good); len(p) > 0; p = p[min(size, len(p)):] {
			if _, err := s.Write(p[:min(size, len(p))]); err != nil {
				t.Fatalf("in pieces of %d: %v", size, err)
			}
		}
		if err := s.close(); err != nil {
			t.Fatalf("in pieces of %d: close: %v", size, err)
		}
		want := []code.LogCommit{
			{OID: a, Parents: []string{b, a}, Author: "Ana", AuthorEmail: "ana@example.com",
				AuthoredAt: time.Unix(1700000000, 0).UTC(), CommittedAt: time.Unix(1700000100, 0).UTC(), Subject: "fix it"},
			{OID: b, Author: "Bo", AuthorEmail: "bo@example.com", AuthoredAt: time.Unix(1600000000, 0).UTC(),
				CommittedAt: time.Unix(1600000100, 0).UTC()},
		}
		if !slices.EqualFunc(got, want, func(x, y code.LogCommit) bool {
			return x.OID == y.OID && slices.Equal(x.Parents, y.Parents) && x.Author == y.Author &&
				x.AuthorEmail == y.AuthorEmail && x.AuthoredAt.Equal(y.AuthoredAt) &&
				x.CommittedAt.Equal(y.CommittedAt) && x.Subject == y.Subject
		}) {
			t.Errorf("in pieces of %d the records read %+v, want %+v", size, got, want)
		}
	}
	for name, text := range map[string]string{
		"a field too many": strings.Join([]string{a, "", "Ana", "a@x", "1", "2", "s", "extra"}, "\x00") + "\x00\x00",
		"a field too few":  strings.Join([]string{a, "", "Ana", "a@x", "1", "2"}, "\x00") + "\x00\x00",
		"no id":            logRecordText("not an id", "", "Ana", "a@x", "1", "2", "s"),
		"a short parent":   logRecordText(a, "abc", "Ana", "a@x", "1", "2", "s"),
		"a doubled space":  logRecordText(a, b+"  "+a, "Ana", "a@x", "1", "2", "s"),
		"no date":          logRecordText(a, "", "Ana", "a@x", "yesterday", "2", "s"),
		"ended inside":     strings.Join([]string{a, "", "Ana"}, "\x00"),
	} {
		s := newLogStream(nil, func(*logRecord) error { return nil })
		_, err := s.Write([]byte(text))
		if err == nil {
			err = s.close()
		}
		if !errors.Is(err, errUnparsed) {
			t.Errorf("%s: %v, want output that does not parse", name, err)
		}
	}
}

// AN OCTOPUS MERGE IS DESCRIBED, NEVER CUT: past code.MaxLogParents its parents
// are counted and none listed, however many there are; up to it every one is.
//
// Mutation: the first MaxLogParents listed past the bound → red.
func TestAMergeOfManyParentsIsDescribedByItsCount(t *testing.T) {
	t.Parallel()
	for _, n := range []int{code.MaxLogParents, code.MaxLogParents + 1, 200} {
		parents := make([]string, n)
		for i := range parents {
			parents[i] = strings.Repeat(string("0123456789abcdef"[i%16]), 40)
		}
		var got code.LogCommit
		s := newLogStream(nil, func(rec *logRecord) error {
			got, _ = rec.commit()
			return nil
		})
		text := logRecordText(strings.Repeat("a", 40), strings.Join(parents, " "), "Ana", "a@x", "1", "2", "s")
		if _, err := s.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
		switch {
		case n <= code.MaxLogParents && (len(got.Parents) != n || got.ParentCount != 0):
			t.Errorf("%d parents read as %d listed and %d counted, want every one listed", n, len(got.Parents),
				got.ParentCount)
		case n > code.MaxLogParents && (got.Parents != nil || got.ParentCount != n):
			t.Errorf("%d parents read as %d listed and %d counted, want none listed and %d counted", n,
				len(got.Parents), got.ParentCount, n)
		}
	}
}

// A FIELD IS DESCRIBED PAST ITS ENCODED BOUND: a subject within its bytes but
// past them once encoded — control bytes, six each — or once neutralised — a
// fence's marker, `[escaped ` each — is answered by its size, as one past its
// bytes is; and one within every measure is answered whole, its neutralising
// counted.
//
// Mutation: the bound held to the raw bytes alone → the control bytes' arm red.
func TestAFieldIsDescribedPastItsEncodedBound(t *testing.T) {
	t.Parallel()
	marker := "[untrusted"
	for _, tc := range []struct {
		name, text string
		answered   bool
	}{
		{"plain, at the bound", strings.Repeat("s", code.MaxLogSubjectBytes), true},
		{"plain, past the bound", strings.Repeat("s", code.MaxLogSubjectBytes+1), false},
		{"control bytes, past it encoded", strings.Repeat("\x01", code.MaxLogSubjectBytes/6+1), false},
		{"markers, past it neutralised", strings.Repeat(marker, code.MaxLogSubjectBytes/len(marker)), false},
		{"a few markers, within it", strings.Repeat(marker, 10), true},
	} {
		h := heldText{cap: code.MaxLogSubjectBytes}
		h.add([]byte(tc.text))
		text, size, growth := h.described(code.MaxLogSubjectBytes)
		switch {
		case tc.answered && (text != tc.text || size != 0):
			t.Errorf("%s: answered %d bytes and size %d, want the text whole", tc.name, len(text), size)
		case tc.answered && growth != neutralGrowth(tc.text):
			t.Errorf("%s: its neutralising counted %d bytes, want %d", tc.name, growth, neutralGrowth(tc.text))
		case !tc.answered && (text != "" || size != len(tc.text)):
			t.Errorf("%s: answered %d bytes and size %d, want none and its %d", tc.name, len(text), size,
				len(tc.text))
		}
	}
}

// porcelain is a blame's output for two lines of a file named f, the first
// changed by commit a under the path old\nname (quoted as git quotes it), the
// second by commit b under f, a's details named once though it is named again.
func porcelain(a, b string) string {
	return a + " 1 1 1\nauthor Ana\nauthor-mail <ana@example.com>\nauthor-time 1700000000\nauthor-tz +0000\n" +
		"committer Ana\ncommitter-mail <ana@example.com>\ncommitter-time 1700000000\ncommitter-tz +0000\n" +
		"summary first\nboundary\nfilename \"old\\nname\\303\\251\"\n\tone\n" +
		b + " 2 2 2\nauthor Bo\nauthor-mail <>\nauthor-time 1600000000\nauthor-tz +0000\n" +
		"committer Bo\ncommitter-mail <>\ncommitter-time 1600000000\ncommitter-tz +0000\nsummary (" + b + ")\n" +
		"previous " + a + " f\nfilename f\n\ttwo\twith a tab\n" +
		b + " 3 3\n\tthree\n" +
		a + " 1 4 1\n\tfour\n"
}

// A BLAME'S PORCELAIN IS READ AS GIT WRITES IT — whole and a byte at a time:
// each line with its group's commit; a commit's details named once and every
// later line of it carrying them; its path C-unquoted as git quotes it;
// git's "(<id>)" stand-in for an empty message answered as no subject; an
// address taken out of its brackets; a detail no answer carries read past.
// And what is no porcelain fails as output that does not parse: a line with no
// header, a detail before any header, an id of a header's alone, a stream
// ending inside a line.
//
// Mutation: a commit's path taken from the line's group rather than kept for
// the commit (the fourth line, a's again, with no filename) → red.
func TestABlamesPorcelainIsReadAsGitWritesIt(t *testing.T) {
	t.Parallel()
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	type seen struct {
		final, orig   int
		commit, path  string
		text          string
		author, email string
		summary       string
		authored      time.Time
	}
	for _, size := range []int{len(porcelain(a, b)), 1} {
		var got []seen
		s := newBlameStream(func(l *blamedLine) error {
			c, _ := l.detail.commit()
			got = append(got, seen{final: l.final, orig: l.orig, commit: l.commit, path: l.detail.path,
				text: string(l.text.buf), author: c.Author, email: c.AuthorEmail, summary: c.Subject,
				authored: c.AuthoredAt})
			return nil
		})
		text := []byte(porcelain(a, b))
		for p := text; len(p) > 0; p = p[min(size, len(p)):] {
			if _, err := s.Write(p[:min(size, len(p))]); err != nil {
				t.Fatalf("in pieces of %d: %v", size, err)
			}
		}
		if err := s.close(); err != nil {
			t.Fatalf("in pieces of %d: close: %v", size, err)
		}
		old := "old\nnameé"
		want := []seen{
			{1, 1, a, old, "one", "Ana", "ana@example.com", "first", time.Unix(1700000000, 0).UTC()},
			{2, 2, b, "f", "two\twith a tab", "Bo", "", "", time.Unix(1600000000, 0).UTC()},
			{3, 3, b, "f", "three", "Bo", "", "", time.Unix(1600000000, 0).UTC()},
			{4, 1, a, old, "four", "Ana", "ana@example.com", "first", time.Unix(1700000000, 0).UTC()},
		}
		if !slices.Equal(got, want) {
			t.Errorf("in pieces of %d the porcelain read\n%+v\nwant\n%+v", size, got, want)
		}
	}
	for name, text := range map[string]string{
		"a line with no header":       "\tone\n",
		"a detail before a header":    "author Ana\n",
		"an id alone":                 a + "\n",
		"a header with no numbers":    a + " x y\n",
		"two headers, no line":        a + " 1 1 1\nfilename f\n" + a + " 2 2\n",
		"ended inside a line":         a + " 1 1 1\nfilename f\n\tone",
		"a filename git never quotes": a + " 1 1 1\nfilename \"f\n\tone\n",
	} {
		s := newBlameStream(func(*blamedLine) error { return nil })
		_, err := s.Write([]byte(text))
		if err == nil {
			err = s.close()
		}
		if !errors.Is(err, errUnparsed) {
			t.Errorf("%s: %v, want output that does not parse", name, err)
		}
	}
}

// A QUOTED PATH IS READ BACK BYTE FOR BYTE: every escape git's quote_c_style
// writes, octal for every other byte it quotes, and a path it left alone as
// itself — and what no quoting writes is refused.
func TestAQuotedPathIsReadBackByteForByte(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`plain/path`:                  "plain/path",
		`"a\"b\\c"`:                   "a\"b\\c",
		`"\a\b\t\n\v\f\r"`:            "\a\b\t\n\v\f\r",
		`"caf\303\251"`:               "café",
		`"\001\177\377"`:              "\x01\x7f\xff",
		`"-r and --output=x"`:         "-r and --output=x",
		`"with \"quotes\" and \\ in"`: "with \"quotes\" and \\ in",
	} {
		got, ok := unquoteC([]byte(in))
		if !ok || got != want {
			t.Errorf("unquoteC(%s) = %q, %t; want %q", in, got, ok, want)
		}
		if !utf8.ValidString(want) && ok && got != want {
			t.Errorf("unquoteC(%s) lost a byte that is not UTF-8", in)
		}
	}
	for _, in := range []string{`"open`, `"a"b"`, `"\q"`, `"\4000"`, `"\1"`, `"trailing\"`} {
		if got, ok := unquoteC([]byte(in)); ok {
			t.Errorf("unquoteC(%s) = %q, want it refused", in, got)
		}
	}
}
