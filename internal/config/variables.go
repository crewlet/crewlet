package config

import (
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// VariablesRead is every variable resolving this document may read, sorted
// and each once: every `${VAR}` its fields reference ([References]), and the
// CONVENTIONAL variable each entry that names none of its own reads in its
// place — an LLM entry's vendor key ([LLMProvider.ConventionalKeyVar]), a
// cli-agent entry's headless token (its profile's token_env) and exported
// credential bundle ([cliprofile.BundleVarName]), and the embedder's key
// ([OpenAIKeyVar]).
//
// # Why the conventions belong in the answer
//
// The engine resolves a document against a snapshot of the WHOLE secret store,
// so a convention needs no listing there. A reader that is handed only the
// values a document reads — a CLI run asking a running node for them, which is
// given exactly the names it asks for rather than the store — needs this list
// to be complete, or a variable the store holds resolves from the environment
// instead: `crewlet llm export -secret-store` writes the bundle under its
// conventional name and `crewlet llm login -capture-token` the token under
// the profile's, and no document references either.
//
// EACH CONVENTION IS LISTED ON THE PREDICATE ITS READER APPLIES — no api_keys,
// an empty token or bundle reference, an empty embedder key — and not on
// whether the value would then be used: an entry whose auth mode never sends
// its token still lists it, because a value fetched and unused costs nothing
// while one used and not fetched resolves from the wrong place.
//
// # Every name listed is one a store can hold
//
// A reference is one by construction, and so is a bundle's conventional name.
// A profile's token_env is one only if the document says so: every write
// refuses one outside the reference grammar
// ([LLMProvider.validateCLICredentials]), but that is an admission rule, and
// a file a provisioner acts on is held to the runnable rules alone
// ([LoadCompanyToRun]). No store keeps such a name — the engine's snapshot
// never holds one either, so its token resolves from the environment there —
// and handed one, a node's batch read refuses the whole request, which failed
// every run that read the company's values through a node while the same run
// on a stopped node read a plain miss. So it is not listed, and the resolver
// answers it from the environment on every route, as the engine does.
func (c *Company) VariablesRead() []string {
	if c == nil {
		return nil
	}
	names := map[string]bool{}
	for _, ref := range References(c) {
		names[ref.Name] = true
	}
	for key, spec := range c.Providers.LLM {
		if len(spec.APIKeys) == 0 {
			if name := spec.ConventionalKeyVar(); name != "" {
				names[name] = true
			}
		}
		if spec.Type != LLMCLIAgent || spec.CLI == nil {
			continue
		}
		cli := spec.CLI
		if cli.Auth.Token == "" {
			// THE MERGED PROFILE, as the backend is built from it: an
			// override can rename the token variable, and a profile
			// that does not load is one validation refuses before any
			// reader builds it.
			if profile, err := cliprofile.Load(cli.Name(), cli.Overrides); err == nil &&
				envref.ValidName(profile.TokenEnv) {
				names[profile.TokenEnv] = true
			}
		}
		if cli.Auth.CredentialBundle == "" {
			names[cliprofile.BundleVarName(key)] = true
		}
	}
	if e := c.Providers.Embeddings; e != nil && strings.TrimSpace(e.APIKey) == "" {
		names[OpenAIKeyVar] = true
	}
	return slices.Sorted(maps.Keys(names))
}
