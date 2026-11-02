package hostbox

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// resolvedTempDir is a fresh temp directory under its RESOLVED name.
//
// SafeJoin returns the path it checked, which is the resolved one, so a test
// comparing against the name t.TempDir hands out compares against a different
// string wherever the temp directory runs through a symlink. macOS is the
// ordinary case: $TMPDIR lives under /var, which is a link to /private/var.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temp directory: %v", err)
	}
	return dir
}

func TestSafeJoinAcceptsPathsInsideTheRoot(t *testing.T) {
	root := resolvedTempDir(t)
	for rel, want := range map[string]string{
		".claude/.credentials.json": ".claude/.credentials.json",
		"workspace/repo/file.txt":   "workspace/repo/file.txt",
		"a/b/../c":                  "a/c",
		"./nested/file":             "nested/file",
	} {
		got, err := SafeJoin(root, rel)
		if err != nil {
			t.Fatalf("SafeJoin(%q): %v", rel, err)
		}
		if want := filepath.Join(root, filepath.FromSlash(want)); got != want {
			t.Fatalf("SafeJoin(%q) = %q, want %q", rel, got, want)
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
	root := resolvedTempDir(t)
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

// WHERE A PATH LANDS IN A LOCAL BOX is one table, because both local cells and
// config validation read it: a case that moved here moves all three at once.
func TestInHomeIsWhereALocalBoxPutsAFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, home, path string
		rel              string // empty: refused
	}{
		{"an absolute path under the home", ContainerHome, "/home/user/.npmrc", ".npmrc"},
		{"deeper under the home", ContainerHome, "/home/user/.local/bin/tool", ".local/bin/tool"},
		{"a doubled separator is still under it", ContainerHome, "/home/user//x", "x"},
		{"a climb that comes back", ContainerHome, "/home/user/../user/x", "x"},
		{"a name that starts with two dots", ContainerHome, "/home/user/..x", "..x"},
		{"a relative path, under any home", ContainerHome, ".config/tool.toml", ".config/tool.toml"},
		{"a relative path that comes back", ContainerHome, "a/../b", "b"},
		{"a system path", ContainerHome, "/usr/local/bin/tool", ""},
		{"a climb out of the home", ContainerHome, "/home/user/../../etc/cron.d/x", ""},
		{"a neighbour sharing the prefix", ContainerHome, "/home/user-2/x", ""},
		{"the home itself, which is no file", ContainerHome, "/home/user", ""},
		{"the home itself with a slash", ContainerHome, "/home/user/", ""},
		{"a relative climb out", ContainerHome, "../escape", ""},
		{"a relative climb that ends at the parent", ContainerHome, "a/../..", ""},
		{"a relative path to the home itself", ContainerHome, ".", ""},
		{"nothing at all", ContainerHome, "", ""},
		// A direct box's home is minted per box, so to a document no
		// absolute path names it, and only a relative path lands.
		{"a minted home and an absolute path", "", "/home/user/.npmrc", ""},
		{"a minted home and a relative path", "", ".npmrc", ".npmrc"},
		{"a minted home and a relative climb out", "", "../x", ""},
		// A leading tilde is the home, in every cell, as a remote box's
		// own agent reads one; another account's home is refused there and
		// so here.
		{"a tilde path", ContainerHome, "~/.npmrc", ".npmrc"},
		{"a tilde path in a minted home", "", "~/.local/bin/tool", ".local/bin/tool"},
		{"the tilde alone, which is the home itself", "", "~", ""},
		{"the tilde and a slash, the home again", ContainerHome, "~/", ""},
		{"a climb out from the tilde", "", "~/../x", ""},
		{"another account's home", ContainerHome, "~bob/.npmrc", ""},
		{"a name that only starts with a tilde", "", "~x", ""},
		{"a tilde that is not the first character", "", "a/~/x", "a/~/x"},
		// The direct box itself holds its home, and so can name it.
		{"a held host home", "/var/lib/crewlet/boxes/ab12/home", "/var/lib/crewlet/boxes/ab12/home/x", "x"},
		{"a neighbouring box's home", "/var/lib/crewlet/boxes/ab12/home", "/var/lib/crewlet/boxes/cd34/home/x", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rel, inside := InHome(tc.home, tc.path)
			if want := tc.rel != ""; inside != want {
				t.Fatalf("InHome(%q, %q) inside = %v, want %v", tc.home, tc.path, inside, want)
			}
			if inside && rel != tc.rel {
				t.Fatalf("InHome(%q, %q) = %q, want %q", tc.home, tc.path, rel, tc.rel)
			}
			if !inside && rel != "" {
				t.Fatalf("InHome(%q, %q) refused but answered %q", tc.home, tc.path, rel)
			}
		})
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

// A BOX REPLACES WHERE ITS CHILD KEEPS ITS FILES, so the base allowlist must
// not carry the engine user's own: a box consumer that inherited HOME and then
// forgot to overwrite one XDG directory would have a coding agent writing its
// login into the engine user's real dotfiles. The host user's locations are a
// separate list a child that runs in no box asks for by name.
func TestTheBaseAllowlistCarriesNoneOfTheHostUsersLocations(t *testing.T) {
	for _, name := range HostUserEnv {
		if slices.Contains(PassthroughEnv, name) {
			t.Errorf("%s is on the base allowlist, so every box inherits the "+
				"engine user's own before overwriting it", name)
		}
	}
	t.Setenv("HOME", "/home/engine")
	if _, ok := Inherit()["HOME"]; ok {
		t.Error("Inherit() passed the engine's HOME to a child that did not ask for it")
	}
	if got := Inherit(HostUserEnv...)["HOME"]; got != "/home/engine" {
		t.Errorf("Inherit(HostUserEnv...) HOME = %q, want the engine user's", got)
	}
}

// A FAMILY IS STILL AN ALLOWLIST: every member of the family arrives, and a
// name outside it — the engine's keyring above all — does not, however much it
// resembles one.
func TestInheritFamiliesTakesTheFamilyAndNothingElse(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///run/user/1000/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "rootless")
	t.Setenv("DOCKERFILE_PATH", "/not/a/runtime/setting")
	t.Setenv("CREWLET_SECRET_KEY_2026_01", "base64-keyring-material-not-for-the-child")

	env := map[string]string{}
	InheritFamilies(env, "DOCKER_")
	if env["DOCKER_HOST"] != "unix:///run/user/1000/docker.sock" || env["DOCKER_CONTEXT"] != "rootless" {
		t.Errorf("the family did not arrive whole: %v", env)
	}
	for _, name := range []string{"DOCKERFILE_PATH", "CREWLET_SECRET_KEY_2026_01"} {
		if _, ok := env[name]; ok {
			t.Errorf("%s is outside the DOCKER_ family and reached the child", name)
		}
	}
}

// A CONTAINER RUNTIME'S CLI GETS WHAT IT DIALS THE DAEMON WITH, and nothing of
// the engine's: the runtime's own families and named settings, the host user's
// locations, and not the keyring — nor a credential a helper might want, which
// is the caller's declared channel to hand over and never this rule's.
func TestContainerRuntimeTakesTheRuntimesSettingsAndNothingOfTheEngines(t *testing.T) {
	for name, value := range map[string]string{
		"DOCKER_HOST":                "unix:///run/user/1000/docker.sock",
		"CONTAINERS_CONF":            "/etc/containers/containers.conf",
		"PODMAN_CONNECTIONS_CONF":    "/home/engine/.config/podman.json",
		"REGISTRY_AUTH_FILE":         "/home/engine/.config/auth.json",
		"XDG_RUNTIME_DIR":            "/run/user/1000",
		"HOME":                       "/home/engine",
		"CREWLET_SECRET_KEY_2026_01": "base64-keyring-material-not-for-the-runtime",
		"SSH_AUTH_SOCK":              "/tmp/ssh-agent.sock",
		"AWS_PROFILE":                "production",
	} {
		t.Setenv(name, value)
	}
	env := ContainerRuntime()
	for _, name := range []string{
		"DOCKER_HOST", "CONTAINERS_CONF", "PODMAN_CONNECTIONS_CONF",
		"REGISTRY_AUTH_FILE", "XDG_RUNTIME_DIR", "HOME",
	} {
		if env[name] != os.Getenv(name) {
			t.Errorf("%s = %q, want the host's %q: the runtime needs it to reach "+
				"the daemon the operator configured", name, env[name], os.Getenv(name))
		}
	}
	for _, name := range []string{"CREWLET_SECRET_KEY_2026_01", "SSH_AUTH_SOCK", "AWS_PROFILE"} {
		if _, ok := env[name]; ok {
			t.Errorf("%s reached the runtime without being declared", name)
		}
	}
}

// WHAT A CHECK IS TOLD THE RUNTIME CARRIES IS WHAT IT CARRIES. A `-e NAME` in a
// container's run arguments is judged by [ContainerRuntimeCarries] and copied
// by [ContainerRuntime], so the two answer the same question from two ends and
// are held together here over every kind of name: allowlisted, the host user's,
// named, a family member, a family look-alike, and a credential.
func TestContainerRuntimeCarriesIsWhatContainerRuntimePasses(t *testing.T) {
	names := []string{
		"PATH", "HTTPS_PROXY", "HOME", "XDG_RUNTIME_DIR", "REGISTRY_AUTH_FILE",
		"DBUS_SESSION_BUS_ADDRESS", "DOCKER_HOST", "DOCKER_CONFIG", "CONTAINER_HOST",
		"CONTAINERS_STORAGE_CONF", "PODMAN_USERNS", "DOCKERFILE_PATH",
		"SSH_AUTH_SOCK", "AWS_ROLE_ARN", "CREWLET_SECRET_KEY_2026_01",
	}
	for _, name := range names {
		t.Setenv(name, "set-by-the-test")
	}
	env := ContainerRuntime()
	for _, name := range names {
		_, passed := env[name]
		if carried := ContainerRuntimeCarries(name); carried != passed {
			t.Errorf("ContainerRuntimeCarries(%q) = %v, but ContainerRuntime passes it: %v",
				name, carried, passed)
		}
	}
}

// Every runtime this engine drives, by name or by path, and nothing that
// merely resembles one.
func TestIsContainerRuntimeNamesTheRuntimesCLI(t *testing.T) {
	for command, want := range map[string]bool{
		"docker":             true,
		"podman":             true,
		"/usr/bin/docker":    true,
		"/opt/bin/podman":    true,
		"npx":                false,
		"uvx":                false,
		"docker-compose":     false,
		"/usr/bin/dockerd":   false,
		"/usr/local/bin/sh":  false,
		"":                   false,
		"docker/credentials": false,
	} {
		if got := IsContainerRuntime(command); got != want {
			t.Errorf("IsContainerRuntime(%q) = %v, want %v", command, got, want)
		}
	}
}

// A MISSING ENVIRONMENT IS NOT AN EMPTY ONE. os/exec reads a nil Cmd.Env as
// "inherit the parent's", so an empty map rendered as nil would hand a child
// the whole engine environment — the exact opposite of the empty one it asked
// for.
func TestEnvironIsNeverNilAndSorted(t *testing.T) {
	if got := Environ(nil); got == nil || len(got) != 0 {
		t.Fatalf("Environ(nil) = %#v, want an empty non-nil slice", got)
	}
	if got := Environ(map[string]string{}); got == nil {
		t.Fatal("Environ of an empty map is nil, which os/exec reads as the engine's own")
	}
	got := Environ(map[string]string{"B": "2", "A": "1", "C": ""})
	if want := []string{"A=1", "B=2", "C="}; !slices.Equal(got, want) {
		t.Fatalf("Environ = %v, want %v", got, want)
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

func TestReplaceFileWritesTheContentAtFileMode(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst")
	if err := ReplaceFile(dst, strings.NewReader("token")); err != nil {
		t.Fatalf("ReplaceFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "token" {
		t.Fatalf("dst = %q, %v", got, err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// UNCONDITIONAL, and it has to stay that way. This was guarded by
	// `runtime.GOOS != "windows"`, which never held: internal/store refuses to
	// compile off the four supported platforms (see its platform.go), so there
	// is no Windows build of this module for the branch to be false in, and no
	// Windows host `go test ./...` can run on. A carve-out that cannot fire is
	// not portability — it is an assertion about a credential's mode that
	// reads as conditional and is not.
	if info.Mode().Perm() != FileMode {
		t.Fatalf("dst mode = %v, want %v — a credential must never be readable", info.Mode().Perm(), FileMode)
	}
}

func TestReplaceFileLeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	if err := ReplaceFile(filepath.Join(dir, "dst"), strings.NewReader("x")); err != nil {
		t.Fatalf("ReplaceFile: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".crewlet-tmp-") {
			t.Fatalf("temp file %q survived the replacement", entry.Name())
		}
	}
}

// A REPLACEMENT THAT FAILS LEAVES NOTHING STAGED. The content is written to a
// file beside the destination before the rename that puts it in place, and a
// rename refused — a directory at the destination, say — must not leave that
// file behind: a box's home would hold a credential under a name nothing reads,
// and a shared login's directory a file no reader expects. The destination is
// left as it was.
//
// Mutation: drop the removal of the staged file, and it survives the refusal.
func TestReplaceFileInLeavesNothingStagedWhenItsRenameFails(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if err := os.MkdirAll(filepath.Join(dir, "dst", "inside"), DirMode); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFileIn(root, "dst", strings.NewReader("token")); err == nil {
		t.Fatal("a replacement over a directory succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != "dst" {
			t.Errorf("%q survived the failed replacement", entry.Name())
		}
	}
	if info, err := os.Lstat(filepath.Join(dir, "dst", "inside")); err != nil || !info.IsDir() {
		t.Errorf("the destination is now %v, %v; want it as it was", info, err)
	}
}

// THE PUBLISHED TABLE OF WHAT A CHILD SEES IS THESE LISTS, NAME FOR NAME.
//
// docs/guides/tools-and-mcp.md tells an operator exactly which host variables
// a tool server receives, so they know which ones to declare — and it is a
// copy of [PassthroughEnv], [HostUserEnv] and the container runtime's rule,
// written in another file in another language. Copies drift: the CLI backend's
// own copy of the allowlist had already disagreed with this one by five names.
// So each row's names are read out of the page and held to the Go list in BOTH
// directions — a name added here and not there, or published and never passed.
func TestThePublishedChildEnvironmentIsHostboxsOwn(t *testing.T) {
	t.Parallel()
	page := filepath.Join(sourcetree.Root(t), "docs", "guides", "tools-and-mcp.md")
	raw, err := os.ReadFile(page)
	if err != nil {
		t.Fatalf("read %s: %v", page, err)
	}
	names := func(row string) []string {
		t.Helper()
		for line := range strings.SplitSeq(string(raw), "\n") {
			cells := strings.Split(line, "|")
			if len(cells) < 4 || !strings.HasPrefix(strings.TrimSpace(cells[1]), row) {
				continue
			}
			var out []string
			for _, m := range backticked.FindAllStringSubmatch(cells[2], -1) {
				out = append(out, m[1])
			}
			slices.Sort(out)
			return out
		}
		t.Fatalf("%s has no row starting %q: the table this gate reads was renamed "+
			"or removed, so nothing holds what it publishes", page, row)
		return nil
	}
	sorted := func(in []string) []string {
		out := slices.Clone(in)
		slices.Sort(out)
		return out
	}

	if got, want := names("The host allowlist"), sorted(PassthroughEnv); !slices.Equal(got, want) {
		t.Errorf("the published host allowlist is %v, and PassthroughEnv is %v", got, want)
	}
	if got, want := names("The engine user's locations"), sorted(HostUserEnv); !slices.Equal(got, want) {
		t.Errorf("the published host user's locations are %v, and HostUserEnv is %v", got, want)
	}
	var named, families []string
	for _, name := range names("A container runtime's own settings") {
		if family, ok := strings.CutSuffix(name, "*"); ok {
			families = append(families, family)
			continue
		}
		named = append(named, name)
	}
	if want := sorted(ContainerRuntimeEnv); !slices.Equal(named, want) {
		t.Errorf("the published runtime settings by name are %v, and ContainerRuntimeEnv is %v",
			named, want)
	}
	if want := sorted(ContainerRuntimeFamilies); !slices.Equal(sorted(families), want) {
		t.Errorf("the published runtime families are %v, and ContainerRuntimeFamilies is %v",
			families, want)
	}
}

var backticked = regexp.MustCompile("`([^`]+)`")
