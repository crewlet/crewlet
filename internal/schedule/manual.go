package schedule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
)

// A run a person asks for: one schedule fired now, off its cron.
//
// # Why it is not a tick
//
// The tick answers "what came due", and a person asking for a run is not
// asking that: nothing came due. So the ask evaluates no window, moves none,
// and is not the scheduler duty's. Any node that publishes can make it (the
// claim is the fleet's, and the fire is an event on the runner seat's inbox,
// which the node holding that seat consumes), so the node a person reaches
// fires it, and the duty's holder never hears of it.
//
// # Its own identity
//
// A tick's fire is claimed on the schedule's LOCAL MINUTE ([FireLabel]). An
// ask is claimed on the ASK ([ManualFireLabel]), a label no minute renders
// to, so the two never meet in the ledger: an ask at 09:00 does not take the
// 09:00 tick's claim, the tick does not refuse the ask, and the catchup
// pass's question about a minute ([Claimer.MinuteClaimed]) is never answered
// by an ask. What it keeps is the ledger's one promise, on its own key: the
// same ask made again (a retry of a request whose answer was lost) is the
// same fire, and is not sent twice.
//
// # What it shares with a tick
//
// Everything after the decision to fire ([dispatcher]): the claim before the
// publish, the trace, the schedule's conversation, the task the seat is
// handed and the record of the fire. So a run asked for continues the session
// the schedule's last fire left, and the schedule's history lists it, as
// [OutcomeFiredManually] rather than `fired`, so it reads as the run a person
// asked for.
//
// # What it refuses
//
// A schedule that is switched off (`enabled: false`) is refused
// ([ErrScheduleDisabled]): switching it off is somebody's decision that it
// does not run, and a run now would overrule it without changing it. Switch
// it on, then run it. So is a schedule nothing can run ([ErrNoRunners]), and
// a seat a person paused is answered [ManualPaused] rather than woken.

// manualLabelPrefix opens every ask's label. A tick's label is thirteen
// characters of digits around a `T`, so nothing a clock renders begins with
// it.
const manualLabelPrefix = "manual-"

// ManualFireLabel is the ledger identity of one ask for a run: the fire label
// every runner's claim of that ask shares.
//
// DERIVED FROM THE ASK, never from the clock: two people asking in one minute
// are two runs, and one request sent twice is one. A digest, because an ask's
// own key is a caller's string of any length and the label is a column and a
// coordination key.
func ManualFireLabel(ask string) string {
	sum := sha256.Sum256([]byte(ask))
	return manualLabelPrefix + hex.EncodeToString(sum[:10])
}

// ManualOptions configures a [Manual]. The first two are required.
type ManualOptions struct {
	// Publisher is where the fires go. Required.
	Publisher Publisher

	// Ledger is the at-most-once claim, the one the scheduler claims its
	// ticks in. Required, for the reason [Options.Ledger] is.
	Ledger Claimer

	// Paused reports whether a person has a runner seat paused. Nil pauses
	// nothing. A paused seat is not fired for and nothing is recorded: the
	// ask is answered, by a person who can resume the seat and ask again,
	// where a tick's fire is recorded skipped because nobody is there to
	// tell.
	//
	// AN ERROR IS UNKNOWN and the run is sent, as a tick's is
	// ([Options.Paused]): a paused seat's inbox holds it.
	Paused func(seat uuid.UUID) (bool, error)

	// Mailboxes confirms a runner seat's mailbox before its fire is
	// claimed, as a tick does ([Options.Mailboxes]). Nil confirms nothing.
	Mailboxes Mailboxes

	// Trace mints the trace each run is sent under; nil is the scheduler's
	// own minter.
	Trace func(ctx context.Context) events.TraceContext
}

// Manual fires schedules on a person's ask.
type Manual struct {
	dispatcher
	paused    func(seat uuid.UUID) (bool, error)
	mailboxes Mailboxes
}

