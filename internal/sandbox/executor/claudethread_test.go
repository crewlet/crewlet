package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"unsafe"

	"github.com/google/uuid"

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

// The records of a hook's firing the layer reads by no name, beside the three
// it names (hookSuccess, hookAdditionalContext and hookSystemMessage): a
// firing past its timeout, and the summary a Stop firing ends with, a system
// record of this subtype.
const (
	hookCancelled   = "hook_cancelled"
	hookStopSummary = "stop_hook_summary"
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
		line, err := r.Next(context.Background())
		var broken *threadLineError
		switch {
		case err == nil:
			got.lines = append(got.lines, line)
		case errors.As(err, &broken):
			got.broken = append(got.broken, broken)
		default:
			got.end, got.torn = err, r.Torn()
			_, got.again = r.Next(context.Background())
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
// break dropped; a last piece with none is never a line, whatever it holds (a
// record still being written, or the NULs GNU tar pads a shrunk member with),
// and Torn says it was there. A carriage return ends no line. The member's end
// is sticky: asked again, the reader answers it again. A subagent's metadata
// (one JSON document, no break) is only a torn piece. Each line says the bytes
// it takes, break included, hands its bytes minus the break ([threadBytes]),
// and decodes as the record ([decodeThreadLine]).
//
// Mutation: hand out the last piece as a line, count a size without its break,
// read byte facts with the break or a CR, or decode other bytes, and every
// line says so.
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
		// records is each whole record the member holds, its break dropped.
		records := strings.Split(tc.text, "\n")
		records = records[:len(records)-1]
		for i, line := range got.lines {
			if i >= len(records) {
				break
			}
			ctWholeRecord(t, name, i+1, line, records[i])
		}
		if !errors.Is(got.end, io.EOF) || got.torn != tc.torn || !errors.Is(got.again, io.EOF) {
			t.Errorf("%s: the reading ended with %v, torn %t, and then %v; want io.EOF, torn %t, and io.EOF again",
				name, got.end, got.torn, got.again, tc.torn)
		}
	}
	got := ctRead(ctEnqueue+"\r\n"+brief+"\n", threadPlainMax)
	if len(got.lines) != 2 {
		t.Fatalf("a member of two records reads as %d lines", len(got.lines))
	}
	ctWholeRecord(t, "a record ended by CRLF", 1, got.lines[0], ctEnqueue+"\r")
	ctWholeRecord(t, "a record ended by a break alone", 2, got.lines[1], brief)
}

