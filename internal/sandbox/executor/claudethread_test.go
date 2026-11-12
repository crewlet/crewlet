package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"unsafe"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// THE RECORDS EVERY CASE BELOW READS ARE SHAPED AS CLAUDE CODE WRITES THEM, their
// keys in the order written and each of the type written, as measured on
// 2.1.310 and 2.1.322 driven as the engine drives a box's executor: a main
// thread's bookkeeping, its brief, its model's calls and their results, the
// notes' hook records at a batch and at Stop, a firing that timed out, a
// subagent's prompt, its coordinator's message and the main thread's
// notification of its end, a skill's text, and a compaction's boundary and the
// summary record after it; and, as 2.1.118 and 2.1.310 wrote it, a skill's
// PostToolUse hook. Some are ABRIDGED, a key no case reads left out, and each
// such fixture says what it leaves out. Every id in them is synthetic, and long
// text is shortened.

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
	// ctRequest is abridged: its params name the model and the token bound
	// alone, where a request's carry its whole parameters but for the system,
	// the tools and the messages.
	ctRequest = `{"type":"api-request","sessionId":"` + ctSession + `","id":"` + ctUUID(900) + `","timestamp":"` +
		ctStamp + `","version":"2.1.310","querySource":"sdk","shapeHash":"5a5a5a5a","params":{"model":` +
		`"claude-sonnet-4-5","max_tokens":32000},"base":"` + ctUUID(901) + `","keep":1,"tail":["b1b1b1b1"],` +
		`"cacheControl":[{"path":["system",0],"value":{"type":"ephemeral"}}],"requestDigest":"d1d1d1d1"}`
	ctLastPrompt = `{"type":"last-prompt","lastPrompt":"the brief","leafUuid":"` + ctUUID(5) + `","sessionId":"` +
		ctSession + `"}`
	// ctCostState is abridged: its modelUsage holds three of the counts a
	// model's carries.
	ctCostState = `{"type":"cost-state","sessionId":"` + ctSession + `","totalCostUSD":0.0036,"totalAPIDuration":118,` +
		`"totalAPIDurationWithoutRetries":110,"totalToolDuration":61,"totalLinesAdded":0,"totalLinesRemoved":0,` +
		`"totalDuration":21173,"startTime":1767323045000,"modelUsage":{"claude-sonnet-4-5":{"inputTokens":600,` +
		`"outputTokens":120,"costUSD":0.0036}},"hasUnknownModelCost":false}`
	// ctToolsUsed names the tools a session has used, written again whenever
	// that set grows: 2.1.322 writes it, 2.1.310 never does.
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

// ctAssistant is a model's message whose content is content, abridged: its
// usage holds the four counts alone, and it carries no stop_details, which
// 2.1.310 writes after its usage, null.
func ctAssistant(parent, uuid, content string) string {
	return `{"parentUuid":` + ctParent(parent) + `,"isSidechain":false,"message":{"id":"msg_test","type":"message",` +
		`"role":"assistant","model":"claude-sonnet-4-5","content":` + content + `,"stop_reason":"tool_use",` +
		`"stop_sequence":null,"usage":{"input_tokens":100,"cache_creation_input_tokens":0,` +
		`"cache_read_input_tokens":0,"output_tokens":20}},"apiBlockIndex":0,"requestId":"req_test","requestRef":"` +
		ctUUID(900) + `","requestedModel":"claude-sonnet-4-5","type":"assistant","uuid":"` + uuid +
		`","timestamp":"` + ctStamp + `","perTurnEffort":null,` + ctTail("2.1.310") + `}`
}

// ctResult is the user record answering a call: content its message's, result
// the record's own toolUseResult. Abridged: it carries no record-level
// sandboxed, which 2.1.310 writes after the toolUseResult of a shell command's
// result.
func ctResult(parent, uuid, content, result string) string {
	return `{"parentUuid":` + ctParent(parent) + `,"isSidechain":false,"promptId":"` + ctPrompt + `","type":"user",` +
		`"message":{"role":"user","content":` + content + `},"uuid":"` + uuid + `","timestamp":"` + ctStamp +
		`","toolUseResult":` + result + `,"sourceToolAssistantUUID":"` + parent + `","permissionDecision":` +
		`{"decision":"accept","source":"config","reasonType":"mode"},` + ctTail("2.1.310") + `}`
}

// ctInterrupted is the result 2.1.310's resume writes for a call its last
// process left with none: a failure, its content and its own toolUseResult
// the same string.
func ctInterrupted(parent, uuid string) string {
	const text = "[Tool call interrupted: the session ended before this call's result was recorded, so its " +
		"outcome is unknown. Check whether it took effect before relying on it or running it again.]"
	return `{"parentUuid":"` + parent + `","isSidechain":false,"promptId":"` + ctPrompt + `","type":"user","message":` +
		`{"role":"user","content":[{"type":"tool_result","content":"` + text + `","is_error":true,"tool_use_id":"` +
		ctCall + `"}]},"uuid":"` + uuid + `","timestamp":"` + ctStamp + `","toolUseResult":"` + text +
		`","toolDenialKind":"interrupted","sourceToolAssistantUUID":"` + parent + `",` + ctTail("2.1.310") + `}`
}

