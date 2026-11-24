package engine

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
)

// LocalModel is what THIS node resolved for one entry of its active revision,
// read by Settings › Models & Fleets beside the fleet's status records: the sign-in
// route, the credential's variable, whether it resolved, and the value's hint
// and lifetime.
//
// Nothing here is published or stored: the status record carries only the
// route and whether it resolves, and the rest is answered only to the person
// asking this node.
type LocalModel struct {
	Key string
	// Route is harness's sign-in route on this node: token, credentials,
	// api-key, env, inherit-env, environment or none.
	Route string
	// Variable is the variable the credential is read from: the token's under
	// token and inherit-env, the key's under api-key, "" otherwise.
	Variable string
	Resolves bool
	// Hint is the resolved credential's ([harness.Provider.CredentialHint]).
	Hint string
	// TokenLifetime is the harness's documented headless-token lifetime, for a
	// route that signs in with one; zero otherwise.
	TokenLifetime time.Duration
	// StoredAt and StoredBy are when and by whom the store wrote the value
	// this node resolves Variable from, as its snapshot holds it
	// ([Engine.storedRecords], which the expiry alarm judges from too); zero
	// where the value did not come from the store.
	StoredAt time.Time
	StoredBy string
}

// LocalModels answers [LocalModel] for every entry of this node's active
// revision, in key order; nil before a company is running.
func (e *Engine) LocalModels() []LocalModel {
	c := e.Company()
	if c == nil || c.Config == nil || c.Models == nil {
		return nil
	}
	ctx := context.Background()
	keys := slices.Sorted(maps.Keys(c.Config.Providers.LLM))
	stored := e.storedRecords()
	out := make([]LocalModel, 0, len(keys))
	for _, key := range keys {
		row := LocalModel{Key: key, Route: "none"}
		provider, ok := c.Models.Provider(key)
		agent, isHarness := provider.(*harness.Provider)
		if !ok || !isHarness {
			out = append(out, row)
			continue
		}
		entry := c.Config.Providers.LLM[key]
		route, resolves := agent.SignInRoute(ctx)
		row.Route, row.Resolves = string(route), resolves
		row.Variable = credentialVariable(key, &entry, row.Route)
		row.Hint = agent.CredentialHint()
		if row.Route == "token" {
			row.TokenLifetime = agent.TokenLifetime()
		}
		if rec, held := stored[row.Variable]; held && row.Variable != "" {
			row.StoredAt, row.StoredBy = rec.UpdatedAt, rec.UpdatedBy
		}
		out = append(out, row)
	}
	return out
}

// credentialVariable is the variable an entry's credential is read from on
// route: the one ${VAR} its field writes, or the convention it falls back to
// ([config.LLMProvider.CredentialSources]).
func credentialVariable(key string, entry *config.LLMProvider, route string) string {
	sources := entry.CredentialSources(key)
	var src config.CredentialSource
	switch route {
	case "token", "inherit-env":
		src = sources.Token
	case "api-key":
		src = sources.APIKey
	default:
		return ""
	}
	if name, ok := envref.Whole(src.Value); ok {
		return name
	}
	if src.Value == "" {
		return src.Fallback
	}
	return ""
}
