package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/logging"
)

// e2bLog names the remote backend, so its lines are not attributed to local.go's
// "sandbox.local" logger.
var e2bLog = logging.Get("sandbox.e2b")

// The remote backend: every box a fresh VM, on E2B cloud or a self-hosted
// cluster, never the engine host, so an agent's mistake takes only its own box
// (the local backend suits a company on its own machine and checkouts).
//
// A [Spec]'s subscription login files are IGNORED here, the one field the two
// backends deliberately treat apart: their refresh token's rotation is shared
// fleet state, too much trust to push onto a VM somebody else operates. A
// subscription still works through the scoped headless token the run
// environment exports; a harness that mints none needs the local backend.
//
// A template is how a box is SIZED: vCPU and RAM are fixed when it is built,
// and the create API takes no resource arguments ([e2bAgentTemplates]).

// E2BKind is the backend's name on the operator surface.
const E2BKind = "e2b"

// e2bAgentTemplates are E2B's prebuilt images, by harness: a company naming no
// template gets its agent's, since a bare box has no harness and fails at the
// first command with "command not found", which reads as a broken runner.
var e2bAgentTemplates = map[string]string{
	"claude-code": "claude",
	"opencode":    "opencode",
}

// e2bFallbackTemplate is what an unrecognised harness gets: E2B's minimal
// "base", with no harness, so the run fails. That is deliberate: an agent this
// build does not know is a config problem, and inventing a template for it
// would hide it behind a runtime error further in.
const e2bFallbackTemplate = "base"

// E2BProvider mints sandboxes on E2B.
type E2BProvider struct {
	api      *e2bAPI
	template string
	http     *http.Client
}

// E2BOptions configure an [E2BProvider].
type E2BOptions struct {
	// APIKey authenticates every control-plane call. REQUIRED, including
	// against a self-hosted cluster: `domain` changes which API is talked
	// to, never whether it authenticates.
	APIKey string

	// Domain is a self-hosted cluster, or empty for the vendor cloud.
	Domain string

	// Template is the company-wide default box image, or empty to pick one
	// per harness.
	Template string

	// HTTP is the client both planes use. Nil takes one bounded by
	// [E2BClientTimeout].
	HTTP *http.Client
}

// NewE2B builds the remote backend.
func NewE2B(opts E2BOptions) (*E2BProvider, error) {
	if strings.TrimSpace(opts.APIKey) == "" {
		// REFUSED AT CONSTRUCTION, so the apply fails rather than every
		// launch, each a 401 at its first create, minutes into a turn.
		return nil, errors.New(
			"e2b: providers.sandbox.e2b.api_key resolved empty — the API " +
				"authenticates every call, including against a self-hosted " +
				"cluster, where `domain` changes which API is talked to and " +
				"not whether it authenticates")
	}
	client := opts.HTTP
	if client == nil {
		client = httpx.Client(E2BClientTimeout)
	}
	return &E2BProvider{
		api:      newE2BAPI(opts.APIKey, opts.Domain, client),
		template: strings.TrimSpace(opts.Template),
		http:     client,
	}, nil
}

// Kind implements [Provider].
func (p *E2BProvider) Kind() string { return E2BKind }

// templateFor is the image one run gets: the company's, then the harness's own.
// It also decides the box's vCPU, RAM and disk (providers.sandbox.e2b.template).
func (p *E2BProvider) templateFor(spec Spec) string {
	if p.template != "" {
		return p.template
	}
	if t, ok := e2bAgentTemplates[spec.Harness]; ok {
		return t
	}
	return e2bFallbackTemplate
}

// Create implements [Provider].
//
// EVERY BOX IS PRIVATE AND SECURED ([e2bAPI.createBox]): E2B's proxy passes a
// request to a port but envd's only with the box's traffic token, and envd
// refuses a request without the box's access token, which every handle this
// backend builds carries ([e2bBox.EnvdAccessToken]) and every envd call
// presents.
func (p *E2BProvider) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	template := p.templateFor(spec)
	box, err := p.api.createBox(ctx, template, spec.TimeoutSec, spec.Env)
	if err != nil {
		return nil, err
	}
	e2bLog.Info("e2b_sandbox_created", "sandbox_id", box.SandboxID,
		"template", template, "domain", p.api.domain,
		"envd_version", box.EnvdVersion, "envd_token", box.EnvdAccessToken != "")
	return p.box(box), nil
}

