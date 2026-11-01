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
//
// It is also where a turn is charged to the work it was on: which item a turn
// is on is decided in worksubject.go and what each of its segments charges that
// item in turnspend.go, because only this package holds the trigger, the
// runner's tally and the tracker's writer together. ADR-0022 is the decision.
//
// And it is where an agent's own GitHub App token is issued: when a value
// naming it is read, for that seat's resolution alone, renewed before it runs
// short in the seat's tool servers and in a running box's token files, and
// never sealed (githubtoken.go), because only this package holds a seat's
// configuration, its app's key and its boxes together. ADR-0058 is the
// decision.
//
// And it is where a person's note to a running turn is carried: in the first
// segment's box on the node running the turn until the turn launches its
// executor (steer.go), and from that launch to the turn's end sealed on the
// run's record, which any node files on, the run's bridge serves and the
// review reads (runsteer.go) — because only this package holds the turn, the
// run's record, the bridge and the fleet keyring together. ADR-0067 is the
// decision.
//
// And it is where a seat whose models are all at their usage limit is parked
// rather than retried into the limit (providerpark.go, on the machinery the
// budget park shares, seatpark.go), because only this package holds the
// dispatcher, the seat's inbox and the chain's verdict together. ADR-0064 is
// the decision.
package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// buildProviders turns the company's providers.llm map into the ordered
// registry the per-phase resolver reads.
//
// ORDER COMES FROM THE CONFIG DOCUMENT, not from the map. Resolution's last
// resort is "the first provider configured", which is a real answer over the
// order an operator wrote and no answer at all over a Go map — two seats booted
// from one config would land on different models, and one seat would change
// model across a restart. config.Company.ProviderOrder is what preserves it.
//
// NO PROVIDERS IS NO REGISTRY, and not an error. A company with an empty
// providers.llm validates (an org chart written before its credentials exist,
// and what a company created from the dashboard is until one is added), so
// refusing it here made every node refuse to apply a revision its own dry run
// had just called valid. The epoch carries a nil registry instead, which is
// what [Company.Models] documents and what the dispatcher reads to hold every
// seat's work until a provider arrives.
//
// EVERY ENTRY ON THE NODE'S ONE [harness.Plans], which outlives the revision:
// an entry rebuilt with the same sign-in finds the windows its predecessor was
// sitting out there, and nil (a company built to validate it, not to run it)
// gives each entry a registry of its own.
func buildProviders(c *config.Company, r *config.Resolver, plans *harness.Plans) (*phase.Registry, error) {
	order := c.Providers.ProviderOrder()
	if len(order) == 0 {
		return nil, nil
	}
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
		p, err := BuildHarness(key, spec, r, plans)
		if err != nil {
			return nil, err
		}
		entries = append(entries, phase.Entry{Key: key, Provider: p})
	}
	return phase.NewRegistry(entries)
}

// log is the package logger.
var log = logging.Get("engine")

// restoreBundles restores every cli-agent entry's configured credential bundle
// into its state directory where that directory holds no login yet
// ([harness.Provider.RestoreConfiguredBundle]).
//
// A STEP OF ITS OWN, made by the paths that build an epoch to RUN it — a node's
// boot and every apply — and not by building one: [NewCompany] is also what
// `crewlet validate` calls, and validating a document must neither write a
// login onto the host nor wait on the shared login's lock. It runs before
// anything calls the epoch's models — before the epoch is published, and on an
// apply before the revision's reflection workers are swapped in behind the
// running seats ([Engine.prepareReflection]) — so the epoch's first call
// already finds the login. And on an apply only after every stage that can
// refuse the revision ([Engine.Apply]), because the state directory it writes
// is the running epoch's too, and a refused revision must leave no login
// behind.
//
// A failed restore is WARNED and never refuses the epoch: a node with a broken
// bundle can still be logged in by hand, and refusing a whole revision over one
// blob is the worse outcome. An entry `crewlet llm logout` emptied restores
// nothing either ([harness.ErrLoggedOut]) — the logout is the operator's
// decision and outlives every apply until a login is written again — and that
// is warned as what it is, `cli_agent_bundle_withheld`, naming the logout and
// how to sign in again, rather than as a bundle that failed.
func (c *Company) restoreBundles(ctx context.Context) {
	if c == nil || c.Models == nil {
		return
	}
	for key, provider := range c.Models.All() {
		cli, ok := provider.(*harness.Provider)
		if !ok {
			continue
		}
		err := cli.RestoreConfiguredBundle(ctx)
		switch {
		case err == nil:
		case errors.Is(err, harness.ErrLoggedOut):
			log.WarnContext(ctx, "cli_agent_bundle_withheld", "provider", key, "detail", err.Error())
		default:
			log.WarnContext(ctx, "cli_agent_bundle_restore_failed", "provider", key, "error", err)
		}
	}
}

