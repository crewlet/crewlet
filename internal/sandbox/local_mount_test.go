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
	writeExecutable(t, path, script)
	return path
}

// writeExecutable writes a script a case is about to exec, where no process
// this one forks can still hold it open.
//
// # A FRESHLY WRITTEN EXECUTABLE CAN BE ETXTBSY, and this package execs them
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
// syscall.ForkLock is what a fork takes for writing, so holding it for
// reading while the file is open keeps every fork of this process out of
// that window: no child can inherit the descriptor, rather than one that did
// being waited out by running the script until it starts. The opencode
// script's suite writes its fake the same way, for the same reason.
func writeExecutable(t *testing.T, path, script string) {
	t.Helper()
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // the case's own program, to be run
		t.Fatal(err)
	}
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
				opts: LocalOptions{Placement: Container, Image: "img"},
				cli:  containerCLI{path: fakeRuntime(t, tc.runtime, tc.format, tc.info, tc.exit)},
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
			Placement: Container, Image: "img",
			Network: "none", RunArgs: []string{"--user", "0:0", "--cap-add", "SYS_PTRACE"},
		},
		cli: containerCLI{path: fakeRuntime(t, "docker", dockerRootlessFormat, `[name=seccomp]`, 0)},
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

// homeOf is the home of the box whose directory is root: what its container
// sees at [DefaultHome].
func homeOf(root string) string { return boxLayout{root: root}.home() }

// stubBox is a container that shares the box's home faithfully.
type stubBox struct {
	// root is the host directory the container sees as its home.
	root string
	// token, if set, is what `cat` answers instead of the file's content —
	// a container reading a DIFFERENT filesystem.
	token string
	// asFile creates the probe's directory as a plain file, standing in for
	// a container whose writes the engine cannot live in. Simulated this
	// way rather than by ownership because a test cannot change uids, and
	// the branch it exercises is the same one.
	asFile bool
	// refused is the runtime refusing the exec itself, with the exit a
	// runtime's own failure has and its own words: the container gone.
	refused bool
	// exit, if set, is the status the probe ends with whatever it read: 124
	// for a probe that ran out of its time.
	exit int
	// execErr is the runtime that could not be run at all.
	execErr error
	// unmade is a box that cannot make a directory in its own home.
	unmade bool
}

// Exec reports a failure the way a real box does: a non-zero EXIT CODE, never
// a Go error. runHost draws the same line — an error there means the runtime
// could not be run at all, not that the command inside it failed. The probe's
// own two exits are the command's: the file it reads not there, and the
// directory it makes not made.
func (b *stubBox) Exec(context.Context, string, ExecOptions) (ExecResult, error) {
	switch {
	case b.execErr != nil:
		return ExecResult{}, b.execErr
	case b.refused:
		return ExecResult{ExitCode: 1,
			Stderr: "Error response from daemon: container crewlet-sbx-b is not running"}, nil
	case b.exit != 0:
		return ExecResult{ExitCode: b.exit, Stderr: "timed out after 10s"}, nil
	}
	out := b.token
	if out == "" {
		blob, err := os.ReadFile(filepath.Join(b.root, ".crewlet", "mount-probe"))
		if err != nil {
			//nolint:nilerr // The exit code IS the report; see above.
			return ExecResult{ExitCode: probeUnread, Stderr: err.Error()}, nil
		}
		out = string(blob)
	}
	if b.unmade {
		return ExecResult{ExitCode: probeUnmade, Stdout: out,
			Stderr: "mkdir: cannot create directory: Permission denied"}, nil
	}
	made := filepath.Join(b.root, ".crewlet", "mount-probe.d")
	if b.asFile {
		if err := os.WriteFile(made, nil, 0o600); err != nil {
			//nolint:nilerr // The exit code IS the report; see above.
			return ExecResult{ExitCode: probeUnmade, Stdout: out, Stderr: err.Error()}, nil
		}
		return ExecResult{Stdout: out}, nil
	}
	if err := os.MkdirAll(made, 0o700); err != nil {
		//nolint:nilerr // The exit code IS the report; see above.
		return ExecResult{ExitCode: probeUnmade, Stdout: out, Stderr: err.Error()}, nil
	}
	return ExecResult{Stdout: out}, nil
}

