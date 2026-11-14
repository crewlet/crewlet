package builtin_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/tools"
)

// scheduleLeads is the chart run_schedule is decided against: jane leads both
// seats of the quality team, omar leads its lead's seat alone, the first seat
// its standup wakes.
var scheduleLeads = leadPairs{
	{"jane", "qa-lead"}: true, {"jane", "qa-dev"}: true, {"omar", "qa-lead"}: true,
}

// qualityTeam is a unit whose standup wakes both its seats, its lead's own
// triage, its developer's own smoke run, and a schedule somebody switched off.
func qualityTeam() *org.Organization {
	return &org.Organization{
		Name: "Acme",
		Units: []*org.Unit{{
			Name: "Quality", ID: "quality", Type: org.UnitTypeTeam, Lead: "qa-lead",
			Roles: []*org.Role{
				{Name: "QA Lead", DeclaredHandle: "qa-lead", Schedules: []org.Schedule{
					{Name: "triage", Cron: "0 8 * * *", Task: "triage the night's failures"},
				}},
				{Name: "QA Dev", DeclaredHandle: "qa-dev", Schedules: []org.Schedule{
					{Name: "smoke", Cron: "0 9 * * *", Task: "run smoke tests"},
				}},
			},
			Schedules: []org.Schedule{
				{Name: "standup", Cron: "30 9 * * 1-5", Task: "post your standup"},
				{Name: "retro", Cron: "0 16 * * 5", Task: "hold the retro", Enabled: org.Off()},
			},
		}},
	}
}

// manualRunner is the engine's manual fire over a memory ledger, as
// [engine.Engine.RunScheduleNow] builds it once the company's own conditions
// hold, and the asks it was handed.
type manualRunner struct {
	m      *schedule.Manual
	pub    *announced
	asks   []schedule.ManualAsk
	paused map[uuid.UUID]bool
}

func (r *manualRunner) RunScheduleNow(ctx context.Context, ask schedule.ManualAsk) ([]schedule.ManualRun, error) {
	r.asks = append(r.asks, ask)
	return r.m.Fire(ctx, ask)
}

// stillRunner runs nothing, for a catalogue that only has to serve the tool.
type stillRunner struct{}

func (stillRunner) RunScheduleNow(context.Context, schedule.ManualAsk) ([]schedule.ManualRun, error) {
	return nil, schedule.ErrNotPublishing
}

// sendingRunner sends every ask to each seat the schedule wakes, for a
// surface that only has to reach the answer.
type sendingRunner struct{}

func (sendingRunner) RunScheduleNow(_ context.Context, ask schedule.ManualAsk) ([]schedule.ManualRun, error) {
	var out []schedule.ManualRun
	for _, handle := range ask.Entry.Runners(ask.Company) {
		out = append(out, schedule.ManualRun{Handle: handle, Status: schedule.ManualFired,
			TraceID: "0af7651916cd43dd8448eb211c80319c"})
	}
	return out, nil
}

type scheduleRig struct {
	runner *manualRunner
	tool   tools.Callable
	o      *org.Organization
}

// newScheduleRig is the operator catalogue's run_schedule over a real manual
// fire, decided by the real authority table over [scheduleLeads]. key is the
// operation the request names, as the act transport's Idempotency-Key does.
func newScheduleRig(t *testing.T, key string) *scheduleRig {
	t.Helper()
	o := qualityTeam()
	runner := &manualRunner{pub: &announced{}, paused: map[uuid.UUID]bool{}}
	m, err := schedule.NewManual(schedule.ManualOptions{
		Publisher: runner.pub, Ledger: schedule.NewMemoryLedger(),
		Paused: func(seat uuid.UUID) (bool, error) { return runner.paused[seat], nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	runner.m = m
	actor := func(ctx context.Context, turn *turnctx.Turn) (builtin.Actor, error) {
		a, err := builtin.PrincipalActor(ctx, turn)
		a.WorkKey = key
		return a, err
	}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		ScheduleRuns: builtin.ScheduleRunDeps{
			Runner: runner, Org: func() *org.Organization { return o }, Actor: actor,
			Now: func() time.Time { return time.Date(2026, 11, 12, 14, 12, 0, 0, time.UTC) },
		},
		Authorize: builtin.Decide(scheduleLeads),
	}) {
		if tool.Name() == builtin.RunScheduleTool {
			return &scheduleRig{runner: runner, tool: tool, o: o}
		}
	}
	t.Fatal("the operator catalogue serves no run_schedule with a runner wired")
	return nil
}

func (r *scheduleRig) run(t *testing.T, caller iam.Principal, scope, ref, name string) (tools.Result, map[string]any) {
	t.Helper()
	ctx := builtin.OutsideAnyTurn(iam.WithPrincipal(context.Background(), caller))
	res, err := r.tool.Call(ctx, map[string]any{"scope_type": scope, "scope": ref, "name": name})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if !res.Failed {
		if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
			t.Fatalf("answered %q, not JSON: %v", res.Output, err)
		}
	}
	return res, out
}

