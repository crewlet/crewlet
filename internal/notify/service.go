package notify

import (
	"github.com/google/uuid"

	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tracing"
)

// The inbound edge: a verified webhook becomes a woken seat, or a recorded
// reason why not.
//
// ONE fleet-wide consumer group. The node that wins a delivery is rarely the
// node running the recipient — which is exactly why every resolution here
// goes through the org-derived [Registry] and every wake is a publish to the
// seat's inbox topic rather than a local call. A service that resolved
// against local state would drop most of a fleet's mail.

// InboundGroup is the fleet-wide consumer group for raw webhooks.
const InboundGroup = "notify-inbound"

// Parser turns one verified delivery into the notifications it implies.
//
// A third-party app's whole inbound surface, and the ONLY thing it must write
// to be routed: the guards, the merge, the valve and the wake are all above
// it. Returning several is normal — a comment naming three colleagues is
// three notifications, one per recipient.
//
// The registry is passed so a parser can intersect its own mention list
// against the parties the engine can actually route to. Without that, a
// comment mentioning outsiders fans out to notifications nobody can deliver.
type Parser interface {
	// Source is the integration name, matching the delivery's own.
	Source() string

	// Parse reports what a delivery means. An error is a malformed
	// payload — unless it carries a [Deferral], a record this node cannot
	// read and a peer can, which the service hands back to the group for
	// that peer rather than record as a parse failure. No notifications is
	// an ordinary outcome, and by far the most common one — most webhooks
	// concern nobody here.
	//
	// It takes a context because parsing genuinely does I/O on some
	// backends: a chat message consults the thread-follow store, and a
	// tracker fans an update out to a work item's subscribers. Making it
	// pure would only move that work somewhere with less context about
	// what it is for.
	Parse(ctx context.Context, w types.RawWebhook, r *Registry) ([]Routed, error)
}

// Routed is one parsed notification and who it is for.
type Routed struct {
	Inbound

	// To names the recipient in whatever terms the third-party app could supply.
	To Recipient

	// WakeID makes this wake RECOGNISABLE as a duplicate of itself.
	//
	// Nil takes an id derived from the delivery's and the recipient's
	// ([DeliveryWakeID]); redeliveries and replays handle a delivery again,
	// and a random id woke the seat each time. The engine's producers and the
	// Mattermost socket set it, since their first dedupe layer fails open by
	// design; a derived id lets the inbox dedupe and completion ledger catch
	// what slips through. PER RECIPIENT: one change makes several wakes, and
	// a shared id would deduplicate all but the first. See [changefeed.WakeID].
	WakeID uuid.UUID

	// WakeAt is when the change a DERIVED wake announces was made — the
	// record's own instant — and it becomes the wake's timestamp.
	//
	// WITH THE ID: the timestamp is half of the identity a turn derives
	// ([inbox.WorkSinceFor] takes the earliest trigger's, and every operation
	// id carries it). A fresh clock on each producer retry gave a re-run turn
	// different operation ids, so its writes were not collapsed. The record's
	// instant is the same on every copy and earlier than the wake, the safe
	// side for a lower bound.
	//
	// Zero keeps the wake's own instant, right only for a record that
	// carries none; a delivery-derived id takes the delivery's instant.
	WakeAt time.Time

	// Held is a recipient the change told and does not wake: nothing is
	// delivered, and an agent seat's held wake is recorded ([types.WakeHeld]).
	Held bool
}

// Recipient is an addressee, in any of the forms a third-party app can name one.
//
// SEVERAL forms rather than one, because third-party apps genuinely differ
// in what they put in a payload and a parser must not have to invent what
// it does not know: a tracker names an account id, a code host a username,
// a chat backend a user id, and only the engine's own producers know a
// handle.
// [Service] tries them in order.
type Recipient struct {
	// Handle is the seat's own identity, when the producer knew it.
	Handle string

	// Role is an exact role name, which subscriptions and schedules use.
	Role string

	// Email is a plus-address or a declared address.
	Email string

	// ExternalIDs are the third-party app's own identifiers, tried in the order
	// given within the notification's own source namespace. A slice
	// because a payload often carries several candidates — an assignee
	// id, a reporter login — and which one names a colleague is not
	// something the parser can tell.
	ExternalIDs []string
}

// Valve is the shared per-seat notification rate limiter.
//
// THREE-VALUED, and the error is which of the three: nil means the bool is an
// answer (under the cap, or definitively over it); a non-nil error means the
// valve could not say, WHATEVER the bool beside it. The caller fails open on
// that, so an implementation may return the false that comes naturally.
//
// Reopens is the end of the window containing now, the first instant a bucket
// refused at now is counted afresh. The live path drops a refused
// notification and never asks; replays (replay.go, a change feed's dead
// letters) arrive in a burst and are paced, so a refused seat's wake waits
// out the window rather than being lost.
type Valve interface {
	Allow(ctx context.Context, bucket string, limit int, now time.Time) (bool, error)
	Reopens(now time.Time) time.Time
}

// Admitter reports whether this node may take inbound work right now.
//
// The config posture. Nil is "always", which is the single-node case and the
// case before a control plane exists.
type Admitter func() bool

