package gitproc_test

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// THE VERSION LINE IS READ FOR ITS THREE NUMBERS, whatever a vendor or a build
// appends — and anything that is not git's own line is refused rather than
// read as some version.
func TestParseVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want gitproc.Version
	}{
		{"git version 2.43.0", gitproc.Version{Major: 2, Minor: 43}},
		{"git version 2.39.3 (Apple Git-145)", gitproc.Version{Major: 2, Minor: 39, Patch: 3}},
		{"git version 2.47.2\n", gitproc.Version{Major: 2, Minor: 47, Patch: 2}},
		{"git version 2.45.GIT", gitproc.Version{Major: 2, Minor: 45}},
		{"git version 2.48.0.rc1", gitproc.Version{Major: 2, Minor: 48}},
		{"git version 2.43.0.windows.1", gitproc.Version{Major: 2, Minor: 43}},
		{"hub version 2.14.2\ngit version 2.40.1", gitproc.Version{Major: 2, Minor: 40, Patch: 1}},
	} {
		got, err := gitproc.ParseVersion(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"", "git version", "git version x.y", "version 2.43.0", "git version 99999999999999999999.1"} {
		if got, err := gitproc.ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) = %v, want an error", in, got)
		}
	}
}

// A GIT BELOW 2.40 REFUSES THE POOL, naming the binary, what it is, the
// minimum and where to get one — the boundary itself is accepted.
func TestAnOldGitRefusesToStart(t *testing.T) {
	t.Parallel()
	old := gittest.Fake(t, "git version 2.39.0", "")
	_, err := gitproc.New(t.Context(), gitproc.Options{Path: old, Home: filepath.Join(t.TempDir(), "home")})
	var tooOld *gitproc.ErrTooOld
	if !errors.As(err, &tooOld) {
		t.Fatalf("New over git 2.39.0 = %v, want *ErrTooOld", err)
	}
	if tooOld.Path != old || tooOld.Found != (gitproc.Version{Major: 2, Minor: 39}) || tooOld.Want != (gitproc.Version{Major: 2, Minor: 40}) {
		t.Fatalf("ErrTooOld = %+v", tooOld)
	}
	for _, want := range []string{old, "2.39.0", "2.40.0", "2.47 (Debian trixie)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}

	newPool(t, gitproc.Options{Path: gittest.Fake(t, "git version 2.40.0", "")})

	_, err = gitproc.New(t.Context(), gitproc.Options{Path: gittest.Fake(t, "git version garbage", ""), Home: filepath.Join(t.TempDir(), "home")})
	if err == nil || !strings.Contains(err.Error(), "garbage") {
		t.Fatalf("New over a git printing garbage = %v, want an error naming the text", err)
	}
}

// THE PACK REUSE VALUE FOLLOWS THE BINARY: 2.43 refuses "multi".
func TestPackReuseFollowsTheVersion(t *testing.T) {
	t.Parallel()
	if got := newPool(t, gitproc.Options{Path: gittest.Fake(t, "git version 2.43.0", "")}).PackReuse(); got != "true" {
		t.Errorf("PackReuse on 2.43.0 = %q, want true", got)
	}
	if got := newPool(t, gitproc.Options{Path: gittest.Fake(t, "git version 2.44.0", "")}).PackReuse(); got != "multi" {
		t.Errorf("PackReuse on 2.44.0 = %q, want multi", got)
	}
}

// THE POOL NEVER HANDS A GIT A VALUE IT REFUSES: the real git accepts the
// pack.allowPackReuse value the pool chose for it.
func TestThePackReuseValueIsOneThisGitAccepts(t *testing.T) {
	t.Parallel()
	c := gittest.NewClient(t)
	wt := c.Init(t)
	tip := c.Commit(t, wt, map[string]string{"a.txt": "a\n"}, "one")
	bare := gittest.Empty(t)
	gittest.Pack(t, c, bare, wt, tip)
	gittest.SetRefs(t, bare, map[string]string{"refs/heads/main": tip})

	p := realPool(t, gitproc.Options{})
	var sent countingWriter
	_, err := p.Run(t.Context(), gitproc.Cmd{
		Dir: bare, Args: []string{"pack-objects", "--revs", "--stdout", "-q"},
		Config: []gitproc.KV{{Key: "pack.allowPackReuse", Value: p.PackReuse()}},
		Stdin:  strings.NewReader("main\n"), Stdout: &sent,
		Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline,
	})
	if err != nil || sent.n == 0 {
		t.Fatalf("pack-objects with pack.allowPackReuse=%s: %d bytes, %v", p.PackReuse(), sent.n, err)
	}
}

// countingWriter counts what it is given and keeps none of it.
type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

func TestAPathThatIsNotAbsoluteIsRefused(t *testing.T) {
	t.Parallel()
	_, err := gitproc.New(t.Context(), gitproc.Options{Path: "git", Home: filepath.Join(t.TempDir(), "home")})
	if err == nil || !strings.Contains(err.Error(), `"git"`) {
		t.Fatalf("New with a relative path = %v, want a refusal naming it", err)
	}
}

// THE LANES ARE SIZED HERE AND NOWHERE ELSE: 0 resolves to the cores (and four
// transfers a core, no more than one for every TransferShare of the node's
// memory), and a negative size names the setting an operator changes.
//
// The memory bound is what a container limited in memory and not in CPU
// needs: it sees its host's every core, and on 64 of them with 16 GiB four a
// core admitted 256 transfers that could hold 56 GiB — it runs 32, as the
// eight cores its memory backs would. A node of CoreMemory a core or more is
// sized by its cores as it always was, a node too small for one transfer
// still runs one, and an operator's own count is taken as given.
//
// Mutation: size the default by the cores alone, and the 64-core row fails.
func TestTheLaneSizesAreResolvedHere(t *testing.T) {
	t.Parallel()
	cores := runtime.GOMAXPROCS(0)
	const plenty = 1 << 40
	for _, tc := range []struct {
		o                      gitproc.Options
		read, maint, transfers int
	}{
		{gitproc.Options{Memory: plenty}, cores, (cores + 1) / 2, gitproc.TransfersPerProcess * cores},
		{gitproc.Options{MaxProcesses: 3, Memory: plenty}, 3, 2, 12},
		{gitproc.Options{MaxProcesses: 1, Memory: plenty}, 1, 1, 4},
		{gitproc.Options{MaxProcesses: 3, MaxTransfers: 5, Memory: plenty}, 3, 2, 5},
		{gitproc.Options{MaxProcesses: 4, Memory: 4 * gitproc.CoreMemory}, 4, 2, 16},
		{gitproc.Options{MaxProcesses: 64, Memory: 16 << 30}, 64, 32, 32},
		{gitproc.Options{MaxProcesses: 4, Memory: 1 << 30}, 4, 2, 2},
		{gitproc.Options{MaxProcesses: 4, Memory: 100 << 20}, 4, 2, 1},
		{gitproc.Options{MaxProcesses: 64, MaxTransfers: 300, Memory: 16 << 30}, 64, 32, 300},
	} {
		p := fakePool(t, "exit 0", tc.o)
		if r, m, x := p.Slots(gitproc.ClassRead), p.Slots(gitproc.ClassMaintenance), p.Slots(gitproc.ClassTransfer); r != tc.read || m != tc.maint || x != tc.transfers {
			t.Errorf("%+v: read %d, maintenance %d, transfer %d; want %d, %d, %d", tc.o, r, m, x, tc.read, tc.maint, tc.transfers)
		}
		if got := p.Slots("bogus"); got != 0 {
			t.Errorf("Slots(bogus) = %d, want 0", got)
		}
	}
	fake := gittest.Fake(t, fakeVersion, "exit 0")
	for _, tc := range []struct {
		o    gitproc.Options
		name string
	}{
		{gitproc.Options{MaxProcesses: -1}, "git.max_processes"},
		{gitproc.Options{MaxTransfers: -1}, "git.max_transfers"},
		{gitproc.Options{Memory: -1}, "Memory"},
	} {
		tc.o.Path, tc.o.Home = fake, filepath.Join(t.TempDir(), "home")
		if _, err := gitproc.New(t.Context(), tc.o); err == nil || !strings.Contains(err.Error(), tc.name) {
			t.Errorf("New(%+v) = %v, want a refusal naming %s", tc.o, err, tc.name)
		}
	}
}