// BuildHarness constructs one providers.llm entry's provider.
//
// Exported for `crewlet llm`, which has to build the SAME provider the engine
// would in order to report on it: a doctor that constructed its own would
// diagnose a provider no seat is using, and the two would drift the first
// time a default changed.
//
// A provider whose credentials are missing still BUILDS. Every call then fails
// with the CLI's own "not authenticated", which names the CLI and is exactly
// what `crewlet llm doctor` explains — far easier to diagnose than a
// constructor that refused to exist and took the whole company down at boot
// over one login.
//
// ITS CREDENTIALS DO NOT ARRIVE AS A BAG OF KEYS. The entry holds ONE login,
// kept by the vendor's CLI in a directory on disk, and at most a single
// long-lived token or API key beside it, so there is no pool here and nothing
// to rotate: a second key is a second entry in the seat's fallback chain.
//
// plans is the node's registry of sign-ins ([harness.Config.Plans]); nil
// gives the entry one of its own, which is what a command that reports on one
// entry needs.
func BuildHarness(key string, spec config.LLMProvider, r *config.Resolver,
	plans *harness.Plans) (*harness.Provider, error) {
	stateDir, err := spec.ResolvedStateDir(key)
	if err != nil {
		return nil, fmt.Errorf("engine: provider %q: %w", key, err)
	}
	// The model, RESOLVED HERE: Tier B stores "${VAR}" verbatim, so a CLI
	// handed the raw field would receive the literal reference on its
	// --model flag and refuse a model by that name.
	//
	// A REFERENCE THAT RESOLVED TO NOTHING is refused, and that is not the
	// same as a field somebody left out (which validation refuses). An entry
	// that plainly reads `model: "${LLM_MODEL}"` and silently ran on the
	// CLI's own default model when LLM_MODEL was unset would bill and answer
	// as a model nobody chose; naming the variable sends the operator to the
	// one thing they have to change. A missing CREDENTIAL still builds,
	// because every call then fails with the CLI's own sign-in error that
	// names it; a missing model has no such tell.
	//
	// "RESOLVED TO NOTHING" IS THE PRESENCE RULE, NOT SUBSTITUTION'S. Expand
	// reports a name only when no source answered, so a variable set to ""
	// — compose passing through an unset host variable, an empty Kubernetes
	// env value, an empty stored secret, which the resolver treats as
	// authoritative — expanded to "" with nothing reported, and the entry
	// built with no model at all. [config.Resolver.Missing] counts an empty
	// answer as absent and tests every reference rather than the result, so
	// `${PROVIDER}/sonnet` with PROVIDER unset is refused too instead of
	// handing the CLI "/sonnet". The blank check catches what is left: a
	// value that is all white space, which no CLI reads as a model.
	model := strings.TrimSpace(r.Value(spec.Model))
	if missing := r.Missing(spec.Model); len(missing) > 0 || model == "" {
		if len(missing) == 0 {
			// Every reference answered, with white space: config refuses
			// a blank written field, so the blank came from them.
			missing = envref.Names(spec.Model)
		}
		return nil, fmt.Errorf(
			"engine: provider %q: model is %q and %s resolved to nothing — unset, "+
				"or set to an empty value. Set it in the environment, or with "+
				"`crewlet secrets set`",
			key, spec.Model, strings.Join(missing, ", "))
	}
	// RESOLVED HERE for the same reason: a child handed the raw map would
	// receive the literal reference as its value.
	env, missing := r.Map(fmt.Sprintf("providers.llm.%s.env", key), spec.Env)
	for _, ref := range missing {
		// Warned rather than refused: a CLI whose optional tuning
		// variable is unset still runs, and a provider that refused to
		// build would take the company down over it.
		log.Warn("cli_agent_env_unresolved", "provider", key,
			"path", ref.Path, "variables", strings.Join(ref.Names, ","))
	}

	// Config loads this same merged profile under its runnable rules
	// (LLMProvider.validateProfile), so a failure here is a document that
	// never went through validation — refused here rather than built on
	// whatever CredentialSources made of a profile it could not load.
	if _, err := harnessprofile.Load(spec.HarnessName(), spec.Overrides); err != nil {
		return nil, fmt.Errorf("engine: provider %q: %w", key, err)
	}

	// Every credential falls back to a CONVENTIONAL name when the entry
	// points at none, and each goes through the resolver, which reads the
	// secret store BEFORE the environment. That order is the whole point:
	// `crewlet llm login <key> -capture-token` writes the token into the
	// store, and an entry that had to name it explicitly would leave every
	// operator wiring up a ${VAR} for a value Crewlet itself just wrote. The
	// API key reads the CLI's own key variable — ANTHROPIC_API_KEY for
	// Claude Code — so a key already exported, or set with `crewlet secrets
	// set`, signs an api-key entry in with no YAML change; it reaches the
	// child only under auth.mode api-key ([harness.Auth]).
	//
	// WHICH variable each falls back to is [config.LLMProvider.CredentialSources],
	// the one rule the company's account of the variables it reads also
	// follows, so a credential this resolves is never one the Secrets screen
	// calls read by nothing.
	sources := spec.CredentialSources(key)
	auth := harness.Auth{
		Mode:            harness.AuthMode(spec.ResolvedAuthMode()),
		Token:           resolveOrConvention(r, sources.Token.Value, sources.Token.Fallback),
		TokenConfigured: spec.Auth.Token != "",
		APIKey:          strings.TrimSpace(resolveOrConvention(r, sources.APIKey.Value, sources.APIKey.Fallback)),
	}
	bundle := resolveOrConvention(r, sources.Bundle.Value, sources.Bundle.Fallback)
	bundleConfigured := spec.Auth.CredentialBundle != ""

	return harness.New(harness.Config{
		Key:                        key,
		Model:                      model,
		Harness:                    spec.HarnessName(),
		StateDir:                   stateDir,
		Overrides:                  spec.Overrides,
		Timeout:                    time.Duration(spec.Timeout() * float64(time.Second)),
		MaxConcurrent:              spec.Concurrency(),
		Env:                        env,
		Auth:                       auth,
		CredentialBundle:           bundle,
		CredentialBundleConfigured: bundleConfigured,
		Plans:                      plans,
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
