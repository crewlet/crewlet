package cliagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	}, "")
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

// THE CAP NEVER LEAVES THE BUFFER HOLDING INVALID UTF-8. The cut lands where
// the byte count runs out, which for any output that is not ASCII is
// mid-character — a progress frame's box-drawing runes, a model's em dash, a
// non-Latin filename. A split character reaches an operator as U+FFFD through
// the error it is pasted into, and reaches a model the same way.
//
// Every cap position across a string whose runes are 1, 2, 3 and 4 bytes wide,
// so some boundary falls inside each width.
func TestTheCapNeverSplitsACharacter(t *testing.T) {
	t.Parallel()
	const mixed = "aé€𝄞bcé€𝄞"
	for limit := range len(mixed) + 2 {
		for _, chunk := range []int{1, 3, len(mixed)} {
			var buf cappedBuffer
			buf.limit = limit
			// Written in chunks as a pipe hands them over, because a
			// rune straddles two Writes whenever a read splits it: a
			// walk back inside one chunk alone leaves the buffer
			// ending on a lead byte whose remainder was in it.
			for i := 0; i < len(mixed); i += chunk {
				end := min(i+chunk, len(mixed))
				if _, err := buf.Write([]byte(mixed[i:end])); err != nil {
					t.Fatalf("Write: %v", err)
				}
			}
			got := buf.String()
			if !utf8.ValidString(got) {
				t.Errorf("limit %d, chunk %d: kept %q, which is not valid UTF-8",
					limit, chunk, got)
			}
			if !strings.HasPrefix(mixed, got) {
				t.Errorf("limit %d, chunk %d: kept %q, which is not a prefix",
					limit, chunk, got)
			}
			// THE COUNT STAYS HONEST: every byte the reader is not
			// seeing is reported, the split character's included, or
			// the marker that renders it understates the loss.
			if want := len(mixed) - len(got); buf.Truncated() != want {
				t.Errorf("limit %d, chunk %d: Truncated = %d, want %d",
					limit, chunk, buf.Truncated(), want)
			}
		}
	}
}

