/**
 * One reading of every seat's live state, for each surface that shows the
 * running company and its controls: Structure's chart and table, the seat
 * panel and the seat's page.
 *
 * THE ENGINE'S WORDS, NOTHING DERIVED. The state, its word and its tone are
 * `lib/seats.ts`'s; what waits is `lib/inboxWaiting.ts`'s; a model limit is
 * the engine's `provider_limit`. What this adds is the one list of things a
 * person would act on (a seat's problems), the pause a running turn has not
 * reached yet, who may control the seat, and the question its executor asks.
 *
 * A PAUSE IS ON A SEAT BEFORE IT TAKES EFFECT. The engine lets a running turn
 * finish, so a paused seat reads `working` until it does, and `needs` while
 * its executor asks; both are said beside the state ([PauseNote]) rather than
 * left to read as a seat nobody paused.
 */

import { useMemo } from "react";
import { waitedOn } from "./budget.ts";
import { useNow } from "./clock.ts";
import {
  awaitingPerson,
  failureWords,
  indexOrg,
  liveRowFor,
  nameOfIn,
  shownStateOf,
  stateLine,
  stateWord,
  toneOf,
  turnItemKey,
  type NameOf,
  type OrgIndex,
  type Seat,
  type SeatTone,
  type ShownState,
} from "./seats.ts";
import { useAgents, useExecutors, useOrg } from "./store-hooks.ts";
import { useViewer } from "./viewer.ts";
import { mayActOnSeat } from "./useWriteAccess.ts";
import type { AgentRow, ExecutorEntry } from "~/protocol/index.ts";

/** Something on a seat a person would act on, worst first. */
export interface SeatProblem {
  key: "failed" | "limit" | "fallback" | "budget" | "near" | "unplaced";
  /** The short form, for a tag. */
  tag: string;
  /** The whole sentence, for a tooltip and a panel. */
  sentence: string;
  tone: "danger" | "warning" | "info";
}

/**
 * A pause the seat has not reached yet: `pausing` while its running turn
 * finishes, `held` while its executor asks a person (it takes the answer now
 * and carries on once resumed).
 */
export type PauseNote = "pausing" | "held";

/** The note's own words: a quiet word after the state, a tag, and the whole sentence. */
export const PAUSE_NOTE_WORDS: Readonly<
  Record<PauseNote, { note: string; tag: string; sentence: string }>
> = {
  pausing: {
    note: "pausing",
    tag: "Pauses after this turn",
    sentence: "A pause is on it, and it takes effect when the running turn finishes.",
  },
  held: {
    note: "paused",
    tag: "Paused",
    sentence: "A pause is on it. Its executor takes your answer now and carries on once resumed.",
  },
};

/** Which pause, if any, a seat's row carries that its state does not yet say. */
export function pauseNoteOf(row: AgentRow | null | undefined): PauseNote | null {
  if (!row?.paused) return null;
  if (row.activity === "working") return "pausing";
  if (row.activity === "needs") return "held";
  return null;
}

export interface SeatLive {
  seat: Seat;
  row: AgentRow | undefined;
  state: ShownState;
  word: string;
  tone: SeatTone | "neutral";
  /** What it is doing, in the state line's words. */
  line: string;
  /** The work item its turn is on, when it names one. */
  item: string;
  /** Whether a pause is on it, one its running turn has not reached included. */
  paused: boolean;
  pauseNote: PauseNote | null;
  /** People waiting on it, and other queued mail. */
  waiting: { priority: number; background: number };
  problems: SeatProblem[];
  /** Whether the reader may pause, stop, resume and steer it: admin, or a lead above it. */
  controllable: boolean;
  /** Its executor waiting on a person, when one is: the question, and any call it holds. */
  ask: ExecutorEntry | null;
}

/** How long until an instant, as "1h 35m" or "12m"; "" once it has passed. */
export function untilWords(at: string | null | undefined, now: number): string {
  const ms = at ? Date.parse(at) - now : NaN;
  if (!Number.isFinite(ms) || ms <= 0) return "";
  const minutes = Math.round(ms / 60_000);
  const h = Math.floor(minutes / 60);
  return h > 0 ? `${h}h ${minutes % 60}m` : `${minutes}m`;
}

/** The share of its tightest capped window a seat has spent, 0 to 1, or null with none capped. */
export function budgetShare(row: AgentRow | undefined): number | null {
  const windows = row?.budget?.windows ?? [];
  const pick =
    waitedOn(windows, "refusing") ??
    waitedOn(windows, "near") ??
    windows.find((w) => (w.limit ?? 0) > 0);
  if (!pick || !pick.limit) return null;
  return Math.min(1, pick.used / pick.limit);
}

