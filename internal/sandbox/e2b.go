package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// The remote backend: a real VM per coding run, on E2B cloud or a
// self-hosted cluster.
//
// # What this backend is FOR, next to the local one
//
// The local backend runs the coding agent on the engine host, which is right
// for a company running on somebody's own machine against their own
// checkouts, and wrong for anything shared. This one gives every run a fresh
// machine that is not the engine's: an agent that deletes the wrong directory
// takes its own box with it.
//
// # Credential files never leave the engine host
//
// A [Spec] can carry a subscription CLI's login files, and this backend
// IGNORES them — the one place the two backends deliberately diverge on the
// same field. Those files hold a refresh token whose rotation is shared fleet
// state, so pushing them onto a VM somebody else operates is a materially
// larger trust step than the scoped headless token the run environment
// already exports. A subscription still works here through that token; a CLI
// that mints no headless token needs the local backend.
//
// # One template per coding agent, by default
//
// E2B publishes prebuilt templates with each coding-agent CLI installed, so a
// company that names none still gets a box the agent can run in. Naming one
// is how a box is SIZED — vCPU and RAM are properties of a template, fixed
// when it is built, and the create API takes no resource arguments at all.

// E2BKind is the backend's name on the operator surface.
const E2BKind = "e2b"

// e2bAgentTemplates are E2B's prebuilt images, by coding agent.
//
// A company that names no template gets the one matching its agent rather
// than a bare box, because a bare box has no coding-agent CLI in it and the
// run fails at the first command with "command not found" — a template
// problem that reads as a broken runner.
var e2bAgentTemplates = map[string]string{
	"claude-code": "claude",
	"opencode":    "opencode",
}

// e2bFallbackTemplate is what an unrecognised coding agent gets.
//
// "base" is E2B's own minimum-viable image. It carries no coding-agent CLI,
// so a run on it fails — which is correct and is the honest outcome: a
// company naming an agent this build does not know has a config problem, and
// inventing a template for it would hide that behind a runtime error further
// in.
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
	// per coding agent.
	Template string

	// HTTP is the client both planes use. Nil takes one bounded by
	// [E2BClientTimeout].
	HTTP *http.Client
}

// NewE2B builds the remote backend.
func NewE2B(opts E2BOptions) (*E2BProvider, error) {
	if strings.TrimSpace(opts.APIKey) == "" {
		// REFUSED AT CONSTRUCTION, so an apply fails rather than every
		// coding run: a provider built without a key would report a
		// configured sandbox and 401 at the first create, minutes into a
		// turn that already spent a Plan phase.
		return nil, errors.New(
			"e2b: providers.sandbox.api_key resolved empty — the API " +
				"authenticates every call, including against a self-hosted " +
				"cluster, where `domain` changes which API is talked to and " +
				"not whether it authenticates")
	}
	client := opts.HTTP
	if client == nil {
		client = &http.Client{Timeout: E2BClientTimeout}
	}
	return &E2BProvider{
		api:      newE2BAPI(opts.APIKey, opts.Domain, client),
		template: strings.TrimSpace(opts.Template),
		http:     client,
	}, nil
}

// Kind implements [Provider].
func (p *E2BProvider) Kind() string { return E2BKind }

// templateFor is the image one run gets: the spec's, then the company's,
// then the coding agent's own.
func (p *E2BProvider) templateFor(spec Spec) string {
	if t := strings.TrimSpace(spec.Template); t != "" {
		return t
	}
	if p.template != "" {
		return p.template
	}
	if t, ok := e2bAgentTemplates[spec.CodingAgent]; ok {
		return t
	}
	return e2bFallbackTemplate
}

// Create implements [Provider].
func (p *E2BProvider) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	template := p.templateFor(spec)
	box, err := p.api.createBox(ctx, template, spec.TimeoutSec, spec.Env)
	if err != nil {
		return nil, err
	}
	log.Info("e2b_sandbox_created", "sandbox_id", box.SandboxID,
		"template", template, "domain", p.api.domain,
		"envd_version", box.EnvdVersion)
	return p.box(box), nil
}

