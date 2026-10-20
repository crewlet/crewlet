package api

import (
	"net/http"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/code"
)

// CodeRead is the handler of one kind of code read with no guard in front of
// it, for the case that holds what a request carrying no operator identity is
// answered — a request no route the app mounts lets through, since the guard
// stamps every caller it admits.
func (a *App) CodeRead(kind code.ReadKind) http.Handler { return a.serveCodeRead(kind, "code read") }

// Routes is every pattern the app mounted, in mount order, for the listener
// cases to walk whole. See [routeTable].
func (a *App) Routes() []string { return slices.Clone(a.routes) }

// ThroughDrainGate is next behind this app's drain gate and nothing else, so a
// case can hold a git request where the gate counts it, in a handler of its own
// rather than the code host the app mounts behind it.
func (a *App) ThroughDrainGate(next http.Handler) http.Handler { return a.drainGate(next) }

// SetGitExchangeGap scales [gitExchangeGap] on one app, so the drain's wait
// for an exchange's next request is a case's to measure in milliseconds.
func SetGitExchangeGap(a *App, gap time.Duration) { a.gitGap = gap }
