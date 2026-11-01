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
// as Settings › Models reads it beside the fleet's heartbeats: how the entry
// signs in here, the variable its credential is read from, whether it
// resolved, and the hint and lifetime of the value it resolved.
//
// NOTHING HERE IS PUBLISHED OR STORED. The heartbeat carries the route and
// whether it resolves; the variable's name, the hint and the lifetime are
// answered only to the person asking this node, which is why they are read
// here rather than off a lease.
type LocalModel struct {
	Key string
	// Route is harness's sign-in route on this node: token, credentials,
	// api-key, env, inherit-env, environment or none.
	Route string
	// Variable is the variable the entry's credential is read from: its
	// token's under subscription and inherit-env, its key's under api-key,
	// "" for a route that reads none.
	Variable string
	Resolves bool
	// Hint is the resolved credential's ([harness.Provider.CredentialHint]).
	Hint string
	// TokenLifetime is the harness's documented headless-token lifetime, for a
	// route that signs in with one; zero otherwise.
	TokenLifetime time.Duration
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
