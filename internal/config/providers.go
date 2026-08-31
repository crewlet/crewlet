package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Providers is the Tier B model surface: which LLMs a seat can be pointed
// at, what embeds its memories, and where its code work runs.
//
// The Tier A infrastructure — store, stream, coordination — is deliberately
// not here: it cannot be changed without a restart, and mixing the two
// tiers in one block is how an operator comes to believe a DSN is
// live-editable.
type Providers struct {
	// LLM is the named provider chain seats select from by key. A seat
	// naming a key that is not here has no model at all, so the keys are
	// the company's model vocabulary.
	LLM map[string]LLMProvider `yaml:"llm,omitempty" json:"llm,omitempty" desc:"Named LLM providers; seats select one by key."`

	// LLMOrder is the order the providers were DECLARED in, preserved
	// because per-phase resolution's last resort is "the first provider
	// configured" — see agent/phase.Registry.
	//
	// It is a field and not a derivation because a Go map has no order and
	// this document does not stay YAML: a company config is stored as JSON
	// and re-read on every boot and every rollout, so an order recovered
	// only at YAML-parse time is an order the running engine never sees.
	// Two nodes booted from one stored revision would then resolve an
	// unpinned seat to different models, and one node would change model
	// across a restart.
	//
	// Normally DERIVED from the document's own key order and not written
	// by hand — but it is a readable field, because the config read
	// surface emits it and must accept it back. Go marshals a map with
	// sorted keys, so a document that made this round trip carries the
	// authored order here and nowhere else; a reader that refused it would
	// make GET-edit-PUT reject its own output.
	//
	// An explicit value WINS over the document's key order, which is what
	// makes that round trip lossless and lets an operator pin the order
	// deliberately. A document carrying neither — hand-written JSON, or a
	// revision stored before this existed — falls back to sorted keys,
	// arbitrary but stable; see ProviderOrder.
	LLMOrder []string `yaml:"llm_order,omitempty" json:"llm_order,omitempty" desc:"Provider precedence; normally derived from the order they are written in."`

	// Embeddings powers the learning subsystem's vector recall. Nil
	// disables it — episodes are still written, but nothing searches them
	// by similarity.
	Embeddings *EmbeddingProvider `yaml:"embeddings,omitempty" json:"embeddings,omitempty" desc:"Embedding provider for diary and episode recall."`

	// Sandbox is the code-runtime backend. Nil (or type none) means no
	// seat can run a sandboxed Execute phase, whatever its own gate says.
	Sandbox *SandboxProvider `yaml:"sandbox,omitempty" json:"sandbox,omitempty" desc:"Code sandbox backend. Absent = no seat runs code."`
}

// DefaultProviders is the empty provider surface: no models, no
// embeddings, no sandbox. A company with no LLM provider parses — that is
// what lets an org chart be authored before the credentials exist — and
// fails at the first turn, which is where the failure is actionable.
func DefaultProviders() Providers { return Providers{} }

// llmKeyOrder reads the declaration order of providers.llm off the document
// node, which is the only place it exists.
//
// Called from ParseCompany against the WHOLE document rather than from an
// UnmarshalYAML on Providers, and that is not a style choice. The package's
// strict decoder round-trips a node through the encoder to get KnownFields,
// which serialises that subtree ALONE — so an alias inside it pointing at an
// anchor defined elsewhere in the file stops resolving. Measured: adding a
// custom decoder to Providers turned `<<: [*one, *two]` into
// "unknown anchor 'one' referenced" on a document that had parsed fine.
// Reading the order from the document leaves every decoder exactly as it was.
func llmKeyOrder(doc *yaml.Node) []string {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	providers := mappingValue(root, "providers")
	if providers == nil {
		return nil
	}
	return mappingKeys(mappingValue(providers, "llm"))
}

