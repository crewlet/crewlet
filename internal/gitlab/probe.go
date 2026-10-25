package gitlab

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/setup"
)

// Probe checks a typed GitLab token before anything stores it.
//
// TWO READS, both read-only. GET /user is who the token is, and it answers
// for every kind of token on every GitLab this engine supports. GET
// /personal_access_tokens/self is what the token may do and until when, and
// it answers only on GitLab 15.5 and later and only for a personal, group or
// project token: where it does not answer, the check stands on /user alone
// rather than failing a token that works.
//
// THE SCOPE IS READ BECAUSE A TOKEN WITHOUT IT AUTHENTICATES. The access
// token creates the agents' service accounts and needs `api`; the routing
// token reads threads and needs `read_api` (or `api`, which includes it). A
// `read_user` token passes /user and fails every call that matters, minutes
// later, as a finding on the card.
//
// httpClient is nil in production, which takes the client's own transport
// and timeout; a test hands one that reaches a fake instance.
func Probe(httpClient *http.Client) setup.Probe {
	return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
		var needs []string
		var purpose string
		switch check.Field {
		case "admin_token":
			needs, purpose = []string{"api"}, "create the agents' service accounts"
		case "token":
			needs, purpose = []string{"read_api", "api"}, "read who takes part in a thread"
		default:
			return setup.Verdict{}, setup.NotCheckable("GitLab", check.Field)
		}
		base := strings.TrimSpace(check.Value("url"))
		if base == "" {
			return setup.Verdict{}, setup.Unusable("Enter your GitLab instance's " +
				"address: the token is checked against it.")
		}
		client, err := NewClient(ClientOptions{URL: base, Token: check.Secret(), HTTP: httpClient})
		if err != nil {
			return setup.Verdict{}, setup.Unusable("Enter the token to test it.")
		}
		user, err := client.Me(ctx)
		if err != nil {
			return judge(err, check.Secret()), nil
		}
		self, err := client.SelfToken(ctx)
		if err != nil {
			// THE TOKEN WORKS. What did not answer is the description of
			// it, which an older instance or a non-personal token does not
			// serve, so the verdict is the identity's alone.
			//nolint:nilerr // Deliberate: see the paragraph above.
			return setup.Works("Works. GitLab accepted it for the account " + user + "."), nil
		}
		if !slices.ContainsFunc(needs, func(scope string) bool {
			return slices.Contains(self.Scopes, scope)
		}) {
			return setup.Refused("GitLab accepted it for " + user + ", but it does not " +
				"carry the " + needs[0] + " scope, so it cannot " + purpose +
				". Create a token with that scope."), nil
		}
		detail := "Works. GitLab accepted it for the account " + user
		if !self.ExpiresAt.IsZero() {
			detail += ", and it expires on " + self.ExpiresAt.Format(time.DateOnly)
		}
		return setup.Works(detail + "."), nil
	}
}

// judge classifies a failed /user.
func judge(err error, secret string) setup.Verdict {
	var api *APIError
	reason := ""
	if errors.As(err, &api) {
		// GitLab's refusals carry a sentence under `message` and an OAuth
		// one under `error_description`.
		reason = setup.ReasonIn(api.Detail, "message", "error_description", "error")
	}
	return setup.Judge(setup.Failure{
		App: "GitLab", Err: err, Status: Status(err), Reason: reason,
	}, secret)
}
