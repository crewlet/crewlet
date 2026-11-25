package imagetest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// base is the image the real builds start from: a shell is all they need, and
// its tag is pinned, so a run names the build it ran on.
const base = "alpine:3.24.4"

// img is an image as a caller defines one, for the cases that fake the runtime.
var img = Image{Repo: "crewlet-imagetest", Name: "ubuntu:22.04",
	Text: "FROM ubuntu:22.04\nRUN apt-get -q update\n", CacheEnv: "CREWLET_IMAGETEST_CACHE"}

// A BUILD THAT FAILS SAYS WHY: the error a failed image build answers carries
// what its failing step printed, as apt's own error would be, not only the
// step's exit status. The sentinel is assembled by the step's shell, so the
// step's command line, which a quiet build still repeats, does not hold it.
// Mutation: build with -q, or drop the output from the error, and this fails.
func TestABuildThatFailsSaysWhy(t *testing.T) {
	t.Parallel()
	rt := runtimeOf(Require(t))
	failing := Image{Repo: "crewlet-imagetest", Name: "fails",
		Text: fmt.Sprintf("FROM %s\nRUN printf 'E: %%s-%%s\\n' mirror unreachable >&2; exit 100\n", base)}
	ctx, cancel := context.WithTimeout(t.Context(), buildTimeout)
	defer cancel()
	err := build(ctx, rt, failing)
	if err == nil {
		t.Fatal("a build whose step exits 100 answered no error")
	}
	if !strings.Contains(err.Error(), "E: mirror-unreachable") {
		t.Fatalf("the failed build's error leaves out what its step printed:\n%v", err)
	}
}

// A BUILD WITH A CONTEXT DIRECTORY COPIES FROM IT: the runtime takes the
// Dockerfile on stdin beside the directory, and the image holds what the
// directory held, which the build's own step checks. The text names what it
// copies, so each run builds a tag of its own, removed when the case ends.
// Mutation: build from stdin alone, leaving out the directory, and this fails.
func TestABuildCopiesFromItsContextDirectory(t *testing.T) {
	t.Parallel()
	name := Require(t)
	said := rand.Text()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "said"), []byte(said), 0o644); err != nil {
		t.Fatal(err)
	}
	copying := Image{Repo: "crewlet-imagetest", Name: "context", Context: dir,
		Text: fmt.Sprintf("FROM %s\nCOPY said /said\nRUN test \"$(cat /said)\" = %s\n", base, said)}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
		defer cancel()
		_ = exec.CommandContext(ctx, name, "rmi", "-f", copying.Tag()).Run()
	})
	ctx, cancel := context.WithTimeout(t.Context(), buildTimeout)
	defer cancel()
	if err := build(ctx, runtimeOf(name), copying); err != nil {
		t.Fatalf("the build from the context directory %s failed: %v", dir, err)
	}
}

// fakeRuntime stands in for docker: it records each command by its verb, holds
// an image only where told, and saves a tar where `save -o` names one, which it
// records, failing after it is written where told.
type fakeRuntime struct {
	held                       bool
	loadErr, buildErr, saveErr error
	buildSaid                  string
	calls                      []string
	builtWith                  []string
	builtFrom                  string
	savedTo                    string
}

func (f *fakeRuntime) runtime() runtime {
	return runtime{name: "docker", run: func(_ context.Context, in io.Reader, args ...string) ([]byte, error) {
		f.calls = append(f.calls, args[0])
		switch args[0] {
		case "image":
			if !f.held {
				return []byte("Error: No such image"), errors.New("exit status 1")
			}
		case "load":
			return nil, f.loadErr
		case "build":
			f.builtWith = args
			text, err := io.ReadAll(in)
			if err != nil {
				return nil, err
			}
			f.builtFrom = string(text)
			return []byte(f.buildSaid), f.buildErr
		case "save":
			f.savedTo = args[2]
			if err := os.WriteFile(args[2], []byte("tar"), 0o600); err != nil {
				return nil, err
			}
			return nil, f.saveErr
		}
		return nil, nil
	}}
}

