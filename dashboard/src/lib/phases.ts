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
  PartialRound,
  PromptMessage,
  ToolExecution,
} from "~/protocol/index.ts";
import { tsKey } from "./format.ts";

export interface ToolCall {
  name: string;
  round: number;
  /** JSON text as the engine encoded it, or "" — never a Go-syntax dump. */
  args: string;
  result: string;
  failed: boolean;
}

/** One round's model turn: what it reasoned, and what it said out loud. */
export interface Narration {
  round: number;
  reasoning: string;
  content: string;
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
}

export interface PhaseRecord {
  /** `turn|phase|iteration`. The SAME value live and finished. */
  key: string;
  turnId: string;
  phase: string;
  iteration: number;
  role: string;
  model: string;
  providerKey: string;
  /** Live means the phase has not published its completed event yet. */
  live: boolean;
  failed: boolean;
  error: string;
  errorKind: string;
  systemPrompt: string;
  userPrompt: string;
  response: string;
  tools: ToolCall[];
  /** Per-round model turns. Empty on a phase recorded before the engine
      sent them — see `ledgerOf`. */
  narration: Narration[];
  /** The round being written right now. Live phases only. */
  partial: PartialRound | null;
  inputTokens: number;
  outputTokens: number;
  totalTokens: number;
  roundNum: number;
  roundsUsed: number;
  exhaustedRounds: boolean;
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
  trigger: { type?: string; summary?: string; actor?: string; integration?: string } | null;
  /** When the phase finished, or when the live call last moved. */
  at: string;
  /** When a live call BEGAN. Never moves — `at` does, on every round. */
  startedAt: string;
  /** The event id, when this came from the store — for a deep link. */
  eventId: string;
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
  return (raw as ToolExecution[]).map((ex, i) => {
    const rec = ex as Record<string, unknown>;
    return {
      name: String(rec.name ?? rec.tool ?? "tool"),
      // A producer that never set `round` still gets a stable ledger: the
      // array's own order is the sequence, and it only appends. ONE-BASED,
      // because the engine's own `round` is (it is `roundsUsed`), and a
      // fallback numbering from 0 would put two producers' rounds on
      // different scales in the same list.
      round: typeof rec.round === "number" ? rec.round : i + 1,
      args: str(rec.arguments ?? rec.args),
      result: str(rec.result ?? rec.output ?? rec.error),
      failed: rec.success === false || rec.failed === true || Boolean(rec.error),
    };
  });
}

