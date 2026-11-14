package executor_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// claudeAssistant is one assistant line of a Claude Code stream, as 2.1.310
// streams one (probe P2's run streams): a block of message id, its usage (raw
// JSON, "" for none) and, on a subagent's line, the subagent's own keys; on the
// main thread's where parent is "null", a subagent's where it is a call's
// quoted id, and with no parent_tool_use_id at all where it is "".
func claudeAssistant(id, parent, usage string) string {
	line := `{"type":"assistant","message":{"id":"` + id + `","type":"message","role":"assistant",` +
		`"model":"claude-sonnet-4-5","content":[{"type":"text","text":"working"}]`
	if usage != "" {
		line += `,"usage":` + usage
	}
	line += `}`
	if parent != "" {
		line += `,"parent_tool_use_id":` + parent
		if parent != "null" {
			line += `,"agent_id":"a0123456789abcdef","subagent_type":"general-purpose","task_description":"look"`
		}
	}
	return line + `,"session_id":"5e55f1d0-0000-4000-8000-00000000c1a0","uuid":"u-` + id + `"}`
}

// claudeUsageOf is a usage of input uncached tokens, read from the cache and
// written to it: the request's input side, input + read + write.
func claudeUsageOf(input, read, write int) string {
	return fmt.Sprintf(`{"input_tokens":%d,"output_tokens":1,"cache_creation_input_tokens":%d,`+
		`"cache_read_input_tokens":%d}`, input, write, read)
}

// THE NEWEST MAIN-THREAD REQUEST OF A CLAUDE CODE RUN IS READ OFF ITS STREAM, and
// never a later subagent's line. Lines shaped as the measured streams of probe
// P2 (2.1.310, cases c, g and h): every main-thread assistant line carries
// `parent_tool_use_id` null, every subagent's, a foreground's and a background's,
// the id of the call that started it, and a message's lines repeat its usage;
// a background subagent's line can come after the main thread's newest. The
// figure is the newest main-thread request's input side, its uncached input,
// cache reads and cache writes, the run's own.
//
// Mutation: read the usage whatever its parent_tool_use_id, and a later
// subagent's figure is taken; sum the figures, and a message's lines and its
// earlier messages are counted with it.
func TestAClaudeRunsLastMainThreadRequestIsReadOffItsStream(t *testing.T) {
	t.Parallel()
	got, why, failed := executor.StreamRequest(executor.ClaudeCode{},
		`{"type":"system","subtype":"init","session_id":"5e55f1d0-0000-4000-8000-00000000c1a0"}`,
		claudeAssistant("msg_probe_1", "null", claudeUsageOf(100, 4000, 0)),
		claudeAssistant("msg_probe_1", "null", claudeUsageOf(100, 4000, 0)),
		claudeAssistant("msg_probe_2", `"toolu_c0"`, claudeUsageOf(70000, 0, 0)),
		claudeAssistant("msg_probe_3", "null", claudeUsageOf(100, 5000, 300)),
		// A LINE OF THE NEWEST MESSAGE WITH NO USAGE says nothing of its request.
		claudeAssistant("msg_probe_3", "null", ""),
		claudeAssistant("msg_probe_4", `"toolu_g_bg0"`, claudeUsageOf(90000, 0, 0)),
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_g_bg0",`+
			`"content":"done"}]},"parent_tool_use_id":null,"session_id":"5e55f1d0-0000-4000-8000-00000000c1a0"}`,
		claudeResultLine(1, 1, 1, 1, 0.1, ""))
	if want := (sandbox.RequestMeasure{Tokens: 100 + 5000 + 300, Own: true}); got != want {
		t.Errorf("the run's newest request read %+v (%s, failed %v), want %+v, the main thread's newest", got, why,
			failed, want)
	}
}

// A LINE WITH NO THREAD KEY IS NO MAIN-THREAD REQUEST. Every stream line measured
// carries parent_tool_use_id, null on the main thread's own; a line that carries
// none is of a shape this build does not read, so its usage is no figure, never
// read as the main thread's, which a subagent's could then be. And the
// collection says so at warning, since a release that drops the key leaves
// every job unmeasured.
//
// Mutation: read an absent key as the main thread's, and the figure is taken.
func TestALineWithNoThreadKeyIsNoMainThreadRequest(t *testing.T) {
	t.Parallel()
	got, why, failed := executor.StreamRequest(executor.ClaudeCode{},
		claudeAssistant("msg_1", "", claudeUsageOf(100, 4000, 0)),
		claudeAssistant("msg_2", "", claudeUsageOf(100, 6000, 0)))
	if got != (sandbox.RequestMeasure{}) {
		t.Errorf("a stream with no thread key read %+v, want no figure", got)
	}
	if !failed || !strings.Contains(why, "parent_tool_use_id") {
		t.Errorf("no figure is said as %q (a failed reading %v), want a reading that failed, naming the key", why,
			failed)
	}
}

