package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// A provider chain that falls through has to say so ON THE TURN.
//
// The event type, its category and its documentation all existed while nothing
// wired [chain.Options.OnFallback], so no code path could produce one: every
// screen read a company whose providers never failed. These cases hold the
// wiring in place, and they hold the ADDRESSING in place too — `turn_id` is
// the promoted column the turn lookup selects on, so a hand-off published
// without one is invisible to the screen built to show a turn end to end.

// benched is a member that fails the way a spent key does — retryably, so the
// chain moves on rather than returning the error to the caller.
type benched struct{ model string }

func (b benched) Model() string { return b.model }

func (b benched) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return nil, &llm.Error{
		Kind: llm.KindRateLimit, Provider: "p", Model: b.model,
		Err: fmt.Errorf("rate limited"),
	}
}

// fallbacks returns every hand-off the phase published, in order.
func (c *capture) fallbacks() []*types.ProviderFallback {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	var out []*types.ProviderFallback
	for _, ev := range c.events {
		if got, ok := events.DataAs[*types.ProviderFallback](ev); ok {
			out = append(out, got)
		}
	}
	return out
}

// reviewing is a provider whose reviewer answers in prose before it decides,
// so its phase takes two harness runs — the prose one and the corrected one — and
// a chain in front of it is walked on more than one.
func reviewing(t *testing.T) *scriptedProvider {
	t.Helper()
	return &scriptedProvider{review: []llmtest.Response{
		text("Reading the log first."),
		submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`),
	}}
}

func TestAProviderHandOffIsPublishedAgainstTheTurnItHappenedIn(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := reviewing(t)
	r, _ := buildWith(t, []phase.Entry{
		{Key: "benched", Provider: benched{model: "benched-model"}},
		{Key: "executor", Provider: prov},
	}, buildOpts{pub: pub, reviewChain: org.ProviderKeys{"benched", "executor"}})

	if _, err := r.Review(context.Background(), 1, workFor("posted"), nil); err != nil {
		t.Fatalf("Review: %v", err)
	}

	// ONE PER PROVIDER CALL, not one per phase: a chain is walked on every
	// harness run, so a benched member is a hand-off on every run. Counting them
	// per phase would report one flap where there were three, which is the
	// difference between a blip and a provider to take out of the chain.
	runs := len(prov.requestsFor("review"))
	got := pub.fallbacks()
	if runs < 2 {
		t.Fatalf("the reviewer reached its provider %d times, want a hand-off per run to count", runs)
	}
	if len(got) != runs {
		t.Fatalf("published %d provider_fallback events over %d runs, want one each",
			len(got), runs)
	}

	f := got[0]
	// EVERY field, because each one is a different way for the row to be
	// useless: without the turn id the Turn screen cannot select it, without
	// the phase and iteration it cannot be attributed to the round it broke,
	// and without the two keys it names neither the provider that failed nor
	// the one that took over.
	for _, c := range []struct{ field, got, want string }{
		{"turn_id", f.TurnID, "t-1"},
		{"agent_id", f.Agent, "a-1"},
		{"role", f.RoleName, "CTO"},
		{"phase", string(f.Phase), "review"},
		{"from_provider_key", f.FromProviderKey, "benched"},
		{"to_provider_key", f.ToProviderKey, "executor"},
		{"error_kind", f.ErrorKind, llm.KindRateLimit.String()},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	if f.Iteration != 1 {
		t.Errorf("iteration = %d, want 1", f.Iteration)
	}
}

// fallingLauncher stands for a launch that skipped a member sitting out before
// the one that took the run: it reports the hand-off through the request, as
// the engine's launch does, and starts nothing.
type fallingLauncher struct{ skipped chain.Fallback }

func (l fallingLauncher) LaunchExecutor(_ context.Context, req runner.AgentRunRequest) (runner.Launched, error) {
	req.OnFallback(l.skipped)
	return runner.Launched{LaunchID: "job-1"}, nil
}

// A LAUNCH-TIME HAND-OFF IS PUBLISHED AGAINST THE TURN TOO.
//
// The executor's launch walks the seat's executor chain for a member that can
// take the run, and a member sitting out is skipped exactly as a failing call
// is. The pick is the engine's and the engine has no emitter, so the runner
// hands the launch a route bound to the turn, the phase and the iteration —
// without it a launch that fell back is a hand-off published nowhere, which
// is the "type with no producer" this suite exists for.
//
// Mutation: drop OnFallback from the request, and the launch panics; publish
// it without the iteration, and the row names the wrong round.
func TestALaunchTimeHandOffIsPublishedAgainstTheTurn(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	skipped := chain.Fallback{From: "spent-plan", To: "backup-plan", Kind: llm.KindRateLimit,
		Err: errors.New("usage limit reached — sitting out until 10:00")}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{pub: pub, launcher: fallingLauncher{skipped: skipped}})

	if _, _, err := r.Execute(context.Background(), 2, "try again", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := pub.fallbacks()
	if len(got) != 1 {
		t.Fatalf("published %d provider_fallback events for one skipped member, want 1", len(got))
	}
	f := got[0]
	for _, c := range []struct{ field, got, want string }{
		{"turn_id", f.TurnID, "t-1"},
		{"phase", string(f.Phase), "execute"},
		{"from_provider_key", f.FromProviderKey, "spent-plan"},
		{"to_provider_key", f.ToProviderKey, "backup-plan"},
		{"error_kind", f.ErrorKind, llm.KindRateLimit.String()},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	if f.Iteration != 2 {
		t.Errorf("iteration = %d, want the round the run was launched in", f.Iteration)
	}
}

func TestTheLastMemberOfAChainFallsBackToNothingAndSaysSo(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{
		{Key: "benched", Provider: benched{model: "benched-model"}},
		{Key: "also-benched", Provider: benched{model: "also-benched-model"}},
	}, buildOpts{pub: pub, reviewChain: org.ProviderKeys{"benched", "also-benched"}})

	// The phase fails: no member answered. That is the point — the hand-offs
	// are what tell an operator WHY, and they are published on the way down
	// rather than reconstructed from the failure afterwards.
	if _, err := r.Review(context.Background(), 1, workFor("posted"), nil); err == nil {
		t.Fatal("Review succeeded with every member benched")
	}

	got := pub.fallbacks()
	if len(got) != 2 {
		t.Fatalf("published %d provider_fallback events, want 2 — one per member",
			len(got))
	}
	if got[1].ToProviderKey != "" {
		t.Errorf("to_provider_key = %q on the last member, want empty: there is "+
			"nothing left to fall to, and naming one would name a provider that "+
			"does not exist", got[1].ToProviderKey)
	}
	// The summary has to READ as the end of the chain rather than as a line
	// that lost its second half.
	want := "CTO also-benched failed (rate_limit) — no provider left in the chain"
	if summary := got[1].SummaryFor("CTO"); summary != want {
		t.Errorf("summary = %q, want %q", summary, want)
	}
}

func TestAChainThatDoesNotFallThroughPublishesNothing(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{{Key: "executor", Provider: reviewing(t)}},
		buildOpts{pub: pub})

	if _, err := r.Review(context.Background(), 1, workFor("posted"), nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	// A healthy turn is a SILENT one here. An event published per phase
	// regardless would make the Turn screen's anomaly section permanently
	// non-empty, which is the same as having no anomaly section.
	if got := pub.fallbacks(); len(got) != 0 {
		t.Errorf("published %d provider_fallback events on a chain that never "+
			"fell through, want 0", len(got))
	}
}

// sizes returns every prompt measurement the phase published.
func (c *capture) sizes() []*types.PromptSize {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	var out []*types.PromptSize
	for _, ev := range c.events {
		if got, ok := events.DataAs[*types.PromptSize](ev); ok {
			out = append(out, got)
		}
	}
	return out
}

// A phase measures the prompt it is ABOUT TO SEND, addressed to its turn.
//
// The type was registered and documented with no producer, so "is the prompt
// getting smaller" was a question with a schema and no data. It is a separate
// row rather than a derivation because the prompts themselves live on
// AgentPhaseCompleted, and counting their characters means hauling every
// phase's whole payload back across the driver.
func TestAPhaseMeasuresTheFinalPromptItSends(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := reviewing(t)
	r, _ := buildWith(t, []phase.Entry{{Key: "executor", Provider: prov}},
		buildOpts{pub: pub})

	if _, err := r.Review(context.Background(), 1, workFor("posted"), nil); err != nil {
		t.Fatalf("Review: %v", err)
	}

	got := pub.sizes()
	// ONE PER PHASE, not one per round: the prompt is measured where the
	// phase opens, which is the one frame holding the final system and user
	// text after every builder, prefetch and ledger has had its say.
	if len(got) != 1 {
		t.Fatalf("published %d prompt.size events for one phase, want 1", len(got))
	}
	m := got[0]
	if m.TurnID != "t-1" || m.Iteration != 1 || string(m.Phase) != "review" {
		t.Errorf("addressed to turn=%q iter=%d phase=%q, want t-1/1/review",
			m.TurnID, m.Iteration, m.Phase)
	}
	// The measurement is of the REAL prompt, so it has to match what the
	// provider actually received. A builder that measured a draft would
	// report a number that shrinks every time a section moves.
	run := prov.runsFor("review")[0]
	sent := run.req
	var system, user int
	for _, msg := range sent.Messages {
		switch msg.Role {
		case llm.RoleSystem:
			system += len(msg.Content)
		case llm.RoleUser:
			user += len(msg.Content)
		}
	}
	if m.SystemBytes != system || m.UserBytes != user {
		t.Errorf("measured %d/%d bytes, provider received %d/%d",
			m.SystemBytes, m.UserBytes, system, user)
	}
	// AND THE TOOL ARRAY, which the harness lists over MCP and hands its model
	// with every request, billed as input. The meter was blind to it: a
	// measured turn reported ~6,900 tokens against the provider's 205,000,
	// and this row is what "is the prompt getting smaller" is answered from.
	// Read off what the harness LISTED when it opened the run — the array as it
	// reached the model, not as the engine meant to send it.
	offered := run.offered[0]
	tools := toolArrayBytes(t, offered)
	// The fixture has to have SENT tools, or the two assertions below hold
	// nothing: every term on both sides of them is then zero, and a surface
	// built wrong or a fake that dropped the array would read as a meter
	// that measured it exactly.
	if len(offered) == 0 || tools == 0 {
		t.Fatalf("the provider was handed %d tool definitions at %d chars; a term at "+
			"zero is a term this case is not holding", len(offered), tools)
	}
	if m.ToolCount != len(offered) || m.ToolBytes != tools {
		t.Errorf("measured %d tools at %d chars, provider received %d at %d",
			m.ToolCount, m.ToolBytes, len(offered), tools)
	}
	// EQUALITY, NOT "NOT ZERO". Every review surface carries submit_review,
	// so the tool term alone makes the sum non-zero — a `!= 0` assertion
	// here would pass with the system and user terms deleted, which is
	// worse than having no assertion at all.
	if want := (system + user + tools) / 4; m.ApproximateTokens != want {
		t.Errorf("approximate_tokens = %d, want %d over what the provider received",
			m.ApproximateTokens, want)
	}
}

// toolArrayBytes is the compact JSON the engine measures a tool array as,
// rebuilt from what the harness listed.
//
// Restated here rather than reaching for the engine's own unexported helper:
// this suite is the one thing holding that helper's answer to what the
// provider actually received, and a test that calls the function it is
// checking agrees with itself whatever either of them does.
func toolArrayBytes(t *testing.T, defs []llm.ToolDef) int {
	t.Helper()
	type wire struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters,omitempty"`
	}
	if len(defs) == 0 {
		return 0
	}
	out := make([]wire, 0, len(defs))
	for _, d := range defs {
		out = append(out, wire{Name: d.Name, Description: d.Description, Parameters: d.Parameters})
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("the fixture's tool array is not JSON: %v", err)
	}
	return len(encoded)
}

// THE MEASUREMENT IS IN BYTES, which is what the fields are named for and what
// every reader of them renders.
//
// The case above cannot see this: it compares len() against len(), so it holds
// just as well for a build that counted runes. The two quantities only diverge
// on a prompt carrying multi-byte runes — a roster of non-Latin names, a chat
// thread with emoji in it, a CJK knowledge block — which is why the brief here
// is one, and why a prompt whose byte count equalled its rune count would make
// this case prove nothing.
func TestAPhaseMeasuresItsPromptInBytesRatherThanRunes(t *testing.T) {
	t.Parallel()
	// Eight runes, twenty-four bytes: every one of them is three bytes in
	// UTF-8, so the two readings of "size" differ by 16 on this phrase alone.
	const brief = "投稿してください"
	pub := newCapture()
	prov := reviewing(t)
	r, _ := buildWith(t, []phase.Entry{{Key: "executor", Provider: prov}},
		buildOpts{pub: pub, task: brief})

	if _, err := r.Review(context.Background(), 1, workFor("posted"), nil); err != nil {
		t.Fatalf("Review: %v", err)
	}

	got := pub.sizes()
	if len(got) != 1 {
		t.Fatalf("published %d prompt.size events for one phase, want 1", len(got))
	}
	sent := prov.requestsFor("review")[0]
	var user int
	for _, msg := range sent.Messages {
		if msg.Role == llm.RoleUser {
			user += len(msg.Content)
		}
	}
	if !strings.Contains(userText(sent), brief) {
		t.Fatalf("the brief never reached the user prompt, so this case measures nothing")
	}
	if utf8.RuneCountInString(userText(sent)) == user {
		t.Fatalf("the user prompt is pure ASCII at %d bytes, so bytes and runes "+
			"agree and this case cannot tell them apart", user)
	}
	if got[0].UserBytes != user {
		t.Errorf("UserBytes = %d, want %d (the bytes the provider received); "+
			"a rune count would report %d",
			got[0].UserBytes, user, utf8.RuneCountInString(userText(sent)))
	}
}

// userText is every user message of a request, joined as the measurement sums
// them.
func userText(req llm.Request) string {
	var b strings.Builder
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleUser {
			b.WriteString(msg.Content)
		}
	}
	return b.String()
}