// ONCE THE CAP HAS TAKEN A BYTE THE BUFFER IS SEALED: what it holds is a
// PREFIX of the stream, not a sampling of it. The rune trim frees room exactly
// the width of the character it removed, and the very next bytes off the pipe
// are that character's own continuation bytes — so a buffer that kept
// accepting would refill the hole with them and put the invalid encoding
// straight back, one byte at a time, on the input shape this type is written
// for. Bytes from past the cut are not contiguous with what precedes them
// either: spliced in, they show an operator a line that never followed the one
// above it.
func TestOnceTheCapHasDroppedTheBufferIsSealed(t *testing.T) {
	t.Parallel()
	// 61 c3a9 e282ac f09d849e: the cap falls one byte into the four-byte
	// rune, so the trim gives back three bytes of room and the three bytes
	// that follow are exactly what it removed.
	const mixed = "aé€𝄞"
	var buf cappedBuffer
	buf.limit = 7
	for i := range len(mixed) {
		if _, err := buf.Write([]byte(mixed[i : i+1])); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if got := buf.String(); got != "aé€" {
		t.Errorf("kept %q, want the whole characters that fit before the cap", got)
	}
	// Every byte the reader is not shown is counted, the trimmed lead byte
	// included, or the marker that renders the loss understates it.
	if want := len(mixed) - len("aé€"); buf.Truncated() != want {
		t.Errorf("Truncated = %d, want %d", buf.Truncated(), want)
	}
	// And nothing arriving later is spliced into the freed room, however
	// well it would fit.
	if _, err := buf.Write([]byte("tail")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := buf.String(); got != "aé€" {
		t.Errorf("a later write was spliced past the cut: %q", got)
	}
	if want := len(mixed) - len("aé€") + len("tail"); buf.Truncated() != want {
		t.Errorf("Truncated = %d, want %d", buf.Truncated(), want)
	}
}

// Output the CLI itself emitted broken is NOT rewritten. Trimming it would be
// this package claiming a cut it did not make, and would put bytes into a
// count that means "what the cap took".
//
// The two shapes that must survive the trim are the two a truncated character
// is not: a byte no UTF-8 encoding begins with, and a run of continuation
// bytes with no lead byte anywhere behind them.
func TestTheCapLeavesTheCLIsOwnBrokenBytesAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		bytes []byte
	}{
		{"an impossible lead byte", []byte{0xff, 0xfe, 0xfd, 0xfc, 0xfb}},
		{"orphaned continuation bytes", []byte{0x80, 0x81, 0x82, 0x83, 0x84}},
		// A lead byte followed by something that cannot continue it is a
		// broken encoding rather than a short one, and stays as it is.
		{"a lead byte with no continuation", []byte{0xe2, 0x41, 0x42, 0x43, 0x44}},
	} {
		var buf cappedBuffer
		buf.limit = 4
		if _, err := buf.Write(tc.bytes); err != nil {
			t.Fatalf("%s: Write: %v", tc.name, err)
		}
		if got := buf.String(); len(got) != 4 {
			t.Errorf("%s: kept %d bytes, want the 4 the cap allowed: %q",
				tc.name, len(got), got)
		}
		if buf.Truncated() != 1 {
			t.Errorf("%s: Truncated = %d, want 1", tc.name, buf.Truncated())
		}
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
	got := tail(strings.Join(lines, "\n"), 0)
	if !strings.Contains(got, "70 earlier lines omitted") {
		t.Errorf("the tail does not say what it dropped:\n%s", got)
	}
	if strings.Count(got, "\n") != 50 {
		t.Errorf("kept %d lines, want 50", strings.Count(got, "\n"))
	}
	// A short one is passed through whole, with no marker.
	if got := tail("one\ntwo", 0); got != "one\ntwo" {
		t.Errorf("a short stderr was altered: %q", got)
	}
}

// THE LINES A TAIL LEAVES OUT ARE WHERE ITS MARKER SAYS.
//
// The marker names a debug event, so every omitted line has to be emitted as
// one — in order, numbered, and none of the lines the window kept. A marker
// naming an event nothing emits sends an operator looking for lines that are
// not there, which is worse than the silence it replaced.
func TestTheLinesATailOmitsAreLoggedWhereItsMarkerSays(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := range 53 {
		lines = append(lines, fmt.Sprintf("line-%02d", i+1))
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got := renderTail(t.Context(), logger, "stderr", strings.Join(lines, "\n"), 0)
	if !strings.Contains(got, "3 earlier lines omitted") || !strings.Contains(got, omittedLineEvent) {
		t.Fatalf("the marker does not count the omission or say where it went:\n%s", got)
	}
	var emitted []string
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec struct {
			Msg    string `json:"msg"`
			Stream string `json:"stream"`
			LineNo int    `json:"line_no"`
			Line   string `json:"line"`
		}
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			t.Fatalf("an emitted record is not JSON: %v: %q", err, raw)
		}
		if rec.Msg != omittedLineEvent || rec.Stream != "stderr" || rec.LineNo != len(emitted)+1 {
			t.Errorf("record %d = %+v, want %s on stderr numbered in order", len(emitted), rec, omittedLineEvent)
		}
		emitted = append(emitted, rec.Line)
	}
	if strings.Join(emitted, ",") != "line-01,line-02,line-03" {
		t.Errorf("logged %v, want exactly the three lines the window left out", emitted)
	}

	// A stream that fits emits nothing: there is nothing it left out.
	buf.Reset()
	renderTail(t.Context(), logger, "stderr", "one\ntwo", 0)
	if buf.Len() != 0 {
		t.Errorf("a whole stream logged lines it did not omit:\n%s", buf.String())
	}
}

