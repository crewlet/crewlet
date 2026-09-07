package cliagent

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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
	comp, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: claudeCodeAnswer})
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
	comp, err := prov.completion(t.Context(), "a prompt of some length", &rawResult{stdout: `{"result":"hi"}`})
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
	_, err = prov.completion(t.Context(), "prompt", &rawResult{
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
	_, err := p.completion(t.Context(), "prompt", &rawResult{
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
	comp, err := p.completion(t.Context(), "prompt", &rawResult{
		stdout: "the answer", stderr: "noise", droppedStderr: 4096,
	})
	if err != nil {
		t.Fatalf("a clipped stderr refused a good answer: %v", err)
	}
	if comp.Content != "the answer" {
		t.Errorf("content = %q", comp.Content)
	}
}

// ---------------------------------------------------------------------------
// Absent versus empty: the distinction that decides whether a vendor's own
// telemetry can be spoken as an agent.
// ---------------------------------------------------------------------------

// The observed regression, verbatim. Claude Code exits 0, reports success and
// leaves `result` EMPTY — the whole answer went into hidden reasoning, which
// the token breakdown here shows: 627 output tokens, 537 of them thinking.
//
// Pinned verbatim rather than reduced to `{"result":""}` because the point is
// the SHAPE: an envelope this large, parsed and handed back as an answer, is
// what a reader saw on the dashboard as the sentence their CEO agent had
// spoken, and what the reviewer then judged the turn on.
const claudeCodeEmptyAnswer = `{"duration_api_ms":11377,"stop_reason":"end_turn",
"session_id":"59756a06-ceb5-417c-a88e-601097145e01","total_cost_usd":0.0348807,
"usage":{"input_tokens":9,"cache_creation_input_tokens":10525,
"cache_read_input_tokens":11308,"output_tokens":627,
"output_tokens_details":{"thinking_tokens":537},"service_tier":"standard"},
"permission_denials":[],"is_error":false,"num_turns":1,"subtype":"success",
"api_error_status":null,"result":"","type":"result","duration_ms":10207}`

// An empty answer is an EMPTY ANSWER, never the envelope that carried it —
// and never a transport fault either.
//
// Two invariants in one case, because they are the same decision seen from
// two sides: what the envelope must NOT become (the sentence a seat speaks),
// and what an empty answer IS (a completion with no content, charged, for the
// tool loop to correct — the shape both API backends return for a model that
// thought and said nothing).
func TestAnEmptyResultIsAChargedCompletionAndNotTheCLIsOwnTelemetry(t *testing.T) {
	t.Parallel()
	p, err := Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := extract(p, claudeCodeEmptyAnswer)
	if got.text != "" {
		t.Errorf("text = %q, want the empty answer the CLI actually gave", got.text)
	}
	if !got.located {
		t.Error("`result` is present in the output, so the profile DID locate the answer")
	}

	prov := &Provider{profile: p, model: "haiku", agent: "claude-code", key: "default"}
	comp, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: claudeCodeEmptyAnswer})
	if err != nil {
		t.Fatalf("an empty answer was reported as a failure: %v", err)
	}
	if comp.Content != "" {
		t.Errorf("content = %q, want the empty answer — the envelope that carried "+
			"it is not the model's reply", comp.Content)
	}
	if len(comp.ToolCalls) != 0 {
		t.Errorf("tool calls = %v, want none", comp.ToolCalls)
	}
	// CHARGED. The round burned 627 output tokens against the subscription
	// whether or not it said anything, and the branch this replaced
	// returned before the usage was ever attached — so an empty answer was
	// the one outcome that spent tokens no budget ever saw.
	if comp.OutputTokens != 627 {
		t.Errorf("output tokens = %d, want 627 — an empty answer still costs", comp.OutputTokens)
	}
	if want := 9 + 10525 + 11308; comp.InputTokens != want {
		t.Errorf("input tokens = %d, want %d", comp.InputTokens, want)
	}
}

// A profile that no longer matches its CLI fails NAMING THE FIELD TO CHANGE,
// rather than passing the CLI's output off as the model's words.
func TestADriftedProfileFailsAndNamesTheOverride(t *testing.T) {
	t.Parallel()
	p := Profile{Output: OutputJSON, TextPaths: []Path{{"result"}}}
	// The vendor renamed the field: valid JSON, nothing where we look.
	const drifted = `{"reply":"the answer nobody found","type":"result"}`

	if got := extract(p, drifted); got.located {
		t.Fatal("a document with no matching path reported that it located the answer")
	}

	prov := &Provider{profile: p, model: "m", agent: "codex", key: "coding"}
	if _, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: drifted}); err == nil {
		t.Fatal("a drifted profile produced a completion")
	} else {
		for _, want := range []string{"text_paths", "result", "crewlet llm doctor",
			"providers.llm.coding.cli.overrides.text_paths"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the message never mentions %q — an operator cannot act on it:\n%v",
					want, err)
			}
		}
		// The shape belongs IN the message: it is the only place the
		// person who can write the override will read it.
		if !strings.Contains(err.Error(), "the answer nobody found") {
			t.Errorf("the message does not show what the CLI printed:\n%v", err)
		}
		if got := llm.KindOf(err); got != llm.KindServer {
			t.Errorf("kind = %v, want server", got)
		}
	}
}

// text_paths is a LIST so a vendor that moved the field needs no override, and
// cursor-agent's [["result"], ["response"]] depends on an empty `result`
// falling through. An empty hit must not end the walk.
func TestAnEmptyTextPathFallsThroughToTheNextOne(t *testing.T) {
	t.Parallel()
	p := Profile{Output: OutputJSON, TextPaths: []Path{{"result"}, {"response"}}}
	got := extract(p, `{"result":"","response":"the answer"}`)
	if got.text != "the answer" {
		t.Errorf("text = %q, want the later path's value", got.text)
	}
	if !got.located {
		t.Error("a resolved path was not reported as located")
	}
}

