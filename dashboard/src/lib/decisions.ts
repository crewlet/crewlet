/**
 * What waits on a reader's decision, composed ONE way for every surface that
 * draws or counts it: Home's Needs your decision card, the Inbox's Needs you,
 * and, where the company runs no Worklet, the Inbox's badge.
 *
 * Pure but for the count's hook, and free of any control, so the frame can
 * count what the screens draw without carrying their write controls into the
 * entry chunk (`components/DecisionRow.tsx` draws the rows).
 */

import { useQuery } from "./useQuery.ts";
import { useAgents } from "./store-hooks.ts";
import { conditionsToDecide } from "./attention.ts";
import { useAttention } from "./useAttention.ts";
import { waitedOn } from "./budget.ts";
import type {
  AgentRow,
  BudgetWindow,
  DecisionsAnswer,
  SandboxRun,
  WorkAskRow,
} from "~/protocol/index.ts";

/** A seat the engine stopped for a spent token budget, as a decision. */
export interface SeatCondition {
  row: AgentRow;
  /** The window that is spent: the seat's own, or the company's. */
  window: BudgetWindow | undefined;
  /** When it last turned a call away, where the gate recorded it. */
  at?: string;
}

export type DecisionSubject =
  | { kind: "ask"; at: string; ask: WorkAskRow }
  | { kind: "run"; at: string; run: SandboxRun }
  | { kind: "seat"; at?: string; seat: SeatCondition };

/**
 * The seats the engine stopped for a spent budget, each with the window that
 * is spent, the seat's own where it names one, else the company's the gate
 * refused on, and when the gate last turned a call away.
 */
export function seatConditionsOf(agents: readonly AgentRow[]): SeatCondition[] {
  const out: SeatCondition[] = [];
  for (const row of agents) {
    if (row.activity !== "stopped" || row.stopped_reason !== "budget") continue;
    const window = waitedOn(row.budget?.windows, "refusing");
    out.push({ row, window, at: window?.refused_at });
  }
  return out;
}

/**
 * The engine's decisions and the stopped seats the reader can act on as one
 * list, newest first: the order Home's card and the Inbox's "Needs you" both draw.
 */
export function decisionSubjects(
  answer: DecisionsAnswer | null,
  seats: readonly SeatCondition[],
): DecisionSubject[] {
  const subjects: DecisionSubject[] = [];
  for (const item of answer?.items ?? []) {
    if (item.kind === "ask" && item.ask) subjects.push({ kind: "ask", at: item.at, ask: item.ask });
    if (item.kind === "run" && item.run) subjects.push({ kind: "run", at: item.at, run: item.run });
  }
  for (const seat of seats) subjects.push({ kind: "seat", at: seat.at, seat });
  subjects.sort((a, b) => (Date.parse(b.at ?? "") || 0) - (Date.parse(a.at ?? "") || 0));
  return subjects;
}

/**
 * How many things wait on the reader in the Inbox's Needs you, where the
 * company runs no Worklet and so no notice counts toward its badge: the
 * asks and parked runs `decisions` answers, the seats stopped on a spent
 * budget and the conditions a person decides. The same three readings the
 * Inbox composes Needs you from, so the badge and the list cannot disagree.
 * Null until `decisions` has answered, or where `enabled` is false.
 */
export function useNeedsYouCount(enabled: boolean): number | null {
  const decisions = useQuery("decisions", undefined, { enabled, pollMs: 60_000 });
  const agents = useAgents();
  const attention = useAttention();
  if (!enabled || !decisions.data) return null;
  return (
    decisionSubjects(decisions.data, seatConditionsOf(agents)).length +
    conditionsToDecide(attention).length
  );
}
