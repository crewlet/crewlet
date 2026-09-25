package builtin

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// The tools' wire names.
const (
	PauseSeatTool  = "pause_seat"
	ResumeSeatTool = "resume_seat"
)

// MaxPauseReasonRunes bounds the reason a pause carries.
//
// The same bound as a reassignment's reason (E-W15), and for the same
// reader: it is one line on a seat's profile and in the feed, saying why, and
// a person writing more than that is writing a note that belongs on the work
// item the seat was on.
const MaxPauseReasonRunes = 500

// pauseAttempts bounds how many times a pause or a resume re-reads the record
// after losing a compare-and-set to somebody else's write. Each loss means a
// person changed the seat between this call's read and its write; four in a
// row is contention no person produces, and the call refuses rather than spin.
const pauseAttempts = 4

// SeatPauseStore is the record pause_seat and resume_seat change —
// [coord.SeatPauses] less its watch, which only the nodes carrying a pause out
// read.
type SeatPauseStore interface {
	SeatPause(ctx context.Context, handle string) (coord.SeatPause, bool, error)
	CreateSeatPause(ctx context.Context, p coord.SeatPause) (coord.SeatPause, bool, error)
	UpdateSeatPause(ctx context.Context, p coord.SeatPause) (coord.SeatPause, bool, error)
	DeleteSeatPause(ctx context.Context, handle string, version uint64) (bool, error)
}

// Announcer publishes the record of a change.
type Announcer interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
}

// SeatPauseDeps are the pause tools' dependencies.
type SeatPauseDeps struct {
	// Pauses is the fleet's record. Nil omits both tools.
	Pauses SeatPauseStore

	// Announce publishes seat_paused and seat_resumed, once per change, by
	// the caller whose compare-and-set won. Required with Pauses.
	Announce Announcer

	// Org is the company, resolved per call: a pause names a seat of it.
	// Required with Pauses.
	Org func() *org.Organization

	// Actor is who is pausing, off the caller's own credential — the same
	// resolution every other operator write takes. Required with Pauses.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)

	// Now is the clock a pause is stamped with; nil is the wall clock.
	Now func() time.Time
}

func (d SeatPauseDeps) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func (d SeatPauseDeps) wired() bool {
	return d.Pauses != nil && d.Announce != nil && d.Org != nil && d.Actor != nil
}

// pauseSeat pauses a seat: it takes no new work until somebody resumes it.
//
// # What it promises
//
// `applied` means the pause is the fleet's record: every node holding or later
// acquiring the seat holds its mail, its schedules are skipped, and — with
// `stop_running` — the turn it is on ends at its next round. The node holding
// the seat carries that out from its own watched copy, typically within a
// second; nothing here waits for it, because the record IS the pause and a
// node that has not heard yet holds its mail the moment it does.
//
// # Every node has to be able to carry it
//
// Any live node may be the next to hold the seat, and an older build would run
// its mail as if nothing had happened. So the gate asks all of them, and a
// fleet mid-upgrade refuses `peer_upgrading` until every node has the build.
//
// # A second pause is not a second change
//
// Pausing a paused seat changes nothing and announces nothing — unless it asks
// to stop the running turn and the pause in place did not, which is a real
// change: the record is amended, under the version read, to name the person
// who asked for the stop.
type pauseSeat struct {
	deps  SeatPauseDeps
	fleet Fleet
}

var _ tools.SeatCallable = (*pauseSeat)(nil)

func (t *pauseSeat) Name() string { return PauseSeatTool }

func (t *pauseSeat) Description() string {
	return "Pause an agent seat: it starts no new turn until somebody resumes it, its " +
		"incoming work waits on its inbox in order, and its scheduled runs are skipped. " +
		"The turn it is on finishes first unless `stop_running` is true, which ends that " +
		"turn at its next round instead. Pausing a paused seat changes nothing, except to " +
		"add `stop_running`. Resume it with resume_seat."
}

func (t *pauseSeat) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"handle": map[string]any{
				"type":        "string",
				"description": "The handle of the agent seat to pause.",
			},
			"reason": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("Why, in a line — shown on the seat and in the "+
					"feed. At most %d characters.", MaxPauseReasonRunes),
			},
			"stop_running": map[string]any{
				"type": "boolean",
				"description": "End the turn the seat is on at its next round, rather " +
					"than letting it finish. The stopped turn is not run again.",
			},
		},
		"required": []any{"handle"},
	}
}