// NewManual builds a Manual, refusing an unusable wiring as [New] does.
func NewManual(opts ManualOptions) (*Manual, error) {
	switch {
	case opts.Publisher == nil:
		return nil, ErrNoPublisher
	case opts.Ledger == nil:
		return nil, ErrNoLedger
	}
	return &Manual{
		dispatcher: dispatcher{pub: opts.Publisher, ledger: opts.Ledger,
			trace: cmpOrFunc(opts.Trace, newTrace)},
		paused:    opts.Paused,
		mailboxes: opts.Mailboxes,
	}, nil
}

// ManualAsk is one ask that a schedule run now.
type ManualAsk struct {
	// Company is the org the schedule was resolved in, and its runners are.
	Company *org.Organization

	// Entry is the schedule, as [Find] resolved it from Company.
	Entry Entry

	// Ask names the ask: the same value is the same run, however often it
	// is sent. Required.
	Ask string

	// RequestedBy is the person asking, as the seat is told and the record
	// keeps.
	RequestedBy string

	// At is the moment of the ask, which the run is recorded as due at.
	// Zero reads the clock.
	At time.Time
}

// ManualStatus is what became of an ask for one runner seat.
type ManualStatus string

const (
	// ManualFired is a run sent to the seat's inbox.
	ManualFired ManualStatus = "fired"
	// ManualAlreadyFired is an ask this seat's run was already claimed for:
	// the same ask, made again. Nothing more was sent.
	ManualAlreadyFired ManualStatus = "already_fired"
	// ManualPaused is a seat a person has paused. Nothing was sent or
	// recorded.
	ManualPaused ManualStatus = "paused"
	// ManualNotReady is a seat whose mailbox could not be confirmed.
	// Nothing was claimed, so asking again is safe.
	ManualNotReady ManualStatus = "not_ready"
	// ManualUnknown is a run that may or may not have been sent: the claim
	// could not be read, or the publish after it failed. The same ask made
	// again does not send it twice.
	ManualUnknown ManualStatus = "unknown"
)

// ManualRun is the ask's answer for one runner seat.
type ManualRun struct {
	Handle string
	Status ManualStatus
	// TraceID and RunID name the run where one was sent.
	TraceID string
	RunID   string
}

// ErrNoRunners is an ask for a schedule nothing can run: a unit with no
// direct agent member, or a `lead` schedule on a unit no agent leads.
var ErrNoRunners = errors.New("schedule: nothing can run this schedule")

// ErrScheduleDisabled is an ask for a schedule switched off
// (`enabled: false`).
var ErrScheduleDisabled = errors.New("schedule: the schedule is switched off")

// What keeps a company's schedules from running at all, by cron or by hand.
// The engine answers an ask with one where its own tick loop would not be
// armed for the same reason.
var (
	// ErrSchedulingOff is a company that switched scheduling off.
	ErrSchedulingOff = errors.New("schedule: scheduling is switched off for this company")
	// ErrNoModel is a company that configures no model, whose seats hold
	// their inboxes until one exists: a run sent now would wait behind that
	// hold and start whenever a provider arrived.
	ErrNoModel = errors.New("schedule: the company configures no model to run a fire on")
	// ErrNotPublishing is a node that wakes no seats: one running no
	// company, or in a maintenance mode.
	ErrNotPublishing = errors.New("schedule: this node publishes no fires")
)

// ErrNoAsk is an ask that names no operation.
var ErrNoAsk = errors.New("schedule: an ask for a run names no operation")