func TestAFaithfulMountIsAccepted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := verifyMount(t.Context(), &stubBox{root: homeOf(root)}, boxLayout{id: "b", root: root}); err != nil {
		t.Fatalf("verifyMount: %v", err)
	}
	// AND IT LEAVES NOTHING BEHIND: a probe file the agent finds in its own
	// home is a probe that became part of the brief.
	entries, err := os.ReadDir(filepath.Join(homeOf(root), ".crewlet"))
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
		&stubBox{root: homeOf(root), token: "somebody else's box"}, boxLayout{id: "b", root: root})
	if err == nil {
		t.Fatal("verifyMount accepted a mount the container cannot see")
	}
	if !strings.Contains(err.Error(), "cannot see its own box directory") {
		t.Fatalf("error = %v; it must name the cause, not the symptom", err)
	}
	// AND IT IS CLASSED AS THE HOST'S SETUP, which a person changes: the
	// runtime is driving the wrong daemon, and no wait moves it.
	if why, ok := WhyOf(err); !ok || why != WhyConfiguration {
		t.Errorf("a mount on another host is classed %q (%v), want %q", why, ok, WhyConfiguration)
	}
}

// A PROBE THAT NEVER READ ANYTHING IS NOT A MOUNT ON ANOTHER HOST. Each way the
// probe can fail has its own class and its own words: a runtime that refused
// to run it is the runtime's refusal (a configuration, as every refusal of
// the runtime's is), one that ran out of its time is the runtime not
// answering (a wait clears it), a box that could not make a directory in its
// own home is a mount it may not write, and a runtime this host could not
// start at all is the host's own fault, no class. Each was told as a daemon
// on another host, which sent an operator looking for one that was not there.
//
// Mutation: fold any of them back into the remote-daemon message, or class
// the host's own failure, and its row goes red.
func TestAProbeThatNeverReadIsToldApartFromOneThatReadElsewhere(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		box     stubBox
		why     Why
		classed bool
		says    string
	}{
		"a runtime that refused the exec": {box: stubBox{refused: true},
			why: WhyConfiguration, classed: true, says: "refused to run"},
		"a probe that ran out of its time": {box: stubBox{exit: 124},
			why: WhyProviderUnavailable, classed: true, says: "did not answer its mount probe"},
		"a home the box cannot write": {box: stubBox{unmade: true},
			why: WhyConfiguration, classed: true, says: "cannot make a directory in its own home"},
		"a runtime this host cannot run": {box: stubBox{execErr: errors.New(
			`sandbox: starting "/usr/bin/docker": fork/exec: resource temporarily unavailable`)},
			says: "could not run the container runtime"},
	} {
		root := t.TempDir()
		box := c.box
		box.root = homeOf(root)
		err := verifyMount(t.Context(), &box, boxLayout{id: "b", root: root})
		why, classed := WhyOf(err)
		switch {
		case err == nil:
			t.Errorf("%s: verifyMount accepted it", name)
		case strings.Contains(err.Error(), "cannot see its own box directory"):
			t.Errorf("%s: told as a mount on another host: %v", name, err)
		case !strings.Contains(err.Error(), c.says):
			t.Errorf("%s: error = %v; want it to say %q", name, err, c.says)
		case classed != c.classed || why != c.why:
			t.Errorf("%s: classed %q (%v); want %q (%v)", name, why, classed, c.why, c.classed)
		}
	}

	// A PROBE THAT RAN AND FOUND NOTHING THERE TO READ is the usual remote
	// daemon, which bind-mounts a fresh empty directory of its own host: the
	// probe's own exit for it is that, never the runtime refusing the exec.
	// Mutation: read only a probe that read other bytes (exit 0) as a mount
	// elsewhere, and this one is told as a refusal.
	elsewhere := t.TempDir()
	err := verifyMount(t.Context(), &stubBox{root: t.TempDir()}, boxLayout{id: "b", root: elsewhere})
	if why, _ := WhyOf(err); why != WhyConfiguration ||
		!strings.Contains(fmt.Sprint(err), "cannot see its own box directory") {
		t.Errorf("a probe with nothing to read = %v (%q); want a mount on another host, "+
			"a configuration", err, why)
	}

	// AND A CALLER THAT STOPPED WAITING is told as that, never as a probe
	// that ran out of its time.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	root := t.TempDir()
	err = verifyMount(ctx, &stubBox{root: homeOf(root), exit: 124}, boxLayout{id: "b", root: root})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("an abandoned probe = %v; want the caller's own cancellation", err)
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
		&stubBox{root: homeOf(root), asFile: true}, boxLayout{id: "b", root: root})
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
	if why, ok := WhyOf(err); !ok || why != WhyConfiguration {
		t.Errorf("a box directory the engine cannot manage is classed %q (%v), want %q",
			why, ok, WhyConfiguration)
	}
}

