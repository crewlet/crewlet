package engine

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/slack"
)

// The hosted chat surface, wired. It has no lifecycle to speak of: unlike the
// self-hosted backend's websocket per seat, Slack pushes each delivery to the
// API edge, which verifies it, so starting the transport is one auth.test per
// seat and an apply has no connection to drop. What an apply must redo is the
// identity resolution a payload names a seat by: a seat whose id is unknown
// cannot recognise its own messages, and answers itself in a loop.

// startSlack brings up the hosted chat surface.
func (e *Engine) startSlack(ctx context.Context, c *Company, cfg *config.Slack) (*slack.Transport, error) {
	env := e.resolver()
	seats := slack.SeatsFrom(c.Org, env.LookupOK)
	if len(seats) == 0 {
		// No installed app is a company mid-setup, not a failure: no install
		// approved yet, or its token has not reached this node.
		log.InfoContext(ctx, "slack_configured_with_no_apps",
			"detail", "no seat's integrations.slack.bot_token resolved, so "+
				"nothing sends or receives on this surface")
		return nil, nil
	}
	transport, err := slack.NewTransport(slack.TransportOptions{
		Config: slack.Config{
			Status:  notify.StatusMode(cfg.Status()),
			Phrases: notify.NewPhrases(statusPhrases(cfg.StatusPhrases)),
			Seats:   seats,
		},
		Follows:  e.followStore(),
		Registry: e.Registry,
	})
	if err != nil {
		return nil, err
	}
	e.notify.mu.Lock()
	e.notify.slack = transport
	e.notify.mu.Unlock()

	if err := transport.Start(ctx); err != nil {
		return transport, err
	}
	return transport, nil
}

// SlackApps is the Slack app each running seat authenticates as, by handle.
//
// A live fact, learned from `auth.test` as each seat is wired, so empty before
// the seats come up: the company document never names an agent's app, since
// the app issues the token. The setup screen shows it so an operator can tell
// which of their apps each agent is.
func (e *Engine) SlackApps() map[string]string {
	e.notify.mu.Lock()
	transport := e.notify.slack
	e.notify.mu.Unlock()
	if transport == nil {
		return nil
	}
	return transport.Apps()
}

// reconcileSlack brings the hosted chat surface in line with the applied
// revision. [Engine.startNotifications] assembles the parsers once, so without
// this a Slack connected after boot, or a seat's rotated token, routes
// nowhere (`inbound_source_unparsed`) until a restart.
//
// Rebuilt on every apply, unlike [Engine.reconcileMattermost]'s socket per
// seat: what is torn down is an HTTP client and a working-indicator driver,
// since Slack's inbound half is this engine's own API edge.
func (e *Engine) reconcileSlack(ctx context.Context, c *Company) {
	cfg := c.Config.Integrations.Slack
	e.notify.mu.Lock()
	svc, previous := e.notify.service, e.notify.slack
	e.notify.mu.Unlock()
	if svc == nil {
		return
	}
	// Retired when the revision drops the block, as every reconciler here
	// does: an operator who removes it after a leak must not have deliveries
	// still routed under the credential being revoked.
	if cfg == nil {
		e.notify.mu.Lock()
		e.notify.slack = nil
		e.notify.mu.Unlock()
		if previous != nil {
			previous.Stop(ctx)
		}
		if svc.Unregister(slack.Backend) {
			log.InfoContext(ctx, "slack_retired",
				"detail", "the revision no longer configures slack; its deliveries "+
					"are refused at the webhook route and route to no seat")
		}
		return
	}
	transport, err := e.startSlack(ctx, c, cfg)
	if err != nil || transport == nil {
		// The previous transport keeps running, as the code host's does:
		// a stale credential beats not routing at all. startSlack stores
		// what it built before starting it, so the field is put back.
		e.notify.mu.Lock()
		e.notify.slack = previous
		e.notify.mu.Unlock()
		if err != nil {
			log.ErrorContext(ctx, "slack_reconcile_failed", "error", errorText(err),
				"detail", "the previous hosted chat wiring is still current")
		}
		return
	}
	if err := svc.Replace(transport.Parser(), transport.Prompt()); err != nil {
		log.ErrorContext(ctx, "slack_reconcile_failed", "error", err.Error(),
			"detail", "the previous hosted chat wiring is still current")
		return
	}
	if previous != nil && previous != transport {
		// After the swap, so no delivery falls between the two. Stop
		// clears the old transport's working indicators, which would
		// otherwise stay up visibly.
		previous.Stop(ctx)
	}
	log.InfoContext(ctx, "slack_reconciled", "company", c.Config.Name)
}

// statusPhrases maps the config block onto the phrase registry's shape, here
// rather than in either package: config names the phases as fields so a
// misspelt one is refused (a map in config would silently ignore it), while
// the registry keys on the phase strings the turn engine reports.
func statusPhrases(p config.StatusPhrases) map[string][]string {
	return map[string][]string{
		"onboarding":        p.Onboarding,
		"execute":           p.Execute,
		"review":            p.Review,
		notify.DefaultPhase: p.Default,
	}
}

// SlackInstalls mints seats' Slack install links and finishes the installs
// they return to, for the setup surface and the webhook landing alike, so a
// link minted on one is finished by the other. Every part is the fleet's (the
// state signed under the shared keyring, its nonce on the fleet's claim
// registry, the bot token sealed into the company's secret store), because
// Slack rarely returns the browser to the node that minted the link.
func (e *Engine) SlackInstalls(material runtoken.Material) (*slack.Installs, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil, errors.New("engine: this engine holds no fleet store to record " +
			"Slack install links in; it was not built by engine.New")
	}
	return slack.NewInstalls(slack.InstallOptions{
		Material: material,
		Claims:   e.backends.Fleet,
		Base:     func() string { return e.webhookBase },
		Resolve:  func(ref string) string { return e.resolver().Value(ref) },
		Sink: func(context.Context) (provision.TokenSink, error) {
			return e.SetupSink(loopActor())
		},
	})
}
