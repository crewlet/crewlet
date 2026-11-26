package homecache

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeBuild is a Builder writing files into a fresh home beside the cache:
// files(n, steps) is what its nth build (from 1) leaves, escape(steps) the
// step it reports leaving the home, and blockFirst holds the first build until
// release closes, having said so on building.
type fakeBuild struct {
	base   string
	files  func(n int, steps []Step) map[string]string
	escape func(steps []Step) *Escape

	mu    sync.Mutex
	steps [][]Step

	blockFirst bool
	building   chan struct{}
	release    chan struct{}
}

func (f *fakeBuild) build(_ context.Context, steps []Step) (Built, error) {
	f.mu.Lock()
	f.steps = append(f.steps, steps)
	n := len(f.steps)
	f.mu.Unlock()
	if f.blockFirst && n == 1 {
		close(f.building)
		<-f.release
	}
	home, err := os.MkdirTemp(f.base, "build-")
	if err != nil {
		return Built{}, err
	}
	for rel, body := range f.files(n, steps) {
		path := filepath.Join(home, rel)
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return Built{}, err
		}
		if err = os.WriteFile(path, []byte(body), 0o600); err != nil {
			return Built{}, err
		}
	}
	b := Built{Home: home}
	if f.escape != nil {
		b.Escaped = f.escape(steps)
	}
	return b, nil
}

func (f *fakeBuild) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.steps)
}

// release writes the build's number, as an unpinned install writes whatever
// release is newest when it runs.
func release(n int, _ []Step) map[string]string {
	return map[string]string{".npm-global/harness/VERSION": fmt.Sprint(n)}
}

func newCache(t *testing.T) (*Cache, string) {
	t.Helper()
	base := t.TempDir()
	c, err := Open(filepath.Join(base, "cache"), "workspace", ".crewlet")
	if err != nil {
		t.Fatal(err)
	}
	return c, base
}

func input(commands ...string) Input {
	return Input{
		Runtime: "docker",
		ImageID: "sha256:aaaa",
		Env:     map[string]string{"CREWLET_AGENT_HANDLE": "sre"},
		Steps:   []Step{{Name: "coding-tools", Commands: commands}},
	}
}

