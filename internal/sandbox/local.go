package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/procgroup"
)

// localLog names the LOCAL backend. The package-neutral logger is in
// protocol.go; binding one var for the whole package stamped the remote
// backend and the shared runtime as "sandbox.local" too.
var localLog = logging.Get("sandbox.local")

// WorkspaceSubdir is where a box's checkout lives, relative to its home: the
// coding agent's working directory. Kept under the home so container mode
// needs exactly one bind mount.
const WorkspaceSubdir = "workspace"

// ContainerPrefix names every container this backend starts, so a stray box is
// identifiable — and greppable in `docker ps` — without consulting Crewlet.
const ContainerPrefix = "crewlet-sbx-"

// controlTimeout bounds a CONTROL-PLANE command: mkdir, `docker exec`, the
// liveness probe. Never the coding job itself, which is detached and
// deliberately uncapped. Sized for a container runtime under load, not for
// work.
const controlTimeout = 120 * time.Second

// termGrace is how long a direct box's process group gets between SIGTERM and
// SIGKILL.
const termGrace = 5 * time.Second

// pidReuseGrace is the slack allowed when comparing a process's start time
// against the box's creation time.
//
// A box's own job cannot predate the box, so a pid whose process started
// EARLIER is a recycled one pointing at some unrelated long-lived process —
// which, without this, would keep a dead box's directory alive forever. The
// grace absorbs only clock and filesystem timestamp granularity between the
// two readings (the directory's mtime versus /proc/<pid>'s), which is
// sub-second; a second is ample and far below any interval over which a pid
// could wrap.
const pidReuseGrace = time.Second

// minReapAge floors the orphan reaper's cutoff regardless of the TTL it is
// handed. A spec with a tiny (or zero) timeout would otherwise make every box
// on the host instantly reapable, including ones another engine just created
// and has not yet heart-beaten.
const minReapAge = time.Minute

// BoxRootEnv overrides where local boxes are created.
const BoxRootEnv = "CREWLET_SANDBOX_LOCAL_HOME"

// LocalError reports a local box that could not be created, reached, or torn
// down.
type LocalError struct{ msg string }

func (e *LocalError) Error() string { return e.msg }

func localErrorf(format string, args ...any) error {
	return &LocalError{msg: fmt.Sprintf(format, args...)}
}

// DefaultBoxRoot is the parent directory local boxes are created under.
func DefaultBoxRoot() string {
	if override := strings.TrimSpace(os.Getenv(BoxRootEnv)); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".crewlet", "sandboxes")
}

// ---------------------------------------------------------------------
// box layout
// ---------------------------------------------------------------------

// boxLayout is where one box's files live on the engine host.
//
// Everything the box needs to be SELF-DESCRIBING lives under .crewlet/:
// Connect is handed nothing but an id, possibly in a different process from
// the one that created the box, so anything a reconnect must know has to be on
// disk rather than in the object Create returned.
type boxLayout struct {
	id   string
	root string
}

func (l boxLayout) workspace() string { return filepath.Join(l.root, WorkspaceSubdir) }
func (l boxLayout) meta() string      { return filepath.Join(l.root, ".crewlet") }

// pidFile records the detached job's pid so teardown can reach its group even
// in a fresh engine that never held the handle.
func (l boxLayout) pidFile() string { return filepath.Join(l.meta(), "box.pid") }

// aliveFile is the keepalive stamp — the local counterpart of a remote box's
// TTL.
//
// A directory's mtime does NOT move when files are written inside its
// subdirectories, so the box root's own timestamp is frozen at creation for
// the box's entire life. Anything reading it as "recently used" is reading a
// constant. This file is touched on every SetTimeout, which the waiter calls
// once per poll for exactly the box it is keeping alive.
func (l boxLayout) aliveFile() string { return filepath.Join(l.meta(), "alive") }

