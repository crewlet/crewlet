package executor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// THE RECORDS EVERY CASE BELOW READS ARE SHAPED AS CLAUDE CODE WRITES THEM, key
// for key and type for type, as measured on 2.1.310 and 2.1.322 driven as the
// engine drives a box's executor: a main thread's bookkeeping, its brief, its
// model's calls and their results, the notes' hook records at a batch and at
// Stop, a firing that timed out, a subagent's prompt, its coordinator's message
// and the main thread's notification of its end, and a skill's text; and, as
// 2.1.118 wrote it, a skill's PostToolUse hook. Every id in them is synthetic,
// and long text is shortened. A compaction's boundary is the one record no
// measured run wrote, so it is built as 2.1.310's own code builds one.

const (
	ctSession   = "5e55100a-0000-4000-8000-000000000001"
	ctOther     = "5e55100a-0000-4000-8000-000000000002"
	ctPrompt    = "9707b7a1-0000-4000-8000-000000000001"
	ctAgent     = "a0123456789abcdef"
	ctBatchFire = "hook-f1ee0001-0000-4000-8000-000000000001"
	ctLateFire  = "hook-f1ee0002-0000-4000-8000-000000000002"
	ctStopFire  = "f1ee0003-0000-4000-8000-000000000003"
	ctCall      = "toolu_test_b1"
	ctMCPCall   = "toolu_test_m1"
	ctSkillCall = "toolu_test_skill"
	ctStamp     = "2026-01-02T03:04:05.000Z"
)

// ctUUID is record n's synthetic uuid.
func ctUUID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

// ctTail is the keys a chained record ends with, as release writes them.
func ctTail(release string) string {
	return `"userType":"external","entrypoint":"sdk-cli","cwd":"/tmp/box","sessionId":"` + ctSession +
		`","version":"` + release + `","gitBranch":"HEAD"`
}

// ctParent is a parentUuid as written: null for "", else the quoted uuid.
func ctParent(parent string) string {
	if parent == "" {
		return "null"
	}
	return `"` + parent + `"`
}

// The harness's bookkeeping: records outside the chain, which carry no parentUuid.
var (
	ctEnqueue = `{"type":"queue-operation","operation":"enqueue","timestamp":"` + ctStamp + `","sessionId":"` +
		ctSession + `","content":"the brief\n"}`
	ctDequeue = `{"type":"queue-operation","operation":"dequeue","timestamp":"` + ctStamp + `","sessionId":"` +
		ctSession + `"}`
	ctAtisLatch    = `{"type":"atis-latch","atis":"","sessionId":"` + ctSession + `"}`
	ctRequestShape = `{"type":"api-request-shape","sessionId":"` + ctSession + `","shapeHash":"5a5a5a5a","timestamp":"` +
		ctStamp + `","version":"2.1.310","shape":{"system":[{"type":"text","text":"You are a Claude agent."}],` +
		`"tools":[{"name":"Bash","description":"Runs a command.","input_schema":{"type":"object",` +
		`"properties":{"command":{"type":"string"}}}}]}}`
	// ctRequestBlob repeats what a request sent the model: a call's result, the
	// note appended to it.
	ctRequestBlob = `{"type":"api-request-blob","sessionId":"` + ctSession + `","hash":"b1b1b1b1","message":` +
		`{"role":"user","content":[{"tool_use_id":"` + ctCall + `","type":"tool_result","content":"b1\n\n` +
		`<system-reminder>\nPostToolBatch hook additional context: ## A note\n</system-reminder>","is_error":false}]}}`
	ctRequest = `{"type":"api-request","sessionId":"` + ctSession + `","id":"` + ctUUID(900) + `","timestamp":"` +
		ctStamp + `","version":"2.1.310","querySource":"sdk","shapeHash":"5a5a5a5a","params":{"model":` +
		`"claude-sonnet-4-5","max_tokens":32000},"base":"` + ctUUID(901) + `","keep":1,"tail":["b1b1b1b1"],` +
		`"cacheControl":[{"path":["system",0],"value":{"type":"ephemeral"}}],"requestDigest":"d1d1d1d1"}`
	ctLastPrompt = `{"type":"last-prompt","lastPrompt":"the brief","leafUuid":"` + ctUUID(5) + `","sessionId":"` +
		ctSession + `"}`
	ctCostState = `{"type":"cost-state","sessionId":"` + ctSession + `","totalCostUSD":0.0036,"totalAPIDuration":118,` +
		`"totalAPIDurationWithoutRetries":110,"totalToolDuration":61,"totalLinesAdded":0,"totalLinesRemoved":0,` +
		`"totalDuration":21173,"startTime":1767323045000,"modelUsage":{"claude-sonnet-4-5":{"inputTokens":600,` +
		`"outputTokens":120,"costUSD":0.0036}},"hasUnknownModelCost":false}`
	ctToolsUsed = `{"type":"tools-used","tools":["Bash"],"sessionId":"` + ctSession + `"}`
	ctMode      = `{"type":"mode","mode":"normal","sessionId":"` + ctSession + `"}`
)

// ctBrief is a session's first prompt, which roots its chain.
func ctBrief(uuid string) string {
	return `{"parentUuid":null,"isSidechain":false,"promptId":"` + ctPrompt + `","type":"user","message":` +
		`{"role":"user","content":"the brief\n"},"uuid":"` + uuid + `","timestamp":"` + ctStamp +
		`","permissionMode":"bypassPermissions","promptSource":"sdk","turnOrigin":"sdk",` +
		`"turnPosition":{"promptIndex":0,"turnIndex":1},` + ctTail("2.1.310") + `}`
}

// ctAttachment is an attachment record holding attachment, after its own keys
// those the record carries beside it (each ending in a comma).
func ctAttachment(parent, uuid, attachment, beside string) string {
	return `{"parentUuid":` + ctParent(parent) + `,"isSidechain":false,"attachment":` + attachment +
		`,"type":"attachment","uuid":"` + uuid + `","timestamp":"` + ctStamp + `",` + beside + ctTail("2.1.310") + `}`
}

// ctTokens is the reminder of a run's tokens, an attachment that is no hook's,
// and what its record renders of it.
const (
	ctTokens         = `{"type":"total_tokens_reminder","text":"<total_tokens>15000000 tokens left</total_tokens>"}`
	ctTokensRendered = `"rendered":[{"content":"<system-reminder>\n<total_tokens>15000000 tokens left</total_tokens>` +
		`\n</system-reminder>"}],"renderedRole":"system",`
)

// ctAssistant is a model's message whose content is content.
func ctAssistant(parent, uuid, content string) string {
	return `{"parentUuid":` + ctParent(parent) + `,"isSidechain":false,"message":{"id":"msg_test","type":"message",` +
		`"role":"assistant","model":"claude-sonnet-4-5","content":` + content + `,"stop_reason":"tool_use",` +
		`"stop_sequence":null,"usage":{"input_tokens":100,"cache_creation_input_tokens":0,` +
		`"cache_read_input_tokens":0,"output_tokens":20}},"apiBlockIndex":0,"requestId":"req_test","requestRef":"` +
		ctUUID(900) + `","requestedModel":"claude-sonnet-4-5","type":"assistant","uuid":"` + uuid +
		`","timestamp":"` + ctStamp + `","perTurnEffort":null,` + ctTail("2.1.310") + `}`
}

// ctResult is the user record answering a call: content its message's, result
// the record's own toolUseResult.
func ctResult(parent, uuid, content, result string) string {
	return `{"parentUuid":` + ctParent(parent) + `,"isSidechain":false,"promptId":"` + ctPrompt + `","type":"user",` +
		`"message":{"role":"user","content":` + content + `},"uuid":"` + uuid + `","timestamp":"` + ctStamp +
		`","toolUseResult":` + result + `,"sourceToolAssistantUUID":"` + parent + `","permissionDecision":` +
		`{"decision":"accept","source":"config","reasonType":"mode"},` + ctTail("2.1.310") + `}`
}

// A model's calls and their results: a shell command answered with its output,
// and an MCP tool answered with blocks.
const (
	ctUseContent = `[{"type":"tool_use","id":"` + ctCall + `","name":"Bash","input":` + ctUseInput + `}]`
	ctUseInput   = `{"command":"echo b1","description":"batch 1"}`
	ctResultBody = `[{"tool_use_id":"` + ctCall + `","type":"tool_result","content":"b1","is_error":false}]`
	ctResultOwn  = `{"stdout":"b1","stderr":"","interrupted":false,"isImage":false,"noOutputExpected":false}`
	ctMCPUse     = `[{"type":"tool_use","id":"` + ctMCPCall + `","name":"mcp__crewlet__slow","input":{"tag":"m1","seconds":0}}]`
	ctMCPBlocks  = `[{"type":"text","text":"crewlet m1 done"}]`
	ctMCPResult  = `[{"tool_use_id":"` + ctMCPCall + `","type":"tool_result","content":` + ctMCPBlocks + `}]`
)

