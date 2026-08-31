package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
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
// A vendor's whole inbound surface, and the ONLY thing a vendor must write
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
	// payload; no notifications is an ordinary outcome, and by far the
	// most common one — most webhooks concern nobody here.
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

	// To names the recipient in whatever terms the vendor could supply.
	To Recipient
}

// Recipient is an addressee, in any of the forms a vendor can name one.
//
// SEVERAL forms rather than one, because vendors genuinely differ in what
// they put in a payload and a parser must not have to invent what it does
// not know: a tracker names an account id, a code host a username, a chat
// backend a user id, and only the engine's own producers know a handle.
// [Service] tries them in order.
type Recipient struct {
	// Handle is the seat's own identity, when the producer knew it.
	Handle string

	// Role is an exact role name, which subscriptions and schedules use.
	Role string

	// Email is a plus-address or a declared address.
	Email string

	// ExternalIDs are the vendor's own identifiers, tried in the order
	// given within the notification's own source namespace. A slice
	// because a payload often carries several candidates — an assignee
	// id, a reporter login — and which one names a colleague is not
	// something the parser can tell.
	ExternalIDs []string
}

// Valve is the shared per-seat notification rate limiter.
//
// Reports whether the seat is under its cap, counting this call. The error
// is for logging: the valve FAILS OPEN, so an unreachable counter reports
// true — a valve that cannot be reached must not stop real notifications.
type Valve interface {
	Allow(ctx context.Context, bucket string, limit int, now time.Time) (bool, error)
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

	// Prompts renders each vendor's trigger and answers its per-source
	// questions.
	Prompts Prompts

	// Parsers are the vendors this node can route. A delivery from a
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
}

// Service routes verified deliveries to the seats they concern.
type Service struct {
	queue    queue.EventQueue
	registry func() *Registry
	prompts  Prompts
	parsers  map[string]Parser
	valve    Valve
	limit    func() int
	admits   Admitter
	now      func() time.Time

	mu      sync.RWMutex
	started bool
	// parsers and prompts are guarded because a vendor can JOIN after the
	// service is running — an extension registering a custom transport,
	// or a backend that came up late — and the delivery path reads them
	// on every event.
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
		registry: opts.Registry,
		prompts:  opts.Prompts,
		parsers:  make(map[string]Parser, len(opts.Parsers)),
		valve:    opts.Valve,
		limit:    opts.RateLimit,
		admits:   opts.Admits,
		now:      opts.Now,
	}
	if s.now == nil {
		s.now = time.Now
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

// Register adds a vendor to a running service.
//
// This is how a custom transport joins: an integration that is not one of
// the shipped ones, or one that came up after boot. A source already claimed
// is refused rather than replaced — two parsers for one source means every
// delivery is interpreted by whichever registered last, which is not a
// choice anybody made.
//
// The prompt may be nil, and then this source renders through the generic
// fallback: a vendor that can say who a delivery is FOR is already useful,
// and requiring it to also write a prompt before it can be routed at all
// would be a higher bar than the seam needs.
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
	if prompt != nil {
		s.prompts = s.prompts.With(prompt)
	}
	return nil
}

// Replace swaps the parser and prompt for a source already registered.
//
// The APPLY path. A parser is built from the company — which projects exist,
// who leads them, which instance to read — and an epoch is published rather
// than mutated, so a node that kept its boot-time parser would route a new
// revision's events by the old company's org chart. That failure is silent:
// every event still routes, just to whoever led the project when the process
// started.
//
// Distinct from [Service.Register], which REFUSES a duplicate source, and
// deliberately: at boot a second parser claiming one source name is two
// integrations colliding, and swapping one for the other silently would make
// whichever registered last win. The two callers want opposite answers to
// the same situation, so they ask different questions.
func (s *Service) Replace(p Parser, prompt Prompt) error {
	if p == nil || p.Source() == "" {
		return fmt.Errorf("notify: a parser must name its source")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parsers[p.Source()] = p
	if prompt != nil {
		s.prompts = s.prompts.With(prompt)
	}
	return nil
}

// Unregister removes a source's parser, reporting whether one was there.
//
// The counterpart of [Service.Replace], and the half that was missing: every
// vendor reconciler converged only toward "configured", so setting
// `integrations.<vendor>.enabled: false` — the gesture an operator makes
// after a credential leak — applied cleanly, changed nothing, and left the
// boot-time parser routing deliveries under the credential being revoked.
//
// The PROMPT is deliberately left in place. Prompts are additive guidance
// keyed by their own identity rather than by source, several vendors
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
	return true
}

