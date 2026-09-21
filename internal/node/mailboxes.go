package node

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// mailboxes is what this node believes about the company's mailboxes.
//
// EVERYTHING HERE IS DERIVED FROM THE BROKER, never from what this node did.
// The distinction is the whole design: a set filled in by "I created these"
// is a set that survives the consumers it describes, and a recreated stream
// has no consumers at all — so a node trusting its own memory would leave
// every seat in the company with no mailbox and nothing to say so, which is
// the failure [Node.EnsureMailboxes] exists to prevent rather than a new way
// to cause it.
//
// Guarded by its own mutex rather than by the node's, because the node's
// guards the attachment map that the seat host's hooks write from their own
// goroutines and this is written by the convergence loop. Never held across a
// broker call.
type mailboxes struct {
	// pass serialises whole convergence passes against each other. Two
	// callers drive this concurrently — the loop's tick and a config
	// apply — and without it an apply landing on a tick sends the
	// broker two creates for every seat the revision added, which is
	// precisely the cost this pass exists to remove. A pass with nothing
	// to do makes no broker call at all, so the wait is a wait on
	// nothing in the normal case.
	//
	// Lock order is pass -> mu, never the reverse; mu is taken only by
	// the small accessors below and never held across a broker call.
	pass sync.Mutex

	mu sync.Mutex

	// ensured is the seats the broker has said it holds a mailbox for,
	// plus the ones this pass has just made. KEYED ON THE SEAT'S ID, which
	// is what its mailbox is named by. Pruned to the company's current
	// seats on every pass: a seat that leaves the company and comes back is
	// one whose mailbox the retirement sweep may have deleted in between
	// (see [MailboxRegistry]), so remembering it across its absence is how
	// a returning seat gets no mailbox.
	ensured map[uuid.UUID]struct{}

	// seededAt is when ensured was last taken from the broker's own
	// listing. Zero means never, which is a node that has not converged
	// once yet.
	seededAt time.Time

	// missingSince is when this node first failed to make a seat's
	// mailbox, and alarmedAt when it last said so out loud. Both are
	// cleared the moment the mailbox exists.
	missingSince map[uuid.UUID]time.Time
	alarmedAt    map[uuid.UUID]time.Time
}

func newMailboxes() *mailboxes {
	return &mailboxes{
		ensured:      map[uuid.UUID]struct{}{},
		missingSince: map[uuid.UUID]time.Time{},
		alarmedAt:    map[uuid.UUID]time.Time{},
	}
}

