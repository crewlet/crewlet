package atlassian

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/setup"
)

// Probe checks a typed organization API key before anything stores it, by
// reading one page of the organization's service accounts
// ([Client.ReadsServiceAccounts]).
//
// THE KEY IS CHECKED FOR WHAT THIS INTEGRATION DOES WITH IT. A scoped key
// authenticates at every other organization route and is refused at the one
// that creates an agent's account, so a 403 here is the answer the form
// most needs to give, and it says what to do about it.
//
// opts is the zero value in production, which reaches api.atlassian.com; a
// test points BaseURL at a fake one.
func Probe(opts ClientOptions) setup.Probe {
	return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
		if check.Field != "api_key" {
			return setup.Verdict{}, setup.NotCheckable("Atlassian", check.Field)
		}
		orgID := strings.TrimSpace(check.Value("org_id"))
		if orgID == "" {
			return setup.Verdict{}, setup.Unusable("Enter the Atlassian organization ID " +
				"first: the key is checked against that organization.")
		}
		key := strings.TrimSpace(check.Secret())
		if key == "" {
			return setup.Verdict{}, setup.Unusable("Enter the key to test it.")
		}
		err := NewClient(opts).ReadsServiceAccounts(ctx, key, orgID)
		if err == nil {
			return setup.Works("Works. Atlassian accepted it for this organization, " +
				"and it can manage the agents' accounts."), nil
		}
		if Status(err) == http.StatusForbidden {
			// THE SCOPED KEY, which is the likeliest 403 by far and the one
			// whose remedy nobody would guess from the status.
			return setup.Refused("Atlassian refused it for managing accounts. A key " +
				"created with scopes cannot create the agents' accounts, so create " +
				"one without scopes."), nil
		}
		var api *APIError
		reason := ""
		if errors.As(err, &api) {
			reason = api.Detail
		}
		return setup.Judge(setup.Failure{
			App: "Atlassian", Err: err, Status: Status(err), Reason: reason,
		}, key), nil
	}
}