// ctWholeRecord fails t unless line is record, a whole record its break
// dropped, read as line n of the member it was read from (name): numbered n,
// taking its record's bytes and its break, handing its record's byte facts
// and reading, every accessor, as the record's whole decode.
func ctWholeRecord(t *testing.T, name string, n int, line threadLine, record string) {
	t.Helper()
	if line.N != n || line.Size != len(record)+1 || line.Bytes != wholeLineBytes([]byte(record)) {
		t.Errorf("%s: line %d is numbered %d, says it takes %d bytes and hands byte facts of %d; want %d with "+
			"its break, and the facts of its bytes but the break", name, n, line.N, line.Size, line.Bytes.Len(),
			len(record)+1)
	}
	decoded, err := decodeThreadLine(n, []byte(record), nil)
	if err != nil {
		t.Fatalf("%s: line %d, decoded whole, is no record: %v", name, n, err)
	}
	if got, want := ctAll(line), ctAll(decoded); got != want {
		t.Errorf("%s: line %d reads as\n%s\nwant its record's whole decode\n%s", name, n, got, want)
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

// A LINE OR A MEMBER PAST ITS BOUND ENDS THE READING: a line whose facts pass
// [threadLineMax], one holding a value it reads whole one byte past
// [threadValueMax], or a member one byte longer than its reader's bound, ends
// it with errThreadBound, saying which it ran past, and the reading stays
// over. A member is past its bound EVEN WHERE THE BOUND FALLS RIGHT AFTER A
// BREAK, where io.LimitReader would end it cleanly. A value or a member AT its
// bound reads whole, and a 9 MiB line of a value no reader reads is no bound's.
//
// Mutation: read a line past a bound as a refused line, and the record after
// it is read; meter the member with io.LimitReader, and a member past its
// bound ends cleanly; swap two bounds' words, and one reads as the other.
func TestALineOrAMemberPastItsBoundEndsTheReading(t *testing.T) {
	t.Parallel()
	// valued is a brief whose uuid, as written, is n bytes long.
	valued := func(n int) string { return ctBrief(strings.Repeat("u", n-2)) }
	// called is a reply of n calls, whose facts pass the bound before its
	// bytes do.
	called := func(n int) string {
		return ctAssistant(ctUUID(1), ctUUID(2), "["+strings.Repeat(`{"type":"tool_use","id":"toolu_1",`+
			`"name":"Bash","input":{}},`, n-1)+`{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]`)
	}
	brief := ctBrief(ctUUID(1))
	head := ctThread(ctEnqueue, brief)
	n := int64(len(head))
	// What the error of a reading past a bound says it ran past.
	const value, facts, member = "a value is read whole within", "facts are held within", "the member runs past"
	for name, tc := range map[string]struct {
		text     string
		maxBytes int64
		lines    int
		past     string
	}{
		"a 9 MiB line of a value no reader reads, a record after it": {
			text: ctPadded(9<<20) + ctThread(brief), maxBytes: threadPlainMax, lines: 2},
		"a value at its bound": {text: ctThread(valued(threadValueMax), brief), maxBytes: threadPlainMax, lines: 2},
		"a value a byte past its bound, a record after it": {
			text: ctThread(ctEnqueue, valued(threadValueMax+1), brief), maxBytes: threadPlainMax, lines: 1,
			past: value},
		"a line whose facts pass their bound, a record after it": {
			text: ctThread(ctEnqueue, called(200_000), brief), maxBytes: threadPlainMax, lines: 1, past: facts},
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

// A MEMBER OF MORE WHOLE LINES THAN ITS BOUND ENDS THE READING: a line costs
// its reading whatever its length, so once a reader has read the line past
// its lines' bound, a line that is no record counted as any other, it ends
// with errThreadBound and stays over. A member at its bound reads whole, and
// so does one that ends there in a torn piece, which is no line. A reader of
// a whole thread reads within [threadLinesMax].
//
// Mutation: drop the lines' bound, or make a reader's default any other, and
// a member past it reads to its end.
func TestAMemberPastItsLinesBoundEndsTheReading(t *testing.T) {
	t.Parallel()
	if r := newThreadReader(strings.NewReader(""), threadPlainMax); r.maxLines != threadLinesMax {
		t.Errorf("a whole thread is read within %d lines, want %d", r.maxLines, threadLinesMax)
	}
	brief := ctBrief(ctUUID(1))
	for name, tc := range map[string]struct {
		text           string
		maxLines       int
		lines, broken  int
		past, tornHere bool
	}{
		"a member at its bound": {text: ctThread(brief, brief, brief), maxLines: 3, lines: 3},
		"a member at its bound, then a torn piece": {text: ctThread(brief, brief, brief) + "{", maxLines: 3, lines: 3,
			tornHere: true},
		"a member a record past its bound": {text: ctThread(brief, brief, brief, brief), maxLines: 3, lines: 3,
			past: true},
		"a member past its bound, a line no record among it": {text: ctThread(brief, "0", brief, "0"), maxLines: 3,
			lines: 2, broken: 1, past: true},
		"a line at a bound of none":          {text: "0\n", past: true},
		"a line at a bound below none":       {text: ctThread(brief), maxLines: -1, past: true},
		"an empty member at a bound of none": {text: ""},
	} {
		r := newThreadReader(strings.NewReader(""), threadPlainMax)
		r.Reset(strings.NewReader(tc.text), threadPlainMax, tc.maxLines)
		lines, broken := 0, 0
		var end error
		for end == nil {
			_, err := r.Next(t.Context())
			var refused *threadLineError
			switch {
			case err == nil:
				lines++
			case errors.As(err, &refused):
				broken++
			default:
				end = err
			}
		}
		_, again := r.Next(t.Context())
		if lines != tc.lines || broken != tc.broken {
			t.Errorf("%s: %d lines read and %d broken, want %d and %d", name, lines, broken, tc.lines, tc.broken)
		}
		switch {
		case tc.past && (!errors.Is(end, errThreadBound) || !errors.Is(again, errThreadBound) ||
			!strings.Contains(end.Error(), "whole lines it is read within")):
			t.Errorf("%s: the reading ended with %v and then %v, want errThreadBound both times, past its lines",
				name, end, again)
		case !tc.past && (!errors.Is(end, io.EOF) || r.Torn() != tc.tornHere):
			t.Errorf("%s: the reading ended with %v, torn %t, want io.EOF, torn %t", name, end, r.Torn(), tc.tornHere)
		}
	}
}

// A MEMBER'S READER THAT FAILS ENDS THE READING WITH ITS ERROR, never the
// member's end: an archive cut short (io.ErrUnexpectedEOF) or a box's stream
// reset mid-member ends it with that error after the whole lines before it; the
// half record is no torn piece, and asked again the reader answers the same
// error. Only io.EOF is the end: read as one, a cut stream would pass for a
// thread whose last write tore, and what the cut took would be neither read nor
// missed. An error wrapping the scrub's bound (a reader metering a kept archive
// with the scrub's own meter) is returned in its own words, never the layer's.
//
// Mutation: read a reader's error as the end, and each reading ends with
// io.EOF; take a scrub-bound error for the layer's own, and an inflated archive
// ends as a member past its bound.
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

// A READING ENDS AT THE LINE ITS CONTEXT ENDED: Next asks its context before
// each piece a line takes, so once a caller's context has ended the next Next
// answers its error and hands no more lines, and the reading stays over,
// whatever context a later Next is handed. The lines before it were each read
// whole.
//
// Mutation: ask no context in Next, and the line after the end is read; or
// answer a context's end for its own Next alone, and a later one reads on.
func TestAReadingEndsAtTheLineItsContextEnded(t *testing.T) {
	t.Parallel()
	r := newThreadReader(strings.NewReader(ctThread(ctBrief(ctUUID(1)), ctBrief(ctUUID(2)), ctBrief(ctUUID(3)))),
		threadPlainMax)
	ctx, cancel := context.WithCancel(t.Context())
	if line, err := r.Next(ctx); err != nil || line.N != 1 {
		t.Fatalf("the first line read as %d, %v; want line 1", line.N, err)
	}
	cancel()
	for _, next := range []context.Context{ctx, t.Context()} {
		if line, err := r.Next(next); !errors.Is(err, context.Canceled) {
			t.Errorf("a Next after the reading's context ended handed line %d, %v; want %v", line.N, err,
				context.Canceled)
		}
	}
	if r.Torn() {
		t.Errorf("a reading its context ended reads as torn")
	}
}

// A HEAD IS REFUSED BY ITS KEYS' KINDS ALONE: each head key, of every kind a
// JSON value is, refuses the head with the same error whether its reading
// hands the rule the key's value or only its kind, as a reading that keeps a
// value too long to keep still knows its kind, and both say what the line's
// reading says of the same key on a user record. A boolean's value is its
// kind, so a head of kinds alone reads `isSidechain` and `replacesSpan` as the
// whole one does.
//
// Mutation: hold a string key, `parentUuid` or a boolean key to its type by
// its raw value, and a key of the wrong kind whose value was not kept reads
// as absent, or one of the right kind as of none.
func TestAHeadIsRefusedByItsKeysKindsAlone(t *testing.T) {
	t.Parallel()
	values := []string{`"x"`, `{"a":1}`, `[1]`, `true`, `false`, `null`, `1`}
	for _, key := range []string{"type", "subtype", "agentId", "parentUuid", "isSidechain", "replacesSpan"} {
		for _, value := range values {
			// keys is the head of a user record with key's value raw, kept or
			// its kind alone.
			keys := func(kept bool) headKeys {
				k := headKeys{typ: headKeyOf(json.RawMessage(`"user"`))}
				v := headKeyOf(json.RawMessage(value))
				if !kept {
					v.raw = nil
				}
				switch key {
				case "type":
					k.typ = v
				case "subtype":
					k.subtype = v
				case "agentId":
					k.agentID = v
				case "parentUuid":
					k.parent = v
				case "isSidechain":
					k.sidechain = v
				case "replacesSpan":
					k.replacesSpan = v
				}
				return k
			}
			whole, wholeErr := headOf(keys(true))
			kinds, kindsErr := headOf(keys(false))
			line := `{"type":"user","` + key + `":` + value + `}`
			if key == "type" {
				line = `{"type":` + value + `}`
			}
			_, readErr := newThreadReader(strings.NewReader(line+"\n"), threadPlainMax).Next(t.Context())
			var refused *threadLineError
			if errors.As(readErr, &refused) {
				readErr = refused.Err
			}
			if fmt.Sprint(kindsErr) != fmt.Sprint(wholeErr) || fmt.Sprint(wholeErr) != fmt.Sprint(readErr) {
				t.Errorf("%s of %s: its kind alone gives %v, its value %v, and the line's reading %v; want one "+
					"error", key, value, kindsErr, wholeErr, readErr)
				continue
			}
			if wholeErr == nil && (ctSidechain(kinds) != ctSidechain(whole) || kinds.ReplacesSpan != whole.ReplacesSpan) {
				t.Errorf("%s of %s: its kind alone reads isSidechain %s and replacesSpan %t, its value %s and %t",
					key, value, ctSidechain(kinds), kinds.ReplacesSpan, ctSidechain(whole), whole.ReplacesSpan)
			}
		}
	}
}

// ctSidechain is h's `isSidechain` as text: "absent", "true" or "false".
func ctSidechain(h threadHead) string {
	if h.Sidechain == nil {
		return "absent"
	}
	return fmt.Sprint(*h.Sidechain)
}

// ctRefusal is a line the layer reads as no record, and how Claude Code's load
// reads it: its fault, and the kind of record its refused head names.
type ctRefusal struct {
	line  string
	fault lineFault
	kind  recordKind
}

// ctBrokenLines is the lines that are not one record, or one whose head key
// is of the wrong type, as [TestEveryLineIsReadToItsEndBeforeAnythingReadsIt]
// reads them among records, by name.
func ctBrokenLines() map[string]ctRefusal {
	brief := ctBrief(ctUUID(1))
	summary := ctSummary(ctUUID(1), ctUUID(2))
	prompt := ctSubagentPrompt(ctUUID(3))
	return map[string]ctRefusal{
		"two values":                         {ctDequeue + ctDequeue, faultTrailing, ""},
		"two values a carriage return parts": {ctDequeue + "\r" + ctDequeue, faultTrailing, ""},
		"a value cut short":                  {brief[:len(brief)/2], faultPrefix, ""},
		"a numeric type": {strings.Replace(ctDequeue, `"type":"queue-operation"`, `"type":7`, 1), faultHead,
			""},
		"a null type": {strings.Replace(ctDequeue, `"type":"queue-operation"`, `"type":null`, 1), faultHead,
			""},
		"a string isSidechain": {strings.Replace(brief, `"isSidechain":false`, `"isSidechain":"false"`, 1),
			faultHead, recordUser},
		"a null isSidechain": {strings.Replace(brief, `"isSidechain":false`, `"isSidechain":null`, 1), faultHead,
			recordUser},
		"a string replacesSpan": {strings.Replace(brief, `"isSidechain":false`,
			`"isSidechain":false,"replacesSpan":"true"`, 1), faultHead, recordUser},
		"a null replacesSpan": {strings.Replace(brief, `"isSidechain":false`,
			`"isSidechain":false,"replacesSpan":null`, 1), faultHead, recordUser},
		"a numeric parentUuid": {strings.Replace(brief, `"parentUuid":null`, `"parentUuid":7`, 1), faultHead,
			recordUser},
		"a parentUuid that is an object": {strings.Replace(brief, `"parentUuid":null`, `"parentUuid":{}`, 1),
			faultHead, recordUser},
		"a boolean subtype": {strings.Replace(summary, `"subtype":"stop_hook_summary"`, `"subtype":true`, 1),
			faultHead, recordSystem},
		"a numeric agentId": {strings.Replace(prompt, `"agentId":"`+ctAgent+`"`, `"agentId":7`, 1), faultHead,
			recordUser},
		"null":                                   {"null", faultNotObject, ""},
		"a list holding a record":                {"[" + ctDequeue + "]", faultNotObject, ""},
		"a string":                               {`"queue-operation"`, faultNotObject, ""},
		"nothing at all":                         {"", faultSyntax, ""},
		"NULs before a break":                    {"\x00\x00\x00", faultLead, ""},
		"a record behind a byte that is no JSON": {"\x00" + ctDequeue, faultLead, ""},
	}
}

// EVERY LINE IS READ TO ITS END BEFORE ANYTHING READS IT: a line that is not
// one record (two values, one cut short, a non-object) or whose head key is of
// the wrong type (`type`, `subtype`, `agentId` strings; `parentUuid` null or a
// string; `isSidechain` a boolean; a user record's `replacesSpan` a boolean) is
// a *threadLineError naming its line, whatever keywords it holds, and the
// reader reads on. None holds a key a reader of hooks or calls looks for. Each
// error says how Claude Code's load reads the line, the kind a refused head's
// type names where the load reads it, and the bytes it takes, break included.
// Mutation: read only lines naming a hook or call, and every broken line reads
// as a record; read replacesSpan by text alone; read every refused line as one
// yielding nothing; or hold a refused type off the kinds the load reads, and a
// numeric type names one.
func TestEveryLineIsReadToItsEndBeforeAnythingReadsIt(t *testing.T) {
	t.Parallel()
	brief := ctBrief(ctUUID(1))
	summary := ctSummary(ctUUID(1), ctUUID(2))
	prompt := ctSubagentPrompt(ctUUID(3))
	broken := ctBrokenLines()
	names := slices.Sorted(maps.Keys(broken))
	lines := make([]string, 0, 2*len(names)+3)
	for _, name := range names {
		lines = append(lines, ctDequeue, broken[name].line)
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
		if want := broken[name]; got.broken[i].Fault != want.fault || got.broken[i].Type != want.kind ||
			got.broken[i].Size != len(want.line)+1 {
			t.Errorf("%s: the load reads it as %q, a record of the kind %q, of %d bytes; want %q, %q and %d", name,
				got.broken[i].Fault, got.broken[i].Type, got.broken[i].Size, want.fault, want.kind, len(want.line)+1)
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

// ctDeep is a user record whose one key holds lists nested to levels in all.
func ctDeep(levels int) string {
	return `{"type":"user","x":` + strings.Repeat("[", levels-1) + strings.Repeat("]", levels-1) + `}`
}

// ctRefusedLines is a line of every way a line is no record, as
// [TestALineThatIsNoRecordSaysHowTheLoadReadsIt] reads each alone, by name.
func ctRefusedLines(t *testing.T) map[string]ctRefusal {
	t.Helper()
	brief := ctBrief(ctUUID(1))
	return map[string]ctRefusal{
		"a record a later write tore into":       {brief[:60] + ctDequeue, faultSyntax, ""},
		"white space alone":                      {" \t ", faultSyntax, ""},
		"a list of numbers":                      {"[1,2]", faultNotObject, ""},
		"a list ended by a carriage return":      {"[1,2]\r", faultNotObject, ""},
		"a queue operation of a numeric agentId": {`{"type":"queue-operation","agentId":5}`, faultHead, ""},
		"a summary of a string isSidechain": {`{"type":"summary","isSidechain":"no","leafUuid":"` + ctUUID(5) + `"}`,
			faultHead, ""},
		"a checkpoint of a numeric agentId": {ctReplace(t, ctLastPrompt, `"type":"last-prompt"`,
			`"type":"last-prompt","agentId":5`), faultHead, recordLastPrompt},
		"a progress record of a null isSidechain": {`{"type":"progress","isSidechain":null,"uuid":"` + ctUUID(6) +
			`"}`, faultHead, recordProgress},
		"a fork's reference of a numeric subtype": {`{"type":"fork-context-ref","subtype":1}`, faultHead,
			recordForkContextRef},
		"a content replacement of a numeric agentId": {ctReplace(t, ctReplacement(ctAgent, ctReplaced(ctCall)),
			`"agentId":"`+ctAgent+`"`, `"agentId":5`), faultHead, recordContentReplacement},
		"a reply of a string isSidechain": {ctReplace(t, ctAssistant(ctUUID(3), ctUUID(4), ctUseContent),
			`"isSidechain":false`, `"isSidechain":"false"`), faultHead, recordAssistant},
		"an attachment of a numeric parentUuid": {ctReplace(t, ctAttachment(ctUUID(3), ctUUID(4), ctTokens,
			ctTokensRendered), `"parentUuid":"`+ctUUID(3)+`"`, `"parentUuid":3`), faultHead, recordAttachment},
		"a value cut at the break":           {brief[:len(brief)-9], faultPrefix, ""},
		"a record followed by a number":      {ctDequeue + " 7", faultTrailing, ""},
		"a record followed by a record":      {ctDequeue + ctDequeue, faultTrailing, ""},
		"a record led by a NUL":              {"\x00" + ctDequeue, faultLead, ""},
		"a record led by a byte order mark":  {"\xef\xbb\xbf" + ctDequeue, faultLead, ""},
		"a record nested 10,001 levels deep": {ctDeep(10001), faultDepth, ""},
	}
}

// A LINE THAT IS NO RECORD SAYS HOW THE LOAD READS IT: faultSyntax (JSON fails
// before any whole value) yields nothing; faultNotObject is passed over;
// faultHead (mistyped head key) names the kind when its type is one of the
// eight the load reads, else none; faultPrefix (a value cut at the break) the
// load reads on past; faultTrailing (a whole value then more) it keeps;
// faultLead (NUL or byte order mark); faultDepth (past the decoder's 10,000
// levels; 10,000 deep reads as a record). Each says its bytes, break and a CR
// before it included, and hands its reader them minus the break
// ([threadBytes]). NO LINE IS READ TWICE OR HELD: five faults past a MiB cost
// at most 16 KiB more than a KiB twin. Not parallel: the allocation count is
// the process's. Mutation: misread a fault, skip a check or copy pieces, and it fails.
func TestALineThatIsNoRecordSaysHowTheLoadReadsIt(t *testing.T) {
	for name, tc := range ctRefusedLines(t) {
		got := ctRead(ctThread(tc.line), threadPlainMax)
		if len(got.lines) != 0 || len(got.broken) != 1 {
			t.Errorf("%s reads as %d records and %d lines that are none; want one that is none", name,
				len(got.lines), len(got.broken))
			continue
		}
		if e := got.broken[0]; e.Fault != tc.fault || e.Type != tc.kind || e.Size != len(tc.line)+1 ||
			!e.Fault.Valid() || (e.Type != "") != e.Type.Valid() {
			t.Errorf("%s: the load reads it as %q, a record of the kind %q, of %d bytes; want %q, %q and %d", name,
				e.Fault, e.Type, e.Size, tc.fault, tc.kind, len(tc.line)+1)
		}
		if e := got.broken[0]; e.Bytes != wholeLineBytes([]byte(tc.line)) {
			t.Errorf("%s hands its reader the byte facts of %d bytes of it, headed %q; want the line's own %d, its "+
				"break dropped", name, e.Bytes.Len(), e.Bytes.Head(), len(tc.line))
		}
	}
	if got := ctRead(ctThread(ctDeep(10000)), threadPlainMax); len(got.lines) != 1 || len(got.broken) != 0 {
		t.Errorf("a record nested 10,000 levels deep reads as %d records and %d lines that are none; want one record",
			len(got.lines), len(got.broken))
	}
	if lineFault("").Valid() || recordKind("").Valid() || recordKind("queue-operation").Valid() {
		t.Error("the zero fault, the zero kind or a kind the load keeps no record of reads as valid")
	}

	ctx := t.Context()
	// warm is a record, which a reader is warmed on before each measure.
	warm := scanLine(scanLit(`{"type":"user","x":"` + strings.Repeat("x", 1<<10) + `"}`))
	for fault, tc := range map[lineFault]struct{ head, tail string }{
		faultSyntax:    {`{"type":"user","x":"`, `",]`},
		faultTrailing:  {`{"type":"user","x":"`, `"} x`},
		faultPrefix:    {`{"type":"user","x":"`, ``},
		faultHead:      {`{"type":"user","x":"`, `","isSidechain":"no"}`},
		faultNotObject: {`["`, `"]`},
	} {
		line := func(n int) []scanPart { return scanLine(scanLit(tc.head), scanRun('x', n), scanLit(tc.tail)) }
		twinCost, _, twinErr := scanAllocated(ctx, warm, line(1<<10), 3)
		cost, _, err := scanAllocated(ctx, warm, line(1<<20), 2)
		var refused, twinRefused *threadLineError
		if !errors.As(err, &refused) || !errors.As(twinErr, &twinRefused) || refused.Fault != fault ||
			twinRefused.Fault != fault {
			t.Fatalf("a line refused as %s reads as %v, its twin as %v; want both refused so", fault, err, twinErr)
		}
		if cost > twinCost+16<<10 {
			t.Errorf("a line of a MiB refused as %s costs its reader %d bytes, and its twin of a KiB %d: more than "+
				"16 KiB more", fault, cost, twinCost)
		}
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

// A KEY ONLY ONE READER READS COSTS THAT READER ALONE: a key of the wrong type
// leaves the rest of the record whole and is refused only by its own accessor,
// never read as a zero value: attachment (AttachmentType), origin (OriginKind),
// a reply's stop_reason, null included (StopReason), edit report
// (ToolResultsCleared), usage (Usage), API-error mark (APIError), leafUuid
// (Checkpoint, LeafKeyed), compaction metadata (Preserved), retractions
// (Retracted), replacements (ReplacedResults, ReplacementCount), timestamp,
// meta mark, hook event (HookEvent), a record's uuid (UUID). A key two readers
// read is refused by both: model (Usage, Model), id (Usage, MessageID).
// Mutation: hold such a key in the head, Message or Usage's decode, or read
// null stop_reason as none, and a reshaped record is no record or reads wrong.
func TestAKeyOneReaderReadsCostsThatReaderAlone(t *testing.T) {
	t.Parallel()
	attachment := ctLine(t, ctAttachment(ctUUID(1), ctUUID(2), `"hook_success"`, ""))
	if h := attachment.Head; h.Type != recordAttachment || !h.Chained() || !h.Main() {
		t.Errorf("a record whose attachment is no object reads with the head %+v", h)
	}
	if m, err := attachment.Message(); err != nil || m.IsText() || m.Blocks().Len() != 0 || m.Calls() != nil {
		t.Errorf("a record whose attachment is no object reads its message as %+v, %v", m, err)
	}
	if kind, err := attachment.OriginKind(); kind != "" || err != nil {
		t.Errorf("a record whose attachment is no object reads its origin as %q, %v", kind, err)
	}
	if kind, err := attachment.AttachmentType(); err == nil {
		t.Errorf("an attachment that is no object reads as one of the type %q, never as one it cannot read", kind)
	}
	token := claudeTakeToken(ctProbeStopTake)
	said := ctReplace(t, ctBatchSystem, `"content":"crewlet: a note reached the run"`,
		`"content":"crewlet: a note reached the run `+token+`"`)
	for name, event := range map[string]string{"a number": `7`, "null": `null`, "an object": `{}`} {
		line := ctAsked(t, ctAttachment(ctUUID(1), ctUUID(2), ctReplace(t, said, `"hookEvent":"PostToolBatch"`,
			`"hookEvent":`+event), ""), []string{token})
		if got, err := line.HookEvent(); err == nil {
			t.Errorf("a hook whose event is %s reads the event %q, want an error", name, got)
		}
		kind, kindErr := line.AttachmentType()
		firing, keyed := line.AttachmentToolUseID()
		content, stdout := line.HookTakes()
		formed, formedErr := line.AttachmentWellFormed()
		if kind != hookSystemMessage || kindErr != nil || !keyed || firing != sha256.Sum256([]byte(ctBatchFire)) ||
			content != 1 || stdout != 0 || !formed || formedErr != nil || !line.Head.Main() {
			t.Errorf("a hook whose event is %s reads as a %q record, %v, keyed %t, naming the takes %b and %b, kept "+
				"%t, %v; want its system message, its firing and its take whole", name, kind, kindErr, keyed, content,
				stdout, formed, formedErr)
		}
	}
	if leaf, marked, err := attachment.Checkpoint(); leaf != "" || marked || err != nil {
		t.Errorf("a record whose attachment is no object reads as the checkpoint %q (%t), %v", leaf, marked, err)
	}
	for name, leaf := range map[string]string{"a number": `7`, "an object": `{"uuid":"` + ctUUID(5) + `"}`,
		"a boolean": `true`, "a list": `["` + ctUUID(5) + `"]`} {
		line := ctLine(t, ctReplace(t, ctLastPrompt, `"leafUuid":"`+ctUUID(5)+`"`, `"leafUuid":`+leaf))
		if got, marked, err := line.Checkpoint(); err == nil {
			t.Errorf("a checkpoint whose leafUuid is %s names %q (%t), want an error", name, got, marked)
		}
		if keyed, err := line.LeafKeyed(); err == nil {
			t.Errorf("a checkpoint whose leafUuid is %s reads as keyed %t, want an error", name, keyed)
		}
		if h := line.Head; h.Type != recordLastPrompt || h.Chained() {
			t.Errorf("a checkpoint whose leafUuid is %s reads with the head %+v", name, h)
		}
	}
	for name, metadata := range map[string]string{"a string": `"kept"`, "a list": `[]`, "a number": `7`} {
		line := ctLine(t, ctReplace(t, ctBoundary(ctUUID(5), ctUUID(6), ctUUID(3)), `"compactMetadata":{`,
			`"compactMetadata":`+metadata+`,"was":{`))
		if p, err := line.Preserved(); err == nil {
			t.Errorf("a boundary whose compactMetadata is %s keeps %+v, want an error", name, p)
		}
		if uuid, err := line.UUID(); uuid != ctUUID(5) || err != nil ||
			line.Head.Subtype != subtypeCompactBoundary || !line.Head.Parentless() {
			t.Errorf("a boundary whose compactMetadata is %s reads with the head %+v and the uuid %q, %v", name,
				line.Head, uuid, err)
		}
	}
	for name, list := range map[string]string{"a string": `"` + ctUUID(4) + `"`, "an object": `{}`, "null": `null`} {
		line := ctLine(t, ctReplace(t, ctRetry(ctUUID(5), ctUUID(6), ctUUID(4)),
			`"retractedMessageUuids":["`+ctUUID(4)+`"]`, `"retractedMessageUuids":`+list))
		if r, err := line.Retracted(); err == nil {
			t.Errorf("a retry whose retractedMessageUuids is %s retracts %+v with no error; want one", name, r)
		}
		if m, err := line.Message(); err != nil || line.Head.Type != recordUser || !line.Head.Main() {
			t.Errorf("a retry whose retractedMessageUuids is %s reads with the head %+v and the message %+v, %v",
				name, line.Head, m, err)
		}
	}
	for name, list := range map[string]string{"a string": `"` + ctCall + `"`, "an object": `{}`, "null": `null`} {
		line := ctLine(t, ctReplace(t, ctReplacement(ctAgent, ctReplaced(ctCall)), `"replacements":[`+
			ctReplaced(ctCall)+`]`, `"replacements":`+list))
		if err := line.ReplacedResults(func(string) {}); err == nil {
			t.Errorf("a record whose replacements are %s replaces results with no error; want one", name)
		}
		if n, err := line.ReplacementCount(); err == nil {
			t.Errorf("a record whose replacements are %s lists %d with no error; want one", name, n)
		}
		if h := line.Head; h.Type != recordContentReplacement || h.AgentID != ctAgent || h.Chained() {
			t.Errorf("a record whose replacements are %s reads with the head %+v", name, h)
		}
	}
	reply := ctAssistant(ctUUID(3), ctUUID(4), ctUseContent)
	result := ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)
	for name, stamp := range map[string]string{"a number": `1767323045000`, "null": `null`,
		"an offset in place of the Z": `"2026-01-02T03:04:05.000+00:00"`} {
		for kind, record := range map[string]string{"a reply": reply, "a result": result} {
			line := ctLine(t, ctReplace(t, record, `"timestamp":"`+ctStamp+`"`, `"timestamp":`+stamp))
			if ms, present, err := line.Timestamp(); err == nil {
				t.Errorf("%s whose timestamp is %s was written at %d (%t), want an error", kind, name, ms, present)
			}
			if m, err := line.Message(); err != nil || len(m.Calls()) != 1 || !line.Head.Main() {
				t.Errorf("%s whose timestamp is %s reads with the head %+v and the calls %+v, %v", kind, name,
					line.Head, m.Calls(), err)
			}
		}
	}
	for name, mark := range map[string]string{"a string": `"true"`, "a number": `1`, "null": `null`} {
		line := ctLine(t, ctMarked(t, result, "isMeta", mark))
		if meta, err := line.IsMeta(); err == nil {
			t.Errorf("a result whose isMeta is %s reads as meta %t, want an error", name, meta)
		}
		if m, err := line.Message(); err != nil || len(m.Calls()) != 1 || !line.Head.Main() {
			t.Errorf("a result whose isMeta is %s reads with the head %+v and the calls %+v, %v", name, line.Head,
				m.Calls(), err)
		}
	}
	for name, source := range map[string]string{"a number": `7`, "null": `null`, "an object": `{}`} {
		line := ctLine(t, ctReplace(t, result, `"sourceToolAssistantUUID":"`+ctUUID(4)+`"`,
			`"sourceToolAssistantUUID":`+source))
		if got, ok := line.SourceToolAssistantUUID(); ok {
			t.Errorf("a result whose source is %s names %x", name, got)
		}
		if m, err := line.Message(); err != nil || len(m.Calls()) != 1 || !line.Head.Main() {
			t.Errorf("a result whose source is %s reads with the head %+v and the calls %+v, %v", name, line.Head,
				m.Calls(), err)
		}
	}
	for name, tc := range map[string]struct {
		record, key string
		read        func(threadLine) error
	}{
		"a result's isVirtual": {record: result, key: "isVirtual",
			read: func(l threadLine) error { _, err := l.IsVirtual(); return err }},
		"a reply's isVirtual": {record: reply, key: "isVirtual",
			read: func(l threadLine) error { _, err := l.IsVirtual(); return err }},
		"a result's interruptedByShutdown": {record: result, key: "interruptedByShutdown",
			read: func(l threadLine) error { _, err := l.InterruptedByShutdown(); return err }},
		"a result's isCompactSummary": {record: result, key: "isCompactSummary",
			read: func(l threadLine) error { _, err := l.IsCompactSummary(); return err }},
		"a reply's batchToolUses": {record: reply, key: "batchToolUses",
			read: func(l threadLine) error { _, err := l.BatchToolUses(func(string) {}); return err }},
	} {
		for kind, value := range map[string]string{"a string": `"true"`, "null": `null`, "a number": `1`} {
			line := ctLine(t, ctMarked(t, tc.record, tc.key, value))
			if err := tc.read(line); err == nil {
				t.Errorf("%s that is %s reads with no error; want one", name, kind)
			}
			if m, err := line.Message(); err != nil || len(m.Calls()) != 1 || m.Calls()[0].ID != ctCall ||
				!line.Head.Main() || line.TaskDelivery() {
				t.Errorf("%s that is %s reads with the head %+v and the calls %+v, %v", name, kind, line.Head,
					m.Calls(), err)
			}
			usage, ok, err := line.Usage()
			if line.Head.Type == recordAssistant && (!ok || err != nil || usage.Input != 100) {
				t.Errorf("%s that is %s says its request took %+v (%t), %v; want its usage", name, kind, usage, ok, err)
			}
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
		if usage, ok, err := line.Usage(); !ok || err != nil || usage.ID != "msg_test" || usage.Input != 100 {
			t.Errorf("a reply whose stop_reason is %s says its request took %+v (%t), %v; want its usage", name,
				usage, ok, err)
		}
		if model, err := line.Model(); model != "claude-sonnet-4-5" || err != nil {
			t.Errorf("a reply whose stop_reason is %s was written by %q, %v; want its model", name, model, err)
		}
		if id, err := line.MessageID(); id != "msg_test" || err != nil {
			t.Errorf("a reply whose stop_reason is %s is the reply %q, %v; want its id", name, id, err)
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
		if model, err := line.Model(); model != "claude-sonnet-4-5" || err != nil {
			t.Errorf("a reply whose report is %s was written by %q, %v; want its model", name, model, err)
		}
		if id, err := line.MessageID(); id != "msg_test" || err != nil {
			t.Errorf("a reply whose report is %s is the reply %q, %v; want its id", name, id, err)
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
		if model, err := line.Model(); model != "claude-sonnet-4-5" || err != nil {
			t.Errorf("a reply whose usage is %s was written by %q, %v; want its model", name, model, err)
		}
		if id, err := line.MessageID(); id != "msg_test" || err != nil {
			t.Errorf("a reply whose usage is %s is the reply %q, %v; want its id", name, id, err)
		}
	}
	for name, tc := range map[string]struct {
		from, to string
		// model is whether the key is the model's, which Model reads too; id
		// whether it is the reply's id, which MessageID reads too.
		model, id bool
	}{
		"an id that is a number": {from: `"id":"msg_test"`, to: `"id":7`, id: true},
		"a model that is null":   {from: `"model":"claude-sonnet-4-5"`, to: `"model":null`, model: true},
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
		if model, err := line.Model(); (err != nil) != tc.model || (!tc.model && model != "claude-sonnet-4-5") {
			t.Errorf("a reply whose message holds %s was written by %q, %v; want it refused %t", name, model, err,
				tc.model)
		}
		if id, err := line.MessageID(); (err != nil) != tc.id || (!tc.id && id != "msg_test") {
			t.Errorf("a reply whose message holds %s is the reply %q, %v; want it refused %t", name, id, err, tc.id)
		}
	}
	synthetic := ctSynthetic(ctUUID(30), ctUUID(31))
	for name, mark := range map[string]string{"null": `null`, "a string": `"true"`, "a number": `1`} {
		line := ctLine(t, ctReplace(t, synthetic, `"isApiErrorMessage":false`, `"isApiErrorMessage":`+mark))
		if failed, err := line.APIError(); err == nil {
			t.Errorf("a reply whose mark of an API error is %s reads as a failed request's %t, want an error", name,
				failed)
		}
		if m, err := line.Message(); err != nil || ctBlocks(m.Blocks()) != "1 [text]" {
			t.Errorf("a reply whose mark is %s reads the blocks %s, %v; want its text", name, ctBlocks(m.Blocks()),
				err)
		}
		if reason, err := line.StopReason(); reason != "stop_sequence" || err != nil {
			t.Errorf("a reply whose mark is %s stopped for %q, %v; want stop_sequence", name, reason, err)
		}
		if model, err := line.Model(); model != "<synthetic>" || err != nil {
			t.Errorf("a reply whose mark is %s was written by %q, %v; want <synthetic>", name, model, err)
		}
		if usage, ok, err := line.Usage(); !ok || err != nil || usage.Model != "<synthetic>" {
			t.Errorf("a reply whose mark is %s says its request took %+v (%t), %v; want its usage", name, usage, ok,
				err)
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
	} else if agent, leg, ok := m.Notification(); !m.IsText() || !ok || agent != ctAgent || leg != "toolu_test_agent" {
		t.Errorf("a notification whose origin is no object reads its message as a text %t naming %q's leg %q (%t)",
			m.IsText(), agent, leg, ok)
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

	answer := `{"status":"completed","agentId":"` + ctAgent + `","handback":"withheld"}`
	assistant := strings.Replace(ctAssistant(ctUUID(3), ctUUID(4), ctUseContent), `"apiBlockIndex"`,
		`"toolUseResult":`+answer+`,"apiBlockIndex"`, 1)
	for name, tc := range map[string]struct {
		record  string
		answers bool
	}{
		"a result's that answers a leg":         {record: ctResult(ctUUID(4), ctUUID(5), ctResultBody, answer), answers: true},
		"a command's result's, its output":      {record: ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)},
		"an MCP result's, a list of its blocks": {record: ctResult(ctUUID(9), ctUUID(10), ctMCPResult, ctMCPBlocks)},
		"an assistant's":                        {record: assistant},
		"a brief's, which has none":             {record: ctBrief(ctUUID(1))},
		"an attachment's":                       {record: ctAttachment(ctUUID(1), ctUUID(2), ctTokens, ctTokensRendered)},
		"a request blob's, which has none":      {record: ctRequestBlob},
	} {
		line := ctLine(t, tc.record)
		completed, agent, refused := line.AgentAnswer()
		if handback := line.HandsBack(); completed != tc.answers || (agent == ctAgent) != tc.answers ||
			(agent != "" && agent != ctAgent) || refused || handback != tc.answers {
			t.Errorf("%s toolUseResult answers completed %t, agent %q, refused %t, a handback %t; want an answer "+
				"%t: only a user record's answers", name, completed, agent, refused, handback, tc.answers)
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
// head, uuid, message and calls, origin kind, toolUseResult answer, cleared
// tool results, stop reason, API-error mark, model, reply id, usage, what a
// compaction kept, the resume leaf, attachment type, retractions, replaced
// results, its virtual, shutdown, summary, task-delivery and batch marks, the
// batch's split calls, time, meta mark, results' source reply, attachment's
// call key, whether its message says anything, whether a resume keeps it, its
// hook's event and takes, and the writing release, each beside the error it
// answered. A raw value is printed by [ctRaw], so absent and null never alike.
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
	m, err := l.Message()
	ctMessage(&b, m, err)
	kind, err := l.OriginKind()
	fmt.Fprintf(&b, "origin: kind %q; %v\n", kind, err)
	ctLegAnswer(&b, l)
	ctFacts(&b, l, m)
	return b.String()
}

// ctMessage writes to b what a message m, read with err, hands out: whether it
// is one text, the subagent and the leg a task notification's text names,
// whether it names the handback tool, its blocks ([ctBlocks]), and its calls.
func ctMessage(b *strings.Builder, m threadMessage, err error) {
	agent, leg, notice := m.Notification()
	fmt.Fprintf(b, "message: a text %t, a notification %t of %q's leg %q, naming the handback %t, blocks %s; %v\n",
		m.IsText(), notice, agent, leg, m.NamesHandback(), ctBlocks(m.Blocks()), err)
	for _, c := range m.Calls() {
		ctCallFacts(b, c)
	}
}

// ctBlocks is what blocks hand out, as text: how many there are, and each
// kind among them, the ones the layer names in its order and then "other" for
// any it does not: "2 [tool_use text]".
func ctBlocks(blocks threadBlocks) string {
	var kinds []string
	for _, k := range []struct {
		member blockSet
		name   string
	}{{blockSetToolUse, blockToolUse}, {blockSetToolResult, blockToolResult}, {blockSetText, blockText},
		{blockSetThinking, blockThinking}, {blockSetRedactedThinking, blockRedactedThinking},
		{blockOther, "other"}} {
		if blocks.seen&k.member != 0 {
			kinds = append(kinds, k.name)
		}
	}
	return fmt.Sprintf("%d [%s]", blocks.Len(), strings.Join(kinds, " "))
}

// ctCallFacts writes to b everything a call hands out: its kind, its id, its
// tool's name, the subagent it sends to, whether its result failed, and its
// result's text as its length and digests.
func ctCallFacts(b *strings.Builder, c threadCall) {
	fmt.Fprintf(b, "call %q: id %q, name %q, sends to %q, failed %t, its result's text %d bytes, opening %x, "+
		"whole %x\n", c.Kind, c.ID, c.Name, c.To, c.Failed, c.Text.Bytes(), c.Text.Opening, c.Text.Whole)
}

// ctLegAnswer writes to b what a line's own toolUseResult says: what it answers
// of a subagent's leg, and whether it carries a handback.
func ctLegAnswer(b *strings.Builder, l threadLine) {
	completed, agent, refused := l.AgentAnswer()
	fmt.Fprintf(b, "its answer: completed %t, agent %q, refused %t, a handback %t\n", completed, agent, refused,
		l.HandsBack())
}

// ctFacts writes to b everything [ctAllButUUID] prints after a line's answer
// ([ctLegAnswer]), its message m among it.
func ctFacts(b *strings.Builder, l threadLine, m threadMessage) {
	cleared, err := l.ToolResultsCleared()
	fmt.Fprintf(b, "tool results cleared: %t; %v\n", cleared, err)
	reason, err := l.StopReason()
	fmt.Fprintf(b, "stop reason: %q; %v\n", reason, err)
	failed, err := l.APIError()
	fmt.Fprintf(b, "a failed request's reply: %t; %v\n", failed, err)
	model, err := l.Model()
	fmt.Fprintf(b, "model: %q; %v\n", model, err)
	reply, err := l.MessageID()
	fmt.Fprintf(b, "reply: %q; %v\n", reply, err)
	usage, used, err := l.Usage()
	fmt.Fprintf(b, "usage %t: id %q, model %q, input %d, cache writes %d, cache reads %d, output %d; %v\n", used,
		usage.ID, usage.Model, usage.Input, usage.CacheWrite, usage.CacheRead, usage.Output, err)
	preserved, err := l.Preserved()
	fmt.Fprintf(b, "preserved: %+v; %v\n", preserved, err)
	leaf, marked, err := l.Checkpoint()
	fmt.Fprintf(b, "checkpoint: leaf %q, marked %t; %v\n", leaf, marked, err)
	keyed, err := l.LeafKeyed()
	fmt.Fprintf(b, "leaf keyed: %t; %v\n", keyed, err)
	attached, err := l.AttachmentType()
	fmt.Fprintf(b, "attachment type: %q; %v\n", attached, err)
	retracted, err := l.Retracted()
	prefixes, all := retracted.Prefixes()
	fmt.Fprintf(b, "retracted: %d entries, the prefixes %x, all %t; %v\n", retracted.Entries, prefixes, all, err)
	var replaced []string
	err = l.ReplacedResults(func(id string) { replaced = append(replaced, id) })
	fmt.Fprintf(b, "replaced results: %q; %v\n", strings.Join(replaced, " "), err)
	replacements, err := l.ReplacementCount()
	fmt.Fprintf(b, "replacements listed: %d; %v\n", replacements, err)
	virtual, err := l.IsVirtual()
	fmt.Fprintf(b, "virtual: %t; %v\n", virtual, err)
	shutdown, err := l.InterruptedByShutdown()
	fmt.Fprintf(b, "interrupted by a shutdown: %t; %v\n", shutdown, err)
	summary, err := l.IsCompactSummary()
	fmt.Fprintf(b, "a compaction's summary: %t; %v\n", summary, err)
	fmt.Fprintf(b, "a task's delivery: %t\n", l.TaskDelivery())
	var batch []string
	batched, err := l.BatchToolUses(func(id string) { batch = append(batch, id) })
	fmt.Fprintf(b, "a batch's reply: %t, splitting %q; %v\n", batched, strings.Join(batch, " "), err)
	ms, stamped, err := l.Timestamp()
	fmt.Fprintf(b, "written %t at %d; %v\n", stamped, ms, err)
	meta, err := l.IsMeta()
	fmt.Fprintf(b, "meta: %t; %v\n", meta, err)
	source, sourced := l.SourceToolAssistantUUID()
	fmt.Fprintf(b, "source %t: %x\n", sourced, source)
	call, isCall := l.AttachmentToolUseID()
	fmt.Fprintf(b, "followed %t: %x\n", isCall, call)
	saysText, saysAny := m.Says()
	fmt.Fprintf(b, "says text %t, anything %t\n", saysText, saysAny)
	formed, err := l.AttachmentWellFormed()
	fmt.Fprintf(b, "kept as well formed: %t; %v\n", formed, err)
	event, err := l.HookEvent()
	content, stdout := l.HookTakes()
	fmt.Fprintf(b, "a hook's event %q, naming the takes %x in its content and %x in its stdout; %v\n", event, content,
		stdout, err)
	release, err := l.Version()
	fmt.Fprintf(b, "written by %q; %v\n", release, err)
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

// A KEY IS READ EXACTLY AS WRITTEN, as Claude Code's JSON.parse reads its own
// records: a key differing from one the layer reads only in case is another
// key, and alone leaves that one absent; a key written twice holds its last
// value. Every object the layer reads is held so: the head, the keys one
// reader reads, hook attachments, origins, messages, blocks, usage and its
// iterations, the context-edit report, compaction metadata, replacements and
// batch entries. So a line hands out exactly what it would without the other
// key, through every accessor. 2.1.322 already spells a call's id `toolUseId`.
//
// Mutation: take a case-only key for the layer's own, as encoding/json's
// struct decode does, and records read as the other key says; refuse a key
// written twice, and a record that writes one twice is no record.
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
	synthetic := ctSynthetic(ctUUID(30), ctUUID(31))
	compacting := ctBoundary(ctUUID(10), ctUUID(11), ctUUID(3), ctUUID(4))
	segmenting := ctReplace(t, compacting, ctListed(ctUUID(11), ctUUID(3), ctUUID(4)), "")
	retracting := ctFallback(ctUUID(5), ctUUID(6), ctUUID(2))
	replacing := ctReplacement("", ctReplaced(ctCall))
	batching := ctReplace(t, call, `"apiBlockIndex"`, `"batchToolUses":[{"id":"`+ctCall+`","name":"Bash"}],"apiBlockIndex"`)
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
		"a version, then Version": {record: brief, from: `"version":"2.1.310"`,
			to: `"version":"2.1.310","Version":"2.1.118"`},
		"Version alone": {record: ctDequeue, from: `"type":"queue-operation"`,
			to: `"type":"queue-operation","Version":"2.1.310"`},
		"a version written twice": {record: brief, from: `"version":"2.1.310"`,
			to: `"version":"2.1.118","version":"2.1.310"`},
		// The keys only one reader reads.
		"a message, then Message": {record: result, from: `"uuid":"` + ctUUID(5) + `"`,
			to: `"Message":{"role":"user","content":[{"tool_use_id":"` + other + `","type":"tool_result",` +
				`"content":"other"}]},"uuid":"` + ctUUID(5) + `"`},
		"an attachment, then Attachment": {record: hook, from: `,"type":"attachment"`,
			to: `,"Attachment":` + strings.Replace(ctBatchSuccess, ctBatchFire, other, 1) + `,"type":"attachment"`},
		"an origin, then Origin": {record: notification, from: ctNotificationOrigin,
			to: ctNotificationOrigin + `,"Origin":{"kind":"coordinator"}`},
		"a toolUseResult, then ToolUseResult": {record: result, from: `"toolUseResult":` + ctResultOwn,
			to: `"toolUseResult":` + ctResultOwn + `,"ToolUseResult":{"stdout":"other"}`},
		"ToolUseResult null alone": {record: brief, from: `"isSidechain":false`,
			to: `"isSidechain":false,"ToolUseResult":null`},
		"a mark of an API error, then IsApiErrorMessage": {record: synthetic, from: `"isApiErrorMessage":false`,
			to: `"isApiErrorMessage":false,"IsApiErrorMessage":true`},
		"IsApiErrorMessage alone": {record: call, from: `"type":"assistant"`,
			to: `"IsApiErrorMessage":true,"type":"assistant"`},
		"a checkpoint's leafUuid, then LeafUuid": {record: ctLastPrompt, from: `"leafUuid":"` + ctUUID(5) + `"`,
			to: `"leafUuid":"` + ctUUID(5) + `","LeafUuid":"` + ctUUID(9) + `"`},
		"a checkpoint's LEAFUUID alone": {record: ctReplace(t, ctLastPrompt, `"leafUuid":"`+ctUUID(5)+`",`, ""),
			from: `"type":"last-prompt"`, to: `"type":"last-prompt","LEAFUUID":"` + ctUUID(9) + `"`},
		"a checkpoint's Explicit alone": {record: ctLastPrompt, from: `"type":"last-prompt"`,
			to: `"type":"last-prompt","Explicit":true`},
		"a checkpoint's Rewound alone": {record: ctLastPrompt, from: `"type":"last-prompt"`,
			to: `"type":"last-prompt","Rewound":true`},
		"a compactMetadata, then CompactMetadata": {record: compacting, from: `,"slug":"` + ctSlug + `"`,
			to: `,"CompactMetadata":{"preservedMessages":{"anchorUuid":"` + ctUUID(9) + `","uuids":["` + ctUUID(9) +
				`"]}},"slug":"` + ctSlug + `"`},
		"a preservedMessages, then PreservedMessages": {record: compacting, from: `,"postTokens":2602`,
			to: `,"PreservedMessages":{"anchorUuid":"` + ctUUID(9) + `","uuids":["` + ctUUID(9) + `"]},"postTokens":2602`},
		"a list's anchorUuid, then AnchorUuid": {record: compacting, from: `"uuids":[`,
			to: `"AnchorUuid":"` + ctUUID(9) + `","uuids":[`},
		"a list's uuids, then UUIDs": {record: compacting, from: `"allUuids":[`,
			to: `"UUIDs":["` + ctUUID(9) + `"],"allUuids":[`},
		"a preservedSegment, then PreservedSegment": {record: segmenting, from: `,"postTokens":2602`,
			to: `,"PreservedSegment":{"headUuid":"` + ctUUID(9) + `","anchorUuid":"` + ctUUID(9) + `","tailUuid":"` +
				ctUUID(9) + `"},"postTokens":2602`},
		"a segment's headUuid, then HeadUuid": {record: segmenting, from: `"anchorUuid":"` + ctUUID(11) + `","tailUuid"`,
			to: `"HeadUuid":"` + ctUUID(9) + `","anchorUuid":"` + ctUUID(11) + `","tailUuid"`},
		"a retractedMessageUuids, then RetractedMessageUuids": {record: retracting,
			from: `"retractedMessageUuids":["` + ctUUID(2) + `"]`,
			to:   `"retractedMessageUuids":["` + ctUUID(2) + `"],"RetractedMessageUuids":["` + ctUUID(9) + `"]`},
		"RETRACTEDMESSAGEUUIDS alone": {record: ctRetry(ctUUID(5), ctUUID(6)), from: `"retractedMessageUuids":[]`,
			to: `"retractedMessageUuids":[],"RETRACTEDMESSAGEUUIDS":["` + ctUUID(9) + `"]`},
		"a replacements, then Replacements": {record: replacing, from: `"replacements":[` + ctReplaced(ctCall) + `]`,
			to: `"replacements":[` + ctReplaced(ctCall) + `],"Replacements":[` + ctReplaced(other) + `]`},
		"a replacement's kind, then Kind": {record: replacing, from: `"kind":"tool-result"`,
			to: `"kind":"tool-result","Kind":"other"`},
		"a replacement's toolUseId, then ToolUseId": {record: replacing, from: `"toolUseId":"` + ctCall + `"`,
			to: `"toolUseId":"` + ctCall + `","ToolUseId":"` + other + `"`},
		"a replacement's toolUseId written twice": {record: replacing, from: `"toolUseId":"` + ctCall + `"`,
			to: `"toolUseId":"` + other + `","toolUseId":"` + ctCall + `"`},
		"IsVirtual alone": {record: call, from: `"type":"assistant"`, to: `"IsVirtual":true,"type":"assistant"`},
		"an isVirtual, then ISVIRTUAL": {record: call, from: `"type":"assistant"`,
			to: `"isVirtual":false,"ISVIRTUAL":true,"type":"assistant"`},
		"InterruptedByShutdown alone": {record: result, from: `"type":"user"`,
			to: `"InterruptedByShutdown":true,"type":"user"`},
		"an isCompactSummary, then IsCompactSummary": {record: ctCompactSummary(ctUUID(5), ctUUID(6)),
			from: `"isCompactSummary":true`, to: `"isCompactSummary":true,"IsCompactSummary":false`},
		"TaskDelivery alone": {record: result, from: `"type":"user"`, to: `"TaskDelivery":{},"type":"user"`},
		"BatchToolUses alone": {record: call, from: `"type":"assistant"`,
			to: `"BatchToolUses":[{"id":"` + ctCall + `"}],"type":"assistant"`},
		"a batch entry's id, then Id": {record: batching, from: `{"id":"` + ctCall + `","name":"Bash"}`,
			to: `{"id":"` + ctCall + `","Id":"` + other + `","name":"Bash"}`},
		"a virtual mark written twice": {record: call, from: `"type":"assistant"`,
			to: `"isVirtual":true,"isVirtual":false,"type":"assistant"`},
		"a timestamp, then Timestamp": {record: call, from: `"timestamp":"` + ctStamp + `"`,
			to: `"timestamp":"` + ctStamp + `","Timestamp":"2028-02-29T00:00:00.000Z"`},
		"TIMESTAMP alone": {record: ctReplace(t, call, `"timestamp":"`+ctStamp+`",`, ""), from: `"type":"assistant"`,
			to: `"TIMESTAMP":"` + ctStamp + `","type":"assistant"`},
		"a timestamp written twice": {record: call, from: `"timestamp":"` + ctStamp + `"`,
			to: `"timestamp":"2028-02-29T00:00:00.000Z","timestamp":"` + ctStamp + `"`},
		"an isMeta, then IsMeta": {record: ctCoordinator(ctUUID(1), ctUUID(4)), from: `"isMeta":true`,
			to: `"isMeta":true,"IsMeta":false`},
		"IsMeta alone": {record: result, from: `"type":"user"`, to: `"IsMeta":true,"type":"user"`},
		"a sourceToolAssistantUUID, then SourceToolAssistantUuid": {record: result,
			from: `"sourceToolAssistantUUID":"` + ctUUID(4) + `"`,
			to:   `"sourceToolAssistantUUID":"` + ctUUID(4) + `","SourceToolAssistantUuid":"` + ctUUID(9) + `"`},
		"a text block's text, then Text": {record: ctAssistant(ctUUID(2), ctUUID(3), `[{"type":"text","text":" "}]`),
			from: `"text":" "`, to: `"text":" ","Text":"batch 1."`},
		"a skill listing's names, then Names": {record: ctAttachment(ctUUID(1), ctUUID(2),
			`{"type":"skill_listing","names":["a"]}`, ""), from: `"names":["a"]`, to: `"names":["a"],"Names":7`},
		"a withheld memory's why, then Why": {record: ctAttachment(ctUUID(1), ctUUID(2), ctWithheld(
			`{"path":"a","why":"denied","displayPath":"a"}`), ""), from: `"why":"denied"`,
			to: `"why":"denied","Why":"no reason"`},
		// A hook's attachment.
		"an attachment's firing, then a call's toolUseId": {record: hook, from: firing,
			to: firing + `,"toolUseId":"` + other + `"`},
		"a call's toolUseId, then an attachment's firing": {record: hook, from: firing,
			to: `"toolUseId":"` + other + `",` + firing},
		"an attachment's firing spelled TOOLUSEID alone": {record: unfired, from: `"hookName":"PostToolBatch"`,
			to: `"hookName":"PostToolBatch","TOOLUSEID":"` + ctBatchFire + `"`},
		"an attachment's type, then Type": {record: hook, from: `"type":"hook_success"`,
			to: `"type":"hook_success","Type":"hook_cancelled"`},
		"an attachment's content, then Content": {record: hook, from: `"content":""`, to: `"content":"","Content":7`},
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
		"a reply's Model alone": {record: ctReplace(t, call, `"model":"claude-sonnet-4-5",`, ""),
			from: `"type":"message"`, to: `"Model":"<synthetic>","type":"message"`},
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
		"a mark of an API error written twice": {record: synthetic, from: `"isApiErrorMessage":false`,
			to: `"isApiErrorMessage":true,"isApiErrorMessage":false`},
		"a checkpoint's leafUuid written twice": {record: ctLastPrompt, from: `"leafUuid":"` + ctUUID(5) + `"`,
			to: `"leafUuid":"` + ctUUID(9) + `","leafUuid":"` + ctUUID(5) + `"`},
		"a checkpoint's mark written twice": {record: ctLastPrompt, from: `"type":"last-prompt"`,
			to: `"explicit":true,"type":"last-prompt","explicit":false`},
		"a retractedMessageUuids written twice": {record: retracting,
			from: `"retractedMessageUuids":["` + ctUUID(2) + `"]`,
			to:   `"retractedMessageUuids":["` + ctUUID(9) + `"],"retractedMessageUuids":["` + ctUUID(2) + `"]`},
		"a compaction's list written twice": {record: compacting, from: `,"preservedMessages":{`,
			to: `,"preservedMessages":{"anchorUuid":"` + ctUUID(9) + `","uuids":["` + ctUUID(9) + `"]},` +
				`"preservedMessages":{`},
	} {
		variant := ctReplace(t, tc.record, tc.from, tc.to)
		if got, want := ctAll(ctLine(t, variant)), ctAll(ctLine(t, tc.record)); got != want {
			t.Errorf("%s reads as\n%swant\n%s", name, got, want)
		}
	}
}

// ctOutcome is how a reader reads record, the one line of a thread: where it
// is a record, what every accessor reads of it ([ctAll]); where it is none,
// its fault, the type it names and its error. A line's place and size are left
// out, so two lines that differ in nothing a reader reads print alike.
func ctOutcome(t *testing.T, record string) string {
	t.Helper()
	got := ctRead(ctThread(record), threadPlainMax)
	switch {
	case len(got.broken) == 1 && len(got.lines) == 0 && errors.Is(got.end, io.EOF):
		b := got.broken[0]
		return fmt.Sprintf("no record: fault %q, type %q; %v\n", b.Fault, b.Type, b.Err)
	case len(got.lines) != 1 || len(got.broken) != 0 || !errors.Is(got.end, io.EOF):
		t.Fatalf("%.120s reads as %d lines and %d broken, ended by %v; want one line", record, len(got.lines),
			len(got.broken), got.end)
	}
	l := got.lines[0]
	var b strings.Builder
	h := l.Head
	side := "absent"
	if h.Sidechain != nil {
		side = strconv.FormatBool(*h.Sidechain)
	}
	fmt.Fprintf(&b, "head: type %q, subtype %q, parent %s, sidechain %s, agent %q, replaces a span %t\n",
		h.Type, h.Subtype, ctRaw(h.Parent), side, h.AgentID, h.ReplacesSpan)
	uuid, err := l.UUID()
	fmt.Fprintf(&b, "uuid: %q; %v\n", uuid, err)
	m, err := l.Message()
	ctMessage(&b, m, err)
	kind, err := l.OriginKind()
	fmt.Fprintf(&b, "origin: kind %q; %v\n", kind, err)
	ctLegAnswer(&b, l)
	ctFacts(&b, l, m)
	return b.String()
}

// ctTwice is a key written twice into a line: in writes the keys it is given
// into the line, first is the first occurrence, between what lies between the
// two ("" where nothing does), and last the last.
type ctTwice struct {
	in                   func(keys string) string
	first, between, last string
}

// ctAt writes keys into record in place of from, which it holds exactly once.
func ctAt(t *testing.T, record, from string) func(keys string) string {
	return func(keys string) string { return ctReplace(t, record, from, keys) }
}

// ctKeys is the keys given, those that are not "", as an object writes them.
func ctKeys(keys ...string) string {
	return strings.Join(slices.DeleteFunc(keys, func(k string) bool { return k == "" }), ",")
}

// A KEY WRITTEN TWICE IS READ WHOLLY BY ITS LAST, as Claude Code's JSON.parse
// reads it, at every level of a line: the line reads exactly as the line
// holding only the last occurrence, through every accessor and reader, record
// or no record. Each case's first occurrence reads otherwise on its own (a key
// the last lacks, or another value), so a reader keeping any of it fails. Cases
// cover a record's message, toolUseResult, attachment, compactMetadata,
// replacements, batch and origin; the head keys (a mistyped one then a right
// one is a record, the reverse none; replacesSpan between an assistant's and a
// user's type; a head fault before a type no record carries); a message's
// content, usage, edit report, id, model, stop reason; a block's fields; every
// nested type, count, kind and entry. Mutation: merge repeats into the earlier
// one, read the first, or refuse the repeat.
func TestAKeyWrittenTwiceIsReadWhollyByItsLast(t *testing.T) {
	t.Parallel()
	const other = "toolu_test_other"
	brief, prompt := ctBrief(ctUUID(1)), ctSubagentPrompt(ctUUID(3))
	call := ctAssistant(ctUUID(3), ctUUID(4), ctUseContent)
	result := ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)
	summary := ctSummary(ctUUID(15), ctUUID(16))
	compacting := ctBoundary(ctUUID(10), ctUUID(11), ctUUID(3), ctUUID(4))
	cleared := ctReported(t, ctEdits(ctClearing))
	said := ctAssistant(ctUUID(3), ctUUID(4), `[{"type":"text","text":"hi"}]`)
	sent := ctAssistant(ctUUID(3), ctUUID(4), `[{"type":"tool_use","id":"`+ctCall+`","name":"`+
		claudeToolSendMessage+`","input":{"to":"`+ctAgent+`","message":"continue"}}]`)
	blocked := ctResult(ctUUID(4), ctUUID(5), `[{"tool_use_id":"`+ctCall+`","type":"tool_result","content":`+
		`[{"type":"text","text":"x"}]}]`, ctResultOwn)
	// owned is a result whose own toolUseResult is own.
	owned := func(own string) string { return ctResult(ctUUID(4), ctUUID(5), ctResultBody, own) }
	// iteration is the model's call whose usage lists one iteration, of keys.
	iteration := func(keys string) string {
		return ctReplace(t, call, `"output_tokens":20}`, `"output_tokens":20,"iterations":[{`+keys+`}]}`)
	}
	// batchOf is the model's call whose batch lists one entry, of keys.
	batchOf := func(keys string) string {
		return ctReplace(t, call, `"apiBlockIndex"`, `"batchToolUses":[{`+keys+`}],"apiBlockIndex"`)
	}
	// attached is an attachment record whose payload is attachment.
	attached := func(attachment string) string { return ctAttachment(ctUUID(1), ctUUID(2), attachment, "") }
	member := func(record, key string) string {
		t.Helper()
		var found string
		if err := eachKey(json.RawMessage(record), func(name []byte, value json.RawMessage, _ int) error {
			if string(name) == key {
				found = `"` + key + `":` + string(value)
			}
			return nil
		}); err != nil || found == "" {
			t.Fatalf("%.80s holds no %s: %v", record, key, err)
		}
		return found
	}
	segment := `"preservedSegment":{"headUuid":"` + ctUUID(20) + `","anchorUuid":"` + ctUUID(21) + `","tailUuid":"` +
		ctUUID(22) + `"}`
	withheld := `{"path":"a","why":"denied","displayPath":"a"}`
	listed := ctListed(ctUUID(11), ctUUID(3), ctUUID(4))[1:]
	batching := ctReplace(t, call, `"apiBlockIndex"`, `"batchToolUses":[{"id":"`+ctCall+`","name":"Bash"}],"apiBlockIndex"`)
	for name, tc := range map[string]ctTwice{
		// A record.
		"a message": {in: ctAt(t, call, member(call, "message")), first: member(call, "message"),
			last: `"message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}`},
		"a toolUseResult": {in: ctAt(t, owned(`{}`), `"toolUseResult":{}`),
			first: `"toolUseResult":{"status":"completed","agentId":"` + ctAgent + `","handback":{"report":"x"}}`,
			last:  `"toolUseResult":{}`},
		"an attachment": {in: ctAt(t, attached(`{}`), `"attachment":{}`),
			first: `"attachment":{"type":"` + attachmentForkBriefing + `","toolUseID":"` + ctCall + `"}`,
			last:  `"attachment":{"content":"x"}`},
		"a compactMetadata": {in: ctAt(t, compacting, member(compacting, "compactMetadata")),
			first: member(compacting, "compactMetadata"), last: `"compactMetadata":{` + segment + `}`},
		"a replacements": {in: ctAt(t, ctReplacement("", ctReplaced(ctCall)), `"replacements":[`+ctReplaced(ctCall)+`]`),
			first: `"replacements":[` + ctReplaced(ctCall) + `]`, last: `"replacements":[]`},
		"a batchToolUses": {in: ctAt(t, batching, `"batchToolUses":[{"id":"`+ctCall+`","name":"Bash"}]`),
			first: `"batchToolUses":[{"id":"` + ctCall + `","name":"Bash"}]`, last: `"batchToolUses":[]`},
		"an origin": {in: ctAt(t, ctNotification(ctUUID(1), ctUUID(3)), ctNotificationOrigin),
			first: ctNotificationOrigin, last: `"origin":{}`},
		// The head.
		"an isSidechain of no boolean, then false": {in: ctAt(t, brief, `"isSidechain":false`),
			first: `"isSidechain":"x"`, last: `"isSidechain":false`},
		"an isSidechain false, then of no boolean": {in: ctAt(t, brief, `"isSidechain":false`),
			first: `"isSidechain":false`, last: `"isSidechain":"x"`},
		"a type of no string, then user": {in: ctAt(t, brief, `"type":"user"`),
			first: `"type":7`, last: `"type":"user"`},
		"a type user, then of no string": {in: ctAt(t, brief, `"type":"user"`),
			first: `"type":"user"`, last: `"type":7`},
		"a subtype of no string, then one": {in: ctAt(t, summary, `"subtype":"stop_hook_summary"`),
			first: `"subtype":7`, last: `"subtype":"stop_hook_summary"`},
		"a subtype, then of no string": {in: ctAt(t, summary, `"subtype":"stop_hook_summary"`),
			first: `"subtype":"stop_hook_summary"`, last: `"subtype":7`},
		"an agentId of no string, then one": {in: ctAt(t, prompt, `"agentId":"`+ctAgent+`"`),
			first: `"agentId":7`, last: `"agentId":"` + ctAgent + `"`},
		"an agentId, then of no string": {in: ctAt(t, prompt, `"agentId":"`+ctAgent+`"`),
			first: `"agentId":"` + ctAgent + `"`, last: `"agentId":7`},
		"a parentUuid of neither null nor a string, then one": {in: ctAt(t, result, `"parentUuid":"`+ctUUID(4)+`"`),
			first: `"parentUuid":7`, last: `"parentUuid":"` + ctUUID(4) + `"`},
		"a parentUuid, then of neither null nor a string": {in: ctAt(t, result, `"parentUuid":"`+ctUUID(4)+`"`),
			first: `"parentUuid":"` + ctUUID(4) + `"`, last: `"parentUuid":7`},
		"a replacesSpan of no boolean between an assistant's type and a user's": {in: ctAt(t, brief, `"type":"user"`),
			first: `"type":"assistant"`, between: `"replacesSpan":"x"`, last: `"type":"user"`},
		"a replacesSpan of no boolean between a user's type and an assistant's": {in: ctAt(t, brief, `"type":"user"`),
			first: `"type":"user"`, between: `"replacesSpan":"x"`, last: `"type":"assistant"`},
		"a head fault between a user's type and one no record carries": {
			in:    ctAt(t, brief, `"isSidechain":false,"promptId":"`+ctPrompt+`","type":"user"`),
			first: `"type":"user"`, between: `"isSidechain":1`, last: `"type":"bogus"`},
		// A message.
		"a message's content": {in: ctAt(t, call, `"content":`+ctUseContent),
			first: `"content":` + ctUseContent, last: `"content":"hi"`},
		"a message's usage": {in: ctAt(t, call, ctUsage), first: ctUsage,
			last: `"usage":{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`},
		"a message's context_management": {in: ctAt(t, cleared, `"context_management":`+ctEdits(ctClearing)),
			first: `"context_management":` + ctEdits(ctClearing), last: `"context_management":{}`},
		"a message's id": {in: ctAt(t, call, `"id":"msg_test"`), first: `"id":"msg_other"`, last: `"id":"msg_test"`},
		"a message's model": {in: ctAt(t, call, `"model":"claude-sonnet-4-5"`), first: `"model":"<synthetic>"`,
			last: `"model":"claude-sonnet-4-5"`},
		"a message's stop_reason": {in: ctAt(t, call, `"stop_reason":"tool_use"`), first: `"stop_reason":"refusal"`,
			last: `"stop_reason":"tool_use"`},
		// A message's blocks.
		"a block's id": {in: ctAt(t, call, `"id":"`+ctCall+`"`), first: `"id":"` + other + `"`,
			last: `"id":"` + ctCall + `"`},
		"a block's type": {in: ctAt(t, call, `"type":"tool_use"`), first: `"type":"tool_use"`, last: `"type":"text"`},
		"a block's tool_use_id": {in: ctAt(t, result, `"tool_use_id":"`+ctCall+`"`),
			first: `"tool_use_id":"` + other + `"`, last: `"tool_use_id":"` + ctCall + `"`},
		"a block's is_error": {in: ctAt(t, result, `"is_error":false`), first: `"is_error":true`,
			last: `"is_error":false`},
		"a block's input": {in: ctAt(t, sent, `"input":{"to":"`+ctAgent+`","message":"continue"}`),
			first: `"input":{"to":"` + ctAgent + `","message":"continue"}`, last: `"input":{}`},
		"a block's content": {in: ctAt(t, result, `"content":"b1"`), first: `"content":"b1"`,
			last: `"content":[]`},
		"a block's text, blank first": {in: ctAt(t, said, `"text":"hi"`), first: `"text":" "`, last: `"text":"hi"`},
		"a block's text, blank last":  {in: ctAt(t, said, `"text":"hi"`), first: `"text":"hi"`, last: `"text":" "`},
		// A result's text block.
		"a result's block's type": {in: ctAt(t, blocked, `"type":"text"`), first: `"type":"text"`,
			last: `"type":"image"`},
		"a result's block's text": {in: ctAt(t, blocked, `"text":"x"`), first: `"text":"y"`, last: `"text":"x"`},
		// A record's own toolUseResult.
		"an answer's status": {in: ctAt(t, owned(`{"status":"completed","agentId":"`+ctAgent+`"}`),
			`"status":"completed"`), first: `"status":"completed"`, last: `"status":"async_launched"`},
		"a send's success": {in: ctAt(t, owned(`{"success":false}`), `"success":false`), first: `"success":false`,
			last: `"success":true`},
		// A usage's iterations.
		"an iteration's type": {in: iteration, first: `"type":"` + iterationFallback + `"`,
			between: `"input_tokens":7,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":20`,
			last:    `"type":"` + iterationAdvisor + `"`},
		"an iteration's count": {in: iteration, first: `"cache_read_input_tokens":5`,
			between: `"type":"message","input_tokens":100,"cache_creation_input_tokens":0,"output_tokens":20`,
			last:    `"cache_read_input_tokens":0`},
		// An edit the server applied.
		"an edit's type": {in: ctAt(t, cleared, `"type":"clear_tool_uses_20250919"`),
			first: `"type":"clear_tool_uses_20250919"`, last: `"type":"clear_thinking_20251015"`},
		"an edit's count": {in: ctAt(t, cleared, `"cleared_tool_uses":18`), first: `"cleared_tool_uses":18`,
			last: `"cleared_tool_uses":0`},
		// A replacement, a batch's entry and an attachment's payload.
		"a replacement's kind": {in: ctAt(t, ctReplacement("", ctReplaced(ctCall)), `"kind":"tool-result"`),
			first: `"kind":"tool-result"`, last: `"kind":"image"`},
		"a batch entry's id": {in: batchOf, first: `"id":"` + other + `"`, last: `"id":"` + ctCall + `","name":"Bash"`},
		"an attachment's type": {in: ctAt(t, attached(`{"type":"`+attachmentForkBriefing+`"}`),
			`"type":"`+attachmentForkBriefing+`"`), first: `"type":"` + attachmentForkBriefing + `"`,
			last: `"type":"hook_success"`},
		"a skill listing's names": {in: ctAt(t, attached(`{"type":"`+attachmentSkillListing+`","names":["a"]}`),
			`"names":["a"]`), first: `"names":["a"]`, last: `"names":7`},
		"a withheld memory's entries": {in: ctAt(t, attached(ctWithheld(withheld)), `"entries":[`+withheld+`]`),
			first: `"entries":[` + withheld + `]`, last: `"entries":[{"path":1}]`},
		"a withheld entry's why": {in: ctAt(t, attached(ctWithheld(withheld)), `"why":"denied"`),
			first: `"why":"denied"`, last: `"why":"bogus"`},
		// What a compaction says it kept.
		"a compaction's list": {in: ctAt(t, compacting, listed), first: listed,
			last: `"preservedMessages":{"anchorUuid":"` + ctUUID(11) + `"}`},
	} {
		got := ctOutcome(t, tc.in(ctKeys(tc.first, tc.between, tc.last)))
		if want := ctOutcome(t, tc.in(ctKeys(tc.between, tc.last))); got != want {
			t.Errorf("%s reads as\n%swant it read as its last alone\n%s", name, got, want)
		}
		if first := ctOutcome(t, tc.in(ctKeys(tc.first, tc.between))); first == got {
			t.Errorf("%s reads as its first alone reads, so no reader that keeps the first fails it:\n%s", name, got)
		}
	}
}

// A STRING CLAUDE CODE WROTE BROKEN READS AS THE REPLACEMENT CHARACTER, never
// as no record: a surrogate with no pair, which JSON.stringify writes as a
// `\udXXX` escape where a text was cut inside a pair (an emoji at a truncation
// point), and a byte that is no UTF-8 each read as U+FFFD, as encoding/json's
// own decode reads them, wherever a line holds one: in a text the layer reads
// (a brief's, and a result's, whose length and digests are its reading's), in
// a hook's record, in a result's own output, which its answer is read past,
// and in a key no reader reads. Each line reads whole. Refused, one such line
// would cost a reader its whole reading of a thread its harness reads whole.
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
			} else if _, says := m.Says(); !m.IsText() || !says {
				t.Errorf("%s: a brief holding it reads as a text %t that says something %t; want both", name,
					m.IsText(), says)
			}
		}
		if result, ok := read(ctResult(ctUUID(4), ctUUID(5), ctReplace(t, ctResultBody, `"content":"b1"`,
			`"content":`+tc.written), ctResultOwn)); ok {
			m, err := result.Message()
			switch {
			case err != nil || len(m.Calls()) != 1:
				t.Errorf("%s: a result holding it hands out the calls %+v, %v", name, m.Calls(), err)
			case m.Calls()[0].Text != ltWhole(tc.text).Result:
				t.Errorf("%s: a result's content holding it reads as the text %+v, want %q's %+v", name,
					m.Calls()[0].Text, tc.text, ltWhole(tc.text).Result)
			}
		}
		if note, ok := read(ctAttachment(ctUUID(6), ctUUID(7), ctReplace(t, ctBatchSystem,
			`"content":"crewlet: a note reached the run"`, `"content":`+tc.written), "")); ok {
			kind, err := note.AttachmentType()
			if key, keyed := note.AttachmentToolUseID(); kind != hookSystemMessage || err != nil || !keyed ||
				key != sha256.Sum256([]byte(ctBatchFire)) {
				t.Errorf("%s: a hook's record holding it reads as the attachment %q (%v), keyed %t", name, kind, err,
					keyed)
			}
		}
		if output, ok := read(ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctReplace(t, ctResultOwn, `"stdout":"b1"`,
			`"status":"completed","agentId":"`+ctAgent+`","stdout":`+tc.written))); ok {
			m, err := output.Message()
			completed, agent, refused := output.AgentAnswer()
			if err != nil || len(m.Calls()) != 1 || m.Calls()[0].ID != ctCall || !completed || agent != ctAgent ||
				refused {
				t.Errorf("%s: a result whose own output holds it hands out the calls %+v, %v, and the answer "+
					"completed %t, agent %q, refused %t; want its call, and its answer read past it", name, m.Calls(),
					err, completed, agent, refused)
			}
		}
	}
}

// ABSENT IS NOT NULL: a record outside the chain carries no parentUuid and is
// not chained, its parent absent, the brief's null roots the chain, and a
// chained record's parent is its quoted uuid, each handed over as written.
// Every kind of the harness's bookkeeping measured is outside the chain, the
// nine of them: queue-operation, atis-latch, api-request-shape,
// api-request-blob, api-request, last-prompt, cost-state, mode, and
// tools-used, which 2.1.322 writes whenever the tools a session has used grow,
// and 2.1.310 never does. Each of the three is parentless as Claude Code's
// resume reads a parent but the chained record, which names the brief.
//
// Mutation: read an absent parentUuid as null, and the bookkeeping before the
// brief, and every kind of it, reads as chained, its parent null.
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
		if h := line.Head; h.Chained() || h.Parent != nil || !h.Parentless() {
			t.Errorf("line %d, a %s, carries no parentUuid and reads as chained %t, parentless %t, its parent %s",
				line.N, h.Type, h.Chained(), h.Parentless(), h.Parent)
		}
	}
	if h := got.lines[2].Head; !h.Chained() || !h.Parentless() || string(h.Parent) != "null" {
		t.Errorf("the brief reads as chained %t, parentless %t, its parent %s; want a root, its parent null",
			h.Chained(), h.Parentless(), h.Parent)
	}
	if h := got.lines[3].Head; !h.Chained() || h.Parentless() || string(h.Parent) != `"`+ctUUID(1)+`"` {
		t.Errorf("a chained record reads as chained %t, parentless %t, its parent %s; want chained to the brief",
			h.Chained(), h.Parentless(), h.Parent)
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

// A RECORD NAMES THE RELEASE THAT WROTE IT, by its accessor: a chained record's
// `version` as written, whatever its kind, inside the reading's floor or not, a
// release or not, since the layer holds it to a string alone and its reader
// holds it to its own rule; "" where the key is absent. A `version` that is no
// string is refused by Version alone: the line still reads, with its head, uuid
// and calls as written.
//
// Mutation: hand out no release, and every record reads ""; read it off the
// head, and a non-string `version` is no record; take a non-string for none,
// and a reshaped writer reads as one with no key.
func TestALineHandsTheReleaseThatWroteIt(t *testing.T) {
	t.Parallel()
	release := `"version":"2.1.310"`
	reply := ctAssistant(ctUUID(3), ctUUID(4), ctUseContent)
	for name, tc := range map[string]struct {
		record, release string
	}{
		"the brief":                 {record: ctBrief(ctUUID(1)), release: "2.1.310"},
		"a model's call":            {record: reply, release: "2.1.310"},
		"a hook's record":           {record: ctAttachment(ctUUID(5), ctUUID(6), ctBatchSuccess, ""), release: "2.1.310"},
		"a Stop's summary":          {record: ctSummary(ctUUID(15), ctUUID(16)), release: "2.1.310"},
		"a subagent's prompt":       {record: ctSubagentPrompt(ctUUID(3)), release: "2.1.310"},
		"a skill's hook of 2.1.118": {record: ctSkillHook(ctUUID(5), ctUUID(6)), release: "2.1.118"},
		"a span of 2.1.322":         {record: ctSpan(ctUUID(3), ctUUID(4), ""), release: "2.1.322"},
		"a reply naming no release": {record: ctReplace(t, reply, ","+release, "")},
		"a reply of no release's shape": {record: ctReplace(t, reply, release, `"version":"two"`),
			release: "two"},
		"a request's shape, bookkeeping": {record: ctRequestShape, release: "2.1.310"},
		"a checkpoint, bookkeeping":      {record: ctLastPrompt},
	} {
		if got, err := ctLine(t, tc.record).Version(); got != tc.release || err != nil {
			t.Errorf("%s names its writer as %q, %v; want %q", name, got, err, tc.release)
		}
	}
	for name, value := range map[string]string{"a number": `2.1`, "null": `null`, "an object": `{"release":"2.1.310"}`} {
		line := ctLine(t, ctReplace(t, reply, release, `"version":`+value))
		if got, err := line.Version(); err == nil {
			t.Errorf("a reply whose version is %s names its writer as %q, want an error", name, got)
		}
		uuid, err := line.UUID()
		m, merr := line.Message()
		if h := line.Head; h.Type != recordAssistant || !h.Chained() || !h.Main() || uuid != ctUUID(4) || err != nil ||
			merr != nil || len(m.Calls()) != 1 {
			t.Errorf("a reply whose version is %s reads with the head %+v, the uuid %q (%v) and the calls %+v (%v)",
				name, h, uuid, err, m.Calls(), merr)
		}
	}
}

// A RESTART IS WHERE WHAT CAME BEFORE MAY BE GONE: a compaction's boundary; a
// root after the thread's first chained record; a first chained record that
// roots no chain (a head trimmed away). A first chained record that roots its
// chain, or a thread with none, restarts nowhere. So do the records 2.1.310's
// and 2.1.322's readers stop at: a microcompact_boundary system record and a
// user record whose replacesSpan is true, chained to the record before and
// rooting nothing, each read on its own record's type alone (replacesSpan
// false, absent or of another type on another record restarts nothing). A ROOT IS WHAT
// CLAUDE CODE'S WALK READS AS ONE: a user, assistant, attachment or system
// record whose parentUuid is null, "" or absent (all falsy to it); a parentless
// opening brief restarts nothing; another type with none is bookkeeping, in no
// chain. Mutation: count every root, drop a boundary kind, or read a type or key wrongly.
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
		"a user record whose parentUuid is the empty string": {lines: after(ctReplace(t,
			ctSpan(ctUUID(3), ctUUID(7), ""), `"parentUuid":"`+ctUUID(3)+`"`, `"parentUuid":""`)), restarts: []int{5}},
		"a reply whose parentUuid is the empty string": {lines: after(ctReplace(t,
			ctAssistant(ctUUID(3), ctUUID(7), `[{"type":"text","text":"batch 2."}]`), `"parentUuid":"`+ctUUID(3)+`"`,
			`"parentUuid":""`)), restarts: []int{5}},
		"an attachment whose parentUuid is the empty string": {lines: after(ctReplace(t,
			ctAttachment(ctUUID(3), ctUUID(7), ctTokens, ctTokensRendered), `"parentUuid":"`+ctUUID(3)+`"`,
			`"parentUuid":""`)), restarts: []int{5}},
		"a system record whose parentUuid is the empty string": {lines: after(ctReplace(t,
			ctSummary(ctUUID(3), ctUUID(7)), `"parentUuid":"`+ctUUID(3)+`"`, `"parentUuid":""`)), restarts: []int{5}},
		"a user record with no parentUuid": {lines: after(ctReplace(t, ctSpan(ctUUID(3), ctUUID(7), ""),
			`"parentUuid":"`+ctUUID(3)+`",`, "")), restarts: []int{5}},
		"a reply with no parentUuid": {lines: after(ctReplace(t,
			ctAssistant(ctUUID(3), ctUUID(7), `[{"type":"text","text":"batch 2."}]`), `"parentUuid":"`+ctUUID(3)+`",`,
			"")), restarts: []int{5}},
		"an attachment with no parentUuid": {lines: after(ctReplace(t,
			ctAttachment(ctUUID(3), ctUUID(7), ctTokens, ctTokensRendered), `"parentUuid":"`+ctUUID(3)+`",`, "")),
			restarts: []int{5}},
		"a system record with no parentUuid": {lines: after(ctReplace(t, ctSummary(ctUUID(3), ctUUID(7)),
			`"parentUuid":"`+ctUUID(3)+`",`, "")), restarts: []int{5}},
		"a brief with no parentUuid, opening its thread": {lines: []string{ctEnqueue,
			ctReplace(t, brief, `"parentUuid":null,`, ""), tokens, reply}},
		"a queue operation after the brief, which carries no parentUuid": {lines: after(ctDequeue)},
		"a queue operation after the brief whose parentUuid is null": {lines: after(ctReplace(t, ctDequeue,
			`"type":"queue-operation"`, `"parentUuid":null,"type":"queue-operation"`)), restarts: []int{5}},
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

// A HOOK'S FIRING IS NEVER A CALL'S ID: each of the notes' hook records and a
// Stop's summary hands out no call, and an attachment among them hands its
// firing out only as a key (AttachmentToolUseID), a summary's firing at the
// top never; and a skill's PostToolUse hook, whose firing IS the id of the
// call it followed, hands that id out of Calls never, only as its key. Over a
// whole thread, the calls come only from the model's messages and their
// results, and the firings only as the keys of hook attachments.
//
// Mutation: hand an attachment's toolUseID out as a call (one accessor of a
// line's ids), or key a summary's firing at the top, and a record's id comes
// out where this says it never does.
func TestAHooksFiringIsNeverACallsID(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		record string
		kind   string // its attachment's type, or a summary's subtype
		firing string // what it keys, or "" for nothing
	}{
		"a batch's success": {record: ctAttachment(ctUUID(5), ctUUID(6), ctBatchSuccess, ""), kind: hookSuccess,
			firing: ctBatchFire},
		"a batch's system message": {record: ctAttachment(ctUUID(6), ctUUID(7), ctBatchSystem, ""),
			kind: hookSystemMessage, firing: ctBatchFire},
		"a batch's additional context": {record: ctAttachment(ctUUID(7), ctUUID(8), ctBatchContext, ctBatchRendered),
			kind: hookAdditionalContext, firing: ctBatchFire},
		"a firing past its timeout": {record: ctAttachment(ctUUID(10), ctUUID(11), ctCancelled, ""),
			kind: hookCancelled, firing: ctLateFire},
		"a Stop's success": {record: ctAttachment(ctUUID(12), ctUUID(13), ctStopSuccess, ""), kind: hookSuccess,
			firing: ctStopFire},
		"a Stop's system message": {record: ctAttachment(ctUUID(13), ctUUID(14), ctStopSystem, ""),
			kind: hookSystemMessage, firing: ctStopFire},
		"a Stop's additional context": {record: ctAttachment(ctUUID(14), ctUUID(15), ctStopContext, ctStopRendered),
			kind: hookAdditionalContext, firing: ctStopFire},
		"a Stop's summary":              {record: ctSummary(ctUUID(15), ctUUID(16)), kind: hookStopSummary},
		"a skill's PostToolUse success": {record: ctSkillHook(ctUUID(4), ctUUID(5)), kind: hookSuccess, firing: ctSkillCall},
	} {
		line := ctLine(t, tc.record)
		kind, err := line.AttachmentType()
		if line.Head.Type == recordSystem {
			kind = line.Head.Subtype
		}
		if kind != tc.kind || err != nil {
			t.Errorf("%s reads as the record %q, %v; want %q", name, kind, err, tc.kind)
		}
		want := [32]byte{}
		if tc.firing != "" {
			want = sha256.Sum256([]byte(tc.firing))
		}
		if key, keyed := line.AttachmentToolUseID(); keyed != (tc.firing != "") || key != want {
			t.Errorf("%s keys %x (%t); want the key of %q", name, key, keyed, tc.firing)
		}
		if m, err := line.Message(); err != nil || m.Calls() != nil {
			t.Errorf("%s hands out the calls %+v, %v", name, m.Calls(), err)
		}
	}

	lines := append(slices.Clone(ctMain), ctSkillHook(ctUUID(16), ctUUID(17)))
	got := ctRead(ctThread(lines...), threadPlainMax)
	if len(got.lines) != len(lines) {
		t.Fatalf("%d of %d lines read", len(got.lines), len(lines))
	}
	firings := map[[32]byte]string{}
	for _, id := range []string{ctBatchFire, ctLateFire, ctStopFire, ctSkillCall} {
		firings[sha256.Sum256([]byte(id))] = id
	}
	var ids []ctID
	for _, line := range got.lines {
		if key, keyed := line.AttachmentToolUseID(); keyed {
			id, known := firings[key]
			if !known {
				t.Fatalf("line %d keys %x, no firing of the thread's", line.N, key)
			}
			ids = append(ids, ctID{line.N, "key", id})
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
		{11, "key", ctBatchFire}, {12, "key", ctBatchFire}, {13, "key", ctBatchFire},
		{16, blockToolUse, ctMCPCall}, {17, blockToolResult, ctMCPCall}, {18, "key", ctLateFire},
		{20, "key", ctStopFire}, {21, "key", ctStopFire}, {22, "key", ctStopFire},
		{26, "key", ctSkillCall},
	}
	if !slices.Equal(ids, want) {
		t.Errorf("the thread hands out the ids %+v, want %+v", ids, want)
	}
}

// CALLS COME ONLY FROM A USER OR ASSISTANT MESSAGE: an assistant's tool_use
// blocks and a user's tool_result blocks, each with its id and what its readers
// read (the subagent an input sends to, whether a result failed, a result's text
// as length and digests), nothing from any other record, an api-request-blob's
// repeated result included. A call block in the other record, which the Messages
// API never writes, is a kind and no call. A failed result says so, as a
// resume's unanswered-call and a killed command's do. A message that does not
// decode (a non-object, bad content, a block with no string type or a mistyped
// id, name, tool_use_id or is_error) is an error, never half a call, naming the
// block and record type. Mutation: read calls off every record, decode loosely,
// accept an unnamed id, either block in either record, or is_error false, drop
// the agent rule, read no text as empty, or check only the first block.
func TestCallsComeOnlyFromAUserOrAssistantMessage(t *testing.T) {
	t.Parallel()
	interrupted := "[Tool call interrupted: the session ended before this call's result was recorded, so its " +
		"outcome is unknown. Check whether it took effect before relying on it or running it again.]"
	text := func(s string) threadResultText { return ltWhole(s).Result }
	none := threadResultText{}
	send := func(input string) string {
		return `[{"type":"tool_use","id":"` + ctCall + `","name":"SendMessage","input":` + input + `}]`
	}
	for name, tc := range map[string]struct {
		record string
		calls  []threadCall
	}{
		"a model's call": {record: ctAssistant(ctUUID(3), ctUUID(4), ctUseContent), calls: []threadCall{
			{Kind: blockToolUse, ID: ctCall, Name: "Bash"}}},
		"a message sent to a subagent": {record: ctAssistant(ctUUID(3), ctUUID(4),
			send(`{"to":"`+ctAgent+`","message":"continue"}`)), calls: []threadCall{
			{Kind: blockToolUse, ID: ctCall, Name: "SendMessage", To: ctAgent}}},
		"a message sent to no agent": {record: ctAssistant(ctUUID(3), ctUUID(4),
			send(`{"to":"no agent","message":"continue"}`)), calls: []threadCall{
			{Kind: blockToolUse, ID: ctCall, Name: "SendMessage"}}},
		"its result": {record: ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn), calls: []threadCall{
			{Kind: blockToolResult, ID: ctCall, Text: text("b1")}}},
		"an MCP tool's result, in blocks": {record: ctResult(ctUUID(9), ctUUID(10), ctMCPResult, ctMCPBlocks),
			calls: []threadCall{{Kind: blockToolResult, ID: ctMCPCall, Text: text("crewlet m1 done")}}},
		"a result of no text": {record: ctResult(ctUUID(4), ctUUID(5), ctReplace(t, ctResultBody, `"content":"b1"`,
			`"content":[{"type":"image","source":{"type":"base64","data":"AA=="}}]`), ctResultOwn),
			calls: []threadCall{{Kind: blockToolResult, ID: ctCall, Text: none}}},
		"the result a resume writes for a call it found unanswered": {record: ctInterrupted(ctUUID(4), ctUUID(5)),
			calls: []threadCall{{Kind: blockToolResult, ID: ctCall, Failed: true, Text: text(interrupted)}}},
		"a killed command's result": {record: ctKilled(ctUUID(4), ctUUID(5)), calls: []threadCall{
			{Kind: blockToolResult, ID: ctCall, Failed: true, Text: text("Exit code 137")}}},
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
			if want := tc.calls[i]; c != want {
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
		if err != nil || m.Calls() != nil || ctBlocks(m.Blocks()) != "1 ["+tc.kind+"]" {
			t.Errorf("%s reads as the blocks %s and the calls %+v, %v; want a %s and no call", name,
				ctBlocks(m.Blocks()), m.Calls(), err, tc.kind)
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
			t.Errorf("%s reads as the blocks %s and the calls %+v, %v; want an error naming block 2", name,
				ctBlocks(m.Blocks()), m.Calls(), err)
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

// A MESSAGE SAYS ITS CONTENT'S SHAPE: content that is one string is one text,
// no blocks, and its readers read off it (the subagent and leg a task
// notification names, whether it names the handback tool, wherever it falls);
// content that is a list is how many blocks it holds and which kinds, no text
// (a notification's words in a block name none), and an empty list is a list
// of no blocks, never the text a non-list reads as. Every measured record holds
// one block; the API's own messages hold several.
//
// Mutation: read content as one shape, an empty list as a text, only a list's
// first block or last kind, or stop reading a text once it says something or
// before its last line is decided, and a notification or handback is missed.
func TestAMessageSaysItsContentsShape(t *testing.T) {
	t.Parallel()
	notification := ctNotification(ctUUID(1), ctUUID(3))
	handback := ctReplace(t, notification, `</task-notification>"`,
		`<result>`+claudeToolHandback+` delivered the report</result>\n</task-notification>"`)
	// late names the handback tool past the first piece its text is read in
	// ([textPieceBytes]), where whether it says anything is long decided.
	late := ctReplace(t, notification, `</task-notification>"`, `<result>`+strings.Repeat("x", 2*textPieceBytes)+
		claudeToolHandback+`</result>\n</task-notification>"`)
	// atStatus ends at its status line, the last line a notification is read
	// to, which only the text's end decides.
	atStatus := ctReplace(t, notification, `\n</task-notification>"`, `"`)
	inBlock := `[{"type":"text","text":"` + strings.ReplaceAll(ctNotificationText, "\n", `\n`) + `"}]`
	for name, tc := range map[string]struct {
		record string
		// notice is whether its text names a leg, the notification's own;
		// handback whether it names the handback tool.
		notice, handback bool
		// blocks is its blocks as [ctBlocks] prints them; "" for a text.
		blocks string
	}{
		"a brief":                      {record: ctBrief(ctUUID(1))},
		"a notification":               {record: notification, notice: true},
		"a notification of a handback": {record: handback, notice: true, handback: true},
		"a notification of a handback named late": {record: late, notice: true, handback: true},
		"a model's text":                    {record: ctAssistant(ctUUID(2), ctUUID(3), `[{"type":"text","text":"batch 1."}]`), blocks: "1 [text]"},
		"a notification's words in a block": {record: ctAssistant(ctUUID(2), ctUUID(3), inBlock), blocks: "1 [text]"},
		"a call's result":                   {record: ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn), blocks: "1 [tool_result]"},
		"a skill's text":                    {record: ctSkillText(ctUUID(4), ctUUID(5)), blocks: "1 [text]"},
		"a text and a call":                 {record: ctAssistant(ctUUID(2), ctUUID(3), `[{"type":"text","text":"batch 1."},`+ctUseContent[1:]), blocks: "2 [tool_use text]"},
		"an empty list":                     {record: ctAssistant(ctUUID(2), ctUUID(3), `[]`), blocks: "0 []"},
		"a notification to its status":      {record: atStatus, notice: true},
	} {
		m, err := ctLine(t, tc.record).Message()
		if err != nil {
			t.Errorf("%s reads no message: %v", name, err)
			continue
		}
		want := tc.blocks
		if want == "" {
			want = "0 []"
		}
		if m.IsText() != (tc.blocks == "") || ctBlocks(m.Blocks()) != want {
			t.Errorf("%s reads as a text %t and the blocks %s, want a text %t and %s", name, m.IsText(),
				ctBlocks(m.Blocks()), tc.blocks == "", want)
		}
		agent, leg, ok := m.Notification()
		if ok != tc.notice || (ok && (agent != ctAgent || leg != "toolu_test_agent")) || (!ok && agent+leg != "") ||
			m.NamesHandback() != tc.handback {
			t.Errorf("%s names %q's leg %q (%t) and the handback %t; want a notification %t, the handback %t", name,
				agent, leg, ok, m.NamesHandback(), tc.notice, tc.handback)
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

// A REPLY SAYS WHETHER ITS HARNESS WROTE IT FOR A REQUEST THAT FAILED: APIError
// reads an assistant record's own `isApiErrorMessage` as written, true where
// it says so, false where it is false (the resume's own reply) or absent
// (every model's reply measured); nothing on a record that is no assistant's,
// whatever it carries there; and an error where the mark is no boolean, null
// among it, never false, which its readers would take for a model's reply.
//
// Mutation: read the mark off every record, and a result carrying it is a
// failed request's reply; read the mark as false whatever it holds, and the
// failed request's reply reads as a model's; or read a null as no mark, and
// it is no error.
func TestAReplySaysWhetherItsHarnessWroteItForAFailedRequest(t *testing.T) {
	t.Parallel()
	marked := func(mark string) string {
		return ctReplace(t, ctSynthetic(ctUUID(30), ctUUID(31)), `"isApiErrorMessage":false`,
			`"isApiErrorMessage":`+mark)
	}
	for name, tc := range map[string]struct {
		record string
		failed bool
	}{
		"a failed request's reply":         {marked("true"), true},
		"the reply a resume writes":        {ctSynthetic(ctUUID(30), ctUUID(31)), false},
		"a model's reply, which says none": {ctAssistant(ctUUID(2), ctUUID(3), `[{"type":"text","text":"done."}]`), false},
		"a result carrying the mark": {ctReplace(t, ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn),
			`"type":"user"`, `"isApiErrorMessage":true,"type":"user"`), false},
	} {
		if failed, err := ctLine(t, tc.record).APIError(); failed != tc.failed || err != nil {
			t.Errorf("%s reads as a failed request's %t, %v; want %t", name, failed, err, tc.failed)
		}
	}
	for name, mark := range map[string]string{"null": `null`, "a string": `"true"`, "a number": `1`, "an object": `{}`} {
		if failed, err := ctLine(t, marked(mark)).APIError(); err == nil {
			t.Errorf("a reply whose mark is %s reads as a failed request's %t, want an error", name, failed)
		}
	}
}

// A REPLY SAYS WHICH MODEL WROTE IT: Model reads an assistant record's model
// as written, "<synthetic>" on every reply its harness wrote itself in place
// of a model's (the reply a resume writes, a failed request's), and "" where
// the key is absent; nothing on a record that is no assistant's, whatever its
// message carries; and an error where the model is no string, null among it,
// or the record's message is no object, never "", which its readers would
// take for a model's reply.
//
// Mutation: read the model off every record's message, and a result carrying
// one names a model; read it as any JSON value, and a number reads as "";
// read it as "" whatever it holds, and the harness's own replies read as a
// model's.
func TestAReplySaysWhichModelWroteIt(t *testing.T) {
	t.Parallel()
	text := ctAssistant(ctUUID(2), ctUUID(3), `[{"type":"text","text":"batch 1."}]`)
	failed := ctReplace(t, ctSynthetic(ctUUID(30), ctUUID(31)), `"isApiErrorMessage":false`,
		`"isApiErrorMessage":true`)
	result := ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)
	for name, tc := range map[string]struct {
		record string
		model  string
	}{
		"a model's reply":             {record: text, model: "claude-sonnet-4-5"},
		"the reply a resume writes":   {record: ctSynthetic(ctUUID(30), ctUUID(31)), model: "<synthetic>"},
		"a failed request's reply":    {record: failed, model: "<synthetic>"},
		"a reply that names no model": {record: ctReplace(t, text, `"model":"claude-sonnet-4-5",`, "")},
		"a result whose message carries one": {record: ctReplace(t, result, `"role":"user",`,
			`"role":"user","model":"claude-sonnet-4-5",`)},
		"a brief, which is no reply": {record: ctBrief(ctUUID(1))},
	} {
		if model, err := ctLine(t, tc.record).Model(); model != tc.model || err != nil {
			t.Errorf("%s was written by %q, %v; want %q", name, model, err, tc.model)
		}
	}
	for name, model := range map[string]string{
		"null":      `null`,
		"a number":  `7`,
		"an object": `{"model":"claude-sonnet-4-5"}`,
		"a list":    `["claude-sonnet-4-5"]`,
		"a boolean": `true`,
	} {
		if got, err := ctLine(t, ctReplace(t, text, `"model":"claude-sonnet-4-5"`, `"model":`+model)).Model(); err == nil {
			t.Errorf("a reply whose model is %s was written by %q, want an error", name, got)
		}
	}
	for name, message := range map[string]string{
		"a reply whose message is null": `"message":null,`,
		"a reply with no message":       "",
	} {
		record := `{"parentUuid":"` + ctUUID(30) + `","isSidechain":false,` + message + `"type":"assistant","uuid":"` +
			ctUUID(31) + `","timestamp":"` + ctStamp + `",` + ctTail("2.1.310") + `}`
		if got, err := ctLine(t, record).Model(); err == nil {
			t.Errorf("%s was written by %q, want an error", name, got)
		}
	}
}

// A REPLY SAYS WHICH REPLY IT IS: MessageID reads an assistant record's
// message id as written, which every record of one reply carries; "" where
// absent; nothing on a non-assistant record; an error where the id is no string
// (null included) or the message no object, never "", which readers would take
// for a reply recovering nothing. Also an error where the id holds U+FFFD, as
// written or from a lone surrogate or non-UTF-8 byte: the layer reads each such
// byte as one where Claude Code can read a run as one, so two ids it holds
// apart could read as one.
//
// Mutation: read the id off every record, as any JSON value, or as "" whatever
// it holds, or hand out an id holding U+FFFD, and the matching case fails.
func TestAReplySaysWhichReplyItIs(t *testing.T) {
	t.Parallel()
	call := ctAssistant(ctUUID(3), ctUUID(4), ctUseContent)
	result := ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)
	for name, tc := range map[string]struct {
		record string
		id     string
	}{
		"a model's reply":           {record: call, id: "msg_test"},
		"the reply a resume writes": {record: ctSynthetic(ctUUID(30), ctUUID(31)), id: ctUUID(902)},
		"a reply that names no id":  {record: ctReplace(t, call, `"id":"msg_test",`, "")},
		"a result whose message carries one": {record: ctReplace(t, result, `"role":"user",`,
			`"role":"user","id":"msg_test",`)},
		"a brief, which is no reply": {record: ctBrief(ctUUID(1))},
	} {
		if id, err := ctLine(t, tc.record).MessageID(); id != tc.id || err != nil {
			t.Errorf("%s is the reply %q, %v; want %q", name, id, err, tc.id)
		}
	}
	for name, id := range map[string]string{"null": `null`, "a number": `7`, "an object": `{"id":"msg_test"}`,
		"a string holding a lone surrogate": `"msg_\ud800"`, "a string holding a byte that is no UTF-8": "\"msg_\xff\"",
		"a string holding the replacement character": "\"msg_�\""} {
		if got, err := ctLine(t, ctReplace(t, call, `"id":"msg_test"`, `"id":`+id)).MessageID(); err == nil {
			t.Errorf("a reply whose id is %s is the reply %q, want an error", name, got)
		}
	}
	record := `{"parentUuid":"` + ctUUID(30) + `","isSidechain":false,"message":null,"type":"assistant","uuid":"` +
		ctUUID(31) + `","timestamp":"` + ctStamp + `",` + ctTail("2.1.310") + `}`
	if got, err := ctLine(t, record).MessageID(); err == nil {
		t.Errorf("a reply whose message is null is the reply %q, want an error", got)
	}
}

// ctMarked is record, a user or assistant record ctAssistant, ctResult or their
// kin builds, with the key key holding value, a JSON value, beside its
// isSidechain.
func ctMarked(t *testing.T, record, key, value string) string {
	t.Helper()
	return ctReplace(t, record, `"isSidechain":false`, `"isSidechain":false,"`+key+`":`+value)
}

// A RECORD SAYS WHETHER IT IS VIRTUAL: IsVirtual reads a user or assistant
// record's own isVirtual as written, true where it is the JSON true, false
// where it is false or absent; nothing on a record of another type, whatever
// it carries; and an error where the mark is no boolean, null among it, never
// false, which its reader would take for a plain record.
//
// Mutation: read the mark off every record, and an attachment carrying it is
// virtual; read it off a reply alone, or a user record alone, and the other
// marked one is not; read it as JavaScript's truth, and a string or a number
// reads as true with no error; or read a null as no mark, and it is no error.
func TestARecordSaysWhetherItIsVirtual(t *testing.T) {
	t.Parallel()
	reply := ctAssistant(ctUUID(3), ctUUID(4), ctUseContent)
	result := ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)
	for name, tc := range map[string]struct {
		record  string
		virtual bool
	}{
		"a virtual reply":                {ctMarked(t, reply, "isVirtual", "true"), true},
		"a virtual result":               {ctMarked(t, result, "isVirtual", "true"), true},
		"a reply marked false":           {ctMarked(t, reply, "isVirtual", "false"), false},
		"a reply as measured, with none": {reply, false},
		"an attachment carrying the mark": {ctAttachment(ctUUID(1), ctUUID(2), ctTokens, `"isVirtual":true,`),
			false},
		"a system record carrying the mark": {ctMarked(t, ctSummary(ctUUID(15), ctUUID(16)), "isVirtual", "true"),
			false},
	} {
		if virtual, err := ctLine(t, tc.record).IsVirtual(); virtual != tc.virtual || err != nil {
			t.Errorf("%s reads as virtual %t, %v; want %t", name, virtual, err, tc.virtual)
		}
	}
	for name, mark := range map[string]string{"null": `null`, "a string": `"true"`, "a number": `1`, "an object": `{}`} {
		for kind, record := range map[string]string{"a reply": reply, "a result": result} {
			if virtual, err := ctLine(t, ctMarked(t, record, "isVirtual", mark)).IsVirtual(); err == nil {
				t.Errorf("%s whose isVirtual is %s reads as virtual %t, want an error", kind, name, virtual)
			}
		}
	}
}

// A RESULT SAYS WHETHER A SHUTDOWN INTERRUPTED ITS CALLS:
// InterruptedByShutdown reads a user record's own interruptedByShutdown as
// written, true where it is the JSON true, false where it is false or absent;
// nothing on a record that is no user's, whatever it carries; and an error
// where the mark is no boolean, null among it, never false.
//
// Mutation: read the mark off every record, and a reply carrying it reads as
// interrupted; read it as JavaScript's truth, and a string or a number reads
// as true with no error; or read a null as no mark, and it is no error.
func TestAResultSaysWhetherAShutdownInterruptedItsCalls(t *testing.T) {
	t.Parallel()
	result := ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)
	for name, tc := range map[string]struct {
		record      string
		interrupted bool
	}{
		"a result a shutdown interrupted": {ctMarked(t, result, "interruptedByShutdown", "true"), true},
		"a result marked false":           {ctMarked(t, result, "interruptedByShutdown", "false"), false},
		"a result as measured, with none": {result, false},
		"a reply carrying the mark": {ctMarked(t, ctAssistant(ctUUID(3), ctUUID(4), ctUseContent),
			"interruptedByShutdown", "true"), false},
	} {
		if interrupted, err := ctLine(t, tc.record).InterruptedByShutdown(); interrupted != tc.interrupted || err != nil {
			t.Errorf("%s reads as interrupted by a shutdown %t, %v; want %t", name, interrupted, err, tc.interrupted)
		}
	}
	for name, mark := range map[string]string{"null": `null`, "a string": `"true"`, "a number": `1`, "an object": `{}`} {
		line := ctLine(t, ctMarked(t, result, "interruptedByShutdown", mark))
		if interrupted, err := line.InterruptedByShutdown(); err == nil {
			t.Errorf("a result whose interruptedByShutdown is %s reads as interrupted %t, want an error", name,
				interrupted)
		}
	}
}

// A RECORD SAYS WHETHER IT IS A COMPACTION'S SUMMARY: IsCompactSummary reads a
// user record's own isCompactSummary as written, true on the summary a
// compaction hands its model, false where it is false or absent; nothing on a
// record that is no user's, whatever it carries; and an error where the mark
// is no boolean, null among it, never false.
//
// Mutation: read the mark off every record, and a reply carrying it reads as
// a summary; read it as JavaScript's truth, and a string or a number reads as
// true with no error; or read a null as no mark, and it is no error.
func TestARecordSaysWhetherItIsACompactionsSummary(t *testing.T) {
	t.Parallel()
	summary := ctCompactSummary(ctUUID(5), ctUUID(6))
	for name, tc := range map[string]struct {
		record  string
		summary bool
	}{
		"a compaction's summary, as 2.1.310 writes it": {summary, true},
		"a summary marked false": {ctReplace(t, summary, `"isCompactSummary":true`, `"isCompactSummary":false`),
			false},
		"a result, with none": {ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn), false},
		"a reply carrying the mark": {ctMarked(t, ctAssistant(ctUUID(3), ctUUID(4), ctUseContent),
			"isCompactSummary", "true"), false},
	} {
		if got, err := ctLine(t, tc.record).IsCompactSummary(); got != tc.summary || err != nil {
			t.Errorf("%s reads as a compaction's summary %t, %v; want %t", name, got, err, tc.summary)
		}
	}
	for name, mark := range map[string]string{"null": `null`, "a string": `"true"`, "a number": `1`, "an object": `{}`} {
		line := ctLine(t, ctReplace(t, summary, `"isCompactSummary":true`, `"isCompactSummary":`+mark))
		if got, err := line.IsCompactSummary(); err == nil {
			t.Errorf("a summary whose isCompactSummary is %s reads as one %t, want an error", name, got)
		}
	}
}

// ctTaskDelivery is a background task's result as a user record carries it in
// its `taskDelivery`, abridged: its keys are those 2.1.322 reads of one.
const ctTaskDelivery = `{"taskId":"task_test","status":"completed","result":{"content":[{"type":"text",` +
	`"text":"the task's result"}],"isError":false}}`

// A RECORD SAYS WHETHER IT DELIVERS A TASK: TaskDelivery reads whether a user
// record carries taskDelivery at all, whatever it holds, a task's result,
// null, false or an empty object among it, as the harness's widest reading of
// the key does (its presence); and nothing on a record that is no user's,
// whatever it carries.
//
// Mutation: read the key off every record, and a reply carrying it delivers a
// task; or read it by its truth, as the fold does, and a record whose
// taskDelivery is null or false delivers none, though the pass that orders
// attachments reads it by its presence.
func TestARecordSaysWhetherItDeliversATask(t *testing.T) {
	t.Parallel()
	result := ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)
	for name, tc := range map[string]struct {
		record    string
		delivered bool
	}{
		"a result delivering a task":       {ctMarked(t, result, "taskDelivery", ctTaskDelivery), true},
		"a result whose delivery is null":  {ctMarked(t, result, "taskDelivery", "null"), true},
		"a result whose delivery is false": {ctMarked(t, result, "taskDelivery", "false"), true},
		"a result whose delivery is empty": {ctMarked(t, result, "taskDelivery", "{}"), true},
		"a result as measured, with none":  {result, false},
		"a reply carrying the key": {ctMarked(t, ctAssistant(ctUUID(3), ctUUID(4), ctUseContent), "taskDelivery",
			ctTaskDelivery), false},
		"an attachment carrying the key": {ctAttachment(ctUUID(1), ctUUID(2), ctTokens,
			`"taskDelivery":`+ctTaskDelivery+`,`), false},
	} {
		if delivered := ctLine(t, tc.record).TaskDelivery(); delivered != tc.delivered {
			t.Errorf("%s reads as a task's delivery %t; want %t", name, delivered, tc.delivered)
		}
	}
}

// A REPLY SAYS THE CALLS ITS BATCH WAS SPLIT INTO: BatchToolUses hands each
// batchToolUses entry's id in order from an assistant record (an empty list
// hands nothing); nothing on a non-assistant record; an error where the key
// holds no list (null included) or an entry is no object or its id no string,
// never a reply that splits nothing. An error ends the walk, entries before it
// handed.
//
// Mutation: read the key off every record, hand entries of no string id, read
// null as none, or answer no batch for an empty list, and a case fails.
func TestAReplySaysTheCallsItsBatchWasSplitInto(t *testing.T) {
	t.Parallel()
	reply := ctAssistant(ctUUID(3), ctUUID(4), ctUseContent)
	entries := `[{"id":"` + ctCall + `","name":"Bash"},{"id":"` + ctMCPCall + `","name":"mcp__crewlet__slow"}]`
	for name, tc := range map[string]struct {
		record  string
		batched bool
		ids     []string
	}{
		"a batch's reply":                {ctMarked(t, reply, "batchToolUses", entries), true, []string{ctCall, ctMCPCall}},
		"a batch's reply listing none":   {ctMarked(t, reply, "batchToolUses", "[]"), true, nil},
		"an entry of no name":            {ctMarked(t, reply, "batchToolUses", `[{"id":"`+ctCall+`"}]`), true, []string{ctCall}},
		"a reply as measured, with none": {reply, false, nil},
		"a result carrying the key":      {ctMarked(t, ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn), "batchToolUses", entries), false, nil},
	} {
		var got []string
		batched, err := ctLine(t, tc.record).BatchToolUses(func(id string) { got = append(got, id) })
		if batched != tc.batched || err != nil || !slices.Equal(got, tc.ids) {
			t.Errorf("%s reads as a batch's reply %t, splitting %q, %v; want %t, splitting %q", name, batched,
				strings.Join(got, ","), err, tc.batched, strings.Join(tc.ids, ","))
		}
	}
	for name, list := range map[string]string{
		"null":                          `null`,
		"an object":                     `{"id":"` + ctCall + `"}`,
		"a string":                      `"` + ctCall + `"`,
		"a list holding a string":       `["` + ctCall + `"]`,
		"a list holding null":           `[null]`,
		"an entry of no id":             `[{"name":"Bash"}]`,
		"an entry whose id is null":     `[{"id":null,"name":"Bash"}]`,
		"an entry whose id is a number": `[{"id":7,"name":"Bash"}]`,
	} {
		if batched, err := ctLine(t, ctMarked(t, reply, "batchToolUses", list)).BatchToolUses(func(string) {}); err == nil {
			t.Errorf("a reply whose batchToolUses is %s reads as a batch's reply %t, want an error", name, batched)
		}
	}
	var got []string
	_, err := ctLine(t, ctMarked(t, reply, "batchToolUses", `[{"id":"`+ctCall+`"},7]`)).BatchToolUses(
		func(id string) { got = append(got, id) })
	if err == nil || !slices.Equal(got, []string{ctCall}) {
		t.Errorf("a list whose second entry is no object hands %q, %v; want its first entry, then an error", got, err)
	}
}

// A LAST-PROMPT RECORD SAYS THE LEAF ITS RESUME STARTS FROM: Checkpoint reads
// its leafUuid where a string, "" where absent, null or empty, each naming no
// leaf to the harness; it is marked where `explicit` or `rewound` is JSON true
// (a rewind, withdrawal or fork), as the harness tests (=== true); another
// type is none. LeafKeyed says whether the record carries a leafUuid at all,
// null and "" included, as the harness tests it (!== undefined); false where
// absent and on another type. A leafUuid of another type is an error of both
// ([TestAKeyOneReaderReadsCostsThatReaderAlone]).
//
// Mutation: read a mark as set by any value but true, ignore rewound, read the
// keys off every record, or key the leaf by its truth, and a case fails.
func TestALastPromptSaysTheLeafItsResumeStartsFrom(t *testing.T) {
	t.Parallel()
	leafed := func(to string) string {
		return ctReplace(t, ctLastPrompt, `"leafUuid":"`+ctUUID(5)+`"`, to)
	}
	named := `"leafUuid":"` + ctUUID(5) + `"`
	for name, tc := range map[string]struct {
		record        string
		leaf          string
		marked, keyed bool
	}{
		"its metadata, naming a leaf":   {record: ctLastPrompt, leaf: ctUUID(5), keyed: true},
		"naming none":                   {record: leafed(`"leafUuid":null`), keyed: true},
		"naming none, the empty string": {record: leafed(`"leafUuid":""`), keyed: true},
		"with no leafUuid":              {record: leafed(`"lastPromptAt":1`)},
		"a rewind's": {record: leafed(named + `,"explicit":true,"rewound":true`), leaf: ctUUID(5),
			marked: true, keyed: true},
		"a withdrawal's":           {record: leafed(`"leafUuid":null,"explicit":true`), marked: true, keyed: true},
		"rewound alone":            {record: leafed(named + `,"rewound":true`), leaf: ctUUID(5), marked: true, keyed: true},
		"explicit false":           {record: leafed(named + `,"explicit":false`), leaf: ctUUID(5), keyed: true},
		"explicit the string true": {record: leafed(named + `,"explicit":"true"`), leaf: ctUUID(5), keyed: true},
		"rewound the number 1":     {record: leafed(named + `,"rewound":1`), leaf: ctUUID(5), keyed: true},
		"a reply carrying a checkpoint's keys": {record: ctReplace(t, ctAssistant(ctUUID(3), ctUUID(4), ctUseContent),
			`"type":"assistant"`, named+`,"explicit":true,"type":"assistant"`)},
	} {
		line := ctLine(t, tc.record)
		leaf, marked, err := line.Checkpoint()
		if leaf != tc.leaf || marked != tc.marked || err != nil {
			t.Errorf("%s names the leaf %q, marked %t, %v; want %q, marked %t", name, leaf, marked, err, tc.leaf,
				tc.marked)
		}
		if keyed, err := line.LeafKeyed(); keyed != tc.keyed || err != nil {
			t.Errorf("%s reads as keyed %t, %v; want %t", name, keyed, err, tc.keyed)
		}
	}
}

// AN ATTACHMENT SAYS ITS TYPE: AttachmentType reads an attachment record's
// attachment's type as written, a fork's briefing's among them; "" where the
// attachment or its type is absent, and on a record that is no attachment,
// whatever it carries; and an error where the attachment is no object or its
// type no string, never "", which a reader can take for no fork's briefing
// only by its own choice ([TestAKeyOneReaderReadsCostsThatReaderAlone]).
//
// Mutation: read the type off every record, and a reply carrying an
// attachment reads as one; or read it as any JSON value, and a number reads as
// "".
func TestAnAttachmentSaysItsType(t *testing.T) {
	t.Parallel()
	briefing := ctAttachment(ctUUID(1), ctUUID(2), `{"type":"fork_briefing","text":"the fork's brief"}`, "")
	for name, tc := range map[string]struct {
		record string
		kind   string
	}{
		"a fork's briefing": {record: briefing, kind: attachmentForkBriefing},
		"a reminder of tokens": {record: ctAttachment(ctUUID(1), ctUUID(2), ctTokens, ctTokensRendered),
			kind: "total_tokens_reminder"},
		"an attachment of no type": {record: ctReplace(t, briefing, `"type":"fork_briefing",`, "")},
		"an attachment record with none": {record: ctReplace(t, briefing,
			`"attachment":{"type":"fork_briefing","text":"the fork's brief"},`, "")},
		"a reply carrying an attachment": {record: ctReplace(t, ctAssistant(ctUUID(3), ctUUID(4), ctUseContent),
			`"type":"assistant"`, `"attachment":{"type":"fork_briefing"},"type":"assistant"`)},
	} {
		if kind, err := ctLine(t, tc.record).AttachmentType(); kind != tc.kind || err != nil {
			t.Errorf("%s is an attachment of the type %q, %v; want %q", name, kind, err, tc.kind)
		}
	}
	for name, kind := range map[string]string{"null": `null`, "a number": `7`, "an object": `{}`} {
		line := ctLine(t, ctReplace(t, briefing, `"type":"fork_briefing"`, `"type":`+kind))
		if got, err := line.AttachmentType(); err == nil {
			t.Errorf("an attachment whose type is %s is of the type %q, want an error", name, got)
		}
	}
}

// ctReplacement is a content-replacement record of entries, each a JSON value,
// in ctSession's thread of agent, "" for the main thread, whose record names
// none. No run reached one, which waits on a flag only Anthropic's servers
// set: its keys and their order are read from 2.1.310's and 2.1.322's code.
func ctReplacement(agent string, entries ...string) string {
	named := ""
	if agent != "" {
		named = `"agentId":"` + agent + `",`
	}
	return `{"type":"content-replacement","sessionId":"` + ctSession + `",` + named + `"replacements":[` +
		strings.Join(entries, ",") + `]}`
}

// ctReplaced is the entry of a content-replacement record that replaces call
// id's result, its preview abridged.
func ctReplaced(id string) string {
	return `{"kind":"tool-result","toolUseId":"` + id + `","replacement":"<persisted-output>\nOutput too large ` +
		`(212.4KB). Full output saved to: /tmp/box/tool-results/` + id + `.txt\n</persisted-output>"}`
}

// A CONTENT-REPLACEMENT RECORD NAMES THE RESULTS IT REPLACED: ReplacedResults
// hands, in order, the id of each entry of kind "tool-result" (main thread and
// subagent alike); passes over an entry of another kind, as the resume does;
// reads nothing off another record type; and errors where replacements have a
// shape the harness never writes: no list, an object, a non-object entry, a
// non-string kind or tool id.
//
// Mutation: read ids off every record listing replacements, take every entry
// for a tool result, or pass an entry of no shape over, and a case fails.
func TestAContentReplacementRecordNamesTheResultsItReplaced(t *testing.T) {
	t.Parallel()
	other := `{"kind":"image","toolUseId":"` + ctCall + `","replacement":"x"}`
	listed := ctReplacement("", ctReplaced(ctCall))
	for name, tc := range map[string]struct {
		record string
		ids    []string
	}{
		"the main thread's":   {ctReplacement("", ctReplaced(ctCall), ctReplaced(ctMCPCall)), []string{ctCall, ctMCPCall}},
		"a subagent's":        {ctReplacement(ctAgent, ctReplaced(ctCall)), []string{ctCall}},
		"one of another kind": {ctReplacement("", other, ctReplaced(ctMCPCall)), []string{ctMCPCall}},
		"one of no entries":   {ctReplacement(""), nil},
		"a result listing replacements": {ctReplace(t, ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn),
			`"type":"user"`, `"replacements":[`+ctReplaced(ctCall)+`],"type":"user"`), nil},
	} {
		var got []string
		err := ctLine(t, tc.record).ReplacedResults(func(id string) { got = append(got, id) })
		if err != nil || !slices.Equal(got, tc.ids) {
			t.Errorf("%s names the results %q, %v; want %q", name, strings.Join(got, ","), err,
				strings.Join(tc.ids, ","))
		}
	}
	for name, record := range map[string]string{
		"no list":                    `{"type":"content-replacement","sessionId":"` + ctSession + `"}`,
		"an object for its list":     strings.Replace(listed, `"replacements":[`, `"replacements":{"x":[`, 1) + "}",
		"an entry that is no object": ctReplacement("", `"x"`),
		"a kind that is no string":   ctReplacement("", `{"kind":1,"toolUseId":"`+ctCall+`"}`),
		"a result naming no call":    ctReplacement("", `{"kind":"tool-result","replacement":"x"}`),
		"a call named by null":       ctReplacement("", `{"kind":"tool-result","toolUseId":null}`),
	} {
		if err := ctLine(t, record).ReplacedResults(func(string) {}); err == nil {
			t.Errorf("a record of %s replaces results with no error; want one", name)
		}
	}
}

// A CONTENT-REPLACEMENT RECORD SAYS HOW MANY ENTRIES ITS LOAD SPREADS:
// ReplacementCount counts every entry of a content-replacement record's
// replacements, whatever its kind or shape, since 2.1.310's load spreads them
// all into one call's arguments; reads nothing off a record of another type,
// though it lists replacements; and is an error where the record lists them
// in no list, which 2.1.310's load fails at: absent, null, a string, a number,
// an object.
//
// Mutation: count only the entries that replace a tool's result, and a list
// holding another kind or an entry of no shape reads short; or read an absent
// list as none, and a record its load fails at reads with no error.
func TestAContentReplacementRecordSaysHowManyEntriesItsLoadSpreads(t *testing.T) {
	t.Parallel()
	listed := ctReplacement("", ctReplaced(ctCall))
	for name, tc := range map[string]struct {
		record string
		n      int
	}{
		"the main thread's": {ctReplacement("", ctReplaced(ctCall), ctReplaced(ctMCPCall)), 2},
		"a subagent's":      {ctReplacement(ctAgent, ctReplaced(ctCall)), 1},
		"one of another kind and of no shape": {ctReplacement("", `{"kind":"image","replacement":"x"}`, `"x"`, `7`,
			ctReplaced(ctMCPCall)), 4},
		"one of no entries": {ctReplacement(""), 0},
		"a result listing replacements": {ctReplace(t, ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn),
			`"type":"user"`, `"replacements":[`+ctReplaced(ctCall)+`],"type":"user"`), 0},
	} {
		if n, err := ctLine(t, tc.record).ReplacementCount(); n != tc.n || err != nil {
			t.Errorf("%s lists %d entries, %v; want %d", name, n, err, tc.n)
		}
	}
	for name, list := range map[string]string{"absent": "", "null": `,"replacements":null`,
		"a string": `,"replacements":"` + ctCall + `"`, "a number": `,"replacements":7`,
		"an object": `,"replacements":{"x":[]}`} {
		record := ctReplace(t, listed, `,"replacements":[`+ctReplaced(ctCall)+`]`, list)
		if n, err := ctLine(t, record).ReplacementCount(); err == nil {
			t.Errorf("a record whose replacements are %s lists %d with no error; want one", name, n)
		}
	}
}

// ctQuoted is uuids, each quoted, joined as a JSON list's elements.
func ctQuoted(uuids []string) string {
	if len(uuids) == 0 {
		return ""
	}
	return `"` + strings.Join(uuids, `","`) + `"`
}

// ctFallback is the system record 2.1.322 writes where a model refused a reply
// as it streamed and the harness moved the request to another model, chained
// to parent, naming the records it took back out of the conversation:
// retracted, each written as given. No run reached one, which waits on a
// refusal: its keys and their order are read from 2.1.322's code, the
// record's own beside the keys every chained record carries.
func ctFallback(parent, uuid string, retracted ...string) string {
	return `{"parentUuid":"` + parent + `","isSidechain":false,"type":"system","subtype":"model_refusal_fallback",` +
		`"direction":"retry","scope":"session","content":"Switched to claude-sonnet-4-5","level":"warning",` +
		`"trigger":"refusal","originalModel":"claude-opus-4-5","fallbackModel":"claude-sonnet-4-5",` +
		`"requestId":"req_test","apiRefusalCategory":null,"apiRefusalExplanation":null,"retractedMessageUuids":[` +
		ctQuoted(retracted) + `],"refusedUserMessageUuid":null,"isMeta":false,"timestamp":"` + ctStamp +
		`","uuid":"` + uuid + `",` + ctTail("2.1.322") + `}`
}

// ctRetry is the meta user record 2.1.322 sends where it retries a reply whose
// call it could not parse, chained to parent, naming the records of that reply
// it took back: retracted, each written as given. Abridged: its text is not
// the harness's own. Read from 2.1.322's code, as ctFallback is.
func ctRetry(parent, uuid string, retracted ...string) string {
	return `{"parentUuid":"` + parent + `","isSidechain":false,"promptId":"` + ctPrompt + `","type":"user",` +
		`"message":{"role":"user","content":"The tool call could not be parsed; try again."},"isMeta":true,` +
		`"turnCompanion":true,"uuid":"` + uuid + `","timestamp":"` + ctStamp + `","retractedMessageUuids":[` +
		ctQuoted(retracted) + `],` + ctTail("2.1.322") + `}`
}

// ctPrefix is the canonical prefix an entry beginning with uuid's first 24
// characters names, read as the uuid package reads uuid: its first 10 bytes.
func ctPrefix(u string) [chainPrefixBytes]byte {
	whole := uuid.MustParse(u)
	return [chainPrefixBytes]byte(whole[:chainPrefixBytes])
}

// ctRetracting is a refused reply's fallback whose list names n entries, the
// uuids ctUUID numbers from 1000, then after, as written.
func ctRetracting(t *testing.T, n int, after string) string {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = ctUUID(1000 + i)
	}
	return ctReplace(t, ctFallback(ctUUID(5), ctUUID(6), entries...), `],"refusedUserMessageUuid"`,
		after+`],"refusedUserMessageUuid"`)
}

// A RECORD SAYS WHICH RECORDS ITS HARNESS TOOK BACK: Retracted counts each
// retractedMessageUuids entry and holds, in order, the first 24 characters of
// each that begins with a canonical uuid's, on the records 2.1.322 reads the
// key on: a refused reply's fallback (system), a malformed reply's retry (meta
// user) and a reply (assistant). The resume cuts at 24 itself, so entries of 24
// and 30 characters count and name a prefix; 23, another form or uppercase
// counts and names none. Nothing where absent or on another record; an error
// where the key is no list of strings (null included), handed with the entries
// before the first non-string. At most threadChainMaxRecords prefixes, still
// counting every entry. Mutation: read the key off every record or a fallback
// alone as 2.1.310 does, skip non-strings, read null as none, drop short
// entries, count only named ones, lose entries before an error, ignore the bound.
func TestARecordSaysWhichRecordsItsHarnessTookBack(t *testing.T) {
	t.Parallel()
	named := []string{ctUUID(4), ctUUID(5)}
	reply := ctReplace(t, ctAssistant(ctUUID(3), ctUUID(4), ctUseContent), `"type":"assistant"`,
		`"retractedMessageUuids":[`+ctQuoted(named)+`],"type":"assistant"`)
	cut := []string{ctUUID(7)[:24], ctUUID(8)[:30], ctUUID(9)[:23]}
	for name, tc := range map[string]struct {
		record  string
		entries int
		// named is the uuids whose prefixes the list holds, in order.
		named []string
	}{
		"a refused reply's fallback": {ctFallback(ctUUID(5), ctUUID(6), named...), 2, named},
		"a malformed reply's retry":  {ctRetry(ctUUID(5), ctUUID(6), named...), 2, named},
		"a reply":                    {reply, 2, named},
		"a fallback naming none":     {ctFallback(ctUUID(5), ctUUID(6)), 0, nil},
		"a fallback naming entries of 24, 30 and 23 characters": {ctFallback(ctUUID(5), ctUUID(6), cut...), 3,
			[]string{ctUUID(7), ctUUID(8)}},
		"a fallback naming a uuid of another form": {ctFallback(ctUUID(5), ctUUID(6), "not a uuid"), 1, nil},
		"a fallback naming a uuid in uppercase": {ctFallback(ctUUID(5), ctUUID(6),
			strings.ToUpper("abcdef01-0000-4000-8000-000000000001")), 1, nil},
		"a record that names none": {ctAssistant(ctUUID(3), ctUUID(4), ctUseContent), 0, nil},
		"a stop hook's summary carrying the key": {ctReplace(t, ctSummary(ctUUID(15), ctUUID(16)),
			`"hookCount":1`, `"retractedMessageUuids":["`+ctUUID(4)+`"],"hookCount":1`), 0, nil},
		"an attachment carrying the key": {ctAttachment(ctUUID(3), ctUUID(4), ctTokens,
			`"retractedMessageUuids":["`+ctUUID(2)+`"],`), 0, nil},
	} {
		r, err := ctLine(t, tc.record).Retracted()
		held, all := r.Prefixes()
		want := make([][chainPrefixBytes]byte, len(tc.named))
		for i, u := range tc.named {
			want[i] = ctPrefix(u)
		}
		if err != nil || r.Entries != tc.entries || !slices.Equal(held, want) || !all {
			t.Errorf("%s retracts %d entries, the prefixes %x (all %t), %v; want %d and %x", name, r.Entries, held,
				all, err, tc.entries, want)
		}
	}
	fallback := ctFallback(ctUUID(5), ctUUID(6), named...)
	for name, list := range map[string]string{"null": `null`, "a string": `"` + ctUUID(4) + `"`,
		"an object": `{"uuid":"` + ctUUID(4) + `"}`, "a list holding a number": `[7,"` + ctUUID(4) + `"]`,
		"a list holding null": `[null]`} {
		for kind, record := range map[string]string{"a refused reply's fallback": fallback,
			"a malformed reply's retry": ctRetry(ctUUID(5), ctUUID(6), named...)} {
			line := ctLine(t, ctReplace(t, record, `"retractedMessageUuids":[`+ctQuoted(named)+`]`,
				`"retractedMessageUuids":`+list))
			if r, err := line.Retracted(); err == nil || r.Entries != 0 {
				t.Errorf("%s whose retractedMessageUuids is %s retracts %d entries, %v; want none and an error",
					kind, name, r.Entries, err)
			}
		}
	}
	r, err := ctLine(t, ctReplace(t, fallback, `"retractedMessageUuids":[`+ctQuoted(named)+`]`,
		`"retractedMessageUuids":["x",`+ctQuoted(named)+`,7,"`+ctUUID(9)+`"]`)).Retracted()
	if held, all := r.Prefixes(); err == nil || r.Entries != 3 || !all ||
		!slices.Equal(held, [][chainPrefixBytes]byte{ctPrefix(named[0]), ctPrefix(named[1])}) {
		t.Errorf("a list of three strings, then a number, retracts %d entries, the prefixes %x (all %t), %v; want "+
			"the three, the two uuids' prefixes and an error", r.Entries, held, all, err)
	}

	for name, tc := range map[string]struct {
		record  string
		entries int
		all     bool
	}{
		"a list of the bound's prefixes and an entry of none": {ctRetracting(t, threadChainMaxRecords, `,"x"`),
			threadChainMaxRecords + 1, true},
		"a list of a prefix past the bound": {ctRetracting(t, threadChainMaxRecords+1, ""),
			threadChainMaxRecords + 1, false},
	} {
		r, err := ctLine(t, tc.record).Retracted()
		held, all := r.Prefixes()
		if err != nil || r.Entries != tc.entries || all != tc.all || len(held) != threadChainMaxRecords ||
			held[len(held)-1] != ctPrefix(ctUUID(1000+threadChainMaxRecords-1)) {
			t.Errorf("%s retracts %d entries, %d prefixes, all %t, %v; want %d, the bound's first %d and %t", name,
				r.Entries, len(held), all, err, tc.entries, threadChainMaxRecords, tc.all)
		}
	}
}

// ctListed is the list a compaction's boundary built by ctBoundary carries,
// as written: its `preservedMessages`, the key before it included.
func ctListed(summary string, kept ...string) string {
	ids := ctQuoted(kept)
	return `,"preservedMessages":{"anchorUuid":"` + summary + `","uuids":[` + ids + `],"allUuids":[` + ids + `]}`
}

// ctKept is the uuids a boundary lists, each read as the uuid package reads it.
func ctKept(uuids ...string) [][16]byte {
	var kept [][16]byte
	for _, u := range uuids {
		kept = append(kept, [16]byte(uuid.MustParse(u)))
	}
	return kept
}

// A COMPACTION'S BOUNDARY SAYS WHAT IT KEPT, as a resume relinks a thread by it:
// Preserved reads a compact_boundary's preservedMessages (anchor, entry count,
// each canonical uuid in order, whether any entry is none), else its
// preservedSegment (anchor, both ends); nothing where it carries neither, a
// null for either, no compactMetadata, or on another type or subtype. A
// boundary carrying either names what it kept, even a list of none with no
// anchor, since the resume drops every record before the last boundary. Any
// other shape is an error handed with the entries before the first non-string,
// none where the anchor fails (checked before the list). At most
// threadChainMaxRecords uuids, still counting every entry. Mutation: read the
// segment where a list is, take a list of none or null for a kept one, count
// only uuids, lose entries before an error, check the list before its anchor, or ignore the bound.
func TestACompactionsBoundarySaysWhatItKept(t *testing.T) {
	t.Parallel()
	kept := []string{ctUUID(3), ctUUID(5), ctUUID(6)}
	boundary := ctBoundary(ctUUID(7), ctUUID(8), kept...)
	listed := ctListed(ctUUID(8), kept...)
	segment := ctReplace(t, boundary, listed, "")
	listing := func(entries string) string {
		return ctReplace(t, boundary, listed, `,"preservedMessages":{"anchorUuid":"`+ctUUID(8)+`","uuids":[`+
			entries+`]}`)
	}
	for name, tc := range map[string]struct {
		record string
		want   threadPreserved
	}{
		"a boundary listing what it kept beside its segment": {boundary, threadPreserved{Named: true, Kept: true,
			Anchor: ctUUID(8), Listed: 3, UUIDs: ctKept(kept...)}},
		"a boundary listing an entry that is no uuid": {listing(`"` + kept[0] + `","x","` + kept[1] + `"`),
			threadPreserved{Named: true, Kept: true, Anchor: ctUUID(8), Listed: 3, UUIDs: ctKept(kept[:2]...),
				Nowhere: true}},
		"a boundary naming its segment alone": {segment, threadPreserved{Named: true, Kept: true,
			Anchor: ctUUID(8), Segment: true, Head: ctUUID(3), Tail: ctUUID(6)}},
		"a boundary whose list is null, beside its segment": {ctReplace(t, boundary, listed,
			`,"preservedMessages":null`), threadPreserved{Named: true, Kept: true, Anchor: ctUUID(8),
			Segment: true, Head: ctUUID(3), Tail: ctUUID(6)}},
		"a boundary listing none": {ctReplace(t, segment, `"compactMetadata":{`,
			`"compactMetadata":{"preservedMessages":{"anchorUuid":"`+ctUUID(8)+`","uuids":[]},`),
			threadPreserved{Named: true, Anchor: ctUUID(8)}},
		"a boundary listing none of no anchor": {ctReplace(t, segment, `"compactMetadata":{`,
			`"compactMetadata":{"preservedMessages":{"anchorUuid":"","uuids":[]},`),
			threadPreserved{Named: true}},
		"a boundary keeping nothing": {ctReplace(t, segment, `"preservedSegment":{"headUuid":"`+ctUUID(3)+
			`","anchorUuid":"`+ctUUID(8)+`","tailUuid":"`+ctUUID(6)+`"},`, ""), threadPreserved{}},
		"a boundary whose segment is null": {ctReplace(t, segment, `"preservedSegment":{"headUuid":"`+ctUUID(3)+
			`","anchorUuid":"`+ctUUID(8)+`","tailUuid":"`+ctUUID(6)+`"}`, `"preservedSegment":null`),
			threadPreserved{}},
		"a boundary whose metadata is null": {ctReplace(t, ctReplace(t, boundary, listed, ""),
			`"compactMetadata":{"trigger":"manual"`, `"compactMetadata":null,"was":{"trigger":"manual"`),
			threadPreserved{}},
		"a boundary with no metadata": {ctReplace(t, ctReplace(t, boundary, listed, ""),
			`"compactMetadata":{"trigger":"manual"`, `"was":{"trigger":"manual"`), threadPreserved{}},
		"a microcompaction's boundary carrying a list": {ctReplace(t, boundary, `"subtype":"compact_boundary"`,
			`"subtype":"microcompact_boundary"`), threadPreserved{}},
		"a stop hook's summary carrying a list": {ctReplace(t, ctSummary(ctUUID(15), ctUUID(16)), `"hookCount":1`,
			`"compactMetadata":{"preservedMessages":{"anchorUuid":"`+ctUUID(8)+`","uuids":["`+ctUUID(3)+`"]}},`+
				`"hookCount":1`), threadPreserved{}},
	} {
		p, err := ctLine(t, tc.record).Preserved()
		if got, want := fmt.Sprintf("%+v", p), fmt.Sprintf("%+v", tc.want); err != nil || got != want {
			t.Errorf("%s keeps %s, %v; want %s", name, got, err, want)
		}
	}
	for name, tc := range map[string]struct {
		record string
		// held is what the list holds before its fault.
		held threadPreserved
	}{
		"a metadata that is a string": {record: ctReplace(t, ctReplace(t, boundary, listed, ""),
			`"compactMetadata":{`, `"compactMetadata":"kept","was":{`)},
		"a list that is a string": {record: ctReplace(t, boundary, listed, `,"preservedMessages":"kept"`)},
		"a list's uuids that are a string": {record: ctReplace(t, boundary, listed,
			`,"preservedMessages":{"anchorUuid":"`+ctUUID(8)+`","uuids":"`+ctUUID(3)+`"}`)},
		"a list's uuids holding a number after two strings": {record: listing(`"` + ctUUID(3) + `","x",7,"` +
			ctUUID(5) + `"`), held: threadPreserved{Listed: 2, UUIDs: ctKept(ctUUID(3)), Nowhere: true}},
		"a list of no anchor": {record: ctReplace(t, boundary, listed,
			`,"preservedMessages":{"uuids":["`+ctUUID(3)+`"]}`)},
		"a segment whose head is a number": {record: ctReplace(t, segment, `"headUuid":"`+ctUUID(3)+`"`,
			`"headUuid":3`)},
		"a segment of no tail": {record: ctReplace(t, segment, `,"tailUuid":"`+ctUUID(6)+`"`, "")},
	} {
		p, err := ctLine(t, tc.record).Preserved()
		if got, want := fmt.Sprintf("%+v", p), fmt.Sprintf("%+v", tc.held); err == nil || got != want {
			t.Errorf("%s keeps %s, %v; want %s and an error", name, got, err, want)
		}
	}

	many := make([]string, threadChainMaxRecords+1)
	for i := range many {
		many[i] = `"` + ctUUID(1000+i) + `"`
	}
	for name, tc := range map[string]struct {
		entries      string
		listed       int
		more, absent bool
	}{
		"a list of the bound's uuids and an entry of none": {strings.Join(many[:threadChainMaxRecords], ",") +
			`,"x"`, threadChainMaxRecords + 1, false, true},
		"a list of a uuid past the bound": {strings.Join(many, ","), threadChainMaxRecords + 1, true, false},
	} {
		p, err := ctLine(t, listing(tc.entries)).Preserved()
		if err != nil || p.Listed != tc.listed || p.More != tc.more || p.Nowhere != tc.absent ||
			len(p.UUIDs) != threadChainMaxRecords || p.UUIDs[threadChainMaxRecords-1] !=
			[16]byte(uuid.MustParse(ctUUID(1000+threadChainMaxRecords-1))) {
			t.Errorf("%s lists %d, holds %d uuids, more %t, an entry of none %t, %v; want %d, the bound's first "+
				"%d, %t and %t", name, p.Listed, len(p.UUIDs), p.More, p.Nowhere, err, tc.listed,
				threadChainMaxRecords, tc.more, tc.absent)
		}
	}
}

// A RECORD SAYS WHEN IT WAS WRITTEN: Timestamp reads the form Claude Code
// writes, YYYY-MM-DDTHH:MM:SS.sssZ, of a real time (a leap day of a leap year
// and the first year included), as the milliseconds since the Unix epoch
// Date.parse reads; absent where none; an error for any other form or value:
// a leap day of a year with none, an offset for the Z, no or more milliseconds,
// hour 24, second 60, lowercase z, a space for the T, a signed year, a
// one-digit hour (which package time parses though the form never writes one),
// a number, null.
//
// Mutation: read any RFC 3339 time, skip the check that the time formats back
// to its text, or read null as absent, and a case fails.
func TestARecordSaysWhenItWasWritten(t *testing.T) {
	t.Parallel()
	reply := ctAssistant(ctUUID(3), ctUUID(4), ctUseContent)
	stamped := func(stamp string) string {
		return ctReplace(t, reply, `"timestamp":"`+ctStamp+`"`, `"timestamp":`+stamp)
	}
	for name, tc := range map[string]struct {
		record  string
		ms      int64
		present bool
	}{
		"a reply, as measured":                {reply, 1767323045000, true},
		"the last millisecond of a leap day":  {stamped(`"2028-02-29T23:59:59.999Z"`), 1835481599999, true},
		"the first year":                      {stamped(`"0001-01-01T00:00:00.000Z"`), -62135596800000, true},
		"the harness's bookkeeping, with one": {ctDequeue, 1767323045000, true},
		"a record that carries none":          {ctAtisLatch, 0, false},
	} {
		if ms, present, err := ctLine(t, tc.record).Timestamp(); ms != tc.ms || present != tc.present || err != nil {
			t.Errorf("%s was written at %d (%t), %v; want %d (%t)", name, ms, present, err, tc.ms, tc.present)
		}
	}
	for name, stamp := range map[string]string{
		"a leap day of a year with none": `"2026-02-29T00:00:00.000Z"`,
		"an offset in place of the Z":    `"2026-01-02T03:04:05.000+00:00"`,
		"no milliseconds":                `"2026-01-02T03:04:05Z"`,
		"microseconds":                   `"2026-01-02T03:04:05.000000Z"`,
		"an hour of 24":                  `"2026-01-02T24:00:00.000Z"`,
		"a second of 60":                 `"2026-12-31T23:59:60.000Z"`,
		"a lowercase z":                  `"2026-01-02T03:04:05.000z"`,
		"a space for the T":              `"2026-01-02 03:04:05.000Z"`,
		"a year written with a sign":     `"+026-01-02T03:04:05.000Z"`,
		"an hour of one digit":           `"2026-01-02T3:04:05.000Z"`,
		"the empty string":               `""`,
		"a number":                       `1767323045000`,
		"null":                           `null`,
	} {
		if ms, present, err := ctLine(t, stamped(stamp)).Timestamp(); err == nil {
			t.Errorf("a reply whose timestamp is %s was written at %d (%t), want an error", name, ms, present)
		}
	}
}

// A RECORD SAYS WHETHER IT IS META: IsMeta reads a user record's own isMeta as
// written, true where it is the JSON true (a coordinator's message, a skill's
// text), false where it is false or absent; nothing on a record that is no
// user's, whatever it carries (a compaction's boundary writes false, a reply
// or an attachment may carry true); and an error where the mark is no
// boolean, the string "true" and the number 1 among it, which its readers
// each read their own way: Claude Code's resume as no mark, the reading that
// tells a relaunch which of its calls a kept session holds as the mark.
//
// Mutation: read the mark off every record, and a reply carrying it is meta;
// read it as JavaScript's truth, and the string "true" or the number 1 reads
// as meta with no error; or read null as no mark, and it is no error.
func TestARecordSaysWhetherItIsMeta(t *testing.T) {
	t.Parallel()
	result := ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)
	for name, tc := range map[string]struct {
		record string
		meta   bool
	}{
		"a coordinator's message":        {ctCoordinator(ctUUID(1), ctUUID(4)), true},
		"a skill's text":                 {ctSkillText(ctUUID(4), ctUUID(5)), true},
		"a result marked false":          {ctMarked(t, result, "isMeta", "false"), false},
		"a brief, with none":             {ctBrief(ctUUID(1)), false},
		"a compaction's boundary, false": {ctBoundary(ctUUID(5), ctUUID(6), ctUUID(3)), false},
		"a reply carrying the mark": {ctMarked(t, ctAssistant(ctUUID(3), ctUUID(4), ctUseContent), "isMeta",
			"true"), false},
		"an attachment carrying the mark": {ctAttachment(ctUUID(1), ctUUID(2), ctTokens, `"isMeta":true,`), false},
	} {
		if meta, err := ctLine(t, tc.record).IsMeta(); meta != tc.meta || err != nil {
			t.Errorf("%s reads as meta %t, %v; want %t", name, meta, err, tc.meta)
		}
	}
	for name, mark := range map[string]string{"the string true": `"true"`, "the number 1": `1`, "null": `null`,
		"an object": `{}`} {
		if meta, err := ctLine(t, ctMarked(t, result, "isMeta", mark)).IsMeta(); err == nil {
			t.Errorf("a result whose isMeta is %s reads as meta %t, want an error", name, meta)
		}
	}
}