/** A seat's problems, worst first, from its row alone. */
export function problemsOf(row: AgentRow | undefined, now: number): SeatProblem[] {
  const out: SeatProblem[] = [];
  if (!row) return out;
  if (row.last_turn?.outcome === "failed" && row.last_error) {
    const words = failureWords(row.last_error.kind);
    out.push({
      key: "failed",
      tag: words.tag,
      sentence: row.last_error.detail || words.sentence,
      tone: "danger",
    });
  }
  if (row.provider_limit) {
    const resets = untilWords(row.provider_limit.resets_at, now);
    const when = resets ? ` in ${resets}` : " when the limit resets";
    out.push(
      row.provider_limit.parked
        ? {
            key: "limit",
            tag: "Plan limit",
            sentence: `Every model it may use is at its plan's limit. It picks up again on its own${when}.`,
            tone: "danger",
          }
        : {
            key: "fallback",
            tag: "On fallback",
            sentence: `Its first model is at its plan's limit, so it runs on its fallback${when}.`,
            tone: "info",
          },
    );
  }
  const share = budgetShare(row);
  if (row.stopped_reason === "budget") {
    out.push({
      key: "budget",
      tag: "Budget spent",
      sentence: "Its token budget is spent until the window resets or the ceiling is raised.",
      tone: "danger",
    });
  } else if (share !== null && row.budget?.windows?.some((w) => w.state === "near")) {
    const percent = Math.round(share * 100);
    out.push({
      key: "near",
      tag: `Budget ${percent}%`,
      sentence: `It has spent ${percent}% of its tightest token ceiling.`,
      tone: "warning",
    });
  }
  if (row.stopped_reason === "unplaced") {
    out.push({
      key: "unplaced",
      tag: "No node",
      sentence: "No engine node has taken this seat, so it runs nothing.",
      tone: "danger",
    });
  }
  return out;
}

/** One seat's reading. A person's seat has no live row: it reads as its availability. */
export function seatLiveOf(
  seat: Seat,
  row: AgentRow | undefined,
  opts: { now: number; nameOf: NameOf; controllable: boolean; ask: ExecutorEntry | null },
): SeatLive {
  const human = seat.kind === "human";
  const state: ShownState = human ? "idle" : shownStateOf(row);
  return {
    seat,
    row: human ? undefined : row,
    state,
    word: human ? "Person" : stateWord(state),
    tone: human ? "neutral" : toneOf(state),
    line: stateLine(row, { now: opts.now, seat, nameOf: opts.nameOf }),
    item: human ? "" : turnItemKey(row),
    paused: !human && !!row?.paused,
    pauseNote: human ? null : pauseNoteOf(row),
    waiting: {
      priority: row?.waiting?.priority ?? 0,
      background: row?.waiting?.background ?? 0,
    },
    problems: human ? [] : problemsOf(row, opts.now),
    controllable: !human && opts.controllable,
    ask: human ? null : opts.ask,
  };
}

/** The executor of a seat that waits on a person, if one does. */
export function askOf(executors: readonly ExecutorEntry[], seat: Seat): ExecutorEntry | null {
  return executors.find((e) => e.agent_handle === seat.handle && awaitingPerson(e.status)) ?? null;
}

/** Every seat's reading, by handle, with the org it was read from. */
export interface LiveOrg {
  index: OrgIndex;
  live: ReadonlyMap<string, SeatLive>;
  /** False until the org snapshot has arrived: nothing is counted or controlled before. */
  loaded: boolean;
  nameOf: NameOf;
}

export function useLiveOrg(): LiveOrg {
  const org = useOrg();
  const agents = useAgents();
  const executors = useExecutors();
  const viewer = useViewer();
  const now = useNow();
  return useMemo(() => {
    const index = indexOrg(org);
    const nameOf = nameOfIn(index);
    const live = new Map<string, SeatLive>();
    for (const seat of index.seats) {
      const controllable = mayActOnSeat(viewer, index, seat.handle);
      live.set(
        seat.handle,
        seatLiveOf(seat, liveRowFor(agents, seat), {
          now,
          nameOf,
          controllable,
          ask: askOf(executors, seat),
        }),
      );
    }
    return { index, live, loaded: org !== null, nameOf };
  }, [org, agents, executors, viewer, now]);
}

/** The counts the status chips show, over the agent seats a view draws. */
export interface StatusCounts {
  all: number;
  working: number;
  needs: number;
  /** A person's pause, apart from a fault's stop: the two words never share a count. */
  paused: number;
  stopped: number;
  idle: number;
  /** No row from the engine yet: counted on its own, never as idle. */
  offline: number;
  /** Agents with at least one problem. */
  problems: number;
}

