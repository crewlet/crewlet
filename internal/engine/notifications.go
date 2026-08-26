package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/slack"
)

// The inbound edge, wired.
//
// # Why the registry is rebuilt per epoch and the transports are not
//
// A party registry is DERIVED from one org and answers for it permanently —
// so an apply builds a new one, and everything reading parties reads it
// through a function rather than holding it. A transport is the opposite: it
// holds live sockets and resolved vendor identities, and rebuilding it on
// every apply would drop every connection whenever an unrelated field
// changed. So the registry is swapped and the transports are reconciled.

// notifications is this node's inbound machinery.
type notifications struct {
	mu       sync.Mutex
	registry *notify.Registry
	admits   notify.Admitter

	service    *notify.Service
	mattermost *mattermost.Transport

	// slack is the hosted chat surface. A company may run BOTH — they are
	// different workspaces with different people in them, and an org
	// migrating from one to the other runs both for a while — so this is
	// a second field rather than a choice between two.
	slack *slack.Transport

	// plane is the tracker's contribution: a parser and a knowledge
	// searcher. No lifecycle, because Plane is inbound-only — there is no
	// connection to lose, so an unreachable instance degrades the reads
	// that enrich routing and never takes a surface down.
	plane planeParts

	// jira remembers which account each seat credential authenticates as.
	// Outside the mutex above for the same reason the code host's is: it
	// OUTLIVES an epoch, because identity is a function of the credential
	// and a revision that changed something else must not re-spend a
	// request per seat to re-learn what it already knows.
	jira jiraIdentities

	// gitlab remembers which account each seat credential authenticates
	// as. Outside the mutex above because it has its own, and because it
	// OUTLIVES an epoch: identity is a function of the credential, so a
	// revision that changed something else must not re-spend a request
	// per seat to re-learn what it already knows.
	gitlab gitlabIdentities
}

// Registry is the live party registry.
//
// NEVER NIL, and structurally so rather than by a guard here: the engine
// indexes its first company during construction, before it returns and so
// before anything can ask. A check on the read would suggest a window that
// does not exist.
func (e *Engine) Registry() *notify.Registry {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	return e.notify.registry
}

// RoutedSources lists the integrations whose deliveries can actually wake a
// seat on this node, sorted.
//
// NOT the integrations that are CONFIGURED, which is the distinction the
// whole method exists for. A vendor's webhook route verifies and stores its
// deliveries as soon as its block is present; whether one then reaches an
// agent depends on a parser existing, and four vendors have the first half
// and not the second. On every operator surface those look identical —
// configured, secret present, deliveries arriving — so an integration that
// ingests and routes nothing renders exactly like one that works.
//
// Nil when notifications have not started, which is the honest answer for
// "cannot say" and is why this is a separate return rather than an empty
// slice: an empty one means "nothing routes", and a company mid-boot has
// not established that.
func (e *Engine) RoutedSources() []string {
	e.notify.mu.Lock()
	svc := e.notify.service
	e.notify.mu.Unlock()
	if svc == nil {
		return nil
	}
	return svc.Sources()
}

// Status is the working-indicator driver set for chat-triggered turns.
//
// A SET rather than one driver, because a company can run both chat
// surfaces and a turn is triggered by exactly one of them. Handing out "the"
// driver would raise the indicator on the wrong backend or, far more likely,
// on none — a driver refuses a trigger whose transport is not its own, so
// the second surface would go silently unindicated for ever.
//
// NEVER NIL, like the registry and for the same reason: a company with no
// chat backend gets an empty set, whose Begin reports no session and whose
// nil session's methods are no-ops. The turn engine then says what phase it
// is in without first asking whether indicators exist anywhere — which is a
// question about this node's wiring that a turn has no business knowing the
// answer to.
func (e *Engine) Status() *notify.Statuses {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	var drivers []*notify.StatusDriver
	if e.notify.mattermost != nil {
		drivers = append(drivers, e.notify.mattermost.Status())
	}
	if e.notify.slack != nil {
		drivers = append(drivers, e.notify.slack.Status())
	}
	return notify.NewStatuses(drivers...)
}