// Options configure a [Service].
type Options struct {
	// Queue is where deliveries arrive and where wakes are published.
	Queue queue.EventQueue

	// Registry resolves parties. Read through a function rather than
	// held, because an epoch swap builds a NEW registry and a service
	// holding the old one would resolve against a company that is no
	// longer running.
	Registry func() *Registry

	// Prompts renders each third-party app's trigger and answers its per-source
	// questions.
	Prompts Prompts

	// Parsers are the third-party apps this node can route. A delivery from a
	// source with no parser is recorded as skipped rather than dropped:
	// "nobody parses this" and "nothing happened" send an operator to
	// very different places.
	Parsers []Parser

	// Valve and RateLimit are the per-seat notification cap. A limit of
	// zero or less is off, which is the default, and then the valve is
	// never consulted.
	Valve     Valve
	RateLimit func() int

	// Admits gates the tick on the config posture. Nil is always.
	Admits Admitter

	// Now is the clock.
	Now func() time.Time

	// Observe is told each delivery of a source a parser reads, before it
	// is routed: an effect it has beyond its wakes. It never holds a
	// delivery back, so a failure there is its own to recover from.
	Observe func(ctx context.Context, source string, w types.RawWebhook)

	// FirstParty is every source this BUILD's own producers write a
	// delivery under (types.RawWebhook.FirstParty) — whether or not this
	// company runs it: the engine's native halves' feeds, its alarms' wakes,
	// its access reviews and its atlas's drift wakes. A first-party delivery
	// from a source NOT among them is one a newer build added, which this
	// node passes to a peer that parses it ([DeferNewerSource]); one from a
	// declared source this company does not run is acknowledged, as a source
	// nothing here parses.
	FirstParty []string

	// DeadLetters is what the replay of the inbound group's unhandled
	// deliveries lists, reads and drops them through (replay.go). Nil is
	// Queue's own verbs. The engine hands over the ledger its alarm reading
	// shares, so a drop the replay makes is accounted for as a wake
	// delivered, never counted as one the retention lost.
	DeadLetters DeadLetters
}

// DeadLetters is the queue's dead-letter verbs as the replay of the inbound
// group's unhandled deliveries calls them: the marks a running replay asks
// whether the group's unhandled subject moved by, the listing of that subject,
// one copy's frame, and the drop of a copy it handled.
type DeadLetters interface {
	DeadLetterMarks(ctx context.Context, under string) (queue.Marks, error)
	DeadLettersOn(ctx context.Context, subject string) ([]queue.Kept, error)
	DeadLetterFrame(ctx context.Context, subject string, seq uint64) ([]byte, bool, error)
	DropDeadLetter(ctx context.Context, subject string, seq uint64) error
}

// Service routes verified deliveries to the seats they concern.
type Service struct {
	queue    queue.EventQueue
	letters  DeadLetters
	registry func() *Registry
	prompts  Prompts
	parsers  map[string]Parser
	valve    Valve
	limit    func() int
	admits   Admitter
	now      func() time.Time
	observe  func(ctx context.Context, source string, w types.RawWebhook)

	// firstParty is [Options.FirstParty], as a set.
	firstParty map[string]bool

	// retired is every source whose parser [Service.Unregister] took away,
	// the only way to tell a DISCONNECTED surface from one nothing here ever
	// parsed. The next steps differ: a webhook still registered at the app
	// for a gone integration (remove it) versus an integration wired at the
	// edge only (wire it); one warning for both read as parser failures
	// (inbound_source_unparsed).
	//
	// Cleared only by a re-registration, the one event that makes the source
	// parseable again.
	retired map[string]bool

	mu      sync.RWMutex
	started bool
	// parsers, prompts and retired are guarded because a third-party app can
	// JOIN after the service is running — an extension registering a custom
	// transport, or a backend that came up late — and the delivery path
	// reads them on every event.

	// replaying is the replay [Service.Start] began, on a goroutine of its
	// own, until [Service.Stop] ends it: its cancel, first, closed once its
	// first pass has returned, and done, closed once it has. Nil before
	// Start.
	replaying *replayRun
}

// replayRun is one replay's goroutine, as the service that started it stops it.
type replayRun struct {
	cancel context.CancelFunc
	first  chan struct{}
	done   chan struct{}
}

