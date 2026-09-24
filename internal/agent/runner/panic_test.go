package runner_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// panicsAfterStreaming streams its fragments into the round in flight and then
// panics inside the call, the way a provider SDK with a bug does.
type panicsAfterStreaming struct {
	fragments []llm.Delta
	value     any
}

func (p *panicsAfterStreaming) Model() string { return "panicky" }

func (p *panicsAfterStreaming) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	for _, d := range p.fragments {
		req.Send(d)
	}
	panic(p.value)
}

// A PANICKING PHASE PUBLISHES ITS RECORD, AND THE PANIC STILL REACHES THE GUARD.
//
// The turn loop's guard recovers the panic and holds none of what the phase
// did, so the phase's own failure record is the one place the round it was
// writing — which the live frames showed only as a tail — is kept. The phase
// publishes that record, the round among its abandoned attempts whole, and
// panics on with the same value: the guard still ends the turn with an
// unhandled_exception breach naming it.
func TestAPanickingPhasePublishesItsRecordAndThePanicReachesTheGuard(t *testing.T) {
	t.Parallel()
	wrote := strings.Repeat("drafting the weekly summary for #eng, ", 200) + "[where the SDK panicked]"
	value := &struct{ where string }{"the provider SDK"}
	prov := &panicsAfterStreaming{value: value, fragments: []llm.Delta{
		{Reasoning: "what the week looked like"}, {Content: wrote},
	}}
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{pub: pub})

	res, err := turn.Run(context.Background(), r, settings(),
		turn.Input{RunID: "t-panic", Reply: turn.ToolReply("")})

	var panicked *turn.PanicError
	if !errors.As(err, &panicked) || panicked.Value != value {
		t.Fatalf("turn.Run = %v; want the guard to recover the provider's own panic value", err)
	}
	if res.Breach == nil || res.Breach.Kind != types.GuardUnhandledException {
		t.Errorf("breach = %+v, want unhandled_exception", res.Breach)
	}
	done := completedPhase(t, pub, "execute")
	if !done.Failed || done.ErrorKind != string(types.GuardUnhandledException) {
		t.Errorf("the record is failed=%v with kind %q; want a failed record of kind unhandled_exception",
			done.Failed, done.ErrorKind)
	}
	if !strings.Contains(done.Error, "the provider SDK") {
		t.Errorf("the record's error %q does not name the panic", done.Error)
	}
	if len(done.AbandonedAttempts) != 1 {
		t.Fatalf("the record carries %d abandoned attempts, want the round the phase panicked in",
			len(done.AbandonedAttempts))
	}
	round, reasoning, content := attemptOf(t, done.AbandonedAttempts[0])
	if round != 1 || reasoning != "what the week looked like" || content != wrote {
		t.Errorf("the record's attempt is round %d, reasoning %q and %d bytes of content; want round 1, "+
			"the attempt's reasoning and its whole %d", round, reasoning, len(content), len(wrote))
	}
}

// panicsOnRecord is a publisher that panics as a phase record is published and
// captures every other event.
type panicsOnRecord struct{ capture *capture }

func (p *panicsOnRecord) Publish(ctx context.Context, topic string, ev *events.Event) error {
	if ev.Type == (types.AgentPhaseCompleted{}).EventType() {
		panic("the record's publisher broke")
	}
	return p.capture.Publish(ctx, topic, ev)
}

// A PANIC WHILE PUBLISHING THE RECORD DOES NOT REPLACE THE PHASE'S OWN.
//
// The guard names the panic that ended the turn in its breach and logs its
// stack. A second panic raised while the first one's record was being
// published is logged on its own line, and the phase's panic goes on to the
// guard unchanged.
func TestAPanicPublishingTheRecordDoesNotReplaceThePhasesOwn(t *testing.T) {
	t.Parallel()
	value := &struct{ where string }{"the phase"}
	prov := &panicsAfterStreaming{value: value}
	pub := &panicsOnRecord{capture: newCapture()}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{pub: pub})

	_, err := turn.Run(context.Background(), r, settings(),
		turn.Input{RunID: "t-panic-record", Reply: turn.ToolReply("")})

	var panicked *turn.PanicError
	if !errors.As(err, &panicked) || panicked.Value != value {
		t.Fatalf("turn.Run = %v; want the phase's own panic value at the guard", err)
	}
	var lines int
	for _, line := range logs.records(t, "phase_record_panicked") {
		if line["panic"] == "the record's publisher broke" {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("%d phase_record_panicked lines for the publisher's panic; want one", lines)
	}
}

// panicsDeciding is a round-cap judge with a bug.
type panicsDeciding struct{}

func (panicsDeciding) Decide(context.Context, extension.Request) (extension.Decision, error) {
	panic("the judge broke")
}

// A PANIC BETWEEN INVOCATIONS PUTS EACH ROUND ON THE RECORD ONCE.
//
// The judge runs after an invocation's rounds are folded into the phase's own
// account, while the loop's failure view still holds that same invocation. A
// record folding the two would carry every round of it twice.
func TestAPanicBetweenInvocationsPutsEachRoundOnTheRecordOnce(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		thinkAndCall(t, "read_file", `{"path":"/a"}`, "start with the file"),
		thinkAndCall(t, "read_file", `{"path":"/b"}`, "and the other one"),
	}}
	pub := newCapture()
	r := extendableRunner(t, prov, pub, panicsDeciding{})

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the judge's panic did not leave the phase")
			}
		}()
		_, _, _ = r.Execute(context.Background(), 1, "", nil)
	}()

	done := completedPhase(t, pub, "execute")
	if !done.Failed {
		t.Error("the record of a phase whose judge panicked is not marked failed")
	}
	if done.RoundsUsed != 2 || len(done.ToolExecutions) != 2 || len(done.RoundNarration) != 2 {
		t.Errorf("the record carries %d rounds, %d calls and %d narrated rounds; want each of the "+
			"phase's 2 rounds once", done.RoundsUsed, len(done.ToolExecutions), len(done.RoundNarration))
	}
	if done.TotalTokens != 2*(60+40) {
		t.Errorf("total_tokens = %d, want the 200 the phase's two rounds billed", done.TotalTokens)
	}
}
