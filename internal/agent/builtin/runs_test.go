package builtin_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/tools"
)

// sweepRuns is the runs around the sweep's run: one of the same requester
// whose title carries the sweep's words, and one of another requester's.
type sweepRuns struct{}

func (sweepRuns) Around(context.Context, string) ([]prompts.RunAround, error) {
	return []prompts.RunAround{
		{Turn: "0190f3a8-0000-7000-8000-0000000000b1", Seat: "agent-cto", OwnSeat: true, Stage: "running",
			Item: "ENG-1", SameRequester: true, Conversation: "chatlet", Branch: "logout",
			Started: time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC), Title: sweepWords + " add the logout button",
			Label: provenance.ExecutorSession("agent-cto"), TakesMessages: true},
		{Turn: "0190f3a8-0000-7000-8000-0000000000b2", Seat: "agent-cto", OwnSeat: true, Stage: "running",
			Item: "ENG-2"},
	}, nil
}

func (sweepRuns) Message(context.Context, string, string, string, string) error { return nil }

func (sweepRuns) Wait(context.Context, string, string, string, string, string) error { return nil }

// runsDesk is a desk recording the last message and wait, refusing as told.
type runsDesk struct {
	sweepRuns
	refuse  error
	message []string
	wait    []string
}

func (d *runsDesk) Message(_ context.Context, turnID, job, target, text string) error {
	d.message = []string{turnID, job, target, text}
	return d.refuse
}

func (d *runsDesk) Wait(_ context.Context, turnID, job, target, reason, branch string) error {
	d.wait = []string{turnID, job, target, reason, branch}
	return d.refuse
}

// LIST_RUNS HANDS A TITLE OVER IN THE FENCE OF WHAT ITS TURN WAS ASKED. A run of
// the same requester is answered with its title inside the label the engine
// gave it (what another run's turn was asked, at external trust, naming its
// seat) and an envelope saying so; another requester's run carries none.
//
// Mutation: answer the title bare (unfenced-title), and this goes red.
func TestListRunsHandsATitleOverInTheFenceOfWhatItsTurnWasAsked(t *testing.T) {
	t.Parallel()
	list := builtin.NewRunTools(sweepRuns{})[0]
	b := provenance.NewBoundary()
	got, err := list.CallForTurn(t.Context(), &turnctx.Turn{RunID: "0190f3a8-0000-7000-8000-0000000000a1",
		Boundary: b}, nil)
	if err != nil || got.Failed {
		t.Fatalf("list_runs = %+v, %v", got, err)
	}
	fences := provenance.Fences(got.Output, b)
	if len(fences) != 1 || !strings.Contains(fences[0], sweepWords) ||
		!strings.Contains(fences[0], "source=executor_session") || !strings.Contains(fences[0], "trust=external") {
		t.Errorf("the title is not in one fence of what its turn was asked: %q\n%s", fences, got.Output)
	}
	var answer struct {
		Runs []struct {
			Title         string `json:"title"`
			SameRequester bool   `json:"same_requester"`
		} `json:"runs"`
		Provenance json.RawMessage `json:"provenance"`
	}
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil || len(answer.Runs) != 2 ||
		answer.Runs[1].Title != "" || len(answer.Provenance) == 0 {
		t.Errorf("list_runs answered %s (%v)", got.Output, err)
	}
}

// THE RUN TOOLS ACT ONLY FOR AN EXECUTOR'S RUN AND HOLD THEIR ARGUMENTS. A call
// naming no run's job is refused as unavailable before the engine is asked; a
// message past its bound is refused, never cut, and a wait needs its reason;
// the engine's refusal is said as it words it, under its class.
//
// Mutation: cut a long message to fit (cut-message), and this goes red.
func TestTheRunToolsActOnlyForAnExecutorsRunAndHoldTheirArguments(t *testing.T) {
	t.Parallel()
	desk := &runsDesk{}
	runTools := builtin.NewRunTools(desk)
	message, wait := runTools[1], runTools[2]
	turn := &turnctx.Turn{RunID: "0190f3a8-0000-7000-8000-0000000000a1"}
	job := turnctx.WithJob(t.Context(), "job-1")
	target := "0190f3a8-0000-7000-8000-0000000000b1"
	if got, _ := message.CallForTurn(t.Context(), turn, map[string]any{"turn_id": target, "text": "hi"}); tools.RefusalOf(got) != tools.RefusalUnavailable {
		t.Errorf("a message with no job answered %+v", got)
	}
	long := strings.Repeat("a", steer.MaxWakeRunes+1)
	if got, _ := message.CallForTurn(job, turn, map[string]any{"turn_id": target, "text": long}); !got.Failed || desk.message != nil {
		t.Errorf("a message past its bound answered %+v, and reached the engine: %v", got, desk.message != nil)
	}
	if got, _ := message.CallForTurn(job, turn, map[string]any{"turn_id": target, "text": "I pushed it"}); got.Failed ||
		desk.message[2] != target || desk.message[3] != "I pushed it" {
		t.Errorf("a message answered %+v, filed %v", got, desk.message)
	}
	if got, _ := wait.CallForTurn(job, turn, map[string]any{"turn_id": target}); !got.Failed || desk.wait != nil {
		t.Errorf("a wait with no reason answered %+v", got)
	}
	desk.refuse = &builtin.RunRefused{Reason: builtin.RunRefusedRequester, Said: "it works for another requester"}
	got, _ := wait.CallForTurn(job, turn, map[string]any{"turn_id": target, "reason": "its branch first"})
	if tools.RefusalOf(got) != tools.RefusalForbidden || got.Output != "it works for another requester" {
		t.Errorf("the engine's refusal is said as %+v", got)
	}
}