// New builds the inbound service.
func New(opts Options) (*Service, error) {
	if opts.Queue == nil {
		return nil, fmt.Errorf("notify: the inbound service needs a queue")
	}
	if opts.Registry == nil {
		return nil, fmt.Errorf("notify: the inbound service needs a registry")
	}
	s := &Service{
		queue:    opts.Queue,
		letters:  opts.DeadLetters,
		registry: opts.Registry,
		prompts:  opts.Prompts,
		parsers:  make(map[string]Parser, len(opts.Parsers)),
		valve:    opts.Valve,
		limit:    opts.RateLimit,
		admits:   opts.Admits,
		now:      opts.Now,
		observe:  opts.Observe,

		firstParty: make(map[string]bool, len(opts.FirstParty)),
	}
	for _, source := range opts.FirstParty {
		s.firstParty[source] = true
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.letters == nil {
		s.letters = opts.Queue
	}
	for _, p := range opts.Parsers {
		if p == nil || p.Source() == "" {
			continue
		}
		if _, dup := s.parsers[p.Source()]; dup {
			return nil, fmt.Errorf("notify: two parsers claim source %q", p.Source())
		}
		s.parsers[p.Source()] = p
	}
	return s, nil
}

// Register adds a third-party app to a running service.
//
// This is how a custom transport joins: an integration that is not one of
// the shipped ones, or one that came up after boot. A source already claimed
// is refused rather than replaced — two parsers for one source means every
// delivery is interpreted by whichever registered last, which is not a
// choice anybody made.
//
// The prompt may be nil, and then this source renders through the generic
// fallback: a third-party app that can say who a delivery is FOR is already
// useful, and requiring it to also write a prompt before it can be routed
// at all would be a higher bar than the seam needs.
func (s *Service) Register(p Parser, prompt Prompt) error {
	if p == nil || p.Source() == "" {
		return fmt.Errorf("notify: a parser must name its source")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.parsers[p.Source()]; dup {
		return fmt.Errorf("notify: source %q already has a parser", p.Source())
	}
	s.parsers[p.Source()] = p
	delete(s.retired, p.Source())
	if prompt != nil {
		s.prompts = s.prompts.With(prompt)
	}
	return nil
}

// Prompts is the third-party app registry as it stands right now.
//
// A READ OF THE LIVE VALUE, because the registry is not fixed at boot: an
// apply calls [Service.Replace] for every integration the new revision
// enables, and a caller holding a copy taken at construction would keep
// rendering (and merging) with the third-party apps the process started
// with. The second reader is the inbox coalescer, whose per-integration
// supersede rules are exactly what a stale copy would drop.
//
// A VALUE, and [Prompts.With] copies on write, so what the caller gets cannot
// change underneath it mid-delivery.
func (s *Service) Prompts() Prompts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prompts
}

// Replace swaps the parser and prompt for a source already registered.
//
// The APPLY path. A parser is built from the company (projects, leads,
// instance), so a node that kept its boot-time parser would silently route a
// new revision's events by the old org chart.
//
// Distinct from [Service.Register], which REFUSES a duplicate source: at boot
// a second parser claiming one name is two integrations colliding, and
// swapping would let whichever registered last win.
func (s *Service) Replace(p Parser, prompt Prompt) error {
	if p == nil || p.Source() == "" {
		return fmt.Errorf("notify: a parser must name its source")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parsers[p.Source()] = p
	delete(s.retired, p.Source())
	if prompt != nil {
		s.prompts = s.prompts.With(prompt)
	}
	return nil
}

// Unregister removes a source's parser, reporting whether one was there.
//
// The counterpart of [Service.Replace], and the half that was missing: every
// integration reconciler converged only toward "configured", so setting
// `integrations.<vendor>.enabled: false` (the gesture an operator makes
// after a credential leak) applied cleanly, changed nothing, and left the
// boot-time parser routing deliveries under the credential being revoked.
//
// The PROMPT is deliberately left in place. Prompts are additive guidance
// keyed by their own identity rather than by source, several third-party apps
// contribute overlapping text, and a seat that reads one for a surface it no
// longer has is harmless where a seat missing one it does have is not.
func (s *Service) Unregister(source string) bool {
	if source == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.parsers[source]; !ok {
		return false
	}
	delete(s.parsers, source)
	if s.retired == nil {
		s.retired = map[string]bool{}
	}
	s.retired[source] = true
	return true
}

// Start attaches to the inbound topic, then begins the replay of the inbound
// group's unhandled deliveries — the wakes no node handled while they were
// offered (replay.go) — once at once and again whenever they move, for as long
// as the service runs, and returns without waiting for it.
//
// The replay follows the subscribe, so a failing replay never leaves the node
// deaf. It runs on its own goroutine, which [Service.Stop] ends, because a
// paused roll's backlog would hold boot and apply; its context keeps ctx's
// values but not its end. It outlives start: a copy kept later would wait for
// a restart (replay.go). Outside the lock, since the replay calls
// [Service.Handle], which read-locks, and RWMutex is not reentrant.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	if err := s.queue.Subscribe(ctx, topics.NotificationsInbound, InboundGroup, s.Handle); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("notify: subscribe inbound: %w", err)
	}
	s.started = true
	// The list is built HERE rather than through Sources(): this holds
	// the write lock, and Go's RWMutex is not reentrant — a read lock
	// taken by the same goroutine deadlocks the boot.
	sources := slices.Sorted(maps.Keys(s.parsers))
	replayCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	run := &replayRun{cancel: cancel, first: make(chan struct{}), done: make(chan struct{})}
	s.replaying = run
	s.mu.Unlock()
	log.InfoContext(ctx, "notify_inbound_started", "sources", sources)
	go func() {
		defer close(run.done)
		s.replay(replayCtx, run.first)
	}()
	return nil
}

// Stop ends what [Service.Start] began that outlives it, the replay, and waits
// for it to return, or for ctx to end, which it answers with ctx's error
// wrapped: the replay is then still running. Nothing before Start, and nothing
// more the second time.
//
// A REPLAY STOPPED PART WAY LOSES NOTHING: a copy it never reached is still
// kept, and one it was handling when stopped either settled — its wakes
// published and its entry dropped — or is kept as a delivery whose wake was not
// published, for another node's replay or the next start.
//
// The inbound subscription is left as it is: the queue it is on ends it, as it
// ends every subscription of the node.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	run := s.replaying
	s.mu.Unlock()
	if run == nil {
		return nil
	}
	run.cancel()
	select {
	case <-run.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("notify: stop the replay of the inbound group's unhandled "+
			"deliveries: %w", ctx.Err())
	}
}

