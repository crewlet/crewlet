// Package gittest is THE CLIENT GIT EVERY TEST DRIVES, the stand-in gits a
// test swaps for the real one, and repositories shaped the way the engine
// shapes them. It and gitproc are the only packages of the module outside
// cmd/ that may start git (gitproc's TestOnlyGitprocExecsGit).
//
// HERMETIC because a developer's own configuration — a commit.gpgSign, a
// url.<base>.insteadOf, a credential helper, an init.defaultBranch —
// otherwise decides what a suite builds: every client runs with a HOME of its
// own, no system configuration, and a fixed identity and clock, so a fixture
// is the same bytes on every machine and every run, and an object id a test
// asserts is stable.
//
// Git is a PREREQUISITE of the suite, not a property of the machine: [Require]
// fails a test on a machine without git 2.40 or newer and never skips it,
// because a skip there would be a pass the suite never earned.
//
// A client trusts one repository a test did not make: the checkout this
// module was compiled from, for a test whose subject is the repository itself
// ([Client.ModuleCheckout]) — whoever owns it, because the developer's own
// safe.directory is global configuration and never reaches a client.
package gittest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// ClientDeadline bounds one client invocation: a fixture's commit or a clone
// of a test-sized repository is milliseconds, and a hung git otherwise holds
// the suite until go test's ten-minute timeout.
const ClientDeadline = 2 * time.Minute

// waitDelay is how long a client invocation's pipes are given after it is
// cancelled. SAFE HERE, unlike in gitproc's pool: a client's stdout and stderr
// are bytes.Buffers whose Write never blocks, so the timer can only ever fire
// on a pipe a straggler holds, which is the one case it exists for.
const waitDelay = 5 * time.Second

// clientDate is the author and committer date of every commit a client
// makes, so a fixture's ids are the same on every run.
const clientDate = "2026-01-01T00:00:00Z"

// probe is the argument a stand-in answers by exiting at once, which is how
// writeScript execs one without running its body or leaving a call record.
const probe = "--gittest-probe"

// found is the git on PATH and its version, read once per test binary.
var found = sync.OnceValues(func() (string, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("no git on PATH (%w)", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ClientDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "version")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=/dev/null/gittest-home",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"LC_ALL=C",
	}
	out, err := cmd.Output()
	if err != nil {
		return git, fmt.Errorf("git at %s did not answer `git version` (%w)", git, err)
	}
	v, err := gitproc.ParseVersion(string(out))
	if err != nil {
		return git, fmt.Errorf("git at %s (%w)", git, err)
	}
	if !v.AtLeast(gitproc.MinVersion) {
		return git, fmt.Errorf("git %s at %s", v, git)
	}
	return git, nil
})

// Require is the git every test drives: exec.LookPath("git"), at least
// gitproc.MinVersion, read once per test binary. Absent, unreadable or older,
// it FAILS t — never skips.
func Require(t testing.TB) string {
	t.Helper()
	git, err := found()
	if err != nil {
		t.Fatalf("%v cannot run this suite: the code host's tests drive real git, and need git %s or newer on PATH "+
			"(CONTRIBUTING.md, Development setup)", err, gitproc.MinVersion)
	}
	return git
}

// Client is a person's or a fixture's git.
type Client struct {
	git  string
	home string
	env  []string
	// opts are git's own options, given before every subcommand: the one
	// path a client trusts whoever owns it ([Client.ModuleCheckout]).
	opts []string
}

// clientConfig is every client's whole configuration. gc and maintenance are
// off because an automatic run detaches a background process that outlives
// the command and races t.TempDir's removal.
const clientConfig = `[user]
	name = Crewlet Test
	email = test@example.com
[init]
	defaultBranch = main
[advice]
	detachedHead = false
[protocol]
	version = 2
[gc]
	auto = 0
[maintenance]
	auto = false
`

