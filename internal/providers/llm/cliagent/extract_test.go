package cliagent

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// A real Claude Code answer, trimmed to the fields the profile reads. Pinned
// verbatim rather than hand-written so the built-in profile is measured
// against what the vendor actually emits — a synthetic sample would agree
// with the profile by construction and prove nothing.
const claudeCodeAnswer = `{"is_error":false,"duration_api_ms":2046,"num_turns":1,
"stop_reason":"end_turn","session_id":"3f2a","total_cost_usd":0.0373944,
"usage":{"input_tokens":2,"cache_creation_input_tokens":7319,
"cache_read_input_tokens":35502,"output_tokens":5,"service_tier":"standard"},
"permission_denials":[],"subtype":"success","result":"PONG","type":"result"}`

func TestTheClaudeCodeProfileReadsARealAnswer(t *testing.T) {
	t.Parallel()
	p, err := Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := extract(p, claudeCodeAnswer)
	if got.text != "PONG" {
		t.Errorf("text = %q, want PONG", got.text)
	}
	if !got.reported {
		t.Fatal("usage went unread, so budgets would run on estimates")
	}
	if got.failed {
		t.Error("a successful answer was read as a failure")
	}
	if got.input != 2 || got.output != 5 || got.cacheRead != 35502 || got.cacheWrite != 7319 {
		t.Errorf("usage = in %d out %d read %d write %d",
			got.input, got.output, got.cacheRead, got.cacheWrite)
	}
}

// The contract's rule: InputTokens is ALWAYS the full prompt count, cache
// included, so it stays a correct budget figure whatever the cache did.
// Reporting only the uncached base would have charged this call 2 tokens for
// a 42,823-token prompt.
func TestInputTokensCarryTheWholePromptIncludingCache(t *testing.T) {
	t.Parallel()
	p, err := Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prov := &Provider{profile: p, model: "sonnet"}
	comp, err := prov.completion("prompt", &rawResult{stdout: claudeCodeAnswer})
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	const want = 2 + 35502 + 7319
	if comp.InputTokens != want {
		t.Errorf("InputTokens = %d, want %d (base + cache read + cache write)",
			comp.InputTokens, want)
	}
	if comp.CacheRead+comp.CacheWrite+2 != comp.InputTokens {
		t.Error("the cache breakdown does not add back up to the total")
	}
	if comp.TotalTokens() != want+5 {
		t.Errorf("TotalTokens = %d", comp.TotalTokens())
	}
}

// A JSONL stream spells one answer across several events, and reports a
// RUNNING total: text concatenates, usage takes the last value.
func TestAStreamConcatenatesTextAndTakesTheFinalUsage(t *testing.T) {
	t.Parallel()
	p := Profile{
		Output:    OutputJSONL,
		TextPaths: []Path{{"item", "text"}, {"msg", "message"}},
		Usage: UsagePaths{
			Input:  []Path{{"msg", "info", "total_token_usage", "input_tokens"}},
			Output: []Path{{"msg", "info", "total_token_usage", "output_tokens"}},
		},
	}
	stream := strings.Join([]string{
		`{"item":{"text":"first"}}`,
		`{"msg":{"info":{"total_token_usage":{"input_tokens":10,"output_tokens":1}}}}`,
		`{"msg":{"message":"second"}}`,
		`not json at all`,
		`{"msg":{"info":{"total_token_usage":{"input_tokens":40,"output_tokens":9}}}}`,
	}, "\n")

	got := extract(p, stream)
	if got.text != "first\nsecond" {
		t.Errorf("text = %q, want both events in order", got.text)
	}
	if got.input != 40 || got.output != 9 {
		t.Errorf("usage = in %d out %d, want the final running total", got.input, got.output)
	}
}

// A CLI that printed a banner before its JSON is common, and an operator
// should not need an override for it.
func TestAJSONAnswerIsFoundBehindABanner(t *testing.T) {
	t.Parallel()
	p := Profile{Output: OutputJSON, TextPaths: []Path{{"result"}}}
	got := extract(p, "Loading model…\n"+`{"result":"the answer"}`)
	if got.text != "the answer" {
		t.Errorf("text = %q", got.text)
	}
}

// A profile whose usage paths find nothing must SAY so, because a budget
// built on estimates is a different promise from one built on the vendor's
// own counts — and `crewlet llm doctor` prints which.
func TestUnreportedUsageIsMarkedRatherThanReportedAsZero(t *testing.T) {
	t.Parallel()
	p := Profile{Output: OutputJSON, TextPaths: []Path{{"result"}}}
	got := extract(p, `{"result":"hi"}`)
	if got.reported {
		t.Fatal("a profile that read no usage claimed it had")
	}

	prov := &Provider{profile: p, model: "m"}
	comp, err := prov.completion("a prompt of some length", &rawResult{stdout: `{"result":"hi"}`})
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	if comp.InputTokens == 0 || comp.OutputTokens == 0 {
		t.Errorf("estimates were not applied: in=%d out=%d", comp.InputTokens, comp.OutputTokens)
	}
}

