package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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
type capture struct {
	buf      []byte
	overflow bool
}

func (c *capture) Write(p []byte) (int, error) {
	if room := captureLimit - len(c.buf); room > 0 {
		c.buf = append(c.buf, p[:min(room, len(p))]...)
	} else {
		c.overflow = true
	}
	return len(p), nil
}

func (c *capture) String() string {
	if c.overflow {
		return string(c.buf) + "\n… output truncated"
	}
	return string(c.buf)
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
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------
// direct
// ---------------------------------------------------------------------

// directBox is a [Sandbox] that is a process tree on the engine host.
type directBox struct {
	layout      boxLayout
	env         map[string]string
	credentials map[string]string

	// mu guards job, which Close and StartBackground both touch.
	mu sync.Mutex
	// job is held so the Wait goroutine that reaps the detached process
	// keeps running and the child is never left a zombie. See
	// StartBackground.
	job *exec.Cmd
}

var _ Sandbox = (*directBox)(nil)

func (b *directBox) ID() string   { return b.layout.id }
func (b *directBox) Home() string { return b.layout.root }

// childEnv is the allowlisted host environment plus the box's home and the run
// env.
//
// Allowlisted for the same reason the CLI LLM backend allowlists: the engine's
// environment holds the org's chat token, its database DSN and possibly a
// metered API key, none of which the coding agent has any business reading.
// The run env is what config deliberately put there.
func (b *directBox) childEnv(extra map[string]string) map[string]string {
	env := hostbox.Inherit()
	home := b.layout.root
	env["HOME"] = home
	env["XDG_CONFIG_HOME"] = filepath.Join(home, ".config")
	env["XDG_DATA_HOME"] = filepath.Join(home, ".local", "share")
	env["XDG_STATE_HOME"] = filepath.Join(home, ".local", "state")
	env["XDG_CACHE_HOME"] = filepath.Join(home, ".cache")
	env["TMPDIR"] = filepath.Join(home, ".tmp")
	for key, value := range b.env {
		env[key] = value
	}
	for key, value := range extra {
		env[key] = value
	}
	return env
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
	return runHost(ctx, hostCommand{
		argv:    []string{"/bin/sh", "-c", cmd},
		cwd:     dir,
		env:     b.childEnv(opts.Env),
		timeout: time.Duration(opts.TimeoutSec * float64(time.Second)),
	})
}

// StartBackground spawns the detached coding job in its own session.
//
// Spawned directly rather than backgrounded inside a throwaway shell, because
// the pid has to be usable for three different things and only a direct spawn
// gives all three — see [detachAttr].
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
	// Not exec.CommandContext: the job must outlive the turn that starts it,
	// so binding it to the caller's context would kill it at the first
	// return.
	proc := exec.Command("/bin/sh", "-c", cmd) //nolint:noctx // deliberate; see above
	proc.Dir = dir
	proc.Env = flattenEnv(b.childEnv(opts.Env))
	procgroup.Detach(proc)
	// nil stdio is os/exec's /dev/null, which is what we want: nothing reads
	// these, and a pipe nobody drains would block the agent on a full buffer.
	proc.Stdin, proc.Stdout, proc.Stderr = nil, nil, nil

	if err := proc.Start(); err != nil {
		return "", localErrorf("local sandbox %s could not start a background job: %v", b.layout.id, err)
	}
	// Reaped in the background so the detached job does not become a
	// zombie. Its exit status is deliberately discarded: the runner reads
	// the job's OUTCOME from the marker and result files it wrote, and a
	// non-zero exit is one of the outcomes those already describe.
	go func() { _ = proc.Wait() }()

	b.mu.Lock()
	b.job = proc
	b.mu.Unlock()

	pid := proc.Process.Pid
	if err := os.MkdirAll(b.layout.meta(), hostbox.DirMode); err == nil {
		err = os.WriteFile(b.layout.pidFile(), []byte(strconv.Itoa(pid)), hostbox.FileMode)
		if err != nil {
			// Without the pid file nothing in a later process can ever
			// reach this job's group — it would run to completion
			// unkillable. Kill it now rather than leak it.
			logSignal("kill", pid, procgroup.Kill(pid))
			return "", localErrorf("local sandbox %s could not record its job's pid: %v", b.layout.id, err)
		}
	} else {
		logSignal("kill", pid, procgroup.Kill(pid))
		return "", localErrorf("local sandbox %s could not record its job's pid: %v", b.layout.id, err)
	}
	localLog.Info("local_sandbox_job_started", "sandbox_id", b.layout.id, "pid", pid)
	return strconv.Itoa(pid), nil
}

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
		root, err := filepath.EvalSymlinks(b.layout.root)
		if err != nil {
			root = filepath.Clean(b.layout.root)
		}
		clean := filepath.Clean(path)
		// An absolute path that already names somewhere in the box is the
		// ordinary case: a brief and a setup step both speak in the home the
		// box reports.
		if within, err := filepath.Rel(root, clean); err == nil && !strings.HasPrefix(within, "..") {
			rel = within
		} else if within, err := filepath.Rel(filepath.Clean(b.layout.root), clean); err == nil && !strings.HasPrefix(within, "..") {
			rel = within
		} else {
			return "", b.escapeError(path)
		}
	}
	resolved, err := hostbox.SafeJoin(b.layout.root, rel)
	if err != nil {
		return "", b.escapeError(path)
	}
	return resolved, nil
}

