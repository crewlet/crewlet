// Package engine assembles one process's participation in a company: the
// config, the org, the models, the tools, and the turn machinery that runs
// between them.
//
// It is the ENTANGLEMENT POINT, and the reason it is a package of small files
// rather than one large one. Collected into a single module this is thousands
// of lines whose two hardest passages — the inbox handler and the config
// apply — can only be read by scrolling past everything else. Here
// each of those is a package of its own (internal/agent/inbox, and the config
// plane), and what remains here is the WIRING: which concrete thing satisfies
// which seam.
package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/credential"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/anthropic"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/providers/llm/openai"
)

// buildProviders turns the company's providers.llm map into the ordered
// registry the per-phase resolver reads.
//
// ORDER COMES FROM THE CONFIG DOCUMENT, not from the map. Resolution's last
// resort is "the first provider configured", which is a real answer over the
// order an operator wrote and no answer at all over a Go map — two seats booted
// from one config would land on different models, and one seat would change
// model across a restart. config.Company.ProviderOrder is what preserves it.
func buildProviders(c *config.Company, r *config.Resolver) (*phase.Registry, error) {
	order := c.Providers.ProviderOrder()
	entries := make([]phase.Entry, 0, len(order))
	for _, key := range order {
		spec, ok := c.Providers.LLM[key]
		if !ok {
			// ProviderOrder is derived from the same map, so this cannot
			// happen — but a silent skip here would produce a registry
			// missing a provider the operator configured, and the seat
			// using it would fall back to another model with nothing said.
			return nil, fmt.Errorf("engine: provider %q is in the order but not in the map", key)
		}
		p, err := buildProvider(key, spec, r)
		if err != nil {
			return nil, err
		}
		entries = append(entries, phase.Entry{Key: key, Provider: p})
	}
	return phase.NewRegistry(entries)
}