// mappingValue returns a mapping's value for key.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// mappingKeys lists a mapping's keys in document order, expanding merge keys
// in place.
//
// The merge expansion is not decoration. `<<: *shared` is how an operator
// shares a block of providers between documents, and yaml.v3 resolves it during
// DECODE — so the map ends up with the merged entries while a naive read of the
// raw node sees only the literal `<<`. Those providers then fall off the order
// entirely and are appended by the sorted-tail fallback, which silently moves
// whichever one an operator listed first. Measured before this existed:
// `<<: &base {zulu: …}` plus a literal `alpha` ordered [alpha zulu].
//
// Explicit keys still win over merged ones, which is YAML's own rule; here that
// falls out of the dedupe in ProviderOrder rather than needing its own pass.
//
// NO ALIAS FOLLOWING, and that is a probed conclusion rather than an omission.
// An alias node reached here can only ever name a mapping whose anchor was
// defined at a point that ALREADY contributed its keys — YAML requires the
// definition before the use, and every top-level key in this document is known
// and typed, so an anchor-holder key is rejected as an unknown setting and
// there is nowhere else a provider-map anchor can live. Following the alias
// therefore cannot change the answer; it was written, mutated away, and no test
// could be made to see the difference.
//
// If a future top-level field is ever shaped like a provider map, this becomes
// reachable and its absence silently drops that block from the order. That is
// the one thing to check here.
func mappingKeys(node *yaml.Node) []string {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]string, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Tag != "!!merge" && key.Value != "<<" {
			out = append(out, key.Value)
			continue
		}
		// A merge value is one mapping (usually via an alias) or a
		// sequence of them, and a sequence merges left-to-right.
		if value.Kind == yaml.SequenceNode {
			for _, item := range value.Content {
				out = append(out, mappingKeys(item)...)
			}
			continue
		}
		out = append(out, mappingKeys(value)...)
	}
	return out
}

