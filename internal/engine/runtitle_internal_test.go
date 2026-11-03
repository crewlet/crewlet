package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN EXECUTOR IS LISTED UNDER WHAT ITS TURN WAS ASKED, IN A PERSON'S WORDS.
//
// A wake reaches swe, and the turn launches its executor's run. The run's
// brief carries the turn's task, whatever somebody else wrote in it fenced
// under the turn's token for the model that reads it; the run's description
// and label, which Live › Executors, Now running and Home draw as its title
// and executor_launched announces, carry what the turn was asked, as it was
// written, with no fence line, token or escape: a fence is a model's
// (ADR-0040). So for every surface a seat is woken from ([wakeSurfaces]) and
// every other kind of wake that runs a turn ([wakeTypes]). A chat message that
// says nothing (an attachment alone) asks nothing, and its run is listed under
// the line a feed shows the message by, never under nothing. A run a resumed
// segment launches (a reviewer's send-back, or a person's answer carried on)
// is listed as its turn's first run is, off that run's row: the resumed
// segment re-reads no trigger.
//
// Mutation: write the row from the turn's Task, as the launch did, and every
// case whose trigger fences somebody's words is red; list a turn that was
// asked nothing under its ask alone, or a resumed segment's run under the
// parked ask alone, and the silent message's case is.
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

	const (
		said   = "can you look at the deploy?"
		silent = "a chat message that says nothing"
	)
	wakes := map[string]func(*testing.T) *events.Event{
		silent: func(t *testing.T) *events.Event {
			return deliveredTo(t, o, reg, "swe", chatMessageSaying(t, "ada", ""))
		},
	}
	for source, s := range wakeSurfaces() {
		wakes[source] = func(t *testing.T) *events.Event { return s.wake(t, o, reg, said) }
	}
	for kind, w := range wakeTypes(o, reg, said) {
		if kind == (types.ExternalNotification{}).EventType() {
			// Every surface's, above.
			continue
		}
		wakes[kind] = w.wake
	}
	for name, wake := range wakes {
		t.Run(name, func(t *testing.T) {
			ev := wake(t)
			dispatch, err := e.describeTurn(t.Context(), c, Request{Handle: "swe",
				Events: []*events.Event{ev}, RunID: statelog.NewOpID(time.Now(), "")})
			if err != nil {
				t.Fatalf("describe the turn: %v", err)
			}
			want, whole := said, false
			if name == silent {
				want, whole = dispatch.trigger.Summary, true
				if dispatch.ask != "" || want == "" {
					t.Fatalf("the turn was asked %q under a trigger shown as %q, so the "+
						"case asserts nothing about a turn asked nothing", dispatch.ask, want)
				}
			}
			tokens := []provenance.Boundary{dispatch.boundary}
			if n, ok := events.DataAs[*types.ExternalNotification](ev); ok && n != nil &&
				n.FenceBoundary != "" {
				tokens = append(tokens, provenance.Boundary(n.FenceBoundary))
			}
			dispatched := dispatch.runnerTurn(c, 0, nil, dispatch.task, turn.ToolReply(""),
				nil).Context
			first := listedRun(t, e, c, swe, dispatched)
			listedAs(t, "the turn's first run", first, want, whole, tokens...)

			// THE RESUMED SEGMENT, parked on the row its first run wrote,
			// which a run it launches is a new row of (its first run's
			// having ended).
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
				t.Fatalf("describe the resumed segment: %v", err)
			}
			resumed := resume.runnerTurn(c, 0, nil, resumeTask(parked), turn.ToolReply(""),
				nil).Context
			listedAs(t, "a run a resumed segment launches", listedRun(t, e, c, swe, resumed),
				want, whole, append(tokens, resume.boundary)...)
		})
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

// listedAs asserts the run is described by want (the whole description where
// whole, else words it holds) and labelled by the description's first line,
// with nothing a fence wrote for a model: no token of the turn's or the
// wake's, which every fence line carries, and no escape.
func listedAs(t *testing.T, name string, row sandbox.PendingRun, want string, whole bool,
	tokens ...provenance.Boundary) {

	t.Helper()
	described := strings.Contains(row.TaskDescription, want)
	if whole {
		described = row.TaskDescription == want
	}
	if !described {
		t.Errorf("%s: the run's description is %q, want what the turn was asked, %q",
			name, row.TaskDescription, want)
	}
	if line, _, _ := strings.Cut(row.TaskDescription, "\n"); row.Label != line {
		t.Errorf("%s: the run's label is %q, want its description's first line, %q",
			name, row.Label, line)
	}
	model := []string{provenance.TokenRemoved, "[escaped "}
	for _, token := range tokens {
		model = append(model, string(token))
	}
	for field, text := range map[string]string{
		"description": row.TaskDescription, "label": row.Label,
	} {
		for _, wrote := range model {
			if strings.Contains(text, wrote) {
				t.Errorf("%s: the run's %s carries %q, a fence's token or escape, "+
					"which is a model's:\n%s", name, field, wrote, text)
			}
		}
	}
}