// buildProvider constructs one backend.
//
// A provider whose credentials are missing still BUILDS. Every call then comes
// back a clean 401, which names the provider and the vendor — far easier to
// diagnose than a constructor that refused to exist and took the whole company
// down at boot with a message about one key.
func buildProvider(key string, spec config.LLMProvider, r *config.Resolver) (llm.Provider, error) {
	timeout := time.Duration(spec.TimeoutSeconds * float64(time.Second))
	// RESOLVED HERE, at the moment the provider is built, which is the only
	// place a key value ever exists in this process. Tier B stores its
	// references verbatim — that is what keeps an exported revision free of
	// resolved secrets — so a backend handed spec.APIKeys directly would
	// send the literal "${ANTHROPIC_API_KEY}" as its credential and get a
	// 401 that names the vendor rather than the misconfiguration.
	keys := spec.ResolvedKeys(r)
	// The SAME resolution the keys get, for the other two scalars a Tier B
	// document is allowed to write a reference into. Tier B stores "${VAR}"
	// verbatim, so a backend handed the raw field receives the literal
	// "${LLM_BASE_URL}" and sends every request to a URL that is not one —
	// and examples/nimbus.company.yaml ships exactly that reference, against
	// the variable docs/reference/environment-variables.md documents.
	model, modelMissing := r.Expand(spec.Model)
	baseURL, baseURLMissing := r.Expand(spec.BaseURL)
	// A REFERENCE THAT RESOLVED TO NOTHING is not the same as a field
	// somebody left out, and the two need different sentences. A provider
	// entry that plainly reads `model: "${LLM_MODEL}"` and is refused with
	// "Model is required" sends an operator to look at a field they can see
	// is filled in; naming the variable sends them to the one thing they
	// have to change.
	//
	// This is a REFUSAL where a missing api_key is not, and the asymmetry is
	// deliberate: a missing credential still builds, because every call then
	// comes back a clean 401 that names the provider — far easier to
	// diagnose than a boot that died over one key. A missing MODEL has no
	// such tell; the request is malformed rather than unauthorised.
	if len(modelMissing) > 0 && strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf(
			"engine: provider %q: model is %q and %s resolved to nothing. Set "+
				"it in the environment, or with `crewlet secrets set`",
			key, spec.Model, strings.Join(modelMissing, ", "))
	}
	// AN OPENAI-COMPATIBLE ENTRY WITH NO ENDPOINT IS REFUSED, and this one
	// is not a message fix. `openai-compatible` means "not OpenAI" by
	// definition — the type exists to point somewhere else — and an empty
	// base URL takes the openai backend's own default, so a company whose
	// ${LLM_BASE_URL} was unset sends every request, under a key that is not
	// an OpenAI key, to api.openai.com. That is a silent misroute of the
	// company's whole model traffic to a third party, and the only symptom
	// is a 401 naming a vendor the operator never configured.
	if spec.Type == config.LLMOpenAICompatible && strings.TrimSpace(baseURL) == "" {
		where := "base_url is empty"
		if len(baseURLMissing) > 0 {
			where = fmt.Sprintf("base_url is %q and %s resolved to nothing",
				spec.BaseURL, strings.Join(baseURLMissing, ", "))
		}
		return nil, fmt.Errorf(
			"engine: provider %q: %s. An openai-compatible entry needs an endpoint "+
				"— without one every request would go to api.openai.com under "+
				"this entry's own key", key, where)
	}
	// Through the accessors, not the raw fields: those apply the bounds and
	// the defaults, and reading the fields directly would send a zero
	// cooldown for every provider that did not configure one.
	cooldowns := credential.Policy{
		RateLimit: time.Duration(spec.Cooldowns.RateLimit()) * time.Second,
		Auth:      time.Duration(spec.Cooldowns.Auth()) * time.Second,
	}

	switch spec.Type {
	case config.LLMAnthropic:
		return anthropic.New(anthropic.Config{
			Model: model, APIKeys: keys, BaseURL: baseURL,
			Timeout: timeout, Cooldowns: cooldowns,
			Reasoning: spec.Reasoning, ThinkingBudget: spec.ReasoningBudgetTokens,
			// The conventional-key fallback — ANTHROPIC_API_KEY, taken when
			// the entry names no api_keys — reads a VARIABLE rather than
			// expanding a reference, so it needs the resolver itself.
			// os.Getenv cannot see what `crewlet secrets set` put in the
			// store, and the fallback would answer "unset" for a credential
			// the operator deliberately stored.
			LookupEnv: r.Lookup,
		})
	case config.LLMOpenAI, config.LLMOpenAICompatible:
		// Name labels errors, logs and the chain's telemetry. An
		// openai-compatible entry passes its CONFIG KEY so a failure names
		// the endpoint that answered rather than claiming to be OpenAI;
		// a plain openai entry leaves it empty and keeps the vendor's name.
		name := ""
		if spec.Type == config.LLMOpenAICompatible {
			name = key
		}
		return openai.New(openai.Config{
			Model: model, Name: name, APIKeys: keys, BaseURL: baseURL,
			Timeout: timeout, Cooldowns: cooldowns,
			Reasoning: spec.Reasoning, ReasoningEffort: string(spec.ReasoningEffort),
			LookupEnv: r.Lookup,
		})
	case config.LLMCLIAgent:
		return buildCLIAgent(key, spec, r, keys)
	default:
		return nil, fmt.Errorf("engine: provider %q: unknown type %q", key, spec.Type)
	}
}

// log is the package logger.
var log = logging.Get("engine")

// BuildCLIAgent constructs one cli-agent provider from its config entry.
//
// Exported for `crewlet llm`, which has to build the SAME provider the engine
// would in order to report on it: a doctor that constructed its own would
// diagnose a provider no seat is using, and the two would drift the first
// time a default changed.
func BuildCLIAgent(key string, spec config.LLMProvider, r *config.Resolver) (*cliagent.Provider, error) {
	return buildCLIAgent(key, spec, r, spec.ResolvedKeys(r))
}