// refreshParties rebuilds the registry for a newly applied epoch.
//
// Called on EVERY apply, because an epoch is published rather than mutated:
// a node that indexed only its first company would resolve every party
// against an org that is no longer running, and a seat added by an apply
// would be permanently unreachable with nothing failing.
//
// The vendor identities a transport resolved against a live server are
// re-registered into the new registry, because they are facts about the
// SERVER rather than about the config — losing them on an apply would make
// every agent's own message annotate as a stranger until something
// reconnected.
func (e *Engine) refreshParties(c *Company) {
	reg := notify.NewRegistry(c.Org)
	rec := reg.ReconcileHumanContacts(c.Org, e.resolver().LookupOK)

	// The CODE HOST's seat identities are config-derived, so they are
	// rebuilt from the new company here rather than carried across. Doing
	// it before the registry is published means no window where a GitLab
	// webhook resolves to nobody.
	if gl := c.Config.Integrations.GitLab; gl != nil && gl.Enabled {
		e.notify.gitlab.register(reg, c, e.resolver())
	}
	// THE TRACKER'S ARE TOO, and for the same reason: a Jira account id is
	// what a webhook names a seat by, and the mapping is derived from the
	// seat's own credential rather than declared anywhere in the org.
	if j := c.Config.Integrations.Jira; j != nil {
		e.notify.jira.register(reg, c, e.resolver())
	}

	e.notify.mu.Lock()
	e.notify.registry = reg
	chat := e.notify.mattermost
	hosted := e.notify.slack
	e.notify.mu.Unlock()

	// THE CHAT IDENTITIES ARE CARRIED ACROSS, not rebuilt: they are facts
	// about a live server that the transports resolved once at connect,
	// where a tracker's or a code host's are config-derived and rebuilt
	// above. Losing them on an apply would make every agent's own message
	// annotate as a stranger until something reconnected.
	if chat != nil {
		chat.Reregister(reg)
	}
	if hosted != nil {
		hosted.Reregister(reg)
	}
	log.Info("parties_indexed", "company", c.Config.Name, "parties", reg.Len(),
		"human_contacts", rec.Registered, "unresolved", rec.Unresolved,
		"conflicts", len(rec.Conflicts))
}

// startNotifications brings up the inbound edge for the applied company.
//
// It runs after the node, because the service subscribes to a fleet-wide
// group and a transport publishes onto this node's queue.
func (e *Engine) startNotifications(ctx context.Context, c *Company) error {
	if c == nil {
		return nil
	}
	e.refreshParties(c)

	var (
		parsers []notify.Parser
		prompts []notify.Prompt
	)
	if mm := c.Config.Integrations.Mattermost; mm != nil && mm.Enabled {
		transport, err := e.startMattermost(ctx, c, mm)
		if err != nil {
			// The company runs WITHOUT that surface rather than not
			// at all. A chat instance being unreachable at boot is
			// an ordinary state — it restarts, it moves, its
			// certificate lapses — and refusing to start the
			// company over it takes down every seat's scheduled and
			// tracker work too.
			log.Error("mattermost_unavailable", "error", err.Error(),
				"detail", "the company is running without its chat surface")
		}
		if transport != nil {
			parsers = append(parsers, transport.Parser())
			prompts = append(prompts, transport.Prompt())
		}
	}
	if sl := c.Config.Integrations.Slack; sl != nil {
		transport, err := e.startSlack(ctx, c, sl)
		if err != nil {
			// Same posture as the self-hosted chat surface: the company
			// runs WITHOUT it rather than not at all. A workspace that
			// refuses a seat's token is an ordinary state — a token gets
			// revoked, an app gets uninstalled — and refusing to start
			// the company over it would take every seat's tracker and
			// scheduled work down with it.
			log.Error("slack_unavailable", "error", err.Error(),
				"detail", "the company is running without its hosted chat surface")
		}
		if transport != nil {
			parsers = append(parsers, transport.Parser())
			prompts = append(prompts, transport.Prompt())
		}
	}
	if gl := c.Config.Integrations.GitLab; gl != nil && gl.Enabled {
		parser, err := e.startGitLab(ctx, c, gl)
		if err != nil {
			// Same posture as the other two surfaces: the company runs
			// without its code host rather than not at all.
			log.Error("gitlab_unavailable", "error", err.Error(),
				"detail", "the company is running without its code host")
		}
		if parser != nil {
			parsers = append(parsers, parser)
			prompts = append(prompts, gitlabPrompt())
		}
	}
	if j := c.Config.Integrations.Jira; j != nil {
		parser, err := e.startJira(ctx, c, j)
		if err != nil {
			// Same posture as the other surfaces: the company runs
			// WITHOUT its Atlassian tracker rather than not at all.
			log.Error("jira_unavailable", "error", err.Error(),
				"detail", "the company is running without its Atlassian tracker")
		}
		if parser != nil {
			parsers = append(parsers, parser)
			prompts = append(prompts, jiraPrompt())
		}
	}
	if p := c.Config.Integrations.Plane; p != nil && p.Enabled {
		parts, err := e.startPlane(c, p)
		if err != nil {
			// Same posture as the chat surface: the company runs
			// WITHOUT its tracker rather than not at all. Refusing to
			// boot over a tracker misconfiguration would take down
			// every seat's chat and scheduled work with it.
			log.Error("plane_unavailable", "error", err.Error(),
				"detail", "the company is running without its tracker")
		}
		if parts.parser != nil {
			e.notify.mu.Lock()
			e.notify.plane = parts
			e.notify.mu.Unlock()
			parsers = append(parsers, parts.parser)
			prompts = append(prompts, planePrompt())
			e.startSkillSync(ctx, c)
		}
	}

	svc, err := notify.New(notify.Options{
		Queue:    e.backends.Queue,
		Registry: e.Registry,
		Prompts:  notify.NewPrompts(prompts...),
		Parsers:  parsers,
		Valve:    e.notifyValve(),
		// Read live off the epoch rather than captured: an apply that
		// changes the cap must take effect on the next notification,
		// not on the next restart.
		RateLimit: func() int { return e.Company().Config.NotificationRateLimit },
		// The config posture, supplied by whoever holds the control
		// plane. A shedding node PARKS inbound deliveries rather than
		// routing them against a company it is not sure of — and nil
		// admits everything, which is the single-node case and the case
		// before a control plane exists.
		Admits: e.admits,
	})
	if err != nil {
		return fmt.Errorf("engine: notifications: %w", err)
	}
	if err := svc.Start(ctx); err != nil {
		return fmt.Errorf("engine: notifications: %w", err)
	}

	e.notify.mu.Lock()
	e.notify.service = svc
	e.notify.mu.Unlock()
	return nil
}

