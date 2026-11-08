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

// newEngineHost lays the stand-in out OUTSIDE /tmp, under /var/tmp, which the
// FHS and macOS both have: bubblewrap gives a confined job a private /tmp, so
// a tree under the host's /tmp is hidden from the job WHOLE, whether or not a
// single denial holds, and a write into it lands in memory whether or not the
// host's root is bound read-only. Out here, each denial and the read-only
// root are what keep the job out, which is what a case has to hold.
func newEngineHost(t *testing.T) engineHost {
	t.Helper()
	root, err := os.MkdirTemp("/var/tmp", "crewlet-confine-") //nolint:usetesting // t.TempDir is under the host's /tmp, which bubblewrap hides whole
	if err != nil {
		t.Fatalf("this host cannot make the stand-in engine host outside /tmp, where a "+
			"confinement's denials are what keep a job out: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
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
//
// EACH REFUSAL IS JUDGED BY ITS EFFECT, NEVER BY AN EXIT STATUS. Landlock and
// sandbox-exec REFUSE a denied path, but bubblewrap MASKS it: a denied
// directory is an empty tmpfs, a denied file is /dev/null, and /tmp is a
// private tmpfs that takes a write in memory and loses it with the job. So a
// masked `ls` exits 0 having seen nothing, and a write into the private /tmp
// exits 0 having written nothing the host keeps; read by exit status, both
// were reported as reaching the engine on every host that runs bubblewrap.
// What holds under all three mechanisms is what reaches the job's stdout and
// what lands on the host, so a read fails when a byte or a real directory
// entry of what it was denied reaches the job, and a write when its target
// exists on the host. The stand-in engine host is laid out OUTSIDE /tmp
// ([newEngineHost]): under bubblewrap every path under /tmp is the private
// tmpfs, which would hide the whole tree from the job, its denials and a host
// root bound writable alike.
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
	run := func(cmd string) ExecResult {
		t.Helper()
		res, err := box.Exec(ctx, cmd, ExecOptions{TimeoutSec: 30})
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		return res
	}

	// EVERY WITNESS IS FIRST SHOWN TO BE THERE, read on the host: what a
	// file holds and what a directory lists, so a refusal is the boundary's
	// and never a typo's, and a case that leaked nothing had something to
	// leak.
	holding := func(path string) []string {
		t.Helper()
		body, err := os.ReadFile(path)
		if err != nil || len(body) == 0 {
			t.Fatalf("the fixture's %s holds nothing to leak (%v)", path, err)
		}
		return []string{string(body)}
	}
	entries := func(dir string) []string {
		t.Helper()
		listed, err := os.ReadDir(dir)
		if err != nil || len(listed) == 0 {
			t.Fatalf("the fixture's %s lists nothing to leak (%v)", dir, err)
		}
		names := make([]string, 0, len(listed))
		for _, entry := range listed {
			names = append(names, entry.Name())
		}
		return names
	}
	records := filepath.Join(filepath.Dir(box.Home()), "records")
	theirs := filepath.Join(other.Home(), "theirs.txt")
	boxes := filepath.Dir(filepath.Dir(box.Home()))
	otherID := filepath.Base(filepath.Dir(other.Home()))
	if !slices.Contains(entries(boxes), otherID) {
		t.Fatalf("the boxes' directory %s does not list the other box %s", boxes, otherID)
	}
	type deniedRead struct {
		what, cmd string
		leaked    []string
	}
	reads := []deniedRead{
		{"the stream store", "cat " + q(filepath.Join(h.data, "stream", "store.db")),
			holding(filepath.Join(h.data, "stream", "store.db"))},
		{"the data directory listing", "ls " + q(h.data), entries(h.data)},
		{"the keyring", "cat " + q(h.keyring), holding(h.keyring)},
		{"the config file", "cat " + q(h.config), holding(h.config)},
		{"the operator's home", "cat " + q(h.secret), holding(h.secret)},
		{"another box", "cat " + q(theirs), holding(theirs)},
		{"the box's own records", "ls " + q(records), entries(records)},
		// THIS BOX'S OWN ID IS NO LEAK: bubblewrap masks the boxes'
		// directory and then makes this box's home's path components
		// inside the mask to bind the home there, so a masked listing
		// shows this box and nothing beside it.
		{"the boxes' directory listing", "ls " + q(boxes), []string{otherID}},
	}
	if runtime.GOOS == "linux" {
		reads = append(reads, deniedRead{"the engine's environment",
			"cat /proc/" + strconv.Itoa(os.Getpid()) + "/environ", holding("/proc/self/environ")})
	}
	for _, read := range reads {
		res := run(read.cmd)
		for _, leaked := range read.leaked {
			if strings.Contains(res.Stdout, leaked) {
				t.Errorf("%s reached a confined box: %q printed %q", read.what, read.cmd, res.Stdout)
				break
			}
		}
	}

	hostTmpProbe := filepath.Join("/tmp", "crewlet-confine-probe-"+box.ID())
	t.Cleanup(func() { _ = os.Remove(hostTmpProbe) })
	// Beside the stand-in's paths, outside /tmp, where only the host's root
	// bound read-only refuses it.
	planted := filepath.Join(filepath.Dir(h.data), "planted")
	install := holding(h.cli)[0]
	exists := func(path string) func() bool {
		return func() bool {
			_, err := os.Lstat(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the host cannot tell whether %s exists: %v", path, err)
			}
			return err == nil
		}
	}
	type deniedWrite struct {
		what, cmd string
		landed    func() bool
	}
	writes := []deniedWrite{
		{"a write outside the box", "echo x > " + q(planted), exists(planted)},
		{"a write to the cli install", "echo x >> " + q(h.cli), func() bool {
			body, err := os.ReadFile(h.cli)
			if err != nil {
				t.Fatalf("the host cannot read the cli install back: %v", err)
			}
			return string(body) != install
		}},
		// The host's own temporary root, where a harness left at its default
		// would make its per-uid directory: refused, which is why every
		// temp root is pointed inside the box rather than this let through.
		{"a directory under the host's /tmp", "mkdir -p " + q(hostTmpProbe), exists(hostTmpProbe)},
	}
	for _, write := range writes {
		run(write.cmd)
		if write.landed() {
			t.Errorf("%s landed on the host from a confined box: %q", write.what, write.cmd)
		}
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
// the real harness under this host's confinement: one headless run asked to use
// its Bash tool. It needs a signed-in harness, so it runs only where
// CREWLET_CONFINED_CLAUDE_SMOKE=1 and CLAUDE_CODE_OAUTH_TOKEN are set, and
// otherwise logs what it did not check and passes. The temp roots the harness
// resolves are held by TestAConfinedHostBoxCannotReachTheEngine on every run.
func TestTheRealClaudeCodeRunsAShellCommandInAConfinedBox(t *testing.T) {
	token := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
	if os.Getenv("CREWLET_CONFINED_CLAUDE_SMOKE") != "1" || token == "" {
		t.Log("the real Claude Code smoke runs only with CREWLET_CONFINED_CLAUDE_SMOKE=1 " +
			"and CLAUDE_CODE_OAUTH_TOKEN set; the harness's Bash tool under confinement was not checked")
		return
	}
	cli, err := exec.LookPath("claude")
	if err != nil {
		t.Fatalf("the smoke was asked for, but no claude harness is on PATH: %v", err)
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
		t.Fatalf("the harness's Bash tool did not run in a confined box: exit %d\nstdout: %s\nstderr: %s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
}