// A RESULT SAYS THE REPLY IT NAMES AS ITS SOURCE: SourceToolAssistantUUID
// reads a user record's own sourceToolAssistantUUID as the 16 bytes of the
// lowercase canonical uuid it holds, whatever its version; and nothing where
// the key is absent, holds a uuid of another form (an uppercase one, one in
// braces, one without its hyphens, one a hyphen out of place, one of a
// character that is no hex digit), or no string, or the record is no user's.
//
// Mutation: take an uppercase uuid for its bytes, and one names a reply;
// read the key off every record, and a reply carrying it names one; or skip
// the check of the hyphens' places, and a uuid whose hyphens are digits names
// one.
func TestAResultSaysTheReplyItNamesAsItsSource(t *testing.T) {
	t.Parallel()
	result := ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn)
	sourced := func(source string) string {
		return ctReplace(t, result, `"sourceToolAssistantUUID":"`+ctUUID(4)+`"`, `"sourceToolAssistantUUID":`+source)
	}
	other := "f81d4fae-7dec-11d0-a765-00a0c91e6bf6"
	for name, tc := range map[string]struct {
		record string
		want   string
	}{
		"a result, as measured":              {result, ctUUID(4)},
		"a uuid of another version":          {sourced(`"` + other + `"`), other},
		"an uppercase uuid":                  {sourced(`"` + strings.ToUpper(other) + `"`), ""},
		"a uuid in braces":                   {sourced(`"{` + other + `}"`), ""},
		"a uuid without its hyphens":         {sourced(`"` + strings.ReplaceAll(other, "-", "") + `"`), ""},
		"a uuid a hyphen out of place":       {sourced(`"f81d4fa-e7dec-11d0-a765-00a0c91e6bf6"`), ""},
		"a uuid whose hyphens are digits":    {sourced(`"f81d4fae07dec011d00a765000a0c91e6bf6"`), ""},
		"a uuid of a character no hex digit": {sourced(`"g81d4fae-7dec-11d0-a765-00a0c91e6bf6"`), ""},
		"a source that is a number":          {sourced(`7`), ""},
		"a result with none":                 {ctReplace(t, result, `"sourceToolAssistantUUID":"`+ctUUID(4)+`",`, ""), ""},
		"a reply carrying one": {ctMarked(t, ctAssistant(ctUUID(3), ctUUID(4), ctUseContent),
			"sourceToolAssistantUUID", `"`+ctUUID(2)+`"`), ""},
	} {
		got, ok := ctLine(t, tc.record).SourceToolAssistantUUID()
		if tc.want == "" {
			if ok || got != ([16]byte{}) {
				t.Errorf("%s names the reply %x (%t); want none", name, got, ok)
			}
			continue
		}
		if want := uuid.MustParse(tc.want); !ok || got != [16]byte(want) {
			t.Errorf("%s names the reply %x (%t); want %x", name, got, ok, want)
		}
	}
}