// ctKilled is the result 2.1.310 writes for a shell command killed by a signal:
// a failure, its content the exit code.
func ctKilled(parent, uuid string) string {
	return `{"parentUuid":"` + parent + `","isSidechain":false,"promptId":"` + ctPrompt + `","type":"user","message":` +
		`{"role":"user","content":[{"type":"tool_result","content":"Exit code 137","is_error":true,"tool_use_id":"` +
		ctCall + `"}]},"uuid":"` + uuid + `","timestamp":"` + ctStamp + `","toolUseResult":"Error: Exit code 137",` +
		`"sandboxed":false,"sourceToolAssistantUUID":"` + parent + `","permissionDecision":{"decision":"accept",` +
		`"source":"config","reasonType":"mode"},` + ctTail("2.1.310") + `}`
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

// ctMain is a main thread as 2.1.310 writes one, line by line, abridged: a
// batch whose hook took a note, an MCP call whose batch's hook timed out, and
// a Stop that took one, which 2.1.310 follows with the request and the reply
// that answer the Stop's note and a second Stop that takes none, left out
// here; and at its end, the tools-used record 2.1.322 writes beside the cost
// state, which 2.1.310 never writes.
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

// ctMeta is a subagent's metadata, as 2.1.310 writes it: one JSON document, the
// call that started the subagent named, and no line break after it.
const ctMeta = `{"agentType":"general-purpose","isBuiltIn":true,"description":"the subagent's task",` +
	`"toolUseId":"toolu_test_agent","spawnDepth":1,"requestShape":"foreground","requestNonInteractive":true,` +
	`"finished":true}`

// ctSkillHook is a skill's PostToolUse hook's success, as 2.1.118 records it,
// and 2.1.310 with the same keys: its firing is the id of the call it
// followed.
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

// ctSlug is the name 2.1.310 gives a session it compacted.
const ctSlug = "test-slug-words"

// ctBoundary is a compaction's boundary, as 2.1.310 writes one: it roots a
// chain of its own and names the last record before it as its logical parent,
// and its metadata names the records the compaction kept, kept, and the summary
// record that anchors them.
func ctBoundary(uuid, summary string, kept ...string) string {
	last, ids := kept[len(kept)-1], `"`+strings.Join(kept, `","`)+`"`
	return `{"parentUuid":null,"logicalParentUuid":"` + last + `","isSidechain":false,"type":"system",` +
		`"subtype":"compact_boundary","content":"Conversation compacted","isMeta":false,"timestamp":"` + ctStamp +
		`","uuid":"` + uuid + `","level":"info","compactMetadata":{"trigger":"manual","preTokens":120,` +
		`"durationMs":53,"preservedSegment":{"headUuid":"` + kept[0] + `","anchorUuid":"` + summary +
		`","tailUuid":"` + last + `"},"preservedMessages":{"anchorUuid":"` + summary + `","uuids":[` + ids +
		`],"allUuids":[` + ids + `]},"postTokens":2602,"cumulativeDroppedTokens":0},` + ctTail("2.1.310") +
		`,"slug":"` + ctSlug + `"}`
}

// ctCompactSummary is the record a compaction hands its summary on in, chained
// to its boundary, as 2.1.310 writes one.
func ctCompactSummary(boundary, uuid string) string {
	return `{"parentUuid":"` + boundary + `","isSidechain":false,"promptId":"` + ctPrompt + `","type":"user",` +
		`"message":{"role":"user","content":"This session is being continued from a previous conversation that ran ` +
		`out of context. The summary below covers the earlier portion of the conversation.\n\nSummary:\nthe summary"},` +
		`"isVisibleInTranscriptOnly":true,"isCompactSummary":true,"uuid":"` + uuid + `","timestamp":"` + ctStamp +
		`","turnPosition":{"promptIndex":0,"turnIndex":1},` + ctTail("2.1.310") + `,"slug":"` + ctSlug + `"}`
}

// ctMicrocompactBoundary is a microcompaction's boundary, chained to parent so
// that only its kind can make it a restart. Neither 2.1.310 nor 2.1.322 writes
// one: its type and subtype are what both releases' readers know it by, and
// its other keys are a compaction boundary's.
func ctMicrocompactBoundary(parent, uuid string) string {
	return `{"parentUuid":"` + parent + `","isSidechain":false,"type":"system","subtype":"microcompact_boundary",` +
		`"isMeta":false,"timestamp":"` + ctStamp + `","uuid":"` + uuid + `","level":"info",` + ctTail("2.1.322") + `}`
}

// ctSpan is a user record chained to parent whose replacesSpan is replaces, a
// JSON value, or that carries none where replaces is "". Claude Code's
// user-message factory takes the key, and no caller of it in 2.1.310 or
// 2.1.322 passes it: its other keys are a user record's as they write one.
func ctSpan(parent, uuid, replaces string) string {
	span := ""
	if replaces != "" {
		span = `,"replacesSpan":` + replaces
	}
	return `{"parentUuid":"` + parent + `","isSidechain":false,"promptId":"` + ctPrompt + `","type":"user",` +
		`"message":{"role":"user","content":"a summary of the span"}` + span + `,"uuid":"` + uuid + `","timestamp":"` +
		ctStamp + `",` + ctTail("2.1.322") + `}`
}

// ctSynthetic is the reply a resume writes for a turn it found unanswered, as
// 2.1.310 writes it: a model of its own, and a message whose report of the
// context edits applied is null.
func ctSynthetic(parent, uuid string) string {
	return `{"parentUuid":"` + parent + `","isSidechain":false,"type":"assistant","uuid":"` + uuid + `","timestamp":"` +
		ctStamp + `","message":{"diagnostics":null,"id":"` + ctUUID(902) + `","container":null,"model":"<synthetic>",` +
		`"role":"assistant","stop_details":null,"stop_reason":"stop_sequence","stop_sequence":"","type":"message",` +
		`"usage":{"output_tokens_details":null,"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,` +
		`"cache_read_input_tokens":0,"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},` +
		`"service_tier":null,"cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},` +
		`"inference_geo":null,"iterations":null,"speed":null,"fallback_credit":null},"content":[{"type":"text",` +
		`"text":"No response requested."}],"context_management":null},"isApiErrorMessage":false,` +
		ctTail("2.1.310") + `}`
}

// Two entries of the edits the server reports it applied to a request, as the
// API writes them: the clearing of old tool results, and that of a model's
// thinking.
const (
	ctClearing = `{"type":"clear_tool_uses_20250919","cleared_tool_uses":18,"cleared_input_tokens":41250}`
	ctThinking = `{"type":"clear_thinking_20251015","cleared_thinking_turns":2,"cleared_input_tokens":3000}`
)

// ctEdits is the report of edits a reply's message carries where Claude Code
// asked for the clearing: the response's own list, beside the counts it asked
// the server for.
func ctEdits(edits ...string) string {
	return `{"applied_edits":[` + strings.Join(edits, ",") + `],"client_sent_trigger_tool_uses":27,` +
		`"client_sent_keep_tool_uses":12}`
}

// ctReported is a model's reply whose message carries report as its report of
// the context edits applied, as 2.1.322 writes it. No run reached the clearing,
// which waits on a flag only Anthropic's servers set: where the report sits
// and what it holds are read from that release's code.
func ctReported(t *testing.T, report string) string {
	t.Helper()
	return ctReplace(t, ctAssistant(ctUUID(30), ctUUID(31), `[{"type":"text","text":"back."}]`),
		`"output_tokens":20}}`, `"output_tokens":20},"context_management":`+report+`}`)
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
// asked again, the reader answers it again. A subagent's metadata, one JSON
// document with no break after it, is no line at all, only a torn piece: no
// reader of a thread reads it through this one.
//
// Mutation: hand out the last piece with no break as a line, and the torn
// record, the padding, a piece a carriage return ends and a subagent's
// metadata each come back as a line, or a line that is no record, where the
// member's end belongs.
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
		"a subagent's metadata":                     {text: ctMeta, torn: true},
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
// reader's bound, ends it with errThreadBound, saying which of the two it ran
// past, and the reading stays over, the record after the line never read. A
// member past its bound is past it EVEN WHERE THE BOUND FALLS RIGHT AFTER A
// LINE BREAK, where a member metered by io.LimitReader would end cleanly and
// read as whole. A line or a member AT its bound reads whole, and the bounds
// are the take's scrub's own.
//
// Mutation: skip a line past the bound and read on, and the record after it
// is read; meter the member with io.LimitReader, and a member past its bound
// by a break, a record or a line ends cleanly; swap the two bounds' words, and
// a line past its bound reads as a member past its.
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
	// What the error of a reading past a bound says it ran past.
	const line, member = "a line runs past", "the member runs past"
	for name, tc := range map[string]struct {
		text     string
		maxBytes int64
		lines    int
		past     string
	}{
		"a line at its bound, its break included": {
			text: ctPadded(threadLineMax) + ctThread(brief), maxBytes: threadPlainMax, lines: 2},
		"a line a byte past its bound, a record after it": {
			text: ctThread(ctEnqueue) + ctPadded(threadLineMax+1) + ctThread(brief), maxBytes: threadPlainMax,
			lines: 1, past: line},
		"a member at its bound": {text: head, maxBytes: n, lines: 2},
		"a member a byte past its bound, which falls right after a break": {
			text: head + "\n", maxBytes: n, lines: 2, past: member},
		"a member past its bound by a record that starts on it": {
			text: head + ctThread(ctDequeue), maxBytes: n, lines: 2, past: member},
		"a member past its bound by a piece with no break": {text: head + "{", maxBytes: n, lines: 2, past: member},
		"a member whose bound falls inside a line":         {text: head, maxBytes: n - 1, lines: 1, past: member},
		"an empty member at a bound of none":               {text: "", maxBytes: 0},
		"a break at a bound of none":                       {text: "\n", maxBytes: 0, past: member},
		"a break at a bound below none":                    {text: "\n", maxBytes: -1, past: member},
		"an empty member at a bound below none":            {text: "", maxBytes: -1},
	} {
		got := ctRead(tc.text, tc.maxBytes)
		if len(got.lines) != tc.lines || len(got.broken) != 0 {
			t.Errorf("%s: %d lines read and %d broken, want %d and none", name, len(got.lines), len(got.broken), tc.lines)
		}
		switch {
		case tc.past != "" && (!errors.Is(got.end, errThreadBound) || !errors.Is(got.again, errThreadBound) ||
			!strings.Contains(got.end.Error(), tc.past)):
			t.Errorf("%s: the reading ended with %v and then %v, want errThreadBound both times, saying %q", name,
				got.end, got.again, tc.past)
		case tc.past == "" && (!errors.Is(got.end, io.EOF) || got.torn):
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
// boundary) would be neither read nor missed. A reader's error that wraps the
// take's scrub's bound, as a reader metering a kept archive with the scrub's
// own meter returns once the archive inflates past a reading, is that error
// too, in its own words, never the bound the layer reads within.
//
// Mutation: read a reader's error as the member's end, the half record before
// it its torn piece, and each reading ends with io.EOF; take a reader's error
// that wraps the scrub's bound for the layer's own, and the archive that
// inflated past its reading ends as a line past its bound.
func TestAMembersReaderThatFailsEndsTheReadingWithItsError(t *testing.T) {
	t.Parallel()
	reset := errors.New("read: connection reset by peer")
	inflated := fmt.Errorf("%w: a kept archive's plaintext runs past what one reading holds", errScrubBound)
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
		"an archive inflated past its reading part way through a record": {text: whole + call[:len(call)/2],
			err: inflated},
		"an archive inflated past its reading right after a break": {text: whole, err: inflated},
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
		if errors.Is(got.end, errThreadBound) || got.end == nil || got.end.Error() != tc.err.Error() {
			t.Errorf("%s: the reading ended with %v; want the reader's own error in its own words, never the "+
				"layer's bound", name, got.end)
		}
	}
}

// EVERY WHOLE LINE IS DECODED INTO ITS HEAD BEFORE ANYTHING READS IT: a line
// that is not one record (two values, one cut short, a value that is no
// object) or one whose head key is of the wrong type (`type`, `subtype` and
// `agentId` strings, `parentUuid` null or a string, `isSidechain` a boolean,
// and a user record's `replacesSpan` a boolean, as the brief's here) is a
// *threadLineError naming its line, whatever keywords it holds, and the reader
// reads on to the next line; and a whole record's head is what it says. None
// of these lines holds a key a reader of hooks or calls looks for.
//
// Mutation: decode only the lines that name a hook or a call, and every
// broken line here reads as a record, and the records' heads as nothing; read
// replacesSpan by its text alone, and one that is a string or null reads as a
// record.
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
		"a string replacesSpan":                  strings.Replace(brief, `"isSidechain":false`, `"isSidechain":false,"replacesSpan":"true"`, 1),
		"a null replacesSpan":                    strings.Replace(brief, `"isSidechain":false`, `"isSidechain":false,"replacesSpan":null`, 1),
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
// whole and is refused by OriginKind alone. A reply's stop_reason that is no
// string, null among them, leaves its message, its report of the edits
// applied, its usage and its hooks whole, and is refused by StopReason alone,
// never read as ""; its report of the edits applied, of the wrong shape,
// leaves its message, its stop_reason and its usage whole and is refused by
// ToolResultsCleared alone; and its usage of the wrong shape, or its id or
// model of the wrong type, leaves its message, its stop_reason, its report and
// its hooks whole and is refused by Usage alone. A record's own uuid that is
// no string (null, a number, an object, a boolean), on the harness's
// bookkeeping, on a model's call or on a hook's record, leaves everything else
// the record hands out as it reads with its uuid as written, and is refused by
// UUID alone. As written, each reads: an origin's kind, and a user record's
// own toolUseResult, raw, which no other record hands out.
//
// Mutation: hold the attachment to an object in the head, and the record whose
// attachment is not one is no record at all; hold the stop_reason, the report
// or the usage in Message, and a reply whose key is reshaped reads no message;
// read a null stop_reason as none, and it reads as a reply that did not stop
// for its calls; hold the uuid to its type in the head, and every record here
// whose uuid is reshaped is no record at all.
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

	call := ctAssistant(ctUUID(3), ctUUID(4), ctUseContent)
	for name, reason := range map[string]string{
		"null":      `null`,
		"a number":  `7`,
		"an object": `{"reason":"tool_use"}`,
	} {
		line := ctLine(t, ctReplace(t, call, `"stop_reason":"tool_use"`, `"stop_reason":`+reason))
		if got, err := line.StopReason(); err == nil {
			t.Errorf("a reply whose stop_reason is %s stopped for %q, want an error", name, got)
		}
		if m, err := line.Message(); err != nil || len(m.Calls()) != 1 || m.Calls()[0].ID != ctCall {
			t.Errorf("a reply whose stop_reason is %s reads the calls %+v, %v; want its call", name, m.Calls(), err)
		}
		if cleared, err := line.ToolResultsCleared(); cleared || err != nil {
			t.Errorf("a reply whose stop_reason is %s reads as cleared %t, %v; want no clearing", name, cleared, err)
		}
		if _, isHook, err := line.Hook(); isHook || err != nil {
			t.Errorf("a reply whose stop_reason is %s reads as a hook's record %t, %v", name, isHook, err)
		}
		if usage, ok, err := line.Usage(); !ok || err != nil || usage.ID != "msg_test" || usage.Input != 100 {
			t.Errorf("a reply whose stop_reason is %s says its request took %+v (%t), %v; want its usage", name,
				usage, ok, err)
		}
	}
	for name, report := range map[string]string{
		"a string": `"cleared"`,
		"a list":   `[]`,
		"a number": `7`,
	} {
		line := ctLine(t, ctReplace(t, call, `"output_tokens":20}}`, `"output_tokens":20},"context_management":`+report+`}`))
		if cleared, err := line.ToolResultsCleared(); err == nil {
			t.Errorf("a reply whose report of the edits applied is %s reads as cleared %t, want an error", name, cleared)
		}
		if m, err := line.Message(); err != nil || len(m.Calls()) != 1 || m.Calls()[0].ID != ctCall {
			t.Errorf("a reply whose report is %s reads the calls %+v, %v; want its call", name, m.Calls(), err)
		}
		if reason, err := line.StopReason(); reason != "tool_use" || err != nil {
			t.Errorf("a reply whose report is %s stopped for %q, %v; want tool_use", name, reason, err)
		}
		if usage, ok, err := line.Usage(); !ok || err != nil || usage.ID != "msg_test" || usage.Input != 100 {
			t.Errorf("a reply whose report is %s says its request took %+v (%t), %v; want its usage", name, usage,
				ok, err)
		}
	}
	for name, usage := range map[string]string{
		"null":                    `null`,
		"a list":                  `[100,0,0,20]`,
		"of a count that is null": `{"input_tokens":null,"output_tokens":20}`,
		"of a negative count":     `{"input_tokens":-100,"output_tokens":20}`,
	} {
		line := ctLine(t, ctReplace(t, call, ctUsage, `"usage":`+usage))
		if got, ok, err := line.Usage(); err == nil {
			t.Errorf("a reply whose usage is %s says its request took %+v (%t), want an error", name, got, ok)
		}
		if m, err := line.Message(); err != nil || len(m.Calls()) != 1 || m.Calls()[0].ID != ctCall {
			t.Errorf("a reply whose usage is %s reads the calls %+v, %v; want its call", name, m.Calls(), err)
		}
		if reason, err := line.StopReason(); reason != "tool_use" || err != nil {
			t.Errorf("a reply whose usage is %s stopped for %q, %v; want tool_use", name, reason, err)
		}
		if cleared, err := line.ToolResultsCleared(); cleared || err != nil {
			t.Errorf("a reply whose usage is %s reads as cleared %t, %v; want no clearing", name, cleared, err)
		}
		if _, isHook, err := line.Hook(); isHook || err != nil {
			t.Errorf("a reply whose usage is %s reads as a hook's record %t, %v", name, isHook, err)
		}
	}
	for name, tc := range map[string]struct{ from, to string }{
		"an id that is a number": {from: `"id":"msg_test"`, to: `"id":7`},
		"a model that is null":   {from: `"model":"claude-sonnet-4-5"`, to: `"model":null`},
	} {
		line := ctLine(t, ctReplace(t, call, tc.from, tc.to))
		if got, ok, err := line.Usage(); err == nil {
			t.Errorf("a reply whose message holds %s says its request took %+v (%t), want an error", name, got, ok)
		}
		if m, err := line.Message(); err != nil || len(m.Calls()) != 1 || m.Calls()[0].ID != ctCall {
			t.Errorf("a reply whose message holds %s reads the calls %+v, %v; want its call", name, m.Calls(), err)
		}
		if reason, err := line.StopReason(); reason != "tool_use" || err != nil {
			t.Errorf("a reply whose message holds %s stopped for %q, %v; want tool_use", name, reason, err)
		}
	}

	queued := ctReplace(t, ctDequeue, `"type":"queue-operation"`, `"type":"queue-operation","uuid":"`+ctUUID(8)+`"`)
	for record, tc := range map[string]struct{ line, uuid string }{
		"the harness's bookkeeping": {line: queued, uuid: ctUUID(8)},
		"a model's call":            {line: call, uuid: ctUUID(4)},
		"a hook's record":           {line: ctAttachment(ctUUID(6), ctUUID(7), ctBatchSystem, ""), uuid: ctUUID(7)},
	} {
		want := ctAllButUUID(ctLine(t, tc.line))
		for name, reshaped := range map[string]string{
			"null":      `null`,
			"a number":  `7`,
			"an object": `{"uuid":"` + tc.uuid + `"}`,
			"a boolean": `true`,
		} {
			line := ctLine(t, ctReplace(t, tc.line, `"uuid":"`+tc.uuid+`"`, `"uuid":`+reshaped))
			if got, err := line.UUID(); err == nil {
				t.Errorf("%s whose uuid is %s reads its uuid as %q, want an error", record, name, got)
			}
			if got := ctAllButUUID(line); got != want {
				t.Errorf("%s whose uuid is %s reads as\n%swant it read as with its uuid as written\n%s", record, name,
					got, want)
			}
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
// head, its own uuid, its hook, its message and its calls, its origin's kind,
// its own toolUseResult, whether it says tool results were cleared, why its
// reply stopped and what its request took, each beside the error it answered.
// A raw value it hands out is printed by [ctRaw], so an absent one and a null
// one never read alike.
func ctAll(l threadLine) string {
	uuid, err := l.UUID()
	return fmt.Sprintf("uuid: %q; %v\n", uuid, err) + ctAllButUUID(l)
}

// ctAllButUUID is everything [ctAll] prints but the record's own uuid.
func ctAllButUUID(l threadLine) string {
	var b strings.Builder
	h := l.Head
	side := "absent"
	if h.Sidechain != nil {
		side = strconv.FormatBool(*h.Sidechain)
	}
	fmt.Fprintf(&b, "head: type %q, subtype %q, parent %s, sidechain %s, agent %q, replaces a span %t\n",
		h.Type, h.Subtype, ctRaw(h.Parent), side, h.AgentID, h.ReplacesSpan)
	hook, isHook, err := l.Hook()
	fmt.Fprintf(&b, "hook %t: record %q, event %q, firing %q, command %q, content %s, stdout %s; %v\n", isHook,
		hook.Record, hook.Event, hook.Firing, hook.Command, ctRaw(hook.Content), ctRaw(hook.Stdout), err)
	m, err := l.Message()
	text, isText := m.Text()
	fmt.Fprintf(&b, "message: text %q (%t), kinds %q; %v\n", text, isText, strings.Join(m.Kinds(), " "), err)
	for _, c := range m.Calls() {
		failed := "absent"
		if c.IsError != nil {
			failed = strconv.FormatBool(*c.IsError)
		}
		fmt.Fprintf(&b, "call %q: id %q, name %q, input %s, is_error %s, content %s\n", c.Kind, c.ID, c.Name,
			ctRaw(c.Input), failed, ctRaw(c.Content))
	}
	kind, err := l.OriginKind()
	fmt.Fprintf(&b, "origin: kind %q; %v\n", kind, err)
	fmt.Fprintf(&b, "toolUseResult: %s\n", ctRaw(l.ToolUseResult()))
	cleared, err := l.ToolResultsCleared()
	fmt.Fprintf(&b, "tool results cleared: %t; %v\n", cleared, err)
	reason, err := l.StopReason()
	fmt.Fprintf(&b, "stop reason: %q; %v\n", reason, err)
	usage, used, err := l.Usage()
	fmt.Fprintf(&b, "usage %t: id %q, model %q, input %d, cache writes %d, cache reads %d, output %d; %v\n", used,
		usage.ID, usage.Model, usage.Input, usage.CacheWrite, usage.CacheRead, usage.Output, err)
	return b.String()
}

// ctRaw is a raw value a line hands out, as text: "absent" where it is nil,
// else its bytes. Never %s alone, which prints a nil json.RawMessage as null:
// an absent parentUuid would read as a root's, and an absent toolUseResult as
// a null one.
func ctRaw(v json.RawMessage) string {
	if v == nil {
		return "absent"
	}
	return string(v)
}

// A KEY IS READ EXACTLY AS WRITTEN, as Claude Code reads its own records with
// JSON.parse: a key that differs from one the layer reads only in case is
// another key, after that one or before it, and alone it leaves that one
// absent; and a key written twice holds its last value. Every object the layer
// reads is held so: a line's head and the keys only one reader reads (its own
// uuid, a message, an attachment, an origin, a firing at the top, a record's
// own toolUseResult), a hook's attachment, an origin, a message (its content,
// its stop_reason, its id, its model and its usage), a message's blocks, its
// usage's counts and iterations, each iteration's type and counts, its report
// of the context edits applied, and each edit the report lists. So a line
// hands out exactly what it hands out without the other key, through every
// accessor. 2.1.322 already spells a call's id `toolUseId` (a subagent's
// metadata names the call that started it so), a key away from a hook's
// firing.
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
	cleared := ctReported(t, ctEdits(ctClearing))
	// listing is the model's call whose usage lists entries as its iterations.
	listing := func(entries ...string) string {
		return ctReplace(t, call, `"output_tokens":20}`, `"output_tokens":20,"iterations":[`+
			strings.Join(entries, ",")+`]}`)
	}
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
		"ParentUuid null alone": {record: ctDequeue, from: `"type":"queue-operation"`,
			to: `"type":"queue-operation","ParentUuid":null`},
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
		"a replacesSpan, then ReplacesSpan": {record: ctSpan(ctUUID(3), ctUUID(4), "false"),
			from: `"replacesSpan":false`, to: `"replacesSpan":false,"ReplacesSpan":true`},
		"ReplacesSpan alone": {record: brief, from: `"isSidechain":false`, to: `"isSidechain":false,"ReplacesSpan":true`},
		"a uuid, then UUID": {record: brief, from: `"uuid":"` + ctUUID(1) + `"`,
			to: `"uuid":"` + ctUUID(1) + `","UUID":"` + ctUUID(9) + `"`},
		"a Uuid, then uuid": {record: brief, from: `"uuid":"` + ctUUID(1) + `"`,
			to: `"Uuid":"` + ctUUID(9) + `","uuid":"` + ctUUID(1) + `"`},
		"Uuid alone": {record: ctDequeue, from: `"type":"queue-operation"`,
			to: `"type":"queue-operation","Uuid":"` + ctUUID(9) + `"`},
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
		"ToolUseResult null alone": {record: brief, from: `"isSidechain":false`,
			to: `"isSidechain":false,"ToolUseResult":null`},
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
		"a reply's stop_reason, then Stop_Reason": {record: call, from: `"stop_reason":"tool_use"`,
			to: `"stop_reason":"tool_use","Stop_Reason":"end_turn"`},
		"a reply's Stop_Reason alone": {record: ctReplace(t, call, `"stop_reason":"tool_use",`, ""),
			from: `"stop_sequence":null`, to: `"stop_sequence":null,"Stop_Reason":"end_turn"`},
		"a reply's id, then Id": {record: call, from: `"id":"msg_test"`, to: `"id":"msg_test","Id":"msg_other"`},
		"a reply's ID alone": {record: ctReplace(t, call, `"id":"msg_test",`, ""), from: `"type":"message"`,
			to: `"ID":"msg_other","type":"message"`},
		"a reply's model, then Model": {record: call, from: `"model":"claude-sonnet-4-5"`,
			to: `"model":"claude-sonnet-4-5","Model":"<synthetic>"`},
		"a reply's usage, then Usage": {record: call, from: ctUsage, to: ctUsage + `,"Usage":{"input_tokens":7}`},
		"a reply's Usage alone": {record: ctReplace(t, call, ","+ctUsage, ""), from: `"stop_sequence":null`,
			to: `"stop_sequence":null,"Usage":{"input_tokens":7}`},
		// A reply's usage.
		"an input_tokens, then Input_Tokens": {record: call, from: `"input_tokens":100`,
			to: `"input_tokens":100,"Input_Tokens":7`},
		"a Cache_Creation_Input_Tokens, then cache_creation_input_tokens": {record: call,
			from: `"cache_creation_input_tokens":0`, to: `"Cache_Creation_Input_Tokens":7,"cache_creation_input_tokens":0`},
		"Cache_Read_Input_Tokens alone": {record: ctReplace(t, call, `"cache_read_input_tokens":0,`, ""),
			from: `"output_tokens":20`, to: `"Cache_Read_Input_Tokens":7,"output_tokens":20`},
		"an output_tokens, then OUTPUT_TOKENS": {record: call, from: `"output_tokens":20`,
			to: `"output_tokens":20,"OUTPUT_TOKENS":7`},
		"an iterations, then Iterations": {record: listing(ctIteration("message", 100)),
			from: `"iterations":[` + ctIteration("message", 100) + `]`,
			to: `"iterations":[` + ctIteration("message", 100) + `],"Iterations":[` +
				ctIteration("message", 7) + `]`},
		"Iterations alone": {record: call, from: `"output_tokens":20`,
			to: `"output_tokens":20,"Iterations":[` + ctIteration("message", 7) + `]`},
		// A usage's iterations.
		"an iteration's type, then Type": {record: listing(ctIteration("message", 7)),
			from: `"type":"message","input_tokens":7`, to: `"type":"message","Type":"user","input_tokens":7`},
		"a passed-over iteration's type, then Type": {
			record: listing(ctIteration("message", 7), ctIteration("advisor_message", 9)),
			from:   `"type":"advisor_message"`, to: `"type":"advisor_message","Type":"message"`},
		"an iteration's input_tokens, then Input_Tokens": {record: listing(ctIteration("message", 100)),
			from: `"type":"message","input_tokens":100`, to: `"type":"message","input_tokens":100,"Input_Tokens":7`},
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
		// A message's report of the context edits applied, and its edits.
		"a context_management, then Context_Management": {record: cleared,
			from: `"context_management":` + ctEdits(ctClearing),
			to:   `"context_management":` + ctEdits(ctClearing) + `,"Context_Management":null`},
		"an applied_edits, then Applied_Edits": {record: cleared, from: `"applied_edits":[` + ctClearing + `]`,
			to: `"applied_edits":[` + ctClearing + `],"Applied_Edits":[]`},
		"an edit's type, then Type": {record: cleared, from: `"type":"clear_tool_uses_20250919"`,
			to: `"type":"clear_tool_uses_20250919","Type":"clear_thinking_20251015"`},
		"an edit's cleared_tool_uses, then Cleared_Tool_Uses": {record: cleared, from: `"cleared_tool_uses":18`,
			to: `"cleared_tool_uses":18,"Cleared_Tool_Uses":0`},
		// A key written twice.
		"a type written twice": {record: retyped, from: `"type":"attachment"`, to: `"type":"user","type":"attachment"`},
		"a uuid written twice": {record: brief, from: `"uuid":"` + ctUUID(1) + `"`,
			to: `"uuid":"` + ctUUID(9) + `","uuid":"` + ctUUID(1) + `"`},
		"a reply's usage written twice": {record: call, from: ctUsage,
			to: `"usage":{"input_tokens":7,"output_tokens":7},` + ctUsage},
		"a count written twice": {record: call, from: `"output_tokens":20`, to: `"output_tokens":7,"output_tokens":20`},
		"an attachment's firing written twice": {record: hook, from: firing,
			to: `"toolUseID":"` + other + `",` + firing},
	} {
		variant := ctReplace(t, tc.record, tc.from, tc.to)
		if got, want := ctAll(ctLine(t, variant)), ctAll(ctLine(t, tc.record)); got != want {
			t.Errorf("%s reads as\n%swant\n%s", name, got, want)
		}
	}
}

// A STRING CLAUDE CODE WROTE BROKEN READS AS THE REPLACEMENT CHARACTER, never
// as no record: a surrogate with no pair, which JSON.stringify writes as a
// `\udXXX` escape where a text was cut inside a pair (an emoji at a truncation
// point), and a byte that is no UTF-8 each read as U+FFFD, as encoding/json's
// own decode reads them, wherever a line holds one: in a text the layer reads
// (a brief's), in a value it hands out raw (a result's content, as written,
// which its own decode then reads the same way), in a hook's record, and in a
// key no reader reads. Each line reads whole. Refused, one such line would
// cost a reader its whole reading of a thread its harness reads whole.
//
// Mutation: drop the option that reads them, and every line here is no
// record.
func TestAStringClaudeCodeWroteBrokenReadsAsTheReplacementCharacter(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		written string // the string as its line holds it
		text    string // what it reads as
	}{
		"a high surrogate a cut left alone": {written: `"an emoji cut at \ud83d"`, text: "an emoji cut at �"},
		"a low surrogate alone":             {written: `"\ude00 is half an emoji"`, text: "� is half an emoji"},
		"a byte that is no UTF-8":           {written: "\"a byte \xff here\"", text: "a byte � here"},
	} {
		var v1 string
		if err := json.Unmarshal([]byte(tc.written), &v1); err != nil || v1 != tc.text {
			t.Fatalf("%s: encoding/json reads %q as %q, %v; this case wants it read as %q", name, tc.written, v1,
				err, tc.text)
		}
		read := func(record string) (threadLine, bool) {
			t.Helper()
			got := ctRead(ctThread(record), threadPlainMax)
			if len(got.lines) != 1 || len(got.broken) != 0 || !errors.Is(got.end, io.EOF) {
				var broken error
				if len(got.broken) > 0 {
					broken = got.broken[0]
				}
				t.Errorf("%s: a line holding %q reads as %d records, broken by %v; want one record", name,
					tc.written, len(got.lines), broken)
				return threadLine{}, false
			}
			return got.lines[0], true
		}

		if brief, ok := read(ctReplace(t, ctBrief(ctUUID(1)), `"content":"the brief\n"`, `"content":`+tc.written)); ok {
			if m, err := brief.Message(); err != nil {
				t.Errorf("%s: a brief holding it reads no message: %v", name, err)
			} else if text, isText := m.Text(); !isText || text != tc.text {
				t.Errorf("%s: a brief holding it reads as %q (%t), want %q", name, text, isText, tc.text)
			}
		}
		if result, ok := read(ctResult(ctUUID(4), ctUUID(5), ctReplace(t, ctResultBody, `"content":"b1"`,
			`"content":`+tc.written), ctResultOwn)); ok {
			m, err := result.Message()
			var text string
			switch {
			case err != nil || len(m.Calls()) != 1:
				t.Errorf("%s: a result holding it hands out the calls %+v, %v", name, m.Calls(), err)
			case string(m.Calls()[0].Content) != tc.written:
				t.Errorf("%s: a result holding it hands its content out as %q, want it as written", name,
					m.Calls()[0].Content)
			case wireDecode(m.Calls()[0].Content, &text) != nil || text != tc.text:
				t.Errorf("%s: a result's content holding it reads as %q, want %q", name, text, tc.text)
			}
		}
		if note, ok := read(ctAttachment(ctUUID(6), ctUUID(7), ctReplace(t, ctBatchSystem,
			`"content":"crewlet: a note reached the run"`, `"content":`+tc.written), "")); ok {
			if hook, isHook, err := note.Hook(); !isHook || err != nil || hook.Firing != ctBatchFire ||
				string(hook.Content) != tc.written {
				t.Errorf("%s: a hook's record holding it reads as the hook %+v (%t, %v)", name, hook, isHook, err)
			}
		}
		if output, ok := read(ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctReplace(t, ctResultOwn, `"stdout":"b1"`,
			`"stdout":`+tc.written))); ok {
			if m, err := output.Message(); err != nil || len(m.Calls()) != 1 || m.Calls()[0].ID != ctCall ||
				!bytes.Contains(output.ToolUseResult(), []byte(tc.written)) {
				t.Errorf("%s: a result whose own output holds it hands out the calls %+v, %v, and the output %q", name,
					m.Calls(), err, output.ToolUseResult())
			}
		}
	}
}

// ABSENT IS NOT NULL: a record outside the chain carries no parentUuid and is
// neither chained nor a root, the brief's null roots the chain, and a chained
// record's parent is its quoted uuid, each handed over as written. Every kind
// of the harness's bookkeeping measured is outside the chain, the nine of
// them: queue-operation, atis-latch, api-request-shape, api-request-blob,
// api-request, last-prompt, cost-state, mode, and tools-used, which 2.1.322
// writes whenever the tools a session has used grow, and 2.1.310 never does.
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

// A RECORD'S OWN UUID IS HANDED OVER AS WRITTEN, by its accessor: a chained
// record's uuid as it writes it, a compaction's boundary's among them, the one
// record its reader reads it on; "" on every record outside the chain, the
// harness's bookkeeping of every kind, which carries none, though a request
// names an id of its own and a last prompt the uuid of a record; and a uuid of
// another length, the empty string, a short one and one far past a uuid's 36
// bytes, each as written, never refused or cut, since the layer holds a uuid
// to a string alone and its reader holds it to a rule of its own.
//
// Mutation: hand out no uuid, and every record's reads as ""; require one, and
// every record of the bookkeeping is an error; hold it to a uuid's 36 bytes,
// and each uuid of another length is an error.
func TestARecordsOwnUUIDIsHandedOverAsWritten(t *testing.T) {
	t.Parallel()
	boundary := ctBoundary(ctUUID(5), ctUUID(6), ctUUID(3))
	uuid := `"uuid":"` + ctUUID(5) + `"`
	long := strings.Repeat("b", 4096)
	for name, tc := range map[string]struct {
		record, uuid string
	}{
		"the brief":                      {record: ctBrief(ctUUID(1)), uuid: ctUUID(1)},
		"a model's call":                 {record: ctAssistant(ctUUID(3), ctUUID(4), ctUseContent), uuid: ctUUID(4)},
		"a call's result":                {record: ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn), uuid: ctUUID(5)},
		"a hook's record":                {record: ctAttachment(ctUUID(5), ctUUID(6), ctBatchSuccess, ""), uuid: ctUUID(6)},
		"a Stop's summary":               {record: ctSummary(ctUUID(15), ctUUID(16)), uuid: ctUUID(16)},
		"a subagent's prompt":            {record: ctSubagentPrompt(ctUUID(3)), uuid: ctUUID(3)},
		"a compaction's boundary":        {record: boundary, uuid: ctUUID(5)},
		"a boundary whose uuid is empty": {record: ctReplace(t, boundary, uuid, `"uuid":""`)},
		"a boundary whose uuid is short": {record: ctReplace(t, boundary, uuid, `"uuid":"b0"`), uuid: "b0"},
		"a boundary whose uuid is long":  {record: ctReplace(t, boundary, uuid, `"uuid":"`+long+`"`), uuid: long},
	} {
		if uuid, err := ctLine(t, tc.record).UUID(); uuid != tc.uuid || err != nil {
			t.Errorf("%s reads its uuid as %.60q, %v; want %.60q", name, uuid, err, tc.uuid)
		}
	}
	bookkeeping := []string{ctEnqueue, ctDequeue, ctAtisLatch, ctRequestShape, ctRequestBlob, ctRequest, ctLastPrompt,
		ctCostState, ctToolsUsed, ctMode}
	got := ctRead(ctThread(bookkeeping...), threadPlainMax)
	if len(got.lines) != len(bookkeeping) || len(got.broken) != 0 {
		t.Fatalf("%d lines of bookkeeping read and %d broken, want %d and none", len(got.lines), len(got.broken),
			len(bookkeeping))
	}
	for _, line := range got.lines {
		if uuid, err := line.UUID(); uuid != "" || err != nil {
			t.Errorf("line %d, a %s, carries no uuid and reads one as %q, %v", line.N, line.Head.Type, uuid, err)
		}
	}
}