// Fire sends the schedule's task to each of its runners now, and answers
// what became of each, in the runners' own order.
//
// PER RUNNER, as a tick's fan-out is: each seat has its own claim, so one
// paused or unreachable member does not keep its colleagues from running.
//
// A DISABLED SCHEDULE IS REFUSED, before anything is claimed: switching it
// off is somebody's decision that it does not run, which a run now would
// overrule without changing.
func (m *Manual) Fire(ctx context.Context, ask ManualAsk) ([]ManualRun, error) {
	if strings.TrimSpace(ask.Ask) == "" {
		return nil, ErrNoAsk
	}
	if !ask.Entry.Schedule.IsEnabled() {
		return nil, ErrScheduleDisabled
	}
	runners := ask.Entry.Runners(ask.Company)
	if len(runners) == 0 {
		return nil, ErrNoRunners
	}
	at := ask.At.UTC()
	if ask.At.IsZero() {
		at = now()
	}
	label := ManualFireLabel(ask.Ask)
	out := make([]ManualRun, 0, len(runners))
	for _, handle := range runners {
		out = append(out, m.fire(ctx, ask, handle, at, label))
	}
	return out, nil
}

// fire is the ask for one runner seat.
func (m *Manual) fire(ctx context.Context, ask ManualAsk, handle string, at time.Time, label string) ManualRun {
	run := ManualRun{Handle: handle}
	e := ask.Entry
	seat := ask.Company.AgentSeatByHandle(handle)
	if seat == nil {
		// The runners were resolved from this same org a moment ago, so
		// this is a chart that names a runner it does not hold. Nothing is
		// claimed.
		log.WarnContext(ctx, "schedule_runner_not_found", "handle", handle,
			"schedule", e.Schedule.Name, "scope", e.ScopeName, "scope_id", e.ScopeID)
		run.Status = ManualNotReady
		return run
	}
	agentID, ok := ask.Company.AgentIDFor(seat)
	if !ok {
		run.Status = ManualNotReady
		return run
	}
	if m.paused != nil {
		paused, err := m.paused(agentID)
		if err != nil {
			log.WarnContext(ctx, "schedule_pause_unknown", "schedule", e.Schedule.Name,
				"handle", handle, "agent_id", agentID.String(), "error", err,
				"detail", "whether the seat is paused could not be read, so the run is "+
					"sent; a paused seat's inbox holds it rather than running it")
		} else if paused {
			run.Status = ManualPaused
			return run
		}
	}
	// THE MAILBOX BEFORE THE CLAIM: see [Options.Mailboxes].
	if m.mailboxes != nil {
		if err := m.mailboxes.ConfirmMailbox(ctx, agentID); err != nil {
			log.WarnContext(ctx, "schedule_fire_deferred", "schedule", e.Schedule.Name,
				"handle", handle, "fire_label", label, "error", err,
				"detail", "the runner seat's mailbox could not be confirmed, so the run a "+
					"person asked for was not claimed")
			run.Status = ManualNotReady
			return run
		}
	}
	sent := m.dispatch(ctx, dispatch{
		entry: e, seat: seat, agentID: agentID, handle: handle, at: at, label: label,
		outcome: OutcomeFiredManually, requestedBy: ask.RequestedBy,
	})
	run.TraceID, run.RunID = sent.traceID, sent.runID
	switch sent.status {
	case dispatchSent:
		run.Status = ManualFired
	case dispatchHeld:
		run.Status = ManualAlreadyFired
	case dispatchClaimUnknown, dispatchPublishFailed:
		run.Status = ManualUnknown
	}
	return run
}

// Find resolves one schedule of the company by its identity as a person names
// it: the scope's type, the scope by its ID or its name (a seat's agent id or
// its handle, a unit's key), and the schedule's name.
//
// BY EITHER, because the two surfaces that name a schedule hold different
// halves: a `schedules` row carries the identity the ledger keys on, and a
// person typing one knows the handle.
func Find(o *org.Organization, scope types.ScheduleScope, scopeRef, name string) (Entry, bool) {
	for _, e := range Entries(o) {
		if e.Scope == scope && e.Schedule.Name == name && (e.ScopeID == scopeRef || e.ScopeName == scopeRef) {
			return e, true
		}
	}
	return Entry{}, false
}
