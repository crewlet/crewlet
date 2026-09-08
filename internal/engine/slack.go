package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/slack"
)

// The hosted chat surface, wired.
//
// # It has no lifecycle worth the name, and that is the difference
//
// The self-hosted chat backend holds one websocket per seat, because it has
// no usable inbound webhook — so its transport reconnects, backs off and
// backfills. Slack PUSHES: each seat's app posts to its own request URL, the
// API edge verifies the signature and the parser reads it. So starting the
// Slack transport is one auth.test per seat and nothing else, and there is
// no connection for an apply to drop.
//
// What an apply does have to redo is the identity resolution, because that
// is what a Slack payload names a seat by — and a seat whose id is unknown
// cannot recognise its own messages, which is the loop this integration
// costs the most to get wrong.

// startSlack brings up the hosted chat surface.
func (e *Engine) startSlack(ctx context.Context, c *Company, cfg *config.Slack) (*slack.Transport, error) {
	env := e.resolver()
	seats := slack.SeatsFrom(c.Org, env.LookupOK)
	if len(seats) == 0 {
		// Configured with no provisioned apps is a company mid-setup, not
		// a failure: `crewlet slack provision` has not run yet, or its
		// tokens have not reached this node's environment.
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
// A LIVE FACT, and the only kind available: an agent's Slack app is named
// nowhere in the company document, because the app is what issues the token
// rather than something the token points at. The transport learns it from
// `auth.test` when it wires the seat, so this answers for the seats that came
// up and is empty before they do.
//
// Read by the setup screen, which shows an operator which of their apps each
// agent is: the id every Slack settings page is keyed on, and the answer to
// the question a roster of identical-looking agents raises.
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
// revision.
//
// IT HAD NONE, and the gap is the one an operator meets first. The parser set
// is assembled once in [Engine.startNotifications], so a company that
// connected Slack after boot had every delivery verified at the webhook route
// and turned into work for nobody: `inbound_source_unparsed` in the log,
// `routes nowhere` on the card, and an agent that answers nothing, until the
// process was restarted. Connecting from the dashboard is exactly that
// sequence, and so is rotating a seat's token.
//
// REBUILT ON EVERY APPLY, unlike the self-hosted surface next door. What is
// torn down here is an HTTP client and a working-indicator driver, because
// Slack's inbound half is this engine's own API edge and there is no socket
// to drop; [Engine.reconcileMattermost] holds one per seat and is guarded
// accordingly.
func (e *Engine) reconcileSlack(ctx context.Context, c *Company) {
	cfg := c.Config.Integrations.Slack
	e.notify.mu.Lock()
	svc, previous := e.notify.service, e.notify.slack
	e.notify.mu.Unlock()
	if svc == nil {
		return
	}
	// RETIRED when the revision no longer declares it, the same posture
	// every other reconciler here takes: an operator who removes the block
	// after a leak must not be left with the boot-time parser routing
	// deliveries under the credential being revoked.
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
		// THE PREVIOUS TRANSPORT KEEPS RUNNING, the same posture as the
		// code host's: routing by a stale credential is worse than the
		// new one and much better than not routing at all. startSlack
		// stores what it built before starting it, so the field is put
		// back rather than left pointing at something that did not come
		// up.
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
		// AFTER the swap, so no delivery falls between the two: what
		// Stop does here is clear working indicators the old transport
		// raised, and one left up outlives the process visibly.
		previous.Stop(ctx)
	}
	log.InfoContext(ctx, "slack_reconciled", "company", c.Config.Name)
}

// statusPhrases maps the config block onto the phrase registry's own shape.
//
// The MAPPING IS THE POINT and it belongs here rather than in either
// package: config states the phases as fields because a YAML author needs
// them named and checked, while the registry keys on the phase strings the
// turn engine actually reports. Written as a map literal in config, a
// misspelt phase would be silently accepted and silently ignored.
func statusPhrases(p config.StatusPhrases) map[string][]string {
	return map[string][]string{
		"onboarding":        p.Onboarding,
		"execute":           p.Execute,
		"review":            p.Review,
		notify.DefaultPhase: p.Default,
	}
}
