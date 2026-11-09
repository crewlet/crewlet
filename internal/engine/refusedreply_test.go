package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A REFUSED REPLY IS KEPT AT THE HEAD OF THE INBOX AND RUNS NO TURN (ADR-0114).
//
// The coordinator found the delivery too large to be the answer its run's
// question waits on, recorded its refusal as that question's outcome, and
// resumes the run told so ([sandbox.AnswerRefused]). The run's row carries
// none of the reply, so this delivery is the only copy there is. It is handed
// back by deferral — at the HEAD, as an answer still owed is, never a Nak that
// returns it behind the person's next message — and recorded as worked
// nowhere: a turn that keeps the refusal spends it, and a refusal let go of
// leaves it to come round as the ordinary message it is.
//
// Mutation: spend it and acknowledge it as a consumed answer, and the ledger
// drops the very reply a let-go hands back; let it fall through to the
// ordinary route, and the seat answers a reply its run was told about.
func TestARefusedReplyIsKeptAtTheHeadAndRunsNoTurn(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Completions = ledgerstore.NewMemoryCompletions()
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, SandboxAwaitsAnswer: true}
	}
	d.Answer = func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
		return sandbox.AnswerRefused, nil
	}

	reply := inThread("notification", "chat:C1")
	got := d.Dispatch(t.Context(), "swe", []*events.Event{reply})

	if got.Outcome != queue.OutcomeDefer || got.Cause != queue.DeferAnswerOwed {
		t.Errorf("outcome = %v (%s), want a deferral for the run its refusal is owed to: the one "+
			"return that keeps the reply at the head", got.Outcome, got.Cause)
	}
	if !strings.Contains(got.Reason, "refusal") {
		t.Errorf("the deferral says %q, want the refusal it is kept for", got.Reason)
	}
	if len(r.reqs) != 0 {
		t.Errorf("a turn ran on a reply whose refusal its run was told of: %d", len(r.reqs))
	}
	if len(r.parked) != 0 {
		t.Errorf("the reply was republished as well as handed back (%v)", r.parked)
	}
	if d.AnswerWorked(t.Context(), "swe", reply) {
		t.Error("the kept delivery is recorded as worked, so a refusal let go of loses the reply")
	}
	if len(r.deferred) != 1 {
		t.Errorf("deferrals noted %v, want this one", r.deferred)
	}
}