// RouteInbound adds a vendor to this node's inbound edge after boot.
//
// The seam a CUSTOM TRANSPORT joins through — an integration that is not one
// of the shipped ones, or one that came up late. Refused rather than queued
// when the service is not running: a caller told its transport was routed
// when nothing will ever reach it is worse off than one told it failed.
func (e *Engine) RouteInbound(_ context.Context, parsers []notify.Parser, prompts []notify.Prompt) error {
	e.notify.mu.Lock()
	svc := e.notify.service
	e.notify.mu.Unlock()
	if svc == nil {
		return fmt.Errorf("engine: no inbound service is running")
	}
	bySource := make(map[string]notify.Prompt, len(prompts))
	for _, p := range prompts {
		if p != nil {
			bySource[p.Source()] = p
		}
	}
	for _, parser := range parsers {
		if parser == nil {
			continue
		}
		if err := svc.Register(parser, bySource[parser.Source()]); err != nil {
			return fmt.Errorf("engine: %w", err)
		}
	}
	log.Info("inbound_sources_routed", "sources", svc.Sources())
	return nil
}

// WebhookSecrets is the verification material the inbound edge checks a
// delivery against, resolved through this node's own chain.
//
// It lives here rather than at the call site because both halves are the
// engine's: the applied company and the resolver. The CLI assembling it
// meant one line deciding a security property, and the mistake it invites —
// passing the config through unresolved — leaves seven routes verifying
// against the literal "${GITLAB_SIGNING_SECRET}".
func (e *Engine) WebhookSecrets() webhooks.Secrets {
	company := e.Company()
	if company == nil {
		return webhooks.Secrets{}
	}
	return webhooks.SecretsOf(company.Config, company.Org, e.Resolve)
}

// VerifiableSources lists the integrations whose RESOLVED material can
// actually verify an inbound delivery on this node, sorted.
//
// The companion to [Engine.RoutedSources], and the other half of "is this
// integration really working". Routed answers whether a verified delivery
// would reach a seat; this answers whether one would be verified at all.
// Both are facts only a co-located engine holds, because both depend on what
// this process resolved rather than on what the document says.
//
// Nil when there is no company yet — "cannot say", which is not the same
// claim as an empty slice, and a standalone API answers it for every
// integration.
func (e *Engine) VerifiableSources() []string {
	if e.Company() == nil {
		return nil
	}
	sources := e.WebhookSecrets().Verifiable()
	if sources == nil {
		// A REAL EMPTY ANSWER, not "cannot say": this node has a company
		// and nothing in it can verify a delivery. Collapsing the two here
		// would report the most alarming state as unknown.
		return []string{}
	}
	return sources
}

// Mattermost is the running chat transport, or nil when the company has no
// chat surface. Beside [Engine.Registry] and [Engine.Status]: the facts only
// a co-located engine can answer about what it actually built.
func (e *Engine) Mattermost() *mattermost.Transport {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	return e.notify.mattermost
}