func (t *pauseSeat) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *pauseSeat) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, seat, refusal := resolvePauseCall(ctx, t.deps, turn, PauseSeatTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	reason := strings.TrimSpace(argString(args, "reason"))
	if n := utf8.RuneCountInString(reason); n > MaxPauseReasonRunes {
		return failed(fmt.Sprintf("That `reason` is %d characters and a pause's reason may be "+
			"at most %d: it is one line on the seat. Put the detail on the work item.",
			n, MaxPauseReasonRunes)), nil
	}
	stop := argBool(args, "stop_running")
	if refusal := fleetCanCarry(ctx, t.fleet, PauseSeatTool, coord.FeatureSeatPause); refusal != nil {
		return *refusal, nil
	}

	for range pauseAttempts {
		current, paused, err := t.deps.Pauses.SeatPause(ctx, seat.Handle())
		if err != nil {
			return refused(tools.RefusalUnavailable, fmt.Sprintf("pause_seat could not read "+
				"whether %s is paused right now (%v). Nothing was changed — try again.",
				seat.Handle(), err)), nil
		}
		switch {
		case !paused:
			stored, created, err := t.deps.Pauses.CreateSeatPause(ctx, coord.SeatPause{
				Handle: seat.Handle(), By: actor.Handle, OperatorID: actor.OperatorID,
				Seat: actor.Seat, Reason: reason, StopRunning: stop, At: t.deps.now(),
			})
			if err != nil {
				return pauseUnknown(ctx, PauseSeatTool, seat.Handle(), err), nil
			}
			if !created {
				continue // somebody paused it first: read theirs
			}
			announceSeatPaused(ctx, t.deps, seat, stored)
			return pauseAnswer(stored, true)
		case stop && !current.StopRunning:
			amended := current
			amended.StopRunning = true
			amended.By, amended.OperatorID, amended.Seat = actor.Handle, actor.OperatorID, actor.Seat
			if reason != "" {
				amended.Reason = reason
			}
			stored, ok, err := t.deps.Pauses.UpdateSeatPause(ctx, amended)
			if err != nil {
				return pauseUnknown(ctx, PauseSeatTool, seat.Handle(), err), nil
			}
			if !ok {
				continue // resumed or amended meanwhile: decide again
			}
			announceSeatPaused(ctx, t.deps, seat, stored)
			return pauseAnswer(stored, true)
		default:
			return pauseAnswer(current, false)
		}
	}
	return refused(tools.RefusalConflict, fmt.Sprintf("pause_seat lost %d races in a row for "+
		"%s to other changes to its pause. Read the seat again before retrying.",
		pauseAttempts, seat.Handle())), nil
}

// resumeSeat lifts a seat's pause: it takes work again, the mail that waited
// first and in order.
//
// Resuming a seat that is not paused changes nothing and announces nothing, as
// a second pause does not: both answer `applied`, because the seat is in the
// state asked for.
type resumeSeat struct {
	deps  SeatPauseDeps
	fleet Fleet
}

var _ tools.SeatCallable = (*resumeSeat)(nil)

func (t *resumeSeat) Name() string { return ResumeSeatTool }

func (t *resumeSeat) Description() string {
	return "Resume a paused agent seat: it takes work again, starting with what waited " +
		"on its inbox while it was paused, in order. Scheduled runs skipped during the " +
		"pause are not replayed. Resuming a seat that is not paused changes nothing."
}

func (t *resumeSeat) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"handle": map[string]any{
				"type":        "string",
				"description": "The handle of the agent seat to resume.",
			},
		},
		"required": []any{"handle"},
	}
}

func (t *resumeSeat) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *resumeSeat) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, seat, refusal := resolvePauseCall(ctx, t.deps, turn, ResumeSeatTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	if refusal := fleetCanCarry(ctx, t.fleet, ResumeSeatTool, coord.FeatureSeatPause); refusal != nil {
		return *refusal, nil
	}
	for range pauseAttempts {
		current, paused, err := t.deps.Pauses.SeatPause(ctx, seat.Handle())
		if err != nil {
			return refused(tools.RefusalUnavailable, fmt.Sprintf("resume_seat could not read "+
				"whether %s is paused right now (%v). Nothing was changed — try again.",
				seat.Handle(), err)), nil
		}
		if !paused {
			return jsonResult(map[string]any{
				"handle": seat.Handle(), "outcome": string(statelog.OutcomeApplied),
				"paused": false, "changed": false,
			})
		}
		lifted, err := t.deps.Pauses.DeleteSeatPause(ctx, seat.Handle(), current.Version)
		if err != nil {
			return pauseUnknown(ctx, ResumeSeatTool, seat.Handle(), err), nil
		}
		if !lifted {
			continue // amended or resumed meanwhile: decide again
		}
		role, agentID := seatIdentityOf(t.deps.Org(), seat)
		announce(ctx, t.deps.Announce, events.New(types.SeatResumed{
			Agent: agentID, AgentHandle: seat.Handle(), RoleName: role,
			ResumedBy: actor.Handle, ResumedBySeat: actor.Seat,
			PausedBy: current.By, PausedAt: current.At,
		}, events.TraceContext{}), role)
		return jsonResult(map[string]any{
			"handle": seat.Handle(), "outcome": string(statelog.OutcomeApplied),
			"paused": false, "changed": true,
		})
	}
	return refused(tools.RefusalConflict, fmt.Sprintf("resume_seat lost %d races in a row for "+
		"%s to other changes to its pause. Read the seat again before retrying.",
		pauseAttempts, seat.Handle())), nil
}

