package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/setup"
)

// Probe checks a typed bot token before anything stores it, with auth.test:
// the read every seat already makes at boot to learn who it is.
//
// httpClient is nil in production, which takes the client's own transport
// and timeout; a test hands one that reaches a fake Slack.
func Probe(httpClient *http.Client) setup.Probe {
	return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
		if check.Field != "bot_token" {
			return setup.Verdict{}, setup.NotCheckable("Slack", check.Field)
		}
		client, err := NewClient(check.Secret(), httpClient)
		if err != nil {
			return setup.Verdict{}, setup.Unusable("Enter the bot token to test it.")
		}
		id, err := client.AuthTest(ctx)
		if err != nil {
			return judge(err, check.Secret()), nil
		}
		// A TOKEN THAT WORKS AND IS NOT A BOT'S. A user token authenticates
		// too, and an agent posting with one posts as the person who
		// installed the app; auth.test names a bot only for a bot token.
		if id.BotID == "" {
			return setup.Refused("Slack accepted it, but it is not a bot token. " +
				"Copy the Bot User OAuth Token, which begins xoxb-."), nil
		}
		who := "the bot"
		if name := strings.TrimSpace(id.UserName); name != "" {
			who = "@" + name
		}
		if team := strings.TrimSpace(id.TeamName); team != "" {
			return setup.Works(fmt.Sprintf("Works. Slack accepted it as %s in the %s workspace.",
				who, team)), nil
		}
		return setup.Works("Works. Slack accepted it as " + who + "."), nil
	}
}

// refusalCodes are the auth.test errors that are Slack refusing the token,
// from Slack's own list for the method.
//
// EVERY OTHER CODE IS SLACK NOT SAYING: `internal_error`, `fatal_error`,
// `service_unavailable` and their kin are Slack's outage, and reporting one
// as a refusal sends somebody to regenerate a token that works.
//
//nolint:gochecknoglobals // an immutable table, not state
var refusalCodes = map[string]bool{
	"not_authed":             true,
	"invalid_auth":           true,
	"account_inactive":       true,
	"token_revoked":          true,
	"token_expired":          true,
	"no_permission":          true,
	"not_allowed_token_type": true,
	"org_login_required":     true,
	"ekm_access_denied":      true,
	"missing_scope":          true,
}

// judge classifies a failed auth.test.
//
// Slack answers every refusal as HTTP 200 with a code in the body, so the
// shared rule's status has nothing to read here and the code decides
// instead: a refusal code is a refusal, a rate limit is a 429, and anything
// else Slack answered is Slack unable to say.
func judge(err error, secret string) setup.Verdict {
	var limited *RateLimited
	if errors.As(err, &limited) {
		return setup.Judge(setup.Failure{
			App: "Slack", Err: err, Status: http.StatusTooManyRequests,
		}, secret)
	}
	var api *APIError
	if errors.As(err, &api) {
		if refusalCodes[api.Code] {
			return setup.Judge(setup.Failure{
				App: "Slack", Err: err, Refused: true, Reason: api.Code,
			}, secret)
		}
		return unsaid("Slack answered " + api.Code)
	}
	var transport *url.Error
	if errors.As(err, &transport) {
		return setup.Judge(setup.Failure{App: "Slack", Err: err}, secret)
	}
	// AN ANSWER THAT IS NOT THE WEB API'S ENVELOPE. The shared wording
	// blames the address for this, which is right where a person typed one
	// and wrong here: Slack's address is this package's constant, so what
	// came back is Slack (or a proxy in front of it) failing, typically an
	// outage page.
	return unsaid("Slack answered with something that is not its API")
}

// unsaid is Slack answering without saying anything about the token.
func unsaid(what string) setup.Verdict {
	return setup.Verdict{Outcome: setup.VerifyUnreachable,
		Detail: what + ", so it could not say whether this works. Nothing was stored."}
}
