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

// A LINK AT THE LAST ELEMENT IS NOT FOLLOWED. The path a read is handed has
// already had every link in it followed by the escape check, so a link there
// when the file is opened was put there since — by the side of a container's
// mount that does not have to stay inside it — and following it would read a
// host file into the run's record.
//
// Mutation: drop O_NOFOLLOW, and the link is read through.
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
	if f, err := openHostRegular(link); err == nil {
		_ = f.Close()
		t.Fatal("a link at the path was followed")
	} else if !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("err = %v; want it to say the path is a link", err)
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