// EnsureMailboxes converges the durable subscription behind every agent seat
// in the company — not just the ones this node claims.
//
// A DURABLE SUBSCRIPTION IS A SEAT'S MAILBOX: it exists without a consumer and
// retains what is published while nothing is attached. Its absence is not an
// error anybody sees, because publishing to a topic no subscription covers
// DROPS THE EVENT SILENTLY. That is a whole class of quiet loss: a company's
// seats are claimed a few at a time across successive sweeps, so every
// webhook, notification and scheduled trigger aimed at a seat this fleet had
// not reached yet goes nowhere — during boot, during a rollout, and
// permanently for any seat no live node's placement matches.
//
// EVERY seat, not this node's share: a mailbox is a fact about the company,
// and the node that ends up serving a seat may not be this one.
//
// # It is a convergence, not a sweep
//
// It used to be a walk: every seat in the company, one EnsureSubscription
// each, on boot and on every config apply. Each of those is a CREATE, and on
// the broker this engine ships a create is a proposal through the metadata
// Raft group whether or not the consumer is already there — so a company of N
// seats spent N replicated writes per apply to be told what it already knew.
// See docs/concepts/scaling.md for the measured numbers.
//
// So this pass asks what is missing before it writes anything:
//
//   - The set of mailboxes that exist is SEEDED FROM THE BROKER, with one
//     subscription listing over the seat-inbox subject space, diffed against
//     the company's seats. A listing is a read; the creates were writes.
//   - Only the difference is ensured. A pass with nothing missing and a fresh
//     seed makes no broker call at all, which is what a seat-sweep tick costs
//     in the steady state.
//   - The seed is re-taken when it is older than the seat lease TTL, because
//     a set is a claim about somebody else's state and this one has exactly
//     one way to go wrong: the stream is recreated, its consumers are gone,
//     and a node reading its own memory would never notice. The TTL is not a
//     knob of this pass's own — it is how long the fleet already tolerates a
//     seat being unserved, and therefore how long a mailbox may be missing
//     before somebody has to be told.
//
// # What a failure costs
//
// Best effort, per seat. A subscription that cannot be created is left OUT of
// the set, so the next tick tries it again, and the rest of the company still
// gets theirs — the alternative is a node that refuses to start because one
// topic was unreachable, which loses strictly more mail. A seat still missing
// one lease TTL later raises [Node.alarmMissingMailbox]; a single failure that
// the next tick repairs is not news, and *still failing* is.
//
// REGISTERED FIRST. Each seat's mailbox is recorded with the fleet before it
// is created, because once the seat leaves the company the handle is gone from
// the org every node derives the name from, and the record is what the
// retirement of that mailbox runs on. A registration that fails is logged and
// the mailbox is created anyway, since a seat in the company losing mail is
// worse than a mailbox the maintenance sweep registers on its next tick.
//
// Exported because three callers drive it: the node's own start, before it
// claims anything; the config apply, because a revision that ADDS a role adds
// a seat whose mail is dropped until something makes it a mailbox; and the
// convergence loop, which is what makes the other two a floor rather than the
// only chances this node gets.
func (n *Node) EnsureMailboxes(ctx context.Context) {
	n.mail.pass.Lock()
	defer n.mail.pass.Unlock()

	// ONE CEILING OVER THE WHOLE PASS, because a pass that does have work
	// is one replicated create PER SEAT in a row and each carries its own
	// per-create budget. A company of thirty seats on a wedged metadata
	// group would otherwise hold the boot for thirty of them, serially —
	// the product again, which is the bound [jsprovision.SequenceBudget]
	// exists to replace.
	//
	// It reads the topology off the queue, so a solo node keeps the short
	// one and nothing here has to be told which it is.
	if c, ok := n.cfg.Queue.(interface {
		Clustered() jsprovision.Clustered
	}); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Clustered().SequenceBudget())
		defer cancel()
	}

	// ONE READ of the seat list for the walk and the line that reports it.
	// Two reads straddled an apply that changed the seats, and the log then
	// claimed a count the walk never covered.
	seats := n.addressableSeats()

	missing, stale := n.mail.diff(seats, n.host.TTL(), time.Now())
	if len(missing) == 0 && !stale {
		// THE STEADY STATE, and it is silent: every seat in the company
		// has a mailbox and the set saying so is younger than a lease.
		// No listing, no create, nothing sent to the broker at all.
		n.log.Debug("seat_mailboxes_converged", "seats", len(seats))
		return
	}

	if listed, ok := n.listMailboxes(ctx); ok {
		// THE BROKER DECIDES, replacing whatever this node believed. A
		// consumer it has just created and not yet reported back costs
		// one redundant ensure on this pass, which is a create that
		// returns the consumer that is already there; a consumer that is
		// GONE and remembered as present costs a seat its mail for the
		// life of the process.
		missing = n.mail.adopt(seats, listed, time.Now())
	} else if len(missing) == 0 {
		// The listing is the only thing that could have told this pass
		// anything, and it did not. Nothing is known to be missing, so
		// there is nothing to do but come back on the next tick.
		return
	}

	created := 0
	for _, s := range missing {
		handle := s.Handle
		inbox, group := topics.AgentInbox(s.ID), topics.AgentInboxGroup(s.ID)
		if n.cfg.Mailboxes != nil {
			if err := n.cfg.Mailboxes.Register(ctx, s); err != nil {
				if ctx.Err() != nil {
					n.reportInterrupted(ctx)
					return
				}
				n.log.Warn("seat_mailbox_unregistered", "handle", handle, "error", err,
					"detail", "the mailbox is created anyway; until the maintenance sweep "+
						"registers it, it cannot be retired if this seat is removed")
			}
		}
		made, err := n.cfg.Queue.EnsureSubscription(ctx, inbox, group)
		if errors.Is(err, queue.ErrNotLive) {
			// The queue is not up yet, which is not a fault: the boot
			// apply runs before Start and Start does this again a moment
			// later. Returning rather than continuing, because every
			// remaining seat would report the same thing — one honest
			// line beats seven identical warnings about a state that is
			// about to resolve itself.
			n.log.Debug("seat_mailboxes_deferred",
				"detail", "the broker client is not started yet; the node's "+
					"own start creates these")
			return
		}
		if err != nil {
			if ctx.Err() != nil {
				// The pass ran out of ceiling, or the node is
				// stopping. Neither says anything about this seat,
				// so it must not start the clock on an alarm about
				// one.
				n.reportInterrupted(ctx)
				return
			}
			n.alarmMissingMailbox(s, err)
			continue
		}
		n.mail.ensuredNow(s.ID)
		if made {
			created++
		}
	}
	if created == 0 {
		return
	}
	// "provisioned" RATHER THAN "created", because on a fleet booting
	// together this counts what THIS node found absent and then made —
	// and two members that create one mailbox in the same instant are
	// both handed it, with no way to tell which one's create did it. See
	// [queue.EventQueue.EnsureSubscription]. Reading these lines across a
	// fleet, the counts can sum to more than the company has seats.
	//
	// ONLY WHEN SOMETHING WAS MADE. This line used to close every walk,
	// which was once per boot and once per apply; it now runs on the seat
	// sweep's cadence, where a pass that changed nothing is the normal
	// case and an unconditional INFO would be the loudest thing in the log.
	n.log.Info("seat_mailboxes_ready", "seats", len(seats), "provisioned", created)
}

