package api

import (
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/authz"
)

// NamedRoute is one named read route — its method and pattern, and the
// registry question behind it — for the case that holds every question to the
// one path it is answered on.
type NamedRoute struct{ Method, Pattern, What string }

// NamedRoutes is [namedRoutes], in its order.
func NamedRoutes() []NamedRoute {
	out := make([]NamedRoute, len(namedRoutes))
	for i, r := range namedRoutes {
		out[i] = NamedRoute{Method: r.method, Pattern: r.pattern, What: r.what}
	}
	return out
}

// GitRefusal is [gitRefusal], for the case that holds a git route's refusal to
// git's own form: on every path the app serves, the guard in front of the
// router answers first whatever the router could refuse.
var GitRefusal = gitRefusal

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

// ThroughDrainGate is next behind this app's drain gate and nothing else, so a
// case can hold a git request where the gate counts it, in a handler of its own
// rather than the code host the app mounts behind it.
func (a *App) ThroughDrainGate(next http.Handler) http.Handler { return a.drainGate(next) }

// SetGitExchangeGap scales [gitExchangeGap] on one app, so the drain's wait
// for an exchange's next request is a case's to measure in milliseconds.
func SetGitExchangeGap(a *App, gap time.Duration) { a.gitGap = gap }
