package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/words"
)

// Providers is the Tier B model surface: which LLMs a seat can be pointed
// at, what embeds its memories, and where its executor runs.
//
// The Tier A infrastructure — store, stream, coordination — is deliberately
// not here: it cannot be changed without a restart, and mixing the two
// tiers in one block is how an operator comes to believe the broker's
// address is live-editable.
type Providers struct {
	// LLM is the named models seats select from by key, each a harness
	// the engine drives ([LLMProvider]). A seat naming a key that is not
	// here falls back to `default`, else the first provider declared — a
	// model nobody chose for it — so the keys are the company's model
	// vocabulary, and a company naming one is refused at Validate.
	LLM map[string]LLMProvider `yaml:"llm,omitempty" json:"llm,omitempty" desc:"Named models, each a harness the engine drives; seats select one by key."`

	// LLMOrder is the order the providers were DECLARED in; per-phase
	// resolution's last resort is "the first provider configured"
	// (agent/phase.Registry).
	//
	// A field, not a derivation: a Go map has no order, and a company config is
	// stored as JSON and re-read on every boot, so an order recovered only at
	// YAML-parse time is one the running engine never sees.
	//
	// Normally derived from key order, but readable, because the config read
	// surface emits it and must accept it back (GET-edit-PUT). An explicit
	// value WINS over key order, so the round trip is lossless and an operator
	// can pin it. With neither, sorted keys; see ProviderOrder.
	LLMOrder []string `yaml:"llm_order,omitempty" json:"llm_order,omitempty" desc:"Provider precedence; normally derived from the order they are written in."`

	// Embeddings powers every vector this engine computes: the semantic half
	// of a Doclet knowledge search and the similarity halves of diary and
	// episode recall. Nil disables them — a search is keyword only, and
	// episodes are still written, but nothing searches them by similarity.
	Embeddings *EmbeddingProvider `yaml:"embeddings,omitempty" json:"embeddings,omitempty" desc:"Embedding provider for semantic knowledge search and for diary and episode recall."`

	// Sandbox is the catalogue of cells every agent seat's executor runs
	// in: each executor is its harness's own run, in a box. Required as
	// soon as the company has an agent seat AND a providers.llm entry to
	// run it on ([Company.validateSandboxPlacement]); nil only while the
	// company has no agent seat or no model — an org chart written before
	// its credentials runs no executor, so it needs no cell either.
	Sandbox *SandboxProvider `yaml:"sandbox,omitempty" json:"sandbox,omitempty" desc:"Where every agent seat's executor runs. Required once the company has an agent seat and a providers.llm entry."`
}

// DefaultProviders is the empty provider surface: no models, no
// embeddings, no sandbox. A company with no LLM provider parses, validates
// and runs, which is what lets an org chart be authored before the
// credentials exist: its seats are placed and hold their work until a
// revision adds a provider — and, with it, the sandbox catalogue its
// executors run in.
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
// yaml.v3 resolves `<<: *shared` during DECODE, so the map holds the merged
// entries while the raw node shows only `<<`; without the expansion those
// providers fall to the sorted tail and silently move whichever one an
// operator listed first. Explicit keys still win, via ProviderOrder's dedupe.
//
// NO ALIAS FOLLOWING, deliberately: an alias here can only name a mapping whose
// anchor already contributed its keys, and every top-level key is typed, so no
// provider-map anchor can live elsewhere. If a future top-level field is
// shaped like a provider map, this becomes reachable: check it then.
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
// two travel separately through JSON and a stored revision can desync them.
// Keys the order names but the map lacks are dropped; keys the map has but the
// order omits are appended sorted, so the result is always a permutation of
// the map's keys.
//
// The sorted tail is arbitrary but stable, the only honest answer when the
// declaration order was never recorded, and better than a map range.
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

// LLMProvider is one named entry under providers.llm: a harness (Claude
// Code, Codex, OpenCode and the rest of [Harnesses]) the engine drives as its
// model.
//
// There is no HTTP client to a model vendor here, by decision: each vendor's
// harness owns its request shapes, thinking replay, caching and sign-in, and
// moves them with every model, so a client here would be a trailing copy.
//
// A seat's EXECUTOR is its harness's agentic run in a box ([ExecutorHarnesses]
// have an in-box runner; tools come over internal/api/mcpbridge). Every OTHER
// phase runs the harness on this host, shell and editor denied, tools as the
// `crewlet` MCP server ([ToolServerName]); any harness can serve one.
type LLMProvider struct {
	// Harness is which harness to drive. Empty drives Claude Code.
	Harness Harness `yaml:"harness,omitempty" json:"harness,omitempty" js:"enum=claude-code|codex|gemini-cli|qwen-code|opencode|cursor-agent|copilot|grok|muse-code|kimi-code|hermes|pi|custom" desc:"Which harness to drive (default claude-code)."`

	// Model is handed to the harness's model flag, in that harness's own grammar:
	// an alias or id for Claude Code (`sonnet`, `claude-opus-5-5`), an
	// OpenAI id for Codex, `<provider>/<model>` for OpenCode.
	Model string `yaml:"model,omitempty" json:"model,omitempty" desc:"Model id, in the harness's own grammar (OpenCode: <provider>/<model>)."`

	// Effort is how hard the model reasons, as the harness spells its own levels
	// ([harnessprofile.Effort]). Empty passes none: the harness's own default.
	Effort string `yaml:"effort,omitempty" json:"effort,omitempty" desc:"Reasoning effort, in the harness's own levels (claude-code: low, medium, high, xhigh, max). Refused on a harness with no such setting, or a level it does not take. Absent: the harness's default."`

	// RunIn is WHERE a run launched on this entry happens, naming a cell of
	// providers.sandbox. Empty takes the catalogue's default.
	//
	// On the entry, not the seat: it is a property of this RUNTIME. The entry
	// whose harness needs the host's reach runs `direct`, the rest run in a
	// `container`; an operator wanting both for one harness makes two entries.
	//
	// READ WHENEVER AN EXECUTOR LAUNCHES ON THIS ENTRY: a launch takes the
	// chain's first member that can run and is not sitting out, so a fallback
	// is placed by its own run_in. REFUSED ON A HARNESS THAT CANNOT LAUNCH ONE
	// ([ExecutorHarnesses]): nothing would read the field.
	RunIn Placement `yaml:"run_in,omitempty" json:"run_in,omitempty" js:"enum=direct|container" desc:"Where a seat's executor run on this entry happens. Empty = providers.sandbox.default_run_in."`

	// StateDir is where this provider keeps its credential directory and
	// per-seat homes, under a subdirectory named for its harness
	// (<state_dir>/<harness>). Empty derives one per key, so providers never
	// collide.
	//
	// ENTRIES ON ONE DIRECTORY DRIVING ONE HARNESS SHARE ONE LOGIN, one set of
	// per-seat homes and one generation: that is how per-phase models work off
	// a single `crewlet llm login`. Entries on one directory driving DIFFERENT
	// HARNESSES share nothing, since harnesses disagree on which files are
	// credentials and which are conversation memory; so an entry can move to
	// another harness in a live revision and moving back finds the old login.
	StateDir string `yaml:"state_dir,omitempty" json:"state_dir,omitempty" desc:"Credential + per-seat home directory, each harness under <state_dir>/<harness>. Entries sharing one and a harness share a login."`

	// TimeoutSeconds caps one STRETCH of the harness's own work, wall clock:
	// from its launch, or the end of the tool call it last waited on, to the
	// start of its next tool call or its exit. That covers process launch (a
	// Node runtime costs seconds), model answers and the harness's own retry
	// and backoff. The engine's tools are not on the clock, so a run is never
	// cut for its length, only for a stretch with no progress the engine could
	// see. Default: [defaultHarnessTimeoutSeconds].
	//
	// THE PHASES ON THIS HOST ONLY. An executor run is a detached run in a box
	// and is never stopped on a clock: its rounds are capped only by
	// providers.sandbox.default_max_turns and role.sandbox.max_turns.
	TimeoutSeconds float64 `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty" js:"min=0" desc:"Wall-clock cap on one stretch of a harness run on this host with no tool call in flight (default 600). Executor runs are not on this clock."`

	// MaxConcurrent is how many harness processes this provider runs at once on
	// the engine's own host. A model call made from INSIDE one of its tool calls
	// on this same entry (a delegated worker, an auxiliary rewrite) runs in the
	// caller's slot, which a run holding the last slot could never free.
	//
	// Each is a full Node or Rust runtime at roughly 200-400 MB resident; 4
	// keeps peak near 1.5 GB, fitting the smallest realistic host while
	// overlapping long I/O waits. Subscription plans throttle concurrency below
	// what an API key allows, so more mostly buys rate-limit errors.
	//
	// THE PHASES ON THIS HOST ONLY, like timeout_seconds: an executor run is a
	// process in its own box, and each seat runs at most one.
	MaxConcurrent int `yaml:"max_concurrent,omitempty" json:"max_concurrent,omitempty" js:"min=0" desc:"Concurrent harness processes on this host; each costs 200-400 MB. Executor runs are not counted: each seat runs at most one."`

	// Env is extra environment for every invocation, ${VAR}-resolved. The
	// child gets an ALLOWLISTED environment, not the engine's, so anything the
	// harness needs beyond PATH, locale, TLS and proxy is declared here (for
	// OpenCode, the key of the provider its model names).
	//
	// EVERY INVOCATION INCLUDES A SEAT'S EXECUTOR RUN, in whichever cell its
	// box is: the whole map is exported into the box with the entry's sign-in,
	// beneath the seat's role.sandbox.env, which wins a name. A seat that signs
	// Claude Code in itself replaces the entry's sign-in whole, its
	// credential-named variables and cloud toggles included, and keeps the
	// rest.
	Env map[string]string `secret:"true" yaml:"env,omitempty" json:"env,omitempty" desc:"Extra environment for the harness, on this host and in every executor run's box (beneath the seat's role.sandbox.env)."`

	Auth HarnessAuth `yaml:"auth,omitempty" json:"auth"`

	// Overrides replaces fields of the built-in profile.
	//
	// Harness flags drift between releases, and a vendor renaming
	// --output-format must be a config edit rather than a Crewlet release,
	// so every profile field is replaceable. Lists replace wholesale —
	// position matters in an argv.
	Overrides map[string]any `yaml:"overrides,omitempty" json:"overrides,omitempty" desc:"Field-wise overrides of the built-in harness profile."`
}

