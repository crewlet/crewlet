package github

import (
	"context"
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/setup"
)

// Probe checks a typed organization token before anything stores it, with
// GET /user: the account the token belongs to.
//
// THE ADDRESS IS DERIVED THE WAY EVERY OTHER CLIENT DERIVES IT
// ([config.GitHub.APIBase]): github.com serves its API from api.github.com
// and an Enterprise Server from /api/v3 on the instance, so the form's
// instance address is read through the same rule rather than a second one.
//
// httpClient is nil in production, which takes the client's own transport
// and timeout; a test hands one that reaches a fake GitHub.
func Probe(httpClient *http.Client) setup.Probe {
	return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
		if check.Field != "token" {
			return setup.Verdict{}, setup.NotCheckable("GitHub", check.Field)
		}
		apiBase := (&config.GitHub{URL: check.Value("url")}).APIBase()
		client, err := NewClient(ClientOptions{
			APIBase: apiBase, Token: check.Secret(), HTTP: httpClient,
		})
		if err != nil {
			return setup.Verdict{}, setup.Unusable("Enter the token to test it.")
		}
		login, err := client.Me(ctx)
		if err != nil {
			var api *APIError
			reason := ""
			if errors.As(err, &api) {
				reason = setup.ReasonIn(api.Detail, "message")
			}
			return setup.Judge(setup.Failure{
				App: "GitHub", Err: err, Status: Status(err), Reason: reason,
			}, check.Secret()), nil
		}
		return setup.Works("Works. GitHub accepted it for the account " + login + "."), nil
	}
}