// Sources lists the integrations this service can route, sorted — the
// operator's answer to "which of my integrations is actually wired?".
func (s *Service) Sources() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := slices.Sorted(maps.Keys(s.parsers))
	return out
}

// Handle routes one delivery.
//
// # A failure NAKs, a shed DEFERS, a record a newer build wrote PASSES
//
// A wake that could not publish fails as the NODE's failure where the node is
// why ([nodeFault]), kept for a replay ([queue.NakNode]). A shedding node
// DEFERS to the group's head and stops consuming until its posture admits
// work (resumeInbound): a NAK is withheld group-wide while this node fails the
// next too, and a republish reorders the conversation and returns here. A
// record a newer build wrote PASSES ([queue.Pass], from a [Deferral]) to a
// peer that can read it, never acked (crewlet#993) or deferred; unhandled after
// the group's 25 deliveries it is dead-lettered, then replayed.
func (s *Service) Handle(ctx context.Context, ev *events.Event) queue.Result {
	return s.handle(ctx, ev, false)
}

// handle routes one delivery, as [Service.Handle] does; replaying is the
// replay's (replay.go). A delivery the replay handles, and one a change feed's
// replay relayed (types.RawWebhook.Replayed), is PACED: its wake is asked of
// the rate valve in the replays' own bucket, and one the valve refuses waits
// for the valve's window to reopen and asks once more, rather than being
// dropped ([Service.deliver]). One the valve still refuses is kept by the
// replay, and failed as this node's own by the live path, so the group
// redelivers it; never skipped.
func (s *Service) handle(ctx context.Context, ev *events.Event, replaying bool) queue.Result {
	if s.admits != nil && !s.admits() {
		log.InfoContext(ctx, "inbound_delivery_deferred", "source", ev.Source,
			"event", ev.ID, "reason", "config posture is shedding")
		return queue.Defer(queue.DeferShedding, "config posture is shedding")
	}

	w, ok := events.DataAs[*types.RawWebhook](ev)
	if !ok || w == nil {
		// A payload this node cannot read will not become readable on a
		// redelivery, so ACK it rather than NAK: retrying a malformed
		// delivery burns the redelivery budget and dead-letters it
		// anyway, just later and noisier.
		log.ErrorContext(ctx, "inbound_payload_unreadable", "source", ev.Source, "event", ev.ID)
		s.skip(ctx, ev.Source, "", ReasonUnreadable, nil)
		return queue.Ack()
	}

	s.mu.RLock()
	parser, ok := s.parsers[ev.Source]
	retired := s.retired[ev.Source]
	prompts := s.prompts
	s.mu.RUnlock()
	if !ok {
		// RECORDED, not dropped. "Nobody parses this source" and
		// "nothing happened" are opposite facts, and only the first one
		// tells an operator their integration is wired at the edge and
		// nowhere else.
		//
		// AND A DISCONNECTED SURFACE IS NEITHER. A delivery arriving after
		// [Service.Unregister] is a webhook still registered at a
		// third-party app for an integration this company no longer has,
		// which reads as a parser failure under the same word and sends an
		// operator looking at the engine rather than at the registration
		// they have to remove. See [Service.retired].
		if retired {
			log.InfoContext(ctx, "inbound_source_retired",
				"source", ev.Source, "event", ev.ID,
				"detail", "this company no longer enables "+ev.Source+
					", so the delivery reaches no seat: remove the webhook "+
					"still registered at "+ev.Source)
			s.skip(ctx, ev.Source, "", ReasonRetired, nil)
			return queue.Ack()
		}
		// AND A FIRST-PARTY SOURCE THIS BUILD DOES NOT DECLARE IS A NEWER
		// BUILD'S. The engine's own producers mark what they publish, and
		// this build knows every source they write under, so a first-party
		// delivery from any other is a source a newer build added — a
		// domain given a wake feed during a roll — which a peer on that
		// build parses. Acked here, its wake was gone for every node, which
		// is crewlet#993's loss on the branch before the parser: it is
		// PASSED for that peer, as a newer record is, and kept with the
		// group's unhandled deliveries for a later replay should no such
		// peer win it. A vendor's delivery, and one from a declared source
		// this company does not run, is acknowledged as before.
		if w.FirstParty && !s.firstParty[ev.Source] {
			d := &Deferral{Reason: DeferNewerSource, Err: fmt.Errorf(
				"no parser for %q, a first-party source this build does not declare", ev.Source)}
			log.InfoContext(ctx, "inbound_deferred", "source", ev.Source,
				"event", ev.ID, "reason", string(d.Reason), "error", d.Err)
			return queue.Pass(string(d.Reason), d)
		}
		log.WarnContext(ctx, "inbound_source_unparsed", "source", ev.Source, "event", ev.ID)
		s.skip(ctx, ev.Source, "", ReasonUnparsed, nil)
		return queue.Ack()
	}

	reg := s.registry()
	if s.observe != nil {
		s.observe(ctx, ev.Source, *w)
	}
	routed, err := parser.Parse(ctx, *w, reg)
	if d, deferred := deferralOf(err); deferred {
		// NO SKIP IS RECORDED, because the delivery is not skipped: a
		// peer takes it, and a skip would tell a reader of the event log
		// that it reached nobody.
		log.InfoContext(ctx, "inbound_deferred", "source", ev.Source,
			"event", ev.ID, "reason", string(d.Reason), "error", d.Err)
		return queue.Pass(string(d.Reason), d)
	}
	if err != nil {
		log.ErrorContext(ctx, "inbound_parse_failed", "source", ev.Source,
			"event", ev.ID, "error", err.Error())
		s.skip(ctx, ev.Source, "", ReasonParseFailed, err)
		return queue.Ack()
	}
	if len(routed) == 0 {
		// The ordinary outcome. Most webhooks concern nobody here, and
		// recording a skip for each would bury the ones that matter.
		return queue.Ack()
	}

	paced := replaying || w.Replayed
	var errs []error
	for _, r := range routed {
		if err := s.deliver(ctx, prompts, reg, ev, w.Trigger, r, paced); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		// A publish failure is worth a redelivery: the delivery ledger holds
		// the claim, so the provider's own retry is refused.
		//
		// A failure of THIS NODE's ([nodeFault]) is not the delivery's: it is
		// never counted against the delivery, so one whose budget it spends
		// is kept with the unhandled deliveries, which a replay delivers,
		// not with the poison, which nothing replays ([queue.RetiredTo]).
		if nodeFault(ctx, errs) {
			return queue.NakNode(err)
		}
		return queue.Nak(err)
	}
	return queue.Ack()
}

