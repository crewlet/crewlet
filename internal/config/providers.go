package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
	"github.com/crewlet/crewlet/internal/words"
)

// Providers is the Tier B model surface: which LLMs a seat can be pointed
// at, what embeds its memories, and where its code work runs.
//
// The Tier A infrastructure — store, stream, coordination — is deliberately
// not here: it cannot be changed without a restart, and mixing the two
// tiers in one block is how an operator comes to believe the broker's
// address is live-editable.
type Providers struct {
	// LLM is the named models seats select from by key, each a coding CLI
	// the engine drives ([LLMProvider]). A seat naming a key that is not
	// here falls back to `default`, else the first provider declared — a
	// model nobody chose for it — so the keys are the company's model
	// vocabulary, and a company naming one is refused at Validate.
	LLM map[string]LLMProvider `yaml:"llm,omitempty" json:"llm,omitempty" desc:"Named models, each a coding CLI the engine drives; seats select one by key."`

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
	// deliberately. A document carrying neither — a value built in Go —
	// falls back to sorted keys, arbitrary but stable; see ProviderOrder.
	LLMOrder []string `yaml:"llm_order,omitempty" json:"llm_order,omitempty" desc:"Provider precedence; normally derived from the order they are written in."`

	// Embeddings powers every vector this engine computes: the semantic half
	// of a Doclet knowledge search and the similarity halves of diary and
	// episode recall. Nil disables them — a search is keyword only, and
	// episodes are still written, but nothing searches them by similarity.
	Embeddings *EmbeddingProvider `yaml:"embeddings,omitempty" json:"embeddings,omitempty" desc:"Embedding provider for semantic knowledge search and for diary and episode recall."`

	// Sandbox is the code-runtime backend. Nil (or type none) means no
	// seat can run a sandboxed coding run, whatever its own gate says.
	Sandbox *SandboxProvider `yaml:"sandbox,omitempty" json:"sandbox,omitempty" desc:"Code sandbox backend. Absent = no seat runs code."`
}

// DefaultProviders is the empty provider surface: no models, no
// embeddings, no sandbox. A company with no LLM provider parses, validates
// and runs, which is what lets an org chart be authored before the
// credentials exist: its seats are placed and hold their work until a
// revision adds a provider.
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

func (p *Providers) validate(path Path) error {
	var probs problems
	llmPath := at(path, "llm")
	for key, provider := range p.LLM {
		if strings.TrimSpace(key) == "" {
			probs.add(llmPath, ErrMissing,
				"a provider key must not be empty: it is the name seats "+
					"select this provider by")
			continue
		}
		probs.wrap(provider.validate(entry(llmPath, key)))
	}
	if p.Embeddings != nil {
		probs.wrap(p.Embeddings.validate(at(path, "embeddings")))
	}
	if p.Sandbox != nil {
		probs.wrap(p.Sandbox.validate(at(path, "sandbox")))
	}
	return probs.err()
}

// ---- LLM providers --------------------------------------------------- //

// LLMProvider is one named entry under providers.llm: a coding CLI — Claude
// Code, Codex, OpenCode and the rest of [CLIAgentNames] — that the engine
// drives as its model.
//
// # The CLI is the only backend
//
// There is no HTTP client to a model vendor in this engine, and that is the
// decision rather than a gap. Each vendor ships its own harness — the request
// shape each model accepts, how its thinking is replayed, what is cached and
// where, how a stream ends, which sign-in bills which account — and moves it
// with every model it releases. Re-implementing that per vendor and per model
// was a second, permanently trailing copy of work the vendors already do in
// the CLIs they maintain. So a model is reached THROUGH one: Claude Code for
// Anthropic's models, on a subscription or an API key; Codex for OpenAI's;
// OpenCode for open models on any provider it speaks to, a local server
// included.
//
// The entry is therefore the CLI's own settings and nothing else: which CLI,
// which model in that CLI's grammar, whether it runs on this host turn by
// turn or as the executor detached in a box, where its login and per-seat
// homes live, and how it signs in.
type LLMProvider struct {
	// Agent is which CLI to drive. Empty drives Claude Code.
	Agent CLIAgentName `yaml:"agent,omitempty" json:"agent,omitempty" js:"enum=claude-code|codex|gemini-cli|qwen-code|opencode|cursor-agent|copilot|grok|muse-code|kimi-code|hermes|pi|custom" desc:"Which coding CLI to drive (default claude-code)."`

	// Model is handed to the CLI's model flag, in that CLI's own grammar:
	// an alias or id for Claude Code (`sonnet`, `claude-opus-5-5`), an
	// OpenAI id for Codex, `<provider>/<model>` for OpenCode.
	Model string `yaml:"model,omitempty" json:"model,omitempty" desc:"Model id, in the CLI's own grammar (OpenCode: <provider>/<model>)."`

	// Mode is host (the default) or box. See [CLIMode].
	Mode CLIMode `yaml:"mode,omitempty" json:"mode,omitempty" js:"enum=host|box" desc:"host (the CLI runs on this engine's host, turn by turn) or box (the executor runs detached in a box of providers.sandbox)."`

	// RunIn is WHERE a box-mode run happens, naming a cell of
	// providers.sandbox. Empty takes the catalogue's default.
	//
	// On the entry rather than on the seat, because it is a property of
	// this RUNTIME: the CLI's subscription login lives on the engine host,
	// so `direct` and `container` reach it directly and a remote cell
	// needs the headless token instead. An operator who wants both makes
	// two entries and points seats at whichever one is right for them —
	// which is also how they already choose between two models.
	//
	// Read only in box mode; a value here in host mode configures
	// nothing and is refused.
	RunIn Placement `yaml:"run_in,omitempty" json:"run_in,omitempty" js:"enum=direct|container|e2b" desc:"Where a box-mode run happens. Empty = providers.sandbox.default_run_in."`

	// StateDir is where this provider keeps its credential directory and
	// its per-seat homes, under a subdirectory named for the CLI it drives
	// (<state_dir>/<agent>). Empty derives one per key, so unrelated
	// providers never collide.
	//
	// ENTRIES POINTING AT ONE DIRECTORY AND DRIVING ONE CLI SHARE ONE LOGIN
	// — and one set of per-seat homes, and one generation. That is how
	// per-phase models work off a single `crewlet llm login`, and entries
	// that should share have to say so. Entries on one directory that drive
	// DIFFERENT CLIs share nothing: each CLI keeps its own subdirectory,
	// because two CLIs agree on neither which files are credentials nor
	// which are conversation memory. That is also what lets an entry move
	// to another CLI in a live revision: the new CLI opens a login
	// directory of its own, and moving back finds the old one intact.
	StateDir string `yaml:"state_dir,omitempty" json:"state_dir,omitempty" desc:"Credential + per-seat home directory, each CLI under <state_dir>/<agent>. Entries sharing one and a CLI share a login."`

	// TimeoutSeconds caps one STRETCH of the CLI's own work, wall clock:
	// from its launch, or from the end of the tool call it last waited on,
	// to the start of its next tool call or its exit — a process launch (a
	// Node runtime costs seconds before its first byte), its model's
	// answers, and its own internal retry and backoff. The engine's tools
	// are not on the clock: they hold their own bounds, and a CLI run that
	// drives a phase through many requests and calls is never cut for its
	// length, only for a stretch in which it made no progress the engine
	// could see. See [defaultCLITimeoutSeconds] for where the default comes
	// from.
	TimeoutSeconds float64 `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty" js:"min=0" desc:"Wall-clock cap on one stretch of the CLI's own work with no tool call in flight (default 600)."`

	// MaxConcurrent is how many CLI processes this provider runs at once. A
	// process lives for a whole run of a phase, its requests and tool calls
	// included; a model call made from INSIDE one of its tool calls on this
	// same entry — a delegated worker, an auxiliary rewrite — runs in the
	// caller's slot rather than waiting for one, which a run holding the
	// last slot could never free.
	//
	// Each is a full Node or Rust runtime at roughly 200-400 MB resident,
	// so an unbounded fleet of seats starting a turn together can exhaust
	// the engine host; 4 keeps peak usage near 1.5 GB, which fits the
	// smallest realistic host while still overlapping the calls' long I/O
	// waits. Subscription plans also throttle concurrency well below what
	// an API key allows, so a higher number mostly buys rate-limit errors.
	MaxConcurrent int `yaml:"max_concurrent,omitempty" json:"max_concurrent,omitempty" js:"min=0" desc:"Concurrent CLI processes; each costs 200-400 MB."`

	// Env is extra environment for every invocation, ${VAR}-resolved. The
	// child gets an ALLOWLISTED environment, not the engine's, so anything
	// the CLI needs beyond PATH, locale, TLS and proxy is declared here —
	// for OpenCode, the key of the provider its model names.
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

// CLIAgentName is which coding CLI a providers.llm entry drives.
//
// A NAMED TYPE over a closed set, like every other closed set in this
// package. Two things followed from a bare string. The generated schema
// emitted `"type":"string"` while its sibling RoleSandbox.CodingAgent emitted
// an enum, so an editor blessed `agent: claud-code` — which [names]'s own doc
// calls worse than no schema — and with no `js:` tag the field could not join
// schema_test.go's enum table, so nothing noticed.
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
	CLIMuseCode    CLIAgentName = "muse-code"
	CLIKimiCode    CLIAgentName = "kimi-code"
	CLIHermes      CLIAgentName = "hermes"
	CLIPi          CLIAgentName = "pi"
	CLICustom      CLIAgentName = "custom"
)