export function countsOf(seats: Iterable<SeatLive>): StatusCounts {
  const c: StatusCounts = {
    all: 0,
    working: 0,
    needs: 0,
    paused: 0,
    stopped: 0,
    idle: 0,
    offline: 0,
    problems: 0,
  };
  for (const s of seats) {
    if (s.seat.kind === "human") continue;
    c.all += 1;
    c[s.state] += 1;
    if (s.problems.length > 0) c.problems += 1;
  }
  return c;
}

/** The status chips, by the word an address keeps them under (`show`). */
export const STATUS_FILTERS = [
  "all",
  "working",
  "needs",
  "stopped",
  "idle",
  "offline",
  "problems",
] as const;
export type StatusFilter = (typeof STATUS_FILTERS)[number];

/**
 * The filters the chart's legend keys: the four states. The table's further
 * chips (no state yet, problems) have no key there, so the chart draws every
 * card while one of those is chosen rather than grey cards under no pressed key.
 */
export const LEGEND_FILTERS = [
  "working",
  "needs",
  "stopped",
  "idle",
] as const satisfies readonly StatusFilter[];
export type LegendFilter = (typeof LEGEND_FILTERS)[number];

/** A `show=` value as a filter, every unknown word as all. */
export function statusFilterOf(value: string): StatusFilter {
  return (STATUS_FILTERS as readonly string[]).includes(value) ? (value as StatusFilter) : "all";
}

/** Whether a chip keeps an agent seat. A person's seat is kept by All alone. */
export function matches(s: SeatLive, filter: StatusFilter): boolean {
  if (filter === "all") return true;
  if (s.seat.kind === "human") return false;
  switch (filter) {
    case "working":
    case "needs":
    case "idle":
    case "offline":
      return s.state === filter;
    case "stopped":
      return s.state === "paused" || s.state === "stopped";
    case "problems":
      return s.problems.length > 0;
  }
}

/** How a team's agents are doing, what needs a person first: "1 needs you · 3 working · 6 idle". */
export function summaryOf(seats: Iterable<SeatLive>, none: string): string {
  return summaryLinesOf(seats, none).filter(Boolean).join(" · ");
}

/**
 * The same summary as two lines: what waits on a person (needs you, stopped,
 * paused), then the rest (working, idle, no state yet). Either may be empty;
 * with no agents the first holds `none`.
 */
export function summaryLinesOf(seats: Iterable<SeatLive>, none: string): [string, string] {
  const c = countsOf(seats);
  if (c.all === 0) return [none, ""];
  // EACH COUNT KEEPS ITS WORDS TOGETHER (no-break spaces), so a line breaks only after a separator.
  const line = (parts: string[]) =>
    parts
      .filter(Boolean)
      .map((part) => part.replaceAll(" ", " "))
      .join(" · ");
  return [
    line([
      c.needs ? `${c.needs} ${c.needs === 1 ? "needs" : "need"} you` : "",
      c.stopped ? `${c.stopped} stopped` : "",
      c.paused ? `${c.paused} paused` : "",
    ]),
    line([
      c.working ? `${c.working} working` : "",
      c.idle ? `${c.idle} idle` : "",
      c.offline ? `${c.offline} with no state yet` : "",
    ]),
  ];
}

/**
 * A state's word and a line about it, as one line that says the word once:
 * "Working · Executing on ENG-7", and "Working on ENG-7" where the line
 * already begins with it, which the engine's line does when no phase names
 * what the turn is doing.
 */
export function withWord(word: string, line: string): string {
  if (!line) return word;
  const lead = line.slice(0, word.length);
  const next = line.charAt(word.length);
  const begins = lead.toLowerCase() === word.toLowerCase() && (next === "" || !/\p{L}/u.test(next));
  return begins ? line : `${word} · ${line}`;
}

/** What a seat does, asks or was paused for, without the state word its status already says. */
export function doingLine(s: SeatLive): string {
  const held = s.ask?.held_call;
  if (held) return `Asks to call ${held.tool} on ${held.server}`;
  if (s.ask?.question) return `Asks: ${s.ask.question}`;
  let line = s.line;
  if (s.seat.kind === "agent") {
    for (const lead of [`${s.word} · `, `${s.word}: `]) {
      if (line.startsWith(lead)) {
        const rest = line.slice(lead.length);
        line = rest.charAt(0).toUpperCase() + rest.slice(1);
        break;
      }
    }
  }
  const reason = s.row?.paused?.reason;
  return reason && s.state === "paused" ? `${line} · “${reason}”` : line;
}