func provision(t *testing.T, c *Cache, in Input, secrets []string, f *fakeBuild) (Result, string, error) {
	t.Helper()
	dst := t.TempDir()
	res, err := c.Provision(t.Context(), in, secrets, f.build, dst)
	return res, dst, err
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

// A build is handed no credential, so a credential in what it left is an
// engine bug: the tree is discarded, nothing is copied, and the error names
// where without saying what.
func TestABuildLeavingACredentialIsKeptNowhere(t *testing.T) {
	const secret = "ghs_0123456789abcdefSECRET"
	for name, files := range map[string]map[string]string{
		"in a file": {".npmrc": "//registry/:_authToken=" + secret + "\n"},
		// Across the scan's chunk boundary, which a scan of each chunk alone misses.
		"across a chunk": {"bin/tool": strings.Repeat("a", scanChunk-5) + secret + "b"},
		"in a name":      {".config/" + secret + "/x": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			c, base := newCache(t)
			f := &fakeBuild{base: base, files: func(int, []Step) map[string]string { return files }}
			_, dst, err := provision(t, c, input("install"), []string{"", secret}, f)
			var se *SecretError
			if !errors.As(err, &se) {
				t.Fatalf("a build that left the credential was taken: %v", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the refusal says the credential: %v", err)
			}
			if got := entries(t, c.trees()); len(got) != 0 {
				t.Errorf("the tree was kept: %v", got)
			}
			if got := entries(t, dst); len(got) != 0 {
				t.Errorf("the box was handed %v", got)
			}
		})
	}
	t.Run("a link to it", func(t *testing.T) {
		c, base := newCache(t)
		f := &fakeBuild{base: base, files: release}
		build := func(ctx context.Context, steps []Step) (Built, error) {
			b, err := f.build(ctx, steps)
			if err == nil {
				err = os.Symlink("/tmp/"+secret, filepath.Join(b.Home, "token"))
			}
			return b, err
		}
		_, err := c.Provision(t.Context(), input("install"), []string{secret}, build, t.TempDir())
		var se *SecretError
		if !errors.As(err, &se) || se.Path != "token" {
			t.Fatalf("a link naming the credential was taken: %v", err)
		}
	})
}

// An unpinned install is frozen at the release its build found: nothing but a
// change of its key builds it again. Every input the key covers rebuilds.
func TestAHomeIsRebuiltWhenItsKeyChangesAndNeverOtherwise(t *testing.T) {
	c, base := newCache(t)
	f := &fakeBuild{base: base, files: release}
	in := input("npm install -g some-harness")
	for range 3 {
		_, dst, err := provision(t, c, in, nil, f)
		if err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, filepath.Join(dst, ".npm-global/harness/VERSION")); got != "1" {
			t.Fatalf("an unchanged key was rebuilt: the box holds release %s", got)
		}
	}
	changes := map[string]func(*Input){
		"a newer image":  func(in *Input) { in.ImageID = "sha256:bbbb" },
		"the runtime":    func(in *Input) { in.Runtime = "podman" },
		"the identity":   func(in *Input) { in.Env = map[string]string{"CREWLET_AGENT_HANDLE": "qa"} },
		"a command":      func(in *Input) { in.Steps[0].Commands = []string{"npm install -g some-harness@2"} },
		"a file":         func(in *Input) { in.Steps[0].Files = map[string]string{".profile": "x"} },
		"a step's env":   func(in *Input) { in.Steps[0].Env = map[string]string{"A": "b"} },
		"a step's bound": func(in *Input) { in.Steps[0].TimeoutSeconds = 60 },
		"a step's name":  func(in *Input) { in.Steps[0].Name = "tools" },
	}
	for _, name := range slices.Sorted(maps.Keys(changes)) {
		changed := input("npm install -g some-harness")
		changes[name](&changed)
		before := f.calls()
		res, dst, err := provision(t, c, changed, nil, f)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Built || f.calls() != before+1 {
			t.Errorf("%s did not rebuild the home", name)
		}
		if got := readFile(t, filepath.Join(dst, ".npm-global/harness/VERSION")); got != fmt.Sprint(before+1) {
			t.Errorf("%s: the box holds release %s, not the new build's", name, got)
		}
	}
	if KeyOf(Input{Env: map[string]string{}, Steps: in.Steps}, 1) != KeyOf(Input{Steps: in.Steps}, 1) {
		t.Error("an empty environment and none are two keys")
	}
}

// Two takers of one key, in two descriptions of its lock as two processes
// are, build it once: the second waits, then copies the first's tree.
func TestTwoTakersOfOneKeyBuildItOnce(t *testing.T) {
	first, base := newCache(t)
	second, err := Open(first.root, "workspace", ".crewlet")
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	second.waiting = func(Key) { close(waiting) }
	f := &fakeBuild{base: base, files: release, blockFirst: true,
		building: make(chan struct{}), release: make(chan struct{})}
	in := input("install")
	type outcome struct {
		dst string
		err error
	}
	firstDone, secondDone := make(chan outcome, 1), make(chan outcome, 1)
	firstDst, secondDst := t.TempDir(), t.TempDir()
	ctx := t.Context()
	go func() {
		_, perr := first.Provision(ctx, in, nil, f.build, firstDst)
		firstDone <- outcome{firstDst, perr}
	}()
	<-f.building
	go func() {
		_, perr := second.Provision(ctx, in, nil, f.build, secondDst)
		secondDone <- outcome{secondDst, perr}
	}()
	select {
	case <-waiting:
	case o := <-secondDone:
		close(f.release)
		<-firstDone
		t.Fatalf("the second taker did not wait for the first's build (%v): %d builds", o.err, f.calls())
	}
	close(f.release)
	for _, done := range []chan outcome{firstDone, secondDone} {
		o := <-done
		if o.err != nil {
			t.Fatal(o.err)
		}
		if got := readFile(t, filepath.Join(o.dst, ".npm-global/harness/VERSION")); got != "1" {
			t.Errorf("a taker's box holds release %s, not the one build's", got)
		}
	}
	if f.calls() != 1 {
		t.Errorf("two takers of one key built it %d times", f.calls())
	}
}