// The notes' hook records, each its attachment: at a batch (a `hook-<uuid>`
// firing) a success holding the hook's whole answer, a system message and an
// additional context; a firing past the hook's timeout, cancelled; and the same
// three at Stop (a bare uuid, and no accountMemoryBatch).
const (
	ctAnswer = `"{\"hookSpecificOutput\":{\"hookEventName\":\"PostToolBatch\",\"additionalContext\":\"## A note\"},` +
		`\"systemMessage\":\"crewlet: a note reached the run\"}"`
	ctBatchSuccess = `{"type":"hook_success","hookName":"PostToolBatch","toolUseID":"` + ctBatchFire +
		`","hookEvent":"PostToolBatch","content":"","stdout":` + ctAnswer + `,"stderr":"","exitCode":0,` +
		`"command":"crewlet/steer_take","accountMemoryBatch":false}`
	ctBatchSystem = `{"type":"hook_system_message","content":"crewlet: a note reached the run","hookName":` +
		`"PostToolBatch","toolUseID":"` + ctBatchFire + `","hookEvent":"PostToolBatch","accountMemoryBatch":false}`
	ctBatchContext = `{"type":"hook_additional_context","content":["## A note"],"hookName":"PostToolBatch",` +
		`"toolUseID":"` + ctBatchFire + `","hookEvent":"PostToolBatch","accountMemoryBatch":false}`
	ctBatchRendered = `"rendered":[{"content":"<system-reminder>\nPostToolBatch hook additional context: ## A note\n` +
		`</system-reminder>"}],"renderedRole":"system",`
	ctCancelled = `{"type":"hook_cancelled","hookName":"PostToolBatch","toolUseID":"` + ctLateFire +
		`","hookEvent":"PostToolBatch","command":"crewlet/steer_take","timedOut":true,"timeoutMs":10000,` +
		`"accountMemoryBatch":false}`
	ctStopAnswer = `"{\"hookSpecificOutput\":{\"hookEventName\":\"Stop\",\"additionalContext\":\"## A note\"},` +
		`\"systemMessage\":\"crewlet: a note reached the run as it was finishing\"}"`
	ctStopSuccess = `{"type":"hook_success","hookName":"Stop","toolUseID":"` + ctStopFire + `","hookEvent":"Stop",` +
		`"content":"","stdout":` + ctStopAnswer + `,"stderr":"","exitCode":0,"command":"crewlet/steer_take"}`
	ctStopSystem = `{"type":"hook_system_message","content":"crewlet: a note reached the run as it was finishing",` +
		`"hookName":"Stop","toolUseID":"` + ctStopFire + `","hookEvent":"Stop"}`
	ctStopContext = `{"type":"hook_additional_context","content":["## A note"],"hookName":"Stop","toolUseID":"` +
		ctStopFire + `","hookEvent":"Stop"}`
	ctStopRendered = `"rendered":[{"content":"<system-reminder>\nStop hook additional context: ## A note\n` +
		`</system-reminder>"}],"renderedRole":"system",`
)

// ctSummary is the summary a Stop firing ends with, its firing at the top.
func ctSummary(parent, uuid string) string {
	return `{"parentUuid":"` + parent + `","isSidechain":false,"type":"system","subtype":"stop_hook_summary",` +
		`"hookCount":1,"hookInfos":[{"command":"crewlet/steer_take"}],"hookErrors":[],"hookAdditionalContext":` +
		`["## A note"],"preventedContinuation":false,"stopReason":"","hasOutput":true,"level":"suggestion",` +
		`"timestamp":"` + ctStamp + `","uuid":"` + uuid + `","toolUseID":"` + ctStopFire + `",` + ctTail("2.1.310") + `}`
}

// ctMain is a main thread as 2.1.310 writes one, line by line: a batch whose
// hook took a note, an MCP call whose batch's hook timed out, and a Stop that
// took one.
var ctMain = []string{
	ctEnqueue, // 1
	ctDequeue, // 2
	ctBrief(ctUUID(1)),
	ctAttachment(ctUUID(1), ctUUID(2), ctTokens, ctTokensRendered),
	ctAtisLatch, // 5
	ctRequestShape,
	ctAssistant(ctUUID(2), ctUUID(3), `[{"type":"text","text":"batch 1."}]`),
	ctAssistant(ctUUID(3), ctUUID(4), ctUseContent), // 8: a call
	ctLastPrompt,
	ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn), // 10: its result
	ctAttachment(ctUUID(5), ctUUID(6), ctBatchSuccess, ""),    // 11-13: its batch's hook
	ctAttachment(ctUUID(6), ctUUID(7), ctBatchSystem, ""),
	ctAttachment(ctUUID(7), ctUUID(8), ctBatchContext, ctBatchRendered),
	ctRequestBlob,
	ctRequest, // 15
	ctAssistant(ctUUID(8), ctUUID(9), ctMCPUse),               // 16: a call
	ctResult(ctUUID(9), ctUUID(10), ctMCPResult, ctMCPBlocks), // 17: its result
	ctAttachment(ctUUID(10), ctUUID(11), ctCancelled, ""),     // 18: its batch's hook, timed out
	ctAssistant(ctUUID(11), ctUUID(12), `[{"type":"text","text":"done."}]`),
	ctAttachment(ctUUID(12), ctUUID(13), ctStopSuccess, ""), // 20-23: the Stop hook
	ctAttachment(ctUUID(13), ctUUID(14), ctStopSystem, ""),
	ctAttachment(ctUUID(14), ctUUID(15), ctStopContext, ctStopRendered),
	ctSummary(ctUUID(15), ctUUID(16)),
	ctCostState, // 24
	ctToolsUsed,
}

// ctSubagentPrompt is a subagent's prompt, which roots its own thread's chain.
func ctSubagentPrompt(uuid string) string {
	return `{"parentUuid":null,"isSidechain":true,"promptId":"` + ctPrompt + `","agentId":"` + ctAgent +
		`","type":"user","message":{"role":"user","content":"the subagent's task"},"uuid":"` + uuid +
		`","timestamp":"` + ctStamp + `",` + ctTail("2.1.310") + `}`
}

// ctCoordinator is what a subagent's thread records of a message its
// coordinator sent it while it worked.
func ctCoordinator(parent, uuid string) string {
	return `{"parentUuid":"` + parent + `","isSidechain":true,"promptId":"` + ctPrompt + `","agentId":"` + ctAgent +
		`","type":"user","message":{"role":"user","content":"The coordinator sent a message while you were ` +
		`working:\ncontinue\n\nAddress this before completing your current task."},"isMeta":true,"uuid":"` + uuid +
		`","timestamp":"` + ctStamp + `","origin":{"kind":"coordinator"},` + ctTail("2.1.310") + `}`
}

// ctNotificationText is a task notification's text: the subagent, and the call
// that started it.
const ctNotificationText = "<task-notification>\n<task-id>" + ctAgent +
	"</task-id>\n<tool-use-id>toolu_test_agent</tool-use-id>\n<status>completed</status>\n</task-notification>"

// ctNotificationOrigin is the origin a task notification is recorded with.
const ctNotificationOrigin = `"origin":{"kind":"task-notification","producer":"session-task","runId":"run-test"}`

// ctNotification is the main thread's record of a subagent's end.
func ctNotification(parent, uuid string) string {
	return `{"parentUuid":"` + parent + `","isSidechain":false,"promptId":"` + ctPrompt + `","type":"user",` +
		`"message":{"role":"user","content":"` + strings.ReplaceAll(ctNotificationText, "\n", `\n`) +
		`"},"uuid":"` + uuid + `","timestamp":"` + ctStamp + `","permissionMode":"bypassPermissions",` +
		ctNotificationOrigin + `,"promptSource":"system","turnOrigin":"task_notification",` +
		`"turnPosition":{"promptIndex":0,"turnIndex":2},"queueSkipAttachments":true,` + ctTail("2.1.310") + `}`
}

// ctSkillHook is a skill's PostToolUse hook's success, as 2.1.118 records it:
// its firing is the id of the call it followed.
func ctSkillHook(parent, uuid string) string {
	return `{"parentUuid":"` + parent + `","isSidechain":false,"attachment":{"type":"hook_success","hookName":` +
		`"PostToolUse:Skill","toolUseID":"` + ctSkillCall + `","hookEvent":"PostToolUse","content":` +
		`"PostToolUse:Skill completed","stdout":"","stderr":"","command":"crewlet/skill_loaded","durationMs":5},` +
		`"type":"attachment","uuid":"` + uuid + `","timestamp":"` + ctStamp + `",` + ctTail("2.1.118") + `}`
}

// ctSkillText is the text a skill's call hands the model: a user record of
// text blocks.
func ctSkillText(parent, uuid string) string {
	return `{"parentUuid":"` + parent + `","isSidechain":false,"promptId":"` + ctPrompt + `","type":"user",` +
		`"message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /tmp/box/skill"}]},` +
		`"isMeta":true,"turnCompanion":true,"uuid":"` + uuid + `","timestamp":"` + ctStamp + `","sourceToolUseID":"` +
		ctSkillCall + `",` + ctTail("2.1.310") + `}`
}

// ctBoundary is a compaction's boundary, as 2.1.310's own code builds one and
// its writer chains it.
func ctBoundary(logicalParent, uuid string) string {
	return `{"parentUuid":null,"logicalParentUuid":"` + logicalParent + `","isSidechain":false,"type":"system",` +
		`"subtype":"compact_boundary","content":"Conversation compacted","level":"info","compactMetadata":` +
		`{"trigger":"auto","preTokens":160000},"uuid":"` + uuid + `","timestamp":"` + ctStamp + `",` +
		ctTail("2.1.310") + `}`
}

