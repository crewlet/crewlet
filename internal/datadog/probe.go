package datadog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
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
//  4. One read of each listing the atlas reads: monitors, the service
//     catalogue, hosts and Synthetic tests. A refusal here is a warning on
//     a working verdict, never a refusal of the keys ([atlasWarnings]).
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
		}, func(said setup.Fact) setup.Verdict {
			return setup.Refused("Datadog refused the API key.", said)
		}); failed {
			return v, nil
		}

		var org Org
		if v, failed := stage(ctx, creds, "the application key", func(ctx context.Context) error {
			var err error
			org, err = client.VerifyCredentials(ctx, creds)
			return err
		}, func(said setup.Fact) setup.Verdict {
			return setup.Refused("The API key is valid, but Datadog refused the application key.", said,
				setup.Warning{Action: "Check that it belongs to the organization in the region chosen above."})
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
			if v, failed := stage(ctx, creds, read.stage, read.call, func(said setup.Fact) setup.Verdict {
				return setup.Refused("Both keys are valid, but the application key lacks a permission "+
					"for "+read.needs+".", said)
			}); failed {
				return v, nil
			}
		}

		details := []setup.Detail{
			setup.Fact{Label: "Organization", Value: org.Name},
			setup.Fact{Label: "Site", Value: client.Site()},
			setup.Fact{Label: "They can read", Value: "Users, roles and webhooks"},
		}
		return setup.Works("Works. Both keys are valid.", append(details, atlasWarnings(ctx, client, creds)...)...), nil
	}
}

// atlasWarnings reads one row of each listing the atlas reads, and answers a
// warning for each the application key may not read.
//
// NEVER A REFUSAL OF THE KEYS: the integration works without any of them,
// and the atlas shows what it may read. Only a refusal is said; a listing
// that did not answer in time, or failed otherwise, says nothing about the
// keys and is left out. The four run together, inside what is left of the
// check's budget, so they cost a person one call's wait rather than four.
func atlasWarnings(ctx context.Context, client *Client, creds Credentials) []setup.Detail {
	listings := Listings()
	refused := make([]bool, len(listings))
	var wg sync.WaitGroup
	for i, l := range listings {
		wg.Go(func() {
			bounded, cancel := context.WithTimeout(ctx, probeCallTimeout)
			defer cancel()
			refused[i] = Status(client.CanList(bounded, creds, l)) == http.StatusForbidden
		})
	}
	wg.Wait()
	var out []setup.Detail
	for i, l := range listings {
		if refused[i] {
			title, action := l.Missing()
			out = append(out, setup.Warning{Title: title, Action: action})
		}
	}
	return out
}

// probeCallTimeout bounds one call of the staged check, inside the whole
// check's [setup.VerifyBudget]. Datadog's documented p99 is under two
// seconds, so this is slack for a slow region rather than a wait.
const probeCallTimeout = 5 * time.Second

// stage makes one call under the check's shared deadline and answers the
// verdict that ends the check when it fails: a refusal through refused (with
// Datadog's reason as a note), a timeout naming the stage, the rest Judge's.
func stage(
	ctx context.Context, creds Credentials, name string,
	call func(context.Context) error, refused func(said setup.Fact) setup.Verdict,
) (setup.Verdict, bool) {
	ctx, cancel := context.WithTimeout(ctx, probeCallTimeout)
	defer cancel()
	err := call(ctx)
	if err == nil {
		return setup.Verdict{}, false
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return setup.Unreachable("Datadog did not answer in time while checking " + name + "."), true
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
	return refused(setup.SaidBy("Datadog", reason, creds.APIKey, creds.AppKey)), true
}