// A MAIN-THREAD USAGE THAT DOES NOT READ GIVES NO FIGURE, NEVER AN EARLIER ONE. The
// usage is read by the layer's one rule for a usage object (claudethread.go's
// readUsage): each count a whole number in digits where present, so a null count
// and a negative one are no reading, and the newest request's figure is then
// unknown. An earlier request's figure taken in its place could be far less than
// the newest carried, and the floor of a relaunch that continues the copy would
// be held to less than it sends.
//
// Mutation: read the usage loosely, a null count as 0 and a negative one as
// written, and the line's figure is taken.
func TestAMainThreadUsageThatDoesNotReadGivesNoFigure(t *testing.T) {
	t.Parallel()
	for name, usage := range map[string]string{
		"a null count": `{"input_tokens":null,"output_tokens":1,"cache_creation_input_tokens":0,` +
			`"cache_read_input_tokens":6000}`,
		"a negative count": `{"input_tokens":-5,"output_tokens":1,"cache_creation_input_tokens":0,` +
			`"cache_read_input_tokens":6000}`,
		"a usage that is no object": `"6000"`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, why, failed := executor.StreamRequest(executor.ClaudeCode{},
				claudeAssistant("msg_1", "null", claudeUsageOf(100, 4000, 0)),
				claudeAssistant("msg_2", "null", usage))
			if got != (sandbox.RequestMeasure{}) {
				t.Errorf("a newest request whose usage does not read gave %+v, want no figure", got)
			}
			if !failed || !strings.Contains(why, "does not read") {
				t.Errorf("no figure is said as %q (a failed reading %v), want a usage that does not read", why,
					failed)
			}
		})
	}
}

// A LINE PAST THE STREAM'S BOUND GIVES NO FIGURE, NEVER AN EARLIER ONE. The line is
// skipped unread, and it may be a later request's, which carried anything: so a
// Claude Code stream's newest figure is cleared by a skipped line after it, and
// an OpenCode stream's reading of its session's message list ends with none at
// one before any page that measured a prompt, since the page it may be held the
// figure, and a later page's would be an older request's.
//
// Mutation: keep a Claude Code figure across a skipped line, or read on past one
// in an OpenCode stream, and a figure is taken.
func TestAContextLinePastTheLineBoundGivesNoFigure(t *testing.T) {
	t.Parallel()
	t.Run("claude-code", func(t *testing.T) {
		t.Parallel()
		got, _, _ := executor.StreamRequest(executor.ClaudeCode{},
			claudeAssistant("msg_1", "null", claudeUsageOf(100, 4000, 0)), executor.SkippedLine)
		if got != (sandbox.RequestMeasure{}) {
			t.Errorf("a figure before a skipped line stood: %+v", got)
		}
	})
	t.Run("opencode", func(t *testing.T) {
		t.Parallel()
		got, _, _ := executor.StreamRequest(executor.OpenCode{},
			openCodeContextLine(openCodePage(openCodeAborted("msg_3"))), executor.SkippedLine,
			openCodeContextLine(openCodePage(openCodeSettled("msg_2", 1015, 200, 0))))
		if got != (sandbox.RequestMeasure{}) {
			t.Errorf("a page after a skipped line was read: %+v", got)
		}
	})
}

// A LINE CUT SHORT GIVES NO FIGURE, NEVER AN EARLIER ONE. A harness killed while it
// writes a line, by its memory limit with no end asked, leaves its stream's last
// line cut short, which no read ever has whole and the decoder cannot read: it
// may be the newest request's assistant line, which carried anything, so the
// figure before it is no measure of the newest request, as it is not before a
// line past the stream's bound, and the collection says why at info, since the
// stream is of a shape this build reads.
//
// Mutation: pass over a line that does not decode as though it were not there,
// and the earlier request's figure is taken.
func TestALineCutShortGivesNoFigure(t *testing.T) {
	t.Parallel()
	whole := claudeAssistant("msg_2", "null", claudeUsageOf(100, 9000, 0))
	got, why, failed := executor.StreamRequest(executor.ClaudeCode{},
		claudeAssistant("msg_1", "null", claudeUsageOf(100, 4000, 0)), whole[:len(whole)/2])
	if got != (sandbox.RequestMeasure{}) {
		t.Errorf("a figure before a line cut short stood: %+v", got)
	}
	if failed || !strings.Contains(why, "does not decode") {
		t.Errorf("no figure is said as %q (a failed reading %v), want the line cut short, said at info", why,
			failed)
	}
}

// openCodeSettled is an assistant message of an OpenCode session's list that
// settled with tokens of input uncached, read from the cache and written to it.
func openCodeSettled(id string, input, read, write int) string {
	return fmt.Sprintf(`{"id":%q,"time":{"created":1794632160771,"completed":1794632160784},"type":"assistant",`+
		`"agent":"build","content":[{"type":"text","text":"Done."}],"finish":"stop","cost":0,`+
		`"tokens":{"input":%d,"output":30,"reasoning":0,"cache":{"read":%d,"write":%d}}}`, id, input, read, write)
}

// openCodeAborted is an assistant message that settled interrupted, with the
// tokens its request had measured: as each real export's interrupted step is.
func openCodeAborted(id string) string {
	return fmt.Sprintf(`{"id":%q,"time":{"created":1794632161091,"completed":1794632161101},"type":"assistant",`+
		`"content":[],"finish":"error","error":{"type":"aborted","message":"Step interrupted"},`+
		`"tokens":{"input":5000,"output":30,"reasoning":0,"cache":{"read":200,"write":0}}}`, id)
}

// openCodeUnsettled is the step in flight, which the list hands too: tokens of
// its own, no completion, and a failed tool's error in its content, which a
// script's test of a page for a failed message counts.
func openCodeUnsettled(id string) string {
	return fmt.Sprintf(`{"id":%q,"time":{"created":1794632162000},"type":"assistant","content":[{"type":"tool",`+
		`"id":"call_1","name":"read","state":{"status":"error","error":{"message":"no such file"}}}],`+
		`"tokens":{"input":3000,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}}`, id)
}

