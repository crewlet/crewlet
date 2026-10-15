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
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/procgroup"
)

// hostCommand is one command to run on the engine host.
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

// flattenEnv renders an env map as os/exec's KEY=value slice.
//
// Sorted, so a spawn is reproducible and a test can assert on it; nil for an
// empty map, which is os/exec's "inherit the parent" — a distinction the
// callers here rely on being explicit, since inheriting the ENGINE's
// environment is exactly what the allowlist exists to prevent. Every caller
// passes a populated map.
func flattenEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	slices.Sort(out)
	return out
}

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
	credentials map[string]string
	// readCap is the whole-read cap the suite gave the box, zero — every
	// box in production — for [MaxFileBytes]: see [readLimit].
	readCap int
}

var _ Sandbox = (*directBox)(nil)

func (b *directBox) ID() string   { return b.layout.id }
func (b *directBox) Home() string { return b.layout.home() }

// childEnv is the allowlisted host environment, the run env, and the box's
// isolation over both.
//
// Allowlisted — [hostbox.PassthroughEnv], the list the CLI LLM backend starts
// from too — because the engine's environment holds the org's chat token, the
// keyring that decrypts its secret store and possibly a metered API key, none
// of which the coding agent has any business reading. The run env is what
// config deliberately put there.
//
// THE ISOLATION IS SET LAST ([directIsolation]), over every layer the run env
// is assembled from — role.sandbox.env, a setup step's env, a command's own —
// because it is the only boundary a direct box has: a HOME written in any of
// them landed after it and moved the coding agent's home out of its box, onto
// whatever directory of the engine host it named. What a run env says about
// those names is reported when the box is created ([warnIsolationKept]).
func (b *directBox) childEnv(extra map[string]string) map[string]string {
	env := hostbox.Inherit()
	maps.Copy(env, b.runEnv(extra))
	return env
}

// runEnv is what the box SETS, as against what it inherits from the host: the
// box's environment, a command's own over it, and the isolation over both. It
// is what a login shell's profile is not allowed to replace
// ([ExecOptions.Login]); the inherited PATH is exactly what it is allowed to.
func (b *directBox) runEnv(extra map[string]string) map[string]string {
	env := maps.Clone(b.env)
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, extra)
	maps.Copy(env, directIsolation(b.layout.home()))
	return env
}

// shell is the argv and environment one command runs with on the engine host:
// a plain `sh -c`, or a login shell with the run environment over its profile.
func (b *directBox) shell(cmd string, opts ExecOptions) ([]string, map[string]string, error) {
	env := b.childEnv(opts.Env)
	if !opts.Login {
		return []string{"/bin/sh", "-c", cmd}, env, nil
	}
	script, stashed, err := loginScript(cmd, b.runEnv(opts.Env))
	if err != nil {
		return nil, nil, err
	}
	maps.Copy(env, stashed)
	return []string{"/bin/sh", "-l", "-c", script}, env, nil
}

// directIsolation is a direct box's ISOLATION, by name and value: HOME, the
// XDG base directories and TMPDIR, each inside the box's own home — every
// place a coding agent keeps state, so that state is the box's rather than
// the engine user's.
func directIsolation(home string) map[string]string {
	return map[string]string{
		"HOME":            home,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
		"TMPDIR":          filepath.Join(home, ".tmp"),
	}
}

// isolationKept is every name of the isolation a run env also writes, sorted:
// the values a box will not use.
func isolationKept(env, isolation map[string]string) []string {
	var kept []string
	for name := range isolation {
		if _, written := env[name]; written {
			kept = append(kept, name)
		}
	}
	slices.Sort(kept)
	return kept
}

// containerIsolation is a container box's isolation: its HOME, the bind-mounted
// [DefaultHome] the container was started with. The rest of the box's
// environment is the image's own, inside a boundary of its own.
func containerIsolation() map[string]string { return map[string]string{"HOME": DefaultHome} }

// warnIsolationKept says, once per box, which of a run env's variables the box
// overrides with its own isolation — so a value an operator wrote is reported
// as doing nothing rather than ignored in silence.
func warnIsolationKept(id string, placement Placement, env, isolation map[string]string) {
	kept := isolationKept(env, isolation)
	if len(kept) == 0 {
		return
	}
	localLog.Warn("local_sandbox_env_isolation_kept", "sandbox_id", id,
		"placement", string(placement), "vars", kept,
		"detail", "the box sets these itself, over role.sandbox.env and every setup "+
			"step's env, so the coding agent's state stays inside its box: the values "+
			"written for them are not used")
}

