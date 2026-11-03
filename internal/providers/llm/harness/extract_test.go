package harness

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// A real Claude Code answer, trimmed to the fields the profile reads. Pinned
// verbatim rather than hand-written so the built-in profile is measured
// against what the vendor actually emits — a synthetic sample would agree
// with the profile by construction and prove nothing.
//
// The stream's LAST event, after the run's other events: the result event is
// the very object `--output-format json` printed alone, on one line of the
// stream-json record the profile reads now (both measured on 2.1.310).
const claudeCodeAnswer = `{"type":"system","subtype":"init","model":"claude-sonnet-4-5"}` + "\n" +
	`{"is_error":false,"duration_api_ms":2046,"num_turns":1,` +
	`"stop_reason":"end_turn","session_id":"3f2a","total_cost_usd":0.0373944,` +
	`"usage":{"input_tokens":2,"cache_creation_input_tokens":7319,` +
	`"cache_read_input_tokens":35502,"output_tokens":5,"service_tier":"standard"},` +
	`"permission_denials":[],"subtype":"success","result":"PONG","type":"result"}` + "\n"

func TestTheClaudeCodeProfileReadsARealAnswer(t *testing.T) {
	t.Parallel()
	p, err := harnessprofile.Load("claude-code", nil)
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
	p, err := harnessprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prov := &Provider{profile: p, model: "sonnet", now: time.Now}
	comp, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: claudeCodeAnswer}, "")
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