// openCodeAbortedWith is an interrupted step, as [openCodeAborted] is, whose
// content holds a text of n bytes: a step whose page is about that size.
func openCodeAbortedWith(id string, n int) string {
	return fmt.Sprintf(`{"id":%q,"time":{"created":1794632161091,"completed":1794632161101},"type":"assistant",`+
		`"content":[{"type":"text","text":%q}],"finish":"error",`+
		`"error":{"type":"aborted","message":"Step interrupted"},`+
		`"tokens":{"input":5000,"output":30,"reasoning":0,"cache":{"read":200,"write":0}}}`, id, strings.Repeat("a", n))
}

// openCodePage is one page of the session's message list, as the run's server
// answers one: its messages, and the cursor to the pages either side of it.
func openCodePage(messages ...string) string {
	return openCodePageTo("bmV4dA", messages...)
}

// openCodePageTo is a page of the session's message list, as [openCodePage] is,
// whose cursor names next as the page after it.
func openCodePageTo(next string, messages ...string) string {
	return `{"data":[` + strings.Join(messages, ",") + `],"cursor":{"previous":"cHJldmlvdXM","next":"` + next +
		`"}}`
}

// openCodeContextLine is the line a run's script prints of one page.
func openCodeContextLine(page string) string {
	return `{"type":"` + executor.OpenCodeContextEvent + `","page":` + page + `}`
}

// openCodeRunStep is one event of a run's stream that names the step it is of.
func openCodeRunStep(message string) string {
	return `{"type":"text","part":{"messageID":"` + message + `","type":"text","text":"working"}}`
}

// OPENCODE'S FIGURE IS ITS SESSION'S NEWEST SETTLED MESSAGE THAT MEASURED A PROMPT,
// read off the pages of its message list the run's script printed, newest first,
// one message a page: a step interrupted or refused, the step still in flight
// and one whose prompt measured nothing are passed over, as an export's are
// (measure.go's measuredPrompt), and the first that measured one is the figure;
// the run's own where its stream named that step, and else an earlier job's
// request of the session it took up.
//
// Mutation: take the newest message whatever it holds, or one in flight, and a
// failed or an unsettled step's figure is taken; read every figure as the run's
// own, and an earlier job's request is.
func TestOpenCodesFigureIsItsNewestSettledMeasuredMessage(t *testing.T) {
	t.Parallel()
	pages := []string{
		openCodeContextLine(openCodePage(openCodeAborted("msg_4"))),
		openCodeContextLine(openCodePage(openCodeUnsettled("msg_3"))),
		openCodeContextLine(openCodePage(openCodeSettled("msg_2", 0, 0, 0))),
		openCodeContextLine(openCodePage(openCodeSettled("msg_1", 1015, 200, 0))),
	}
	for name, tc := range map[string]struct {
		named []string
		want  sandbox.RequestMeasure
	}{
		"a step the run made":                 {named: []string{"msg_1", "msg_4"}, want: sandbox.RequestMeasure{Tokens: 1215, Own: true}},
		"a step of an earlier job's":          {named: []string{"msg_4"}, want: sandbox.RequestMeasure{Tokens: 1215}},
		"a run whose stream named no step at": {want: sandbox.RequestMeasure{Tokens: 1215}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var lines []string
			for _, step := range tc.named {
				lines = append(lines, openCodeRunStep(step))
			}
			got, why, _ := executor.StreamRequest(executor.OpenCode{}, append(lines, pages...)...)
			if got != tc.want {
				t.Errorf("the session's newest request read %+v (%s), want %+v", got, why, tc.want)
			}
		})
	}
}

// THE RULES FOR AN EXPORT AND FOR THE MESSAGE LIST ARE ONE. A session's measure is
// read off two documents: the export a take streams out of its box (the
// capture's), and the session's own message list as the job ends (the job's
// record's). Both read a message by the one rule, so the real export each
// release this build drives printed for a real session gives one figure either
// way: its steps handed to the list's reading one a page, newest first, give
// the export's own 1215, the step that answered "Carry on.", behind two the
// session was interrupted in and one its provider refused.
//
// Mutation: drop the error check from the rule, and an interrupted step's
// figure is taken, by both readings alike.
func TestTheRulesForAnExportAndForTheAPIAreOne(t *testing.T) {
	t.Parallel()
	for _, version := range openCodeFixtureVersions {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			var export struct {
				Messages []json.RawMessage `json:"messages"`
			}
			if err := json.Unmarshal([]byte(openCodeExportFixture(t, version)), &export); err != nil {
				t.Fatalf("the fixture does not decode: %v", err)
			}
			var steps []string
			for i := len(export.Messages) - 1; i >= 0; i-- {
				var kind struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(export.Messages[i], &kind); err != nil {
					t.Fatal(err)
				}
				if kind.Type == "assistant" {
					steps = append(steps, openCodeContextLine(openCodePage(string(export.Messages[i]))))
				}
			}
			listed, why, _ := executor.StreamRequest(executor.OpenCode{}, steps...)
			all, err := json.Marshal(export.Messages)
			if err != nil {
				t.Fatal(err)
			}
			exported := openCodeTaken(t, string(all))
			if listed.Tokens != 1215 || exported != 1215 {
				t.Errorf("the list's reading gives %d (%s) and the export's %d, want both the export's 1215",
					listed.Tokens, why, exported)
			}
		})
	}
}