// AN IMAGE THE RUNTIME WILL NOT START is the host's setup — the image, the
// runtime, its daemon or the flags — which a person changes; and a directory
// the engine host itself could not make is no class at all, but the node's own
// fault.
//
// Mutation: create the start refusal with localErrorf, and it reads as a fault
// of the node.
func TestAnImageTheRuntimeWillNotStartIsConfiguration(t *testing.T) {
	t.Parallel()
	runtime := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n[ \"$1\" = run ] && { echo 'pull access denied for img' >&2; exit 125; }\nexit 0\n"
	writeExecutable(t, runtime, script)
	l := &Local{
		opts: LocalOptions{Placement: Container, Image: "img"},
		root: t.TempDir(),
		cli:  containerCLI{path: runtime},
	}
	box, err := l.Create(t.Context(), Spec{})
	if err == nil {
		_ = box.Close(t.Context())
		t.Fatal("Create handed out a box whose container never started")
	}
	if !strings.Contains(err.Error(), "pull access denied") {
		t.Errorf("the runtime's own account was dropped: %v", err)
	}
	if why, ok := WhyOf(err); !ok || why != WhyConfiguration {
		t.Errorf("an image the runtime will not start is classed %q (%v), want %q", why, ok,
			WhyConfiguration)
	}
	if why, ok := WhyOf(localErrorf("could not create local sandbox %s", "b")); ok {
		t.Errorf("a directory the host could not make is classed %q", why)
	}
}

// A LAUNCH THIS NODE CANCELLED IS NO CLASS: the runtime's CLI is killed with
// its tree, and the launch is told as abandoned, never as an image the runtime
// would not start. runHost answered a caller's cancellation as its own timeout
// (124), which Create classed a configuration, so a drain was recorded as a
// setup somebody had to fix.
//
// Mutation: answer a cancellation as 124 in runHost, or class whatever Create
// met, and this goes red.
func TestALaunchItsCallerCancelledIsNoClass(t *testing.T) {
	t.Parallel()
	runtime := filepath.Join(t.TempDir(), "docker")
	started := filepath.Join(t.TempDir(), "started")
	script := "#!/bin/sh\n[ \"$1\" = run ] && { : > \"" + started + "\"; exec sleep 60; }\nexit 0\n"
	writeExecutable(t, runtime, script)
	l := &Local{
		opts: LocalOptions{Placement: Container, Image: "img"},
		root: t.TempDir(),
		cli:  containerCLI{path: runtime},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(started); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	box, err := l.Create(ctx, Spec{})
	if err == nil {
		_ = box.Close(t.Context())
		t.Fatal("Create handed out a box its caller cancelled")
	}
	if why, ok := WhyOf(err); ok || !errors.Is(err, context.Canceled) {
		t.Errorf("a launch its caller cancelled = %v, classed %q (%v); want the "+
			"cancellation, unclassed", err, why, ok)
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
  cat "$root/home/.crewlet/mount-probe" || exit 3
  mkdir -p "$root/home/.crewlet/mount-probe.d" || exit 4
  exit 0 ;;
rm) printf '%s' "$*" >> "` + filepath.Join(t.TempDir(), "removed") + `"; exit 0 ;;
*) exit 0 ;;
esac
`
	// Written the way fakeRuntime writes, for the same reason: this script
	// is exec'd by parallel subtests, which fork. It would fail differently
	// here — Create's mount proof cannot start the container at all — and
	// that reads as the proof legitimately refusing a box, which is exactly
	// what these cases assert.
	writeExecutable(t, path, script)
	return path
}

// CREATE PROVES THE MOUNT BEFORE IT HANDS THE BOX OUT. Without this the check
// above is a function nothing calls, and both failures it catches are silent
// at the moment they happen.
func TestCreateRefusesABoxWhoseMountIsNotShared(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	l := &Local{
		opts: LocalOptions{Placement: Container, Image: "img"},
		root: state,
		cli:  containerCLI{path: stubRuntime(t, state, false)},
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
		opts: LocalOptions{Placement: Container, Image: "img"},
		root: state,
		cli:  containerCLI{path: stubRuntime(t, state, true)},
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
// The guard for [writeExecutable]. Without it this package's own rootless
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
		forkers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					_ = exec.Command("/bin/true").Run() //nolint:noctx // a fork, not a subject
				}
			}
		})
	}
	t.Cleanup(func() { close(stop); forkers.Wait() })

	var probes sync.WaitGroup
	failures := make(chan string, 64)
	for range 64 {
		probes.Go(func() {
			runtime := fakeRuntime(t, "docker", dockerRootlessFormat,
				`[name=rootless name=seccomp,profile=builtin]`, 0)
			if !runtimeIsRootless(t.Context(), containerCLI{path: runtime}) {
				failures <- runtime
			}
		})
	}
	probes.Wait()
	close(failures)
	if n := len(failures); n > 0 {
		t.Fatalf("%d of 64 rootless probes could not run the runtime they "+
			"were just handed; every one of them reads downstream as a "+
			"rootful runtime and adds --user", n)
	}
}