// Start attaches to the inbound topic.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	if err := s.queue.Subscribe(ctx, topics.NotificationsInbound, InboundGroup, s.Handle); err != nil {
		return fmt.Errorf("notify: subscribe inbound: %w", err)
	}
	s.started = true
	// The list is built HERE rather than through Sources(): this holds
	// the write lock, and Go's RWMutex is not reentrant — a read lock
	// taken by the same goroutine deadlocks the boot.
	sources := make([]string, 0, len(s.parsers))
	for src := range s.parsers {
		sources = append(sources, src)
	}
	slices.Sort(sources)
	log.InfoContext(ctx, "notify_inbound_started", "sources", sources)
	return nil
}

// Sources lists the integrations this service can route, sorted — the
// operator's answer to "which of my integrations is actually wired?".
func (s *Service) Sources() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.parsers))
	for src := range s.parsers {
		out = append(out, src)
	}
	slices.Sort(out)
	return out
}

// Handle routes one delivery.
//
// # Returning an error NAKs; returning DeferDelivery parks
//
// A shedding node DEFERS — leaves the delivery unacked and stops consuming —
// rather than NAKing or republishing. A NAK dead-letters a healthy delivery
// after a few one-second redeliveries while a shed can last minutes. A
// republish is worse: inbound events are key-partitioned by conversation, so
// a copy sent to the topic's tail while its siblings replay from the head
// reorders that conversation, and it lands on a topic this node is still
// attached to — so it comes straight back, is shed again and republished
// again as fast as the broker will serve it. A deferral cannot spin.
func (s *Service) Handle(ctx context.Context, ev *events.Event) queue.Result {
	if s.admits != nil && !s.admits() {
		log.InfoContext(ctx, "inbound_delivery_deferred", "source", ev.Source,
			"event", ev.ID, "reason", "config posture is shedding")
		return queue.Defer("config posture is shedding")
	}

	w, ok := events.DataAs[*types.RawWebhook](ev)
	if !ok || w == nil {
		// A payload this node cannot read will not become readable on a
		// redelivery, so ACK it rather than NAK: retrying a malformed
		// delivery burns the redelivery budget and dead-letters it
		// anyway, just later and noisier.
		log.ErrorContext(ctx, "inbound_payload_unreadable", "source", ev.Source, "event", ev.ID)
		s.skip(ctx, ev.Source, "", "unreadable delivery payload")
		return queue.Ack()
	}

	s.mu.RLock()
	parser, ok := s.parsers[ev.Source]
	prompts := s.prompts
	s.mu.RUnlock()
	if !ok {
		// RECORDED, not dropped. "Nobody parses this source" and
		// "nothing happened" are opposite facts, and only the first one
		// tells an operator their integration is wired at the edge and
		// nowhere else.
		log.WarnContext(ctx, "inbound_source_unparsed", "source", ev.Source, "event", ev.ID)
		s.skip(ctx, ev.Source, "", "no parser for this source")
		return queue.Ack()
	}

	reg := s.registry()
	routed, err := parser.Parse(ctx, *w, reg)
	if err != nil {
		log.ErrorContext(ctx, "inbound_parse_failed", "source", ev.Source,
			"event", ev.ID, "error", err.Error())
		s.skip(ctx, ev.Source, "", "parse failed: "+err.Error())
		return queue.Ack()
	}
	if len(routed) == 0 {
		// The ordinary outcome. Most webhooks concern nobody here, and
		// recording a skip for each would bury the ones that matter.
		return queue.Ack()
	}

	var errs []error
	for _, r := range routed {
		if err := s.deliver(ctx, prompts, reg, ev, r); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		// A publish failure is worth a redelivery: the wake never
		// happened. The API's delivery ledger holds the claim, so the
		// provider's own retry is refused — this redelivery is the only
		// second chance the wake gets.
		return queue.Nak(err)
	}
	return queue.Ack()
}

