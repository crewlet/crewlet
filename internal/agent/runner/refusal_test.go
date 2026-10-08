package runner_test

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// refusing is a scripted provider whose phases named in `refuse` decline,
// the way a backend reports it: a classified error carrying the refusal and
// the billed completion it arrived on.
type refusing struct {
	*scriptedProvider
	refuse string
	calls  int
}

func (p *refusing) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	if which, _ := p.scriptFor(req); which == p.refuse {
		p.calls++
		return nil, llm.Refused("anthropic", "scripted", &llm.Refusal{
			Category: "cyber", Explanation: "exploit development",
			Completion: &llm.Completion{Model: "scripted", InputTokens: 40, OutputTokens: 2,
				StopReason: llm.StopRefusal},
		})
	}
	return p.scriptedProvider.Complete(ctx, req)
}

// A REFUSED EXECUTOR IS A NAMED OUTCOME, NOT A RESCUE. The rescue path writes
// `incomplete` for an executor that gave no account of itself and hands the
// turn to the reviewer, which would send it round again — re-asking a model
// that declined. A refusal instead fails the phase with the provider's own
// classification, and the record says the model declined and what it said.
func TestARefusedExecutorIsANamedOutcomeNotARescue(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &refusing{scriptedProvider: &scriptedProvider{}, refuse: "execute"}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{pub: pub})

	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if llm.KindOf(err) != llm.KindRefusal {
		t.Fatalf("Execute = %+v, %v — want the refusal, not a rescued Work", w, err)
	}
	if w.Rescued || w.Outcome != "" {
		t.Fatalf("work = %+v, want no engine-written outcome", w)
	}
	if prov.calls != 1 {
		t.Fatalf("the executor was asked %d times — a refusal is not re-prompted", prov.calls)
	}
	done := completedPhase(t, pub, "execute")
	if !done.Failed || done.RescueFired || done.ErrorKind != "refusal" {
		t.Fatalf("record failed=%v rescue=%v kind=%q, want a failed refusal and no rescue",
			done.Failed, done.RescueFired, done.ErrorKind)
	}
	if done.Refusal == nil || done.Refusal.Category != "cyber" ||
		done.Refusal.Explanation != "exploit development" {
		t.Fatalf("refusal = %+v, want what the vendor said", done.Refusal)
	}
	if len(done.Rounds) != 1 || done.Rounds[0].StopReason != "refusal" || done.InputTokens != 40 {
		t.Fatalf("rounds = %+v tokens %d, want the billed refused round on the record",
			done.Rounds, done.InputTokens)
	}
}

// A REFUSED REVIEWER DOES NOT SEND THE TURN ROUND AGAIN. Its rescue is
// `self_iterate` — the right answer for a reviewer that never decided, and the
// wrong one for a reviewer that declined: the next round would put the same
// work in front of the same model.
func TestARefusedReviewerDoesNotSelfIterate(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &refusing{scriptedProvider: &scriptedProvider{}, refuse: "review"}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{pub: pub})

	rev, err := r.Review(context.Background(), 1, workFor("s"), nil)
	if llm.KindOf(err) != llm.KindRefusal {
		t.Fatalf("Review = %+v, %v — want the refusal", rev, err)
	}
	if rev.Decision == phase.SelfIterate {
		t.Fatal("a refused review was rescued into self_iterate")
	}
	done := completedPhase(t, pub, "review")
	if !done.Failed || done.RescueFired || done.Refusal == nil {
		t.Fatalf("record failed=%v rescue=%v refusal=%+v", done.Failed, done.RescueFired, done.Refusal)
	}
}

// THE TURN THAT MET A REFUSAL IS NOT RUN AGAIN FROM ITS TRIGGER — the same
// rule, one frame up: a redelivery is a re-ask.
func TestARefusedTurnIsAbandonedNotRedelivered(t *testing.T) {
	t.Parallel()
	prov := &refusing{scriptedProvider: &scriptedProvider{}, refuse: "execute"}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{})
	_, _, err := r.Execute(context.Background(), 1, "", nil)
	if reason, abandon := turn.Abandon(turn.Result{}, err); !abandon || reason != turn.AbandonedRefused {
		t.Fatalf("Abandon = (%q, %v), want the refusal's reason", reason, abandon)
	}
}

// A ROUND THE CAP CUT OFF fails the phase by its stop reason, and the record
// says which — `max_tokens`, not a rescue and not a generic `error`.
func TestAPhaseEndedByItsStopReasonIsNamed(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &scriptedProvider{execute: []llm.Completion{{
		Content: "Posting", StopReason: llm.StopMaxTokens,
		ToolCalls: []llm.ToolCall{{ID: "c", Name: runner.SubmitWorkTool, Arguments: map[string]any{}}},
	}}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{pub: pub})
	_, _, err := r.Execute(context.Background(), 1, "", nil)
	var stop *toolloop.StopError
	if !errors.As(err, &stop) {
		t.Fatalf("err = %v, want a StopError", err)
	}
	done := completedPhase(t, pub, "execute")
	if !done.Failed || done.ErrorKind != "max_tokens" || done.Refusal != nil || done.RescueFired {
		t.Fatalf("record failed=%v kind=%q refusal=%+v rescue=%v",
			done.Failed, done.ErrorKind, done.Refusal, done.RescueFired)
	}
	if len(done.Rounds) != 1 || done.Rounds[0].StopReason != "max_tokens" {
		t.Fatalf("rounds = %+v, want the cut round with its stop reason", done.Rounds)
	}
}
