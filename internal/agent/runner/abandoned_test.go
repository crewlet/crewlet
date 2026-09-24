package runner_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// streamedRound is one call a streamingScript answers: the fragments it
// streams, then its completion — or its error, when err is set.
type streamedRound struct {
	fragments []llm.Delta
	answer    llm.Completion
	err       error
}

// streamingScript streams each round's fragments to the loop before it
// answers, the way a backend that fails over partway through an answer does.
// The last round repeats once the script runs out.
type streamingScript struct {
	mu     sync.Mutex
	rounds []streamedRound
	n      int
}

func (p *streamingScript) Model() string { return "streamer" }

func (p *streamingScript) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	p.mu.Lock()
	round := p.rounds[min(p.n, len(p.rounds)-1)]
	p.n++
	p.mu.Unlock()
	for _, d := range round.fragments {
		req.Send(d)
	}
	if round.err != nil {
		return nil, round.err
	}
	answer := round.answer
	return &answer, nil
}

// attemptOf is one row of a record's abandoned attempts, as the runner built
// it: the round as an int and the texts as strings.
func attemptOf(t *testing.T, row types.RoundNarration) (int, string, string) {
	t.Helper()
	round, _ := row["round"].(int)
	reasoning, _ := row["reasoning"].(string)
	content, _ := row["content"].(string)
	return round, reasoning, content
}

// A PHASE RECORD KEEPS EACH ABANDONED ATTEMPT WHOLE.
//
// A live frame shows the tail of an attempt a provider gave up on, and only
// while its round is open; once the round commits, the phase's completed
// record is the one place that attempt can still be read. It is there whole,
// numbered with the round it was an attempt at, beside the narration the
// retry committed.
func TestAPhaseRecordKeepsEachAbandonedAttemptWhole(t *testing.T) {
	t.Parallel()
	thought := strings.Repeat("the channel is quiet today ", 300) + "[end of the thought]"
	wrote := strings.Repeat("Сводка за неделю: ", 400) + "[end of the draft]"
	answer := submitWork(t)
	answer.Content = "Nothing to post yet."
	prov := &streamingScript{rounds: []streamedRound{{
		fragments: []llm.Delta{
			{Reasoning: thought}, {Content: wrote},
			{Restart: true, Model: "backup"},
			{Content: "Nothing to post yet."},
		},
		answer: answer,
	}}}
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{pub: pub})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	done := completedPhase(t, pub, "execute")
	if len(done.AbandonedAttempts) != 1 {
		t.Fatalf("the record carries %d abandoned attempts, want the one the provider gave up on",
			len(done.AbandonedAttempts))
	}
	round, reasoning, content := attemptOf(t, done.AbandonedAttempts[0])
	if round != 1 || reasoning != thought || content != wrote {
		t.Errorf("the record's abandoned attempt is round %d with %d bytes of reasoning and %d of "+
			"content; want round 1 with the whole %d and %d", round, len(reasoning), len(content),
			len(thought), len(wrote))
	}
	if len(done.RoundNarration) != 1 || done.RoundNarration[0]["content"] != "Nothing to post yet." {
		t.Errorf("round_narration = %v, want the retry's committed answer alone", done.RoundNarration)
	}
}

// A FAILED PHASE KEEPS THE ATTEMPT IT FAILED DURING.
//
// The round a provider dies in never commits, so its narration never exists —
// and the live frames that showed it carried its tail alone. The failure
// record is published from the loop's snapshot, and that snapshot holds the
// attempt as far as it had streamed.
func TestAFailedPhaseKeepsTheAttemptItFailedDuring(t *testing.T) {
	t.Parallel()
	wrote := strings.Repeat("posting the summary to #eng, ", 300) + "[where the stream died]"
	prov := &streamingScript{rounds: []streamedRound{{
		fragments: []llm.Delta{{Reasoning: "checking the channel"}, {Content: wrote}},
		err: &llm.Error{Kind: llm.KindFatal, Provider: "stub", Model: "streamer",
			Err: errors.New("connection reset by peer")},
	}}}
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{pub: pub})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err == nil {
		t.Fatal("the provider failed and the phase reported no error")
	}

	done := completedPhase(t, pub, "execute")
	if !done.Failed {
		t.Fatal("the record of a phase whose provider failed is not marked failed")
	}
	if len(done.AbandonedAttempts) != 1 {
		t.Fatalf("the failure record carries %d abandoned attempts, want the one the phase died in",
			len(done.AbandonedAttempts))
	}
	round, reasoning, content := attemptOf(t, done.AbandonedAttempts[0])
	if round != 1 || reasoning != "checking the channel" || content != wrote {
		t.Errorf("the failure record's attempt is round %d, reasoning %q and %d bytes of content; "+
			"want round 1, the attempt's reasoning and its whole %d", round, reasoning, len(content),
			len(wrote))
	}
}

