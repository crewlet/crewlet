package sandbox

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// These cases hold the two silent ways a container box's shared directory
// breaks (local_mount.go): a mount on another filesystem, and a container
// writing it as a user the engine cannot manage.

// fakeRuntime writes a script under the runtime's NAME, which picks the
// probe, answering `info` with the given text ONLY for the template that
// runtime understands: a real one fails the wrong template, so a probe asking
// Podman a Docker question must not look like it worked.
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
// this one forks can still hold it open. A child forked while os.WriteFile's
// descriptor is open inherits it until its own exec, and Linux refuses
// (ETXTBSY) to exec a file held open for writing; under -race this package
// forks often enough to hit it, and it reads as the code under test being
// wrong. Holding syscall.ForkLock for reading, which a fork takes for
// writing, keeps every fork out of that window. Every script this package's
// cases exec goes through it, the external suite's too ([WriteExecutable]);
// internal/sandbox/executor writes its stand-ins with cp instead.
func writeExecutable(t *testing.T, path, script string) {
	t.Helper()
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // the case's own program, to be run
		t.Fatal(err)
	}
}

// The template each runtime answers (see runtimeIsRootless).
const (
	dockerRootlessFormat = "{{.SecurityOptions}}"
	podmanRootlessFormat = "{{.Host.Security.Rootless}}"
)

// THE CONTAINER RUNS AS THE ENGINE USER ON A ROOTFUL RUNTIME, and not on a
// rootless one, where --user would map into the subuid range and leave the
// box unable to enter its own 0700 directory.
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
		// Podman's "false" is no Docker list mentioning the word.
		{"rootful podman", "podman", podmanRootlessFormat, `false`, 0, true},
		// An unanswered probe takes the loud side (containerUserArgs).
		// A NON-ZERO EXIT IS NOT AN ANSWER even with stdout: `docker
		// info` against a down daemon prints its client section, which
		// lists `rootlesskit`, then fails.
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

// AN OPERATOR'S OWN run_args WIN, the escape hatch for an image that needs
// root: they are spliced last, and both runtimes take a repeated flag's last
// occurrence.
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
	// The image and its command still close the line, or the operator's
	// flags would be arguments to `sleep`.
	if argv[len(argv)-3] != "img" {
		t.Fatalf("argv = %v, want the image third from the end", argv)
	}
}

// homeOf is the home of the box whose directory is root: what its container
// sees at [DefaultHome].
func homeOf(root string) string { return boxLayout{root: root}.home() }