// Harness is which harness a providers.llm entry drives.
//
// A NAMED TYPE over a closed set, like every other closed set in this
// package. Two things followed from a bare string. The generated schema
// emitted `"type":"string"` where a closed set emits an enum, so an editor
// blessed `harness: claud-code` — which [names]'s own doc calls worse than no
// schema — and with no `js:` tag the field could not join schema_test.go's
// enum table, so nothing noticed.
type Harness string

// The harnesses the engine knows how to drive. `custom` ships no defaults
// at all — supply everything under Overrides.
const (
	HarnessClaudeCode  Harness = "claude-code"
	HarnessCodex       Harness = "codex"
	HarnessGeminiCLI   Harness = "gemini-cli"
	HarnessQwenCode    Harness = "qwen-code"
	HarnessOpenCode    Harness = "opencode"
	HarnessCursorAgent Harness = "cursor-agent"
	HarnessCopilot     Harness = "copilot"
	HarnessGrok        Harness = "grok"
	HarnessMuseCode    Harness = "muse-code"
	HarnessKimiCode    Harness = "kimi-code"
	HarnessHermes      Harness = "hermes"
	HarnessPi          Harness = "pi"
	HarnessCustom      Harness = "custom"
)

// Harnesses is the closed set.
var Harnesses = []Harness{
	HarnessClaudeCode, HarnessCodex, HarnessGeminiCLI, HarnessQwenCode,
	HarnessOpenCode, HarnessCursorAgent, HarnessCopilot, HarnessGrok, HarnessMuseCode,
	HarnessKimiCode, HarnessHermes, HarnessPi, HarnessCustom,
}

// Valid reports whether n is one the engine knows.
//
// Empty is NOT valid here — unlike an optional enum elsewhere — because the
// caller checks emptiness first: an absent harness takes the default rather
// than meaning "any".
func (n Harness) Valid() bool { return slices.Contains(Harnesses, n) }

// ExecutorHarnesses is the closed set of harnesses that can run a seat's
// EXECUTOR: those with an in-box runner (internal/sandbox/executor). Every
// other harness drives text phases only. A launch walks the seat's
// executor chain for its first member here that is not sitting out, so a chain
// with none can never run the turn, and config refuses it
// ([Company.validateExecutors]).
//
// DERIVED FROM THE BUILT-IN PROFILES' executor blocks, in their order
// ([harnessprofile.ExecutorRunners]): config cannot import the runners, and
// the executor holds every listed name registered and every runner listed.
// Read at first use, never at init: a push's hook links this package and pays
// for nothing it does not read. The caller must not modify the slice.
var ExecutorHarnesses = sync.OnceValue(func() []Harness {
	names := harnessprofile.ExecutorRunners()
	out := make([]Harness, len(names))
	for i, name := range names {
		out[i] = Harness(name)
	}
	return out
})

// HarnessAuthMode is how a providers.llm entry's harness authenticates.
type HarnessAuthMode string

const (
	// AuthSubscription uses the operator's own harness login — a token in the
	// secret store, or the credential files `crewlet llm login` wrote.
	// The default.
	AuthSubscription HarnessAuthMode = "subscription"
	// AuthAPIKey puts the entry's API key into the harness's key variable (its
	// profile's api_key_env), billing the metered account: auth.api_key,
	// or that variable itself looked up through the secret store and then
	// the environment when the entry names none. Refused on a harness whose
	// profile names no such variable, where the key would reach nothing.
	AuthAPIKey HarnessAuthMode = "api-key"
	// AuthInheritEnv forwards whichever of the two variables the engine's
	// own environment has — an escape hatch for an unusual harness, and the
	// only mode that lets a host credential reach the child.
	AuthInheritEnv HarnessAuthMode = "inherit-env"
	// AuthEnv signs the harness in with the entry's own env: a cloud provider's
	// switch and credentials (CLAUDE_CODE_USE_BEDROCK with the AWS pair), or a
	// gateway's address and token (ANTHROPIC_BASE_URL with
	// ANTHROPIC_AUTH_TOKEN). The harness is handed neither the profile's token
	// variable nor its key variable, and runs in a home no login is seeded into.
	//
	// A MODE OF ITS OWN, not an env the default mode reads: under subscription
	// the plan token resolves for every entry and would reach a Bedrock
	// entry's harness, where the cloud switch outranks it, and the entry read
	// as signed in to nothing. Admitted only where the env does sign the
	// harness in ([LLMProvider.validateCredentials]).
	AuthEnv HarnessAuthMode = "env"
)

// HarnessAuthModes is the closed set.
//
// The default is deliberately NOT inherit-env: a subscription entry that
// silently picked up a stray ANTHROPIC_API_KEY would bill the metered
// account while the operator believed they were on a flat-rate plan.
var HarnessAuthModes = []HarnessAuthMode{AuthSubscription, AuthAPIKey, AuthInheritEnv, AuthEnv}