// Connect implements [Provider].
//
// RESUMES UNCONDITIONALLY, because it cannot know whether the box is paused
// and a resume on a running box is a no-op the API reports as a conflict. The
// alternative — read the state, then decide — is two round trips on the
// completion path to learn something the second call handles anyway, and it
// races: a box can be reaped between the read and the decision.
func (p *E2BProvider) Connect(ctx context.Context, sandboxID string) (Sandbox, error) {
	box, err := p.api.boxOf(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	// A FRESH TTL ON RESUME. A snapshot has no kill timer running, so a box
	// woken without one would carry whatever remained of the timer it was
	// paused with — which for a run parked overnight is nothing.
	//
	// A FAILURE HERE IS RETURNED, and the one benign case is already gone
	// by this point: [e2bAPI.resumeBox] maps "already running" onto
	// success, because that is what the caller wanted. Swallowing whatever
	// is left would hand back a handle to a box that is still paused, and
	// every command on it would fail one layer further in — with envd's
	// message rather than the control plane's.
	if err := p.api.resumeBox(ctx, sandboxID, defaultSandboxTTLSeconds); err != nil {
		var apiErr *E2BError
		if errors.As(err, &apiErr) && apiErr.Gone() {
			return nil, fmt.Errorf("e2b: sandbox %s is gone: %w", sandboxID, err)
		}
		return nil, fmt.Errorf("e2b: sandbox %s could not be resumed: %w", sandboxID, err)
	}
	log.Debug("e2b_sandbox_connected", "sandbox_id", sandboxID)
	return p.box(box), nil
}

// defaultSandboxTTLSeconds is the kill timer a resumed box wakes with.
//
// It matches the config layer's own default box TTL. The value is repeated
// rather than imported because this package does not import config — and the
// waiter refreshes the real TTL on its next tick regardless, so this only has
// to be long enough to survive the gap between resume and that tick.
const defaultSandboxTTLSeconds = 900

// Kill implements [Provider].
func (p *E2BProvider) Kill(ctx context.Context, sandboxID string) error {
	if err := p.api.killBox(ctx, sandboxID); err != nil {
		return err
	}
	log.Debug("e2b_sandbox_killed", "sandbox_id", sandboxID)
	return nil
}

func (p *E2BProvider) box(b e2bBox) *E2BSandbox {
	return &E2BSandbox{
		id:   b.SandboxID,
		api:  p.api,
		envd: newEnvdClient(b.host(p.api.domain), p.http),
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

// Home implements [Sandbox].
//
// Every E2B template runs as one account with one home, so this is the
// package default rather than a per-box path — unlike the local backend,
// where many boxes share a filesystem and each needs its own.
func (s *E2BSandbox) Home() string { return DefaultHome }

// Exec implements [Sandbox].
//
// A NON-ZERO EXIT IS DATA, NOT AN ERROR. Callers depend on it: the liveness
// probe reads a failed `kill -0` as "the process is gone", and setup turns a
// failed provisioning command into a reportable failure. An error return is
// reserved for not reaching the box at all.
func (s *E2BSandbox) Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
	res, err := s.envd.start(ctx, cmd, opts, false)
	if err != nil {
		return ExecResult{}, err
	}
	if res.ExitCode < 0 {
		// The stream ended without a verdict — the box was paused or
		// reclaimed mid-command. Reported as a failure with what was
		// captured, because claiming exit 0 would have the caller treat
		// an abandoned command as a successful one.
		return ExecResult{ExitCode: 1, Stdout: res.Stdout, Stderr: res.Stderr},
			fmt.Errorf("e2b: the command's stream ended before it reported an "+
				"exit code, so the box went away mid-command (sandbox %s)", s.id)
	}
	return ExecResult{
		ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr,
	}, nil
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

// WriteFile implements [Sandbox].
func (s *E2BSandbox) WriteFile(ctx context.Context, path string, content []byte) error {
	return s.envd.writeFile(ctx, path, content)
}

// ReadFile implements [Sandbox].
func (s *E2BSandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return s.envd.readFile(ctx, path)
}

// SetTimeout implements [Sandbox]: the keepalive.
func (s *E2BSandbox) SetTimeout(ctx context.Context, seconds float64) error {
	if seconds <= 0 {
		return nil
	}
	return s.api.setBoxTimeout(ctx, s.id, seconds)
}

// Pause implements [Sandbox].
//
// E2B holds a paused box INDEFINITELY and bills for the snapshot, which is
// exactly why expiring it is the engine's job rather than the provider's —
// see the waiter's pause reaper.
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