// ctThread is lines as a thread's file holds them, each ended by its break.
func ctThread(lines ...string) string {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// ctReading is what a reader made of a member: every line it handed out, every
// line it read as no record, what ended the reading, what one more Next
// answered after that, and whether the member ended torn.
type ctReading struct {
	lines  []threadLine
	broken []*threadLineError
	end    error
	again  error
	torn   bool
}

// ctRead reads text, one member, within maxBytes, to the end of the reading.
func ctRead(text string, maxBytes int64) ctReading {
	return ctReadFrom(strings.NewReader(text), maxBytes)
}

// ctReadFrom reads what member hands out, one member, within maxBytes, to the
// end of the reading.
func ctReadFrom(member io.Reader, maxBytes int64) ctReading {
	r := newThreadReader(member, maxBytes)
	var got ctReading
	for {
		line, err := r.Next()
		var broken *threadLineError
		switch {
		case err == nil:
			got.lines = append(got.lines, line)
		case errors.As(err, &broken):
			got.broken = append(got.broken, broken)
		default:
			got.end, got.torn = err, r.Torn()
			_, got.again = r.Next()
			return got
		}
	}
}

// ctLine is record read as the one line of a thread.
func ctLine(t *testing.T, record string) threadLine {
	t.Helper()
	got := ctRead(ctThread(record), threadPlainMax)
	if len(got.lines) != 1 || len(got.broken) != 0 || !errors.Is(got.end, io.EOF) {
		t.Fatalf("%.120s reads as %d lines and %d broken, ended by %v; want one record",
			record, len(got.lines), len(got.broken), got.end)
	}
	return got.lines[0]
}

// ctTypes is the type of every line read.
func ctTypes(lines []threadLine) []string {
	types := make([]string, 0, len(lines))
	for _, line := range lines {
		types = append(types, line.Head.Type)
	}
	return types
}

// A THREAD IS READ A WHOLE LINE AT A TIME: a line is what ends in "\n", its
// break dropped, and a last piece with none is never a line, whatever it holds
// (a record its harness never finished writing, or the NULs GNU tar fills a
// member that shrank out with). Torn says the piece was there. A carriage
// return ends no line: before a break it is the line's own, which JSON reads
// as whitespace, and alone it breaks nothing. The member's end is the end:
// asked again, the reader answers it again.
//
// Mutation: hand out the last piece with no break as a line, and the torn
// record, the padding and a piece a carriage return ends each come back as a
// line, or a line that is no record, where the member's end belongs.
func TestAThreadIsReadAWholeLineAtATime(t *testing.T) {
	t.Parallel()
	brief := ctBrief(ctUUID(1))
	reply := ctAssistant(ctUUID(1), ctUUID(2), `[{"type":"text","text":"batch 1."}]`)
	whole := ctThread(ctEnqueue, brief)
	both := []string{"queue-operation", "user"}
	for name, tc := range map[string]struct {
		text  string
		types []string
		torn  bool
	}{
		"a member that ends in a break":             {text: whole, types: both},
		"a last record its harness never finished":  {text: whole + reply[:len(reply)/2], types: both, torn: true},
		"NUL padding after the last break":          {text: whole + strings.Repeat("\x00", 512), types: both, torn: true},
		"records each ended by CRLF":                {text: ctEnqueue + "\r\n" + brief + "\r\n", types: both},
		"a record ended by a carriage return alone": {text: whole + reply + "\r", types: both, torn: true},
		"an empty member":                           {text: ""},
	} {
		got := ctRead(tc.text, threadPlainMax)
		for _, broken := range got.broken {
			t.Errorf("%s: line %d read as no record: %v", name, broken.N, broken)
		}
		if types := ctTypes(got.lines); !slices.Equal(types, tc.types) {
			t.Errorf("%s: the lines read are of types %q, want %q", name, strings.Join(types, " "),
				strings.Join(tc.types, " "))
		}
		for i, line := range got.lines {
			if line.N != i+1 || bytes.HasSuffix(line.Raw, []byte("\n")) {
				t.Errorf("%s: line %d is numbered %d and keeps its break %t", name, i+1, line.N,
					bytes.HasSuffix(line.Raw, []byte("\n")))
			}
		}
		if !errors.Is(got.end, io.EOF) || got.torn != tc.torn || !errors.Is(got.again, io.EOF) {
			t.Errorf("%s: the reading ended with %v, torn %t, and then %v; want io.EOF, torn %t, and io.EOF again",
				name, got.end, got.torn, got.again, tc.torn)
		}
	}
	got := ctRead(ctEnqueue+"\r\n"+brief+"\n", threadPlainMax)
	if len(got.lines) != 2 || string(got.lines[0].Raw) != ctEnqueue+"\r" || string(got.lines[1].Raw) != brief {
		t.Errorf("a line's bytes are not the record's with its break dropped and its carriage return kept")
	}
}

// ctPadded is a whole line of exactly n bytes, its break included: a queue
// operation whose content fills it out.
func ctPadded(n int) string {
	head := `{"type":"queue-operation","operation":"enqueue","timestamp":"` + ctStamp + `","sessionId":"` +
		ctSession + `","content":"`
	tail := `"}` + "\n"
	return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
}

// A LINE OR A MEMBER PAST ITS BOUND ENDS THE READING: a line one byte longer
// than the take's scrub would hold, or a member one byte longer than its
// reader's bound, ends it with errThreadBound, and the reading stays over, the
// record after the line never read. A member past its bound is past it EVEN
// WHERE THE BOUND FALLS RIGHT AFTER A LINE BREAK, where a member metered by
// io.LimitReader would end cleanly and read as whole. A line or a member AT
// its bound reads whole, and the bounds are the take's scrub's own.
//
// Mutation: skip a line past the bound and read on, and the record after it
// is read; meter the member with io.LimitReader, and a member past its bound
// by a break, a record or a line ends cleanly.
func TestALineOrAMemberPastItsBoundEndsTheReading(t *testing.T) {
	t.Parallel()
	kept := boundsFor(executorkeep.SessionMaxBytes)
	if threadLineMax != kept.piece || threadPlainMax != kept.plain {
		t.Errorf("a thread is read within %d bytes a line and %d a member, the take's scrub within %d and %d",
			threadLineMax, threadPlainMax, kept.piece, kept.plain)
	}
	brief := ctBrief(ctUUID(1))
	head := ctThread(ctEnqueue, brief)
	n := int64(len(head))
	for name, tc := range map[string]struct {
		text     string
		maxBytes int64
		lines    int
		past     bool
	}{
		"a line at its bound, its break included": {
			text: ctPadded(threadLineMax) + ctThread(brief), maxBytes: threadPlainMax, lines: 2},
		"a line a byte past its bound, a record after it": {
			text: ctThread(ctEnqueue) + ctPadded(threadLineMax+1) + ctThread(brief), maxBytes: threadPlainMax,
			lines: 1, past: true},
		"a member at its bound": {text: head, maxBytes: n, lines: 2},
		"a member a byte past its bound, which falls right after a break": {
			text: head + "\n", maxBytes: n, lines: 2, past: true},
		"a member past its bound by a record that starts on it": {
			text: head + ctThread(ctDequeue), maxBytes: n, lines: 2, past: true},
		"a member past its bound by a piece with no break": {text: head + "{", maxBytes: n, lines: 2, past: true},
		"a member whose bound falls inside a line":         {text: head, maxBytes: n - 1, lines: 1, past: true},
		"an empty member at a bound of none":               {text: "", maxBytes: 0},
		"a break at a bound of none":                       {text: "\n", maxBytes: 0, past: true},
		"a break at a bound below none":                    {text: "\n", maxBytes: -1, past: true},
		"an empty member at a bound below none":            {text: "", maxBytes: -1},
	} {
		got := ctRead(tc.text, tc.maxBytes)
		if len(got.lines) != tc.lines || len(got.broken) != 0 {
			t.Errorf("%s: %d lines read and %d broken, want %d and none", name, len(got.lines), len(got.broken), tc.lines)
		}
		switch {
		case tc.past && (!errors.Is(got.end, errThreadBound) || !errors.Is(got.again, errThreadBound)):
			t.Errorf("%s: the reading ended with %v and then %v, want errThreadBound both times", name, got.end, got.again)
		case !tc.past && (!errors.Is(got.end, io.EOF) || got.torn):
			t.Errorf("%s: the reading ended with %v, torn %t, want a whole member's io.EOF", name, got.end, got.torn)
		}
	}
}

// A MEMBER'S READER THAT FAILS ENDS THE READING WITH ITS ERROR, never with the
// member's end: an archive cut short (tar and gzip answer io.ErrUnexpectedEOF)
// or a box's stream reset part way through a member ends the reading with that
// error once the whole lines before it are read, the half record before it is
// no torn piece, and asked again, the reader answers the same error. Only
// io.EOF is the member's end: read as one, a cut stream would pass for a
// thread whose last write tore, and what the cut took (a pair, a compaction's
// boundary) would be neither read nor missed.
//
// Mutation: read a reader's error as the member's end, the half record before
// it its torn piece, and each reading ends with io.EOF.
func TestAMembersReaderThatFailsEndsTheReadingWithItsError(t *testing.T) {
	t.Parallel()
	reset := errors.New("read: connection reset by peer")
	whole := ctThread(ctEnqueue, ctBrief(ctUUID(1)))
	call := ctAssistant(ctUUID(1), ctUUID(2), ctUseContent)
	for name, tc := range map[string]struct {
		text string
		err  error
	}{
		"a reset part way through a record":        {text: whole + call[:len(call)/2], err: reset},
		"an archive cut part way through a record": {text: whole + call[:len(call)/2], err: io.ErrUnexpectedEOF},
		"a reset right after a break":              {text: whole, err: reset},
		"an archive cut right after a break":       {text: whole, err: io.ErrUnexpectedEOF},
	} {
		got := ctReadFrom(io.MultiReader(strings.NewReader(tc.text), iotest.ErrReader(tc.err)), threadPlainMax)
		if types := ctTypes(got.lines); len(got.broken) != 0 || !slices.Equal(types, []string{"queue-operation", "user"}) {
			t.Errorf("%s: the lines read are of types %q, and %d broken; want the two before the failure, none broken",
				name, strings.Join(types, " "), len(got.broken))
		}
		if !errors.Is(got.end, tc.err) || errors.Is(got.end, io.EOF) || got.torn || !errors.Is(got.again, tc.err) {
			t.Errorf("%s: the reading ended with %v, torn %t, and then %v; want %v, not torn, and %v again",
				name, got.end, got.torn, got.again, tc.err, tc.err)
		}
	}
}

// EVERY WHOLE LINE IS DECODED INTO ITS HEAD BEFORE ANYTHING READS IT: a line
// that is not one record (two values, one cut short, a value that is no
// object, a string of bytes that are no UTF-8, which Claude Code never
// writes) or one whose head key is of the wrong type (`type`, `subtype` and
// `agentId` strings, `parentUuid` null or a string, `isSidechain` a boolean)
// is a *threadLineError naming its line, whatever keywords it holds, and the
// reader reads on to the next line; and a whole record's head is what it
// says. None of these lines holds a key a reader of hooks or calls looks for.
//
// Mutation: decode only the lines that name a hook or a call, and every
// broken line here reads as a record, and the records' heads as nothing;
// take bytes that are no UTF-8 as encoding/json does, for the replacement
// character, and the line holding them reads as a record.
func TestEveryWholeLineIsDecodedIntoItsHeadBeforeAnything(t *testing.T) {
	t.Parallel()
	brief := ctBrief(ctUUID(1))
	summary := ctSummary(ctUUID(1), ctUUID(2))
	prompt := ctSubagentPrompt(ctUUID(3))
	broken := map[string]string{
		"two values":                             ctDequeue + ctDequeue,
		"two values a carriage return parts":     ctDequeue + "\r" + ctDequeue,
		"a value cut short":                      brief[:len(brief)/2],
		"a numeric type":                         strings.Replace(ctDequeue, `"type":"queue-operation"`, `"type":7`, 1),
		"a null type":                            strings.Replace(ctDequeue, `"type":"queue-operation"`, `"type":null`, 1),
		"a string isSidechain":                   strings.Replace(brief, `"isSidechain":false`, `"isSidechain":"false"`, 1),
		"a null isSidechain":                     strings.Replace(brief, `"isSidechain":false`, `"isSidechain":null`, 1),
		"a numeric parentUuid":                   strings.Replace(brief, `"parentUuid":null`, `"parentUuid":7`, 1),
		"a parentUuid that is an object":         strings.Replace(brief, `"parentUuid":null`, `"parentUuid":{}`, 1),
		"a boolean subtype":                      strings.Replace(summary, `"subtype":"stop_hook_summary"`, `"subtype":true`, 1),
		"a numeric agentId":                      strings.Replace(prompt, `"agentId":"`+ctAgent+`"`, `"agentId":7`, 1),
		"null":                                   "null",
		"a list holding a record":                "[" + ctDequeue + "]",
		"a string":                               `"queue-operation"`,
		"nothing at all":                         "",
		"NULs before a break":                    "\x00\x00\x00",
		"a record behind a byte that is no JSON": "\x00" + ctDequeue,
		"a string of bytes that are no UTF-8": strings.Replace(ctDequeue, `"operation":"dequeue"`,
			"\"operation\":\"deq\xffueue\"", 1),
	}
	names := slices.Sorted(maps.Keys(broken))
	lines := make([]string, 0, 2*len(names)+3)
	for _, name := range names {
		lines = append(lines, ctDequeue, broken[name])
	}
	lines = append(lines, brief, summary, prompt)
	got := ctRead(ctThread(lines...), threadPlainMax)
	if len(got.broken) != len(names) || !errors.Is(got.end, io.EOF) {
		t.Fatalf("%d lines read as no record and the reading ended with %v, want %d and io.EOF",
			len(got.broken), got.end, len(names))
	}
	for i, name := range names {
		if got.broken[i].N != 2*(i+1) || got.broken[i].Unwrap() == nil {
			t.Errorf("%s: line %d is no record, and the error names line %d (%v)", name, 2*(i+1), got.broken[i].N,
				got.broken[i])
		}
	}
	if len(got.lines) != len(names)+3 {
		t.Fatalf("%d lines read as records, want %d", len(got.lines), len(names)+3)
	}
	for i, line := range got.lines[:len(names)] {
		if line.N != 2*i+1 || line.Head.Type != "queue-operation" {
			t.Errorf("the record before broken line %d reads as line %d of type %q", 2*(i+1), line.N, line.Head.Type)
		}
	}
	isFalse, isTrue := false, true
	for i, want := range []threadHead{
		{Type: "user", Parent: jsonNull, Sidechain: &isFalse},
		{Type: "system", Subtype: hookStopSummary, Parent: []byte(`"` + ctUUID(1) + `"`), Sidechain: &isFalse},
		{Type: "user", Parent: jsonNull, Sidechain: &isTrue, AgentID: ctAgent},
	} {
		h := got.lines[len(names)+i].Head
		if h.Type != want.Type || h.Subtype != want.Subtype || !bytes.Equal(h.Parent, want.Parent) ||
			h.Sidechain == nil || *h.Sidechain != *want.Sidechain || h.AgentID != want.AgentID {
			t.Errorf("record %d's head is %+v, want %+v", len(names)+i+1, h, want)
		}
	}
	if main := []bool{got.lines[0].Head.Main(), got.lines[len(names)].Head.Main(), got.lines[len(names)+1].Head.Main(),
		got.lines[len(names)+2].Head.Main()}; !slices.Equal(main, []bool{false, true, true, false}) {
		t.Errorf("the bookkeeping, the brief, the summary and the subagent's prompt read as the main thread's %v, "+
			"want false, true, true and false", main)
	}
}

// A RECORD IS THE MAIN THREAD'S ONLY WHERE IT SAYS SO: isSidechain present and
// false, and no agentId. A record that names a subagent is no main thread's
// whatever its isSidechain says, a sidechain's record is none though it names
// no subagent, and a record that says neither (the harness's bookkeeping) is
// none, so a reader of the main thread's hook records never pairs a
// subagent's.
//
// Mutation: drop either half of the rule, and a record that names a subagent,
// or a sidechain's, reads as the main thread's.
func TestARecordIsTheMainThreadsOnlyWhereItSaysSo(t *testing.T) {
	t.Parallel()
	brief, prompt := ctBrief(ctUUID(1)), ctSubagentPrompt(ctUUID(3))
	for name, tc := range map[string]struct {
		record string
		main   bool
	}{
		"the brief":           {record: brief, main: true},
		"a hook's record":     {record: ctAttachment(ctUUID(6), ctUUID(7), ctBatchSystem, ""), main: true},
		"a Stop's summary":    {record: ctSummary(ctUUID(15), ctUUID(16)), main: true},
		"a subagent's prompt": {record: prompt},
		"a record that names a subagent and says it is no sidechain's": {
			record: strings.Replace(prompt, `"isSidechain":true`, `"isSidechain":false`, 1)},
		"a sidechain's record that names no subagent": {
			record: strings.Replace(brief, `"isSidechain":false`, `"isSidechain":true`, 1)},
		"the harness's bookkeeping": {record: ctDequeue},
	} {
		if got := ctLine(t, tc.record).Head.Main(); got != tc.main {
			t.Errorf("%s reads as the main thread's %t, want %t", name, got, tc.main)
		}
	}
}

// A KEY ONLY ONE READER READS COSTS THAT READER ALONE: an attachment that is
// no object leaves the record's head and its message whole and is refused by
// Hook alone, and an origin that is no object leaves the message and the hooks
// whole and is refused by OriginKind alone. As written, each reads: an origin's
// kind, and a user record's own toolUseResult, raw, which no other record
// hands out.
//
// Mutation: hold the attachment to an object in the head, and the record whose
// attachment is not one is no record at all.
func TestAKeyOneReaderReadsCostsThatReaderAlone(t *testing.T) {
	t.Parallel()
	attachment := ctLine(t, ctAttachment(ctUUID(1), ctUUID(2), `"hook_success"`, ""))
	if h := attachment.Head; h.Type != recordAttachment || !h.Chained() || !h.Main() {
		t.Errorf("a record whose attachment is no object reads with the head %+v", h)
	}
	if m, err := attachment.Message(); err != nil || m.Kinds() != nil || m.Calls() != nil {
		t.Errorf("a record whose attachment is no object reads its message as %+v, %v", m, err)
	}
	if kind, err := attachment.OriginKind(); kind != "" || err != nil {
		t.Errorf("a record whose attachment is no object reads its origin as %q, %v", kind, err)
	}
	if _, _, err := attachment.Hook(); err == nil {
		t.Error("an attachment that is no object reads as a hook's record or none, never as one it cannot read")
	}
	summary := ctSummary(ctUUID(15), ctUUID(16))
	for name, record := range map[string]string{
		"a system message whose content is a list": strings.Replace(ctBatchSystem,
			`"content":"crewlet: a note reached the run"`, `"content":["crewlet: a note reached the run"]`, 1),
		"an additional context whose content is a string": strings.Replace(ctBatchContext, `"content":["## A note"]`,
			`"content":"## A note"`, 1),
		"an additional context with no content": strings.Replace(ctBatchContext, `"content":["## A note"],`, "", 1),
		"a success with no stdout":              strings.Replace(ctBatchSuccess, `"stdout":`+ctAnswer+`,`, "", 1),
		"a success whose stdout is an object":   strings.Replace(ctBatchSuccess, `"stdout":`+ctAnswer, `"stdout":{}`, 1),
		"a success whose content is a number":   strings.Replace(ctBatchSuccess, `"content":""`, `"content":0`, 1),
		"a firing that is a number": strings.Replace(ctBatchSystem, `"toolUseID":"`+ctBatchFire+`"`,
			`"toolUseID":7`, 1),
		"a firing that names none": strings.Replace(ctBatchSystem, `"toolUseID":"`+ctBatchFire+`"`,
			`"toolUseID":""`, 1),
		"a firing with no event": strings.Replace(ctBatchContext, `"hookEvent":"PostToolBatch",`, "", 1),
		"a firing whose event names none": strings.Replace(ctBatchContext, `"hookEvent":"PostToolBatch"`,
			`"hookEvent":""`, 1),
		"a cancellation whose command is a number": strings.Replace(ctCancelled, `"command":"crewlet/steer_take"`,
			`"command":7`, 1),
		"an attachment whose type is a number": strings.Replace(ctBatchSystem, `"type":"hook_system_message"`,
			`"type":7`, 1),
		"an attachment with no type": strings.Replace(ctBatchSystem, `"type":"hook_system_message",`, "", 1),
	} {
		line := ctLine(t, ctAttachment(ctUUID(6), ctUUID(7), record, ""))
		if hook, isHook, err := line.Hook(); err == nil {
			t.Errorf("%s reads as the hook %+v (%t), want an error", name, hook, isHook)
		}
		if m, err := line.Message(); err != nil || line.Head.Type != recordAttachment || !line.Head.Main() {
			t.Errorf("%s reads with the head %+v and the message %+v, %v", name, line.Head, m, err)
		}
	}
	for name, record := range map[string]string{
		"a summary whose firing is a number": strings.Replace(summary, `"toolUseID":"`+ctStopFire+`"`,
			`"toolUseID":7`, 1),
		"a summary with no firing": strings.Replace(summary, `"toolUseID":"`+ctStopFire+`",`, "", 1),
		"a summary whose firing names none": strings.Replace(summary, `"toolUseID":"`+ctStopFire+`"`,
			`"toolUseID":""`, 1),
	} {
		if hook, isHook, err := ctLine(t, record).Hook(); err == nil {
			t.Errorf("%s reads as the hook %+v (%t), want an error", name, hook, isHook)
		}
	}

	notification := ctNotification(ctUUID(1), ctUUID(3))
	origin := ctLine(t, strings.Replace(notification, ctNotificationOrigin, `"origin":"task-notification"`, 1))
	if m, err := origin.Message(); err != nil {
		t.Errorf("a notification whose origin is no object reads no message: %v", err)
	} else if text, isText := m.Text(); !isText || text != ctNotificationText {
		t.Errorf("a notification whose origin is no object reads its message as %q", text)
	}
	if _, isHook, err := origin.Hook(); isHook || err != nil {
		t.Errorf("a notification whose origin is no object reads as a hook's record %t, %v", isHook, err)
	}
	if _, err := origin.OriginKind(); err == nil {
		t.Error("an origin that is no object reads as a kind")
	}

	for name, tc := range map[string]struct {
		record string
		kind   string
		fails  bool
	}{
		"a notification":                    {record: notification, kind: "task-notification"},
		"a coordinator's message":           {record: ctCoordinator(ctUUID(1), ctUUID(4)), kind: "coordinator"},
		"a brief, which has none":           {record: ctBrief(ctUUID(1))},
		"an origin whose kind is no string": {record: strings.Replace(notification, `"kind":"task-notification"`, `"kind":7`, 1), fails: true},
		"an origin with no kind":            {record: strings.Replace(notification, `"kind":"task-notification",`, "", 1)},
	} {
		kind, err := ctLine(t, tc.record).OriginKind()
		if kind != tc.kind || (err != nil) != tc.fails {
			t.Errorf("%s: its origin reads as %q, %v; want %q, failing %t", name, kind, err, tc.kind, tc.fails)
		}
	}

	if got := ctLine(t, ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)).ToolUseResult(); string(got) != ctResultOwn {
		t.Errorf("a result's own toolUseResult reads as %s, want it as written", got)
	}
	if got := ctLine(t, ctResult(ctUUID(9), ctUUID(10), ctMCPResult, ctMCPBlocks)).ToolUseResult(); string(got) != ctMCPBlocks {
		t.Errorf("an MCP result's own toolUseResult reads as %s, want it as written", got)
	}
	assistant := strings.Replace(ctAssistant(ctUUID(3), ctUUID(4), ctUseContent), `"apiBlockIndex"`,
		`"toolUseResult":`+ctResultOwn+`,"apiBlockIndex"`, 1)
	for name, record := range map[string]string{
		"an assistant's":                   assistant,
		"a brief's, which has none":        ctBrief(ctUUID(1)),
		"an attachment's":                  ctAttachment(ctUUID(1), ctUUID(2), ctTokens, ctTokensRendered),
		"a request blob's, which has none": ctRequestBlob,
	} {
		if got := ctLine(t, record).ToolUseResult(); got != nil {
			t.Errorf("%s toolUseResult reads as %s, want none: only a user record's is one", name, got)
		}
	}
}

