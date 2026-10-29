package codetest

import (
	"context"
	"crypto/rand"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// Person is a person's git: the one client git a case drives
// (gittest.NewClient), with a credential helper answering a node's 401
// challenge with the person's token as the Basic password — the path a
// person's own git takes, never a URL carrying the credential.
type Person struct {
	t     testing.TB
	git   *gittest.Client
	token string
}

// NewPerson is who's git: their token from the fleet ("" is a token bound to
// nobody), on the git the fleet's persons run (Fleet.ClientGit).
func NewPerson(t testing.TB, f Fleet, who string) *Person {
	t.Helper()
	return clientWith(t, f.ClientGit(), "crewlet", f.Token(t, who))
}

// PersonWithToken is a person's git presenting token, on the PATH's git held
// to the client floor (gittest.Require) — for a case with no fleet of this
// package's, an engine's own.
func PersonWithToken(t testing.TB, token string) *Person {
	t.Helper()
	return clientWith(t, gittest.Require(t), "crewlet", token)
}

// clientWith is a git client — a person's, or a coding box's (NewBox) — the
// git at git, presenting user and token.
func clientWith(t testing.TB, git, user, token string) *Person {
	t.Helper()
	// A SHELL FUNCTION, as a person writes one: git runs a helper whose
	// value starts with `!` through the shell, with get, store or erase as
	// its argument, and reads only what `get` prints.
	helper := "!f() { test \"$1\" = get || exit 0; echo username=" + user + "; echo password=" + token + "; }; f"
	return &Person{t: t, token: token, git: gittest.NewClientWith(t, git).Env(
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0="+helper,
		"GIT_TERMINAL_PROMPT=0")}
}

// Git is the person's client, for a command no helper here spells.
func (p *Person) Git() *gittest.Client { return p.git }

// Token is the credential the person presents.
func (p *Person) Token() string { return p.token }

// Init is a fresh work tree on main.
func (p *Person) Init() string {
	p.t.Helper()
	return p.git.Init(p.t)
}

// Commit commits files in dir and returns the commit's id.
func (p *Person) Commit(dir string, files map[string]string, message string) string {
	p.t.Helper()
	return p.git.Commit(p.t, dir, files, message)
}

// Clone clones url into a fresh directory, failing the test on any error.
func (p *Person) Clone(url string) string {
	p.t.Helper()
	dir := filepath.Join(p.t.TempDir(), "clone")
	if _, stderr, err := p.git.Try(p.t, "", nil, "clone", "--quiet", url, dir); err != nil {
		p.t.Fatalf("git clone %s: %v\n%s", url, err, stderr)
	}
	return dir
}

// TryClone is a clone expected to fail: its stderr and error.
func (p *Person) TryClone(ctx context.Context, url string, args ...string) (dir, stderr string, err error) {
	p.t.Helper()
	dir = filepath.Join(p.t.TempDir(), "clone")
	_, stderr, err = p.git.TryContext(ctx, "", nil, slices.Concat([]string{"clone", "--quiet"}, args,
		[]string{url, dir})...)
	return dir, stderr, err
}

// Head is dir's HEAD commit.
func (p *Person) Head(dir string) string {
	p.t.Helper()
	return strings.TrimSpace(p.git.Run(p.t, dir, "rev-parse", "HEAD"))
}

// LsRemote is every ref url advertises (ref → id), HEAD included, under the
// protocol version v (0 or 2).
func (p *Person) LsRemote(url string, v int) map[string]string {
	p.t.Helper()
	out, stderr, err := p.git.Try(p.t, "", nil, "-c", fmt.Sprintf("protocol.version=%d", v), "ls-remote", url)
	if err != nil {
		p.t.Fatalf("git ls-remote %s (protocol %d): %v\n%s", url, v, err, stderr)
	}
	refs, perr := parseLsRemote(out)
	if perr != nil {
		p.t.Fatalf("git ls-remote %s: %v", url, perr)
	}
	return refs
}

// parseLsRemote is `git ls-remote`'s output as ref → id.
func parseLsRemote(out string) (map[string]string, error) {
	refs := map[string]string{}
	for line := range strings.Lines(out) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			continue
		}
		oid, ref, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("a line that is no ref: %q", line)
		}
		refs[ref] = oid
	}
	return refs, nil
}

// Pushed is what one `git push --porcelain` printed.
type Pushed struct {
	// Refs is each destination ref's status line.
	Refs map[string]RefStatus
	// Stdout and Stderr are what git printed; Err is its exit (nil: every
	// ref went through).
	Stdout, Stderr string
	Err            error
}

// RefStatus is one porcelain status line: its flag (' ' fast-forward, '+'
// forced, '-' deleted, '*' new, '=' up to date, '!' rejected) and its summary
// with the reason in parentheses.
type RefStatus struct {
	Flag    byte
	Summary string
}

// OK reports whether git printed ref as updated or up to date.
func (p Pushed) OK(ref string) bool {
	s, ok := p.Refs[ref]
	return ok && s.Flag != '!'
}

// UpToDate reports whether git printed ref up to date — what a person's git,
// without --porcelain, sums up as `Everything up-to-date`.
func (p Pushed) UpToDate(ref string) bool {
	s, ok := p.Refs[ref]
	return ok && s.Flag == '='
}

// Rejected is ref's reason when git printed it rejected by the remote, and
// whether it did.
func (p Pushed) Rejected(ref string) (reason string, ok bool) {
	s, found := p.Refs[ref]
	if !found || s.Flag != '!' {
		return "", false
	}
	return s.Summary, true
}

// String is the push as a failure message prints it.
func (p Pushed) String() string {
	return fmt.Sprintf("exit %v\nstdout:\n%s\nstderr:\n%s", p.Err, p.Stdout, p.Stderr)
}

// Push runs `git push --porcelain <url> <refspecs…>` in dir under ctx.
func (p *Person) Push(ctx context.Context, dir, url string, refspecs ...string) Pushed {
	p.t.Helper()
	return p.PushArgs(ctx, dir, nil, url, refspecs...)
}

// PushArgs is Push with options before the URL (--atomic, --force, -o …).
func (p *Person) PushArgs(ctx context.Context, dir string, opts []string, url string, refspecs ...string) Pushed {
	args := slices.Concat([]string{"push", "--porcelain"}, opts, []string{url}, refspecs)
	stdout, stderr, err := p.git.TryContext(ctx, dir, nil, args...)
	return Pushed{Refs: parsePorcelain(stdout), Stdout: stdout, Stderr: stderr, Err: err}
}

// parsePorcelain reads `git push --porcelain`'s status lines:
// "<flag>\t<src>:<dst>\t<summary>".
func parsePorcelain(out string) map[string]RefStatus {
	refs := map[string]RefStatus{}
	for line := range strings.Lines(out) {
		line = strings.TrimSuffix(line, "\n")
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || len(parts[0]) != 1 {
			continue
		}
		_, dst, ok := strings.Cut(parts[1], ":")
		if !ok {
			continue
		}
		refs[dst] = RefStatus{Flag: parts[0][0], Summary: parts[2]}
	}
	return refs
}

// randomFile is n random bytes as a string — incompressible, so a pack holding
// it is at least n bytes.
func randomFile(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return string(raw)
}