// workdir resolves a command's cwd, defaulting to the box's checkout.
func (b *directBox) workdir(cwd string) (string, error) {
	target := b.layout.workspace()
	if cwd != "" {
		resolved, err := b.resolve(cwd)
		if err != nil {
			return "", err
		}
		target = resolved
	}
	if err := os.MkdirAll(target, hostbox.DirMode); err != nil {
		return "", localErrorf("local sandbox %s could not create %s: %v", b.layout.id, target, err)
	}
	return target, nil
}

func (b *directBox) Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
	dir, err := b.workdir(opts.Cwd)
	if err != nil {
		return ExecResult{}, err
	}
	argv, env, err := b.shell(cmd, opts)
	if err != nil {
		return ExecResult{}, err
	}
	return runHost(ctx, hostCommand{
		argv:    argv,
		cwd:     dir,
		env:     env,
		timeout: time.Duration(opts.TimeoutSec * float64(time.Second)),
	})
}

// StartBackground spawns the detached coding job in its own session.
//
// Spawned directly rather than backgrounded inside a throwaway shell, because
// the pid has to be usable for three different things and only a direct spawn
// gives all three: it leads its own session ([procgroup.Detach]), so one signal
// reaches the tree, no terminal's Ctrl-C reaches it, and it survives the
// engine; and the engine holds it unreaped, so its identity can be read before
// the pid could belong to anybody else.
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
	dir, err := b.workdir(opts.Cwd)
	if err != nil {
		return "", err
	}
	argv, env, err := b.shell(cmd, opts)
	if err != nil {
		return "", err
	}
	// Not exec.CommandContext: the job must outlive the turn that starts it,
	// so binding it to the caller's context would kill it at the first
	// return.
	proc := exec.Command(argv[0], argv[1:]...) //nolint:noctx // deliberate; see above
	proc.Dir = dir
	proc.Env = flattenEnv(env)
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
	localLog.Info("local_sandbox_job_started", "sandbox_id", b.layout.id, "pid", pid)
	return strconv.Itoa(pid), nil
}

// JobRunning implements [Sandbox] for a job on the engine host.
//
// The handle is a HOST pid, so it is only this job while the box's record
// says so and the kernel's record of that pid still carries the start time the
// box recorded. A zombie has exited, so it is not running even before its
// parent reaps it. A handle that is not the recorded job is not running: the
// record is rewritten by every StartBackground, so a handle it no longer names
// belongs to a job this box has already replaced.
//
// The process, not its group, because the question is whether the WRAPPER is
// alive: a wrapper that died without writing its marker is a run that is
// over, even while an orphaned member of its group lingers on.
func (b *directBox) JobRunning(ctx context.Context, commandID string) (bool, error) {
	pid, err := strconv.Atoi(commandID)
	if err != nil || pid <= 1 {
		return false, localErrorf("local sandbox %s: job handle %q is not a process id", b.layout.id, commandID)
	}
	leader, found, err := readJobRecord(b.layout)
	if err != nil {
		return false, err
	}
	if !found || leader.PID != pid {
		return false, nil
	}
	proc, found, err := procgroup.Inspect(pid)
	if err != nil {
		return false, fmt.Errorf("local sandbox %s: reading the kernel's record of job %d: %w", b.layout.id, pid, err)
	}
	return found && !proc.Zombie && proc.Start == leader.Start, nil
}

// reap waits on a detached job so it does not linger as a zombie once it
// exits.
//
// Its exit status is deliberately discarded: the runner reads the job's OUTCOME
// from the marker and result files it wrote, and a non-zero exit is one of the
// outcomes those already describe.
func reap(proc *exec.Cmd) { _ = proc.Wait() }

// resolve maps an in-box path onto the host, refusing escapes.
//
// Direct mode does NOT virtualise the filesystem: there is no chroot and no
// mount namespace, so an in-box path IS a host path. Writing outside the box
// would therefore hit the engine host's real /usr/local/bin (or worse), which
// is why a setup step that provisions a system path is rejected here rather
// than silently doing it. Container mode is the answer for those.
func (b *directBox) resolve(path string) (string, error) {
	rel := path
	if filepath.IsAbs(path) {
		root, err := filepath.EvalSymlinks(b.layout.home())
		if err != nil {
			root = filepath.Clean(b.layout.home())
		}
		clean := filepath.Clean(path)
		// An absolute path that already names somewhere in the box is the
		// ordinary case: a brief and a setup step both speak in the home the
		// box reports.
		if within, err := filepath.Rel(root, clean); err == nil && !strings.HasPrefix(within, "..") {
			rel = within
		} else if within, err := filepath.Rel(filepath.Clean(b.layout.home()), clean); err == nil && !strings.HasPrefix(within, "..") {
			rel = within
		} else {
			return "", b.escapeError(path)
		}
	}
	resolved, err := hostbox.SafeJoin(b.layout.home(), rel)
	switch {
	case errors.Is(err, hostbox.ErrEscape):
		return "", b.escapeError(path)
	case err != nil:
		// NOT AN ESCAPE: a path that could not be resolved at all — one
		// under a file, say — and reporting it as outside the box sent its
		// reader looking for a symlink that was never there.
		return "", localErrorf("local sandbox %s could not resolve %q: %v", b.layout.id, path, err)
	}
	return resolved, nil
}