// CLIAgentNames is the closed set.
var CLIAgentNames = []CLIAgentName{
	CLIClaudeCode, CLICodex, CLIGeminiCLI, CLIQwenCode,
	CLIOpenCode, CLICursorAgent, CLICopilot, CLIGrok, CLIMuseCode,
	CLIKimiCode, CLIHermes, CLIPi, CLICustom,
}

// Valid reports whether n is one the engine knows.
//
// Empty is NOT valid here — unlike an optional enum elsewhere — because the
// caller checks emptiness first: an absent agent takes the default rather
// than meaning "any".
func (n CLIAgentName) Valid() bool { return slices.Contains(CLIAgentNames, n) }

// CLIMode is WHERE a seat's executor runs its CLI.
//
// # Both run the CLI's own loop
//
// In either mode the CLI drives the model and calls the seat's tools as MCP
// tools of one server, every call going through the engine's gates. What the
// mode decides is where that loop runs and how long it lives.
//
// HOST MODE runs it on this engine's host, as a subprocess of the turn, with
// the CLI's own shell and editor denied: its tools are the seat's alone,
// served over a socket in this process's own directory. The turn drives it —
// its rounds are capped, a person's note reaches it between runs, a call that
// parks the turn ends the run — and it ends with the turn.
//
// BOX MODE runs the executor detached, in a box of providers.sandbox, with the
// CLI's own shell and editor enabled and the seat's tools reached over the
// MCP bridge (internal/api/mcpbridge). It outlives its turn, which parks until
// the run reports, and it needs a box.
//
// The mode is NOT inferred, and the reason is that both are defensible for the
// same CLI on the same seat. Host mode needs no sandbox and every step of it
// is the turn's own. Box mode is faster on real code work, where the vendor's
// own tools matter, at the cost of a run that outlives its turn and a tool
// surface reached over the network.
type CLIMode string

const (
	// CLIModeHost runs the CLI on this engine's host, turn by turn. The
	// default.
	CLIModeHost CLIMode = "host"

	// CLIModeBox runs the seat's executor as a detached run of the CLI in a
	// box, with the seat's tools bridged in over MCP.
	CLIModeBox CLIMode = "box"
)

// CLIModes is the closed set.
var CLIModes = []CLIMode{CLIModeHost, CLIModeBox}

// CLIAgentAuthMode is how a providers.llm entry's CLI authenticates.
type CLIAgentAuthMode string

const (
	// AuthSubscription uses the operator's own CLI login — a token in the
	// secret store, or the credential files `crewlet llm login` wrote.
	// The default.
	AuthSubscription CLIAgentAuthMode = "subscription"
	// AuthAPIKey puts the entry's API key into the CLI's key variable (its
	// profile's api_key_env), billing the metered account: auth.api_key,
	// or that variable itself looked up through the secret store and then
	// the environment when the entry names none. Refused on a CLI whose
	// profile names no such variable, where the key would reach nothing.
	AuthAPIKey CLIAgentAuthMode = "api-key"
	// AuthInheritEnv forwards whichever of the two variables the engine's
	// own environment has — an escape hatch for an unusual CLI, and the
	// only mode that lets a host credential reach the child.
	AuthInheritEnv CLIAgentAuthMode = "inherit-env"
)

// CLIAgentAuthModes is the closed set.
//
// The default is deliberately NOT inherit-env: a subscription entry that
// silently picked up a stray ANTHROPIC_API_KEY would bill the metered
// account while the operator believed they were on a flat-rate plan.
var CLIAgentAuthModes = []CLIAgentAuthMode{AuthSubscription, AuthAPIKey, AuthInheritEnv}

// CLIAgentAuth is how a providers.llm entry's CLI authenticates. The login
// itself is performed by `crewlet llm login`, never by the engine.
type CLIAgentAuth struct {
	Mode CLIAgentAuthMode `yaml:"mode,omitempty" json:"mode,omitempty" js:"enum=subscription|api-key|inherit-env" desc:"subscription (default), api-key, or inherit-env."`

	// Token is a ${VAR} reference to a long-lived subscription token.
	// Empty falls back to the profile's own token variable. This is the
	// best headless path where a CLI offers one: no credential files to
	// sync, no refresh rotation, and it survives an ephemeral container.
	// Read only under auth.mode subscription and only by a CLI whose
	// profile names a token_env; set anywhere else it is refused, because
	// it would reach nothing.
	Token string `secret:"true" yaml:"token,omitempty" json:"token,omitempty" desc:"${VAR} holding a long-lived subscription token."`

	// APIKey is a ${VAR} reference to a metered API key, put into the
	// CLI's key variable (its profile's api_key_env) under auth.mode
	// api-key. Empty reads that variable itself through the secret store
	// and then the environment — ANTHROPIC_API_KEY for Claude Code,
	// CODEX_API_KEY for Codex — so a key already exported, or set with
	// `crewlet secrets set`, works with no YAML change.
	//
	// ONE key, never a list: a CLI holds one sign-in and rotates nothing,
	// so a second key belongs on an entry of its own in the seat's
	// fallback chain. Set under any other mode it is refused, because it
	// would reach nothing.
	APIKey string `secret:"true" yaml:"api_key,omitempty" json:"api_key,omitempty" desc:"${VAR} holding an API key, read under auth.mode api-key. Empty reads the CLI's own key variable."`

	// CredentialBundle is a ${VAR} reference to a bundle exported by
	// `crewlet llm export` — the CLI's login (its credential files, or the
	// document its export printed) as one blob, restored into an empty
	// credential directory whenever the engine builds a company to run (boot
	// and every apply it publishes), so a fresh container comes up already
	// logged in — and withheld after `crewlet llm logout` until a login is
	// written into the credential directory again (`crewlet llm login`,
	// `-from-host` or `crewlet llm import`; a headless token does not end it).
	// It holds ONE CLI's files and names that CLI, though it is keyed by the
	// entry: after the entry's agent changes, the new CLI refuses it — by the
	// CLI it names, whatever its files are called — until a bundle exported
	// from that CLI replaces it.
	CredentialBundle string `secret:"true" yaml:"credential_bundle,omitempty" json:"credential_bundle,omitempty" desc:"${VAR} holding a bundle exported by crewlet llm export."`
}

// Default entry knobs. See the field comments for where the numbers come
// from.
const (
	// defaultCLITimeoutSeconds is ten minutes. A stretch is at least one
	// model answer at the CLI's own effort, and on the current Claude models
	// that is adaptive thinking against a 128K-token output cap: ten minutes
	// is Anthropic's documented ceiling for such an answer in one call, and
	// two minutes was measured cutting off exactly the calls doing the most
	// work when the engine still made them itself. A CLI adds its process
	// launch to the first stretch. A hung child holds its seat for no longer
	// than this, and a timeout is retryable, so the seat's fallback chain
	// moves on.
	defaultCLITimeoutSeconds = 600.0
	defaultCLIMaxConcurrent  = 4
	defaultCLIAgentName      = CLIClaudeCode
)

// AgentName is the CLI to drive, applying the default.
//
// A STRING, not a CLIAgentName: the value crosses into internal/providers,
// which does not import config, and the profile registry it keys is that
// package's own. The type exists to bound what config ACCEPTS, which is a
// question answered before this is called.
func (l *LLMProvider) AgentName() string {
	if l.Agent == "" {
		return string(defaultCLIAgentName)
	}
	return string(l.Agent)
}

// Box reports whether this entry runs the executor as a detached run in a box.
func (l *LLMProvider) Box() bool { return l.Mode == CLIModeBox }

// Timeout is the cap on one stretch of the CLI's own work, applying the
// default.
func (l *LLMProvider) Timeout() float64 {
	if l.TimeoutSeconds <= 0 {
		return defaultCLITimeoutSeconds
	}
	return l.TimeoutSeconds
}

// Concurrency is the process cap, applying the default.
func (l *LLMProvider) Concurrency() int {
	if l.MaxConcurrent <= 0 {
		return defaultCLIMaxConcurrent
	}
	return l.MaxConcurrent
}

// CLIHomeEnv is the environment variable naming the root under which every
// providers.llm entry keeps its credential directory and per-seat homes.
const CLIHomeEnv = "CREWLET_LLM_CLI_HOME"

// defaultCLIHome is the per-user fallback root, under the operator's home.
const defaultCLIHome = ".crewlet/llm-cli"

// ResolvedStateDir is the directory under which this entry keeps its login and
// its per-seat homes — in a subdirectory named for its CLI, which the backend
// adds — applying the derivation when the operator named none.
//
// Derived PER PROVIDER KEY, which is what makes sharing a login an explicit
// act: two unrelated entries must not silently land on one directory and
// prune each other's state, so entries that should share have to say so with
// a matching state_dir. See docs/concepts/llm-backends.md.
//
// Read from the process environment rather than the resolver because this is
// where the engine keeps FILES, not a secret — the same reason the store path
// is a Tier A field and not a ${VAR}.
func (l *LLMProvider) ResolvedStateDir(key string) (string, error) {
	if l.StateDir != "" {
		return filepath.Clean(l.StateDir), nil
	}
	if root := os.Getenv(CLIHomeEnv); root != "" {
		return filepath.Join(filepath.Clean(root), key), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf(
			"providers.llm.%s.state_dir: no directory given and no home directory to "+
				"derive one under (%w): set state_dir, or %s", key, err, CLIHomeEnv)
	}
	return filepath.Join(home, defaultCLIHome, key), nil
}

// ResolvedAuthMode is the entry's auth.mode, applying the default.
func (l *LLMProvider) ResolvedAuthMode() CLIAgentAuthMode {
	if l.Auth.Mode == "" {
		return AuthSubscription
	}
	return l.Auth.Mode
}