// NewClient is a client over the git Require finds, with a HOME of its own
// in a t.TempDir and nothing else from the test's environment.
func NewClient(t testing.TB) *Client {
	t.Helper()
	git := Require(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(clientConfig), hostbox.FileMode); err != nil {
		t.Fatalf("writing the client's configuration: %v", err)
	}
	return &Client{git: git, home: home, env: []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
		"GIT_AUTHOR_DATE=" + clientDate,
		"GIT_COMMITTER_DATE=" + clientDate,
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(home),
	}}
}

// Env is a copy of c with extra "NAME=value" variables (GIT_PROTOCOL, a
// credential helper's, GIT_TRACE_PACKET while debugging a case).
func (c *Client) Env(kv ...string) *Client {
	return &Client{git: c.git, home: c.home, env: slices.Concat(c.env, kv), opts: c.opts}
}

// ModuleCheckout is c trusting the checkout this module's tests were compiled
// from, and that checkout's top level — for a test whose SUBJECT is this
// repository (what its attributes make of a path), never for a fixture, which
// a test makes and owns.
//
// TRUSTED ON THE COMMAND LINE, FOR THAT PATH ALONE. Git refuses a repository
// whose owner is not the process's unless safe.directory names it, and
// honours that key only from protected configuration — the system's, the
// global and the command line's — of which a client reads only the last. So a
// checkout another uid owns (a container running as root over a bind mount, a
// devcontainer whose uid is not the files') is refused as dubious ownership,
// though the developer trusts it in their own ~/.gitconfig, which go build's
// VCS stamping already needs there. What a client keeps out of a test is the
// machine's say in what git ANSWERS; whether to read the tree go test is
// already running from is not that, so the client trusts it itself — and
// nothing else, so a fixture beside it is refused as before.
//
// The path is [sourcetree.Root], the module root with its links resolved,
// because git compares it byte for byte with the top level it resolves (a
// trailing slash, or a link's spelling, is refused); and git is asked to
// confirm that the module root IS the checkout's top level, so a path a test
// hands check-attr relative to the module is resolved against the tree it
// names.
//
// It FAILS RATHER THAN SKIPS, as [Require] does and as
// scripts/check-signoff.sh does outside a repository: a test whose subject is
// this checkout has not passed when it cannot read it, and a skip would be one
// internal/skipgate refuses anyway, with the coverage living nowhere else. An
// ownership refusal is told apart from a tree that is no checkout, because
// the remedies differ.
func (c *Client) ModuleCheckout(t testing.TB) (*Client, string) {
	t.Helper()
	root := sourcetree.Root(t)
	trusted := c.trusting(root)
	out, stderr, err := trusted.Try(t, root, nil, "rev-parse", "--show-toplevel")
	switch {
	case err != nil && strings.Contains(stderr, "dubious ownership"):
		t.Fatalf("git refuses the checkout at %s as another user's although this client trusts that "+
			"path: git names its top level by another path, and only the module root is trusted "+
			"(the module root must be the checkout's top level, links resolved): %v\n%s", root, err, stderr)
	case err != nil:
		t.Fatalf("%s is not a git working tree, so a test whose subject is this checkout cannot "+
			"read it: %v\n%s", root, err, stderr)
	}
	if top := strings.TrimSpace(out); top != root {
		t.Fatalf("the checkout's top level is %s, not the module root %s, so a path relative to "+
			"the module would be resolved against another tree", top, root)
	}
	return trusted, root
}

// trusting is c with the repository whose top level is dir trusted whoever
// owns it (safe.directory, given on the command line).
func (c *Client) trusting(dir string) *Client {
	return &Client{git: c.git, home: c.home, env: c.env, opts: slices.Concat(c.opts, []string{"-c", "safe.directory=" + dir})}
}

// Run runs git in dir ("" is the client's HOME, which is no repository) and
// returns its stdout; a non-zero exit fails t with the command and its stderr.
func (c *Client) Run(t testing.TB, dir string, args ...string) string {
	t.Helper()
	stdout, stderr, err := c.Try(t, dir, nil, args...)
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, stderr)
	}
	return stdout
}