// HarnessAuth is how a providers.llm entry's harness authenticates. The login
// itself is performed by `crewlet llm login`, never by the engine.
type HarnessAuth struct {
	Mode HarnessAuthMode `yaml:"mode,omitempty" json:"mode,omitempty" js:"enum=subscription|api-key|inherit-env|env" desc:"subscription (default), api-key, inherit-env, or env (the entry's env signs the harness in: a cloud provider or a gateway)."`

	// Token is a ${VAR} reference to a long-lived subscription token.
	// Empty falls back to the profile's own token variable. This is the
	// best headless path where a harness offers one: no credential files to
	// sync, no refresh rotation, and it survives an ephemeral container.
	// Read only under auth.mode subscription and only by a harness whose
	// profile names a token_env; set anywhere else it is refused, because
	// it would reach nothing.
	Token string `secret:"true" yaml:"token,omitempty" json:"token,omitempty" desc:"${VAR} holding a long-lived subscription token."`

	// APIKey is a ${VAR} reference to a metered API key, put into the
	// harness's key variable (its profile's api_key_env) under auth.mode
	// api-key. Empty reads that variable itself through the secret store
	// and then the environment — ANTHROPIC_API_KEY for Claude Code,
	// CODEX_API_KEY for Codex — so a key already exported, or set with
	// `crewlet secrets set`, works with no YAML change.
	//
	// ONE key, never a list: a harness holds one sign-in and rotates nothing,
	// so a second key belongs on an entry of its own in the seat's
	// fallback chain. Set under any other mode it is refused, because it
	// would reach nothing.
	APIKey string `secret:"true" yaml:"api_key,omitempty" json:"api_key,omitempty" desc:"${VAR} holding an API key, read under auth.mode api-key. Empty reads the harness's own key variable."`

	// CredentialBundle is a ${VAR} reference to a bundle exported by
	// `crewlet llm export`: the harness's login as one blob. It is restored
	// whenever the engine builds a company (boot and every apply) into a
	// credential directory holding no login, or over one installed before the
	// bundle's (the newest wins, by the install record), so a fresh container
	// comes up logged in and a re-login reaches every node. It is withheld
	// after `crewlet llm logout` until a login is written again (`crewlet llm
	// login`, `-from-host`, `crewlet llm import`; a headless token does not end
	// it). It holds ONE HARNESS's files and names that harness: if the entry's
	// harness changes, the new one refuses it until a bundle exported from it
	// replaces it.
	CredentialBundle string `secret:"true" yaml:"credential_bundle,omitempty" json:"credential_bundle,omitempty" desc:"${VAR} holding a bundle exported by crewlet llm export."`
}

// Default entry knobs. See the field comments for where the numbers come
// from.
const (
	// defaultHarnessTimeoutSeconds is ten minutes. A stretch is at least one
	// model answer at the harness's own effort, and on the current Claude models
	// that is adaptive thinking against a 128K-token output cap: ten minutes
	// is Anthropic's documented ceiling for such an answer in one call, and
	// two minutes was measured cutting off exactly the calls doing the most
	// work when the engine still made them itself. A harness adds its process
	// launch to the first stretch. A hung child holds its seat for no longer
	// than this, and a timeout is retryable, so the seat's fallback chain
	// moves on.
	defaultHarnessTimeoutSeconds = 600.0
	defaultHarnessMaxConcurrent  = 4
	defaultHarness               = HarnessClaudeCode
)

// HarnessName is the harness to drive, applying the default.
//
// A STRING, not a [Harness]: the value crosses into internal/providers,
// which does not import config, and the profile registry it keys is that
// package's own. The type exists to bound what config ACCEPTS, which is a
// question answered before this is called.
func (l *LLMProvider) HarnessName() string {
	if l.Harness == "" {
		return string(defaultHarness)
	}
	return string(l.Harness)
}

// runsExecutor reports whether this entry's harness can run a seat's executor:
// whether it has an in-box runner ([ExecutorHarnesses]).
func (l *LLMProvider) runsExecutor() bool {
	return slices.Contains(ExecutorHarnesses(), Harness(l.HarnessName()))
}

// Timeout is the cap on one stretch of the harness's own work, applying the
// default.
func (l *LLMProvider) Timeout() float64 {
	if l.TimeoutSeconds <= 0 {
		return defaultHarnessTimeoutSeconds
	}
	return l.TimeoutSeconds
}

// Concurrency is the process cap, applying the default.
func (l *LLMProvider) Concurrency() int {
	if l.MaxConcurrent <= 0 {
		return defaultHarnessMaxConcurrent
	}
	return l.MaxConcurrent
}

// HarnessHomeEnv is the environment variable naming the root under which every
// providers.llm entry keeps its credential directory and per-seat homes.
const HarnessHomeEnv = "CREWLET_LLM_HARNESS_HOME"

// defaultHarnessHome is the per-user fallback root, under the operator's home.
const defaultHarnessHome = ".crewlet/llm-harness"

// ResolvedStateDir is the directory under which this entry keeps its login and
// its per-seat homes — in a subdirectory named for its harness, which the backend
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
	if root := os.Getenv(HarnessHomeEnv); root != "" {
		return filepath.Join(filepath.Clean(root), key), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf(
			"providers.llm.%s.state_dir: no directory given and no home directory to "+
				"derive one under (%w): set state_dir, or %s", key, err, HarnessHomeEnv)
	}
	return filepath.Join(home, defaultHarnessHome, key), nil
}

// ResolvedAuthMode is the entry's auth.mode, applying the default.
func (l *LLMProvider) ResolvedAuthMode() HarnessAuthMode {
	if l.Auth.Mode == "" {
		return AuthSubscription
	}
	return l.Auth.Mode
}

// CredentialSource is one credential a providers.llm entry signs its harness in
// with: what the entry's own field holds, and the variable read in its place
// when the field holds nothing.
type CredentialSource struct {
	// Value is the field as written: a ${VAR}, a literal, or empty.
	Value string
	// Fallback is the variable read when Value is empty, through the secret
	// store and then the environment. Empty where the entry's auth mode does
	// not read this credential, or its harness names no such variable.
	Fallback string
}

// CredentialSources are the three credentials an entry can sign in with.
type CredentialSources struct {
	Token, APIKey, Bundle CredentialSource
}

