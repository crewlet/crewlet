package gittest_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// A FIXTURE IS THE SAME BYTES ON EVERY RUN: two clients with nothing in common
// but this package make the same commit, because the identity and the clock
// are fixed and nothing of the machine's configuration reaches either.
func TestACommitIsTheSameOnEveryClient(t *testing.T) {
	t.Parallel()
	var ids []string
	for range 2 {
		c := gittest.NewClient(t)
		wt := c.Init(t)
		ids = append(ids, c.Commit(t, wt, map[string]string{"dir/a.txt": "a\n"}, "one"))
	}
	if ids[0] != ids[1] || len(ids[0]) != 40 {
		t.Fatalf("two clients committed %q and %q", ids[0], ids[1])
	}
}

// AN ENGINE-SHAPED REPOSITORY IS ONE THE POOL RUNS GIT IN, and a pack read back
// as a bundle is its three sections, whole, in order.
func TestAFixtureIsARepositoryThePoolAccepts(t *testing.T) {
	t.Parallel()
	c := gittest.NewClient(t)
	wt := c.Init(t)
	first := c.Commit(t, wt, map[string]string{"a.txt": "a\n"}, "one")
	tip := c.Commit(t, wt, map[string]string{"b.txt": "b\n"}, "two")
	bare := gittest.Empty(t)
	older := gittest.Pack(t, c, bare, wt, first)
	newer := gittest.Pack(t, c, bare, wt, tip, "^"+first)
	gittest.SetRefs(t, bare, map[string]string{"refs/heads/main": tip, "refs/tags/v1": first})

	p, err := gitproc.New(t.Context(), gitproc.Options{Path: gittest.RequireNode(t), Home: filepath.Join(t.TempDir(), "home")})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Run(t.Context(), gitproc.Cmd{Dir: bare, Args: []string{"rev-list", "--count", "main"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "2" {
		t.Fatalf("rev-list --count main in the fixture: %q, %v", res.Stdout, err)
	}
	res, err = p.Run(t.Context(), gitproc.Cmd{Dir: bare, Args: []string{"rev-parse", "v1"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != first {
		t.Fatalf("the tag SetRefs wrote: %q, %v", res.Stdout, err)
	}

	for cs, objects := range map[string]int64{older: 3, newer: 3} {
		b := gittest.Bundle(t, bare, cs)
		if b.Checksum != cs || b.Objects != objects {
			t.Errorf("pack %s: checksum %s, %d objects; want %d", cs, b.Checksum, b.Objects, objects)
		}
		var sections [][]byte
		for _, ext := range []string{".pack", ".idx", ".rev"} {
			body, err := os.ReadFile(filepath.Join(bare, "objects", "pack", "pack-"+cs+ext))
			if err != nil {
				t.Fatal(err)
			}
			sections = append(sections, body)
		}
		if !bytes.Equal(b.Bytes, slices.Concat(sections...)) ||
			b.PackBytes != int64(len(sections[0])) || b.IndexBytes != int64(len(sections[1])) || b.ReverseBytes != int64(len(sections[2])) {
			t.Errorf("pack %s's bundle is not its .pack, .idx and .rev back to back", cs)
		}
		if !bytes.HasPrefix(sections[1], []byte{0xff, 't', 'O', 'c', 0, 0, 0, 2}) || !bytes.HasPrefix(sections[2], []byte("RIDX")) {
			t.Errorf("pack %s's index is not version 2 or its reverse index not a RIDX", cs)
		}
	}
}

// THE TRY HELPERS ANSWER WHAT THE OTHERS FAIL ON: a fixture that packs many
// pushes at once does it on goroutines of its own, where t may not fail, so a
// pack of a revision the work tree lacks, a bundle missing a section, one
// that is not a pack and one whose pack ends in another checksum each come
// back as an error, as does a pack whose context has ended — and a good pack
// as the bundle Bundle reads.
func TestTheTryHelpersAnswerTheirFailures(t *testing.T) {
	t.Parallel()
	c := gittest.NewClient(t)
	wt := c.Init(t)
	tip := c.Commit(t, wt, map[string]string{"a.txt": "a\n"}, "one")
	bare := gittest.Empty(t)
	if _, err := gittest.TryPack(t.Context(), c, bare, wt, strings.Repeat("ab", 20)); err == nil {
		t.Error("TryPack of a revision the work tree lacks answered no error")
	}
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := gittest.TryPack(ended, c, bare, wt, tip); err == nil {
		t.Error("TryPack under an ended context answered no error: a fixture's other workers would run on")
	}
	cs, err := gittest.TryPack(t.Context(), c, bare, wt, tip)
	if err != nil {
		t.Fatalf("TryPack of the work tree's tip: %v", err)
	}
	if b, err := gittest.ReadBundle(bare, cs); err != nil || !bytes.Equal(b.Bytes, gittest.Bundle(t, bare, cs).Bytes) {
		t.Fatalf("ReadBundle of a pack TryPack wrote: %v", err)
	}

	pack := filepath.Join(bare, "objects", "pack")
	whole, err := os.ReadFile(filepath.Join(pack, "pack-"+cs+".pack"))
	if err != nil {
		t.Fatal(err)
	}
	// Two more checksums' sections, copied from the good pack's: one whose
	// .pack is no pack, one whose .pack is the good pack under another name.
	notAPack, renamed := strings.Repeat("1", 40), strings.Repeat("2", 40)
	for name, body := range map[string][]byte{notAPack: []byte("not a pack"), renamed: whole} {
		for ext, content := range map[string][]byte{".pack": body, ".idx": {0}, ".rev": {0}} {
			if err := os.WriteFile(filepath.Join(pack, "pack-"+name+ext), content, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Remove(filepath.Join(pack, "pack-"+cs+".rev")); err != nil {
		t.Fatal(err)
	}
	for what, checksum := range map[string]string{
		"a pack with no .rev":                cs,
		"a pack nothing wrote":               strings.Repeat("3", 40),
		"a .pack that is no pack":            notAPack,
		"a .pack ending in another checksum": renamed,
	} {
		if _, err := gittest.ReadBundle(bare, checksum); err == nil {
			t.Errorf("ReadBundle of %s answered no error", what)
		}
	}
}

// A RECORDING RUNS THE REAL GIT AND SAYS WHAT IT RAN, and a FAKE answers its
// version and runs its body.
func TestTheStandInsDoWhatTheySay(t *testing.T) {
	t.Parallel()
	recorder, calls := gittest.Recording(t)
	p, err := gitproc.New(t.Context(), gitproc.Options{Path: recorder, Home: filepath.Join(t.TempDir(), "home")})
	if err != nil {
		t.Fatal(err)
	}
	bare := gittest.Empty(t)
	res, err := p.Run(t.Context(), gitproc.Cmd{Dir: bare, Args: []string{"rev-parse", "--git-dir"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != bare {
		t.Fatalf("rev-parse through the recording: %q, %v", res.Stdout, err)
	}
	if !slices.ContainsFunc(calls(), func(c gittest.Call) bool {
		return c.GitDir == bare && slices.Equal(c.Args, []string{"rev-parse", "--git-dir"})
	}) {
		t.Fatalf("the recording holds %+v, not the rev-parse it ran", calls())
	}

	fake := gittest.Fake(t, "git version 2.55.1", `echo "body: $*"`)
	f, err := gitproc.New(t.Context(), gitproc.Options{Path: fake, Home: filepath.Join(t.TempDir(), "home")})
	if err != nil {
		t.Fatal(err)
	}
	if v := f.Version(); v != (gitproc.Version{Major: 2, Minor: 55, Patch: 1}) {
		t.Fatalf("the fake answered version %v", v)
	}
	res, err = f.Run(t.Context(), gitproc.Cmd{Args: []string{"anything", "it's"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil || string(res.Stdout) != "body: anything it's\n" {
		t.Fatalf("the fake's body: %q, %v", res.Stdout, err)
	}
}

// A WRAP IS THE REAL GIT BEHIND ITS BODY: it answers the real git's own
// version, runs its body first — which may end the call, or run the real git
// itself — and hands the real git every call its body did not end.
func TestAWrapIsTheRealGitBehindItsBody(t *testing.T) {
	t.Parallel()
	direct, err := gitproc.New(t.Context(), gitproc.Options{Path: gittest.RequireNode(t),
		Home: filepath.Join(t.TempDir(), "home")})
	if err != nil {
		t.Fatal(err)
	}
	wrap := gittest.Wrap(t, `case "$1" in
anything) echo "body: $*"; exit 0 ;;
rev-parse) "$real_git" "$@"; echo "after: $?"; exit 0 ;;
esac`)
	w, err := gitproc.New(t.Context(), gitproc.Options{Path: wrap, Home: filepath.Join(t.TempDir(), "home")})
	if err != nil {
		t.Fatal(err)
	}
	if w.Version() != direct.Version() {
		t.Errorf("the wrap answered version %v, the git behind it %v", w.Version(), direct.Version())
	}
	res, err := w.Run(t.Context(), gitproc.Cmd{Args: []string{"anything", "it's"}, Class: gitproc.ClassRead,
		Deadline: gitproc.ReadDeadline})
	if err != nil || string(res.Stdout) != "body: anything it's\n" {
		t.Errorf("the wrap's body: %q, %v", res.Stdout, err)
	}
	bare := gittest.Empty(t)
	res, err = w.Run(t.Context(), gitproc.Cmd{Dir: bare, Args: []string{"rev-parse", "--git-dir"},
		Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil || string(res.Stdout) != bare+"\nafter: 0\n" {
		t.Errorf("rev-parse run by the wrap's body through the real git: %q, %v", res.Stdout, err)
	}
	res, err = w.Run(t.Context(), gitproc.Cmd{Dir: bare, Args: []string{"symbolic-ref", "HEAD"},
		Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "refs/heads/main" {
		t.Errorf("symbolic-ref, which the wrap's body leaves to the real git: %q, %v", res.Stdout, err)
	}
}