// AN EXPORT'S TOKENS ARE READ BY THEIR KEYS EXACTLY AS WRITTEN. A step's tokens are
// decoded as OpenCode's own JSON.parse reads them, so a key that differs from
// one they hold only in case is no count: decoded by encoding/json into the
// message's fields, `INPUT` was taken for `input`, the last of the two written
// winning, and a step's prompt read as what that key said.
//
// Mutation: decode the tokens by encoding/json into the message's fields again,
// and the step reads as the case-differing key's 999,990 tokens.
func TestAnExportsTokensAreReadByTheirKeysExactlyAsWritten(t *testing.T) {
	t.Parallel()
	messages := `[{"id":"msg_1","type":"user","text":"x"},` +
		`{"id":"msg_2","type":"assistant","time":{"created":1,"completed":2},` +
		`"tokens":{"input":10,"INPUT":999990,"output":5,"cache":{"read":200,"write":0,"READ":7}}}]`
	if got := openCodeTaken(t, messages); got != 10+200 {
		t.Errorf("the step's prompt read %d tokens, want 210, its keys read exactly as written", got)
	}
}

// A MESSAGE THAT DOES NOT READ ENDS THE READING WITH NONE, NEVER AN OLDER FIGURE. A
// step whose measured values are of another type than OpenCode writes them, its
// tokens a string, is no message read as one that measured nothing: it may be the
// newest request's step, which carried anything. So on a page of the session's
// list it ends the reading with no figure, said as a reading that failed, and in
// an export it leaves the capture unmeasured; passed over, either would hand the
// older step's figure on as the newest request's.
//
// Mutation: read on past a page that does not decode, or pass over a value of
// another type than its field's, and the older step's 1215 is taken.
func TestAMessageThatDoesNotReadEndsTheReadingWithNone(t *testing.T) {
	t.Parallel()
	const unread = `{"id":"msg_9","type":"assistant","time":{"created":1,"completed":2},"tokens":"lots"}`
	t.Run("a page of the session's list", func(t *testing.T) {
		t.Parallel()
		got, why, failed := executor.StreamRequest(executor.OpenCode{},
			openCodeContextLine(openCodePage(unread)),
			openCodeContextLine(openCodePage(openCodeSettled("msg_8", 1015, 200, 0))))
		if got != (sandbox.RequestMeasure{}) || !failed {
			t.Errorf("the reading gave %+v (%s, a failed reading %v), want no figure from a reading that failed",
				got, why, failed)
		}
	})
	t.Run("an export", func(t *testing.T) {
		t.Parallel()
		messages := `[{"id":"msg_1","type":"user","text":"x"},` + openCodeSettled("msg_8", 1015, 200, 0) + `,` +
			unread + `]`
		if got := openCodeTaken(t, messages); got != 0 {
			t.Errorf("the capture measured %d, want none: its newest step does not read", got)
		}
	})
}

// ONLY WHAT THE MEASURE READS IS HELD TO ITS TYPE. Of a step's time the measure reads
// when it settled, and of its tokens the prompt's counts: a value it never reads, a
// step's `created` or its reply's `output`, of another type than OpenCode writes
// it is no reason to lose the figure the step's prompt and its completion state,
// on a page of the session's list and in an export alike.
//
// Mutation: decode the step's time whole, its `created` among it, or its reply's
// count with its prompt's, and the step reads as a message that does not.
func TestOnlyWhatTheMeasureReadsIsHeldToItsType(t *testing.T) {
	t.Parallel()
	const step = `{"id":"msg_2","type":"assistant","time":{"created":[1793361600000],"completed":2},` +
		`"tokens":{"input":1015,"output":"thirty","reasoning":0,"cache":{"read":200,"write":0}}}`
	got, why, _ := executor.StreamRequest(executor.OpenCode{}, openCodeContextLine(openCodePage(step)))
	if want := (sandbox.RequestMeasure{Tokens: 1215}); got != want {
		t.Errorf("the page's step reads %+v (%s), want %+v", got, why, want)
	}
	if got := openCodeTaken(t, `[{"id":"msg_1","type":"user","text":"x"},`+step+`]`); got != 1215 {
		t.Errorf("the export's step measured %d, want 1215", got)
	}
}

// A FOLLOWED OPENCODE STREAM KEEPS NO MESSAGE IDS. Only a collection asks whose
// step its session's newest request was, so the decoder keeps the steps its
// run's events name while no live reading follows the stream: a live reading
// holds its decoder for as long as somebody watches, and what it holds is to
// stay bounded however long the run. So neither the ids it read before the
// reading first took its entries, nor any it reads after, are held.
//
// Mutation: keep the ids while following, or keep the ones read before the
// first entries were taken, and the decoder holds them.
func TestAFollowedOpenCodeStreamKeepsNoMessageIds(t *testing.T) {
	t.Parallel()
	if held := executor.FollowedNames(openCodeRunStep("msg_1"), openCodeRunStep("msg_2"), openCodeRunStep("msg_3")); held != 0 {
		t.Errorf("a followed stream's decoder holds %d message ids, want none", held)
	}
}

