package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/words"
)

// probeCallTimeout bounds one call of the check inside [setup.VerifyBudget].
// Cloudflare answers these reads in well under a second, so five seconds is
// slack for a slow edge.
const probeCallTimeout = 5 * time.Second

// Probe checks a typed provisioner token against its account before anything
// stores it, with reads only: the token is active, it can read the account's
// permission groups and zones, every zone the company names exists, every
// group an agent level grants resolves, and the token holds each of them
// itself.
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
		if !accountIDPattern().MatchString(account) {
			return setup.Verdict{}, setup.Unusable("Enter the account ID, 32 hexadecimal " +
				"characters from your account's overview page: the token is checked against it.")
		}
		client, err := NewClient(ClientOptions{AccountID: account, Token: token, HTTP: httpClient})
		if err != nil {
			return setup.Verdict{}, fmt.Errorf("cloudflare: probe: %w", err)
		}
		ctx, cancel := context.WithTimeout(ctx, setup.VerifyBudget)
		defer cancel()
		in, err := inspect(ctx, client, check.Zones, func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, probeCallTimeout)
		})
		if err != nil {
			return probeVerdict(ctx, err, token), nil
		}
		if missing := in.cat.missingZones; len(missing) > 0 {
			return setup.Refused("The token works, but it reads no zone named "+words.AndMore(missing, 3)+
				" in the account.", setup.Warning{Action: "Correct `integrations.cloudflare.provisioning.zones`, or give " +
				"the token access to the zone at Cloudflare."}), nil
		}
		if own := ownUnheld(in.unheld); len(own) > 0 {
			them := them(len(own))
			return setup.Refused("The token is missing "+words.And(dashboardNames(own))+".", setup.Warning{
				Title:  "Crewlet needs " + them + " to manage the agents' tokens.",
				Action: "Edit the token at Cloudflare to add " + them + ".",
			}), nil
		}
		// A LEVEL IT CANNOT GRANT is unavailable, not a broken token, unless it
		// is the default one: then no agent can be given a level at all.
		gaps := levelGaps(in.unresolved, in.unheld, "")
		if i := slices.IndexFunc(gaps, func(g seataccess.LevelGap) bool {
			return g.Level == config.CloudflareLevelRead
		}); i >= 0 {
			return setup.Refused("The token works, but no agent can be given a level yet.", setup.Warning{
				Title: gaps[i].Reason + ".", Action: gaps[i].Remedy + ".",
			}), nil
		}
		summary := "Works. The token holds every permission an agent can be given."
		details := []setup.Detail{setup.Fact{Label: "Zones", Value: strconv.Itoa(len(in.zones))}}
		if len(gaps) > 0 {
			summary = "Works. The token is active."
			for _, g := range gaps {
				details = append(details, setup.Warning{Title: g.Reason + ".", Action: g.Remedy + "."})
			}
		}
		details = append(details, setup.Expiry(in.status.ExpiresOn, time.Now().UTC())...)
		// A SUGGESTION, NEVER A REFUSAL (ADR-0145): the token works with or
		// without what the atlas could read more with, so it is said as a
		// fact beside a verdict it does not change.
		if said := seeMoreValue(in.atlasUnheld); said != "" {
			details = append(details, setup.Fact{Label: seeMoreLabel, Value: said})
		}
		return setup.Works(summary, details...), nil
	}
}

// probeVerdict is the verdict for a failed read: a refusal names the read and
// the permission it needs, and everything else is [setup.Judge]'s.
func probeVerdict(ctx context.Context, err error, token string) setup.Verdict {
	var st *stageError
	name := "the token"
	if errors.As(err, &st) {
		name = st.stage.name
	}
	if inactive, ok := errors.AsType[*inactiveError](err); ok {
		return setup.Refused(inactive.Error()+".", setup.Warning{Action: "Create or re-enable a token at Cloudflare."})
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return setup.Unreachable("Cloudflare did not answer in time while checking " + name + ".")
	}
	if TokenRefused(err) {
		return setup.Refused("This is not a valid Cloudflare API token.",
			setup.Warning{Action: "Paste only the token's value, from Copy on the token page."})
	}
	reason := ""
	api, isAPI := errors.AsType[*APIError](err)
	if isAPI && api.Detail != noDetail {
		reason = api.Detail
	}
	// THE ADDRESS IS FIXED (APIBase), so a 4xx other than a rate limit is
	// Cloudflare refusing what was sent, never an address that is wrong.
	refused := isAPI && api.Status >= http.StatusBadRequest && api.Status < http.StatusInternalServerError &&
		api.Status != http.StatusTooManyRequests
	verdict := setup.Judge(setup.Failure{App: "Cloudflare", Err: err, Status: Status(err), Refused: refused}, "")
	if verdict.Outcome != setup.VerifyRefused {
		return verdict
	}
	summary, details := "Cloudflare refused the token.", []setup.Detail{setup.SaidBy("Cloudflare", reason, token)}
	if st != nil && st.stage != stageVerify {
		summary = "The token is active, but Cloudflare refused it reading " + st.stage.name + "."
		if st.stage.needs != "" {
			details = append(details, setup.Warning{
				Title:  "Reading it needs " + st.stage.needs + ".",
				Action: "Edit the token at Cloudflare to add it.",
			})
		}
	}
	return setup.Refused(summary, details...)
}
