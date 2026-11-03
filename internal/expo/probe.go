package expo

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/setup"
)

// Probe answers the connect form's Test button, which Expo has nothing for:
// the form holds no credential, and each robot's token is checked when it is
// pasted for its seat. httpClient is accepted for the shape every probe has.
func Probe(*http.Client) setup.Probe {
	return func(_ context.Context, check setup.Check) (setup.Verdict, error) {
		return setup.Verdict{}, setup.NotCheckable("Expo", check.Field)
	}
}