func (b *directBox) escapeError(path string) error {
	return localErrorf("local sandbox (run_in %q) refuses to touch %q: it is outside "+
		"the box at %s. Direct mode has no filesystem virtualisation, so this would write to "+
		"the engine host itself. Put the file under the box's home, or use "+
		"run_in %q.", Direct, path, b.layout.home(), Container)
}

func (b *directBox) WriteFile(ctx context.Context, path string, content []byte) error {
	target, err := b.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), hostbox.DirMode); err != nil {
		return localErrorf("local sandbox %s could not create %s: %v", b.layout.id, filepath.Dir(target), err)
	}
	return writeHostFile(target, path, content)
}

// ReadFile is EMPTY-ON-MISSING: the detached runner polls for marker and
// result files that do not exist until the job finishes, and a poll is not an
// error.
func (b *directBox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	// The RESOLVE failure is raised, unlike the read below. It means the
	// path left the box — a caller bug, or an escape attempt — and
	// answering "empty" would report a refused traversal as a file that
	// merely is not there yet, which is the one reading a poller acts on.
	target, err := b.resolve(path)
	if err != nil {
		return nil, err
	}
	return readHostFile(target, path, readLimit(b.readCap))
}

// OpenFile implements [Sandbox] for a box on the engine host: the file itself,
// read in place, after the same escape check every other path takes.
func (b *directBox) OpenFile(ctx context.Context, path string) (io.ReadCloser, error) {
	target, err := b.resolve(path)
	if err != nil {
		return nil, err
	}
	return openHostFile(target, path)
}

// ReadTail implements [Sandbox] for a box on the engine host: a seek, so the
// cost is what is read rather than how long the run has been writing.
func (b *directBox) ReadTail(ctx context.Context, path string, n int) (FileTail, error) {
	target, err := b.resolve(path)
	if err != nil {
		return FileTail{}, err
	}
	return readHostTail(target, path, n)
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
	collectCredentials(b.layout, b.credentials)
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
	runtime     string
	container   string
	env         map[string]string
	credentials map[string]string
	// readCap is as [directBox]'s.
	readCap int
}

var _ Sandbox = (*containerBox)(nil)

func (b *containerBox) ID() string   { return b.layout.id }
func (b *containerBox) Home() string { return DefaultHome }

func (b *containerBox) workdir() string { return DefaultHome + "/" + WorkspaceSubdir }

// hostPath maps an in-container path onto its host side of the mount.
//
// The result is a HOST path — this is the host side of a bind mount, so an
// escape here writes to the engine host, not to the container. A prefix test
// alone does not prevent that: "/home/user/../../etc/cron.d/x" starts with the
// mount point and still resolves outside it, and setup-step file paths are
// operator config. The final check is the same one direct mode makes, for the
// same reason.
func (b *containerBox) hostPath(path string) (string, error) {
	clean := filepath.Clean(path)
	if clean == DefaultHome {
		return b.layout.home(), nil
	}
	prefix := DefaultHome + "/"
	rel := path
	switch {
	case strings.HasPrefix(path, prefix):
		rel = path[len(prefix):]
	case filepath.IsAbs(path):
		// Outside the mount — reachable only from inside the container,
		// which is exactly what container mode is for.
		return "", localErrorf("%q is outside the sandbox home mount at %s; write it with a "+
			"setup-step command instead of a file entry", path, DefaultHome)
	}
	resolved, err := hostbox.SafeJoin(b.layout.home(), rel)
	switch {
	case errors.Is(err, hostbox.ErrEscape):
		return "", localErrorf("%q resolves outside the sandbox home mount at %s — it would be "+
			"written to the engine host itself", path, b.layout.home())
	case err != nil:
		// Not an escape — see [directBox.resolve].
		return "", localErrorf("%q could not be resolved under the sandbox home mount at %s: %v",
			path, b.layout.home(), err)
	}
	return resolved, nil
}