// AN ATTACHMENT HANDS THE CALL IT FOLLOWED ONLY AS A KEY: AttachmentToolUseID
// reads an attachment's toolUseID where a string, as the SHA-256 of it, the
// key of the same string made the same way, so a skill's PostToolUse hook
// (whose firing is the id of the call it followed) keys alike with that call's
// id; nothing where it is absent, no string, or the attachment no object, or on
// a non-attachment, a Stop summary's top-level firing included. No id leaves
// the layer: it hands a key, never a string.
//
// Mutation: read a summary's firing at the top, key a toolUseID of any type by
// its bytes, or read keys off a list's elements, and a case fails.
func TestAnAttachmentHandsTheCallItFollowedOnlyAsAKey(t *testing.T) {
	t.Parallel()
	skill, err := ctLine(t, ctAssistant(ctUUID(3), ctUUID(4), `[{"type":"tool_use","id":"`+ctSkillCall+
		`","name":"Skill","input":{}}]`)).Message()
	if err != nil || len(skill.Calls()) != 1 {
		t.Fatalf("a skill's call reads as %+v, %v", skill.Calls(), err)
	}
	for name, tc := range map[string]struct {
		record string
		id     string
		keyed  bool
	}{
		"a batch's hook record":                       {ctAttachment(ctUUID(5), ctUUID(6), ctBatchSuccess, ""), ctBatchFire, true},
		"a skill's PostToolUse hook, keying its call": {ctSkillHook(ctUUID(4), ctUUID(5)), skill.Calls()[0].ID, true},
		"an attachment that names no call":            {ctAttachment(ctUUID(1), ctUUID(2), ctTokens, ctTokensRendered), "", false},
		"an attachment whose toolUseID is a number": {ctAttachment(ctUUID(1), ctUUID(2),
			`{"type":"hook_success","toolUseID":7}`, ""), "", false},
		"an attachment that is a list": {ctAttachment(ctUUID(1), ctUUID(2), `[{"toolUseID":"`+ctCall+`"}]`, ""),
			"", false},
		"a Stop's summary, its firing at the top": {ctSummary(ctUUID(15), ctUUID(16)), "", false},
		"a reply carrying an attachment": {ctReplace(t, ctAssistant(ctUUID(3), ctUUID(4), ctUseContent),
			`"type":"assistant"`, `"attachment":{"toolUseID":"`+ctCall+`"},"type":"assistant"`), "", false},
	} {
		key, keyed := ctLine(t, tc.record).AttachmentToolUseID()
		want := [32]byte{}
		if tc.keyed {
			want = sha256.Sum256([]byte(tc.id))
		}
		if keyed != tc.keyed || key != want {
			t.Errorf("%s keys the call %x (%t); want %x (%t)", name, key, keyed, want, tc.keyed)
		}
	}
}

