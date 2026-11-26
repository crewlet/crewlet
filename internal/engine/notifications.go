package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/slack"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The inbound edge, wired.
//
// The party registry is derived from one org, so an apply builds a new one
// and every reader reaches it through a function rather than holding it. The
// transports hold live sockets and resolved third-party identities, so they
// are reconciled rather than rebuilt: rebuilding would drop every connection
// whenever an unrelated field changed.

// notifications is this node's inbound machinery.
type notifications struct {
	mu       sync.Mutex
	registry *notify.Registry

	// registryFor is the company [notifications.registry] was built from.
	//
	// IT IS THE RACE'S ONLY WITNESS: a seat identity resolved from one revision
	// is written into the live registry both by [Engine.refreshParties] and by
	// the reconcile loop's identity retry (routing.go), and nothing orders the
	// two. A retry begun before an apply would register the previous revision's
	// account over the new one, routing a rotated credential to nobody with no
	// error. An apply publishes the *Company and *Registry together under mu, so
	// comparing the pointer is exact. [Engine.installEpoch] runs the same test
	// the other way, which keeps [Engine.Registry] non-nil for every reader that
	// found a company through [Engine.Company].
	registryFor *Company

	// rebuilding serialises every whole rebuild of the registry, and every write
	// into a live one, against each other (directory.go). The registry has two
	// rebuild triggers, a published company and a moved directory, and the
	// second rebuilds for the same company, which registryFor cannot tell apart:
	// interleaved, the last to finish wins, possibly the one that read the
	// directory first. Its own mutex because a rebuild reads the store and mu is
	// on the inbound hot path. Always taken BEFORE mu, never while holding it.
	rebuilding sync.Mutex

	// directory is the identity directory the registry reads: on a data node
	// this node's own rows ([Engine.startCore]); on a node without `data` a data
	// node's rows through the estate router ([routedIdentity],
	// [Engine.startRoutedDirectory]), with the chart and this node's Tier A
	// tokens applied on the asking side; nil only under `crewlet validate` and
	// in tests. Set once, at boot, under mu.
	directory notify.Directory

	// directoryAt is the identity applier's committed position and readAt its
	// value at the last reading, so the periodic net can tell "nothing applied
	// since" from "rows moved with no committed hook" (an adoption). NIL ON A
	// NODE WITHOUT `data`, which runs no applier, so the net reads every tick
	// there. directoryFailed makes the net retry a failed reading whatever the
	// position. All three are guarded by [notifications.rebuilding].
	directoryAt     func() statelog.Position
	readAt          statelog.Position
	directoryFailed bool

	admits notify.Admitter

	service    *notify.Service
	mattermost *mattermost.Transport

	// chatPrint is what the running self-hosted transport was built from,
	// hashed. An apply that changes nothing this surface reads must not
	// drop every seat's websocket: see [Engine.reconcileMattermost].
	chatPrint string

	// slack is the hosted chat surface, a second field rather than a choice: a
	// company may run both (different workspaces, or mid-migration).
	slack *slack.Transport

	// chatletStatus is the engine's own chat's working-indicator driver over the
	// attached live hub ([Engine.SetChatLive]); chatletStatusMode is the
	// `chat.chatlet.typing_status` it was built for and chatletStatusOn the
	// attachment. Nil where the company does not run Chatlet or no hub is
	// attached. See [Engine.applyChatStatus]. Never named `chatlet`: beside two
	// vendor transports that reads as a third transport, the wrong-backend
	// mix-up [Engine.Status] and [Engine.ChatThreads] exist to prevent.
	chatletStatus     *notify.StatusDriver
	chatletStatusMode notify.StatusMode
	chatletStatusOn   *ChatLive

	// confluence is the knowledge base's parser, searcher and skills-space
	// reader. No lifecycle: it is inbound-only, so an unreachable instance
	// degrades a turn's enrichment without taking a surface down.
	confluence confluenceParts

	// jira remembers which account each seat credential authenticates as,
	// outside mu and outliving an epoch, as gitlab's does below.
	jira jiraIdentities

	// gitlab remembers which account each seat credential authenticates as.
	// Outside mu (it has its own lock), and it OUTLIVES an epoch: identity is a
	// function of the credential, so an unrelated revision must not re-spend a
	// request per seat to re-learn it.
	gitlab gitlabIdentities

	// github is the hosted code host's, on identical terms; a company may run
	// both code hosts.
	github githubIdentities
}

// Registry is the live party registry.
//
// NEVER NIL WHILE AN EPOCH IS CURRENT, structurally: [Engine.installEpoch],
// the one function that makes a company current, indexes it first. A reader
// that found a company through [Engine.Company] always finds a registry, so a
// check here would suggest a window that does not exist.
func (e *Engine) Registry() *notify.Registry {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	return e.notify.registry
}