// CredentialSource is one credential a providers.llm entry signs its CLI in
// with: what the entry's own field holds, and the variable read in its place
// when the field holds nothing.
type CredentialSource struct {
	// Value is the field as written: a ${VAR}, a literal, or empty.
	Value string
	// Fallback is the variable read when Value is empty, through the secret
	// store and then the environment. Empty where the entry's auth mode does
	// not read this credential, or its CLI names no such variable.
	Fallback string
}

// CredentialSources are the three credentials an entry can sign in with.
type CredentialSources struct {
	Token, APIKey, Bundle CredentialSource
}

// CredentialSources says where each of this entry's credentials is read from:
// the field the entry writes or, where it writes none, a conventional variable
// — the CLI's own token or key variable, and the bundle's
// [cliprofile.BundleVarName].
//
// A CONVENTION, so `crewlet llm login -capture-token`, `crewlet llm export
// -secret-store` and a key already exported as ANTHROPIC_API_KEY sign an entry
// in with no YAML change. And ONE RULE with two readers: the engine resolves
// the entry's sign-in through it, and [Company.VariablesRead] reports it, so
// the Secrets screen never calls a variable "read by nothing" while an entry
// signs in with it — which is what invited deleting a live credential.
//
// PER AUTH MODE, as the CLI is handed them: a token only under subscription,
// an API key only under api-key, and the bundle under every mode, since it is
// restored into an empty credential directory whatever signs the calls in.
// inherit-env reads nothing through here: it forwards the engine's own
// environment, which the secret store is not part of.
func (l *LLMProvider) CredentialSources(key string) CredentialSources {
	profile, err := cliprofile.Load(l.AgentName(), l.Overrides)
	if err != nil {
		// A profile that does not load is refused at overrides by
		// validation, and names no variable to fall back to.
		profile = cliprofile.Profile{}
	}
	sources := CredentialSources{
		Token:  CredentialSource{Value: l.Auth.Token},
		APIKey: CredentialSource{Value: l.Auth.APIKey},
		Bundle: CredentialSource{Value: l.Auth.CredentialBundle, Fallback: cliprofile.BundleVarName(key)},
	}
	switch l.ResolvedAuthMode() {
	case AuthSubscription:
		sources.Token.Fallback = profile.TokenEnv
	case AuthAPIKey:
		// NOT a variable other vendors' keys go in too: the entry names
		// its key ([cliprofile.Profile.APIKeyEnvShared]).
		if !profile.APIKeyEnvShared {
			sources.APIKey.Fallback = profile.APIKeyEnv
		}
	}
	return sources
}

func (l *LLMProvider) validate(path Path) error {
	var p problems
	if strings.TrimSpace(l.Model) == "" {
		p.add(at(path, "model"), ErrMissing,
			"name the model this entry's CLI runs, in that CLI's own grammar "+
				"(e.g. sonnet for claude-code, anthropic/claude-sonnet-5 for opencode)")
	}
	if l.Agent != "" && !l.Agent.Valid() {
		p.add(at(path, "agent"), ErrUnknownValue, "%q (want %s)",
			l.Agent, names(CLIAgentNames))
	}
	// Left unbounded, a timeout of 0 or a negative one passes validation
	// and reaches a process supervisor that reads it as "expire
	// immediately" — every call failing instantly, with nothing in the
	// config looking wrong.
	if l.TimeoutSeconds < 0 {
		p.add(at(path, "timeout_seconds"), ErrOutOfRange,
			"must be 0 (the %v s default) or positive, got %v",
			defaultCLITimeoutSeconds, l.TimeoutSeconds)
	}
	if l.MaxConcurrent < 0 {
		p.add(at(path, "max_concurrent"), ErrOutOfRange,
			"must be 0 (the default of %d) or positive, got %d",
			defaultCLIMaxConcurrent, l.MaxConcurrent)
	}
	if l.Auth.Mode != "" && !slices.Contains(CLIAgentAuthModes, l.Auth.Mode) {
		p.add(at(path, "auth.mode"), ErrUnknownValue, "%q (want %s)",
			l.Auth.Mode, names(CLIAgentAuthModes))
	}
	switch {
	case l.Mode != "" && !slices.Contains(CLIModes, l.Mode):
		p.add(at(path, "mode"), ErrUnknownValue, "%q (want %s)",
			l.Mode, names(CLIModes))
	case l.RunIn != "" && !l.Box():
		// A value that means nothing where it is written. In host mode
		// the CLI is a subprocess of this engine and there is no cell to
		// choose — an operator who wrote one meant to switch modes.
		p.add(at(path, "run_in"), ErrConflict,
			"only box mode runs somewhere: a host-mode CLI is a subprocess of "+
				"this engine. Set `mode: box`, or remove the field")
	case l.RunIn != "" && !slices.Contains(BackendPlacements(), l.RunIn):
		// BACKEND CELLS ONLY. `self` is a seat's answer — "my code work
		// rides my executor's run" — and a box-mode entry IS that
		// run, so it has no executor of its own to ride and no backend
		// resolves it. Accepted here it validated cleanly and failed at
		// the seat's first turn, every turn, with the manager's "no
		// backend for self".
		p.add(at(path, "run_in"), ErrUnknownValue, "%q (want %s)",
			l.RunIn, names(BackendPlacements()))
	}
	p.wrap(l.validateProfile(path))
	return p.err()
}

// validateProfile holds an entry to the profile it drives — the built-in
// profile with overrides merged in, loaded by the same [cliprofile.Load] the
// backend is built from — and its model to that profile's model flag.
//
// HERE, AND NOT ONLY WHERE THE BACKEND IS BUILT, because building it is what
// `crewlet validate` and every node's apply do and what no API write does: a
// PUT, a PATCH, a setup write or a revert runs these rules and never builds a
// provider. Judged at the build alone, an override typo was admitted by the
// API, activated, and then refused by every node.
//
// RUNNABLE, because each says the backend cannot be built: the profile does
// not load, or no model_args element carries {model}, and every entry names a
// model (the backend refuses to drop one silently). Where the entry's
// CREDENTIALS go is the admission half, [LLMProvider.validateCredentials].
func (l *LLMProvider) validateProfile(path Path) error {
	if l.Agent != "" && !l.Agent.Valid() {
		return nil // reported at agent; there is no profile to judge
	}
	var p problems
	profile, err := cliprofile.Load(l.AgentName(), l.Overrides)
	switch {
	case errors.Is(err, cliprofile.ErrOverrides):
		p.add(at(path, "overrides"), ErrShape, "%v", err)
		return p.err()
	case err != nil:
		p.add(at(path, "overrides"), ErrConflict, "%v", err)
		return p.err()
	}
	if strings.TrimSpace(l.Model) != "" && !profile.TakesModel() {
		p.add(at(path, "overrides.model_args"), ErrMissing,
			"the %q profile's model_args carry no {model}, so the model %q this entry "+
				"names would never reach the CLI, and every entry names one: declare "+
				`the flag with the placeholder, e.g. overrides.model_args: ["--model", "{model}"]`,
			l.AgentName(), l.Model)
	}
	return p.err()
}

// entryPath is where provider key sits in the document.
func entryPath(key string) Path { return entry(field("providers.llm"), key) }

// validateLLMCredentials is [LLMProvider.validateCredentials] over every
// entry of the company, in key order.
func (c *Company) validateLLMCredentials() error {
	var p problems
	for _, key := range sortedKeys(c.Providers.LLM) {
		spec := c.Providers.LLM[key]
		p.wrap(spec.validateCredentials(entryPath(key)))
	}
	return p.err()
}

// validateLLMModels is [LLMProvider.validateModel] over every entry of the
// company, in key order.
func (c *Company) validateLLMModels() error {
	var p problems
	for _, key := range sortedKeys(c.Providers.LLM) {
		spec := c.Providers.LLM[key]
		p.wrap(spec.validateModel(entryPath(key)))
	}
	return p.err()
}

// validateModel holds an entry's model to the grammar its CLI reads it in,
// where the merged profile declares one
// ([cliprofile.Profile.ModelNamesProvider]): OpenCode splits its model flag at
// the first slash into a provider and a model, so a bare `claude-sonnet-5`
// names the provider "claude-sonnet-5" and no model, and every call the entry
// makes — the text calls and a box-mode run alike, which are handed the
// same value — fails inside the CLI, which OpenCode 2 refuses as "Invalid model
// reference".
//
// JUDGED ON THE VALUE THE CLI IS HANDED, the model rendered through the
// profile's model_args, so an override that puts the provider in the flag
// (["--model", "anthropic/{model}"]) admits a bare model. A model written as a
// `${VAR}` is not judged: its value is the secret store's and the
// environment's, which a write cannot see.
//
// ADMISSION rather than runnable: the provider builds and the CLI starts, the
// refusal is the CLI's grammar — a vendor fact that moves between releases,
// which is why it is a profile declaration — and a newer peer may admit what
// this build would not. So a write is refused and an apply only warns.
func (l *LLMProvider) validateModel(path Path) error {
	if (l.Agent != "" && !l.Agent.Valid()) || envref.Has(l.Model) || strings.TrimSpace(l.Model) == "" {
		return nil
	}
	profile, err := cliprofile.Load(l.AgentName(), l.Overrides)
	if err != nil || !profile.ModelNamesProvider {
		//nolint:nilerr // Deliberate: a profile that does not load is a
		// runnable fault validateProfile already reports at overrides,
		// and there is no grammar to judge the model by.
		return nil
	}
	value := profile.ModelArgument(l.Model)
	provider, model, _ := strings.Cut(value, "/")
	if strings.TrimSpace(provider) != "" && strings.TrimSpace(model) != "" {
		return nil
	}
	var p problems
	what := fmt.Sprintf("names the provider %q and no model", provider)
	if strings.TrimSpace(provider) == "" {
		what = "names no provider"
	}
	// THE VENDOR'S OWN WORDS ONLY FOR THE CLI THEY ARE MEASURED ON. Any
	// profile can declare the grammar through overrides, and an operator
	// pointed at `opencode models` for a hermes entry is sent to a tool
	// that entry does not run.
	failure, listing := "with the CLI's own model-not-found error", ""
	if l.AgentName() == string(CLIOpenCode) {
		failure = `with OpenCode's "Invalid model reference"`
		listing = " (`opencode models --standalone` lists them)"
	}
	p.add(at(path, "model"), ErrConflict,
		"the %s CLI reads its model as <provider>/<model> and splits it at the first "+
			"slash, so %q %s: every call this entry makes, a box-mode run included, "+
			"would fail inside the CLI %s. Name the provider as the CLI spells it%s, e.g. "+
			"anthropic/claude-sonnet-5 or openrouter/anthropic/claude-sonnet-5, with that "+
			"provider's key in env (e.g. ANTHROPIC_API_KEY, OPENROUTER_API_KEY) or a "+
			"login of the CLI's own",
		l.AgentName(), value, what, failure, listing)
	return p.err()
}

