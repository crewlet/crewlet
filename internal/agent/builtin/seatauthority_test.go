package builtin_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// Who may act ON A SEAT — pause it, resume it, steer its turn, answer the
// question its coding run parked on — decided from the chart inside each tool,
// because only the tool has read the object. The cases vary the CALLER over
// one chart, and each refusal is asserted to name who may: a person told only
// "no" asks the wrong somebody next.

// ledDoc is a company with a real management line:
//
//	Jane Founder ─┬─ Omar Lead ── Agent CTO ── Agent SWE
//	              └─ Rui Santos
//	Agent CEO (reports to nobody)
//
// So Jane leads everybody below her, Omar leads the two agents, and Rui — a
// person in the company — leads nobody.
const ledDoc = `
name: Nimbus
providers:
  llm:
    p:
      type: anthropic
      model: m
      api_keys: ["${K}"]
roles:
  - name: Jane Founder
    handle: jane
    kind: human
    contact: {slack_user_id: U0JANE}
    manages: ["Omar Lead", "Rui Santos"]
  - name: Omar Lead
    handle: omar
    kind: human
    contact: {slack_user_id: U0OMAR}
    manages: ["Agent CTO"]
  - name: Rui Santos
    handle: rui
    kind: human
    contact: {slack_user_id: U0RUI}
  - name: Agent CTO
    handle: agent-cto
    llm: p
    manages: ["Agent SWE"]
  - name: Agent SWE
    handle: agent-swe
    llm: p
  - name: Agent CEO
    handle: agent-ceo
    llm: p
`

func ledChart(t *testing.T) *org.Organization {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(ledDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	o, err := cfg.Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	return o
}

// person is a key linked to a seat: a member's, or with admin an admin's.
func person(seat string, admin bool) func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
	return func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
		return builtin.Actor{Handle: seat + "-key", Kind: tracker.AuthorOperator,
			OperatorID: seat + "-key", Seat: seat, Admin: admin}, nil
	}
}

// unlinkedAdmin is an admin's key no seat links — a pipeline.
func unlinkedAdmin(context.Context, *turnctx.Turn) (builtin.Actor, error) {
	return builtin.Actor{Handle: "ops-bot", Kind: tracker.AuthorOperator,
		OperatorID: "ops-bot", Admin: true}, nil
}

// operatorTool is one tool of the operator catalogue over deps.
func operatorTool(t *testing.T, deps builtin.OperatorDeps, name string) tools.Callable {
	t.Helper()
	for _, tool := range builtin.OperatorTools(deps) {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("the operator catalogue serves no %s over these deps", name)
	return nil
}

func callTool(t *testing.T, tool tools.Callable, args map[string]any) tools.Result {
	t.Helper()
	res, err := tool.Call(t.Context(), args)
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	return res
}

// pauseAs is pause_seat and resume_seat as one caller over the led chart and a
// fresh pause record.
func pauseAs(t *testing.T, actor func(context.Context, *turnctx.Turn) (builtin.Actor, error)) (
	pause, resume tools.Callable) {

	t.Helper()
	o := ledChart(t)
	deps := builtin.OperatorDeps{Pauses: builtin.SeatPauseDeps{
		Pauses: coordmem.NewFleet(), Announce: &announced{},
		Org: func() *org.Organization { return o }, Actor: actor,
		Now: func() time.Time { return time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC) },
	}}
	return operatorTool(t, deps, builtin.PauseSeatTool), operatorTool(t, deps, builtin.ResumeSeatTool)
}

// A SEAT IS PAUSED BY THE PEOPLE IT REPORTS TO, AT ANY DEPTH, OR AN ADMIN.
//
// Whether a seat works is a decision about somebody else's work, and the chart
// already says whose: the seat's direct manager's, theirs, and so on up — so
// Omar, who manages the CTO, and Jane, who manages Omar, may both stop it. An
// admin runs the engine and may stop any agent, linked to a seat or not.
func TestASeatIsPausedByItsLeadsAndByAnAdmin(t *testing.T) {
	t.Parallel()
	for name, actor := range map[string]func(context.Context, *turnctx.Turn) (builtin.Actor, error){
		"its direct lead":               person("omar", false),
		"a lead two levels up":          person("jane", false),
		"an admin who leads nobody":     person("rui", true),
		"an admin no seat is linked to": unlinkedAdmin,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pause, resume := pauseAs(t, actor)
			if res := callTool(t, pause, map[string]any{"handle": "agent-cto"}); res.Failed {
				t.Fatalf("pause was refused: %s", res.Output)
			}
			if res := callTool(t, resume, map[string]any{"handle": "agent-cto"}); res.Failed {
				t.Fatalf("resume was refused: %s", res.Output)
			}
		})
	}
}

