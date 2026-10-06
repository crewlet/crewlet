package engine_test

import (
	"context"
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
