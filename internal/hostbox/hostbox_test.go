package hostbox

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSafeJoinAcceptsPathsInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		".claude/.credentials.json",
		"workspace/repo/file.txt",
		"a/b/../c",
		"./nested/file",
	} {
		got, err := SafeJoin(root, rel)
		if err != nil {
			t.Fatalf("SafeJoin(%q): %v", rel, err)
		}
		if !strings.HasPrefix(got, root) {
			t.Fatalf("SafeJoin(%q) = %q, want it under %q", rel, got, root)
		}
	}
}

func TestSafeJoinRefusesAnEscape(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"../outside",
		"../../etc/cron.d/x",
		"a/../../outside",
		"/etc/passwd",
		"",  // resolves back to the root itself
		".", // likewise
	} {
		if got, err := SafeJoin(root, rel); !errors.Is(err, ErrEscape) {
			t.Fatalf("SafeJoin(%q) = %q, %v; want ErrEscape", rel, got, err)
		}
	}
}

// The lexical form of a symlinked path stays under the root, so a Clean-only
// check passes it. This is the case that makes resolution non-negotiable.
func TestSafeJoinRefusesAnEscapeThroughASymlinkedAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// The target does not exist — which is the ordinary case, since these
	// paths are about to be created — so the ancestor is what must resolve.
	if got, err := SafeJoin(root, "link/credentials.json"); !errors.Is(err, ErrEscape) {
		t.Fatalf("SafeJoin through a symlink = %q, %v; want ErrEscape", got, err)
	}
}

// A symlink that stays inside the box is fine: containment is the property,
// not the absence of links.
func TestSafeJoinAllowsASymlinkThatStaysInside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
	root := t.TempDir()
	inner := filepath.Join(root, "real")
	if err := os.MkdirAll(inner, DirMode); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(inner, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	got, err := SafeJoin(root, "link/file")
	if err != nil {
		t.Fatalf("SafeJoin: %v", err)
	}
	if want := filepath.Join(inner, "file"); got != want {
		t.Fatalf("SafeJoin = %q, want %q", got, want)
	}
}

// The root is resolved too, so a caller whose box path runs through a symlink
// (t.TempDir does exactly this on macOS, via /var -> /private/var) is not
// refused for it.
func TestSafeJoinResolvesTheRootAsWell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "boxes")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	got, err := SafeJoin(link, "workspace/file")
	if err != nil {
		t.Fatalf("SafeJoin under a symlinked root: %v", err)
	}
	realResolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if want := filepath.Join(realResolved, "workspace", "file"); got != want {
		t.Fatalf("SafeJoin = %q, want %q", got, want)
	}
}

func TestWithinIsStrictAndNotAPrefixTest(t *testing.T) {
	root := filepath.Clean("/home/user")
	cases := map[string]bool{
		"/home/user/a":   true,
		"/home/user/a/b": true,
		"/home/user":     false, // strictly inside, so not itself
		"/home/user-2/a": false, // the separator is what stops this
		"/home/users":    false,
		"/home":          false,
		"/":              false,
	}
	for path, want := range cases {
		if got := Within(root, filepath.Clean(path)); got != want {
			t.Fatalf("Within(%q, %q) = %v, want %v", root, path, got, want)
		}
	}
}

func TestInheritTakesOnlyTheAllowlist(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-not-for-the-child")
	t.Setenv("CREWLET_SECRET_KEY_2026_01", "base64-keyring-material-not-for-the-child")

	env := Inherit()
	if env["PATH"] != "/usr/bin" {
		t.Fatalf("PATH = %q, want it passed through", env["PATH"])
	}
	for _, secret := range []string{"ANTHROPIC_API_KEY", "CREWLET_SECRET_KEY_2026_01"} {
		if _, ok := env[secret]; ok {
			t.Fatalf("%s reached the child environment", secret)
		}
	}
}

func TestInheritTakesTheExtraNamesAProfileAsksFor(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/somewhere")
	env := Inherit("CLAUDE_CONFIG_DIR")
	if env["CLAUDE_CONFIG_DIR"] != "/somewhere" {
		t.Fatalf("extra name not passed through: %v", env)
	}
}

// A child that tells "unset" from "empty" must see what the engine saw.
func TestInheritOmitsAnUnsetNameRatherThanPassingItEmpty(t *testing.T) {
	os.Unsetenv("NO_PROXY")
	if _, ok := Inherit()["NO_PROXY"]; ok {
		t.Fatal("an unset name was passed through as empty")
	}
	t.Setenv("NO_PROXY", "")
	if value, ok := Inherit()["NO_PROXY"]; !ok || value != "" {
		t.Fatalf("an explicitly empty name was dropped: %q, %v", value, ok)
	}
}

func TestCopyFileAtomicWritesTheContentAtFileMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "nested", "dst")
	if err := os.WriteFile(src, []byte("token"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ran, err := CopyFileAtomic(src, dst)
	if err != nil || !ran {
		t.Fatalf("CopyFileAtomic = %v, %v", ran, err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "token" {
		t.Fatalf("dst = %q, %v", got, err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != FileMode {
		t.Fatalf("dst mode = %v, want %v — a credential must never be readable", info.Mode().Perm(), FileMode)
	}
}

func TestCopyFileAtomicLeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := CopyFileAtomic(src, filepath.Join(dir, "dst")); err != nil {
		t.Fatalf("CopyFileAtomic: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".crewlet-tmp-") {
			t.Fatalf("temp file %q survived the copy", entry.Name())
		}
	}
}

func TestCopyFileAtomicReportsAMissingSourceRatherThanFailing(t *testing.T) {
	dir := t.TempDir()
	ran, err := CopyFileAtomic(filepath.Join(dir, "absent"), filepath.Join(dir, "dst"))
	if err != nil {
		t.Fatalf("a profile naming a file this vendor does not use is not an error: %v", err)
	}
	if ran {
		t.Fatal("CopyFileAtomic reported a copy that could not have happened")
	}
}

func TestCopyFileAtomicRefusesADirectory(t *testing.T) {
	dir := t.TempDir()
	ran, err := CopyFileAtomic(dir, filepath.Join(dir, "dst"))
	if ran || err != nil {
		t.Fatalf("CopyFileAtomic(dir) = %v, %v; want false, nil", ran, err)
	}
}

func TestFileDigestSeparatesChangedFromUnchanged(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte("same"), FileMode); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(b, []byte("same"), FileMode); err != nil {
		t.Fatalf("write: %v", err)
	}
	if FileDigest(a) == "" || FileDigest(a) != FileDigest(b) {
		t.Fatalf("identical contents digested differently: %q vs %q", FileDigest(a), FileDigest(b))
	}
	if err := os.WriteFile(b, []byte("rotated"), FileMode); err != nil {
		t.Fatalf("write: %v", err)
	}
	if FileDigest(a) == FileDigest(b) {
		t.Fatal("a rotated credential digested the same as the old one")
	}
}

func TestFileDigestOfAnUnreadableFileIsEmpty(t *testing.T) {
	if got := FileDigest(filepath.Join(t.TempDir(), "absent")); got != "" {
		t.Fatalf("FileDigest(absent) = %q, want \"\"", got)
	}
}
