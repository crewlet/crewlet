package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/slack"
)

// NewSetupSurface builds a node's setup surface (`/setup`) over its engine, the
// Tier A it runs under, the config surface it writes the company document
// through, the keyring every node holds, and the fleet's Slack install links.
//
// ONE CONSTRUCTION FOR EVERY CALLER, as [NewHumanSurfaces] is: `crewlet run`
// and the fleet suite both build the surface here, so a seam it requires is
// wired once and cannot be added to one composition root and missed in the
// other. Each such miss used to surface only as [setupapi.New] refusing by
// name at run time, in the one suite that builds a fleet.
func NewSetupSurface(boot *config.Bootstrap, e *engine.Engine, configSurface *configapi.Service,
	cipher secrets.Cipher, installs *slack.Installs) (*setupapi.Service, error) {

	switch {
	case boot == nil || e == nil || configSurface == nil || cipher == nil:
		return nil, errors.New("api: the setup surface needs the Tier A file, the engine, " +
			"the config surface and the keyring")
	case installs == nil:
		return nil, errors.New("api: the setup surface needs the fleet's Slack install " +
			"links: an agent's Slack app is installed through one")
	}
	// The fleet's integration status, which both the reconcile loop and a
	// pass run from the dashboard write.
	status, err := e.IntegrationStore()
	if err != nil {
		return nil, fmt.Errorf("api: integration status: %w", err)
	}
	// Connecting an integration from the dashboard. It writes through the
	// TWO surfaces it is handed rather than reaching for the store and the
	// plane itself: a credential is sealed by the same store /secrets
	// serves, and the pointer to it lands through the same merge,
	// validation and activation PATCH /config performs.
	fleetStore := fleetsecrets.New(e.Backends().Fleet, cipher)
	return setupapi.New(setupapi.Options{
		Config: configSurface,
		// The fleet's own store, sealed with the same keyring: the one
		// every node holds, since Tier A refuses a file without it.
		Secrets: fleetStore,
		// WHAT EACH AGENT MAY DO AT EACH VENDOR: the Agents tab's answer,
		// plan and sync, over the engine's own plan, ledger and lease, and
		// each agent's Slack install link.
		Access: e.SeatAccessAPI(configSurface.FleetCompany, fleetStore, configSurface.Managed).
			UseSlackInstalls(slackInstallLink(installs), slack.AwaitsInstall),
		// THIS NODE'S resolution chain, so a requirement can say whether
		// a ${VAR} actually resolved rather than only whether somebody
		// wrote one down. That gap is the silent outage the whole
		// secret_usable family exists to surface.
		Resolve: e.LookupSecret,
		// WHERE A VENDOR REACHES THIS DEPLOYMENT (the public listener's
		// address where one is configured, `api.external_url` otherwise)
		// from Tier A rather than through the resolver above: it cannot
		// change under a running process, so this surface can never answer
		// two different addresses inside one request. And the key it is
		// read from, which every refusal about the address names.
		WebhookBase:    boot.API.WebhookBase(),
		WebhookBaseKey: boot.API.WebhookBaseKey(),
		// The third-party apps this build can provision over the API, the
		// recorder their minted credentials go through, and the fleet row
		// a pass writes its findings to. That last one is the SAME row the
		// reconcile loop writes: a pass an operator ran and a tick that
		// ran a minute later must not disagree about an integration.
		Passes: e.SetupRunner(),
		Sink:   e.SetupSink,
		Status: status,
		// AND THE CHECK A PERSON CAN MAKE BEFORE ANY OF THAT: one
		// read-only identity call per vendor, with a credential typed and
		// not yet stored.
		Probes: engine.SetupProbes(),
		// WHICH SLACK APP EACH AGENT IS. Named nowhere in the company
		// document, because the app is what issues the token; the
		// running transport learned it from Slack when it wired the
		// seat, and this is the only process that holds it.
		SlackApps: e.SlackApps,
		// ONE AGENT'S OWN GITHUB APP, which is the one thing here a
		// reconcile loop cannot do alone: an app is created by POSTing a
		// manifest from a page carrying the operator's own GitHub session.
		// The state ties the browser that comes back to the seat that
		// started and to who started it, sealed under the SAME keyring
		// every node holds, so a fleet where the two halves land on
		// different nodes still agrees.
		StateCipher: cipher,
		// And the fleet's claim registry SPENDS each state, so a link that
		// reached a log or a browser history cannot be presented a second
		// time, on this node or any other.
		StateClaims: e.Backends().Fleet,
	})
}

// slackInstallLink is Slack's install-link mint as the Agents tab's install
// action takes it: Slack's own refusals given in the access routes' terms, so
// the route answers each with Slack's reason and remedy.
func slackInstallLink(installs *slack.Installs) func(context.Context, string, string) (string, error) {
	return func(ctx context.Context, handle, appID string) (string, error) {
		url, err := installs.Link(ctx, handle, appID)
		var refusal *slack.InstallRefusal
		switch {
		case errors.As(err, &refusal):
			return "", &setupapi.AccessRefusal{Status: refusal.Status, Reason: refusal.Reason,
				Remedy: refusal.Remedy, RetryAfter: refusal.RetryAfter}
		case errors.Is(err, slack.ErrNoBaseURL):
			return "", &setupapi.AccessRefusal{Status: http.StatusConflict, Reason: err.Error(),
				Remedy: "set the address a vendor reaches this deployment on, and restart"}
		}
		return url, err
	}
}
