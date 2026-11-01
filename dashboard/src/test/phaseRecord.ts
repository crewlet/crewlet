/**
 * A whole phase record for a suite to override — every field, so a record a
 * test builds is one the constructors in `lib/phases.ts` could have built.
 */

import type { Invocation, PhaseRecord, ToolCall } from "~/lib/phases.ts";

/** One harness run that made round 1, observed and ended by its model, unless the
 *  suite says otherwise. */
export function invocation(over: Partial<Invocation> = {}): Invocation {
  return {
    run: 1,
    startedAt: "",
    durationMs: 0,
    providerKey: "claude-max",
    model: "stub-sonnet",
    outcome: "end",
    firstRound: 1,
    lastRound: 1,
    requestsObserved: true,
    inputTokens: 0,
    outputTokens: 0,
    cacheReadTokens: 0,
    toolCalls: 0,
    ...over,
  };
}

export function phaseRecord(over: Partial<PhaseRecord> = {}): PhaseRecord {
  return {
    key: "turn-1|execute|1",
    turnId: "turn-1",
    workKey: "",
    phase: "execute",
    iteration: 1,
    launch: false,
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
    systemSections: null,
    userSections: null,
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
    harness: "",
    sandboxId: "",
    deliveredRefs: [],
    deliveredRefsElided: 0,
    launchId: "",
    harnessTurns: 0,
    transcript: "",
    transcriptElidedLines: 0,
    transcriptElidedBytes: 0,
    trigger: null,
    timedRounds: [],
    invocations: [],
    hostRound: 0,
    cacheReadTokens: 0,
    maxRounds: 0,
    roundStartedAt: "",
    runStartedAt: "",
    runningCalls: [],
    bodyWithheld: false,
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
    callId: "",
    ...over,
  };
}
