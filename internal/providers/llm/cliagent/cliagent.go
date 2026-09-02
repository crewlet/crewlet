// Package cliagent drives a coding CLI the operator already pays a
// subscription for as a headless text model behind [llm.Provider].
//
// WHICH CLIs is profiles.yaml's answer, not this comment's — [BuiltinNames]
// reads the shipped table and `profile_test.go` holds it to the contract. A
// prose list here would be a second copy of that answer with nothing checking
// it, and it already went stale by three.
//
// The CLI holds the operator's OAuth login, so Crewlet never sees a password
// and never re-implements a vendor's auth. What makes this more than "shell
// out to a CLI" is three problems it has to solve first, and each is a file
// here:
//
//   - SHARED MEMORY. A CLI keeps sessions, history, todos and project notes
//     under one home, so seven seats on one subscription would read each
//     other's transcripts. Every call runs in its own seat home with HOME,
//     the XDG variables and the vendor's own relocation variable pointed
//     inside it, in an empty per-call working directory, with an ALLOWLISTED
//     environment rather than the engine's — see workspace.go and env.go.
//
//   - NO TOOL CHANNEL. The tool loop needs tool calls back, and a CLI prints
//     prose. The catalogue and a response contract ride in the prompt, and
//     the reply is parsed by a deliberately forgiving parser that never
//     errors — see prompt.go and envelope.go.
//
//   - VENDOR DRIFT. These flags and JSON shapes change between releases, so
//     every profile field is data an operator can replace from YAML rather
//     than a Go literal that needs a Crewlet release — see profiles.yaml.
//
// The backend obeys the same two rules as every other member of this
// contract: it does not retry, and it classifies a failure no further than a
// coarse kind. The one classification that is genuinely its own is a SPENT
// SUBSCRIPTION, which arrives as prose on a SUCCESSFUL exit and is recognised
// by a vendor's verbatim sentinel plus the reset instant it carries — never
// by keyword, which is how the same recognition was got wrong twice before.
package cliagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

var log = logging.Get("llm.cliagent")

// providerName is what a classified failure calls this backend.
const providerName = "cli-agent"

// Config is one cli-agent provider entry, fully resolved.
//
// Resolved, not referenced: Token and APIKey hold values, never "${VAR}".
// Tier B stores its references verbatim so an exported revision carries no
// secrets, and resolution happens once, here, where the provider is built.
type Config struct {
	// Key is the providers.llm key, for log lines and error messages.
	Key string
	// Model is passed to the CLI's model flag. Empty means the CLI's own
	// default, and a profile with no model flag requires it to be empty.
	Model string
	// Agent names the built-in profile.
	Agent string
	// StateDir is the resolved credential + per-seat home directory.
	StateDir string
	// Overrides are the operator's profile edits.
	Overrides map[string]any
	// Timeout caps one invocation, wall clock.
	Timeout time.Duration
	// MaxConcurrent caps this provider's simultaneous processes.
	MaxConcurrent int
	// Env is extra child environment, already ${VAR}-resolved.
	Env map[string]string
	// Auth is the resolved credential material and the mode that decides
	// which of it reaches the child.
	Auth Auth
	// CredentialBundle is a resolved bundle from `crewlet llm export`,
	// restored into an EMPTY credential directory at boot so a container
	// rebuilt on every deploy comes up already authenticated.
	CredentialBundle string
}

// Provider is a coding CLI behind the language-model contract.
type Provider struct {
	key     string
	model   string
	agent   string
	profile Profile
	ws      *Workspace
	env     map[string]string
	auth    Auth
	timeout time.Duration

	// slots caps concurrent child processes. A buffered channel rather
	// than a semaphore type because acquisition has to be selectable
	// against the caller's context: a seat whose turn was cancelled while
	// queueing must not go on to launch a process nobody is waiting for.
	slots chan struct{}
}

var _ llm.Provider = (*Provider)(nil)

