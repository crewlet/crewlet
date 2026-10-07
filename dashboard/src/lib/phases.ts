/**
 * One phase of one turn, however it reached the client.
 *
 * A phase arrives twice over its life and in two different shapes: live, as
 * the `live_call` on a seat's overlay (re-broadcast twice per tool-loop round),
 * and durably, as an `agent_phase_completed` event once it finishes. The
 * dashboard this replaces gave those two shapes DIFFERENT IDENTITIES — the
 * live row was keyed `live|turn|phase|iteration` and the stored row
 * `turn|phase|iteration|timestamp` — so the instant a phase completed its row
 * was removed and a different one inserted: the entrance animation replayed,
 * the row relocated from the end of the list into its chronological slot, and
 * its expanded state was lost, because the override was filed under the key
 * that had just changed.
 *
 * That is the "the LLM calls are jumping" complaint, and it is fixed here:
 * **one identity, `turn|phase|iteration`**, for both shapes. A live phase
 * BECOMES a finished phase in place.
 *
 * The second half of the complaint — tool badges moving between paragraphs on
 * every new call — came from distributing tool calls across inter-paragraph
 * slots with `floor(j * slots / tools.length)`. Both the divisor and the slot
 * count grow every round, so every earlier badge was re-placed. The true
 * ordering, `tool_executions[].round`, was on the wire the whole time and never
 * read. It is what `rounds()` below groups on, and rounds only ever append.
 */

import type {
  EventRecord,
  LiveCall,
  LiveTurn,
  PartialRound,
  PromptMessage,
  PromptSection,
  ToolExecution,
  TurnStage,
} from "~/protocol/index.ts";
import type { StopReason } from "~/contract/stops.ts";
import { tsKey } from "./format.ts";
import { decodeSections } from "./promptmap.ts";
import type { Tone } from "~/ui/primitives.tsx";

export interface ToolCall {
  name: string;
  round: number;
  /** JSON text as the engine encoded it, or "" — never a Go-syntax dump. */
  args: string;
  result: string;
  failed: boolean;
  /** How long the call took. A reader inside a transcript asking why a phase
   *  took four minutes is asking this. 0 when the producer did not record it. */
  durationMs: number;
  /** WHERE THE TOOL THAT ANSWERED CAME FROM, recorded at registration and the
   *  one frame that knows: `builtin` or `mcp:<server>` (the agent-to-agent
   *  tools are builtins). Without it a reader cannot tell an engine builtin
   *  from somebody else's MCP server inside the round that called it. "" on a
   *  call no tool answered — an unknown name, one not offered, one a guard
   *  refused — and on a row the engine did not attribute. */
  origin: string;
  /** Which MCP server answered, for an `mcp:` origin; "" otherwise. */
  server: string;
  /** When the call was handed to the tool, as the engine stamped it; "" on a
   *  call nothing timed. The waterfall places a call here, and falls back to
   *  running the round's calls one after another from the model's answer only
   *  where this is absent. */
  startedAt: string;
}

/**
 * One round's MODEL call, as the tool loop timed it (`types.PhaseRound`) —
 * the model's half of a round; its tool calls are timed on their own rows,
 * which carry the same `round`, so a slow model and a slow tool are never one
 * number.
 */
export interface TimedRound {
  round: number;
  startedAt: string;
  durationMs: number;
  model: string;
  inputTokens: number;
  outputTokens: number;
  cacheReadTokens: number;
  toolCalls: number;
  /** Why the model stopped writing this round — `rounds[].stop_reason` — or
   *  "" where its backend reported none. See [stopNote]. */
  stopReason: string;
}

/**
 * What a round that did NOT finish is drawn with: the sentence under its words,
 * keyed on the engine's stop reason. `end` and `tool_use` are a round that
 * finished and have none; each of the others ENDED the phase, and the phase's
 * own error says the rest.
 *
 * Typed over the contract's union, so a stop reason the engine starts
 * recording is a type error here until it has its sentence — the gate behind
 * `contract/stops.ts` is what makes the union the engine's.
 */
const STOP_NOTES: Record<Exclude<StopReason, "end" | "tool_use">, string> = {
  max_tokens:
    "The response was cut off at the model's output cap, so the phase ended here — " +
    "any tool call it was writing was not run.",
  refusal:
    "The model declined this request, so the phase ended here. A refusal is not " +
    "asked again, and the turn's trigger is not redelivered.",
  context_exceeded: "The conversation filled the model's context window, so the phase ended here.",
  paused:
    "The provider paused this turn for the engine to continue, which it never asks " +
    "for, so the phase ended here.",
};

/** The sentence for a round that stopped without finishing, or null for a
 *  round that finished, one whose backend said nothing, and a reason this
 *  build does not know. */
export function stopNote(reason: string): string | null {
  return Object.hasOwn(STOP_NOTES, reason) ? STOP_NOTES[reason as keyof typeof STOP_NOTES] : null;
}

/** One round's model turn: what it reasoned, and what it said out loud. */
export interface Narration {
  round: number;
  reasoning: string;
  content: string;
  /**
   * The round ANSWERED IN PROSE where the phase had to end in a tool call —
   * `round_narration[].declined`, set by the engine when a phase that finishes
   * only by its submission (`submit_work`, `submit_review`, `mark_onboarded`,
   * a worker's `submit_result`) got words and no call. False on every other
   * round, and on a record an engine that did not flag it wrote.
   */
  declined: boolean;
}

export interface Round {
  round: number;
  /** The model's thinking for this round, when it emitted any separately. */
  reasoning: string;
  /** The model's prose for this round. */
  content: string;
  tools: ToolCall[];
  /** Still being written: this text is arriving, not committed. */
  streaming: boolean;
  /** Attempts a provider gave up on partway through, oldest first. */
  abandoned: Narration[];
  /**
   * The model wrote this round's answer as prose and called no tool, in a
   * phase that finishes only by a call — see [Narration.declined]. WHAT
   * HAPPENED NEXT is not on the wire and needs no field: a later round means
   * the engine asked again, and a declined LAST round of a settled phase
   * means the bound was spent and the phase ended without its submission.
   */
  declined: boolean;
  /** Why the model stopped writing this round ([TimedRound.stopReason]), ""
   *  where nothing said — read with [stopNote]. */
  stopReason: string;
}