// ctReplace is record with from, which it holds exactly once, written as to.
func ctReplace(t *testing.T, record, from, to string) string {
	t.Helper()
	if n := strings.Count(record, from); n != 1 {
		t.Fatalf("%.80s holds %q %d times, want once", record, from, n)
	}
	return strings.Replace(record, from, to, 1)
}

// ctAll is everything a line hands out through every accessor, as text: its
// head, its hook, its message and its calls, its origin's kind and its own
// toolUseResult, each beside the error it answered.
func ctAll(l threadLine) string {
	var b strings.Builder
	h := l.Head
	side := "absent"
	if h.Sidechain != nil {
		side = strconv.FormatBool(*h.Sidechain)
	}
	fmt.Fprintf(&b, "head: type %q, subtype %q, parent %s, sidechain %s, agent %q\n", h.Type, h.Subtype, h.Parent, side,
		h.AgentID)
	hook, isHook, err := l.Hook()
	fmt.Fprintf(&b, "hook %t: record %q, event %q, firing %q, command %q, content %s, stdout %s; %v\n", isHook,
		hook.Record, hook.Event, hook.Firing, hook.Command, hook.Content, hook.Stdout, err)
	m, err := l.Message()
	text, isText := m.Text()
	fmt.Fprintf(&b, "message: text %q (%t), kinds %q; %v\n", text, isText, strings.Join(m.Kinds(), " "), err)
	for _, c := range m.Calls() {
		failed := "absent"
		if c.IsError != nil {
			failed = strconv.FormatBool(*c.IsError)
		}
		fmt.Fprintf(&b, "call %q: id %q, name %q, input %s, is_error %s, content %s\n", c.Kind, c.ID, c.Name, c.Input,
			failed, c.Content)
	}
	kind, err := l.OriginKind()
	fmt.Fprintf(&b, "origin: kind %q; %v\n", kind, err)
	fmt.Fprintf(&b, "toolUseResult: %s\n", l.ToolUseResult())
	return b.String()
}