// deliver resolves one recipient and wakes them, or records why not.
func (s *Service) deliver(ctx context.Context, prompts Prompts, reg *Registry, ev *events.Event, r Routed) error {
	party, ok := s.resolve(reg, r)
	if !ok {
		log.WarnContext(ctx, "notification_undeliverable", "source", r.Source,
			"handle", r.To.Handle, "email", r.To.Email)
		s.skip(ctx, r.Source, r.To.Handle, "no seat matches this recipient")
		return nil
	}
	if deliverable, why := Deliverable(prompts, reg, r.Inbound, party); !deliverable {
		log.InfoContext(ctx, "notification_skipped", "source", r.Source,
			"handle", party.Handle, "reason", why)
		s.skip(ctx, r.Source, party.Handle, why)
		return nil
	}
	if allowed, err := s.allow(ctx, party); !allowed {
		log.WarnContext(ctx, "notification_rate_limited", "source", r.Source, "handle", party.Handle)
		s.skip(ctx, r.Source, party.Handle, "rate limit exceeded")
		return nil
	} else if err != nil {
		// FAILED OPEN: the notification is going through. Logged so an
		// operator can see the valve is blind rather than idle.
		log.WarnContext(ctx, "notification_valve_unavailable", "handle", party.Handle,
			"error", err.Error())
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

	out := types.ExternalNotification{
		NotificationSource:   r.Source,
		SourceEventType:      r.EventType,
		RecipientEmail:       r.To.Email,
		Agent:                party.AgentID.String(),
		Sender:               r.Sender,
		Subject:              r.Subject,
		Body:                 prompt.Build(r.Inbound, reg),
		SalientBody:          &salient,
		Metadata:             meta,
		ContextRequiresRecon: prompt.RequiresRecon(r.Inbound),
	}
	// The conversation key rides on the event so the inbox coalescer can
	// partition by it without re-deriving a vendor's rule. Derived here,
	// where the vendor's prompt is already in hand.
	conversation := ""
	if key := prompt.ConversationKey(meta, r.Subject); key != "" {
		conversation = Namespaced(r.Source, key)
		meta[KeyField] = conversation
	}

	// The SAME trace the webhook edge started, so a delivery and the turn
	// it wakes are one story rather than two unrelated ones.
	wake := events.New(out, events.TraceContext{
		TraceID: ev.TraceID, ParentSpanID: ev.SpanID,
	})
	wake.Source = "notify." + r.Source
	// AND ONTO THE ENVELOPE, which is what the inbox actually partitions
	// on. The metadata map above travels INSIDE the typed payload, and the
	// partition function sees only an *events.Event — it reads the
	// envelope's own bag (see [Stamp] and [KeyOf]). While the key was
	// written to the metadata copy alone, every partition fell back to the
	// event's own id, so ten comments on one thread woke a seat ten times
	// and ran ten turns instead of the one digest turn the design
	// describes. Both copies are kept: the metadata one is what a prompt
	// renders from, this one is what the broker groups on.
	Stamp(wake, conversation)
	if err := s.queue.Publish(ctx, topics.AgentInbox(party.Handle), wake); err != nil {
		return fmt.Errorf("notify: wake %s: %w", party.Handle, err)
	}
	log.InfoContext(ctx, "notification_routed", "source", r.Source, "handle", party.Handle,
		"agent_id", party.AgentID.String())
	return nil
}

// resolve runs the recipient cascade.
//
// Handle, then role, then email, then the vendor's own ids — most specific
// first, because each later form is a guess the earlier one did not need to
// make. A handle names a seat exactly; an external id names one only if
// somebody registered it.
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

// allow consults the per-seat valve.
//
// Off by default and skipped entirely when it is off, so a company that
// never asked for a rate limit never reads the counter.
func (s *Service) allow(ctx context.Context, party Party) (bool, error) {
	if s.valve == nil || s.limit == nil {
		return true, nil
	}
	limit := s.limit()
	if limit <= 0 {
		return true, nil
	}
	ok, err := s.valve.Allow(ctx, "notify:"+party.AgentID.String(), limit, s.now())
	return ok, err
}

// skip records why a delivery did not wake anybody.
//
// Best effort and never fatal: the delivery has already been decided, and
// failing it because the bookkeeping could not be published would turn a
// recorded non-event into a redelivered one.
func (s *Service) skip(ctx context.Context, source, handle, reason string) {
	ev := events.New(types.NotificationSkipped{
		Handle: handle, Reason: reason, NotificationSource: source,
	}, tracing.TraceOf(ctx))
	ev.Source = "notify." + source
	if err := s.queue.Publish(ctx, topics.Event(types.NotificationSkipped{}.EventType()), ev); err != nil {
		log.WarnContext(ctx, "notification_skip_unrecorded", "source", source,
			"handle", handle, "reason", reason, "error", err.Error())
	}
}

// DelegationOf reads the delegation bookkeeping a producer put on a
// notification's metadata, so the woken seat's turn engine can enforce the
// depth cap.
//
// Most webhooks set none of it. What is present comes from an in-process
// event or a producer carrying it across a webhook boundary, where metadata
// values are strings of arbitrary shape — so every field is safe-parsed and
// falls back to a default rather than aborting a notification that is
// otherwise perfectly routable.
func DelegationOf(m map[string]string) (depth int, parent string, chain []string) {
	if raw := m["delegation_depth"]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			log.Warn("delegation_depth_unparsed", "raw", raw)
		} else if n > 0 {
			depth = n
		}
	}
	parent = m["parent_turn_id"]
	if raw := m["delegation_chain"]; raw != "" {
		var parsed []any
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			log.Warn("delegation_chain_unparsed", "raw", raw)
		} else {
			for _, v := range parsed {
				// Drop nil and empty, but KEEP falsy-but-valid
				// values: a producer encoding numeric ids must
				// not lose a 0 to a truthiness filter.
				if v == nil {
					continue
				}
				if s := fmt.Sprint(v); s != "" {
					chain = append(chain, s)
				}
			}
		}
	}
	return depth, parent, chain
}
