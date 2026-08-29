package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// What these cases protect: a container box is one directory shared by two
// processes, and both ways that sharing can be broken are SILENT. A mount
// that is not this filesystem loses everything the engine writes; a container
// that writes it as another user leaves the engine unable to install the
// agent's shim or reclaim the box's disk. Neither shows up as an error at the
// point it happens.

// fakeRuntime writes a script under the runtime's own NAME — which is what
// picks the probe — answering `info` with the given text, and ONLY when asked
// with the template that runtime actually understands.
//
// The wrong template is a template error on a real CLI, and answering it
// anyway would let a probe that asked Podman a Docker question look like it
// worked.
func fakeRuntime(t *testing.T, name, wantFormat, info string, exit int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	script := fmt.Sprintf(`#!/bin/sh
[ "$1" = info ] || exit 0
[ "$3" = '%s' ] || { echo "template error: $3" >&2; exit 125; }
printf '%%s' '%s'
exit %d
`, wantFormat, info, exit)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	waitExecutable(t, path)
	return path
}

// waitExecutable blocks until a just-written script can actually be exec'd.
//
// # A FRESHLY WRITTEN EXECUTABLE CAN BE ETXTBSY, and this file execs one
//
// os.WriteFile closes its own descriptor, but any goroutine that forked
// while it was open inherited it, and Linux refuses to exec a file that any
// process holds open for writing. The child closes it at its own exec — Go
// opens with O_CLOEXEC — so the window is short. This package forks
// constantly, and under -race on a loaded runner it is wide enough to hit:
// measured at 17 failures in 200 execs of a just-written script with eight
// goroutines forking alongside.
//
// The symptom was not an error anyone could read. runHost reported a start
// failure, runtimeIsRootless turned that into "not rootless", and the
// rootless subtest failed asserting on an argv that carried --user — a
// result indistinguishable from the detection genuinely being wrong.
//
// Waiting here is enough rather than a retry at every call site: nothing
// reopens the file for writing afterwards, so once one exec gets through,
// every later one does.
func waitExecutable(t *testing.T, path string) {
	t.Helper()
	for attempt := range 100 {
		err := exec.Command(path).Run() //nolint:noctx // a local probe of one script
		if !errors.Is(err, syscall.ETXTBSY) {
			return
		}
		// A few milliseconds is a fork/exec, which is what this waits
		// out; 100 attempts is half a second of a window measured in
		// microseconds.
		time.Sleep(time.Duration(attempt+1) * time.Millisecond)
	}
	t.Fatalf("%s stayed ETXTBSY: something is holding it open for writing", path)
}

// The template each runtime answers. Docker carries the flag among its
// top-level security options; Podman reports it as a boolean of its own.
const (
	dockerRootlessFormat = "{{.SecurityOptions}}"
	podmanRootlessFormat = "{{.Host.Security.Rootless}}"
)

// THE CONTAINER RUNS AS THE ENGINE USER ON A ROOTFUL RUNTIME, and does not on
// a rootless one — where the user namespace already maps container root onto
// this user, and --user would map it into the subuid range instead, leaving
// the box unable to enter its own 0700 directory.
func TestTheContainerRunsAsWhoeverCanManageTheMount(t *testing.T) {
	t.Parallel()
	want := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	for _, tc := range []struct {
		name     string
		runtime  string
		format   string
		info     string
		exit     int
		wantUser bool
	}{
		{"rootful docker", "docker", dockerRootlessFormat,
			`[name=seccomp,profile=builtin name=cgroupns]`, 0, true},
		{"rootless docker", "docker", dockerRootlessFormat,
			`[name=rootless name=seccomp,profile=builtin]`, 0, false},
		{"rootless podman", "podman", podmanRootlessFormat, `true`, 0, false},
		// Podman answers the boolean itself, so "false" must not be read
		// as a list that happens to mention the word.
		{"rootful podman", "podman", podmanRootlessFormat, `false`, 0, true},
		// An unanswered probe takes the LOUD side. --user against a
		// rootless runtime fails the mount proof at creation with a
		// message that names the cause; the opposite mistake is silent
		// until the engine host is full of undeletable box directories.
		//
		// A NON-ZERO EXIT IS NOT AN ANSWER even when something reached
		// stdout: `docker info` against an unreachable daemon still
		// prints its client section — which lists plugins, `rootlesskit`
		// among them — and fails afterwards.
		{"a daemon that is down mid-answer", "docker", dockerRootlessFormat,
			`rootlesskit`, 1, true},
		{"an answer this build does not recognise", "docker", dockerRootlessFormat,
			`something else`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := &Local{
				opts:    LocalOptions{Containment: Container, Image: "img"},
				runtime: fakeRuntime(t, tc.runtime, tc.format, tc.info, tc.exit),
			}
			argv := l.runArgv(t.Context(), boxLayout{id: "b", root: t.TempDir()}, "crewlet-sbx-b")
			at := slices.Index(argv, "--user")
			if tc.wantUser {
				if at < 0 || argv[at+1] != want {
					t.Fatalf("argv = %v, want --user %s", argv, want)
				}
				return
			}
			if at >= 0 {
				t.Fatalf("argv = %v, want no --user: this runtime already maps "+
					"container root onto the engine user", argv)
			}
		})
	}
}

