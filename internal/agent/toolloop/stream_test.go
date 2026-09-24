package toolloop_test

import (
	"context"
	"errors"
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

// AN ABANDONED ATTEMPT OUTLIVES ITS ROUND.
//
// The partial goes the moment its round commits, taking its copy of each
// abandoned attempt with it, and every live frame that showed one carried only
// its tail. The Result keeps each attempt whole, numbered with the round it
// was an attempt at, beside the narration the retry committed.
func TestAnAbandonedAttemptOutlivesItsRound(t *testing.T) {
	t.Parallel()
	thought := strings.Repeat("weighing the options ", 400) + "— and then the stream died"
	wrote := strings.Repeat("ありがとう ", 900) + "[end of the first try]"
	p := &streamingProvider{
		fragments: []llm.Delta{
			{Reasoning: thought}, {Content: wrote},
			{Restart: true, Model: "backup"},
			{Content: "second try"},
		},
		final: llm.Completion{Content: "second try"},
	}
	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{}, MaxRounds: 2,
		StreamPartials: true, PartialInterval: time.Nanosecond,
		OnProgress: func(toolloop.Result) {},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Abandoned) != 1 {
		t.Fatalf("abandoned = %d attempts, want the one the provider gave up on", len(res.Abandoned))
	}
	got := res.Abandoned[0]
	if got.Round != 1 || got.Reasoning != thought || got.Content != wrote {
		t.Errorf("the abandoned attempt is round %d with %d bytes of reasoning and %d of content; "+
			"want round 1 with the whole %d and %d", got.Round, len(got.Reasoning), len(got.Content),
			len(thought), len(wrote))
	}
	if len(res.Narration) != 1 || res.Narration[0].Content != "second try" {
		t.Errorf("narration = %+v, want the retry's committed answer alone", res.Narration)
	}
}

// A ROUND THE PROVIDER FAILS DURING LEAVES ITS LAST ATTEMPT IN THE SNAPSHOT.
//
// The loop returns an error, so the caller publishes the Progress snapshot —
// and the round in flight never commits, so without this the text a live
// frame had been showing of it is kept nowhere. It joins the attempts
// abandoned before it, in order.
func TestARoundThatFailsMidStreamLeavesItsLastAttempt(t *testing.T) {
	t.Parallel()
	p := &failingStream{fragments: []llm.Delta{
		{Content: "first try"},
		{Restart: true, Model: "backup"},
		{Reasoning: "second thoughts"}, {Content: "second try, cut off"},
	}}
	progress := &toolloop.Progress{}
	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{}, MaxRounds: 2, Progress: progress,
		StreamPartials: true, PartialInterval: time.Hour,
		OnProgress: func(toolloop.Result) {},
	})
	if err == nil {
		t.Fatal("the provider failed and the loop reported no error")
	}
	got := progress.Snapshot().Abandoned
	want := []toolloop.Narration{
		{Round: 1, Content: "first try"},
		{Round: 1, Reasoning: "second thoughts", Content: "second try, cut off"},
	}
	if len(got) != len(want) {
		t.Fatalf("the snapshot holds %d abandoned attempts, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("abandoned attempt %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A NEW INVOCATION DOES NOT REPORT THE LAST ONE'S ATTEMPTS.
//
// One Progress serves every invocation of an extended phase, and the caller
// folds each invocation onto what it already holds. An invocation that died
// before it published anything reporting its predecessor's attempts as its
// own would put them on the record twice.
func TestANewInvocationDoesNotReportTheLastOnesAttempts(t *testing.T) {
	t.Parallel()
	progress := &toolloop.Progress{}
	first := &streamingProvider{
		fragments: []llm.Delta{{Content: "given up on"}, {Restart: true}},
		final:     llm.Completion{Content: "answered"},
	}
	if _, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: first, Surface: &fakeSurface{}, MaxRounds: 2, Progress: progress,
		StreamPartials: true, OnProgress: func(toolloop.Result) {},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := progress.Snapshot().Abandoned; len(got) != 1 {
		t.Fatalf("the first invocation's snapshot holds %d abandoned attempts, want its one", len(got))
	}
	// Dies on its first call, before a fragment or a publish.
	second := &scriptedProvider{failAt: 1}
	if _, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: second, Surface: &fakeSurface{}, MaxRounds: 2, Progress: progress,
		StreamPartials: true, OnProgress: func(toolloop.Result) {},
	}); err == nil {
		t.Fatal("the second invocation's provider failed and the loop reported no error")
	}
	if got := progress.Snapshot().Abandoned; len(got) != 0 {
		t.Errorf("the second invocation reports %+v, which the first abandoned", got)
	}
}

// failingStream streams its fragments and then fails the call, the way a
// provider whose connection drops partway through an answer does.
type failingStream struct{ fragments []llm.Delta }

func (p *failingStream) Model() string { return "failing" }

func (p *failingStream) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	for _, d := range p.fragments {
		req.Send(d)
	}
	return nil, errors.New("stream reset by peer")
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