// New builds a cli-agent provider.
//
// Like every other backend, it builds even when no login is present: the call
// then fails with the vendor's own "not authenticated", which names the CLI
// and is exactly what `crewlet llm doctor` explains. A constructor that
// refused to exist would take the whole company down at boot over one
// provider's credentials.
func New(cfg Config) (*Provider, error) {
	profile, err := Load(cfg.Agent, cfg.Overrides)
	if err != nil {
		return nil, err
	}
	if cfg.Model != "" && len(profile.ModelArgs) == 0 {
		return nil, fmt.Errorf(
			"cli-agent %q: the %q CLI takes no model flag, so model %q would be ignored — "+
				"remove it, or declare cli.overrides.model_args",
			cfg.Key, cfg.Agent, cfg.Model)
	}
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("cli-agent %q: state_dir is empty — it is derived per "+
			"provider key at config load, so an empty one means the derivation was skipped", cfg.Key)
	}
	ws, err := Shared(cfg.StateDir, cfg.Agent, profile)
	if err != nil {
		return nil, err
	}
	slots := cfg.MaxConcurrent
	if slots <= 0 {
		slots = 1
	}
	p := &Provider{
		key:     cfg.Key,
		model:   cfg.Model,
		agent:   cfg.Agent,
		profile: profile,
		ws:      ws,
		env:     cfg.Env,
		auth:    cfg.Auth,
		timeout: cfg.Timeout,
		slots:   make(chan struct{}, slots),
	}
	if p.timeout <= 0 {
		return nil, fmt.Errorf("cli-agent %q: timeout is zero — a CLI call with no "+
			"wall-clock cap holds a seat forever", cfg.Key)
	}
	if cfg.CredentialBundle != "" {
		// Restored, not refused, when it fails: a node with a broken
		// bundle can still be logged in by hand, and taking the company
		// down at boot over one blob is the worse outcome. RestoreBundle
		// itself declines to overwrite a login already on disk — the
		// running node has the fresher refresh token.
		if err := p.RestoreBundle(cfg.CredentialBundle); err != nil {
			log.Warn("cli_agent_bundle_restore_failed", "provider", cfg.Key, "error", err)
		}
	}
	return p, nil
}

// Model is the configured model identity.
func (p *Provider) Model() string { return p.model }

// Profile is the resolved profile, for `crewlet llm doctor`.
func (p *Provider) Profile() Profile { return p.profile }

// Workspace is the state directory this provider shares.
func (p *Provider) Workspace() *Workspace { return p.ws }

// Agent is which CLI this provider drives.
func (p *Provider) Agent() string { return p.agent }

// String identifies the provider in a log line.
func (p *Provider) String() string {
	return fmt.Sprintf("%s/%s/%s", providerName, p.agent, p.model)
}

// Complete runs one CLI invocation and reads its answer.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	seat := llm.SeatOf(ctx)
	callID := CallOf(ctx)

	prompt, err := RenderPrompt(req)
	if err != nil {
		return nil, p.fail(llm.KindFatal, 0, err)
	}

	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		// Queued behind other seats and the caller gave up. Reported as
		// a timeout rather than fatal: nothing about the request was
		// wrong, and the chain should be free to try another member.
		return nil, p.fail(llm.KindTimeout, 0, fmt.Errorf("waiting for a CLI slot: %w", ctx.Err()))
	}

	checkout, err := p.ws.Acquire(seat, callID)
	if err != nil {
		return nil, p.fail(llm.KindFatal, 0, err)
	}
	defer func() {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := checkout.Release(); err != nil {
			// Logged, not returned: the completion already happened
			// and failing it over a cleanup would throw away work the
			// operator paid for. The next call into this seat prunes
			// again, so the state does not compound.
			log.WarnContext(ctx, "cli_agent_release_failed", "provider", p.key, "seat", seat, "error", err)
		}
	}()

	in := invocation{
		binary:  p.profile.Binary,
		args:    p.argv(),
		dir:     checkout.Work,
		env:     buildEnv(p.profile, checkout, p.env, p.auth),
		timeout: p.timeout,
	}
	// Streamed only for a JSONL profile, where each event's text IS a
	// fragment of the answer. The other two output modes are extracted
	// from the WHOLE of stdout — a fenced block, one JSON document — so
	// there is no faithful incremental view of them, and forwarding raw
	// lines would stream a banner and a half-written fence as though they
	// were what the model said.
	if req.Streaming() && p.profile.output() == OutputJSONL {
		in.onLine = func(line string) {
			doc, ok := decodeObject(strings.TrimSpace(line))
			if !ok {
				return
			}
			req.Send(llm.Delta{Content: firstString(doc, p.profile.TextPaths)})
		}
	}
	if p.profile.mode() == PromptArgv {
		in.args = append(in.args, prompt)
	} else {
		in.stdin = prompt
	}

	started := time.Now()
	res, err := run(ctx, in)
	if err != nil {
		return nil, p.fail(llm.KindFatal, 0, err)
	}
	log.DebugContext(ctx, "cli_agent_call", "provider", p.key, "agent", p.agent, "model", p.model,
		"seat", seat, "exit", res.exitCode, "elapsed_ms", time.Since(started).Milliseconds())

	return p.completion(prompt, res)
}