// AN INPUT COUNT THAT ALREADY HOLDS THE CACHE IS CHARGED ONCE. Anthropic's
// input_tokens leaves the cached tokens out and OpenAI's counts them in, and
// the two name their fields alike: read the second way round, every cached
// token was charged twice. The profile says which arithmetic its vendor uses,
// and the default is the first.
func TestAnInputCountThatHoldsTheCacheIsChargedOnce(t *testing.T) {
	t.Parallel()
	const answer = `{"result":"ok","usage":{"input":1000,"cached":600,"written":40,"output":50}}`
	for _, tc := range []struct {
		includes bool
		want     int
	}{
		{includes: false, want: 1000 + 600 + 40},
		{includes: true, want: 1000},
	} {
		p := harnessprofile.Profile{
			Output: harnessprofile.OutputJSON, TextPaths: []harnessprofile.Path{{"result"}},
			Usage: harnessprofile.UsagePaths{
				Input:              []harnessprofile.Path{{"usage", "input"}},
				Output:             []harnessprofile.Path{{"usage", "output"}},
				CacheRead:          []harnessprofile.Path{{"usage", "cached"}},
				CacheWrite:         []harnessprofile.Path{{"usage", "written"}},
				InputIncludesCache: tc.includes,
			},
		}
		prov := &Provider{profile: p, model: "m", now: time.Now}
		comp, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: answer}, "")
		if err != nil {
			t.Fatalf("completion: %v", err)
		}
		if comp.InputTokens != tc.want || comp.TotalTokens() != tc.want+50 {
			t.Errorf("input_includes_cache %v: InputTokens = %d, total %d, want %d and %d",
				tc.includes, comp.InputTokens, comp.TotalTokens(), tc.want, tc.want+50)
		}
		// The breakdown is the vendor's either way: what the cache served and
		// wrote is reported as it said.
		if comp.CacheRead != 600 || comp.CacheWrite != 40 {
			t.Errorf("input_includes_cache %v: cache read %d write %d, want 600 and 40",
				tc.includes, comp.CacheRead, comp.CacheWrite)
		}
	}
	// An operator declares it in overrides like any usage field, and the
	// paths it sits beside stay as the profile had them.
	p, err := harnessprofile.Load("claude-code", map[string]any{"usage": map[string]any{"input_includes_cache": true}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !p.Usage.InputIncludesCache || len(p.Usage.Input) == 0 || len(p.Usage.CacheRead) == 0 {
		t.Errorf("usage after the override = %+v, want the declaration beside the built-in paths", p.Usage)
	}
}

// A JSONL stream spells one answer across several events, and reports a
// RUNNING total: text concatenates, usage takes the last value.
func TestAStreamConcatenatesTextAndTakesTheFinalUsage(t *testing.T) {
	t.Parallel()
	p := harnessprofile.Profile{
		Output:    harnessprofile.OutputJSONL,
		TextPaths: []harnessprofile.Path{{"item", "text"}, {"msg", "message"}},
		Usage: harnessprofile.UsagePaths{
			Input:  []harnessprofile.Path{{"msg", "info", "total_token_usage", "input_tokens"}},
			Output: []harnessprofile.Path{{"msg", "info", "total_token_usage", "output_tokens"}},
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

// A harness that printed a banner before its JSON is common, and an operator
// should not need an override for it.
func TestAJSONAnswerIsFoundBehindABanner(t *testing.T) {
	t.Parallel()
	p := harnessprofile.Profile{Output: harnessprofile.OutputJSON, TextPaths: []harnessprofile.Path{{"result"}}}
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
	p := harnessprofile.Profile{Output: harnessprofile.OutputJSON, TextPaths: []harnessprofile.Path{{"result"}}}
	got := extract(p, `{"result":"hi"}`)
	if got.reported {
		t.Fatal("a profile that read no usage claimed it had")
	}

	prov := &Provider{profile: p, model: "m", now: time.Now}
	comp, err := prov.completion(t.Context(), "a prompt of some length", &rawResult{stdout: `{"result":"hi"}`}, "")
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
	p := harnessprofile.Profile{Output: harnessprofile.OutputText}
	if got := extract(p, "  plain prose  \n"); got.text != "plain prose" {
		t.Errorf("text = %q", got.text)
	}
}

// A vendor that reports a failure INSIDE a zero exit must not be read as a
// successful answer — and, saying nothing a marker recognises, it is a SERVER
// failure the seat's chain walks past, not a fatal that stops it: the next
// member is another login or vendor, which this run's failure says nothing
// about.
//
// Mutation: classify a failed run as fatal again, and the kind is wrong.
func TestAFailureFlagInsideAZeroExitIsAServerFailure(t *testing.T) {
	t.Parallel()
	p, err := harnessprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prov := &Provider{profile: p, model: "sonnet", harness: "claude-code", now: time.Now}
	_, err = prov.completion(t.Context(), "prompt", &rawResult{
		stdout: `{"is_error":true,"result":"the model refused","type":"result"}`,
	}, "")
	if err == nil {
		t.Fatal("a harness-reported failure was returned as a completion")
	}
	if got := llm.KindOf(err); got != llm.KindServer {
		t.Errorf("kind = %v, want server", got)
	}
	if !strings.Contains(err.Error(), "the model refused") {
		t.Errorf("err = %v, want what the harness said", err)
	}
	if err := prov.SittingOut(); err != nil {
		t.Errorf("a failed run sat the entry out: %v", err)
	}
}

// Paths index into arrays as well as objects, because vendors put the answer
// in a list of content blocks as often as in a field.
func TestPathsIndexIntoArrays(t *testing.T) {
	t.Parallel()
	p := harnessprofile.Profile{Output: harnessprofile.OutputJSON, TextPaths: []harnessprofile.Path{{"content", "0", "text"}}}
	got := extract(p, `{"content":[{"text":"block one"},{"text":"block two"}]}`)
	if got.text != "block one" {
		t.Errorf("text = %q", got.text)
	}
}

// A COUNT UNDER EVERY MEMBER IS THEIR SUM, for a harness that keys its counts by
// something no profile can name in advance — the model that served each
// request. A member without the count adds nothing, an array's elements are
// members too, and a path no member resolves is unread usage, estimated,
// rather than a zero.
func TestAUsageCountUnderEveryMemberIsTheirSum(t *testing.T) {
	t.Parallel()
	p := harnessprofile.Profile{
		Output:    harnessprofile.OutputJSON,
		TextPaths: []harnessprofile.Path{{"response"}},
		Usage: harnessprofile.UsagePaths{
			Input:     []harnessprofile.Path{{"models", harnessprofile.PathEvery, "tokens", "input"}},
			Output:    []harnessprofile.Path{{"models", harnessprofile.PathEvery, "tokens", "output"}},
			CacheRead: []harnessprofile.Path{{"steps", harnessprofile.PathEvery, "cached"}},
		},
	}
	got := extract(p, `{"response":"hi","models":{
		"big":{"tokens":{"input":100,"output":7}},
		"small":{"tokens":{"input":20,"output":"3"}},
		"idle":{"tokens":{}}},
		"steps":[{"cached":5},{"other":1},{"cached":6}]}`)
	if !got.reported {
		t.Fatal("usage under every member went unread")
	}
	if got.input != 120 || got.output != 10 || got.cacheRead != 11 {
		t.Errorf("usage = in %d out %d cached %d, want 120 10 11", got.input, got.output, got.cacheRead)
	}

	none := extract(p, `{"response":"hi","models":{"big":{"api":{}}}}`)
	if none.reported {
		t.Errorf("no member carried a count, and usage was reported as in %d out %d",
			none.input, none.output)
	}
}

// A MODEL-SIDE TOOL'S PROMPT IS CHARGED AS INPUT. Gemini reports the pages a
// web fetch put in front of the model apart from the prompt count, and bills
// them as input: a fetching call charged only its question would be charged a
// fraction of what it read.
func TestAToolsPromptIsChargedAsInput(t *testing.T) {
	t.Parallel()
	p := harnessprofile.Profile{
		Output:    harnessprofile.OutputJSON,
		TextPaths: []harnessprofile.Path{{"response"}},
		Usage: harnessprofile.UsagePaths{
			Input:      []harnessprofile.Path{{"usage", "input"}},
			Output:     []harnessprofile.Path{{"usage", "output"}},
			CacheRead:  []harnessprofile.Path{{"usage", "cached"}},
			ToolPrompt: []harnessprofile.Path{{"usage", "tool"}},
		},
	}
	stdout := `{"response":"hi","usage":{"input":40,"output":2,"cached":100,"tool":5000}}`
	prov := &Provider{profile: p, model: "m", now: time.Now}
	comp, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: stdout}, "")
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	if comp.InputTokens != 5140 || comp.CacheRead != 100 || comp.OutputTokens != 2 {
		t.Errorf("completion = in %d cached %d out %d, want 5140 100 2",
			comp.InputTokens, comp.CacheRead, comp.OutputTokens)
	}

	// Read wherever usage is: the last event of a stream, and a usage
	// report the harness is asked for after the call.
	stream := p
	stream.Output = harnessprofile.OutputJSONL
	if got := extract(stream, `{"usage":{"tool":1}}`+"\n"+stdout); got.toolPrompt != 5000 {
		t.Errorf("a stream's tool prompt = %d, want the last event's 5000", got.toolPrompt)
	}
	report := p
	report.UsageArgs = []string{"usage"}
	var fromReport extracted
	if outcome := fromReport.applyUsageReport(report, stdout); outcome != usageApplied || fromReport.toolPrompt != 5000 {
		t.Errorf("a usage report's tool prompt = %d (%v), want 5000", fromReport.toolPrompt, outcome)
	}
}

// The cap exists so one runaway harness cannot put the engine under memory
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

	p := &Provider{profile: harnessprofile.Profile{}, model: "m", now: time.Now}
	_, err := p.completion(t.Context(), "prompt", &rawResult{
		stdout: buf.String(), droppedStdout: buf.Truncated(),
	}, "")
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
	p := &Provider{profile: harnessprofile.Profile{}, model: "m", now: time.Now}
	comp, err := p.completion(t.Context(), "prompt", &rawResult{
		stdout: "the answer", stderr: "noise", droppedStderr: 4096,
	}, "")
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
const claudeCodeEmptyAnswer = `{"type":"system","subtype":"init","model":"claude-haiku-4-5"}` + "\n" +
	`{"duration_api_ms":11377,"stop_reason":"end_turn",` +
	`"session_id":"59756a06-ceb5-417c-a88e-601097145e01","total_cost_usd":0.0348807,` +
	`"usage":{"input_tokens":9,"cache_creation_input_tokens":10525,` +
	`"cache_read_input_tokens":11308,"output_tokens":627,` +
	`"output_tokens_details":{"thinking_tokens":537},"service_tier":"standard"},` +
	`"permission_denials":[],"is_error":false,"num_turns":1,"subtype":"success",` +
	`"api_error_status":null,"result":"","type":"result","duration_ms":10207}` + "\n"

// An empty answer is an EMPTY ANSWER, never the envelope that carried it —
// and never a transport fault either.
//
// Two invariants in one case, because they are the same decision seen from
// two sides: what the envelope must NOT become (the sentence a seat speaks),
// and what an empty answer IS (a completion with no content, charged, for the
// tool loop to correct — what a model that thought and said nothing gave).
func TestAnEmptyResultIsAChargedCompletionAndNotTheHarnessOwnTelemetry(t *testing.T) {
	t.Parallel()
	p, err := harnessprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := extract(p, claudeCodeEmptyAnswer)
	if got.text != "" {
		t.Errorf("text = %q, want the empty answer the harness actually gave", got.text)
	}
	if !got.located {
		t.Error("`result` is present in the output, so the profile DID locate the answer")
	}

	prov := &Provider{profile: p, model: "haiku", harness: "claude-code", key: "default", now: time.Now}
	comp, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: claudeCodeEmptyAnswer}, "")
	if err != nil {
		t.Fatalf("an empty answer was reported as a failure: %v", err)
	}
	if comp.Content != "" {
		t.Errorf("content = %q, want the empty answer — the envelope that carried "+
			"it is not the model's reply", comp.Content)
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

// A profile that no longer matches its harness fails NAMING THE FIELD TO CHANGE,
// rather than passing the harness's output off as the model's words.
func TestADriftedProfileFailsAndNamesTheOverride(t *testing.T) {
	t.Parallel()
	p := harnessprofile.Profile{Output: harnessprofile.OutputJSON, TextPaths: []harnessprofile.Path{{"result"}}}
	// The vendor renamed the field: valid JSON, nothing where we look.
	const drifted = `{"reply":"the answer nobody found","type":"result"}`

	if got := extract(p, drifted); got.located {
		t.Fatal("a document with no matching path reported that it located the answer")
	}

	prov := &Provider{profile: p, model: "m", harness: "codex", key: "coding", now: time.Now}
	if _, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: drifted}, ""); err == nil {
		t.Fatal("a drifted profile produced a completion")
	} else {
		for _, want := range []string{"text_paths", "result", "crewlet llm doctor",
			"providers.llm.coding.overrides.text_paths"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the message never mentions %q — an operator cannot act on it:\n%v",
					want, err)
			}
		}
		// The shape belongs IN the message: it is the only place the
		// person who can write the override will read it.
		if !strings.Contains(err.Error(), "the answer nobody found") {
			t.Errorf("the message does not show what the harness printed:\n%v", err)
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
	p := harnessprofile.Profile{Output: harnessprofile.OutputJSON, TextPaths: []harnessprofile.Path{{"result"}, {"response"}}}
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
	p := harnessprofile.Profile{Output: harnessprofile.OutputJSONL, TextPaths: []harnessprofile.Path{{"item", "text"}}}
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
// whose content is the harness's own event log.
func TestAnUnrecognisedStreamIsAFailureRatherThanItsOwnLog(t *testing.T) {
	t.Parallel()
	p := harnessprofile.Profile{Output: harnessprofile.OutputJSONL, TextPaths: []harnessprofile.Path{{"item", "text"}}}
	const log = `{"event":"started"}` + "\n" + `{"event":"finished","output":"hello"}`

	if got := extract(p, log); got.located || got.text != "" {
		t.Fatalf("an unrecognised stream extracted text = %q located = %v", got.text, got.located)
	}
	prov := &Provider{profile: p, model: "m", harness: "codex", key: "coding", now: time.Now}
	if _, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: log}, ""); err == nil {
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
	p, err := harnessprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prov := &Provider{profile: p, model: "haiku", harness: "claude-code", key: "default", now: time.Now}
	_, err = prov.completion(t.Context(), "prompt", &rawResult{stdout: "  \n ", stderr: "node: bad flag"}, "")
	if err == nil {
		t.Fatal("a silent harness produced a completion")
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
// Classified off the whole of stdout when no answer was located, so a drifted
// profile still yields a real rate limit — on the terms an answer is read on:
// the vendor's report opens what the harness printed, or the harness said the run
// failed.
//
// And only on those terms. A drifted profile cannot tell the model's words from
// the vendor's either — the model's answer is somewhere in that stdout — so a
// sentinel in a field this build does not read, on a run the harness did not report
// failed, is the profile drift it looks like: a server failure, which still
// moves the chain on, and not a spent plan that sits the entry out.
func TestASpentPlanIsRecognisedEvenWhenTheAnswerIsNotLocated(t *testing.T) {
	t.Parallel()
	p, err := harnessprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for name, tc := range map[string]struct {
		stdout string
		want   llm.ErrorKind
	}{
		// Prose where the document should be, and a document with no
		// `result` field after it: nothing for text_paths to find.
		"the report printed where the document should be": {
			stdout: "Usage limit reached \u00b7 resets 3pm (Europe/Berlin)\n" +
				`{"type":"result","subtype":"error"}`,
			want: llm.KindRateLimit,
		},
		"the sentinel in a run the harness reported failed": {
			stdout: `{"is_error":true,"note":"Usage limit reached \u00b7 continuing automatically"}`,
			want:   llm.KindRateLimit,
		},
		"the sentinel in a field nothing reads, on a clean run": {
			stdout: `{"note":"Usage limit reached \u00b7 continuing automatically"}`,
			want:   llm.KindServer,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// Pinned to reportedAt (00:16 in Berlin), when the case's 3pm
			// reset lies ahead. On the real clock, the case would find no
			// sit-out from 3pm to 3:05 Berlin time, when the reset has
			// passed by less than clockResetGrace.
			prov := &Provider{profile: p, model: "haiku", harness: "claude-code", key: "default",
				now: func() time.Time { return reportedAt }}
			if out := extract(p, tc.stdout); out.located {
				t.Fatalf("the answer was located (%q) — this case is about one that is not", out.text)
			}
			_, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: tc.stdout}, "")
			if got := llm.KindOf(err); err == nil || got != tc.want {
				t.Fatalf("kind = %v (err %v), want %v", got, err, tc.want)
			}
			if sat := prov.SittingOut() != nil; sat != (tc.want == llm.KindRateLimit) {
				t.Errorf("sat out = %v, want it only for a spent plan", sat)
			}
		})
	}
}