// credentialsFile records the credential map this box was seeded with, so a
// reconnect knows which files to sync back. Without it a reconnected box
// closes with an empty map and the login the coding agent refreshed mid-run is
// discarded — and every production teardown goes through Connect or Kill,
// never the object Create returned, so that would be every run.
func (l boxLayout) credentialsFile() string { return filepath.Join(l.meta(), "credentials.json") }

// envFile is the container runtime's --env-file. See containerBox.envArgs.
func (l boxLayout) envFile() string { return filepath.Join(l.meta(), "env") }

// seedCredentials copies the CLI login into the box before the coding agent
// runs.
//
// The keys are relative paths from an operator-overridable profile, so they go
// through hostbox.SafeJoin: an entry like "../../etc/cron.d/x" would otherwise
// write a host file every time a box was created.
func seedCredentials(l boxLayout, files map[string]string) {
	for relative, source := range files {
		dst, err := hostbox.SafeJoin(l.root, relative)
		if err != nil {
			localLog.Warn("local_sandbox_credential_path_refused",
				"sandbox_id", l.id, "path", relative, "error", err.Error())
			continue
		}
		if _, err := hostbox.CopyFileAtomic(source, dst); err != nil {
			localLog.Warn("local_sandbox_credential_seed_failed",
				"sandbox_id", l.id, "path", relative, "error", err.Error())
		}
	}
}

// collectCredentials writes a credential the run refreshed back to the shared
// login.
//
// OAuth access tokens expire in hours and most vendors rotate the refresh
// token alongside them, so a box that quietly discards the rewritten file logs
// the whole fleet out at the next expiry. Only writes back over a file that
// already exists in the shared store, so a torn-down box can never CREATE a
// login the operator has since removed.
//
// Read through SafeJoin for the same reason the seed is written through it: a
// "../../" key would otherwise copy an arbitrary host file out of the box's
// directory and over the credential store.
func collectCredentials(l boxLayout, files map[string]string) {
	for relative, source := range files {
		src, err := hostbox.SafeJoin(l.root, relative)
		if err != nil {
			continue
		}
		if info, err := os.Stat(src); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info, err := os.Stat(source); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if hostbox.FileDigest(src) == hostbox.FileDigest(source) {
			continue
		}
		if ran, err := hostbox.CopyFileAtomic(src, source); err != nil {
			localLog.Warn("local_sandbox_credential_writeback_failed",
				"sandbox_id", l.id, "file", relative, "error", err.Error())
		} else if ran {
			localLog.Info("local_sandbox_credential_refreshed", "sandbox_id", l.id, "file", relative)
		}
	}
}

// recordCredentialMap persists the box's credential map so Connect can sync it
// back.
//
// It RETURNS its failure rather than logging it, even though the caller only
// logs: the previous shape wrote the WriteFile error into an `if err :=`
// binding that shadowed the one being checked, so the warning this function
// exists to emit could never fire and no test could have caught it. A
// returned error is the shape a test can assert on.
func recordCredentialMap(l boxLayout, files map[string]string) error {
	if len(files) == 0 {
		return nil
	}
	blob, err := json.Marshal(files)
	if err != nil {
		return fmt.Errorf("encoding the credential map: %w", err)
	}
	if err := os.MkdirAll(l.meta(), hostbox.DirMode); err != nil {
		return fmt.Errorf("preparing %q: %w", l.meta(), err)
	}
	if err := os.WriteFile(l.credentialsFile(), blob, hostbox.FileMode); err != nil {
		return fmt.Errorf("writing %q: %w", l.credentialsFile(), err)
	}
	return nil
}

// readCredentialMap is the map recorded at Create; empty when absent.
func readCredentialMap(l boxLayout) map[string]string {
	blob, err := os.ReadFile(l.credentialsFile())
	if err != nil {
		return nil
	}
	var files map[string]string
	if err := json.Unmarshal(blob, &files); err != nil {
		return nil
	}
	return files
}