// A LEAD OF EVERY SEAT A SCHEDULE WAKES RUNS IT NOW, for each of them, in
// their name: the standup reaches both seats of the team, the ask names the
// person, and the answer lists each seat as fired.
func TestALeadOfEverySeatRunsTheScheduleNowForEachOfThem(t *testing.T) {
	t.Parallel()
	rig := newScheduleRig(t, "")
	res, out := rig.run(t, boundTo("jane"), "unit", "quality", "standup")
	if res.Failed || out["outcome"] != "applied" || out["changed"] != true || out["scope_id"] != "quality" {
		t.Fatalf("run = %q, want an applied change of the quality standup", res.Output)
	}
	runs, _ := out["runs"].([]any)
	if len(runs) != 2 {
		t.Fatalf("runs = %v, want one per seat the standup wakes", out["runs"])
	}
	for _, r := range runs {
		if row, _ := r.(map[string]any); row["status"] != string(schedule.ManualFired) {
			t.Errorf("run %v, want fired", row)
		}
	}
	if len(rig.runner.asks) != 1 || rig.runner.asks[0].RequestedBy != "jane" ||
		rig.runner.asks[0].Entry.Schedule.Name != "standup" {
		t.Fatalf("asks = %+v, want one standup asked for by jane", rig.runner.asks)
	}
	if got := rig.runner.pub.types(); len(got) != 4 {
		// Two task_assigned on the seats' inboxes, two scheduled_task_fired.
		t.Errorf("published %v, want a task and its record for each seat", got)
	}
}

// WAKING A SEAT IS ITS LEAD'S TO DECIDE, so the call is decided on EVERY seat
// the schedule wakes: a lead of one of them is refused the team's standup
// whole, and nothing is sent, while the schedule of the seat they lead is
// theirs to run.
//
// Mutation: ask the authority about the first runner alone, and omar's run of
// the standup goes out.
func TestAScheduleIsRunOnlyByWhoeverMayWakeEverySeatItWakes(t *testing.T) {
	t.Parallel()
	rig := newScheduleRig(t, "")
	res, _ := rig.run(t, boundTo("omar"), "unit", "quality", "standup")
	if !res.Failed || tools.RefusalOf(res) != tools.RefusalForbidden {
		t.Fatalf("omar's run of the standup = %+v, want forbidden", res)
	}
	if len(rig.runner.asks) != 0 {
		t.Fatalf("asks = %+v, want none sent for a refused call", rig.runner.asks)
	}
	if res, out := rig.run(t, boundTo("omar"), "role", "qa-lead", "triage"); res.Failed ||
		out["outcome"] != "applied" {
		t.Fatalf("omar's run of the triage of the seat he leads = %q, want applied", res.Output)
	}
}

// THE REFUSAL NEVER DEPENDS ON WHETHER THE SCHEDULE EXISTS: a caller who may
// not run a schedule there is refused the same whether one is declared or
// not, and only an administrator is told that none is.
func TestAScheduleNobodyDeclaresIsNamedOnlyToWhoeverCouldRunIt(t *testing.T) {
	t.Parallel()
	rig := newScheduleRig(t, "")
	if res, _ := rig.run(t, boundTo("jane"), "unit", "quality", "nightly"); tools.RefusalOf(res) != tools.RefusalForbidden {
		t.Fatalf("jane's run of an undeclared unit schedule = %+v, want forbidden", res)
	}
	admin := boundTo("ada", iam.GrantAdmin)
	if res, _ := rig.run(t, admin, "unit", "quality", "nightly"); tools.RefusalOf(res) != tools.RefusalNotFound {
		t.Fatalf("an administrator's run of it = %+v, want not_found", res)
	}
}