// argv is the invocation's arguments: the profile's completion argv, then the
// model, then (in argv mode) the prompt, which the caller appends.
func (p *Provider) argv() []string {
	args := append([]string(nil), p.profile.CompleteArgs...)
	if p.model == "" {
		return args
	}
	for _, arg := range p.profile.ModelArgs {
		args = append(args, strings.ReplaceAll(arg, "{model}", p.model))
	}
	return args
}

// completion turns a finished invocation into an answer or a classified
// failure.
func (p *Provider) completion(prompt string, res *rawResult) (*llm.Completion, error) {
	if res.timedOut {
		return nil, p.fail(llm.KindTimeout, 0, fmt.Errorf(
			"the CLI did not answer within %s — raise cli.timeout_seconds if this model "+
				"legitimately reasons for longer:\n%s", p.timeout, tail(res.stderr)))
	}

	// A CLIPPED ANSWER IS NOT AN ANSWER. stdout past maxOutput is dropped by
	// the capped buffer, and everything below would then parse whatever
	// survived and hand it back as a completion — a half-written envelope, a
	// JSON body missing its close, a report cut mid-sentence — with nothing
	// downstream able to tell it from a model that simply stopped there.
	// Refused as a SERVER failure so the fallback chain may try another
	// member and the credential is not benched: nothing about the prompt was
	// rejected.
	if res.droppedStdout > 0 {
		return nil, p.fail(llm.KindServer, 0, fmt.Errorf(
			"the CLI wrote more than %d bytes to stdout and %d were dropped, so "+
				"the answer is incomplete — raise cli.max_output_bytes for this "+
				"provider, or point it at a model that streams less",
			maxOutput, res.droppedStdout))
	}

	out := extract(p.profile, res.stdout)

	// A spent subscription is checked BEFORE the exit code, because it
	// arrives on a successful one: the process exits 0 and the answer is
	// the vendor's own sentence about the plan.
	if kind, retry, ok := p.classifyMarkers(out.text, res.stderr); ok {
		return nil, p.fail(kind, retry, fmt.Errorf("%s", firstLine(out.text, res.stderr)))
	}

	if res.exitCode != 0 || out.failed {
		return nil, p.fail(llm.KindFatal, 0, fmt.Errorf(
			"the CLI exited %d:\n%s", res.exitCode, tail(nonEmpty(res.stderr, out.text))))
	}
	if strings.TrimSpace(out.text) == "" {
		// Exit zero and nothing on stdout. Not a fatal request problem —
		// nothing about the prompt was refused — so the chain is free to
		// try another member, and the credential is not cooled.
		return nil, p.fail(llm.KindServer, 0, fmt.Errorf(
			"the CLI exited 0 but produced no output:\n%s", tail(res.stderr)))
	}

	env := ParseEnvelope(out.text)
	comp := &llm.Completion{
		Model:        p.model,
		Content:      env.Message,
		FinishReason: "stop",
	}
	if !env.Parsed {
		comp.Content = out.text
	}
	for i, call := range env.ToolCalls {
		comp.ToolCalls = append(comp.ToolCalls, llm.ToolCall{
			// The CLI has no id of its own to give, and the tool loop
			// pairs results to calls by it. Derived from the position
			// so two calls in one answer stay distinguishable.
			ID:        fmt.Sprintf("cli_%d", i),
			Name:      call.Name,
			Arguments: call.Arguments,
		})
	}
	if len(comp.ToolCalls) > 0 {
		comp.FinishReason = "tool_calls"
	}

	if out.reported {
		// InputTokens is ALWAYS the full prompt count, cache included —
		// the contract's rule, and what keeps it a correct budget figure
		// whatever the cache did. CacheRead and CacheWrite break the same
		// total down for cost reporting; adding them again double-counts.
		comp.InputTokens = out.input + out.cacheRead + out.cacheWrite
		comp.OutputTokens = out.output
		comp.CacheRead = out.cacheRead
		comp.CacheWrite = out.cacheWrite
	} else {
		comp.InputTokens = EstimateTokens(prompt)
		comp.OutputTokens = EstimateTokens(out.text)
	}
	return comp, nil
}

