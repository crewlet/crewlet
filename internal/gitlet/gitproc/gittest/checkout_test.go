package gittest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// A CHECKOUT ANOTHER USER OWNS IS READ ONLY WHERE A TEST TRUSTS IT. Git refuses
// a repository whose owner is not the process's unless safe.directory names
// it, and honours that key only from protected configuration — the system's,
// the global and the command line's — of which a client reads only the last:
// its global file is one in its own HOME, so the developer's own trust never
// reaches it. ModuleCheckout trusts the module's own checkout, and that path
// alone, on the command line.
//
// Two ways to make the owner differ, because only one runs everywhere: git's
// own switch, GIT_TEST_ASSUME_DIFFERENT_OWNER, which drives the same refusal
// and the same safe.directory comparison on any machine, the module's
// checkout included; and a real chown, which only a process allowed to chown
// can make, and which is what says the switch stands in for the thing itself.
func TestACheckoutAnotherUserOwnsIsReadOnlyWhereTrusted(t *testing.T) {
	t.Parallel()
	refused := func(t *testing.T, c *Client, dir string) {
		t.Helper()
		_, stderr, err := c.Try(t, dir, nil, "rev-parse", "--show-toplevel")
		if err == nil || !strings.Contains(stderr, "dubious ownership") {
			t.Fatalf("git read %s as another user's without being told to trust it: %v\n%s", dir, err, stderr)
		}
	}

	t.Run("git_assumes_another_owner", func(t *testing.T) {
		t.Parallel()
		c := NewClient(t).Env("GIT_TEST_ASSUME_DIFFERENT_OWNER=1")
		root := sourcetree.Root(t)
		refused(t, c, root)

		trusted, top := c.ModuleCheckout(t)
		if top != root {
			t.Errorf("ModuleCheckout answered %s, want the module root %s", top, root)
		}
		if out := trusted.Run(t, top, "check-attr", "merge", "--", "static/dashboard/index.html"); !strings.Contains(out, ": merge: ") {
			t.Errorf("the trusted client could not read the checkout's attributes: %q", out)
		}
		// A client derived from it keeps the trust: Env copies the options
		// as it copies the environment.
		if got := strings.TrimSpace(trusted.Env("GIT_PROTOCOL=version=2").Run(t, top, "rev-parse", "--show-toplevel")); got != root {
			t.Errorf("a client derived from the trusted one read the top level %s, want %s", got, root)
		}
		// The trust is the module's checkout's alone: a repository anywhere
		// else is refused by the very client that reads the checkout.
		refused(t, trusted, NewClient(t).Init(t))
	})

	t.Run("chowned_to_another_uid", func(t *testing.T) {
		t.Parallel()
		c := NewClient(t)
		wt, err := filepath.EvalSymlinks(c.Init(t))
		if err != nil {
			t.Fatal(err)
		}
		uid, gid := os.Geteuid()+1, os.Getegid()+1
		err = filepath.WalkDir(wt, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Lchown(path, uid, gid)
		})
		if errors.Is(err, fs.ErrPermission) {
			t.Skipf("this process may not chown (%v): the git_assumes_another_owner case drives the same refusal", err)
		}
		if err != nil {
			t.Fatal(err)
		}
		refused(t, c, wt)
		if top := strings.TrimSpace(c.trusting(wt).Run(t, wt, "rev-parse", "--show-toplevel")); top != wt {
			t.Errorf("the trusted client read the top level %s, want %s", top, wt)
		}
	})
}