// A KEY IS READ EXACTLY AS WRITTEN, as Claude Code reads its own records with
// JSON.parse: a key that differs from one the layer reads only in case is
// another key, after that one or before it, and alone it leaves that one
// absent; and a key written twice holds its last value. Every object the layer
// reads is held so: a line's head and the keys only one reader reads (a
// message, an attachment, an origin, a firing at the top, a record's own
// toolUseResult), a hook's attachment, an origin, a message, and a message's
// blocks. So a line hands out exactly what it hands out without the other key,
// through every accessor. 2.1.322 already spells a call's id `toolUseId` (a
// subagent's metadata names the call that started it so), a key away from a
// hook's firing.
//
// Mutation: take a key that differs only in case for the layer's own, as
// encoding/json's decode into a struct does (the last written winning), and
// each record here reads as the other key says; refuse a key written twice, and
// a record that writes one twice is no record.
func TestAKeyIsReadExactlyAsWritten(t *testing.T) {
	t.Parallel()
	brief, prompt := ctBrief(ctUUID(1)), ctSubagentPrompt(ctUUID(3))
	result := ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)
	call := ctAssistant(ctUUID(3), ctUUID(4), ctUseContent)
	summary := ctSummary(ctUUID(15), ctUUID(16))
	notification := ctNotification(ctUUID(1), ctUUID(3))
	hook := ctAttachment(ctUUID(5), ctUUID(6), ctBatchSuccess, "")
	firing := `"toolUseID":"` + ctBatchFire + `"`
	unfired := ctReplace(t, hook, firing+",", "")
	other := "toolu_test_other"
	retyped := ctReplace(t, brief, `"type":"user"`, `"type":"attachment"`)
	for name, tc := range map[string]struct {
		record, from, to string
	}{
		// A line's head.
		"a parentUuid, then ParentUuid null": {record: result, from: `"parentUuid":"` + ctUUID(4) + `"`,
			to: `"parentUuid":"` + ctUUID(4) + `","ParentUuid":null`},
		"a null parentUuid, then PARENTUUID": {record: brief, from: `"parentUuid":null`,
			to: `"parentUuid":null,"PARENTUUID":"` + ctUUID(9) + `"`},
		"parentuuid alone": {record: ctDequeue, from: `"type":"queue-operation"`,
			to: `"type":"queue-operation","parentuuid":"` + ctUUID(9) + `"`},
		"a type, then Type": {record: brief, from: `"type":"user"`, to: `"type":"user","Type":"attachment"`},
		"a Type, then type": {record: brief, from: `"type":"user"`, to: `"Type":"attachment","type":"user"`},
		"Type alone": {record: ctReplace(t, ctDequeue, `"type":"queue-operation",`, ""), from: `"operation"`,
			to: `"Type":"user","operation"`},
		"a subtype, then Subtype": {record: summary, from: `"subtype":"stop_hook_summary"`,
			to: `"subtype":"stop_hook_summary","Subtype":"compact_boundary"`},
		"an isSidechain, then isSideChain": {record: brief, from: `"isSidechain":false`,
			to: `"isSidechain":false,"isSideChain":true`},
		"agentID alone": {record: brief, from: `"isSidechain":false`, to: `"isSidechain":false,"agentID":"` + ctAgent + `"`},
		"an agentId, then AgentId": {record: prompt, from: `"agentId":"` + ctAgent + `"`,
			to: `"agentId":"` + ctAgent + `","AgentId":"a0000000000000000"`},
		// The keys only one reader reads.
		"a message, then Message": {record: result, from: `"uuid":"` + ctUUID(5) + `"`,
			to: `"Message":{"role":"user","content":[{"tool_use_id":"` + other + `","type":"tool_result",` +
				`"content":"other"}]},"uuid":"` + ctUUID(5) + `"`},
		"an attachment, then Attachment": {record: hook, from: `,"type":"attachment"`,
			to: `,"Attachment":` + strings.Replace(ctBatchSuccess, ctBatchFire, other, 1) + `,"type":"attachment"`},
		"an origin, then Origin": {record: notification, from: ctNotificationOrigin,
			to: ctNotificationOrigin + `,"Origin":{"kind":"coordinator"}`},
		"a summary's firing, then a call's toolUseId": {record: summary, from: `"toolUseID":"` + ctStopFire + `"`,
			to: `"toolUseID":"` + ctStopFire + `","toolUseId":"` + other + `"`},
		"a summary's firing spelled ToolUseID alone": {record: ctReplace(t, summary, `"toolUseID":"`+ctStopFire+`",`, ""),
			from: `"hookCount":1`, to: `"hookCount":1,"ToolUseID":"` + ctStopFire + `"`},
		"a toolUseResult, then ToolUseResult": {record: result, from: `"toolUseResult":` + ctResultOwn,
			to: `"toolUseResult":` + ctResultOwn + `,"ToolUseResult":{"stdout":"other"}`},
		// A hook's attachment.
		"an attachment's firing, then a call's toolUseId": {record: hook, from: firing,
			to: firing + `,"toolUseId":"` + other + `"`},
		"a call's toolUseId, then an attachment's firing": {record: hook, from: firing,
			to: `"toolUseId":"` + other + `",` + firing},
		"an attachment's firing spelled TOOLUSEID alone": {record: unfired, from: `"hookName":"PostToolBatch"`,
			to: `"hookName":"PostToolBatch","TOOLUSEID":"` + ctBatchFire + `"`},
		"an attachment's type, then Type": {record: hook, from: `"type":"hook_success"`,
			to: `"type":"hook_success","Type":"hook_cancelled"`},
		"an attachment's hookEvent, then HookEvent": {record: hook, from: `"hookEvent":"PostToolBatch"`,
			to: `"hookEvent":"PostToolBatch","HookEvent":"Stop"`},
		"an attachment's command, then Command": {record: hook, from: `"command":"crewlet/steer_take"`,
			to: `"command":"crewlet/steer_take","Command":"crewlet/other"`},
		"an attachment's content, then Content": {record: hook, from: `"content":""`, to: `"content":"","Content":"other"`},
		"an attachment's stdout, then Stdout": {record: hook, from: `"stdout":` + ctAnswer,
			to: `"stdout":` + ctAnswer + `,"Stdout":"other"`},
		// An origin.
		"an origin's kind, then Kind": {record: notification, from: `"kind":"task-notification"`,
			to: `"kind":"task-notification","Kind":"coordinator"`},
		// A message.
		"a message's content, then Content": {record: result, from: `"role":"user","content":` + ctResultBody,
			to: `"role":"user","content":` + ctResultBody + `,"Content":"other"`},
		// A message's blocks.
		"a tool_use's type, then Type": {record: call, from: `"type":"tool_use"`, to: `"type":"tool_use","Type":"text"`},
		"a tool_use's id, then ID": {record: call, from: `"id":"` + ctCall + `"`,
			to: `"id":"` + ctCall + `","ID":"` + other + `"`},
		"a tool_use's name, then Name": {record: call, from: `"name":"Bash"`, to: `"name":"Bash","Name":"Read"`},
		"a tool_use's input, then Input": {record: call, from: `"input":` + ctUseInput,
			to: `"input":` + ctUseInput + `,"Input":{"command":"echo other"}`},
		"a tool_result's tool_use_id, then Tool_Use_Id": {record: result, from: `"tool_use_id":"` + ctCall + `"`,
			to: `"tool_use_id":"` + ctCall + `","Tool_Use_Id":"` + other + `"`},
		"a tool_result's is_error, then Is_Error": {record: result, from: `"is_error":false`,
			to: `"is_error":false,"Is_Error":true`},
		"a tool_result's content, then Content": {record: result, from: `"content":"b1"`,
			to: `"content":"b1","Content":"other"`},
		// A key written twice.
		"a type written twice": {record: retyped, from: `"type":"attachment"`, to: `"type":"user","type":"attachment"`},
		"an attachment's firing written twice": {record: hook, from: firing,
			to: `"toolUseID":"` + other + `",` + firing},
	} {
		variant := ctReplace(t, tc.record, tc.from, tc.to)
		if got, want := ctAll(ctLine(t, variant)), ctAll(ctLine(t, tc.record)); got != want {
			t.Errorf("%s reads as\n%swant\n%s", name, got, want)
		}
	}
}

