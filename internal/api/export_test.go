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
