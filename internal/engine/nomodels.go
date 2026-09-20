package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A COMPANY WITH NO MODELS RUNS, AND ITS SEATS' WORK WAITS FOR ONE.
//
// An empty providers.llm is a valid company: an org chart written before its
// credentials exist, which is also what a company created from the dashboard
// is until somebody adds a provider. It is applied like any other. The epoch
// carries no model registry ([Company.Models] is nil), its seats are placed,
// and their mailboxes attach and keep what arrives. What no seat can do is
// think, so the inbox screening parks every delivery ([inbox.Screen], stage
// 3): the seat's inbox is paused first, then the partition is requeued and
// acked, and the work waits on the broker.
//
// PARKED RATHER THAN FAILED. A turn that could not build its runner proves
// nothing reached outside the engine, so the dispatcher NAKs it, and a
// condition that holds until an operator edits the configuration would spend
// the broker's whole delivery budget on each event and then lose it. The one
// failure an operator is told about is the one that says what to change: each
// node logs `company_has_no_models` when it installs such an epoch, and
// `seat_inbox_paused` for every seat whose work it holds, both naming
// providers.llm.
//
// SCHEDULED WORK IS NOT HELD, because it is not sent: the scheduler stays
// disarmed while the company has no model ([Engine.schedulerRuns]). A fire
// parked behind the pause is a standup that runs days late, and a week of
// them would all run the moment a provider arrived.
//
// THE PAUSE IS RELEASED WHEN THE CONDITION CLEARS, and that half is what used
// to be missing: nothing lifted the hold, so a seat paused here stayed deaf
// until the process restarted, however long ago its provider had been added.
// The condition clears in exactly one place, an apply that installs an epoch
// with a model, and that apply releases every inbox this client paused. Boot
// needs no release: holds live in the queue client, and a new process has
// none.

// pauseReasonNoTurnEngine is the hold name the no-model park takes on a seat's
// inbox.
//
// A STABLE KEY, not the screening's prose. Pause holds are keyed by reason so
// two subsystems gating one inbox cannot release each other's hold, which
// means the pause and the release must spell it identically. It is the one
// reason the engine takes today: a seat parked on a sandbox run holds nothing,
// because the dispatcher requeues its deliveries instead. Deriving it from the
// human-readable reason would make an edit to a log message silently strand
// every seat that was parked under the old wording.
const pauseReasonNoTurnEngine = "no_turn_engine"

// modelHolds records the seat inboxes this client paused for want of a model.
//
// RECORDED, because which inboxes this client paused is a fact only the pause
// knows: the queue contract lists no holds, and a release that walked the new
// epoch's seats instead would resume every seat on every apply to lift holds
// that almost never exist, and would still miss one the new revision no
// longer names.
type modelHolds struct {
	mu    sync.Mutex
	seats map[string]struct{}
}

// record notes that handle's inbox holds the no-model pause.
func (h *modelHolds) record(handle string) {
	if h.seats == nil {
		h.seats = map[string]struct{}{}
	}
	h.seats[handle] = struct{}{}
}

// pause stops delivery on a seat's inbox before the no-model park, so the
// requeued copies buffer on the queue rather than looping straight back.
//
// THE RACE WITH THE RELEASE is closed by ordering, not by a lock held across
// both. The hold is taken and recorded under the record's lock, and the epoch
// is read after that; an apply installs its epoch before the release snapshots
// the record. So either the release sees this hold, or this read sees the
// epoch that makes it unnecessary and lifts it at once. Holding the lock across
// the queue call instead would deadlock on the in-memory twin, whose resume
// drains synchronously into handlers that come straight back here.
func (e *Engine) pause(ctx context.Context, handle, reason string) error {
	subject, group := topics.AgentInbox(handle), topics.AgentInboxGroup(handle)
	if subject == "" || group == "" {
		return fmt.Errorf("engine: seat %q has no inbox subject", handle)
	}
	e.modelHolds.mu.Lock()
	err := e.backends.Queue.PauseTopic(ctx, subject, group, pauseReasonNoTurnEngine)
	if err == nil {
		e.modelHolds.record(handle)
	}
	e.modelHolds.mu.Unlock()
	if err != nil {
		return err
	}
	log.WarnContext(ctx, "seat_inbox_paused", "handle", handle, "reason", reason,
		"detail", "the company configures no model provider, so this seat's "+
			"work waits on its inbox; it is delivered once a revision adds one "+
			"under providers.llm")
	if c := e.Company(); c != nil && c.Models != nil {
		e.releaseModelHolds(ctx)
	}
	return nil
}

// releaseModelHolds lifts the no-model pause from every inbox this client
// paused, for an epoch that has a model.
//
// The caller has already made that epoch current: a delivery the release lets
// through is screened and run against the company that has a model.
func (e *Engine) releaseModelHolds(ctx context.Context) {
	e.modelHolds.mu.Lock()
	held := slices.Sorted(maps.Keys(e.modelHolds.seats))
	clear(e.modelHolds.seats)
	e.modelHolds.mu.Unlock()

	for _, handle := range held {
		subject, group := topics.AgentInbox(handle), topics.AgentInboxGroup(handle)
		if err := e.backends.Queue.ResumeTopic(ctx, subject, group, pauseReasonNoTurnEngine); err != nil {
			// Kept on the record, so the next release tries it again. A
			// resume fails only on a client that has stopped, which is a
			// process on its way out; forgetting the hold would be the one
			// way to make it permanent.
			e.modelHolds.mu.Lock()
			e.modelHolds.record(handle)
			e.modelHolds.mu.Unlock()
			log.WarnContext(ctx, "seat_inbox_not_resumed", "handle", handle,
				"error", err, "detail", "this seat's held work stays on its inbox "+
					"until the next release")
			continue
		}
		log.InfoContext(ctx, "seat_inbox_resumed", "handle", handle,
			"reason", pauseReasonNoTurnEngine)
	}
}
