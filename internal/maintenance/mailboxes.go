package maintenance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/words"
)

// THE REMOVED SEAT'S MAILBOX. A durable subscription keeps every message until
// acked, so a seat leaving the agent seats leaks its mailbox unless retired
// after [MailboxRetirementGrace] of absence (the id derives from the handle,
// ADR-0013). A registry record per seat, written BEFORE its subscription,
// carries the absence stamp, retirement mark and CAS; the broker backstops
// mailboxes with no record, found by seat ID (the handle is only a label).
// Absence is observed, never inferred; unknown (unreadable roster, unapplied
// activation) is never absent; every write is a CAS. A retirement marks first
// (a dead sweep resumes), holds the seat's lease (a seat host claims without
// reading the record), ends runs ([SeatRunRetirer]) then sessions
// ([SeatSessionRetirer]) before deleting any subscription, and is undone and
// retried if it cannot. Memory is not retired: its rows are reachable by no seat.

// MailboxRetirementGrace is how long a seat must have been absent from the
// active revision before its mailbox is deleted.
//
// Twenty-four hours: long enough that a seat made a person's by mistake and
// made an agent's again within a working day (the one departure that can be
// undone, since a removal retires the address and identity for good) finds its
// backlog intact; short enough that mail for a seat nobody runs is kept about a
// day. The clock starts when a sweep FIRST OBSERVES the absence, so the delay
// is this plus up to one [Interval], longer while no duty holder can read the
// active revision: never early, which is the safe direction for a delete.
const MailboxRetirementGrace = 24 * time.Hour

// mailboxRetireBudget bounds the work of one retirement, from before its mark
// to the delete of its record. Thirty seconds: a lease claim, two deletes and
// two conditional writes (each bounded by the client's five-second API
// timeout), plus a box kill and record delete per left-behind run and the
// session removal's publish. A retirement that outruns it is undone and resumes
// next tick. A registering node that finds a retirement in flight waits twice
// this long before taking the record over, so the retiring sweep can no longer
// delete a subscription the node is about to create. Capped at half the seat
// lease TTL when shorter, since its deletes are excluded from a claiming node
// only while the lease is live; see [Mailboxes.workLimit].
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

// ErrRevisionPropagating reports that the fleet has activated a revision this
// node has not applied yet, and that its lag is still ORDINARY PROPAGATION —
// no attempt of its own at that revision has failed and it has not been behind
// longer than propagation explains, or it is starting and has not reconciled
// yet — rather than a node that failed the apply or is stuck on it.
//
// The second roster failure that is not a fault, for the first one's reason:
// every activation puts every node here for the seconds it takes to apply it,
// and a sweep that met those seconds and warned would teach whoever reads it
// to ignore the warning a node that genuinely failed the apply raises. The
// sweep judges nothing on it, as on any roster error — the seats are judged at
// the next tick, against a grace of a day — and says so at debug.
var ErrRevisionPropagating = errors.New("maintenance: this node is still applying the revision the fleet is on")

// MailboxRecords is the slice of the coordination store's mailbox registry this
// package calls. Declared here, by the consumer, like every other seam in this
// tree; [coord.Mailboxes] satisfies it.
type MailboxRecords interface {
	Mailbox(ctx context.Context, seat uuid.UUID) (coord.MailboxRecord, bool, error)
	Mailboxes(ctx context.Context) ([]coord.MailboxRecord, error)
	CreateMailbox(ctx context.Context, rec coord.MailboxRecord) (coord.MailboxRecord, bool, error)
	UpdateMailbox(ctx context.Context, rec coord.MailboxRecord) (coord.MailboxRecord, bool, error)
	DeleteMailbox(ctx context.Context, seat uuid.UUID, version uint64) (bool, error)
}

