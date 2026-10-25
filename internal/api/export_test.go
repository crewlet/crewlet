package api

import (
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/authz"
)

// NativeAbsent is [nativeAbsent] — why a native half is not here — for the
// external suite, which holds both transports to one answer about it without
// an engine to put in either posture.
var NativeAbsent = nativeAbsent

// Routes is every pattern the app mounted, in mount order, for the listener
// cases to walk whole. See [routeTable].
func (a *App) Routes() []string { return slices.Clone(a.routes) }

// Policies is every guarded route's policy, keyed by its pattern. See
// [routeTable].
func (a *App) Policies() map[string]authz.Policy { return maps.Clone(a.policies) }

// OverviewRoutes are the reads served from the stream service, by pattern.
func (a *App) OverviewRoutes() []string {
	var out []string
	for _, route := range a.overviewRoutes() {
		out = append(out, route.pattern)
	}
	return out
}

// NamedReadRoutes are the named read routes' patterns, each with the
// question it answers.
func NamedReadRoutes() map[string]string {
	out := map[string]string{}
	for _, route := range namedRoutes {
		out[route.method+" "+route.pattern] = route.what
	}
	return out
}