/** Normalise the loose `round_narration` list into something typed. */
export function narrations(raw: unknown): Narration[] {
  if (!Array.isArray(raw)) return [];
  return (raw as Record<string, unknown>[])
    .map((rec, i) => ({
      round: typeof rec.round === "number" ? rec.round : i + 1,
      reasoning: typeof rec.reasoning === "string" ? rec.reasoning : "",
      content: typeof rec.content === "string" ? rec.content : "",
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
): Round[] {
  const byRound = new Map<number, Round>();
  const at = (round: number): Round => {
    let r = byRound.get(round);
    if (!r) {
      r = { round, reasoning: "", content: "", tools: [], streaming: false, abandoned: [] };
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
  }
  for (const call of calls) at(call.round).tools.push(call);
  // The round in flight. The engine clears it the instant that round's real
  // narration exists, so the two can never describe one round at once.
  if (partial && typeof partial.round === "number") {
    const r = at(partial.round);
    r.streaming = true;
    r.reasoning = partial.reasoning ?? "";
    r.content = partial.content ?? "";
    r.abandoned = narrations(partial.abandoned);
  }
  return [...byRound.values()].sort((a, b) => a.round - b.round);
}

/**
 * The phase's rounds, however this build's engine described them.
 *
 * A phase recorded before the engine sent `round_narration` has only the
 * joined `response`, and those events are already in the store — an applied
 * write is history, not source, so they have to keep rendering. The join
 * cannot be undone (its parts are separated by a blank line and prose
 * contains blank lines), so the fallback does not try: it puts the whole
 * response in one trailing pseudo-round, which is what the reader used to
 * get, and every round that DOES have narration renders properly.
 */
export function ledgerOf(record: {
  tools: ToolCall[];
  narration: Narration[];
  partial?: PartialRound | null;
  response: string;
}): {
  ledger: Round[];
  legacy: { thinking: string; answer: string } | null;
} {
  const ledger = rounds(record.tools, record.narration, record.partial);
  if (record.narration.length > 0 || record.partial) return { ledger, legacy: null };
  const legacy = splitThinking(record.response);
  if (!legacy.thinking && !legacy.answer.trim()) return { ledger, legacy: null };
  return { ledger, legacy };
}

/** The content of the first message with this role, or "". */
function promptRole(messages: PromptMessage[] | null | undefined, role: string): string {
  if (!Array.isArray(messages)) return "";
  for (const m of messages) {
    if (m && m.role === role && typeof m.content === "string") return m.content;
  }
  return "";
}

/**
 * A phase's identity: `turn|phase|iteration`, plus the task id where there
 * is one.
 *
 * THE TASK ID IS NOT OPTIONAL for a delegated worker. A `delegate` call of
 * eight runs eight `subagent` phases in one executor round, and without it
 * they share one key — so the map keeps the last one to arrive and seven
 * workers, their prompts, their tools and their failures simply are not on
 * the page. A turn's own phases have no task id and keep the three-part key
 * they have always had.
 */
export function phaseKey(turnId: string, phase: string, iteration: number, taskId = ""): string {
  const base = `${turnId}|${phase}|${iteration}`;
  return taskId ? `${base}|${taskId}` : base;
}

/** A phase still running, from a seat's live overlay. */
export function fromLiveCall(call: LiveCall, role: string): PhaseRecord {
  return {
    key: phaseKey(call.turn_id, call.phase, call.iteration),
    turnId: call.turn_id,
    phase: call.phase,
    iteration: call.iteration,
    role,
    model: call.model,
    providerKey: "",
    live: call.in_progress !== false && !call.failed,
    failed: !!call.failed,
    error: call.error?.message ?? "",
    errorKind: call.error?.kind ?? "",
    // Read off `prompt_messages`, which the engine has always sent and
    // nothing read. Hardcoding "" here meant a RUNNING phase could never
    // show the system prompt it was given — the one moment an operator
    // most wants to know what the model was actually told.
    systemPrompt: promptRole(call.prompt_messages, "system"),
    userPrompt: call.prompt ?? promptRole(call.prompt_messages, "user"),
    response: call.response ?? "",
    tools: toolCalls(call.tool_executions),
    narration: narrations(call.round_narration),
    partial: call.partial_round ?? null,
    inputTokens: call.input_tokens,
    outputTokens: call.output_tokens,
    totalTokens: call.total_tokens,
    roundNum: call.round_num,
    roundsUsed: call.rounds,
    exhaustedRounds: false,
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
    trigger: (call.trigger as PhaseRecord["trigger"]) ?? null,
    at: call.updated_at,
    startedAt: call.started_at || call.updated_at,
    eventId: "",
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
  return {
    key: phaseKey(turnId, phase, iteration, taskId),
    turnId,
    phase,
    iteration,
    role: String(p.role ?? ev.actor ?? ""),
    model: String(p.model ?? ""),
    providerKey: String(p.provider_key ?? ""),
    live: false,
    failed: p.failed === true,
    error: String(p.error ?? ""),
    errorKind: String(p.error_kind ?? ""),
    systemPrompt: String(p.system_prompt ?? ""),
    userPrompt: String(p.user_prompt ?? ""),
    response: String(p.response ?? ""),
    tools: toolCalls(p.tool_executions),
    narration: narrations(p.round_narration),
    // A finished phase never has one: the engine clears it the moment the
    // round commits, so the durable event carries no half sentence.
    partial: null,
    inputTokens: num(p.input_tokens),
    outputTokens: num(p.output_tokens),
    totalTokens: num(p.total_tokens),
    roundNum: num(p.rounds_used),
    roundsUsed: num(p.rounds_used),
    exhaustedRounds: p.exhausted_rounds === true,
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
    trigger: (p.trigger as PhaseRecord["trigger"]) ?? null,
    at: ev.timestamp,
    // A finished phase has one instant that matters — when it landed.
    startedAt: ev.timestamp,
    eventId: ev.id,
  };
}

/**
 * Merge the live view and the durable record into one ordered list.
 *
 * The DURABLE record wins on a key collision, always: it is the complete one,
 * and a live call lingering in the projection after its event has landed would
 * otherwise re-blank the fields only the event carries (decision, notes, the
 * verbatim system prompt).
 */
export function mergePhases(stored: PhaseRecord[], live: PhaseRecord[]): PhaseRecord[] {
  const byKey = new Map<string, PhaseRecord>();
  for (const rec of live) byKey.set(rec.key, rec);
  for (const rec of stored) byKey.set(rec.key, rec);
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
  live: boolean;
  failed: boolean;
  totalTokens: number;
  trigger: PhaseRecord["trigger"];
}

/** Group phases into the turns they belong to, newest turn first. */
export function groupTurns(phases: PhaseRecord[]): TurnGroup[] {
  const byTurn = new Map<string, PhaseRecord[]>();
  for (const rec of phases) byTurn.set(rec.turnId, [...(byTurn.get(rec.turnId) ?? []), rec]);
  return [...byTurn.entries()]
    .map(([turnId, list]) => {
      // Within a turn, OLDEST first: a turn is read forwards — onboarding
      // (first turn only), then execute, then review — which is the opposite
      // of a feed. A phase not on this list sorts after the ones that are and
      // then by time, which is right for the nested calls (subagent, judge,
      // auxiliary) that hang off a host phase.
      const ordered = [...list].sort((a, b) => {
        if (a.iteration !== b.iteration) return a.iteration - b.iteration;
        const order = ["onboarding", "execute", "review"];
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
      // The EARLIEST start across the turn's phases. A turn is "running for"
      // as long as its first phase has been going, not its newest round.
      const startedAt = ordered.reduce(
        (min, r) => (min === "" || tsKey(r.startedAt) < tsKey(min) ? r.startedAt : min),
        "",
      );
      return {
        turnId,
        role: ordered[0]?.role ?? "",
        phases: own,
        nested,
        at,
        startedAt,
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
 * Split the model's reasoning off the front of its answer.
 *
 * The engine keeps a phase's reasoning as a `<think>` prefix of `Response`, so
 * this is a documented shape rather than a guess. Reasoning is collapsed by
 * default: it is long, it is not the answer, and a reader scanning a turn for
 * what it DID should not have to scroll past what it considered.
 */
export function splitThinking(response: string): { thinking: string; answer: string } {
  const m = /^\s*<think(?:ing)?>([\s\S]*?)<\/think(?:ing)?>\s*/i.exec(response ?? "");
  if (!m) return { thinking: "", answer: response ?? "" };
  return { thinking: (m[1] ?? "").trim(), answer: (response ?? "").slice(m[0].length) };
}

/**
 * What a phase's decision means, said in words rather than left as an enum.
 *
 * The executor's decision is its OUTCOME — its own last word on the turn —
 * and `incomplete` is the one word here the model did not write: the engine
 * synthesises it when the executor never submitted at all. It is labelled as
 * such, because a reader who cannot tell an engine-written outcome from a
 * model's own is reading a claim as a commitment.
 *
 * An unknown value falls through verbatim rather than being dropped, which is
 * what keeps a row written by a build this bundle predates readable: the
 * retired `plan` phase's `plan` / `direct` / `skip` still render as
 * themselves.
 */
export function decisionLabel(phase: string, decision: string): string {
  if (!decision) return "";
  const p = phase.toLowerCase();
  if (p === "execute") {
    return (
      {
        delivered: "delivered the work",
        no_action: "nothing to do — ended silently",
        blocked: "blocked, and said why",
        incomplete: "never said what it did — the engine marked it incomplete",
      }[decision] ?? decision
    );
  }
  if (p === "review") {
    return (
      {
        done: "accepted the work",
        self_iterate: "sent the turn back for another round",
        failed: "failed — the turn will not retry",
      }[decision] ?? decision
    );
  }
  if (p === "onboarding") {
    return { done: "read its team's pages and marked itself onboarded" }[decision] ?? decision;
  }
  return decision;
}