// touchAlive refreshes the box's keepalive stamp. Never fatal.
func touchAlive(l boxLayout) {
	if err := os.MkdirAll(l.meta(), hostbox.DirMode); err != nil {
		localLog.Debug("local_sandbox_keepalive_unwritable", "sandbox_id", l.id, "error", err.Error())
		return
	}
	now := time.Now()
	if err := os.Chtimes(l.aliveFile(), now, now); err == nil {
		return
	}
	f, err := os.OpenFile(l.aliveFile(), os.O_CREATE|os.O_WRONLY, hostbox.FileMode)
	if err != nil {
		localLog.Debug("local_sandbox_keepalive_unwritable", "sandbox_id", l.id, "error", err.Error())
		return
	}
	if err := f.Close(); err != nil {
		// A keepalive that did not land is a box the reaper will treat as
		// an orphan, so the failure is worth the same line as the open.
		localLog.Debug("local_sandbox_keepalive_unwritable", "sandbox_id", l.id, "error", err.Error())
	}
}

// readPID is the detached job's pid, or 0.
func readPID(l boxLayout) int {
	raw, err := os.ReadFile(l.pidFile())
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return pid
}

// ---------------------------------------------------------------------
// provider
// ---------------------------------------------------------------------

// LocalOptions configures a [Local].
type LocalOptions struct {
	// Placement is Direct (default) or Container. It is the ONE thing that
	// differs between the two local cells, which is why one backend serves
	// both: everything else — the box root, the reap, the process group,
	// the ask shim — is identical.
	Placement Placement

	// StateDir is where boxes are created. Empty takes [DefaultBoxRoot].
	StateDir string

	// Image is the container image. REQUIRED for Container, and it must have
	// the coding-agent CLI installed: there is no sensible default, and a box
	// without the CLI fails only once an agent tries to use it.
	Image string

	// Runtime is "auto" (default; docker then podman), "docker" or "podman".
	Runtime string

	// Network is passed to the runtime's --network.
	Network string

	// RunArgs are extra arguments spliced into the container run.
	RunArgs []string
}

// Local mints sandboxes on the engine host, directly or in a container.
//
// The flagship backend: it exists so a role can do real code work with the
// coding CLI the operator already logged in to — no account, no API key, no
// token minting. The run reads the same credential directory `crewlet llm
// login` wrote.
type Local struct {
	opts    LocalOptions
	root    string
	runtime string // resolved container binary; "" in direct mode
}

var _ Provider = (*Local)(nil)

// NewLocal validates the options and resolves the container runtime once,
// rather than on every Create.
func NewLocal(opts LocalOptions) (*Local, error) {
	if opts.Placement == "" {
		opts.Placement = Direct
	}
	if opts.Placement != Direct && opts.Placement != Container {
		return nil, localErrorf("the local backend runs %q or %q, not %q — a "+
			"remote placement is another backend's",
			Direct, Container, opts.Placement)
	}
	// Refused at CONSTRUCTION, not at the first Create: an operator whose
	// platform cannot run this backend should be told when the config is
	// applied, not when an agent first tries to do code work.
	if !localSupported {
		return nil, localErrorf("%s", unsupportedReason)
	}
	root := opts.StateDir
	if root == "" {
		root = DefaultBoxRoot()
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, localErrorf("providers.sandbox.local.state_dir %q is not a usable path: %v", root, err)
	}
	local := &Local{opts: opts, root: abs}
	if opts.Placement == Container {
		if strings.TrimSpace(opts.Image) == "" {
			return nil, localErrorf("run_in %q requires providers.sandbox.local.image, an image "+
				"that has the coding-agent CLI installed — there is no sensible default, and a "+
				"box without the CLI fails only once an agent tries to use it", Container)
		}
		if local.runtime, err = ResolveContainerRuntime(opts.Runtime); err != nil {
			return nil, err
		}
	}
	return local, nil
}

// Kind names the backend, for logs and the operator surface.
func (l *Local) Kind() string { return "local" }

