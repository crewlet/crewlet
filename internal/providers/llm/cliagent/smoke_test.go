package cliagent

import (
	"strings"
	"testing"
)

// THE SMOKE TEST PASSES WHEN THE CLI CALLS THE TOOL ON A REAL SESSION — and
// says how: that the call reached the server the profile's tool route started,
// how it was joined to the CLI's record, and what the CLI offered its model,
// which is where a confinement that stopped holding would show first.
//
// Mutation: pass on an answer that names the tool rather than on the call the
// server recorded, and the case below that never calls it passes too.
func TestTheSmokeTestPassesWhenTheCLICallsTheTool(t *testing.T) {
	t.Parallel()
	p := fakeClaude(t, fakeScript{Responses: []fakeResponse{
		{Calls: []fakeCall{{Name: smokeTool, Args: map[string]any{"ok": true}}}, Usage: fakeUsage{Input: 30, Output: 6}},
		{Text: "done", Usage: fakeUsage{Input: 40, Output: 2}},
	}}, nil)
	verdict, answered := p.smokeTest(t.Context())
	if !strings.HasPrefix(verdict, "ok") || !answered {
		t.Fatalf("smoke = %q (answered %v)", verdict, answered)
	}
	for _, want := range []string{
		"called crewlet_smoke through the crewlet server",
		"joined to its record by the id it sends in _meta claudecode/toolUseId",
		"its model was offered WebFetch, WebSearch, mcp__crewlet__crewlet_smoke",
		"70 in / 8 out",
	} {
		if !strings.Contains(verdict, want) {
			t.Errorf("the smoke line does not say %q: %s", want, verdict)
		}
	}
}

// A RECORD THAT NEVER SHOWS THE CALL FAILS THE SMOKE TEST. The CLI reached the
// tools and called one, but the profile's requests rules read nothing of it —
// a CLI that changed what it writes, or one whose record no stand-in can
// produce and only a real sign-in shows (cursor-agent's) — so a phase's
// rounds and spend would not be read as it ran, and nothing else says so.
//
// Mutation: pass on the call alone, and this passes as "ok".
func TestTheSmokeTestFailsARecordThatNeverShowsItsCall(t *testing.T) {
	t.Parallel()
	p := fakeClaude(t, fakeScript{Responses: []fakeResponse{
		{Calls: []fakeCall{{Name: smokeTool, Args: map[string]any{"ok": true}}}, Usage: fakeUsage{Input: 30, Output: 6}},
		{Text: "done", Usage: fakeUsage{Input: 40, Output: 2}},
	}}, func(c *Config) {
		// Rules that match nothing the CLI writes.
		c.Overrides["requests"] = map[string]any{"events": []any{map[string]any{
			"match": []any{map[string]any{"path": []any{"type"}, "equals": "no-such-event"}},
			"opens": true,
			"text":  []any{"text"},
		}}}
	})
	verdict, answered := p.smokeTest(t.Context())
	if !strings.HasPrefix(verdict, "failed") || !answered {
		t.Fatalf("smoke = %q (answered %v), want a failure naming the record", verdict, answered)
	}
	if !strings.Contains(verdict, "record never showed") || !strings.Contains(verdict, "overrides.requests") {
		t.Errorf("the smoke failure does not say the record missed the call, or what to set: %q", verdict)
	}
}

// The one failure nothing else catches: the CLI reaches the tools, and answers
// with PROSE. Every phase a seat runs on such a provider spends corrective
// rounds asking again — and ends without its submission when the model never
// makes the call — while the config looks perfect.
func TestTheSmokeTestCatchesACLIThatAnswersWithoutCallingTheTool(t *testing.T) {
	t.Parallel()
	p := fakeClaude(t, fakeScript{Responses: []fakeResponse{
		{Text: "I would call the tool.", Usage: fakeUsage{Input: 30, Output: 6}},
	}}, nil)
	verdict, answered := p.smokeTest(t.Context())
	if !strings.HasPrefix(verdict, "failed") || !answered {
		t.Fatalf("smoke = %q (answered %v), want a failure from a CLI that answered", verdict, answered)
	}
	if !strings.Contains(verdict, "corrective round") || !strings.Contains(verdict, "I would call the tool.") {
		t.Errorf("the smoke failure does not say what it costs and what the CLI said: %q", verdict)
	}
}

// The other failure the smoke test is the ONLY thing that catches: the CLI
// exits 0, bills output tokens and answers with nothing at all. An empty
// answer is a model outcome the tool loop corrects, so this probe is the one
// place an operator is told the difference between prose and silence —
// reporting silence as `It said: ""` sends them to the prompt for a problem
// only a bigger model fixes.
func TestTheSmokeTestNamesAnEmptyAnswerRatherThanQuotingIt(t *testing.T) {
	t.Parallel()
	p := fakeClaude(t, fakeScript{Responses: []fakeResponse{
		{Usage: fakeUsage{Input: 30, Output: 627}},
	}}, nil)
	verdict, _ := p.smokeTest(t.Context())
	if !strings.HasPrefix(verdict, "failed") || !strings.Contains(verdict, "nothing at all") {
		t.Fatalf("smoke = %q, want a failure naming the silence", verdict)
	}
	// The token count is the EVIDENCE that the model worked and said
	// nothing, which is what tells an operator the fix is the model.
	if !strings.Contains(verdict, "627") {
		t.Errorf("the smoke failure does not report what the silence cost: %q", verdict)
	}
	if strings.Contains(verdict, `It said: ""`) {
		t.Errorf("silence was reported as an empty quotation: %q", verdict)
	}
}

// A PROFILE WITH NO TOOL ROUTE FAILS THE SMOKE TEST BY NAME — its seats would
// be refused — while the doctor still learns whether the CLI answers at all.
func TestTheSmokeTestNamesAProfileWithNoToolRoute(t *testing.T) {
	t.Parallel()
	p := fakeProvider(t, map[string]string{"FAKE_STDOUT": "ok"}, nil)
	verdict, answered := p.smokeTest(t.Context())
	if !strings.HasPrefix(verdict, "failed") || !strings.Contains(verdict, "overrides.mcp") {
		t.Errorf("smoke = %q, want a failure naming overrides.mcp", verdict)
	}
	if !answered {
		t.Error("a CLI that answered was reported as one that did not")
	}
}
