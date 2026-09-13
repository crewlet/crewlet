package maintenance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// THE REMOVED SEAT'S MAILBOX.
//
// A seat's mailbox is a durable subscription, and a durable subscription on
// the agent stream retains every message addressed to it until a consumer
// acks it. That is what makes an unowned seat's mail safe, and it is also why
// a seat REMOVED from the company is a leak: nothing consumes its mailbox
// again, nothing deletes it, and every event still addressed to the handle is
// kept for the life of the deployment. Worse, a seat later added under the
// same handle attaches to that mailbox and works the backlog under a role
// definition that never wrote it.
//
// So a mailbox is retired once its seat has been absent from the active
// revision for [MailboxRetirementGrace]. The pieces, and why each is shaped
// the way it is:
//
//   - THE REGISTRY. The queue contract cannot list subscriptions and a removed
//     handle is gone from the org every node derives mailbox names from, so
//     every node records a handle in the coordination store BEFORE it creates
//     the subscription ([Mailboxes.Register]). That record is the only place a
//     removed seat's mailbox is remembered at all.
//   - ABSENCE IS OBSERVED, NOT INFERRED. The sweep stamps a record absent the
//     first time the active revision lacks its handle, and retires it only
//     once that stamp is older than the grace period. The stamp is cleared by
//     whichever writer sees the seat again first: the sweep, or a node
//     registering the seat on an apply.
//   - UNKNOWN IS NEVER ABSENT. The sweep reads the roster of the revision the
//     fleet is pointed at, and a roster it cannot read, or a node that has not
//     applied that revision yet, stamps nothing and retires nothing. A store
//     blip or a lagging duty holder must not be able to delete a live seat's
//     mail.
//   - EVERY WRITE IS A COMPARE-AND-SET. Two sweeps that overlap during a duty
//     handoff read the same record and exactly one wins the mark that starts a
//     retirement; a returning seat's registration that lands first makes the
//     sweep's mark lose instead.
//   - A RETIREMENT IS MARKED BEFORE IT DELETES ANYTHING. The mark is what makes
//     a sweep that dies mid-way recoverable (a later sweep resumes it once the
//     mark is stale) and what tells a registering node not to create the
//     subscription a retirement is still deleting (it waits instead, and takes
//     the record over if the retirement outlives its own budget).
//   - A RETIREMENT HOLDS THE SEAT'S LEASE WHILE IT DELETES. The mark stops a
//     registering node, but a node's seat host claims a seat off the company
//     it installed without ever reading the record, and attaching the seat's
//     consumer creates the very subscriptions the retirement is about to
//     delete. Only the lease excludes that node, so the retirement claims it
//     under its own owner before the mark and gives it back after the record
//     is gone. A seat whose lease somebody else holds is not retired at all.
//
// Memory is NOT retired. A seat's diary, episodes, counterparty profiles and
// onboarding markers are keyed by its handle or by the agent id derived from
// it, so a seat added again under the same handle reattaches to them. That is a documented
// property of handle identity rather than a leak, and the learning lifecycle
// is what bounds it.

// MailboxRetirementGrace is how long a seat must have been absent from the
// active revision before its mailbox is deleted.
//
// Twenty-four hours, sized from the two things it trades against. Long enough
// that a seat deleted by accident and restored within a working day, from the
// builder's undo, a revert or a corrected import, comes back to its backlog
// intact rather than to an empty mailbox. Short enough that mail addressed to a
// seat nobody runs is not retained for longer than one day plus a tick.
//
// The clock starts when a sweep FIRST OBSERVES the absence, so the effective
// delay is this plus up to one [Interval], and longer while no duty holder can
// read the active revision. A retirement is therefore never early, only late,
// which is the safe direction for a delete that cannot be undone.
const MailboxRetirementGrace = 24 * time.Hour