/**
 * A model DECLINING the phase's request on policy grounds — the record's
 * `refusal`. A named outcome rather than a breakage: the phase failed, but
 * nothing went wrong that an operator can fix by rotating a key, and the
 * engine neither rescued it nor will run the turn again.
 */
export interface Refusal {
  /** The vendor's policy category (`cyber`, `bio`, …), "" where it named none. */
  category: string;
  /** The vendor's own account, "" where it gave none. Not stable wording. */
  explanation: string;
}

export interface PhaseRecord {
  /** `turn|phase|iteration`. The SAME value live and finished. */
  key: string;
  /** ONE RUN of a turn — unique per execution, which is what makes `key`
      unique per execution. See `adr/0017`. */
  turnId: string;
  /** The unit of work behind that run: what groups a redelivered trigger's
      attempts. Empty on a record an engine from before the split wrote. */
  workKey: string;
  phase: string;
  iteration: number;
  role: string;
  /**
   * The seat's own id — the one every node derives for its handle — off the
   * durable record's `agent_id`, and "" where nothing named it (a live call
   * carries no id; its seat is the push row it hangs off).
   *
   * WHAT "ONE SEAT'S PHASES" IS MATCHED ON, never `role`: two unit seats
   * stamped from one template share a role name, and a rename changes it
   * while the history keeps the old one.
   */
  agentId: string;
  model: string;
  providerKey: string;
  /** Live means the phase has not published its completed event yet. */
  live: boolean;
  failed: boolean;
  error: string;
  errorKind: string;
  /** Set when the phase ended because its model declined — see [Refusal].
   *  Null on every other phase, and always on a live one: a refusal ends the
   *  phase, so it arrives on the settled record. */
  refusal: Refusal | null;
  systemPrompt: string;
  userPrompt: string;
  /**
   * Where each prompt's parts begin and end, as the builder that wrote it said
   * (`system_sections` / `user_sections` on the record, a prompt message's
   * `sections` on a live call) — null where the engine sent no map. Read
   * through `lib/promptmap.ts`, which trusts a map only when it tiles its
   * prompt and otherwise derives the outline from the prompt's headings.
   */
  systemSections: PromptSection[] | null;
  userSections: PromptSection[] | null;
  response: string;
  tools: ToolCall[];
  /** Per-round model turns: one entry per round that wrote reasoning or
      text. Empty until a round comes back, on a phase whose rounds only
      called tools, and on a coding run, whose rounds happened in its box. */
  narration: Narration[];
  /** The round being written right now. Live phases only. */
  partial: PartialRound | null;
  inputTokens: number;
  outputTokens: number;
  totalTokens: number;
  /**
   * Rounds that have come back: ONE-BASED, 0 when none has, and the same
   * quantity from both constructors — `live_call.rounds_used` on a running phase,
   * `rounds_used` on a settled one.
   *
   * It is the ONLY round figure a record carries. `roundNum` used to sit beside
   * it holding the engine's ZERO-BASED `round_num` from the live path and
   * `rounds_used` from the event path — one name, two quantities, decided by
   * which constructor ran — and both consumers got it wrong in opposite
   * directions: the model table read `max(roundsUsed, roundNum + 1)` and so
   * added one to every settled phase, and the phase card read
   * `max(ledger.length, roundNum)` and so was one short on a live phase whose
   * rounds narrated nothing. The opening frame's `-1` also never matched that
   * card's `=== 0` guard, so the one phase its dash exists for rendered "0r".
   */
  roundsUsed: number;
  exhaustedRounds: boolean;
  /**
   * Rounds in which the model produced neither prose nor a tool call — it
   * spent its output budget on hidden reasoning and stopped.
   *
   * Zero on a live phase, like the two flags below it: the count is settled
   * only when the phase publishes its record.
   */
  emptyAnswerRounds: number;
  /**
   * The phase ENDED without its submission succeeding, so the engine wrote its
   * decision in its place — `incomplete` for an executor, `self_iterate` for a
   * reviewer. Not "it was re-asked": asking again happens inside the phase, a
   * round at a time, and shows as a declined round followed by another one.
   */
  rescueFired: boolean;
  decision: string;
  notes: string;
  conversationKey: string;
  toolsAvailable: string[];
  toolCatalogue: string[];
  /** The named worker behind this call: a learning worker on an
      `auxiliary` phase, a delegate template on a `subagent` one. */
  worker: string;
  /** A delegated task's own id, as the executor wrote it. `subagent` only. */
  taskId: string;
  /** The phase this one ran UNDER — `execute` for a worker or a judge.
      Empty on a turn's own phases. */
  hostPhase: string;
  /** The iteration of that host phase. */
  hostIteration: number;
  backend: string;
  codingAgent: string;
  /** The box that ran this phase, when a coding agent did. Links a transcript
   *  to the detached run it suspended into. */
  sandboxId: string;
  /** The branches and pull requests the phase delivered. */
  deliveredRefs: string[];
  /**
   * The detached coding run a record reports, on a `sandbox` phase (the run
   * itself) and on the executor that resumed from it. A turn can launch more
   * than one run in an iteration, so on a `sandbox` phase it is part of the
   * record's identity — see [phaseKey].
   */
  launchId: string;
  /**
   * A coding run's own account of what it did — its tool calls and shell
   * commands — on a `sandbox` phase only. Tail-capped and redacted by the
   * engine; empty everywhere else.
   */
  transcript: string;
  /**
   * What woke the turn this phase belongs to, as [types.Trigger.Map] writes
   * it. `id` and `sender` have always been on the wire and were not declared
   * here, so nothing could link a turn to the event that asked for it.
   */
  trigger: {
    id?: string;
    type?: string;
    summary?: string;
    actor?: string;
    integration?: string;
    sender?: string;
    timestamp?: string;
  } | null;
  /**
   * Each round's model call as the loop timed it, oldest first; empty on a
   * phase an engine that did not time rounds recorded, and on a coding run.
   */
  timedRounds: TimedRound[];
  /**
   * The round of the host phase a worker or a judge ran in (`host_round`),
   * 0 where the record does not say — which nests a delegate's workers
   * under the round that spawned them rather than beside it.
   */
  hostRound: number;
  /** The share of `inputTokens` the provider's prompt cache served. */
  cacheReadTokens: number;
  /**
   * The round cap currently granted, which an extension raises mid-phase;
   * 0 where the record does not say.
   */
  maxRounds: number;
  /**
   * When the round in flight began its provider call, and the tool call
   * running right now — a LIVE phase's only, "" / null otherwise.
   */
  roundStartedAt: string;
  runningCall: { round: number; name: string; arguments: string; startedAt: string } | null;
  /** The notes a person sent the turn that this phase read, and the round
   *  whose provider call first saw each. */
  steers: { round: number; noteId: string }[];
  /** The node that ran the phase, off the live call; "" on a stored record,
   *  whose row carries no node (the turn answer's `nodes` says it). */
  node: string;
  /**
   * When the ENGINE says the phase began (`started_at` on the record), or ""
   * where the record does not carry it. Read by the waterfall in preference
   * to [phaseStart]'s landing-less-duration, which is one publish late.
   */
  clockStart: string;
  /** When the phase finished, or when the live call last moved. */
  at: string;
  /**
   * When a live call BEGAN. Never moves — `at` does, on every round.
   *
   * A LIVE record's only instant of its own. A finished one reports
   * `durationMs` instead and leaves this equal to `at`: the engine measures
   * the phase where the clock is and puts the answer on the record, so
   * nothing here subtracts two timestamps that may have been stamped by two
   * processes.
   */
  startedAt: string;
  /**
   * How long this phase took, in milliseconds, as the engine measured it.
   *
   * Straight off `agent_phase_completed.duration_ms`. Zero on a live record,
   * which has not finished, and on a record the engine could not measure —
   * an agent-mode executor whose rounds ran inside a coding CLI's own loop,
   * in another process. Read it through [phaseDuration], never directly:
   * "no duration" and "took no time" must not render alike.
   */
  durationMs: number;
  /** The event id, when this came from the store — for a deep link. */
  eventId: string;
  /**
   * Where the TURN this live call belongs to is, when the seat's own turn is
   * that turn — `parked` while a detached coding run holds it — and "" on a
   * finished record or a call whose turn the seat has moved past.
   *
   * ON THE RECORD because the card that draws a live call's staleness has no
   * seat to ask: a parked executor's call stops moving BY DESIGN for as long
   * as the run takes, and a card that read only its own `at` called every
   * legitimately silent run stalled.
   */
  stage: TurnStage | "";
}