// buildCLIAgent constructs the subscription-CLI backend.
//
// Its credentials do not arrive the way an API entry's do. A metered entry
// has a list of keys and a pool that rotates them; this one has ONE login,
// held by the vendor's CLI in a directory on disk, and at most a single
// long-lived token beside it. So there is no pool here and nothing to rotate
// — the cooldowns an operator configured govern the chain's retry of this
// provider, not a key inside it.
func buildCLIAgent(key string, spec config.LLMProvider, r *config.Resolver, apiKeys []string) (*cliagent.Provider, error) {
	cli := spec.CLI
	if cli == nil {
		// Validation refuses this combination, so reaching it means the
		// provider was built from a document that never went through it.
		return nil, fmt.Errorf("engine: provider %q: type %q with no cli block", key, spec.Type)
	}
	stateDir, err := cli.ResolvedStateDir(key)
	if err != nil {
		return nil, fmt.Errorf("engine: provider %q: %w", key, err)
	}
	// RESOLVED HERE for the same reason an API key is: Tier B stores
	// "${VAR}" verbatim, so a child handed the raw map would receive the
	// literal reference as its value.
	env, missing := r.Map(fmt.Sprintf("providers.llm.%s.cli.env", key), cli.Env)
	for _, ref := range missing {
		// Warned rather than refused: a CLI whose optional tuning
		// variable is unset still runs, and a provider that refused to
		// build would take the company down over it.
		log.Warn("cli_agent_env_unresolved", "provider", key,
			"path", ref.Path, "variables", strings.Join(ref.Names, ","))
	}

	auth := cliagent.Auth{Mode: cliagent.AuthMode(cli.Auth.Mode)}
	if auth.Mode == "" {
		auth.Mode = cliagent.AuthSubscription
	}
	if len(apiKeys) > 0 {
		auth.APIKey = apiKeys[0]
	}

	// Both credentials fall back to a CONVENTIONAL name when the entry
	// points at none, and both go through the resolver, which reads the
	// secret store BEFORE the environment. That order is the whole point:
	// `crewlet llm login <key> -capture-token` writes the token into the
	// store, and an entry that had to name it explicitly would leave every
	// operator wiring up a ${VAR} for a value Crewlet itself just wrote.
	profile, err := cliagent.Load(cli.Name(), cli.Overrides)
	if err != nil {
		return nil, fmt.Errorf("engine: provider %q: %w", key, err)
	}
	auth.Token = resolveOrConvention(r, cli.Auth.Token, profile.TokenEnv)
	bundle := resolveOrConvention(r, cli.Auth.CredentialBundle, cliagent.BundleVarName(key))

	return cliagent.New(cliagent.Config{
		Key: key,
		// Resolved for the same reason cli.env is: a model written as a
		// reference would reach the CLI's --model flag as the literal
		// "${VAR}", and the CLI would reject a model by that name.
		Model:            r.Value(spec.Model),
		Agent:            cli.Name(),
		AgentMode:        cli.AgentMode(),
		StateDir:         stateDir,
		Overrides:        cli.Overrides,
		Timeout:          time.Duration(cli.Timeout() * float64(time.Second)),
		MaxConcurrent:    cli.Concurrency(),
		Env:              env,
		Auth:             auth,
		CredentialBundle: bundle,
	})
}

// resolveOrConvention resolves a configured ${VAR} reference, or falls back to
// looking up a conventional variable name.
//
// The fallback is a LOOKUP, not an expansion: `conventional` is a bare name
// rather than a reference, and passing it through Value would leave it
// unresolved and return the literal name as if it were a credential.
func resolveOrConvention(r *config.Resolver, configured, conventional string) string {
	if configured != "" {
		return r.Value(configured)
	}
	if conventional == "" {
		return ""
	}
	return r.Lookup(conventional)
}
