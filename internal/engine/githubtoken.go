package engine

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
)

// An agent's own GitHub App token: an installed app's installation token
// ([github.Issuer]), named `${GITHUB_APP_TOKEN_<HANDLE>}` ([github.SeatVar])
// and issued on the [seatIssuer] contract (issued.go). Three stories:
//
//   - APP NOT CREATED OR INSTALLED, or its key unresolved
//     ([github.ErrNotInstalled]): a person's step, on the integration's card.
//   - MINT REFUSED or GitHub unreached: the issuer holds it for the restart
//     schedule's wait ([github.Issuer.RetryWait], [Engine.seatRetryDelay]);
//     [mintPasses] decides which the failure says.
//   - GITHUB NOT ENABLED: nothing is issued.

// githubSeatIssuer issues each agent its own app's installation token through
// the process's one [github.Issuer], so an installation costs one mint an hour
// however many readers ask.
type githubSeatIssuer struct {
	issuer *github.Issuer
}

func (githubSeatIssuer) kind() integration.Kind { return integration.KindGitHub }

// variable is [githubIssued].
func (githubSeatIssuer) variable(c *Company, handle string) string { return githubIssued(c, handle) }

// issuing mints seat's token against its own installation, narrowed to the
// seat's tier and repositories, from the app's key resolved through env.
func (g githubSeatIssuer) issuing(c *Company, seat *org.Role, env *config.Resolver) issueFunc {
	// AN OFF SEAT IS ISSUED NOTHING (D1): its installation is suspended, and
	// a token minted before the suspension landed must not be handed on.
	if seat.GitHub == nil || !seat.GitHub.Enabled {
		return nil
	}
	api, ok := githubIssuing(c, env)
	if !ok {
		return nil
	}
	opts, app := github.SeatAppOptions{APIBase: api}, githubSeatApp(seat, env)
	return func(ctx context.Context, life time.Duration) (issuedToken, error) {
		token, err := g.issuer.Token(ctx, opts, app, life)
		if err != nil {
			return issuedToken{}, err
		}
		return issuedToken{Token: token.Token, ExpiresAt: token.ExpiresAt, RenewAt: github.RenewAt(token)}, nil
	}
}

// refusal is the issuer's error and [mintPasses], or "" for an app not yet
// created or installed: that is the waiting story, which the GitHub
// integration's own record tells.
func (githubSeatIssuer) refusal(err error) (string, bool) {
	if errors.Is(err, github.ErrNotInstalled) {
		return "", false
	}
	return err.Error(), mintPasses(err)
}

func (githubSeatIssuer) words() issuedWords {
	return issuedWords{
		source:  "the GitHub App token the engine issues for this seat",
		verb:    "issue",
		arrival: "the agent's GitHub App is installed",
		refused: "it is not issued until somebody fixes the agent's GitHub App (its key, its " +
			"installation or the permissions it asks for)",
		// NOT "at the next attempt": an attempt inside the restart wait gets
		// the held answer without a request ([github.Issuer.RetryWait]).
		retried: "GitHub is asked for it again on the restart schedule",
	}
}

// mintPasses reports whether a mint that failed with err is one a wait clears:
// GitHub not reached (a request that got no answer, its own timeout among
// them), or answering that it could not just now (408, its rate limit, which
// it answers 429 or 403 ([github.APIError.RateLimited]), a 5xx).
//
// ANYTHING ELSE IS A PERSON'S, refused at every ask until fixed: an answer
// refusing the app (401, 403, 404, 422: a bad key, a suspended or removed
// installation, an unapproved permission), and a key the engine cannot sign
// with. Misread either way, a suspended app is promised a retry, or a blip
// sends an operator to fix a healthy app.
func mintPasses(err error) bool {
	var answered *github.APIError
	if errors.As(err, &answered) {
		return answered.RateLimited || answered.Status == http.StatusRequestTimeout ||
			answered.Status >= http.StatusInternalServerError
	}
	var unanswered *url.Error
	return errors.As(err, &unanswered)
}

// githubIssuing reports whether this company issues its agents' GitHub App
// tokens, with the REST base it issues them against. Only while GitHub is
// enabled: switching the block off is how an operator stops every token.
func githubIssuing(c *Company, env *config.Resolver) (string, bool) {
	cfg := c.Config.Integrations.GitHub
	if cfg == nil || !cfg.Enabled {
		return "", false
	}
	resolved := *cfg
	resolved.URL = strings.TrimSpace(env.Value(cfg.URL))
	return resolved.APIBase(), true
}

// githubIssued is the variable the GitHub integration issues for handle's
// seat, or "" where it issues none (no GitHub, or not an agent's handle).
// Declared for EVERY agent seat, app or not: a seat naming its token before
// its app exists is waiting on the card's step, not misconfigured.
func githubIssued(c *Company, handle string) string {
	if c == nil {
		return ""
	}
	if cfg := c.Config.Integrations.GitHub; cfg == nil || !cfg.Enabled {
		return ""
	}
	if c.Org == nil || c.Org.AgentSeatByHandle(handle) == nil {
		return ""
	}
	return github.SeatVar(handle, github.FieldAppToken)
}