// Connect implements [Provider].
//
// ONE CALL, WHATEVER STATE THE BOX IS IN: [e2bAPI.connectBox] reads the box and
// resumes it if paused, so no reaper can race a read and a decision.
//
// A FAILURE IS RETURNED, never swallowed: "already running" is a success here,
// so anything left really failed, and a handle to a still-paused box would fail
// every command one layer further in, with envd's message instead.
func (p *E2BProvider) Connect(ctx context.Context, sandboxID string) (Sandbox, error) {
	// A FRESH TTL: a snapshot has no kill timer running, so a box woken without
	// one keeps what remained of its old timer — for a run parked overnight,
	// nothing.
	box, err := p.api.connectBox(ctx, sandboxID, defaultSandboxTTLSeconds)
	if err != nil {
		var apiErr *E2BError
		if errors.As(err, &apiErr) && apiErr.Gone() {
			return nil, boxGone{fmt.Errorf("e2b: sandbox %s is gone: %w", sandboxID, err)}
		}
		return nil, fmt.Errorf("e2b: sandbox %s could not be connected to: %w", sandboxID, err)
	}
	e2bLog.Debug("e2b_sandbox_connected", "sandbox_id", sandboxID)
	return p.box(box), nil
}

// Attach implements [Provider]: the box READ, never resumed. A paused box has
// no envd to answer and a reader must not wake it, so it is refused with
// [ErrBoxPaused]. A running one is reached through envd as a connected one is,
// its timer left where the waiter's keepalive set it (/connect would reset it
// to 900 seconds on every read).
func (p *E2BProvider) Attach(ctx context.Context, sandboxID string) (Sandbox, error) {
	box, err := p.api.getBox(ctx, sandboxID)
	if err != nil {
		var apiErr *E2BError
		if errors.As(err, &apiErr) && apiErr.Gone() {
			return nil, boxGone{fmt.Errorf("e2b: sandbox %s is gone: %w", sandboxID, err)}
		}
		return nil, fmt.Errorf("e2b: sandbox %s could not be read: %w", sandboxID, err)
	}
	if box.State == e2bPaused {
		return nil, boxPaused{fmt.Errorf("e2b: sandbox %s is paused, and a paused box cannot be "+
			"read without resuming it", sandboxID)}
	}
	return p.box(box), nil
}

// defaultSandboxTTLSeconds is the kill timer a resumed box wakes with: the
// config layer's default box TTL, repeated because this package does not import
// config. It need only outlast the gap until the waiter's first keepalive tick,
// which comes at once since a reused box's job starts with no setup.
const defaultSandboxTTLSeconds = 900

// Kill implements [Provider].
func (p *E2BProvider) Kill(ctx context.Context, sandboxID string) error {
	if err := p.api.killBox(ctx, sandboxID); err != nil {
		return err
	}
	e2bLog.Debug("e2b_sandbox_killed", "sandbox_id", sandboxID)
	return nil
}

func (p *E2BProvider) box(b e2bBox) *E2BSandbox {
	return &E2BSandbox{
		id:   b.SandboxID,
		api:  p.api,
		envd: newEnvdClient(b.host(p.api.domain), b.EnvdAccessToken, p.http),
	}
}

// E2BSandbox is one live box behind the [Sandbox] contract.
type E2BSandbox struct {
	id   string
	api  *e2bAPI
	envd *envdClient
}

// ID implements [Sandbox].
func (s *E2BSandbox) ID() string { return s.id }

// Home implements [Sandbox]: every E2B template runs as one account with one
// home, so it is the package default, not a per-box path as on the local
// backend, where boxes share a filesystem.
func (s *E2BSandbox) Home() string { return DefaultHome }