// mailboxRetireBudget bounds the broker and store work of one retirement, from
// before its mark is written to the delete of its record.
//
// Thirty seconds: the work is a seat lease claim, two subscription deletes and
// two conditional store writes, each a millisecond on a healthy broker and each
// individually bounded by the client's five-second API timeout, so this covers
// every one of them timing out with room to spare. It is load-bearing for the
// registering side as well: a node that finds a retirement in flight waits
// twice this long before it takes the record over, so by then the retiring
// sweep can no longer issue a delete that lands on the subscription the node is
// about to create.
//
// A retirement is also held to half the seat lease TTL when that is shorter,
// because its deletes are only excluded from a claiming node while the lease
// it took is live; see [Mailboxes.workLimit].
const mailboxRetireBudget = 30 * time.Second

// mailboxRetireStale is how old a retirement mark must be before a later sweep
// treats it as abandoned and resumes it.
//
// One [Interval], orders of magnitude past [mailboxRetireBudget], so a sweep
// that is merely slow has long finished or given up before a peer resumes its
// work, and far past any clock skew between the node that stamped the mark and
// the node reading it. The cost of the margin is one tick of delay on a
// retirement whose sweep died, which is nothing against a grace of a day.
const mailboxRetireStale = Interval

// mailboxRegisterPoll is how often a registering node re-reads a record whose
// retirement is in flight.
//
// A quarter second: a retirement finishes in milliseconds, so the common wait
// costs the apply that triggered it one poll, while polling faster would put a
// read per few milliseconds on the coordination store for the whole of an
// abandoned retirement's budget.
const mailboxRegisterPoll = 250 * time.Millisecond

// mailboxCASRetries bounds consecutive lost races on one record.
//
// Every loss means a different writer landed in between, and the writers of one
// seat's record are the nodes applying a revision and one sweep. Sixteen
// consecutive losses, the same bound the KV backend's own counters use, is
// contention nothing legitimate produces, so it is reported as an error rather
// than retried for ever.
const mailboxCASRetries = 16

// mailboxesJobName is what the sweep log and [Worker.Jobs] call the job.
const mailboxesJobName = "seat_mailboxes"

// ErrNoActiveRevision reports that the fleet has never activated a revision.
//
// Its own sentinel because it is the one roster failure that is not a fault: an
// unconfigured deployment has no seats to judge, and a sweep that logged a
// warning for it every tick would be noise on a healthy node. Every other roster
// error is reported, because it is the reason nothing was retired.
var ErrNoActiveRevision = errors.New("maintenance: no company revision has been activated")

// MailboxRecords is the slice of the coordination store's mailbox registry this
// package calls. Declared here, by the consumer, like every other seam in this
// tree; [coord.Mailboxes] satisfies it.
type MailboxRecords interface {
	Mailbox(ctx context.Context, handle string) (coord.MailboxRecord, bool, error)
	Mailboxes(ctx context.Context) ([]coord.MailboxRecord, error)
	CreateMailbox(ctx context.Context, rec coord.MailboxRecord) (coord.MailboxRecord, bool, error)
	UpdateMailbox(ctx context.Context, rec coord.MailboxRecord) (coord.MailboxRecord, bool, error)
	DeleteMailbox(ctx context.Context, handle string, version uint64) (bool, error)
}

// MailboxQueue is the slice of the event queue a retirement calls.
type MailboxQueue interface {
	EnsureSubscription(ctx context.Context, topic, group string) (bool, error)
	DeleteSubscription(ctx context.Context, topic, group string) (bool, error)
}

// SeatLeases is the slice of the lease store a retirement claims a seat through.
type SeatLeases interface {
	TryAcquire(ctx context.Context, resource string, opts coord.AcquireOptions) (*coord.Lease, error)
	Release(ctx context.Context, resource, owner string, epoch int64) (bool, error)
}

// SeatRoster reads the agent seat handles of the revision the fleet is pointed
// at.
//
// An error means UNKNOWN, never "no seats": it is returned when the pointer
// cannot be read, when this node has not applied the revision it names, and
// [ErrNoActiveRevision] when nothing has ever been activated.
type SeatRoster func(ctx context.Context) ([]string, error)