// registryOf is the live registry, but only if it was built from c
// ([notifications.registryFor]). A caller that resolved against a company an
// apply has since replaced gets nil and writes nothing.
//
// NIL IS NOT A FAILURE, hence one value rather than a three-valued shape: the
// work was superseded, and the apply that replaced it has already registered
// its own revision's identities.
func (e *Engine) registryOf(c *Company) *notify.Registry {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	if c == nil || e.notify.registryFor != c {
		return nil
	}
	return e.notify.registry
}

// RoutedSources lists the integrations whose deliveries can actually wake a
// seat on this node, sorted.
//
// NOT the configured ones: a webhook route verifies and stores deliveries as
// soon as its block is present, but reaching an agent needs a parser, and
// several apps have the first without the second. Without this they render
// on every operator surface exactly like one that works.
//
// Nil when notifications have not started ("cannot say"), distinct from an
// empty slice ("nothing routes").
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
// A SET because a company can run several chat surfaces and a turn is
// triggered by one: a driver refuses a trigger whose transport is not its
// own, so a single driver would leave the others silently unindicated.
//
// NEVER NIL: with no chat backend it is an empty set whose Begin reports no
// session and whose nil session's methods are no-ops, so a turn never asks
// about this node's wiring.
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
	if e.notify.chatletStatus != nil {
		drivers = append(drivers, e.notify.chatletStatus)
	}
	return notify.NewStatuses(drivers...)
}

// applyChatStatus brings the engine's own chat's working indicator in line
// with c: a driver over the attached hub, in the `chat.chatlet.typing_status`
// mode, wherever the company runs Chatlet — on every node, not only data
// nodes, since a node holding no data runs Chatlet seats too (remote.go).
//
// Called at boot from [Engine.startNotifications], on every apply, and when a
// hub is attached ([New] starts notifications before one can be). Replacing
// a driver takes its running turns' indicators down, so it is rebuilt only
// when the mode, the company's use of Chatlet or the hub changed: the rule
// [Engine.reconcileMattermost] applies to a transport.
func (e *Engine) applyChatStatus(ctx context.Context, c *Company) {
	e.notify.mu.Lock()
	// Read under the lock, so of an apply and an attachment racing, the last
	// to take it decides and sees the latest hub.
	on := e.chatLive.Load()
	want := notify.StatusOff
	if on != nil && *on != nil && c != nil && c.Config != nil &&
		c.Config.ChatBackendFor() == config.ChatChatlet {
		want = notify.StatusMode(c.Config.Chat.Chatlet.Status())
	}
	old := e.notify.chatletStatus
	if (old == nil && want == notify.StatusOff) ||
		(old != nil && e.notify.chatletStatusMode == want && e.notify.chatletStatusOn == on) {
		e.notify.mu.Unlock()
		return
	}
	var next *notify.StatusDriver
	if want != notify.StatusOff {
		next = notify.NewStatusDriver(notify.StatusOptions{
			Poster: *on, Mode: want,
		})
	}
	e.notify.chatletStatus, e.notify.chatletStatusMode = next, want
	e.notify.chatletStatusOn = on
	e.notify.mu.Unlock()
	// Outside the lock: a stop is a request per live session, and
	// [Engine.Status] takes this lock on every turn's raise.
	if old != nil {
		old.Stop(ctx)
	}
}

// ChatThreads is the read half of every chat surface this node runs, as one.
//
// A set and never nil, on [Engine.Status]'s argument: a turn is woken by one
// of possibly several surfaces, and with none (a maintenance-mode node, or an
// instance unreachable at boot) the empty set reports "nothing could be read".
//
// The engine's own chat has no transport, so its reader reads through the
// estate's router as the woken seat ([Engine.chatletThreads]), giving a node
// that holds no data the same thread as a data node. It is the only thread
// context a Chatlet wake gets: the record carries none ([chatlet.ThreadReader]).
func (e *Engine) ChatThreads() *notify.ThreadReaders {
	chatletReader := e.chatletThreads()
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	var readers []notify.ThreadReader
	if e.notify.mattermost != nil {
		readers = append(readers, e.notify.mattermost)
	}
	if e.notify.slack != nil {
		readers = append(readers, e.notify.slack)
	}
	if chatletReader != nil {
		readers = append(readers, chatletReader)
	}
	return notify.NewThreadReaders(readers...)
}

