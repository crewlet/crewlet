package api

import "slices"

// Route is one mounted route as the route table keeps it: its pattern and the
// reach it declared. See [routeTable].
type Route = route

// Routes is every route the app mounted, in mount order, for the listener and
// reach cases to walk whole. See [routeTable].
func (a *App) Routes() []Route { return slices.Clone(a.routes) }

// QuestionOf is the registry question a named read route answers, and whether
// pattern is one, for the reach gate to hold the route to its question.
func QuestionOf(pattern string) (string, bool) {
	for _, r := range namedRoutes {
		if r.method+" "+r.pattern == pattern {
			return r.what, true
		}
	}
	return "", false
}
