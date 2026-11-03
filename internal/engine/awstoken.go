package engine

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	awsint "github.com/crewlet/crewlet/internal/aws"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
)

// awsSeatIssuer issues each agent's AWS session at use: a certificate named
// for the agent opens the broker, which assumes the agent's own role for 15
// minutes. The session is never sealed; the box's credentials file holds it,
// rewritten as it renews.
type awsSeatIssuer struct {
	issuer *awsint.Issuer
}

func (awsSeatIssuer) kind() integration.Kind { return integration.KindAWS }

// awsLives is a seat session's: 15 minutes, renewed with 5 left, so a session
// that survives an off and an on lasts a quarter of an hour at most.
var awsLives = issuedLives{lifetime: time.Duration(awsint.SeatSeconds) * time.Second, renewBefore: 5 * time.Minute}

func (awsSeatIssuer) lives() issuedLives { return awsLives }

// variable is the agent's session variable while AWS is on and handle is an
// agent's, issuable now or not.
func (awsSeatIssuer) variable(c *Company, handle string) string {
	if c == nil || c.Org == nil || c.Org.AgentSeatByHandle(handle) == nil {
		return ""
	}
	if cfg := c.Config.Integrations.AWS; cfg == nil || !cfg.Enabled {
		return ""
	}
	return awsint.SeatVariable(handle)
}

// issuing is the agent's session, as credential_process prints it, while its
// seat is on at AWS.
func (a awsSeatIssuer) issuing(c *Company, seat *org.Role, env *config.Resolver) issueFunc {
	if c == nil || seat == nil || !awsint.SeatOn(c.Config, seat.Handle()) {
		return nil
	}
	handle := seat.Handle()
	return func(ctx context.Context, life time.Duration) (issuedToken, error) {
		creds, err := a.issuer.Session(ctx, c.Config, env, handle, life)
		if err != nil {
			return issuedToken{}, err
		}
		token, err := awsint.CredentialProcess(creds)
		if err != nil {
			return issuedToken{}, err
		}
		return issuedToken{Token: token, ExpiresAt: creds.Expires, RenewAt: creds.Expires.Add(-awsLives.renewBefore)}, nil
	}
}

// refusal is why an issue failed: a stack output not pasted yet is the
// integration's card's step, a throttle, a server error or a transport failure
// passes, and a refusal is the person's to fix.
func (awsSeatIssuer) refusal(err error) (string, bool) {
	if errors.Is(err, awsint.ErrNotConnected) || errors.Is(err, integration.ErrNotConfigured) {
		return "", false
	}
	status := awsint.Status(err)
	var unanswered *url.Error
	passes := status == http.StatusTooManyRequests || status >= http.StatusInternalServerError ||
		(status == 0 && errors.As(err, &unanswered))
	return err.Error(), passes
}

func (awsSeatIssuer) words() issuedWords {
	return issuedWords{
		source:  "the AWS session the engine issues for this seat",
		verb:    "issue",
		arrival: "the agent's IAM role exists and the AWS connection's stack output is pasted",
		refused: "it is not issued until somebody fixes the AWS connection or the agent's IAM role",
		retried: "AWS is asked for it again on the restart schedule",
	}
}