// MailboxOptions configures [NewMailboxes].
type MailboxOptions struct {
	// Records is the fleet's mailbox registry. Required.
	Records MailboxRecords

	// Queue deletes a retired seat's subscriptions and restores an inbox a
	// lost race may have deleted. Required.
	Queue MailboxQueue

	// Leases is the seat lease store a retirement claims a seat in before it
	// deletes the seat's subscriptions, so no node can claim the seat and
	// attach to them meanwhile. Required.
	Leases SeatLeases

	// Owner is the lease owner a retirement claims a seat under. Required.
	//
	// It must be unique to this process AND differ from the owner the node's
	// own seat host claims under: a claim by an owner that already holds a
	// lease doubles as a renew, so sharing the seat host's owner would let a
	// retirement "win" the lease of a seat this very node is running.
	Owner string

	// LeaseTTL is the seat lease TTL the lease store was opened with.
	// Required, because it has no default that could be right: a claim
	// longer than the store's TTL is refused, and a retirement's work is
	// only excluded from a claiming node while its claim is live, so the
	// work is bounded by half of it (see [Mailboxes.workLimit]).
	LeaseTTL time.Duration

	// Roster reads the active revision's agent seats. Required.
	Roster SeatRoster

	// RetireBudget overrides [mailboxRetireBudget], and RegisterPoll
	// overrides [mailboxRegisterPoll]. Zero takes the shipped value, which
	// is what production passes; a test shrinks them so an abandoned
	// retirement is taken over in milliseconds rather than a minute.
	RetireBudget time.Duration
	RegisterPoll time.Duration
}

// Mailboxes registers seat mailboxes and retires the ones whose seat has left.
//
// One value serves both halves, because they are one protocol over one record:
// the node's registration and the sweep's retirement must agree about what
// every state of a record means, and two types would be two places to write
// that down.
type Mailboxes struct {
	records  MailboxRecords
	queue    MailboxQueue
	leases   SeatLeases
	owner    string
	leaseTTL time.Duration
	roster   SeatRoster
	budget   time.Duration
	poll     time.Duration
}

// NewMailboxes builds the registry and its sweep, refusing a missing
// dependency by name.
func NewMailboxes(opts MailboxOptions) (*Mailboxes, error) {
	var missing []error
	if opts.Records == nil {
		missing = append(missing, errors.New("maintenance: MailboxOptions.Records is required"))
	}
	if opts.Queue == nil {
		missing = append(missing, errors.New("maintenance: MailboxOptions.Queue is required"))
	}
	if opts.Leases == nil {
		missing = append(missing, errors.New("maintenance: MailboxOptions.Leases is required"))
	}
	if opts.Owner == "" {
		missing = append(missing, errors.New("maintenance: MailboxOptions.Owner is required; "+
			"pass an incarnation distinct from the seat host's own"))
	}
	if opts.LeaseTTL <= 0 {
		missing = append(missing, errors.New("maintenance: MailboxOptions.LeaseTTL is required; "+
			"pass the TTL the seat lease store was opened with"))
	}
	if opts.Roster == nil {
		missing = append(missing, errors.New("maintenance: MailboxOptions.Roster is required"))
	}
	if err := errors.Join(missing...); err != nil {
		return nil, err
	}
	m := &Mailboxes{
		records: opts.Records, queue: opts.Queue, leases: opts.Leases,
		owner: opts.Owner, leaseTTL: opts.LeaseTTL, roster: opts.Roster,
		budget: opts.RetireBudget, poll: opts.RegisterPoll,
	}
	if m.budget <= 0 {
		m.budget = mailboxRetireBudget
	}
	if m.poll <= 0 {
		m.poll = mailboxRegisterPoll
	}
	return m, nil
}

// Jobs is the sweep, for [Options.Jobs]. Its horizon is the grace period, so
// the cutoff a tick hands it is the instant before which an absence has lasted
// long enough.
func (m *Mailboxes) Jobs() []Job {
	return []Job{{Name: mailboxesJobName, Horizon: MailboxRetirementGrace, Run: m.sweep}}
}

