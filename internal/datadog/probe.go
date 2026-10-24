package datadog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/setup"
)

// Probe checks a typed Datadog key before anything stores it.
//
// THE TWO KEYS ARE CHECKED TOGETHER, because Datadog checks them together:
// `/api/v1/org` ([Client.VerifyCredentials]) takes the pair and names the
// organization they belong to, and there is no route that proves one key on
// its own. So checking the API key reads the application key from the form
// or the document, and the other way round, and a check with only one of
// them is a request this cannot make rather than a refusal.
//
// httpClient is nil in production, which takes the client's own transport
// and timeout; a test hands one that reaches a fake region.
func Probe(httpClient *http.Client) setup.Probe {
	return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
		var other, otherLabel string
		switch check.Field {
		case "api_key":
			other, otherLabel = check.Value("app_key"), "application key"
		case "app_key":
			other, otherLabel = check.Value("api_key"), "API key"
		default:
			return setup.Verdict{}, setup.NotCheckable("Datadog", check.Field)
		}
		if strings.TrimSpace(other) == "" {
			return setup.Verdict{}, setup.Unusable("Datadog checks the API key and " +
				"the application key together, and there is no " + otherLabel +
				" yet. Enter both to test them.")
		}
		site := check.Value("site")
		if !KnownSite(site) {
			return setup.Verdict{}, setup.Unusable("Choose the Datadog region your " +
				"organization is in: the keys are checked against that region.")
		}
		client, err := NewClient(ClientOptions{Site: site, HTTP: httpClient})
		if err != nil {
			// The site was checked above, so this is the constructor and
			// that check disagreeing, which is the engine's own fault.
			return setup.Verdict{}, fmt.Errorf("datadog: probe: %w", err)
		}
		creds := Credentials{APIKey: check.Value("api_key"), AppKey: check.Value("app_key")}
		org, err := client.VerifyCredentials(ctx, creds)
		if err != nil {
			var api *APIError
			reason := ""
			if errors.As(err, &api) && api.Detail != noDetail {
				reason = api.Detail
			}
			return setup.Judge(setup.Failure{
				App: "Datadog", Err: err, Status: Status(err), Reason: reason,
			}, check.Secret()), nil
		}
		if name := strings.TrimSpace(org.Name); name != "" {
			return setup.Works("Works. Datadog accepted it for the " + name +
				" organization in " + client.Site() + "."), nil
		}
		return setup.Works("Works. Datadog accepted it in " + client.Site() + "."), nil
	}
}
