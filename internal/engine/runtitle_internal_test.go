package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CODING RUN IS LISTED UNDER WHAT ITS TURN WAS ASKED, IN A PERSON'S WORDS.
//
// A Chatlet message wakes swe, and the turn launches its executor's run. The
// run's brief carries the turn's task, the message fenced under the turn's
// token for the model that reads it; the run's description and label, which
// Live › Coding runs, Now running and Home draw as its title and
// sandbox_run_started announces, carry what the turn was asked, the message as
// it was typed, with no fence line, token or escape: a fence is a model's
// (ADR-0040). A message that says nothing (an attachment alone) asks nothing,
// and its run is listed under the line a feed shows the message by, never
// under nothing. A run a resumed segment launches (a reviewer's send-back, or
// a person's answer carried on) is listed as its turn's first run is, off that
// run's row: the resumed segment re-reads no trigger.
//
// Mutation: write the row from the turn's Task, as the launch did, and every
// case is red; list a turn that was asked nothing under its ask alone, or a
// resumed segment's run under the parked ask alone, and the silent message's
// cases are.
func TestACodingRunIsListedUnderWhatItsTurnWasAskedWithNoFence(t *testing.T) {
	t.Parallel()
	c, seat := executorCompany(t)
	o := firstPartyChart(t)
	swe := o.AgentSeatByHandle("swe")
	swe.LLM = seat.LLM
	c.Org = o
	e := launchReadyEngine(t, c)
	reg := partyRegistry(o, nil)
	e.notify.registry = reg

	const typed = "can you look at the deploy?"
	for name, said := range map[string]string{
		"a message":                   typed,
		"a message that says nothing": "",
	} {
		wake := deliveredTo(t, o, reg, "swe", chatMessageSaying(t, "ada", said))
		dispatch, err := e.describeTurn(t.Context(), c, Request{Handle: "swe",
			Events: []*events.Event{wake}, RunID: statelog.NewOpID(time.Now(), "")})
		if err != nil {
			t.Fatalf("%s: describe the turn: %v", name, err)
		}
		want := typed
		if said == "" {
			want = dispatch.trigger.Summary
			if dispatch.ask != "" || want == "" {
				t.Fatalf("%s: the turn was asked %q under a trigger shown as %q, so the "+
					"case asserts nothing about a turn asked nothing", name, dispatch.ask, want)
			}
		}
		dispatched := dispatch.runnerTurn(c, 0, nil, dispatch.task, turn.ToolReply(""), nil).Context
		if said != "" && len(provenance.Fences(dispatched.Task, dispatch.boundary)) != 1 {
			t.Fatalf("%s: the turn's task does not carry the message fenced, so the case "+
				"asserts nothing about a brief kept off the row:\n%s", name, dispatched.Task)
		}
		first := listedRun(t, e, c, swe, dispatched)
		listedAs(t, name+", the turn's first run", first, want,
			dispatch.boundary)

		// THE RESUMED SEGMENT, parked on the row its first run wrote, which a
		// run it launches is a new row of (its first run's having ended).
		parkedRun := first
		parkedRun.TurnID = statelog.NewOpID(time.Now(), "")
		parked := resumeInput{
			Company: c,
			Run:     parkedRun,
			State:   execstate.State{Task: dispatched.Task, Ask: dispatch.ask},
			Turn:    &turnctx.Turn{Seat: swe, Org: o},
		}
		resume, err := e.describeResume(t.Context(), c, parked)
		if err != nil {
			t.Fatalf("%s: describe the resumed segment: %v", name, err)
		}
		resumed := resume.runnerTurn(c, 0, nil, resumeTask(parked), turn.ToolReply(""), nil).Context
		listedAs(t, name+", a run a resumed segment launches",
			listedRun(t, e, c, swe, resumed), want, dispatch.boundary, resume.boundary)
	}
}

// listedRun launches ctxTurn's executor run on swe's seat and answers the row
// it wrote.
func listedRun(t *testing.T, e *Engine, c *Company, swe *org.Role,
	ctxTurn *turnctx.Turn) sandbox.PendingRun {

	t.Helper()
	ctxTurn.Seat, ctxTurn.Org = swe, c.Org
	launcher := &agentLauncher{engine: e, company: c, seat: swe, turn: ctxTurn}
	req := launchRequest(launcher, nil)
	req.Brief = "## Task\n" + ctxTurn.Task
	if _, err := launcher.LaunchExecutor(t.Context(), req); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	row, found, err := e.sandbox.Load().pending.Get(t.Context(), ctxTurn.RunID)
	if err != nil || !found {
		t.Fatalf("no row for the launch (found %v, err %v)", found, err)
	}
	return row
}

// listedAs asserts the run is described and labelled as want, with nothing a
// fence wrote for a model: no fence line, no token of the turn's, no escape.
func listedAs(t *testing.T, name string, row sandbox.PendingRun, want string,
	tokens ...provenance.Boundary) {

	t.Helper()
	for field, text := range map[string]string{
		"description": row.TaskDescription, "label": row.Label,
	} {
		if text != want {
			t.Errorf("%s: the run's %s is %q, want %q", name, field, text, want)
		}
		model := []string{"untrusted:", provenance.TokenRemoved, "[escaped "}
		for _, token := range tokens {
			model = append(model, string(token))
		}
		for _, wrote := range model {
			if strings.Contains(text, wrote) {
				t.Errorf("%s: the run's %s carries %q, a fence's line, token or "+
					"escape, which is a model's:\n%s", name, field, wrote, text)
			}
		}
	}
}