// AN ATTEMPT ABANDONED BEFORE A SUSPENSION IS LOGGED WHOLE, ONCE.
//
// A suspending executor publishes no record, and the pending-run row that
// carries its rounds into the resumed record has no field for an abandoned
// attempt — so the one place its whole survives is the WARN line the
// suspension writes, naming the run, the round and the texts with their
// lengths.
func TestAnAttemptAbandonedBeforeASuspensionIsLoggedWhole(t *testing.T) {
	t.Parallel()
	const marker = "[suspension-case attempt]"
	wrote := marker + strings.Repeat(" unlocking the sandbox first", 300)
	prov := &streamingScript{rounds: []streamedRound{
		{
			fragments: []llm.Delta{{Content: wrote}, {Restart: true, Model: "backup"}},
			answer:    activate("run_sandbox"),
		},
		{answer: submitCall(t, "run_sandbox", `{"task":"fix the failing test"}`)},
	}}
	r, reg := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{})
	if err := reg.RegisterWith(suspendingTool{}, tools.Origin("sandbox"), tools.Annotations{}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !w.Suspended {
		t.Fatal("the phase did not suspend, so nothing was left for the row to carry")
	}

	var lines []map[string]any
	for _, line := range logs.records(t, "abandoned_attempt_suspended") {
		if content, _ := line["content"].(string); strings.HasPrefix(content, marker) {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("%d abandoned_attempt_suspended lines for the attempt; want exactly one", len(lines))
	}
	line := lines[0]
	if line["content"] != wrote || line["content_bytes"] != float64(len(wrote)) {
		t.Errorf("the line carries %d bytes of content and names %v; want the whole %d",
			len(line["content"].(string)), line["content_bytes"], len(wrote))
	}
	if line["level"] != "WARN" || line["turn_id"] != "t-1" || line["phase"] != "execute" ||
		line["iteration"] != float64(1) || line["round"] != float64(1) {
		t.Errorf("the line is %v for run %v, phase %v, iteration %v, round %v; want WARN for run "+
			"t-1, execute, iteration 1, round 1", line["level"], line["turn_id"], line["phase"],
			line["iteration"], line["round"])
	}
}

// A PHASE CANCELLED MID-ROUND STILL PUBLISHES ITS RECORD.
//
// A turn's context can be cancelled under it while a round is being written:
// the provider call in flight fails, and the failure record is then the only
// account of what the phase spent and wrote, the attempt it was writing among
// it. A broker client refuses a publish under a context that is already done,
// so the record goes out without the cancellation.
func TestAPhaseCancelledMidRoundStillPublishesItsRecord(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const wrote = "half of the weekly summary, and then the seat was released"
	prov := &cancellingStream{wrote: wrote, cancel: cancel}
	pub := &refusesWhenDone{capture: newCapture()}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{pub: pub})

	if _, _, err := r.Execute(ctx, 1, "", nil); err == nil {
		t.Fatal("the phase's context was cancelled and it reported no error")
	}

	done := completedPhase(t, pub.capture, "execute")
	if !done.Failed || done.ErrorKind != "canceled" {
		t.Errorf("the record is failed=%v with kind %q; want the cancelled phase's failure",
			done.Failed, done.ErrorKind)
	}
	if len(done.AbandonedAttempts) != 1 {
		t.Fatalf("the record carries %d abandoned attempts, want the one the phase was writing",
			len(done.AbandonedAttempts))
	}
	if _, _, content := attemptOf(t, done.AbandonedAttempts[0]); content != wrote {
		t.Errorf("the record's attempt is %q, want %q", content, wrote)
	}
}

// cancellingStream streams one fragment, then cancels the turn's context and
// fails the call the way a cancelled request does.
type cancellingStream struct {
	wrote  string
	cancel context.CancelFunc
}

func (p *cancellingStream) Model() string { return "cancelled" }

func (p *cancellingStream) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	req.Send(llm.Delta{Content: p.wrote})
	p.cancel()
	<-ctx.Done()
	return nil, ctx.Err()
}

// refusesWhenDone refuses a publish under a context that is already done, as
// a broker client does, and captures every other.
type refusesWhenDone struct{ capture *capture }

func (p *refusesWhenDone) Publish(ctx context.Context, topic string, ev *events.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.capture.Publish(ctx, topic, ev)
}
