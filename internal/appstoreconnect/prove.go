package appstoreconnect

import (
	"context"

	"github.com/crewlet/crewlet/internal/setup"
)

// Probe is the connect form's check: an issuer ID is proven only by an
// agent's own key signing in with it, which the identity check does.
func Probe() setup.Probe {
	return func(context.Context, setup.Check) (setup.Verdict, error) {
		return setup.Verdict{}, setup.ErrNotCheckable
	}
}