// Register records that a seat's mailbox exists or is about to, and returns
// once the caller may create it.
//
// Called by every node for every agent seat of the revision it applied, BEFORE
// the subscription is created, so a crash between the two leaves a record with
// no subscription (harmless, a retirement deletes nothing) rather than a
// subscription nothing remembers.
//
// A record the sweep stamped absent is cleared: the seat is back. A record whose
// retirement is in flight is NOT overwritten at once, because the retiring
// sweep may still delete the subscription this node is about to create; the
// node waits for the retirement to finish, and takes the record over only once
// it has outlived twice its budget, by which point the retiring sweep can no
// longer act. The wait is measured on this process's monotonic clock from the
// moment it first saw the mark, so no two nodes' wall clocks are compared.
//
// An error means the record could not be written. The caller creates the
// mailbox anyway: losing mail for a seat in the company is worse than a
// mailbox the sweep has to register on its next tick.
func (m *Mailboxes) Register(ctx context.Context, handle string) error {
	if handle == "" {
		return errors.New("maintenance: a mailbox registration needs a seat handle")
	}
	var retiringSeen time.Time
	for lost := 0; lost < mailboxCASRetries; {
		rec, found, err := m.records.Mailbox(ctx, handle)
		if err != nil {
			return fmt.Errorf("maintenance: read the mailbox record of seat %q: %w", handle, err)
		}
		if !found {
			_, created, createErr := m.records.CreateMailbox(ctx, coord.MailboxRecord{Handle: handle})
			if createErr != nil {
				return fmt.Errorf("maintenance: register the mailbox of seat %q: %w", handle, createErr)
			}
			if created {
				return nil
			}
			lost++
			continue
		}
		if rec.Present() {
			return nil
		}
		if rec.Retiring() {
			if retiringSeen.IsZero() {
				retiringSeen = time.Now()
			}
			if time.Since(retiringSeen) < 2*m.budget {
				if waitErr := sleep(ctx, m.poll); waitErr != nil {
					return fmt.Errorf("maintenance: waiting for the retirement of seat %q's "+
						"previous mailbox: %w", handle, waitErr)
				}
				continue
			}
			log.WarnContext(ctx, "seat_mailbox_retirement_taken_over", "handle", handle,
				"retiring_since", rec.RetiringSince,
				"detail", "a retirement of this seat's previous mailbox did not finish within its "+
					"budget; the seat is in the active revision again, so its record is reclaimed "+
					"and the mailbox created afresh")
		}
		cleared, ok, err := m.records.UpdateMailbox(ctx, presentRecord(rec))
		if err != nil {
			return fmt.Errorf("maintenance: register the mailbox of seat %q: %w", handle, err)
		}
		if ok {
			log.InfoContext(ctx, "seat_mailbox_returned", "handle", cleared.Handle,
				"absent_since", rec.AbsentSince, "by", "registration")
			return nil
		}
		lost++
	}
	return fmt.Errorf("maintenance: the mailbox record of seat %q changed under %d consecutive "+
		"writes; the mailbox is created unregistered and the next sweep registers it",
		handle, mailboxCASRetries)
}

