package toolloop_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// The whole point of streaming: a round's text reaches the live view WHILE it
// is being written. Before this, a phase composing a long reasoning block
// published nothing at all until the provider call returned, so the screen sat
// visibly frozen for the length of the answer.
func TestARoundIsVisibleWhileItIsStillBeingWritten(t *testing.T) {
	t.Parallel()
	p := &streamingProvider{
		fragments: []llm.Delta{
			{Reasoning: "which tool"}, {Content: "look"}, {Content: "ing it up"},
		},
		final: llm.Completion{Content: "looking it up", ReasoningContent: "which tool"},
	}
	var partials []string
	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{}, MaxRounds: 2,
		// Every fragment publishes, so the accumulation is observable
		// rather than dependent on how fast the test machine is.
		StreamPartials: true, PartialInterval: time.Nanosecond,
		OnProgress: func(live toolloop.Result) {
			if live.Partial != nil {
				partials = append(partials, live.Partial.Content)
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(partials) == 0 {
		t.Fatal("nothing was published while the round was being written")
	}
	// It APPENDS. A consumer that replaced rather than appended would show
	// only the last fragment.
	if last := partials[len(partials)-1]; !strings.Contains(last, "look") {
		t.Errorf("last partial = %q, want the accumulated text", last)
	}
}

// The partial is a view of a call in progress, never a second source of truth.
// Once the round commits, its narration is authoritative and the fragment must
// be gone — otherwise the live view shows a finished round and a piece of the
// same round at once.
func TestThePartialIsClearedWhenItsRoundCommits(t *testing.T) {
	t.Parallel()
	p := &streamingProvider{
		fragments: []llm.Delta{{Content: "hi"}},
		final:     llm.Completion{Content: "hi"},
	}
	var lastHadPartial bool
	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{}, MaxRounds: 2,
		StreamPartials: true,
		OnProgress:     func(live toolloop.Result) { lastHadPartial = live.Partial != nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lastHadPartial {
		t.Error("the last published view still carried a fragment of a committed round")
	}
	if res.Partial != nil {
		t.Error("a finished Result carries a partial; it is a live-only view")
	}
}

// A failover is invisible from inside a delta stream. Without the restart
// signal a consumer appends the replacement to the abandoned attempt and shows
// two half-answers from two models as one paragraph.
func TestAnAbandonedAttemptIsKeptAndTheRetryStartsClean(t *testing.T) {
	t.Parallel()
	p := &streamingProvider{
		fragments: []llm.Delta{
			{Content: "first try"},
			{Restart: true, Model: "backup"},
			{Content: "second try"},
		},
		final: llm.Completion{Content: "second try"},
	}
	var seen *toolloop.Partial
	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{}, MaxRounds: 2,
		StreamPartials: true, PartialInterval: time.Nanosecond,
		OnProgress: func(live toolloop.Result) {
			if live.Partial != nil {
				seen = live.Partial
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if seen == nil {
		t.Fatal("no partial was ever published")
	}
	if strings.Contains(seen.Content, "first try") {
		t.Errorf("content = %q — the retry was appended to the abandoned attempt", seen.Content)
	}
	if len(seen.Abandoned) != 1 || !strings.Contains(seen.Abandoned[0].Content, "first try") {
		t.Errorf("abandoned = %#v, want the attempt that was given up on", seen.Abandoned)
	}
}

// Streaming is opt-in per call. Twelve of the thirteen call sites in this
// engine want an answer, not a running commentary, and must keep taking the
// unary path untouched.
func TestAProviderIsNotAskedToStreamUnlessSomethingIsWatching(t *testing.T) {
	t.Parallel()
	p := &streamingProvider{final: llm.Completion{Content: "hi"}}
	if _, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{}, MaxRounds: 2,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p.askedToStream {
		t.Error("a loop with no progress listener still asked the provider to stream")
	}
}

// streamingProvider replays fragments, then answers.
type streamingProvider struct {
	fragments     []llm.Delta
	final         llm.Completion
	askedToStream bool
}

func (p *streamingProvider) Model() string { return "streamer" }

func (p *streamingProvider) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	p.askedToStream = req.Streaming()
	for _, d := range p.fragments {
		req.Send(d)
	}
	out := p.final
	return &out, nil
}

// A streamed round publishes WHILE the call is open, and the model that served
// it is only known once the call returns — so every frame of a streamed round
// reported an empty model and a running row showed a dash where its model
// should be. The configured name stands in until the real one exists.
func TestAStreamedRoundNamesAModelBeforeTheCallReturns(t *testing.T) {
	t.Parallel()
	p := &streamingProvider{
		fragments: []llm.Delta{{Content: "think"}},
		final:     llm.Completion{Content: "done", Model: "served-model"},
	}
	var duringCall []string
	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{}, MaxRounds: 2,
		StreamPartials: true, PartialInterval: time.Nanosecond,
		OnProgress: func(live toolloop.Result) {
			if live.Partial != nil {
				duringCall = append(duringCall, live.Model)
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range duringCall {
		if m == "" {
			t.Fatalf("a streamed round published with no model: %#v", duringCall)
		}
	}
	// And the model that ACTUALLY served still wins — it is the billable
	// fact and what the per-model breakdown is built from.
	if res.Model != "served-model" {
		t.Errorf("model = %q, want the one the completion named", res.Model)
	}
}