// THE MEASURE RIDES OUT OF THE COLLECTION, for both harnesses. A job's collection
// hands back what its newest request carried beside the rest of its result
// (sandbox.Result.LastRequest), whichever document the result is: Claude Code's
// result file, read in place of the stream's own result, and OpenCode's stream,
// which is its result and names no result file.
//
// Mutation: set the measure inside the block that reads the result file, and an
// OpenCode job, which has none, hands back no figure.
func TestTheMeasureRidesOutOfTheCollection(t *testing.T) {
	t.Parallel()
	t.Run("claude-code", func(t *testing.T) {
		t.Parallel()
		runner := executor.NewClaudeCode()
		b := box(t, runner)
		p := paths(b)
		result := claudeResultLine(30, 9, 4000, 300, 0.2, "")
		b.Put(p.Stream(), claudeAssistant("msg_1", "null", claudeUsageOf(20, 4000, 300))+"\n"+result+"\n")
		b.Put(p.Result(), result)
		b.Put(p.ExitCode(), "0")
		res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if want := (sandbox.RequestMeasure{Tokens: 4320, Own: true}); res.LastRequest != want {
			t.Errorf("the collection handed back %+v, want %+v", res.LastRequest, want)
		}
	})
	t.Run("opencode", func(t *testing.T) {
		t.Parallel()
		runner := executor.NewOpenCode()
		b := box(t, runner)
		p := paths(b)
		b.Put(p.Result(), openCodeRunStep("msg_1")+"\n"+
			openCodeContextLine(openCodePage(openCodeSettled("msg_1", 1015, 200, 0)))+"\n")
		b.Put(p.ExitCode(), "0")
		res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if want := (sandbox.RequestMeasure{Tokens: 1215, Own: true}); res.LastRequest != want {
			t.Errorf("the collection handed back %+v, want %+v", res.LastRequest, want)
		}
	})
}

// A READING THAT FAILED IS SAID AT WARNING, and a run that left nothing to read at
// info. A Claude Code stream whose assistant lines carry a usage and none says it is
// the main thread's is of a shape this build does not read, a release that dropped
// the key, and every job of that release goes unmeasured, which an operator has to
// hear of; a run that made no request is no such thing.
//
// NOT PARALLEL: it reads the process's log, which every parallel case writes to,
// and a sequential test runs with none of them.
//
// Mutation: say every reading with no figure at info, and the first case is red.
func TestAReadingThatFailedIsSaidAtWarning(t *testing.T) {
	logs := &lockedBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })
	runner := executor.NewClaudeCode()
	for _, c := range []struct {
		name, stream, level string
	}{
		{name: "a stream of a shape this build does not read",
			stream: claudeAssistant("msg_1", "", claudeUsageOf(100, 4000, 0)), level: "WARN"},
		{name: "a run that made no request",
			stream: `{"type":"system","subtype":"init","session_id":"5e55f1d0-0000-4000-8000-00000000c1a0"}`,
			level:  "INFO"},
	} {
		from := len(logs.String())
		b := box(t, runner)
		p := paths(b)
		result := claudeResultLine(1, 1, 1, 1, 0.1, "")
		b.Put(p.Stream(), c.stream+"\n"+result+"\n")
		b.Put(p.Result(), result)
		b.Put(p.ExitCode(), "0")
		if _, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}); err != nil {
			t.Fatalf("%s: Collect: %v", c.name, err)
		}
		var said []string
		for line := range strings.SplitSeq(strings.TrimSpace(logs.String()[from:]), "\n") {
			var record struct {
				Level string `json:"level"`
				Msg   string `json:"msg"`
			}
			if line == "" || json.Unmarshal([]byte(line), &record) != nil {
				continue
			}
			if record.Msg == "harness_context_unmeasured" {
				said = append(said, record.Level)
			}
		}
		if len(said) != 1 || said[0] != c.level {
			t.Errorf("%s: harness_context_unmeasured was said at %q, want once at %s", c.name,
				strings.Join(said, ", "), c.level)
		}
	}
}

// openCodeContextFixture is one file a real OpenCode run left of its session's
// message list, testdata/opencode/<version>/context/<name>: a REAL one, of the
// releases a run's script was read against, 2.0.32 and 2.2.7, each run in a
// container against an OpenAI-compatible stub model API (C8's probe): run1.jsonl
// is the `--format json` stream of a run that made one call and answered, and
// page-a1.json what `session.message.list` answered after it, the newest
// assistant message a page; run2.jsonl is the stream of the next run, which its
// provider refused, page-b1.json the list's first page after it and page-b2.json
// the page its cursor names. Byte for byte as OpenCode printed them.
func openCodeContextFixture(t *testing.T, version, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "opencode", version, "context", name))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	return string(raw)
}

// openCodeContextVersions are the releases a real run's message list is kept of.
var openCodeContextVersions = []string{"2.0.32", "2.2.7"}

// streamLines is a run's stream, its lines.
func streamLines(stream string) []string {
	return strings.Split(strings.TrimRight(stream, "\n"), "\n")
}