// sweep is the job: register what the roster has, stamp what it lacks, and
// retire what has been absent past the cutoff.
func (m *Mailboxes) sweep(ctx context.Context, now, cutoff time.Time) (int64, error) {
	roster, err := m.roster(ctx)
	if errors.Is(err, ErrNoActiveRevision) {
		log.DebugContext(ctx, "seat_mailboxes_not_judged", "reason", err.Error())
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("the active revision's seats could not be read, so no mailbox is "+
			"judged absent this tick: %w", err)
	}
	present := make(map[string]bool, len(roster))
	for _, handle := range roster {
		present[handle] = true
	}
	records, err := m.records.Mailboxes(ctx)
	if err != nil {
		return 0, fmt.Errorf("the mailbox registry could not be read: %w", err)
	}

	var retired int64
	var errs []error
	registered := make(map[string]bool, len(records))
	for _, rec := range records {
		registered[rec.Handle] = true
		if present[rec.Handle] {
			if err := m.keep(ctx, rec, now); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		done, err := m.judge(ctx, rec, now, cutoff)
		if err != nil {
			errs = append(errs, err)
		}
		if done {
			retired++
		}
	}
	// A SEAT IN THE ROSTER WITH NO RECORD is a mailbox a node created before
	// it could register it: a coordination store that refused the write, or
	// a build that predates the registry. Registered here, so that if the
	// seat is ever removed its mailbox is remembered.
	for _, handle := range roster {
		if registered[handle] {
			continue
		}
		if _, _, err := m.records.CreateMailbox(ctx, coord.MailboxRecord{Handle: handle}); err != nil {
			errs = append(errs, fmt.Errorf("register the mailbox of seat %q: %w", handle, err))
		}
	}
	return retired, errors.Join(errs...)
}

// keep handles a record whose seat is in the active revision.
func (m *Mailboxes) keep(ctx context.Context, rec coord.MailboxRecord, now time.Time) error {
	switch {
	case rec.Present():
		return nil
	case rec.Retiring() && now.Sub(rec.RetiringSince) < mailboxRetireStale:
		// A PEER'S RETIREMENT IN FLIGHT, started from a roster older than
		// this one. Left to it: that sweep re-reads the roster when it
		// finishes and restores the mailbox of a seat that came back, and
		// interfering would race its deletes.
		return nil
	}
	cleared, ok, err := m.records.UpdateMailbox(ctx, presentRecord(rec))
	if err != nil {
		return fmt.Errorf("clear the absence of seat %q: %w", rec.Handle, err)
	}
	if !ok {
		// Somebody else wrote it first, a registering node or a peer
		// sweep, and either wrote what this would have.
		return nil
	}
	log.InfoContext(ctx, "seat_mailbox_returned", "handle", cleared.Handle,
		"absent_since", rec.AbsentSince, "by", "sweep")
	if rec.Retiring() {
		// AN ABANDONED RETIREMENT of a seat that is back. It may have
		// deleted the inbox before it died, and the seat's mail is dropped
		// until something creates it again.
		return m.restoreInbox(ctx, rec.Handle)
	}
	return nil
}

// judge handles a record whose seat is not in the active revision, reporting
// whether this call retired its mailbox.
func (m *Mailboxes) judge(ctx context.Context, rec coord.MailboxRecord, now, cutoff time.Time) (bool, error) {
	switch {
	case rec.Retiring():
		if now.Sub(rec.RetiringSince) < mailboxRetireStale {
			// A peer sweep's retirement in flight. Two sweeps overlapping
			// during a duty handoff must retire a mailbox once.
			return false, nil
		}
		log.WarnContext(ctx, "seat_mailbox_retirement_resumed", "handle", rec.Handle,
			"retiring_since", rec.RetiringSince,
			"detail", "a previous sweep marked this mailbox for retirement and did not finish")
		return m.retire(ctx, rec, now)
	case rec.AbsentSince.IsZero():
		stamped := rec
		stamped.AbsentSince = now
		if _, ok, err := m.records.UpdateMailbox(ctx, stamped); err != nil {
			return false, fmt.Errorf("record the absence of seat %q: %w", rec.Handle, err)
		} else if ok {
			log.InfoContext(ctx, "seat_mailbox_absent", "handle", rec.Handle,
				"retire_after", now.Add(MailboxRetirementGrace),
				"detail", "the seat is not in the active revision; its mailbox and the mail it "+
					"holds are kept until the grace period ends, and a seat restored under the "+
					"same handle before then finds both")
		}
		return false, nil
	case rec.AbsentSince.After(cutoff):
		return false, nil
	default:
		return m.retire(ctx, rec, now)
	}
}

// retire deletes a removed seat's subscriptions and then its record.
func (m *Mailboxes) retire(ctx context.Context, rec coord.MailboxRecord, now time.Time) (bool, error) {
	handle := rec.Handle

	// THE BUDGET STARTS BEFORE THE CLAIM AND THE MARK, so a registering node
	// that waits twice the budget from the moment it first sees the mark has
	// waited past the last instant this retirement could issue a delete, and
	// the claim below outlives every delete by at least half its TTL.
	work, cancel := context.WithTimeout(ctx, m.workLimit())
	defer cancel()

	// NOTHING ELSE MAY HOLD THE SEAT, AND NOTHING MAY TAKE IT WHILE THIS
	// DELETES. The seat host releases a seat whose role is gone, so a lease
	// somebody else holds is a node still serving a revision that has the
	// seat, and deleting a mailbox its consumer is attached to would pull the
	// subscription out from under that node's turns. Reading the lease is not
	// enough: a node that installs a revision adding the seat back claims it
	// off that company without reading the record, and attaching creates the
	// subscriptions this is about to delete. So the retirement CLAIMS the
	// lease, which is the one thing that node's claim loses to. A claim that
	// cannot be answered is unknown, and unknown retires nothing.
	lease, err := m.leases.TryAcquire(work, coord.SeatResource(handle), coord.AcquireOptions{
		Owner: m.owner,
		TTL:   m.leaseTTL,
		// No Preferred: the hint records the last node that RAN the seat,
		// and a claim that exists only to exclude one must leave it alone.
	})
	if err != nil {
		return false, fmt.Errorf("claim the lease of seat %q before retiring its mailbox: %w", handle, err)
	}
	if lease == nil {
		log.WarnContext(ctx, "seat_mailbox_retirement_held", "handle", handle,
			"absent_since", rec.AbsentSince,
			"detail", "the seat is absent from the active revision but its lease could not be "+
				"claimed: a node still holds it, or a node of an older build holds a lease in "+
				"this fleet; the mailbox is kept and the claim retried on the next tick")
		return false, nil
	}
	defer m.releaseSeat(ctx, *lease)

	mark := rec
	mark.RetiringSince = now
	marked, ok, err := m.records.UpdateMailbox(work, mark)
	if err != nil {
		return false, fmt.Errorf("mark the mailbox of seat %q for retirement: %w", handle, err)
	}
	if !ok {
		// A registering node or a peer sweep wrote first. Either the seat
		// is back or somebody else is retiring it; neither is this call's.
		return false, nil
	}

	if deleteErr := m.deleteSubscriptions(work, handle); deleteErr != nil {
		m.unmark(ctx, marked)
		return false, deleteErr
	}
	gone, err := m.records.DeleteMailbox(work, handle, marked.Version)
	if err != nil {
		// Left marked. A later sweep resumes it once the mark is stale, and
		// a seat that returns first takes the record over.
		return false, fmt.Errorf("delete the mailbox record of retired seat %q: %w", handle, err)
	}
	if !gone {
		return false, m.afterLostDelete(ctx, handle)
	}
	log.InfoContext(ctx, "seat_mailbox_retired", "handle", handle, "absent_since", rec.AbsentSince,
		"detail", "the seat has been absent from the active revision for longer than the grace "+
			"period; its mailbox and the mail it held are deleted, and its memory is kept")

	// A SEAT ADDED AGAIN WHILE THIS RAN. The roster this sweep started from
	// may be older than the revision now active, and a node that applied it
	// may have created the inbox between this sweep's mark and its delete
	// without being able to register it. Re-read, and restore what the seat
	// needs; a roster that cannot be read leaves it to that node's next apply.
	if roster, err := m.roster(ctx); err == nil && slices.Contains(roster, handle) {
		if err := m.restoreInbox(ctx, handle); err != nil {
			return true, err
		}
		if _, _, err := m.records.CreateMailbox(ctx, coord.MailboxRecord{Handle: handle}); err != nil {
			return true, fmt.Errorf("register the restored mailbox of seat %q: %w", handle, err)
		}
	}
	return true, nil
}

// workLimit is how long one retirement may act, from before its lease claim to
// the delete of its record.
//
// [mailboxRetireBudget], or half the seat lease TTL when that is shorter. The
// lease is what keeps a claiming node off the seat while the subscriptions are
// deleted, so no delete may be issued after it could have lapsed. Its expiry is
// the store's clock from the claim and this deadline is the local clock from
// before it; the other half of the TTL is the margin between the two, and
// covers a request sent just before the deadline landing just after it. With
// the shipped 45-second lease TTL that is 22.5 seconds, still above the four
// five-second client timeouts a retirement's writes can each run into.
func (m *Mailboxes) workLimit() time.Duration {
	return min(m.budget, m.leaseTTL/2)
}

// releaseSeat gives back the lease a retirement claimed.
//
// On a context of its own, as a teardown: the retirement may have ended on its
// deadline, and a release that inherited it would leave a seat added back
// unclaimable for a full TTL. A release that fails leaves exactly that, which
// is logged rather than returned because the retirement itself is settled.
func (m *Mailboxes) releaseSeat(ctx context.Context, lease coord.Lease) {
	teardown, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.budget)
	defer cancel()
	if _, err := m.leases.Release(teardown, lease.Resource, lease.Owner, lease.Epoch); err != nil {
		log.WarnContext(ctx, "seat_mailbox_retirement_release_failed", "resource", lease.Resource,
			"error", err.Error(),
			"detail", "the seat's lease lapses on its own TTL; a node can claim the seat after that")
	}
}