// A STREAM CUT AT THE CAP HAS NO END LEFT TO TAKE A TAIL FROM. The capped
// buffer keeps the HEAD, so the last lines of what survived are where the cap
// fell rather than what the CLI last said — and a crash trace's crash is
// exactly the part that goes missing. Unmarked it reads as a CLI that simply
// stopped talking, which sends an operator looking for a hang.
func TestTheTailSaysWhenTheCapTookTheEnd(t *testing.T) {
	t.Parallel()
	got := tail("a\nb", 4096)
	if !strings.Contains(got, "4096") {
		t.Errorf("the tail does not say how much the cap took:\n%s", got)
	}
	if !strings.Contains(got, "where the cap fell") {
		t.Errorf("the tail does not say these are not the CLI's last words:\n%s", got)
	}
	if !strings.HasPrefix(got, "a\nb") {
		t.Errorf("the marker displaced the output it annotates:\n%s", got)
	}
}

// And every operator-facing message that renders a stream carries that mark,
// because [tail] cannot know it unless the caller pairs the text with ITS OWN
// drop count — the mistake the rawResult methods exist to make unwritable.
func TestEveryRenderedStreamCarriesItsOwnDropCount(t *testing.T) {
	t.Parallel()
	res := &rawResult{
		stdout: "out", stderr: "err",
		droppedStdout: 11, droppedStderr: 22,
	}
	for _, tc := range []struct {
		name string
		got  string
		want string
		not  string
	}{
		{"stderr", res.stderrTailText(t.Context()), "22", "11"},
		{"stdout", res.stdoutTailText(t.Context()), "11", "22"},
		{"stderr detail", res.stderrDetailText(t.Context()), "22", "11"},
		// stderr has content, so the failure tail is stderr's and must
		// not be annotated with stdout's loss.
		{"failure picks stderr", res.failureTailText(t.Context(), "parsed"), "22", "11"},
	} {
		if !strings.Contains(tc.got, tc.want) {
			t.Errorf("%s: does not report its own drop count %s:\n%s",
				tc.name, tc.want, tc.got)
		}
		if strings.Contains(tc.got, tc.not) {
			t.Errorf("%s: reports the other stream's drop count %s:\n%s",
				tc.name, tc.not, tc.got)
		}
	}
	// With nothing on stderr the failure tail falls to what was parsed out
	// of stdout.
	quiet := &rawResult{stdout: "out", droppedStdout: 11}
	if got := quiet.failureTailText(t.Context(), "parsed"); !strings.Contains(got, "11") {
		t.Errorf("a parsed answer did not inherit stdout's drop count:\n%s", got)
	}
}

// AND THE CLASSIFIED FAILURE IS A RENDERED STREAM TOO, which is the caller
// that was quoting one line of a possibly-clipped stream with no mark on it at
// all — a blanket "every caller does" in [cappedBuffer]'s own doc that this
// package's own code contradicted.
//
// THE STREAM IS ASKED, NOT INFERRED: the hit names the haystack it matched in,
// so the line cannot be annotated with the other stream's loss.
func TestAClassifiedMarkerCarriesItsOwnStreamsDropCount(t *testing.T) {
	t.Parallel()
	res := &rawResult{
		stdout:        "spent: plan exhausted\nand a transcript after it",
		stderr:        "auth: token expired\nand a trace after it",
		droppedStdout: 11, droppedStderr: 22,
	}
	for _, tc := range []struct {
		name string
		hit  markerHit
		want string
		not  string
	}{
		{"stdout", markerHit{Said: "spent: plan exhausted", Stream: markerStdout}, "11", "22"},
		{"stderr", markerHit{Said: "auth: token expired", Stream: markerStderr}, "22", "11"},
	} {
		got := res.markerText(tc.hit, res.stdout)
		if !strings.Contains(got, tc.hit.Said) {
			t.Errorf("%s: the vendor's own sentence is gone:\n%s", tc.name, got)
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: does not report its own drop count %s:\n%s", tc.name, tc.want, got)
		}
		if strings.Contains(got, tc.not) {
			t.Errorf("%s: reports the other stream's drop count %s:\n%s", tc.name, tc.not, got)
		}
		if !strings.Contains(got, string(tc.hit.Stream)) {
			t.Errorf("%s: does not name the stream the line came out of:\n%s", tc.name, got)
		}
		// MARKED. A selection a reader cannot tell from a whole reply
		// fails the same way an unmarked cut does.
		if !strings.Contains(got, "…") {
			t.Errorf("%s: one line of a longer stream is not marked as one:\n%s", tc.name, got)
		}
		// AND HONEST ABOUT WHERE THE REST IS, which is nowhere: a CLI's
		// streams are read once and dropped, so an invented route would
		// be worse than the admitted gap.
		if !strings.Contains(got, "kept nowhere") {
			t.Errorf("%s: does not say the remainder is unrecoverable:\n%s", tc.name, got)
		}
	}

	// AND SAYS NONE OF IT WHERE THE LINE *IS* THE STREAM. A standing clause
	// about a remainder that does not exist on every rate limit is how a
	// reader learns to skip the sentence it hangs off.
	clean := &rawResult{stderr: "auth: token expired\n"}
	got := clean.markerText(markerHit{Said: "auth: token expired", Stream: markerStderr}, "")
	if got != "auth: token expired" {
		t.Errorf("a line that is the whole stream was annotated anyway: %q", got)
	}
}