// layout is where one box's files live.
//
// AN EMPTY ID IS REFUSED, and this is not defensive tidiness: joining "" gives
// the boxes DIRECTORY ITSELF, so an empty id produces a "box" whose root is
// the parent of every box on the host — one whose Close or Kill would
// os.RemoveAll every other box's checkout. It is reachable: a run's row exists
// before its box is attached, so a poll landing in that window asks to connect
// to "".
func (l *Local) layout(id string) (boxLayout, error) {
	if strings.TrimSpace(id) == "" {
		return boxLayout{}, localErrorf("local sandbox: an empty sandbox id names no box " +
			"(it would resolve to the directory holding every box)")
	}
	if strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		// Ids are minted here and never come from config, but the same
		// join is what a traversal would exploit, and the check is one
		// comparison on a path that is about to be deleted from.
		return boxLayout{}, localErrorf("local sandbox: %q is not a usable box id", id)
	}
	return boxLayout{id: id, root: filepath.Join(l.root, "boxes", id)}, nil
}

func (l *Local) containerName(id string) string { return ContainerPrefix + id }

// Create mints a box and, in container mode, starts the container behind it.
func (l *Local) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	id := uuid.NewString()
	id = strings.ReplaceAll(id, "-", "")[:16]
	layout, err := l.layout(id)
	if err != nil {
		return nil, err
	}

	if err = os.MkdirAll(layout.workspace(), hostbox.DirMode); err != nil {
		return nil, localErrorf("could not create local sandbox %s at %s: %v", id, layout.root, err)
	}
	// MkdirAll honours umask, so the mode is asserted rather than requested:
	// a box holds a credential and must not be group- or world-readable on a
	// host that runs other services.
	if err = os.Chmod(layout.root, hostbox.DirMode); err != nil {
		return nil, localErrorf("could not secure local sandbox %s at %s: %v", id, layout.root, err)
	}
	if err = os.MkdirAll(filepath.Join(layout.root, ".tmp"), hostbox.DirMode); err != nil {
		return nil, localErrorf("could not create local sandbox %s tmpdir: %v", id, err)
	}

	l.reapOrphans(ctx, time.Duration(spec.TimeoutSec)*time.Second)
	seedCredentials(layout, spec.CredentialFiles)
	// The box records what it was seeded with and stamps itself alive before
	// anyone can reap it.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := recordCredentialMap(layout, spec.CredentialFiles); err != nil {
		// The map names paths, not secrets, but losing it means a
		// refreshed login is never written back to the shared directory
		// — worth saying out loud, and not worth failing the box for.
		localLog.Warn("local_sandbox_credential_map_unwritable", "sandbox_id", layout.id, "error", err.Error())
	}
	touchAlive(layout)

	if l.opts.Placement == Direct {
		localLog.Info("local_sandbox_created", "sandbox_id", id, "placement", string(Direct), "home", layout.root)
		return &directBox{layout: layout, env: spec.Env, credentials: spec.CredentialFiles}, nil
	}

	name := l.containerName(id)
	argv := l.runArgv(ctx, layout, name)

	result, err := runHost(ctx, hostCommand{argv: argv})
	if err != nil || result.ExitCode != 0 {
		_ = os.RemoveAll(layout.root)
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(result.Stdout)
		}
		if err != nil {
			detail = err.Error()
		}
		// THE WHOLE DETAIL. This is the container runtime's own account of
		// why the image would not start — a missing platform, a pull
		// failure, a mount that does not exist — and the operator reading
		// it has no other copy. A 400-byte cut removed the second half of
		// exactly the messages worth reading.
		return nil, localErrorf("could not start sandbox container from image %q: %s",
			l.opts.Image, detail)
	}
	box := &containerBox{
		layout: layout, runtime: l.runtime, container: name,
		env: spec.Env, credentials: spec.CredentialFiles,
	}
	// PROVEN BEFORE THE BOX IS HANDED OUT. Both properties this mode rests
	// on fail silently and only much later — see verifyMount — so the box is
	// torn down here rather than returned to do half its job.
	if err := verifyMount(ctx, box, layout); err != nil {
		_, _ = runHost(context.WithoutCancel(ctx), hostCommand{
			argv: []string{l.runtime, "rm", "-f", name},
		})
		_ = os.RemoveAll(layout.root)
		return nil, err
	}
	localLog.Info("local_sandbox_created",
		"sandbox_id", id, "placement", string(Container), "image", l.opts.Image, "container", name)
	return box, nil
}

