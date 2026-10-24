package mattermost

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/setup"
)

// Probe checks a typed administrator token before anything stores it, with
// GET /users/me: who the token is, and whether that account holds the
// system_admin role every bot this integration creates needs.
//
// A TOKEN THAT AUTHENTICATES IS NOT ENOUGH, and that is why the role is read.
// A token from an account without system_admin works for every read and is
// refused on the first bot creation with a 403 naming the endpoint, minutes
// later, as a finding on the card. Here it is the answer to the test.
//
// httpClient and now are nil in production, which takes the client's own
// transport and clock; a test hands a transport reaching a fake server and a
// clock that ends the client's retry window at once.
func Probe(httpClient *http.Client, now func() time.Time) setup.Probe {
	return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
		if check.Field != "admin_token" {
			return setup.Verdict{}, setup.NotCheckable("Mattermost", check.Field)
		}
		base := strings.TrimSpace(check.Value("url"))
		if base == "" {
			return setup.Verdict{}, setup.Unusable("Enter your Mattermost server's " +
				"address: the token is checked against it.")
		}
		client, err := NewClient(ClientOptions{
			URL: base, Token: check.Secret(), HTTP: httpClient, Now: now,
		})
		if err != nil {
			return setup.Verdict{}, setup.Unusable(base + " is not a server address. " +
				"Enter it with its scheme, like https://chat.example.com.")
		}
		me, err := client.Me(ctx)
		if err != nil {
			var api *Error
			reason := ""
			if errors.As(err, &api) {
				reason = api.Message
			}
			return setup.Judge(setup.Failure{
				App: "Mattermost", Err: err, Status: Status(err), Reason: reason,
			}, check.Secret()), nil
		}
		who := "@" + me.Username
		if !me.SystemAdmin() {
			return setup.Refused("Mattermost accepted it for " + who + ", but that " +
				"account is not a system administrator, so it cannot create the " +
				"agents' bots. Use a token from a system administrator."), nil
		}
		return setup.Works("Works. Mattermost accepted it for " + who +
			", a system administrator."), nil
	}
}