// AND THE QUANTITY IT REPORTS IS THE STREAM'S, NEVER THE EXTRACTED ANSWER'S.
// [Provider.completion] hands the classifier the answer the extractor located,
// so on a JSONL profile the sentinel matches inside a few hundred bytes of
// answer that came out of thousands of bytes of transcript. Measuring the
// answer states the wrong quantity about stdout — and where the answer IS the
// matched line it drops the mark entirely, which is the unmarked selection
// [rawResult.markerText] exists to prevent.
func TestAMarkerFoundInAnExtractedAnswerIsMeasuredAgainstItsStream(t *testing.T) {
	t.Parallel()

	answer := "spent: your plan is exhausted"
	transcript := `{"type":"run.lifecycle.started"}` + "\n" +
		strings.Repeat(`{"type":"tool.progress","text":"working"}`+"\n", 40) +
		`{"type":"assistant","text":"spent: your plan is exhausted"}` + "\n"
	res := &rawResult{stdout: transcript}

	got := res.markerText(markerHit{Said: answer, Stream: markerStdout}, answer)
	if !strings.Contains(got, answer) {
		t.Fatalf("the vendor's own sentence is gone:\n%s", got)
	}
	// MARKED, although the located answer is the whole of the matched line:
	// stdout held the transcript around it, and a reader cannot otherwise
	// tell this line from the whole of what the CLI wrote.
	if !strings.Contains(got, "…") {
		t.Errorf("one line out of a transcript is not marked as one:\n%s", got)
	}
	if want := fmt.Sprintf("%d bytes on stdout", len(transcript)); !strings.Contains(got, want) {
		t.Errorf("does not report what stdout held (%q):\n%s", want, got)
	}
	// Anchored on "out of " so the assertion cannot be satisfied by the
	// stream's own figure happening to end in the answer's digits.
	if bad := fmt.Sprintf("out of %d bytes", len(answer)); strings.Contains(got, bad) {
		t.Errorf("reports the extracted answer's length as the stream's (%q):\n%s", bad, got)
	}

	// AND STILL SAYS NOTHING WHERE THE STREAM *IS* THE LINE. A text profile
	// declares stdout to be the answer, so located and the stream are one
	// string and there is no remainder to point at.
	plain := &rawResult{stdout: answer + "\n"}
	if got := plain.markerText(markerHit{Said: answer, Stream: markerStdout}, answer); got != answer {
		t.Errorf("a line that is the whole of stdout was annotated anyway: %q", got)
	}
}

