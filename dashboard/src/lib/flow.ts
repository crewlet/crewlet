/**
 * An item in progress's flow, as words: the engine's state, cause and instants
 * (ADR-0162), never a state this side derives. No function here compares a
 * start with the browser's clock; the clock only measures an age to print.
 */

import type { Tone } from "@crewlethq/ui";
import { FLOW_CAUSES, FLOW_STATES } from "~/contract/work.ts";
import type { WorkFlow, WorkFlowEntry, WorkFlowState, WorkloadRow } from "~/protocol/index.ts";
import { fmtAt, humanize, plural } from "./format.ts";
import { shortAge } from "./seats.ts";

/** Why a state holds where no cause says more. */
const STATE_WHY: Record<string, string> = {
  blocked: "an open blocker holds it",
  due: "its start passed and nothing of its assignee's has run on it since",
  stuck: "nothing changed on it for its project's window",
  moving: "nothing holds it",
};

function stateRow(state: WorkFlowState) {
  return FLOW_STATES.find((row) => row.state === state);
}

/** What a badge says for a state; a state this build does not know reads as its own word. */
export function flowLabel(state: WorkFlowState): string {
  return stateRow(state)?.label ?? humanize(state);
}

/** The tone a state is drawn in. */
export function flowTone(state: WorkFlowState): Tone {
  return stateRow(state)?.tone ?? "neutral";
}

/** The flow a badge draws: none on an item not in progress, and none on the ordinary `moving`. */
export function badgedFlow(flow: WorkFlow | null | undefined): WorkFlow | null {
  return flow && flow.state !== "moving" ? flow : null;
}

/** A badge's time: when the state ends where the engine knows, else how long it has held. */
export function flowWhen(flow: WorkFlow, now: number): string {
  if (flow.until) return `until ${fmtAt(flow.until, now)}`;
  return flow.since ? shortAge(flow.since, now) : "";
}

/** "Waiting · until Nov 20", "Stuck · 9h": a badge's whole text. */
export function flowBadgeText(flow: WorkFlow, now: number): string {
  const when = flowWhen(flow, now);
  return when ? `${flowLabel(flow.state)} · ${when}` : flowLabel(flow.state);
}

/** Why the state holds: its cause's clause, else the state's own; "" for one this build does not know. */
export function flowWhy(flow: Pick<WorkFlow, "state" | "cause">): string {
  return FLOW_CAUSES.find((row) => row.cause === flow.cause)?.phrase ?? STATE_WHY[flow.state] ?? "";
}

/**
 * The item's flow as one sentence: "Held until 13:00: its assignee is out of
 * its model limit". A wait for a start ends with the wait's own words.
 */
export function flowSentence(flow: WorkFlow, now: number): string {
  const head = flow.until
    ? `${flowLabel(flow.state)} until ${fmtAt(flow.until, now)}`
    : flow.since
      ? `${flowLabel(flow.state)} since ${fmtAt(flow.since, now)}`
      : flowLabel(flow.state);
  const why = flow.cause === "start" && flow.reason ? flow.reason : flowWhy(flow);
  return why ? `${head}: ${why}` : head;
}

/**
 * A seat's items in progress as Workload counts them: its load, then each
 * state that keeps an item from moving, zeros left out.
 */
export function workloadWords(row: Pick<WorkloadRow, "flow" | "load">): string[] {
  const out = row.load > 0 ? [`${row.load} to move now`] : [];
  for (const state of ["waiting", "held", "due", "stuck"] as const) {
    const n = row.flow?.[state] ?? 0;
    if (n > 0) out.push(`${n} ${flowLabel(state).toLowerCase()}`);
  }
  return out;
}

/** "rui · 4 open items · 2 to move now": a person picker's line for a seat. */
export function openWords(row: Pick<WorkloadRow, "open" | "load"> | undefined): string {
  const open = row?.open ?? 0;
  if (open === 0) return "nothing open";
  const load = row?.load ?? 0;
  return load > 0
    ? `${plural(open, "open item")} · ${load} to move now`
    : plural(open, "open item");
}

/**
 * One recorded flow record as a history line after "The engine": "recorded it
 * as held until 13:00", "told Maya, as its lead, it reads stuck since Nov 18".
 */
export function flowEntryWords(
  entry: WorkFlowEntry,
  now: number,
  seatName: (handle: string) => string = (handle) => handle,
): { what: string; why: string } {
  const when = entry.until
    ? ` until ${fmtAt(entry.until, now)}`
    : entry.since
      ? ` since ${fmtAt(entry.since, now)}`
      : "";
  const state = flowLabel(entry.state).toLowerCase();
  if (entry.notice) {
    const role = entry.notice === "lead" ? ", as its lead," : ", as its assignee,";
    return { what: `told ${seatName(entry.told)}${role} it reads ${state}${when}`, why: "" };
  }
  return { what: `recorded it as ${state}${when}`, why: flowWhy(entry) };
}