// madeBox is a box directory with its home made, as Create makes it before the
// container starts and the mount is proven.
func madeBox(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(homeOf(root), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

// stubBox is a container that shares the box's home faithfully.
type stubBox struct {
	// root is the host directory the container sees as its home.
	root string
	// token, if set, is what `cat` answers instead of the file: a container
	// reading a DIFFERENT filesystem.
	token string
	// asFile creates the probe's directory as a plain file: a container
	// whose writes the engine cannot live in, since a test cannot change
	// uids and the branch exercised is the same.
	asFile bool
	// linkTo, if set, makes the probe's directory a link to it: a container
	// that linked the engine's write target out of its home.
	linkTo string
	// refused is the runtime refusing the exec itself: the container gone.
	refused bool
	// exit, if set, is the probe's status whatever it read: 124 for a
	// timeout.
	exit int
	// execErr is the runtime that could not be run at all.
	execErr error
	// unmade is a box that cannot make a directory in its own home.
	unmade bool
}

// Exec reports a failure the way a real box does: a non-zero EXIT CODE, never
// a Go error, which (as in runHost) means the runtime could not run at all.
// The probe's own exits are probeUnread and probeUnmade.
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
	var err error
	switch {
	case b.asFile:
		err = os.WriteFile(made, nil, 0o600)
	case b.linkTo != "":
		err = os.Symlink(b.linkTo, made)
	default:
		err = os.MkdirAll(made, 0o700)
	}
	if err != nil {
		//nolint:nilerr // The exit code IS the report; see above.
		return ExecResult{ExitCode: probeUnmade, Stdout: out, Stderr: err.Error()}, nil
	}
	return ExecResult{Stdout: out}, nil
}

func TestAFaithfulMountIsAccepted(t *testing.T) {
	t.Parallel()
	root := madeBox(t)
	if err := verifyMount(t.Context(), &stubBox{root: homeOf(root)}, boxLayout{id: "b", root: root}); err != nil {
		t.Fatalf("verifyMount: %v", err)
	}
	// AND IT LEAVES NOTHING BEHIND for the agent to find in its home.
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

// A RUNTIME DRIVING A REMOTE DAEMON bind-mounts the daemon's filesystem, so
// the agent would find no brief, no credentials and no shim.
func TestAMountOnAnotherHostIsRefused(t *testing.T) {
	t.Parallel()
	root := madeBox(t)
	err := verifyMount(t.Context(),
		&stubBox{root: homeOf(root), token: "somebody else's box"}, boxLayout{id: "b", root: root})
	if err == nil {
		t.Fatal("verifyMount accepted a mount the container cannot see")
	}
	if !strings.Contains(err.Error(), "cannot see its own box directory") {
		t.Fatalf("error = %v; it must name the cause, not the symptom", err)
	}
	// AND IT IS CLASSED AS THE HOST'S SETUP: no wait moves the wrong daemon.
	if why, ok := WhyOf(err); !ok || why != WhyConfiguration {
		t.Errorf("a mount on another host is classed %q (%v), want %q", why, ok, WhyConfiguration)
	}
}

// A PROBE THAT NEVER READ ANYTHING IS NOT A MOUNT ON ANOTHER HOST: each way the
// probe fails keeps the class and words probeFailed gives it.
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
		root := madeBox(t)
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

	// A PROBE THAT RAN AND FOUND NOTHING TO READ is the usual remote daemon,
	// which mounts an empty directory of its own host; never a refusal.
	// Mutation: read only a probe that read other bytes (exit 0) as a mount
	// elsewhere, and this one is told as a refusal.
	elsewhere := madeBox(t)
	err := verifyMount(t.Context(), &stubBox{root: t.TempDir()}, boxLayout{id: "b", root: elsewhere})
	if why, _ := WhyOf(err); why != WhyConfiguration ||
		!strings.Contains(fmt.Sprint(err), "cannot see its own box directory") {
		t.Errorf("a probe with nothing to read = %v (%q); want a mount on another host, "+
			"a configuration", err, why)
	}

	// AND A CALLER THAT STOPPED WAITING is never told as a timed-out probe.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	root := madeBox(t)
	err = verifyMount(ctx, &stubBox{root: homeOf(root), exit: 124}, boxLayout{id: "b", root: root})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("an abandoned probe = %v; want the caller's own cancellation", err)
	}
}

// THE ENGINE MUST BE ABLE TO LIVE IN WHAT THE BOX CREATES, or under a rootful
// runtime it can neither install the agent's shim nor reclaim the box's disk,
// silently.
func TestABoxDirectoryTheEngineCannotManageIsRefused(t *testing.T) {
	t.Parallel()
	root := madeBox(t)
	err := verifyMount(t.Context(),
		&stubBox{root: homeOf(root), asFile: true}, boxLayout{id: "b", root: root})
	if err == nil {
		t.Fatal("verifyMount accepted a box directory the engine cannot write into")
	}
	if !strings.Contains(err.Error(), "cannot manage") {
		t.Fatalf("error = %v; it must name the cause, not the symptom", err)
	}
	// AND IT NAMES run_args, the operator's only lever.
	if !strings.Contains(err.Error(), "run_args") {
		t.Fatalf("error = %v; it must name the field the operator has to change", err)
	}
	if why, ok := WhyOf(err); !ok || why != WhyConfiguration {
		t.Errorf("a box directory the engine cannot manage is classed %q (%v), want %q",
			why, ok, WhyConfiguration)
	}
}