// Where a vendor DOES carry the reset instant beside its sentinel, the
// classification yields a real Retry-After rather than falling back to the
// pool's configured cooldown.
//
// Declared here rather than taken from a shipped profile: no built-in harness
// still emits a machine-readable reset — Claude Code's pipe-and-epoch wording
// went with its 1.x prose — so pinning this to one would be pinning it to a
// string no vendor prints, which is how the sentinel it replaced went stale
// unnoticed. The mechanism is live and operator-reachable through
// `overrides`, so it is tested on its own terms.
func TestAMarkerThatCarriesAResetInstantSitsTheEntryOutUntilIt(t *testing.T) {
	t.Parallel()
	p, err := harnessprofile.Load("claude-code", map[string]any{
		"limit_markers": []any{map[string]any{
			"sentinel": "quota spent", "reset_separator": "|", "reset_unit": "epoch",
		}},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prov := &Provider{profile: p, model: "haiku", harness: "claude-code", key: "default", now: time.Now}
	// Computed rather than pinned: the profile reads it as a Unix epoch, so a
	// literal would quietly become a reset in the past and turn this into a
	// test that asserts nothing.
	reset := time.Now().Add(45 * time.Minute).Unix()
	_, err = prov.completion(t.Context(), "prompt", &rawResult{stdout: fmt.Sprintf(
		`{"type":"result","result":"quota spent|%d"}`, reset)}, "")
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
	if left := time.Until(sitOutUntil(prov)); left < 40*time.Minute || left > 46*time.Minute {
		t.Errorf("the entry sits out %v, want the reset instant the sentinel carried", left)
	}
}