// runArgv is the container-run command line for one box.
//
// Separated from Create so the argument order is assertable without a running
// daemon: what an operator's own run_args may override is a function of where
// they are spliced, and nothing else says it.
func (l *Local) runArgv(ctx context.Context, layout boxLayout, name string) []string {
	argv := []string{
		l.runtime, "run", "-d",
		"--name", name,
		"-v", layout.root + ":" + DefaultHome,
		"-w", DefaultHome + "/" + WorkspaceSubdir,
		"-e", "HOME=" + DefaultHome,
		// A reaping PID 1. The detached coding job is backgrounded inside an
		// `exec`, so without an init that reaps it, a finished job lingers as
		// a zombie — which `kill -0` reports as ALIVE, hanging the runner's
		// completion check on a job that already died.
		"--init",
	}
	argv = append(argv, containerUserArgs(ctx, l.runtime)...)
	if l.opts.Network != "" {
		argv = append(argv, "--network", l.opts.Network)
	}
	// LAST, so an operator's own flags win. Both runtimes take the final
	// occurrence of a repeated flag, which makes run_args the escape hatch
	// for an image that genuinely needs its own user (`--user 0:0`) without
	// a config knob whose only job is to undo the line above.
	argv = append(argv, l.opts.RunArgs...)
	// PID 1 exists only to hold the namespace open; every command is an
	// `exec` into it, which is what makes a detached coding job survive the
	// turn that started it.
	return append(argv, l.opts.Image, "sleep", "infinity")
}

// Connect reattaches to an existing box by id, live or paused.
//
// It AUTO-RESUMES, matching a remote provider: a paused box the coordinator
// reattaches to must be runnable immediately. That is exactly why the pause
// reaper must use Kill instead.
func (l *Local) Connect(ctx context.Context, sandboxID string) (Sandbox, error) {
	layout, err := l.layout(sandboxID)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(layout.root); err != nil || !info.IsDir() {
		return nil, localErrorf("local sandbox %q is gone (its box directory %s no longer "+
			"exists) — the engine host was rebuilt, or the box was reaped", sandboxID, layout.root)
	}
	// Rebuild the credential map from the box's own record, so a reconnected
	// teardown still writes a refreshed login back.
	credentials := readCredentialMap(layout)
	if l.opts.Placement == Direct {
		box := &directBox{layout: layout, credentials: credentials}
		box.resume()
		return box, nil
	}
	box := &containerBox{
		layout: layout, runtime: l.runtime,
		container: l.containerName(sandboxID), credentials: credentials,
	}
	box.unpause(ctx)
	return box, nil
}