// A MEMBER WHO LEADS NOBODY ABOVE THE SEAT IS REFUSED, AND TOLD WHO MAY.
//
// Being a person in the company does not make every agent in it yours to stop.
// The refusal is `forbidden` and names the PEOPLE above the seat, nearest first
// — never the agent between them, which holds no tool that pauses — and an
// admin; a seat nobody reports to names an admin alone. Resume is the same
// authority, so a seat one person stopped is not stuck behind them.
func TestAMemberWhoLeadsNobodyAboveTheSeatIsRefusedAndToldWhoMay(t *testing.T) {
	t.Parallel()
	pause, resume := pauseAs(t, person("rui", false))
	for _, tc := range []struct {
		tool   tools.Callable
		handle string
		want   string
	}{
		{pause, "agent-cto", "Only Omar Lead, Jane Founder, or an admin can pause Agent CTO"},
		{pause, "agent-swe", "Only Omar Lead, Jane Founder, or an admin can pause Agent SWE"},
		{pause, "agent-ceo", "Only an admin can pause Agent CEO"},
		{resume, "agent-cto", "Only Omar Lead, Jane Founder, or an admin can resume Agent CTO"},
	} {
		res := callTool(t, tc.tool, map[string]any{"handle": tc.handle})
		if !res.Failed || res.Refusal != tools.RefusalForbidden {
			t.Errorf("%s %s by a member who leads nobody = %+v, want forbidden",
				tc.tool.Name(), tc.handle, res)
			continue
		}
		if !strings.HasPrefix(res.Output, tc.want) {
			t.Errorf("%s %s refused %q, want it to begin %q", tc.tool.Name(), tc.handle,
				res.Output, tc.want)
		}
	}
	// AND A LEAD OF ONE SEAT IS NOT A LEAD OF ANOTHER: Omar manages the CTO
	// and not the CEO, who reports to nobody.
	omar, _ := pauseAs(t, person("omar", false))
	if res := callTool(t, omar, map[string]any{"handle": "agent-ceo"}); res.Refusal != tools.RefusalForbidden {
		t.Errorf("a lead paused a seat outside their line: %+v", res)
	}
}

// itemsFake is the tracker read steer_turn makes: one task, or an error.
type itemsFake struct {
	task tracker.Task
	err  error
}

func (f itemsFake) Task(_ context.Context, idOrKey string, _ tracker.DetailWants,
	_ statelog.Freshness) (tracker.TaskDetail, error) {

	if f.err != nil {
		return tracker.TaskDetail{}, f.err
	}
	if idOrKey != f.task.ID {
		return tracker.TaskDetail{}, tracker.ErrNoTask
	}
	return tracker.TaskDetail{Task: f.task}, nil
}

// eng42 is the native item the running turn is charged to.
var eng42 = types.WorkItem{Backend: types.WorkNative, ID: "task-42", Key: "ENG-42", Project: "ENG"}

// steerAs is steer_turn as one caller, over the led chart, an item reader and
// a fleet in which agent-swe is running turn run-1 charged to item.
func steerAs(t *testing.T, actor func(context.Context, *turnctx.Turn) (builtin.Actor, error),
	items builtin.SteerItems, item *types.WorkItem) (tools.Callable, *fleetAsker) {

	t.Helper()
	o := ledChart(t)
	asker := &fleetAsker{
		described: []steer.Reply{{Version: 1, TurnID: "run-1", AgentHandle: "agent-swe",
			Status: steer.StatusRunning, WorkItem: item}},
		replies: []steer.Reply{{Version: 1, TurnID: "run-1", AgentHandle: "agent-swe",
			Status: steer.StatusAccepted}},
	}
	deps := builtin.OperatorDeps{Steer: builtin.SteerDeps{
		Asker: asker, Actor: actor, Items: items,
		Org: func() *org.Organization { return o },
	}}
	return operatorTool(t, deps, builtin.SteerTurnTool), asker
}

// offered is the notes the fleet was offered, which a refused steer never is.
func (a *fleetAsker) offered() []steer.Request {
	var out []steer.Request
	for _, req := range a.asked {
		if !req.Describe {
			out = append(out, req)
		}
	}
	return out
}