// ProviderOrder is the order seats resolve providers in.
//
// It reconciles LLMOrder against the map rather than trusting it, because the
// two travel separately through JSON and an operator editing a stored revision
// can desync them. Keys the order names but the map lacks are dropped; keys the
// map has but the order omits are appended in sorted order, so every configured
// provider is reachable and the result is always a permutation of the map's
// keys.
//
// The sorted tail is what makes a document with NO order deterministic. It is
// arbitrary — an operator's first-listed provider is the one they think of as
// primary, and sorting does not know that — but arbitrary-and-stable is the
// only honest answer when the declaration order was never recorded, and it is
// strictly better than a map range that answers differently every call.
func (p *Providers) ProviderOrder() []string {
	out := make([]string, 0, len(p.LLM))
	seen := make(map[string]bool, len(p.LLM))
	for _, key := range p.LLMOrder {
		if _, ok := p.LLM[key]; !ok || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	if len(out) == len(p.LLM) {
		return out
	}
	rest := make([]string, 0, len(p.LLM)-len(out))
	for key := range p.LLM {
		if !seen[key] {
			rest = append(rest, key)
		}
	}
	slices.Sort(rest)
	return append(out, rest...)
}

func (p *Providers) validate(path string) error {
	var probs problems
	llmPath := at(path, "llm")
	for key, provider := range p.LLM {
		if strings.TrimSpace(key) == "" {
			probs.add(llmPath, ErrMissing,
				"a provider key must not be empty — it is the name seats "+
					"select this provider by")
			continue
		}
		probs.wrap(provider.validate(at(llmPath, key)))
	}
	probs.wrap(p.validateSharedStateDirs(llmPath))
	if p.Embeddings != nil {
		probs.wrap(p.Embeddings.validate(at(path, "embeddings")))
	}
	if p.Sandbox != nil {
		probs.wrap(p.Sandbox.validate(at(path, "sandbox")))
	}
	return probs.err()
}

// validateSharedStateDirs holds the rule that two entries sharing one
// cli.state_dir must drive the SAME CLI.
//
// Sharing a directory is the supported way to run several models off a
// single login — an "opus" entry for Plan and a "sonnet" entry for Execute
// pointed at one directory means one `crewlet llm login` instead of two.
// That works only because both entries then agree on which files are
// credentials and which are conversation memory. Two different CLIs over
// one directory agree on neither, so each prunes the other's state and
// seeds credentials the other cannot read.
//
// Caught here so it fails `crewlet validate` rather than at the first turn
// of whichever seat lost the race.
func (p *Providers) validateSharedStateDirs(path string) error {
	type claim struct {
		key   string
		agent CLIAgentName
	}
	byDir := map[string]claim{}
	var probs problems
	// Sorted so the error names the same pair whichever way a map
	// iteration happens to run; an error that changes between runs is one
	// nobody can write a test against.
	for _, key := range sortedKeys(p.LLM) {
		cfg := p.LLM[key]
		if cfg.Type != LLMCLIAgent || cfg.CLI == nil || cfg.CLI.StateDir == "" {
			continue
		}
		// Cleaned before comparison: "/var/lib/x" and "/var/lib/x/" are
		// one directory, and comparing the raw strings let that pair
		// through validation to fail at the first turn instead.
		dir := filepath.Clean(cfg.CLI.StateDir)
		prev, seen := byDir[dir]
		if seen && prev.agent != cfg.CLI.Agent {
			probs.add(at(at(path, key), "cli.state_dir"), ErrConflict,
				"shares %q with providers.llm.%s but drives a different CLI "+
					"(%q vs %q). One state directory is one login and one set "+
					"of per-seat homes, which only works for the same CLI — "+
					"give them separate state_dirs",
				cfg.CLI.StateDir, prev.key, cfg.CLI.Agent, prev.agent)
			continue
		}
		if !seen {
			byDir[dir] = claim{key: key, agent: cfg.CLI.Agent}
		}
	}
	return probs.err()
}

// ---- LLM providers --------------------------------------------------- //

// LLMProviderType is which built-in implementation to construct.
type LLMProviderType string

const (
	// LLMOpenAI is the official OpenAI SDK.
	LLMOpenAI LLMProviderType = "openai"
	// LLMAnthropic is the official Anthropic SDK.
	LLMAnthropic LLMProviderType = "anthropic"
	// LLMOpenAICompatible is any endpoint speaking the OpenAI wire format
	// — an aggregator, a gateway, a local vLLM.
	LLMOpenAICompatible LLMProviderType = "openai-compatible"
	// LLMCLIAgent drives a locally installed coding CLI on the operator's
	// SUBSCRIPTION rather than a metered API key. No HTTP client is
	// involved at all; see docs/concepts/subscription-llm-backends.md.
	LLMCLIAgent LLMProviderType = "cli-agent"
)

// LLMProviderTypes is the closed set.
//
// Closed rather than a free string because the engine DISPATCHES on it: an
// unrecognised value fell through every branch and silently produced no
// provider at all, leaving every seat that named the key without a model
// and no error anywhere.
var LLMProviderTypes = []LLMProviderType{
	LLMOpenAI, LLMAnthropic, LLMOpenAICompatible, LLMCLIAgent,
}

// ReasoningEffort is the OpenAI-side reasoning budget selector.
type ReasoningEffort string

// The reasoning-effort levels.
const (
	EffortLow    ReasoningEffort = "low"
	EffortMedium ReasoningEffort = "medium"
	EffortHigh   ReasoningEffort = "high"
	EffortMax    ReasoningEffort = "max"
)

// ReasoningEfforts is the closed set.
var ReasoningEfforts = []ReasoningEffort{EffortLow, EffortMedium, EffortHigh, EffortMax}

// LLMProvider is one named entry under providers.llm.
type LLMProvider struct {
	Type  LLMProviderType `yaml:"type,omitempty" json:"type,omitempty" js:"enum=openai|anthropic|openai-compatible|cli-agent" desc:"Which implementation to construct."`
	Model string          `yaml:"model,omitempty" json:"model,omitempty" desc:"Model id this provider serves."`

	// APIKeys is always a list: one entry for the ordinary case, several
	// for rotation. With more than one the provider rotates on rate-limit
	// and auth errors and only gives up when every key is cooling down —
	// at which point the seat's own fallback chain tries the next
	// provider.
	//
	// An empty list falls back to the provider's conventional variable
	// (OPENAI_API_KEY, ANTHROPIC_API_KEY) at construction time, so a
	// credential already exported in the shell works with no YAML change.
	APIKeys []string `secret:"true" yaml:"api_keys,omitempty" json:"api_keys,omitempty" desc:"API keys, ${VAR} supported. Several rotate on rate-limit/auth errors."`

	// Cooldowns is the TTL policy applied per error class when a key is
	// marked cooled-down.
	Cooldowns CredentialCooldowns `yaml:"cooldowns,omitempty" json:"cooldowns"`

	// BaseURL redirects any HTTP backend, not just openai-compatible: an
	// Anthropic-API gateway is an `anthropic` entry with one of these, and
	// the code sandbox forwards it to Claude Code as ANTHROPIC_BASE_URL.
	// It is only REQUIRED for openai-compatible, which has no vendor
	// default to fall back to.
	BaseURL string `yaml:"base_url,omitempty" json:"base_url,omitempty" desc:"Endpoint override, ${VAR} supported. Required for openai-compatible; optional for openai and anthropic (a gateway or proxy)."`

	// Reasoning turns on extended thinking.
	Reasoning bool `yaml:"reasoning,omitempty" json:"reasoning,omitempty" desc:"Enable extended thinking (openai and anthropic only)."`
	// ReasoningEffort is the OpenAI-side budget selector.
	ReasoningEffort ReasoningEffort `yaml:"reasoning_effort,omitempty" json:"reasoning_effort,omitempty" js:"enum=low|medium|high|max" desc:"OpenAI reasoning effort when reasoning is on."`
	// ReasoningBudgetTokens is the Anthropic-side thinking budget.
	ReasoningBudgetTokens int `yaml:"reasoning_budget_tokens,omitempty" json:"reasoning_budget_tokens,omitempty" js:"min=0" desc:"Anthropic thinking budget in tokens when reasoning is on."`

	// TimeoutSeconds is the HTTP client timeout for one call. Raise it for
	// slow or large-output models that otherwise time out mid-generation;
	// lower it to fail fast. The cli-agent backend drives a subprocess
	// rather than an HTTP client and has its own timeout.
	TimeoutSeconds float64 `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty" js:"min=0" desc:"Per-request HTTP timeout in seconds."`

	// CLI is the cli-agent block: which coding CLI to drive, where its
	// per-seat state lives, how it authenticates. Required when Type is
	// cli-agent and refused otherwise.
	CLI *CLIAgent `yaml:"cli,omitempty" json:"cli,omitempty" desc:"The cli-agent block. Required for type cli-agent, refused otherwise."`
}

// defaultLLMTimeoutSeconds is the HTTP client timeout an entry that names
// none gets. Two minutes covers an ordinary completion with headroom; a
// reasoning model or a very large output wants more, which is what the
// field is for.
const defaultLLMTimeoutSeconds = 120.0

// Timeout is the per-request timeout, applying the default.
func (l *LLMProvider) Timeout() float64 {
	if l.TimeoutSeconds <= 0 {
		return defaultLLMTimeoutSeconds
	}
	return l.TimeoutSeconds
}

// ResolvedKeys is the configured keys, de-duplicated and with empties
// dropped, in declaration order. References are expanded through r, which
// is the only place a key value ever exists in this process.
func (l *LLMProvider) ResolvedKeys(r *Resolver) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, raw := range l.APIKeys {
		value := strings.TrimSpace(r.Value(raw))
		if value == "" {
			continue
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (l *LLMProvider) validate(path string) error {
	var p problems
	if l.Type != "" && !oneOf(l.Type, LLMProviderTypes) {
		p.add(at(path, "type"), ErrUnknownValue, "%q (want %s)", l.Type, names(LLMProviderTypes))
		return p.err() // every rule below keys on the type
	}
	kind := l.Type
	if kind == "" {
		kind = LLMOpenAI
	}

	if strings.TrimSpace(l.Model) == "" {
		p.add(at(path, "model"), ErrMissing,
			"name the model this provider serves")
	}

	if l.Reasoning {
		switch kind {
		case LLMOpenAICompatible:
			p.add(at(path, "reasoning"), ErrConflict,
				"reasoning is not supported for openai-compatible providers; "+
					"use type openai or anthropic")
		case LLMCLIAgent:
			p.add(at(path, "reasoning"), ErrConflict,
				"reasoning is not a Crewlet setting for cli-agent providers: "+
					"the CLI's plan carries its own reasoning configuration and "+
					"exposes no per-call switch. Drop reasoning, or pick the "+
					"CLI's reasoning model with `model`")
		}
	}
	if l.ReasoningEffort != "" && !oneOf(l.ReasoningEffort, ReasoningEfforts) {
		p.add(at(path, "reasoning_effort"), ErrUnknownValue, "%q (want %s)",
			l.ReasoningEffort, names(ReasoningEfforts))
	}
	if l.ReasoningBudgetTokens < 0 {
		p.add(at(path, "reasoning_budget_tokens"), ErrOutOfRange,
			"must not be negative, got %d", l.ReasoningBudgetTokens)
	}

	// Left unbounded, a timeout of 0 or a negative one passes validation
	// and reaches an HTTP client that reads it as
	// "expire immediately" — every call failing instantly, with nothing in
	// the config looking wrong.
	if l.TimeoutSeconds < 0 {
		p.add(at(path, "timeout_seconds"), ErrOutOfRange,
			"must be 0 (the %v s default) or positive, got %v",
			defaultLLMTimeoutSeconds, l.TimeoutSeconds)
	}
	p.wrap(l.Cooldowns.validate(at(path, "cooldowns")))

	// Both directions matter. A cli-agent entry with no block would
	// silently construct the default Claude Code profile, which may be the
	// wrong CLI entirely; a cli block on an HTTP provider is read by
	// nobody, which is the classic "I configured it and nothing happened".
	switch {
	case kind == LLMCLIAgent && l.CLI == nil:
		p.add(at(path, "cli"), ErrMissing,
			"type cli-agent needs a `cli:` block naming the CLI to drive, "+
				"e.g. `cli: {agent: claude-code}`")
	case kind != LLMCLIAgent && l.CLI != nil:
		p.add(at(path, "cli"), ErrConflict,
			"`cli:` only applies to type cli-agent (this entry is type %q). "+
				"Remove the block, or change the type", kind)
	case l.CLI != nil:
		p.wrap(l.CLI.validate(at(path, "cli")))
	}

	if kind == LLMOpenAICompatible && strings.TrimSpace(l.BaseURL) == "" {
		// An openai-compatible provider with no endpoint is an OpenAI
		// provider wearing a different label: it silently talks to
		// api.openai.com with whatever key it was given.
		p.add(at(path, "base_url"), ErrMissing,
			"an openai-compatible provider needs the endpoint to talk to")
	}
	return p.err()
}

// CredentialCooldowns is the TTL policy for an LLM provider's key pool.
//
// A key is marked cooled-down by ERROR CLASS, because the two classes clear
// on different clocks: a 429 typically clears in an hour, while a 401 or
// 403 often clears in minutes once a token refreshes.
type CredentialCooldowns struct {
	RateLimitSeconds int `yaml:"rate_limit_seconds,omitempty" json:"rate_limit_seconds,omitempty" js:"min=0;max=86400" desc:"Cooldown after a rate-limit error (60..86400s)."`
	AuthSeconds      int `yaml:"auth_seconds,omitempty" json:"auth_seconds,omitempty" js:"min=0;max=86400" desc:"Cooldown after an auth error (60..86400s)."`
}

// The cooldown bounds and defaults.
const (
	defaultRateLimitCooldown = 3600
	defaultAuthCooldown      = 300
	minCooldownSeconds       = 60
	maxCooldownSeconds       = 86400
)

// RateLimit is the rate-limit cooldown, applying the default.
func (c CredentialCooldowns) RateLimit() int {
	if c.RateLimitSeconds == 0 {
		return defaultRateLimitCooldown
	}
	return c.RateLimitSeconds
}

// Auth is the auth-error cooldown, applying the default.
func (c CredentialCooldowns) Auth() int {
	if c.AuthSeconds == 0 {
		return defaultAuthCooldown
	}
	return c.AuthSeconds
}

func (c CredentialCooldowns) validate(path string) error {
	var p problems
	for _, f := range []struct {
		name  string
		value int
	}{{"rate_limit_seconds", c.RateLimitSeconds}, {"auth_seconds", c.AuthSeconds}} {
		if f.value == 0 {
			continue // the default
		}
		if f.value < minCooldownSeconds || f.value > maxCooldownSeconds {
			p.add(at(path, f.name), ErrOutOfRange,
				"must be %d..%d seconds, got %d — below a minute a cooling key "+
					"is retried before the limit clears, and above a day it is "+
					"effectively retired",
				minCooldownSeconds, maxCooldownSeconds, f.value)
		}
	}
	return p.err()
}

// ---- the cli-agent backend ------------------------------------------- //

// CLIAgentName is which coding CLI a cli-agent provider drives.
//
// A NAMED TYPE over a closed set, like every other closed set in this package
// and unlike the []string this replaces. Two things followed from the bare
// string. The generated schema emitted `"type":"string"` while its sibling
// RoleSandbox.CodingAgent emitted an enum, so an editor blessed
// `agent: claud-code` — which oneOf's own doc calls worse than no schema —
// and with no `js:` tag the field could not join schema_test.go's enum table,
// so nothing noticed.
type CLIAgentName string

// The coding CLIs the engine knows how to drive. `custom` ships no defaults
// at all — supply everything under Overrides.
const (
	CLIClaudeCode  CLIAgentName = "claude-code"
	CLICodex       CLIAgentName = "codex"
	CLIGeminiCLI   CLIAgentName = "gemini-cli"
	CLIQwenCode    CLIAgentName = "qwen-code"
	CLIOpenCode    CLIAgentName = "opencode"
	CLICursorAgent CLIAgentName = "cursor-agent"
	CLICopilot     CLIAgentName = "copilot"
	CLIGrok        CLIAgentName = "grok"
	CLICustom      CLIAgentName = "custom"
)

// CLIAgentNames is the closed set.
var CLIAgentNames = []CLIAgentName{
	CLIClaudeCode, CLICodex, CLIGeminiCLI, CLIQwenCode,
	CLIOpenCode, CLICursorAgent, CLICopilot, CLIGrok, CLICustom,
}

// Valid reports whether n is one the engine knows.
//
// Empty is NOT valid here — unlike an optional enum elsewhere — because the
// caller checks emptiness first: an absent agent takes the provider's own
// default rather than meaning "any".
func (n CLIAgentName) Valid() bool { return oneOf(n, CLIAgentNames) }

// CLIAgentAuthMode is how a cli-agent provider authenticates.
type CLIAgentAuthMode string

const (
	// AuthSubscription uses the operator's own CLI login — a token in the
	// secret store, or the credential files `crewlet llm login` wrote.
	// The default, and the point of this backend.
	AuthSubscription CLIAgentAuthMode = "subscription"
	// AuthAPIKey puts the first configured api_key into the CLI's key
	// variable, billing the metered account.
	AuthAPIKey CLIAgentAuthMode = "api-key"
	// AuthInheritEnv forwards whichever of the two variables the engine's
	// own environment has — an escape hatch for an unusual CLI, and the
	// only mode that lets a host credential reach the child.
	AuthInheritEnv CLIAgentAuthMode = "inherit-env"
)

// CLIAgentAuthModes is the closed set.
//
// The default is deliberately NOT inherit-env: a subscription backend that
// silently picked up a stray ANTHROPIC_API_KEY would bill the metered
// account while the operator believed they were on a flat-rate plan.
var CLIAgentAuthModes = []CLIAgentAuthMode{AuthSubscription, AuthAPIKey, AuthInheritEnv}

// CLIAgentAuth is how a cli-agent provider authenticates. The login itself
// is performed by `crewlet llm login`, never by the engine.
type CLIAgentAuth struct {
	Mode CLIAgentAuthMode `yaml:"mode,omitempty" json:"mode,omitempty" js:"enum=subscription|api-key|inherit-env" desc:"subscription (default), api-key, or inherit-env."`

	// Token is a ${VAR} reference to a long-lived subscription token.
	// Empty falls back to the profile's own token variable. This is the
	// best headless path where a CLI offers one: no credential files to
	// sync, no refresh rotation, and it survives an ephemeral container.
	Token string `secret:"true" yaml:"token,omitempty" json:"token,omitempty" desc:"${VAR} holding a long-lived subscription token."`

	// CredentialBundle is a ${VAR} reference to a bundle exported by
	// `crewlet llm export` — the CLI's credential files as one blob,
	// restored into an empty credential directory at boot so a fresh
	// container comes up already logged in.
	CredentialBundle string `secret:"true" yaml:"credential_bundle,omitempty" json:"credential_bundle,omitempty" desc:"${VAR} holding a bundle exported by crewlet llm export."`
}

// CLIAgent is the cli-agent block of a providers.llm entry.
type CLIAgent struct {
	// Agent is which CLI to drive.
	Agent CLIAgentName `yaml:"agent,omitempty" json:"agent,omitempty" js:"enum=claude-code|codex|gemini-cli|qwen-code|opencode|cursor-agent|copilot|grok|custom" desc:"Which coding CLI to drive."`

	// StateDir is where this provider keeps its credential directory and
	// its per-seat homes. Empty derives one per key, so unrelated
	// providers never collide.
	//
	// ENTRIES POINTING AT ONE DIRECTORY SHARE ONE LOGIN — and one set of
	// per-seat homes, and one generation. That is how per-phase models
	// work off a single `crewlet llm login`. Entries that should share
	// have to say so, and must drive the same CLI; see
	// [Providers.validateSharedStateDirs].
	StateDir string `yaml:"state_dir,omitempty" json:"state_dir,omitempty" desc:"Credential + per-seat home directory. Entries sharing one share a login."`

	// TimeoutSeconds caps one CLI invocation.
	//
	// Separate from the HTTP timeout because the transports are not
	// comparable: this covers a process launch (a Node runtime costs
	// seconds before the first byte), the model call, and the CLI's own
	// internal retry and backoff. 120 s — right for HTTP — cuts off
	// legitimate long reasoning turns here, so the default is 2.5x that.
	TimeoutSeconds float64 `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty" js:"min=0" desc:"Wall-clock cap on one CLI invocation."`

	// MaxConcurrent is how many CLI processes this provider runs at once.
	//
	// Each is a full Node or Rust runtime at roughly 200-400 MB resident,
	// so an unbounded fleet of seats entering Plan together can exhaust
	// the engine host; 4 keeps peak usage near 1.5 GB, which fits the
	// smallest realistic host while still overlapping the calls' long I/O
	// waits. Subscription plans also throttle concurrency well below what
	// an API key allows, so a higher number mostly buys rate-limit errors.
	MaxConcurrent int `yaml:"max_concurrent,omitempty" json:"max_concurrent,omitempty" js:"min=0" desc:"Concurrent CLI processes; each costs 200-400 MB."`

	// Env is extra environment for every invocation, ${VAR}-resolved. The
	// child gets an ALLOWLISTED environment, not the engine's, so anything
	// the CLI needs beyond PATH, locale, TLS and proxy is declared here.
	Env map[string]string `secret:"true" yaml:"env,omitempty" json:"env,omitempty" desc:"Extra environment for the CLI child process."`

	Auth CLIAgentAuth `yaml:"auth,omitempty" json:"auth"`

	// Overrides replaces fields of the built-in profile.
	//
	// CLI flags drift between releases, and a vendor renaming
	// --output-format must be a config edit rather than a Crewlet release,
	// so every profile field is replaceable. Lists replace wholesale —
	// position matters in an argv.
	Overrides map[string]any `yaml:"overrides,omitempty" json:"overrides,omitempty" desc:"Field-wise overrides of the built-in CLI profile."`
}

// Default cli-agent knobs. See the field comments for where the numbers
// come from.
const (
	defaultCLITimeoutSeconds = 300.0
	defaultCLIMaxConcurrent  = 4
	defaultCLIAgentName      = "claude-code"
)

// Name is the CLI to drive, applying the default.
//
// A STRING, not a CLIAgentName: the value crosses into internal/providers,
// which does not import config, and the profile registry it keys is that
// package's own. The type exists to bound what config ACCEPTS, which is a
// question answered before this is called.
func (c *CLIAgent) Name() string {
	if c.Agent == "" {
		return defaultCLIAgentName
	}
	return string(c.Agent)
}

// Timeout is the invocation cap, applying the default.
func (c *CLIAgent) Timeout() float64 {
	if c.TimeoutSeconds <= 0 {
		return defaultCLITimeoutSeconds
	}
	return c.TimeoutSeconds
}

// CLIHomeEnv is the environment variable naming the root under which every
// cli-agent provider keeps its credential directory and per-seat homes.
const CLIHomeEnv = "CREWLET_LLM_CLI_HOME"

// defaultCLIHome is the per-user fallback root, under the operator's home.
const defaultCLIHome = ".crewlet/llm-cli"

// ResolvedStateDir is where this entry keeps its login and its per-seat
// homes, applying the derivation when the operator named none.
//
// Derived PER PROVIDER KEY, which is what makes sharing a login an explicit
// act: two unrelated entries must not silently land on one directory and
// prune each other's state, so entries that should share have to say so with
// a matching cli.state_dir. See docs/concepts/subscription-llm-backends.md.
//
// Read from the process environment rather than the resolver because this is
// where the engine keeps FILES, not a secret — the same reason the store path
// is a Tier A field and not a ${VAR}.
func (c *CLIAgent) ResolvedStateDir(key string) (string, error) {
	if c != nil && c.StateDir != "" {
		return filepath.Clean(c.StateDir), nil
	}
	if root := os.Getenv(CLIHomeEnv); root != "" {
		return filepath.Join(filepath.Clean(root), key), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf(
			"providers.llm.%s.cli.state_dir: no directory given and no home directory to "+
				"derive one under (%w) — set cli.state_dir, or %s", key, err, CLIHomeEnv)
	}
	return filepath.Join(home, defaultCLIHome, key), nil
}

// Concurrency is the process cap, applying the default.
func (c *CLIAgent) Concurrency() int {
	if c.MaxConcurrent <= 0 {
		return defaultCLIMaxConcurrent
	}
	return c.MaxConcurrent
}

func (c *CLIAgent) validate(path string) error {
	var p problems
	if c.Agent != "" && !c.Agent.Valid() {
		p.add(at(path, "agent"), ErrUnknownValue, "%q (want %s)",
			c.Agent, names(CLIAgentNames))
	}
	if c.TimeoutSeconds < 0 {
		p.add(at(path, "timeout_seconds"), ErrOutOfRange,
			"must be 0 (the %v s default) or positive, got %v",
			defaultCLITimeoutSeconds, c.TimeoutSeconds)
	}
	if c.MaxConcurrent < 0 {
		p.add(at(path, "max_concurrent"), ErrOutOfRange,
			"must be 0 (the default of %d) or positive, got %d",
			defaultCLIMaxConcurrent, c.MaxConcurrent)
	}
	if c.Auth.Mode != "" && !oneOf(c.Auth.Mode, CLIAgentAuthModes) {
		p.add(at(path, "auth.mode"), ErrUnknownValue, "%q (want %s)",
			c.Auth.Mode, names(CLIAgentAuthModes))
	}
	return p.err()
}

// ---- embeddings ------------------------------------------------------ //

// EmbeddingProviderType is which embedding implementation to construct.
type EmbeddingProviderType string

// The embedding backends. Both are served by the OpenAI client; point
// either at any OpenAI-compatible endpoint with base_url.
const (
	EmbeddingOpenAI           EmbeddingProviderType = "openai"
	EmbeddingOpenAICompatible EmbeddingProviderType = "openai-compatible"
)

// EmbeddingProviderTypes is the closed set.
var EmbeddingProviderTypes = []EmbeddingProviderType{EmbeddingOpenAI, EmbeddingOpenAICompatible}

// EmbeddingProvider is the vector backend behind diary and episode recall.
type EmbeddingProvider struct {
	Type    EmbeddingProviderType `yaml:"type,omitempty" json:"type,omitempty" js:"enum=openai|openai-compatible" desc:"openai (default) or openai-compatible."`
	Model   string                `yaml:"model,omitempty" json:"model,omitempty" desc:"Embedding model id."`
	APIKey  string                `secret:"true" yaml:"api_key,omitempty" json:"api_key,omitempty" desc:"API key or ${VAR} reference."`
	BaseURL string                `yaml:"base_url,omitempty" json:"base_url,omitempty" desc:"Endpoint for an openai-compatible embedding service."`

	// Dimensions is the vector width. It MUST match what the model
	// produces: the store's vector columns are sized from this at open
	// time, and a mismatch is not a degraded search but a write that
	// cannot be read back.
	Dimensions int `yaml:"dimensions,omitempty" json:"dimensions,omitempty" js:"min=0" desc:"Vector width; must match the model's output size."`
}

// defaultEmbeddingDimensions matches text-embedding-3-small, which is what
// an entry that names no model gets.
const defaultEmbeddingDimensions = 1536

// Width is the configured vector width, applying the default.
func (e *EmbeddingProvider) Width() int {
	if e.Dimensions <= 0 {
		return defaultEmbeddingDimensions
	}
	return e.Dimensions
}

func (e *EmbeddingProvider) validate(path string) error {
	var p problems
	if e.Type != "" && !oneOf(e.Type, EmbeddingProviderTypes) {
		p.add(at(path, "type"), ErrUnknownValue, "%q (want %s)", e.Type, names(EmbeddingProviderTypes))
	}
	if strings.TrimSpace(e.Model) == "" {
		p.add(at(path, "model"), ErrMissing, "name the embedding model")
	}
	// Left unbounded, a negative or absurd width is not a tuning
	// mistake — the store sizes its vector columns from it, so it
	// decides whether a written embedding can ever be read back.
	if e.Dimensions < 0 {
		p.add(at(path, "dimensions"), ErrOutOfRange,
			"must be 0 (the default of %d) or positive, got %d",
			defaultEmbeddingDimensions, e.Dimensions)
	}
	if e.Type == EmbeddingOpenAICompatible && strings.TrimSpace(e.BaseURL) == "" {
		p.add(at(path, "base_url"), ErrMissing,
			"an openai-compatible embedding provider needs the endpoint to talk to")
	}
	return p.err()
}
