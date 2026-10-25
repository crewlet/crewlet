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
// is being written. Before this, a phase composing a long answer published
// nothing at all until the provider call returned, so the screen sat visibly
// frozen for the length of the answer.
func TestARoundIsVisibleWhileItIsStillBeingWritten(t *testing.T) {
	t.Parallel()
	p := &streamingProvider{
		fragments: []llm.Delta{{Content: "look"}, {Content: "ing it up"}},
		final:     llm.Completion{Content: "looking it up"},
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

// A RETRACTION ERASES, IT DOES NOT ABANDON. The backend streamed a view it
// could not stand behind once the call ended; no model gave up, so nothing is
// kept as an abandoned attempt, and the text after the retraction is the
// answer from its start rather than a continuation of the retracted view.
func TestARetractedViewIsErasedRatherThanKeptAsAnAbandonedAttempt(t *testing.T) {
	t.Parallel()
	p := &streamingProvider{
		fragments: []llm.Delta{
			{Content: "The sea st"},
			{Retract: true},
			{Content: "The tide turns"},
		},
		final: llm.Completion{Content: "The tide turns"},
	}
	var views []toolloop.Partial
	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{}, MaxRounds: 2,
		StreamPartials: true, PartialInterval: time.Nanosecond,
		OnProgress: func(live toolloop.Result) {
			if live.Partial != nil {
				views = append(views, *live.Partial)
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	erased := false
	for i, v := range views {
		if len(v.Abandoned) != 0 {
			t.Errorf("view %d keeps %#v as abandoned; a retraction is not a failed attempt", i, v.Abandoned)
		}
		if v.Content == "" && i > 0 && views[i-1].Content != "" {
			erased = true
		}
	}
	if !erased {
		t.Errorf("views %#v: the retraction was never published as an erased view", views)
	}
	if last := views[len(views)-1].Content; last != "The tide turns" {
		t.Errorf("last view = %q, want the answer from its start", last)
	}
}

// THE TOKEN STREAM IS ASKED FOR ONLY BY A LOOP THAT SAYS SO, and only while
// something watches. A seat's loop streams its partials and keeps the argv it
// always ran; the Dashboard Assistant's asks for the token stream as well.
func TestTheTokenStreamIsAskedForOnlyWhenTheLoopSaysSo(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		partials, tokens, want bool
	}{{true, false, false}, {true, true, true}, {false, true, false}} {
		p := &streamingProvider{final: llm.Completion{Content: "hi"}}
		if _, err := toolloop.Run(t.Context(), toolloop.Config{
			Provider: p, Surface: &fakeSurface{}, MaxRounds: 2,
			StreamPartials: c.partials, StreamTokens: c.tokens,
			OnProgress: func(toolloop.Result) {},
		}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if p.askedForTokens != c.want {
			t.Errorf("partials %v, tokens %v: token stream asked = %v, want %v",
				c.partials, c.tokens, p.askedForTokens, c.want)
		}
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
	fragments      []llm.Delta
	final          llm.Completion
	askedToStream  bool
	askedForTokens bool
}

func (p *streamingProvider) Model() string { return "streamer" }

func (p *streamingProvider) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	p.askedToStream = req.Streaming()
	p.askedForTokens = req.StreamTokens
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