// nodeFault reports whether every one of errs, a delivery's failed wakes, is
// a failure of THIS NODE's rather than of the delivery: one the queue tells as
// its node's ([queue.NodeFault]: a publish the broker did not answer, one
// refused by a queue that is no longer live, or any failure under a context
// that has ended, a stop cancelling the handler), or a paced wake the rate
// valve still refused ([errValveRefused]). One wake that failed for anything
// else, a wake too large to publish, which no node's retry changes, makes the
// delivery's failure its own.
func nodeFault(ctx context.Context, errs []error) bool {
	for _, err := range errs {
		switch {
		case queue.NodeFault(ctx, err), errors.Is(err, errValveRefused):
		default:
			return false
		}
	}
	return len(errs) > 0
}

// deliver resolves one recipient and wakes them, or records why not.
func (s *Service) deliver(ctx context.Context, prompts Prompts, reg *Registry, ev *events.Event,
	trigger string, r Routed, paced bool) error {
	party, ok := s.resolve(reg, r)
	if !ok {
		log.WarnContext(ctx, "notification_undeliverable", "source", r.Source,
			"handle", r.To.Handle, "email", r.To.Email)
		s.skip(ctx, r.Source, r.To.Handle, ReasonNoSeat, nil)
		return nil
	}
	// EVERY WAKE CARRIES A DERIVED ID — see [Routed.WakeID]: one its parser
	// derived, or else one derived here from the delivery's own and the
	// recipient's, under the delivery's own instant.
	if r.WakeID == uuid.Nil {
		r.WakeID, r.WakeAt = DeliveryWakeID(ev.ID, party.Handle), ev.Timestamp
	}
	if r.Held {
		return s.held(ctx, ev, r, party)
	}
	if deliverable, why := Deliverable(prompts, reg, r.Inbound, party); !deliverable {
		log.InfoContext(ctx, "notification_skipped", "source", r.Source,
			"handle", party.Handle, "reason", string(why))
		s.skip(ctx, r.Source, party.Handle, why, nil)
		return nil
	}
	// AND CAN IT BE ADDRESSED. A seat's mailbox is named by its agent id,
	// so a party with none has no subject to publish to. Separate from
	// [Deliverable] beside it, which answers whether this SHOULD be
	// delivered: that is a policy question about the event and this is a
	// routing fact about the recipient. Skipped rather than raised, because
	// the answer is PERMANENT and a nak would retry it for ever.
	if party.AgentID == uuid.Nil {
		log.WarnContext(ctx, "notification_unaddressable", "source", r.Source,
			"handle", party.Handle, "reason", string(ReasonUnaddressable))
		s.skip(ctx, r.Source, party.Handle, ReasonUnaddressable, nil)
		return nil
	}
	// THREE ANSWERS, NOT TWO. "Under the cap", "over the cap" and "the
	// valve could not say" are three different facts, and the error is
	// what tells the last two apart.
	//
	// The error was checked SECOND, behind `!allowed` — and every error the
	// only production valve returns comes back paired with false, because a
	// counter that cannot read its bucket cannot report a count. So a
	// coordination-store blip did not make the valve blind, it made it
	// SHUT: every inbound notification for every seat was dropped and
	// acked, and the fail-open below — the documented behaviour of
	// [Valve], and the reason a safety valve is allowed to exist on this
	// path at all — was unreachable code the whole time.
	allowed, err := s.allow(ctx, party, paced)
	if err == nil && !allowed && paced {
		// A REPLAYED WAKE IS PACED, NOT DROPPED: the replays' wakes piled
		// up over a roll and come in one burst, which the live path would
		// have spread over it, so the burst waits for the valve's window
		// to reopen and asks once more ([Service.allowReopened]).
		allowed, err = s.allowReopened(ctx, party)
	}
	switch {
	case err != nil:
		// FAILED OPEN: the notification is going through. Logged so an
		// operator can see the valve is blind rather than idle.
		log.WarnContext(ctx, "notification_valve_unavailable", "handle", party.Handle,
			"error", err.Error())
	case !allowed && paced:
		// STILL REFUSED once the window reopened: other replays fill the
		// replays' bucket for the seat — another node's, or a change
		// feed's. The wake is not delivered and never skipped as the live
		// path skips one: the replay keeps its copy, and a delivery a
		// feed's replay relayed fails as this node's own, for the group to
		// redeliver ([nodeFault]).
		return fmt.Errorf("notify: wake %s: %w", party.Handle, errValveRefused)
	case !allowed:
		log.WarnContext(ctx, "notification_rate_limited", "source", r.Source, "handle", party.Handle)
		s.skip(ctx, r.Source, party.Handle, ReasonRateLimited, nil)
		return nil
	}

	prompt := prompts.For(r.Source)
	salient := r.Body
	// A COPY, stamped with the resolved recipient before anything renders.
	//
	// The copy is not tidiness: a parser producing several recipients from
	// one payload may hand back one shared metadata map, and stamping the
	// handle into it would make every copy claim the last recipient. The
	// handle itself is a fact the prompt needs and the parser cannot know
	// — resolution happens here, after the cascade.
	meta := maps.Clone(r.Metadata)
	if meta == nil {
		meta = map[string]string{}
	}
	meta[RecipientField] = party.Handle
	r.Metadata = meta
	// THE WAKE'S OWN FENCE TOKEN, minted here because this is where the
	// trigger is built and no turn exists yet to lend one: the body is
	// fenced under it before the prompt renders it ([Render]), and the turn
	// that reads the trigger moves those fences onto its own token.
	r.Fence = provenance.NewBoundary()

	out := types.ExternalNotification{
		NotificationSource:   r.Source,
		SourceEventType:      r.EventType,
		RecipientEmail:       r.To.Email,
		Agent:                party.AgentID.String(),
		Sender:               r.Sender,
		SenderName:           senderName(reg, r.Inbound),
		Writer:               r.Writer,
		Subject:              r.Subject,
		SubjectRefs:          r.SubjectRefs,
		Body:                 Render(prompt, r.Inbound, reg),
		FenceBoundary:        string(r.Fence),
		SalientBody:          &salient,
		Metadata:             meta,
		ContextRequiresRecon: prompt.RequiresRecon(r.Inbound),
		SubjectIsLabel:       prompt.SubjectIsLabel(r.Inbound),
		Addressed:            prompt.Addressed(r.Inbound),
		Owes:                 r.Owes,
		Origin:               r.Origin,
	}
	// BOTH KEYS ride on the event: the partition key so the inbox coalescer
	// groups without re-deriving a third-party app's rule, and the
	// conversation identity so the turn's rows file where the next turn looks.
	//
	// THROUGH [Prompts.Partition] and [Prompts.Conversation], the namespacing
	// rule's one caller each: a local key is namespaced by source so a Jira
	// issue "42" and a GitLab issue "42" cannot merge into one trigger.
	partition := prompts.Partition(r.Inbound)
	conversation := prompts.Conversation(r.Inbound)
	// THE METADATA COPY CARRIES BOTH, under the envelope's own field names,
	// and that is a decision rather than symmetry: nothing in production
	// reads either back from here — the envelope below is what every reader
	// uses — but this map is what a stored notification shows a person
	// debugging one, and the envelope's own bag is not rendered anywhere.
	if partition != "" {
		meta[PartitionField] = partition
	}
	if conversation != "" {
		meta[ConversationField] = conversation
	}
	if trigger != "" {
		meta[TriggerField] = trigger
	}

	// The SAME trace the webhook edge started, so a delivery and the turn
	// it wakes are one story rather than two unrelated ones.
	wake := events.New(out, events.TraceContext{
		TraceID: ev.TraceID, ParentSpanID: ev.SpanID,
	})
	// THE DERIVED ID — see [Routed.WakeID]. It REPLACES the random one
	// events.New minted rather than riding beside it, because the id is
	// what the inbox and the fleet completion ledger key on, and a second
	// field would be a second thing to remember to check.
	wake.ID = r.WakeID
	// AND ITS INSTANT, which a producer retry must reproduce exactly as it
	// reproduces the id — see [Routed.WakeAt].
	if !r.WakeAt.IsZero() {
		wake.Timestamp = r.WakeAt.UTC()
	}
	wake.Source = "notify." + r.Source
	// AND ONTO THE ENVELOPE, which is what every reader actually uses. The
	// metadata map above travels INSIDE the typed payload, and the partition
	// function sees only an *events.Event — it reads the envelope's own bag
	// (see [Stamp] and [KeyOf]). While the key was written to the metadata
	// copy alone, every partition fell back to the event's own id, so ten
	// comments on one thread woke a seat ten times and ran ten turns instead
	// of the one digest turn the design describes. Both copies are kept: the
	// metadata one is what a prompt renders from, this one is what the broker
	// groups on and what the turn's conversation is keyed off.
	Stamp(wake, partition, conversation)
	// AND WHERE THE CHANGE THAT WOKE THE SEAT WAS COMMITTED, on the same bag
	// for the same reason: the turn reads it off the envelope, never out of
	// a typed payload a build may not decode.
	if trigger != "" {
		if wake.Payload == nil {
			wake.Payload = map[string]any{}
		}
		wake.Payload[TriggerField] = trigger
	}
	// A PERSON WAITING GOES FIRST: a wake that owes a person an answer
	// rides the seat's priority lane, ahead of whatever background work the
	// seat has queued (see priority.go). The mark is on the event, so a park
	// or a requeue of this wake keeps it.
	priority := RidesLane(prompt, reg, r.Inbound)
	if priority {
		queue.Prioritize(wake)
	}
	// THE PARTY'S ID, never its handle: a seat's mailbox is keyed on the
	// agent id derived from the handle (ADR-0013). A party with no id never
	// reaches here — the addressability guard above has already skipped it.
	if err := s.queue.Publish(ctx, topics.AgentInbox(party.AgentID), wake); err != nil {
		return fmt.Errorf("notify: wake %s: %w", party.Handle, err)
	}
	// AND TELL THE SEAT'S NODE a person is waiting, after the wake is
	// durable, so the line it may draw is never for a message that is not
	// there.
	if priority {
		s.announceWaiting(ctx, party.AgentID, wake.ID, meta)
	}
	log.InfoContext(ctx, "notification_routed", "source", r.Source, "handle", party.Handle,
		"agent_id", party.AgentID.String())
	return nil
}

