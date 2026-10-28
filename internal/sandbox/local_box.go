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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/procgroup"
)

// hostCommand is one command to run on the engine host.
//
// Its env is the child's WHOLE environment and it is REQUIRED: [runHost]
// refuses a nil one, because os/exec reads a nil Cmd.Env as "inherit the
// engine's" — the keyring, the Tier A tokens and every credential the engine
// was started with. A box's own command carries [directBox.childEnv]; a
// command for the container runtime's CLI is built by [containerCLI.command].
type hostCommand struct {
	argv    []string
	cwd     string
	env     map[string]string
	timeout time.Duration
}

// captureLimit bounds what one control command's output may cost in memory.
//
// A control command produces a line or two; this exists for the pathological
// case — a runtime that streams a pull progress bar, an image whose entrypoint
// floods stderr — where an unbounded buffer would be the engine's memory. The
// coding job's own output does not come through here at all: it is redirected
// to files inside the box and read back by the runner.
const captureLimit = 256 << 10

// capture is a bounded io.Writer. Once full it keeps the HEAD, because a
// command's first output is its error message and its last is progress noise.
//
// What it drops is COUNTED and said, in whole lines. It used to mark only a
// write that found the buffer already full, so one write crossing the limit —
// the whole output of a command that printed it at once — was clipped with no
// marker at all; and the clip landed wherever the byte count did, mid-line and
// mid-character, which a JSON encoder turns into U+FFFD.
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
	kept, dropped := c.buf, c.dropped
	// Back to the end of the last whole line, so the note follows a line
	// rather than half of one; a buffer with no line break at all keeps
	// itself, held to a whole character.
	if end := bytes.LastIndexByte(kept, '\n'); end >= 0 {
		dropped += len(kept) - end - 1
		kept = kept[:end+1]
	} else {
		whole := wholeRunes(kept)
		dropped += len(kept) - len(whole)
		kept = whole
	}
	return fmt.Sprintf("%s\n(%d more bytes of output not kept: past the %d KiB a control "+
		"command's output may hold)", strings.TrimRight(string(kept), "\n"), dropped, captureLimit>>10)
}

// wholeRunes is b without a character the limit split at its end.
func wholeRunes(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && len(b)-i <= utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			return b
		}
	}
	return b
}

// containerCLI is the container runtime's own CLI as this backend drives it:
// the binary [ResolveContainerRuntime] picked, and the variables an operator
// DECLARED for it in providers.sandbox.local.runtime_env, already resolved.
//
// A VALUE every runtime call is built from rather than a binary path each call
// site pairs with an environment it remembers: every one of these was a
// hostCommand with no environment at all, which os/exec filled with the
// engine's, so the runtime's CLI — and every plugin and credential helper it
// runs — was handed the keyring and the Tier A tokens.
type containerCLI struct {
	path     string
	declared map[string]string
}

// command is one control command for the runtime's CLI — `run`, `exec`,
// `pause`, `rm`, `ps`, `info` — its arguments following the binary.
//
// ITS ENVIRONMENT IS [hostbox.ContainerRuntime] WITH THE DECLARED VARIABLES
// OVER IT. Not the engine's, for the reason no child gets that (see
// [hostbox]); and not the fixed rule alone either, because what a runtime's
// helpers legitimately read is not a list anybody can close — the credential
// helper that pulls the image from a private registry wants its cloud's role,
// profile or key file (docker-credential-ecr-login, docker-credential-gcr),
// and a plugin its own settings — and a fixed list with no
// way to add to it leaves an operator whose runtime needs one of those with
// no way to say so. The declared layer wins over the fixed one, because a
// value an operator wrote down for this CLI is a decision and the host's is a
// default.
//
// The run environment a coding agent gets is NOT here: it travels as an
// --env-file inside the box ([containerBox.envArgs]), so what this CLI holds
// is what the runtime needs and nothing a box does. A bare `-e NAME` in
// `local.run_args` therefore copies only a name this environment carries,
// which is what the configuration's advisory about one says.
func (c containerCLI) command(timeout time.Duration, args ...string) hostCommand {
	env := hostbox.ContainerRuntime()
	maps.Copy(env, c.declared)
	return hostCommand{
		argv:    append([]string{c.path}, args...),
		env:     env,
		timeout: timeout,
	}
}