// validateCredentials holds an entry's credentials to the variables its
// merged profile names, those variables to the reference grammar, and the
// profile itself to keeping every credential behind auth
// ([cliprofile.Profile.ValidateCredentials]).
//
// The rules exist because the backend's applyAuth (in
// internal/providers/llm/cliagent) places a credential only where the profile
// names a variable for it and drops it silently otherwise: an api-key entry
// on a CLI with no api_key_env ran signed in to nothing, and so did a token on
// a CLI with no token_env. Every such shape is refused, naming the route the
// CLI does read.
//
// ADMISSION rather than runnable, because an entry that breaks one still
// runs: the provider builds, the CLI starts, and it signs in from whatever it
// does read — its credential files, a bundle, its own environment — while only
// the misplaced credential goes unused. So a write that keeps one is refused,
// and a revision being applied is only warned about. Refusing it at apply
// would take this node off the fleet's epoch during a rolling upgrade over a
// revision a newer peer admitted — one whose profile gained the very variable
// this build's lacks, which is how a guess like an api_key_env for opencode
// would arrive.
//
// Skipped where the runnable half already refused the entry (an unknown agent,
// a profile that does not load): there is no profile to judge credentials
// against, and the refusal that matters is already reported.
func (l *LLMProvider) validateCredentials(path Path) error {
	if l.Agent != "" && !l.Agent.Valid() {
		return nil
	}
	profile, err := cliprofile.Load(l.AgentName(), l.Overrides)
	if err != nil {
		//nolint:nilerr // Deliberate: a profile that does not load is a
		// runnable fault validateProfile already reports at overrides,
		// and there is no profile to judge credentials by.
		return nil
	}
	var p problems
	agent := l.AgentName()
	if err := profile.ValidateCredentials(agent); err != nil {
		p.add(at(path, "overrides"), ErrConflict, "%v", err)
	}

	// THE TWO VARIABLES A CREDENTIAL IS HANDED OVER IN ARE NAMES OF THE
	// REFERENCE GRAMMAR, because nothing that keeps a credential can hold
	// any other: the secret store refuses the name, so `crewlet llm login
	// -capture-token` cannot write the token there and a `${VAR}` cannot
	// spell it; no shell exports one for auth.mode inherit-env to forward;
	// and a name carrying `=` sets a different variable in the CLI's
	// environment. Only overrides can name one — every shipped profile's
	// names are in the grammar — and the CLI still starts with it, signed in
	// by whatever it does read, so this is an admission rule like the rest.
	// Here rather than in the profile's own rules because the grammar is
	// envref's, which that leaf does not import.
	for _, variable := range []struct{ field, name string }{
		{"token_env", profile.TokenEnv}, {"api_key_env", profile.APIKeyEnv},
	} {
		if variable.name != "" && !envref.ValidName(variable.name) {
			p.add(at(path, "overrides."+variable.field), ErrShape,
				"%q is not an environment variable name (letters, digits and "+
					"underscores, not starting with a digit): the CLI is handed its "+
					"credential under it, and no secret store, ${VAR} or `crewlet llm "+
					"login` can hold a name outside that grammar, nor a shell export "+
					"one for auth.mode inherit-env to forward. Name the variable your "+
					"build of the CLI reads", variable.name)
		}
	}

	// The mode-dependent rules each state what a mode does with a
	// credential, and an unknown mode does nothing this file can describe:
	// judged as one, a typo drew refusals naming a mode that does not exist.
	// The fault is reported once, at auth.mode.
	mode := l.ResolvedAuthMode()
	if !slices.Contains(CLIAgentAuthModes, mode) {
		return p.err()
	}

	if l.Auth.APIKey != "" && mode != AuthAPIKey {
		p.add(at(path, "auth.api_key"), ErrConflict,
			"an API key is read only under auth.mode api-key, which puts it in the "+
				"CLI's key variable; under %s it reaches nothing. Set auth.mode: "+
				"api-key, or drop auth.api_key", mode)
	}

	if mode == AuthAPIKey {
		switch {
		case profile.APIKeyEnv == "" && profile.EnvSignIn:
			p.add(at(path, "auth.mode"), ErrConflict,
				"api-key puts the entry's key in the CLI's key variable, and the "+
					"%q profile names none (api_key_env): this CLI reads each model "+
					"provider's key from that provider's own variable. Leave auth.mode "+
					"at subscription, drop auth.api_key, and set the key in env under "+
					"the variable of the provider its model names, as a ${VAR} — e.g. "+
					`OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"`, agent)
		case profile.APIKeyEnv == "":
			p.add(at(path, "auth.mode"), ErrConflict,
				"api-key puts the entry's key in the CLI's key variable, and the "+
					"%q profile names none (api_key_env), so the key would reach "+
					"nothing: this CLI reads a key only from %s, so put it there — "+
					"`crewlet llm login`, or a credential bundle — or, if your build of "+
					"the CLI does read a key variable, name it with "+
					"overrides.api_key_env",
				agent, profile.LoginLocation())
		case profile.APIKeyEnvShared && strings.TrimSpace(l.Auth.APIKey) == "":
			// Refused rather than left to fall back: the variable the
			// CLI reads holds other vendors' keys too, and the one the
			// engine would find there is the wrong one more often than
			// not — sent to this CLI's endpoint all the same.
			p.add(at(path, "auth.api_key"), ErrMissing,
				"name this entry's key as auth.api_key: the %q CLI reads its key from "+
					"%s, a variable other vendors' keys are kept under too, so the "+
					"engine does not read it from its own environment or secret store "+
					`in this entry's place — e.g. api_key: "${DASHSCOPE_API_KEY}"`,
				agent, profile.APIKeyEnv)
		}
	}

	// SUBSCRIPTION READS A LOGIN THE CLI KEEPS OR A HEADLESS TOKEN. A CLI that
	// keeps neither and signs in with a key alone would run signed in to
	// nothing under it: the mode removes the key variable from the CLI's
	// environment, and every call is refused for want of a key. Qwen Code is
	// that CLI — both measured builds refuse the OAuth login it once kept.
	if mode == AuthSubscription && !profile.KeepsLogin() && profile.TokenEnv == "" &&
		!profile.EnvSignIn && profile.APIKeyEnv != "" {
		p.add(at(path, "auth.mode"), ErrConflict,
			"subscription signs the CLI in with a login it keeps or a headless token, and "+
				"the %q profile names neither — no credential_paths, credential_store or "+
				"token_env — so the entry would sign in to nothing: this CLI signs in with "+
				"its key, so set auth.mode: api-key and give the key as auth.api_key (the "+
				"CLI reads it from %s)", agent, profile.APIKeyEnv)
	}

	if l.Auth.Token != "" {
		switch {
		case profile.TokenEnv == "":
			p.add(at(path, "auth.token"), ErrConflict,
				"the %q profile names no token_env — this CLI mints no headless "+
					"token — so the token would reach nothing: remove it, or name the "+
					"variable your build of the CLI reads with overrides.token_env", agent)
		case mode != AuthSubscription:
			p.add(at(path, "auth.token"), ErrConflict,
				"a token is read only under auth.mode subscription: %s removes %s "+
					"from the CLI's environment or forwards the engine's own. Remove "+
					"it, or set auth.mode: subscription", mode, profile.TokenEnv)
		}
	}

	// NOR FROM THE ENGINE'S ENVIRONMENT BY MODE. inherit-env forwards the key
	// variable as the engine's process holds it, and a shared one holds
	// whichever vendor's key was exported under it — the embeddings key, on a
	// host that runs any — sent to this CLI's endpoint all the same.
	if mode == AuthInheritEnv && profile.APIKeyEnvShared {
		p.add(at(path, "auth.mode"), ErrConflict,
			"inherit-env forwards %s from the engine's own environment, and the %q CLI's key "+
				"variable is one other vendors' keys are kept under too, so the key it forwarded "+
				"would be whichever was exported there: set auth.mode: api-key and name this "+
				`entry's key as auth.api_key, e.g. api_key: "${DASHSCOPE_API_KEY}"`,
			profile.APIKeyEnv, agent)
	}
	if mode == AuthInheritEnv && profile.TokenEnv == "" && profile.APIKeyEnv == "" {
		remedy := "sign the CLI in with `crewlet llm login`"
		if profile.EnvSignIn {
			remedy = "set the key in env under the variable of the provider " +
				"its model names, as a ${VAR}"
		}
		p.add(at(path, "auth.mode"), ErrConflict,
			"inherit-env forwards the profile's token_env and api_key_env from the "+
				"engine's own environment, and the %q profile names neither, so "+
				"nothing would be forwarded: leave auth.mode at subscription and %s",
			agent, remedy)
	}

	// The two variables auth owns. It is layered over env, and whether it
	// then replaces, removes or leaves a value written here turns on the
	// mode and on whether the secret store or the engine's own environment
	// holds a credential — which the document cannot see, so an entry
	// relying on env for one is configured to work only sometimes.
	isolation := profile.IsolationVariables()
	for _, name := range slices.Sorted(maps.Keys(l.Env)) {
		switch name {
		case "":
			continue
		case profile.TokenEnv:
			p.add(entry(at(path, "env"), name), ErrConflict,
				"%s is this CLI's token variable, which auth owns: whether a value "+
					"here reaches the CLI depends on auth.mode and on whether the secret "+
					"store or the engine's environment holds a token of its own. Give "+
					"the token as auth.token", name)
		case profile.APIKeyEnv:
			p.add(entry(at(path, "env"), name), ErrConflict,
				"%s is this CLI's key variable, which auth owns: whether a value "+
					"here reaches the CLI depends on auth.mode, and on whether auth.api_key "+
					"resolves or the engine's environment holds a key of its own. Give "+
					"the key as auth.api_key with auth.mode api-key", name)
		default:
			if subscriptionEnvName(profile, mode, name) {
				// Set with the mode, over env: a value here would be
				// replaced on every call without a word.
				p.add(entry(at(path, "env"), name), ErrConflict,
					"%s is set by the %q profile under auth.mode subscription, to %q, to "+
						"choose the login the seat's home holds; a value here is replaced on "+
						"every call, so drop it — a different value is set with "+
						"overrides.subscription_env", name, l.Agent, profile.SubscriptionEnv[name])
				continue
			}
			// env is layered OVER the variables that keep each seat's CLI
			// inside its own home and its call's working directory, so a
			// value here would replace them for every seat on the entry —
			// the engine user's real dotfiles, a configuration directory the
			// seat's system prompt is never written to, or a vendor switch
			// turned back off (OpenCode's project walk, which hands every
			// seat whatever configuration sits above the state directory) —
			// and nothing would say so.
			if layer, ok := profile.IsolationLayerOf(name); ok {
				p.add(entry(at(path, "env"), name), ErrConflict,
					"%s is a variable the engine sets for every call of this CLI to keep "+
						"it inside its seat's own home and its call's working directory "+
						"(%s); a value here would replace that isolation for every seat, so "+
						"drop it — %s", name, strings.Join(isolation, ", "),
					isolationRemedy(name, layer, profile))
			}
		}
	}
	return p.err()
}

