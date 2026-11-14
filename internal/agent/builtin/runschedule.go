package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	sched "github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// RunScheduleTool is the tool a person runs one schedule now with, its
// authority row's action.
const RunScheduleTool = string(authz.ActionScheduleRun)

// ScheduleRunner fires one schedule on a person's ask, on this node: the
// engine's [sched.Manual] behind the conditions a company's schedules run
// under at all.
type ScheduleRunner interface {
	// RunScheduleNow sends the schedule's task to each of its runner seats
	// and answers what became of each. Its refusals are
	// [sched.ErrSchedulingOff], [sched.ErrNoModel],
	// [sched.ErrScheduleDisabled], [sched.ErrNoRunners] and
	// [sched.ErrNotPublishing].
	RunScheduleNow(ctx context.Context, ask sched.ManualAsk) ([]sched.ManualRun, error)
}

// ScheduleRunDeps are what run_schedule fires through. The zero value omits
// the tool.
type ScheduleRunDeps struct {
	// Runner is this node's manual fire. Nil omits the tool.
	Runner ScheduleRunner

	// Org is the company, resolved per call: an ask names a schedule of it.
	// Required with Runner.
	Org func() *org.Organization

	// Actor is who asks, off the caller's own credential ([PrincipalActor]).
	// Required with Runner.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)

	// Now is the clock an ask is stamped with; nil is the wall clock.
	Now func() time.Time
}

func (d ScheduleRunDeps) serves() bool { return d.Runner != nil && d.Org != nil && d.Actor != nil }

func (d ScheduleRunDeps) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

// runSchedule runs one schedule now, off its cron.
//
// # What it promises
//
// `applied` means the schedule's task is on the inbox of every runner seat
// the answer lists as `fired`: an ordinary turn of the schedule's own
// conversation, started as soon as the seat is free, under the schedule's
// wall-clock cap, and recorded in the schedule's history as
// `fired_manually`. The schedule's cron is untouched: its next fire comes
// when it was going to, and this run takes no tick's place
// ([sched.ManualFireLabel]).
//
// # What it refuses
//
// In the engine's own words, and before anything is claimed: a company that
// switched scheduling off, or configures no model; a schedule switched off
// (`enabled: false`), since running it would overrule that decision without
// changing it; a schedule nothing can run; and a run every one of whose seats
// a person paused. A schedule some of whose seats are paused runs for the
// rest, and the answer names the paused ones.
//
// # Whose decision it is
//
// Waking a seat is its lead's to decide, or an administrator's, as pausing it
// is ([authz.ActionScheduleRun]): the call is decided on EVERY seat the
// schedule would wake, and refused whole where the caller may not wake one of
// them, so a unit's standup is its lead's to run and not any member's.
//
// # The same request again is the same run
//
// A request that names its operation (the dashboard's, a script's) sent again
// claims the run it already made and sends nothing more. A call that names
// none is a new run each time.
//
// # Never an agent's
//
// A seat that could run a schedule would be one starting its colleagues'
// turns, or its own, outside the times the people who run the company set.
type runSchedule struct {
	deps      ScheduleRunDeps
	authorize Authorizer
}

var _ tools.SeatCallable = (*runSchedule)(nil)

func (t *runSchedule) Name() string { return RunScheduleTool }

func (t *runSchedule) Description() string {
	return "Run one schedule now, outside its cron: its task goes to every seat the schedule " +
		"wakes, as one more run of the schedule's own work, recorded in its history as " +
		"`fired_manually`, and its next scheduled run is unchanged. A schedule that is " +
		"switched off (`enabled: false`) is refused: switch it on first. A paused seat is not " +
		"woken, and the answer says so for each seat (`status` per run). An `unknown` outcome " +
		"may or may not have reached a seat: the schedule's runs say which, and only a request " +
		"that names its operation is safe to send again. Name the schedule as the schedules " +
		"list does: its scope type, its scope and its name."
}

func (t *runSchedule) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"scope_type": map[string]any{
				"type": "string",
				"enum": []any{string(types.ScheduleScopeRole), string(types.ScheduleScopeUnit)},
				"description": "What declares the schedule: `role` for a seat's own schedule, " +
					"`unit` for a team's.",
			},
			"scope": map[string]any{
				"type": "string",
				"description": "The seat's handle for a role schedule, or the unit's key for a " +
					"unit schedule. The `scope_id` a schedules row carries is taken too.",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "The schedule's name within that seat or unit.",
			},
		},
		"required": []any{"scope_type", "scope", "name"},
	}
}