// Text output is taken verbatim, with no JSON expectations at all.
func TestTextOutputIsTakenVerbatim(t *testing.T) {
	t.Parallel()
	p := Profile{Output: OutputText}
	if got := extract(p, "  plain prose  \n"); got.text != "plain prose" {
		t.Errorf("text = %q", got.text)
	}
}

// A vendor that reports a failure INSIDE a zero exit must not be read as a
// successful answer.
func TestAFailureFlagInsideAZeroExitIsAFailure(t *testing.T) {
	t.Parallel()
	p, err := Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prov := &Provider{profile: p, model: "sonnet"}
	_, err = prov.completion("prompt", &rawResult{
		stdout: `{"is_error":true,"result":"the model refused","type":"result"}`,
	})
	if err == nil {
		t.Fatal("a CLI-reported failure was returned as a completion")
	}
	if got := llm.KindOf(err); got != llm.KindFatal {
		t.Errorf("kind = %v, want fatal", got)
	}
}

// Paths index into arrays as well as objects, because vendors put the answer
// in a list of content blocks as often as in a field.
func TestPathsIndexIntoArrays(t *testing.T) {
	t.Parallel()
	p := Profile{Output: OutputJSON, TextPaths: []Path{{"content", "0", "text"}}}
	got := extract(p, `{"content":[{"text":"block one"},{"text":"block two"}]}`)
	if got.text != "block one" {
		t.Errorf("text = %q", got.text)
	}
}

// The cap exists so one runaway CLI cannot put the engine under memory
// pressure; dropping past it must not lose what came before.
func TestOutputIsCappedWithoutLosingTheStart(t *testing.T) {
	t.Parallel()
	var buf cappedBuffer
	buf.limit = 10
	if _, err := buf.Write([]byte("0123456789abcdef")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if buf.String() != "0123456789" {
		t.Errorf("kept %q", buf.String())
	}
	if buf.Truncated() != 6 {
		t.Errorf("Truncated = %d, want 6", buf.Truncated())
	}
}

// A stderr tail must say that it is a tail, or an operator reads the last
// fifty lines as the whole story.
func TestTheStderrTailSaysWhatItDropped(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := range 120 {
		lines = append(lines, string(rune('a'+i%26)))
	}
	got := tail(strings.Join(lines, "\n"))
	if !strings.Contains(got, "70 earlier lines omitted") {
		t.Errorf("the tail does not say what it dropped:\n%s", got)
	}
	if strings.Count(got, "\n") != 50 {
		t.Errorf("kept %d lines, want 50", strings.Count(got, "\n"))
	}
	// A short one is passed through whole, with no marker.
	if got := tail("one\ntwo"); got != "one\ntwo" {
		t.Errorf("a short stderr was altered: %q", got)
	}
}

// A CLIPPED ANSWER IS NOT AN ANSWER. stdout past the cap is dropped by the
// capped buffer, and the completion path used to parse whatever survived and
// return it — a half-written envelope, a report cut mid-sentence — with
// nothing downstream able to tell it from a model that stopped there.
func TestAClippedStdoutIsRefusedRatherThanParsed(t *testing.T) {
	t.Parallel()
	var buf cappedBuffer
	buf.limit = 64
	body := strings.Repeat("x", 200)
	if _, err := buf.Write([]byte(body)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if buf.Truncated() != len(body)-64 {
		t.Fatalf("Truncated = %d, want %d", buf.Truncated(), len(body)-64)
	}

	p := &Provider{profile: Profile{}, model: "m"}
	_, err := p.completion("prompt", &rawResult{
		stdout: buf.String(), droppedStdout: buf.Truncated(),
	})
	if err == nil {
		t.Fatal("a clipped stdout was parsed and returned as a completion")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("the refusal does not say the answer is incomplete: %v", err)
	}
	// A SERVER failure, so the fallback chain may try another member and the
	// credential is not benched: nothing about the prompt was rejected.
	var fe *llm.Error
	if !errors.As(err, &fe) || fe.Kind != llm.KindServer {
		t.Errorf("kind = %v, want a server failure", err)
	}
}

// stderr overrunning costs diagnosis, not correctness, so it must NOT refuse.
func TestAClippedStderrStillReturnsTheAnswer(t *testing.T) {
	t.Parallel()
	p := &Provider{profile: Profile{}, model: "m"}
	comp, err := p.completion("prompt", &rawResult{
		stdout: "the answer", stderr: "noise", droppedStderr: 4096,
	})
	if err != nil {
		t.Fatalf("a clipped stderr refused a good answer: %v", err)
	}
	if comp.Content != "the answer" {
		t.Errorf("content = %q", comp.Content)
	}
}
