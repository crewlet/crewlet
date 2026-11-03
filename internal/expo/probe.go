package expo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// probeCallTimeout bounds one call of the check inside [setup.VerifyBudget].
// Expo answers these reads in well under a second, so five seconds is slack
// for a slow answer.
const probeCallTimeout = 5 * time.Second

// Probe checks a typed provisioner token against its account before anything
// stores it, with reads only: the token works, the account resolves by name,
// the token's actor holds Admin there, and the account's projects can be
// counted. Nothing at Expo is created or changed.
//
// httpClient is nil in production; a test hands one that reaches a fake.
func Probe(httpClient *http.Client) setup.Probe {
	return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
		if check.Field != "access_token" {
			return setup.Verdict{}, setup.NotCheckable("Expo", check.Field)
		}
		token := strings.TrimSpace(check.Value("access_token"))
		account := strings.TrimSpace(check.Value("account_name"))
		if token == "" {
			return setup.Verdict{}, setup.Unusable("Enter the provisioner token to test it.")
		}
		if account == "" {
			return setup.Verdict{}, setup.Unusable("Enter the account name, as it appears in " +
				"expo.dev/accounts/<name>: the token is checked against it.")
		}
		client, err := NewClient(ClientOptions{Token: token, HTTP: httpClient})
		if err != nil {
			return setup.Verdict{}, fmt.Errorf("expo: probe: %w", err)
		}
		ctx, cancel := context.WithTimeout(ctx, setup.VerifyBudget)
		defer cancel()
		in, err := inspect(ctx, client, account, func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, probeCallTimeout)
		})
		if err != nil {
			return probeVerdict(ctx, err, token, account), nil
		}
		if !in.admin() {
			return setup.Refused(fmt.Sprintf("The token works, but %s holds %s on %s, not the "+
				"Admin role that managing robot users needs. Give it the Admin role, or use a "+
				"token whose user or robot has it.", actorWords(in.viewer), permissionWords(in.perms),
				in.account.Name)), nil
		}
		return setup.Works(fmt.Sprintf("Works. The token acts as %s, which holds the Admin role "+
			"on %s, and the account has %s.", actorWords(in.viewer), in.account.Name,
			integration.Count(in.apps, "project"))), nil
	}
}

// probeVerdict is the verdict for a failed read: a refusal names the read it
// stopped at, and everything else is [setup.Judge]'s.
func probeVerdict(ctx context.Context, err error, token, account string) setup.Verdict {
	name := stageViewer.name
	st, staged := errors.AsType[*stageError](err)
	if staged {
		name = st.stage.name
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return setup.Verdict{Outcome: setup.VerifyUnreachable, Detail: "Expo did not answer in " +
			"time while checking " + name + ". Nothing was stored."}
	}
	reason := ""
	api, isAPI := errors.AsType[*APIError](err)
	if isAPI && api.Detail != noDetail {
		reason = api.Detail
	}
	status := Status(err)
	if staged && st.stage == stageAccount && (status == http.StatusNotFound || status == http.StatusForbidden) {
		return setup.Refused(fmt.Sprintf("The token works, but Expo shows it no account named %q. "+
			"Check the account name, or use a token from a member of that account.", account))
	}
	if isAPI && api.Code != "" && status == http.StatusBadRequest {
		// A GRAPHQL ERROR ABOUT THE REQUEST, not the address: Expo's schema
		// and this build's query disagree, which no credential settles.
		detail := "Expo could not answer the check reading " + name
		if said := setup.Sayable(reason, token); said != "" {
			detail += " (" + strings.TrimRight(said, ".") + ")"
		}
		return setup.Verdict{Outcome: setup.VerifyUnreachable, Detail: detail + ". Nothing was stored."}
	}
	verdict := setup.Judge(setup.Failure{App: "Expo", Err: err, Status: status}, "")
	if verdict.Outcome != setup.VerifyRefused {
		return verdict
	}
	sentence := "Expo refused the token"
	if staged && st.stage != stageViewer {
		sentence = "The token works, but Expo refused it reading " + st.stage.name
	}
	if said := setup.Sayable(reason, token); said != "" {
		sentence += ": " + strings.TrimRight(said, ".")
	}
	return setup.Refused(sentence + ".")
}