// runEnv is what a command in the container is handed: the box's
// environment, the command's own over it, and the container's isolation over
// both — less any value an env file cannot carry.
//
// THE BOX'S HOME LAST, over every layer: it is the bind-mounted home the
// box's credential files, its records' twins and the coding agent's state live
// in, and an exec env-file is applied over the container's own HOME — so a
// role.sandbox.env HOME moved the agent off its mount, onto a directory the
// container discards and the engine cannot read.
//
// --env-file is line-oriented KEY=value with no quoting, so a newline in a
// value would forge an extra variable. Values that cannot be represented are
// dropped LOUDLY rather than silently truncated into a different env — and
// dropped HERE, before a login shell stashes what it will put back, so a value
// the file cannot carry is absent rather than put back empty.
func (b *containerBox) runEnv(extra map[string]string) map[string]string {
	merged := make(map[string]string, len(b.env)+len(extra)+1)
	maps.Copy(merged, b.env)
	maps.Copy(merged, extra)
	maps.Copy(merged, containerIsolation())
	for key, value := range merged {
		if strings.ContainsAny(value, "\n\r") {
			localLog.Warn("local_sandbox_env_var_unrepresentable", "sandbox_id", b.layout.id, "var", key)
			delete(merged, key)
		}
	}
	return merged
}

// envArgs writes env as the box's env file and returns the --env-file
// arguments that hand it to one exec.
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
// Rewritten per call rather than kept: the environment differs between the
// setup steps and the coding job, and a stale file would hand one phase
// another's environment.
func (b *containerBox) envArgs(env map[string]string) ([]string, error) {
	if err := os.MkdirAll(b.layout.records(), hostbox.DirMode); err != nil {
		return nil, localErrorf("container sandbox %s could not write its env file: %v", b.layout.id, err)
	}
	blob := strings.Join(flattenEnv(env), "\n") + "\n"
	if err := os.WriteFile(b.layout.envFile(), []byte(blob), hostbox.FileMode); err != nil {
		return nil, localErrorf("container sandbox %s could not write its env file: %v", b.layout.id, err)
	}
	return []string{"--env-file", b.layout.envFile()}, nil
}

// execArgv is the runtime invocation that runs cmd in the container: a plain
// `sh -c`, or a login shell with the run environment over its profile
// ([ExecOptions.Login]) — the stashes that puts back riding the env file with
// everything else, never the command line.
func (b *containerBox) execArgv(cmd string, opts ExecOptions) ([]string, error) {
	cwd := opts.Cwd
	if cwd == "" {
		cwd = b.workdir()
	}
	env := b.runEnv(opts.Env)
	shell := []string{"/bin/sh", "-c", cmd}
	if opts.Login {
		script, stashed, err := loginScript(cmd, env)
		if err != nil {
			return nil, err
		}
		env, shell = stashed, []string{"/bin/sh", "-l", "-c", script}
	}
	argv := []string{b.runtime, "exec", "-w", cwd}
	envArgs, err := b.envArgs(env)
	if err != nil {
		return nil, err
	}
	argv = append(argv, envArgs...)
	argv = append(argv, b.container)
	return append(argv, shell...), nil
}

func (b *containerBox) Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
	argv, err := b.execArgv(cmd, opts)
	if err != nil {
		return ExecResult{}, err
	}
	return runHost(ctx, hostCommand{
		argv:    argv,
		timeout: time.Duration(opts.TimeoutSec * float64(time.Second)),
	})
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
	argv, err := b.execArgv(cmd+" & echo $!", opts)
	if err != nil {
		return "", err
	}
	result, err := runHost(ctx, hostCommand{argv: argv})
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
	localLog.Info("local_sandbox_job_started",
		"sandbox_id", b.layout.id, "pid", pid, "container", b.container)
	return pid, nil
}

// JobRunning implements [Sandbox]: the handle is a pid inside the container,
// whose --init reaps a finished job, so `kill -0` there answers it.
func (b *containerBox) JobRunning(ctx context.Context, commandID string) (bool, error) {
	return probeByKill(ctx, b, commandID)
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
	target, err := b.hostPath(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), hostbox.DirMode); err != nil {
		return localErrorf("container sandbox %s could not create %s: %v",
			b.layout.id, filepath.Dir(target), err)
	}
	return writeHostFile(target, path, content)
}

func (b *containerBox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	// Same split as directBox.ReadFile: an escaped path is an error, a
	// file that is not there yet is empty.
	target, err := b.hostPath(path)
	if err != nil {
		return nil, err
	}
	return readHostFile(target, path, readLimit(b.readCap))
}