// A SCHEDULE SWITCHED OFF IS REFUSED IN THE ENGINE'S WORDS, and nothing is
// sent: running it would overrule the decision that switched it off.
func TestASwitchedOffScheduleIsRefusedInTheEnginesWords(t *testing.T) {
	t.Parallel()
	rig := newScheduleRig(t, "")
	res, _ := rig.run(t, boundTo("jane"), "unit", "quality", "retro")
	if tools.RefusalOf(res) != tools.RefusalConflict || !strings.Contains(res.Output, "switched off") {
		t.Fatalf("run of a switched-off schedule = %+v, want a conflict saying it is switched off", res)
	}
	if got := rig.runner.pub.types(); len(got) != 0 {
		t.Fatalf("published %v, want nothing", got)
	}
}

// A RUN EVERY SEAT OF WHICH IS PAUSED IS REFUSED, naming the resume; one with
// a paused seat among others runs for the rest and names the paused one.
func TestAPausedSeatIsNotWokenAndTheAnswerSaysSo(t *testing.T) {
	t.Parallel()
	rig := newScheduleRig(t, "")
	dev, _ := rig.o.AgentIDFor(rig.o.AgentSeatByHandle("qa-dev"))
	rig.runner.paused[dev] = true
	res, _ := rig.run(t, boundTo("jane"), "role", "qa-dev", "smoke")
	if tools.RefusalOf(res) != tools.RefusalConflict || !strings.Contains(res.Output, builtin.ResumeSeatTool) {
		t.Fatalf("run of a paused seat's schedule = %+v, want a conflict naming %s", res,
			builtin.ResumeSeatTool)
	}
	res, out := rig.run(t, boundTo("jane"), "unit", "quality", "standup")
	if res.Failed || out["outcome"] != "applied" {
		t.Fatalf("run of the standup with one seat paused = %q, want applied", res.Output)
	}
	statuses := map[string]any{}
	for _, r := range out["runs"].([]any) {
		row := r.(map[string]any)
		statuses[row["handle"].(string)] = row["status"]
	}
	if statuses["qa-lead"] != string(schedule.ManualFired) || statuses["qa-dev"] != string(schedule.ManualPaused) {
		t.Fatalf("statuses = %v, want the lead fired and the paused developer named", statuses)
	}
}

// THE SAME REQUEST SENT AGAIN IS THE SAME RUN: a request that names its
// operation claims the run it already made and sends nothing more, so a
// retry after a lost answer never runs the standup twice.
//
// Mutation: derive the ask from a fresh id on every call, and the retry
// publishes a second run.
func TestTheSameRequestSentAgainIsTheSameRun(t *testing.T) {
	t.Parallel()
	rig := newScheduleRig(t, "op-1")
	if res, out := rig.run(t, boundTo("jane"), "role", "qa-dev", "smoke"); res.Failed || out["changed"] != true {
		t.Fatalf("first run = %q, want a change", res.Output)
	}
	res, out := rig.run(t, boundTo("jane"), "role", "qa-dev", "smoke")
	if res.Failed || out["changed"] != false {
		t.Fatalf("the same request again = %q, want applied with nothing changed", res.Output)
	}
	if got := rig.runner.pub.types(); len(got) != 2 {
		t.Fatalf("published %v, want the one run's task and record", got)
	}
}

// AN AGENT RUNS NO SCHEDULE: when a seat's scheduled work runs is a person's
// to decide.
func TestAnAgentRunsNoSchedule(t *testing.T) {
	t.Parallel()
	rig := newScheduleRig(t, "")
	agent := iam.Principal{ID: uuid.New(), Kind: iam.KindSeat, Login: "qa-lead", Seat: "qa-lead",
		Stage: iam.StageActive}
	if res, _ := rig.run(t, agent, "role", "qa-dev", "smoke"); tools.RefusalOf(res) != tools.RefusalForbidden {
		t.Fatalf("an agent's run = %+v, want forbidden", res)
	}
	if len(rig.runner.asks) != 0 {
		t.Fatalf("asks = %+v, want none", rig.runner.asks)
	}
}