// podman reports whether the runtime is Podman rather than Docker: the two
// answer a few questions differently, and the binary's name is how this
// backend tells them apart ([ResolveContainerRuntime] picks one of the two).
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
// resolve, or nothing where the runtime defines it on its own.
//
// DOCKER DEFINES IT ONLY ON DOCKER DESKTOP. Docker Engine on Linux leaves
// host.docker.internal unresolved unless the container is started with it,
// and `host-gateway` is the daemon's own spelling of the host's address on
// the container's network (Docker 20.10 and later); on Docker Desktop it
// names the same address Desktop's own entry does. Podman writes
// host.containers.internal into every container's hosts file itself, with the
// address its network mode needs, so nothing is added there.
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
// IT HOLDS NO HANDLE ON THE JOB IT STARTED, and that is the design rather
// than an omission: a detached run outlives the turn that started it and is
// torn down by a LATER process, possibly after a restart, so the only
// addressable thing is the job record. Close reads it (see [jobGroup]); an
// *exec.Cmd on the struct could only ever be right in the one process that
// happened to start the job.
type directBox struct {
	layout      boxLayout
	env         map[string]string
	credentials map[string]seededCredential
	// readCap is the whole-read cap the suite gave the box, zero — every
	// box in production — for [MaxFileBytes]: see [readLimit].
	readCap int
	// confine is the boundary every command runs in, nil for a seat's
	// unconfined Direct cell.
	confine *boxConfine
}

var _ Sandbox = (*directBox)(nil)

// boxConfine is one box's confinement: the host's mechanism and its rules.
type boxConfine struct {
	host  *HostConfinement
	rules confineRules
}

// argv is a shell command line as this box runs it: confined where the box
// is, as written where it is not, and either way ahead of the engine for the
// kernel's OOM killer ([oomFirst]) — every setup and exec command, and the
// detached job, since all of them start here.
//
// Every first word it hands [oomFirst] is an absolute path — /bin/sh, or the
// confinement's helper as [ProbeConfinement] resolved it — so the raise
// searches no PATH of the box's.
func (b *directBox) argv(cmd string) ([]string, error) {
	argv := []string{"/bin/sh", "-c", cmd}
	if b.confine != nil {
		wrapped, err := b.confine.host.wrap(b.confine.rules, argv)
		if err != nil {
			return nil, localErrorf("local sandbox %s: %v", b.layout.id, err)
		}
		argv = wrapped
	}
	return oomFirst(argv), nil
}

func (b *directBox) ID() string   { return b.layout.id }
func (b *directBox) Home() string { return b.layout.home() }

// TempRootEnv is every variable a process in a host box takes its temporary
// directory from, each pointed at the box's own .tmp.
//
// ONE LIST, because a confined box may write only inside itself, and a temp
// root left at its default is a write outside it. TMPDIR alone was not
// enough: Claude Code roots its per-uid temp directory, where its Bash tool
// keeps each command's working state, at CLAUDE_CODE_TMPDIR or else at a
// literal /tmp, never at TMPDIR (measured on 2.1.310). Under the host's
// confinement every shell command a deep-work run made failed before it ran
// ("EPERM: operation not permitted, mkdir '/tmp/claude-<uid>/...'"). TMP and
// TEMP are what Python's tempfile and other runtimes read before falling back
// to /tmp. A container or an E2B VM needs none of this: its /tmp is inside the
// boundary already.
var TempRootEnv = []string{"TMPDIR", "TMP", "TEMP", "CLAUDE_CODE_TMPDIR"}