// MailboxQueue is the slice of the event queue a sweep calls: the listing that
// finds a mailbox nothing registered, and the two verbs a retirement acts with.
type MailboxQueue interface {
	EnsureSubscription(ctx context.Context, topic, group string) (bool, error)
	DeleteSubscription(ctx context.Context, topic, group string) (bool, error)
	ListSubscriptions(ctx context.Context, topicPattern string) ([]queue.Subscription, error)
}

// SeatLeases is the slice of the lease store a retirement claims a seat through.
type SeatLeases interface {
	TryAcquire(ctx context.Context, resource string, opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error)
	Release(ctx context.Context, resource, owner string, epoch int64) (bool, error)
}

// SeatRunRetirer ends every detached executor of a seat whose mailbox is
// being retired, while the retirement holds the seat's lease under owner and
// epoch.
//
// THE SEAT'S ID selects the runs; the handle is only a label, absent from a
// record only the broker's listing found, and selecting by it would end nothing
// and let the retirement delete the runs' subscriptions.
//
// An error means some run was not ended (or the node cannot tell): the
// retirement is undone and retried, since deleting the subscriptions a run's
// completion travels on would strand it. A run left because a tool call is
// still running is answered in [SeatRunsWaiting], beside the error.
type SeatRunRetirer func(ctx context.Context, seat uuid.UUID, handle, owner string, epoch int64,
) (SeatRunsWaiting, error)

// SeatRunsWaiting is the runs of a retired seat a [SeatRunRetirer] left for a
// later tick because a tool call each of them made is still running, named by
// those calls. Its zero value is a retirer that left no run waiting.
//
// A WAIT IS NOT A FAILURE. The call is running, the retirer ends its run once
// the call is recorded, or once the call's bound passes or its process stops,
// and nobody has anything to do about it but wait. So the retirement that
// meets one is undone and retried like one whose runs could not be ended, but
// said at info (`seat_mailbox_retirement_waiting`) rather than failing the
// tick, whose warning sent an operator looking for a fault that was a call
// doing its work. A failure the retirer answers beside it still fails the tick.
type SeatRunsWaiting struct {
	// Calls names each call still running, with the turn it is a call of, in
	// the retirer's words. Empty where no run waits.
	Calls []string
	// Until is the latest instant any of them is still waited for, past which
	// the retirer ends their runs whether or not the calls were recorded. Zero
	// where no run waits, and where a call has no bound, which only its update
	// or its process's end stops the wait for.
	Until time.Time
}

// waiting reports whether the retirer left any run waiting.
func (w SeatRunsWaiting) waiting() bool { return len(w.Calls) > 0 }

// SeatSessionRetirer removes every executor session a seat whose mailbox is
// being retired holds, after its runs are ended and while the retirement holds
// the seat's lease. retiring is the instant this attempt marked the retirement,
// which the removal derives its operation from so every node's ledger can vouch
// for it; a retried retirement republishes it, which applies idempotently.
// THE SEAT'S ID selects the seat, for [SeatRunRetirer]'s reason; handle is a
// label, empty for a mailbox only the broker's listing found.
//
// An error means this node cannot tell which sessions are the seat's, and the
// retirement is undone and retried. A removal the log cannot take now is NOT an
// error: those sessions are left to the conversation retention, which bounds
// them, rather than hold a mailbox as long as the log is unhealthy.
type SeatSessionRetirer func(ctx context.Context, seat uuid.UUID, handle string, retiring time.Time) error

// SeatRoster reads the agent seats of the company the fleet is running: the
// revision the activation pointer names.
//
// THE WHOLE SEAT rather than its handle, because everything this sweep names
// about a mailbox — the registry key, the subscriptions, the lease it claims
// to exclude a running node — is built from the seat's ID. The handle rides
// along as the label every log line and every refusal reads.
//
// An error means UNKNOWN, never "no seats": it is returned when the pointer
// cannot be read, when this node has not applied the activation it names —
// [ErrRevisionPropagating] where that is still ordinary propagation — and
// [ErrNoActiveRevision] when nothing has ever been activated.
type SeatRoster func(ctx context.Context) ([]placement.Seat, error)

