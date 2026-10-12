package engine_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerfit"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/engine"
)

// sessionSpend is a dispatcher's rewriter and its spend flush, recording the
// order they were reached in.
type sessionSpend struct {
	mu    sync.Mutex
	steps []string
}

func (s *sessionSpend) note(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, step)
}

func (s *sessionSpend) Fit(_ context.Context, _ compact.Kind, _ string, _ int) (compact.Result, error) {
	s.note("rewrite")
	return compact.Result{Text: "a shorter account", Compacted: true}, nil
}

// A CONVERSATION ENTRY'S REWRITES ARE PUBLISHED AS THEY ARE MADE.
//
// The entry is written after the turn's end, beside its reflection pass, and
// a payload past the ledger's budget is rewritten by the seat's auxiliary
// model there: that spend's record used to wait for the ledger's next
// interval, up to fifteen seconds behind the pass it sits beside. So once the
// rewrites are made, the run's records are flushed.
func TestAConversationEntrysRewritesAreFlushedOnceMade(t *testing.T) {
	t.Parallel()
	spend := &sessionSpend{}
	var flushed []string
	d := &engine.Dispatcher{
		Conversations: ledgerstore.NewMemoryConversations(),
		Rewriter: func(string, auxspend.Use) ledgerfit.Fitter {
			return spend
		},
		FlushSpend: func(_ context.Context, runID string) {
			spend.note("flush")
			flushed = append(flushed, runID)
		},
	}
	d.RecordSession(t.Context(), "ceo", "slack:C1", "run-7", "wk-7", "a message",
		turn.Result{Decision: phase.Done, LastWork: &turn.Work{
			Summary: "posted the report",
			Calls: []ledger.Call{{Name: "slack_post", Args: map[string]any{
				"text": strings.Repeat("a long report body ", 2000),
			}}},
		}}, clock)

	spend.mu.Lock()
	defer spend.mu.Unlock()
	if len(spend.steps) < 2 || spend.steps[0] != "rewrite" ||
		spend.steps[len(spend.steps)-1] != "flush" {
		t.Fatalf("steps = %v, want the rewrite and then one flush", spend.steps)
	}
	if len(flushed) != 1 || flushed[0] != "run-7" {
		t.Fatalf("flushed %v, want the run's own records once", flushed)
	}
}

// longReport is a turn whose last round posted a payload past the ledger's
// budget, so its conversation entry has a piece to rewrite.
func longReport() turn.Result {
	return turn.Result{Decision: phase.Done, LastWork: &turn.Work{
		Summary: "posted the report",
		Calls: []ledger.Call{{Name: "slack_post", Args: map[string]any{
			"text": strings.Repeat("a long report body ", 2000),
		}}},
	}}
}

// A CONVERSATION ENTRY'S REWRITES WAIT FOR ROOM, AS THE REFLECTION PASS DOES.
//
// They are filed under the reflection stage — what the seat remembers of a
// turn once it is over — and ran with no gate at all, after every turn: on a
// turn the budget ended, whose refused round left the counter past the
// ceiling, they were more spend past it, for a row the seat reads only once
// the window has turned over. With no room the entry is still written, and its
// long payload is named by size and digest rather than rewritten.
//
// Mutation: drop the gate from RecordSession, and the rewrite is made and
// flushed on a seat with no room left.
func TestAConversationEntryIsNotRewrittenWithNoRoomLeft(t *testing.T) {
	t.Parallel()
	spend := &sessionSpend{}
	conversations := ledgerstore.NewMemoryConversations()
	var asked []string
	d := &engine.Dispatcher{
		Conversations: conversations,
		Rewriter:      func(string, auxspend.Use) ledgerfit.Fitter { return spend },
		ReflectionRoom: func(_ context.Context, handle string) (bool, error) {
			asked = append(asked, handle)
			return false, nil
		},
		FlushSpend: func(context.Context, string) { spend.note("flush") },
	}
	res := longReport()
	d.RecordSession(t.Context(), "ceo", "slack:C1", "run-7", "wk-7", "a message", res, clock)

	if len(asked) != 1 || asked[0] != "ceo" {
		t.Fatalf("the gate was asked for %v, want the seat once", asked)
	}
	spend.mu.Lock()
	steps := spend.steps
	spend.mu.Unlock()
	if len(steps) != 0 {
		t.Fatalf("steps = %v, want no rewrite and nothing to flush on a seat with no room", steps)
	}
	entries, err := conversations.History(t.Context(), "ceo", "slack:C1", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("History = %v, %v, want the entry written anyway", entries, err)
	}
	omitted := compact.Omitted(res.LastWork.Calls[0].Args["text"].(string))
	if !strings.Contains(entries[0].Calls, omitted) || strings.Contains(entries[0].Calls, "a long report body") {
		t.Fatalf("the entry's call reads %q, want the payload named as %q", entries[0].Calls, omitted)
	}
}

// AN ENTRY WITH NOTHING TO REWRITE ASKS NOTHING, and a counter that cannot be
// read is not "no room": the reflection pass's own reading, since the charge
// on the way out still counts what the rewrite costs.
func TestAConversationEntryAsksForRoomOnlyWhenItHasSomethingToRewrite(t *testing.T) {
	t.Parallel()
	spend := &sessionSpend{}
	asked := 0
	d := &engine.Dispatcher{
		Conversations: ledgerstore.NewMemoryConversations(),
		Rewriter:      func(string, auxspend.Use) ledgerfit.Fitter { return spend },
		ReflectionRoom: func(context.Context, string) (bool, error) {
			asked++
			return true, errors.New("the coordination store did not answer")
		},
		FlushSpend: func(context.Context, string) { spend.note("flush") },
	}
	d.RecordSession(t.Context(), "ceo", "slack:C1", "run-6", "wk-6", "a message",
		turn.Result{Decision: phase.Done, LastWork: &turn.Work{Summary: "said hello",
			Calls: []ledger.Call{{Name: "slack_post", Args: map[string]any{"text": "hello"}}}}}, clock)
	if asked != 0 {
		t.Fatalf("an entry with no long payload asked the gate %d times", asked)
	}
	d.RecordSession(t.Context(), "ceo", "slack:C1", "run-7", "wk-7", "a message", longReport(), clock)
	spend.mu.Lock()
	defer spend.mu.Unlock()
	if asked != 1 || len(spend.steps) < 2 || spend.steps[0] != "rewrite" {
		t.Fatalf("asked %d, steps %v; want one question, then the rewrite made anyway", asked, spend.steps)
	}
}