// chatletThreads is the engine's own chat's thread reader, or nil where this
// company's chat is not the engine's own here — [OperatorChat]'s rule: it is
// a read, so a maintenance-mode data node, which hands its seats no chat
// writer, still gets one.
//
// OVER THE TURN-START READ ([estate.TurnThreads]), never the [estate.Chat]
// facade: only the turn's read hands over whom each message was written for,
// which a turn works its waiting messages by and no reader of a room may see.
// This is the one place the engine builds that facade.
func (e *Engine) chatletThreads() notify.ThreadReader {
	if _, ok := OperatorChat(e); !ok {
		return nil
	}
	return chatlet.NewThreadReader(e.router.TurnThreads(), e.liveParties)
}

// liveParties is the party registry current when a read renders, as
// [notify.Parties] — nil rather than a typed nil, so a reader checking for
// none gets the answer it asked for.
func (e *Engine) liveParties() notify.Parties {
	if reg := e.Registry(); reg != nil {
		return reg
	}
	return nil
}

// refreshParties rebuilds the registry for a newly applied epoch.
//
// On EVERY apply, because an epoch is published rather than mutated: indexing
// only the first company would leave a seat added later unreachable with
// nothing failing. Third-party identities a transport resolved against a live
// server are re-registered, since they are facts about the server, not the
// config.
//
// Every rebuild also takes a fresh reading of the identity directory, since
// whether a human seat's contacts register depends on whether its holder may
// still be reached; the other trigger, a suspension, lives in directory.go.
func (e *Engine) refreshParties(ctx context.Context, c *Company) {
	e.notify.rebuilding.Lock()
	defer e.notify.rebuilding.Unlock()
	e.rebuildPartiesLocked(c, e.readStandingLocked(ctx))
}

// firstPartySources are the notification sources this engine hosts itself
// (tracker, knowledge base, chat, Gitlet, artifacts), whose senders ARE seat
// handles, declared to every party registry ([partyRegistry]).
//
// EVERY ONE, whether or not the company runs it natively: a source kept on a
// vendor publishes no wake under this name, so declaring it costs nothing,
// and following the company's backends would be a second reading of the
// revision that can disagree with [Engine.nativeParsers]. A native source
// missing here reads every sender as an outside party, so a person's comment
// wakes a seat that will not act for them; a test holds every native
// parser's source to this list.
var firstPartySources = []string{worklet.Source, doclet.Source, chatlet.Source, gitlet.Source,
	artifactsdomain.Source}

// partyRegistry is the ONE construction of a party registry on this node: the
// org's seats, with the engine's own surfaces declared as the namespaces whose
// ids are handles ([firstPartySources]).
func partyRegistry(o *org.Organization, lookup org.EnvLookup) *notify.Registry {
	return notify.NewRegistry(o, lookup, firstPartySources...)
}

// rebuildPartiesLocked builds a WHOLE registry for c under one directory
// reading and swaps it in. The caller holds [notifications.rebuilding].
//
// Never a diff: a patch to a fresh registry drops every identity it did not
// touch, and a patch to the live one mutates a value a running turn may be
// reading.
func (e *Engine) rebuildPartiesLocked(c *Company, standing notify.Standing) {
	// ONE SNAPSHOT for both halves: a seat's address and its contact ids may
	// both be `${VAR}` references, and two reads could straddle a refresh.
	lookup := e.resolver().LookupOK
	reg := partyRegistry(c.Org, lookup)
	rec := reg.ReconcileHumanContacts(c.Org, lookup, standing)

	// The code hosts' seat identities are config-derived, so they are rebuilt
	// here, before the publish, leaving no window where a webhook resolves to
	// nobody.
	if gl := c.Config.Integrations.GitLab; gl != nil && gl.Enabled {
		e.notify.gitlab.register(reg, c, e.resolver())
	}
	if gh := c.Config.Integrations.GitHub; gh != nil && gh.Enabled {
		e.notify.github.register(reg, c, e.resolver())
	}
	// The tracker's too: a Jira account id is what a webhook names a seat by,
	// derived from the seat's own credential.
	if j := c.Config.Integrations.Jira; j != nil {
		e.notify.jira.register(reg, c, e.resolver())
	}

	e.notify.mu.Lock()
	selfHosted := e.notify.mattermost
	hosted := e.notify.slack
	// An apply restates the company; a directory rebuild (directory.go) does
	// not, and only the first answers a person's name reported as moved.
	restated := e.notify.registryFor != c
	e.notify.mu.Unlock()

	// The chat identities are CARRIED ACROSS, not rebuilt: they are facts about
	// a live server the transports resolved at connect, and losing them would
	// annotate every agent's own message as a stranger's. BEFORE THE SWAP, so the
	// published registry never answers an agent's bot as a stranger; a seat
	// connecting meanwhile registers into the registry about to be published.
	if selfHosted != nil {
		// With the org's people, which only the transport can resolve to the
		// account ids their posts carry. Which it may register is judged against
		// reg's own directory reading, not filtered here.
		selfHosted.Reregister(reg, mattermost.PeopleFrom(c.Org, lookup), restated)
	}
	if hosted != nil {
		hosted.Reregister(reg)
	}

	e.notify.mu.Lock()
	e.notify.registry, e.notify.registryFor = reg, c
	e.notify.mu.Unlock()
	log.Info("parties_indexed", "company", c.Config.Name, "parties", reg.Len(),
		"human_contacts", rec.Registered, "unresolved", rec.Unresolved,
		"withheld_seats", len(reg.Withheld()), "directory", standing.Consulted(),
		"conflicts", len(rec.Conflicts))
}