// ABSENT IS NOT NULL: a record outside the chain (every kind of the harness's
// bookkeeping) carries no parentUuid and is neither chained nor a root, the
// brief's null roots the chain, and a chained record's parent is its quoted
// uuid, each handed over as written.
//
// Mutation: read an absent parentUuid as null, and the bookkeeping before the
// brief, and every kind of it, reads as chained, and as roots.
func TestAnAbsentParentIsNotANullOne(t *testing.T) {
	t.Parallel()
	bookkeeping := []string{ctAtisLatch, ctRequestShape, ctRequestBlob, ctRequest, ctLastPrompt, ctCostState,
		ctToolsUsed, ctMode}
	got := ctRead(ctThread(append([]string{ctEnqueue, ctDequeue, ctBrief(ctUUID(1)),
		ctAttachment(ctUUID(1), ctUUID(2), ctTokens, ctTokensRendered)}, bookkeeping...)...), threadPlainMax)
	if len(got.lines) != 4+len(bookkeeping) || len(got.broken) != 0 {
		t.Fatalf("%d lines read and %d broken, want %d and none", len(got.lines), len(got.broken), 4+len(bookkeeping))
	}
	for _, line := range append(got.lines[:2:2], got.lines[4:]...) {
		if h := line.Head; h.Chained() || h.Root() || h.Parent != nil {
			t.Errorf("line %d, a %s, carries no parentUuid and reads as chained %t, a root %t, its parent %s",
				line.N, h.Type, h.Chained(), h.Root(), h.Parent)
		}
	}
	if h := got.lines[2].Head; !h.Chained() || !h.Root() || string(h.Parent) != "null" {
		t.Errorf("the brief reads as chained %t, a root %t, its parent %s; want a root, its parent null",
			h.Chained(), h.Root(), h.Parent)
	}
	if h := got.lines[3].Head; !h.Chained() || h.Root() || string(h.Parent) != `"`+ctUUID(1)+`"` {
		t.Errorf("a chained record reads as chained %t, a root %t, its parent %s; want chained to the brief",
			h.Chained(), h.Root(), h.Parent)
	}
}

// A RESTART IS A COMPACTION'S BOUNDARY OR A LATER ROOT: the thread's first
// chained record roots the chain the thread began with and is none, however
// much bookkeeping comes before it, so a thread as measured restarts nowhere;
// a root after it is one, and so is a boundary, wherever it falls. A thread
// whose first chained record is no root (a trimmed file, its start gone) has
// every root as a restart, its first included.
//
// Mutation: count every root, and the brief restarts every thread; count every
// root but the first one seen, and a trimmed thread's first root is none.
func TestARestartIsACompactBoundaryOrALaterRoot(t *testing.T) {
	t.Parallel()
	brief, again := ctBrief(ctUUID(1)), ctBrief(ctUUID(4))
	tokens := ctAttachment(ctUUID(1), ctUUID(2), ctTokens, ctTokensRendered)
	reply := ctAssistant(ctUUID(2), ctUUID(3), `[{"type":"text","text":"batch 1."}]`)
	boundary := ctBoundary(ctUUID(3), ctUUID(5))
	after := ctAssistant(ctUUID(5), ctUUID(6), `[{"type":"text","text":"done."}]`)
	for name, tc := range map[string]struct {
		lines    []string
		restarts []int
	}{
		"a thread as measured":                            {lines: ctMain},
		"a root after the first chained record":           {lines: []string{ctEnqueue, brief, tokens, reply, again}, restarts: []int{5}},
		"a compaction's boundary":                         {lines: []string{ctEnqueue, brief, reply, boundary, after}, restarts: []int{4}},
		"a boundary that opens a trimmed thread":          {lines: []string{ctEnqueue, boundary, after}, restarts: []int{2}},
		"the first root of a thread trimmed of its start": {lines: []string{ctEnqueue, tokens, reply, again}, restarts: []int{4}},
	} {
		got := ctRead(ctThread(tc.lines...), threadPlainMax)
		if len(got.lines) != len(tc.lines) {
			t.Fatalf("%s: %d of %d lines read", name, len(got.lines), len(tc.lines))
		}
		var seen threadRestarts
		var restarts []int
		for _, line := range got.lines {
			if seen.See(line.Head) {
				restarts = append(restarts, line.N)
			}
		}
		if !slices.Equal(restarts, tc.restarts) {
			t.Errorf("%s: restarts at lines %v, want %v", name, restarts, tc.restarts)
		}
	}
}

