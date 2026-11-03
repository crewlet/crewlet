package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/words"
)

// probeCallTimeout bounds one call of the check inside [setup.VerifyBudget].
// Cloudflare answers these reads in well under a second, so five seconds is
// slack for a slow edge.
const probeCallTimeout = 5 * time.Second

// Probe checks a typed provisioner token against its account before anything
// stores it, with reads only: the token is active, it can read the account's
// permission groups and zones, every group an agent level grants resolves,
// and the token holds each of them itself.
//
// httpClient is nil in production; a test hands one that reaches a fake.
func Probe(httpClient *http.Client) setup.Probe {
	return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
		if check.Field != "api_token" {
			return setup.Verdict{}, setup.NotCheckable("Cloudflare", check.Field)
		}
		token := strings.TrimSpace(check.Value("api_token"))
		account := strings.TrimSpace(check.Value("account_id"))
		if token == "" {
			return setup.Verdict{}, setup.Unusable("Enter the provisioner token to test it.")
		}
		if !accountIDPattern.MatchString(account) {
			return setup.Verdict{}, setup.Unusable("Enter the account ID, 32 hexadecimal " +
				"characters from your account's overview page: the token is checked against it.")
		}
		client, err := NewClient(ClientOptions{AccountID: account, Token: token, HTTP: httpClient})
		if err != nil {
			return setup.Verdict{}, fmt.Errorf("cloudflare: probe: %w", err)
		}
		ctx, cancel := context.WithTimeout(ctx, setup.VerifyBudget)
		defer cancel()
		in, err := inspect(ctx, client, nil, func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, probeCallTimeout)
		})
		if err != nil {
			return probeVerdict(ctx, err, token), nil
		}
		if len(in.unresolved) > 0 {
			var parts []string
			for _, level := range Levels() {
				if missing := in.unresolved[level]; len(missing) > 0 {
					parts = append(parts, fmt.Sprintf("%s for %s", words.And(missing), LevelName(level)))
				}
			}
			return setup.Refused("The token works, but Cloudflare lists no permission group a " +
				"token can be granted named " + strings.Join(parts, "; ") + "."), nil
		}
		if len(in.unheld) > 0 {
			them := "them"
			if len(in.unheld) == 1 {
				them = "it"
			}
			return setup.Refused("The token works, but it is missing " + words.And(in.unheld) +
				", which an agent level grants. Edit the token at Cloudflare to add " + them +
				": Crewlet grants an agent nothing the token does not hold itself."), nil
		}
		detail := fmt.Sprintf("Works. The token is active, reads %s in the account and holds "+
			"every permission an agent level grants", countZones(len(in.zones)))
		if !in.status.ExpiresOn.IsZero() {
			detail += ". It expires on " + in.status.ExpiresOn.Format(time.DateOnly)
		}
		return setup.Works(detail + "."), nil
	}
}

// countZones is "1 zone" or "N zones".
func countZones(n int) string { return integration.Count(n, "zone") }

// probeVerdict is the verdict for a failed read: a refusal names the read and
// the permission it needs, and everything else is [setup.Judge]'s.
func probeVerdict(ctx context.Context, err error, token string) setup.Verdict {
	var st *stageError
	name := "the token"
	if errors.As(err, &st) {
		name = st.stage.name
	}
	if inactive, ok := errors.AsType[*inactiveError](err); ok {
		return setup.Refused(inactive.Error() + ". Create or re-enable a token at Cloudflare.")
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return setup.Verdict{Outcome: setup.VerifyUnreachable, Detail: "Cloudflare did not " +
			"answer in time while checking " + name + ". Nothing was stored."}
	}
	reason := ""
	if api, ok := errors.AsType[*APIError](err); ok && api.Detail != noDetail {
		reason = api.Detail
	}
	verdict := setup.Judge(setup.Failure{App: "Cloudflare", Err: err, Status: Status(err)}, "")
	if verdict.Outcome != setup.VerifyRefused {
		return verdict
	}
	sentence := "Cloudflare refused the token"
	if st != nil && st.stage != stageVerify {
		sentence = "The token is active, but Cloudflare refused it reading " + st.stage.name
		if st.stage.needs != "" {
			sentence += ", which needs " + st.stage.needs
		}
	}
	if said := setup.Sayable(reason, token); said != "" {
		sentence += ": " + strings.TrimRight(said, ".")
	}
	return setup.Refused(sentence + ".")
}
