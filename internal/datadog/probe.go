package datadog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/setup"
)

// Probe checks a typed Datadog key pair before anything stores it.
//
// ONE CHECK FOR BOTH KEYS, asked about the application key, because Datadog
// accepts or refuses them together: the application key's requirement names
// the API key and the region in [setup.Requirement.CheckWith], and the API
// key is not checked on its own.
//
// # In stages, so a refusal names the key to replace
//
//  1. `/api/v1/validate` with the API key alone. A refusal is the API key's.
//  2. `/api/v1/org` with both. The API key is now known good, so a refusal
//     is the application key's, and an acceptance names the organization.
//  3. One read of each resource the reconcile manages: users, roles and the
//     configured webhook. A refusal is a permission the application key
//     lacks. An absent webhook is a read that worked, since the first pass
//     creates it.
//
// Every stage is a read, and nothing at Datadog is created or changed. That
// is also its limit: the WRITE permissions the reconcile needs (managing
// users and roles, writing the webhook) cannot be proved without writing, so
// the verdict says what was read and claims nothing more.
//
// # Budget
//
// The whole check runs under [setup.VerifyBudget], and each call under
// [probeCallTimeout] within it, so one slow stage cannot spend the budget of
// the stages after it unnoticed. A stage that runs out says which stage it
// was.
//
// httpClient is nil in production, which takes the client's own transport
// and timeout; a test hands one that reaches a fake region.
func Probe(httpClient *http.Client) setup.Probe {
	return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
		if check.Field != "app_key" {
			return setup.Verdict{}, setup.NotCheckable("Datadog", check.Field)
		}
		creds := Credentials{
			APIKey: strings.TrimSpace(check.Value("api_key")),
			AppKey: strings.TrimSpace(check.Value("app_key")),
		}
		if creds.APIKey == "" || creds.AppKey == "" {
			return setup.Verdict{}, setup.Unusable("Datadog checks the API key and " +
				"the application key together. Enter both to test them.")
		}
		site := check.Value("site")
		if !KnownSite(site) {
			return setup.Verdict{}, setup.Unusable("Choose the Datadog region your " +
				"organization is in: the keys are checked against that region.")
		}
		client, err := NewClient(ClientOptions{Site: site, HTTP: httpClient})
		if err != nil {
			// The site was checked above, so this is the constructor and
			// that check disagreeing, which is the engine's own fault.
			return setup.Verdict{}, fmt.Errorf("datadog: probe: %w", err)
		}
		webhook := strings.TrimSpace(check.Value("webhook_name"))
		if webhook == "" {
			webhook = DefaultWebhookName
		}

		ctx, cancel := context.WithTimeout(ctx, setup.VerifyBudget)
		defer cancel()

		if v, failed := stage(ctx, creds, "the API key", func(ctx context.Context) error {
			return client.ValidateAPIKey(ctx, creds.APIKey)
		}, func(reason string) string {
			return "Datadog refused the API key" + because(reason)
		}); failed {
			return v, nil
		}

		var org Org
		if v, failed := stage(ctx, creds, "the application key", func(ctx context.Context) error {
			var err error
			org, err = client.VerifyCredentials(ctx, creds)
			return err
		}, func(reason string) string {
			return "The API key is valid, but Datadog refused the application key" +
				because(reason)
		}); failed {
			return v, nil
		}

		for _, read := range []struct {
			stage, needs string
			call         func(context.Context) error
		}{
			{"access to users", "reading users", func(ctx context.Context) error {
				return client.CanReadUsers(ctx, creds)
			}},
			{"access to roles", "reading roles", func(ctx context.Context) error {
				return client.CanReadRoles(ctx, creds)
			}},
			{"access to webhooks", "reading webhooks", func(ctx context.Context) error {
				return client.CanReadWebhook(ctx, creds, webhook)
			}},
		} {
			if v, failed := stage(ctx, creds, read.stage, read.call, func(reason string) string {
				sentence := "Both keys are valid, but the application key lacks a " +
					"permission Crewlet needs: " + read.needs
				if reason != "" {
					sentence += " (" + strings.TrimRight(reason, ".") + ")"
				}
				return sentence
			}); failed {
				return v, nil
			}
		}

		where := "in " + client.Site()
		if name := strings.TrimSpace(org.Name); name != "" {
			where = "for the " + name + " organization in " + client.Site()
		}
		return setup.Works("Works. Both keys are valid " + where +
			", and can read users, roles and webhooks."), nil
	}
}

// probeCallTimeout bounds one call of the staged check, inside the whole
// check's [setup.VerifyBudget]. Datadog's documented p99 is under two
// seconds, so this is slack for a slow region rather than a wait.
const probeCallTimeout = 5 * time.Second

// stage makes one call under ctx, the deadline every stage of the check
// shares, and answers the verdict that ends the check, when it failed.
// refused writes the sentence for a refusal from Datadog's reason, which is
// "" when Datadog gave none fit to repeat; the reason is held against both
// of creds' keys.
//
// ONLY A REFUSAL IS THE STAGE'S OWN SENTENCE. Running out of time says which
// stage did, and everything else (a 5xx, a 429, a dead connection, an answer
// that is not Datadog's API) is [setup.Judge]'s, the rule every probe shares.
func stage(
	ctx context.Context, creds Credentials, name string,
	call func(context.Context) error, refused func(reason string) string,
) (setup.Verdict, bool) {
	ctx, cancel := context.WithTimeout(ctx, probeCallTimeout)
	defer cancel()
	err := call(ctx)
	if err == nil {
		return setup.Verdict{}, false
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return setup.Verdict{Outcome: setup.VerifyUnreachable, Detail: "Datadog did not " +
			"answer in time while checking " + name + ". Nothing was stored."}, true
	}
	var api *APIError
	reason := ""
	if errors.As(err, &api) && api.Detail != noDetail {
		reason = api.Detail
	}
	// The keys are held out of the reason here, so Judge is handed none:
	// none of its other sentences repeats anything Datadog said.
	verdict := setup.Judge(setup.Failure{App: "Datadog", Err: err, Status: Status(err)}, "")
	if verdict.Outcome != setup.VerifyRefused {
		return verdict, true
	}
	return setup.Refused(refused(setup.Sayable(reason, creds.APIKey, creds.AppKey)) + "."), true
}

// because is ": " and Datadog's reason, or nothing when it gave none fit
// to repeat.
func because(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + strings.TrimRight(reason, ".")
}