// CredentialSources says where each of this entry's credentials is read from:
// the field the entry writes or, where it writes none, a conventional variable
// (the harness's token or key variable, [harnessprofile.BundleVarName]).
//
// A CONVENTION, so `crewlet llm login -capture-token`, `crewlet llm export
// -secret-store` and an exported ANTHROPIC_API_KEY sign an entry in with no
// YAML change. ONE RULE, two readers: the engine's sign-in and
// [Company.VariablesRead], so the Secrets screen never calls a live credential
// "read by nothing".
//
// PER AUTH MODE: a token only under subscription, an API key only under
// api-key, the bundle under every mode; inherit-env and env read nothing here.
func (l *LLMProvider) CredentialSources(key string) CredentialSources {
	profile, err := harnessprofile.Load(l.HarnessName(), l.Overrides)
	if err != nil {
		// A profile that does not load is refused at overrides by
		// validation, and names no variable to fall back to.
		profile = harnessprofile.Profile{}
	}
	sources := CredentialSources{
		Token:  CredentialSource{Value: l.Auth.Token},
		APIKey: CredentialSource{Value: l.Auth.APIKey},
		Bundle: CredentialSource{Value: l.Auth.CredentialBundle, Fallback: harnessprofile.BundleVarName(key)},
	}
	switch l.ResolvedAuthMode() {
	case AuthSubscription:
		sources.Token.Fallback = profile.TokenEnv
	case AuthAPIKey:
		// NOT a variable other vendors' keys go in too: the entry names
		// its key ([harnessprofile.Profile.APIKeyEnvShared]).
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
			"name the model this entry's harness runs, in that harness's own grammar "+
				"(e.g. sonnet for claude-code, anthropic/claude-sonnet-5 for opencode)")
	}
	if l.Harness != "" && !l.Harness.Valid() {
		p.add(at(path, "harness"), ErrUnknownValue, "%q (want %s)",
			l.Harness, words.Or(Harnesses))
	}
	// Left unbounded, a timeout of 0 or a negative one passes validation
	// and reaches a process supervisor that reads it as "expire
	// immediately" — every call failing instantly, with nothing in the
	// config looking wrong.
	if l.TimeoutSeconds < 0 {
		p.add(at(path, "timeout_seconds"), ErrOutOfRange,
			"must be 0 (the %v s default) or positive, got %v",
			defaultHarnessTimeoutSeconds, l.TimeoutSeconds)
	}
	if l.MaxConcurrent < 0 {
		p.add(at(path, "max_concurrent"), ErrOutOfRange,
			"must be 0 (the default of %d) or positive, got %d",
			defaultHarnessMaxConcurrent, l.MaxConcurrent)
	}
	if l.Auth.Mode != "" && !slices.Contains(HarnessAuthModes, l.Auth.Mode) {
		p.add(at(path, "auth.mode"), ErrUnknownValue, "%q (want %s)",
			l.Auth.Mode, words.Or(HarnessAuthModes))
	}
	// THE SPELLING, and that the entry can be placed at all. Whether the
	// cell is configured is a question about providers.sandbox, and whether
	// a seat's executor reaches this entry is one about the seats; both are
	// asked by the company ([Company.validateSandboxPlacement]). Whether its
	// harness launches a run anywhere is the entry's own: a harness with no in-box
	// runner serves text phases only, so a launch passes over it in every
	// chain it is in, and a run_in on it is a placement nothing ever reads —
	// refused as a value that means nothing where it is written, rather than
	// admitted and silently ignored, the cell it names unchecked.
	switch {
	case l.RunIn == "":
	case !slices.Contains(Placements, l.RunIn):
		p.add(at(path, "run_in"), ErrUnknownValue, "%q (want %s)",
			l.RunIn, words.Or(Placements))
	case (l.Harness == "" || l.Harness.Valid()) && !l.runsExecutor():
		p.add(at(path, "run_in"), ErrConflict,
			"%s has no in-box runner, so no run is ever launched on this entry and nothing "+
				"reads where it would be placed: only %s launch an executor run. Remove the "+
				"field, or move the entry to one of those harnesses",
			l.HarnessName(), words.And(ExecutorHarnesses()))
	}
	p.wrap(l.validateProfile(path))
	return p.err()
}

// validateProfile holds an entry to the profile it drives (the built-in
// profile with overrides merged, loaded by [harnessprofile.Load]) and its
// model to that profile's model flag.
//
// HERE, NOT ONLY WHERE THE BACKEND IS BUILT: building happens in `crewlet
// validate` and every node's apply, but no API write builds a provider, so
// judged at the build alone an override typo was admitted, activated, then
// refused by every node.
//
// RUNNABLE: the profile does not load, or no model_args element carries
// {model}, or the entry names no model. Credentials are the admission half,
// [LLMProvider.validateCredentials].
func (l *LLMProvider) validateProfile(path Path) error {
	if l.Harness != "" && !l.Harness.Valid() {
		return nil // reported at harness; there is no profile to judge
	}
	var p problems
	if stance, _ := l.Overrides["web_tools"].(string); stance == string(harnessprofile.WebToolsSearch) {
		p.add(at(path, "overrides.web_tools"), ErrConflict, "search is the stance of Ask "+
			"Crewlet's search run, which the engine sets itself; an entry is allowed or denied")
		return p.err()
	}
	profile, err := harnessprofile.Load(l.HarnessName(), l.Overrides)
	switch {
	case errors.Is(err, harnessprofile.ErrExecutorOverride):
		p.add(at(path, "overrides.executor"), ErrConflict, "%v", err)
		return p.err()
	case errors.Is(err, harnessprofile.ErrOverrides):
		p.add(at(path, "overrides"), ErrShape, "%v", err)
		return p.err()
	case err != nil:
		p.add(at(path, "overrides"), ErrConflict, "%v", err)
		return p.err()
	}
	if strings.TrimSpace(l.Model) != "" && !profile.TakesModel() {
		p.add(at(path, "overrides.model_args"), ErrMissing,
			"the %q profile's model_args carry no {model}, so the model %q this entry "+
				"names would never reach the harness, and every entry names one: declare "+
				`the flag with the placeholder, e.g. overrides.model_args: ["--model", "{model}"]`,
			l.HarnessName(), l.Model)
	}
	return p.err()
}

// ProviderKeyPattern is the grammar of a providers.llm key: a lowercase
// letter or digit, then up to 62 more of those, `.`, `_` and `-`.
//
// A GRAMMAR, because a key names a directory (the default state_dir joins it
// under the harness home, [LLMProvider.ResolvedStateDir]: a `/` nested one
// login in another, `..` left the home) and a variable
// ([harnessprofile.BundleVarName] upper-cases it and maps other characters to
// `_`). Lowercase only, so the document, a URL path and a Kubernetes resource
// name are the same string.
// Compiled at first use, not at init: Gitlet's push hook is this binary and
// pays every package's init (cmd/crewlet's hookinit_test.go).
var ProviderKeyPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
})

// validateLLMKeys holds every providers.llm key to [ProviderKeyPattern], and
// no two keys to one credential bundle variable.
//
// ONE BUNDLE PER ENTRY. [harnessprofile.BundleVarName] maps `.`, `_` and `-`
// alike, so `gpt-5`, `gpt_5` and `gpt.5` all read
// CREWLET_LLM_CLI_GPT_5_CREDENTIALS and would restore or overwrite each
// other's login. Each key after the first, sorted, is reported naming the one
// it collides with.
//
// ADMISSION rather than runnable: such an entry still runs, and a refusal at
// apply would take a node off the fleet's epoch over an older revision. (An
// empty key is the runnable rule [Providers.validate] holds.)
func (c *Company) validateLLMKeys() error {
	var p problems
	bundles := make(map[string]string, len(c.Providers.LLM))
	for _, key := range sortedKeys(c.Providers.LLM) {
		if strings.TrimSpace(key) == "" {
			continue
		}
		path := entryPath(key)
		if !ProviderKeyPattern().MatchString(key) {
			p.add(path, ErrShape,
				"%q is not a provider key: a key is lowercase letters, digits, '.', '_' and "+
					"'-', starting with a letter or a digit, at most 63 characters (%s). It "+
					"names the entry's state directory and its credential bundle variable, "+
					"so rename it, and every seat's llm that names it, e.g. %q",
				key, ProviderKeyPattern(), suggestProviderKey(key))
			continue
		}
		bundle := harnessprofile.BundleVarName(key)
		if first, ok := bundles[bundle]; ok {
			p.add(path, ErrConflict,
				"%q and %q read one credential bundle, %s, since the variable maps '.', '_' and "+
					"'-' alike: the two entries would restore and export one login. Rename one "+
					"so the two differ in a letter or a digit", key, first, bundle)
			continue
		}
		bundles[bundle] = key
	}
	return p.err()
}

