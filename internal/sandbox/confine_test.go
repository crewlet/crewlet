//go:build unix

package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// engineHost is a stand-in for an engine host's sensitive paths, in a
// temporary tree: what a confined box is kept from.
type engineHost struct {
	data, keyring, config, home, secret, cli string
}

func newEngineHost(t *testing.T) engineHost {
	t.Helper()
	root := t.TempDir()
	h := engineHost{
		data:    filepath.Join(root, "data"),
		keyring: filepath.Join(root, "keyring.json"),
		config:  filepath.Join(root, "crewlet.yaml"),
		home:    filepath.Join(root, "operator"),
	}
	h.secret = filepath.Join(h.home, ".ssh", "id_ed25519")
	h.cli = filepath.Join(h.home, ".local", "bin", "coding-cli")
	for _, f := range []struct{ path, body string }{
		{filepath.Join(h.data, "stream", "store.db"), "stream store"},
		{filepath.Join(h.data, "node.db"), "node estate"},
		{h.keyring, "fleet keyring"},
		{h.config, "tier a token"},
		{h.secret, "operator key"},
		{h.cli, "#!/bin/sh\necho cli-ran\n"},
	} {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
			t.Fatal(err)
		}
		if f.path == h.cli {
			// THE ONE FILE A JOB RUNS, written as every script a case of
			// this package execs is ([writeExecutable]).
			writeExecutable(t, f.path, f.body)
			continue
		}
		if err := os.WriteFile(f.path, []byte(f.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// TestAConfinedHostBoxCannotReachTheEngine runs real jobs under this host's
// strongest confinement and holds every path the engine keeps from a person's
// deep-work run: its data directory and stream store, its keyring, its
// config, the operator's home, another box and the box's own records. A host
// that confines nothing must say so rather than run anything.
func TestAConfinedHostBoxCannotReachTheEngine(t *testing.T) {
	host, err := ProbeConfinement()
	if err != nil {
		if runtime.GOOS == "darwin" {
			t.Fatalf("macOS ships sandbox-exec, yet the probe refused it: %v", err)
		}
		if !errors.Is(err, ErrConfinementUnavailable) {
			t.Fatalf("an unconfinable host must answer ErrConfinementUnavailable, got %v", err)
		}
		_, refused := NewLocal(LocalOptions{Placement: Direct, StateDir: t.TempDir(),
			Confine: &Confinement{}})
		if refused == nil {
			t.Fatal("a confined box with no mechanism to enforce it was accepted")
		}
		t.Logf("this host confines nothing (%v); the refusal is what was held", err)
		return
	}
	h := newEngineHost(t)
	local, err := NewLocal(LocalOptions{
		Placement: Direct,
		// OUTSIDE the operator's home, so the boxes' own denial is what
		// keeps another box and the records out, not the home's.
		StateDir: filepath.Join(filepath.Dir(h.data), "boxes"),
		Confine: &Confinement{
			Deny: []string{h.data, h.keyring, h.config, h.home},
			Read: []string{filepath.Dir(h.cli)},
		},
		Host: host,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	box, err := local.Create(ctx, Spec{Placement: Direct})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = box.Close(context.WithoutCancel(ctx)) })
	other, err := local.Create(ctx, Spec{Placement: Direct})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close(context.WithoutCancel(ctx)) })
	if err := other.WriteFile(ctx, "theirs.txt", []byte("another person's work")); err != nil {
		t.Fatal(err)
	}
	records := filepath.Join(filepath.Dir(box.Home()), "records")
	// Each refused path is first shown to exist, so a refusal is the
	// boundary's and never a typo's.
	theirs := filepath.Join(other.Home(), "theirs.txt")
	for _, p := range []string{theirs, records, h.keyring, h.config, h.secret, h.data} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("the fixture is missing %s: %v", p, err)
		}
	}

	hostTmpProbe := filepath.Join("/tmp", "crewlet-confine-probe-"+filepath.Base(filepath.Dir(box.Home())))
	t.Cleanup(func() { _ = os.Remove(hostTmpProbe) })
	run := func(cmd string) ExecResult {
		t.Helper()
		res, err := box.Exec(ctx, cmd, ExecOptions{TimeoutSec: 30})
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		return res
	}
	refused := map[string]string{
		"the stream store":             "cat " + q(filepath.Join(h.data, "stream", "store.db")),
		"the data directory listing":   "ls " + q(h.data),
		"the keyring":                  "cat " + q(h.keyring),
		"the config file":              "cat " + q(h.config),
		"the operator's home":          "cat " + q(h.secret),
		"another box":                  "cat " + q(theirs),
		"the box's own records":        "ls " + q(records),
		"the boxes' directory listing": "ls " + q(filepath.Dir(filepath.Dir(box.Home()))),
		"a write outside the box":      "echo x > " + q(filepath.Join(filepath.Dir(h.data), "planted")),
		"a write to the cli install":   "echo x >> " + q(h.cli),
		// The host's own temporary root, where a CLI left at its default
		// would make its per-uid directory: refused, which is why every
		// temp root is pointed inside the box rather than this let through.
		"a directory under the host's /tmp": "mkdir -p " + q(hostTmpProbe),
	}
	if runtime.GOOS == "linux" {
		refused["the engine's environment"] = "cat /proc/" + strconv.Itoa(os.Getpid()) + "/environ"
	}
	for what, cmd := range refused {
		if res := run(cmd); res.ExitCode == 0 {
			t.Errorf("%s was reachable from a confined box: %q answered %q", what, cmd, res.Stdout)
		}
	}
	if _, err := os.Stat(hostTmpProbe); err == nil {
		t.Errorf("a confined job created %s on the host", hostTmpProbe)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.data), "planted")); err == nil {
		t.Error("a confined job wrote outside its box")
	}
	if got, _ := os.ReadFile(h.cli); strings.Contains(string(got), "x") && !strings.Contains(string(got), "cli-ran") {
		t.Error("a confined job changed the CLI's install")
	}

	allowed := map[string]string{
		"its own home":    "echo kept > \"$HOME/note\" && cat \"$HOME/note\"",
		"the cli install": q(h.cli),
		"the system":      "ls /usr/bin >/dev/null && echo system",
		// An absolute path into its own home, made a component at a time.
		"a nested directory by its absolute path": `mkdir -p "$HOME/out/deep" && test -d "$HOME/out/deep"`,
		// What Claude Code's Bash tool does before every command: its per-uid
		// directory under CLAUDE_CODE_TMPDIR, then a script's own temp file.
		"its own temp roots": `mkdir -p "$CLAUDE_CODE_TMPDIR/claude-$(id -u)/project" && ` +
			`f=$(mktemp -p "$TMPDIR") && echo ok > "$f" && cat "$f" && ` +
			`python3 -I -c 'import tempfile; tempfile.mkstemp()' && ` +
			`test -d "$TMP" && test -d "$TEMP"`,
	}
	for what, cmd := range allowed {
		if res := run(cmd); res.ExitCode != 0 {
			t.Errorf("%s was refused to a confined box: %q exited %d: %s", what, cmd, res.ExitCode, res.Stderr)
		}
	}
}