// A REAL RUN'S STREAM NAMES THE STEPS ITS SESSION'S LIST HOLDS, by the one id: what
// OpenCode 2.0.32 and 2.2.7 printed of two real runs of one session and of its
// message list after each. The run that made one call and answered named both
// its steps in its stream (`part.messageID`), and the list's newest page after it
// holds the step that answered under that id, so the figure is that run's own;
// the next run, which its provider refused, made no step that measured a
// prompt, so the list's second page after it, by the first's cursor, holds the
// earlier run's step, which this run's stream never named: the session's,
// never this run's own.
//
// Mutation: keep none of the ids the run's stream names, and the run that
// answered reads as one that made no request of its own.
func TestARealRunsStreamNamesTheStepsItsSessionsListHolds(t *testing.T) {
	t.Parallel()
	for _, version := range openCodeContextVersions {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			fixture := func(name string) string { return openCodeContextFixture(t, version, name) }
			answered, why, _ := executor.StreamRequest(executor.OpenCode{}, append(streamLines(fixture("run1.jsonl")),
				openCodeContextLine(strings.TrimSpace(fixture("page-a1.json"))))...)
			if want := (sandbox.RequestMeasure{Tokens: 1002 + 200, Own: true}); answered != want {
				t.Errorf("the run that answered reads %+v (%s), want %+v, its own step", answered, why, want)
			}
			refused, why, _ := executor.StreamRequest(executor.OpenCode{}, append(streamLines(fixture("run2.jsonl")),
				openCodeContextLine(strings.TrimSpace(fixture("page-b1.json"))),
				openCodeContextLine(strings.TrimSpace(fixture("page-b2.json"))))...)
			if want := (sandbox.RequestMeasure{Tokens: 1002 + 200}); refused != want {
				t.Errorf("the run its provider refused reads %+v (%s), want %+v, the earlier run's step", refused, why,
					want)
			}
		})
	}
}

// THE SCRIPT ASKS THE SESSION'S NEWEST ASSISTANT MESSAGES A PAGE AT A TIME, of the
// run's own server, once the run has ended and its session is exported: by the
// session's id, its assistant messages alone, one a page, and the next page by
// the cursor the page before answered, read off the answer's own text. Against
// the pages a real OpenCode run's server answered after a run its provider
// refused, the first page is that refused step and the second the step that
// answered before it, which is the figure; the run's own stream named neither
// as a step of its own, so the figure is an earlier job's request of the
// session.
//
// Mutation: ask an operation OpenCode's API has no such name for, and nothing is
// asked; ask one page only, and the refused step's page gives no figure.
func TestTheScriptAsksTheSessionsNewestAssistantMessagesAPageAtATime(t *testing.T) {
	t.Parallel()
	for _, version := range openCodeContextVersions {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			first := openCodeContextFixture(t, version, "page-b1.json")
			cursor := pageCursor(t, first)
			forEachShell(t, func(t *testing.T, sh boxShell) {
				run := runScript(t, scriptOpts{shell: sh,
					pages: map[string]string{"first": first, cursor: openCodeContextFixture(t, version, "page-b2.json")}})
				if run.code != 0 {
					t.Fatalf("exit %d:\n%s", run.code, run.stderr)
				}
				session := sessionOfRun(t, run)
				want := []string{
					"--param sessionID=" + session + " --param type=assistant --param limit=1",
					"--param sessionID=" + session + " --param type=assistant --param limit=1 --param cursor=" + cursor,
				}
				if got := listed(t, run); !slices.Equal(got, want) {
					t.Errorf("the session's message list was asked\n%s\nwant\n%s", strings.Join(got, "\n"),
						strings.Join(want, "\n"))
				}
				exported := run.callIndex("session export --server ")
				asked := run.callIndex("api --server http://127.0.0.1:4242 session.message.list ")
				if exported < 0 || asked < exported {
					t.Errorf("the list was asked at %d and the session exported at %d, want after the export:\n%s",
						asked, exported, strings.Join(run.calls, "\n"))
				}
				got, why, _ := executor.StreamRequest(executor.OpenCode{}, streamLines(run.stdout)...)
				if want := (sandbox.RequestMeasure{Tokens: 1002 + 200}); got != want {
					t.Errorf("the run's stream reads %+v (%s), want %+v:\n%s", got, why, want, run.stdout)
				}
			})
		})
	}
}

// pageCursor is the cursor to the page after page, as its answer states it.
func pageCursor(t *testing.T, page string) string {
	t.Helper()
	var answer struct {
		Cursor struct {
			Next string `json:"next"`
		} `json:"cursor"`
	}
	if err := json.Unmarshal([]byte(page), &answer); err != nil || answer.Cursor.Next == "" {
		t.Fatalf("the page names no next cursor (%v):\n%s", err, page)
	}
	return answer.Cursor.Next
}

// sessionOfRun is the session the run's script ran, as its brief's run named it.
func sessionOfRun(t *testing.T, run scriptRun) string {
	t.Helper()
	at := run.callIndex("run ")
	if at < 0 {
		t.Fatalf("the brief was never run:\n%s", strings.Join(run.calls, "\n"))
	}
	_, session, _ := strings.Cut(run.calls[at], "--session ")
	session, _, _ = strings.Cut(session, " ")
	return session
}