// Exec implements [Sandbox].
//
// A NON-ZERO EXIT IS DATA, NOT AN ERROR: the liveness probe reads a failed
// `kill -0` as "gone", and setup reports a failed command. An error is for a
// command the box gave no status for at all.
//
// A STATUS IS READ AS A LOCAL BOX REPORTS ONE, so a setup failure reads the same
// in either cell: a timeout is 124, named on stderr before the output; a signal
// (an OOM kill included) is 128 plus its number, named after the output. envd
// reports a signal as exit -1 beside its name, and a timeout as a stream closed
// with `deadline_exceeded`, usually before any end event — neither is a box gone.
func (s *E2BSandbox) Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
	res, err := s.envd.start(ctx, cmd, opts, false)
	if err != nil {
		return ExecResult{}, err
	}
	failed := ExecResult{ExitCode: 1, Stdout: res.Stdout, Stderr: res.Stderr}
	switch {
	case res.Ended && res.ExitCode >= 0:
		return ExecResult{
			ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr,
		}, nil
	case res.Trailer != nil && res.Trailer.Code == connectDeadline && opts.TimeoutSec > 0:
		// Checked BEFORE the signal: envd kills the process when the
		// timeout runs out, so an end event reporting that kill can
		// arrive beside the deadline — and the timeout is the cause.
		detail := fmt.Sprintf("timed out after %s",
			time.Duration(opts.TimeoutSec*float64(time.Second)))
		if said := strings.TrimSpace(res.Stderr); said != "" {
			detail += "\n" + said
		}
		return ExecResult{ExitCode: 124, Stdout: res.Stdout, Stderr: detail}, nil
	case res.Signal > 0:
		note := fmt.Sprintf("killed by signal %d (%s)", res.Signal, res.SignalName)
		if said := strings.TrimRight(res.Stderr, "\n"); said != "" {
			note = said + "\n" + note
		}
		return ExecResult{ExitCode: 128 + res.Signal, Stdout: res.Stdout, Stderr: note}, nil
	case res.Ended:
		// An end event with no exit code and no signal this can read: say
		// what envd said rather than invent a status.
		return failed, fmt.Errorf("e2b: the command ended as %q, which names "+
			"no exit status (sandbox %s)", res.Status, s.id)
	case res.Trailer != nil:
		// THE BOX'S AGENT GAVE UP ON THE COMMAND, which is the box failing
		// on its side rather than anything the command or its caller did.
		return failed, Because(WhyProviderUnavailable, fmt.Errorf("e2b: the box's agent "+
			"ended the command's stream with %s (%s) before it reported an exit code "+
			"(sandbox %s)", res.Trailer.Code, res.Trailer.Message, s.id))
	default:
		// No verdict and no reason: the box was paused or reclaimed
		// mid-command. A failure with what was captured, never exit 0,
		// which would pass an abandoned command as a success.
		return failed, Because(WhyProviderUnavailable, fmt.Errorf("e2b: the command's "+
			"stream ended before it reported an exit code, so the box went away "+
			"mid-command (sandbox %s)", s.id))
	}
}

// StartBackground implements [Sandbox].
func (s *E2BSandbox) StartBackground(ctx context.Context, cmd string, opts ExecOptions) (string, error) {
	res, err := s.envd.start(ctx, cmd, opts, true)
	if err != nil {
		return "", err
	}
	if res.PID == 0 {
		return "", fmt.Errorf(
			"e2b: the detached start named no process, so nothing here can "+
				"tell whether the job is running (sandbox %s)", s.id)
	}
	return strconv.Itoa(res.PID), nil
}

// Job implements [Sandbox]: the handle is a pid inside the VM, so `kill -0`
// there answers it, and the VM outlives the engine ([remoteJob]).
func (s *E2BSandbox) Job(ctx context.Context, commandID string) (JobState, error) {
	return remoteJob(ctx, s, commandID)
}

// EndJob implements [Sandbox]: script is a pid inside the VM, signalled there
// as the box's user ([endByKill]).
func (s *E2BSandbox) EndJob(ctx context.Context, _ string, script int) error {
	return endByKill(ctx, s, script)
}

// KillJob implements [Sandbox]: EVERY PROCESS OF THE BOX'S USER, `kill -1` as
// `user` ([killUserScript]). envd starts a job in envd's own process group,
// which no signal may reach, so the job's tree has no group to address; envd
// runs as root and is spared; and the box is the run's alone and reclaimed
// right after, so a template's service running as the user goes a little early.
//
// kill(-1) spares its caller, and the script's shell is the process envd
// answers by: envd runs `/bin/sh -c` of a line that execs, through ionice and
// nice, the script's `/bin/bash -l -c` (e2b-dev/infra's envd process handler),
// one process all the way down, so the call returns once the script has run
// its waits. Whether what it killed is over is the job's poll to say.
func (s *E2BSandbox) KillJob(ctx context.Context, _ string) error {
	if _, err := s.Exec(ctx, killUserScript(), ExecOptions{Cwd: DefaultHome}); err != nil {
		return fmt.Errorf("e2b: ending the job in box %s: %w", s.id, err)
	}
	return nil
}