// AN OPERATOR'S OWN run_args WIN, which is the escape hatch for an image that
// genuinely needs root — and it works only because they are spliced last,
// where both runtimes take the final occurrence of a repeated flag.
func TestRunArgsComeAfterEverythingTheBackendChose(t *testing.T) {
	t.Parallel()
	l := &Local{
		opts: LocalOptions{
			Containment: Container, Image: "img",
			Network: "none", RunArgs: []string{"--user", "0:0", "--cap-add", "SYS_PTRACE"},
		},
		runtime: fakeRuntime(t, "docker", dockerRootlessFormat, `[name=seccomp]`, 0),
	}
	argv := l.runArgv(t.Context(), boxLayout{id: "b", root: t.TempDir()}, "crewlet-sbx-b")

	ours := slices.Index(argv, "--user")
	theirs := slices.Index(argv[ours+1:], "--user")
	if ours < 0 || theirs < 0 {
		t.Fatalf("argv = %v, want the backend's --user and then the operator's", argv)
	}
	if argv[ours+1+theirs+1] != "0:0" {
		t.Fatalf("argv = %v; the operator's --user must come last or it cannot override", argv)
	}
	// And the image and its command still close the line, or the operator's
	// flags would be arguments to `sleep`.
	if argv[len(argv)-3] != "img" {
		t.Fatalf("argv = %v, want the image third from the end", argv)
	}
}

// stubBox is a container that shares the box directory faithfully.
type stubBox struct {
	root string
	// token, if set, is what `cat` answers instead of the file's content —
	// a container reading a DIFFERENT filesystem.
	token string
	// asFile creates the probe's directory as a plain file, standing in for
	// a container whose writes the engine cannot live in. Simulated this
	// way rather than by ownership because a test cannot change uids, and
	// the branch it exercises is the same one.
	asFile bool
	// fail makes the exec itself fail.
	fail bool
}

// Exec reports a failure the way a real box does: a non-zero EXIT CODE, never
// a Go error. runHost draws the same line — an error there means the runtime
// could not be run at all, not that the command inside it failed.
func (b *stubBox) Exec(context.Context, string, ExecOptions) (ExecResult, error) {
	out, err := b.run()
	if err != nil {
		//nolint:nilerr // The exit code IS the report; see above.
		return ExecResult{ExitCode: 1, Stderr: err.Error()}, nil
	}
	return ExecResult{Stdout: out}, nil
}

func (b *stubBox) run() (string, error) {
	if b.fail {
		return "", errors.New("no such container")
	}
	out := b.token
	if out == "" {
		blob, err := os.ReadFile(filepath.Join(b.root, ".crewlet", "mount-probe"))
		if err != nil {
			return "", err
		}
		out = string(blob)
	}
	made := filepath.Join(b.root, ".crewlet", "mount-probe.d")
	if b.asFile {
		return out, os.WriteFile(made, nil, 0o600)
	}
	return out, os.MkdirAll(made, 0o700)
}

func TestAFaithfulMountIsAccepted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := verifyMount(t.Context(), &stubBox{root: root}, boxLayout{id: "b", root: root}); err != nil {
		t.Fatalf("verifyMount: %v", err)
	}
	// AND IT LEAVES NOTHING BEHIND: a probe file the agent finds in its own
	// home is a probe that became part of the brief.
	entries, err := os.ReadDir(filepath.Join(root, ".crewlet"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "mount-probe") {
			t.Errorf("the probe left %s behind", entry.Name())
		}
	}
}

// A RUNTIME DRIVING A REMOTE DAEMON bind-mounts the DAEMON's filesystem, so
// the box directory the engine wrote is not the one the container sees. The
// agent would simply find no brief, no credentials and no shim.
func TestAMountOnAnotherHostIsRefused(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	err := verifyMount(t.Context(),
		&stubBox{root: root, token: "somebody else's box"}, boxLayout{id: "b", root: root})
	if err == nil {
		t.Fatal("verifyMount accepted a mount the container cannot see")
	}
	if !strings.Contains(err.Error(), "cannot see its own box directory") {
		t.Fatalf("error = %v; it must name the cause, not the symptom", err)
	}
}

func TestAContainerThatCannotBeReachedIsRefused(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := verifyMount(t.Context(), &stubBox{root: root, fail: true},
		boxLayout{id: "b", root: root}); err == nil {
		t.Fatal("verifyMount accepted a box whose exec failed")
	}
}

