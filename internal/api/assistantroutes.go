package api

import (
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/iam"
)

// --- the Dashboard Assistant's route table (ADR-0052) ---

// assistantMethods are the methods the assistant's generic tools send: a read
// grant's two, and the four an approved change may carry.
var assistantMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost,
	http.MethodPut, http.MethodPatch, http.MethodDelete}

// AssistantRoutes are the routes the Dashboard Assistant's generic API tools
// may name (internal/assistant/catalog's API.Bind): every guarded route with
// the verb it is mounted with, and the reads and the act route that decide
// inside their handlers (the named reads, the question route, the overview
// reads and `POST /operator/act/{tool}`), as this App mounted them.
//
// FROM THE ONE ROUTE TABLE, never a list beside it: a route a surface adds is
// one the assistant can name the moment it is mounted, decided by its own row,
// and a route the edge refuses a grant ([auth.AssistRefusedPath]: the
// credential surface, the socket, both MCP transports, the webhooks and the
// assistant's own routes) is never offered. Every other unguarded route (the
// probes, the dashboard's own files) is not an API a person calls.
func (a *App) AssistantRoutes() []catalog.Route {
	decidedInside := map[string]bool{catalog.QuestionRoute: true, catalog.ActRoute: true}
	for _, r := range a.overviewRoutes() {
		decidedInside[r.pattern] = true
	}
	named := map[string]string{}
	for _, r := range namedRoutes {
		decidedInside[r.method+" "+r.pattern] = true
		named[r.method+" "+r.pattern] = r.what
	}
	patterns := slices.Concat(a.routes, slices.Collect(maps.Keys(a.policies)))
	slices.Sort(patterns)
	var out []catalog.Route
	for _, pattern := range slices.Compact(patterns) {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok || !slices.Contains(assistantMethods, method) || auth.AssistRefusedPath(path) ||
			strings.Contains(path, "{$}") {
			continue
		}
		if policy, guarded := a.policies[pattern]; guarded {
			out = append(out, catalog.Route{Pattern: pattern, Action: policy.Action,
				Absent: policy.Absent, Effect: policy.Effect, DryRun: policy.DryRun,
				Summary: policy.Summary, Replaces: policy.Replaces})
		} else if decidedInside[pattern] {
			// THE ACT ROUTE'S EFFECT is the route table's to state
			// (catalog.NewRouteTable), as every table builds it.
			route := catalog.Route{Pattern: pattern, Apps: a.questionApps(named[pattern])}
			if pattern == catalog.QuestionRoute {
				route.QuestionApps = map[string][]iam.Grant{}
				for _, what := range a.queries.Names() {
					route.QuestionApps[what] = a.questionApps(what)
				}
			}
			out = append(out, route)
		}
	}
	return out
}

// questionApps are the apps a registered question draws on, as its posture
// declares them: the app or admin it requires, or the apps of the verb its
// answer decides by. None for a question everybody asks or none registered.
func (a *App) questionApps(what string) []iam.Grant {
	if what == "" {
		return nil
	}
	posture := a.queries.PostureOf(what)
	if g := posture.Grant(); g != "" {
		return []iam.Grant{g}
	}
	if verb := posture.Verb(); verb != "" {
		return catalog.VerbApps(verb)
	}
	return nil
}