// A TURN IS STEERED BY THE PEOPLE ITS WORK ANSWERS TO.
//
// The seat's leads, at any depth; the people on the item it is charged to —
// who owns it, who filed it (under either of their names: a task a person
// filed through their own key names the key as reporter), who follows it; and
// an admin. Each is admitted and its note offered.
func TestATurnIsSteeredByThePeopleItsWorkAnswersTo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		actor func(context.Context, *turnctx.Turn) (builtin.Actor, error)
		task  tracker.Task
	}{
		{"the seat's lead", person("omar", false), tracker.Task{ID: "task-42"}},
		{"a lead two levels up", person("jane", false), tracker.Task{ID: "task-42"}},
		{"the item's owner", person("rui", false), tracker.Task{ID: "task-42", Assignee: "rui"}},
		{"who filed it, under their key", person("rui", false),
			tracker.Task{ID: "task-42", Reporter: "rui-key"}},
		{"who follows it", person("rui", false),
			tracker.Task{ID: "task-42", Watchers: []string{"rui"}}},
		{"an admin who is none of these", person("rui", true), tracker.Task{ID: "task-42"}},
		{"an admin no seat is linked to", unlinkedAdmin, tracker.Task{ID: "task-42"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			item := eng42
			tool, asker := steerAs(t, tc.actor, itemsFake{task: tc.task}, &item)
			res, out := steerCall(t, tool, "run-1", "use staging")
			if res.Failed {
				t.Fatalf("refused: %s", res.Output)
			}
			if out["outcome"] != "pending" || len(asker.offered()) != 1 {
				t.Errorf("answered %v after offering %d notes, want one pending note",
					out, len(asker.offered()))
			}
		})
	}
}

// ANYBODY ELSE IS REFUSED BEFORE ANYTHING IS SENT, AND TOLD WHO MAY.
//
// A member on nothing the turn is doing — not above its seat, not on its item,
// or on it only as somebody who muted it — is refused `forbidden`, the note is
// never offered, and the refusal names the seat's leads, the item's people and
// an admin. A turn charged to an item this surface cannot read (another
// tracker's) names only the leads and an admin: there is nobody else it could
// check.
func TestAMemberNotOnTheWorkIsRefusedBeforeAnythingIsSent(t *testing.T) {
	t.Parallel()
	item := eng42
	jira := types.WorkItem{Backend: "jira", ID: "10042", Key: "OPS-42", Project: "OPS"}
	for _, tc := range []struct {
		name  string
		task  tracker.Task
		item  *types.WorkItem
		items builtin.SteerItems
		want  string
	}{
		{"a member on nothing", tracker.Task{ID: "task-42", Assignee: "jane"}, &item, nil,
			"Only Omar Lead, Jane Founder, whoever owns, filed or follows ENG-42, " +
				"or an admin can steer Agent SWE's turn"},
		{"a member who muted it", tracker.Task{ID: "task-42", Watchers: []string{"rui"},
			Muted: []string{"rui"}}, &item, nil, "whoever owns, filed or follows ENG-42"},
		{"a turn on nothing", tracker.Task{}, nil, nil,
			"Only Omar Lead, Jane Founder, or an admin can steer Agent SWE's turn"},
		{"a turn on another tracker's item", tracker.Task{}, &jira, nil,
			"Only Omar Lead, Jane Founder, or an admin can steer Agent SWE's turn"},
		{"an item the tracker no longer has", tracker.Task{ID: "other"}, &item, nil,
			"whoever owns, filed or follows ENG-42"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			items := tc.items
			if items == nil {
				items = itemsFake{task: tc.task}
			}
			tool, asker := steerAs(t, person("rui", false), items, tc.item)
			res, _ := steerCall(t, tool, "run-1", "use staging")
			if !res.Failed || res.Refusal != tools.RefusalForbidden {
				t.Fatalf("a member not on the work = %+v, want forbidden", res)
			}
			if !strings.Contains(res.Output, tc.want) {
				t.Errorf("refused %q, want it to name %q", res.Output, tc.want)
			}
			if offered := asker.offered(); len(offered) != 0 {
				t.Errorf("a refused steer offered %+v", offered)
			}
		})
	}
}

// WHAT COULD NOT BE CHECKED IS NOT A REFUSAL, AND NOTHING IS SENT.
//
// No node describing the turn, or a tracker that could not be read, leaves
// whose work it is unknown: the answer is `unavailable` — try again — and the
// note is never offered, because a note handed over on a guess is the steer the
// rule exists to refuse. A turn the describe says has ended is `not_running`,
// before anybody's authority is weighed.
func TestASteerWhoseAuthorityCouldNotBeCheckedSendsNothing(t *testing.T) {
	t.Parallel()
	item := eng42
	tool, asker := steerAs(t, person("rui", false),
		itemsFake{err: context.DeadlineExceeded}, &item)
	if res, _ := steerCall(t, tool, "run-1", "use staging"); res.Refusal != tools.RefusalUnavailable {
		t.Errorf("an unreadable item = %+v, want unavailable", res)
	}
	if offered := asker.offered(); len(offered) != 0 {
		t.Errorf("a steer whose item could not be read offered %+v", offered)
	}

	silent, asker := steerAs(t, person("rui", false), itemsFake{}, &item)
	asker.described = nil
	if res, _ := steerCall(t, silent, "run-1", "use staging"); res.Refusal != tools.RefusalUnavailable {
		t.Errorf("a turn nobody described = %+v, want unavailable", res)
	}
	if offered := asker.offered(); len(offered) != 0 {
		t.Errorf("a steer nobody described offered %+v", offered)
	}

	ended, asker := steerAs(t, person("rui", false), itemsFake{}, &item)
	asker.described = []steer.Reply{{TurnID: "run-1", AgentHandle: "agent-swe",
		Status: steer.StatusClosed}}
	if res, _ := steerCall(t, ended, "run-1", "use staging"); res.Refusal != tools.RefusalNotRunning {
		t.Errorf("an ended turn = %+v, want not_running", res)
	}
}