// ctID is one id a line handed out, and through which accessor.
type ctID struct {
	line int
	kind string
	id   string
}

// A HOOK'S FIRING IS NEVER A CALL'S ID: Hook reads each of the notes' hook
// records and a Stop's summary, the firing it records and its fields as
// written, and nothing else; a top-level toolUseID on any record but a summary
// names no firing; and a skill's PostToolUse hook, whose firing IS the id of
// the call it followed, hands that id out of Hook alone, never Calls. Over a
// whole thread, the firings come only from hook records, and the calls only
// from the model's messages and their results.
//
// Mutation: read a firing off every record that carries a top-level
// toolUseID, or hand a hook record's firing out as a call (one accessor of a
// line's ids), and a record's id comes out of the other accessor.
func TestAHooksFiringIsNeverACallsID(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		record string
		want   threadHook
	}{
		"a batch's success": {record: ctAttachment(ctUUID(5), ctUUID(6), ctBatchSuccess, ""), want: threadHook{
			Record: hookSuccess, Event: "PostToolBatch", Firing: ctBatchFire, Command: "crewlet/steer_take",
			Content: []byte(`""`), Stdout: []byte(ctAnswer)}},
		"a batch's system message": {record: ctAttachment(ctUUID(6), ctUUID(7), ctBatchSystem, ""), want: threadHook{
			Record: hookSystemMessage, Event: "PostToolBatch", Firing: ctBatchFire,
			Content: []byte(`"crewlet: a note reached the run"`)}},
		"a batch's additional context": {record: ctAttachment(ctUUID(7), ctUUID(8), ctBatchContext, ctBatchRendered),
			want: threadHook{Record: hookAdditionalContext, Event: "PostToolBatch", Firing: ctBatchFire,
				Content: []byte(`["## A note"]`)}},
		"a firing past its timeout": {record: ctAttachment(ctUUID(10), ctUUID(11), ctCancelled, ""), want: threadHook{
			Record: hookCancelled, Event: "PostToolBatch", Firing: ctLateFire, Command: "crewlet/steer_take"}},
		"a Stop's success": {record: ctAttachment(ctUUID(12), ctUUID(13), ctStopSuccess, ""), want: threadHook{
			Record: hookSuccess, Event: "Stop", Firing: ctStopFire, Command: "crewlet/steer_take",
			Content: []byte(`""`), Stdout: []byte(ctStopAnswer)}},
		"a Stop's system message": {record: ctAttachment(ctUUID(13), ctUUID(14), ctStopSystem, ""), want: threadHook{
			Record: hookSystemMessage, Event: "Stop", Firing: ctStopFire,
			Content: []byte(`"crewlet: a note reached the run as it was finishing"`)}},
		"a Stop's additional context": {record: ctAttachment(ctUUID(14), ctUUID(15), ctStopContext, ctStopRendered),
			want: threadHook{Record: hookAdditionalContext, Event: "Stop", Firing: ctStopFire,
				Content: []byte(`["## A note"]`)}},
		"a Stop's summary": {record: ctSummary(ctUUID(15), ctUUID(16)), want: threadHook{
			Record: hookStopSummary, Firing: ctStopFire}},
		"a skill's PostToolUse success": {record: ctSkillHook(ctUUID(4), ctUUID(5)), want: threadHook{
			Record: hookSuccess, Event: "PostToolUse", Firing: ctSkillCall, Command: "crewlet/skill_loaded",
			Content: []byte(`"PostToolUse:Skill completed"`), Stdout: []byte(`""`)}},
	} {
		line := ctLine(t, tc.record)
		got, isHook, err := line.Hook()
		if !isHook || err != nil || got.Record != tc.want.Record || got.Event != tc.want.Event ||
			got.Firing != tc.want.Firing || got.Command != tc.want.Command ||
			!bytes.Equal(got.Content, tc.want.Content) || !bytes.Equal(got.Stdout, tc.want.Stdout) {
			t.Errorf("%s reads as the hook %+v (%t, %v), want %+v", name, got, isHook, err, tc.want)
		}
		if m, err := line.Message(); err != nil || m.Calls() != nil {
			t.Errorf("%s hands out the calls %+v, %v", name, m.Calls(), err)
		}
	}

	result := ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)
	for name, record := range map[string]string{
		"a result carrying it": strings.Replace(result, `"toolUseResult":`, `"toolUseID":"`+ctCall+`","toolUseResult":`, 1),
		"a model's call carrying it": strings.Replace(ctAssistant(ctUUID(3), ctUUID(4), ctUseContent), `"apiBlockIndex"`,
			`"toolUseID":"`+ctCall+`","apiBlockIndex"`, 1),
		"an attachment of no hook carrying it": strings.Replace(
			ctAttachment(ctUUID(1), ctUUID(2), ctTokens, ctTokensRendered), `"type":"attachment"`,
			`"toolUseID":"`+ctCall+`","type":"attachment"`, 1),
		"a system record of another subtype": strings.Replace(ctSummary(ctUUID(15), ctUUID(16)),
			`"subtype":"stop_hook_summary"`, `"subtype":"informational"`, 1),
	} {
		if hook, isHook, err := ctLine(t, record).Hook(); isHook || err != nil {
			t.Errorf("%s reads as the hook %+v (%t, %v), want none", name, hook, isHook, err)
		}
	}

	lines := append(slices.Clone(ctMain), ctSkillHook(ctUUID(16), ctUUID(17)))
	got := ctRead(ctThread(lines...), threadPlainMax)
	if len(got.lines) != len(lines) {
		t.Fatalf("%d of %d lines read", len(got.lines), len(lines))
	}
	var ids []ctID
	for _, line := range got.lines {
		if hook, isHook, err := line.Hook(); err != nil {
			t.Fatalf("line %d reads no hook: %v", line.N, err)
		} else if isHook {
			ids = append(ids, ctID{line.N, "firing", hook.Firing})
		}
		m, err := line.Message()
		if err != nil {
			t.Fatalf("line %d reads no message: %v", line.N, err)
		}
		for _, c := range m.Calls() {
			ids = append(ids, ctID{line.N, c.Kind, c.ID})
		}
	}
	want := []ctID{
		{8, blockToolUse, ctCall}, {10, blockToolResult, ctCall},
		{11, "firing", ctBatchFire}, {12, "firing", ctBatchFire}, {13, "firing", ctBatchFire},
		{16, blockToolUse, ctMCPCall}, {17, blockToolResult, ctMCPCall}, {18, "firing", ctLateFire},
		{20, "firing", ctStopFire}, {21, "firing", ctStopFire}, {22, "firing", ctStopFire}, {23, "firing", ctStopFire},
		{26, "firing", ctSkillCall},
	}
	if !slices.Equal(ids, want) {
		t.Errorf("the thread hands out the ids %+v, want %+v", ids, want)
	}
}

// CALLS COME ONLY FROM A USER OR ASSISTANT MESSAGE: an assistant's tool_use
// blocks and a user's tool_result blocks, each with its id and what it
// carries raw (a call's input as written, a result's content, its is_error
// where present), and nothing from any other record, an api-request-blob
// repeating a result included. A message that does not decode is an error,
// never a message with half a call in it: one that is no object, a content
// absent, null or neither a string nor a list, a block that is no object or
// has no string type, a tool_use's id or name not a string, a tool_result's
// tool_use_id not a string or its is_error not a boolean.
//
// Mutation: read calls off every record's message, and the blob's result is a
// call; decode a block loosely, and a tool_use whose id is a number is a call
// with no id.
func TestCallsComeOnlyFromAUserOrAssistantMessage(t *testing.T) {
	t.Parallel()
	isFalse := false
	for name, tc := range map[string]struct {
		record string
		calls  []threadCall
	}{
		"a model's call": {record: ctAssistant(ctUUID(3), ctUUID(4), ctUseContent), calls: []threadCall{
			{Kind: blockToolUse, ID: ctCall, Name: "Bash", Input: []byte(ctUseInput)}}},
		"its result": {record: ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn), calls: []threadCall{
			{Kind: blockToolResult, ID: ctCall, IsError: &isFalse, Content: []byte(`"b1"`)}}},
		"an MCP tool's result, in blocks": {record: ctResult(ctUUID(9), ctUUID(10), ctMCPResult, ctMCPBlocks),
			calls: []threadCall{{Kind: blockToolResult, ID: ctMCPCall, Content: []byte(ctMCPBlocks)}}},
		"a model's text":                    {record: ctAssistant(ctUUID(2), ctUUID(3), `[{"type":"text","text":"batch 1."}]`)},
		"a brief":                           {record: ctBrief(ctUUID(1))},
		"a request blob repeating a result": {record: ctRequestBlob},
		"a hook's success":                  {record: ctAttachment(ctUUID(5), ctUUID(6), ctBatchSuccess, "")},
		"a Stop's summary":                  {record: ctSummary(ctUUID(15), ctUUID(16))},
	} {
		m, err := ctLine(t, tc.record).Message()
		if err != nil {
			t.Errorf("%s reads no message: %v", name, err)
			continue
		}
		calls := m.Calls()
		if len(calls) != len(tc.calls) {
			t.Errorf("%s hands out the calls %+v, want %+v", name, calls, tc.calls)
			continue
		}
		for i, c := range calls {
			want := tc.calls[i]
			if c.Kind != want.Kind || c.ID != want.ID || c.Name != want.Name || !bytes.Equal(c.Input, want.Input) ||
				!bytes.Equal(c.Content, want.Content) || (c.IsError == nil) != (want.IsError == nil) ||
				(c.IsError != nil && *c.IsError != *want.IsError) {
				t.Errorf("%s hands out the call %+v, want %+v", name, c, want)
			}
		}
	}

	user := func(message string) string {
		return `{"parentUuid":"` + ctUUID(4) + `","isSidechain":false,"promptId":"` + ctPrompt + `","type":"user",` +
			message + `"uuid":"` + ctUUID(5) + `","timestamp":"` + ctStamp + `",` + ctTail("2.1.310") + `}`
	}
	for name, record := range map[string]string{
		"a message that is no object":        user(`"message":"b1",`),
		"no message":                         user(""),
		"no content":                         user(`"message":{"role":"user"},`),
		"a null content":                     user(`"message":{"role":"user","content":null},`),
		"a numeric content":                  user(`"message":{"role":"user","content":7},`),
		"an object for content":              user(`"message":{"role":"user","content":{"type":"text"}},`),
		"a block that is no object":          user(`"message":{"role":"user","content":["b1"]},`),
		"a block of no type":                 user(`"message":{"role":"user","content":[{"text":"b1"}]},`),
		"a block whose type is a number":     user(`"message":{"role":"user","content":[{"type":7,"text":"b1"}]},`),
		"a tool_use whose id is a number":    ctAssistant(ctUUID(3), ctUUID(4), `[{"type":"tool_use","id":7,"name":"Bash","input":{}}]`),
		"a tool_use with no id":              ctAssistant(ctUUID(3), ctUUID(4), `[{"type":"tool_use","name":"Bash","input":{}}]`),
		"a tool_use whose name is null":      ctAssistant(ctUUID(3), ctUUID(4), `[{"type":"tool_use","id":"`+ctCall+`","name":null,"input":{}}]`),
		"a tool_result whose id is a number": user(`"message":{"role":"user","content":[{"tool_use_id":7,"type":"tool_result","content":"b1"}]},`),
		"a tool_result whose is_error is a string": user(`"message":{"role":"user","content":[{"tool_use_id":"` + ctCall +
			`","type":"tool_result","content":"b1","is_error":"false"}]},`),
		"a tool_result whose is_error is null": user(`"message":{"role":"user","content":[{"tool_use_id":"` + ctCall +
			`","type":"tool_result","content":"b1","is_error":null}]},`),
	} {
		if m, err := ctLine(t, record).Message(); err == nil {
			t.Errorf("%s reads as the message %+v, want an error", name, m)
		}
	}
}

