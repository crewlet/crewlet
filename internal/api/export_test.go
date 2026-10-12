package api

import "slices"

// Routes is every pattern the app mounted, in mount order, for the listener
// cases to walk whole. See [routeTable].
func (a *App) Routes() []string { return slices.Clone(a.routes) }

// ChatReadsOver is a person's read state told to their sockets through live,
// wired as [NewEngineOperator] wires it.
var ChatReadsOver = chatReadsOver
