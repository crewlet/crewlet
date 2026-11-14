/**
 * What waits on a reader's decision, composed ONE way for every surface that
 * draws or counts it: Home's Needs your decision card, the Inbox's Needs you,
 * and the Inbox's badge.
 *
 * # Two groups, decided by the engine
 *
 * The engine's `decisions` lists what is put to the reader and, on their own
 * record, the parked runs they may answer though they are put to somebody
 * else: a run whose question went to a delegate, or to a colleague, that the
 * reader may still unblock as the seat's lead or an administrator. Each item
 * says where it stands for the reader (`needs_you`, `may_answer`), decided by
 * the answer route's own authority, so no screen keeps a rule of its own:
 *
 *  - WAITS ON YOU: put to the reader, or theirs to answer and escalated by its
 *    delegates or past their deadline. Listed first and counted by the badge.
 *  - WAITING ON OTHERS, YOU CAN ANSWER: listed after, with the same controls,
 *    and counted by nothing, since nobody has asked the reader yet.
 *
 * Pure but for the count's hooks, and free of any control, so the frame can
 * count what the screens draw without carrying their write controls into the
 * entry chunk (`components/DecisionRow.tsx` draws the rows).
 */

import { useMemo } from "react";
import { useQuery } from "./useQuery.ts";
import { useAgents } from "./store-hooks.ts";
import { conditionsToDecide } from "./attention.ts";
import { useAttentionSplit, useReadsNotices } from "./useAttention.ts";
import { waitedOn } from "./budget.ts";
import { useLeadScope } from "./leadScope.ts";
import { managedBlock, mayChangeConfig } from "./useWriteAccess.ts";
import { useViewer } from "./viewer.ts";
import type {
  AgentRow,
  BudgetWindow,
  DecisionsAnswer,
  ExecutorRow,
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

/**
 * One thing a decision list draws. `others` marks a run the reader may answer
 * that waits on somebody else: the second group, never counted.
 */
export type DecisionSubject =
  | { kind: "ask"; at: string; ask: WorkAskRow; accessChanged?: string[]; others?: boolean }
  | { kind: "run"; at: string; run: ExecutorRow; accessChanged?: string[]; others?: boolean }
  | { kind: "seat"; at?: string; seat: SeatCondition };

/** How many change lines a note names before it counts the rest. */
const NOTED_LINES = 1;

/**
 * The note on a question an agent asked before its access changed: the
 * change, short, so a person answering it knows the agent may no longer be
 * able to act on the answer. Undefined for a question asked under the access
 * the agent holds now.
 */
export function accessNote(subject: DecisionSubject): string | undefined {
  if (subject.kind === "seat" || subject.accessChanged === undefined) return undefined;
  const lines = subject.accessChanged;
  if (lines.length === 0) return "Asked before this agent's access changed.";
  const shown = lines.slice(0, NOTED_LINES).join(" ");
  const rest = lines.length - NOTED_LINES;
  const more = rest > 0 ? ` (and ${rest} more ${rest === 1 ? "change" : "changes"})` : "";
  return `Asked before this agent's access changed: ${shown}${more}`;
}

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
 * The engine's decisions and the stopped seats as one list, newest first: the
 * order Home's card and the Inbox draw each group in. An item the engine says
 * does not wait on the reader yet is marked `others` ([splitDecisions]).
 */
export function decisionSubjects(
  answer: DecisionsAnswer | null,
  seats: readonly SeatCondition[],
): DecisionSubject[] {
  const subjects: DecisionSubject[] = [];
  for (const item of answer?.items ?? []) {
    const accessChanged = item.access_changed?.lines;
    const others = item.needs_you ? undefined : true;
    if (item.kind === "ask" && item.ask)
      subjects.push({ kind: "ask", at: item.at, ask: item.ask, accessChanged, others });
    if (item.kind === "run" && item.run)
      subjects.push({ kind: "run", at: item.at, run: item.run, accessChanged, others });
  }
  for (const seat of seats) subjects.push({ kind: "seat", at: seat.at, seat });
  subjects.sort((a, b) => (Date.parse(b.at ?? "") || 0) - (Date.parse(a.at ?? "") || 0));
  return subjects;
}

/** The two groups a decision list draws: what waits on the reader, and what
 *  they may answer that waits on somebody else. */
export function splitDecisions(subjects: readonly DecisionSubject[]): {
  yours: DecisionSubject[];
  others: DecisionSubject[];
} {
  const yours: DecisionSubject[] = [];
  const others: DecisionSubject[] = [];
  for (const s of subjects) (s.kind !== "seat" && s.others ? others : yours).push(s);
  return { yours, others };
}

/**
 * Which budget-stopped seats are THIS reader's decision, and which are only a
 * condition they can see.
 *
 * A STOPPED SEAT HAS TWO WAYS OUT, and a seat is waiting on a reader exactly
 * when they can take one of them:
 *
 *  - RAISE THE CEILING — a change to the company document, which `/config`
 *    takes from an admin, and a SEAT's ceiling also from whoever leads it
 *    (`lib/leadScope.ts`) — so the reader's access or the units they lead
 *    decide it, never a binding of their own. The company's ceiling, which
 *    stops a seat whose own window has room, is Admin's alone. Where another
 *    system manages the document (ADR-0030), only its writers raise anything:
 *    `configWrite` is `mayChangeConfig` (`lib/useWriteAccess.ts`), the same
 *    answer the raise control is disabled by, and the caller passes no leads —
 *    so a seat is never counted as a decision whose control then refuses the
 *    reader;
 *  - HAND THE ITEM ON — `update_work_item` as the reader, which needs the
 *    engine to serve that write for them (`viewer.acts`) AND an item the seat
 *    was on: a seat stopped between turns has nothing to hand on.
 *
 * Everything else a reader sees is somebody else's decision, so it is counted
 * with the conditions that "need a look", never as one waiting on them: a
 * sentence telling a person a decision waits that they cannot make is the one
 * thing this screen must not say.
 */
export function seatDecisionsFor<T extends { row: AgentRow; window?: BudgetWindow }>(
  conditions: readonly T[],
  reader: {
    /** Whether the reader may change the company's configuration: `mayChangeConfig`. */
    configWrite: boolean;
    acts: readonly string[];
  },
  /**
   * The handles of the seats the reader leads ([LeadScope.seats]) — EMPTY
   * on a managed document, where a lead's raise is refused like anybody's.
   */
  leads: { has(handle: string): boolean } = new Set(),
): { mine: T[]; others: T[] } {
  const mine: T[] = [];
  const others: T[] = [];
  for (const c of conditions) {
    const raises = reader.configWrite || (c.window !== undefined && leads.has(c.row.handle ?? ""));
    const item = c.row.turn?.work_item ?? c.row.live_call?.work_item ?? null;
    const reassign = item !== null && reader.acts.includes("update_work_item");
    (raises || reassign ? mine : others).push(c);
  }
  return { mine, others };
}

/** The leads a managed document honours: none, since it refuses a lead's write too. */
const NO_LEADS: ReadonlySet<string> = new Set();

/**
 * The budget-stopped seats, split by whether the viewer can act on them
 * ([seatDecisionsFor]) with the viewer's own access and the seats they lead.
 */
export function useSeatDecisions(): { mine: SeatCondition[]; others: SeatCondition[] } {
  const viewer = useViewer();
  const agents = useAgents();
  const leads = useLeadScope().seats;
  const managed = managedBlock(viewer) !== null;
  return useMemo(
    () =>
      seatDecisionsFor(
        seatConditionsOf(agents),
        { configWrite: mayChangeConfig(viewer), acts: viewer.acts },
        managed ? NO_LEADS : leads,
      ),
    [agents, viewer, leads, managed],
  );
}

/** What waits on a reader, as one figure: null until it is known. */
export interface NeedsYouCount {
  waiting: number | null;
  /** The engine's count of asks reached its ceiling, so `waiting` is a floor. */
  capped: boolean;
  /**
   * The alarm kinds whose platform notices are in the reader's Needs you, and
   * those they put off, for the engine panel to say where each standing alarm
   * is; null for a reader who is told no notice (`lib/alarms.ts`' alarmPlace).
   */
  alarms?: { needs: readonly string[]; putOff: readonly string[] } | null;
}

/**
 * THE FIGURE THE BADGE SAYS: everything that waits on the reader and nothing
 * else. The engine's `total` of what `decisions` puts to them (the open asks,
 * the runs put to them, and the runs they may answer whose delegates
 * escalated them or let them pass their deadline), the budget-stopped seats
 * they can act on ([seatDecisionsFor]), and the conditions a person decides
 * that are not snoozed. A mention, a comment or a notice that only informs
 * is listed in the Inbox and counted by none of it.
 */
export function needsYouCount(
  answer: DecisionsAnswer | null | undefined,
  seats: number,
  conditions: number,
): NeedsYouCount {
  if (!answer) return { waiting: null, capped: false };
  return { waiting: answer.total + seats + conditions, capped: answer.capped };
}

/**
 * [needsYouCount] for the viewer, asked once by the frame (`useInboxCounts`).
 * The same three readings Home and the Inbox compose their lists from, so the
 * badge and the lists cannot disagree. `enabled` is false where there is
 * nobody to ask for.
 */
export function useNeedsYouCount(enabled: boolean, pollMs: number, owner = ""): NeedsYouCount {
  // ASKED AGAIN ON THE PUSH for the viewer's own record: an ask put to them
  // lands as a notice on it (`refetchOnInboxOf`).
  const decisions = useQuery("decisions", undefined, { enabled, pollMs, refetchOnInboxOf: owner });
  const seats = useSeatDecisions();
  const { shown, putOff } = useAttentionSplit();
  const readsNotices = useReadsNotices();
  return useMemo(() => {
    const alarms = readsNotices
      ? {
          needs: shown.flatMap((a) => (a.alarm ? [a.alarm] : [])),
          putOff: putOff.flatMap((a) => (a.alarm ? [a.alarm] : [])),
        }
      : null;
    return enabled
      ? {
          ...needsYouCount(decisions.data, seats.mine.length, conditionsToDecide(shown).length),
          alarms,
        }
      : { waiting: null, capped: false, alarms };
  }, [enabled, decisions.data, seats.mine.length, shown, putOff, readsNotices]);
}