// deleteSubscriptions deletes every durable subscription a seat's mailbox
// comprises: the inbox every node creates, and the sandbox control topic the
// seat's owner subscribes. Both are attempted even when one fails, so a
// retirement that is retried has less left to do.
func (m *Mailboxes) deleteSubscriptions(ctx context.Context, handle string) error {
	var errs []error
	for _, sub := range [][2]string{
		{topics.AgentInbox(handle), topics.AgentInboxGroup(handle)},
		{topics.AgentControl(handle), topics.AgentControlGroup(handle)},
	} {
		if _, err := m.queue.DeleteSubscription(ctx, sub[0], sub[1]); err != nil {
			errs = append(errs, fmt.Errorf("delete subscription %s/%s of retired seat %q: %w",
				sub[0], sub[1], handle, err))
		}
	}
	return errors.Join(errs...)
}

// afterLostDelete handles a retirement whose record changed between its mark
// and its delete.
//
// The only writers that can do that are a registering node that took an
// abandoned-looking retirement over, and a peer sweep that resumed one. The
// first may have created the inbox before this retirement's delete landed, so
// the inbox is restored; the second is finishing the same work, so nothing is.
func (m *Mailboxes) afterLostDelete(ctx context.Context, handle string) error {
	current, found, err := m.records.Mailbox(ctx, handle)
	if err != nil {
		return fmt.Errorf("re-read the mailbox record of seat %q after a lost retirement: %w", handle, err)
	}
	if !found || current.Retiring() {
		return nil
	}
	log.WarnContext(ctx, "seat_mailbox_retirement_raced", "handle", handle,
		"detail", "the seat was registered again while its previous mailbox was being retired; "+
			"its inbox is restored in case the retirement deleted it after it was created")
	return m.restoreInbox(ctx, handle)
}

