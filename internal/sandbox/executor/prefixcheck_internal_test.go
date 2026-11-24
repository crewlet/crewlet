package executor

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// A CHAT FILE'S RUN START IS CHECKED ON THE ARCHIVE'S ONE REGULAR MEMBER, FED
// ONCE A BYTE: the scrub's first reading feeds the check the chat file's raw
// bytes, a directory before it passed over, so its prefix is kept; an archive
// holding a second regular member, or a prefix whose bytes changed, keeps
// none. A second reading after the first fed part of the prefix (a resumed
// reading copies through the bytes before its unit in small chunks) feeds only
// what the first did not, and the prefix is kept.
//
// Mutations, each red: read the first regular member as the one where another
// follows; feed a chunk the first reading already fed (it slices past its end).
func TestAChatRunStartIsCheckedOnTheOneRegularMemberFedOnce(t *testing.T) {
	t.Parallel()
	chat := `{"a":1}` + "\n" + `{"b":2}` + "\n" + `{"c":3}` + "\n" + `{"d":4}` + "\n"
	var sum cksum
	_, _ = sum.Write([]byte(strings.Join(strings.SplitAfter(chat, "\n")[:3], "")))
	want := chatRunStartOf("3 " + sum.String())
	if want.Lines != 3 {
		t.Fatalf("the prefix reads %+v, want three lines", want)
	}
	reg := func(name, body string) tarMember {
		return tarMember{h: tar.Header{Name: name, Mode: 0o600, Typeflag: tar.TypeReg}, body: body}
	}
	dir := tarMember{h: tar.Header{Name: "chats/", Mode: 0o700, Typeflag: tar.TypeDir}}
	for _, tc := range []struct {
		name    string
		members []tarMember
		lines   int64
	}{
		{"the chat file alone", []tarMember{reg("chats/s.jsonl", chat)}, 3},
		{"behind a directory", []tarMember{dir, reg("chats/s.jsonl", chat)}, 3},
		{"beside a second regular member", []tarMember{reg("chats/s.jsonl", chat), reg("notes.txt", "a note\n")}, 0},
		{"its prefix changed", []tarMember{reg("chats/s.jsonl", strings.Replace(chat, "2", "9", 1))}, 0},
	} {
		check := newPrefixCheck(want)
		archive := tarOfMembers(t, tc.members)
		if err := scrubTar(bytes.NewReader(archive), reopenOf(archive), io.Discard, newScrubber(nil),
			testBounds.checking(check)); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := check.lines(); got != tc.lines {
			t.Errorf("%s: the take keeps the run's start at line %d, want %d", tc.name, got, tc.lines)
		}
	}
	check := newPrefixCheck(want)
	h := &tar.Header{Typeflag: tar.TypeReg}
	if _, err := io.CopyN(io.Discard, iotest.OneByteReader(check.member(0, h, strings.NewReader(chat))), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, iotest.OneByteReader(check.member(0, h, strings.NewReader(chat)))); err != nil {
		t.Fatal(err)
	}
	if got := check.lines(); got != 3 {
		t.Errorf("read again after part of its prefix, the take keeps the run's start at line %d, want 3", got)
	}
}
