package toolloop_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// markup is a call typed out in a vendor's own form, as the measured Dashboard
// Assistant turn ended on it.
const markup = `<invoke name="propose_api_change"><parameter name="route">PUT /config/units/{id}</parameter></invoke>`

// A round whose reply asked for calls in a form the backend could not read is
// asked again, once, and is never shown: not narrated, not in the
// conversation, not in the phase's text.
func TestAnUnreadableCallIsAskedAgainAndNeverShown(t *testing.T) {
	t.Parallel()
	cases := map[string]llm.Completion{
		"the backend said so":         {Content: markup, ProtocolError: "it named a tool not offered"},
		"markup the backend let pass": {Content: "Adding the role.\n" + markup},
	}
	for name, first := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := &scriptedProvider{turns: []llm.Completion{
				first,
				{ToolCalls: []llm.ToolCall{toolCall("1", "read")}},
				{Content: "The role is added."},
			}}
			s := &fakeSurface{tools: []llm.ToolDef{def("read")}}
			var shown []string
			res, err := toolloop.Run(t.Context(), toolloop.Config{
				Provider: p, Surface: s, MaxRounds: 5,
				Messages: []llm.Message{{Role: llm.RoleUser, Content: "add a role"}},
				OnProgress: func(r toolloop.Result) {
					for _, n := range r.Narration {
						shown = append(shown, n.Content)
					}
				},
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.RoundsUsed != 3 {
				t.Errorf("rounds = %d, want 3: the faulty round counts as a step", res.RoundsUsed)
			}
			for _, n := range shown {
				if strings.Contains(n, "<invoke") {
					t.Fatalf("narration showed the markup: %q", n)
				}
			}
			for _, m := range res.Messages {
				if strings.Contains(m.Content, "<invoke") && m.Role == llm.RoleAssistant {
					t.Fatalf("the conversation kept the faulty reply: %q", m.Content)
				}
			}
			if strings.Contains(res.Text, "<invoke") {
				t.Errorf("the phase's text carries the markup: %q", res.Text)
			}
			second := p.seen[1].Messages
			if last := second[len(second)-1]; last.Role != llm.RoleUser ||
				!strings.Contains(last.Content, "nothing ran") {
				t.Errorf("the round after the fault was not the corrective: %+v", last)
			}
			if len(s.ran) != 1 || s.ran[0] != "read" {
				t.Errorf("ran %v, want only the corrected round's read", s.ran)
			}
		})
	}
}

// A model that repeats the fault after the corrective fails the loop with a
// protocol error; it never ends on the markup as its answer.
func TestARepeatedProtocolFaultFailsTheLoop(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{turns: []llm.Completion{{Content: markup}, {Content: markup}}}
	s := &fakeSurface{tools: []llm.ToolDef{def("read")}}
	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
	})
	var protocol *toolloop.ProtocolError
	if !errors.Is(err, toolloop.ErrModelProtocol) || !errors.As(err, &protocol) || protocol.Round != 2 {
		t.Fatalf("err = %v, want a protocol error at round 2", err)
	}
	if p.calls != 2 {
		t.Errorf("provider calls = %d, want 2: one corrective, no more", p.calls)
	}
}

// On the last round there is no round to read a corrective, so the fault
// fails the loop at once.
func TestAProtocolFaultOnTheLastRoundFailsAtOnce(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{turns: []llm.Completion{{Content: markup}}}
	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{}, MaxRounds: 1,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
	})
	if !errors.Is(err, toolloop.ErrModelProtocol) {
		t.Fatalf("err = %v, want ErrModelProtocol", err)
	}
}

// A round that called a tool clears the allowance, so a later fault earns its
// own corrective.
func TestTheProtocolAllowanceResetsAfterACall(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{turns: []llm.Completion{
		{Content: markup},
		{ToolCalls: []llm.ToolCall{toolCall("1", "read")}},
		{Content: markup},
		{Content: "done"},
	}}
	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{tools: []llm.ToolDef{def("read")}}, MaxRounds: 6,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// Markup shown as code is prose like any other.
func TestMarkupInCodeIsAnAnswer(t *testing.T) {
	t.Parallel()
	answer := "A call looks like this:\n```xml\n" + markup + "\n```"
	p := &scriptedProvider{turns: []llm.Completion{{Content: answer}}}
	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{}, MaxRounds: 2,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "how is a call written?"}},
	})
	if err != nil || res.Text != answer {
		t.Fatalf("res, err = %q, %v; want the answer", res.Text, err)
	}
}

// A streamed round that starts writing markup is drawn no further than its
// prose while it streams.
func TestALiveRoundIsCutShortOfMarkup(t *testing.T) {
	t.Parallel()
	p := &streamingProvider{fragments: []llm.Delta{{Content: "Adding it. "}, {Content: "<inv"},
		{Content: "oke name=\"x\">"}}, final: llm.Completion{Content: "Done."}}
	var partials []string
	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{}, MaxRounds: 2, StreamPartials: true,
		PartialInterval: time.Nanosecond,
		Messages:        []llm.Message{{Role: llm.RoleUser, Content: "go"}},
		OnProgress: func(r toolloop.Result) {
			if r.Partial != nil {
				partials = append(partials, r.Partial.Content)
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(partials) == 0 {
		t.Fatal("no partial was published")
	}
	for _, c := range partials {
		if strings.Contains(c, "<") {
			t.Fatalf("a partial drew a tag: %q", c)
		}
	}
}