// suggestProviderKey is key in [ProviderKeyPattern]'s grammar, for the
// refusal's example: lowercased, every other character a `-`, cut to length.
func suggestProviderKey(key string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.TrimLeft(b.String(), "._-")
	if len(out) > 63 {
		out = out[:63]
	}
	if out == "" {
		return "default"
	}
	return out
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

// validateLLMModels is [LLMProvider.validateModel] and
// [LLMProvider.validateEffort] over every entry of the company, in key order.
func (c *Company) validateLLMModels() error {
	var p problems
	for _, key := range sortedKeys(c.Providers.LLM) {
		spec := c.Providers.LLM[key]
		p.wrap(spec.validateModel(entryPath(key)))
		p.wrap(spec.validateEffort(entryPath(key)))
	}
	return p.err()
}

// validateEffort refuses an effort the entry's harness would drop: a level its
// merged profile does not take on the model, or any on a harness with none.
// ADMISSION, like validateModel: the levels are a vendor fact a newer peer may know.
func (l *LLMProvider) validateEffort(path Path) error {
	if l.Effort == "" || (l.Harness != "" && !l.Harness.Valid()) {
		return nil
	}
	profile, err := harnessprofile.Load(l.HarnessName(), l.Overrides)
	if err != nil {
		//nolint:nilerr // validateProfile reports a profile that does not load.
		return nil
	}
	model := profile.ModelArgument(strings.TrimSpace(l.Model))
	if envref.Has(l.Model) {
		model = ""
	}
	problem := profile.EffortProblem("the "+l.HarnessName()+" harness", model, l.Effort)
	if problem == "" {
		return nil
	}
	var p problems
	switch {
	case !profile.TakesEffort() && l.HarnessName() == string(HarnessCustom):
		p.add(at(path, "effort"), ErrConflict, "%s: declare overrides.effort with args carrying "+
			"{effort} and the levels the harness takes, or remove the field", problem)
	case !profile.TakesEffort():
		p.add(at(path, "effort"), ErrConflict, "%s. Remove the field: the harness runs at its "+
			"own default", problem)
	default:
		p.add(at(path, "effort"), ErrUnknownValue, "%s", problem)
	}
	return p.err()
}

// validateModel holds an entry's model to the grammar its harness reads it in,
// where the merged profile declares one
// ([harnessprofile.Profile.ModelNamesProvider]): OpenCode splits its model flag
// at the first slash into provider and model, so a bare `claude-sonnet-5`
// names a provider and no model and every call fails inside the harness.
//
// JUDGED ON THE VALUE THE HARNESS IS HANDED, the model rendered through
// model_args, so an override putting the provider in the flag
// (["--model", "anthropic/{model}"]) admits a bare model. A `${VAR}` model is
// not judged: a write cannot see its value.
//
// ADMISSION rather than runnable: the grammar moves between vendor releases.
func (l *LLMProvider) validateModel(path Path) error {
	if (l.Harness != "" && !l.Harness.Valid()) || envref.Has(l.Model) || strings.TrimSpace(l.Model) == "" {
		return nil
	}
	profile, err := harnessprofile.Load(l.HarnessName(), l.Overrides)
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
	// THE VENDOR'S OWN WORDS ONLY FOR THE HARNESS THEY ARE MEASURED ON. Any
	// profile can declare the grammar through overrides, and an operator
	// pointed at `opencode models` for a hermes entry is sent to a tool
	// that entry does not run.
	failure, listing := "with the harness's own model-not-found error", ""
	if l.HarnessName() == string(HarnessOpenCode) {
		failure = `with OpenCode's "Invalid model reference"`
		listing = " (`opencode models --standalone` lists them)"
	}
	p.add(at(path, "model"), ErrConflict,
		"the %s harness reads its model as <provider>/<model> and splits it at the first "+
			"slash, so %q %s: every call this entry makes, an executor run included, "+
			"would fail inside the harness %s. Name the provider as the harness spells it%s, e.g. "+
			"anthropic/claude-sonnet-5 or openrouter/anthropic/claude-sonnet-5, with that "+
			"provider's key in env (e.g. ANTHROPIC_API_KEY, OPENROUTER_API_KEY) or a "+
			"login of the harness's own",
		l.HarnessName(), value, what, failure, listing)
	return p.err()
}

// validateCredentials holds an entry's credentials to the variables its
// merged profile names, those variables to the reference grammar, and the
// profile to keeping every credential behind auth
// ([harnessprofile.Profile.ValidateCredentials]).
//
// The backend's applyAuth (internal/providers/llm/harness) places a credential
// only where the profile names a variable for it and silently drops it
// otherwise, so an api-key entry on a harness with no api_key_env ran signed
// in to nothing. Such shapes are refused, naming the route the harness reads.
// ADMISSION rather than runnable: the entry still runs, only the misplaced
// credential goes unused; an apply only warns, since refusing would take this
// node off the fleet's epoch over a revision a newer peer admitted.
func (l *LLMProvider) validateCredentials(path Path) error {
	if l.Harness != "" && !l.Harness.Valid() {
		return nil
	}
	profile, err := harnessprofile.Load(l.HarnessName(), l.Overrides)
	if err != nil {
		//nolint:nilerr // Deliberate: a profile that does not load is a
		// runnable fault validateProfile already reports at overrides,
		// and there is no profile to judge credentials by.
		return nil
	}
	var p problems
	harness := l.HarnessName()
	if err := profile.ValidateCredentials(harness); err != nil {
		p.add(at(path, "overrides"), ErrConflict, "%v", err)
	}

	// THE TWO VARIABLES A CREDENTIAL IS HANDED OVER IN ARE NAMES OF THE
	// REFERENCE GRAMMAR, because nothing that keeps a credential can hold
	// any other: the secret store refuses the name, so `crewlet llm login
	// -capture-token` cannot write the token there and a `${VAR}` cannot
	// spell it; no shell exports one for auth.mode inherit-env to forward;
	// and a name carrying `=` sets a different variable in the harness's
	// environment. Only overrides can name one — every shipped profile's
	// names are in the grammar — and the harness still starts with it, signed in
	// by whatever it does read, so this is an admission rule like the rest.
	// Here rather than in the profile's own rules because the grammar is
	// envref's, which that leaf does not import.
	for _, variable := range []struct{ field, name string }{
		{"token_env", profile.TokenEnv}, {"api_key_env", profile.APIKeyEnv},
	} {
		if variable.name != "" && !envref.ValidName(variable.name) {
			p.add(at(path, "overrides."+variable.field), ErrShape,
				"%q is not an environment variable name (letters, digits and "+
					"underscores, not starting with a digit): the harness is handed its "+
					"credential under it, and no secret store, ${VAR} or `crewlet llm "+
					"login` can hold a name outside that grammar, nor a shell export "+
					"one for auth.mode inherit-env to forward. Name the variable your "+
					"build of the harness reads", variable.name)
		}
	}

	// The mode-dependent rules each state what a mode does with a
	// credential, and an unknown mode does nothing this file can describe:
	// judged as one, a typo drew refusals naming a mode that does not exist.
	// The fault is reported once, at auth.mode.
	mode := l.ResolvedAuthMode()
	if !slices.Contains(HarnessAuthModes, mode) {
		return p.err()
	}

	if l.Auth.APIKey != "" && mode != AuthAPIKey {
		p.add(at(path, "auth.api_key"), ErrConflict,
			"an API key is read only under auth.mode api-key, which puts it in the "+
				"harness's key variable; under %s it reaches nothing. Set auth.mode: "+
				"api-key, or drop auth.api_key", mode)
	}

	if mode == AuthAPIKey {
		switch {
		case profile.APIKeyEnv == "" && profile.EnvSignIn:
			p.add(at(path, "auth.mode"), ErrConflict,
				"api-key puts the entry's key in the harness's key variable, and the "+
					"%q profile names none (api_key_env): this harness reads each model "+
					"provider's key from that provider's own variable. Leave auth.mode "+
					"at subscription, drop auth.api_key, and set the key in env under "+
					"the variable of the provider its model names, as a ${VAR}, e.g. "+
					`OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"`, harness)
		case profile.APIKeyEnv == "":
			p.add(at(path, "auth.mode"), ErrConflict,
				"api-key puts the entry's key in the harness's key variable, and the "+
					"%q profile names none (api_key_env), so the key would reach "+
					"nothing: this harness reads a key only from %s, so put it there "+
					"(`crewlet llm login`, or a credential bundle), or, if your build of "+
					"the harness does read a key variable, name it with "+
					"overrides.api_key_env",
				harness, profile.LoginLocation())
		case profile.APIKeyEnvShared && strings.TrimSpace(l.Auth.APIKey) == "":
			// Refused rather than left to fall back: the variable the
			// harness reads holds other vendors' keys too, and the one the
			// engine would find there is the wrong one more often than
			// not — sent to this harness's endpoint all the same.
			p.add(at(path, "auth.api_key"), ErrMissing,
				"name this entry's key as auth.api_key: the %q harness reads its key from "+
					"%s, a variable other vendors' keys are kept under too, so the "+
					"engine does not read it from its own environment or secret store "+
					`in this entry's place (for example api_key: "${DASHSCOPE_API_KEY}")`,
				harness, profile.APIKeyEnv)
		}
	}

	// SUBSCRIPTION READS A LOGIN THE HARNESS KEEPS OR A HEADLESS TOKEN. A harness that
	// keeps neither and signs in with a key alone would run signed in to
	// nothing under it: the mode removes the key variable from the harness's
	// environment, and every call is refused for want of a key. Qwen Code is
	// that harness — both measured builds refuse the OAuth login it once kept.
	if mode == AuthSubscription && !profile.KeepsLogin() && profile.TokenEnv == "" &&
		!profile.EnvSignIn && profile.APIKeyEnv != "" {
		p.add(at(path, "auth.mode"), ErrConflict,
			"subscription signs the harness in with a login it keeps or a headless token, and "+
				"the %q profile names neither (no credential_paths, credential_store or "+
				"token_env), so the entry would sign in to nothing. This harness signs in with "+
				"its key, so set auth.mode: api-key and give the key as auth.api_key (the "+
				"harness reads it from %s)", harness, profile.APIKeyEnv)
	}

	if l.Auth.Token != "" {
		switch {
		case profile.TokenEnv == "":
			p.add(at(path, "auth.token"), ErrConflict,
				"the %q profile names no token_env (this harness mints no headless "+
					"token), so the token would reach nothing: remove it, or name the "+
					"variable your build of the harness reads with overrides.token_env", harness)
		case mode != AuthSubscription:
			p.add(at(path, "auth.token"), ErrConflict,
				"a token is read only under auth.mode subscription: %s removes %s "+
					"from the harness's environment or forwards the engine's own. Remove "+
					"it, or set auth.mode: subscription", mode, profile.TokenEnv)
		}
	}

	// NOR FROM THE ENGINE'S ENVIRONMENT BY MODE. inherit-env forwards the key
	// variable as the engine's process holds it, and a shared one holds
	// whichever vendor's key was exported under it — the embeddings key, on a
	// host that runs any — sent to this harness's endpoint all the same.
	if mode == AuthInheritEnv && profile.APIKeyEnvShared {
		p.add(at(path, "auth.mode"), ErrConflict,
			"inherit-env forwards %s from the engine's own environment, and the %q harness's key "+
				"variable is one other vendors' keys are kept under too, so the key it forwarded "+
				"would be whichever was exported there: set auth.mode: api-key and name this "+
				`entry's key as auth.api_key, e.g. api_key: "${DASHSCOPE_API_KEY}"`,
			profile.APIKeyEnv, harness)
	}
	if mode == AuthInheritEnv && profile.APIKeyModelEnv != "" {
		p.add(at(path, "auth.mode"), ErrConflict,
			"inherit-env forwards %s from the engine's own environment, and the %q harness "+
				"reads that key only beside its model in %s, which inherit-env does not set: "+
				"set auth.mode: api-key and give the key as auth.api_key",
			profile.APIKeyEnv, harness, profile.APIKeyModelEnv)
	}
	if mode == AuthInheritEnv && profile.TokenEnv == "" && profile.APIKeyEnv == "" {
		remedy := "sign the harness in with `crewlet llm login`"
		if profile.EnvSignIn {
			remedy = "set the key in env under the variable of the provider " +
				"its model names, as a ${VAR}"
		}
		p.add(at(path, "auth.mode"), ErrConflict,
			"inherit-env forwards the profile's token_env and api_key_env from the "+
				"engine's own environment, and the %q profile names neither, so "+
				"nothing would be forwarded: leave auth.mode at subscription and %s",
			harness, remedy)
	}

	p.wrap(l.validateEnvSignIn(path, profile, mode))

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
				"%s is this harness's token variable, which auth owns: whether a value "+
					"here reaches the harness depends on auth.mode and on whether the secret "+
					"store or the engine's environment holds a token of its own. Give "+
					"the token as auth.token", name)
		case harnessprofile.ZoneVariable:
			// The engine pins the zone of every harness it starts, and reads a
			// reset the harness names with no zone in it; a value here would be
			// replaced without a word, or, honoured, would have the engine
			// read the harness's reset in the wrong zone.
			p.add(entry(at(path, "env"), name), ErrConflict,
				"%s is the zone the engine hands every harness it starts (its own TZ, else "+
					"UTC), and reads the harness's limit resets in; it is not set per entry. "+
					"Drop it, and set TZ in the engine's own environment to move every "+
					"harness's zone", name)
		case profile.APIKeyModelEnv:
			p.add(entry(at(path, "env"), name), ErrConflict,
				"%s is where this harness reads its model beside its key, which the engine "+
					"sets from the entry: set here it would swap the entry's model and provider and "+
					"sign the harness in uncounted. Give the key as auth.api_key with auth.mode "+
					"api-key, and the model as model", name)
		case profile.APIKeyEnv:
			p.add(entry(at(path, "env"), name), ErrConflict,
				"%s is this harness's key variable, which auth owns: whether a value "+
					"here reaches the harness depends on auth.mode, and on whether auth.api_key "+
					"resolves or the engine's environment holds a key of its own. Give "+
					"the key as auth.api_key with auth.mode api-key", name)
		default:
			if subscriptionEnvName(profile, mode, name) {
				// Set with the mode, over env: a value here would be
				// replaced on every call without a word.
				p.add(entry(at(path, "env"), name), ErrConflict,
					"%s is set by the %q profile under auth.mode subscription, to %q, to "+
						"choose the login the seat's home holds; a value here is replaced on "+
						"every call, so drop it. A different value is set with "+
						"overrides.subscription_env", name, l.Harness, profile.SubscriptionEnv[name])
				continue
			}
			// env is layered OVER the variables that keep each seat's harness
			// inside its own home and its call's working directory, so a
			// value here would replace them for every seat on the entry —
			// the engine user's real dotfiles, a configuration directory the
			// seat's system prompt is never written to, or a vendor switch
			// turned back off (OpenCode's project walk, which hands every
			// seat whatever configuration sits above the state directory) —
			// and nothing would say so.
			if layer, ok := profile.IsolationLayerOf(name); ok {
				p.add(entry(at(path, "env"), name), ErrConflict,
					"%s is a variable the engine sets for every call of this harness to keep "+
						"it inside its seat's own home and its call's working directory "+
						"(%s); a value here would replace that isolation for every seat, so "+
						"drop it: %s", name, strings.Join(isolation, ", "),
					isolationRemedy(name, layer, profile))
			}
		}
	}
	// A ROUTE VARIABLE'S LITERAL VALUE IS A ROUTE, or the entry's identity
	// would take it whole (harnessprofile.Profile.RouteValue); a ${VAR} is
	// held to it once resolved, when the entry is built (harness.New). The
	// error never holds the value, which may hold a credential.
	for _, name := range profile.RouteEnv() {
		value := strings.TrimSpace(l.Env[name])
		if value == "" || envref.Has(value) {
			continue
		}
		if _, err := profile.RouteValue(value); err != nil {
			p.add(entry(at(path, "env"), name), ErrShape, "%s says where the %q harness sends its calls: %v",
				name, l.Harness, err)
		}
	}
	return p.err()
}