// subscriptionEnvName reports whether name is one the profile sets under
// subscription ([cliprofile.Profile.SubscriptionEnv]) on an entry running
// under it.
func subscriptionEnvName(profile cliprofile.Profile, mode CLIAgentAuthMode, name string) bool {
	_, ok := profile.SubscriptionEnv[name]
	return ok && mode == AuthSubscription
}

// isolationRemedy is where an isolation variable's value IS changed, for the
// refusal of an entry's env that names one: the field of the layer that sets
// it ([cliprofile.Profile.IsolationLayerOf]), or none.
//
// PER LAYER, because the three fields refuse each other's variables: one
// remedy naming overrides.config_env for every variable sent an operator who
// followed it to a second refusal for both of OpenCode's —
// OPENCODE_CONFIG_DIR is work_config_env's, so config_env naming it too is
// "one variable, two directories", and OPENCODE_CONFIG_PROJECT_DISABLE is an
// isolation_env switch, which may not also be a relocated variable.
func isolationRemedy(name string, layer cliprofile.IsolationLayer, profile cliprofile.Profile) string {
	switch layer {
	case cliprofile.IsolationConfigEnv:
		return relocationRemedy("config_env", "the seat's home", profile.ConfigEnv[name],
			profile.KeptUnder(name))
	case cliprofile.IsolationWorkConfigEnv:
		return relocationRemedy("work_config_env", "the call's working directory",
			profile.WorkConfigEnv[name], profile.KeptUnder(name))
	case cliprofile.IsolationSwitch:
		// Not refused there: an override is the profile itself, written on
		// purpose. Turning the switch off is a decision about every seat's
		// isolation, and the remedy says so rather than hand it out as a
		// setting.
		return fmt.Sprintf("it is the profile's isolation switch, which the profile sets to "+
			"%q, changed only deliberately, with overrides.isolation_env, since a value "+
			"that turns it off undoes the isolation it is there for", profile.IsolationEnv[name])
	default: // cliprofile.IsolationHome, which every profile field refuses
		return "the engine points it at or inside the seat's own home for every CLI, " +
			"and no field changes it"
	}
}

// relocationRemedy is the remedy for a variable a profile relocates: the
// field that does it, and what has to move with a different directory.
//
// WITH THE PATHS KEPT UNDER IT, because the directory alone is not a remedy
// that works: the CLI finds the profile's login, sessions, tool policy or
// agent file through the variable ([cliprofile.Profile.KeptUnder]), so "a
// different directory is set with overrides.work_config_env", followed as
// written, was an entry every write path accepted whose OpenCode found no
// agent and no policy. The profile refuses such a move now; the remedy says
// what makes one load.
func relocationRemedy(field, root, dir string, kept []string) string {
	remedy := fmt.Sprintf("the profile points it at %s under %s with %s", dir, root, field)
	if len(kept) == 0 {
		return remedy + "; a different directory is set with overrides." + field
	}
	return fmt.Sprintf("%s; a different directory is set with overrides.%s, moving the "+
		"%s kept under %s beneath it in the same override, since the CLI finds them "+
		"through this variable", remedy, field, fieldNames(kept), dir)
}

