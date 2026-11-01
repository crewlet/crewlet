package harness

import (
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// claudeResult is Claude Code's JSON document carrying said as its answer, on
// a run it did not report failed.
func claudeResult(said string) string {
	return `{"type":"result","subtype":"success","is_error":false,"result":` + strconv.Quote(said) + `}`
}

// A SENTINEL IN THE MODEL'S OWN WORDS IS AN ANSWER, ON EVERY SHIPPED PROFILE
// THAT READS THE ANSWER FOR ONE.
//
// What a harness says on a healthy run is the model's prose and the arguments of
// every tool call it makes, and the shipped sentinels include generic phrases
// a model writes as readily as a vendor prints them: "rate limit", "quota",
// "Usage limit reached". Matched anywhere in there, the answer was thrown away
// as a spent plan and the entry sat out for every seat on the node — and the
// redelivered turn wrote the same sentence a minute later. On a run the harness
// did not report failed, a sentinel counts only where it opens what was said.
//
// Mutation: match a clean run's answer anywhere ([haystack.find] ignoring
// anchored), and every case here is thrown away and sits the entry out.
func TestAnAnswerThatMentionsALimitIsAnAnswer(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		agent  string
		stdout string
		// want is what must come back in the answer.
		want string
	}{
		"copilot, on backing off the GitHub API": {
			agent:  "copilot",
			stdout: "We back off when we hit the GitHub rate limit, then retry with jitter.",
			want:   "rate limit",
		},
		"claude-code, mid-paragraph": {
			agent: "claude-code",
			stdout: claudeResult("Here is the banner copy for the quota feature.\n\n" +
				"Usage limit reached. Upgrade your plan to keep going."),
			want: "Usage limit reached",
		},
		"cursor-agent, on a Kubernetes ResourceQuota": {
			agent:  "cursor-agent",
			stdout: `{"result":"Add a ResourceQuota so the namespace cannot exceed its quota of 4 CPUs."}`,
			want:   "quota",
		},
		"codex, on a client's retry policy": {
			agent: "codex",
			stdout: `{"type":"item.completed","item":{"type":"agent_message",` +
				`"text":"The client sleeps until the reset once the rate limit reached zero."}}`,
			want: "rate limit reached",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := fakeProvider(t, map[string]string{"FAKE_STDOUT": tc.stdout}, shippedShape(t, tc.agent))
			comp, err := ask(t, p, llm.Request{})
			if err != nil {
				t.Fatalf("the model's answer was taken for the vendor's report: %v", err)
			}
			if !strings.Contains(comp.Content, tc.want) {
				t.Errorf("came back %q, want the answer carrying %q", comp.Content, tc.want)
			}
			if err := p.SittingOut(); err != nil {
				t.Errorf("an answer sat the entry out: %v", err)
			}
		})
	}
}

// THE VENDOR'S OWN REPORT STILL CLASSIFIES AND STILL SITS THE ENTRY OUT —
// standing where the answer should be on a clean exit, anywhere in a run the
// harness reported failed, and anywhere on stderr. The rule above is only safe
// beside this half: a rule that stopped recognising a spent plan would leave
// every seat on the entry relaunching the harness to be refused again.
//
// Mutation: drop the failed-run exception (search what was said only at its
// start), and the relayed API error stops classifying; drop the anchored match
// altogether, and Claude Code's own line does.
func TestTheVendorsOwnReportStillSitsTheEntryOut(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		agent string
		env   map[string]string
	}{
		"claude-code's spent plan, standing as its answer": {
			agent: "claude-code",
			env:   map[string]string{"FAKE_STDOUT": claudeResult("Usage limit reached · resets 3pm (Europe/Berlin)")},
		},
		"claude-code's relayed API error, in a run it reported failed": {
			agent: "claude-code",
			env: map[string]string{"FAKE_EXIT": "1", "FAKE_STDOUT": `{"type":"result","is_error":true,` +
				`"result":"API Error: 429 {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",` +
				`\"message\":\"Number of request tokens has exceeded your per-minute rate limit\"}}"}`},
		},
		"copilot's report on stderr": {
			agent: "copilot",
			env:   map[string]string{"FAKE_EXIT": "1", "FAKE_STDERR": "Error: you have hit the rate limit for this model\n"},
		},
		"cursor-agent's report in a failed run": {
			agent: "cursor-agent",
			env:   map[string]string{"FAKE_EXIT": "1", "FAKE_STDOUT": `{"result":"Error: your usage limit for this month has been reached"}`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := fakeProvider(t, tc.env, shippedShape(t, tc.agent))
			if _, err := ask(t, p, llm.Request{}); llm.KindOf(err) != llm.KindRateLimit {
				t.Fatalf("the vendor's report answered %v, want a rate limit", err)
			}
			if err := p.SittingOut(); err == nil {
				t.Error("the vendor's report did not sit the entry out")
			}
		})
	}
}

// A SIT-OUT SAYS WHERE THE VENDOR SAID IT. It takes the entry from every seat
// on the node, and the one question an operator who doubts one has is whether
// the vendor said it or the model did: the line names the channel the
// sentinel matched in beside the line it matched.
//
// NOT PARALLEL: it reads the process's log, which every parallel case writes
// to, and a sequential test runs with none of them.
//
// Mutation: drop the source from the line, and this is red.
func TestASitOutSaysWhereTheVendorSaidIt(t *testing.T) {
	logs := captureLogs(t)
	for name, tc := range map[string]struct {
		env    map[string]string
		source markerSource
	}{
		"standing as the answer": {
			env:    map[string]string{"FAKE_STDOUT": claudeResult("Usage limit reached · resets 3pm (Europe/Berlin)")},
			source: sourceReport,
		},
		"on stderr": {
			env:    map[string]string{"FAKE_EXIT": "1", "FAKE_STDERR": "Usage limit reached · resets 3pm (Europe/Berlin)\n"},
			source: sourceStderr,
		},
	} {
		logs.Reset()
		p := fakeProvider(t, tc.env, shippedShape(t, "claude-code"))
		if _, err := ask(t, p, llm.Request{}); llm.KindOf(err) != llm.KindRateLimit {
			t.Fatalf("%s: answered %v, want a rate limit", name, err)
		}
		got := logs.String()
		for _, want := range []string{"harness_limit_reported", "source=" + string(tc.source), "Usage limit reached"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: the sit-out logged %q, want it to carry %q", name, got, want)
			}
		}
	}
}
