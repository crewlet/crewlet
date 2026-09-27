package auth

import "github.com/crewlet/crewlet/internal/iam/credential"

// WithBearerCurve replaces the curve a guard judges bearers on, so a suite can
// drive it on a clock and a sleep of its own. A running node never replaces
// it: [New] builds the guard's own.
func (g *Guard) WithBearerCurve(curve *credential.Throttle) *Guard {
	g.bearers = curve
	return g
}