// DeliveryWakeID is the id of the wake delivery makes for the seat handle
// where its parser derived none ([Routed.WakeID]): the same on every node, on
// every redelivery of the delivery and on every replay of its copy, so the
// seat's inbox and the fleet's completion ledger collapse a second wake of one
// delivery as they collapse any redelivered wake. PER RECIPIENT, as
// changefeed.WakeID is, because one delivery legitimately wakes several seats.
func DeliveryWakeID(delivery uuid.UUID, handle string) uuid.UUID {
	return uuid.NewSHA1(deliveryWakeNamespace, []byte(delivery.String()+"\x00"+handle))
}

// deliveryWakeNamespace scopes the wake ids derived from a delivery's own
// ([DeliveryWakeID]). Fixed for the life of the deployment: a new one would
// make every redelivery a fresh wake.
var deliveryWakeNamespace = uuid.MustParse("7c4e9a12-3b5d-4f6e-8a90-1b2c3d4e5f60")

// HeldID is the id a held wake is recorded under: name-based on the id its
// wake would have had, the same on every node and redelivery, never that id.
func HeldID(wake uuid.UUID) uuid.UUID { return uuid.NewSHA1(wake, []byte("held")) }

// held records an agent seat's held wake, once per change and seat: under an id
// derived from the one its wake would have had, so a redelivery collapses onto
// it and a wake another build makes of the same change is never taken for it.
// A person, or a seat with no id, has no turn to hold and records nothing.
func (s *Service) held(ctx context.Context, ev *events.Event, r Routed, party Party) error {
	if party.AgentID == uuid.Nil || party.Human {
		return nil
	}
	rec := events.New(types.WakeHeld{Agent: party.AgentID.String(), AgentHandle: party.Handle,
		NotificationSource: r.Source, Origin: r.Origin},
		events.TraceContext{TraceID: ev.TraceID, ParentSpanID: ev.SpanID})
	if r.WakeID != uuid.Nil {
		rec.ID = HeldID(r.WakeID)
		if !r.WakeAt.IsZero() {
			rec.Timestamp = r.WakeAt.UTC()
		}
	}
	rec.Source = "notify." + r.Source
	if err := s.queue.Publish(ctx, topics.Event(types.WakeHeld{}.EventType()), rec); err != nil {
		return fmt.Errorf("notify: record %s's held wake: %w", party.Handle, err)
	}
	return nil
}

