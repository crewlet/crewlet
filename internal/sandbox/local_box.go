package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/browser/hostreach"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/shellword"
)

// hostCommand is one command to run on the engine host.
//
// env is the child's WHOLE environment and REQUIRED: [runHost] refuses a nil
// one, since os/exec reads a nil Cmd.Env as "inherit the engine's", every
// credential included. A box's command carries [directBox.childEnv]; the
// container runtime's CLI, [containerCLI.command].
type hostCommand struct {
	argv    []string
	cwd     string
	env     map[string]string
	timeout time.Duration
}

// captureLimit bounds one control command's output in memory, for the
// pathological case (a pull progress bar, an entrypoint flooding stderr), on
// both cells: a direct and a container box's commands ([runHost]). A launched
// job's output never comes through here: it goes to files in the box.
const captureLimit = 256 << 10

// capture is a bounded io.Writer. Once full it keeps the HEAD, because a
// command's first output is its error message and its last is progress noise.
// What it keeps once it dropped anything is held to what its redaction settles
// ([redact.Settled]), so a reader that redacts it later reads an opening of the
// whole output's redaction; the rest is counted and said after a whole line.
type capture struct {
	buf     []byte
	dropped int
}

func (c *capture) Write(p []byte) (int, error) {
	take := min(max(captureLimit-len(c.buf), 0), len(p))
	c.buf = append(c.buf, p[:take]...)
	c.dropped += len(p) - take
	return len(p), nil
}

func (c *capture) String() string {
	if c.dropped == 0 {
		return string(c.buf)
	}
	kept := string(c.buf)
	kept = kept[:redact.Settled(kept)]
	note := fmt.Sprintf("(%d more bytes of output not kept: past the %d KiB a control command's output may hold)",
		c.dropped+len(c.buf)-len(kept), captureLimit>>10)
	if kept = strings.TrimRight(kept, "\n"); kept == "" {
		return note
	}
	return kept + "\n" + note
}

// containerCLI is the container runtime's CLI as this backend drives it: the
// binary [ResolveContainerRuntime] picked and the variables an operator
// declared for it in providers.sandbox.local.runtime_env, already resolved.
// Every runtime call is built from it ([containerCLI.command]), so none runs
// with os/exec's default: the engine's environment, keyring and Tier A tokens.
type containerCLI struct {
	path     string
	declared map[string]string
}

// command is one control command for the runtime's CLI (`run`, `exec`,
// `pause`, `rm`, `ps`, `info`), its arguments following the binary.
//
// Its environment is [hostbox.ContainerRuntime] with the declared variables
// over it: never the engine's ([hostbox]), and not the fixed rule alone, since
// what a runtime's credential helpers and plugins read is no closed list
// (docker-credential-ecr-login wants its cloud's role or key file). Declared
// wins: an operator's value is a decision, the host's a default.
//
// The run environment a box gets is NOT here but in an --env-file
// ([containerBox.envArgs]), so a bare `-e NAME` in `local.run_args` copies only
// a name this environment carries, as the configuration's advisory says.
func (c containerCLI) command(timeout time.Duration, args ...string) hostCommand {
	env := hostbox.ContainerRuntime()
	maps.Copy(env, c.declared)
	return hostCommand{
		argv:    append([]string{c.path}, args...),
		env:     env,
		timeout: timeout,
	}
}

// podman reports whether the runtime is Podman rather than Docker, told apart
// by the binary's name ([ResolveContainerRuntime] picks one of the two).
func (c containerCLI) podman() bool { return filepath.Base(c.path) == "podman" }

// hostAlias is the name the engine host answers to inside a container this
// runtime starts: each runtime's own documented name, so an image or a run
// that already uses it keeps working ([EngineAddress]).
func (c containerCLI) hostAlias() string {
	if c.podman() {
		return podmanHostAlias
	}
	return dockerHostAlias
}

// hostAliasArgs are the `run` arguments that make [containerCLI.hostAlias]
// resolve, or nothing where the runtime defines it itself.
//
// Docker defines it only on Docker Desktop: Docker Engine on Linux needs
// `host-gateway`, the daemon's spelling of the host's address on the
// container's network (Docker 20.10+), which on Desktop names the same address.
// Podman writes host.containers.internal into every container's hosts file.
func (c containerCLI) hostAliasArgs() []string {
	if c.podman() {
		return nil
	}
	return []string{"--add-host", dockerHostAlias + ":host-gateway"}
}

const (
	dockerHostAlias = "host.docker.internal"
	podmanHostAlias = "host.containers.internal"
)

// ---------------------------------------------------------------------
// direct
// ---------------------------------------------------------------------

// directBox is a [Sandbox] that is a process tree on the engine host.
//
// It holds NO handle on the job it started, by design: a detached run is torn
// down by a later process, possibly after a restart, so the only addressable
// thing is the job record ([jobGroup]).
type directBox struct {
	layout      boxLayout
	env         map[string]string
	credentials map[string]seededCredential
	// readCap is the whole-read cap the suite gave the box; zero, every
	// production box, is [MaxFileBytes] ([readLimit]).
	readCap int
}

var _ Sandbox = (*directBox)(nil)

// argv is a shell command line as this box runs it: ahead of the engine for
// the kernel's OOM killer ([oomFirst]). Every setup and exec command and the
// detached job start here. Its first word is the absolute /bin/sh, so the
// raise searches no PATH of the box's.
func (b *directBox) argv(cmd string) []string {
	return oomFirst([]string{"/bin/sh", "-c", cmd})
}

func (b *directBox) ID() string   { return b.layout.id }
func (b *directBox) Home() string { return b.layout.home() }

// TempRootEnv is every variable a process in a host box takes its temporary
// directory from, each pointed at the box's own .tmp.
//
// ONE LIST, so no process in the box writes a temporary file outside it.
// TMPDIR alone is not enough: Python's tempfile reads TMP and TEMP, and a
// harness may root its own temporary directory at a variable of its own or
// else a literal /tmp (its built-in profile's executor.temp_env), each set for
// every box since a reconnected box knows no harness. A container needs none
// of this: its /tmp is inside the boundary.
var TempRootEnv = sync.OnceValue(func() []string {
	env := []string{"TMPDIR", "TMP", "TEMP"}
	for _, name := range harnessprofile.ExecutorRunners() {
		if runner, _ := harnessprofile.ExecutorOf(name); runner.TempEnv != "" && !slices.Contains(env, runner.TempEnv) {
			env = append(env, runner.TempEnv)
		}
	}
	return env
})