// listed is every call of the session's message list the fake's server was
// asked, its parameters, in order.
func listed(t *testing.T, run scriptRun) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(run.dir, "listed"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// THE SCRIPT STOPS AT A PAGE WITH NO FAILED MESSAGE: the newest message that did
// not fail is the figure, or a reason to stop with none, so a run that ended
// cleanly asks one page. Against the page a real OpenCode run's server answered
// after a run that made one call and answered, which names that answer's step,
// the figure is that step's, the run's own.
//
// Mutation: ask every page the bound allows, and a page past the one with no
// failed message is asked.
func TestTheScriptStopsAtAPageWithNoError(t *testing.T) {
	t.Parallel()
	for _, version := range openCodeContextVersions {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			first := openCodeContextFixture(t, version, "page-a1.json")
			var step struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(first), &step); err != nil || len(step.Data) != 1 {
				t.Fatalf("the page holds no one message (%v):\n%s", err, first)
			}
			forEachShell(t, func(t *testing.T, sh boxShell) {
				run := runScript(t, scriptOpts{shell: sh, pages: map[string]string{"first": first,
					pageCursor(t, first): openCodeContextFixture(t, version, "page-b2.json")},
					env: []string{"FAKE_RUN_MESSAGE=" + step.Data[0].ID}})
				if got := listed(t, run); len(got) != 1 {
					t.Errorf("the list was asked %d times, want once:\n%s", len(got), strings.Join(got, "\n"))
				}
				got, why, _ := executor.StreamRequest(executor.OpenCode{}, streamLines(run.stdout)...)
				if want := (sandbox.RequestMeasure{Tokens: 1002 + 200, Own: true}); got != want {
					t.Errorf("the run's stream reads %+v (%s), want %+v", got, why, want)
				}
			})
		})
	}
}

// A PAGE PAST THE BUDGET IS SAID AND ENDS THE READING. A message carries its content
// whole, an image a tool read among it as a data URI, so a page is bounded in
// count alone: one that would take the pages past what they may put into the
// stream in all is said by its size alone, never printed, and nothing after it
// is asked, so the figure is none, never an older page's.
//
// Mutation: print the page whatever its size, and the collection reads a page
// past what its read may carry back.
func TestAPagePastTheBudgetIsSaidAndEndsTheReading(t *testing.T) {
	t.Parallel()
	big := openCodePage(`{"id":"msg_1","time":{"created":1,"completed":2},"type":"assistant","content":` +
		`[{"type":"text","text":"` + strings.Repeat("a", executor.OpenCodeContextBytes) + `"}],` +
		`"tokens":{"input":1015,"output":30,"reasoning":0,"cache":{"read":200,"write":0}}}`)
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, pages: map[string]string{"first": big + "\n"}})
		marker := fmt.Sprintf(`{"type":"%s","skipped":%d}`, executor.OpenCodeContextEvent, len(big)+1)
		if !slices.Contains(streamLines(run.stdout), marker) {
			t.Errorf("the stream does not say the page by its size (%s)", marker)
		}
		if strings.Contains(run.stdout, strings.Repeat("a", 1024)) {
			t.Error("the page past the budget was printed")
		}
		got, why, _ := executor.StreamRequest(executor.OpenCode{}, streamLines(run.stdout)...)
		if got != (sandbox.RequestMeasure{}) || !strings.Contains(why, "past the") {
			t.Errorf("the run's stream reads %+v (%s), want no figure, the page said past the budget", got, why)
		}
	})
}

// THE PAGES' BUDGET IS THEIRS IN ALL, NEVER A PAGE'S. What the pages put into the
// run's stream is what the read that collects the job carries back for them, so
// pages each within the budget and past it together are printed only while they
// fit what is left of it: the first that does not is said by its size, and
// nothing after it is asked. Pages of failed steps, each two fifths of the
// budget: the first two are printed, the third is said, and the figure is none,
// never the older page's after it.
//
// Mutation: hold each page to the whole budget, and the third page is printed
// and a fourth asked, the stream carrying past the budget.
func TestThePagesBudgetIsTheirsInAll(t *testing.T) {
	t.Parallel()
	near := executor.OpenCodeContextBytes * 2 / 5
	third := openCodePageTo("cGFnZTQ", openCodeAbortedWith("msg_2", near))
	pages := map[string]string{
		"first":   openCodePageTo("cGFnZTI", openCodeAbortedWith("msg_4", near)),
		"cGFnZTI": openCodePageTo("cGFnZTM", openCodeAbortedWith("msg_3", near)),
		"cGFnZTM": third,
		"cGFnZTQ": openCodePage(openCodeSettled("msg_1", 1015, 200, 0)),
	}
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, pages: pages})
		if got := listed(t, run); len(got) != 3 {
			t.Errorf("the list was asked %d times, want three, the third said by its size and nothing after it",
				len(got))
		}
		printed, marker := 0, fmt.Sprintf(`{"type":"%s","skipped":%d}`, executor.OpenCodeContextEvent, len(third))
		for _, line := range streamLines(run.stdout) {
			if strings.HasPrefix(line, `{"type":"`+executor.OpenCodeContextEvent+`","page":`) {
				printed++
			}
		}
		if printed != 2 || !slices.Contains(streamLines(run.stdout), marker) {
			t.Errorf("the stream holds %d pages and the third said by its size: %v, want two and %s", printed,
				slices.Contains(streamLines(run.stdout), marker), marker)
		}
		got, why, _ := executor.StreamRequest(executor.OpenCode{}, streamLines(run.stdout)...)
		if got != (sandbox.RequestMeasure{}) || !strings.Contains(why, "past the") {
			t.Errorf("the run's stream reads %+v (%s), want no figure, the third page said past the budget", got,
				why)
		}
	})
}