// resolve runs the recipient cascade.
//
// Handle, then role, then email, then the third-party app's own ids, most
// specific first, because each later form is a guess the earlier one did
// not need to make. A handle names a seat exactly; an external id names one
// only if somebody registered it.
func (s *Service) resolve(reg *Registry, r Routed) (Party, bool) {
	if reg == nil {
		return Party{}, false
	}
	if h := r.To.Handle; h != "" {
		if p, ok := reg.ByHandle(h); ok {
			return p, true
		}
	}
	if n := r.To.Role; n != "" {
		if p, ok := reg.ByRole(n); ok {
			return p, true
		}
	}
	if e := r.To.Email; e != "" {
		if p, ok := reg.ByEmail(e); ok {
			return p, true
		}
	}
	for _, id := range r.To.ExternalIDs {
		if p, ok := reg.ByExternalID(r.Source, id); ok {
			return p, true
		}
	}
	return Party{}, false
}

// errValveRefused is a paced wake the rate valve refused even once its window
// had reopened ([Service.allowReopened]): not delivered, and never skipped —
// the replay keeps its copy for a later replay, and the live path fails a
// delivery a feed's replay relayed as this node's own, for the group to
// redeliver.
var errValveRefused = errors.New("the notification rate limit refused the wake")

// allowReopened waits for the end of the valve's window that refused party's
// paced wake ([Valve.Reopens]) and asks the valve once more, in the replays'
// bucket: a burst paced at the valve's own rate. A context that ends while it
// waits answers refused, never a failure, which fails open: a replay that
// stops keeps the copy.
//
// ONCE, AND NOT UNTIL ADMITTED. The replay's own copies are asked one after
// another, so the window that opens holds none of them yet, and a refusal
// there is other replays filling the replays' bucket — another node's, or a
// change feed's — which a replay waiting it out would join, holding every
// other seat's copies behind it.
func (s *Service) allowReopened(ctx context.Context, party Party) (bool, error) {
	if wait := s.valve.Reopens(s.now()).Sub(s.now()); wait > 0 {
		reopened := time.NewTimer(wait)
		defer reopened.Stop()
		select {
		case <-ctx.Done():
			return false, nil
		case <-reopened.C:
		}
	}
	return s.allow(ctx, party, true)
}