// childEnv is the allowlisted host environment ([hostbox.Inherit]) plus the
// box's home, its temporary directory and the run env. Allowlisted because the
// engine's holds the chat token, the collector credential
// (OTEL_EXPORTER_OTLP_HEADERS) and possibly a metered API key.
func (b *directBox) childEnv(extra map[string]string) map[string]string {
	env := hostbox.Inherit()
	home := b.layout.home()
	env["HOME"] = home
	env["XDG_CONFIG_HOME"] = filepath.Join(home, ".config")
	env["XDG_DATA_HOME"] = filepath.Join(home, ".local", "share")
	env["XDG_STATE_HOME"] = filepath.Join(home, ".local", "state")
	env["XDG_CACHE_HOME"] = filepath.Join(home, ".cache")
	for _, name := range TempRootEnv() {
		env[name] = b.layout.tmp()
	}
	for key, value := range b.env {
		env[key] = value
	}
	for key, value := range extra {
		env[key] = value
	}
	return env
}

// workdir resolves a command's cwd, defaulting to the box's checkout, makes it
// through the box's home ([boxLayout.mkdirHome]) and answers it relative to
// the home.
//
// THE ENGINE NEVER ENTERS IT: a command starts in the box's home, which the
// box cannot rename, and its own shell enters the directory by its relative
// name ([inWorkdir]). Walked by host path on the engine's side, a link the box
// swapped in on the way would move where its command starts; walked by the
// box's process, it is the box's walk.
func (b *directBox) workdir(cwd string) (string, error) {
	named, rel := b.layout.workspace(), WorkspaceSubdir
	if cwd != "" {
		resolved, err := b.homeRel(cwd)
		if err != nil {
			return "", err
		}
		named, rel = cwd, resolved
	}
	if err := b.layout.mkdirHome(rel); err != nil {
		return "", b.explain(named, rel, localErrorf("local sandbox %s could not create %s: %v",
			b.layout.id, filepath.Join(b.layout.home(), rel), err))
	}
	return rel, nil
}

// inWorkdir is cmd run in rel, the working directory relative to the box's
// home that the command's shell enters first ([directBox.workdir]): a shell
// that cannot enter it runs nothing and fails as the command.
func inWorkdir(rel, cmd string) string {
	if rel == "" || rel == "." {
		return cmd
	}
	return "cd -- " + shellword.Quote(rel) + " || exit 1\n" + cmd
}

func (b *directBox) Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
	rel, err := b.workdir(opts.Cwd)
	if err != nil {
		return ExecResult{}, err
	}
	return runHost(ctx, hostCommand{
		argv:    b.argv(inWorkdir(rel, cmd)),
		cwd:     b.layout.home(),
		env:     b.childEnv(opts.Env),
		timeout: time.Duration(opts.TimeoutSec * float64(time.Second)),
	})
}

// StartBackground spawns a launch's detached job in its own session.
//
// Spawned directly, not backgrounded in a shell, so the pid leads its own
// session ([procgroup.Detach]): one signal reaches the tree, no terminal's
// Ctrl-C does, and it survives the engine's process (not a stopped container
// or a rebooted host, which the box's witness tells from a crash,
// local_witness.go); and the engine holds it unreaped, so its identity is read
// before the pid can be reissued.
//
// The [reap] goroutine is required: Go reaps a child only on Wait, and kill(0)
// reads a zombie as ALIVE, so the runner's completion probe would never see
// the job end. Its stdio is /dev/null: the runner's script redirects output.
func (b *directBox) StartBackground(ctx context.Context, cmd string, opts ExecOptions) (string, error) {
	rel, err := b.workdir(opts.Cwd)
	if err != nil {
		return "", err
	}
	// Not exec.CommandContext: the job must outlive the turn that starts it.
	argv := b.argv(inWorkdir(rel, cmd))
	proc := exec.Command(argv[0], argv[1:]...) //nolint:noctx // deliberate; see above
	proc.Dir = b.layout.home()
	proc.Env = hostbox.Environ(b.childEnv(opts.Env))
	procgroup.Detach(proc)
	// nil stdio is os/exec's /dev/null, which is what we want: nothing reads
	// these, and a pipe nobody drains would block the agent on a full buffer.
	proc.Stdin, proc.Stdout, proc.Stderr = nil, nil, nil

	if err = proc.Start(); err != nil {
		return "", localErrorf("local sandbox %s could not start a background job: %v", b.layout.id, err)
	}
	pid := proc.Process.Pid

	// IDENTIFIED BEFORE ANYTHING CAN REAP IT: until Wait runs, the leader keeps
	// its kernel record and its pid cannot be reissued, so this is the one moment
	// its start time is certainly this job's.
	leader, err := procgroup.Identify(pid)
	if err == nil {
		err = recordLeader(b.layout, leader)
	}
	if err != nil {
		// Without the record no later process can reach this job's group, so it
		// would run unkillable. Kill it now, while the unreaped leader still makes
		// the group certainly the job's.
		logSignal("kill", pid, procgroup.Kill(pid))
		go reap(proc)
		return "", localErrorf("local sandbox %s could not record its job's process group: %v", b.layout.id, err)
	}
	go reap(proc)
	// THIS PROCESS SAW IT START, and is its witness until another process
	// of this engine finds it running ([witnessJob]).
	witnessJob(b.layout)
	localLog.Info("local_sandbox_job_started", "sandbox_id", b.layout.id, "pid", pid)
	return strconv.Itoa(pid), nil
}

// Job implements [Sandbox] for a job on the engine host.
//
// The handle is a HOST pid, so it is this job only while the box's record
// names it and the kernel's record of that pid carries the recorded start
// time; a handle the record no longer names is a job this box replaced, so
// ENDED. A zombie is not running. The process, not its group, is asked: a
// wrapper that died without writing its marker is a run that is over, even
// while an orphan of its group lingers.
//
// A job not running ended or was lost, as the box's witness says ([jobEnd]);
// one found running makes this process its witness.
func (b *directBox) Job(ctx context.Context, commandID string) (JobState, error) {
	pid, err := strconv.Atoi(commandID)
	if err != nil || pid <= 1 {
		return "", localErrorf("local sandbox %s: job handle %q is not a process id", b.layout.id, commandID)
	}
	leader, found, err := readJobRecord(b.layout)
	if err != nil {
		return "", err
	}
	if !found || leader.PID != pid {
		return JobEnded, nil
	}
	proc, found, err := procgroup.Inspect(pid)
	if err != nil {
		return "", fmt.Errorf("local sandbox %s: reading the kernel's record of job %d: %w", b.layout.id, pid, err)
	}
	if found && !proc.Zombie && proc.Start == leader.Start {
		witnessJob(b.layout)
		return JobRunning, nil
	}
	state, err := jobEnd(b.layout)
	if err != nil {
		return "", fmt.Errorf("local sandbox %s: %w", b.layout.id, err)
	}
	return state, nil
}

