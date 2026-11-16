package gitref_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitref"
)

// tagOID is the id every record of the cases below names.
var tagOID = strings.Repeat("a", gitref.OIDHexLen)

// tagRecord is body as `cat-file --batch` writes a tag object: its header, its
// bytes and a newline.
func tagRecord(body string) string {
	return fmt.Sprintf("%s tag %d\n%s\n", tagOID, len(body), body)
}

// tagHead is a tag object's header lines and the empty line after them.
func tagHead(tagger string) string {
	return "object " + strings.Repeat("b", gitref.OIDHexLen) + "\ntype commit\ntag v1\n" + tagger + "\n\n"
}

const tagger = "tagger A U Thor <author@example.com> 1700000000 +0200"

// readTags is every tag stream reads of out, written whole and then a byte at
// a time, the two required to agree.
func readTags(t *testing.T, caps gitref.TagCaps, out string) ([]gitref.Tag, error) {
	t.Helper()
	read := func(chunk int) ([]gitref.Tag, error) {
		var got []gitref.Tag
		s := gitref.NewTagStream(caps, func(tag gitref.Tag) error {
			got = append(got, tag)
			return nil
		})
		for p := out; p != ""; {
			n := min(chunk, len(p))
			if _, err := s.Write([]byte(p[:n])); err != nil {
				return got, err
			}
			p = p[n:]
		}
		return got, s.Close()
	}
	whole, werr := read(len(out) + 1)
	bytewise, berr := read(1)
	if fmt.Sprint(whole, werr) != fmt.Sprint(bytewise, berr) {
		t.Fatalf("read whole: %v, %v; a byte at a time: %v, %v", whole, werr, bytewise, berr)
	}
	return whole, werr
}

