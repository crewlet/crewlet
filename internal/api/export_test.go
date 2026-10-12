package api

import (
	"net/http"
	"slices"
	"time"
)

// Routes is every pattern the app mounted, in mount order, for the listener
// cases to walk whole. See [routeTable].
func (a *App) Routes() []string { return slices.Clone(a.routes) }

// ThroughDrainGate is next behind this app's drain gate and nothing else, so a
// case can hold a git request where the gate counts it: no node mounts a git
// route yet, and the mux behind the gate answers every one 404 at once.
func (a *App) ThroughDrainGate(next http.Handler) http.Handler { return a.drainGate(next) }

// SetGitExchangeGap scales [gitExchangeGap] on one app, so the drain's wait
// for an exchange's next request is a case's to measure in milliseconds.
func SetGitExchangeGap(a *App, gap time.Duration) { a.gitGap = gap }