// validateEnvSignIn holds an entry to the rule that its env is its sign-in
// exactly when auth.mode says so ([AuthEnv]).
//
// UNDER env, THE ENV MUST SIGN THE HARNESS IN: the mode hands it no token or
// key and seeds no login, so an env setting none of the names the profile
// signs in by ([harnessprofile.Profile.SignInEnv], [harnessprofile.Profile.EnvSignIn])
// signs in to nothing.
//
// UNDER ANY OTHER MODE, A NAME THAT SIGNS THE HARNESS IN IS REFUSED in env:
// the harness ranks it against the mode's own credential by its own rules
// (Claude Code routes a cloud switch away from the vendor's API whatever
// token it holds), so one goes unused. ADMISSION, like validateCredentials.
func (l *LLMProvider) validateEnvSignIn(path Path, profile harnessprofile.Profile, mode HarnessAuthMode) error {
	var p problems
	harness := l.HarnessName()
	if mode != AuthEnv {
		for _, name := range profile.SignsInWithEnv(l.Env) {
			p.add(entry(at(path, "env"), name), ErrConflict,
				"%s signs the %q harness in from its env, and auth.mode %s hands it a credential of "+
					"its own beside it, which the harness then ranks by rules of its own: set "+
					"auth.mode: env, so the entry's env is its one sign-in", name, harness, mode)
		}
		return p.err()
	}
	if l.Auth.CredentialBundle != "" {
		// A bundle is a login, and an env entry seeds none, on this host or in
		// a box: one named here would be stored, restored and never read.
		p.add(at(path, "auth.credential_bundle"), ErrConflict,
			"auth.mode env signs the %q harness in with the entry's env alone and seeds no "+
				"login, so a credential bundle reaches nothing. Drop auth.credential_bundle, "+
				"or set auth.mode: subscription to sign in with that login", harness)
	}
	switch {
	case len(profile.SignInEnv) > 0:
		if len(profile.SignsInWithEnv(l.Env)) == 0 {
			p.add(at(path, "env"), ErrMissing,
				"auth.mode env signs the harness in with the entry's env, and env sets none of the "+
					"names the %q harness signs in by (%s), so the entry would sign in to nothing: set "+
					"the one your cloud provider, gateway or account uses, any credential as a ${VAR}, "+
					"e.g. %s: \"${TOKEN}\"",
				harness, strings.Join(profile.SignInEnv, ", "), profile.SignInEnv[0])
		}
	case profile.EnvSignIn:
		signed := slices.ContainsFunc(slices.Collect(maps.Keys(l.Env)), func(name string) bool {
			return harnessprofile.IsCredentialName(name) && strings.TrimSpace(l.Env[name]) != ""
		})
		if !signed {
			p.add(at(path, "env"), ErrMissing,
				"auth.mode env signs the harness in with the entry's env, and env holds no "+
					"credential: the %q harness reads the key of the provider its model names from "+
					"that provider's own variable, so set it there as a ${VAR}, e.g. "+
					`OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"`, harness)
		}
	default:
		p.add(at(path, "auth.mode"), ErrConflict,
			"env signs the harness in with the entry's env, and the %q profile names no variable "+
				"that does (sign_in_env) and does not read its provider's key from its "+
				"environment (env_sign_in), so the entry would sign in to nothing: use "+
				"subscription or api-key, or name the variables your build of the harness signs in "+
				"by with overrides.sign_in_env", harness)
	}
	return p.err()
}