// A MESSAGE SAYS ITS CONTENT'S SHAPE: content that is one string is Text, with
// no blocks; content that is a list is each block's type, in order, and no
// Text. Every user and assistant record measured holds one block a record;
// the API's own messages hold several, which a list of two holds to its order.
//
// Mutation: read content as one shape, and a string content is no message.
func TestAMessageSaysItsContentsShape(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		record string
		text   string
		kinds  []string
	}{
		"a brief":           {record: ctBrief(ctUUID(1)), text: "the brief\n"},
		"a notification":    {record: ctNotification(ctUUID(1), ctUUID(3)), text: ctNotificationText},
		"a model's text":    {record: ctAssistant(ctUUID(2), ctUUID(3), `[{"type":"text","text":"batch 1."}]`), kinds: []string{"text"}},
		"a call's result":   {record: ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn), kinds: []string{blockToolResult}},
		"a skill's text":    {record: ctSkillText(ctUUID(4), ctUUID(5)), kinds: []string{"text"}},
		"a text and a call": {record: ctAssistant(ctUUID(2), ctUUID(3), `[{"type":"text","text":"batch 1."},`+ctUseContent[1:]), kinds: []string{"text", blockToolUse}},
	} {
		m, err := ctLine(t, tc.record).Message()
		if err != nil {
			t.Errorf("%s reads no message: %v", name, err)
			continue
		}
		text, isText := m.Text()
		if isText != (tc.kinds == nil) || text != tc.text || !slices.Equal(m.Kinds(), tc.kinds) ||
			(tc.kinds == nil) != (m.Kinds() == nil) {
			t.Errorf("%s reads as the text %q (%t) and the blocks %q, want %q and %q", name, text, isText,
				strings.Join(m.Kinds(), " "), tc.text, strings.Join(tc.kinds, " "))
		}
	}
}

// MEMBERS ARE NAMED EXACTLY: the session's main thread, a subagent's thread
// and its metadata, each named from a session's id and an agent's, and read
// back from that name and no other. Another session's file, which a person's
// transcript takes for a main thread, a deeper path under the session's
// directory, a name the caller did not clean, `agent-.jsonl` and an agent's id
// outside the rule (1 to 64 bytes of letters, digits, `-` and `_`) name no
// member, and an id outside its rule names none in either direction: a
// session's id outside sandbox.ValidSessionID's shape names no member, nor
// reads back as the session of the names its own spelling would make
// (`projects/crewlet/a/b.jsonl` for `a/b`).
//
// Mutation: read a member's name through claudeThread, and another session's
// file reads as this session's main thread; drop the session's check from a
// subagent's names or from the way back, and `a/b` names a member.
func TestMembersAreNamedExactly(t *testing.T) {
	t.Parallel()
	dir := "projects/" + claudeProjectDir + "/" + ctSession
	longest := strings.Repeat("a", claudeAgentMaxBytes)
	for _, tc := range []struct {
		agent  string
		name   func(session, agent string) (string, bool)
		member threadMember
		want   string
	}{
		{name: func(session, _ string) (string, bool) { return claudeMainThread(session) },
			member: threadMember{Kind: memberMainThread}, want: dir + ".jsonl"},
		{agent: ctAgent, name: claudeSubagentThread, member: threadMember{Kind: memberSubagentThread, Agent: ctAgent},
			want: dir + "/subagents/agent-" + ctAgent + ".jsonl"},
		{agent: ctAgent, name: claudeSubagentMeta, member: threadMember{Kind: memberSubagentMeta, Agent: ctAgent},
			want: dir + "/subagents/agent-" + ctAgent + ".meta.json"},
		{agent: longest, name: claudeSubagentThread, member: threadMember{Kind: memberSubagentThread, Agent: longest},
			want: dir + "/subagents/agent-" + longest + ".jsonl"},
		{agent: "x_-9Z", name: claudeSubagentMeta, member: threadMember{Kind: memberSubagentMeta, Agent: "x_-9Z"},
			want: dir + "/subagents/agent-x_-9Z.meta.json"},
	} {
		name, ok := tc.name(ctSession, tc.agent)
		if !ok || name != tc.want {
			t.Errorf("%+v is named %q (%t), want %q", tc.member, name, ok, tc.want)
		}
		if member, ok := claudeMemberOf(ctSession, name); !ok || member != tc.member {
			t.Errorf("%q reads back as %+v (%t), want %+v", name, member, ok, tc.member)
		}
	}

	other := "projects/" + claudeProjectDir + "/" + ctOther + ".jsonl"
	if thread, ok := claudeThread(other); !ok || thread != "" {
		t.Fatalf("a person's transcript reads %q as the thread %q (%t); this case wants it read as a main thread",
			other, thread, ok)
	}
	for _, name := range []string{
		other,
		"projects/" + claudeProjectDir + "/" + ctOther + "/subagents/agent-" + ctAgent + ".jsonl",
		dir,
		dir + ".jsonl.bak",
		dir + ".json",
		dir + "/subagents/nested/agent-" + ctAgent + ".jsonl",
		dir + "/subagents/agent-" + ctAgent + "/subagents/agent-" + ctAgent + ".jsonl",
		dir + "/tool-results/" + ctCall + ".txt",
		dir + "/subagents/agent-.jsonl",
		dir + "/subagents/agent-.meta.json",
		dir + "/subagents/agent-a.b.jsonl",
		dir + "/subagents/agent-a b.meta.json",
		dir + "/subagents/agent-" + longest + "a.jsonl",
		dir + "/subagents/" + ctAgent + ".jsonl",
		"projects/" + claudeProjectDir + "/./" + ctSession + ".jsonl",
		"./" + dir + ".jsonl",
		"/" + dir + ".jsonl",
		"projects/elsewhere/" + ctSession + ".jsonl",
	} {
		if member, ok := claudeMemberOf(ctSession, name); ok {
			t.Errorf("%q reads as the member %+v", name, member)
		}
	}
	for _, session := range []string{"", "..", "a/b", "a.b", strings.Repeat("s", executorkeep.SessionIDMaxBytes+1)} {
		if name, ok := claudeMainThread(session); ok {
			t.Errorf("the session %q names its main thread %q", session, name)
		}
		if name, ok := claudeSubagentThread(session, ctAgent); ok {
			t.Errorf("the session %q names its subagent's thread %q", session, name)
		}
		if name, ok := claudeSubagentMeta(session, ctAgent); ok {
			t.Errorf("the session %q names its subagent's metadata %q", session, name)
		}
		own := "projects/" + claudeProjectDir + "/" + session
		for _, name := range []string{dir + ".jsonl", own + ".jsonl", own + "/subagents/agent-" + ctAgent + ".jsonl",
			own + "/subagents/agent-" + ctAgent + ".meta.json"} {
			if member, ok := claudeMemberOf(session, name); ok {
				t.Errorf("the session %q reads %q as the member %+v", session, name, member)
			}
		}
	}
	for _, agent := range []string{"", "a/b", "a.b", "a\x00b", "..", longest + "a"} {
		if name, ok := claudeSubagentThread(ctSession, agent); ok {
			t.Errorf("the agent %q names its thread %q", agent, name)
		}
		if name, ok := claudeSubagentMeta(ctSession, agent); ok {
			t.Errorf("the agent %q names its metadata %q", agent, name)
		}
	}
}
