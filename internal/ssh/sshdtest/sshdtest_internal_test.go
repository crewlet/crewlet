package sshdtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A BUILD THAT FAILS SAYS WHY: the error a failed image build answers carries
// what its failing step printed, as apt's own error would be, not only the
// step's exit status. The sentinel is assembled by the step's shell, so the
// step's command line, which a quiet build still repeats, does not hold it.
// Mutation: build with -q, or drop the output from the error, and this fails.
func TestABuildThatFailsSaysWhy(t *testing.T) {
	t.Parallel()
	Require(t)
	text := fmt.Sprintf("FROM %s\nRUN printf 'E: %%s-%%s\\n' mirror unreachable >&2; exit 100\n", Images[0])
	ctx, cancel := context.WithTimeout(t.Context(), buildTimeout)
	defer cancel()
	err := build(ctx, runtimeOf(runtimeName()), "crewlet-sshdtest:fails", text)
	if err == nil {
		t.Fatal("a build whose step exits 100 answered no error")
	}
	if !strings.Contains(err.Error(), "E: mirror-unreachable") {
		t.Fatalf("the failed build's error leaves out what its step printed:\n%v", err)
	}
}

// fakeRuntime stands in for docker: it records each command by its verb, holds
// an image only where told, and saves a tar where `save -o` names one.
type fakeRuntime struct {
	held                       bool
	loadErr, buildErr, saveErr error
	buildSaid                  string
	calls                      []string
	builtWith                  []string
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
			if _, err := io.ReadAll(in); err != nil {
				return nil, err
			}
			return []byte(f.buildSaid), f.buildErr
		case "save":
			if err := os.WriteFile(args[2], []byte("tar"), 0o600); err != nil {
				return nil, err
			}
			return nil, f.saveErr
		}
		return nil, nil
	}}
}

// AN IMAGE IS TAKEN CHEAPEST FIRST: one the runtime holds is used as it is;
// else one the cache keeps is loaded; else it is built, and saved to the cache
// when there is one. With no cache it is held or built, as before there was
// one, and a cached tar that will not load is built over. The cache is marked
// complete only once it keeps every image of Images.
// Mutation: skip the cache, skip the runtime's own image, never save, or mark
// the cache complete after one save, and this fails.
func TestAnImageIsHeldThenLoadedThenBuiltAndSaved(t *testing.T) {
	t.Parallel()
	base := Images[0]
	tag := tagOf(base, textOf(base))
	for _, tc := range []struct {
		name     string
		fake     fakeRuntime
		cached   bool
		dir      bool
		others   bool
		want     []string
		saved    bool
		complete bool
	}{
		{name: "held by the runtime", fake: fakeRuntime{held: true}, dir: true,
			want: []string{"image"}},
		{name: "kept by the cache", cached: true, dir: true, want: []string{"image", "load"}},
		{name: "in neither", dir: true, want: []string{"image", "build", "save"}, saved: true},
		{name: "in neither, the cache holding the others", dir: true, others: true,
			want: []string{"image", "build", "save"}, saved: true, complete: true},
		{name: "a cached tar that will not load", fake: fakeRuntime{loadErr: errors.New("corrupt")},
			cached: true, dir: true, want: []string{"image", "load", "build", "save"}, saved: true},
		{name: "no cache named", want: []string{"image", "build"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := ""
			if tc.dir {
				dir = t.TempDir()
			}
			if tc.cached {
				if err := os.WriteFile(tarOf(dir, tag), []byte("tar"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.others {
				for _, other := range Images[1:] {
					if err := os.WriteFile(tarOf(dir, tagOf(other, textOf(other))), []byte("tar"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			got, err := provide(t.Context(), tc.fake.runtime(), base, dir, t.Logf)
			if err != nil || got != tag {
				t.Fatalf("provide = %q, %v; want %q", got, err, tag)
			}
			if !slices.Equal(tc.fake.calls, tc.want) {
				t.Errorf("the runtime was asked %v, want %v", tc.fake.calls, tc.want)
			}
			if dir == "" {
				return
			}
			if _, err := os.Stat(tarOf(dir, tag)); (err == nil) != (tc.saved || tc.cached) {
				t.Errorf("the cache keeps the image: %v, want %v", err == nil, tc.saved || tc.cached)
			}
			if _, err := os.Stat(filepath.Join(dir, completeFile)); (err == nil) != tc.complete {
				t.Errorf("the cache is marked complete: %v, want %v", err == nil, tc.complete)
			}
		})
	}
}

// A SAVE THAT FAILS IS SAID AND LEAVES NOTHING: a save the runtime fails,
// having written part of the tar, leaves the cache directory as it found it,
// and the case is told once, with the save's error and the variable that
// chose the directory; the image is the case's all the same.
//
// Mutation: leave the runtime's partial tar, or name no variable, and this
// fails.
func TestASaveThatFailsIsSaidAndLeavesNothing(t *testing.T) {
	t.Parallel()
	fake := &fakeRuntime{saveErr: errors.New("no space left on device")}
	dir := t.TempDir()
	var said []string
	logf := func(format string, args ...any) { said = append(said, fmt.Sprintf(format, args...)) }
	if _, err := provide(t.Context(), fake.runtime(), Images[0], dir, logf); err != nil {
		t.Fatalf("provide = %v, want the image whatever became of its save", err)
	}
	if len(said) != 1 || !strings.Contains(said[0], "no space left on device") || !strings.Contains(said[0], CacheEnv) {
		t.Errorf("the failed save was said as %q, want its error once, naming %s", said, CacheEnv)
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range left {
		t.Errorf("the cache keeps %s after a failed save, want nothing", entry.Name())
	}
}

// A FAILED BUILD'S ERROR CARRIES WHAT THE BUILD SAID, and docker is asked for
// every step's output, never a quiet build. Mutation: drop the output from the
// error, ask for -q, or drop --progress=plain, and this fails.
func TestAFailedBuildsErrorCarriesWhatTheBuildSaid(t *testing.T) {
	t.Parallel()
	fake := &fakeRuntime{buildErr: errors.New("exit status 1"),
		buildSaid: "#5 3.2 E: Failed to fetch http://archive.ubuntu.com/ubuntu/dists/jammy/InRelease"}
	_, err := provide(t.Context(), fake.runtime(), Images[0], "", t.Logf)
	if err == nil || !strings.Contains(err.Error(), "E: Failed to fetch") {
		t.Fatalf("the failed build answered %v, want it to carry apt's own line", err)
	}
	if !slices.Contains(fake.builtWith, "--progress=plain") || slices.Contains(fake.builtWith, "-q") {
		t.Errorf("docker was asked %v, want plain progress and never -q", fake.builtWith)
	}
}

// THE TAG NAMES THE TEXT THAT DEFINES THE IMAGE: one Dockerfile text, one tag;
// any change to the text, or another base, another tag, so the runtime or the
// cache never answers for an image built from other text.
// Mutation: hash only the base, or nothing, and this fails.
func TestTheTagNamesTheTextThatDefinesTheImage(t *testing.T) {
	t.Parallel()
	base := Images[0]
	text := textOf(base)
	if tagOf(base, text) != tagOf(base, textOf(base)) {
		t.Fatal("one text answered two tags")
	}
	if tagOf(base, text) == tagOf(base, text+"\n") {
		t.Error("a changed Dockerfile kept its tag, so a stale image would answer for it")
	}
	if tagOf(Images[0], textOf(Images[0])) == tagOf(Images[1], textOf(Images[1])) {
		t.Error("two bases share a tag")
	}
}
