package jira

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/setup"
)

// Probe checks a typed Jira token before anything stores it, with the read
// every Jira client already makes to learn who it is ([Client.MyName]).
//
// THE ADDRESS AND THE SCHEME ARE THE ENGINE'S OWN RULES, not this probe's:
// the REST base is [config.Jira.BaseURL] over the form's site address, cloud
// id and link address, and which deployment that base is on, and so which
// API version and which credential scheme it takes, is [DeploymentOf] over
// the company's declared Atlassian deployment ([setup.Check.Declared]),
// exactly as every client built for the company decides it.
//
// httpClient is nil in production, which takes the client's own transport
// and timeout; a test hands one that reaches a fake site.
func Probe(httpClient *http.Client) setup.Probe {
	return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
		if check.Field != "token" {
			return setup.Verdict{}, setup.NotCheckable("Jira", check.Field)
		}
		base := (&config.Jira{
			URL: check.Value("url"), CloudID: check.Value("cloud_id"),
			SiteURL: check.Value("site_url"),
		}).BaseURL()
		if strings.TrimSpace(base) == "" {
			return setup.Verdict{}, setup.Unusable("There is no Jira site to check the " +
				"token against yet. Connect Atlassian so the site is discovered, or " +
				"enter the Jira site address.")
		}
		email := strings.TrimSpace(check.Value("email"))
		// CLOUD TAKES THE TOKEN WITH ITS ACCOUNT'S EMAIL and refuses it as a
		// bearer, so checking a Cloud token without the email would report a
		// working token refused.
		if email == "" && DeploymentOf(base, check.Declared) == Cloud {
			return setup.Verdict{}, setup.Unusable("Atlassian Cloud checks the token " +
				"together with its account's email. Enter the email too.")
		}
		client, err := NewClient(ClientOptions{
			URL: base, Email: email, Token: check.Secret(),
			Declared: check.Declared, HTTP: httpClient,
		})
		if err != nil {
			return setup.Verdict{}, setup.Unusable("Enter the token to test it.")
		}
		name, err := client.MyName(ctx)
		if err != nil {
			var api *APIError
			reason := ""
			if errors.As(err, &api) {
				reason = setup.ReasonIn(api.Detail, "errorMessages", "message")
			}
			return setup.Judge(setup.Failure{
				App: "Jira", Err: err, Status: Status(err), Reason: reason,
			}, check.Secret()), nil
		}
		return setup.Works("Works. Jira accepted it for " + name + "."), nil
	}
}
