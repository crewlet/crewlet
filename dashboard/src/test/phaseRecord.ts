/**
 * A whole phase record for a suite to override — every field, so a record a
 * test builds is one the constructors in `lib/phases.ts` could have built.
 */

import type { PhaseRecord, ToolCall } from "~/lib/phases.ts";

export function phaseRecord(over: Partial<PhaseRecord> = {}): PhaseRecord {
  return {
    key: "turn-1|execute|1",
    turnId: "turn-1",
    workKey: "",
    phase: "execute",
    iteration: 1,
    role: "SWE",
    agentId: "",
    model: "stub-sonnet",
    providerKey: "",
    live: false,
    failed: false,
    error: "",
    errorKind: "",
    systemPrompt: "",
    userPrompt: "",
    response: "",
    tools: [],
    narration: [],
    partial: null,
    inputTokens: 0,
    outputTokens: 0,
    totalTokens: 0,
    roundsUsed: 0,
    exhaustedRounds: false,
    emptyAnswerRounds: 0,
    rescueFired: false,
    decision: "",
    notes: "",
    conversationKey: "",
    toolsAvailable: [],
    toolCatalogue: [],
    worker: "",
    taskId: "",
    hostPhase: "",
    hostIteration: 0,
    backend: "",
    codingAgent: "",
    sandboxId: "",
    deliveredRefs: [],
    launchId: "",
    transcript: "",
    trigger: null,
    timedRounds: [],
    hostRound: 0,
    cacheReadTokens: 0,
    maxRounds: 0,
    roundStartedAt: "",
    runningCall: null,
    steers: [],
    node: "",
    clockStart: "",
    at: "2026-09-28T10:00:00Z",
    startedAt: "2026-09-28T10:00:00Z",
    durationMs: 0,
    eventId: "",
    stage: "",
    ...over,
  };
}

/** One tool call, untimed unless the suite says otherwise. */
export function toolCall(over: Partial<ToolCall> = {}): ToolCall {
  return {
    name: "read_file",
    round: 1,
    args: "{}",
    result: "",
    failed: false,
    durationMs: 0,
    origin: "builtin",
    server: "",
    startedAt: "",
    ...over,
  };
}
