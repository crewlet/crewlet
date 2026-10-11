/**
 * The seats the engine STOPPED for a spent token budget, as the console reads
 * them: which ones there are, which of them are this reader's decision, and
 * the row each is drawn as — the Overview's third kind of decision, beside
 * the asks and the parked runs every screen draws (`components/DecisionRow.tsx`).
 *
 * THE CONSOLE'S, AND ONLY THE CONSOLE'S. Both ways out of a budget stop start
 * from the budget — the window that is spent, how far over it is, the ceiling
 * that refused — and spend is what the machine processed, which a member is
 * not served (ADR-0031): a member's seat rows carry no budget, the meter a
 * member is pushed carries no figure, and raising a ceiling is a `/config`
 * change, which is an admin's. So none of this is in the person shell, whose
 * screens ask only what a member is served (`app/shell.test.ts`); it sat in
 * the shared row once, and that put the budgets read and the admin's meter in
 * the import graph of Today, the Inbox and My work.
 *
 * Drawn with the shared row's own parts ([DecisionLine]), so every decision on
 * the Overview is one shape.
 */

import { useMemo } from "react";
import { EMPTY_VALUE } from "@crewlethq/ui";
import {
  DecisionKey,
  DecisionLine,
  decisionFacts,
  decisionSeat,
  decisionSubjects,
  newestFirst,
  subjectKey,
  type DecisionSubject,
} from "~/components/DecisionRow.tsx";
import { AssignButton } from "~/components/writes.tsx";
import { RaiseBudgetButton } from "~/routes/admin/spend/budgetWrite.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { PERIOD_ADJECTIVE, waitedOn } from "~/lib/budget.ts";
import { relTime } from "~/lib/format.ts";
import type { AgentRow, BudgetWindow, DecisionsAnswer } from "~/protocol/index.ts";

/** A seat the engine stopped for a spent token budget, as a decision. */
export interface SeatCondition {
  row: AgentRow;
  /** The window that is spent: the seat's own, or the company's. */
  window: BudgetWindow | undefined;
  /** When it last turned a call away, where the gate recorded it. */
  at?: string;
}

/** A decision on the Overview: an ask, a parked run, or a stopped seat. */
export type ConsoleSubject = DecisionSubject | { kind: "seat"; at?: string; seat: SeatCondition };

/**
 * The seats the engine stopped for a spent budget, each with the window that
 * is spent — the seat's own where it names one, else the company's the gate
 * refused on — and when the gate last turned a call away.
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
 * Which budget-stopped seats are THIS reader's decision, and which are only a
 * condition they can see.
 *
 * A STOPPED SEAT HAS TWO WAYS OUT, and a seat is waiting on a reader exactly
 * when they can take one of them:
 *
 *  - RAISE THE CEILING — a change to the company document, which `/config`
 *    takes from an admin key (ADR-0031) and, where another system manages
 *    the document (ADR-0030), only from one of its writers: `mayChangeConfig`
 *    (`lib/useWriteAccess.ts`), the same answer the raise control is
 *    disabled by, so a seat is never counted as a decision whose control
 *    then refuses the reader;
 *  - HAND THE ITEM ON — `update_work_item` as the reader, which needs the
 *    engine to serve that write for them (`viewer.acts`) AND an item the seat
 *    was on: a seat stopped between turns has nothing to hand on.
 *
 * Everything else a reader sees is somebody else's decision, so it is counted
 * with the conditions that "need a look", never as one waiting on them: a
 * sentence telling a person a decision waits that they cannot make is the one
 * thing this screen must not say.
 */
export function seatDecisionsFor<T extends { row: AgentRow }>(
  conditions: readonly T[],
  reader: {
    /** Whether the reader may change the company's configuration: `mayChangeConfig`. */
    configWrite: boolean;
    acts: readonly string[];
  },
): { mine: T[]; others: T[] } {
  const mine: T[] = [];
  const others: T[] = [];
  for (const c of conditions) {
    const item = c.row.turn?.work_item ?? c.row.live_call?.work_item ?? null;
    const reassign = item !== null && reader.acts.includes("update_work_item");
    (reader.configWrite || reassign ? mine : others).push(c);
  }
  return { mine, others };
}

/**
 * The engine's decisions and the stopped seats the reader can act on as one
 * list, in the order every list of decisions is drawn in ([newestFirst]).
 */
export function consoleSubjects(
  answer: DecisionsAnswer | null,
  seats: readonly SeatCondition[],
): ConsoleSubject[] {
  const subjects: ConsoleSubject[] = [...decisionSubjects(answer)];
  for (const seat of seats) subjects.push({ kind: "seat", at: seat.at, seat });
  return subjects.sort(newestFirst);
}

/** A stable key for one subject: the shared row's, or the seat's own. */
export function consoleSubjectKey(s: ConsoleSubject): string {
  return s.kind === "seat" ? `seat:${s.seat.row.id}` : subjectKey(s);
}

/**
 * A budget-stopped seat as a decision: which window is spent, the item it was
 * on, and the two ways out — raise the ceiling, or hand the item on.
 */
export function SeatRow({ seat, now }: { seat: SeatCondition; now: number }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const handle = seat.row.handle;
  const who = decisionSeat(index, handle);
  const item = seat.row.turn?.work_item ?? seat.row.live_call?.work_item ?? null;
  const period = seat.window ? PERIOD_ADJECTIVE[seat.window.period] : "";
  const sub = decisionFacts([
    item ? <DecisionKey key="k" value={item.key} /> : "",
    item ? "stopped mid-turn" : "",
    seat.at ? relTime(seat.at, now) : "",
  ]);
  return (
    <DecisionLine
      who={who}
      ring="danger"
      title={`${who.name} stopped — ${period ? `${period} ` : ""}token budget exhausted`}
      sub={sub || EMPTY_VALUE}
      actions={
        <>
          {/* RAISED WHERE IT IS REPORTED: the dialog opens on the scope
              that is refusing this seat, the stopped window focused. */}
          <RaiseBudgetButton handle={handle} name={who.name} window={seat.window} />
          {item && <ReassignItem item={item.key} />}
        </>
      }
    />
  );
}

/**
 * Hand the item a stopped seat was on to somebody else — conditional on the
 * version this reader saw, like every assignment (`AssignButton`).
 */
function ReassignItem({ item }: { item: string }) {
  const read = useQuery("work_item", { id: item });
  const task = read.data?.task;
  if (!task) return null;
  return <AssignButton item={item} version={task.version} assignee={task.assignee ?? ""} />;
}