// addressableSeats is the company's seats that can hold a mailbox at all, in
// the order the org lists them.
//
// A seat that derives no inbox subject is skipped rather than refused: the
// seat list is whatever the current revision says, and a pass that failed on
// one unroutable entry would deny every seat after it a mailbox.
func (n *Node) addressableSeats() []placement.Seat {
	seats := n.cfg.Seats()
	out := make([]placement.Seat, 0, len(seats))
	for _, s := range seats {
		if topics.AgentInbox(s.ID) == "" || topics.AgentInboxGroup(s.ID) == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// listMailboxes asks the broker which seats it holds an inbox mailbox for,
// reporting false when it could not say.
//
// THE PAIR IS WHAT IDENTIFIES ONE, never the subject: a consumer on a seat's
// inbox subject under some other group belongs to somebody else, and counting
// it as the seat's mailbox would leave that seat with none. [topics.MailboxSeat]
// is the inverse of the grammar that built the name, and the control
// subscription it also recognises is deliberately not counted here — this pass
// creates inboxes, and a seat whose control subscription exists is not a seat
// whose inbox does.
func (n *Node) listMailboxes(ctx context.Context) (map[uuid.UUID]struct{}, bool) {
	subs, err := n.cfg.Queue.ListSubscriptions(ctx, topics.AgentInboxPrefix+">")
	if err != nil {
		if !errors.Is(err, queue.ErrNotLive) && ctx.Err() == nil {
			n.log.Warn("seat_mailboxes_unlisted", "error", err,
				"detail", "the broker did not say which mailboxes it holds, so this pass "+
					"falls back to what this node already knew and ensures only what that "+
					"leaves missing; a mailbox deleted since is not noticed until a listing "+
					"answers again")
		}
		return nil, false
	}
	out := make(map[uuid.UUID]struct{}, len(subs))
	for _, sub := range subs {
		id, ok := topics.MailboxSeat(sub.Topic, sub.Group)
		if !ok || sub.Group != topics.AgentInboxGroup(id) {
			continue
		}
		out[id] = struct{}{}
	}
	return out, true
}

// reportInterrupted says a pass stopped early, and tells the two reasons
// apart because one of them is routine and the other is not.
//
// A CANCEL is this node stopping, which happens on every clean shutdown and
// is worth a debug line at most. A DEADLINE is the pass's own ceiling running
// out against a broker that is not answering, which leaves seats without
// mailboxes and is worth saying.
func (n *Node) reportInterrupted(ctx context.Context) {
	detail := "the pass stopped before every seat's mailbox was created; " +
		"the next tick, apply or start runs it again"
	if errors.Is(ctx.Err(), context.Canceled) {
		n.log.Debug("seat_mailboxes_interrupted", "error", ctx.Err(), "detail", detail)
		return
	}
	n.log.Warn("seat_mailboxes_interrupted", "error", ctx.Err(), "detail", detail)
}

// alarmMissingMailbox reports a seat this node could not give a mailbox.
//
// ONE LEASE TTL, and the threshold is not this alarm's own — that is ADR-0015.
// A mailbox that cannot be created on one tick is retried on the next, nine
// times inside a shipped lease, and an alarm on the first failure would fire
// twelve times a minute for a condition that clears itself. What is worth an
// operator's attention is a seat that has been without a mailbox for as long
// as the fleet already tolerates a seat being unserved — the same number that
// decides when its lease lapses and its work moves — because past that point
// its mail is being dropped and nothing else in the engine will say so.
//
// READ FROM THE HOST rather than copied from [seat.SeatLeaseTTL], which is the
// ADR's second clause and a real property here: a deployment that shortens its
// lease shortens this with it, in the same setting, with nobody remembering to.
//
// Re-raised on that same interval rather than on every tick, for the reason
// [seat.UndeadAlarmInterval] gives about the other seat-level alarm: the
// failure itself is not news, *still failing* is, and one stuck seat must not
// be able to fill a log with its own retries.
func (n *Node) alarmMissingMailbox(seat placement.Seat, cause error) {
	handle := seat.Handle
	now := time.Now()
	ttl := n.host.TTL()
	outstanding, alarm := n.mail.missing(seat.ID, ttl, now)
	if !alarm {
		// Logged all the same, because a cause that never reaches the log
		// is a cause nobody has when the alarm does fire.
		n.log.Debug("seat_mailbox_ensure_failed", "handle", handle, "error", cause,
			"outstanding", outstanding)
		return
	}
	n.log.Warn("seat_mailbox_unavailable", "handle", handle, "error", cause,
		"outstanding", outstanding, "grace", ttl,
		"detail", "this seat has had no mailbox for longer than a seat lease, so every "+
			"event published to it — every webhook, notification and scheduled trigger — "+
			"is being dropped rather than retained. The next tick retries it")
}

// --- the set ---------------------------------------------------------------

// diff reports the seats with no known mailbox, and whether the set is old
// enough that the broker has to be asked again.
//
// It PRUNES to the company's current seats as it goes, which is both why the
// maps stay bounded in a process that reconfigures often and why a seat that
// left and came back is ensured again rather than assumed.
func (m *mailboxes) diff(seats []placement.Seat, ttl time.Duration, now time.Time) (
	missing []placement.Seat, stale bool) {

	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(seats)
	for _, s := range seats {
		if _, ok := m.ensured[s.ID]; !ok {
			missing = append(missing, s)
		}
	}
	return missing, now.Sub(m.seededAt) > ttl
}

// adopt replaces the set with what the broker answered, and reports what the
// company's seats are still missing.
func (m *mailboxes) adopt(seats []placement.Seat, listed map[uuid.UUID]struct{},
	now time.Time) []placement.Seat {

	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensured = map[uuid.UUID]struct{}{}
	var missing []placement.Seat
	for _, s := range seats {
		if _, ok := listed[s.ID]; ok {
			m.ensured[s.ID] = struct{}{}
			delete(m.missingSince, s.ID)
			delete(m.alarmedAt, s.ID)
			continue
		}
		missing = append(missing, s)
	}
	m.seededAt = now
	return missing
}

// ensuredNow records a mailbox this node has just been told exists.
func (m *mailboxes) ensuredNow(id uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensured[id] = struct{}{}
	delete(m.missingSince, id)
	delete(m.alarmedAt, id)
}

// missing records a failed ensure and reports how long this seat has been
// without a mailbox, and whether that is now worth saying out loud.
func (m *mailboxes) missing(id uuid.UUID, ttl time.Duration, now time.Time) (time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	since, known := m.missingSince[id]
	if !known {
		since = now
		m.missingSince[id] = since
	}
	outstanding := now.Sub(since)
	if outstanding <= ttl {
		return outstanding, false
	}
	if last, alarmed := m.alarmedAt[id]; alarmed && now.Sub(last) < ttl {
		return outstanding, false
	}
	m.alarmedAt[id] = now
	return outstanding, true
}

func (m *mailboxes) pruneLocked(seats []placement.Seat) {
	keep := make(map[uuid.UUID]struct{}, len(seats))
	for _, s := range seats {
		keep[s.ID] = struct{}{}
	}
	for id := range m.ensured {
		if _, ok := keep[id]; !ok {
			delete(m.ensured, id)
		}
	}
	for id := range m.missingSince {
		if _, ok := keep[id]; !ok {
			delete(m.missingSince, id)
			delete(m.alarmedAt, id)
		}
	}
}

// --- the loop --------------------------------------------------------------

// converge runs [Node.EnsureMailboxes] on the seat sweep's own cadence.
//
// ON THE SWEEP'S CADENCE AND NOT ON ITS TICK, because the seat host has no
// hook for one and a mailbox is not the host's business: the host decides
// which seats THIS node runs, and a mailbox belongs to the company whoever
// runs the seat. Sharing the interval is what makes the two converge together
// — a seat that becomes claimable and a seat that becomes reachable are the
// same event from an operator's side.
//
// A pass that has nothing to do sends nothing to the broker, which is why this
// can run at the sweep's rate at all; see [Node.EnsureMailboxes].
func (n *Node) converge(ctx context.Context) {
	ticker := time.NewTicker(n.convergeEvery())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		n.EnsureMailboxes(ctx)
	}
}

// convergeEvery is this node's sweep interval, defaulted the way the seat host
// defaults its own so the two cannot drift apart when a deployment shortens it.
func (n *Node) convergeEvery() time.Duration {
	if n.cfg.SweepInterval > 0 {
		return n.cfg.SweepInterval
	}
	return seat.SweepInterval
}