// AN ADMIN'S NOTE IS NOT HELD UP BY A DESCRIBE: nothing the turn could say
// would refuse it, so it is offered straight away.
func TestAnAdminsNoteSkipsTheDescribe(t *testing.T) {
	t.Parallel()
	item := eng42
	tool, asker := steerAs(t, person("rui", true), itemsFake{}, &item)
	if res, _ := steerCall(t, tool, "run-1", "use staging"); res.Failed {
		t.Fatalf("refused: %s", res.Output)
	}
	for _, req := range asker.asked {
		if req.Describe {
			t.Errorf("an admin's steer described the turn first: %+v", asker.asked)
		}
	}
}

// answerAs is answer_run as one caller, over a run parked on a question put to
// audience.
func answerAs(t *testing.T, actor func(context.Context, *turnctx.Turn) (builtin.Actor, error),
	audience ...string) (tools.Callable, *deskFake) {

	t.Helper()
	o := ledChart(t)
	run := parked("t1", sandbox.StatusAwaiting)
	run.AgentHandle = "agent-swe"
	run.AudienceHandles = audience
	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": run}}
	return operatorTool(t, builtin.OperatorDeps{Runs: builtin.RunDeps{
		Desk: desk, Actor: actor, Org: func() *org.Organization { return o },
	}}, builtin.AnswerRunTool), desk
}

// A PARKED QUESTION IS ANSWERED BY WHOEVER IT WAS PUT TO, OR AN ADMIN.
//
// The park resolved who the run asked — its requester, a manager, a team — and
// that person answers it, under either of their names. Anybody else, a lead of
// the seat included, is refused `forbidden` naming who it was put to, and
// nothing is delivered: an answer from a stranger to the work would resume the
// run on a guess.
func TestAParkedQuestionIsAnsweredByWhoeverItWasPutTo(t *testing.T) {
	t.Parallel()
	args := map[string]any{"turn_id": "t1", "answer": "keep the old API"}
	for name, actor := range map[string]func(context.Context, *turnctx.Turn) (builtin.Actor, error){
		"the person it was put to":      person("rui", false),
		"an admin it was not put to":    person("omar", true),
		"an admin no seat is linked to": unlinkedAdmin,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tool, desk := answerAs(t, actor, "rui")
			if res := callTool(t, tool, args); res.Failed {
				t.Fatalf("refused: %s", res.Output)
			}
			if len(desk.delivered) != 1 {
				t.Errorf("delivered %d answers, want one", len(desk.delivered))
			}
		})
	}

	// UNDER EITHER NAME: a question put to a person's key — the name their
	// own rows carry — is theirs too.
	byKey, desk := answerAs(t, person("rui", false), "rui-key")
	if res := callTool(t, byKey, args); res.Failed || len(desk.delivered) != 1 {
		t.Errorf("a question put to rui's key was refused rui: %+v", res)
	}

	for name, tc := range map[string]struct {
		actor    func(context.Context, *turnctx.Turn) (builtin.Actor, error)
		audience []string
		want     string
	}{
		"a member it was not put to": {person("jane", false), []string{"rui"},
			"Only Rui Santos or an admin can answer the question Agent SWE's coding run is waiting on"},
		"a question put to nobody the chart has": {person("jane", false), nil,
			"Only an admin can answer"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tool, desk := answerAs(t, tc.actor, tc.audience...)
			res := callTool(t, tool, args)
			if !res.Failed || res.Refusal != tools.RefusalForbidden {
				t.Fatalf("answered %+v, want forbidden", res)
			}
			if !strings.HasPrefix(res.Output, tc.want) {
				t.Errorf("refused %q, want it to begin %q", res.Output, tc.want)
			}
			if len(desk.delivered) != 0 {
				t.Errorf("a refused answer was delivered: %+v", desk.delivered)
			}
		})
	}
}