// AN IMAGE THE RUNTIME WILL NOT START is the host's setup, which a person
// changes; a directory the engine host could not make is the node's own
// fault, no class.
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
// its tree, and the launch is told as abandoned, never as a configuration a
// person must fix (a drain is not one).
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
			time.Sleep(10 * time.Millisecond) //fixedwait:liveness
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

// stubRuntime is a container runtime that shares the box directory as a
// local daemon does, or, unless faithful, as a remote one does not. It
// emulates the EFFECT on the mount rather than parsing the command line:
// these cases are about what Create does with the answer.
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
	// Parallel subtests fork, and ETXTBSY here would read as the mount
	// proof refusing a box, exactly what these cases assert.
	writeExecutable(t, path, script)
	return path
}

// CREATE PROVES THE MOUNT BEFORE IT HANDS THE BOX OUT, or verifyMount is a
// function nothing calls.
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
	// AND IT TAKES THE BOX WITH IT, or every retry leaks a directory and a
	// container.
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

// THE FAKE RUNTIME IS EXECUTABLE BY THE TIME fakeRuntime RETURNS: the guard
// for [writeExecutable], whose ETXTBSY would read as runtimeIsRootless
// answering "not rootless". The forkers must be goroutines of THIS process:
// the inherited descriptor does the refusing, and outside load never does.
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

	// The probes run at once, since their own forks widen the window beside
	// the forkers'. Each is a SUBTEST because fakeRuntime fails through
	// FailNow, which only a test's own goroutine may call; t.Run may be
	// called from many goroutines, so long as all return before this test.
	var probes sync.WaitGroup
	failures := make(chan string, 64)
	for probe := range 64 {
		probes.Go(func() {
			t.Run(fmt.Sprintf("probe-%d", probe), func(t *testing.T) {
				runtime := fakeRuntime(t, "docker", dockerRootlessFormat,
					`[name=rootless name=seccomp,profile=builtin]`, 0)
				if !runtimeIsRootless(t.Context(), containerCLI{path: runtime}) {
					failures <- runtime
				}
			})
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

// EVERY EXECUTABLE THIS SUITE WRITES IS WRITTEN UNDER THE FORK LOCK, by
// writeExecutable, or a fork can make its first exec fail ETXTBSY.
//
// Across this package's test files, the external suite's included, it refuses
// every os.WriteFile, os.OpenFile and os.Chmod outside writeExecutable whose
// mode has an exec bit, and every mode that is not a literal. It requires
// writeExecutable's own write to be found, or a reader that recognised
// nothing would pass on any tree.
//
// Mutation: write the paused-container stub with os.WriteFile(…, 0o700)
// again, and this names the line.
func TestEveryExecutableThisSuiteWritesIsWrittenUnderTheForkLock(t *testing.T) {
	t.Parallel()
	// Where each call takes its mode among its arguments.
	modeAt := map[string]int{"WriteFile": 2, "OpenFile": 2, "Chmod": 1}
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	settled := 0
	for _, name := range files {
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			helper := isFunc && fn.Recv == nil && fn.Name.Name == "writeExecutable"
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				at, writes := modeAt[sel.Sel.Name]
				if !ok || pkg.Name != "os" || !writes || len(call.Args) <= at {
					return true
				}
				where := fset.Position(call.Pos())
				lit, ok := call.Args[at].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					t.Errorf("%s: os.%s with a mode this gate cannot read; spell it as an integer literal",
						where, sel.Sel.Name)
					return true
				}
				mode, err := strconv.ParseUint(lit.Value, 0, 32)
				switch {
				case err != nil:
					t.Errorf("%s: os.%s with a mode this gate cannot read (%v)", where, sel.Sel.Name, err)
				case mode&0o111 == 0:
				case helper:
					settled++
				default:
					t.Errorf("%s: os.%s writes an executable outside writeExecutable, so a fork "+
						"can hold it open at its first exec; write it with writeExecutable",
						where, sel.Sel.Name)
				}
				return true
			})
		}
	}
	if settled != 1 {
		t.Fatalf("found %d executable writes in writeExecutable, want its one os.WriteFile: "+
			"this gate is not reading what it thinks it reads", settled)
	}
}