// childEnv is the allowlisted host environment plus the box's home, its
// temporary directory and the run env.
//
// Allowlisted for the same reason the CLI LLM backend allowlists: the engine's
// environment holds the org's chat token, its collector credential
// (OTEL_EXPORTER_OTLP_HEADERS) and possibly a metered API key, none of which
// the coding agent has any business reading.
// The run env is what config deliberately put there.
func (b *directBox) childEnv(extra map[string]string) map[string]string {
	env := hostbox.Inherit()
	home := b.layout.home()
	env["HOME"] = home
	env["XDG_CONFIG_HOME"] = filepath.Join(home, ".config")
	env["XDG_DATA_HOME"] = filepath.Join(home, ".local", "share")
	env["XDG_STATE_HOME"] = filepath.Join(home, ".local", "state")
	env["XDG_CACHE_HOME"] = filepath.Join(home, ".cache")
	for _, name := range TempRootEnv {
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
// — through the box's home, as every write into the box is
// ([boxLayout.mkdirHome]) — and answers it relative to the home.
//
// THE ENGINE NEVER ENTERS IT. A command starts in the box's home, a directory
// the engine made and the box cannot rename, and its own shell enters the
// working directory, by its name relative to the home ([inWorkdir]). Entered
// by its joined host path, the directory was walked on the engine's side by
// name after it was made, and a box that put a link in place of one on the way
// between the two moved where its command started; walked by the box's own
// process, it is the box's walk, under whatever confines the box. And it is
// the one way every confinement starts the command where it was asked to:
// bubblewrap enters the home whatever directory the process was handed.
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
	return "cd -- '" + strings.ReplaceAll(rel, "'", `'\''`) + "' || exit 1\n" + cmd
}

func (b *directBox) Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
	rel, err := b.workdir(opts.Cwd)
	if err != nil {
		return ExecResult{}, err
	}
	argv, err := b.argv(inWorkdir(rel, cmd))
	if err != nil {
		return ExecResult{}, err
	}
	return runHost(ctx, hostCommand{
		argv:    argv,
		cwd:     b.layout.home(),
		env:     b.childEnv(opts.Env),
		timeout: time.Duration(opts.TimeoutSec * float64(time.Second)),
	})
}