// A MESSAGE SAYS WHETHER IT SAID ANYTHING, as Claude Code reads its text with
// JavaScript's trim(): Says's text holds where a "text" block holds a string
// trim() leaves something of, and its any where the content is one such string
// or any block of any type holds one. U+FEFF alone, which trim() takes and
// strings.TrimSpace leaves, says nothing; U+0085 alone, the reverse, says
// something; so do a lone surrogate (read as U+FFFD) and a text after a blank
// one. A blank text, thinking block, call, non-string text, empty list and the
// zero message say nothing.
//
// Mutation: trim with strings.TrimSpace, read only "text" blocks for any, take
// any block's text for text, or stop at the first block, and a case fails.
func TestAMessageSaysWhetherItSaidAnything(t *testing.T) {
	t.Parallel()
	said := func(content string) string { return ctAssistant(ctUUID(2), ctUUID(3), content) }
	text := func(s string) string { return said(`[{"type":"text","text":` + s + `}]`) }
	for name, tc := range map[string]struct {
		record    string
		text, any bool
	}{
		"a brief, one string":                 {ctBrief(ctUUID(1)), false, true},
		"a string of white space alone":       {ctReplace(t, ctBrief(ctUUID(1)), `"content":"the brief\n"`, `"content":" \n\t"`), false, false},
		"a text":                              {text(`"batch 1."`), true, true},
		"a text of white space and breaks":    {text(`" \t\n\r\u000b\u000c"`), false, false},
		"a text of U+00A0, U+2028 and U+3000": {text(`"\u00a0\u2028\u3000"`), false, false},
		"a text of U+FEFF alone":              {text(`"\ufeff"`), false, false},
		"a text of U+0085 alone":              {text(`"\u0085"`), true, true},
		"a text of a lone surrogate":          {text(`"\ud800"`), true, true},
		"a thinking block":                    {said(`[{"type":"thinking","thinking":"the batch","signature":"c2ln"}]`), false, false},
		"a call":                              {said(ctUseContent), false, false},
		"another block's text":                {said(`[{"type":"x","text":"hello"}]`), false, true},
		"a text that is no string":            {said(`[{"type":"text","text":7}]`), false, false},
		"a blank text, then a text":           {said(`[{"type":"text","text":" "},{"type":"text","text":"done."}]`), true, true},
		"another block's text, then a text":   {said(`[{"type":"x","text":"hello"},{"type":"text","text":"done."}]`), true, true},
		"an empty list":                       {said(`[]`), false, false},
		"a hook's record, which holds none":   {ctAttachment(ctUUID(5), ctUUID(6), ctBatchSuccess, ""), false, false},
	} {
		m, err := ctLine(t, tc.record).Message()
		if err != nil {
			t.Errorf("%s reads no message: %v", name, err)
			continue
		}
		if saysText, saysAny := m.Says(); saysText != tc.text || saysAny != tc.any {
			t.Errorf("%s says text %t and anything %t; want %t and %t", name, saysText, saysAny, tc.text, tc.any)
		}
	}
}

