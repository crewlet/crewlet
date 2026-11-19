/**
 * An item in progress's flow, as words: the engine's state, cause and instants
 * (ADR-0162), never a state this side derives. No function here compares a
 * start with the browser's clock; the clock only measures an age to print.
 *
 * Its instants are read on the COMPANY's clock, as Worklet's dates are
 * (`companyDay.ts`): a wait until a start is the company's midnight, which the
 * reader's clock west of the company reads as the evening before.
 */

import type { Tone } from "@crewlethq/ui";
import { FLOW_CAUSES, FLOW_STATES } from "~/contract/work.ts";
import type {
  WorkFlow,
  WorkFlowCause,
  WorkFlowEntry,
  WorkFlowState,
  WorkloadRow,
} from "~/protocol/index.ts";
import { companyZone, dayLabelIn, fmtCompanyDay } from "./companyDay.ts";
import { humanize, parseUTC, plural } from "./format.ts";
import { dateTimeFormat, type DateShape } from "./formatters.ts";
import { zone as readerZone } from "./prefs.ts";
import { shortAge, type SeatKind } from "./seats.ts";

/**
 * What a flow's words name beyond the flow: the company's zone, the item's
 * assignee as the chart knows them, and its project's stuck window in hours
 * where the screen holds the project.
 */
export interface FlowWords {
  zone: string;
  assignee?: string | undefined;
  seatName?: ((handle: string) => string) | undefined;
  seatKind?: ((handle: string) => SeatKind | undefined) | undefined;
  stuckHours?: number | undefined;
}

const CLOCK: DateShape = Object.freeze({ hour: "2-digit", minute: "2-digit", hourCycle: "h23" });
const CLOCK_NAMED: DateShape = Object.freeze({ ...CLOCK, timeZoneName: "short" });
const SECONDS: DateShape = Object.freeze({
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hourCycle: "h23",
});

/**
 * An instant the flow names, on the company's clock: a company midnight is
 * the day it opens ("Nov 22"); any other instant its clock time, after its day
 * where that is not today, and with the zone's name where the reader's clock
 * is another.
 */
export function flowInstant(at: string, now: number, zone: string): string {
  const d = parseUTC(at);
  if (!d) return "";
  const ms = d.getTime();
  const tz = companyZone(zone);
  const day = fmtCompanyDay(at, zone, now);
  if (dateTimeFormat("en-GB", tz, SECONDS).format(ms) === "00:00:00") return day;
  const clock = dateTimeFormat(undefined, tz, tz === readerZone() ? CLOCK : CLOCK_NAMED).format(ms);
  return dayLabelIn(ms, zone) === dayLabelIn(now, zone) ? clock : `${day}, ${clock}`;
}

function stateRow(state: WorkFlowState) {
  return FLOW_STATES.find((row) => row.state === state);
}

function causeRow(cause: WorkFlowCause) {
  return FLOW_CAUSES.find((row) => row.cause === cause);
}

/**
 * What a badge says: the cause's own word where it is the whole story ("No
 * assignee"), else the state's; a state this build does not know reads as its
 * own word.
 */
export function flowLabel(state: WorkFlowState, cause?: WorkFlowCause): string {
  const own = cause ? causeRow(cause) : undefined;
  if (own && "label" in own) return own.label;
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
export function flowWhen(flow: WorkFlow, now: number, zone: string): string {
  if (flow.until) return `until ${flowInstant(flow.until, now, zone)}`;
  return flow.since ? shortAge(flow.since, now) : "";
}

/** "Waiting · until Nov 22", "Stuck · 9h": a badge's whole text. */
export function flowBadgeText(flow: WorkFlow, now: number, zone: string): string {
  const when = flowWhen(flow, now, zone);
  const label = flowLabel(flow.state, flow.cause);
  return when ? `${label} · ${when}` : label;
}

/** The assignee as a sentence names them, and whether they are a person. */
function assigneeOf(words: FlowWords | undefined): { name: string; person: boolean } {
  const handle = words?.assignee ?? "";
  if (!handle) return { name: "its assignee", person: false };
  return {
    name: words?.seatName?.(handle) || handle,
    person: words?.seatKind?.(handle) === "human",
  };
}

/**
 * Why the state holds: its cause's clause, else the state's own, in the
 * assignee's terms (a person picks an item up, an agent takes a turn on it);
 * "" for a state this build does not know.
 */
export function flowWhy(flow: Pick<WorkFlow, "state" | "cause">, words?: FlowWords): string {
  const cause = causeRow(flow.cause);
  if (cause) return cause.phrase;
  switch (flow.state) {
    case "blocked":
      return "an open blocker holds it";
    case "due": {
      const { name, person } = assigneeOf(words);
      return person
        ? `its start passed and ${name} has not picked it up since`
        : `its start passed and ${name} has not taken a turn on it since`;
    }
    case "stuck": {
      const hours = words?.stuckHours;
      return hours
        ? `nothing changed on it for ${plural(hours, "hour")}, its project's stuck window`
        : "nothing changed on it for its project's stuck window";
    }
    case "moving":
      return "nothing holds it";
  }
  return "";
}

/**
 * The item's flow as one sentence: "Held until 13:00: its assignee is out of
 * its model limit". A wait for a start ends with the wait's own words.
 */
export function flowSentence(flow: WorkFlow, now: number, words: FlowWords): string {
  const label = flowLabel(flow.state, flow.cause);
  const head = flow.until
    ? `${label} until ${flowInstant(flow.until, now, words.zone)}`
    : flow.since
      ? `${label} since ${flowInstant(flow.since, now, words.zone)}`
      : label;
  const why = flow.cause === "start" && flow.reason ? flow.reason : flowWhy(flow, words);
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
  words: FlowWords,
): { what: string; why: string } {
  const when = entry.until
    ? ` until ${flowInstant(entry.until, now, words.zone)}`
    : entry.since
      ? ` since ${flowInstant(entry.since, now, words.zone)}`
      : "";
  const state = flowLabel(entry.state).toLowerCase();
  if (entry.notice) {
    const told = words.seatName?.(entry.told) || entry.told;
    const role = entry.notice === "lead" ? ", as its lead," : ", as its assignee,";
    return { what: `told ${told}${role} it reads ${state}${when}`, why: "" };
  }
  return {
    what: `recorded it as ${state}${when}`,
    why: flowWhy(entry, { ...words, assignee: entry.seat }),
  };
}