// AN IMAGE IS TAKEN CHEAPEST FIRST: one the runtime holds is used as it is;
// else one the cache keeps is loaded; else it is built under its tag, and
// saved to the cache when there is one. With no cache it is held or built, and
// a cached tar that will not load is built over. A build reads the Dockerfile
// on stdin, as its whole context or named as the file beside its directory.
// Mutation: skip the cache, skip the runtime's own image, never save, or build
// without the context directory, and this fails.
func TestAnImageIsHeldThenLoadedThenBuiltAndSaved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		fake    fakeRuntime
		cached  bool
		dir     bool
		context bool
		want    []string
		saved   bool
	}{
		{name: "held by the runtime", fake: fakeRuntime{held: true}, dir: true,
			want: []string{"image"}},
		{name: "kept by the cache", cached: true, dir: true, want: []string{"image", "load"}},
		{name: "in neither", dir: true, want: []string{"image", "build", "save"}, saved: true},
		{name: "in neither, built from its context directory", dir: true, context: true,
			want: []string{"image", "build", "save"}, saved: true},
		{name: "a cached tar that will not load", fake: fakeRuntime{loadErr: errors.New("corrupt")},
			cached: true, dir: true, want: []string{"image", "load", "build", "save"}, saved: true},
		{name: "no cache named", want: []string{"image", "build"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			img := img
			if tc.context {
				img.Context = t.TempDir()
			}
			dir := ""
			if tc.dir {
				dir = t.TempDir()
			}
			if tc.cached {
				if err := os.WriteFile(img.Tar(dir), []byte("tar"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := provide(t.Context(), tc.fake.runtime(), img, dir, t.Logf); err != nil {
				t.Fatalf("provide = %v", err)
			}
			if !slices.Equal(tc.fake.calls, tc.want) {
				t.Errorf("the runtime was asked %v, want %v", tc.fake.calls, tc.want)
			}
			if slices.Contains(tc.want, "build") {
				from := []string{"-"}
				if tc.context {
					from = []string{"-f", "-", img.Context}
				}
				got := tc.fake.builtWith
				tagged := slices.Index(got, "-t")
				if tagged < 0 || tagged+1 >= len(got) || got[tagged+1] != img.Tag() || len(got) < len(from) ||
					!slices.Equal(got[len(got)-len(from):], from) || tc.fake.builtFrom != img.Text {
					t.Errorf("the build was asked %v with %q on stdin, want -t %s, ending %v, with the Dockerfile",
						got, tc.fake.builtFrom, img.Tag(), from)
				}
			}
			if dir == "" {
				return
			}
			if _, err := os.Stat(img.Tar(dir)); (err == nil) != (tc.saved || tc.cached) {
				t.Errorf("the cache keeps the image: %v, want %v", err == nil, tc.saved || tc.cached)
			}
		})
	}
}

// A FAILED BUILD'S ERROR CARRIES WHAT THE BUILD SAID, and docker is asked for
// every step's output, never a quiet build. Mutation: drop the output from the
// error, ask for -q, or drop --progress=plain, and this fails.
func TestAFailedBuildsErrorCarriesWhatTheBuildSaid(t *testing.T) {
	t.Parallel()
	fake := &fakeRuntime{buildErr: errors.New("exit status 1"),
		buildSaid: "#5 3.2 E: Failed to fetch http://archive.ubuntu.com/ubuntu/dists/jammy/InRelease"}
	err := provide(t.Context(), fake.runtime(), img, "", t.Logf)
	if err == nil || !strings.Contains(err.Error(), "E: Failed to fetch") {
		t.Fatalf("the failed build answered %v, want it to carry apt's own line", err)
	}
	if !slices.Contains(fake.builtWith, "--progress=plain") || slices.Contains(fake.builtWith, "-q") {
		t.Errorf("docker was asked %v, want plain progress and never -q", fake.builtWith)
	}
}

// A SAVE THAT FAILS IS SAID, NEVER A FAILURE, AND LEAVES NOTHING BEHIND: the
// case gets its image, the line names the variable that chose the directory,
// and the directory keeps neither a tar nor the part of one the runtime wrote,
// which nothing else would ever remove. Mutation: answer the save's error,
// keep its partial file, or leave the variable out of its line, and this fails.
func TestASaveThatFailsIsSaidAndLeavesNothing(t *testing.T) {
	t.Parallel()
	fake := &fakeRuntime{saveErr: errors.New("no space left on device")}
	dir := t.TempDir()
	var said []string
	logf := func(format string, args ...any) { said = append(said, fmt.Sprintf(format, args...)) }
	if err := provide(t.Context(), fake.runtime(), img, dir, logf); err != nil {
		t.Fatalf("provide = %v, want the image whatever became of its save", err)
	}
	if len(said) != 1 || !strings.Contains(said[0], "no space left on device") || !strings.Contains(said[0], img.CacheEnv) {
		t.Errorf("the failed save was said as %q, want its error once, naming %s", said, img.CacheEnv)
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range left {
		t.Errorf("the cache keeps %s after a failed save, want nothing", entry.Name())
	}
}

// A TAR IS KEPT WHOLE OR NOT AT ALL: the runtime writes beside the tar, which a
// rename then takes, so a save cut short (a run killed, a disk filled) never
// leaves part of one under the image's name. A cache directory that holds the
// tar holds the image, which is what CI saves a cache on.
// Mutation: save straight to the tar, and this fails.
func TestATarIsKeptWholeOrNotAtAll(t *testing.T) {
	t.Parallel()
	fake := &fakeRuntime{}
	dir := t.TempDir()
	if err := provide(t.Context(), fake.runtime(), img, dir, t.Logf); err != nil {
		t.Fatalf("provide = %v", err)
	}
	if fake.savedTo == "" || fake.savedTo == img.Tar(dir) {
		t.Errorf("the runtime was asked to save to %q, want a file beside %s", fake.savedTo, img.Tar(dir))
	}
	if _, err := os.Stat(img.Tar(dir)); err != nil {
		t.Errorf("the cache does not keep the image once its save is done: %v", err)
	}
}

// THE TAG NAMES THE TEXT THAT DEFINES THE IMAGE, and the tar is named as the
// tag is: one Dockerfile text, one tag and one tar, whatever its context
// directory, which differs from run to run; any change to the text, or another
// image, another tag and another tar, so neither the runtime nor the cache
// answers for an image built from other text.
// Mutation: hash only the name, hash the context directory, or name a tar by
// the name alone, and this fails.
func TestTheTagNamesTheTextThatDefinesTheImage(t *testing.T) {
	t.Parallel()
	again := img
	again.Context = t.TempDir()
	if again.Tag() != img.Tag() || again.Tar("/cache") != img.Tar("/cache") {
		t.Fatalf("one text answered %s and %s, kept as %s and %s", img.Tag(), again.Tag(), img.Tar("/cache"),
			again.Tar("/cache"))
	}
	changed := img
	changed.Text += "\n"
	if changed.Tag() == img.Tag() || changed.Tar("/cache") == img.Tar("/cache") {
		t.Error("a changed Dockerfile kept its tag or its tar, so a stale image would answer for it")
	}
	other := Image{Repo: img.Repo, Name: "ubuntu:24.04", Text: "FROM ubuntu:24.04\nRUN apt-get -q update\n"}
	if other.Tag() == img.Tag() || other.Tar("/cache") == img.Tar("/cache") {
		t.Error("two images share a tag or a tar")
	}
}