// Try is Run for a command expected to fail (a refused push): stdin may be
// nil; err is the *exec.ExitError (or the deadline's) and never fails t.
func (c *Client) Try(t testing.TB, dir string, stdin io.Reader, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), ClientDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.git, slices.Concat(c.opts, args)...)
	procgroup.Set(cmd)
	cmd.Cancel = func() error { return procgroup.Terminate(cmd.Process.Pid) }
	cmd.WaitDelay = waitDelay
	cmd.Env = c.env
	cmd.Dir = dir
	if dir == "" {
		cmd.Dir = c.home
	}
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.String(), errb.String(), err
}

// Init is a fresh work tree on main in a t.TempDir().
func (c *Client) Init(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	c.Run(t, dir, "init", "-q")
	return dir
}

// Commit writes files (relative path → content, directories created), stages
// them all and commits — an empty commit when files is empty — and returns
// the new commit's id.
func (c *Client) Commit(t testing.TB, dir string, files map[string]string, message string) string {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), hostbox.DirMode); err != nil {
			t.Fatalf("making the directory of %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), hostbox.FileMode); err != nil {
			t.Fatalf("writing %s: %v", rel, err)
		}
	}
	c.Run(t, dir, "add", "-A")
	c.Run(t, dir, "commit", "-q", "--allow-empty", "-m", message)
	return strings.TrimSpace(c.Run(t, dir, "rev-parse", "HEAD"))
}

// Empty is a repository the engine's way with nothing in it: HEAD ("ref:
// refs/heads/main\n"), config (gitproc.RepoConfig), refs/heads/, refs/tags/,
// objects/pack/ and a packed-refs holding only its header, in a t.TempDir().
func Empty(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"refs/heads", "refs/tags", "objects/pack"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(sub)), hostbox.DirMode); err != nil {
			t.Fatalf("making %s: %v", sub, err)
		}
	}
	for name, content := range map[string]string{
		"HEAD":        "ref: refs/heads/main\n",
		"config":      gitproc.RepoConfig,
		"packed-refs": packedRefsHeader,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), hostbox.FileMode); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	return dir
}

// packedRefsHeader claims only that the file is sorted, so git trusts no peel
// it does not find and reads an annotated tag's target from the object itself.
const packedRefsHeader = "# pack-refs with: sorted \n"

// Pack writes ONE pack into bare's objects/pack holding the objects `git
// rev-list --objects <revs…>` names in worktree — with the .idx (version 2)
// and the .rev beside it that index-pack --rev-index writes — and returns its
// checksum. It moves no ref.
func Pack(t testing.TB, c *Client, bare, worktree string, revs ...string) string {
	t.Helper()
	base := filepath.Join(bare, "objects", "pack", "pack")
	stdout, stderr, err := c.Try(t, worktree, strings.NewReader(strings.Join(revs, "\n")+"\n"),
		"-c", "pack.writeReverseIndex=true", "-c", "pack.indexVersion=2", "pack-objects", "--revs", "-q", base)
	if err != nil {
		t.Fatalf("packing %v from %s into %s: %v\n%s", revs, worktree, bare, err, stderr)
	}
	return strings.TrimSpace(stdout)
}

// SetRefs rewrites bare's packed-refs to exactly refs (ref → oid), sorted,
// under a header that claims only the sort.
func SetRefs(t testing.TB, bare string, refs map[string]string) {
	t.Helper()
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(packedRefsHeader)
	for _, name := range names {
		fmt.Fprintf(&b, "%s %s\n", refs[name], name)
	}
	if err := os.WriteFile(filepath.Join(bare, "packed-refs"), []byte(b.String()), hostbox.FileMode); err != nil {
		t.Fatalf("writing %s's packed-refs: %v", bare, err)
	}
}

// Bundled is a pack Pack wrote, read back as the ONE object a push stores:
// its .pack, .idx and .rev back to back, each section's length, and the
// object count the pack's header states.
type Bundled struct {
	Bytes                               []byte
	Checksum                            string // the pack's trailer, hex
	PackBytes, IndexBytes, ReverseBytes int64
	Objects                             int64
}

