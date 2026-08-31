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

import type { EventRecord, LiveCall, ToolExecution } from "~/protocol/index.ts";
import { tsKey } from "./format.ts";

export interface ToolCall {
  name: string;
  round: number;
  /** JSON text as the engine encoded it, or "" — never a Go-syntax dump. */
  args: string;
  result: string;
  failed: boolean;
}

export interface Round {
  round: number;
  tools: ToolCall[];
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
  worker: string;
  hostPhase: string;
  backend: string;
  codingAgent: string;
  trigger: { type?: string; summary?: string; actor?: string; integration?: string } | null;
  /** When the phase finished, or when the live call last moved. */
  at: string;
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
      // array's own order is the sequence, and it only appends.
      round: typeof rec.round === "number" ? rec.round : i,
      args: str(rec.arguments ?? rec.args),
      result: str(rec.result ?? rec.output ?? rec.error),
      failed: rec.success === false || rec.failed === true || Boolean(rec.error),
    };
  });
}

/**
 * Group a phase's tool calls into rounds.
 *
 * Rounds are ORDERED and only ever appended to, which is the whole property
 * this display depends on: nothing above the insertion point can move, so a
 * reader's eye stays where they left it while a turn runs underneath.
 */
export function rounds(calls: ToolCall[]): Round[] {
  const byRound = new Map<number, ToolCall[]>();
  for (const call of calls) byRound.set(call.round, [...(byRound.get(call.round) ?? []), call]);
  return [...byRound.entries()]
    .sort((a, b) => a[0] - b[0])
    .map(([round, tools]) => ({ round, tools }));
}

export function phaseKey(turnId: string, phase: string, iteration: number): string {
  return `${turnId}|${phase}|${iteration}`;
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
    systemPrompt: "",
    userPrompt: call.prompt ?? "",
    response: call.response ?? "",
    tools: toolCalls(call.tool_executions),
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
    hostPhase: "",
    backend: "",
    codingAgent: "",
    trigger: (call.trigger as PhaseRecord["trigger"]) ?? null,
    at: call.updated_at,
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
  return {
    key: phaseKey(turnId, phase, iteration),
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
    hostPhase: String(p.host_phase ?? ""),
    backend: String(p.backend ?? ""),
    codingAgent: String(p.coding_agent ?? ""),
    trigger: (p.trigger as PhaseRecord["trigger"]) ?? null,
    at: ev.timestamp,
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
  phases: PhaseRecord[];
  /** The newest instant in the group — what the group is ordered by. */
  at: string;
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
      // Within a turn, OLDEST first: a turn is read forwards — plan, then
      // execute, then review — which is the opposite of a feed.
      const ordered = [...list].sort((a, b) => {
        if (a.iteration !== b.iteration) return a.iteration - b.iteration;
        const order = ["plan", "execute", "review"];
        const ai = order.indexOf(a.phase);
        const bi = order.indexOf(b.phase);
        if (ai !== bi) return (ai < 0 ? 99 : ai) - (bi < 0 ? 99 : bi);
        return tsKey(a.at) - tsKey(b.at);
      });
      const at = ordered.reduce((max, r) => (tsKey(r.at) > tsKey(max) ? r.at : max), "");
      return {
        turnId,
        role: ordered[0]?.role ?? "",
        phases: ordered,
        at,
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

/** What a phase's decision means, said in words rather than left as an enum. */
export function decisionLabel(phase: string, decision: string): string {
  if (!decision) return "";
  const p = phase.toLowerCase();
  if (p === "plan") {
    return (
      {
        plan: "planned the work",
        direct: "answered directly, no plan needed",
        skip: "skipped — nothing to do",
      }[decision] ?? decision
    );
  }
  if (p === "review") {
    return (
      { done: "accepted the work", self_iterate: "sent the turn back to Plan" }[decision] ??
      decision
    );
  }
  return decision;
}