// startMattermost brings up the chat surface.
func (e *Engine) startMattermost(ctx context.Context, c *Company, cfg *config.Mattermost) (*mattermost.Transport, error) {
	// RESOLVED HERE, at construction, exactly as the tracker and the code
	// host resolve theirs: a ${VAR} stays verbatim in the stored config,
	// which is what makes re-activating an unchanged revision pick up a
	// rotated value. Passing cfg.URL through raw handed every seat the
	// literal "${MATTERMOST_URL}" and failed all seven at the URL parse,
	// with the company running "without its chat surface" — measured
	// against a real instance whose address was exported correctly.
	env := e.resolver()
	url, team := env.Value(cfg.URL), env.Value(cfg.Team)
	if url == "" {
		// FOR THE MESSAGE, not for the outcome: NewClient refuses an
		// empty address on its own, so removing this changes nothing
		// about what gets built. What it changes is what an operator
		// reads — "no instance url" sends them to the config, which is
		// fine, while naming the reference sends them to the variable
		// that answered nothing, which is where the fix is. The
		// validator already refused an enabled Mattermost with no url,
		// so empty here can only be an unresolved ${VAR}.
		return nil, fmt.Errorf("engine: mattermost: url resolved empty (%q)", cfg.URL)
	}

	seats := mattermost.SeatsFrom(c.Org, e.resolver().LookupOK)
	if len(seats) == 0 {
		// Enabled with no provisioned seats is a company mid-setup, not
		// a failure: `crewlet mattermost provision` has not run yet.
		log.Info("mattermost_enabled_with_no_seats", "url", url)
		return nil, nil
	}
	transport, err := mattermost.NewTransport(mattermost.TransportOptions{
		Config: mattermost.Config{
			URL: url, Team: team,
			Status: notify.StatusMode(cfg.Status()),
			Seats:  seats,
		},
		Publisher: e.backends.Queue,
		Follows:   e.followStore(),
		Registry:  e.Registry,
	})
	if err != nil {
		return nil, err
	}
	e.notify.mu.Lock()
	e.notify.mattermost = transport
	e.notify.mu.Unlock()

	if err := transport.Start(ctx); err != nil {
		return transport, err
	}
	return transport, nil
}

// followStore is the durable thread-follow state, or nil with no store.
//
// Nil turns thread routing OFF rather than holding follows in memory: a
// process-local follow set dies with the process, so every restart would
// make every seat deaf to every thread it was following — and it would do so
// silently, which is worse than not having the feature.
func (e *Engine) followStore() notify.FollowStore {
	if e.backends.Store == nil {
		return nil
	}
	return e.backends.Store.ThreadFollows()
}

// notifyValve is the shared per-seat notification cap, or nil with no
// coordination store.
//
// Nil leaves the valve off, which is right: the counter has to be FLEET-WIDE
// to work at all — the loop it exists to catch bounces between nodes, so no
// single process sees enough of it to trip — and a per-process substitute
// would report a limit it is not enforcing.
//
// It was on the node's own database, which made it exactly that per-process
// substitute: a company on four nodes ran four valves, and a seat configured
// for five notifications a second could emit twenty.
func (e *Engine) notifyValve() notify.Valve {
	if e.backends.Fleet == nil {
		return nil
	}
	return &notifyValve{counter: e.backends.Fleet}
}

// notifyValve adapts the fleet counter to the notification seam.
type notifyValve struct{ counter coord.Counter }

func (v *notifyValve) Allow(ctx context.Context, bucket string, limit int, now time.Time) (bool, error) {
	return v.counter.Allow(ctx, bucket, limit, coord.RateWindow, now)
}

// admits reports whether this node may take inbound work, deferring to the
// caller-supplied posture. Nil admits everything.
func (e *Engine) admits() bool {
	e.notify.mu.Lock()
	gate := e.notify.admits
	e.notify.mu.Unlock()
	return gate == nil || gate()
}

// stopNotifications takes the inbound edge down.
func (e *Engine) stopNotifications(ctx context.Context) {
	e.notify.mu.Lock()
	chat := e.notify.mattermost
	hosted := e.notify.slack
	e.notify.mattermost, e.notify.slack = nil, nil
	e.notify.mu.Unlock()
	if chat != nil {
		chat.Stop(ctx)
	}
	if hosted != nil {
		// Nothing to disconnect — the inbound half is the API edge — but
		// a raised working indicator outlives the process visibly, so a
		// seat that vanished mid-turn would look like it was still
		// thinking until Slack expired it.
		hosted.Stop(ctx)
	}
}