// fieldNames is a profile's field names as a sentence about the file says
// them, bare beside the overrides paths it names: "a", "a and b".
func fieldNames(fields []string) string {
	said := make([]words.Said, len(fields))
	for i, f := range fields {
		said[i] = words.Said(f)
	}
	return words.And(said)
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

// EmbeddingProvider is the vector backend, and it has four callers: the
// fleet-singleton duty that embeds the company's own pages and work items for
// the semantic half of a Doclet knowledge search, each such search's query
// vector, and the similarity halves of diary and episode recall.
type EmbeddingProvider struct {
	Type    EmbeddingProviderType `yaml:"type,omitempty" json:"type,omitempty" js:"enum=openai|openai-compatible" desc:"openai (default) or openai-compatible."`
	Model   string                `yaml:"model,omitempty" json:"model,omitempty" desc:"Embedding model id."`
	APIKey  string                `secret:"true" yaml:"api_key,omitempty" json:"api_key,omitempty" desc:"API key or ${VAR} reference."`
	BaseURL string                `yaml:"base_url,omitempty" json:"base_url,omitempty" desc:"Endpoint for an openai-compatible embedding service."`

	// Dimensions is the vector width, and 0 means THE MODEL'S OWN.
	//
	// The width is a property of the model, not a setting with a global
	// default: text-embedding-3-large emits 3072 and -3-small emits 1536,
	// and a number that was right for one is silently wrong for the other.
	// So an unset value resolves from [EmbeddingModels], and a model this
	// build does not know is REFUSED rather than given a guess — see
	// Width.
	//
	// A value is an OVERRIDE, 64..4096, for a model that supports
	// shortening its output. It must match what the model actually
	// produces: the store's vector columns are checked against it, and a
	// mismatch is not a degraded search but a write that cannot be read
	// back. So a model whose endpoint documents the `dimensions` parameter
	// as unsupported ([DimensionsUnsupported] — embed-v4.0 through
	// Cohere's Compatibility API) is refused any width but its own, and is
	// never sent the parameter at that one.
	Dimensions int `yaml:"dimensions,omitempty" json:"dimensions,omitempty" js:"min=0;max=4096" desc:"Vector width; 0 takes the named model's own width. 64..4096 to override, and it must match what the model produces."`

	// MaxInputTokens, MaxBatchInputs and MaxBatchTokens are the model's
	// LIMITS — the most tokens one input may hold, the most inputs one
	// request may carry, and the most tokens one request may carry summed
	// over its inputs — and 0 means THE MODEL'S OWN, on exactly the rule
	// Dimensions follows and for the same reason: a limit is a fact about
	// the model, and a guess at it is a company whose every long document
	// is refused by its provider, or whose every batch is, from the day
	// embeddings are turned on.
	//
	// So an unset value resolves from [EmbeddingModels], and one the table
	// does not carry — a model this build does not know, or a limit its
	// vendor does not document for the endpoint this backend calls — is
	// REFUSED until it is stated. A stated value for a limit the table does
	// carry may only LOWER it, which is what it is for: a gateway or a proxy
	// in front of the model that accepts less than the model does. Raising a
	// documented limit is refused, naming the field, because the provider
	// would refuse what this engine then sends.
	//
	// Tokens, because that is the unit every vendor documents a limit in;
	// what the engine counts is bytes, and [EmbeddingProvider.InputBound] is
	// the one conversion, which [EmbeddingProvider.Limits] carries.
	//
	// THIS BUILD REFUSES A STORED REVISION THAT LACKS ONE IT NEEDS — a
	// company naming gemini-embedding-001, embed-v4.0 or a model the table
	// does not carry, with no limits stated — exactly as it refuses an
	// unknown model with no width: a RUNNABLE rule, not an admission one,
	// because there is no running such a company without either guessing
	// the limits or sending what the model refuses. It is refused at boot as
	// well as at an apply, so no node starts on that revision; the
	// configuration guide tells an operator to state the limits in the same
	// edit that names the model.
	MaxInputTokens int `yaml:"max_input_tokens,omitempty" json:"max_input_tokens,omitempty" js:"min=0" desc:"Most tokens one input may hold; 0 takes the named model's documented window. Required for a model this build does not know; may only lower a documented one."`
	MaxBatchInputs int `yaml:"max_batch_inputs,omitempty" json:"max_batch_inputs,omitempty" js:"min=0" desc:"Most inputs one request may carry; 0 takes the named model's documented limit. Required where none is documented; may only lower a documented one."`
	MaxBatchTokens int `yaml:"max_batch_tokens,omitempty" json:"max_batch_tokens,omitempty" js:"min=0" desc:"Most tokens one request may carry, summed over its inputs; 0 takes the named model's documented limit. Required where none is documented; may only lower a documented one."`
}

// EmbeddingModel is what this build knows about one embedding model: the width
// it emits, what the endpoint the embedding backend calls does with a width
// asked for, and the limits its vendor documents for that endpoint — an
// OpenAI-shaped POST /embeddings carrying `model`, an `input` array and, where
// the endpoint takes it, `dimensions` (internal/providers/embeddings).
//
// A ZERO LIMIT IS "NOT DOCUMENTED", never "unlimited": [Undocumented] says
// which source was read and what it did not say, and validation refuses the
// provider until the operator states the value ([EmbeddingProvider]). A
// guessed limit is the failure the table exists to prevent — and a limit
// copied from a DIFFERENT endpoint of the same vendor is a guess with a
// citation, which is worse, because it reads as checked.
type EmbeddingModel struct {
	// Width is the vector width the model emits when no shorter one is
	// asked for.
	Width int

	// Dimensions is what the endpoint does with the request's `dimensions`
	// parameter for this model, as its vendor documents it — which decides
	// whether the embedder sends it ([EmbeddingProvider.SendsDimensions])
	// and whether a width other than [EmbeddingModel.Width] can be
	// configured at all.
	Dimensions DimensionsSupport

	// DimensionsSource is the documentation [EmbeddingModel.Dimensions] is
	// read from, which a refusal of a width the endpoint cannot produce
	// quotes — so the operator is told where to check, not merely that
	// this build says no.
	DimensionsSource string

	// InputTokens is the most tokens one input may hold.
	InputTokens int

	// BatchInputs is the most inputs one request may carry.
	BatchInputs int

	// BatchTokens is the most tokens one request may carry, summed over its
	// inputs.
	BatchTokens int

	// WrapTokens is how many tokens the server may add around each input
	// beyond the input's own: the special tokens a model is fed with its
	// text. Zero only where the vendor documents its count as the input's
	// own tokens and nothing else; [EmbeddingWrapTokens] everywhere else.
	WrapTokens int

	// Undocumented names, when any limit above is zero, the documentation
	// that was read and what it does not state — which is what the refusal
	// tells an operator, so the field they are asked for comes with the
	// reason nobody filled it in for them.
	Undocumented string
}

// DimensionsSupport is what an embedding endpoint does with the request's
// `dimensions` parameter for one model, as its vendor documents it.
//
// THREE VALUES, because the documentation says one of three things, and each
// is acted on differently. An endpoint that SHORTENS to the width asked for is
// sent it, so a store sized below the model's own width is filled at that
// width. One documented as NOT TAKING it is never sent it, and holds a company
// to the model's own width, because a width it cannot produce is a store whose
// every vector would be refused. And where the documentation is SILENT the
// parameter is sent as it always has been, since dropping it would rest on a
// guess about the endpoint as much as keeping it does — and the width check
// every response passes (internal/providers/embeddings) is what catches an
// endpoint that ignores it.
type DimensionsSupport string

// The documented answers.
const (
	// DimensionsShortens is an endpoint that documents the parameter and
	// returns a vector of the width asked for.
	DimensionsShortens DimensionsSupport = "shortens"

	// DimensionsUnsupported is an endpoint that documents the parameter as
	// one it does not take, so it returns the model's own width and no
	// other.
	DimensionsUnsupported DimensionsSupport = "unsupported"

	// DimensionsUnstated is an endpoint whose documentation says nothing
	// about the parameter.
	DimensionsUnstated DimensionsSupport = "unstated"
)

// Valid reports whether d is one of the documented answers.
func (d DimensionsSupport) Valid() bool {
	switch d {
	case DimensionsShortens, DimensionsUnsupported, DimensionsUnstated:
		return true
	}
	return false
}

// EmbeddingModels is every embedding model this build knows, keyed by the id
// the provider is configured with.
//
// # Why a table and not a default
//
// The width decides whether a vector written today can be read tomorrow, and
// the limits decide whether a request is accepted at all — and all four are
// facts about the MODEL. A single default was wrong the moment a company named
// a model that emits something else — and the default this replaced justified
// itself by a case that could not exist: it was documented as "what an entry
// that names no model gets", while validation refuses exactly that entry. A
// model absent from this table is refused rather than guessed at, naming both
// ways to fix it: name a model this build knows, or state the values.
//
// # Why the engine counts BYTES against a limit stated in tokens
//
// Every tokenizer these models use emits at most one token per byte of the
// text it is given, plus the special tokens a server wraps an input in:
//
//   - a BYTE-LEVEL BPE (OpenAI's cl100k_base) starts from one token per byte
//     and every merge replaces two tokens with one;
//   - a SENTENCEPIECE model with byte fallback (Gemma's, which the Gemini
//     embedders share, and most open models') emits pieces that each cover at
//     least one character, falling back to one token per BYTE for a character
//     its vocabulary lacks — plus, at most, a word-boundary marker in front of
//     the first piece;
//   - a WORDPIECE model (the BERT family most self-hosted embedders are)
//     emits pieces that each cover at least one character, and one [UNK] for
//     a word it cannot split.
//
// So a text of N bytes is at most N tokens plus the wrapper, which is a bound
// that needs NO TOKENIZER — and that is the reason for it. A tokenizer port
// (a Go tiktoken with its ranks embedded) was the alternative and is
// rejected: it is exact for one vendor only, every other model this backend
// serves has its own, and the standard library plus this argument already
// keeps every input inside every window.
//
// The argument has one hole and it is stated rather than papered over: a
// tokenizer that applies Unicode COMPATIBILITY normalisation (NFKC) before it
// splits can lengthen a few characters — U+FDFA, one character of three bytes,
// becomes eighteen. Byte-level BPE normalises nothing, so OpenAI's bound is
// exact; for a normalising model the wrap allowance absorbs a handful of
// those characters, and an input dense with them is refused by the provider
// as a request it will refuse again, which the embedder classifies so a
// caller can set that one input aside.
//
// The price of counting bytes is that ordinary prose — three to four bytes a
// token — is held to about a quarter of a model's window. For a caller that
// represents a long text in CHUNKS that costs nothing but more chunks for the
// same tokens; for a caller that embeds a text's OPENING it is a shorter
// opening, which is that caller's policy to weigh.
//
// # Over the limit, refused or truncated?
//
// Each entry says what its vendor documents, and none of it decides anything:
// the embedder refuses, BEFORE any request, an input past the byte bound, and
// the byte bound is inside the window, so no server is ever sent an input its
// over-length behaviour applies to.
var EmbeddingModels = map[string]EmbeddingModel{
	// OpenAI's embeddings reference
	// (https://developers.openai.com/api/reference/resources/embeddings/methods/create):
	// "The input must not exceed the max input tokens for the model (8192
	// tokens for all embedding models)", an array of at most 2048 inputs, and
	// "a maximum of 300,000 tokens summed across all inputs in a single
	// request". 8192, not the 8191 older pages gave: the reference and the
	// embeddings guide's model table (developers.openai.com/api/docs/guides/
	// embeddings) both say 8192 today. WrapTokens is 0 because the guide's
	// own way to check an input against the limit is to count it with
	// cl100k_base and nothing else — so the per-input byte bound is exactly
	// 8192, which is the size the knowledge corpus has always embedded at
	// (search.EmbedInputBytes): a byte less would move the opening of every
	// long document, change the digest its replicated vector record carries,
	// and re-embed the corpus.
	//
	// `dimensions`: "The number of dimensions the resulting output
	// embeddings should have. Only supported in text-embedding-3 and later
	// models" (the same reference), and the guide's table gives each
	// model's own width.
	"text-embedding-3-large": {
		Width: 3072, Dimensions: DimensionsShortens, DimensionsSource: openAIDimensions,
		InputTokens: 8192, BatchInputs: 2048, BatchTokens: 300_000,
	},
	"text-embedding-3-small": {
		Width: 1536, Dimensions: DimensionsShortens, DimensionsSource: openAIDimensions,
		InputTokens: 8192, BatchInputs: 2048, BatchTokens: 300_000,
	},

	// The model page (https://ai.google.dev/gemini-api/docs/models/gemini-embedding-001):
	// an input token limit of 2,048 and a flexible output of 128..3072. The
	// OpenAI-compatible endpoint this backend calls is documented only by an
	// example (https://ai.google.dev/gemini-api/docs/openai); the native API
	// truncates an over-long input silently when asked to (autoTruncate,
	// https://ai.google.dev/api/embeddings), and neither page says what the
	// compatible endpoint does.
	//
	// `dimensions` is not mentioned on the compatibility page at all, and its
	// examples send none. The native API shortens on request through its own
	// `output_dimensionality` and emits 3072 by default
	// (https://ai.google.dev/gemini-api/docs/embeddings), which says nothing
	// about whether the compatible endpoint maps the OpenAI parameter onto
	// it, ignores it or refuses it — so it is sent as it always was, and the
	// width check on every response is what holds the vector to the store.
	"gemini-embedding-001": {
		Width: 3072, Dimensions: DimensionsUnstated,
		DimensionsSource: "Google's page for the OpenAI-compatible endpoint " +
			"(ai.google.dev/gemini-api/docs/openai) does not mention `dimensions`",
		InputTokens: 2048, WrapTokens: EmbeddingWrapTokens,
		Undocumented: "Google's page for the OpenAI-compatible endpoint " +
			"(ai.google.dev/gemini-api/docs/openai) states no limit on the " +
			"inputs or the tokens one request may carry, and the native " +
			"batchEmbedContents reference states none either",
	},

	// The model page (https://docs.cohere.com/docs/cohere-embed): a 128k
	// context, read as 128 000 — the smaller of the two things "128k" can
	// mean, so the bound holds under either — and a default output of 1536.
	// The Compatibility API this backend calls
	// (https://docs.cohere.com/docs/compatibility-api) documents no limit at
	// all; the 96 texts a call of the native Embed API
	// (https://docs.cohere.com/reference/embed) is that endpoint's, and so is
	// its silent END truncation.
	//
	// That page lists `dimensions` among the embeddings parameters it does
	// NOT support (it takes `input`, `model` and `encoding_format`), and
	// says neither whether one sent is ignored or refused nor how a width is
	// chosen instead — the native API's `output_dimension` is among the
	// Cohere-specific parameters it does not take either. So the endpoint
	// returns the model's own width, 1536 of 256, 512, 1024 and 1536 (the
	// model page), and nothing else: the parameter is never sent, and no
	// other width can be configured.
	"embed-v4.0": {
		Width: 1536, Dimensions: DimensionsUnsupported,
		DimensionsSource: "Cohere's Compatibility API " +
			"(docs.cohere.com/docs/compatibility-api) lists `dimensions` among " +
			"the embeddings parameters it does not support",
		InputTokens: 128_000, WrapTokens: EmbeddingWrapTokens,
		Undocumented: "Cohere's Compatibility API " +
			"(docs.cohere.com/docs/compatibility-api) states no limit on the " +
			"inputs or the tokens one request may carry — the native Embed " +
			"API's 96 texts a call is a different endpoint's",
	},
}

// openAIDimensions is where OpenAI documents the `dimensions` parameter for its
// third-generation models.
const openAIDimensions = "OpenAI's embeddings reference " +
	"(developers.openai.com/api/reference/resources/embeddings/methods/create): " +
	"`dimensions` is supported in text-embedding-3 and later models"

// EmbeddingWrapTokens is the allowance, in tokens, for what a server adds
// around each input of a model whose vendor does not document its count —
// every model in [EmbeddingModels] that is not OpenAI's, and every model it
// does not carry.
//
// SIXTEEN. The wrappers in use are two or three tokens — [CLS] and [SEP] for
// the BERT family, <s> and </s> plus SentencePiece's word-boundary marker,
// BOS and EOS for a decoder used as an embedder — so sixteen is five times the
// largest, with room left for the few NFKC expansions the byte argument cannot
// cover ([EmbeddingModels]). It costs sixteen bytes of an input's bound,
// under one percent of a 2 048-token window. A server configured to PREPEND
// an instruction of its own is outside it: declare `max_input_tokens` net of
// that instruction.
const EmbeddingWrapTokens = 16

// EmbeddingLimits is what the embedder enforces, in the unit it counts without
// a tokenizer: bytes of PREPARED text — whitespace collapsed and every stray
// byte a U+FFFD, which is the text the server decodes from the request
// (embeddings.Prepare).
type EmbeddingLimits struct {
	// InputBytes is the most prepared bytes one input may hold.
	InputBytes int

	// BatchInputs is the most inputs one request may carry.
	BatchInputs int

	// BatchBytes is the most one request may carry, counting each input as
	// its prepared bytes plus InputOverhead.
	BatchBytes int

	// InputOverhead is what each input costs a request beyond its own
	// bytes: the model's wrap allowance.
	InputOverhead int
}

// MinEmbeddingInputBytes is the narrowest input bound the limits may resolve
// to: one character of any script, which is four bytes in UTF-8. A bound
// narrower than that cannot hold every text at all, so a declaration that
// resolves below it is refused rather than turned into an embedder that
// refuses whatever it is given.
const MinEmbeddingInputBytes = 4

// The bounds on an explicit width.
const (
	// MinEmbeddingWidth is the narrowest width worth storing. Below it the
	// binary quantization the search's first stage uses degrades sharply,
	// and the vendors that support shortening do not go lower.
	MinEmbeddingWidth = 64

	// MaxEmbeddingWidth is anchored to the widest width any model in
	// current use emits.
	MaxEmbeddingWidth = 4096
)

// Width is the vector width this provider produces, and 0 when the answer is
// unknown — which validation refuses, so no caller meets it.
func (e *EmbeddingProvider) Width() int {
	if e.Dimensions > 0 {
		return e.Dimensions
	}
	return EmbeddingModels[strings.TrimSpace(e.Model)].Width
}

// SendsDimensions is whether the embedder asks for its width in every request
// — the `dimensions` parameter — rather than only checking the width of what
// comes back.
//
// Every model but one kind is asked: an endpoint that shortens answers at the
// width the store was sized for, and one whose documentation is silent, or a
// model this build does not know, is asked as it always has been. The one
// kind is a model whose endpoint documents the parameter as NOT SUPPORTED
// ([DimensionsUnsupported]) at the width it emits on its own: the answer is
// that width without the parameter, and a parameter an endpoint says it does
// not take is one it may refuse. At any other width the parameter is sent as
// before — validation refuses that width, so only a caller that skipped
// validation meets it, and it then fails as it always did, at the width check.
func (e *EmbeddingProvider) SendsDimensions() bool {
	model, known := EmbeddingModels[strings.TrimSpace(e.Model)]
	return !known || model.Dimensions != DimensionsUnsupported || e.Width() != model.Width
}

// InputBound is the most PREPARED BYTES one input to this provider may hold,
// and 0 when the model's window is unknown — which validation refuses, so no
// caller of [EmbeddingProvider.Limits] meets it.
//
// THE ONE CONVERSION from what a vendor documents (tokens) to what the engine
// counts (bytes), by the argument at [EmbeddingModels]: a prepared text of N
// bytes is at most N tokens plus the wrap allowance, so an input of
// window − wrap bytes is inside the window. The window is the model's
// documented one, lowered by a stated `max_input_tokens`, and — because one
// input must fit one request — lowered again to the request's own token total
// where that is known and smaller: a gateway whose request is smaller than the
// model's window bounds the input too.
//
// Its own method rather than a field of [EmbeddingProvider.Limits], because
// the bound is known wherever the window is, and the request limits are not:
// a model whose vendor documents its window but nothing per request still has
// a per-input bound a reader of its vectors needs — the window a stored corpus
// was embedded at — while it has no limits an embedder could be built from.
// Limits takes this value; nothing re-derives it.
//
// A stated limit LOWERS a documented one and never raises it — validation
// refuses the attempt, and this takes the lower of the two regardless, so a
// bound the provider would refuse is not enforced even by a caller that
// skipped validation.
func (e *EmbeddingProvider) InputBound() int {
	model := EmbeddingModels[strings.TrimSpace(e.Model)]
	window := limitOf(e.MaxInputTokens, model.InputTokens)
	if tokens := limitOf(e.MaxBatchTokens, model.BatchTokens); tokens > 0 {
		window = min(window, tokens)
	}
	if bound := window - e.wrapTokens(); window > 0 && bound >= MinEmbeddingInputBytes {
		return bound
	}
	return 0
}

// Limits is what the embedder enforces for this provider, and the zero value
// when any limit is unknown — which validation refuses, so no caller meets it.
//
// The per-input bound is [EmbeddingProvider.InputBound], the one conversion;
// the request half is counted in the same unit — a request whose inputs'
// bytes plus one wrap allowance each sum to the request's token total is
// inside that total. A stated limit lowers a documented one and never raises
// it, as there.
func (e *EmbeddingProvider) Limits() EmbeddingLimits {
	model := EmbeddingModels[strings.TrimSpace(e.Model)]
	bound := e.InputBound()
	inputs := limitOf(e.MaxBatchInputs, model.BatchInputs)
	tokens := limitOf(e.MaxBatchTokens, model.BatchTokens)
	if bound == 0 || inputs <= 0 || tokens <= 0 {
		return EmbeddingLimits{}
	}
	return EmbeddingLimits{
		InputBytes:    bound,
		BatchInputs:   inputs,
		BatchBytes:    tokens,
		InputOverhead: e.wrapTokens(),
	}
}

// wrapTokens is the model's wrap allowance: the table's for a model it
// carries, [EmbeddingWrapTokens] for one it does not.
func (e *EmbeddingProvider) wrapTokens() int {
	if model, known := EmbeddingModels[strings.TrimSpace(e.Model)]; known {
		return model.WrapTokens
	}
	return EmbeddingWrapTokens
}

// limitOf is a limit as stated over the documented one: the stated value where
// there is one, never above the documented one, and the documented one where
// nothing was stated. Zero is "unknown".
func limitOf(stated, documented int) int {
	switch {
	case stated <= 0:
		return documented
	case documented > 0:
		return min(stated, documented)
	default:
		return stated
	}
}

// EmbeddingKeyVar is the variable an embedding provider that names no
// api_key reads its key from: the one OpenAI's own SDK documents, so a key
// already exported in the shell works with no YAML change.
const EmbeddingKeyVar = "OPENAI_API_KEY"

// ResolvedKey is the embedder's credential, resolved through r and trimmed.
//
// A field left EMPTY reads [EmbeddingKeyVar], and a reference that resolves to
// nothing stays nothing. An embedder told to use EMBED_KEY that quietly ran on
// the account behind OPENAI_API_KEY when EMBED_KEY was unset would bill the
// wrong account with nothing on any surface to say so.
func (e *EmbeddingProvider) ResolvedKey(r *Resolver) string {
	if strings.TrimSpace(e.APIKey) == "" {
		return strings.TrimSpace(r.Lookup(EmbeddingKeyVar))
	}
	return strings.TrimSpace(r.Value(e.APIKey))
}

func (e *EmbeddingProvider) validate(path Path) error {
	var p problems
	if e.Type != "" && !slices.Contains(EmbeddingProviderTypes, e.Type) {
		p.add(at(path, "type"), ErrUnknownValue, "%q (want %s)", e.Type, names(EmbeddingProviderTypes))
	}
	if strings.TrimSpace(e.Model) == "" {
		p.add(at(path, "model"), ErrMissing, "name the embedding model")
	}
	// A width is not a tuning mistake — the store checks its vector
	// columns against it, so it decides whether a written embedding can
	// ever be read back.
	//
	// THE js TAG CANNOT EXPRESS "0 OR 64..4096", so the schema publishes
	// 0..4096 and the floor lives here, in a message naming both forms.
	switch {
	case e.Dimensions < 0:
		p.add(at(path, "dimensions"), ErrOutOfRange,
			"must be 0 (the named model's own width) or %d..%d, got %d",
			MinEmbeddingWidth, MaxEmbeddingWidth, e.Dimensions)
	case e.Dimensions > 0 && e.Dimensions < MinEmbeddingWidth:
		p.add(at(path, "dimensions"), ErrOutOfRange,
			"%d is below the %d floor — leave it unset to take %q's own width, "+
				"or state a width of at least %d",
			e.Dimensions, MinEmbeddingWidth, strings.TrimSpace(e.Model), MinEmbeddingWidth)
	case e.Dimensions > MaxEmbeddingWidth:
		p.add(at(path, "dimensions"), ErrOutOfRange,
			"%d is past the %d ceiling, which is the widest any model in current "+
				"use emits", e.Dimensions, MaxEmbeddingWidth)
	case e.Dimensions > 0 && fixedWidth(strings.TrimSpace(e.Model)) && e.Dimensions != e.modelWidth():
		// A WIDTH THE ENDPOINT CANNOT PRODUCE is refused here, not
		// discovered at the first embedding: the store's columns are
		// sized from this value, and an endpoint that takes no
		// `dimensions` answers at the model's own width whatever was
		// configured, so every vector would be refused as the wrong
		// width for as long as the company ran.
		model := EmbeddingModels[strings.TrimSpace(e.Model)]
		p.add(at(path, "dimensions"), ErrConflict,
			"%q emits %d through the endpoint this backend calls, and that "+
				"endpoint takes no width of its own: %s. Leave `dimensions` "+
				"unset, or state %d",
			strings.TrimSpace(e.Model), model.Width, model.DimensionsSource, model.Width)
	case e.Dimensions == 0 && strings.TrimSpace(e.Model) != "":
		// AN UNKNOWN MODEL WITH NO WIDTH IS REFUSED, not defaulted. The
		// width decides whether a vector written today can be read
		// tomorrow, and a guess here is a company whose recall silently
		// stops working the day it is turned on.
		if _, known := EmbeddingModels[strings.TrimSpace(e.Model)]; !known {
			p.add(at(path, "dimensions"), ErrMissing,
				"this build does not know how wide %q's vectors are. Either name "+
					"a model it knows (%s) or state `dimensions` yourself",
				strings.TrimSpace(e.Model), knownModels())
		}
	}
	if strings.TrimSpace(e.Model) != "" {
		e.validateLimits(path, &p)
	}
	if e.Type == EmbeddingOpenAICompatible && strings.TrimSpace(e.BaseURL) == "" {
		p.add(at(path, "base_url"), ErrMissing,
			"an openai-compatible embedding provider needs the endpoint to talk to")
	}
	return p.err()
}

// fixedWidth reports whether the endpoint for model takes no `dimensions`, so
// the model's own width is the only one it produces.
func fixedWidth(model string) bool {
	return EmbeddingModels[model].Dimensions == DimensionsUnsupported
}

// modelWidth is the width the named model emits on its own, 0 for a model this
// build does not carry.
func (e *EmbeddingProvider) modelWidth() int {
	return EmbeddingModels[strings.TrimSpace(e.Model)].Width
}

// validateLimits refuses a limit that is unknown, negative, above the model's
// documented one, or too small to hold a character — each naming the field.
func (e *EmbeddingProvider) validateLimits(path Path, p *problems) {
	name := strings.TrimSpace(e.Model)
	model, known := EmbeddingModels[name]
	wrap := e.wrapTokens()
	for _, limit := range []struct {
		field  string
		stated int
		of     func(EmbeddingModel) int
		what   string
	}{
		{"max_input_tokens", e.MaxInputTokens, func(m EmbeddingModel) int { return m.InputTokens }, "tokens one input may hold"},
		{"max_batch_inputs", e.MaxBatchInputs, func(m EmbeddingModel) int { return m.BatchInputs }, "inputs one request may carry"},
		{"max_batch_tokens", e.MaxBatchTokens, func(m EmbeddingModel) int { return m.BatchTokens }, "tokens one request may carry"},
	} {
		documented := limit.of(model)
		switch {
		case limit.stated < 0:
			p.add(at(path, limit.field), ErrOutOfRange,
				"must be 0 (the named model's documented limit) or the most %s, "+
					"got %d", limit.what, limit.stated)
		case limit.stated > 0 && documented > 0 && limit.stated > documented:
			// RAISING A DOCUMENTED LIMIT is refused, not obeyed: the
			// provider would refuse what the engine then sends, and
			// the field exists to LOWER one — a gateway or proxy in
			// front of the model that accepts less.
			p.add(at(path, limit.field), ErrOutOfRange,
				"%d is above the %d %s for %q as its vendor documents it; this "+
					"field may only lower a documented limit (for a gateway or a "+
					"proxy that accepts less) — leave it unset to take %d",
				limit.stated, documented, limit.what, name, documented)
		case limit.stated == 0 && documented == 0 && known:
			p.add(at(path, limit.field), ErrMissing,
				"this build carries no value for the most %s for %q: %s. State "+
					"`%s` from the limit your endpoint enforces",
				limit.what, name, model.Undocumented, limit.field)
		case limit.stated == 0 && documented == 0:
			// THE WAY OUT NAMES ONLY MODELS THAT TAKE IT: a model the
			// table carries without this limit would be refused for
			// the same field, so suggesting it is advice that does not
			// clear the error. With none to name, stating it is the
			// one way out, and the refusal says only that.
			if models := modelsDocumenting(limit.of); models != "" {
				p.add(at(path, limit.field), ErrMissing,
					"this build does not know the most %s for %q. Either name a "+
						"model whose limit it carries (%s) or state `%s` yourself",
					limit.what, name, models, limit.field)
			} else {
				p.add(at(path, limit.field), ErrMissing,
					"this build does not know the most %s for %q, nor for any "+
						"model it carries. State `%s` from the limit your endpoint "+
						"enforces", limit.what, name, limit.field)
			}
		}
	}
	// AN INPUT BOUND THAT CANNOT HOLD A CHARACTER is refused here rather
	// than built into an embedder that refuses everything: the bound is a
	// window less the wrap allowance, and a window at or under the
	// allowance leaves nothing for the text.
	input := limitOf(e.MaxInputTokens, model.InputTokens)
	tokens := limitOf(e.MaxBatchTokens, model.BatchTokens)
	floor := wrap + MinEmbeddingInputBytes
	if e.MaxInputTokens > 0 && input < floor {
		p.add(at(path, "max_input_tokens"), ErrOutOfRange,
			"%d leaves no room for text: %d tokens are reserved for what a "+
				"server wraps around each input, so the window must be at least %d",
			e.MaxInputTokens, wrap, floor)
	}
	if e.MaxBatchTokens > 0 && tokens < floor {
		p.add(at(path, "max_batch_tokens"), ErrOutOfRange,
			"%d leaves no room for text: %d tokens are reserved for what a "+
				"server wraps around each input, so a request must take at least %d",
			e.MaxBatchTokens, wrap, floor)
	}
}

// knownModels lists the models this build carries, sorted, for the refusals
// above.
func knownModels() string {
	return modelsDocumenting(func(EmbeddingModel) int { return 1 })
}

// modelsDocumenting lists, sorted, the models this build carries a value of
// limit for — the models that name in a refusal of that limit is a way out of
// it — or "" when none does.
func modelsDocumenting(limit func(EmbeddingModel) int) string {
	out := make([]string, 0, len(EmbeddingModels))
	for name, model := range EmbeddingModels {
		if limit(model) > 0 {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}