// Kill terminates a box by id WITHOUT resuming it.
//
// The pause reaper's primitive: Connect resumes, so reclaiming a paused box
// through it would restart the work purely to stop it. Best-effort — a box
// that is already gone is not an error.
func (l *Local) Kill(ctx context.Context, sandboxID string) error {
	layout, err := l.layout(sandboxID)
	if err != nil {
		// Nothing to reclaim, and refusing is what stops the join below
		// resolving to the directory that holds every box.
		localLog.Warn("local_sandbox_kill_refused", "sandbox_id", sandboxID, "error", err.Error())
		return nil
	}
	if l.opts.Placement == Container {
		_, _ = runHost(ctx, hostCommand{argv: []string{l.runtime, "rm", "-f", l.containerName(sandboxID)}})
	} else if pid := readPID(layout); pid > 0 {
		// SIGKILL alone, with NO SIGCONT first. A stopped process is killed
		// by SIGKILL directly — the signal cannot be caught, blocked or
		// ignored, so it needs no scheduling to take effect. Waking the
		// group first would let a paused run execute again, briefly, on its
		// way out: this method exists precisely so a reaped clarification
		// pause is never resumed, and the SIGCONT that Close needs (a
		// stopped process really does never run to handle SIGTERM) would
		// quietly defeat that here.
		logSignal("kill", pid, procgroup.Kill(pid))
		// And WAIT for it to actually go. SIGKILL is delivered, not
		// applied: the kernel takes the process down some moments later,
		// and until it does the coding agent's wrapper is still writing
		// its exit code and marker into the box. Removing the tree under
		// those writes fails with the directory non-empty — a failure the
		// removal below would report and nothing could act on, leaving a
		// box for the orphan reaper to find hours later.
		awaitGroupExit(ctx, pid)
	}
	// Kill is a teardown like any other, and the box on disk may hold a login
	// the run refreshed before it was reclaimed. The files are already there
	// — collecting them needs no resume, which is the one thing this method
	// must not do.
	collectCredentials(layout, readCredentialMap(layout))
	removeBox(layout)
	localLog.Debug("local_sandbox_killed", "sandbox_id", sandboxID)
	return nil
}

// removeBox deletes a box's directory, saying so when it cannot.
//
// A box that survives its own teardown is not harmless: it holds the seeded
// login and whatever the run wrote, and the only thing that will ever clean it
// up is the orphan reaper on some later create. Reported rather than swallowed
// so the operator learns of it now instead of finding the directory later.
func removeBox(l boxLayout) {
	if err := os.RemoveAll(l.root); err != nil {
		localLog.Warn("local_sandbox_not_removed", "sandbox_id", l.id,
			"path", l.root, "error", err.Error())
	}
}

// awaitGroupExit waits for a signalled process group to actually be gone.
//
// Bounded by [termGrace] and by the caller's context: a group that will not go
// is not a reason to hold a teardown open forever, and the caller proceeds
// either way.
func awaitGroupExit(ctx context.Context, pid int) {
	deadline := time.Now().Add(termGrace)
	for time.Now().Before(deadline) {
		if !procgroup.Exists(pid) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(exitPollInterval):
		}
	}
}

// exitPollInterval is how often a teardown asks whether a signalled group has
// gone. Short, because the answer is usually yes on the first ask and the
// caller is holding a turn open.
const exitPollInterval = 20 * time.Millisecond

// ---------------------------------------------------------------------
// orphan reaping
// ---------------------------------------------------------------------

// reapOrphans removes box directories an engine crash left behind.
//
// Teardown normally deletes a box, so anything here outlived the engine that
// made it. Bounded by the box TTL the caller already configures rather than a
// knob of its own — that value is exactly "how long a box may outlive its
// engine".
//
// AGE ALONE DOES NOT MEAN ORPHANED, and reading it off the directory's mtime
// does not even mean age. A directory's mtime moves only when an entry is
// added or removed directly in it, and a box's root entries are all made at
// Create — so the root mtime is the box's birth time, frozen, however busy the
// coding agent inside workspace/ is. Reaping on it deleted the checkout, the
// seeded credentials and .crewlet/box.pid of every run that lasted longer than
// the TTL, while its process tree kept going: without the pid file nothing
// could ever kill it, so the job became an unkillable orphan writing into a
// directory that no longer existed.
//
// So the two questions are asked separately. IS IT IN USE? — a live process
// group (direct) or an existing container, checked against the OS rather than
// inferred. HAS IT BEEN ABANDONED? — the keepalive stamp the waiter refreshes
// on every poll, which is what SetTimeout means on this backend. A box is
// reaped only when it fails BOTH: nothing running, and nothing has touched it
// for a whole TTL.
//
// A paused box is deliberately covered by the first: SIGSTOPped processes stop
// being heart-beaten but stay alive, and bounding THAT wait belongs to the
// clarification pause TTL, which already owns it.
func (l *Local) reapOrphans(ctx context.Context, olderThan time.Duration) {
	boxes := filepath.Join(l.root, "boxes")
	entries, err := os.ReadDir(boxes)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-max(minReapAge, olderThan))
	var candidates []boxLayout
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		layout := boxLayout{id: entry.Name(), root: filepath.Join(boxes, entry.Name())}
		if lastSeen(layout).Before(cutoff) {
			candidates = append(candidates, layout)
		}
	}
	if len(candidates) == 0 {
		// The common case, and the reason age is tested first: this runs on
		// Create, which an agent is waiting on, so a fleet with nothing stale
		// pays no liveness call at all.
		return
	}

	var live map[string]bool
	if l.opts.Placement == Container {
		if live, err = l.liveContainers(ctx); err != nil {
			// Cannot tell what is alive, so reap nothing: deleting a live
			// box's checkout is unrecoverable, a lingering directory is not.
			return
		}
	}
	for _, layout := range candidates {
		if l.boxIsAlive(layout, live) {
			continue
		}
		collectCredentials(layout, readCredentialMap(layout))
		removeBox(layout)
		localLog.Info("local_sandbox_orphan_reaped", "sandbox_id", layout.id)
	}
}