// killUserScript is [E2BSandbox.KillJob]'s script: `kill -1` as the box's user,
// in [killScript]'s shape, each wait over once no process of the user's is left
// but the script itself ([userLive]) — NOT once `kill -0 -1` fails, which in a
// VM it never does: kill(-1) succeeds wherever it counted a process, refused
// ones included (the kernel's kill_something_info), and root's kernel threads
// and envd are always there, so every kill would run both its graces out. The
// user's id is the shell's own, read as the walk reads every process's.
func killUserScript() string {
	return `u=; while read -r k v r; do [ "$k" != Uid: ] || { u=$v; break; }; done < /proc/$$/status
` + userLive("/proc") + killAndAwait("-1", "userlive")
}

// userLive defines the shell function `userlive`: whether root (a /proc) holds a
// process, other than the shell itself, that kill(-1) sent as user id $u reaches
// and that has not exited — real or saved uid $u, the ids the kernel lets that
// sender signal, and no zombie. Builtins only, so the walk forks nothing it
// would then find; a record that goes as it is read is a process that went.
func userLive(root string) string {
	quoted := "'" + strings.ReplaceAll(root, "'", `'\''`) + "'"
	return `userlive() { for d in ` + quoted + `/[0-9]*; do [ "${d##*/}" != "$$" ] || continue; z= m=; ` +
		`while read -r k a b c r; do case "$k" in State:) [ "$a" != Z ] || z=1 ;; ` +
		`Uid:) if [ "$a" = "$u" ] || [ "$c" = "$u" ]; then m=1; fi; break ;; esac; done < "$d/status"; ` +
		`[ -z "$m" ] || [ -n "$z" ] || return 0; done 2> /dev/null; return 1; }
`
}

// WriteFile implements [Sandbox]: the file is REPLACED whole, never uploaded
// over in place, which is how envd writes an upload ([envdClient.writeFile]).
func (s *E2BSandbox) WriteFile(ctx context.Context, path string, content []byte) error {
	return s.envd.writeFile(ctx, path, content)
}

// WriteStream implements [Sandbox]: the same staged upload and move, its body
// streamed to envd as it is read ([envdClient.writeStream]).
func (s *E2BSandbox) WriteStream(ctx context.Context, path string, content io.Reader) error {
	return s.envd.writeStream(ctx, path, content)
}

// ReadFile implements [Sandbox].
func (s *E2BSandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return s.envd.readFile(ctx, path)
}

// OpenFile implements [Sandbox]: envd's download body, read as it arrives.
func (s *E2BSandbox) OpenFile(ctx context.Context, path string) (io.ReadCloser, error) {
	return s.envd.openFile(ctx, path)
}

// ReadTail implements [Sandbox]: a suffix range on envd's download.
func (s *E2BSandbox) ReadTail(ctx context.Context, path string, n int) (FileTail, error) {
	return s.envd.readTail(ctx, path, n)
}

// OpenTail implements [Sandbox]: a suffix range on envd's download, its body
// read as it arrives.
func (s *E2BSandbox) OpenTail(ctx context.Context, path string, n int64) (io.ReadCloser, int64, error) {
	return s.envd.openTail(ctx, path, n)
}

// SetTimeout implements [Sandbox]: the keepalive.
func (s *E2BSandbox) SetTimeout(ctx context.Context, seconds float64) error {
	if seconds <= 0 {
		return nil
	}
	return s.api.setBoxTimeout(ctx, s.id, seconds)
}

// Pause implements [Sandbox]. E2B holds a paused box INDEFINITELY and bills for
// the snapshot, so expiring it is the engine's job: the waiter's pause reaper.
func (s *E2BSandbox) Pause(ctx context.Context) error {
	return s.api.pauseBox(ctx, s.id)
}

// Close implements [Sandbox].
func (s *E2BSandbox) Close(ctx context.Context) error {
	return s.api.killBox(ctx, s.id)
}

// Both contracts, asserted at compile time.
var (
	_ Provider = (*E2BProvider)(nil)
	_ Sandbox  = (*E2BSandbox)(nil)
)