// Bundle reads pack-<checksum>.{pack,idx,rev} from bare's objects/pack; a
// missing section fails t (Pack always writes all three).
func Bundle(t testing.TB, bare, checksum string) Bundled {
	t.Helper()
	read := func(ext string) []byte {
		body, err := os.ReadFile(filepath.Join(bare, "objects", "pack", "pack-"+checksum+ext))
		if err != nil {
			t.Fatalf("reading the %s of pack %s: %v", ext, checksum, err)
		}
		return body
	}
	pack, idx, rev := read(".pack"), read(".idx"), read(".rev")
	const header, trailer = 12, 20
	if len(pack) < header+trailer || string(pack[:4]) != "PACK" {
		t.Fatalf("pack %s is not a pack: %d bytes", checksum, len(pack))
	}
	b := Bundled{
		Bytes:        append(append(append(make([]byte, 0, len(pack)+len(idx)+len(rev)), pack...), idx...), rev...),
		Checksum:     hex.EncodeToString(pack[len(pack)-trailer:]),
		PackBytes:    int64(len(pack)),
		IndexBytes:   int64(len(idx)),
		ReverseBytes: int64(len(rev)),
		Objects:      int64(binary.BigEndian.Uint32(pack[8:header])),
	}
	if b.Checksum != checksum {
		t.Fatalf("pack-%s.pack ends in the checksum %s", checksum, b.Checksum)
	}
	return b
}

// Fake is a stand-in git: an executable named git in a fresh directory that
// prints version (a whole `git version …` line) for `git version` and runs
// body — POSIX sh, "$@" the arguments after "git" — for anything else. Its
// path, for gitproc.Options.Path or a PATH a test sets.
func Fake(t testing.TB, version, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git")
	writeScript(t, path, "#!/bin/sh\n"+
		"if [ \"${1-}\" = "+probe+" ]; then exit 0; fi\n"+
		"if [ \"${1-}\" = version ]; then printf '%s\\n' "+quote(version)+"; exit 0; fi\n"+
		body+"\n")
	return path
}

// Call is one invocation a [Recording] saw.
type Call struct {
	GitDir string   // GIT_DIR as the process saw it ("" when none)
	Args   []string // after "git"
}

// Recording is the real git behind a recorder: every invocation writes one
// file (GIT_DIR and argv, NUL-separated — a file per invocation, so
// concurrent processes never interleave) and execs the real git. calls
// returns every invocation so far, in no particular order.
func Recording(t testing.TB) (path string, calls func() []Call) {
	t.Helper()
	git := Require(t)
	dir := t.TempDir()
	path = filepath.Join(dir, "git")
	writeScript(t, path, "#!/bin/sh\n"+
		"if [ \"${1-}\" = "+probe+" ]; then exit 0; fi\n"+
		"f=$(mktemp "+quote(filepath.Join(dir, "call.XXXXXX"))+") || exit 125\n"+
		"printf '%s\\0' \"${GIT_DIR-}\" \"$@\" > \"$f\"\n"+
		"exec "+quote(git)+" \"$@\"\n")
	return path, func() []Call {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("listing the recorded calls: %v", err)
		}
		var out []Call
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "call.") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("reading a recorded call: %v", err)
			}
			fields := strings.Split(strings.TrimSuffix(string(body), "\x00"), "\x00")
			out = append(out, Call{GitDir: fields[0], Args: fields[1:]})
		}
		return out
	}
}

// writeScript writes an executable stand-in and waits until it can be exec'd
// (hostbox.Settle, with the probe every stand-in here answers at once): a
// suite of parallel cases, each writing a stand-in and forking at once, is
// exactly the window in which Linux refuses to exec a file another child
// still holds open for writing.
func writeScript(t testing.TB, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o700); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	if err := hostbox.Settle(t.Context(), path, probe); err != nil {
		t.Fatalf("settling %s: %v", path, err)
	}
}

// quote is s as one POSIX shell word.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
