package gittest_test

import (
	"bytes"
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

	p, err := gitproc.New(t.Context(), gitproc.Options{Path: gittest.Require(t), Home: filepath.Join(t.TempDir(), "home")})
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

	fake := gittest.Fake(t, "git version 2.41.0", `echo "body: $*"`)
	f, err := gitproc.New(t.Context(), gitproc.Options{Path: fake, Home: filepath.Join(t.TempDir(), "home")})
	if err != nil {
		t.Fatal(err)
	}
	if v := f.Version(); v != (gitproc.Version{Major: 2, Minor: 41}) {
		t.Fatalf("the fake answered version %v", v)
	}
	res, err = f.Run(t.Context(), gitproc.Cmd{Args: []string{"anything", "it's"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil || string(res.Stdout) != "body: anything it's\n" {
		t.Fatalf("the fake's body: %q, %v", res.Stdout, err)
	}
}