// EndJob implements [Sandbox] for a job on the engine host. script is a HOST
// pid the job wrote for itself, and the job runs as the engine's user, so it
// could name any of that user's pids, the engine's included: the request is
// held to the recorded process group ([procgroup.Leader.SignalMember]), and a
// pid outside it, or a handle the record no longer names, is
// [ErrNotJobProcess] and is sent nothing.
func (b *directBox) EndJob(ctx context.Context, commandID string, script int) error {
	pid, err := strconv.Atoi(commandID)
	if err != nil || pid <= 1 {
		return localErrorf("local sandbox %s: job handle %q is not a process id", b.layout.id, commandID)
	}
	if script < 2 {
		return fmt.Errorf("%w: pid %d is no process an end request is sent to", ErrNotJobProcess, script)
	}
	leader, found, err := readJobRecord(b.layout)
	if err != nil {
		return fmt.Errorf("local sandbox %s: %w", b.layout.id, err)
	}
	if !found || leader.PID != pid {
		return fmt.Errorf("%w: local sandbox %s records no job %s", ErrNotJobProcess, b.layout.id, commandID)
	}
	if err := askEnd(leader, script); err != nil {
		if errors.Is(err, procgroup.ErrNotMember) {
			return fmt.Errorf("%w: local sandbox %s: %w", ErrNotJobProcess, b.layout.id, err)
		}
		return fmt.Errorf("local sandbox %s: asking pid %d to end its job: %w", b.layout.id, script, err)
	}
	return nil
}

// KillJob implements [Sandbox] for a job on the engine host: its recorded
// group, killed as a teardown kills it ([killJobGroup]), the box left for the
// read that follows. A handle the record no longer names is a job already
// over, nothing to kill: the group recorded now is another job's.
func (b *directBox) KillJob(ctx context.Context, commandID string) error {
	pid, err := strconv.Atoi(commandID)
	if err != nil || pid <= 1 {
		return localErrorf("local sandbox %s: job handle %q is not a process id", b.layout.id, commandID)
	}
	leader, found, err := readJobRecord(b.layout)
	if err != nil {
		return fmt.Errorf("local sandbox %s: %w", b.layout.id, err)
	}
	if !found || leader.PID != pid {
		return nil
	}
	killJobGroup(ctx, b.layout)
	return nil
}

// reap waits on a detached job so it does not linger as a zombie. Its exit
// status is discarded: the runner reads the job's outcome from its marker and
// result files.
func reap(proc *exec.Cmd) { _ = proc.Wait() }

// homeRel maps an in-box path onto the box's home: the path relative to it,
// which every access then makes through the home opened as an os.Root
// ([boxLayout.openHome]).
//
// Direct mode does NOT virtualise the filesystem, so an in-box path IS a host
// path, and a setup step provisioning a system path is refused rather than
// writing the host's real /usr/local/bin. Container mode refuses it too
// ([containerBox.homeRel]); what works in every cell is the box's home.
// Where a path lands is [hostbox.InHome]'s answer, the one config validation
// gives a setup step's files, so the two cannot disagree; where its links lead
// is the root's to decide at the access ([directBox.explain]).
func (b *directBox) homeRel(path string) (string, error) {
	// An absolute path naming somewhere in the box is the ordinary case, written
	// as the home reads or as its links resolve.
	rel, inside := hostbox.InHome(b.layout.home(), path)
	if !inside && filepath.IsAbs(path) {
		if root, err := filepath.EvalSymlinks(b.layout.home()); err == nil {
			rel, inside = hostbox.InHome(root, path)
		}
	}
	if !inside {
		return "", b.escapeError(path)
	}
	return rel, nil
}

// explain is a failed access to path (rel in the box's home) as this box
// reports it. The root decided the refusal ([boxLayout.openHome]); this only
// says why: where rel leads as the host's links stand now ([hostbox.SafeJoin])
// tells an escape ([directBox.escapeError]) from a path that cannot be
// resolved at all (one under a file, say, which reported as an escape sends
// its reader hunting a symlink). Any other failure (absence, not a regular
// file, past its cap) is the access's own and passes through.
func (b *directBox) explain(path, rel string, err error) error {
	if err == nil || absent(err) || errors.Is(err, ErrNotRegularFile) || errors.Is(err, ErrFileTooLarge) {
		return err
	}
	switch _, check := hostbox.SafeJoin(b.layout.home(), rel); {
	case errors.Is(check, hostbox.ErrEscape):
		return b.escapeError(path)
	case check != nil:
		return localErrorf("local sandbox %s could not resolve %q: %v", b.layout.id, path, check)
	}
	return err
}

func (b *directBox) escapeError(path string) error {
	return localErrorf("local sandbox (run_in %q) refuses to touch %q: it is outside "+
		"the box at %s. Direct mode has no filesystem virtualisation, so this would write to "+
		"the engine host itself. Put the file under the box's home: name it relative to the "+
		"home (\".npmrc\", \".local/bin/tool\"), which every cell writes under its home, or "+
		"have a setup command write it under \"$HOME\".", Direct, path, b.layout.home())
}

func (b *directBox) WriteFile(ctx context.Context, path string, content []byte) error {
	return b.WriteStream(ctx, path, bytes.NewReader(content))
}

// WriteStream implements [Sandbox]: the write [directBox.WriteFile] makes, its
// content read a piece at a time into the file staged beside the path
// ([writeHomeFile]).
func (b *directBox) WriteStream(ctx context.Context, path string, content io.Reader) error {
	rel, err := b.homeRel(path)
	if err != nil {
		return err
	}
	if err := b.layout.mkdirHome(filepath.Dir(rel)); err != nil {
		return b.explain(path, rel, localErrorf("local sandbox %s could not create %s: %v",
			b.layout.id, filepath.Join(b.layout.home(), filepath.Dir(rel)), err))
	}
	return b.explain(path, rel, writeHomeFile(b.layout, rel, path, readUnder(ctx, content)))
}

// ReadFile is EMPTY-ON-MISSING: the detached runner polls for marker and
// result files that do not exist until the job finishes, and a poll is not an
// error.
func (b *directBox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	// A RESOLVE failure is raised, unlike a missing file: the path left the
	// box, and "empty" would report a refused traversal as a file not there
	// yet, the one reading a poller acts on.
	rel, err := b.homeRel(path)
	if err != nil {
		return nil, err
	}
	content, err := readHomeFile(b.layout, rel, path, readLimit(b.readCap))
	return content, b.explain(path, rel, err)
}

// OpenWhole implements [Sandbox] for a box on the engine host: the file
// opened once and its size read off that open ([openHomeWhole]).
func (b *directBox) OpenWhole(ctx context.Context, path string) (io.ReadCloser, int64, error) {
	rel, err := b.homeRel(path)
	if err != nil {
		return nil, 0, err
	}
	r, size, err := openHomeWhole(b.layout, rel, path, readLimit(b.readCap))
	return r, size, b.explain(path, rel, err)
}