func (t *runSchedule) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *runSchedule) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if !t.deps.serves() {
		return refusedBy(tools.RefusalUnavailable, waitless(), RunScheduleTool+
			" is unavailable: this surface was built without the scheduler."), nil
	}
	actor, err := t.deps.Actor(ctx, turn)
	if err != nil {
		return refused(tools.RefusalForbidden, RunScheduleTool+
			" acts as the person whose credential calls it, and this call carries none."), nil
	}
	if actor.Kind == worklet.AuthorAgent {
		return refused(tools.RefusalForbidden, RunScheduleTool+" ran nothing: when a seat's "+
			"scheduled work runs is a person's to decide."), nil
	}
	scope := types.ScheduleScope(strings.TrimSpace(argString(args, "scope_type")))
	ref := strings.TrimPrefix(strings.TrimSpace(argString(args, "scope")), "@")
	name := strings.TrimSpace(argString(args, "name"))
	switch {
	case !scope.Valid():
		return failed(RunScheduleTool + " needs a `scope_type` of `role` or `unit`."), nil
	case ref == "":
		return failed(RunScheduleTool + " needs a `scope`: the seat's handle, or the unit's key."), nil
	case name == "":
		return failed(RunScheduleTool + " needs a `name`: the schedule's name."), nil
	}

	company := t.deps.Org()
	entry, found := sched.Find(company, scope, ref, name)
	var runners []string
	if found {
		runners = entry.Runners(company)
	}
	// DECIDED ON THE SEATS IT WOULD WAKE, every one. A schedule that names
	// none (it is not declared, or nothing can run it) is decided on the
	// scope AS TYPED, which nobody leads, so only an administrator passes
	// and is then told what is wrong: the refusal a caller reads never
	// depends on whether the schedule exists.
	owners := runners
	if len(owners) == 0 {
		owners = []string{ref}
	}
	for _, owner := range owners {
		if refusal := askAuthority(ctx, t.authorize, authz.ActionScheduleRun,
			authz.Object{Kind: authz.KindPerson, Owner: owner}); refusal != nil {
			return *refusal, nil
		}
	}
	if !found {
		return refused(tools.RefusalNotFound, fmt.Sprintf("%s ran nothing: no %s %q declares a "+
			"schedule named %q. The schedules list names every one, with its scope.",
			RunScheduleTool, scopeWord(scope), clip(ref), clip(name))), nil
	}

	// THE REQUEST'S OWN OPERATION names the ask where its transport carries
	// one, scoped by who sent it, so the request sent again is the same run.
	// A call that names none is a new ask.
	ask := uuid.NewString()
	if actor.WorkKey != "" {
		ask = actor.Handle + "\x00" + actor.OperatorID + "\x00" + actor.WorkKey
	}
	runs, err := t.deps.Runner.RunScheduleNow(ctx, sched.ManualAsk{
		Company: company, Entry: entry, Ask: ask, RequestedBy: actor.Handle, At: t.deps.now(),
	})
	switch {
	case errors.Is(err, sched.ErrNoRunners):
		return toPerson(refused(tools.RefusalConflict, fmt.Sprintf("%s ran nothing: no seat runs %q. %s",
			RunScheduleTool, clip(name), noRunnersWhy(entry))),
			caveat.ScheduleHasNobodyToRun(entry.Schedule.TargetsLead())), nil
	case errors.Is(err, sched.ErrScheduleDisabled):
		return toPerson(refused(tools.RefusalConflict, fmt.Sprintf("%s ran nothing: %q is switched off "+
			"(`enabled: false`), and a schedule switched off does not run, by its cron or by hand. "+
			"Switch it on, then run it.", RunScheduleTool, clip(name))), caveat.ScheduleSwitchedOff()), nil
	case errors.Is(err, sched.ErrSchedulingOff):
		return toPerson(refused(tools.RefusalConflict, RunScheduleTool+" ran nothing: scheduling is "+
			"switched off for this company (`scheduling.enabled: false`), so no schedule runs, by its "+
			"cron or by hand."), caveat.SchedulingSwitchedOff()), nil
	case errors.Is(err, sched.ErrNoModel):
		return toPerson(refused(tools.RefusalConflict, RunScheduleTool+" ran nothing: the company "+
			"configures no model, so no seat can work. Add one under `providers.llm`, and run it "+
			"again."), caveat.SchedulesWaitForAModel()), nil
	case errors.Is(err, sched.ErrNotPublishing):
		return refusedBy(tools.RefusalUnavailable, waitless(), RunScheduleTool+" is unavailable on "+
			"this node: it wakes no seats. Ask a node that serves the company."), nil
	case err != nil:
		return nodeFailure(ctx, RunScheduleTool, err, RunScheduleTool+" could not run "+clip(name),
			"Nothing was sent."), nil
	}
	return scheduleRunAnswer(entry, runs)
}

