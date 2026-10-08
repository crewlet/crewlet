/**
 * A turn's AUXILIARY SPEND: what the seat's cheap model cost for the work that
 * is not a phase — the turn-start memory filter, knowledge query and episode
 * summary, the rewrites its ledgers needed, its card, and the reflection
 * workers after it.
 *
 * Read from the engine's `auxiliary_spend` records, one per (stage, purpose,
 * model, provider entry) per flush of the node's ledger — coalesced, so a
 * compaction of seventy rewrites is one record. ACCOUNTING, NOT A TRANSCRIPT:
 * a record carries no prompt and no response, only what was spent, in how many
 * calls, and when the first began and the last returned.
 *
 * THE STAGE IS WHOSE COST IT IS (`internal/events/types/auxiliary.go`):
 *
 *  - `turn` — spent inside the turn, for its work. Part of the turn's cost,
 *    in the turn list's tokens and on the work item it was charged to;
 *  - `reflection` — spent after the turn, on the seat's behalf. Drawn beside
 *    the turn, in its Reflection lane, and never in its total — it is how much
 *    the seat had to remember, not what the work cost.
 *
 * A record arrives up to one ledger flush (15 s) after its last call — a
 * turn's in-turn records are flushed when the turn ends — so a running turn's
 * figure trails its phases by that much.
 */

import type { EventRecord } from "~/protocol/index.ts";

/** The `auxiliary_spend` wire type. */
export const AUXILIARY_SPEND = "auxiliary_spend";

/** One coalesced record, as its payload states it. */
export interface AuxRecord {
  id: string;
  /** `turn`, `reflection`, `background` or `operator`. */
  stage: string;
  /** What the calls were for: `memory_filter`, `condense_thread`, … */
  purpose: string;
  model: string;
  providerKey: string;
  calls: number;
  failedCalls: number;
  inputTokens: number;
  outputTokens: number;
  totalTokens: number;
  /** When the first call began and the last returned, RFC 3339. */
  startedAt: string;
  endedAt: string;
  /** The calls' own time, summed. */
  durationMs: number;
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

function num(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

/** Every auxiliary record among a turn's events, oldest call first. */
export function auxiliaryRecords(events: readonly EventRecord[]): AuxRecord[] {
  const out: AuxRecord[] = [];
  for (const ev of events) {
    if (ev.type !== AUXILIARY_SPEND) continue;
    const p = (ev.payload as Record<string, unknown> | undefined) ?? {};
    out.push({
      id: ev.id,
      stage: str(p.stage),
      purpose: str(p.purpose),
      model: str(p.model) || str(p.provider_key),
      providerKey: str(p.provider_key),
      calls: num(p.calls),
      failedCalls: num(p.failed_calls),
      inputTokens: num(p.input_tokens),
      outputTokens: num(p.output_tokens),
      totalTokens: num(p.total_tokens),
      startedAt: str(p.started_at) || ev.timestamp,
      endedAt: str(p.ended_at) || ev.timestamp,
      durationMs: num(p.duration_ms),
    });
  }
  return out.sort((a, b) => Date.parse(a.startedAt) - Date.parse(b.startedAt));
}

/** The tokens the records of one stage add up to. */
export function auxiliaryTokens(records: readonly AuxRecord[], stage: string): number {
  return records.reduce((n, r) => n + (r.stage === stage ? r.totalTokens : 0), 0);
}

/** A purpose as a reader says it: `condense_thread` is "condense thread". */
export function purposeLabel(purpose: string): string {
  return purpose ? purpose.replaceAll("_", " ") : "auxiliary call";
}