// OpenFile implements [Sandbox] for a box on the engine host: the file itself,
// read in place, after the same escape check every other path takes.
func (b *directBox) OpenFile(ctx context.Context, path string) (io.ReadCloser, error) {
	rel, err := b.homeRel(path)
	if err != nil {
		return nil, err
	}
	r, err := openHomeFile(b.layout, rel, path)
	return r, b.explain(path, rel, err)
}

// ReadTail implements [Sandbox] for a box on the engine host: a seek, so the
// cost is what is read rather than how long the run has been writing.
func (b *directBox) ReadTail(ctx context.Context, path string, n int) (FileTail, error) {
	rel, err := b.homeRel(path)
	if err != nil {
		return FileTail{}, err
	}
	tail, err := readHomeTail(b.layout, rel, path, n)
	return tail, b.explain(path, rel, err)
}

// OpenTail implements [Sandbox] for a box on the engine host: a seek, then the
// section to the end the open found.
func (b *directBox) OpenTail(ctx context.Context, path string, n int64) (io.ReadCloser, int64, error) {
	rel, err := b.homeRel(path)
	if err != nil {
		return nil, 0, err
	}
	r, size, err := openHomeTail(b.layout, rel, path, n)
	return r, size, b.explain(path, rel, err)
}

// OpenRange implements [Sandbox] for a box on the engine host: a seek, then the
// section the open found ([openHomeRange]).
func (b *directBox) OpenRange(ctx context.Context, path string, off, n int64) (io.ReadCloser, int64, error) {
	if err := rangeRefused(path, off, n); err != nil {
		return nil, 0, err
	}
	rel, err := b.homeRel(path)
	if err != nil {
		return nil, 0, err
	}
	r, size, err := openHomeRange(b.layout, rel, path, off, n)
	return r, size, b.explain(path, rel, err)
}

// Touch refreshes the box's keepalive stamp ([boxLayout.aliveFile]): the
// orphan reaper reclaims a box that records neither a run nor a check and whose
// job is gone once its stamp ages past the cutoff, so a box being provisioned,
// which has no job yet, is touched by its launch, and a running one per poll by
// the waiter.
func (b *directBox) Touch(ctx context.Context) error {
	touchAlive(b.layout)
	return nil
}

// Pause SIGSTOPs the job's process group, resumed on Connect. A paused box
// still holds its directory and whatever of the group is left in RAM, which
// is why the waiter's pause reaper bounds it.
func (b *directBox) Pause(ctx context.Context) error {
	if signalJob(b.layout, "stop", procgroup.Stop) {
		localLog.Debug("local_sandbox_paused", "sandbox_id", b.layout.id)
	}
	return nil
}

// resume SIGCONTs a paused box — the Connect auto-resume.
func (b *directBox) resume() {
	signalJob(b.layout, "continue", procgroup.Continue)
}

// Close kills the job's process group ([killJobGroup], which continues a
// stopped tree first so it can handle SIGTERM), syncs credentials and removes
// the box.
func (b *directBox) Close(ctx context.Context) error {
	killJobGroup(ctx, b.layout)
	collectCredentials(ctx, b.layout, b.credentials)
	removeBox(b.layout)
	localLog.Debug("local_sandbox_closed", "sandbox_id", b.layout.id)
	return nil
}

// ---------------------------------------------------------------------
// container
// ---------------------------------------------------------------------

// containerBox is a [Sandbox] backed by a long-lived container. The box's home
// (never its records, [boxLayout]) is bind-mounted at [DefaultHome], so file
// access happens on the HOST side of the mount, with no copy through the
// runtime.
type containerBox struct {
	layout      boxLayout
	cli         containerCLI
	container   string
	env         map[string]string
	credentials map[string]seededCredential
	// readCap is as [directBox]'s.
	readCap int
}

var _ Sandbox = (*containerBox)(nil)

func (b *containerBox) ID() string   { return b.layout.id }
func (b *containerBox) Home() string { return DefaultHome }

// EngineHost is the name the engine host answers to inside this container:
// its loopback is the container's own ([EngineAddress]).
func (b *containerBox) EngineHost() string { return b.cli.hostAlias() }

// EngineKernel is the kernel this box's runtime runs on, as the launch's
// check of the box's network holds it ([onEngineHost]): the engine host's own,
// since the runtime's CLI runs there, and the box shows whether it runs on it.
func (b *containerBox) EngineKernel() (hostreach.Kernel, error) { return hostreach.EngineKernel() }

func (b *containerBox) workdir() string { return DefaultHome + "/" + WorkspaceSubdir }

// homeRel maps an in-container path onto the host side of the mount: the path
// relative to the box's home, accessed through the home as an os.Root
// ([boxLayout.openHome]).
//
// An escape here writes to the engine host, not the container, and a prefix
// test does not stop "/home/user/../../etc/cron.d/x" (setup-step paths are
// operator config). Where a path lands is
// [hostbox.InHome]'s answer, as config validation gives it; where its links
// lead is the root's to decide at the access ([containerBox.explain]).
func (b *containerBox) homeRel(path string) (string, error) {
	rel, inside := hostbox.InHome(DefaultHome, path)
	if !inside {
		// Not under the mount, so no write from here reaches it. A command in the
		// container might, but only as its user (the engine's uid on a rootful
		// runtime, [containerUserArgs]), whom system directories usually refuse,
		// so the remedy that holds is the home.
		return "", localErrorf("%q is not a file under the sandbox home mount at %s, the only "+
			"place a file can be written into this box; name it relative to the home "+
			"(\".npmrc\", \".local/bin/tool\"), which every cell writes under its home, or have a "+
			"setup command write it under \"$HOME\"", path, DefaultHome)
	}
	return rel, nil
}

// explain is a failed access to path (rel in the box's home) as this box
// reports it, as [directBox.explain] does.
func (b *containerBox) explain(path, rel string, err error) error {
	if err == nil || absent(err) || errors.Is(err, ErrNotRegularFile) || errors.Is(err, ErrFileTooLarge) {
		return err
	}
	switch _, check := hostbox.SafeJoin(b.layout.home(), rel); {
	case errors.Is(check, hostbox.ErrEscape):
		return localErrorf("%q resolves outside the sandbox home mount at %s — it would be "+
			"written to the engine host itself", path, b.layout.home())
	case check != nil:
		// Not an escape — see [directBox.explain].
		return localErrorf("%q could not be resolved under the sandbox home mount at %s: %v",
			path, b.layout.home(), check)
	}
	return err
}