// OpenFile implements [Sandbox]: the host side of the mount, read in place.
func (b *containerBox) OpenFile(ctx context.Context, path string) (io.ReadCloser, error) {
	target, err := b.hostPath(path)
	if err != nil {
		return nil, err
	}
	return openHostFile(target, path)
}

// ReadTail implements [Sandbox]: a seek on the host side of the mount.
func (b *containerBox) ReadTail(ctx context.Context, path string, n int) (FileTail, error) {
	target, err := b.hostPath(path)
	if err != nil {
		return FileTail{}, err
	}
	return readHostTail(target, path, n)
}

// openHostFile is a local box's OpenFile once the path is resolved: the file,
// or a reader that yields nothing for one that is not there yet.
//
// ONLY ABSENCE IS EMPTY. A file that exists and cannot be opened is an error,
// because the reader of a stream decides what the run did from what it reads,
// and an unreadable event log answered as an empty one is a run that "said
// nothing" — which a collection settles as a run that produced nothing. So is
// anything that is not a regular file ([openHostRegular]), for all three
// reads alike.
func openHostFile(target, path string) (io.ReadCloser, error) {
	f, err := openHostRegular(target)
	switch {
	case absent(err):
		return io.NopCloser(strings.NewReader("")), nil
	case err != nil:
		return nil, fmt.Errorf("local sandbox: open %s: %w", path, err)
	}
	return f, nil
}

// readHostTail is a local box's ReadTail once the path is resolved.
//
// A SECTION OF THE FILE, never a read to its end: a job still writing can
// have grown it since the size was taken, and a read to EOF would then answer
// more than was asked for. Whatever arrived inside the window is counted into
// the size, so Data never claims to be more of the file than the file was.
func readHostTail(target, path string, n int) (FileTail, error) {
	f, err := openHostRegular(target)
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

// readHostFile is a local box's ReadFile once the path is resolved: empty for
// a file that is not there, and REFUSED past limit (the box's [readLimit]) for
// the reason [readCapped] gives — which a plain os.ReadFile skipped, so a job
// that looped printing errors into its stderr file put all of it in the
// engine's memory on the host it shares, where a remote box's identical file
// was refused.
//
// ONLY ABSENCE IS EMPTY, as for [openHostFile]. Every failure to open or read
// used to answer empty too, so a findings report the engine was not permitted
// to read — a container writing its mount as a user the engine is not —
// collected as a run that wrote none, and a question file as a run that asked
// nothing; a remote box's envd answers the same failure as an error, which
// is what a collection retries and a person can act on.
func readHostFile(target, path string, limit int) ([]byte, error) {
	f, err := openHostRegular(target)
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

// writeHostFile is a local box's WriteFile once the path is resolved: content
// put at target whole, in place, through [openHostWritable] — so a link at the
// path is refused rather than followed out of the box, and a named pipe is
// refused rather than waited on, each as a [NotRegularFileError] naming what
// is there.
func writeHostFile(target, path string, content []byte) error {
	f, err := openHostWritable(target)
	if err != nil {
		return fmt.Errorf("local sandbox: open %s for writing: %w", path, err)
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("local sandbox: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("local sandbox: write %s: %w", path, err)
	}
	return nil
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
	result, err := runHost(ctx, hostCommand{argv: []string{b.runtime, "pause", b.container}})
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
	_, _ = runHost(ctx, hostCommand{argv: []string{b.runtime, "unpause", b.container}})
}

// paused reports whether the runtime says the container is paused: its own
// `.State.Paused`, read without changing it — the same field on Docker and
// Podman. Anything but a plain "true" is not a pause, a failed inspection
// included: see [Local.Attach].
func (b *containerBox) paused(ctx context.Context) bool {
	res, err := runHost(ctx, hostCommand{
		argv: []string{b.runtime, "inspect", "--format", "{{.State.Paused}}", b.container},
	})
	return err == nil && res.ExitCode == 0 && strings.TrimSpace(res.Stdout) == "true"
}

func (b *containerBox) Close(ctx context.Context) error {
	// A removal that failed LEAKS a container on the engine host, which is
	// the one outcome here an operator has to be able to see: nothing else
	// in the teardown path will mention it again.
	if res, err := runHost(ctx, hostCommand{
		argv: []string{b.runtime, "rm", "-f", b.container},
	}); err != nil || res.ExitCode != 0 {
		localLog.Warn("local_sandbox_container_not_removed", "sandbox_id", b.layout.id,
			"container", b.container, "exit", res.ExitCode,
			"stderr", strings.TrimSpace(res.Stderr))
	}
	collectCredentials(b.layout, b.credentials)
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
