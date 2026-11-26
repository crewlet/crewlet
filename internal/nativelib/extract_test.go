package nativelib

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture: a tar.bz2 holding two "libraries" and a file nobody keeps.
const (
	fixtureSHA = "6a8dc3f9c6491c465d01385d7602c55b6db2bc1bab6f757914e1688ac23e3a9c"
	libaSHA    = "accc5c58586795aa2769ca02d6116de7cb538965a97441b57ba9d8e567e533a0"
	libbSHA    = "eab64d31d21d642da8273ef9f39adf30a82aa975d54aced8ba2ff4e5d71a0871"
)

var kept = []File{
	{Path: "fixture/lib/liba.so", SHA256: libaSHA},
	{Path: "fixture/lib/libb.so", SHA256: libbSHA},
}

func fixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "fixture.tar.bz2"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// extractInto runs one verified pass of the fixture into dir.
func extractInto(t *testing.T, dir, archiveSHA string) error {
	t.Helper()
	want := map[string]string{}
	for _, f := range kept {
		want[f.Path] = filepath.Join(dir, filepath.Base(f.Path))
	}
	staged, err := Extract(bytes.NewReader(fixture(t)), archiveSHA, want)
	if err != nil {
		return err
	}
	return Commit(staged)
}

// AN ARCHIVE'S KEPT MEMBERS ARE WRITTEN UNDER THEIR BASE NAMES AND VERIFY, and
// the file nobody keeps is not written.
func TestAnArchivesKeptMembersAreExtractedAndVerify(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := extractInto(t, dir, fixtureSHA); err != nil {
		t.Fatal(err)
	}
	if err := Verify(dir, kept); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "liba.so,libb.so" {
		t.Errorf("the directory holds %v, want the two kept files and nothing else", names)
	}
}

// A FILE THAT NO LONGER VERIFIES IS REPLACED, by a new file rather than
// rewritten: a process that has the old one mapped keeps its inode.
// Mutation: write the members in place, and the held file is the same one.
func TestAReplacedFileIsANewFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := extractInto(t, dir, fixtureSHA); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(dir, "liba.so")
	held, err := os.Open(lib)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	before, _ := held.Stat()
	if err := os.WriteFile(lib, []byte("half a libr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if Verify(dir, kept) == nil {
		t.Fatal("a corrupt file verified")
	}
	if err := extractInto(t, dir, fixtureSHA); err != nil {
		t.Fatal(err)
	}
	if err := Verify(dir, kept); err != nil {
		t.Errorf("the replaced file does not verify: %v", err)
	}
	after, err := os.Stat(lib)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the file was rewritten in place, not replaced")
	}
}

// AN ARCHIVE WHOSE DIGEST DOES NOT MATCH ITS PIN LEAVES NOTHING LOADABLE.
func TestAnArchiveThatDoesNotMatchItsPinLeavesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := extractInto(t, dir, strings.Repeat("0", 64)); err == nil {
		t.Fatal("an archive that does not match its pin was extracted")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("%s was left behind", e.Name())
	}
}

// A MEMBER THE PIN NAMES AND THE ARCHIVE LACKS IS AN ERROR naming it.
func TestAMissingMemberIsNamed(t *testing.T) {
	t.Parallel()
	want := map[string]string{"fixture/lib/libc.so": filepath.Join(t.TempDir(), "libc.so")}
	_, err := Extract(bytes.NewReader(fixture(t)), fixtureSHA, want)
	if err == nil || !strings.Contains(err.Error(), "fixture/lib/libc.so") {
		t.Errorf("extract = %v, want an error naming the missing member", err)
	}
}

// A STAGED FILE IS KEPT ONLY WHEN IT IS THE PINNED ONE, and nothing of a
// mismatch is left.
func TestAStagedFileIsKeptOnlyWhenItMatchesItsPin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	final := filepath.Join(dir, "liba.so")
	if _, err := Stage(strings.NewReader("not the library\n"), final, libaSHA); err == nil {
		t.Error("a file that does not match its pin was staged")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a refused stage left %d files", len(entries))
	}
	tmp, err := Stage(strings.NewReader("the first library\n"), final, libaSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := Commit(map[string]string{final: tmp}); err != nil {
		t.Fatal(err)
	}
	if err := Verify(dir, kept[:1]); err != nil {
		t.Error(err)
	}
}