function str(v: unknown): string {
  if (v == null) return "";
  if (typeof v === "string") return v;
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}

function num(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

/** Normalise the loose `tool_executions` map into something typed. */
export function toolCalls(raw: unknown): ToolCall[] {
  if (!Array.isArray(raw)) return [];
  return (raw as ToolExecution[]).map((ex) => {
    const rec = ex as Partial<Record<keyof ToolExecution, unknown>>;
    return {
      name: String(rec.name ?? "tool"),
      // ONE-BASED, because the engine's own `round` is (it is `roundsUsed`).
      round: num(rec.round),
      args: str(rec.arguments),
      result: str(rec.result),
      failed: rec.success === false || Boolean(rec.error),
      // How long the call took, and who answered it: the tool loop times
      // every call and the surface names the origin that served it. Absent
      // on a row nothing timed (an agent-mode run's bridged call, whose
      // rounds ran inside somebody else's loop), which reads as 0 / "" —
      // "not recorded", never "instant" or "the engine's own".
      durationMs: typeof rec.duration_ms === "number" ? rec.duration_ms : 0,
      origin: typeof rec.origin === "string" ? rec.origin : "",
      server: typeof rec.server === "string" ? rec.server : "",
      startedAt: typeof rec.started_at === "string" ? rec.started_at : "",
    };
  });
}

/** Normalise the `rounds` list — each round's model call, as the loop timed it. */
export function timedRounds(raw: unknown): TimedRound[] {
  if (!Array.isArray(raw)) return [];
  return (raw as Record<string, unknown>[])
    .map((rec) => ({
      round: num(rec.round),
      startedAt: typeof rec.started_at === "string" ? rec.started_at : "",
      durationMs: num(rec.duration_ms),
      model: typeof rec.model === "string" ? rec.model : "",
      inputTokens: num(rec.input_tokens),
      outputTokens: num(rec.output_tokens),
      cacheReadTokens: num(rec.cache_read_tokens),
      toolCalls: num(rec.tool_calls),
      stopReason: typeof rec.stop_reason === "string" ? rec.stop_reason : "",
    }))
    .filter((r) => r.round > 0)
    .sort((a, b) => a.round - b.round);
}

/** Normalise the loose `round_narration` list into something typed. */
export function narrations(raw: unknown): Narration[] {
  if (!Array.isArray(raw)) return [];
  return (raw as Record<string, unknown>[])
    .map((rec, i) => ({
      round: typeof rec.round === "number" ? rec.round : i + 1,
      reasoning: typeof rec.reasoning === "string" ? rec.reasoning : "",
      content: typeof rec.content === "string" ? rec.content : "",
      // `=== true`, never truthiness: the engine omits the key on every
      // round that is not one, and anything else says nothing this build reads.
      declined: rec.declined === true,
    }))
    .filter((n) => n.reasoning.trim() !== "" || n.content.trim() !== "");
}

/**
 * Group a phase's tool calls into rounds.
 *
 * Rounds are ORDERED and only ever appended to, which is the whole property
 * this display depends on: nothing above the insertion point can move, so a
 * reader's eye stays where they left it while a turn runs underneath.
 */
export function rounds(
  calls: ToolCall[],
  narration: Narration[] = [],
  partial?: PartialRound | null,
  timed: TimedRound[] = [],
): Round[] {
  const byRound = new Map<number, Round>();
  const at = (round: number): Round => {
    let r = byRound.get(round);
    if (!r) {
      r = {
        round,
        reasoning: "",
        content: "",
        tools: [],
        streaming: false,
        abandoned: [],
        declined: false,
        stopReason: "",
      };
      byRound.set(round, r);
    }
    return r;
  };
  // Narration first, so a round that only THOUGHT still gets a slot: the
  // final round of a phase calls no tools, and it is the one holding the
  // answer.
  for (const n of narration) {
    const r = at(n.round);
    r.reasoning = n.reasoning;
    r.content = n.content;
    r.declined = n.declined;
  }
  for (const call of calls) at(call.round).tools.push(call);
  // Each round's stop reason, off the loop's own timing of it. A round that
  // did NOT finish gets a slot even when it said nothing and ran nothing — a
  // refusal with no text is exactly that round, and it is the one that
  // explains why the phase ended; a round that finished and left no trace
  // stays out, as it always has.
  for (const t of timed) {
    if (byRound.has(t.round) || stopNote(t.stopReason) !== null)
      at(t.round).stopReason = t.stopReason;
  }
  // The round in flight. The engine clears it the instant that round's real
  // narration exists, so the two can never describe one round at once.
  if (partial && typeof partial.round === "number") {
    const r = at(partial.round);
    r.streaming = true;
    // Arriving text has not ended its round, so it has declined nothing yet.
    r.declined = false;
    r.reasoning = partial.reasoning ?? "";
    r.content = partial.content ?? "";
    r.abandoned = narrations(partial.abandoned);
  }
  return [...byRound.values()].sort((a, b) => a.round - b.round);
}

/** The first message with this role that carries text, or null. */
function promptRole(
  messages: PromptMessage[] | null | undefined,
  role: string,
): { content: string; sections: PromptSection[] | null } | null {
  if (!Array.isArray(messages)) return null;
  for (const m of messages) {
    if (m && m.role === role && typeof m.content === "string") {
      return { content: m.content, sections: decodeSections(m.sections) };
    }
  }
  return null;
}

/**
 * A phase's identity: `turn|phase|iteration`, plus the task id or the launch
 * id where there is one.
 *
 * THE TASK ID IS NOT OPTIONAL for a delegated worker. A `delegate` call of
 * eight runs eight `subagent` phases in one executor round, and without it
 * they share one key — so the map keeps the last one to arrive and seven
 * workers, their prompts, their tools and their failures simply are not on
 * the page. A turn's own phases have no task id and keep the three-part key
 * they have always had.
 *
 * THE LAUNCH ID IS NOT OPTIONAL for a coding run, for the same reason: a
 * resumed executor that calls `run_sandbox` again in one iteration launches a
 * second run, and each is a `sandbox` phase of that iteration. It keys the
 * `sandbox` phase ONLY — the resumed executor's record names the run it
 * collected too, and keyed on it that record would stop matching the live
 * call it replaces.
 */
export function phaseKey(
  turnId: string,
  phase: string,
  iteration: number,
  taskId = "",
  launchId = "",
): string {
  const base = `${turnId}|${phase}|${iteration}`;
  const discriminator = taskId || (phase === "sandbox" ? launchId : "");
  return discriminator ? `${base}|${discriminator}` : base;
}

/**
 * A phase still running, from a seat's live overlay — and the seat's own turn,
 * which is where the stage is kept.
 */
export function fromLiveCall(call: LiveCall, role: string, turn?: LiveTurn | null): PhaseRecord {
  const system = promptRole(call.prompt_messages, "system");
  // THE TEXT AND ITS MAP FROM ONE MESSAGE. This read `call.prompt` first and
  // the message only when `prompt` was null — which it never is on the wire
  // (an absent one decodes as ""), so the user message was never read, and a
  // map read off the message would have described text taken from somewhere
  // else. `prompt` is the same text, carried for a frame that has no message.
  const user = promptRole(call.prompt_messages, "user");
  return {
    key: phaseKey(call.turn_id, call.phase, call.iteration),
    turnId: call.turn_id,
    workKey: call.work_key ?? "",
    phase: call.phase,
    iteration: call.iteration,
    role,
    agentId: "",
    model: call.model,
    providerKey: "",
    live: call.in_progress !== false && !call.failed,
    failed: !!call.failed,
    error: call.error?.message ?? "",
    errorKind: call.error?.kind ?? "",
    // A refusal ends the phase, so it is on the settled record and never a
    // running one's.
    refusal: null,
    // Read off `prompt_messages`, which the engine has always sent and
    // nothing read. Hardcoding "" here meant a RUNNING phase could never
    // show the system prompt it was given — the one moment an operator
    // most wants to know what the model was actually told.
    systemPrompt: system?.content ?? "",
    userPrompt: user ? user.content : (call.prompt ?? ""),
    systemSections: system?.sections ?? null,
    userSections: user?.sections ?? null,
    response: call.response ?? "",
    tools: toolCalls(call.tool_executions),
    narration: narrations(call.round_narration),
    partial: call.partial_round ?? null,
    inputTokens: call.input_tokens,
    outputTokens: call.output_tokens,
    totalTokens: call.total_tokens,
    roundsUsed: call.rounds_used,
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
    // A RUNNING phase has neither yet: the box id is stamped when the run is
    // registered, and the refs are what it REPORTS back.
    sandboxId: "",
    deliveredRefs: [],
    launchId: "",
    transcript: "",
    trigger: (call.trigger as PhaseRecord["trigger"]) ?? null,
    timedRounds: timedRounds(call.rounds),
    hostRound: 0,
    cacheReadTokens: call.cache_read_tokens ?? 0,
    maxRounds: call.max_rounds ?? 0,
    roundStartedAt: call.round_started_at ?? "",
    runningCall: call.running_call
      ? {
          round: call.running_call.round,
          name: call.running_call.name,
          arguments: call.running_call.arguments,
          startedAt: call.running_call.started_at,
        }
      : null,
    steers: (call.steers ?? []).map((s) => ({ round: s.round, noteId: s.note_id })),
    node: call.node ?? "",
    clockStart: call.started_at ?? "",
    at: call.updated_at,
    startedAt: call.started_at || call.updated_at,
    // A running phase has not taken a length yet. Its elapsed time is read
    // off `startedAt` against the clock, which keeps ticking; this is the
    // engine's final measurement and does not exist until it lands.
    durationMs: 0,
    eventId: "",
    // THE SEAT'S TURN ONLY WHEN IT IS THIS CALL'S TURN: a stage read off a
    // turn the seat has since started would describe some other call.
    stage: turn && turn.turn_id === call.turn_id ? turn.stage : "",
  };
}

/** The record's `refusal`, or null where it carries none (or something this
 *  build cannot read as one). */
function refusalOf(raw: unknown): Refusal | null {
  if (!raw || typeof raw !== "object") return null;
  const r = raw as Record<string, unknown>;
  return {
    category: typeof r.category === "string" ? r.category : "",
    explanation: typeof r.explanation === "string" ? r.explanation : "",
  };
}

/** A finished phase, from its durable `agent_phase_completed` event. */
export function fromPhaseEvent(ev: EventRecord): PhaseRecord | null {
  const p = ev.payload as Record<string, unknown> | undefined;
  if (!p) return null;
  const phase = String(p.phase ?? "");
  const turnId = String(p.turn_id ?? "");
  const iteration = num(p.iteration);
  const taskId = String(p.task_id ?? "");
  const launchId = String(p.launch_id ?? "");
  return {
    key: phaseKey(turnId, phase, iteration, taskId, launchId),
    turnId,
    // THE ROW'S OWN COLUMN FIRST, the payload only as what a live frame
    // carries. The stored column is backfilled across the split
    // (migration 0029) and the payload is not, so a payload-only read
    // reports no unit of work for every turn older than the split.
    workKey: String(ev.work_key ?? p.work_key ?? ""),
    phase,
    iteration,
    role: String(p.role ?? ev.actor ?? ""),
    agentId: String(p.agent_id ?? ev.tags?.agent_id ?? ""),
    model: String(p.model ?? ""),
    providerKey: String(p.provider_key ?? ""),
    live: false,
    failed: p.failed === true,
    error: String(p.error ?? ""),
    errorKind: String(p.error_kind ?? ""),
    refusal: refusalOf(p.refusal),
    systemPrompt: String(p.system_prompt ?? ""),
    userPrompt: String(p.user_prompt ?? ""),
    systemSections: decodeSections(p.system_sections),
    userSections: decodeSections(p.user_sections),
    response: String(p.response ?? ""),
    tools: toolCalls(p.tool_executions),
    narration: narrations(p.round_narration),
    // A finished phase never has one: the engine clears it the moment the
    // round commits, so the durable event carries no half sentence.
    partial: null,
    inputTokens: num(p.input_tokens),
    outputTokens: num(p.output_tokens),
    totalTokens: num(p.total_tokens),
    roundsUsed: num(p.rounds_used),
    exhaustedRounds: p.exhausted_rounds === true,
    emptyAnswerRounds: num(p.empty_answer_rounds),
    rescueFired: p.rescue_fired === true,
    decision: String(p.decision ?? ""),
    notes: String(p.notes ?? ""),
    conversationKey: String(p.conversation_key ?? ""),
    toolsAvailable: Array.isArray(p.tools_available) ? (p.tools_available as string[]) : [],
    toolCatalogue: Array.isArray(p.tool_catalogue) ? (p.tool_catalogue as string[]) : [],
    worker: String(p.worker ?? ""),
    taskId,
    hostPhase: String(p.host_phase ?? ""),
    hostIteration: num(p.host_iteration),
    backend: String(p.backend ?? ""),
    codingAgent: String(p.coding_agent ?? ""),
    // WHAT A SANDBOX RUN LEAVES ON `AgentPhaseCompleted`: which box ran it (so
    // the badge naming the coding agent can reach the run) and the branches
    // and pull requests the phase produced. The record also carries what the
    // run's own CLI said it cost, and that is deliberately NOT read: the
    // dashboard renders tokens and never money (rule 19 in
    // docs/reference/dashboard-design.md), and a price parsed onto the record
    // is one a component is a single line away from drawing.
    sandboxId: String(p.sandbox_id ?? ""),
    deliveredRefs: Array.isArray(p.delivered_refs) ? (p.delivered_refs as string[]) : [],
    launchId,
    transcript: String(p.activity_transcript ?? ""),
    trigger: (p.trigger as PhaseRecord["trigger"]) ?? null,
    timedRounds: timedRounds(p.rounds),
    hostRound: num(p.host_round),
    cacheReadTokens: num(p.cache_read_tokens),
    maxRounds: num(p.max_rounds),
    roundStartedAt: "",
    runningCall: null,
    steers: Array.isArray(p.steers)
      ? (p.steers as Record<string, unknown>[]).map((s) => ({
          round: num(s.round),
          noteId: String(s.note_id ?? ""),
        }))
      : [],
    node: "",
    clockStart: typeof p.started_at === "string" ? p.started_at : "",
    at: ev.timestamp,
    // A finished phase has one instant that matters — when it landed. How
    // long it took is a measurement rather than a second instant, and it
    // arrives on the same record.
    startedAt: ev.timestamp,
    durationMs: num(p.duration_ms),
    eventId: ev.id,
    stage: "",
  };
}

/**
 * The phases that completed on the wire while this tab was watching.
 *
 * Every screen that renders a phase reads two sources: a query, answered ONCE
 * at mount, and the seat's live overlay. Neither covers a phase that finishes
 * while the reader is looking at it — the overlay's `live_call` is cleared the
 * moment it lands, and the query is never re-asked — so the phase, and with it
 * the whole turn, went away. Most visibly on a seat's first turn, where the
 * mount-time history is empty and the page was left saying the seat had never
 * run at all.
 *
 * The durable record is already on the wire: the engine broadcasts the whole
 * `agent_phase_completed` envelope, payload included, and the store keeps the
 * recent ones. This is the same `fromPhaseEvent` the stored half goes through,
 * so a streamed record and the one the same query would return next time are
 * the same record with the same key — which is what lets it merge in place.
 *
 * `keep` narrows the buffer to the screen's own scope (this seat, this turn,
 * this role filter); the buffer itself is company-wide because one socket
 * serves every screen.
 */
export function streamedPhases(
  events: readonly EventRecord[],
  keep: (record: PhaseRecord) => boolean,
): PhaseRecord[] {
  const out: PhaseRecord[] = [];
  for (const ev of events) {
    const record = fromPhaseEvent(ev);
    if (record && keep(record)) out.push(record);
  }
  return out;
}

/**
 * The four fields a phase's own clock is read from.
 *
 * Declared so the two readers below, and `turnSpan` on the Turn screen, can
 * be exercised against the instants alone. A pure function over a value is
 * testable; one that demands a whole forty-field record to answer "when did
 * this begin" is a function nobody re-measures.
 */
export type Timed = Pick<PhaseRecord, "live" | "at" | "startedAt" | "durationMs">;

/**
 * How long a phase took, or null when nothing measured it.
 *
 * STRAIGHT OFF THE RECORD. This used to subtract the phase's own
 * `agent_phase_started` timestamp from its completion instant, folded on by a
 * `withStarts` pass over a `phaseStarts` map — and that reconstruction needed
 * BOTH events in one reader's hands, which three readers never have:
 *
 *  - A turn deep-linked WHILE IT RUNS asks the `turn` query before its phases
 *    start, and the only envelopes buffered afterwards are
 *    `agent_phase_completed` ones. So the map was built from a slice that
 *    could not contain a single start, and every phase on the screen this
 *    exists for reported no duration at all.
 *  - A NESTED phase — a delegate's worker, the round-cap judge — publishes no
 *    start by design, so no worker of a fan-out ever had a duration and
 *    "which one was slow" had no answer anywhere.
 *  - A phase started on one node and completed on another subtracts two
 *    clocks nothing reconciles.
 *
 * The engine measures the phase where the clock is and puts the answer on the
 * record. Zero is "not measured", never "took no time" — see
 * [PhaseRecord.durationMs].
 */
export function phaseDuration(rec: Timed): number | null {
  if (rec.live) return null;
  return rec.durationMs > 0 ? rec.durationMs : null;
}

/**
 * When a phase BEGAN, as a timestamp, or 0 where nothing can say.
 *
 * A live record knows its own start. A finished one is the completion instant
 * less what the engine measured — derived rather than published, because the
 * record carries a duration and a landing instant and a third field would be
 * a third thing to keep consistent with the other two.
 */
export function phaseStart(rec: Timed): number {
  if (rec.live) return tsKey(rec.startedAt);
  const at = tsKey(rec.at);
  if (at <= 0) return 0;
  return rec.durationMs > 0 ? at - rec.durationMs : at;
}

/**
 * The window a set of phases ran in.
 *
 * THE SPAN OVER EVERYTHING THE CALLER HOLDS, not over one list. Read off a
 * page's events only, a turn whose phases all arrived on the stream reported a
 * duration of "—" beside a phase list several minutes long.
 *
 * The start is a minimum over EVERY phase, never over `phases[0]`. That list is
 * ordered by when each phase LANDED, so its first element is the earliest
 * FINISHER — and a worker a delegate spawned lands inside the window of the
 * execute round that spawned it. On the one case the span exists for, a turn
 * deep-linked while it runs (no query answer to supply the other term), that
 * made the window open at the first worker's start and "Took" under-report the
 * whole stretch before the fan-out.
 *
 * Each phase's start comes from [phaseStart], which is the live record's own
 * instant or the finished record's landing less what the engine measured.
 * Reading `startedAt` off a finished record put its END into the minimum.
 *
 * A zero is dropped rather than taken as a minimum: [tsKey] answers 0 for a
 * timestamp it cannot parse, and 0 is the epoch — one unreadable instant would
 * report a turn that has been running since 1970.
 *
 * HERE RATHER THAN ON THE TURN SCREEN, which is where it was written. The turn
 * CARD subtracted two LANDING instants instead — `last.at - first.at` — which
 * drops the first phase's own length: an execute-then-review turn reported its
 * review's duration as the whole turn's, printed above a phase card showing
 * three minutes. Two rules for one measurement is two answers on one screen.
 */
export function turnSpan(
  events: readonly { timestamp: string }[],
  phases: readonly Timed[],
): { from: number; to: number } {
  const live = (instants: number[]) => instants.filter((t) => t > 0);
  // EVERY instant on both sides, never the first and last of either. Indexing
  // would make the caller's sort order a precondition this function cannot
  // state or check, and it is the precondition the phase list already broke.
  const stamps = events.map((e) => tsKey(e.timestamp));
  const starts = live([...stamps, ...phases.map(phaseStart)]);
  const ends = live([...stamps, ...phases.map((p) => tsKey(p.at))]);
  // Both or neither: a start with no end would render a duration measured
  // against nothing, which is worse than the em dash the caller falls back to.
  if (!starts.length || !ends.length) return { from: 0, to: 0 };
  return { from: Math.min(...starts), to: Math.max(...ends) };
}

/**
 * Merge the live view and the durable record into one ordered list.
 *
 * The DURABLE record wins over a LIVE one on a key collision: it is the
 * complete one, and a live call lingering in the projection after its event
 * has landed would otherwise re-blank the fields only the event carries
 * (decision, notes, the verbatim system prompt).
 *
 * Between two DURABLE records the NEWER one wins, and that rule had to be
 * written down. The premise underneath the old code — that two records
 * sharing a key are "the same durable record", so whichever arrived by the
 * more authoritative route could win — held only while a phase key was unique.
 * It was not: a turn id used to be the work key, which a redelivery
 * reproduces, so a retry's `execute` record collided with the failed
 * attempt's. Both were real, different phases; this function kept the one the
 * mount-time query happened to apply last, which is the OLDEST, and the retry
 * was invisible for as long as it ran. Run ids are unique per execution now
 * (see `adr/0017`) so the collision should not recur — and a merge that
 * silently prefers stale data on a key it cannot prove unique is the shape
 * that hid it, so it does not go back.
 */
export function mergePhases(stored: PhaseRecord[], live: PhaseRecord[]): PhaseRecord[] {
  const byKey = new Map<string, PhaseRecord>();
  for (const rec of live) byKey.set(rec.key, rec);
  for (const rec of stored) {
    const held = byKey.get(rec.key);
    // A live row always yields to a durable one; between two durable rows the
    // newer wins, and a tie keeps the one already held so the order a caller
    // passes them in cannot change the answer.
    if (held && !held.live && tsKey(held.at) >= tsKey(rec.at)) continue;
    byKey.set(rec.key, rec);
  }
  return [...byKey.values()].sort((a, b) => {
    // Newest first, and NEVER by comparing the ISO strings: Go trims trailing
    // zeros from RFC3339Nano, so `…:07Z` sorts before `…:07.42Z` by comparing
    // 'Z' against '.', which orders the later instant first.
    const at = tsKey(a.at);
    const bt = tsKey(b.at);
    if (at !== bt) return bt - at;
    // A stable, transitive tiebreak. The idiom this replaces returned -1 for
    // equal operands, so equal rows genuinely swapped places between renders.
    return a.key < b.key ? 1 : a.key > b.key ? -1 : 0;
  });
}

export interface TurnGroup {
  turnId: string;
  /** The unit of work this run was an attempt at. Empty when its phases carry
      none — a trigger with no ledgerable id, or records an engine from before
      the split wrote. Two groups sharing one of these are two attempts at the
      same trigger; see `adr/0017`. */
  workKey: string;
  role: string;
  /** The turn's OWN phases, in the order they ran. A nested call is not
      here — it hangs off the phase that made it, see `nested`. */
  phases: PhaseRecord[];
  /** Nested calls keyed by the key of the phase that made them: the
      workers a `delegate` call ran, the round-cap judge, a learning
      worker. `host_phase` and `host_iteration` have always been on the
      wire and nothing read them, so a fan-out of eight rendered as eight
      siblings of the turn's own two phases and the reader had to work out
      which round each belonged to. */
  nested: Map<string, PhaseRecord[]>;
  /** The newest instant in the group — what the group is ordered by. */
  at: string;
  /** When the turn's OLDEST phase began. Never moves; `at` does. */
  startedAt: string;
  /**
   * How long this turn ran, across its phases, or null when nothing here can
   * say.
   *
   * NOT `last.at - first.at`. Both are LANDING instants, so that subtraction
   * drops the first phase's own length: an execute-then-review turn reported
   * its review's duration as the whole turn's, printed above a phase card
   * showing three minutes. Through [turnSpan] so this and the Turn screen
   * cannot disagree — a minimum over every phase's [phaseStart] and a maximum
   * over every landing, with unreadable instants dropped, and never an index
   * into a list whose sort is the caller's business.
   */
  span: number | null;
  /**
   * The highest self-iterate round the turn's OWN phases reached — the same
   * quantity `store.Turns` reports as `MAX(iteration)`, so the turns table and
   * this card state one number. A worker's iteration belongs to the delegate
   * call that spawned it, not to this turn.
   */
  iterations: number;
  live: boolean;
  failed: boolean;
  totalTokens: number;
  trigger: PhaseRecord["trigger"];
}

/** Which attempt at its trigger a turn was, for the turns a screen holds. */
export interface Attempt {
  /** 1-based, oldest attempt first. */
  index: number;
  total: number;
}

/**
 * Number each turn among the other attempts at the same trigger.
 *
 * A turn id names ONE RUN (see `adr/0017`), so a trigger that failed without
 * acting and was redelivered is several turns — which is honest, and on its
 * own leaves an operator looking at two rows with no way to tell they are the
 * same work. This is what tells them.
 *
 * SCOPED TO WHAT THE CALLER HOLDS, deliberately, and the caller says so in the
 * tooltip: these are the attempts on this screen, not a claim about every
 * attempt that ever ran. Counting the rest would need a query per work key,
 * and a number quietly computed from a page is the kind of figure that reads
 * as authoritative and is not.
 *
 * A turn with no work key gets no attempt at all: an empty key is the absence
 * of an identity, not a value, so grouping on it would report every
 * unledgered turn on the page as attempts at one another.
 */
export function attempts(groups: readonly TurnGroup[]): Map<string, Attempt> {
  const byKey = new Map<string, TurnGroup[]>();
  for (const g of groups) {
    if (!g.workKey) continue;
    byKey.set(g.workKey, [...(byKey.get(g.workKey) ?? []), g]);
  }
  const out = new Map<string, Attempt>();
  for (const list of byKey.values()) {
    if (list.length < 2) continue;
    // Oldest first, so "attempt 1" is the one that ran first however the
    // caller happened to sort them.
    const ordered = [...list].sort((a, b) => tsKey(a.startedAt) - tsKey(b.startedAt));
    ordered.forEach((g, i) => out.set(g.turnId, { index: i + 1, total: ordered.length }));
  }
  return out;
}

/** Group phases into the turns they belong to, newest turn first. */
export function groupTurns(phases: PhaseRecord[]): TurnGroup[] {
  const byTurn = new Map<string, PhaseRecord[]>();
  for (const rec of phases) byTurn.set(rec.turnId, [...(byTurn.get(rec.turnId) ?? []), rec]);
  return [...byTurn.entries()]
    .map(([turnId, list]) => {
      // Within a turn, OLDEST first: a turn is read forwards — onboarding
      // (first turn only), then execute, then the coding runs it launched,
      // then review — which is the opposite of a feed. A `sandbox` phase is
      // placed after its executor's record although it ran inside that
      // phase's window: the executor publishes one record, when it resumes,
      // and a run read before the executor that launched it reads as work
      // nobody asked for. Two runs of one iteration keep their time order. A
      // phase not on this list sorts after the ones that are and then by
      // time, which is right for the nested calls (subagent, judge,
      // auxiliary) that hang off a host phase.
      const ordered = [...list].sort((a, b) => {
        if (a.iteration !== b.iteration) return a.iteration - b.iteration;
        const order = ["onboarding", "execute", "sandbox", "review"];
        const ai = order.indexOf(a.phase);
        const bi = order.indexOf(b.phase);
        if (ai !== bi) return (ai < 0 ? 99 : ai) - (bi < 0 ? 99 : bi);
        return tsKey(a.at) - tsKey(b.at);
      });
      // A NESTED call belongs UNDER the phase that made it. It is split
      // out here rather than filtered at render time so every consumer —
      // the card, the trace tree, the counts — agrees about what a turn's
      // phases are.
      const own: PhaseRecord[] = [];
      const nested = new Map<string, PhaseRecord[]>();
      for (const rec of ordered) {
        if (!rec.hostPhase) {
          own.push(rec);
          continue;
        }
        const host = phaseKey(rec.turnId, rec.hostPhase, rec.hostIteration);
        nested.set(host, [...(nested.get(host) ?? []), rec]);
      }
      const at = ordered.reduce((max, r) => (tsKey(r.at) > tsKey(max) ? r.at : max), "");
      // THE WINDOW THIS TURN RAN IN, by the one rule [turnSpan] states. It was
      // two rules: a reduce here for the start, and a subtraction of two
      // LANDING instants in `TurnCard` for the length. A turn is "running for"
      // as long as its first phase has been going, not its newest round; and
      // read off `at`, a completed phase contributes its END.
      const { from, to } = turnSpan([], ordered);
      const startedAt = from > 0 ? new Date(from).toISOString() : "";
      return {
        turnId,
        // OFF THE PHASES, and the first that HAS one rather than the first
        // phase: a record written before the identities were split carries
        // none, and a turn whose opening phase is such a record still belongs
        // to whatever unit of work its later phases name.
        workKey: ordered.find((r) => r.workKey)?.workKey ?? "",
        role: ordered[0]?.role ?? "",
        phases: own,
        nested,
        at,
        startedAt,
        span: to > from ? to - from : null,
        iterations: own.reduce((n, p) => Math.max(n, p.iteration), 0),
        live: ordered.some((r) => r.live),
        failed: ordered.some((r) => r.failed),
        totalTokens: ordered.reduce((n, r) => n + r.totalTokens, 0),
        trigger: ordered.find((r) => r.trigger)?.trigger ?? null,
      };
    })
    .sort((a, b) => {
      const at = tsKey(a.at);
      const bt = tsKey(b.at);
      if (at !== bt) return bt - at;
      return a.turnId < b.turnId ? 1 : a.turnId > b.turnId ? -1 : 0;
    });
}

/**
 * What woke a turn, as one line: the trigger's own summary, else its bare type,
 * else the word a card must still print.
 *
 * ONE EXPRESSION because two readers need the IDENTICAL string — the text a
 * clamp cuts, and the `title` that carries what the clamp cut. Spelled at each
 * site, a tooltip can come to claim something its own card does not say.
 *
 * `live/Turn.tsx` deliberately does NOT read this: its chain has a third
 * source between the two (the turn record's own `summary`) and ends at "" rather
 * than at a word, because that screen has a heading to fall back on and a card
 * does not.
 */
export function triggerHeadline(trigger: PhaseRecord["trigger"]): string {
  return trigger?.summary || trigger?.type || "turn";
}

/**
 * What each decision MEANS: the words a reader sees, and whether it is something
 * they have to act on.
 *
 * ONE ROW FOR BOTH, because they are one fact. Spelled as two tables a decision
 * gets a sentence and no hue, which is exactly what happened: the phase card's
 * tone was an inline `=== "self_iterate"` at its own call site, so `blocked` —
 * the executor saying it could not do the work — drew the same neutral pill as
 * `delivered`. So did the engine-written `incomplete`, and so did the reviewer's
 * `failed`, which nothing else on that card draws red (a review record never
 * sets the phase's own `failed` flag). The three outcomes a reader has to act on
 * were the same grey as the one that needs nothing. Written as a `Record` of
 * `{label, tone}`, adding a decision without a tone is a type error rather than
 * a silent grey pill.
 *
 * The tones are the design doc's own rule for the turn header's outcome tile
 * (`done` positive, `self_iterate` caution, a failure critical); this chip is
 * the per-phase form of that tile and was the one surface not keeping it.
 *
 * `no_action` is deliberately NEUTRAL rather than a quiet caution. It is the
 * ordinary, uneventful end — nobody was asking — and a seat's feed is mostly
 * made of those; four status hues spent on every row is four hues spent on none.
 *
 * `incomplete` is the one word here the model did not write: the engine
 * synthesises it when the executor never submitted at all. It is labelled as
 * such, because a reader who cannot tell an engine-written outcome from a
 * model's own is reading a claim as a commitment.
 */
type Meaning = { label: string; tone: Tone };

const DECISIONS: Record<string, Record<string, Meaning>> = {
  execute: {
    delivered: { label: "delivered the work", tone: "positive" },
    no_action: { label: "nothing to do — ended silently", tone: "neutral" },
    blocked: { label: "blocked, and said why", tone: "caution" },
    incomplete: {
      label: "never said what it did — the engine marked it incomplete",
      // CAUTION, NOT CRITICAL. The reviewer still judges the turn after this;
      // what is critical is the reviewer deciding against it.
      tone: "caution",
    },
  },
  review: {
    done: { label: "accepted the work", tone: "positive" },
    self_iterate: { label: "sent the turn back for another round", tone: "caution" },
    failed: { label: "failed — the turn will not retry", tone: "critical" },
  },
  onboarding: {
    done: { label: "read its team's pages and marked itself onboarded", tone: "positive" },
  },
};

/**
 * What a decision means when the ENGINE wrote it — `rescue_fired`: the phase
 * ended without its submission succeeding, so the engine decided in its place.
 *
 * Only the reviewer needs a row of its own. The executor's rescue always writes
 * `incomplete`, whose sentence already says the engine wrote it; the reviewer's
 * writes `self_iterate`, the same word a reviewer chooses on purpose, and
 * "sent the turn back" said the REVIEWER judged the round when nothing judged
 * it at all. Same tone: the turn does go round again either way.
 */
const RESCUED: Record<string, Record<string, Meaning>> = {
  review: {
    self_iterate: {
      label: "never decided — the engine sent the turn back for another round",
      tone: "caution",
    },
  },
};

/**
 * The row for one decision, or nothing for a decision this build has never heard
 * of.
 *
 * The THIRD value matters to one caller: the turn header says "the executor said
 * <word>" for a word it cannot gloss, which a label falling through verbatim
 * could not tell it.
 *
 * `rescued` is the record's `rescue_fired`. A caller that has no record (an
 * episode's stored review outcome) leaves it off and gets the model's reading.
 */
export function decisionMeaning(
  phase: string,
  decision: string,
  rescued = false,
): Meaning | undefined {
  if (!decision) return undefined;
  const key = (phase || "").toLowerCase();
  return (rescued ? RESCUED[key]?.[decision] : undefined) ?? DECISIONS[key]?.[decision];
}

/**
 * What a phase's decision means, said in words rather than left as an enum.
 *
 * An unknown value falls through verbatim rather than being dropped, which is
 * what keeps a row written by a build this bundle predates readable: the retired
 * `plan` phase's `plan` / `direct` / `skip` still render as themselves.
 */
export function decisionLabel(phase: string, decision: string, rescued = false): string {
  if (!decision) return "";
  return decisionMeaning(phase, decision, rescued)?.label ?? decision;
}

/**
 * The hue that decision is drawn in.
 *
 * NEUTRAL for anything the table does not carry, which is the same rule the
 * label keeps and covers two real cases rather than one. A rolling upgrade puts
 * a later build's decision on the wire, and a hue invented for a word this build
 * cannot read is a claim about a fact it does not have. And the phases the table
 * deliberately omits — `subagent`, `judge` — already draw a `danger` pill of
 * their own off the record's `failed` flag, so toning their decision too would
 * report one stop twice, side by side.
 */
export function decisionTone(phase: string, decision: string, rescued = false): Tone {
  return decisionMeaning(phase, decision, rescued)?.tone ?? "neutral";
}