// A PHASE RECORD NAMES THE ENTRY THAT SERVED IT, which after a hand-off is not
// the head of its chain.
//
// provider_key is the operator's name for what answered — the spend-by-provider
// rollup is keyed on it — and the phase record left it empty on every turn
// phase: only a delegated worker set it, and that one named the head it was
// resolved under whatever took over. A phase that never reached a model names
// no entry at all, exactly as it names no model. (The executor's record names
// the member its launch picked — see TestAResumedAgentRunPublishesWhatTheRunDid.)
func TestAPhaseRecordNamesTheEntryThatServedIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		chain org.ProviderKeys
		want  string
		fails bool
	}{
		{name: "the head answers", chain: org.ProviderKeys{"executor", "benched"}, want: "executor"},
		{name: "a hand-off", chain: org.ProviderKeys{"benched", "executor"}, want: "executor"},
		// NAMES NOBODY, exactly as its model does: no entry served a
		// phase whose every member failed, and naming the head would
		// charge it for a call it refused.
		{name: "nobody answers", chain: org.ProviderKeys{"benched", "also-benched"},
			want: "", fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pub := newCapture()
			r, _ := buildWith(t, []phase.Entry{
				{Key: "benched", Provider: benched{model: "benched-model"}},
				{Key: "also-benched", Provider: benched{model: "also-benched-model"}},
				{Key: "executor", Provider: reviewing(t)},
			}, buildOpts{pub: pub, reviewChain: tc.chain})

			_, err := r.Review(context.Background(), 1, workFor("posted"), nil)
			if (err != nil) != tc.fails {
				t.Fatalf("Review: %v, want a failure: %v", err, tc.fails)
			}
			if got := completedPhase(t, pub, "review").ProviderKey; got != tc.want {
				t.Errorf("provider_key = %q, want %q", got, tc.want)
			}
		})
	}
}