// Collection never takes a tree a box is being copied from; once the copy is
// done, it takes the tree and every file it alone held.
func TestCollectionSparesACopyInFlight(t *testing.T) {
	c, base := newCache(t)
	f := &fakeBuild{base: base, files: release}
	in := input("install")
	inCopy, resume := make(chan struct{}), make(chan struct{})
	c.copying = func(Key) {
		close(inCopy)
		<-resume
	}
	done := make(chan error, 1)
	dst, ctx := t.TempDir(), t.Context()
	go func() {
		_, perr := c.Provision(ctx, in, nil, f.build, dst)
		done <- perr
	}()
	<-inCopy
	removed, err := c.Collect(nil)
	if err != nil {
		t.Fatal(err)
	}
	close(resume)
	if perr := <-done; perr != nil {
		t.Fatalf("the copy in flight failed: %v", perr)
	}
	if len(removed) != 0 {
		t.Fatalf("collection took %v while a box was copied from it", removed)
	}
	c.copying = nil
	if removed, err = c.Collect([]Input{in}); err != nil || len(removed) != 0 {
		t.Fatalf("collection took a key in use: %v, %v", removed, err)
	}
	if removed, err = c.Collect(nil); err != nil || len(removed) != 1 {
		t.Fatalf("collection left a key nothing uses: %v, %v", removed, err)
	}
	for _, dir := range []string{c.trees(), c.objects()} {
		if got := entries(t, dir); len(got) != 0 {
			t.Errorf("collection left %v in %s", got, dir)
		}
	}
}

// A step whose effects leave the home ends the cached prefix: the cache builds
// once more without it, says so once, and never tries the longer prefix again
// while the setup is unchanged.
func TestAStepLeavingTheHomeIsCutOnce(t *testing.T) {
	steps := []Step{{Name: "a", Commands: []string{"a"}}, {Name: "apt", Commands: []string{"apt-get install x"}},
		{Name: "c", Commands: []string{"c"}}}
	for _, tc := range []struct {
		name   string
		at     int
		builds []int
	}{
		{"after a step", 1, []int{3, 1}},
		{"at the first step", 0, []int{3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, base := newCache(t)
			f := &fakeBuild{base: base, files: release, escape: func(s []Step) *Escape {
				if len(s) > tc.at {
					return &Escape{Step: tc.at, Name: s[tc.at].Name, Paths: []string{"/usr/bin/x"}}
				}
				return nil
			}}
			in := Input{Runtime: "docker", ImageID: "sha256:aaaa", Steps: steps}
			res, _, err := provision(t, c, in, nil, f)
			if err != nil {
				t.Fatal(err)
			}
			if res.Prefix != tc.at || res.Cut == nil || res.Cut.Name != steps[tc.at].Name {
				t.Fatalf("the first box: prefix %d, cut %+v", res.Prefix, res.Cut)
			}
			var lens []int
			for _, s := range f.steps {
				lens = append(lens, len(s))
			}
			if !slices.Equal(lens, tc.builds) {
				t.Errorf("builds of %v steps, want %v", lens, tc.builds)
			}
			res, _, err = provision(t, c, in, nil, f)
			if err != nil {
				t.Fatal(err)
			}
			if res.Prefix != tc.at || res.Cut != nil || f.calls() != len(tc.builds) {
				t.Errorf("the next box: prefix %d, cut %+v, %d builds: the cut was not kept",
					res.Prefix, res.Cut, f.calls())
			}
			if removed, cerr := c.Collect([]Input{in}); cerr != nil || len(removed) != 0 {
				t.Errorf("collection took a cut key in use: %v, %v", removed, cerr)
			}
		})
	}
}

