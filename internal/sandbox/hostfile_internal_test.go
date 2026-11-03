//go:build unix

package sandbox

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// A LINK AT THE LAST ELEMENT IS NOT FOLLOWED, AND IS REFUSED AS A LINK — not
// even one that leads somewhere inside the root, which the root itself would
// follow. A link there leads nowhere, or somewhere the side of a container's
// mount that does not have to stay inside it chose: following the second would
// read a host file into the run's record; following the first read as a file
// not written yet. Both are a link, and both are refused as one — a refusal of
// that piece ([ErrNotRegularFile]), saying so rather than claiming a swap that
// may never have happened.
//
// Mutation: drop O_NOFOLLOW, and the link is read through and the dangling
// one reads as absent.
func TestAHostReadDoesNotFollowALinkPutThereSince(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	outside := filepath.Join(dir, "outside")
	if err := os.WriteFile(outside, []byte("a host file"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "done")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "findings.md")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	beside := filepath.Join(dir, "result.json")
	if err := os.Symlink("outside", beside); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"a link to a file": link, "a dangling link": dangling, "a link to a file beside it": beside,
	} {
		f, err := openRegularIn(root, filepath.Base(path))
		if err == nil {
			_ = f.Close()
			t.Errorf("%s at the path was followed", name)
			continue
		}
		var notRegular *NotRegularFileError
		if !errors.As(err, &notRegular) || !errors.Is(err, ErrNotRegularFile) || notRegular.Kind != kindSymlink {
			t.Errorf("%s = %v; want a NotRegularFileError naming a symbolic link", name, err)
		}
		if msg := err.Error(); !strings.Contains(msg, "is a symbolic link, and a box's files are read only where they lie") ||
			strings.Contains(msg, "after its path was checked") {
			t.Errorf("%s = %q; want it named for what it is, and no claim of when it was put there", name, msg)
		}
	}

	// And a regular file reads as it always did, a missing one as missing.
	f, err := openRegularIn(root, "outside")
	if err != nil {
		t.Fatalf("a regular file: %v", err)
	}
	_ = f.Close()
	if _, err := openRegularIn(root, "absent"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing file = %v; want fs.ErrNotExist, which a read answers as empty", err)
	}
}

// A WRITE REFUSED AFTER ITS LOOK IS NAMED FOR WHAT IT MET, AND LEAVES NOTHING
// STAGED. The box writes its own tree, so what the look before a write found
// can change before the rename: a directory put at the path in between makes
// the rename fail, and that failure is the same refusal the look gives — what
// is there, as a write refused — rather than a rename's "file exists". The
// file the write staged beside the path is removed with it, so a box is never
// left holding new content under a name nothing reads, and what the box put
// at the path is left as it is.
//
// Mutation: return the replacement's own error without looking again, and the
// refusal is a rename's error; or keep the staged file when the rename fails
// ([hostbox.ReplaceFileIn]'s removal), and it is left in the directory.
func TestAWriteRefusedAfterItsLookIsNamedAndLeavesNothingStaged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	obstacle := func(root *os.Root, name string, content io.Reader) error {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(dir, name, "the-box's"), 0o700); err != nil {
			return err
		}
		return hostbox.ReplaceFileIn(root, name, content)
	}
	err = replaceWith(root, "token", strings.NewReader("new"), obstacle)
	var refused *NotRegularFileError
	if !errors.As(err, &refused) || refused.Kind != "a directory" || !refused.Write {
		t.Fatalf("a write that met a directory put at its path = %v; want it refused as a write over "+
			"a directory", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "token" {
			t.Errorf("the directory holds %s beside the path: the write left it there", entry.Name())
		}
	}
	if info, err := os.Lstat(filepath.Join(dir, "token", "the-box's")); err != nil || !info.IsDir() {
		t.Errorf("what the box put at the path is now %v, %v; want it as the box made it", info, err)
	}
}