// resolvePauseCall resolves who is calling and which seat, refusing in the
// class a person's surface acts on.
func resolvePauseCall(ctx context.Context, deps SeatPauseDeps, turn *turnctx.Turn,
	tool string, args map[string]any) (Actor, *org.Role, *tools.Result) {

	if !deps.wired() {
		return Actor{}, nil, refusalOf(refused(tools.RefusalUnavailable,
			tool+" is unavailable: this surface was built without the pause record."))
	}
	actor, err := deps.Actor(ctx, turn)
	if err != nil {
		return Actor{}, nil, refusalOf(refused(tools.RefusalForbidden, tool+
			" acts as the person whose credential calls it, and this call carries none."))
	}
	handle := strings.TrimPrefix(strings.TrimSpace(argString(args, "handle")), "@")
	if handle == "" {
		return Actor{}, nil, refusalOf(failed(tool + " needs a `handle` — the agent seat's handle."))
	}
	company := deps.Org()
	var seat *org.Role
	if company != nil {
		seat = company.AgentSeatByHandle(handle)
	}
	if seat == nil {
		return Actor{}, nil, refusalOf(refused(tools.RefusalNotFound, fmt.Sprintf(
			"No agent seat has the handle %q, so there is nothing to %s. Only an agent "+
				"seat takes work a pause can hold; a person's seat does not.",
			clip(handle), strings.TrimSuffix(tool, "_seat"))))
	}
	return actor, seat, nil
}

// pauseUnknown answers a write the store may or may not have taken.
//
// NOT A REFUSAL: a compare-and-set whose reply never came may have landed, and
// the watch will say so on every node. A retry is safe — a pause of a paused
// seat and a resume of a free one both change nothing.
func pauseUnknown(ctx context.Context, tool, handle string, err error) tools.Result {
	log.WarnContext(ctx, "seat_pause_write_unknown", "tool", tool, "seat", handle,
		"error", err.Error())
	res, _ := jsonResult(map[string]any{
		"handle": handle, "outcome": string(statelog.OutcomeUnknown),
		"detail": "the write may or may not have landed; reading the seat again says which",
	})
	return res
}

func pauseAnswer(p coord.SeatPause, changed bool) (tools.Result, error) {
	return jsonResult(map[string]any{
		"handle": p.Handle, "outcome": string(statelog.OutcomeApplied),
		"paused": true, "changed": changed,
		"paused_by": p.By, "paused_by_seat": p.Seat, "paused_at": p.At.Format(time.RFC3339),
		"reason": p.Reason, "stop_running": p.StopRunning,
	})
}

// announceSeatPaused publishes the change the caller's compare-and-set won.
func announceSeatPaused(ctx context.Context, deps SeatPauseDeps, seat *org.Role, p coord.SeatPause) {
	role, agentID := seatIdentityOf(deps.Org(), seat)
	announce(ctx, deps.Announce, events.New(types.SeatPaused{
		Agent: agentID, AgentHandle: seat.Handle(), RoleName: role,
		PausedBy: p.By, PausedBySeat: p.Seat, Reason: p.Reason,
		StopRunning: p.StopRunning, PausedAt: p.At,
	}, events.TraceContext{}), role)
}

// announce publishes one record, BEST EFFORT: the pause is the coordination
// record, and a feed row that could not be published is a missing line, not a
// pause that did not happen.
func announce(ctx context.Context, pub Announcer, ev *events.Event, role string) {
	ev.Source = role
	if err := pub.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "seat_pause_not_announced", "type", ev.Type,
			"error", err.Error(), "detail", "the pause itself is recorded; the feed "+
				"has no row for it")
	}
}

// seatIdentityOf names a seat as its events do: its role name and agent id.
func seatIdentityOf(company *org.Organization, seat *org.Role) (string, string) {
	if company == nil || seat == nil {
		return "", ""
	}
	id, ok := company.AgentIDFor(seat)
	if !ok {
		return seat.Name, ""
	}
	return seat.Name, id.String()
}