// Homes that differ only by a seat's identity share their harness's bytes,
// and a box is handed copies, never the cache's own files.
func TestIdenticalFilesOfTwoHomesAreOneInodeAndABoxGetsACopy(t *testing.T) {
	c, base := newCache(t)
	f := &fakeBuild{base: base, files: func(_ int, _ []Step) map[string]string {
		return map[string]string{"bin/harness": strings.Repeat("x", 4096)}
	}}
	// In a directory without write permission, as Go's module cache keeps one.
	build := func(ctx context.Context, steps []Step) (Built, error) {
		b, err := f.build(ctx, steps)
		if err != nil {
			return b, err
		}
		return b, os.Chmod(filepath.Join(b.Home, "bin"), 0o555)
	}
	sre, qa := input("install"), input("install")
	qa.Env = map[string]string{"CREWLET_AGENT_HANDLE": "qa"}
	dst, dst2 := t.TempDir(), t.TempDir()
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(dst, "bin"), 0o700)
		_ = os.Chmod(filepath.Join(dst2, "bin"), 0o700)
		_, _ = c.Collect(nil)
	})
	r1, err := c.Provision(t.Context(), sre, nil, build, dst)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := c.Provision(t.Context(), qa, nil, build, dst2)
	if err != nil {
		t.Fatal(err)
	}
	a, err := os.Stat(filepath.Join(c.tree(r1.Key), "bin/harness"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(filepath.Join(c.tree(r2.Key), "bin/harness"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Error("two homes keep the same file twice")
	}
	inBox, err := os.Stat(filepath.Join(dst, "bin/harness"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(a, inBox) {
		t.Error("a box was handed the cache's own file, which its run would write")
	}
}

// A copied home holds what the build left, modes, links and read-only
// directories included, and none of the box's own paths.
func TestACopiedHomeIsTheBuildsLessTheBoxsOwn(t *testing.T) {
	c, base := newCache(t)
	f := &fakeBuild{base: base, files: func(int, []Step) map[string]string {
		return map[string]string{"workspace/repo/x": "x", ".crewlet/run": "r", ".profile": "p", "ro/f": "f"}
	}}
	build := func(ctx context.Context, steps []Step) (Built, error) {
		b, err := f.build(ctx, steps)
		if err != nil {
			return b, err
		}
		return b, errors.Join(
			os.Chmod(filepath.Join(b.Home, ".profile"), 0o755),
			os.Symlink(".profile", filepath.Join(b.Home, "link")),
			os.Chmod(filepath.Join(b.Home, "ro"), 0o500))
	}
	dst := t.TempDir()
	if _, err := c.Provision(t.Context(), input("install"), nil, build, dst); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dst, "ro"), 0o700) })
	// A tree holding a directory without write permission is collected whole.
	t.Cleanup(func() {
		if removed, err := c.Collect(nil); err != nil || len(removed) != 1 {
			t.Errorf("collecting a tree with a read-only directory: %v, %v", removed, err)
		}
	})
	if got := slices.Sorted(slices.Values(entries(t, dst))); !slices.Equal(got, []string{".profile", "link", "ro"}) {
		t.Errorf("the box holds %v", got)
	}
	if info, err := os.Stat(filepath.Join(dst, ".profile")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf(".profile: %v, %v", info, err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != ".profile" {
		t.Errorf("link: %q, %v", target, err)
	}
	if info, err := os.Stat(filepath.Join(dst, "ro")); err != nil || info.Mode().Perm() != 0o500 {
		t.Errorf("ro: %v, %v", info, err)
	}
	if got := readFile(t, filepath.Join(dst, "ro/f")); got != "f" {
		t.Errorf("ro/f holds %q", got)
	}
}

// A build that fails keeps nothing and holds nothing: the next box builds.
func TestAFailedBuildKeepsNothing(t *testing.T) {
	c, base := newCache(t)
	f := &fakeBuild{base: base, files: release}
	failing := func(context.Context, []Step) (Built, error) { return Built{}, errors.New("exit 1") }
	if _, err := c.Provision(t.Context(), input("install"), nil, failing, t.TempDir()); err == nil {
		t.Fatal("a failed build was taken")
	}
	if got := entries(t, c.trees()); len(got) != 0 {
		t.Fatalf("a failed build left %v", got)
	}
	if res, _, err := provision(t, c, input("install"), nil, f); err != nil || !res.Built {
		t.Fatalf("the next box did not build: %+v, %v", res, err)
	}
}