// The same distinction on a stream: an event carrying the path with an empty
// fragment is an ordinary part of one answer, not a shape this build fails to
// recognise.
func TestAStreamIsLocatedByAnyEventCarryingThePath(t *testing.T) {
	t.Parallel()
	p := Profile{Output: OutputJSONL, TextPaths: []Path{{"item", "text"}}}
	got := extract(p, strings.Join([]string{
		`{"item":{"text":""}}`,
		`{"unrelated":"event"}`,
		`{"item":{"text":"the answer"}}`,
	}, "\n"))
	if got.text != "the answer" {
		t.Errorf("text = %q — an empty fragment must not break the join", got.text)
	}
	if !got.located {
		t.Error("a stream whose events carry the path was not located")
	}
}

// A stream this profile does not recognise is a FAILURE, not a completion
// whose content is the CLI's own event log.
func TestAnUnrecognisedStreamIsAFailureRatherThanItsOwnLog(t *testing.T) {
	t.Parallel()
	p := Profile{Output: OutputJSONL, TextPaths: []Path{{"item", "text"}}}
	const log = `{"event":"started"}` + "\n" + `{"event":"finished","output":"hello"}`

	if got := extract(p, log); got.located || got.text != "" {
		t.Fatalf("an unrecognised stream extracted text = %q located = %v", got.text, got.located)
	}
	prov := &Provider{profile: p, model: "m", agent: "codex", key: "coding"}
	if _, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: log}); err == nil {
		t.Fatal("an unrecognised stream produced a completion")
	} else if !strings.Contains(err.Error(), "text_paths") {
		t.Errorf("the message does not name the field to change: %v", err)
	}
}

// Nothing on stdout at all is its OWN message: a reader told "the field is
// empty" about output that does not exist goes looking for a shape that was
// never printed.
func TestNoOutputAtAllIsReportedAsNoOutput(t *testing.T) {
	t.Parallel()
	p, err := Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prov := &Provider{profile: p, model: "haiku", agent: "claude-code", key: "default"}
	_, err = prov.completion(t.Context(), "prompt", &rawResult{stdout: "  \n ", stderr: "node: bad flag"})
	if err == nil {
		t.Fatal("a silent CLI produced a completion")
	}
	if !strings.Contains(err.Error(), "printed nothing at all") {
		t.Errorf("message = %v", err)
	}
	if !strings.Contains(err.Error(), "node: bad flag") {
		t.Errorf("stderr was dropped, which is the only clue there is: %v", err)
	}
	if got := llm.KindOf(err); got != llm.KindServer {
		t.Errorf("kind = %v, want server", got)
	}
}

// A spent plan is the vendor's own sentence, and whether THIS build's profile
// can find the answer field says nothing about whether the vendor said it.
// Classified off the whole of stdout, so a drifted profile still yields a real
// rate limit rather than burning the chain's next member on a server fault.
func TestASpentPlanIsRecognisedEvenWhenTheAnswerIsNotLocated(t *testing.T) {
	t.Parallel()
	p, err := Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prov := &Provider{profile: p, model: "haiku", agent: "claude-code", key: "default"}
	// Valid JSON with no `result` field: nothing for text_paths to find, and
	// the vendor's sentinel sitting in a field this build never reads.
	_, err = prov.completion(t.Context(), "prompt", &rawResult{
		stdout: `{"note":"Usage limit reached \u00b7 continuing automatically"}`})
	if err == nil {
		t.Fatal("a spent plan produced a completion")
	}
	if got := llm.KindOf(err); got != llm.KindRateLimit {
		t.Fatalf("kind = %v, want rate limit", got)
	}
}

// Where a vendor DOES carry the reset instant beside its sentinel, the
// classification yields a real Retry-After rather than falling back to the
// pool's configured cooldown.
//
// Declared here rather than taken from a shipped profile: no built-in CLI
// still emits a machine-readable reset — Claude Code's pipe-and-epoch wording
// went with its 1.x prose — so pinning this to one would be pinning it to a
// string no vendor prints, which is how the sentinel it replaced went stale
// unnoticed. The mechanism is live and operator-reachable through
// `cli.overrides`, so it is tested on its own terms.
func TestAMarkerThatCarriesAResetInstantYieldsARealRetryAfter(t *testing.T) {
	t.Parallel()
	p, err := Load("claude-code", map[string]any{
		"limit_markers": []any{map[string]any{
			"sentinel": "quota spent", "reset_separator": "|", "reset_unit": "epoch",
		}},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prov := &Provider{profile: p, model: "haiku", agent: "claude-code", key: "default"}
	// Computed rather than pinned: the profile reads it as a Unix epoch, so a
	// literal would quietly become a reset in the past and turn this into a
	// test that asserts nothing.
	reset := time.Now().Add(90 * time.Minute).Unix()
	_, err = prov.completion(t.Context(), "prompt", &rawResult{stdout: fmt.Sprintf(
		`{"note":"quota spent|%d"}`, reset)})
	if err == nil {
		t.Fatal("a spent plan produced a completion")
	}
	var failure *llm.Error
	if !errors.As(err, &failure) {
		t.Fatalf("not a classified failure: %v", err)
	}
	if failure.Kind != llm.KindRateLimit {
		t.Errorf("kind = %v, want rate limit", failure.Kind)
	}
	if failure.RetryAfter <= 0 {
		t.Error("the reset instant the sentinel carried was not read")
	}
}