// A RESTART IS WHERE WHAT CAME BEFORE MAY BE GONE from what a resume holds: a
// compaction's boundary, wherever it falls; a root after the thread's first
// chained record; and a first chained record that roots no chain, the start of
// a thread whose head is gone. A trim after a compaction leaves exactly that, as
// measured: the reply it kept, chained to a record the trim took, before the
// boundary. The thread's first chained record that roots its chain is no
// restart, however much bookkeeping comes before it, so a thread as measured
// restarts nowhere, and a thread with no chained record at all restarts
// nowhere either: it holds no chain to start over.
//
// So are the two records 2.1.310's and 2.1.322's own readers stop at as at a
// compaction, though neither release writes one: a system record of subtype
// microcompact_boundary and a user record whose replacesSpan is true, each a
// restart where it is chained to the record before it, roots nothing and
// follows the brief. A replacesSpan that is false or absent replaces nothing,
// and each kind is read on its own record's type alone, as the harness reads
// it: the subtype on an attachment, and replacesSpan true on a model's reply,
// restart nothing. Nor is a replacesSpan on a record that is no user's held to
// its type: a model's reply whose replacesSpan is a string, and an attachment
// whose replacesSpan is null, each read whole, and restart nothing.
//
// Mutation: count every root, and the brief restarts every thread; count every
// root but the first one seen, and the first root of a thread whose start is
// gone is none; read a thread whose start is gone as whole, and one that never
// roots again restarts nowhere; take the first record for the first chained
// one, and the bookkeeping before a brief restarts the thread; drop the
// boundary, and one that opens a thread is none; drop the microcompaction's
// boundary or the replaced span, and its record restarts nothing; read
// replacesSpan by its presence, and one that is false restarts the thread;
// read either kind off any record's type, and the attachment or the reply
// restarts it; hold replacesSpan to its type on every record, and the reply
// and the attachment that carry one of another type are no records.
func TestARestartIsWhereWhatCameBeforeMayBeGone(t *testing.T) {
	t.Parallel()
	brief, again := ctBrief(ctUUID(1)), ctBrief(ctUUID(4))
	tokens := ctAttachment(ctUUID(1), ctUUID(2), ctTokens, ctTokensRendered)
	reply := ctAssistant(ctUUID(2), ctUUID(3), `[{"type":"text","text":"batch 1."}]`)
	// The thread as measured up to its first reply: what each record below
	// follows, chained to that reply.
	head := []string{ctEnqueue, brief, tokens, reply}
	after := func(record string) []string { return append(slices.Clone(head), record) }
	boundary, summary := ctBoundary(ctUUID(5), ctUUID(6), ctUUID(3)), ctCompactSummary(ctUUID(5), ctUUID(6))
	// The file a trim after a compaction leaves, as 2.1.310 and 2.1.322 wrote it:
	// the reply the compaction kept, chained to ctUUID(30), which the trim took;
	// that reply's Stop firing and its summary; the boundary; the bookkeeping;
	// and the summary record.
	trimmed := []string{
		ctAssistant(ctUUID(30), ctUUID(31), `[{"type":"text","text":"done."}]`),
		ctAttachment(ctUUID(31), ctUUID(32), ctStopSuccess, ""),
		ctSummary(ctUUID(32), ctUUID(33)),
		ctBoundary(ctUUID(34), ctUUID(35), ctUUID(31), ctUUID(32), ctUUID(33)),
		ctEnqueue, ctAtisLatch, ctCostState, ctMode,
		ctCompactSummary(ctUUID(34), ctUUID(35)),
	}
	for name, tc := range map[string]struct {
		lines    []string
		restarts []int
	}{
		"a thread as measured": {lines: ctMain},
		"a root after the first chained record": {lines: []string{ctEnqueue, brief, tokens, reply, again},
			restarts: []int{5}},
		"a compaction's boundary, as measured": {lines: []string{ctEnqueue, brief, tokens, reply, ctMode, ctAtisLatch,
			ctEnqueue, ctDequeue, boundary, summary}, restarts: []int{9}},
		"a file a trim left, as measured": {lines: trimmed, restarts: []int{1, 4}},
		"a boundary that opens a thread":  {lines: []string{ctEnqueue, boundary, summary}, restarts: []int{2}},
		"a thread whose start is gone, and a root after it": {lines: []string{ctEnqueue, tokens, reply, again},
			restarts: []int{2, 4}},
		"a thread whose start is gone, that never roots again": {lines: []string{ctEnqueue, tokens, reply},
			restarts: []int{2}},
		"a thread with no chained record": {lines: []string{ctEnqueue, ctDequeue, ctAtisLatch, ctCostState}},
		"a microcompaction's boundary": {lines: after(ctMicrocompactBoundary(ctUUID(3), ctUUID(7))),
			restarts: []int{5}},
		"a user record that replaces a span":        {lines: after(ctSpan(ctUUID(3), ctUUID(7), "true")), restarts: []int{5}},
		"a user record whose replacesSpan is false": {lines: after(ctSpan(ctUUID(3), ctUUID(7), "false"))},
		"a user record with no replacesSpan":        {lines: after(ctSpan(ctUUID(3), ctUUID(7), ""))},
		"a microcompaction's subtype on an attachment": {lines: after(ctReplace(t,
			ctAttachment(ctUUID(3), ctUUID(7), ctTokens, ctTokensRendered), `"type":"attachment"`,
			`"type":"attachment","subtype":"microcompact_boundary"`))},
		"replacesSpan true on a model's reply": {lines: after(ctReplace(t,
			ctAssistant(ctUUID(3), ctUUID(7), `[{"type":"text","text":"batch 2."}]`), `"type":"assistant"`,
			`"type":"assistant","replacesSpan":true`))},
		"a replacesSpan that is a string on a model's reply": {lines: after(ctReplace(t,
			ctAssistant(ctUUID(3), ctUUID(7), `[{"type":"text","text":"batch 2."}]`), `"type":"assistant"`,
			`"type":"assistant","replacesSpan":"true"`))},
		"a null replacesSpan on an attachment": {lines: after(ctReplace(t,
			ctAttachment(ctUUID(3), ctUUID(7), ctTokens, ctTokensRendered), `"type":"attachment"`,
			`"type":"attachment","replacesSpan":null`))},
	} {
		got := ctRead(ctThread(tc.lines...), threadPlainMax)
		if len(got.lines) != len(tc.lines) {
			t.Errorf("%s: %d of %d lines read, %d read as no record", name, len(got.lines), len(tc.lines),
				len(got.broken))
			continue
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
// repeating a result included. A failed call's result says so, as the resume's
// result for a call it found unanswered and a killed command's do. A call
// block in the other record, which the Messages API never writes, is a kind
// and no call: a result in a model's message, a call in a user's. A message
// that does not decode is an error, never a message with half a call in it:
// one that is no object, a content absent, null or neither a string nor a
// list, a block that is no object or has no string type, a tool_use's id not
// a string that names a call or its name not a string, a tool_result's
// tool_use_id not a string that names a call or its is_error not a boolean.
// Each block is held so as it is read, wherever it falls: a bad block after
// one accepted is the message's error as much as a bad first block, and the
// error names the block, and the record: an assistant's or a user's.
//
// Mutation: read calls off every record's message, and the blob's result is a
// call; decode a block loosely, and a tool_use whose id is a number is a call
// with no id; take an id that names no call, and a block whose id is empty is
// a call with no id; take either kind of call block in either record, and a
// result in a model's message, or a call in a user's, is a call; read is_error
// as false wherever it is present, and a failed call's result reads as a
// success; hold only a message's first block to its type, and a bad block
// after an accepted one reads as a block of no kind; name every record with
// its type after an "a", and a model's reply is "a assistant record".
func TestCallsComeOnlyFromAUserOrAssistantMessage(t *testing.T) {
	t.Parallel()
	isFalse, isTrue := false, true
	interrupted := "\"[Tool call interrupted: the session ended before this call's result was recorded, so its " +
		"outcome is unknown. Check whether it took effect before relying on it or running it again.]\""
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
		"the result a resume writes for a call it found unanswered": {record: ctInterrupted(ctUUID(4), ctUUID(5)),
			calls: []threadCall{{Kind: blockToolResult, ID: ctCall, IsError: &isTrue, Content: []byte(interrupted)}}},
		"a killed command's result": {record: ctKilled(ctUUID(4), ctUUID(5)), calls: []threadCall{
			{Kind: blockToolResult, ID: ctCall, IsError: &isTrue, Content: []byte(`"Exit code 137"`)}}},
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

	for name, tc := range map[string]struct {
		record string
		kind   string
	}{
		"a result in a model's message": {record: ctAssistant(ctUUID(3), ctUUID(4), ctResultBody), kind: blockToolResult},
		"a call in a user's message":    {record: ctResult(ctUUID(4), ctUUID(5), ctUseContent, ctResultOwn), kind: blockToolUse},
	} {
		m, err := ctLine(t, tc.record).Message()
		if err != nil || m.Calls() != nil || !slices.Equal(m.Kinds(), []string{tc.kind}) {
			t.Errorf("%s reads as the blocks %q and the calls %+v, %v; want a %s and no call", name,
				strings.Join(m.Kinds(), " "), m.Calls(), err, tc.kind)
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
		"a tool_use whose id names none":     ctAssistant(ctUUID(3), ctUUID(4), `[{"type":"tool_use","id":"","name":"Agent","input":{}}]`),
		"a tool_result whose id names none":  user(`"message":{"role":"user","content":[{"tool_use_id":"","type":"tool_result","content":"b1"}]},`),
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

	for name, record := range map[string]string{
		"a reply whose call after its text has a numeric id": ctAssistant(ctUUID(3), ctUUID(4),
			`[{"type":"text","text":"batch 1."},{"type":"tool_use","id":7,"name":"Bash","input":{}}]`),
		"a result after a result, its is_error a string": user(`"message":{"role":"user","content":[` +
			`{"tool_use_id":"` + ctCall + `","type":"tool_result","content":"b1"},{"tool_use_id":"` + ctMCPCall +
			`","type":"tool_result","content":"m1","is_error":"false"}]},`),
	} {
		if m, err := ctLine(t, record).Message(); err == nil || !strings.Contains(err.Error(), "content block 2:") {
			t.Errorf("%s reads as the blocks %q and the calls %+v, %v; want an error naming block 2", name,
				strings.Join(m.Kinds(), " "), m.Calls(), err)
		}
	}

	for record, want := range map[string]string{
		ctAssistant(ctUUID(3), ctUUID(4), `[{"type":"tool_use","id":7,"name":"Bash","input":{}}]`): "an assistant " +
			"record's message: content block 1: id is a number, not a string",
		user(`"message":"b1",`): "a user record's message is a string, not an object",
	} {
		if _, err := ctLine(t, record).Message(); err == nil || err.Error() != want {
			t.Errorf("%.60s reads no message for %v; want %q", record, err, want)
		}
	}
}

// A MESSAGE SAYS ITS CONTENT'S SHAPE: content that is one string is Text, with
// no blocks; content that is a list is each block's type, in order, and no
// Text, and an empty list is an empty Kinds, never the nil one a content that
// is no list reads as. Every user and assistant record measured holds one
// block a record; the API's own messages hold several, which a list of two
// holds to its order.
//
// Mutation: read content as one shape, and a string content is no message;
// start a list's kinds at nil, and an empty list reads as no list.
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
		"an empty list":     {record: ctAssistant(ctUUID(2), ctUUID(3), `[]`), kinds: []string{}},
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

// A REPLY SAYS WHY IT STOPPED: an assistant record's stop_reason as written,
// "" where the key is absent and for every record that is no assistant's, a
// result's message carrying one included, and an error where it is no string
// or the record's message is no object. A reply 2.1.310 wrote as a record a
// block says "tool_use" on its text's record as on its call's, so a thread cut
// between the two never reads as a reply that ended its turn, which a reply of
// text alone and the reply a resume writes each say they did.
//
// Mutation: read stop_reason off every record's message, and a result
// carrying one says why it stopped; read it as any JSON value, and a number
// reads as "".
func TestAReplySaysWhyItStopped(t *testing.T) {
	t.Parallel()
	text := ctAssistant(ctUUID(2), ctUUID(3), `[{"type":"text","text":"batch 1."}]`)
	ended := ctReplace(t, ctAssistant(ctUUID(11), ctUUID(12), `[{"type":"text","text":"done."}]`),
		`"stop_reason":"tool_use"`, `"stop_reason":"end_turn"`)
	result := ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)
	for name, tc := range map[string]struct {
		record string
		reason string
	}{
		"a reply's text, its call in the record after": {record: text, reason: "tool_use"},
		"that reply's call":                            {record: ctAssistant(ctUUID(3), ctUUID(4), ctUseContent), reason: "tool_use"},
		"a reply that ended its turn":                  {record: ended, reason: "end_turn"},
		"the reply a resume writes":                    {record: ctSynthetic(ctUUID(30), ctUUID(31)), reason: "stop_sequence"},
		"a reply that says nothing of its stop":        {record: ctReplace(t, text, `"stop_reason":"tool_use",`, "")},
		"a result whose message carries one":           {record: ctReplace(t, result, `"role":"user",`, `"role":"user","stop_reason":"end_turn",`)},
		"a brief, which is no reply":                   {record: ctBrief(ctUUID(1))},
	} {
		if reason, err := ctLine(t, tc.record).StopReason(); reason != tc.reason || err != nil {
			t.Errorf("%s stopped for %q, %v; want %q", name, reason, err, tc.reason)
		}
	}
	for name, reason := range map[string]string{
		"a number":  `7`,
		"an object": `{"reason":"tool_use"}`,
		"a list":    `["tool_use"]`,
		"a boolean": `true`,
	} {
		if got, err := ctLine(t, ctReplace(t, text, `"stop_reason":"tool_use"`, `"stop_reason":`+reason)).StopReason(); err == nil {
			t.Errorf("a reply whose stop_reason is %s stopped for %q, want an error", name, got)
		}
	}
	for name, message := range map[string]string{
		"a reply whose message is null": `"message":null,`,
		"a reply with no message":       "",
	} {
		record := `{"parentUuid":"` + ctUUID(30) + `","isSidechain":false,` + message + `"type":"assistant","uuid":"` +
			ctUUID(31) + `","timestamp":"` + ctStamp + `",` + ctTail("2.1.310") + `}`
		if got, err := ctLine(t, record).StopReason(); err == nil {
			t.Errorf("%s stopped for %q, want an error", name, got)
		}
	}
}

// ctUsage is the usage ctAssistant's message carries, as written.
const ctUsage = `"usage":{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,` +
	`"output_tokens":20}`

// ctIteration is an entry of a usage's iterations of type kind whose input is
// input, its other counts ctUsage's.
func ctIteration(kind string, input int) string {
	return fmt.Sprintf(`{"type":%q,"input_tokens":%d,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,`+
		`"output_tokens":20}`, kind, input)
}

// A REPLY SAYS WHAT ITS REQUEST TOOK: an assistant record's message's id, its
// model and its usage's four counts, each as written, ok saying the usage is
// there. A count the usage does not name reads 0, the others as written. A
// reply with no usage hands over its id and its model still, every count 0
// and ok false, since Claude Code takes a reply's id with no usage; one with
// no id, or no model, hands over "" for it and the rest as written; and the
// reply a resume writes reads as the counts it writes, all 0, under a model
// of its own. Every record that is no assistant's reads as nothing, a result
// and a request blob whose message carries a usage among them. A usage of the
// wrong shape is an error and never a figure: one that is no object, null
// among it; a count that is no whole number as JSON writes one (negative,
// fractional, in an exponent, a string, null, a boolean, an object) or past
// an int64, whichever count it is; an id or a model that is no string, null
// among it; and a reply whose message is no object.
//
// Mutation: answer for every record's type, and a result carrying a usage
// reads as one; read a null usage as none, or a null count as 0, and each
// reads as a figure; hold a count to a number alone, and a negative or a
// fractional one reads as a count; hand over the id and the model only with
// a usage, and a reply with none reads as no reply's; read two counts each
// into the other's place, and a reply's figures read as another's; require
// every count, and a usage that names three is an error; read the id by its
// text alone, and a numeric id reads as one.
func TestAReplySaysWhatItsRequestTook(t *testing.T) {
	t.Parallel()
	reply := ctAssistant(ctUUID(3), ctUUID(4), ctUseContent)
	using := func(usage string) string { return ctReplace(t, reply, ctUsage, `"usage":`+usage) }
	// every names each count with a figure of its own, so a count read into
	// another's place reads otherwise.
	const every = `{"input_tokens":3,"cache_creation_input_tokens":1500,"cache_read_input_tokens":41250,` +
		`"output_tokens":812}`
	// without is every with one count, written as count is, left out.
	without := func(count string) string { return using(ctReplace(t, every, count, "")) }
	result := ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)
	carrying := func(record string) string {
		return ctReplace(t, record, `"role":"user",`, `"role":"user",`+ctUsage+`,`)
	}
	measured := threadUsage{ID: "msg_test", Model: "claude-sonnet-4-5", Input: 100, Output: 20}
	whole := threadUsage{ID: "msg_test", Model: "claude-sonnet-4-5", Input: 3, CacheWrite: 1500, CacheRead: 41250,
		Output: 812}
	for name, tc := range map[string]struct {
		record string
		usage  threadUsage
		ok     bool
	}{
		"a reply as measured":       {record: reply, usage: measured, ok: true},
		"a reply naming each count": {record: using(every), usage: whole, ok: true},
		"a usage that names no input_tokens": {record: without(`"input_tokens":3,`),
			usage: threadUsage{ID: "msg_test", Model: "claude-sonnet-4-5", CacheWrite: 1500, CacheRead: 41250,
				Output: 812}, ok: true},
		"a usage that names no cache_creation_input_tokens": {record: without(`"cache_creation_input_tokens":1500,`),
			usage: threadUsage{ID: "msg_test", Model: "claude-sonnet-4-5", Input: 3, CacheRead: 41250, Output: 812},
			ok:    true},
		"a usage that names no cache_read_input_tokens": {record: without(`"cache_read_input_tokens":41250,`),
			usage: threadUsage{ID: "msg_test", Model: "claude-sonnet-4-5", Input: 3, CacheWrite: 1500, Output: 812},
			ok:    true},
		"a usage that names no output_tokens": {record: without(`,"output_tokens":812`),
			usage: threadUsage{ID: "msg_test", Model: "claude-sonnet-4-5", Input: 3, CacheWrite: 1500,
				CacheRead: 41250}, ok: true},
		"a usage that names no count": {record: using(`{"service_tier":"standard"}`),
			usage: threadUsage{ID: "msg_test", Model: "claude-sonnet-4-5"}, ok: true},
		"a reply with no usage": {record: ctReplace(t, reply, ","+ctUsage, ""),
			usage: threadUsage{ID: "msg_test", Model: "claude-sonnet-4-5"}},
		"a reply with no id": {record: ctReplace(t, reply, `"id":"msg_test",`, ""),
			usage: threadUsage{Model: "claude-sonnet-4-5", Input: 100, Output: 20}, ok: true},
		"a reply with no model": {record: ctReplace(t, reply, `"model":"claude-sonnet-4-5",`, ""),
			usage: threadUsage{ID: "msg_test", Input: 100, Output: 20}, ok: true},
		"the reply a resume writes": {record: ctSynthetic(ctUUID(30), ctUUID(31)),
			usage: threadUsage{ID: ctUUID(902), Model: "<synthetic>"}, ok: true},
		"a result whose message carries one":         {record: carrying(result)},
		"a request blob whose message carries one":   {record: carrying(ctRequestBlob)},
		"a brief, which is no reply":                 {record: ctBrief(ctUUID(1))},
		"a hook's record, which is no reply":         {record: ctAttachment(ctUUID(5), ctUUID(6), ctBatchSuccess, "")},
		"a Stop's summary, which is no reply either": {record: ctSummary(ctUUID(15), ctUUID(16))},
	} {
		usage, ok, err := ctLine(t, tc.record).Usage()
		if usage != tc.usage || ok != tc.ok || err != nil {
			t.Errorf("%s says its request took %+v (%t), %v; want %+v (%t)", name, usage, ok, err, tc.usage, tc.ok)
		}
	}

	for name, record := range map[string]string{
		"a usage that is null":         using(`null`),
		"a usage that is a string":     using(`"100"`),
		"a usage that is a list":       using(`[100,0,0,20]`),
		"a usage that is a number":     using(`120`),
		"a negative count":             using(ctReplace(t, every, `"input_tokens":3`, `"input_tokens":-3`)),
		"a fractional count":           using(ctReplace(t, every, `"input_tokens":3`, `"input_tokens":3.5`)),
		"a count in an exponent":       using(ctReplace(t, every, `"input_tokens":3`, `"input_tokens":3e0`)),
		"a count that is a string":     using(ctReplace(t, every, `"input_tokens":3`, `"input_tokens":"3"`)),
		"a count that is null":         using(ctReplace(t, every, `"input_tokens":3`, `"input_tokens":null`)),
		"a count that is a boolean":    using(ctReplace(t, every, `"input_tokens":3`, `"input_tokens":true`)),
		"a count that is an object":    using(ctReplace(t, every, `"input_tokens":3`, `"input_tokens":{}`)),
		"a count past an int64":        using(ctReplace(t, every, `"input_tokens":3`, `"input_tokens":9223372036854775808`)),
		"a null cache write":           using(ctReplace(t, every, `:1500`, `:null`)),
		"a negative cache read":        using(ctReplace(t, every, `:41250`, `:-41250`)),
		"a fractional output":          using(ctReplace(t, every, `:812`, `:812.0`)),
		"an id that is a number":       ctReplace(t, reply, `"id":"msg_test"`, `"id":7`),
		"an id that is null":           ctReplace(t, reply, `"id":"msg_test"`, `"id":null`),
		"an id that is an object":      ctReplace(t, reply, `"id":"msg_test"`, `"id":{"id":"msg_test"}`),
		"a model that is null":         ctReplace(t, reply, `"model":"claude-sonnet-4-5"`, `"model":null`),
		"a model that is a list":       ctReplace(t, reply, `"model":"claude-sonnet-4-5"`, `"model":["claude-sonnet-4-5"]`),
		"an id that is null, no usage": ctReplace(t, ctReplace(t, reply, ","+ctUsage, ""), `"id":"msg_test"`, `"id":null`),
	} {
		if usage, ok, err := ctLine(t, record).Usage(); err == nil {
			t.Errorf("%s says its request took %+v (%t), want an error", name, usage, ok)
		}
	}
	for name, message := range map[string]string{
		"a reply whose message is null": `"message":null,`,
		"a reply with no message":       "",
	} {
		record := `{"parentUuid":"` + ctUUID(30) + `","isSidechain":false,` + message + `"type":"assistant","uuid":"` +
			ctUUID(31) + `","timestamp":"` + ctStamp + `",` + ctTail("2.1.310") + `}`
		if usage, ok, err := ctLine(t, record).Usage(); err == nil {
			t.Errorf("%s says its request took %+v (%t), want an error", name, usage, ok)
		}
	}
}

// A REPLY'S COUNTS ARE HELD TO WHAT JSON.parse READS EXACTLY: a count is read
// up to 2^53 - 1, up to which Claude Code's own JSON.parse reads every whole
// number as written, and one past it is an error, whichever count it is:
// past it JSON.parse can read another number than the one written, and four
// counts read as int64s can sum past an int64 and wrap to a negative, where
// the harness's own sum of doubles never does. Four counts at the bound read
// as written, and their sum is exact.
//
// Mutation: drop the bound, and a count of 2^53 reads as one, and four of
// 2^61 sum to a negative; hold a count below the bound, and one at it is an
// error; let the bound itself through one place higher, and a count of 2^53
// reads as one.
func TestAReplysCountsAreHeldToWhatJSONParseReadsExactly(t *testing.T) {
	t.Parallel()
	reply := ctAssistant(ctUUID(3), ctUUID(4), ctUseContent)
	counts := func(input, write, read, output string) string {
		return ctReplace(t, reply, ctUsage, `"usage":{"input_tokens":`+input+`,"cache_creation_input_tokens":`+write+
			`,"cache_read_input_tokens":`+read+`,"output_tokens":`+output+`}`)
	}
	const (
		bound = "9007199254740991" // 2^53 - 1
		past  = "9007199254740992" // 2^53
		far   = "2305843009213693952"
	)
	usage, ok, err := ctLine(t, counts(bound, bound, bound, bound)).Usage()
	if !ok || err != nil || usage.Input != wireCountMax || usage.CacheWrite != wireCountMax ||
		usage.CacheRead != wireCountMax || usage.Output != wireCountMax {
		t.Errorf("a reply whose four counts are each 2^53 - 1 says its request took %+v (%t), %v; want each as written",
			usage, ok, err)
	}
	if sum := usage.Input + usage.CacheWrite + usage.CacheRead + usage.Output; sum != 4*wireCountMax {
		t.Errorf("four counts of 2^53 - 1 sum to %d, want %d", sum, int64(4*wireCountMax))
	}
	for name, record := range map[string]string{
		"an input of 2^53":      counts(past, "0", "0", "0"),
		"a cache write of 2^53": counts("0", past, "0", "0"),
		"a cache read of 2^53":  counts("0", "0", past, "0"),
		"an output of 2^53":     counts("0", "0", "0", past),
		"an input of 2^53 + 1":  counts("9007199254740993", "0", "0", "0"),
		"four counts of 2^61":   counts(far, far, far, far),
		"three of 2^62 and one of 2^0": counts("4611686018427387904", "4611686018427387904",
			"4611686018427387904", "1"),
	} {
		if usage, ok, err := ctLine(t, record).Usage(); err == nil {
			t.Errorf("%s says its request took %+v (%t), want an error: past 2^53 - 1, JSON.parse may read another "+
				"number than the one written", name, usage, ok)
		}
	}
}

// A REPLY'S USAGE IS WHAT CLAUDE CODE COUNTS IT BY, OR AN ERROR: where the
// usage lists iterations, Claude Code counts the list's last entry that is no
// advisor's or compaction's in the usage's place, where that entry is whole
// (of type message or fallback_message, four counts each a finite number at
// or above zero, its input side above zero), and Usage, which hands the
// usage's own counts, is an error where that entry holds other counts, never
// Message's error. Where Claude Code counts the usage's own counts, Usage
// hands them: no iterations, null ones, ones that are no list, an empty list,
// an advisor's or a compaction's entry alone, a counted entry equal to the
// usage (written with fractions among it), a last entry that is not whole
// (of another type, a count that is a string, negative or past a double's
// range, an input side of zero, or no object), and a usage whose own input
// side is zero, whatever its entries say.
//
// Mutation: leave the list unread, and a reply whose counted entry holds more
// reads by its usage alone; count an advisor's or a compaction's entry, and
// the one before it is never compared; count the first entry, not the last,
// and an equal entry hides the one after it; read the list where the usage's
// input side is zero, or count an entry whose own is, or one of another type,
// or with a negative count, and a reply Claude Code counts by its usage is an
// error; count no fallback_message entry, and one that differs reads by the
// usage alone.
func TestAReplysUsageIsWhatClaudeCodeCountsItByOrAnError(t *testing.T) {
	t.Parallel()
	reply := ctAssistant(ctUUID(3), ctUUID(4), ctUseContent)
	const (
		every = `"input_tokens":3,"cache_creation_input_tokens":1500,"cache_read_input_tokens":41250,` +
			`"output_tokens":812`
		same = `{"type":"message",` + every + `}`
		more = `{"type":"message","input_tokens":4,"cache_creation_input_tokens":1500,` +
			`"cache_read_input_tokens":41250,"output_tokens":812}`
		advisor = `{"type":"advisor_message","input_tokens":9000,"cache_creation_input_tokens":0,` +
			`"cache_read_input_tokens":0,"output_tokens":90}`
		compaction = `{"type":"compaction","input_tokens":9000,"cache_creation_input_tokens":0,` +
			`"cache_read_input_tokens":0,"output_tokens":90}`
		flat = `{"type":"message","input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,` +
			`"output_tokens":9000}`
	)
	// listed is the reply whose usage holds every count and lists iterations,
	// as written; listing, the one whose usage lists entries as its iterations.
	listed := func(iterations string) string {
		return ctReplace(t, reply, ctUsage, `"usage":{`+every+`,"iterations":`+iterations+`}`)
	}
	listing := func(entries ...string) string { return listed("[" + strings.Join(entries, ",") + "]") }
	// as is entry with from, which it holds once, written as to.
	as := func(entry, from, to string) string { return ctReplace(t, entry, from, to) }
	whole := threadUsage{ID: "msg_test", Model: "claude-sonnet-4-5", Input: 3, CacheWrite: 1500, CacheRead: 41250,
		Output: 812}
	for name, record := range map[string]string{
		"a usage with no iterations":                    ctReplace(t, reply, ctUsage, `"usage":{`+every+`}`),
		"null iterations":                               listed(`null`),
		"iterations that are an object":                 listed(more),
		"an empty list":                                 listing(),
		"an advisor's entry alone":                      listing(advisor),
		"a compaction's entry alone":                    listing(compaction),
		"a counted entry equal to the usage":            listing(same),
		"an equal entry, then an advisor's":             listing(same, advisor),
		"a fallback entry equal to the usage":           listing(as(same, `"message"`, `"fallback_message"`)),
		"an equal entry written with fractions":         listing(as(same, `:3,`, `:3.0,`)),
		"a differing entry, then an equal one":          listing(more, same),
		"a differing entry of another type":             listing(as(more, `"message"`, `"user"`)),
		"a differing entry whose type is no string":     listing(as(more, `"message"`, `7`)),
		"a differing entry whose count is a string":     listing(as(more, `:4,`, `:"4",`)),
		"a differing entry with a negative count":       listing(as(more, `:812`, `:-812`)),
		"a differing entry with a count past a double":  listing(as(more, `:812`, `:1e400`)),
		"a differing entry with no output count":        listing(as(more, `,"output_tokens":812`, ``)),
		"a differing entry whose input side is zero":    listing(flat),
		"a differing entry, then one that is no object": listing(more, `7`),
	} {
		if usage, ok, err := ctLine(t, record).Usage(); usage != whole || !ok || err != nil {
			t.Errorf("%s says its request took %+v (%t), %v; want %+v, the usage's own counts", name, usage, ok, err,
				whole)
		}
	}
	unanchored := ctReplace(t, reply, ctUsage, `"usage":{"input_tokens":0,"cache_creation_input_tokens":0,`+
		`"cache_read_input_tokens":0,"output_tokens":20,"iterations":[`+more+`]}`)
	if usage, ok, err := ctLine(t, unanchored).Usage(); !ok || err != nil || usage.Output != 20 || usage.Input != 0 {
		t.Errorf("a usage whose input side is zero, listing a differing entry, says its request took %+v (%t), %v; "+
			"want its own counts", usage, ok, err)
	}

	for name, record := range map[string]string{
		"a counted entry that differs":             listing(more),
		"a fallback entry that differs":            listing(as(more, `"message"`, `"fallback_message"`)),
		"a differing entry, then an advisor's":     listing(more, advisor),
		"a differing entry, then a compaction's":   listing(more, compaction),
		"an equal entry, then a differing one":     listing(same, more),
		"an entry that differs in its output":      listing(as(same, `:812`, `:813`)),
		"an entry that counts less than the usage": listing(as(same, `:41250`, `:41249`)),
	} {
		line := ctLine(t, record)
		if usage, ok, err := line.Usage(); err == nil {
			t.Errorf("%s says its request took %+v (%t), want an error: Claude Code counts the entry", name, usage, ok)
		}
		if m, err := line.Message(); err != nil || len(m.Calls()) != 1 || m.Calls()[0].ID != ctCall {
			t.Errorf("%s reads the calls %+v, %v; want its call", name, m.Calls(), err)
		}
	}
}

// A REPLY'S ITERATIONS COST ITS USAGE AN ENTRY AT A TIME: Usage reads a
// usage's iterations an entry at a time and keeps a copy of one entry, the
// last it may count, so a list of listBytes of entries none of which it
// counts costs it no more allocations than a list of one, and no more bytes
// than a few copies of the line, which its decodes of the message and the
// usage make.
//
// Not parallel: the counts are the process's, and a parallel test's
// allocations would be counted with this one's.
//
// Mutation: decode the list whole, or collect its entries, before its last is
// found, and a long list costs an allocation or more for every entry.
func TestAReplysIterationsCostItsUsageAnEntryAtATime(t *testing.T) {
	const (
		head = `{"type":"assistant","message":{"role":"assistant","content":[],"usage":{"input_tokens":3,` +
			`"iterations":[`
		tail = `]}}}`
	)
	usage := func(l threadLine) func() { return func() { _, _, _ = l.Usage() } }
	one := ctLine(t, head+"1"+tail)
	long := ctLine(t, ctListRecord(t, head, "1", tail, listBytes))
	for name, l := range map[string]threadLine{"a list of one": one, "a long list": long} {
		if u, ok, err := l.Usage(); !ok || err != nil || u.Input != 3 {
			t.Fatalf("%s of entries no harness counts says its request took %+v (%t), %v; want its usage's own", name,
				u, ok, err)
		}
	}
	fewObjects, fewBytes := allocationsOf(usage(one))
	manyObjects, manyBytes := allocationsOf(usage(long))
	if manyObjects > fewObjects+listSlack {
		t.Errorf("a usage listing %d bytes of iterations costs %d allocations, and one listing one entry %d",
			len(long.Raw), manyObjects, fewObjects)
	}
	if manyBytes > fewBytes+listCopies*uint64(len(long.Raw)) {
		t.Errorf("a usage listing %d bytes of iterations costs %d bytes, and one listing one entry %d: more than %d "+
			"copies of the line", len(long.Raw), manyBytes, fewBytes, listCopies)
	}
}

// A LIST IS WALKED AN ELEMENT AT A TIME: each element is handed out as it is
// read, in order and as written but for the whitespace around it; the walk
// stops at the first error its callback returns, returns that error itself,
// never wrapped, and reads no element after it, so a list whose later elements
// are no JSON at all still ends with the callback's error; and a value that is
// no list, a list cut short, one whose element is no JSON and one followed by
// more are each an error of the walk's own, never io.EOF. An empty list hands
// out nothing.
//
// Mutation: read the list whole before handing out its first element, and the
// list whose third element is no JSON ends with the decode's error, its first
// two never handed out; wrap the callback's error, and it no longer comes back
// as itself; walk on past it, and the walk ends with the decode's error; take
// any value for a list, and a string or nothing ends the walk with io.EOF;
// take what follows a list, and a list followed by another reads as one.
func TestAListIsWalkedAnElementAtATime(t *testing.T) {
	t.Parallel()
	walk := func(list string, stopAt int, stop error) ([]string, error) {
		var handed []string
		err := eachElement(json.RawMessage(list), func(element json.RawMessage) error {
			handed = append(handed, string(element))
			if len(handed) == stopAt {
				return stop
			}
			return nil
		})
		return handed, err
	}
	handed, err := walk(` [ 1 , "two" , {"three": [3, 3]} , [ ] , null , false ] `, 0, nil)
	if want := []string{`1`, `"two"`, `{"three": [3, 3]}`, `[ ]`, `null`, `false`}; err != nil || !slices.Equal(handed, want) {
		t.Errorf("a list's elements are handed out as %q, %v; want %q", handed, err, want)
	}
	handed, err = walk(`[]`, 0, nil)
	if err != nil || handed != nil {
		t.Errorf("an empty list hands out %q, %v; want nothing", handed, err)
	}
	stop := errors.New("the reader has read enough")
	handed, err = walk(`[1,"two",}`, 2, stop)
	if !errors.Is(err, stop) || errors.Unwrap(err) != nil || !slices.Equal(handed, []string{`1`, `"two"`}) {
		t.Errorf("a walk its callback stops at the second element hands out %q and ends with %v; want the two "+
			"elements, and the callback's own error as it returned it", handed, err)
	}
	for name, list := range map[string]string{
		"an object":                       `{"a":[1]}`,
		"a string":                        `"[1]"`,
		"nothing":                         ``,
		"a list cut short":                `[1,2`,
		"a list whose element is no JSON": `[1,}]`,
		"a list followed by more":         `[1] [2]`,
	} {
		if handed, err := walk(list, 0, nil); err == nil || errors.Is(err, io.EOF) {
			t.Errorf("%s walks as %q, %v; want an error of the walk's own, never io.EOF, which a reader could "+
				"take for the list's end", name, handed, err)
		}
	}
}

// ctListRecord is a record that opens with head, ends with tail, and holds
// between them a list of element written over and over, filled out to a line
// of exactly size bytes, its break included.
func ctListRecord(t *testing.T, head, element, tail string, size int) string {
	t.Helper()
	fill := size - 1 - len(head) - len(element) - len(tail)
	if fill < 0 {
		t.Fatalf("a line of %d bytes cannot hold %s%s%s", size, head, element, tail)
	}
	more, pad := fill/(len(element)+1), fill%(len(element)+1)
	return head + element + strings.Repeat(","+element, more) + strings.Repeat(" ", pad) + tail
}

// AN OBJECT IS WALKED A KEY AT A TIME: each key is handed out as it is read,
// in order, its name unquoted as the layer's options read a name (an escape
// resolved, a lone surrogate and a byte that is no UTF-8 each as U+FFFD, and
// its case as written) and its value as written but for the whitespace around
// it; a key written twice is handed out both times, each in its place; the
// walk stops at the first error its callback returns, returns that error
// itself, never wrapped, and reads no key after it, so an object whose third
// key's value is no JSON still ends with the callback's error; and a value
// that is no object, an object cut short, one whose key is no string, one
// whose value is no JSON and one followed by more are each an error of the
// walk's own, never io.EOF. An empty object hands out nothing.
//
// Mutation: read the object whole before handing out its first key, and the
// object whose third key is no JSON ends with the decode's error, its first
// two never handed out; hand a name out as written, and an escaped name keeps
// its escape; fold a key written twice into one, and its second value is
// never handed out; wrap the callback's error, and it no longer comes back as
// itself; walk on past it, and the walk ends with the decode's error; take any
// value for an object, and a string or nothing ends the walk with io.EOF;
// take what follows an object, and an object followed by another reads as
// one.
func TestAnObjectIsWalkedAKeyAtATime(t *testing.T) {
	t.Parallel()
	type key struct{ name, value string }
	walk := func(object string, stopAt int, stop error) ([]key, error) {
		var handed []key
		err := eachKey(json.RawMessage(object), func(name []byte, value json.RawMessage) error {
			handed = append(handed, key{string(name), string(value)})
			if len(handed) == stopAt {
				return stop
			}
			return nil
		})
		return handed, err
	}
	handed, err := walk(` { "a" : 1 , "b\u0061" : "two" , "c" : {"three": [3, 3]} , "a" : [ ] , "A" : null , `+
		`"cut \ud83d" : false , "bad `+"\xff"+`" : 5 } `, 0, nil)
	want := []key{{"a", `1`}, {"ba", `"two"`}, {"c", `{"three": [3, 3]}`}, {"a", `[ ]`}, {"A", `null`},
		{"cut \ufffd", `false`}, {"bad \ufffd", `5`}}
	if err != nil || !slices.Equal(handed, want) {
		t.Errorf("an object's keys are handed out as %q, %v; want %q", handed, err, want)
	}
	handed, err = walk(`{}`, 0, nil)
	if err != nil || handed != nil {
		t.Errorf("an empty object hands out %q, %v; want nothing", handed, err)
	}
	stop := errors.New("the reader has read enough")
	handed, err = walk(`{"a":1,"b":"two","c":}`, 2, stop)
	if !errors.Is(err, stop) || errors.Unwrap(err) != nil || !slices.Equal(handed, []key{{"a", `1`}, {"b", `"two"`}}) {
		t.Errorf("a walk its callback stops at the second key hands out %q and ends with %v; want the two keys, "+
			"and the callback's own error as it returned it", handed, err)
	}
	for name, object := range map[string]string{
		"a list":                           `[{"a":1}]`,
		"a string":                         `"{}"`,
		"nothing":                          ``,
		"an object cut short":              `{"a":1`,
		"an object whose key is no string": `{1:2}`,
		"an object whose value is no JSON": `{"a":}`,
		"an object followed by more":       `{"a":1} {"b":2}`,
	} {
		if handed, err := walk(object, 0, nil); err == nil || errors.Is(err, io.EOF) {
			t.Errorf("%s walks as %q, %v; want an error of the walk's own, never io.EOF, which a reader could "+
				"take for the object's end", name, handed, err)
		}
	}
}

// ctKeysRecord is a record that opens with head, ends with tail, and holds
// between them the keys of an object, key(0), key(1) and on, each a whole
// `"name":value`, as many as a line of exactly size bytes holds, its break
// included, spaces making up the rest; and how many keys it holds.
func ctKeysRecord(t *testing.T, head string, key func(i int) string, tail string, size int) (string, int) {
	t.Helper()
	room := size - 1 - len(head) - len(tail)
	var keys strings.Builder
	n := 0
	for ; ; n++ {
		next := key(n)
		if n > 0 {
			next = "," + next
		}
		if keys.Len()+len(next) > room {
			break
		}
		keys.WriteString(next)
	}
	if n == 0 {
		t.Fatalf("a line of %d bytes cannot hold %s%s%s", size, head, key(0), tail)
	}
	return head + keys.String() + strings.Repeat(" ", room-keys.Len()) + tail, n
}

// A CONTENT LIST COSTS ONE BLOCK AT A TIME: a message's content and a reply's
// applied context edits are read a block at a time, each held to its type as
// it is read. So a list refused at its first block costs what a list of that
// block alone costs, however many follow it: no allocation more, and no byte
// more than the copies of the list its reader makes as it decodes the record
// around it (Message one, its content's; ToolResultsCleared two, its report's
// and its edits'), never a header and a copy for each element, nor a slice of
// them, before the first is checked. A line at the layer's bound holding four
// million `1`s, or a list of empty objects, shows it. The walk itself, with
// nobody keeping what it hands out, costs the same for a long list as for a
// list of one, in allocations and in bytes: it never collects the elements.
// And a list read to its end leaves held what Message says it does, a kind
// for each block and a call for each call block, and nothing of the list: for
// a list of the smallest blocks of a kind no reader names, and of the smallest
// results, each a header for each kind and each call's fields, at most a
// quarter over for append's growth, and the bytes of each kind and id.
//
// Not parallel: the counts are the process's, and a parallel test's
// allocations would be counted with this one's.
//
// Mutation: decode a message's content, or a reply's edits, whole before its
// first block is checked, and a list at the bound costs millions of
// allocations more than a list of one; collect its blocks or its edits before
// the first is checked, size its kinds from the list's length, or have the
// walk collect the list before it hands out the first element, and it costs
// from several times to tens of times the line more, in a few allocations;
// keep a copy of the content in the message, and a list read to its end holds
// that copy beside its kinds.
func TestAContentListCostsOneBlockAtATime(t *testing.T) {
	// slack is how many more allocations a list at the bound may cost than a
	// list of one, though it reads no more of it. The count is the process's,
	// and the bound's copy of its content can set off a collection while it
	// is measured, which empties the process's pools (fmt's printers, the
	// decoders): the call goes on to build what it takes from them afresh, a
	// few allocations each, eleven in all where measured. Under the race
	// detector a pool also drops a quarter of what is put in it. A whole
	// decode costs millions more.
	const slack = 64
	message := func(l threadLine) error {
		_, err := l.Message()
		return err
	}
	cleared := func(l threadLine) error {
		_, err := l.ToolResultsCleared()
		return err
	}
	user := `{"type":"user","message":{"role":"user","content":[`
	reply := `{"type":"assistant","message":{"role":"assistant","content":[],"context_management":{"applied_edits":[`
	for name, tc := range map[string]struct {
		head, element, tail string
		read                func(threadLine) error
		// copies is how many copies of the list its reader makes as it decodes
		// the record around it, measured: Message its content's, and
		// ToolResultsCleared its report's and its edits'. A list may cost
		// those and ONE LINE MORE, so a reader that makes a copy more still
		// passes, where a slice of the list's elements, or a header for each,
		// costs several lines.
		copies uint64
	}{
		"a message's content of numbers":       {head: user, element: "1", tail: `]}}`, read: message, copies: 1},
		"a message's content of empty objects": {head: user, element: "{}", tail: `]}}`, read: message, copies: 1},
		"a reply's applied edits of numbers":   {head: reply, element: "1", tail: `]}}}`, read: cleared, copies: 2},
	} {
		one := ctLine(t, tc.head+tc.element+tc.tail)
		full := ctLine(t, ctListRecord(t, tc.head, tc.element, tc.tail, threadLineMax))
		if len(full.Raw) != threadLineMax-1 || tc.read(one) == nil || tc.read(full) == nil {
			t.Fatalf("%s: a line of %d bytes whose first block is none reads with no error", name, len(full.Raw)+1)
		}
		fewObjects, fewBytes := allocationsOf(func() { _ = tc.read(one) })
		manyObjects, manyBytes := allocationsOf(func() { _ = tc.read(full) })
		if manyObjects > fewObjects+slack {
			t.Errorf("%s: refused at its first block, a list at the line's bound costs %d allocations, and a list "+
				"of one %d", name, manyObjects, fewObjects)
		}
		if manyBytes > fewBytes+(tc.copies+1)*uint64(threadLineMax) {
			t.Errorf("%s: refused at its first block, a list at the line's bound costs %d bytes, and a list of one "+
				"%d: more than %d copies of the line and one line more", name, manyBytes, fewBytes, tc.copies)
		}
	}

	// A list of listBytes shows per element what one at the bound costs, in an
	// eighth of the time, which the race detector multiplies.
	oneList := json.RawMessage(`[1]`)
	longList := json.RawMessage(ctListRecord(t, "[", "1", "]", listBytes))
	elements := 0
	count := func(json.RawMessage) error {
		elements++
		return nil
	}
	if err := eachElement(longList, count); err != nil || elements != strings.Count(string(longList), "1") {
		t.Fatalf("a list of %d bytes walks as %d elements, %v; want every one of its %d", len(longList), elements,
			err, strings.Count(string(longList), "1"))
	}
	keepNone := func(json.RawMessage) error { return nil }
	fewObjects, fewBytes := allocationsOf(func() { _ = eachElement(oneList, keepNone) })
	manyObjects, manyBytes := allocationsOf(func() { _ = eachElement(longList, keepNone) })
	if manyObjects > fewObjects+slack || manyBytes > fewBytes+uint64(len(longList)) {
		t.Errorf("a walk of a list of %d bytes costs %d allocations and %d bytes, and of a list of one %d and %d: "+
			"more than the list's own size more", len(longList), manyObjects, manyBytes, fewObjects, fewBytes)
	}

	// heldSlack is how many bytes a message read to its end may hold beyond its
	// kinds and its calls: the message itself, and whatever else the heap holds
	// at the second collection that it did not at the first. Measured, a
	// message held its two arrays and nothing more to within a few dozen
	// bytes; a copy of the list is the list's size.
	const heldSlack = 64 << 10
	for name, tc := range map[string]struct {
		element string
		// own is how many bytes of its own each block may hold beyond its kind's
		// header and its call's fields: the text of a kind the layer does not
		// name, and a call's id. A kind the layer names is its own constant
		// (TestAKnownBlockKindIsHeldAsTheLayersOwnString).
		own   uint64
		calls bool
	}{
		"blocks of a kind no reader names": {element: `{"type":"x"}`, own: 1},
		"the smallest results":             {element: `{"type":"tool_result","tool_use_id":"a"}`, own: 1, calls: true},
	} {
		line := ctLine(t, ctListRecord(t, user, tc.element, `]}}`, listBytes))
		read := func() any {
			m, _ := line.Message()
			return m
		}
		m, err := line.Message()
		blocks, calls := uint64(strings.Count(string(line.Raw), tc.element)), uint64(0)
		if tc.calls {
			calls = blocks
		}
		if err != nil || uint64(len(m.Kinds())) != blocks || uint64(len(m.Calls())) != calls {
			t.Fatalf("%s: a list of %d blocks reads as %d kinds and %d calls, %v; want %d and %d", name, blocks,
				len(m.Kinds()), len(m.Calls()), err, blocks, calls)
		}
		kinds := blocks * (uint64(unsafe.Sizeof("")) + tc.own)
		fields := calls * uint64(unsafe.Sizeof(threadCall{}))
		if held, limit := heldBy(read), (kinds+fields)*5/4+heldSlack; held > limit {
			t.Errorf("%s: a list of %d bytes read to its end holds %d bytes, more than its %d kinds and %d calls "+
				"take: %d", name, len(line.Raw)+1, held, blocks, calls, limit)
		}
	}
}

// AN OBJECT'S WALK COSTS WHAT ONE KEY COSTS: the walk itself, with nobody
// keeping what it hands out, costs an object of tens of thousands of keys the
// allocations an object of one key costs, and no byte more than the object's
// own size, whether its names are written plainly or escaped: it never
// collects the keys, and hands each name out in a buffer it reuses, never as a
// string or a copy of its own, either of which is an allocation for every key.
//
// Not parallel: the counts are the process's, and a parallel test's
// allocations would be counted with this one's.
//
// Mutation: have the walk collect the keys before it hands out the first, or
// hand out a copy of each name, and an object of listBytes costs an
// allocation or more for every key it holds.
func TestAnObjectsWalkCostsWhatOneKeyCosts(t *testing.T) {
	keepNone := func([]byte, json.RawMessage) error { return nil }
	for name, key := range map[string]func(int) string{
		"names written plainly": func(i int) string { return fmt.Sprintf(`"m%06x":{}`, i) },
		"names escaped":         func(i int) string { return fmt.Sprintf(`"\u006d%06x":{}`, i) },
	} {
		one := json.RawMessage(`{` + key(0) + `}`)
		long, n := ctKeysRecord(t, `{`, key, `}`, listBytes)
		object := json.RawMessage(long)
		keys := 0
		if err := eachKey(object, func([]byte, json.RawMessage) error {
			keys++
			return nil
		}); err != nil || keys != n {
			t.Fatalf("%s: an object of %d keys walks as %d keys, %v", name, n, keys, err)
		}
		fewObjects, fewBytes := allocationsOf(func() { _ = eachKey(one, keepNone) })
		manyObjects, manyBytes := allocationsOf(func() { _ = eachKey(object, keepNone) })
		if manyObjects > fewObjects+listSlack || manyBytes > fewBytes+uint64(len(object)) {
			t.Errorf("%s: a walk of an object of %d keys costs %d allocations and %d bytes, and of an object of "+
				"one %d and %d: more than the object's own size more", name, n, manyObjects, manyBytes, fewObjects,
				fewBytes)
		}
	}
}

// A KNOWN BLOCK KIND IS HELD AS THE LAYER'S OWN STRING: a block of a kind the
// layer names (a text, a thinking and a redacted thinking, a call and a
// result) is held in Kinds as the layer's own constant, so a list of such
// blocks holds no string of its own for a block's kind, only its place in the
// list; a kind the layer does not name is held as written.
//
// Mutation: hold every kind as its decode left it, and each known kind is a
// string of its own.
func TestAKnownBlockKindIsHeldAsTheLayersOwnString(t *testing.T) {
	t.Parallel()
	reply, err := ctLine(t, ctAssistant(ctUUID(2), ctUUID(3), `[{"type":"text","text":"batch 1."},`+
		`{"type":"thinking","thinking":"the batch","signature":"c2ln"},{"type":"redacted_thinking","data":"ZGF0YQ=="},`+
		ctUseContent[1:len(ctUseContent)-1]+`,{"type":"server_tool_use","id":"srvtoolu_test","name":"web_search",`+
		`"input":{}}]`)).Message()
	if err != nil {
		t.Fatalf("a reply of every kind reads no message: %v", err)
	}
	result, err := ctLine(t, ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)).Message()
	if err != nil {
		t.Fatalf("a result reads no message: %v", err)
	}
	kinds := append(slices.Clone(reply.Kinds()), result.Kinds()...)
	want := []string{blockText, blockThinking, blockRedactedThinking, blockToolUse, "server_tool_use", blockToolResult}
	if !slices.Equal(kinds, want) {
		t.Fatalf("the blocks read as %q, want %q", strings.Join(kinds, " "), strings.Join(want, " "))
	}
	for i, kind := range kinds {
		if known := i != 4; known != (unsafe.StringData(kind) == unsafe.StringData(want[i])) {
			t.Errorf("block %d, a %s, is held as the layer's own string %t, want %t", i+1, kind, !known, known)
		}
	}
}

// A SERVER'S CLEARING IS WHAT AN ASSISTANT RECORD SAYS IT APPLIED: a model's
// reply whose message reports, among the context edits the server applied to
// the request it answers, the clearing of old tool results with a count above
// none. Nothing else is one: that edit with a count of none, another edit
// (even one carrying the clearing's count), no edit, a report that lists none,
// the null report of the reply a resume writes, a reply as measured, which
// carries no report at all; nor the clearing reported on a record that is no
// model's reply, which answered no request. A report of the wrong shape (a
// report neither an object nor null, edits that are no list, an edit that is
// no object or whose type is no string, the clearing's count no number) is an
// error, wherever its bad edit falls, never a report read as none, and it
// costs this accessor alone: the record's message still reads. So is a reply
// whose message is no object, a null one among them.
//
// Mutation: count a count of none, or read the count off every edit, and a
// report that cleared nothing reads as cleared; answer for every record's
// type, and a result carrying the clearing reads as cleared; refuse a null
// report, and the reply a resume writes is an error; let a null message
// through, and it reads as one that reports nothing; stop at the first
// clearing, and an edit after it of the wrong shape passes; hold the report in
// Message, and a record whose report is of the wrong shape reads no message.
func TestAServerClearingIsWhatAnAssistantRecordSaysItApplied(t *testing.T) {
	t.Parallel()
	result := ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)
	carrying := func(record, report string) string {
		return ctReplace(t, record, `"role":"user",`, `"role":"user","context_management":`+report+`,`)
	}
	for name, tc := range map[string]struct {
		record  string
		cleared bool
	}{
		"the clearing, with a count":      {record: ctReported(t, ctEdits(ctClearing)), cleared: true},
		"the clearing after another edit": {record: ctReported(t, ctEdits(ctThinking, ctClearing)), cleared: true},
		"the clearing with a count of none": {record: ctReported(t, ctEdits(ctReplace(t, ctClearing,
			`"cleared_tool_uses":18`, `"cleared_tool_uses":0`)))},
		"another edit, with a count of its own": {record: ctReported(t, `{"applied_edits":[`+ctThinking+`]}`)},
		"another edit, carrying the clearing's count": {record: ctReported(t,
			ctEdits(`{"type":"clear_thinking_20251015","cleared_tool_uses":18}`))},
		"no edit applied":                            {record: ctReported(t, ctEdits())},
		"a report that lists no edits":               {record: ctReported(t, `{"client_sent_keep_tool_uses":12}`)},
		"the reply a resume writes, its report null": {record: ctSynthetic(ctUUID(30), ctUUID(31))},
		"a reply as measured, with no report": {
			record: ctAssistant(ctUUID(30), ctUUID(31), `[{"type":"text","text":"back."}]`)},
		"a result carrying the clearing":         {record: carrying(result, ctEdits(ctClearing))},
		"a result carrying a report of no shape": {record: carrying(result, `"cleared"`)},
		"a request blob carrying the clearing":   {record: carrying(ctRequestBlob, ctEdits(ctClearing))},
	} {
		if cleared, err := ctLine(t, tc.record).ToolResultsCleared(); cleared != tc.cleared || err != nil {
			t.Errorf("%s reads as cleared %t, %v; want %t", name, cleared, err, tc.cleared)
		}
	}

	count := func(to string) string { return ctReplace(t, ctClearing, `"cleared_tool_uses":18,`, to) }
	for name, report := range map[string]string{
		"a report that is a string":         `"clear_tool_uses_20250919"`,
		"a report that is a list":           `[` + ctClearing + `]`,
		"edits that are an object":          `{"applied_edits":` + ctClearing + `}`,
		"edits that are null":               `{"applied_edits":null}`,
		"an edit that is no object":         ctEdits(`"clear_tool_uses_20250919"`),
		"an edit of no type":                ctEdits(`{"cleared_tool_uses":18}`),
		"an edit whose type is a number":    ctEdits(`{"type":7,"cleared_tool_uses":18}`),
		"the clearing, its count a string":  ctEdits(count(`"cleared_tool_uses":"18",`)),
		"the clearing, its count null":      ctEdits(count(`"cleared_tool_uses":null,`)),
		"the clearing with no count":        ctEdits(count("")),
		"the clearing, then a null edit":    ctEdits(ctClearing, "null"),
		"another edit, then a numeric edit": ctEdits(ctThinking, "7"),
	} {
		line := ctLine(t, ctReported(t, report))
		if cleared, err := line.ToolResultsCleared(); err == nil {
			t.Errorf("%s reads as cleared %t, want an error", name, cleared)
		}
		if m, err := line.Message(); err != nil || !slices.Equal(m.Kinds(), []string{"text"}) {
			t.Errorf("%s reads its message as the blocks %q, %v; want it whole", name, strings.Join(m.Kinds(), " "), err)
		}
	}

	for name, message := range map[string]string{
		"a reply whose message is null": `"message":null,`,
		"a reply with no message":       "",
	} {
		record := `{"parentUuid":"` + ctUUID(30) + `","isSidechain":false,` + message + `"type":"assistant","uuid":"` +
			ctUUID(31) + `","timestamp":"` + ctStamp + `",` + ctTail("2.1.310") + `}`
		if cleared, err := ctLine(t, record).ToolResultsCleared(); err == nil {
			t.Errorf("%s reads as cleared %t, want an error", name, cleared)
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

// A SAVED COMPACTION IS NAMED BY BOTH NAMES CLAUDE CODE READS IT BY: the one
// it writes, in the session's directory, and the sibling beside that
// directory, in the order it reads them, each read back as the session's
// saved compaction, one kind for both. A name beside either (a temporary one,
// another extension, either with no separator), either name under the
// subagents' directory or a subagent's, either name of another session or
// project, and a name the caller did not clean name none; and a session's id
// outside sandbox.ValidSessionID's shape names none, nor reads back as the
// session of the names its own spelling would make.
//
// Mutation: name the first alone, and the sibling is no name of the session's;
// swap the two, and the one Claude Code reads second reads first; read back
// either as no member, and a session holding it reads as one that holds none;
// match a name by its last element, and the name under the subagents'
// directory reads as one; drop the session's check, and `a/b` names one.
func TestASavedCompactionIsNamedByBothNamesClaudeCodeReads(t *testing.T) {
	t.Parallel()
	dir := "projects/" + claudeProjectDir + "/" + ctSession
	names, ok := claudePrecompact(ctSession)
	if want := []string{dir + "/precompact.json", dir + ".precompact.json"}; !ok || !slices.Equal(names, want) {
		t.Errorf("a session's saved compaction is named %q (%t), want %q", names, ok, want)
	}
	for _, name := range names {
		if member, ok := claudeMemberOf(ctSession, name); !ok || member != (threadMember{Kind: memberPrecompact}) {
			t.Errorf("%q reads back as %+v (%t), want the session's saved compaction", name, member, ok)
		}
	}

	other := "projects/" + claudeProjectDir + "/" + ctOther
	for _, name := range []string{
		dir + "/precompact.json.tmp",
		dir + ".precompact.json.tmp",
		dir + "/precompact.jsonl",
		dir + "precompact.json",
		dir + "/precompact.json/",
		dir + "/subagents/precompact.json",
		dir + "/subagents/agent-" + ctAgent + "/precompact.json",
		dir + "/subagents/agent-" + ctAgent + ".precompact.json",
		dir + "/" + ctSession + ".precompact.json",
		other + "/precompact.json",
		other + ".precompact.json",
		"projects/" + claudeProjectDir + "/./" + ctSession + "/precompact.json",
		"/" + dir + "/precompact.json",
		"/" + dir + ".precompact.json",
		"projects/elsewhere/" + ctSession + "/precompact.json",
		"precompact.json",
	} {
		if member, ok := claudeMemberOf(ctSession, name); ok {
			t.Errorf("%q reads as the member %+v", name, member)
		}
	}
	for _, session := range []string{"", "..", "a/b", "a.b", strings.Repeat("s", executorkeep.SessionIDMaxBytes+1)} {
		if names, ok := claudePrecompact(session); ok || names != nil {
			t.Errorf("the session %q names its saved compaction %q (%t)", session, names, ok)
		}
		own := "projects/" + claudeProjectDir + "/" + session
		for _, name := range []string{dir + "/precompact.json", own + "/precompact.json", own + ".precompact.json"} {
			if member, ok := claudeMemberOf(session, name); ok {
				t.Errorf("the session %q reads %q as the member %+v", session, name, member)
			}
		}
	}
}