// envArgs renders the run env as --env-file arguments.
//
// NOT "-e KEY=value": argv is world-readable (/proc/<pid>/cmdline, ps), and
// this env carries the seat's LLM key and role.sandbox.env's tokens. The file
// is one of the box's RECORDS (0700 directory, 0600 file), beside its home and
// never in it: inside the mount the job could swap it between execs for a link
// the rewrite would follow onto a host file, or a pipe whose open never returns.
// Rewritten per call because extra differs between the setup steps and the
// launched job, and a stale file would hand one phase another's environment.
func (b *containerBox) envArgs(extra map[string]string) ([]string, error) {
	merged := make(map[string]string, len(b.env)+len(extra))
	for key, value := range b.env {
		merged[key] = value
	}
	for key, value := range extra {
		merged[key] = value
	}
	if len(merged) == 0 {
		return nil, nil
	}
	// --env-file is line-oriented KEY=value with no quoting, so a newline in a
	// value would forge a variable: such a value is dropped LOUDLY, never
	// truncated.
	var lines []string
	for _, assignment := range hostbox.Environ(merged) {
		key, value, _ := strings.Cut(assignment, "=")
		if strings.ContainsAny(value, "\n\r") {
			localLog.Warn("local_sandbox_env_var_unrepresentable", "sandbox_id", b.layout.id, "var", key)
			continue
		}
		lines = append(lines, assignment)
	}
	if err := os.MkdirAll(b.layout.records(), hostbox.DirMode); err != nil {
		return nil, localErrorf("container sandbox %s could not write its env file: %v", b.layout.id, err)
	}
	blob := strings.Join(lines, "\n") + "\n"
	if err := writeRecord(b.layout, envRecord, []byte(blob)); err != nil {
		return nil, localErrorf("container sandbox %s could not write its env file: %v", b.layout.id, err)
	}
	return []string{"--env-file", b.layout.envFile()}, nil
}

func (b *containerBox) execArgv(cmd string, opts ExecOptions) ([]string, error) {
	cwd := opts.Cwd
	if cwd == "" {
		cwd = b.workdir()
	}
	args := []string{"exec", "-w", cwd}
	envArgs, err := b.envArgs(opts.Env)
	if err != nil {
		return nil, err
	}
	args = append(args, envArgs...)
	return append(args, b.container, "/bin/sh", "-c", cmd), nil
}

func (b *containerBox) Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
	args, err := b.execArgv(cmd, opts)
	if err != nil {
		return ExecResult{}, err
	}
	return runHost(ctx, b.cli.command(
		time.Duration(opts.TimeoutSec*float64(time.Second)), args...))
}

// StartBackground backgrounds the job inside the container and echoes its pid,
// so `docker exec` returns at once while the job runs under the container's
// PID 1, started with --init to reap it: kill(0) reads a zombie as ALIVE.
//
// It also RECORDS THE JOB'S PROCESS GROUP ([boxLayout.groupFile]), which only
// this exec can name: runc makes an exec's first process a session leader, and
// a backgrounded job stays in its shell's group, so one signal to that group
// reaches the whole job and spares the container's main process, as
// [containerBox.KillJob] must. The shell exits at once, so $! leads no group
// and only the kernel's word read here names it. A runtime whose exec leads no
// session of its own records none ([containerBox.recordGroup]). Direct mode
// spawns the job in its own session and needs none of this.
func (b *containerBox) StartBackground(ctx context.Context, cmd string, opts ExecOptions) (string, error) {
	// THE LAST JOB'S GROUP GOES BEFORE THIS ONE STARTS, so a start failing part
	// way leaves this job with none, never with that one's.
	if err := removeRecord(b.layout, groupRecord); err != nil {
		localLog.Warn("local_sandbox_job_group_unremoved", "sandbox_id", b.layout.id, "error", err.Error())
	}
	args, err := b.execArgv(cmd+" & "+groupReport, opts)
	if err != nil {
		return "", err
	}
	result, err := runHost(ctx, b.cli.command(0, args...))
	if err != nil {
		return "", err
	}
	pid := trailingPID(result.Stdout)
	if pid == "" {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(result.Stdout)
		}
		return "", localErrorf("container sandbox %s could not start a background job: %s",
			b.layout.id, detail)
	}
	b.recordGroup(result.Stdout, pid)
	// THIS PROCESS SAW IT START, as for a direct box ([witnessJob]).
	witnessJob(b.layout)
	localLog.Info("local_sandbox_job_started",
		"sandbox_id", b.layout.id, "pid", pid, "container", b.container)
	return pid, nil
}

// Job implements [Sandbox]: the handle is a pid in the container, whose --init
// reaps a finished job, so `kill -0` there answers it once the runtime says
// the container is up.
//
// THE RUNTIME IS ASKED FIRST ([containerBox.state]): an exec that found no job
// and one the runtime could not run (no daemon yet after a reboot) both exit
// non-zero, and read as the job's answer a briefly absent daemon would end a
// running job, which a restarted engine ([jobEnd]) takes as lost and runs
// again beside it. A gone or stopped container ran its job down with it, so
// the box's witness decides ended or lost. A PAUSED container takes no exec:
// an error, which [Local.Attach] keeps the poll from reaching.
func (b *containerBox) Job(ctx context.Context, commandID string) (JobState, error) {
	state, err := b.state(ctx)
	if err != nil {
		return "", err
	}
	switch state {
	case "paused":
		return "", localErrorf("container sandbox %s is paused, and a paused container's job "+
			"cannot be asked about without unpausing it", b.layout.id)
	case "running":
		running, probeErr := probeByKill(ctx, b, commandID)
		if probeErr != nil {
			return "", probeErr
		}
		if running {
			witnessJob(b.layout)
			return JobRunning, nil
		}
	}
	ended, err := jobEnd(b.layout)
	if err != nil {
		return "", fmt.Errorf("container sandbox %s: %w", b.layout.id, err)
	}
	return ended, nil
}