// ctWithheld is a withheld_memory attachment of entries, each a JSON value, as
// 2.1.322 writes one, abridged to the keys its resume reads.
func ctWithheld(entries ...string) string {
	return `{"type":"withheld_memory","by":"a Read deny rule","entries":[` + strings.Join(entries, ",") + `]}`
}

// AN ATTACHMENT SAYS WHETHER THE RESUME KEEPS IT: AttachmentWellFormed reads a
// payload by Claude Code's rules for the types it holds to a shape, each at its
// edge: invoked_skills' every skill an object or a list; hook_success's content
// a string; skill_listing's names absent or at most 4,096 strings of at most
// 512 UTF-16 code units (counted so, not as bytes or runes); a
// hook_additional_context's a list of strings; task_reminder's and
// todo_reminder's a list of objects; file's and already_read_file's an object;
// withheld_memory's a non-empty string and entries of a string path,
// displayPath and one of the harness's four reasons, never an inherited key;
// any other type kept; no payload, a non-object or no string type dropped; a
// non-attachment kept. A name past the bound holding non-UTF-8 is an error.
// Mutation: drop none, count bytes or runes, check names before the list's length, or accept any typeof-object or key.
func TestAnAttachmentSaysWhetherTheResumeKeepsIt(t *testing.T) {
	t.Parallel()
	attached := func(payload string) string { return ctAttachment(ctUUID(1), ctUUID(2), payload, "") }
	listing := func(names ...string) string {
		quoted := make([]string, len(names))
		for i, name := range names {
			quoted[i] = strconv.Quote(name)
		}
		return attached(`{"type":"skill_listing","names":[` + strings.Join(quoted, ",") + `]}`)
	}
	many := func(n int) []string { return slices.Repeat([]string{"a"}, n) }
	entry := func(why string) string { return `{"path":"a","why":` + why + `,"displayPath":"a"}` }
	for name, tc := range map[string]struct {
		record string
		kept   bool
	}{
		"invoked skills of objects and lists":        {attached(`{"type":"invoked_skills","skills":[{},[]]}`), true},
		"invoked skills of none":                     {attached(`{"type":"invoked_skills","skills":[]}`), true},
		"invoked skills holding null":                {attached(`{"type":"invoked_skills","skills":[{},null]}`), false},
		"invoked skills holding a number":            {attached(`{"type":"invoked_skills","skills":[1]}`), false},
		"invoked skills that are an object":          {attached(`{"type":"invoked_skills","skills":{}}`), false},
		"invoked skills absent":                      {attached(`{"type":"invoked_skills"}`), false},
		"a hook's success, as measured":              {attached(ctBatchSuccess), true},
		"a hook's success of numeric content":        {attached(`{"type":"hook_success","content":7}`), false},
		"a hook's success of null content":           {attached(`{"type":"hook_success","content":null}`), false},
		"a hook's success of no content":             {attached(`{"type":"hook_success","stdout":""}`), false},
		"a skill listing of no names":                {attached(`{"type":"skill_listing"}`), true},
		"a skill listing of null names":              {attached(`{"type":"skill_listing","names":null}`), false},
		"a skill listing of names that are a string": {attached(`{"type":"skill_listing","names":"a"}`), false},
		"a skill listing of none":                    {listing(), true},
		"a skill listing of 4,096 names":             {listing(many(4096)...), true},
		"a skill listing of 4,097 names":             {listing(many(4097)...), false},
		"a name of 512 units":                        {listing(strings.Repeat("a", 512)), true},
		"a name of 513 units":                        {listing(strings.Repeat("a", 513)), false},
		"a name of 256 runes past the basic plane":   {listing(strings.Repeat("\U0001F600", 256)), true},
		"a name of 257 runes past the basic plane":   {listing(strings.Repeat("\U0001F600", 257)), false},
		"a name of 300 runes of two bytes":           {listing(strings.Repeat("\u00e9", 300)), true},
		"a name that is a number":                    {attached(`{"type":"skill_listing","names":["a",7]}`), false},
		"4,097 names, one past the bound and no UTF-8": {attached(`{"type":"skill_listing","names":["` +
			strings.Repeat("\xff", 600) + `"` + strings.Repeat(`,"a"`, 4096) + `]}`), false},
		"a short name holding a byte that is no UTF-8": {attached(`{"type":"skill_listing","names":["a` + "\xff" +
			`b"]}`), true},
		"an additional context, as measured":           {attached(ctBatchContext), true},
		"an additional context of none":                {attached(`{"type":"hook_additional_context","content":[]}`), true},
		"an additional context of a number":            {attached(`{"type":"hook_additional_context","content":["a",7]}`), false},
		"an additional context that is a string":       {attached(`{"type":"hook_additional_context","content":"a"}`), false},
		"an additional context of no content":          {attached(`{"type":"hook_additional_context"}`), false},
		"a task reminder of objects":                   {attached(`{"type":"task_reminder","content":[{}]}`), true},
		"a task reminder of a list":                    {attached(`{"type":"task_reminder","content":[[]]}`), false},
		"a task reminder of null":                      {attached(`{"type":"task_reminder","content":[null]}`), false},
		"a todo reminder of a string":                  {attached(`{"type":"todo_reminder","content":["a"]}`), false},
		"a todo reminder of no content":                {attached(`{"type":"todo_reminder"}`), false},
		"a file of an object":                          {attached(`{"type":"file","content":{}}`), true},
		"a file of a list":                             {attached(`{"type":"file","content":[]}`), false},
		"a file read already, of a string":             {attached(`{"type":"already_read_file","content":"a"}`), false},
		"a file of no content":                         {attached(`{"type":"file"}`), false},
		"a withheld memory":                            {attached(ctWithheld(entry(`"denied"`), entry(`"unsettled"`))), true},
		"a withheld memory of none":                    {attached(ctWithheld()), true},
		"a withheld memory of an empty by":             {attached(`{"type":"withheld_memory","by":"","entries":[]}`), false},
		"a withheld memory of no by":                   {attached(`{"type":"withheld_memory","entries":[]}`), false},
		"a withheld memory whose why is constructor":   {attached(ctWithheld(entry(`"constructor"`))), false},
		"a withheld memory whose why is a number":      {attached(ctWithheld(entry(`7`))), false},
		"a withheld memory of an entry that is a list": {attached(ctWithheld(`[]`)), false},
		"a withheld memory of no displayPath":          {attached(ctWithheld(`{"path":"a","why":"denied"}`)), false},
		"a withheld memory of entries that are none":   {attached(`{"type":"withheld_memory","by":"x"}`), false},
		"a reminder of tokens, a type of no rule":      {attached(ctTokens), true},
		"a payload of no type":                         {attached(`{"content":"a"}`), false},
		"a payload whose type is a number":             {attached(`{"type":7}`), false},
		"a payload that is a list":                     {attached(`[{"type":"file"}]`), false},
		"a payload that is a string":                   {attached(`"file"`), false},
		"a payload that is null":                       {attached(`null`), false},
		"an attachment record of none":                 {ctReplace(t, attached(ctTokens), `"attachment":`+ctTokens+`,`, ""), false},
		"a reply, which is no attachment":              {ctAssistant(ctUUID(3), ctUUID(4), ctUseContent), true},
	} {
		if kept, err := ctLine(t, tc.record).AttachmentWellFormed(); kept != tc.kept || err != nil {
			t.Errorf("%s is kept %t, %v; want %t", name, kept, err, tc.kept)
		}
	}
	unreadable := attached(`{"type":"skill_listing","names":["` + strings.Repeat("\xff", 600) + `"]}`)
	if kept, err := ctLine(t, unreadable).AttachmentWellFormed(); err == nil {
		t.Errorf("a name past the bound holding bytes that are no UTF-8 is kept %t, want an error", kept)
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

// A REPLY SAYS WHAT ITS REQUEST TOOK: an assistant record's message id, model
// and the usage's four counts as written, ok saying the usage is there. An
// unnamed count reads 0. A reply with no usage hands its id and model still,
// counts 0, ok false, as Claude Code takes an id with no usage; no id or model
// reads "", and the reply a resume writes reads all 0 under a model of its
// own. Any non-assistant record, a result or a request blob carrying a usage
// included, reads nothing. A usage of the wrong shape is an error, never a
// figure: a non-object or null; a count not a whole JSON number (negative,
// fractional, exponent, string, null, boolean, object) or past int64; an id or
// model not a string; a message not an object. Mutation: answer for every type,
// read null as none or 0, accept any number, hand id and model only with a
// usage, swap counts, require every count, or read the id by text alone.
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
// usage lists iterations, Claude Code counts the last entry that is no
// advisor's or compaction's in the usage's place, if whole (type message or
// fallback_message, four counts each finite and >= 0, input side above zero);
// Usage, handing the usage's own counts, then errors where that entry holds
// other counts, never Message. Where Claude Code counts the usage's own, Usage
// hands them: no, null, non-list or empty iterations, only advisor or
// compaction entries, a counted entry equal to the usage (fractions included),
// a last entry not whole, and a usage whose own input side is zero.
// Mutation: leave the list unread, count an advisor or compaction entry, the
// first not the last entry, or no fallback_message, read the list where the
// input side is zero, or count a zero-input, other-typed or negative entry.
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

// A REPLY'S ITERATIONS COST ITS USAGE AN ENTRY AT A TIME: Usage reads the list
// an entry at a time and keeps a copy of one, the last it may count, so a usage
// near [threadValueMax] of uncounted entries costs no more allocations than a
// list of one and no more bytes than a few usage copies. A usage past
// [threadValueMax] is no reading: the line ends it ([errThreadBound]).
//
// Not parallel: the counts are the process's.
//
// Mutation: decode the list whole, or collect its entries, and a long list
// costs an allocation per entry; drop the bound, and the line past it reads.
func TestAReplysIterationsCostItsUsageAnEntryAtATime(t *testing.T) {
	const (
		head = `{"type":"assistant","message":{"role":"assistant","content":[],"usage":{"input_tokens":3,` +
			`"iterations":[`
		tail = `]}}}`
	)
	usage := func(l threadLine) func() { return func() { _, _, _ = l.Usage() } }
	one := ctLine(t, head+"1"+tail)
	// The usage of a line of threadValueMax bytes is within its bound by the
	// keys around it.
	longRecord := ctListRecord(t, head, "1", tail, threadValueMax)
	long := ctLine(t, longRecord)
	past := ctRead(ctThread(ctListRecord(t, head, "1", tail, threadValueMax+1<<10)), threadPlainMax)
	if len(past.lines) != 0 || !errors.Is(past.end, errThreadBound) {
		t.Errorf("a usage past %d bytes reads as %d lines, ended by %v; want none, and %v", threadValueMax,
			len(past.lines), past.end, errThreadBound)
	}
	for name, l := range map[string]threadLine{"a list of one": one, "a long list": long} {
		if u, ok, err := l.Usage(); !ok || err != nil || u.Input != 3 {
			t.Fatalf("%s of entries no harness counts says its request took %+v (%t), %v; want its usage's own", name,
				u, ok, err)
		}
	}
	fewObjects, fewBytes := allocationsOf(t, usage(one))
	manyObjects, manyBytes := allocationsOf(t, usage(long))
	if manyObjects > fewObjects+listSlack {
		t.Errorf("a usage listing %d bytes of iterations costs %d allocations, and one listing one entry %d",
			len(longRecord), manyObjects, fewObjects)
	}
	if manyBytes > fewBytes+listCopies*uint64(len(longRecord)) {
		t.Errorf("a usage listing %d bytes of iterations costs %d bytes, and one listing one entry %d: more than %d "+
			"copies of the line", len(longRecord), manyBytes, fewBytes, listCopies)
	}
}

// A LIST IS WALKED AN ELEMENT AT A TIME: each element is handed out as it is
// read, in order and as written but for surrounding whitespace, with its index,
// so the list holds those very bytes there; the walk stops at the callback's
// first error, returns it unwrapped, and reads no element after it, so later
// non-JSON elements do not change it; a non-list, a cut list, a non-JSON
// element and trailing data are each the walk's own error, never io.EOF. An
// empty list hands out nothing.
//
// Mutation: read the list whole first, hand out an element's end for its index,
// wrap the callback's error, walk on past it, take any value for a list, or
// ignore what follows a list, and a case fails.
func TestAListIsWalkedAnElementAtATime(t *testing.T) {
	t.Parallel()
	walk := func(list string, stopAt int, stop error) ([]string, error) {
		var handed []string
		err := eachElement(json.RawMessage(list), func(element json.RawMessage, at int) error {
			if at < 0 || at+len(element) > len(list) || list[at:at+len(element)] != string(element) {
				t.Errorf("%q hands out %s as sitting at %d", list, element, at)
			}
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

// AN OBJECT IS WALKED A KEY AT A TIME: each key is handed out as it is read, in
// order, its name unquoted as the layer's options read it (escapes resolved, a
// lone surrogate or non-UTF-8 byte as U+FFFD, case as written), its value as
// written but for surrounding whitespace, with its index, so the object holds
// those very bytes there; a key written twice is handed out both times; the
// walk stops at the callback's first error, returns it unwrapped, and reads no
// key after it; a non-object, a cut object, a non-string key, a non-JSON value
// and trailing data are each the walk's own error, never io.EOF. An empty
// object hands out nothing.
// Mutation: read the object whole first, hand a name out as written, an end for
// an index, fold repeated keys, wrap the callback's error, walk on past it, or
// take any value for an object, and a case fails.
func TestAnObjectIsWalkedAKeyAtATime(t *testing.T) {
	t.Parallel()
	type key struct{ name, value string }
	walk := func(object string, stopAt int, stop error) ([]key, error) {
		var handed []key
		err := eachKey(json.RawMessage(object), func(name []byte, value json.RawMessage, at int) error {
			if at < 0 || at+len(value) > len(object) || object[at:at+len(value)] != string(value) {
				t.Errorf("%q hands out %s's value %s as sitting at %d", object, name, value, at)
			}
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

// A CONTENT LIST COSTS ONE BLOCK AT A TIME: a message's content is read a block
// at a time in its line's one pass, each held to its type as read, and a
// reply's applied context edits a block at a time off the report its line
// keeps. A list refused at its first block costs what a list of that block
// alone costs, at 8 MiB of `1`s or of empty objects: at most 16 KiB more, never
// a copy of the list nor a header per element. An edits list near
// [threadValueMax] costs ToolResultsCleared no more allocations than one block
// and no more bytes than two copies of the list; a walk nobody keeps costs the
// same for a long list as for one. A list read to its end leaves held what
// Message says: a call per call block, the array at most twice them, the bytes
// of each id. Not parallel: the counts are the process's.
// Mutation: copy pieces, read past a refused block, decode edits whole, collect the list, or grow calls past twice.
func TestAContentListCostsOneBlockAtATime(t *testing.T) {
	ctx := t.Context()
	// slack is how many more allocations a long list's edits may cost than a
	// list of one, though they read no more of it: a collection while it is
	// measured empties the process's pools (fmt's printers, the decoders),
	// which the call builds afresh, eleven allocations in all where measured,
	// and the race detector drops a quarter of what is put in a pool.
	const slack = 64
	user := `{"type":"user","message":{"role":"user","content":`
	for name, element := range map[string]string{"numbers": "1", "empty objects": "{}"} {
		line := func(n int) []scanPart { return scanLine(scanLit(user), scanList(element, n), scanLit(`}}`)) }
		n := (8 << 20) / (len(element) + 1)
		oneCost, _, oneErr := scanAllocated(ctx, line(1), line(1), 3)
		cost, read, err := scanAllocated(ctx, line(1), line(n), 2)
		if _, refused := read.Message(); oneErr != nil || err != nil || refused == nil {
			t.Fatalf("a content list of %d %s reads as %v, its message %v, and one of one as %v; want records whose "+
				"message is refused", n, name, err, refused, oneErr)
		}
		if cost > oneCost+16<<10 {
			t.Errorf("a content list of %d %s, refused at its first block, costs its line's reading %d bytes, and a "+
				"list of one %d: more than 16 KiB more", n, name, cost, oneCost)
		}
	}
	reply := `{"type":"assistant","message":{"role":"assistant","content":[],"context_management":{"applied_edits":[`
	// cleared reads the report a reply's line keeps: what it costs is the
	// read alone, of a line read before it is measured.
	cleared := func(record string) func() error {
		l := ctLine(t, record)
		return func() error {
			_, err := l.ToolResultsCleared()
			return err
		}
	}
	edits := ctListRecord(t, reply, "1", `]}}}`, threadValueMax-64)
	one, full := cleared(reply+`1]}}}`), cleared(edits)
	if one() == nil || full() == nil {
		t.Fatalf("an edits list whose first block is none reads with no error")
	}
	oneObjects, oneBytes := allocationsOf(t, func() { _ = one() })
	fullObjects, fullBytes := allocationsOf(t, func() { _ = full() })
	if fullObjects > oneObjects+slack || fullBytes > oneBytes+2*uint64(len(edits)) {
		t.Errorf("refused at its first block, an edits list of %d bytes costs %d allocations and %d bytes, and a "+
			"list of one %d and %d: more than a copy of the list more", len(edits), fullObjects, fullBytes,
			oneObjects, oneBytes)
	}

	// The walk of a list of listBytes, the stream's and a kept report's.
	oneList := json.RawMessage(`[1]`)
	longList := json.RawMessage(ctListRecord(t, "[", "1", "]", listBytes))
	elements := 0
	count := func(json.RawMessage, int) error {
		elements++
		return nil
	}
	if err := eachElement(longList, count); err != nil || elements != strings.Count(string(longList), "1") {
		t.Fatalf("a list of %d bytes walks as %d elements, %v; want every one of its %d", len(longList), elements,
			err, strings.Count(string(longList), "1"))
	}
	keepNone := func(json.RawMessage, int) error { return nil }
	fewObjects, fewBytes := allocationsOf(t, func() { _ = eachElement(oneList, keepNone) })
	manyObjects, manyBytes := allocationsOf(t, func() { _ = eachElement(longList, keepNone) })
	if manyObjects > fewObjects+slack || manyBytes > fewBytes+uint64(len(longList)) {
		t.Errorf("a walk of a list of %d bytes costs %d allocations and %d bytes, and of a list of one %d and %d: "+
			"more than the list's own size more", len(longList), manyObjects, manyBytes, fewObjects, fewBytes)
	}

	// heldSlack is how many bytes a message read to its end may hold beyond its
	// calls: the message itself, and whatever else the heap holds at the
	// second collection that it did not at the first. Measured, a message held
	// its array of calls and nothing more to within a few dozen bytes; a copy
	// of the list is the list's size, and a kind's header for each of its
	// blocks a dozen times the slack.
	const heldSlack = 64 << 10
	for name, tc := range map[string]struct {
		element string
		// own is how many bytes of its own each call may hold beyond its
		// fields: its id. A call's kind is the layer's own constant
		// (TestAKnownBlockKindIsHeldAsTheLayersOwnString).
		own   uint64
		calls bool
	}{
		"blocks of a kind no reader names": {element: `{"type":"x"}`},
		"the smallest results":             {element: `{"type":"tool_result","tool_use_id":"a"}`, own: 1, calls: true},
	} {
		// Half of listBytes: a line of the smallest results holds 16,384
		// calls within threadLineMax, and this one holds half of them.
		record := []byte(ctListRecord(t, user+"[", tc.element, `]}}`, listBytes/2))
		read := func() any {
			m, _ := ctLine(t, string(record)).Message()
			return m
		}
		m, err := ctLine(t, string(record)).Message()
		blocks, calls := uint64(strings.Count(string(record), tc.element)), uint64(0)
		if tc.calls {
			calls = blocks
		}
		if err != nil || uint64(m.Blocks().Len()) != blocks || uint64(len(m.Calls())) != calls {
			t.Fatalf("%s: a list of %d blocks reads as %d blocks and %d calls, %v; want %d and %d", name, blocks,
				m.Blocks().Len(), len(m.Calls()), err, blocks, calls)
		}
		fields := calls * (uint64(unsafe.Sizeof(threadCall{})) + tc.own)
		if held, limit := heldBy(t, read), fields*2+heldSlack; held > limit {
			t.Errorf("%s: a list of %d bytes read to its end holds %d bytes, more than its %d calls of %d blocks "+
				"take: %d", name, len(record)+1, held, calls, blocks, limit)
		}
	}
}

// AN OBJECT'S WALK COSTS WHAT ONE KEY COSTS: a walk nobody keeps costs an object
// of tens of thousands of keys the allocations one key costs, and no byte more
// than the object's size, plain or escaped names alike: it never collects the
// keys, and hands each name out in a reused buffer, never a string or copy,
// either of which allocates per key.
//
// Not parallel: the counts are the process's.
//
// Mutation: collect the keys first, or hand out a copy of each name, and an
// object of listBytes costs an allocation or more per key.
func TestAnObjectsWalkCostsWhatOneKeyCosts(t *testing.T) {
	keepNone := func([]byte, json.RawMessage, int) error { return nil }
	for name, key := range map[string]func(int) string{
		"names written plainly": func(i int) string { return fmt.Sprintf(`"m%06x":{}`, i) },
		"names escaped":         func(i int) string { return fmt.Sprintf(`"\u006d%06x":{}`, i) },
	} {
		one := json.RawMessage(`{` + key(0) + `}`)
		long, n := ctKeysRecord(t, `{`, key, `}`, listBytes)
		object := json.RawMessage(long)
		keys := 0
		if err := eachKey(object, func([]byte, json.RawMessage, int) error {
			keys++
			return nil
		}); err != nil || keys != n {
			t.Fatalf("%s: an object of %d keys walks as %d keys, %v", name, n, keys, err)
		}
		fewObjects, fewBytes := allocationsOf(t, func() { _ = eachKey(one, keepNone) })
		manyObjects, manyBytes := allocationsOf(t, func() { _ = eachKey(object, keepNone) })
		if manyObjects > fewObjects+listSlack || manyBytes > fewBytes+uint64(len(object)) {
			t.Errorf("%s: a walk of an object of %d keys costs %d allocations and %d bytes, and of an object of "+
				"one %d and %d: more than the object's own size more", name, n, manyObjects, manyBytes, fewObjects,
				fewBytes)
		}
	}
}

// A KNOWN BLOCK KIND IS HELD AS THE LAYER'S OWN STRING: a call's or result's
// kind is the layer's own constant, so a list of calls holds no string of its
// own for it; every block is counted in Blocks and held as its kind's member of
// a set, nothing more: each kind the layer names (text, thinking, redacted
// thinking, call, result) its own, any other kind one no reader can ask for, so
// a reply holding a server's call among its text and thinking is not all the
// named kinds.
//
// Mutation: hold a call's kind as decoded, or read an unnamed kind as a named
// one or match it by name, and a case fails.
func TestAKnownBlockKindIsHeldAsTheLayersOwnString(t *testing.T) {
	t.Parallel()
	named := `[{"type":"text","text":"batch 1."},{"type":"thinking","thinking":"the batch","signature":"c2ln"},` +
		`{"type":"redacted_thinking","data":"ZGF0YQ=="},` + ctUseContent[1:len(ctUseContent)-1]
	server := `{"type":"server_tool_use","id":"srvtoolu_test","name":"web_search","input":{}}`
	every := []string{blockText, blockThinking, blockRedactedThinking, blockToolUse, blockToolResult}
	for name, tc := range map[string]struct {
		record string
		// blocks is the message's blocks as [ctBlocks] prints them; all
		// whether they are all of the kinds the layer names.
		blocks string
		all    bool
	}{
		"a reply of every kind the layer names": {record: ctAssistant(ctUUID(2), ctUUID(3), named+`]`),
			blocks: "4 [tool_use text thinking redacted_thinking]", all: true},
		"a reply holding a server's call beside them": {record: ctAssistant(ctUUID(2), ctUUID(3),
			named+`,`+server+`]`), blocks: "5 [tool_use text thinking redacted_thinking other]"},
		"a result": {record: ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn),
			blocks: "1 [tool_result]", all: true},
	} {
		m, err := ctLine(t, tc.record).Message()
		if err != nil {
			t.Errorf("%s reads no message: %v", name, err)
			continue
		}
		b := m.Blocks()
		if ctBlocks(b) != tc.blocks || b.All(every...) != tc.all || b.All(append(every, "server_tool_use")...) != tc.all {
			t.Errorf("%s reads as the blocks %s, all of the named kinds %t, and with the server's call's beside "+
				"them %t; want %s and %t", name, ctBlocks(b), b.All(every...),
				b.All(append(every, "server_tool_use")...), tc.blocks, tc.all)
		}
		if len(m.Calls()) != 1 {
			t.Errorf("%s holds the calls %+v; want one", name, m.Calls())
			continue
		}
		kind := m.Calls()[0].Kind
		if own := map[string]string{blockToolUse: blockToolUse, blockToolResult: blockToolResult}[kind]; own == "" ||
			unsafe.StringData(kind) != unsafe.StringData(own) {
			t.Errorf("%s holds a call of the kind %q, not as the layer's own string", name, kind)
		}
	}
}

// A SERVER'S CLEARING IS WHAT AN ASSISTANT RECORD SAYS IT APPLIED: a model's
// reply whose message reports the clearing of old tool results with a count
// above none among the context edits the server applied. Nothing else is one:
// a count of none, another edit (even with the clearing's count), no edit, an
// empty or null report (a resume's reply), a reply as measured with no report,
// or the clearing on a non-reply record. A report of the wrong shape (neither
// object nor null, non-list edits, a non-object edit or non-string type, a
// non-number count) is an error, wherever the bad edit falls, never none, and
// costs this accessor alone; so is a reply whose message is no object, null
// included. Mutation: count none, read counts off every edit, answer for every
// type, refuse a null report or let a null message through, stop at the first
// clearing, or hold the report in Message, and a case fails.
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
		if m, err := line.Message(); err != nil || ctBlocks(m.Blocks()) != "1 [text]" {
			t.Errorf("%s reads its message as the blocks %s, %v; want it whole", name, ctBlocks(m.Blocks()), err)
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

// MEMBERS ARE NAMED EXACTLY: the session's main thread, a subagent's thread and
// its metadata, each named from a session's id and an agent's, and read back
// from that name and no other. Another session's file (which a person's
// transcript takes for a main thread), a deeper path, an uncleaned name,
// `agent-.jsonl` and an agent's id outside the rule (1 to 64 bytes of letters,
// digits, `-`, `_`) name no member, and a session id outside
// sandbox.ValidSessionID's shape names none either way, nor reads back as the
// session its own spelling would make (`projects/crewlet/a/b.jsonl` for `a/b`).
//
// Mutation: read a name through claudeThread, and another session's file reads
// as this main thread; drop the session's check, and `a/b` names a member.
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

// A SAVED COMPACTION IS NAMED BY BOTH NAMES CLAUDE CODE READS IT BY: the one it
// writes, in the session's directory, and the sibling beside that directory, in
// the order it reads them, each read back as the session's saved compaction.
// A name beside either (a temporary one, another extension, none with a
// separator), either under the subagents' directory, either of another session
// or project, and an uncleaned name name none; a session id outside
// sandbox.ValidSessionID's shape names none, nor reads back as the session its
// own spelling would make.
//
// Mutation: name the first alone, swap the two, read either back as no member,
// match by last element, or drop the session's check, and a case fails.
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

// A LINE THAT HOLDS NO FACTS READS AS A RECORD OF NO KEYS: every accessor reads
// a line whose facts are nil, as the zero line's are, exactly as it reads the
// decode of a record that carries its type and subtype and nothing else,
// whatever that type, so a helper or a reader that hands out the zero line
// never hands out one an accessor cannot read.
//
// Mutation: read the facts of a line that holds none as they lie, and the
// first accessor that reads one panics.
func TestALineThatHoldsNoFactsReadsAsARecordOfNoKeys(t *testing.T) {
	t.Parallel()
	for _, head := range []string{
		`{}`, `{"type":"queue-operation"}`, `{"type":"system","subtype":"compact_boundary"}`,
		`{"type":"system","subtype":"model_refusal_fallback"}`,
	} {
		want := ctLine(t, head)
		got := threadLine{N: want.N, Head: want.Head}
		if ctAll(got) != ctAll(want) {
			t.Errorf("a line of no facts with the head of %s reads as\n%s\nwant\n%s", head, ctAll(got), ctAll(want))
		}
	}
	for _, kind := range recordKinds {
		head := `{"type":"` + string(kind) + `"}`
		want := ctLine(t, head)
		got := threadLine{N: want.N, Head: want.Head}
		if ctAll(got) != ctAll(want) {
			t.Errorf("a line of no facts with the head of %s reads as\n%s\nwant\n%s", head, ctAll(got), ctAll(want))
		}
	}
	if got := ctAll(threadLine{}); got != ctAll(threadLine{Head: ctLine(t, `{}`).Head}) {
		t.Errorf("the zero line reads as\n%s", got)
	}
}

// The takes the probes' notes' hooks took, the firings that took them
// (2.1.310's and 2.1.322's kept threads, ctProbeThreads), and one take no
// firing took.
const (
	ctProbeBatchTake  = "00000000-0000-4000-8000-000000000020"
	ctProbeBatchFire  = "hook-00000000-0000-4000-8000-000000000019"
	ctProbeStopTake   = "00000000-0000-4000-8000-000000000028"
	ctProbeStopFire   = "00000000-0000-4000-8000-000000000027"
	ctProbeUntakenOne = "00000000-0000-4000-8000-0000000000ff"
)

// ctProbeThreads are the main threads the probes kept of a session whose hooks
// took a note at a batch and at Stop, under testdata/claude.
var ctProbeThreads = []string{
	"2.1.310/holdings/s1/projects/crewlet/5e55100a-0000-4000-8000-000000000106.jsonl",
	"2.1.322/holdings/s1/projects/crewlet/5e55100a-0000-4000-8000-000000000113.jsonl",
}

// A HOOK RECORD NAMES ONLY THE TAKES ITS READER ASKED, each in the words that
// carry it: over the probes' kept threads, asked the batch's take, the Stop's
// and one no firing took, each firing's system message names its take in its
// content alone and its success in its stdout alone, and no other record
// names any; asked nothing, nothing is named. A token is met as decoded (an
// escape) and where a cut splits it; one in a context's list, another hook
// record's stdout or another attachment's content names nothing.
//
// Mutation: match a token by its prefix alone, and the take no firing took is
// named; read a success's stdout into its content's takes, or read a
// context's list, any record's stdout, the raw bytes or a skill listing, and
// a record names takes it should not, or misses one.
func hookRecordNamesOnlyTheTakesItWasAsked(t *testing.T) {
	t.Parallel()
	asked := []string{claudeTakeToken(ctProbeBatchTake), claudeTakeToken(ctProbeStopTake),
		claudeTakeToken(ctProbeUntakenOne)}
	firings := map[[32]byte]uint64{
		sha256.Sum256([]byte(ctProbeBatchFire)): 1 << 0,
		sha256.Sum256([]byte(ctProbeStopFire)):  1 << 1,
	}
	for _, thread := range ctProbeThreads {
		raw, err := os.ReadFile(filepath.Join("testdata", "claude", thread))
		if err != nil {
			t.Fatal(err)
		}
		for _, ask := range [][]string{asked, nil} {
			r := newThreadReader(bytes.NewReader(raw), threadPlainMax)
			if err := r.Asking(ask); err != nil {
				t.Fatal(err)
			}
			named := 0
			for {
				line, err := r.Next(t.Context())
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("%s: %v", thread, err)
				}
				kind, _ := line.AttachmentType()
				firing, _ := line.AttachmentToolUseID()
				var wantContent, wantStdout uint64
				switch {
				case ask == nil:
				case kind == hookSystemMessage:
					wantContent = firings[firing]
				case kind == hookSuccess:
					wantStdout = firings[firing]
				}
				content, stdout := line.HookTakes()
				if content != wantContent || stdout != wantStdout {
					t.Errorf("%s, asked %d takes: line %d, a %q record, names the takes %b in its content and %b "+
						"in its stdout; want %b and %b", thread, len(ask), line.N, kind, content, stdout, wantContent,
						wantStdout)
				}
				if content|stdout != 0 {
					named++
				}
			}
			if want := 4 * min(len(ask), 1); named != want {
				t.Errorf("%s, asked %d takes: %d records name one; want %d, each firing's system message and "+
					"success", thread, len(ask), named, want)
			}
		}
	}
	escaped := `\u005bcrewlet take ` + ctProbeStopTake + `]`
	for name, tc := range map[string]struct {
		attachment      string
		content, stdout uint64
	}{
		"a system message whose token is escaped": {ctReplace(t, ctStopSystem, `finishing"`,
			`finishing `+escaped+`"`), 1 << 1, 0},
		"a system message carrying a stdout": {ctReplace(t, ctStopSystem, `"hookName"`,
			`"stdout":"`+asked[1]+`","hookName"`), 0, 0},
		"an additional context listing a token": {ctReplace(t, ctStopContext, `["## A note"]`,
			`["## A note `+asked[1]+`"]`), 0, 0},
		"a skill listing naming a token": {`{"type":"skill_listing","content":"- ` + asked[1] + `"}`, 0, 0},
	} {
		line := ctAsked(t, ctAttachment(ctUUID(1), ctUUID(2), tc.attachment, ""), asked)
		if content, stdout := line.HookTakes(); content != tc.content || stdout != tc.stdout {
			t.Errorf("%s names the takes %b in its content and %b in its stdout; want %b and %b", name, content,
				stdout, tc.content, tc.stdout)
		}
	}
	text := "crewlet: a note reached the run as it was finishing " + asked[1]
	for cut := 1; cut < len(text); cut++ {
		takes := textTakes{asked: asked}
		takes.write([]byte(text[:cut]))
		takes.write([]byte(text[cut:]))
		if takes.bits != 1<<1 {
			t.Errorf("a system message cut after %d bytes names the takes %b; want the Stop's alone", cut, takes.bits)
		}
	}
}

// A HOOK'S EVENT IS HELD TO ITS TYPE BY ITS OWN ACCESSOR: a string is the
// event, an absent key none, and any other value HookEvent's error alone,
// which costs the record nothing else: its type, its firing and the shape its
// resume keeps it by read as they do with its event a string (the resume's
// drop of a record is the chain's to judge, never this accessor's). Every
// other record has no event.
//
// Mutation: fold the event's error into the attachment's, and a success whose
// event is a number is no attachment its resume keeps.
func hooksEventIsHeldToItsType(t *testing.T) {
	t.Parallel()
	const event = `"hookEvent":"PostToolBatch",`
	for name, tc := range map[string]struct {
		event, want string
		fails       bool
	}{
		"a string":  {event: event, want: "PostToolBatch"},
		"absent":    {event: ""},
		"a number":  {event: `"hookEvent":7,`, fails: true},
		"null":      {event: `"hookEvent":null,`, fails: true},
		"an object": {event: `"hookEvent":{"name":"PostToolBatch"},`, fails: true},
		"a list":    {event: `"hookEvent":["PostToolBatch"],`, fails: true},
	} {
		line := ctLine(t, ctAttachment(ctUUID(1), ctUUID(2), ctReplace(t, ctBatchSuccess, event, tc.event), ""))
		got, err := line.HookEvent()
		if got != tc.want || (err != nil) != tc.fails {
			t.Errorf("a success whose hookEvent is %s reads the event %q, %v; want %q, failing %t", name, got, err,
				tc.want, tc.fails)
		}
		kind, kindErr := line.AttachmentType()
		firing, keyed := line.AttachmentToolUseID()
		formed, formedErr := line.AttachmentWellFormed()
		if kind != hookSuccess || kindErr != nil || !keyed || firing != sha256.Sum256([]byte(ctBatchFire)) ||
			!formed || formedErr != nil {
			t.Errorf("a success whose hookEvent is %s reads as a %q record, %v, keyed %t, kept %t, %v; want a "+
				"success of its firing its resume keeps", name, kind, kindErr, keyed, formed, formedErr)
		}
	}
	if got, err := ctLine(t, ctBrief(ctUUID(1))).HookEvent(); got != "" || err != nil {
		t.Errorf("a prompt reads the hook event %q, %v; want none", got, err)
	}
}

// ctAsked is record read as the one line of a thread whose reader asks tokens.
func ctAsked(t *testing.T, record string, tokens []string) threadLine {
	t.Helper()
	r := newThreadReader(strings.NewReader(ctThread(record)), threadPlainMax)
	if err := r.Asking(tokens); err != nil {
		t.Fatal(err)
	}
	line, err := r.Next(t.Context())
	if err != nil {
		t.Fatalf("%.120s reads as no record: %v", record, err)
	}
	return line
}