// allow consults the per-seat valve: in the live path's bucket, or, for a
// paced wake, in the replays' own. Off by default and then never reads the
// counter.
//
// THE REPLAYS' OWN BUCKET, at the same limit: paced in the live bucket, a
// replay filled each window as it opened, and the live path drops a refused
// wake, so a person's mention in that window was lost; a refused replayed
// wake costs nothing, being kept or redelivered. A seat can thus be woken up
// to twice its limit in a window while a replay runs, the backlog no faster
// than the limit.
func (s *Service) allow(ctx context.Context, party Party, paced bool) (bool, error) {
	if s.valve == nil || s.limit == nil {
		return true, nil
	}
	limit := s.limit()
	if limit <= 0 {
		return true, nil
	}
	bucket := "notify:" + party.AgentID.String()
	if paced {
		bucket = "notify-replay:" + party.AgentID.String()
	}
	return s.valve.Allow(ctx, bucket, limit, s.now())
}

// SkipReason is why a delivery woke nobody, in the words its
// notification_skipped record carries.
//
// DECLARED ONCE, HERE, because the words tell every reader WHICH gate dropped
// a delivery; spelled again at each reader, a reworded reason leaves copies
// asserting the old words. The record's field stays a string
// ([types.NotificationSkipped] cannot import this package), compared as
// SkipReason(record.Reason). Its Reason is one of these words and nothing
// more: what a gate says about one delivery goes in the record's Detail
// ([ReasonParseFailed]).
type SkipReason string

// The reasons a delivery is skipped for, in the order the service meets them.
const (
	// ReasonUnreadable: the delivery's payload is not a webhook this node
	// can read, and a redelivery will not make it one.
	ReasonUnreadable SkipReason = "unreadable delivery payload"
	// ReasonRetired: the delivery is from an integration this company has
	// disconnected — a webhook still registered at the third-party app.
	ReasonRetired SkipReason = "this source was disconnected"
	// ReasonUnparsed: nothing parses this source — an integration wired at
	// the edge and nowhere else.
	ReasonUnparsed SkipReason = "no parser for this source"
	// ReasonParseFailed: the source's parser refused the payload. The one
	// reason recorded with a Detail — the parser's error — because which
	// payload a parser cannot read is the whole of what an operator needs
	// to fix it.
	ReasonParseFailed SkipReason = "parse failed"
	// ReasonNoSeat: the parser named a recipient no seat answers to.
	ReasonNoSeat SkipReason = "no seat matches this recipient"
	// ReasonHumanSeat: the recipient is a person, who reads the surface the
	// event arrived on rather than being woken ([Deliverable]).
	ReasonHumanSeat SkipReason = "human seat"
	// ReasonSelfAction: the recipient caused the event, and it is not an
	// outcome they need to hear about ([Deliverable]).
	ReasonSelfAction SkipReason = "self-action: the recipient caused this event"
	// ReasonUnaddressable: the recipient resolved to a seat with no agent
	// id, so it has no mailbox to publish to. Permanent, which is why it is
	// a skip rather than a retry.
	ReasonUnaddressable SkipReason = "the seat has no agent id, so it has no mailbox"
	// ReasonRateLimited: the company's notification rate limit refused the
	// recipient another wake.
	ReasonRateLimited SkipReason = "rate limit exceeded"
)

// Valid reports whether r is a reason this build records. A reason read off
// a record a newer build wrote may not be, and is still a value to show
// rather than one to refuse.
func (r SkipReason) Valid() bool {
	switch r {
	case ReasonUnreadable, ReasonRetired, ReasonUnparsed, ReasonParseFailed,
		ReasonNoSeat, ReasonHumanSeat, ReasonSelfAction, ReasonUnaddressable,
		ReasonRateLimited:
		return true
	}
	return false
}

// skip records why a delivery did not wake anybody: the reason, and cause's
// text as the record's Detail when there is one ([ReasonParseFailed]).
//
// Best effort and never fatal: the delivery has already been decided, and
// failing it because the bookkeeping could not be published would turn a
// recorded non-event into a redelivered one.
func (s *Service) skip(ctx context.Context, source, handle string, reason SkipReason, cause error) {
	var detail string
	if cause != nil {
		detail = cause.Error()
	}
	ev := events.New(types.NotificationSkipped{
		Handle: handle, Reason: string(reason), Detail: detail, NotificationSource: source,
	}, tracing.TraceOf(ctx))
	ev.Source = "notify." + source
	if err := s.queue.Publish(ctx, topics.Event(types.NotificationSkipped{}.EventType()), ev); err != nil {
		log.WarnContext(ctx, "notification_skip_unrecorded", "source", source,
			"handle", handle, "reason", string(reason), "detail", detail, "error", err.Error())
	}
}

// senderName is the name the company knows a wake's sender by, the party the
// registry resolves it to, and "" for a sender nobody in the company is. It
// titles the turn the wake starts ([types.ExternalNotification.Summary]).
func senderName(reg *Registry, in Inbound) string {
	if reg == nil {
		return ""
	}
	party, ok := reg.ByExternalID(in.Source, strings.TrimSpace(in.Sender))
	if !ok {
		return ""
	}
	return strings.TrimSpace(party.Name)
}