// StartBackground spawns the detached coding job in its own session.
//
// Spawned directly rather than backgrounded inside a throwaway shell, because
// the pid has to be usable for three different things and only a direct spawn
// gives all three: it leads its own session ([procgroup.Detach]), so one signal
// reaches the tree, no terminal's Ctrl-C reaches it, and it survives the
// engine's process — though not what usually replaces one, a container or a
// systemd control group stopped whole or a host rebooted, which the box's
// witness tells apart from a crash (local_witness.go); and the engine holds it
// unreaped, so its identity can be read before the pid could belong to
// anybody else.
//
// The Wait goroutine is the fourth, and it is the one with no POSIX
// equivalent: Go will not reap a child until something calls Wait on it, and
// an unreaped process lingers as a zombie, which kill(0) reports as ALIVE. The
// runner's completion probe is exactly that call, so without this the job
// would look like it was still working forever. It is a goroutine rather than
// a synchronous wait because the whole point is that the caller's turn ENDS
// while the job runs.
//
// Its stdio is /dev/null: the runner's script already redirects the agent's
// output into the box's result and error files.
func (b *directBox) StartBackground(ctx context.Context, cmd string, opts ExecOptions) (string, error) {
	rel, err := b.workdir(opts.Cwd)
	if err != nil {
		return "", err
	}
	// Not exec.CommandContext: the job must outlive the turn that starts it,
	// so binding it to the caller's context would kill it at the first
	// return.
	argv, err := b.argv(inWorkdir(rel, cmd))
	if err != nil {
		return "", err
	}
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

	// IDENTIFIED BEFORE ANYTHING CAN REAP IT. Until Wait runs the leader
	// keeps its kernel record even if it has already exited, and its pid
	// cannot belong to anybody else, so this is the one moment its start
	// time is certainly this job's. Read once the reaping goroutine has
	// started, a job that exits at once could be reaped first and its pid
	// reissued.
	leader, err := procgroup.Identify(pid)
	if err == nil {
		err = recordLeader(b.layout, leader)
	}
	if err != nil {
		// Without the record nothing in a later process can ever reach
		// this job's group: it would run to completion unkillable. Kill it
		// now rather than leak it, while this process still holds the
		// unreaped leader and so knows the group is the job's.
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
// The handle is a HOST pid, so it is only this job while the box's record
// says so and the kernel's record of that pid still carries the start time the
// box recorded. A zombie has exited, so it is not running even before its
// parent reaps it. A handle that is not the recorded job ENDED: the record is
// rewritten by every StartBackground, so a handle it no longer names belongs
// to a job this box has already replaced.
//
// The process, not its group, because the question is whether the WRAPPER is
// alive: a wrapper that died without writing its marker is a run that is
// over, even while an orphaned member of its group lingers on.
//
// A JOB THAT IS NOT RUNNING ENDED OR WAS LOST, and the box's witness says
// which ([jobEnd]); a job found running makes this process its witness.
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

// reap waits on a detached job so it does not linger as a zombie once it
// exits.
//
// Its exit status is deliberately discarded: the runner reads the job's OUTCOME
// from the marker and result files it wrote, and a non-zero exit is one of the
// outcomes those already describe.
func reap(proc *exec.Cmd) { _ = proc.Wait() }

// homeRel maps an in-box path onto the box's home: the path relative to it,
// which every access then makes through the home opened as an os.Root
// ([boxLayout.openHome]).
//
// Direct mode does NOT virtualise the filesystem: there is no chroot and no
// mount namespace, so an in-box path IS a host path. Writing outside the box
// would therefore hit the engine host's real /usr/local/bin (or worse), which
// is why a setup step that provisions a system path is rejected here rather
// than silently doing it. Container mode is NOT the answer for those, though
// it once said so: it writes files only under its mount and refuses the same
// path ([containerBox.homeRel]). What works in every cell is the box's home:
// a file named relative to it, or a setup command writing under "$HOME".
//
// Where a path lands is [hostbox.InHome]'s answer, the one config validation
// gives a setup step's files before any box exists, so the two cannot
// disagree about which file a box refuses. WHERE ITS LINKS LEAD is the root's
// to decide, at the access itself ([directBox.explain]).
func (b *directBox) homeRel(path string) (string, error) {
	// An absolute path that already names somewhere in the box is the
	// ordinary case: a brief and a setup step both speak in the home the box
	// reports. It may name that home as written or as its links resolve.
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

// explain is a failed access to path — rel in the box's home — as this box
// reports it.
//
// THE ROOT DECIDED the refusal ([boxLayout.openHome]): no component of rel,
// whatever the box made of it and whenever, led the access outside the home.
// This only says why, the way it has always been said: where rel leads as the
// host's links stand now ([hostbox.SafeJoin]) tells an access refused for
// leaving the box ([directBox.escapeError]) from a path that could not be
// resolved at all — one under a file, say, which reported as outside the box
// sent its reader looking for a symlink that was never there. Every other
// failure — absence, a file that is no regular file, one past its cap — is the
// access's own and passes through.
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
	rel, err := b.homeRel(path)
	if err != nil {
		return err
	}
	if err := b.layout.mkdirHome(filepath.Dir(rel)); err != nil {
		return b.explain(path, rel, localErrorf("local sandbox %s could not create %s: %v",
			b.layout.id, filepath.Join(b.layout.home(), filepath.Dir(rel)), err))
	}
	return b.explain(path, rel, writeHomeFile(b.layout, rel, path, content))
}

// ReadFile is EMPTY-ON-MISSING: the detached runner polls for marker and
// result files that do not exist until the job finishes, and a poll is not an
// error.
func (b *directBox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	// The RESOLVE failure is raised, unlike the read below. It means the
	// path left the box — a caller bug, or an escape attempt — and
	// answering "empty" would report a refused traversal as a file that
	// merely is not there yet, which is the one reading a poller acts on.
	rel, err := b.homeRel(path)
	if err != nil {
		return nil, err
	}
	content, err := readHomeFile(b.layout, rel, path, readLimit(b.readCap))
	return content, b.explain(path, rel, err)
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

// SetTimeout refreshes the box's keepalive stamp.
//
// This DOES have a counterpart here, and missing it was the bug: the orphan
// reaper reclaims local boxes on a clock, so a running box that never says it
// is alive is one the next Create on this host deletes. The waiter calls this
// once per poll for exactly the boxes it is keeping alive, which is the same
// contract a remote TTL refresh has. The argument is unused: a local box has
// no provider-side deadline to extend, only a last-seen time to move forward.
func (b *directBox) SetTimeout(ctx context.Context, seconds float64) error {
	touchAlive(b.layout)
	return nil
}

// Pause SIGSTOPs the job's process group.
//
// The local analogue of a remote snapshot: the run holds its exact state (open
// files, the checkout, the agent's memory) and resumes on Connect. It also
// holds RAM, which is why the waiter's pause reaper bounds it exactly as it
// bounds a billed snapshot.
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

// Close kills the job's process group, syncs credentials, and removes the box.
//
// Each signal re-reads the job's identity rather than reusing one reading: the
// group may end between two of them, and its pid is then free to be reissued
// to a stranger before the next.
func (b *directBox) Close(ctx context.Context) error {
	// SIGCONT first: a stopped process never runs again to handle SIGTERM,
	// so tearing down a paused box without it would leave the tree alive
	// and the directory in use.
	if signalJob(b.layout, "continue", procgroup.Continue) {
		signalJob(b.layout, "terminate", procgroup.Terminate)
		awaitJobExit(ctx, b.layout)
		// Whether or not it went: SIGKILL reaches nothing on a group that
		// is already gone, and the alternative is a tree left running
		// because the grace expired.
		if signalJob(b.layout, "kill", procgroup.Kill) {
			// Waited for again, because the removal below races the dying
			// wrapper's last writes exactly as Kill's does.
			awaitJobExit(ctx, b.layout)
		}
	}
	collectCredentials(ctx, b.layout, b.credentials)
	removeBox(b.layout)
	localLog.Debug("local_sandbox_closed", "sandbox_id", b.layout.id)
	return nil
}

// ---------------------------------------------------------------------
// container
// ---------------------------------------------------------------------

// containerBox is a [Sandbox] backed by a long-lived container.
//
// The box's home — never its records ([boxLayout]) — is bind-mounted at
// [DefaultHome], so in-box paths are identical to a remote backend's and file
// reads and writes happen on the HOST side of the mount, with no copy round
// trip through the runtime.
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

func (b *containerBox) workdir() string { return DefaultHome + "/" + WorkspaceSubdir }

// homeRel maps an in-container path onto its host side of the mount: the path
// relative to the box's home, which every access then makes through the home
// opened as an os.Root ([boxLayout.openHome]).
//
// This is the host side of a bind mount, so an escape here writes to the
// engine host, not to the container. A prefix test alone does not prevent
// that: "/home/user/../../etc/cron.d/x" starts with the mount point and still
// resolves outside it, and setup-step file paths are operator config. Where a
// path lands under the mount is [hostbox.InHome]'s answer, which config
// validation gives a setup step's files too; where its links lead is the
// root's to decide, at the access itself, as it is for a direct box
// ([containerBox.explain]).
func (b *containerBox) homeRel(path string) (string, error) {
	rel, inside := hostbox.InHome(DefaultHome, path)
	if !inside {
		// Not a file under the mount, so a write from out here cannot reach
		// it. A command inside the container might, but only as whatever
		// user the container runs as — the engine's own uid on a rootful
		// runtime ([containerUserArgs]), which an image's system
		// directories usually refuse — so the remedy that holds is the home.
		return "", localErrorf("%q is not a file under the sandbox home mount at %s, the only "+
			"place a file can be written into this box; name it relative to the home "+
			"(\".npmrc\", \".local/bin/tool\"), which every cell writes under its home, or have a "+
			"setup command write it under \"$HOME\"", path, DefaultHome)
	}
	return rel, nil
}

// explain is a failed access to path — rel in the box's home — as this box
// reports it: the root decided the refusal, and this says why, as
// [directBox.explain] does.
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
// NOT "-e KEY=value": a process's argv is world-readable on a normal Linux box
// (/proc/<pid>/cmdline, and every ps on the host), and this env carries the
// seat's LLM key and whatever code-host token role.sandbox.env declares. A file
// the runtime reads keeps them off the command line; it is one of the box's
// RECORDS, under its 0700 directory and written 0600 — and beside its home,
// never in it. The runtime's client reads it on the host, so the container
// has no need of it, and inside the mount the job could replace it between
// two execs: with a link that this rewrite followed to overwrite whatever host
// file it named, or with a named pipe whose open never returned.
//
// Rewritten per call rather than kept: extra differs between the setup steps
// and the coding job, and a stale file would hand one phase another's
// environment.
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
	// --env-file is line-oriented KEY=value with no quoting, so a newline in
	// a value would forge an extra variable. Values that cannot be
	// represented are dropped LOUDLY rather than silently truncated into a
	// different env.
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

// StartBackground backgrounds the job inside the container and echoes its pid.
//
// `docker exec` would otherwise block for the whole length of a coding job;
// backgrounding and echoing $! lets the exec return at once while the job
// keeps running under the container's PID 1. That PID 1 is started with
// --init precisely so it reaps the job when it finishes — an unreaped process
// becomes a zombie, and kill(0) reports a zombie as ALIVE, which would hang
// the runner's completion check on a job that had already died.
//
// Direct mode does not need this: it spawns the job itself with its own
// session, which gets both properties without a shell trick.
func (b *containerBox) StartBackground(ctx context.Context, cmd string, opts ExecOptions) (string, error) {
	args, err := b.execArgv(cmd+" & echo $!", opts)
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
	// THIS PROCESS SAW IT START, as for a direct box ([witnessJob]).
	witnessJob(b.layout)
	localLog.Info("local_sandbox_job_started",
		"sandbox_id", b.layout.id, "pid", pid, "container", b.container)
	return pid, nil
}

// Job implements [Sandbox]: the handle is a pid inside the container, whose
// --init reaps a finished job, so `kill -0` there answers it — once the
// runtime has said the container is up.
//
// THE RUNTIME IS ASKED FIRST, because an exec answers two questions with one
// exit status: a `kill -0` that found no job and a runtime that could not run
// the exec at all — no daemon, a socket not yet there after a reboot — both
// exit non-zero. Read as the job's answer, a daemon that was briefly away
// ended a job that was still running; and since an engine that has just
// restarted is no witness to it ([jobEnd]), that job was taken for one its
// host's restart had killed, and its round started again beside it. The
// listing tells the two apart: a runtime that answers it can say whether the
// container is there and up, and one that does not is the error that is no
// answer at all.
//
// A container that is gone or stopped ran its job down with it — a host that
// rebooted, a daemon restarted without live-restore — so its job is not
// running, and the box's witness says whether it ended or was lost. A PAUSED
// container takes no exec, so its job cannot be asked about: an error, which
// [Local.Attach] keeps the poll from ever reaching.
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

// state is the container's state as its runtime lists it — "running",
// "paused", "exited" and so on, lower-cased — and "" where the runtime lists no
// container of this box's name. An error is a runtime that did not answer,
// which says nothing about the container.
//
// A LISTING, NOT AN INSPECTION: `inspect` of a container that is not there
// fails as a runtime that is not there does, while a listing the runtime
// answers is a full answer, absence included. Filtered by the box's name and
// read for that exact name, since the filter matches by substring.
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

// trailingPID is the last numeric line of output — the backgrounded job's pid.
//
// The LAST, because a shell may print a job-control line first, and the echo
// is the final thing the command does.
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
	rel, err := b.homeRel(path)
	if err != nil {
		return err
	}
	if err := b.layout.mkdirHome(filepath.Dir(rel)); err != nil {
		return b.explain(path, rel, localErrorf("container sandbox %s could not create %s: %v",
			b.layout.id, filepath.Join(b.layout.home(), filepath.Dir(rel)), err))
	}
	return b.explain(path, rel, writeHomeFile(b.layout, rel, path, content))
}

func (b *containerBox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	// Same split as directBox.ReadFile: an escaped path is an error, a
	// file that is not there yet is empty.
	rel, err := b.homeRel(path)
	if err != nil {
		return nil, err
	}
	content, err := readHomeFile(b.layout, rel, path, readLimit(b.readCap))
	return content, b.explain(path, rel, err)
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

// openHomeFile is a local box's OpenFile once the path is mapped onto its home:
// the file, or a reader that yields nothing for one that is not there yet.
//
// ONLY ABSENCE IS EMPTY. A file that exists and cannot be opened is an error,
// because the reader of a stream decides what the run did from what it reads,
// and an unreadable event log answered as an empty one is a run that "said
// nothing" — which a collection settles as a run that produced nothing. So is
// anything that is not a regular file ([openRegularIn]), for all three reads
// alike.
//
// The home is closed once the file is open: what was opened through it stays
// open, and holds no path that anything could move.
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

// readHomeTail is a local box's ReadTail once the path is mapped onto its home.
//
// A SECTION OF THE FILE, never a read to its end: a job still writing can
// have grown it since the size was taken, and a read to EOF would then answer
// more than was asked for. Whatever arrived inside the window is counted into
// the size, so Data never claims to be more of the file than the file was.
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
// empty for a file that is not there, and REFUSED past limit (the box's
// [readLimit]) for the reason [readCapped] gives — which a plain os.ReadFile
// skipped, so a job that looped printing errors into its stderr file put all
// of it in the engine's memory on the host it shares, where a remote box's
// identical file was refused.
//
// ONLY ABSENCE IS EMPTY, as for [openHomeFile]. Every failure to open or read
// used to answer empty too, so a findings report the engine was not permitted
// to read — a container writing its mount as a user the engine is not —
// collected as a run that wrote none, and a question file as a run that asked
// nothing; a remote box's envd answers the same failure as an error, which
// is what a collection retries and a person can act on.
func readHomeFile(l boxLayout, rel, path string, limit int) ([]byte, error) {
	f, err := openHomeRegular(l, rel)
	switch {
	case absent(err):
		// Empty-on-missing IS the contract here: the detached runner
		// polls for marker and result files that do not exist until the
		// job finishes, and a poll is not an error.
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

// writeHomeFile is a local box's WriteFile once the path is mapped onto its
// home and its directory made: the file at rel REPLACED whole through
// [replaceIn] — so a process in the box reading it meanwhile (the git
// credential helper reading the token file the poll keeps current) finds the
// old file or the new one, never an empty or part-written one; no component
// of the path leads the write out of the box; and anything at the path that
// is not a regular file — a link, a named pipe, a directory — is refused as a
// [NotRegularFileError] naming what is there, and left as the box made it.
func writeHomeFile(l boxLayout, rel, path string, content []byte) error {
	home, err := l.openHome()
	if err != nil {
		return fmt.Errorf("local sandbox: open %s for writing: %w", path, err)
	}
	defer func() { _ = home.Close() }()
	if err := replaceIn(home, rel, content); err != nil {
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

// SetTimeout refreshes the box's keepalive stamp — see [directBox.SetTimeout].
func (b *containerBox) SetTimeout(ctx context.Context, seconds float64) error {
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

// unpause resumes a paused container. Best-effort: an already-running
// container reports an error we ignore, which keeps Connect a single
// unconditional call.
func (b *containerBox) unpause(ctx context.Context) {
	_, _ = runHost(ctx, b.cli.command(0, "unpause", b.container))
}

// paused reports whether the runtime says the container is paused: its own
// `.State.Paused`, read without changing it — the same field on Docker and
// Podman. Anything but a plain "true" is not a pause, a failed inspection
// included: see [Local.Attach].
func (b *containerBox) paused(ctx context.Context) bool {
	res, err := runHost(ctx, b.cli.command(0, "inspect", "--format", "{{.State.Paused}}", b.container))
	return err == nil && res.ExitCode == 0 && strings.TrimSpace(res.Stdout) == "true"
}

func (b *containerBox) Close(ctx context.Context) error {
	// A removal that failed LEAKS a container on the engine host, which is
	// the one outcome here an operator has to be able to see: nothing else
	// in the teardown path will mention it again.
	if res, err := runHost(ctx, b.cli.command(0, "rm", "-f", b.container)); err != nil ||
		res.ExitCode != 0 {
		localLog.Warn("local_sandbox_container_not_removed", "sandbox_id", b.layout.id,
			"container", b.container, "exit", res.ExitCode,
			"stderr", strings.TrimSpace(res.Stderr))
	}
	collectCredentials(ctx, b.layout, b.credentials)
	removeBox(b.layout)
	localLog.Debug("local_sandbox_closed", "sandbox_id", b.layout.id)
	return nil
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
		return "", localErrorf("run_in %q is set but neither "+
			"docker nor podman is on the engine host's PATH. Install one, set "+
			"providers.sandbox.local.runtime explicitly, or use run_in %q", Container, Direct)
	default:
		return "", localErrorf("providers.sandbox.local.runtime %q is not one of "+
			`"auto", "docker" or "podman"`, preference)
	}
}