// THE SCRIPT ASKS NO MORE PAGES THAN ITS BOUND. A page past the first is asked only
// where the one before held a failed message, and never past openCodeContextPages
// however many failed steps the session holds, newest first: a session whose
// newest steps all failed is unmeasured rather than paged back without end, and
// the figure is none, never one that far back.
//
// Mutation: lift the bound, and every page the cursors name is asked, down to
// the step that answered.
func TestTheScriptAsksNoMorePagesThanItsBound(t *testing.T) {
	t.Parallel()
	pages, cursor := map[string]string{}, "first"
	for i := range executor.OpenCodeContextPages + 2 {
		next := fmt.Sprintf("cGFnZ%d", i+2)
		pages[cursor] = openCodePageTo(next, openCodeAborted(fmt.Sprintf("msg_%d", 100-i)))
		cursor = next
	}
	pages[cursor] = openCodePage(openCodeSettled("msg_1", 1015, 200, 0))
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, pages: pages})
		if got := listed(t, run); len(got) != executor.OpenCodeContextPages {
			t.Errorf("the list was asked %d times, want %d, the bound", len(got), executor.OpenCodeContextPages)
		}
		got, why, _ := executor.StreamRequest(executor.OpenCode{}, streamLines(run.stdout)...)
		if got != (sandbox.RequestMeasure{}) {
			t.Errorf("the run's stream reads %+v (%s), want no figure past the bound", got, why)
		}
	})
}

// THE SCRIPT ASKS NOTHING ON AN END REQUEST: a stop's, a loss's and a turn's cap end
// the job's round, which no relaunch continues in place, and the pages would
// spend the end's wait the export needs. So a run ended on request goes from its
// export to its login's and its server's end without asking the session's
// message list.
//
// Mutation: drop the test of the end request, and the list is asked.
func TestTheScriptAsksNothingOnAnEndRequest(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		j := startScript(t, scriptOpts{shell: sh, env: []string{"FAKE_RUN_UNTIL=interrupted"},
			pages: map[string]string{"first": openCodePage(openCodeSettled("msg_1", 1015, 200, 0))}})
		j.await("the run to start", func() bool {
			_, err := os.Stat(filepath.Join(j.dir, "refreshed"))
			return err == nil
		})
		j.signalScript(syscall.SIGUSR1)
		run := j.waitWithin(promptEnd, "an ended run waited on something its end had already stopped")
		if run.callIndex("session export --server ") < 0 {
			t.Fatalf("the ended run exported nothing:\n%s", strings.Join(run.calls, "\n"))
		}
		if got := listed(t, run); len(got) != 0 {
			t.Errorf("an ended run asked its session's message list:\n%s", strings.Join(got, "\n"))
		}
	})
}

// A REQUEST WHILE THE PAGES ARE ASKED STOPS THE ASKING AT ONCE. The round a
// request ends is one no relaunch continues in place, and the pages would spend the
// end's wait the export needs: so a request that comes while the script waits on
// a page — a failed step's, whose cursor names another — ends that page's call
// (crewlet#1088's bounded call, which an end request ends where it is a page),
// and asks no other.
//
// Mutation: test for a request once, before the first page, and the page the
// cursor names is asked after the request came.
func TestARequestWhileThePagesAreAskedStopsTheAsking(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		j := startScript(t, scriptOpts{shell: sh, env: []string{"FAKE_LIST_UNTIL=answer"},
			pages: map[string]string{"first": openCodePageTo("c2Vjb25k", openCodeAborted("msg_2")),
				"c2Vjb25k": openCodePage(openCodeSettled("msg_1", 1015, 200, 0))}})
		j.await("the script to ask the session's message list", func() bool {
			_, err := os.Stat(filepath.Join(j.dir, "listing"))
			return err == nil
		})
		j.signalScript(syscall.SIGUSR1)
		if err := os.WriteFile(filepath.Join(j.dir, "answer"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		run := j.waitWithin(promptEnd, "an ended run waited on a page it would never ask")
		if got := listed(t, run); len(got) != 1 {
			t.Errorf("the list was asked %d times, want once, the page under way as the request came:\n%s",
				len(got), strings.Join(got, "\n"))
		}
	})
}

// THE CURSOR IS THE ANSWER'S OWN. A box may have no JSON parser, so the script reads
// the cursor as text, anchored at the answer's end, where the list puts its
// cursor's next: a `next` key inside a message's content, which a tool's answer
// may well hold, comes before it, and is never taken for it.
//
// Mutation: take the first `next` key of the answer, and the page a message's
// content named is asked.
func TestTheCursorIsTheAnswersOwn(t *testing.T) {
	t.Parallel()
	failed := `{"data":[{"id":"msg_3","time":{"created":1,"completed":2},"type":"assistant",` +
		`"content":[{"type":"tool","id":"call_1","name":"read","state":{"status":"completed",` +
		`"content":[{"type":"text","text":"{\"next\":\"Ym9ndXM\"}"}],"input":{"cursor":{"next":"Ym9ndXM"}}}}],` +
		`"error":{"type":"provider.invalid-request","message":"refused"}}],` +
		`"cursor":{"previous":"cHJldmlvdXM","next":"b3du"}}`
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, pages: map[string]string{
			"first":   failed,
			"b3du":    openCodePage(openCodeSettled("msg_2", 1015, 200, 0)),
			"Ym9ndXM": openCodePage(openCodeSettled("msg_9", 9000, 0, 0)),
		}})
		got, why, _ := executor.StreamRequest(executor.OpenCode{}, streamLines(run.stdout)...)
		if want := (sandbox.RequestMeasure{Tokens: 1215}); got != want {
			t.Errorf("the run's stream reads %+v (%s), want %+v, the page the answer's own cursor names:\n%s", got,
				why, want, strings.Join(listed(t, run), "\n"))
		}
	})
}