// subscriptionEnvName reports whether name is one the profile sets under
// subscription ([harnessprofile.Profile.SubscriptionEnv]) on an entry running
// under it.
func subscriptionEnvName(profile harnessprofile.Profile, mode HarnessAuthMode, name string) bool {
	_, ok := profile.SubscriptionEnv[name]
	return ok && mode == AuthSubscription
}

// isolationRemedy is where an isolation variable's value IS changed, for the
// refusal of an entry's env that names one: the field of the layer that sets
// it ([harnessprofile.Profile.IsolationLayerOf]), or none.
//
// PER LAYER, because the three fields refuse each other's variables: one
// remedy naming overrides.config_env for every variable sent an operator who
// followed it to a second refusal for both of OpenCode's —
// OPENCODE_CONFIG_DIR is work_config_env's, so config_env naming it too is
// "one variable, two directories", and OPENCODE_CONFIG_PROJECT_DISABLE is an
// isolation_env switch, which may not also be a relocated variable.
func isolationRemedy(name string, layer harnessprofile.IsolationLayer, profile harnessprofile.Profile) string {
	switch layer {
	case harnessprofile.IsolationConfigEnv:
		return relocationRemedy("config_env", "the seat's home", profile.ConfigEnv[name],
			profile.KeptUnder(name))
	case harnessprofile.IsolationWorkConfigEnv:
		return relocationRemedy("work_config_env", "the call's working directory",
			profile.WorkConfigEnv[name], profile.KeptUnder(name))
	case harnessprofile.IsolationSwitch:
		// Not refused there: an override is the profile itself, written on
		// purpose. Turning the switch off is a decision about every seat's
		// isolation, and the remedy says so rather than hand it out as a
		// setting.
		return fmt.Sprintf("it is the profile's isolation switch, which the profile sets to "+
			"%q, changed only deliberately, with overrides.isolation_env, since a value "+
			"that turns it off undoes the isolation it is there for", profile.IsolationEnv[name])
	default: // harnessprofile.IsolationHome, which every profile field refuses
		return "the engine points it at or inside the seat's own home for every harness, " +
			"and no field changes it"
	}
}