// unmark returns a retirement that could not delete its subscriptions to an
// ordinary absent record, so the next tick retries it from the start and a
// returning seat is not made to wait out a mark nothing is acting on.
//
// On a context of its own: the failure being undone is often the budget's
// deadline, and a rollback that inherited it would do nothing.
func (m *Mailboxes) unmark(ctx context.Context, marked coord.MailboxRecord) {
	rollback, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.budget)
	defer cancel()
	absent := marked
	absent.RetiringSince = time.Time{}
	if _, _, err := m.records.UpdateMailbox(rollback, absent); err != nil {
		log.WarnContext(ctx, "seat_mailbox_unmark_failed", "handle", marked.Handle, "error", err.Error(),
			"detail", "the retirement mark stays until a later sweep resumes it")
	}
}

// restoreInbox creates a seat's inbox, the half of its mailbox every node
// creates and a retirement may have deleted under it.
func (m *Mailboxes) restoreInbox(ctx context.Context, handle string) error {
	made, err := m.queue.EnsureSubscription(ctx, topics.AgentInbox(handle), topics.AgentInboxGroup(handle))
	if errors.Is(err, queue.ErrNotLive) {
		// A node that is shutting down. Its peers' next apply or this
		// duty's next holder creates it; nothing here can.
		return nil
	}
	if err != nil {
		return fmt.Errorf("restore the inbox of seat %q: %w", handle, err)
	}
	if made {
		log.WarnContext(ctx, "seat_mailbox_restored", "handle", handle,
			"detail", "the inbox of a seat in the active revision was missing and has been created")
	}
	return nil
}

// presentRecord is rec with its absence and any retirement cleared, at the
// version it was read at.
func presentRecord(rec coord.MailboxRecord) coord.MailboxRecord {
	rec.AbsentSince, rec.RetiringSince = time.Time{}, time.Time{}
	return rec
}

// sleep waits d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