// classifyMarkers recognises a spent subscription or an expired login in a
// CLI's own words.
//
// Structure, not keywords. A marker is a sentinel the vendor emits VERBATIM,
// and where the vendor carries the reset instant alongside it, the
// classification yields a real Retry-After rather than a guess. Matching
// "usage limit" case-insensitively across a reply is what made this wrong
// before: a model asked about rate limits writes the phrase itself, and every
// such answer was thrown away as a spent plan.
func (p *Provider) classifyMarkers(text, stderr string) (llm.ErrorKind, time.Duration, bool) {
	haystacks := []string{text, stderr}
	for _, marker := range p.profile.LimitMarkers {
		for _, hay := range haystacks {
			idx := strings.Index(hay, marker.Sentinel)
			if idx < 0 {
				continue
			}
			return llm.KindRateLimit, resetAfter(hay[idx:], marker), true
		}
	}
	for _, marker := range p.profile.AuthMarkers {
		for _, hay := range haystacks {
			if strings.Contains(hay, marker.Sentinel) {
				return llm.KindAuth, 0, true
			}
		}
	}
	return llm.KindFatal, 0, false
}

// resetAfter reads the reset instant a limit marker carries.
//
// Zero when the vendor gave none, which the credential pool reads as "use the
// configured cooldown" — the honest answer, rather than inventing a window.
func resetAfter(from string, marker LimitMarker) time.Duration {
	if marker.ResetSeparator == "" {
		return 0
	}
	_, rest, found := strings.Cut(from, marker.ResetSeparator)
	if !found {
		return 0
	}
	// The value ends at the first character that cannot be part of it, so
	// a sentinel embedded in a longer sentence still yields its number.
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	value, err := time.ParseDuration(rest[:end] + "s")
	if err != nil {
		return 0
	}
	if marker.ResetUnit == "epoch" {
		until := time.Until(time.Unix(int64(value.Seconds()), 0))
		if until <= 0 {
			// A reset already in the past means the window has turned
			// over; the caller should try again rather than wait.
			return 0
		}
		return until
	}
	return value
}

// fail wraps an error as a classified provider failure.
func (p *Provider) fail(kind llm.ErrorKind, retryAfter time.Duration, err error) error {
	return &llm.Error{
		Kind: kind, Provider: providerName + "/" + p.agent, Model: p.model,
		RetryAfter: retryAfter, Err: err,
	}
}

// firstLine is the first non-empty line across the given texts, for a failure
// message that names the vendor's own sentence rather than its whole reply.
func firstLine(texts ...string) string {
	for _, text := range texts {
		for line := range strings.SplitSeq(text, "\n") {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				return trimmed
			}
		}
	}
	return "no output"
}

// nonEmpty is the first of the given strings with content.
func nonEmpty(candidates ...string) string {
	for _, c := range candidates {
		if strings.TrimSpace(c) != "" {
			return c
		}
	}
	return ""
}