// relocationRemedy is the remedy for a variable a profile relocates: the
// field that does it, and what has to move with a different directory.
//
// WITH THE PATHS KEPT UNDER IT, because the directory alone is not a remedy
// that works: the harness finds the profile's login, sessions, tool policy or
// agent file through the variable ([harnessprofile.Profile.KeptUnder]), so "a
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
		"%s kept under %s beneath it in the same override, since the harness finds them "+
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
	// The width is a property of the model (text-embedding-3-large emits 3072,
	// -3-small 1536), so an unset value resolves from [EmbeddingModels] and a
	// model this build does not know is REFUSED rather than guessed; see Width.
	//
	// A value is an OVERRIDE, 64..4096, for a model that can shorten its
	// output. It must match what the model produces: the store's vector columns
	// are checked against it, and a mismatch is a write that cannot be read
	// back. A model whose endpoint documents `dimensions` as unsupported
	// ([DimensionsUnsupported]) is refused any width but its own and is never
	// sent the parameter.
	Dimensions int `yaml:"dimensions,omitempty" json:"dimensions,omitempty" js:"min=0;max=4096" desc:"Vector width; 0 takes the named model's own width. 64..4096 to override, and it must match what the model produces."`

	// MaxInputTokens, MaxBatchInputs and MaxBatchTokens are the model's LIMITS:
	// most tokens per input, most inputs per request, most tokens per request
	// summed over its inputs. 0 means THE MODEL'S OWN, on Dimensions' rule: a
	// guessed limit refuses every long document or batch.
	//
	// An unset value resolves from [EmbeddingModels]; one the table lacks is
	// REFUSED until stated. A stated value for a limit the table carries may
	// only LOWER it (a gateway accepting less than the model). Tokens, the unit
	// vendors document; the engine counts bytes ([EmbeddingProvider.InputBound]).
	//
	// A stored revision lacking a limit it needs is refused at boot as well as
	// at apply (RUNNABLE): the company could only run by guessing limits.
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
// THREE VALUES, each acted on differently. An endpoint that SHORTENS is sent
// the width, so a store sized below the model's own is filled at it. One
// documented as NOT TAKING it is never sent it and holds a company to the
// model's own width. Where the documentation is SILENT the parameter is sent
// as always, since dropping it is as much a guess as keeping it; the width
// check on every response (internal/providers/embeddings) catches an endpoint
// that ignores it.
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
// the provider is configured with. A table, not a default: width and limits
// are facts about the MODEL, so one absent here is refused.
//
// Bytes are counted against limits stated in tokens because every tokenizer
// these models use emits at most one token per byte (BPE merges only shrink;
// SentencePiece and WordPiece pieces cover at least one character) plus a
// wrapper: a bound needing NO TOKENIZER. One hole: NFKC-normalising tokenizers
// can lengthen a few characters (U+FDFA: three bytes, eighteen tokens);
// [EmbeddingWrapTokens] absorbs a handful. Prose at three to four bytes a
// token is held to about a quarter of the window: a shorter opening for a
// caller that embeds a text's opening.
var EmbeddingModels = map[string]EmbeddingModel{
	// OpenAI's embeddings reference
	// (https://developers.openai.com/api/reference/resources/embeddings/methods/create):
	// at most 8192 tokens an input for all embedding models, 2048 inputs, and
	// 300,000 tokens summed per request. WrapTokens is 0 because the guide's own
	// way to check an input is to count it with cl100k_base, so the per-input
	// byte bound is exactly 8192, the size the knowledge corpus has always
	// embedded at (search.EmbedInputBytes): a byte less would move the opening
	// of every long document, change the digest its replicated vector record
	// carries and re-embed the corpus.
	//
	// `dimensions` is "Only supported in text-embedding-3 and later models"
	// (same reference); the guide's table gives each model's own width.
	"text-embedding-3-large": {
		Width: 3072, Dimensions: DimensionsShortens, DimensionsSource: openAIDimensions,
		InputTokens: 8192, BatchInputs: 2048, BatchTokens: 300_000,
	},
	"text-embedding-3-small": {
		Width: 1536, Dimensions: DimensionsShortens, DimensionsSource: openAIDimensions,
		InputTokens: 8192, BatchInputs: 2048, BatchTokens: 300_000,
	},

	// The model page (https://ai.google.dev/gemini-api/docs/models/gemini-embedding-001):
	// an input limit of 2,048 tokens and a flexible output of 128..3072. The
	// OpenAI-compatible endpoint this backend calls is documented only by an
	// example (https://ai.google.dev/gemini-api/docs/openai); the native API
	// can silently truncate an over-long input (autoTruncate), and neither page
	// says what the compatible endpoint does.
	//
	// `dimensions` is not mentioned on the compatibility page. The native API
	// shortens through `output_dimensionality` (default 3072), which says
	// nothing about the compatible endpoint, so it is sent as always and the
	// width check on every response holds the vector to the store.
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
	// context, read as 128 000 (the smaller meaning of "128k", so the bound
	// holds under either), default output 1536. The Compatibility API this
	// backend calls (https://docs.cohere.com/docs/compatibility-api) documents
	// no limit; the 96 texts a call and the silent END truncation are the
	// native Embed API's.
	//
	// That page lists `dimensions` among the parameters it does NOT support and
	// does not say whether one sent is ignored or refused, nor how a width is
	// chosen. So the endpoint returns the model's own width, 1536 (of 256, 512,
	// 1024, 1536 per the model page): the parameter is never sent and no
	// other width can be configured.
	"embed-v4.0": {
		Width: 1536, Dimensions: DimensionsUnsupported,
		DimensionsSource: "Cohere's Compatibility API " +
			"(docs.cohere.com/docs/compatibility-api) lists `dimensions` among " +
			"the embeddings parameters it does not support",
		InputTokens: 128_000, WrapTokens: EmbeddingWrapTokens,
		Undocumented: "Cohere's Compatibility API " +
			"(docs.cohere.com/docs/compatibility-api) states no limit on the " +
			"inputs or the tokens one request may carry (the native Embed " +
			"API's 96 texts a call is a different endpoint's)",
	},
}

// openAIDimensions is where OpenAI documents the `dimensions` parameter for its
// third-generation models.
const openAIDimensions = "OpenAI's embeddings reference " +
	"(developers.openai.com/api/reference/resources/embeddings/methods/create): " +
	"`dimensions` is supported in text-embedding-3 and later models"

// EmbeddingWrapTokens is the allowance, in tokens, for what a server adds
// around each input of a model whose vendor does not document its count: every
// model in [EmbeddingModels] but OpenAI's, and any it does not carry.
//
// SIXTEEN. Wrappers are two or three tokens ([CLS]/[SEP], <s>/</s> plus a
// SentencePiece boundary marker, BOS/EOS), so sixteen is five times the
// largest, with room for the few NFKC expansions the byte argument cannot
// cover ([EmbeddingModels]), at under one percent of a 2 048-token window. A
// server that PREPENDS its own instruction is outside it: declare
// `max_input_tokens` net of that.
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
// (the `dimensions` parameter) rather than only checking the width returned.
//
// Every model is asked except one whose endpoint documents the parameter as
// NOT SUPPORTED ([DimensionsUnsupported]), at the width it emits on its own:
// an endpoint that does not take a parameter may refuse it. At any other width
// it is sent as before; validation refuses that width, so only a caller that
// skipped validation meets it, and it fails at the width check.
func (e *EmbeddingProvider) SendsDimensions() bool {
	model, known := EmbeddingModels[strings.TrimSpace(e.Model)]
	return !known || model.Dimensions != DimensionsUnsupported || e.Width() != model.Width
}

// InputBound is the most PREPARED BYTES one input to this provider may hold,
// and 0 when the model's window is unknown, which validation refuses.
//
// THE ONE CONVERSION from what a vendor documents (tokens) to what the engine
// counts (bytes), by the argument at [EmbeddingModels]: N bytes are at most N
// tokens plus the wrap allowance, so window - wrap bytes fit. The window is
// the documented one, lowered by a stated `max_input_tokens` and by the
// request's own token total where known and smaller.
//
// Its own method, not part of [EmbeddingProvider.Limits]: a reader of stored
// vectors needs the window the corpus was embedded at, known wherever the
// window is. A stated limit only LOWERS a documented one, even unvalidated.
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
		p.add(at(path, "type"), ErrUnknownValue, "%q (want %s)", e.Type, words.Or(EmbeddingProviderTypes))
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
			"%d is below the %d floor: leave it unset to take %q's own width, "+
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
					"proxy that accepts less): leave it unset to take %d",
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