// ensureInboundMailbox makes the inbound edge's mailbox, the router's fleet
// group on [topics.NotificationsInbound], before anything on this node
// publishes onto it.
//
// The stream keeps only what a consumer covers: a delivery published before
// the group's durable exists is discarded yet reported as sent. The router's
// attachment ([notify.Service.Start]) starts last on the edge, after the
// change feeds and the Mattermost transport, so each path that starts a
// publisher makes the mailbox first: boot, [Engine.startNativeFor] and
// [Engine.startNotifications]. An error is the caller's to refuse on: a node
// that cannot hold its inbound mail routes nothing while looking healthy.
func (e *Engine) ensureInboundMailbox(ctx context.Context) error {
	if _, err := e.backends.Queue.EnsureSubscription(ctx,
		topics.NotificationsInbound, notify.InboundGroup); err != nil {
		return fmt.Errorf("ensure the inbound edge's mailbox: %w", err)
	}
	return nil
}

// startNotifications brings up the inbound edge for the applied company. It
// runs after the node: the service subscribes to a fleet-wide group and a
// transport publishes onto this node's queue. The party registry was built
// when the company was published ([Engine.Registry]).
//
// A node with no company starts nothing: the group is fleet-wide, so a
// service with no parsers would take peers' deliveries and acknowledge each
// as unparsed. Its first company starts it through [Engine.startInbound].
func (e *Engine) startNotifications(ctx context.Context, c *Company) error {
	if c == nil {
		return nil
	}
	// THE MAILBOX BEFORE THE TRANSPORTS: the Mattermost transport below
	// publishes onto the edge before the router attaches to it.
	if err := e.ensureInboundMailbox(ctx); err != nil {
		return fmt.Errorf("engine: %w", err)
	}

	var (
		parsers []notify.Parser
		prompts []notify.Prompt
	)
	if mm := c.Config.Integrations.Mattermost; mm != nil && mm.Enabled {
		transport, err := e.startMattermost(ctx, c, mm)
		if err != nil {
			// The company runs WITHOUT that surface rather than not at all: an
			// unreachable chat instance is an ordinary state, and refusing would
			// take every seat's scheduled and tracker work down too.
			log.ErrorContext(ctx, "mattermost_unavailable", "error", err.Error(),
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
			// Same posture as Mattermost: a revoked token or an uninstalled app is
			// an ordinary state.
			log.ErrorContext(ctx, "slack_unavailable", "error", err.Error(),
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
			// Same posture: the company runs without its code host.
			log.ErrorContext(ctx, "gitlab_unavailable", "error", err.Error(),
				"detail", "the company is running without its code host")
		}
		if parser != nil {
			parsers = append(parsers, parser)
			prompts = append(prompts, gitlabPrompt())
		}
	}
	if gh := c.Config.Integrations.GitHub; gh != nil && gh.Enabled {
		parser, err := e.startGitHub(ctx, c, gh)
		if err != nil {
			// Same posture: the company runs without its hosted code host.
			log.ErrorContext(ctx, "github_unavailable", "error", err.Error(),
				"detail", "the company is running without its hosted code host")
		}
		if parser != nil {
			parsers = append(parsers, parser)
			prompts = append(prompts, githubPrompt())
		}
	}
	if dd := c.Config.Integrations.Datadog; dd != nil && dd.Enabled {
		// No error path: an alert carries everything its routing needs on the
		// payload, so there is nothing to construct that could fail.
		parsers = append(parsers, datadog.NewParser(datadog.ParserOptions{
			HandleTag: dd.HandleTagOrDefault(),
			Fallback:  dd.RouteTo,
		}))
		prompts = append(prompts, datadogPrompt())
	}
	if j := c.Config.Integrations.Jira; j != nil {
		parser, err := e.startJira(ctx, c, j)
		if err != nil {
			// Same posture: the company runs without its Atlassian tracker.
			log.ErrorContext(ctx, "jira_unavailable", "error", err.Error(),
				"detail", "the company is running without its Atlassian tracker")
		}
		if parser != nil {
			parsers = append(parsers, parser)
			prompts = append(prompts, jiraPrompt())
		}
	}
	if cf := c.Config.Integrations.Confluence; cf != nil {
		parts, err := e.startConfluence(c, cf)
		if err != nil {
			// Same posture: the company runs without its knowledge base.
			log.ErrorContext(ctx, "confluence_unavailable", "error", err.Error(),
				"detail", "the company is running without its knowledge base, "+
					"so every turn gets an empty knowledge block")
		}
		if parts.parser != nil {
			e.notify.mu.Lock()
			e.notify.confluence = parts
			e.notify.mu.Unlock()
			parsers = append(parsers, parts.parser)
			prompts = append(prompts, confluencePrompt())
		}
	}

	// And the engine's own chat's working indicator, which no transport
	// carries ([Engine.applyChatStatus]).
	e.applyChatStatus(ctx, c)

	// The native backends' edge last, so a vendor parser stays first. The order
	// is cosmetic (each answers its own source) but stable, so two boots of one
	// config log the same thing.
	nativeParsers, nativePrompts := e.nativeParsers(c)
	parsers = append(parsers, nativeParsers...)
	prompts = append(prompts, nativePrompts...)

	// THROUGH THE ONE DOOR every fleet-wide group passes ([Engine.joins]).
	if !e.joins(ctx, notify.InboundGroup) {
		return nil
	}
	svc, err := e.inboundService(c, parsers, prompts)
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

// inboundService is the notification service this node routes c's inbound
// deliveries with, through parsers and prompts: everything [Engine.startNotifications]
// starts but the start.
func (e *Engine) inboundService(c *Company, parsers []notify.Parser,
	prompts []notify.Prompt) (*notify.Service, error) {

	// The replay's drops go through the ledger the alarm reads, so a copy the
	// replay delivered is never counted as a lost wake (feeddeadletters.go).
	// Nil leaves the service on its queue's own verbs.
	var letters notify.DeadLetters
	if e.deadLetterLedger != nil {
		letters = e.deadLetterLedger
	}
	return notify.New(notify.Options{
		// The node's wake queue, so a delivery to a seat whose mailbox is not
		// complete is NAKed and redelivered, not discarded ([Engine.wakes]).
		Queue:     e.wakes(),
		Registry:  e.Registry,
		Prompts:   notify.NewPrompts(prompts...),
		Parsers:   parsers,
		Valve:     e.notifyValve(),
		RateLimit: e.notificationRateLimit(c),
		// The config posture: a shedding node PARKS inbound deliveries rather
		// than routing against a company it is unsure of. Nil admits everything
		// (a single node, or no control plane yet).
		Admits: e.admits,
		// AND A LINKED CHANGE'S SETTLE, which no wake carries (ADR-0121).
		Observe: e.observeInbound,
		// Every source this build's producers write, run here or not, so a
		// first-party delivery from any other (a newer build's) passes to a peer.
		FirstParty:  declaredInboundSources(),
		DeadLetters: letters,
	})
}

// notificationRateLimit is the per-seat notification cap the inbound edge
// started for c enforces, read live off the epoch so an apply takes effect on
// the next notification. c answers until an epoch is current: a node's first
// company starts the edge before publishing its epoch ([Engine.startInbound]),
// and a peer's delivery can arrive in between.
func (e *Engine) notificationRateLimit(c *Company) func() int {
	return func() int {
		if live := e.Company(); live != nil {
			return live.Config.NotificationRateLimit
		}
		return c.Config.NotificationRateLimit
	}
}

// inboundStarted reports whether this node's inbound edge is running.
func (e *Engine) inboundStarted() bool {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	return e.notify.service != nil
}

// startInbound starts the inbound edge for the first company a node that
// booted with none is handed, through the function boot runs, so the edge
// does not depend on whether the node met the company at boot or at its first
// apply (the reconcilers only rebuild a surface on a running service).
//
// Called at the integrations stage, before the epoch is published, so a
// failed start can refuse the apply; what it brought up is taken down, since
// a leftover chat transport would hold every seat's socket beside the retry's.
//
// Nothing on a node in a maintenance mode: the edge is a fleet-wide consumer
// routing onto seats' mailboxes, and such a node publishes nothing.
func (e *Engine) startInbound(ctx context.Context, c *Company) error {
	if !e.mode.Publishes() {
		return nil
	}
	if err := e.startNotifications(ctx, c); err != nil {
		e.stopNotifications(ctx)
		return fmt.Errorf("start the inbound edge: %w", err)
	}
	log.InfoContext(ctx, "inbound_started", "company", c.Config.Name,
		"sources", e.RoutedSources())
	return nil
}

// RouteInbound adds a third-party app to this node's inbound edge after boot:
// the seam a custom or late transport joins through. Refused rather than
// queued when the service is not running, since a caller told it was routed
// when nothing will reach it is worse off than one told it failed.
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
// delivery against, resolved through this node's own chain. Resolving it
// here, not at the call site, keeps the security property in the engine:
// passing the config through unresolved verifies against the literal
// "${GITLAB_SIGNING_SECRET}".
//
// Built once per published company, keyed on its identity as [Engine.indexes]
// is: it walks every seat and resolves each `${VAR}`, on every webhook's path,
// and depends only on the company and the resolver snapshot, which move only
// on a publish (a rotation re-activates). Lazy, so a node serving no webhooks
// never builds it; the returned map is shared, for readers only.
func (e *Engine) WebhookSecrets() webhooks.Secrets {
	company := e.Company()
	if company == nil {
		return webhooks.Secrets{}
	}
	e.hooks.mu.Lock()
	defer e.hooks.mu.Unlock()
	if e.hooks.built == company {
		return e.hooks.material
	}
	e.hooks.material = webhooks.SecretsOf(company.Config, company.Org, e.Resolve)
	e.hooks.built = company
	return e.hooks.material
}

// VerifiableSources lists the integrations whose RESOLVED material can
// actually verify an inbound delivery on this node, sorted: the companion to
// [Engine.RoutedSources], which answers whether a verified delivery would
// reach a seat. Nil when there is no company yet ("cannot say").
func (e *Engine) VerifiableSources() []string {
	if e.Company() == nil {
		return nil
	}
	sources := e.WebhookSecrets().Verifiable()
	if sources == nil {
		// A real empty answer: collapsing it into nil would report the most
		// alarming state as unknown.
		return []string{}
	}
	return sources
}

// IngressHolds names, by surface, why a route refuses every delivery of a
// kind although its material resolved ([webhooks.Secrets.IngressHolds]). Nil
// when there is no company yet ("cannot say").
func (e *Engine) IngressHolds() map[string]string {
	if e.Company() == nil {
		return nil
	}
	return e.WebhookSecrets().IngressHolds()
}

// AtlassianCloudIDs is the process's one reader of a url-named Cloud site's
// cloud id, for the Forge route and setup to share with the engine.
func (e *Engine) AtlassianCloudIDs() *atlassian.CloudIDs { return e.cloudIDs }

// Mattermost is the running chat transport, or nil when the company has no
// chat surface. Beside [Engine.Registry] and [Engine.Status]: the facts only
// the engine can answer about what it actually built.
func (e *Engine) Mattermost() *mattermost.Transport {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	return e.notify.mattermost
}

// startMattermost brings up the chat surface.
func (e *Engine) startMattermost(ctx context.Context, c *Company, cfg *config.Mattermost) (*mattermost.Transport, error) {
	// Resolved here, at construction, as the tracker and the code host do: a
	// ${VAR} stays verbatim in the stored config, which is what makes
	// re-activating an unchanged revision pick up a rotated value.
	env := e.resolver()
	url, team := env.Value(cfg.URL), env.Value(cfg.Team)
	if url == "" {
		// For the message only (NewClient refuses an empty address too): the
		// validator already refused an enabled Mattermost with no url, so empty
		// here is an unresolved ${VAR}, and naming the reference sends the
		// operator to the variable.
		return nil, fmt.Errorf("engine: mattermost: url resolved empty (%q)", cfg.URL)
	}

	// EVERY SEAT, ON EVERY NODE, not only this node's: a node that restarts,
	// drains or loses a lease leaves its seats heard by the others, so a socket
	// has no handover gap. The cost is N sockets and backfills per bot on N
	// nodes; the fleet-wide claim each post takes (Claims, below) makes exactly
	// one node deliver it.
	seats := mattermost.SeatsFrom(c.Org, e.resolver().LookupOK)
	if len(seats) == 0 {
		// No seat holding a bot token is a company mid-setup (no agent turned on
		// in the integration's Agents tab, or no token sealed yet), not a failure.
		log.InfoContext(ctx, "mattermost_enabled_with_no_seats", "url", url)
		return nil, nil
	}
	transport, err := mattermost.NewTransport(mattermost.TransportOptions{
		Config: mattermost.Config{
			URL: url, Team: team,
			Status: notify.StatusMode(cfg.Status()),
			Seats:  seats,
			People: mattermost.PeopleFrom(c.Org, env.LookupOK),
		},
		// A person named by an account nobody holds is a finding of the
		// integration's status, which the reconcile loop otherwise re-reads only
		// hourly; this brings it forward on the node holding the duty.
		OnPeople:  func() { e.Integrations().Refresh(integration.KindMattermost) },
		Publisher: e.backends.Queue,
		// The fleet's: a process-local claim would wake a post once per node.
		Claims:   e.backends.Fleet,
		Follows:  e.followStore(),
		Registry: e.Registry,
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

	// Recorded where it is built, so boot and apply agree on what the running
	// transport came from (else the first apply drops every socket for nothing),
	// and only once it is proven to run: [Transport.Start] errs when every seat
	// failed, leaving no socket and no reconnect loop, and a fingerprint recorded
	// for it would make every later reconcile return early, the surface dead
	// until a restart.
	e.notify.mu.Lock()
	e.notify.chatPrint = chatFingerprint(url, team, string(cfg.Status()), seats)
	e.notify.mu.Unlock()
	return transport, nil
}

// followStore is the durable thread-follow state, or nil with no
// coordination store.
//
// THE FLEET'S, not this node's: the notification group is a competing
// consumer, so the next reply in a thread may land on any node, which must
// see the follow (ADR-0003). Nil turns thread routing OFF rather than holding
// follows in memory, which a restart would silently lose.
func (e *Engine) followStore() notify.FollowStore {
	if e.backends.Fleet == nil {
		return nil
	}
	return e.backends.Fleet
}

// notifyValve is the shared per-seat notification cap, or nil with no
// coordination store.
//
// The counter must be FLEET-WIDE: the loop it catches bounces between nodes,
// so no one process sees enough to trip, and a per-process valve lets N nodes
// emit N times the configured cap. Nil leaves the valve off rather than
// report a limit nobody enforces.
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

// Reopens implements [notify.Valve]: the end of the fixed window containing
// now, which the counter truncates from the zero time ([coord.Counter]).
func (v *notifyValve) Reopens(now time.Time) time.Time {
	return now.Truncate(coord.RateWindow).Add(coord.RateWindow)
}

// admits reports whether this node may take inbound work, deferring to the
// caller-supplied posture. Nil admits everything.
func (e *Engine) admits() bool {
	e.notify.mu.Lock()
	gate := e.notify.admits
	e.notify.mu.Unlock()
	return gate == nil || gate()
}

// resumeInbound restarts the inbound consumer once the posture admits work
// again.
//
// A deferral returns the delivery to the broker AND quiesces the attachment.
// A seat's mailbox is resumed by its own lease admission
// (seat.Host.noteAdmission); the ingress topic has no lease, so without this
// a node that shed one webhook would read nothing for the life of the process.
// A CONVERGENCE, not an edge: the refusal runs on the delivery path and the
// posture on the reconcile loop, so an edge could fire before the last
// in-flight delivery quiesces. Unquiesce on an unquiesced attachment reports
// false and does nothing, so an idle tick costs one local call.
func (e *Engine) resumeInbound(ctx context.Context) {
	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	resumed, err := e.backends.Queue.Unquiesce(ctx,
		topics.NotificationsInbound, notify.InboundGroup)
	switch {
	case err != nil && !errors.Is(err, queue.ErrNotLive):
		// Not retried here: the next tick asks again.
		log.WarnContext(ctx, "inbound_resume_failed", "error", err)
	case resumed:
		log.InfoContext(ctx, "inbound_resumed",
			"detail", "the config posture admits work again")
	}
}

// reconcileMattermost brings the self-hosted chat surface in line with the
// applied revision.
//
// GUARDED, unlike the other reconcilers, because it holds a websocket per
// seat: a seat is deaf across a reconnect and the backfill re-reads its
// channels. So it rebuilds only when [chatFingerprint], over exactly what the
// transport is built from, has changed.
func (e *Engine) reconcileMattermost(ctx context.Context, c *Company) {
	cfg := c.Config.Integrations.Mattermost
	e.notify.mu.Lock()
	svc, previous, was := e.notify.service, e.notify.mattermost, e.notify.chatPrint
	e.notify.mu.Unlock()
	if svc == nil {
		return
	}
	if cfg == nil || !cfg.Enabled {
		e.notify.mu.Lock()
		e.notify.mattermost, e.notify.chatPrint = nil, ""
		e.notify.mu.Unlock()
		if previous != nil {
			previous.Stop(ctx)
		}
		if svc.Unregister(mattermost.Backend) {
			log.InfoContext(ctx, "mattermost_retired",
				"detail", "the revision no longer enables mattermost; no seat "+
					"holds a connection and nothing routes to one")
		}
		return
	}
	// The same inputs startMattermost reads, resolved the same way, so an
	// unchanged fingerprint means "nothing to do".
	env := e.resolver()
	print := chatFingerprint(env.Value(cfg.URL), env.Value(cfg.Team),
		string(cfg.Status()), mattermost.SeatsFrom(c.Org, env.LookupOK))
	if previous != nil && print == was {
		return
	}
	transport, err := e.startMattermost(ctx, c, cfg)
	if err != nil || transport == nil {
		// The previous connections keep running: a stale credential still
		// routes, and a dropped one routes nothing.
		e.notify.mu.Lock()
		e.notify.mattermost = previous
		e.notify.mu.Unlock()
		if err != nil {
			log.ErrorContext(ctx, "mattermost_reconcile_failed", "error", errorText(err),
				"detail", "the previous chat wiring is still current")
		}
		return
	}
	if err := svc.Replace(transport.Parser(), transport.Prompt()); err != nil {
		log.ErrorContext(ctx, "mattermost_reconcile_failed", "error", err.Error(),
			"detail", "the previous chat wiring is still current")
		return
	}
	if previous != nil && previous != transport {
		previous.Stop(ctx)
	}
	log.InfoContext(ctx, "mattermost_reconciled", "company", c.Config.Name)
}

// chatFingerprint is what a chat transport was built from, as one comparable
// value. HASHED, because a seat's configuration carries its bot token, and
// LENGTH-PREFIXED, because joined with a separator a seat "a" holding token
// "b:c" and a seat "a:b" holding "c" are the same bytes.
func chatFingerprint(url, team, status string, seats []mattermost.SeatConfig) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, part := range parts {
			fmt.Fprintf(h, "%d:%s", len(part), part)
		}
	}
	write(url, team, status)
	for _, seat := range seats {
		write(seat.Handle, seat.Token, seat.Username)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// stopNotifications takes the inbound edge down.
func (e *Engine) stopNotifications(ctx context.Context) {
	e.notify.mu.Lock()
	service := e.notify.service
	selfHosted := e.notify.mattermost
	hosted := e.notify.slack
	chatletStatus := e.notify.chatletStatus
	e.notify.mattermost, e.notify.slack, e.notify.chatletStatus = nil, nil, nil
	e.notify.mu.Unlock()
	if service != nil {
		// The service's replay goroutine reads the queue this teardown is about
		// to close, so it is ended and waited for first; what it did not reach is
		// kept for the next start.
		if err := service.Stop(ctx); err != nil {
			log.WarnContext(ctx, "inbound_replay_stop_unfinished", "error", err.Error(),
				"detail", "the replay of the inbound group's unhandled deliveries was "+
					"still running when the stop's allowance ran out")
		}
	}
	if chatletStatus != nil {
		// Else the room's presence would show a seat working on a turn this
		// process is about to stop running.
		chatletStatus.Stop(ctx)
	}
	if selfHosted != nil {
		selfHosted.Stop(ctx)
	}
	if hosted != nil {
		// Nothing to disconnect, but a raised working indicator would outlive
		// the process until Slack expired it.
		hosted.Stop(ctx)
	}
}

// errorText renders a third-party app reconcile failure for a log line,
// including "it built but produced nothing", which carries no error. Shared
// by every integration reconciler so the wording cannot drift.
func errorText(err error) string {
	if err == nil {
		return "the wiring produced no parser"
	}
	return err.Error()
}

// reconcileDatadog brings the alert parser in line with the applied revision,
// since [Engine.startNotifications] assembles the parser set once: without it
// Datadog connected after boot routes nothing, and a changed monitor tag key
// or fallback seat goes unread until a restart. Nothing here can fail: an
// alert carries everything its routing needs.
func (e *Engine) reconcileDatadog(ctx context.Context, c *Company) {
	cfg := c.Config.Integrations.Datadog
	e.notify.mu.Lock()
	svc := e.notify.service
	e.notify.mu.Unlock()
	if svc == nil {
		return
	}
	// RETIRED when the revision disables it, the gesture after a leaked webhook
	// token: a parser left behind would go on waking seats from deliveries
	// already accepted.
	if cfg == nil || !cfg.Enabled {
		if svc.Unregister(datadog.Backend) {
			log.InfoContext(ctx, "datadog_retired",
				"detail", "the revision no longer enables datadog; its alerts "+
					"are refused at the webhook route and wake no seat")
		}
		return
	}
	parser := datadog.NewParser(datadog.ParserOptions{
		HandleTag: cfg.HandleTagOrDefault(),
		Fallback:  cfg.RouteTo,
	})
	if err := svc.Replace(parser, datadogPrompt()); err != nil {
		log.ErrorContext(ctx, "datadog_reconcile_failed", "error", err.Error(),
			"detail", "the previous alert routing is still current")
		return
	}
	log.InfoContext(ctx, "datadog_reconciled", "company", c.Config.Name,
		"handle_tag", cfg.HandleTagOrDefault(), "route_to", cfg.RouteTo)
}