// MailboxOptions configures [NewMailboxes].
type MailboxOptions struct {
	// Records is the fleet's mailbox registry. Required.
	Records MailboxRecords

	// Queue lists the seat mailboxes the broker holds, deletes a retired
	// seat's subscriptions and restores an inbox a lost race may have
	// deleted. Required.
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

	// Runs ends a retired seat's executors before its subscriptions are
	// deleted. Required, because there is no value that could mean "this
	// node has nothing to end": whether a removed seat left runs behind is
	// a fact about the fleet's run records, not about this node.
	Runs SeatRunRetirer

	// Sessions removes a retired seat's executor sessions after its runs
	// and before its subscriptions. Required, for Runs' reason: whether a
	// removed seat left sessions behind is a fact about the replicated
	// estate, not about this node.
	Sessions SeatSessionRetirer

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
	runs     SeatRunRetirer
	sessions SeatSessionRetirer
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
	if opts.Runs == nil {
		missing = append(missing, errors.New("maintenance: MailboxOptions.Runs is required"))
	}
	if opts.Sessions == nil {
		missing = append(missing, errors.New("maintenance: MailboxOptions.Sessions is required"))
	}
	if err := errors.Join(missing...); err != nil {
		return nil, err
	}
	m := &Mailboxes{
		records: opts.Records, queue: opts.Queue, leases: opts.Leases,
		owner: opts.Owner, leaseTTL: opts.LeaseTTL, roster: opts.Roster,
		runs: opts.Runs, sessions: opts.Sessions,
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
	// [Fleet]: what this deletes is BROKER state — a seat's durable
	// subscriptions and the mail they hold — which the whole company shares
	// one copy of, so a second node running it would be deleting what the
	// first already did. It is the one job here whose rows are not rows.
	return []Job{{Name: mailboxesJobName, Scope: Fleet, Horizon: Fixed(MailboxRetirementGrace), Run: m.sweep}}
}

// Register records that a seat's mailbox exists or is about to, and returns
// once the caller may create it.
//
// Called by every node for every agent seat, BEFORE the subscription is
// created, so a crash between leaves a harmless record, not a subscription
// nothing remembers. A stamped-absent record is cleared. One whose retirement
// is in flight is NOT overwritten at once, since the sweep may still delete the
// subscription this node is about to create: the node waits, taking it over
// after twice the budget on its own monotonic clock (no two nodes' wall clocks
// compared). On error the caller creates the mailbox anyway: lost mail is worse
// than a record the next sweep adds.
func (m *Mailboxes) Register(ctx context.Context, seat placement.Seat) error {
	handle := seat.Handle
	if seat.ID == uuid.Nil {
		return fmt.Errorf("maintenance: the mailbox registration of seat %q names no "+
			"seat id, and a mailbox record is filed under one", handle)
	}
	var retiringSeen time.Time
	for lost := 0; lost < mailboxCASRetries; {
		rec, found, err := m.records.Mailbox(ctx, seat.ID)
		if err != nil {
			return fmt.Errorf("maintenance: read the mailbox record of seat %q: %w", handle, err)
		}
		if !found {
			_, created, createErr := m.records.CreateMailbox(ctx,
				coord.MailboxRecord{Seat: seat.ID, Handle: handle})
			if createErr != nil {
				return fmt.Errorf("maintenance: register the mailbox of seat %q: %w", handle, createErr)
			}
			if created {
				return nil
			}
			lost++
			continue
		}
		if rec.Present() && rec.Handle == handle {
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
		cleared, ok, err := m.records.UpdateMailbox(ctx, presentRecord(rec, seat))
		if err != nil {
			return fmt.Errorf("maintenance: register the mailbox of seat %q: %w", handle, err)
		}
		if ok {
			if !rec.Present() {
				log.InfoContext(ctx, "seat_mailbox_returned", "handle", cleared.Handle,
					"absent_since", rec.AbsentSince, "by", "registration")
			}
			return nil
		}
		lost++
	}
	return fmt.Errorf("maintenance: the mailbox record of seat %q changed under %d consecutive "+
		"writes; the mailbox is created unregistered and the next sweep registers it",
		handle, mailboxCASRetries)
}

// sweepClock is one tick's clock: the instant the worker handed the tick,
// carried forward by how long this process has spent on it since.
//
// A tick is not an instant. It walks every record in turn, and each retirement
// may take up to its work limit, so on a slow broker a sweep that retires many
// seats runs for minutes. A retirement MARK stamped with the tick's starting
// instant would then look older to a peer than it is, and a mark that looks
// stale is one a peer resumes or clears while this sweep is still deleting
// under it: exactly the overlap the staleness threshold exists to rule out. So
// a mark is stamped when it is written, and a peer's mark is judged against
// the time it is read. The tick's own instant stays the base, so a clock a
// caller injects into the worker still decides every comparison.
type sweepClock struct {
	at      time.Time
	started time.Time
}

func newSweepClock(at time.Time) sweepClock {
	return sweepClock{at: at, started: time.Now()}
}

// now is the tick's instant plus the monotonic time elapsed since it began.
func (c sweepClock) now() time.Time { return c.at.Add(time.Since(c.started)) }

// sweep is the job: register what the roster has, stamp what it lacks, and
// retire what has been absent past the cutoff.
func (m *Mailboxes) sweep(ctx context.Context, now, cutoff time.Time) (int64, error) {
	clock := newSweepClock(now)
	roster, err := m.roster(ctx)
	if errors.Is(err, ErrNoActiveRevision) || errors.Is(err, ErrRevisionPropagating) {
		log.DebugContext(ctx, "seat_mailboxes_not_judged", "reason", err.Error())
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("the active revision's seats could not be read, so no mailbox is "+
			"judged absent this tick: %w", err)
	}
	present := make(map[uuid.UUID]placement.Seat, len(roster))
	for _, s := range roster {
		present[s.ID] = s
	}
	records, err := m.records.Mailboxes(ctx)
	if err != nil {
		return 0, fmt.Errorf("the mailbox registry could not be read: %w", err)
	}

	var retired int64
	var errs []error
	registered := make(map[uuid.UUID]bool, len(records))
	for _, rec := range records {
		registered[rec.Seat] = true
		if seat, in := present[rec.Seat]; in {
			if err := m.keep(ctx, rec, seat, clock); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		done, err := m.judge(ctx, rec, clock, cutoff)
		if err != nil {
			errs = append(errs, err)
		}
		if done {
			retired++
		}
	}
	// A SEAT IN THE ROSTER WITH NO RECORD is a mailbox a node created before
	// it could register it: a coordination store that refused the write, or
	// a registration that lost every compare-and-set. Registered here, so
	// that if the seat is ever removed its mailbox is remembered.
	for _, s := range roster {
		if registered[s.ID] {
			continue
		}
		if _, _, err := m.records.CreateMailbox(ctx,
			coord.MailboxRecord{Seat: s.ID, Handle: s.Handle}); err != nil {

			errs = append(errs, fmt.Errorf("register the mailbox of seat %q: %w", s.Handle, err))
		}
	}
	if err := m.discover(ctx, present, registered, clock, cutoff); err != nil {
		errs = append(errs, err)
	}
	return retired, errors.Join(errs...)
}

// discover registers the mailboxes the broker holds for seats that are neither
// in the roster nor in the registry, and stamps each absent.
//
// THE LISTING IS READ AFTER THE REGISTRY, and the order is what keeps it from
// resurrecting a mailbox a peer is retiring. A retirement deletes the
// subscriptions before the record, so a record this sweep did not see was
// either never written or already deleted after its subscriptions were; a
// listing taken later cannot find subscriptions a finished retirement left.
//
// A listing that cannot be read discovers nothing this tick, and says why:
// the registry's own mailboxes were judged above regardless, and a mailbox
// that escaped the registry has waited this long already.
func (m *Mailboxes) discover(ctx context.Context, present map[uuid.UUID]placement.Seat,
	registered map[uuid.UUID]bool, clock sweepClock, cutoff time.Time,
) error {
	subs, err := m.queue.ListSubscriptions(ctx, topics.AgentInboxPrefix+">")
	if errors.Is(err, queue.ErrNotLive) {
		// A node that is shutting down. The duty's next holder lists them.
		return nil
	}
	if err != nil {
		return fmt.Errorf("the broker's seat mailboxes could not be listed, so a mailbox with no "+
			"registry record is not found this tick: %w", err)
	}
	var seats []uuid.UUID
	for _, sub := range subs {
		id, ok := topics.MailboxSeat(sub.Topic, sub.Group)
		if !ok || registered[id] || slices.Contains(seats, id) {
			continue
		}
		if _, inRoster := present[id]; inRoster {
			continue
		}
		seats = append(seats, id)
	}
	slices.SortFunc(seats, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })

	var errs []error
	for _, id := range seats {
		// NO HANDLE, and that is the honest record: this mailbox belongs to
		// a seat that is in neither the roster nor the registry, so nothing
		// this node holds can say what it was called. The id is what the
		// retirement needs — its runs, lease and subscriptions are all found
		// by it — and the label is written by the first registration or
		// sweep that finds the seat in the roster again ([presentRecord]).
		handle := id.String()
		rec, created, err := m.records.CreateMailbox(ctx, coord.MailboxRecord{Seat: id})
		if err != nil {
			errs = append(errs, fmt.Errorf("register the unrecorded mailbox of seat %q: %w", handle, err))
			continue
		}
		if !created {
			// A node registered it since the registry was read; the next
			// tick judges that record like any other.
			continue
		}
		log.WarnContext(ctx, "seat_mailbox_discovered", "handle", handle,
			"detail", "the broker holds a mailbox for a seat that is not in the active revision and "+
				"had no registry record (a registration that failed); it is registered now and "+
				"retired after the grace period like any other removed seat's")
		if _, err := m.judge(ctx, rec, clock, cutoff); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// keep handles a record whose seat is in the active revision: it clears an
// absence, and gives a record the sweep registered with no handle the one the
// roster names.
func (m *Mailboxes) keep(ctx context.Context, rec coord.MailboxRecord, seat placement.Seat, clock sweepClock) error {
	switch {
	case rec.Present() && rec.Handle == seat.Handle:
		return nil
	case rec.Retiring() && clock.now().Sub(rec.RetiringSince) < mailboxRetireStale:
		// A PEER'S RETIREMENT IN FLIGHT, started from a roster older than
		// this one. Left to it: that sweep re-reads the roster when it
		// finishes and restores the mailbox of a seat that came back, and
		// interfering would race its deletes.
		return nil
	}
	cleared, ok, err := m.records.UpdateMailbox(ctx, presentRecord(rec, seat))
	if err != nil {
		return fmt.Errorf("clear the absence of seat %q: %w", seat.Handle, err)
	}
	if !ok {
		// Somebody else wrote it first, a registering node or a peer
		// sweep, and either wrote what this would have.
		return nil
	}
	if !rec.Present() {
		log.InfoContext(ctx, "seat_mailbox_returned", "handle", cleared.Handle,
			"absent_since", rec.AbsentSince, "by", "sweep")
	}
	if rec.Retiring() {
		// AN ABANDONED RETIREMENT of a seat that is back. It may have
		// deleted the inbox before it died, and the seat's mail is dropped
		// until something creates it again.
		return m.restoreInbox(ctx, seat)
	}
	return nil
}

// judge handles a record whose seat is not in the active revision, reporting
// whether this call retired its mailbox.
func (m *Mailboxes) judge(ctx context.Context, rec coord.MailboxRecord, clock sweepClock, cutoff time.Time) (bool, error) {
	switch {
	case rec.Retiring():
		if clock.now().Sub(rec.RetiringSince) < mailboxRetireStale {
			// A peer sweep's retirement in flight. Two sweeps overlapping
			// during a duty handoff must retire a mailbox once.
			return false, nil
		}
		log.WarnContext(ctx, "seat_mailbox_retirement_resumed", "handle", recordLabel(rec),
			"retiring_since", rec.RetiringSince,
			"detail", "a previous sweep marked this mailbox for retirement and did not finish")
		return m.retire(ctx, rec, clock)
	case rec.AbsentSince.IsZero():
		// THE TICK'S OWN INSTANT, not the clock's current reading: the
		// roster that showed the seat missing was read when the tick began,
		// and an absence is never observed later than the read that saw it.
		stamped := rec
		stamped.AbsentSince = clock.at
		if _, ok, err := m.records.UpdateMailbox(ctx, stamped); err != nil {
			return false, fmt.Errorf("record the absence of seat %q: %w", recordLabel(rec), err)
		} else if ok {
			log.InfoContext(ctx, "seat_mailbox_absent", "handle", recordLabel(rec),
				"retire_after", clock.at.Add(MailboxRetirementGrace),
				"detail", "the seat is not in the active revision; its mailbox and the mail it "+
					"holds are kept until the grace period ends, and a seat restored under the "+
					"same handle before then finds both")
		}
		return false, nil
	case rec.AbsentSince.After(cutoff):
		return false, nil
	default:
		return m.retire(ctx, rec, clock)
	}
}

// retire deletes a removed seat's subscriptions and then its record.
//
// Everything it acts on — the lease, the executors, the subscriptions, the
// record — is found by the seat's ID; the handle names the seat in what it says
// and nowhere else, since a discovered record has none.
func (m *Mailboxes) retire(ctx context.Context, rec coord.MailboxRecord, clock sweepClock) (bool, error) {
	handle := recordLabel(rec)

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
	lease, refused, err := m.leases.TryAcquire(work, coord.SeatResource(rec.Seat), coord.AcquireOptions{
		Owner: m.owner,
		TTL:   m.leaseTTL,
		// No Preferred: the hint records the last node that RAN the seat,
		// and a claim that exists only to exclude one must leave it alone.
	})
	if err != nil {
		return false, fmt.Errorf("claim the lease of seat %q before retiring its mailbox: %w", handle, err)
	}
	if lease == nil {
		// WHICH of the two the refusal says, rather than both: an
		// operator reading "a node still holds it" goes looking for a
		// node serving a stale revision, and one reading "a node on a
		// lower lease protocol" waits for that node to leave — and only
		// one of them is true.
		detail := "the seat is absent from the active revision but a node still holds its " +
			"lease — one still serving a revision that has the seat; the mailbox is kept and " +
			"the claim retried on the next tick"
		if refused != coord.RefusedHeld {
			detail = "the seat is absent from the active revision but its lease could not be " +
				"claimed because a node on a lower lease protocol holds a presence or seat " +
				"lease in this fleet; the mailbox is kept until that node has left"
		}
		log.WarnContext(ctx, "seat_mailbox_retirement_held", "handle", handle,
			"absent_since", rec.AbsentSince, "refused", string(refused), "detail", detail)
		return false, nil
	}
	defer m.releaseSeat(ctx, *lease)

	// STAMPED AS IT IS WRITTEN, after the claim: see [sweepClock].
	mark := rec
	mark.RetiringSince = clock.now()
	marked, ok, err := m.records.UpdateMailbox(work, mark)
	if err != nil {
		return false, fmt.Errorf("mark the mailbox of seat %q for retirement: %w", handle, err)
	}
	if !ok {
		// A registering node or a peer sweep wrote first. Either the seat
		// is back or somebody else is retiring it; neither is this call's.
		return false, nil
	}

	// THE RUNS BEFORE THE SUBSCRIPTIONS, under the same lease and budget: a
	// run left behind once its control topic and inbox are gone is one no
	// completion, answer or resume can ever reach again. A run left WAITING on
	// a call is left behind too, so it keeps the mailbox as a failure does —
	// without failing the tick, and said beside a failure the retirer answers
	// with it.
	waiting, runsErr := m.runs(work, rec.Seat, rec.Handle, lease.Owner, lease.Epoch)
	if waiting.waiting() {
		sayWaiting(ctx, handle, waiting)
	}
	if runsErr != nil || waiting.waiting() {
		m.unmark(ctx, marked)
	}
	if runsErr != nil {
		return false, fmt.Errorf("end the executor of retired seat %q: %w", handle, runsErr)
	}
	if waiting.waiting() {
		return false, nil
	}
	// AND ITS SESSIONS, once nothing can capture another: its runs are
	// ended, and the lease keeps any node from starting one.
	if sessionsErr := m.sessions(work, rec.Seat, rec.Handle, marked.RetiringSince); sessionsErr != nil {
		m.unmark(ctx, marked)
		return false, fmt.Errorf("remove the executor sessions of retired seat %q: %w",
			handle, sessionsErr)
	}
	if deleteErr := m.deleteSubscriptions(work, rec.Seat, handle); deleteErr != nil {
		m.unmark(ctx, marked)
		return false, deleteErr
	}
	gone, err := m.records.DeleteMailbox(work, rec.Seat, marked.Version)
	if err != nil {
		// Left marked. A later sweep resumes it once the mark is stale, and
		// a seat that returns first takes the record over.
		return false, fmt.Errorf("delete the mailbox record of retired seat %q: %w", handle, err)
	}
	if !gone {
		return false, m.afterLostDelete(ctx, rec)
	}
	log.InfoContext(ctx, "seat_mailbox_retired", "handle", handle, "absent_since", rec.AbsentSince,
		"detail", "the seat has been absent from the active revision for longer than the grace "+
			"period; its mailbox, the mail it held and its executor sessions are deleted, "+
			"and its memory is kept")

	// A SEAT ADDED AGAIN WHILE THIS RAN. The roster this sweep started from
	// may be older than the revision now active, and a node that applied it
	// may have created the inbox between this sweep's mark and its delete
	// without being able to register it. Re-read, and restore what the seat
	// needs; a roster that cannot be read leaves it to that node's next apply.
	if roster, err := m.roster(ctx); err == nil {
		if back, in := seatIn(roster, rec.Seat); in {
			if err := m.restoreInbox(ctx, back); err != nil {
				return true, err
			}
			if _, _, err := m.records.CreateMailbox(ctx,
				coord.MailboxRecord{Seat: back.ID, Handle: back.Handle}); err != nil {

				return true, fmt.Errorf("register the restored mailbox of seat %q: %w",
					back.Handle, err)
			}
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
// deadline, and a release that inherited it would leave a seat made an
// agent's again unclaimable for a full TTL. A release that fails leaves exactly that, which
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
// comprises: the inbox every node creates, and the sandbox control and
// reflection topics the seat's owner subscribes — every one named by the
// seat's ID, as the seat host names them. Each is attempted even when one
// fails, so a retirement that is retried has less left to do.
func (m *Mailboxes) deleteSubscriptions(ctx context.Context, seat uuid.UUID, handle string) error {
	var errs []error
	for _, sub := range [][2]string{
		{topics.AgentInbox(seat), topics.AgentInboxGroup(seat)},
		{topics.AgentControl(seat), topics.AgentControlGroup(seat)},
		{topics.AgentReflect(seat), topics.AgentReflectGroup(seat)},
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
func (m *Mailboxes) afterLostDelete(ctx context.Context, rec coord.MailboxRecord) error {
	handle := recordLabel(rec)
	current, found, err := m.records.Mailbox(ctx, rec.Seat)
	if err != nil {
		return fmt.Errorf("re-read the mailbox record of seat %q after a lost retirement: %w", handle, err)
	}
	if !found || current.Retiring() {
		return nil
	}
	log.WarnContext(ctx, "seat_mailbox_retirement_raced", "handle", recordLabel(current),
		"detail", "the seat was registered again while its previous mailbox was being retired; "+
			"its inbox is restored in case the retirement deleted it after it was created")
	// THE RECORD AS IT IS NOW, not as this retirement read it: the
	// registration that took the record over may have written the handle a
	// discovered record lacked, and the id is the same either way.
	return m.restoreInbox(ctx, placement.Seat{ID: current.Seat, Handle: current.Handle})
}

// seatIn finds a seat by id in a roster, reporting whether it is there.
func seatIn(roster []placement.Seat, id uuid.UUID) (placement.Seat, bool) {
	for _, s := range roster {
		if s.ID == id {
			return s, true
		}
	}
	return placement.Seat{}, false
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
		log.WarnContext(ctx, "seat_mailbox_unmark_failed", "handle", recordLabel(marked), "error", err.Error(),
			"detail", "the retirement mark stays until a later sweep resumes it")
	}
}

// sayWaiting says, at info, that the retirement of the seat handle names left
// its runs waiting on the calls waiting names, and until when.
//
// SAID ON EVERY TICK THE WAIT LASTS, never only when it changes. A tick is
// [Interval] apart, so a wait costs one line a quarter of an hour for as long
// as its calls run, and each line names the whole wait — every call, and the
// latest instant any of them is waited for — so any one line reads alone.
// Saying it once per change would need what was said last kept for each seat
// on whichever node holds the duty, and that memory goes when the duty moves to
// another node, which then says it again anyway: state kept to spare a line
// every fifteen minutes.
func sayWaiting(ctx context.Context, handle string, waiting SeatRunsWaiting) {
	const kept = "a tool call of the seat's run is still running, so the run is not ended and its " +
		"mailbox is kept, which is no failure: "
	if waiting.Until.IsZero() {
		log.InfoContext(ctx, "seat_mailbox_retirement_waiting", "handle", handle,
			"calls", words.And(waiting.Calls),
			"detail", kept+"a call names no bound, so the retirement is tried again on every tick, and "+
				"ends the run once the call is recorded or its process stops")
		return
	}
	log.InfoContext(ctx, "seat_mailbox_retirement_waiting", "handle", handle,
		"calls", words.And(waiting.Calls), "until", waiting.Until,
		"detail", kept+"the retirement is tried again on every tick, and ends the run once the call is "+
			"recorded, its process stops or until passes")
}

// restoreInbox creates a seat's inbox, the half of its mailbox every node
// creates and a retirement may have deleted under it.
func (m *Mailboxes) restoreInbox(ctx context.Context, seat placement.Seat) error {
	handle := seat.Handle
	made, err := m.queue.EnsureSubscription(ctx,
		topics.AgentInbox(seat.ID), topics.AgentInboxGroup(seat.ID))
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

// presentRecord is rec as the seat it belongs to, back in the roster, makes it:
// its absence and any retirement cleared, and its label the seat's handle, at
// the version it was read at.
//
// THE LABEL TOO, because a record the sweep registered from the broker's
// listing alone carries none, and this is the one moment anything that knows
// the seat's handle writes the record. The handle is the seat's for good (its
// id is derived from it), so writing it over a label that is already there
// writes the same value.
func presentRecord(rec coord.MailboxRecord, seat placement.Seat) coord.MailboxRecord {
	rec.AbsentSince, rec.RetiringSince = time.Time{}, time.Time{}
	rec.Handle = seat.Handle
	return rec
}

// recordLabel is what a log line or an error calls a record's seat: its handle,
// or its id for a record the broker's listing alone found, which nothing the
// fleet holds can name.
func recordLabel(rec coord.MailboxRecord) string {
	if rec.Handle != "" {
		return rec.Handle
	}
	return rec.Seat.String()
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
