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
