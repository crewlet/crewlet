package api

import "slices"

// Routes is every pattern the app mounted, in mount order, for the listener
// cases to walk whole. See [routeTable].
func (a *App) Routes() []string { return slices.Clone(a.routes) }

// RefuseOTLP is the OTLP route's answer to an export the receiver refused.
var RefuseOTLP = refuseOTLP
