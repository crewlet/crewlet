package harness

import (
	"strings"
	"testing"
)

// THE DOCTOR'S SMOKE TEST CERTIFIES EVERY SHIPPED TOOL ROUTE, and says how each
// harness's calls are joined to its record: Codex and Muse Code by the id each
// sends in `_meta`, every other by name and arguments. It runs the route a
// seat's phase runs — the profile's configuration, the relay, the record — so
// a route the doctor passes is one a seat can use. The requests are of a real
// size: a record with no counts per request (Muse Code's) is charged an
// estimate of each as it is read, which is never refunded.
//
// Mutation: judge the join as though no profile read its record, and every
// harness's smoke line says a run counts as one round.
func TestTheSmokeTestCertifiesEveryShippedToolRoute(t *testing.T) {
	t.Parallel()
	for agent, join := range map[string]string{
		"codex":        "joined to its record by the id it sends in _meta callId",
		"gemini-cli":   "joined to its record by name and arguments",
		"qwen-code":    "joined to its record by name and arguments",
		"opencode":     "joined to its record by name and arguments",
		"copilot":      "joined to its record by name and arguments",
		"grok":         "joined to its record by name and arguments",
		"pi":           "joined to its record by name and arguments",
		"kimi-code":    "joined to its record by name and arguments",
		"muse-code":    "joined to its record by the id it sends in _meta idempotencyKey",
		"hermes":       "joined to its record by name and arguments",
		"cursor-agent": "joined to its record by name and arguments",
	} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()
			p := fakeAs(t, agent, fakeScript{Responses: []fakeResponse{
				{Calls: []fakeCall{{Name: smokeTool, Args: map[string]any{"ok": true}}}, Usage: fakeUsage{Input: 3000, Output: 6}},
				{Text: "done", Usage: fakeUsage{Input: 4000, Output: 2}},
			}}, nil)
			verdict, answered := p.smokeTest(t.Context())
			if !strings.HasPrefix(verdict, "ok") || !answered {
				t.Fatalf("smoke = %q (answered %v)", verdict, answered)
			}
			for _, want := range []string{"called crewlet_smoke through the crewlet server", join, "7000 in / 8 out"} {
				if !strings.Contains(verdict, want) {
					t.Errorf("the smoke line does not say %q: %s", want, verdict)
				}
			}
		})
	}
}