// THE ENGINE MUST BE ABLE TO LIVE IN WHAT THE BOX CREATES. Under a rootful
// runtime every directory the container makes in the mount is root-owned, and
// the engine can then neither install the agent's shim into it nor reclaim the
// box's disk — the failure that is otherwise invisible until an operator
// wonders why the host is full of box directories.
func TestABoxDirectoryTheEngineCannotManageIsRefused(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	err := verifyMount(t.Context(),
		&stubBox{root: root, asFile: true}, boxLayout{id: "b", root: root})
	if err == nil {
		t.Fatal("verifyMount accepted a box directory the engine cannot write into")
	}
	if !strings.Contains(err.Error(), "cannot manage") {
		t.Fatalf("error = %v; it must name the cause, not the symptom", err)
	}
	// AND IT SAYS WHAT TO DO ABOUT IT: the operator's only lever is
	// run_args, and an error that does not name it leaves them guessing.
	if !strings.Contains(err.Error(), "run_args") {
		t.Fatalf("error = %v; it must name the field the operator has to change", err)
	}
}

// stubRuntime is a container runtime that shares the box directory the way a
// local daemon does, or — when told to — the way a remote one does not.
//
// It emulates the EFFECT a container has on the mount rather than parsing the
// command line: what these cases are about is what Create does with the answer.
func stubRuntime(t *testing.T, stateDir string, faithful bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
verb="$1"
case "$verb" in
info) printf '%s' '[name=seccomp]'; exit 0 ;;
exec)
  root=""
  for candidate in "` + filepath.Join(stateDir, "boxes") + `"/*; do root="$candidate"; done
  [ -d "$root" ] || exit 1
  if [ "` + fmt.Sprint(faithful) + `" = false ]; then printf 'another host'; exit 0; fi
  cat "$root/.crewlet/mount-probe" || exit 1
  mkdir -p "$root/.crewlet/mount-probe.d" || exit 1
  exit 0 ;;
rm) printf '%s' "$*" >> "` + filepath.Join(t.TempDir(), "removed") + `"; exit 0 ;;
*) exit 0 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// CREATE PROVES THE MOUNT BEFORE IT HANDS THE BOX OUT. Without this the check
// above is a function nothing calls, and both failures it catches are silent
// at the moment they happen.
func TestCreateRefusesABoxWhoseMountIsNotShared(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	l := &Local{
		opts:    LocalOptions{Containment: Container, Image: "img"},
		root:    state,
		runtime: stubRuntime(t, state, false),
	}
	box, err := l.Create(t.Context(), Spec{})
	if err == nil {
		_ = box.Close(t.Context())
		t.Fatal("Create handed out a box the container cannot see")
	}
	if !strings.Contains(err.Error(), "cannot see its own box directory") {
		t.Fatalf("error = %v", err)
	}
	// AND IT TAKES THE BOX WITH IT. A refusal that left the directory (and
	// the container behind it) would leak one per attempt, on the path an
	// operator retries hardest.
	entries, err := os.ReadDir(filepath.Join(state, "boxes"))
	if err == nil && len(entries) > 0 {
		t.Fatalf("the refused box left %d directory(ies) behind", len(entries))
	}
}

func TestCreateAcceptsABoxWhoseMountIsShared(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	l := &Local{
		opts:    LocalOptions{Containment: Container, Image: "img"},
		root:    state,
		runtime: stubRuntime(t, state, true),
	}
	box, err := l.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if box.Home() != DefaultHome {
		t.Fatalf("home = %q, want %q", box.Home(), DefaultHome)
	}
}

// THE FAKE RUNTIME IS EXECUTABLE BY THE TIME fakeRuntime RETURNS.
//
// The guard for [waitExecutable]. Without it this package's own rootless
// probe intermittently could not START the script it had just written —
// Linux refuses to exec a file another process holds open for writing, and a
// goroutine that forks while os.WriteFile has it open inherits exactly that.
// runHost reported a start failure, runtimeIsRootless read that as "not
// rootless", and TestTheContainerRunsAsWhoeverCanManageTheMount failed on an
// argv carrying --user — a result indistinguishable from the detection being
// wrong, which is what makes it worth a test of its own.
//
// The forkers are the point: this reproduces at ~8% without them being
// goroutines of THIS process, and not at all with load applied from outside
// it, because the inherited descriptor is what does the refusing.
func TestAFakeRuntimeIsExecutableWhenItIsHandedOver(t *testing.T) {
	t.Parallel()
	stop := make(chan struct{})
	var forkers sync.WaitGroup
	for range 8 {
		forkers.Add(1)
		go func() {
			defer forkers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = exec.Command("/bin/true").Run() //nolint:noctx // a fork, not a subject
				}
			}
		}()
	}
	t.Cleanup(func() { close(stop); forkers.Wait() })

	var probes sync.WaitGroup
	failures := make(chan string, 64)
	for range 64 {
		probes.Add(1)
		go func() {
			defer probes.Done()
			runtime := fakeRuntime(t, "docker", dockerRootlessFormat,
				`[name=rootless name=seccomp,profile=builtin]`, 0)
			if !runtimeIsRootless(t.Context(), runtime) {
				failures <- runtime
			}
		}()
	}
	probes.Wait()
	close(failures)
	if n := len(failures); n > 0 {
		t.Fatalf("%d of 64 rootless probes could not run the runtime they "+
			"were just handed; every one of them reads downstream as a "+
			"rootful runtime and adds --user", n)
	}
}