// AND THE CLASSIFIER NAMES THE STREAM ON EVERY HIT, because the render above
// reads that field and a zero value would silently pair a stderr line with
// stdout's loss. Both marker kinds, both haystacks — the pairing is only
// unwritable if the name is always written.
func TestEveryMarkerHitNamesTheStreamItMatchedIn(t *testing.T) {
	t.Parallel()
	p := Profile{
		LimitMarkers: []LimitMarker{{Sentinel: "usage limit reached"}},
		AuthMarkers:  []AuthMarker{{Sentinel: "please run /login"}},
	}
	for _, tc := range []struct {
		name   string
		answer string
		stderr string
		want   markerStream
	}{
		{"limit in the answer", "your usage limit reached for today", "", markerStdout},
		{"limit on stderr", "", "error: usage limit reached", markerStderr},
		{"auth in the answer", "please run /login first", "", markerStdout},
		{"auth on stderr", "", "error: please run /login", markerStderr},
	} {
		hit, ok := classifyMarkers(p, tc.answer, tc.stderr)
		if !ok {
			t.Fatalf("%s: nothing classified", tc.name)
		}
		if hit.Stream != tc.want {
			t.Errorf("%s: Stream = %q, want %q", tc.name, hit.Stream, tc.want)
		}
	}
}

// A VERSION PROBE READS ONE LINE, so the output cap reaches it in exactly one
// shape: a stdout that overran with no newline anywhere in what survived. Then
// the "version" is a PREFIX, and a report printing it beside `written for` —
// which an operator compares by eye — is worse than a report printing nothing.
//
// The two inputs to that answer sit in different places, which is why this is
// asked of [rawResult] rather than derived at the call site: a caller reading
// only the drop count refuses every long-but-fine probe, and one reading only
// the newline refuses none of the broken ones.
func TestTheVersionLineKnowsWhetherTheCapReachedIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		res     rawResult
		want    string
		wantCut bool
	}{
		{"a clean probe", rawResult{stdout: "2.0.31 (Claude Code)\n"},
			"2.0.31 (Claude Code)", false},
		{"clipped after the version line",
			rawResult{stdout: "2.0.31 (Claude Code)\nthen a stream", droppedStdout: 4096},
			"2.0.31 (Claude Code)", false},
		{"clipped inside the version line",
			rawResult{stdout: "2.0.3", droppedStdout: 4096}, "2.0.3", true},
		{"one line, nothing dropped", rawResult{stdout: "2.0.31"}, "2.0.31", false},
	} {
		line, cut := tc.res.stdoutFirstLine()
		if line != tc.want || cut != tc.wantCut {
			t.Errorf("%s: (%q, %v), want (%q, %v)", tc.name, line, cut, tc.want, tc.wantCut)
		}
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
	}, "")
	if err == nil {
		t.Fatal("a clipped stdout was parsed and returned as a completion")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("the refusal does not say the answer is incomplete: %v", err)
	}
	// IT MUST NAME A REMEDY THAT EXISTS. It used to say "raise
	// cli.max_output_bytes", a field neither config tier has ever had:
	// `crewlet validate` refuses it as unknown, so the one instruction
	// given to an operator whose seat had stopped answering was a dead end.
	if strings.Contains(err.Error(), "max_output_bytes") {
		t.Errorf("the refusal names a config field that does not exist: %v", err)
	}
	// AND BOTH HALVES OF A REMEDY THAT TAKES TWO FIELDS: [Profile.validate]
	// refuses text_events without event_type_path, so a message naming only
	// the first sends an operator to a config that will not load.
	for _, want := range []string{
		"not configurable", "event_type_path", "text_events", "refused at load",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not offer %q: %v", want, err)
		}
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
	comp, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: claudeCodeEmptyAnswer}, "")
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
	if _, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: drifted}, ""); err == nil {
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
	p, err := Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prov := &Provider{profile: p, model: "haiku", agent: "claude-code", key: "default"}
	_, err = prov.completion(t.Context(), "prompt", &rawResult{stdout: "  \n ", stderr: "node: bad flag"}, "")
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
		stdout: `{"note":"Usage limit reached \u00b7 continuing automatically"}`}, "")
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
		`{"note":"quota spent|%d"}`, reset)}, "")
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
