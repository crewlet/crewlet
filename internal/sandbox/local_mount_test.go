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
	"testing"

	"github.com/crewlet/crewlet/internal/hostbox"
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
	writeRuntime(t, path, fmt.Sprintf(`[ "$1" = info ] || exit 0
[ "$3" = '%s' ] || { echo "template error: $3" >&2; exit 125; }
printf '%%s' '%s'
exit %d
`, wantFormat, info, exit))
	return path
}

// settleProbe is the argument every runtime stub answers by exiting at once:
// writeRuntime puts that answer ahead of each stub's body and settles the stub
// with it, so settling runs none of a body and leaves nothing in a log a case
// reads back. The engine never sends it, so the line answers nothing a case
// asserts on.
const settleProbe = "--settle-probe"

// writeRuntime writes a runtime stub at path — `#!/bin/sh`, the answer to
// [settleProbe], then body — and returns once the stub can be exec'd, through
// [hostbox.Settle], the one wait for a freshly written executable. It is the
// ONE way this package writes an executable, and
// TestEveryExecutableThisSuiteWritesIsSettled holds it to that, so no stub can
// skip the wait.
//
// This package forks constantly, and a goroutine that forks while
// os.WriteFile has a script open holds it open for writing until its child
// execs, so the script's first exec can fail ETXTBSY. The symptom is never an
// error anyone can read, because every caller of the runtime reads a start
// failure as an answer: runtimeIsRootless as "not rootless", so the argv
// carries --user; Create's mount proof as a refusal of the box; and
// containerBox.paused as "not paused", so an attach to a paused container
// succeeds. Each reads as the code under test being wrong — or, for the mount
// proof, as exactly the refusal its cases assert.
func writeRuntime(t *testing.T, path, body string) {
	t.Helper()
	script := "#!/bin/sh\nif [ \"$1\" = " + settleProbe + " ]; then exit 0; fi\n" + body
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := hostbox.Settle(t.Context(), path, settleProbe); err != nil {
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
			argv := l.runArgv(t.Context(), boxLayout{id: "b", root: t.TempDir()}, "crewlet-sbx-b", nil)
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
	argv := l.runArgv(t.Context(), boxLayout{id: "b", root: t.TempDir()}, "crewlet-sbx-b", nil)

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
}

func TestAContainerThatCannotBeReachedIsRefused(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := verifyMount(t.Context(), &stubBox{root: homeOf(root), fail: true},
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
}

// stubRuntime is a container runtime that shares the box directory the way a
// local daemon does, or — when told to — the way a remote one does not.
//
// It emulates the EFFECT a container has on the mount rather than parsing the
// command line: what these cases are about is what Create does with the answer.
func stubRuntime(t *testing.T, stateDir string, faithful bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker")
	writeRuntime(t, path, `verb="$1"
case "$verb" in
info) printf '%s' '[name=seccomp]'; exit 0 ;;
exec)
  root=""
  for candidate in "`+filepath.Join(stateDir, "boxes")+`"/*; do root="$candidate"; done
  [ -d "$root" ] || exit 1
  if [ "`+fmt.Sprint(faithful)+`" = false ]; then printf 'another host'; exit 0; fi
  cat "$root/home/.crewlet/mount-probe" || exit 1
  mkdir -p "$root/home/.crewlet/mount-probe.d" || exit 1
  exit 0 ;;
rm) printf '%s' "$*" >> "`+filepath.Join(t.TempDir(), "removed")+`"; exit 0 ;;
*) exit 0 ;;
esac
`)
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

// A LOCAL BOX IS GIVEN ONLY THE MOUNTS ITS CELL TAKES. A direct box refuses one
// and leaves nothing behind. A container's host directory must be one before
// the runtime is asked for anything — a runtime handed a path that does not
// exist makes it, as root on a rootful one, and mounts it empty — and its
// mounts follow the home on the runtime's command line, each `-v host:box`,
// `:ro` where it is read-only, ahead of the operator's run_args.
//
// Mutations: drop the cell's rule from Create — a direct box is made, red;
// drop the host directory's check — the missing directory's box is made, red;
// drop `:ro` — red.
func TestALocalBoxIsGivenOnlyTheMountsItsCellTakes(t *testing.T) {
	t.Parallel()
	host := t.TempDir()
	mount := Mount{Host: host, Box: "/crewlet/borrowed/web/objects", ReadOnly: true}
	noBoxes := func(t *testing.T, state string) {
		t.Helper()
		if entries, err := os.ReadDir(filepath.Join(state, "boxes")); err == nil && len(entries) != 0 {
			t.Errorf("a refused box left %d directories behind", len(entries))
		}
	}

	state := t.TempDir()
	direct, err := NewLocal(LocalOptions{Placement: Direct, StateDir: state})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if box, err := direct.Create(t.Context(), Spec{Mounts: []Mount{mount}}); err == nil {
		_ = box.Close(context.WithoutCancel(t.Context()))
		t.Error("a direct box was made with a mount")
	} else if !strings.Contains(err.Error(), "the direct cell takes no mount") {
		t.Errorf("a direct box with a mount: %v, want it refused naming the cell", err)
	}
	noBoxes(t, state)

	state = t.TempDir()
	l := &Local{
		opts: LocalOptions{Placement: Container, Image: "img"},
		root: state,
		cli:  containerCLI{path: stubRuntime(t, state, true)},
	}
	file := filepath.Join(host, "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, at := range map[string]string{"gone": filepath.Join(host, "gone"), "a file": file} {
		missing := Mount{Host: at, Box: "/crewlet/objects", ReadOnly: true}
		if box, err := l.Create(t.Context(), Spec{Mounts: []Mount{missing}}); err == nil {
			_ = box.Close(context.WithoutCancel(t.Context()))
			t.Errorf("a container was made with a mount of a host path that is %s", name)
		} else if !strings.Contains(err.Error(), "cannot be mounted") {
			t.Errorf("a mount of a host path that is %s: %v, want it refused", name, err)
		}
	}
	noBoxes(t, state)
	box, err := l.Create(t.Context(), Spec{Mounts: []Mount{mount}})
	if err != nil {
		t.Fatalf("a container with a mount of a directory: %v", err)
	}
	_ = box.Close(context.WithoutCancel(t.Context()))

	scratch := t.TempDir()
	argvOf := &Local{
		opts: LocalOptions{Placement: Container, Image: "img", RunArgs: []string{"--cpus", "2"}},
		cli:  containerCLI{path: fakeRuntime(t, "docker", dockerRootlessFormat, `[name=seccomp]`, 0)},
	}
	layout := boxLayout{id: "b", root: t.TempDir()}
	argv := argvOf.runArgv(t.Context(), layout, "crewlet-sbx-b",
		[]Mount{mount, {Host: scratch, Box: "/crewlet/scratch"}})
	at := slices.Index(argv, layout.home()+":"+DefaultHome)
	want := []string{"-v", host + ":/crewlet/borrowed/web/objects:ro", "-v", scratch + ":/crewlet/scratch"}
	if at < 1 || argv[at-1] != "-v" || len(argv) < at+1+len(want) || !slices.Equal(argv[at+1:at+1+len(want)], want) {
		t.Fatalf("argv = %q, want the home's mount followed by %q", argv, want)
	}
	if cpus := slices.Index(argv, "--cpus"); cpus < at+len(want) {
		t.Errorf("argv = %q, want the operator's run_args after the mounts", argv)
	}
}

// A RUNTIME STUB IS EXECUTABLE BY THE TIME writeRuntime RETURNS.
//
// The guard for [writeRuntime], driven through fakeRuntime. Without the wait
// this package's own rootless probe intermittently could not START the script
// it had just written — Linux refuses to exec a file another process holds
// open for writing, and a goroutine that forks while os.WriteFile has it open
// inherits exactly that. runHost reported a start failure, runtimeIsRootless
// read that as "not rootless", and
// TestTheContainerRunsAsWhoeverCanManageTheMount failed on an argv carrying
// --user — a result indistinguishable from the detection being wrong, which
// is what makes it worth a test of its own.
//
// The forkers are the point: with eight of them forking as goroutines of THIS
// process, a just-written script's exec fails ETXTBSY about 8% of the time
// (17 of 200 execs), and not at all under load applied from outside the
// process, because the inherited descriptor is what does the refusing.
func TestARuntimeStubIsExecutableWhenItIsHandedOver(t *testing.T) {
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

	// Each probe is a SUBTEST, started from a goroutine of its own. The
	// probes run at once because their own forks hold the window open
	// beside the forkers' — with the wait removed, and counting whole runs
	// of this 64-probe test under -race, probing one at a time let 4 runs
	// in 20 pass, and probing at once 2 in 40 — and each is a subtest
	// because fakeRuntime fails through FailNow, which only a test's own
	// goroutine may call: from a goroutine of this test's, it ended that
	// goroutine and marked this test finished while it ran on. t.Run,
	// unlike FailNow, may be called from many goroutines at once, so long
	// as every call returns before this test does.
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

// EVERY EXECUTABLE THIS SUITE WRITES IS SETTLED, because writeRuntime writes
// every one. A stub written any other way skips the wait, and its first exec
// fails ETXTBSY a few runs in a few hundred, as an answer that reads as the
// code under test being wrong: the paused-container case's stub was written
// that way while the two beside it were settled, and with eight goroutines
// forking beside it, 3 of its 200 runs read a paused container as running.
//
// It reads this package's own test files and refuses every os.WriteFile,
// os.OpenFile and os.Chmod outside writeRuntime whose mode carries an exec
// bit — and every mode it cannot read as a literal, which it names rather
// than assumes harmless. It requires the one write writeRuntime makes to be
// found as well, or a reader that recognised nothing would pass on any tree.
//
// Mutation: write the paused-container stub with os.WriteFile(…, 0o700)
// again, and this names the line.
func TestEveryExecutableThisSuiteWritesIsSettled(t *testing.T) {
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
			helper := isFunc && fn.Recv == nil && fn.Name.Name == "writeRuntime"
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
					t.Errorf("%s: os.%s writes an executable outside writeRuntime, so nothing "+
						"waits for it to settle before its first exec; write it with writeRuntime",
						where, sel.Sel.Name)
				}
				return true
			})
		}
	}
	if settled != 1 {
		t.Fatalf("found %d executable writes in writeRuntime, want its one os.WriteFile: "+
			"this gate is not reading what it thinks it reads", settled)
	}
}