func q(p string) string { return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'" }

// TestTheProfileGivesBackOnlyTheHomeAndTheInstall pins the sandbox profile's
// order: the last matching rule wins, so the home and the install must be
// given back after every denial, and every write is denied before them.
func TestTheProfileGivesBackOnlyTheHomeAndTheInstall(t *testing.T) {
	p := sandboxProfile(confineRules{deny: []string{"/srv/data", "/home/op"},
		read: []string{"/home/op/.local/bin"}, write: "/home/op/.crewlet/sandboxes/boxes/a/home"})
	order := []string{
		`(deny file-read* file-write* (subpath "/srv/data") (subpath "/home/op"))`,
		`(deny file-write* (subpath "/"))`,
		`(deny process-info* (target others))`,
		`(allow file-read* (subpath "/home/op/.local/bin"))`,
		`(allow file-read-metadata (literal "/") (literal "/home") (literal "/home/op") ` +
			`(literal "/home/op/.crewlet") (literal "/home/op/.crewlet/sandboxes") ` +
			`(literal "/home/op/.crewlet/sandboxes/boxes") (literal "/home/op/.crewlet/sandboxes/boxes/a"))`,
		`(allow file-read* file-write* (subpath "/home/op/.crewlet/sandboxes/boxes/a/home"))`,
	}
	at := -1
	for _, rule := range order {
		i := strings.Index(p, rule)
		if i <= at {
			t.Fatalf("rule %q is missing or out of order in:\n%s", rule, p)
		}
		at = i
	}
}

// TestCarvingLeavesOutOnlyTheDeniedTrees holds Landlock's allowlist to the
// host minus the denied trees: every sibling kept whole, every denied path
// and nothing else left out.
func TestCarvingLeavesOutOnlyTheDeniedTrees(t *testing.T) {
	tree := map[string][]string{
		"/":                {"bin", "home", "usr", "var"},
		"/home":            {"op", "other"},
		"/var":             {"lib", "log"},
		"/var/lib":         {"crewlet", "dpkg"},
		"/home/op":         {"x"},
		"/var/lib/crewlet": {"data"},
	}
	readDir := func(dir string) ([]os.DirEntry, error) {
		var out []os.DirEntry
		for _, name := range tree[dir] {
			out = append(out, fakeEntry(name))
		}
		return out, nil
	}
	got := carve("/", []string{"/home/op", "/var/lib/crewlet"}, readDir)
	slices.Sort(got)
	want := []string{"/bin", "/home/other", "/usr", "/var/lib/dpkg", "/var/log"}
	if !slices.Equal(got, want) {
		t.Fatalf("carve = %v, want %v", got, want)
	}
}

type fakeEntry string

func (e fakeEntry) Name() string               { return string(e) }
func (e fakeEntry) IsDir() bool                { return true }
func (e fakeEntry) Type() os.FileMode          { return os.ModeDir }
func (e fakeEntry) Info() (os.FileInfo, error) { return nil, os.ErrNotExist }

// TestAConfinementWithoutAMechanismIsRefused holds the construction rule: a
// confined box that nothing enforces is refused, never built open.
func TestAConfinementWithoutAMechanismIsRefused(t *testing.T) {
	if _, err := NewLocal(LocalOptions{Placement: Direct, StateDir: t.TempDir(),
		Confine: &Confinement{Deny: []string{"/srv"}}}); err == nil {
		t.Fatal("a confinement with no mechanism was accepted")
	}
	if _, err := NewLocal(LocalOptions{Placement: Container, Image: "x", StateDir: t.TempDir(),
		Confine: &Confinement{}, Host: &HostConfinement{Mechanism: MechanismSandboxExec}}); err == nil {
		t.Fatal("a confined container box was accepted")
	}
}

// TestTheRealClaudeCodeRunsAShellCommandInAConfinedBox is the opt-in smoke for
// the real CLI under this host's confinement: one headless run asked to use
// its Bash tool. It needs a signed-in CLI, so it runs only where
// CREWLET_CONFINED_CLAUDE_SMOKE=1 and CLAUDE_CODE_OAUTH_TOKEN are set, and
// otherwise logs what it did not check and passes. The temp roots the CLI
// resolves are held by TestAConfinedHostBoxCannotReachTheEngine on every run.
func TestTheRealClaudeCodeRunsAShellCommandInAConfinedBox(t *testing.T) {
	token := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
	if os.Getenv("CREWLET_CONFINED_CLAUDE_SMOKE") != "1" || token == "" {
		t.Log("the real Claude Code smoke runs only with CREWLET_CONFINED_CLAUDE_SMOKE=1 " +
			"and CLAUDE_CODE_OAUTH_TOKEN set; the CLI's Bash tool under confinement was not checked")
		return
	}
	cli, err := exec.LookPath("claude")
	if err != nil {
		t.Fatalf("the smoke was asked for, but no claude CLI is on PATH: %v", err)
	}
	if cli, err = filepath.EvalSymlinks(cli); err != nil {
		t.Fatal(err)
	}
	host, err := ProbeConfinement()
	if err != nil {
		t.Fatalf("the smoke was asked for on a host that confines nothing: %v", err)
	}
	local, err := NewLocal(LocalOptions{Placement: Direct, StateDir: t.TempDir(),
		Confine: &Confinement{Read: []string{filepath.Dir(cli)}}, Host: host})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	box, err := local.Create(ctx, Spec{Placement: Direct,
		Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": token}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = box.Close(context.WithoutCancel(ctx)) })
	res, err := box.Exec(ctx, q(cli)+" -p "+q("Use your Bash tool to run exactly this "+
		"command: echo confined-$((20+22)). Then reply with its output and nothing else.")+
		" --permission-mode bypassPermissions --output-format json", ExecOptions{TimeoutSec: 240})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "confined-42") ||
		strings.Contains(res.Stdout, "EPERM") {
		t.Fatalf("the CLI's Bash tool did not run in a confined box: exit %d\nstdout: %s\nstderr: %s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
}
