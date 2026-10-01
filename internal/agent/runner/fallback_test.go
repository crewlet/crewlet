package runner_test

import (
	"context"
	"encoding/json"
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

func TestAProviderHandOffIsPublishedAgainstTheTurnItHappenedIn(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &scriptedProvider{execute: deliver(t, "posted the weekly summary")}
	r, _ := buildWith(t, []phase.Entry{
		{Key: "benched", Provider: benched{model: "benched-model"}},
		{Key: "executor", Provider: prov},
	}, buildOpts{pub: pub, execChain: org.ProviderKeys{"benched", "executor"}})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// ONE PER PROVIDER CALL, not one per phase: a chain is walked on every
	// round, so a benched member is a hand-off on every round. Counting them
	// per phase would report one flap where there were three, which is the
	// difference between a blip and a provider to take out of the chain.
	rounds := len(prov.requestsFor("execute"))
	got := pub.fallbacks()
	if rounds == 0 {
		t.Fatal("the executor never reached its provider")
	}
	if len(got) != rounds {
		t.Fatalf("published %d provider_fallback events over %d rounds, want one each",
			len(got), rounds)
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
		{"phase", string(f.Phase), "execute"},
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

func TestTheLastMemberOfAChainFallsBackToNothingAndSaysSo(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{
		{Key: "benched", Provider: benched{model: "benched-model"}},
		{Key: "also-benched", Provider: benched{model: "also-benched-model"}},
	}, buildOpts{pub: pub, execChain: org.ProviderKeys{"benched", "also-benched"}})

	// The phase fails: no member answered. That is the point — the hand-offs
	// are what tell an operator WHY, and they are published on the way down
	// rather than reconstructed from the failure afterwards.
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err == nil {
		t.Fatal("Execute succeeded with every member benched")
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
	prov := &scriptedProvider{execute: deliver(t, "posted the weekly summary")}
	r, _ := buildWith(t, []phase.Entry{{Key: "executor", Provider: prov}},
		buildOpts{pub: pub})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
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
	prov := &scriptedProvider{execute: deliver(t, "posted the weekly summary")}
	r, _ := buildWith(t, []phase.Entry{{Key: "executor", Provider: prov}},
		buildOpts{pub: pub})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := pub.sizes()
	// ONE PER PHASE, not one per round: the prompt is measured where the
	// phase opens, which is the one frame holding the final system and user
	// text after every builder, prefetch and ledger has had its say.
	if len(got) != 1 {
		t.Fatalf("published %d prompt.size events for one phase, want 1", len(got))
	}
	m := got[0]
	if m.TurnID != "t-1" || m.Iteration != 1 || string(m.Phase) != "execute" {
		t.Errorf("addressed to turn=%q iter=%d phase=%q, want t-1/1/execute",
			m.TurnID, m.Iteration, m.Phase)
	}
	// The measurement is of the REAL prompt, so it has to match what the
	// provider actually received. A builder that measured a draft would
	// report a number that shrinks every time a section moves.
	sent := prov.requestsFor("execute")[0]
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
	// A fresh phase opens the conversation, so there is nothing seeded to
	// measure. Zero here is the fact that separates it from a resume.
	if m.MessageBytes != 0 {
		t.Errorf("message_chars = %d on a phase that opened its own conversation, want 0",
			m.MessageBytes)
	}
	// AND THE TOOL ARRAY, which both HTTP vendors bill as input and the
	// cli-agent text backend renders into the prompt literally. The meter
	// was blind to it: a measured turn reported ~6,900 tokens against the
	// provider's 205,000, and this row is what "is the prompt getting
	// smaller" is answered from.
	tools := toolArrayBytes(t, sent.Tools)
	// The fixture has to have SENT tools, or the two assertions below hold
	// nothing: every term on both sides of them is then zero, and a surface
	// built wrong or a fake that dropped the array would read as a meter
	// that measured it exactly. Same rule the resumed case applies to its
	// own terms.
	if len(sent.Tools) == 0 || tools == 0 {
		t.Fatalf("the provider was handed %d tool definitions at %d chars; a term at "+
			"zero is a term this case is not holding", len(sent.Tools), tools)
	}
	if m.ToolCount != len(sent.Tools) || m.ToolBytes != tools {
		t.Errorf("measured %d tools at %d chars, provider received %d at %d",
			m.ToolCount, m.ToolBytes, len(sent.Tools), tools)
	}
	// EQUALITY, NOT "NOT ZERO". Every executor surface carries submit_work,
	// so the tool term alone makes the sum non-zero — a `!= 0` assertion
	// here would pass with the system and user terms deleted, which is
	// worse than having no assertion at all.
	if want := (system + user + tools) / 4; m.ApproximateTokens != want {
		t.Errorf("approximate_tokens = %d, want %d over what the provider received",
			m.ApproximateTokens, want)
	}
}

// toolArrayBytes is the compact JSON the engine measures a tool array as,
// rebuilt from what the provider was handed.
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

// seedChars is what the engine measures a parked conversation as, rebuilt from
// what the provider was handed: every message's text, each assistant round's
// reasoning ONCE — the structured blocks where a round has them, the prose
// where it does not — and the compact JSON of its tool calls' arguments.
//
// Restated here rather than reaching for the engine's own unexported helper,
// for the reason toolArrayBytes gives: this suite is the one thing holding that
// helper's answer against what a provider actually received, and a test that
// calls the function it is checking agrees with itself whatever either of them
// does. Restating it is also what makes the double-count visible from here — a
// helper that summed both reasoning fields would have to be written down as
// summing both.
func seedChars(t *testing.T, msgs []llm.Message) int {
	t.Helper()
	total := 0
	for _, msg := range msgs {
		total += len(msg.Content)
		if len(msg.ThinkingBlocks) > 0 {
			for _, tb := range msg.ThinkingBlocks {
				total += len(tb.Thinking) + len(tb.Data)
			}
		} else {
			total += len(msg.ReasoningContent)
		}
		for _, tc := range msg.ToolCalls {
			if len(tc.Arguments) == 0 {
				total += len(`{}`)
				continue
			}
			encoded, err := json.Marshal(tc.Arguments)
			if err != nil {
				t.Fatalf("the fixture's tool call arguments are not JSON: %v", err)
			}
			total += len(encoded)
		}
	}
	return total
}

// A RESUMED PHASE MEASURES THE CONVERSATION IT RE-ENTERS — ALL OF IT.
//
// A detached coding run stops the executor mid-loop with its tool call
// unanswered, and the resume re-enters that same loop from the saved messages
// — so `system` and `user` are ignored and were the only thing this meter
// looked at. Every resumed executor published 0/0, on the one phase that
// carries the most: a whole pre-suspend conversation plus the array. Counting
// only the tool term would have made it worse, reading as a phase offered
// every tool and asked nothing.
//
// The parked rounds here carry REASONING AND TOOL-CALL ARGUMENTS, which is what
// a real one carries — `reasoning: true` gives every round a four-figure
// thinking allowance by default, the tool loop stores the blocks on each
// assistant message and execstate serialises them into the row — and they were
// the next term to go missing after the two above: text alone under-reported a
// resumed executor by the largest thing in its prompt.
func TestAResumedPhaseMeasuresTheConversationItReEnters(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &scriptedProvider{execute: []llm.Completion{
		submitCall(t, runner.SubmitWorkTool,
			`{"outcome":"blocked","summary":"the run reported a failing build",`+
				`"evidence":"the box could not compile it"}`),
	}}
	state := suspendedAfterTwoRounds()
	// The shape the Anthropic backend hands back: the blocks verbatim, and
	// the same thinking text rendered beside them as prose. Added here
	// rather than to the shared fixture, which the round and duration cases
	// read for their own reasons.
	const thought = "the module is untidy, so the box has to run go mod tidy"
	for i, msg := range state.Messages {
		if msg.Role != llm.RoleAssistant {
			continue
		}
		state.Messages[i].ReasoningContent = thought
		state.Messages[i].ThinkingBlocks = []llm.ThinkingBlock{
			{Type: "thinking", Thinking: thought, Signature: "provider-minted"},
		}
	}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		pub: pub,
		resume: &runner.Resume{
			State:  state,
			Answer: "the run succeeded, the merge request is open",
		},
	})
	if _, _, err := r.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	got := pub.sizes()
	if len(got) != 1 {
		t.Fatalf("published %d prompt.size events for one resumed phase, want 1", len(got))
	}
	m := got[0]
	sent := prov.requestsFor("execute")[0]
	messages := seedChars(t, sent.Messages)
	var text, reasoned, args int
	for _, msg := range sent.Messages {
		text += len(msg.Content)
		reasoned += len(msg.ReasoningContent)
		for _, tc := range msg.ToolCalls {
			args += len(tc.Arguments)
		}
	}
	// Each term has to be present in the fixture, or the case cannot tell a
	// fix from a bug: the meter would report the same figure either way.
	if text == 0 || reasoned == 0 || args == 0 {
		t.Fatalf("the fixture sent text=%d reasoning=%d tool-call arguments=%d; a term at "+
			"zero is a term this case is not holding", text, reasoned, args)
	}
	if m.MessageBytes != messages {
		t.Errorf("message_chars = %d, the provider received %d characters of conversation",
			m.MessageBytes, messages)
	}
	// The double count, named: on this backend's shape ReasoningContent is a
	// rendering of the blocks beside it, so summing both would report the
	// prompt's largest term twice.
	if m.MessageBytes == messages+reasoned {
		t.Error("message_chars counted the reasoning twice — the blocks and the prose " +
			"rendering of the same thinking are one term")
	}
	// The two a resume does NOT prepend. Reporting them would report bytes
	// nothing sent, which is the mirror of the bug above.
	if m.SystemBytes != 0 || m.UserBytes != 0 {
		t.Errorf("measured %d/%d system/user chars on a resumed phase, which prepends neither",
			m.SystemBytes, m.UserBytes)
	}
	tools := toolArrayBytes(t, sent.Tools)
	// The tool array is a term of this case exactly as the three above are,
	// and it is held to the same bar: at zero the comparison below is 0 == 0
	// and says nothing about what a resumed phase re-offers.
	if len(sent.Tools) == 0 || tools == 0 {
		t.Fatalf("the resumed phase was handed %d tool definitions at %d chars; a term "+
			"at zero is a term this case is not holding", len(sent.Tools), tools)
	}
	if m.ToolCount != len(sent.Tools) || m.ToolBytes != tools {
		t.Errorf("measured %d tools at %d chars, provider received %d at %d",
			m.ToolCount, m.ToolBytes, len(sent.Tools), tools)
	}
	if want := (messages + tools) / 4; m.ApproximateTokens != want {
		t.Errorf("approximate_tokens = %d, want %d over what the provider received",
			m.ApproximateTokens, want)
	}
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
	prov := &scriptedProvider{execute: deliver(t, "posted the weekly summary")}
	r, _ := buildWith(t, []phase.Entry{{Key: "executor", Provider: prov}},
		buildOpts{pub: pub, task: brief})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := pub.sizes()
	if len(got) != 1 {
		t.Fatalf("published %d prompt.size events for one phase, want 1", len(got))
	}
	sent := prov.requestsFor("execute")[0]
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
// no entry at all, exactly as it names no model.
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
			prov := &scriptedProvider{execute: deliver(t, "posted the weekly summary")}
			r, _ := buildWith(t, []phase.Entry{
				{Key: "benched", Provider: benched{model: "benched-model"}},
				{Key: "also-benched", Provider: benched{model: "also-benched-model"}},
				{Key: "executor", Provider: prov},
			}, buildOpts{pub: pub, execChain: tc.chain})

			_, _, err := r.Execute(context.Background(), 1, "", nil)
			if (err != nil) != tc.fails {
				t.Fatalf("Execute: %v, want a failure: %v", err, tc.fails)
			}
			if got := completedPhase(t, pub, "execute").ProviderKey; got != tc.want {
				t.Errorf("provider_key = %q, want %q", got, tc.want)
			}
		})
	}
}