func (b *directBox) escapeError(path string) error {
	return localErrorf("local sandbox (run_in %q) refuses to touch %q: it is outside "+
		"the box at %s. Direct mode has no filesystem virtualisation, so this would write to "+
		"the engine host itself. Put the file under the box's home, or use "+
		"run_in %q.", Direct, path, b.layout.root, Container)
}

func (b *directBox) WriteFile(ctx context.Context, path string, content []byte) error {
	target, err := b.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), hostbox.DirMode); err != nil {
		return localErrorf("local sandbox %s could not create %s: %v", b.layout.id, filepath.Dir(target), err)
	}
	return os.WriteFile(target, content, hostbox.FileMode)
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
	content, err := os.ReadFile(target)
	if err != nil {
		//nolint:nilerr // Empty-on-missing IS the contract here: the
		// detached runner polls for marker and result files that do not
		// exist until the job finishes, and a poll is not an error.
		return nil, nil
	}
	return content, nil
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
	pid := readPID(b.layout)
	if pid <= 0 {
		return nil
	}
	logSignal("stop", pid, procgroup.Stop(pid))
	localLog.Debug("local_sandbox_paused", "sandbox_id", b.layout.id, "pid", pid)
	return nil
}

// resume SIGCONTs a paused box — the Connect auto-resume.
func (b *directBox) resume() {
	if pid := readPID(b.layout); pid > 0 {
		logSignal("continue", pid, procgroup.Continue(pid))
	}
}

// Close kills the job's process group, syncs credentials, and removes the box.
func (b *directBox) Close(ctx context.Context) error {
	if pid := readPID(b.layout); pid > 0 {
		// SIGCONT first: a stopped process never runs again to handle
		// SIGTERM, so tearing down a paused box without it would leave the
		// tree alive and the directory in use.
		logSignal("continue", pid, procgroup.Continue(pid))
		logSignal("terminate", pid, procgroup.Terminate(pid))
		awaitGroupExit(ctx, pid)
		// Whether or not it went: SIGKILL costs nothing on a group that is
		// already gone, and the alternative is a tree left running because
		// the grace expired.
		logSignal("kill", pid, procgroup.Kill(pid))
		// Waited for again, because the removal below races the dying
		// wrapper's last writes exactly as Kill's does.
		awaitGroupExit(ctx, pid)
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
// The box directory is bind-mounted at [DefaultHome], so in-box paths are
// identical to a remote backend's and file reads and writes happen on the HOST
// side of the mount — no copy round trip through the runtime.
type containerBox struct {
	layout      boxLayout
	runtime     string
	container   string
	env         map[string]string
	credentials map[string]string
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
		return b.layout.root, nil
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
	resolved, err := hostbox.SafeJoin(b.layout.root, rel)
	if err != nil {
		return "", localErrorf("%q resolves outside the sandbox home mount at %s — it would be "+
			"written to the engine host itself", path, b.layout.root)
	}
	return resolved, nil
}

// envArgs renders the run env as --env-file arguments.
//
// NOT "-e KEY=value": a process's argv is world-readable on a normal Linux box
// (/proc/<pid>/cmdline, and every ps on the host), and this env carries the
// seat's LLM key and whatever code-host token role.sandbox.env declares. A file
// the runtime reads keeps them off the command line; it lives inside the box,
// which is already 0700, and is written 0600.
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
	for _, assignment := range flattenEnv(merged) {
		key, value, _ := strings.Cut(assignment, "=")
		if strings.ContainsAny(value, "\n\r") {
			localLog.Warn("local_sandbox_env_var_unrepresentable", "sandbox_id", b.layout.id, "var", key)
			continue
		}
		lines = append(lines, assignment)
	}
	if err := os.MkdirAll(b.layout.meta(), hostbox.DirMode); err != nil {
		return nil, localErrorf("container sandbox %s could not write its env file: %v", b.layout.id, err)
	}
	blob := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(b.layout.envFile(), []byte(blob), hostbox.FileMode); err != nil {
		return nil, localErrorf("container sandbox %s could not write its env file: %v", b.layout.id, err)
	}
	return []string{"--env-file", b.layout.envFile()}, nil
}

func (b *containerBox) execArgv(cmd string, opts ExecOptions) ([]string, error) {
	cwd := opts.Cwd
	if cwd == "" {
		cwd = b.workdir()
	}
	argv := []string{b.runtime, "exec", "-w", cwd}
	envArgs, err := b.envArgs(opts.Env)
	if err != nil {
		return nil, err
	}
	argv = append(argv, envArgs...)
	return append(argv, b.container, "/bin/sh", "-c", cmd), nil
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
			b.layout.id, truncate(detail, 400))
	}
	localLog.Info("local_sandbox_job_started",
		"sandbox_id", b.layout.id, "pid", pid, "container", b.container)
	return pid, nil
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
	return os.WriteFile(target, content, hostbox.FileMode)
}

func (b *containerBox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	// Same split as directBox.ReadFile: an escaped path is an error, a
	// file that is not there yet is empty.
	target, err := b.hostPath(path)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(target)
	if err != nil {
		//nolint:nilerr // Empty-on-missing IS the contract; see directBox.ReadFile.
		return nil, nil
	}
	return content, nil
}

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
		localLog.Warn("local_sandbox_pause_failed", "sandbox_id", b.layout.id, "error", truncate(detail, 200))
	}
	return nil
}

// unpause resumes a paused container. Best-effort: an already-running
// container reports an error we ignore, which keeps Connect a single
// unconditional call.
func (b *containerBox) unpause(ctx context.Context) {
	_, _ = runHost(ctx, hostCommand{argv: []string{b.runtime, "unpause", b.container}})
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
