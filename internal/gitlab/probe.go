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
// TWO READS, both read-only. GET /user is who the token is, for every kind on
// every supported GitLab. GET /personal_access_tokens/self is what it may do
// and until when, only on 15.5+ for a personal, group or project token; where
// it does not answer, the check stands on /user rather than failing the token.
//
// THE SCOPE IS READ BECAUSE A TOKEN WITHOUT IT AUTHENTICATES: the access token
// needs `api`, the routing token `read_api` (or `api`); `read_user` passes /user.
//
// httpClient is nil in production, which takes the client's own transport
// and timeout; a test hands one that reaches a fake instance.
func Probe(httpClient *http.Client) setup.Probe {
	return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
		var needs []string
		var purpose string
		switch check.Field {
		case "admin_token":
			needs, purpose = []string{scopeAPI}, "create the agents' service accounts"
		case "token":
			needs, purpose = []string{scopeReadAPI, scopeAPI}, "read who takes part in a thread"
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
			return setup.Works("Works. GitLab accepted it.", setup.Fact{Label: "Account", Value: user}), nil
		}
		if !slices.ContainsFunc(needs, func(scope string) bool {
			return slices.Contains(self.Scopes, scope)
		}) {
			return setup.Refused("GitLab accepted it for "+user+", but it does not carry the "+needs[0]+" scope.",
				setup.Warning{Title: "Without it, Crewlet cannot " + purpose + ".", Action: "Create a token with that scope."}), nil
		}
		details := []setup.Detail{setup.Fact{Label: "Account", Value: user}}
		details = append(details, setup.Expiry(self.ExpiresAt.Time, time.Now().UTC())...)
		return setup.Works("Works. GitLab accepted it.", details...), nil
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
