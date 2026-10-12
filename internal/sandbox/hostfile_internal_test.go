//go:build unix

package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A LINK AT THE LAST ELEMENT IS NOT FOLLOWED, AND IS REFUSED AS A LINK. The
// escape check follows every link in a path that leads somewhere, so a link
// still there when the file is opened either leads nowhere — the check
// resolves a missing last element to itself — or was put there since, by the
// side of a container's mount that does not have to stay inside it. Following
// the second would read a host file into the run's record; following the
// first read as a file not written yet. Both are a link, and both are refused
// as one — a refusal of that piece ([ErrNotRegularFile]), saying so rather
// than claiming a swap that may never have happened.
//
// Mutation: drop O_NOFOLLOW, and the link is read through and the dangling
// one reads as absent.
func TestAHostReadDoesNotFollowALinkPutThereSince(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
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
	for name, path := range map[string]string{"a link to a file": link, "a dangling link": dangling} {
		f, err := openHostRegular(path)
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
	f, err := openHostRegular(outside)
	if err != nil {
		t.Fatalf("a regular file: %v", err)
	}
	_ = f.Close()
	if _, err := openHostRegular(filepath.Join(dir, "absent")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing file = %v; want fs.ErrNotExist, which a read answers as empty", err)
	}
}