// state is the container's state as its runtime lists it ("running",
// "paused", "exited" and so on, lower-cased), "" where it lists no container of
// this name; an error is a runtime that did not answer.
//
// A LISTING, NOT `inspect`, which fails for a missing container as for a
// missing runtime. The filter matches by substring, so the exact name is read.
func (b *containerBox) state(ctx context.Context) (string, error) {
	res, err := runHost(ctx, b.cli.command(0, "ps", "-a",
		"--filter", "name="+b.container, "--format", "{{.Names}}\t{{.State}}"))
	if err != nil {
		return "", localErrorf("container sandbox %s: listing its container: %v", b.layout.id, err)
	}
	if res.ExitCode != 0 {
		return "", localErrorf("container sandbox %s: listing its container failed with exit %d: %s",
			b.layout.id, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	for line := range strings.SplitSeq(res.Stdout, "\n") {
		name, state, _ := strings.Cut(strings.TrimSpace(line), "\t")
		if strings.TrimSpace(name) == b.container {
			return strings.ToLower(strings.TrimSpace(state)), nil
		}
	}
	return "", nil
}

// groupReport is what a container box's exec prints after backgrounding the
// job ([containerBox.StartBackground]): a line naming the exec shell's pid and
// the job's process group (field 5 of /proc/<pid>/stat, counted after the LAST
// ") " since the command name is the process's to choose), then the job's pid
// alone on the last line for [trailingPID]. An unreadable stat names no group.
const groupReport = `p=$!; read -r s < /proc/$p/stat; s=${s##*) }; set -- $s; echo "` + groupLine + ` $$ $3"; echo "$p"`

// groupLine opens [groupReport]'s line.
const groupLine = "crewlet-group"

// reportedGroup is the exec shell's pid and the job's process group that
// [groupReport] printed, false where it printed no line naming both.
func reportedGroup(output string) (shell, group int, ok bool) {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		fields := strings.Fields(lines[i])
		if len(fields) == 0 || fields[0] != groupLine {
			continue
		}
		if len(fields) != 3 {
			return 0, 0, false
		}
		shell, shellErr := strconv.Atoi(fields[1])
		group, groupErr := strconv.Atoi(fields[2])
		return shell, group, shellErr == nil && groupErr == nil
	}
	return 0, 0, false
}

// recordGroup records the job's process group as its exec reported it
// ([groupReport]), only where that group is the exec shell's own; otherwise it
// logs that none was recorded, since [containerBox.KillJob] can then name the
// group only where an exec's own session leads it.
func (b *containerBox) recordGroup(output, pid string) {
	shell, group, ok := reportedGroup(output)
	if !ok || group != shell || group < 2 {
		localLog.Info("local_sandbox_job_group_unrecorded", "sandbox_id", b.layout.id, "pid", pid,
			"detail", "the container's exec did not lead a process group of its own, so a kill of the "+
				"job signals nothing of it unless an exec's own session leads the group its first "+
				"process is in, and the box's reclaim ends it")
		return
	}
	if err := writeRecord(b.layout, groupRecord, []byte(strconv.Itoa(group)+" "+pid)); err != nil {
		localLog.Warn("local_sandbox_job_group_unrecorded", "sandbox_id", b.layout.id, "pid", pid,
			"error", err.Error())
	}
}

// EndJob implements [Sandbox]: script is a pid in the container's namespace,
// signalled there ([endByKill]) from the box's home, since an exec's working
// directory must exist and the job can remove its checkout but not the home.
func (b *containerBox) EndJob(ctx context.Context, _ string, script int) error {
	return endByKill(ctx, b, script)
}

// KillJob implements [Sandbox] in the container: the job's own process group,
// sparing the container's main process, which the following read execs beside:
// the group recorded at start ([containerJobGroup]), or else the one its first
// process is in where an exec's own session leads it ([killFirstProcessGroup]).
//
// A JOB WHOSE GROUP NEITHER NAMES IS SIGNALLED NOTHING ([ErrNoJobGroup]), never
// its first process alone: that is the runner's wrapper, whose death reads as
// the job ended ([containerBox.Job]) while its script and harness, children a
// signal to it does not reach, go on writing to a session taken as final. Left
// alone, the job reads as running until the box's reclaim ends it.
func (b *containerBox) KillJob(ctx context.Context, commandID string) error {
	pid, err := strconv.Atoi(commandID)
	if err != nil || pid <= 1 {
		return localErrorf("container sandbox %s: job handle %q is not a process id", b.layout.id, commandID)
	}
	script := killFirstProcessGroup(pid)
	if group, ok := containerJobGroup(b.layout, pid); ok {
		script = killScript("-" + strconv.Itoa(group))
	}
	res, err := b.Exec(ctx, script, ExecOptions{Cwd: DefaultHome})
	if err != nil {
		return fmt.Errorf("container sandbox %s: ending its job: %w", b.layout.id, err)
	}
	if lastLine(res.Stdout) == killUngrouped {
		return fmt.Errorf("container sandbox %s: job %d recorded no process group as it started, and its "+
			"first process is gone or in a group no exec's own session leads: %w", b.layout.id, pid, ErrNoJobGroup)
	}
	if res.ExitCode != 0 {
		return localErrorf("container sandbox %s: ending its job failed with exit %d: %s",
			b.layout.id, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// killUngrouped is what [killFirstProcessGroup] prints where it names no
// group, having signalled nothing.
const killUngrouped = "crewlet-kill-ungrouped"

// killFirstProcessGroup is the script that kills, in the container, the
// process group a job's first process (pid) is in, read off the kernel at the
// kill, for a job whose start recorded none. Only where that group is one an
// exec's own session leads (its id equal to the session's, fields 5 and 6 of
// /proc/<pid>/stat counted as [groupReport] counts; 2 or more; not the killing
// shell's own): runc's group for every exec, never the container's main
// process's. Anywhere else it prints [killUngrouped] and signals nothing: a
// first process gone, a runtime whose exec leads no session (the group reads
// as 0), a group of a session another process leads.
//
// READ AND SIGNALLED IN ONE EXEC: between two execs the pid could be reissued.
func killFirstProcessGroup(pid int) string {
	return fmt.Sprintf(`if read -r s < /proc/%d/stat; then s=${s##*) }; set -- $s; fi 2> /dev/null
if [ "${3:-0}" = "${4:-}" ] && [ "$3" -ge 2 ] 2> /dev/null && [ "$3" != "$$" ]; then g=-$3; else echo %s; exit 0; fi
`, pid, killUngrouped) + killScript(`"$g"`)
}

// trailingPID is the last numeric line of output, the backgrounded job's pid:
// the LAST, because a shell may print a job-control line first and the echo
// comes last.
func trailingPID(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(lines[i])
		if candidate == "" {
			continue
		}
		if _, err := strconv.Atoi(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func (b *containerBox) WriteFile(ctx context.Context, path string, content []byte) error {
	return b.WriteStream(ctx, path, bytes.NewReader(content))
}

// WriteStream implements [Sandbox]: on the host side of the mount, as every
// other access to the box's files is.
func (b *containerBox) WriteStream(ctx context.Context, path string, content io.Reader) error {
	rel, err := b.homeRel(path)
	if err != nil {
		return err
	}
	if err := b.layout.mkdirHome(filepath.Dir(rel)); err != nil {
		return b.explain(path, rel, localErrorf("container sandbox %s could not create %s: %v",
			b.layout.id, filepath.Join(b.layout.home(), filepath.Dir(rel)), err))
	}
	return b.explain(path, rel, writeHomeFile(b.layout, rel, path, readUnder(ctx, content)))
}

func (b *containerBox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	// An escaped path is an error, a file not there yet empty, as for
	// [directBox.ReadFile].
	rel, err := b.homeRel(path)
	if err != nil {
		return nil, err
	}
	content, err := readHomeFile(b.layout, rel, path, readLimit(b.readCap))
	return content, b.explain(path, rel, err)
}

// OpenWhole implements [Sandbox]: the host side of the mount, opened once.
func (b *containerBox) OpenWhole(ctx context.Context, path string) (io.ReadCloser, int64, error) {
	rel, err := b.homeRel(path)
	if err != nil {
		return nil, 0, err
	}
	r, size, err := openHomeWhole(b.layout, rel, path, readLimit(b.readCap))
	return r, size, b.explain(path, rel, err)
}

// OpenFile implements [Sandbox]: the host side of the mount, read in place.
func (b *containerBox) OpenFile(ctx context.Context, path string) (io.ReadCloser, error) {
	rel, err := b.homeRel(path)
	if err != nil {
		return nil, err
	}
	r, err := openHomeFile(b.layout, rel, path)
	return r, b.explain(path, rel, err)
}

// ReadTail implements [Sandbox]: a seek on the host side of the mount.
func (b *containerBox) ReadTail(ctx context.Context, path string, n int) (FileTail, error) {
	rel, err := b.homeRel(path)
	if err != nil {
		return FileTail{}, err
	}
	tail, err := readHomeTail(b.layout, rel, path, n)
	return tail, b.explain(path, rel, err)
}

// OpenTail implements [Sandbox]: a seek on the host side of the mount.
func (b *containerBox) OpenTail(ctx context.Context, path string, n int64) (io.ReadCloser, int64, error) {
	rel, err := b.homeRel(path)
	if err != nil {
		return nil, 0, err
	}
	r, size, err := openHomeTail(b.layout, rel, path, n)
	return r, size, b.explain(path, rel, err)
}

// OpenRange implements [Sandbox]: a seek on the host side of the mount.
func (b *containerBox) OpenRange(ctx context.Context, path string, off, n int64) (io.ReadCloser, int64, error) {
	if err := rangeRefused(path, off, n); err != nil {
		return nil, 0, err
	}
	rel, err := b.homeRel(path)
	if err != nil {
		return nil, 0, err
	}
	r, size, err := openHomeRange(b.layout, rel, path, off, n)
	return r, size, b.explain(path, rel, err)
}

// openHomeFile is a local box's OpenFile once the path is mapped onto its home:
// the file, or a reader yielding nothing for one not there yet.
//
// ONLY ABSENCE IS EMPTY: a stream's reader decides what the run did from what
// it reads, so an unreadable event log answered empty would settle as a run
// that produced nothing. Anything not a regular file is an error too
// ([openRegularIn]). The home is closed once the file is open; the open file
// holds no path anything could move.
func openHomeFile(l boxLayout, rel, path string) (io.ReadCloser, error) {
	f, err := openHomeRegular(l, rel)
	switch {
	case absent(err):
		return io.NopCloser(strings.NewReader("")), nil
	case err != nil:
		return nil, fmt.Errorf("local sandbox: open %s: %w", path, err)
	}
	return f, nil
}

// openHomeTail is a local box's OpenTail once the path is mapped onto its
// home: the file's last n bytes as its size stood at the open, a SECTION for
// [readHomeTail]'s reason, closed with the file.
func openHomeTail(l boxLayout, rel, path string, n int64) (io.ReadCloser, int64, error) {
	f, err := openHomeRegular(l, rel)
	switch {
	case absent(err):
		return io.NopCloser(strings.NewReader("")), 0, nil
	case err != nil:
		return nil, 0, fmt.Errorf("local sandbox: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("local sandbox: stat %s: %w", path, err)
	}
	size := info.Size()
	start := max(size-max(n, 0), 0)
	return hostSection{Reader: io.NewSectionReader(f, start, size-start), Closer: f}, size, nil
}

// openHomeRange is a local box's OpenRange once the path is mapped onto its
// home: at most n bytes from off of the file as its size stood at the open, a
// SECTION for [readHomeTail]'s reason, closed with the file. An offset at or
// past that size is nothing, with the size.
func openHomeRange(l boxLayout, rel, path string, off, n int64) (io.ReadCloser, int64, error) {
	f, err := openHomeRegular(l, rel)
	switch {
	case absent(err):
		return io.NopCloser(strings.NewReader("")), 0, nil
	case err != nil:
		return nil, 0, fmt.Errorf("local sandbox: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("local sandbox: stat %s: %w", path, err)
	}
	size := info.Size()
	if off >= size {
		_ = f.Close()
		return io.NopCloser(strings.NewReader("")), size, nil
	}
	return hostSection{Reader: io.NewSectionReader(f, off, min(n, size-off)), Closer: f}, size, nil
}

// hostSection is a section of a file on the engine host, closed with it.
type hostSection struct {
	io.Reader
	io.Closer
}

// readHomeTail is a local box's ReadTail once the path is mapped onto its home.
// A SECTION of the file, never a read to EOF, which a job still writing could
// have grown past what was asked; what arrived in the window counts into the
// size, so Data never claims more of the file than the file was.
func readHomeTail(l boxLayout, rel, path string, n int) (FileTail, error) {
	f, err := openHomeRegular(l, rel)
	switch {
	case absent(err):
		return FileTail{}, nil
	case err != nil:
		return FileTail{}, fmt.Errorf("local sandbox: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return FileTail{}, fmt.Errorf("local sandbox: stat %s: %w", path, err)
	}
	want := int64(max(n, 0))
	start := max(info.Size()-want, 0)
	data, err := io.ReadAll(io.NewSectionReader(f, start, want))
	if err != nil {
		return FileTail{}, fmt.Errorf("local sandbox: read %s: %w", path, err)
	}
	return FileTail{Data: data, Size: max(info.Size(), start+int64(len(data)))}, nil
}

// readHomeFile is a local box's ReadFile once the path is mapped onto its home:
// empty for a file not there, and REFUSED past limit (the box's [readLimit])
// for [readCapped]'s reason: the job's output would otherwise land in the
// memory of the engine on its host.
//
// ONLY ABSENCE IS EMPTY, as for [openHomeFile]: a findings report the engine
// may not read (a container writing its mount as another user) answered empty
// would collect as a run that wrote none; an error is what a collection
// retries and a person can act on.
func readHomeFile(l boxLayout, rel, path string, limit int) ([]byte, error) {
	f, err := openHomeRegular(l, rel)
	switch {
	case absent(err):
		// Empty-on-missing is the contract ([directBox.ReadFile]).
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("local sandbox: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	content, err := readCapped(f, path, limit)
	if err != nil && !errors.Is(err, ErrFileTooLarge) {
		return nil, fmt.Errorf("local sandbox: read %s: %w", path, err)
	}
	return content, err
}

// openHomeWhole is a local box's OpenWhole once the path is mapped onto its
// home: the file's size from a stat of the open file, never of the path, and
// a SECTION to that size, so bytes written after the open are not read.
// Refused past limit as [readHomeFile] refuses it, the file closed.
func openHomeWhole(l boxLayout, rel, path string, limit int) (io.ReadCloser, int64, error) {
	f, err := openHomeRegular(l, rel)
	switch {
	case absent(err):
		return io.NopCloser(strings.NewReader("")), 0, nil
	case err != nil:
		return nil, 0, fmt.Errorf("local sandbox: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("local sandbox: stat %s: %w", path, err)
	}
	if size := info.Size(); size > int64(limit) {
		_ = f.Close()
		return nil, 0, &FileTooLargeError{Path: path, Limit: limit, Size: size}
	}
	return hostSection{Reader: io.NewSectionReader(f, 0, info.Size()), Closer: f}, info.Size(), nil
}

// writeHomeFile is a local box's WriteFile once the path is mapped onto its
// home and its directory made: the file at rel REPLACED whole ([replaceIn]),
// so a process reading it meanwhile (the git credential helper reading the
// token file) finds the old file or the new, never a part-written one; no
// component of the path leads out of the box; and anything at rel that is not
// a regular file is refused as a [NotRegularFileError] and left as it was. A
// source failing part way leaves rel as it was ([hostbox.ReplaceFileIn]).
func writeHomeFile(l boxLayout, rel, path string, content io.Reader) error {
	home, err := l.openHome()
	if err != nil {
		return fmt.Errorf("local sandbox: open %s for writing: %w", path, err)
	}
	defer func() { _ = home.Close() }()
	if err := replaceFrom(home, rel, content); err != nil {
		return fmt.Errorf("local sandbox: write %s: %w", path, err)
	}
	return nil
}

// openHomeRegular opens rel in l's home for reading, as [openRegularIn] opens
// a file: a home that is not there is a file that is not there.
func openHomeRegular(l boxLayout, rel string) (*os.File, error) {
	home, err := l.openHome()
	if err != nil {
		return nil, err
	}
	defer func() { _ = home.Close() }()
	return openRegularIn(home, rel)
}

// absent is whether an open failed because there is no file at the path —
// the one failure a read answers as an empty file.
func absent(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// Touch refreshes the box's keepalive stamp — see [directBox.Touch].
func (b *containerBox) Touch(ctx context.Context) error {
	touchAlive(b.layout)
	return nil
}

func (b *containerBox) Pause(ctx context.Context) error {
	result, err := runHost(ctx, b.cli.command(0, "pause", b.container))
	if err != nil || result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if err != nil {
			detail = err.Error()
		}
		localLog.Warn("local_sandbox_pause_failed", "sandbox_id", b.layout.id, "error", detail)
	}
	return nil
}

// unpause resumes a paused container, best-effort: an already-running one
// reports an error that is ignored, so Connect makes one unconditional call.
func (b *containerBox) unpause(ctx context.Context) {
	_, _ = runHost(ctx, b.cli.command(0, "unpause", b.container))
}

// paused reports whether the runtime says the container is paused, by its
// `.State.Paused` (the same on Docker and Podman), read without changing it.
// Anything but a plain "true" is not a pause, a failed inspection included:
// see [Local.Attach].
func (b *containerBox) paused(ctx context.Context) bool {
	res, err := runHost(ctx, b.cli.command(0, "inspect", "--format", "{{.State.Paused}}", b.container))
	return err == nil && res.ExitCode == 0 && strings.TrimSpace(res.Stdout) == "true"
}

// Close removes the container, then writes back a login the run refreshed and
// removes the box; a container its runtime keeps is an error, and the box is
// left whole ([containerBox.remove]).
func (b *containerBox) Close(ctx context.Context) error {
	if err := b.remove(ctx); err != nil {
		return err
	}
	collectCredentials(ctx, b.layout, b.credentials)
	removeBox(b.layout)
	localLog.Debug("local_sandbox_closed", "sandbox_id", b.layout.id)
	return nil
}

// remove removes the box's container: nil once its runtime lists it no more,
// `rm -f` done, or failed for one already gone (Podman refuses a missing one)
// by the box's own listing ([containerBox.state]); an error, with the
// runtime's words, for one still listed or not said to be gone. ON AN ERROR
// THE CALLER KEEPS THE BOX: its directory holds the record its reclaimer lists
// it by ([recordedRun]), and a box deleted beside its container is one nothing
// finds again, since every reclaimer walks the box directories. A removal that
// timed out is not asked after, which would wait as long again on the runtime
// that let it.
func (b *containerBox) remove(ctx context.Context) error {
	res, err := runHost(ctx, b.cli.command(0, "rm", "-f", b.container))
	if err == nil && res.ExitCode == 0 {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		// 124 is runHost's own timeout or the caller's deadline.
		if res.ExitCode != 124 {
			switch state, listErr := b.state(ctx); {
			case listErr == nil && state == "":
				return nil
			case listErr == nil:
				err = fmt.Errorf("%w; the runtime still lists it %s", err, state)
			default:
				err = fmt.Errorf("%w; and whether it is gone is unknown: %w", err, listErr)
			}
		}
	}
	return fmt.Errorf("sandbox: container %s was not removed, so box %s is kept for its next reclaim, "+
		"which tries again; if the runtime keeps refusing, remove it with `%s rm -f %s`: %w",
		b.container, b.layout.id, filepath.Base(b.cli.path), b.container, err)
}

// discard removes a box [Local.Create] will not hand out, its container
// first, under a context its caller's cancellation does not reach, since the
// failure it undoes is often that cancellation. A container the runtime keeps
// keeps the box, record and all, for the reclaimer its record names.
func (b *containerBox) discard(ctx context.Context) {
	if kept := b.remove(context.WithoutCancel(ctx)); kept != nil {
		localLog.Warn("local_sandbox_container_kept", "sandbox_id", b.layout.id, "error", kept.Error())
		return
	}
	_ = os.RemoveAll(b.layout.root)
}

// ---------------------------------------------------------------------
// container runtime
// ---------------------------------------------------------------------

// ResolveContainerRuntime picks the container CLI to drive.
//
// "auto" prefers Docker and falls back to Podman — Docker because it is the
// overwhelmingly common one, Podman because it is the rootless default on
// Fedora and RHEL and takes the same subcommands.
func ResolveContainerRuntime(preference string) (string, error) {
	switch preference {
	case "docker", "podman":
		found, err := exec.LookPath(preference)
		if err != nil {
			return "", localErrorf("providers.sandbox.local.runtime is %q but that command is "+
				"not on the engine host's PATH", preference)
		}
		return found, nil
	case "", "auto":
		for _, candidate := range []string{"docker", "podman"} {
			if found, err := exec.LookPath(candidate); err == nil {
				return found, nil
			}
		}
		// NO RUNTIME FIELD IN THE REMEDY: each value is looked up on this same
		// PATH, so it could not find what this loop did not. The directory is a
		// remedy because a service manager may start the engine with a narrower
		// PATH than its operator's login shell.
		return "", localErrorf("run_in %q is set but neither "+
			"docker nor podman is on the engine host's PATH. Install one, put the "+
			"directory that holds it on that PATH, or use run_in %q", Container, Direct)
	default:
		return "", localErrorf("providers.sandbox.local.runtime %q is not one of "+
			`"auto", "docker" or "podman"`, preference)
	}
}