// lastSeen is when this box was last known to be in use: the keepalive stamp
// when there is one, else the directory's own mtime — which is the box's
// CREATION time and nothing else.
func lastSeen(l boxLayout) time.Time {
	if info, err := os.Stat(l.aliveFile()); err == nil {
		return info.ModTime()
	}
	if info, err := os.Stat(l.root); err == nil {
		return info.ModTime()
	}
	// Vanished mid-scan. Report it as brand new so this pass leaves it alone
	// rather than racing whatever is deleting it.
	return time.Now()
}

// boxIsAlive reports whether this box still has something running in it.
func (l *Local) boxIsAlive(layout boxLayout, live map[string]bool) bool {
	if l.opts.Placement == Container {
		return live[l.containerName(layout.id)]
	}
	pid := readPID(layout)
	if pid <= 0 {
		return false
	}
	info, err := os.Stat(layout.root)
	if err != nil {
		return false
	}
	return processGroupAlive(pid, info.ModTime())
}

// liveContainers names every crewlet container this runtime still has.
//
// One listing per reap, rather than an inspect per box: the reap runs on the
// Create path, which an agent is waiting on.
func (l *Local) liveContainers(ctx context.Context) (map[string]bool, error) {
	result, err := runHost(ctx, hostCommand{argv: []string{
		l.runtime, "ps", "-a", "--filter", "name=" + ContainerPrefix, "--format", "{{.Names}}",
	}})
	if err != nil || result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if err != nil {
			detail = err.Error()
		}
		localLog.Warn("local_sandbox_reap_listing_failed", "error", detail)
		// WRAPPED, and carrying the detail. The reaper reads this to decide
		// which boxes are live, so a listing failure that reported only
		// "container listing failed" left the caller unable to say whether
		// the runtime was down, unreachable or refusing — three different
		// operator actions behind one string.
		if err != nil {
			return nil, fmt.Errorf("listing sandbox containers: %w", err)
		}
		return nil, localErrorf("listing sandbox containers failed with exit %d: %s",
			result.ExitCode, detail)
	}
	names := map[string]bool{}
	for line := range strings.SplitSeq(result.Stdout, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names[name] = true
		}
	}
	return names, nil
}

// logSignal reports a signal that did not reach its process group.
//
// Every caller is on a teardown or a pause path, where there is nothing to
// return the failure to and nothing useful to do about it — but a kill that
// silently did not land leaves a coding agent running on the engine host,
// which is the one outcome an operator has to be able to find. An empty group
// is not a failure and never reaches here: procgroup answers ESRCH as success.
func logSignal(what string, pid int, err error) {
	if err == nil {
		return
	}
	localLog.Warn("local_sandbox_signal_failed", "signal", what, "pgid", pid, "error", err.Error())
}