// scopeWord is a scope as a sentence names it.
func scopeWord(scope types.ScheduleScope) string {
	if scope == types.ScheduleScopeRole {
		return "seat"
	}
	return "unit"
}

// noRunnersWhy says why a declared schedule has nobody to run it.
func noRunnersWhy(e sched.Entry) string {
	if e.Scope == types.ScheduleScopeUnit && e.Schedule.TargetsLead() {
		return "It targets the unit's lead, and no agent seat leads the unit."
	}
	return "It reaches each direct agent member of its unit, and the unit has none."
}

// scheduleRunAnswer is an ask as its caller is answered: what became of it
// for each seat, and a refusal where no seat was sent the run.
func scheduleRunAnswer(e sched.Entry, runs []sched.ManualRun) (tools.Result, error) {
	rows := make([]map[string]any, 0, len(runs))
	by := map[sched.ManualStatus][]string{}
	for _, run := range runs {
		row := map[string]any{"handle": run.Handle, "status": string(run.Status)}
		if run.TraceID != "" {
			row["trace_id"] = run.TraceID
		}
		rows = append(rows, row)
		by[run.Status] = append(by[run.Status], run.Handle)
	}
	sent := len(by[sched.ManualFired]) + len(by[sched.ManualAlreadyFired])
	if sent == 0 {
		// NOBODY WAS WOKEN, each reason in its own words, the one a person
		// can act on first.
		switch {
		case len(by[sched.ManualUnknown]) > 0:
			// MAY OR MAY NOT HAVE REACHED THE SEATS: the schedule's runs say
			// which, and the same request sent again sends nothing twice.
			return jsonResult(map[string]any{
				"name": e.Schedule.Name, "scope_type": string(e.Scope), "scope_id": e.ScopeID,
				"outcome": string(statelog.OutcomeUnknown), "changed": false, "runs": rows,
			})
		case len(by[sched.ManualPaused]) > 0:
			return toPerson(refused(tools.RefusalConflict, fmt.Sprintf("%s ran nothing: %s paused, "+
				"and a paused seat starts no work. Resume it with %s and run the schedule again.",
				RunScheduleTool, isAre(by[sched.ManualPaused]), ResumeSeatTool)),
				caveat.ScheduleSeatsPaused(by[sched.ManualPaused])), nil
		default:
			return toPerson(refusedBy(tools.RefusalUnavailable, nil, fmt.Sprintf("%s ran nothing: "+
				"the inbox of %s is not ready on this node yet. Nothing was sent, so run it again "+
				"in a moment.", RunScheduleTool, words.And(by[sched.ManualNotReady]))),
				caveat.ScheduleSeatsNotReady(by[sched.ManualNotReady])), nil
		}
	}
	return jsonResult(map[string]any{
		"name": e.Schedule.Name, "scope_type": string(e.Scope), "scope_id": e.ScopeID,
		"outcome": string(statelog.OutcomeApplied),
		// FALSE WHERE THIS CALL SENT NOTHING NEW: the same request, again.
		"changed": len(by[sched.ManualFired]) > 0,
		"runs":    rows,
	})
}

// isAre is "a is" or "a and b are".
func isAre(handles []string) string {
	if len(handles) == 1 {
		return handles[0] + " is"
	}
	return words.And(handles) + " are"
}