// A tag's subject is its first paragraph read as git's %s reads a commit's, its
// tagger read off the tagger line, each field capped, and a non-tag refused.
// Mutations: keep trailing whitespace; let a signature in; pass a cap.
func TestATagsSubjectIsReadAsACommitsIs(t *testing.T) {
	t.Parallel()
	caps := gitref.TagCaps{Person: 1 << 10, Subject: 4 << 10}
	at := time.Unix(1700000000, 0).UTC()
	for _, tc := range []struct {
		name, body, subject string
	}{
		{"one line", tagHead(tagger) + "release one\n", "release one"},
		{"leading empty lines", tagHead(tagger) + "\n \n\t\nrelease one\n", "release one"},
		{"CRLF line ends", tagHead(tagger) + "first\r\nsecond\r\n\r\nbody\r\n", "first second"},
		{"trailing spaces on a subject line", tagHead(tagger) + "first \t\nsecond  \n\nbody\n", "first second"},
		{"leading spaces kept", tagHead(tagger) + "  first\n  second\n", "  first   second"},
		{"no trailing newline", tagHead(tagger) + "first\nsecond", "first second"},
		{"an empty message", tagHead(tagger), ""},
		{"a signed tag with no message", tagHead(tagger) + "-----BEGIN PGP SIGNATURE-----\n\niQEz\n" +
			"-----END PGP SIGNATURE-----\n", ""},
		{"a signature after the subject's lines", tagHead(tagger) + "first\n-----BEGIN SSH SIGNATURE-----\nU1NI\n" +
			"-----END SSH SIGNATURE-----\n", "first"},
		{"a second paragraph that is a signature", tagHead(tagger) + "first\n\n-----BEGIN PGP MESSAGE-----\n",
			"first"},
		{"a line that only begins like a marker", tagHead(tagger) + "-----BEGIN PGP thoughts\n", "-----BEGIN PGP thoughts"},
		{"a dash line", tagHead(tagger) + "-----\nnext\n", "----- next"},
		{"a gpgsig continuation header", "object " + strings.Repeat("b", 40) + "\ntype commit\ntag v1\n" + tagger +
			"\ngpgsig-sha256 -----BEGIN PGP SIGNATURE-----\n \n iQEz\n -----END PGP SIGNATURE-----\n\nthe subject\n",
			"the subject"},
	} {
		got, err := readTags(t, caps, tagRecord(tc.body))
		if err != nil || len(got) != 1 {
			t.Errorf("%s: read %d tags, %v; want one", tc.name, len(got), err)
			continue
		}
		g := got[0]
		if string(g.Subject.Bytes) != tc.subject || g.Subject.Len != len(tc.subject) {
			t.Errorf("%s: subject %q of %d bytes, want %q", tc.name, g.Subject.Bytes, g.Subject.Len, tc.subject)
		}
		if g.OID != tagOID || g.Object != strings.Repeat("b", 40) || g.Type != "commit" ||
			string(g.TaggerName.Bytes) != "A U Thor" || string(g.TaggerEmail.Bytes) != "author@example.com" ||
			!g.TaggedAt.Equal(at) || g.TaggedAt.Location() != time.UTC {
			t.Errorf("%s: read %+v, want the header's object, type and tagger", tc.name, g)
		}
	}

	// THE TAGGER LINE: a date git reads, or none, the rest standing.
	for _, tc := range []struct {
		line, name, email string
		at                time.Time
	}{
		{"tagger A U Thor <a@b> 1700000000 +0200", "A U Thor", "a@b", at},
		{"tagger   A U Thor  \t <a@b>   1700000000   -0000", "A U Thor", "a@b", at},
		{"tagger A U Thor <a@b>", "A U Thor", "a@b", time.Time{}},
		{"tagger A U Thor <a@b> 1700000000", "A U Thor", "a@b", time.Time{}},
		{"tagger A U Thor <a@b> noon +0200", "A U Thor", "a@b", time.Time{}},
		{"tagger A U Thor <a@b> 12345678901234567890 +0200", "A U Thor", "a@b", time.Time{}},
		{"tagger A U Thor <a@b", "A U Thor", "", time.Time{}},
		{"tagger A U Thor", "", "", time.Time{}},
		{"tagger <a@b> 0 +0000", "", "a@b", time.Unix(0, 0).UTC()},
	} {
		got, err := readTags(t, caps, tagRecord(tagHead(tc.line)+"s\n"))
		if err != nil || len(got) != 1 {
			t.Errorf("%q: read %d tags, %v; want one", tc.line, len(got), err)
			continue
		}
		g := got[0]
		if string(g.TaggerName.Bytes) != tc.name || string(g.TaggerEmail.Bytes) != tc.email ||
			!g.TaggedAt.Equal(tc.at) || string(g.Subject.Bytes) != "s" {
			t.Errorf("%q: name %q, email %q, date %v, subject %q; want %q, %q, %v, \"s\"", tc.line,
				g.TaggerName.Bytes, g.TaggerEmail.Bytes, g.TaggedAt, g.Subject.Bytes, tc.name, tc.email, tc.at)
		}
	}
	untagged := tagRecord("object " + strings.Repeat("b", 40) + "\ntype commit\n\nno tagger\n")
	if got, err := readTags(t, caps, untagged); err != nil || len(got) != 1 || !got[0].TaggedAt.IsZero() ||
		got[0].TaggerName.Len != 0 || string(got[0].Subject.Bytes) != "no tagger" {
		t.Errorf("a tag with no tagger: %+v, %v; want no tagger and no date", got, err)
	}

	// THE CAPS: a 5 KiB name and a 6 KiB subject, held to 1 and 4 KiB and
	// counted whole; trailing whitespace past a cap is no part of the count.
	name, subject := strings.Repeat("n", 5<<10), strings.Repeat("s", 3<<10)+"   "+strings.Repeat("t", 3<<10)
	wideTag := tagHead("tagger "+name+" <a@b> 1 +0000") + subject + strings.Repeat(" ", 8<<10) + "\n"
	got, err := readTags(t, caps, tagRecord(wideTag))
	if err != nil || len(got) != 1 {
		t.Fatalf("the wide tag: %d tags, %v", len(got), err)
	}
	if g := got[0]; string(g.TaggerName.Bytes) != name[:1<<10] || g.TaggerName.Len != len(name) ||
		string(g.Subject.Bytes) != subject[:4<<10] || g.Subject.Len != len(subject) {
		t.Errorf("the wide tag held a name of %d bytes counted %d and a subject of %d counted %d; want %d of %d "+
			"and %d of %d", len(g.TaggerName.Bytes), g.TaggerName.Len, len(g.Subject.Bytes), g.Subject.Len, 1<<10,
			len(name), 4<<10, len(subject))
	}

	// TWO OBJECTS IN ONE STREAM, each handed on in its order.
	second := strings.Replace(tagRecord(tagHead(tagger)+"second\n"), tagOID, strings.Repeat("c", 40), 1)
	got, err = readTags(t, caps, tagRecord(tagHead(tagger)+"first\n")+second)
	if err != nil || len(got) != 2 || string(got[0].Subject.Bytes) != "first" ||
		string(got[1].Subject.Bytes) != "second" || got[1].OID != strings.Repeat("c", 40) {
		t.Errorf("two tags in one stream: %+v, %v", got, err)
	}

	// WHAT IS NO TAG OBJECT.
	for name, out := range map[string]string{
		"missing":                tagOID + " missing\n",
		"a blob":                 tagOID + " blob 2\nhi\n",
		"cut inside an object":   tagRecord(tagHead(tagger) + "s\n")[:60],
		"cut before its newline": strings.TrimSuffix(tagRecord(tagHead(tagger)+"s\n"), "\n"),
		"a size that is no size": tagOID + " tag +2\nhi\n",
		"no id":                  "HEAD tag 2\nhi\n",
		"a long object header":   tagRecord("object " + strings.Repeat("b", 65) + "\n\n"),
		"bytes past its newline": strings.TrimSuffix(tagRecord("x"), "\n") + "y",
	} {
		if _, err := readTags(t, caps, out); !errors.Is(err, gitref.ErrNotATag) {
			t.Errorf("%s: %v, want ErrNotATag", name, err)
		}
	}

	// THE CALLBACK'S ERROR ENDS THE RUN.
	stop := errors.New("enough")
	s := gitref.NewTagStream(caps, func(gitref.Tag) error { return stop })
	if _, err := s.Write([]byte(tagRecord(tagHead(tagger) + "s\n"))); !errors.Is(err, stop) {
		t.Errorf("a callback's error: Write = %v, want it", err)
	}
	if err := s.Close(); !errors.Is(err, stop) {
		t.Errorf("a callback's error: Close = %v, want it", err)
	}
}
