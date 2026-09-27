/**
 * The landing screen's arithmetic and its sentences, apart from the screen.
 *
 * PURE FUNCTIONS OVER THE ANSWERS, because every one of them is a small honest
 * decision about what a number covers and how it is said — which window a
 * delta compares, which parts a sub-line names, what a count of nothing reads
 * as — and a decision that can only be exercised through a rendered page is one
 * nobody re-reads.
 *
 * # Prose never prints a zero, and never a wrong plural
 *
 * A figure is a figure: a tile may read `0`. A SENTENCE may not: "0 agents are
 * working" is a sentence a reader stumbles on, "1 conditions need" is one they
 * stop trusting. Every clause here is written for the none, the one and the
 * many ("no agents are working right now", "1 condition needs", "2 conditions
 * need"), and `model.test.ts` holds each.
 */

import type {
  AgentRow,
  BudgetWindow,
  CompanyFeedAnswer,
  DecisionsAnswer,
  FeedEntry,
  FeedScheduleRun,
  WorkFlowPoint,
} from "~/protocol/index.ts";
import { plural } from "~/lib/format.ts";

/** The three windows the screen reads over, and what each is called. */
export const HOME_RANGES = [
  { value: "today", label: "Today", days: 1, words: "today", previous: "yesterday" },
  { value: "7d", label: "7 days", days: 7, words: "7 days", previous: "previous 7 days" },
  { value: "30d", label: "30 days", days: 30, words: "30 days", previous: "previous 30 days" },
] as const;

export type HomeRange = (typeof HOME_RANGES)[number];

/** The default window: a week, the span the design draws. */
export const DEFAULT_RANGE = "7d";

/** A `range=` value as its window, and the default for anything else. */
export function rangeOf(value: string): HomeRange {
  return (
    HOME_RANGES.find((r) => r.value === value) ??
    HOME_RANGES.find((r) => r.value === DEFAULT_RANGE)!
  );
}

/**
 * How many days of the series the screen asks for: the fortnight the
 * completed-per-day chart draws, or two of the window where the window is
 * longer — the window itself and the one before it, which the delta compares.
 */
export function flowPoints(range: HomeRange): number {
  return Math.max(14, 2 * range.days);
}

/** How many tasks were delivered in the last `days` days of a day series, and
 *  in the `days` before those; `previous` is null where the series is too
 *  short to hold them. */
export function completedIn(
  points: readonly WorkFlowPoint[],
  days: number,
): { current: number; previous: number | null } {
  const sum = (from: number, to: number) =>
    points.slice(Math.max(0, from), to).reduce((n, p) => n + p.completed, 0);
  const current = sum(points.length - days, points.length);
  const previous =
    points.length >= 2 * days ? sum(points.length - 2 * days, points.length - days) : null;
  return { current, previous };
}

/** A change as its tile writes it: a signed count, or a signed percentage
 *  where the earlier reading is not zero. Null where there is nothing to
 *  compare — never "+0%" over a window nothing happened in. */
export function deltaWords(
  current: number,
  previous: number | null,
  percent: boolean,
): string | null {
  if (previous === null) return null;
  const diff = current - previous;
  if (percent && previous > 0) {
    const pct = Math.round((diff / previous) * 100);
    return `${pct > 0 ? "+" : pct < 0 ? "−" : "±"}${Math.abs(pct)}%`;
  }
  return `${diff > 0 ? "+" : diff < 0 ? "−" : "±"}${Math.abs(diff).toLocaleString()}`;
}

/** The seats in each state the engine names, for the first tile. */
export interface Crew {
  working: number;
  needs: number;
  stopped: number;
  idle: number;
  total: number;
}

export function crewOf(agents: readonly AgentRow[]): Crew {
  const crew: Crew = { working: 0, needs: 0, stopped: 0, idle: 0, total: agents.length };
  for (const a of agents) {
    if (a.activity === "working") crew.working++;
    else if (a.activity === "needs") crew.needs++;
    else if (a.activity === "stopped") crew.stopped++;
    else if (a.activity === "idle") crew.idle++;
  }
  return crew;
}

/**
 * A tile's second line is PARTS, never one string: the tile joins them with
 * " · " and keeps each part whole, so a line too long for a narrow tile
 * breaks BETWEEN facts ("+17 vs last week" / "· 2 blocked") rather than in
 * the middle of one ("+17 vs last" / "week · 2 blocked"). `joinParts` is the
 * same line as text.
 */
export function joinParts(parts: readonly string[]): string {
  return parts.join(" · ");
}

/** "1 waiting · 1 stopped · 1 idle" — the parts that are not working, each
 *  named only when it has a seat in it. */
export function crewParts(crew: Crew): string[] {
  if (crew.total === 0) return ["No agent seats in the chart"];
  const parts = [
    crew.needs && `${crew.needs.toLocaleString()} waiting`,
    crew.stopped && `${crew.stopped.toLocaleString()} stopped`,
    crew.idle && `${crew.idle.toLocaleString()} idle`,
  ].filter(Boolean) as string[];
  if (parts.length === 0) return crew.working === crew.total ? ["Every seat is working"] : [];
  return parts;
}

/** "2 blocked · 1 overdue", naming only the shapes of trouble that are there. */
export function troubleParts(now: { blocked: number; overdue: number }): string[] {
  return [
    now.blocked && `${now.blocked.toLocaleString()} blocked`,
    now.overdue && `${now.overdue.toLocaleString()} overdue`,
  ].filter(Boolean) as string[];
}

/** The in-progress tile's second line: what its delta compares against, and
 *  the shapes of trouble that are there — "vs last week · 2 blocked". */
export function inProgressParts(compared: boolean, trouble: readonly string[]): string[] {
  return [...(compared ? ["vs last week"] : []), ...trouble];
}

/** The company week window a meter draws, off the org meter, or undefined
 *  where the company caps no week. */
export function weekWindow(windows: readonly BudgetWindow[] | undefined): BudgetWindow | undefined {
  return windows?.find((w) => w.period === "week" && w.limit !== undefined);
}

/**
 * "63% of this week's budget · resets Mon" — the share and when it comes back,
 * labelled as the WEEK's so nobody reads it against the rolling range the
 * tile's own figure covers. The weekday is on the company's clock.
 */
export function budgetCaption(week: BudgetWindow, zone: string | undefined): string[] {
  const limit = week.limit ?? 0;
  const pct = limit > 0 ? Math.round((week.used / limit) * 100) : 0;
  const resets = weekday(week.resets_at, zone);
  const reset = resets ? [`resets ${resets}`] : [];
  if (week.state === "refusing") return ["This week's budget is spent", ...reset];
  return [`${pct}% of this week's budget`, ...reset];
}

function weekday(at: string, zone: string | undefined): string {
  const t = Date.parse(at);
  if (!Number.isFinite(t)) return "";
  try {
    return new Intl.DateTimeFormat(undefined, {
      weekday: "short",
      timeZone: zone || undefined,
    }).format(t);
  } catch {
    return new Intl.DateTimeFormat(undefined, { weekday: "short" }).format(t);
  }
}

/**
 * Which budget-stopped seats are THIS reader's decision, and which are only a
 * condition they can see.
 *
 * A STOPPED SEAT HAS TWO WAYS OUT, and a seat is waiting on a reader exactly
 * when they can take one of them:
 *
 *  - RAISE THE CEILING — a change to the company document, which `/config`
 *    takes from any presented token (`api.auth.tokens` gates writes and all of
 *    `/config`, with no narrower grant), so `viewer.operator`;
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
  viewer: { operator: boolean; acts: readonly string[] },
): { mine: T[]; others: T[] } {
  const mine: T[] = [];
  const others: T[] = [];
  for (const c of conditions) {
    const item = c.row.turn?.work_item ?? c.row.live_call?.work_item ?? null;
    const reassign = item !== null && viewer.acts.includes("update_work_item");
    (viewer.operator || reassign ? mine : others).push(c);
  }
  return { mine, others };
}

/** What waits on the reader: the engine's count, whether it is a floor, and
 *  when the longest one began. Null while it is not known — anonymous,
 *  unbound, or not answered yet — which is never drawn as zero. */
export interface Waiting {
  count: number;
  floor: boolean;
  oldestAt: string | null;
}

export function waitingOf(
  answer: DecisionsAnswer | null,
  seatConditions: readonly { at?: string }[],
): Waiting | null {
  if (!answer || typeof answer.total !== "number") return null;
  let oldest = answer.oldest_at ?? null;
  for (const c of seatConditions) {
    if (c.at && (!oldest || Date.parse(c.at) < Date.parse(oldest))) oldest = c.at;
  }
  return { count: answer.total + seatConditions.length, floor: answer.capped, oldestAt: oldest };
}

/** "2h 10m", "38m", "3d" — how long something has waited. */
export function waitedFor(at: string, now: number): string {
  const t = Date.parse(at);
  if (!Number.isFinite(t)) return "";
  const minutes = Math.max(0, Math.floor((now - t) / 60_000));
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return minutes % 60 ? `${hours}h ${minutes % 60}m` : `${hours}h`;
  return `${Math.floor(hours / 24)}d`;
}

/** One run of the status sentence: plain, or the figure it weights. */
export interface Segment {
  text: string;
  strong?: boolean;
  /** Where the run's subject is listed, when the sentence names something
   *  the reader has to go and look at. */
  href?: string;
}

/**
 * The one sentence under the greeting, as runs of text.
 *
 * THE ORDER IS PRECEDENCE: a refused token and a lost connection are said
 * before anything the last push claimed, because what was pushed may be stale;
 * an engine running no company is said before a fleet size, because a fleet
 * running nothing is not "running"; a node that shed its seats says so rather
 * than beside a sentence that says all is well. Then the fleet, what waits on
 * the reader, the other conditions, and who is working.
 *
 * THE DECISIONS FIGURE IS ITS OWN RUN, weighted by the screen: it is the one
 * number in the sentence that asks something of the reader.
 */
export function statusSentence(input: {
  company: string;
  connected: boolean;
  authRejected: boolean;
  configured: boolean | undefined;
  posture: string | undefined;
  draining: boolean;
  nodes: number | undefined;
  /** What waits on the reader, or null where nobody is bound or it is not known. */
  waiting: Waiting | null;
  /** Other conditions the engine raised that are not the reader's decisions. */
  conditions: number;
  /** Where those conditions are listed — a clause that says something needs
   *  a look and not where would send the reader hunting. */
  conditionsHref?: string;
  working: number;
}): Segment[] {
  const { company, connected, authRejected, configured, posture, draining, nodes } = input;
  const sole = (text: string): Segment[] => [{ text }];
  if (authRejected)
    return sole("The engine refused this browser's token — set one to read the company.");
  if (!connected)
    return sole("Not connected to the engine. What is shown is the last state it sent.");
  if (configured === false) return sole("No configuration is active, so no seat is running.");
  if (posture && ["shed", "stuck", "isolated"].includes(posture)) {
    return sole(
      posture === "shed"
        ? "This node has released its seats: it cannot reach the configuration it should run."
        : "This node cannot converge on the fleet's active configuration.",
    );
  }
  const who = company || "The company";
  const fleet = draining
    ? `${who} is running, and this node is draining.`
    : // A FLEET OF NONE IS NOT A COUNT: the presence read answered nothing
      // it could count, and "running on 0 nodes" is the zero digit a
      // sentence must not print — about a company that is running.
      nodes === undefined || nodes <= 0
      ? `${who} is running.`
      : `${who} is running on ${plural(nodes, "node")}.`;

  // Each clause is one or more runs; the first is capitalised, and they are
  // joined as a list whose last item takes the "and".
  const clauses: Segment[][] = [];
  const w = input.waiting;
  if (w) {
    if (w.count === 0) clauses.push([{ text: "nothing needs your decision" }]);
    else {
      const one = w.count === 1 && !w.floor;
      clauses.push([
        {
          text: `${w.count.toLocaleString()}${w.floor ? "+" : ""} ${one ? "decision" : "decisions"}`,
          strong: true,
        },
        { text: one ? " is waiting on you" : " are waiting on you" },
      ]);
    }
  }
  if (input.conditions > 0) {
    clauses.push([
      {
        text: `${plural(input.conditions, "condition")} ${input.conditions === 1 ? "needs" : "need"} a look`,
        ...(input.conditionsHref ? { href: input.conditionsHref } : {}),
      },
    ]);
  }
  clauses.push([
    {
      text:
        input.working === 0
          ? "no agents are working right now"
          : `${plural(input.working, "agent")} ${input.working === 1 ? "is" : "are"} working right now`,
    },
  ]);

  const out: Segment[] = [{ text: `${fleet} ` }];
  clauses.forEach((clause, i) => {
    if (i > 0) {
      const last = i === clauses.length - 1;
      // ", and" before the last clause at any length, as the design writes
      // it: "3 decisions are waiting on you, and 4 agents are working".
      out.push({ text: last ? ", and " : ", " });
    }
    clause.forEach((run, j) => {
      out.push(i === 0 && j === 0 ? { ...run, text: capitalise(run.text) } : run);
    });
  });
  out.push({ text: "." });
  return out;
}

/** The sentence as plain text, for a test or a label. */
export function sentenceText(segments: readonly Segment[]): string {
  return segments.map((s) => s.text).join("");
}

function capitalise(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/** The feed filter's four settings and the kinds each asks the engine for. */
export const FEED_FILTERS = [
  { value: "all", label: "Everything", kinds: "" },
  { value: "completed", label: "Completed", kinds: "completed" },
  { value: "handoffs", label: "Hand-offs", kinds: "handoff" },
  { value: "schedules", label: "Schedules", kinds: "schedule" },
] as const;

export type FeedFilter = (typeof FEED_FILTERS)[number];

export function feedFilterOf(value: string): FeedFilter {
  return FEED_FILTERS.find((f) => f.value === value) ?? FEED_FILTERS[0];
}

/**
 * The rows of every page read so far, in order, with no row twice — and a
 * schedule's runs that one page ended on and the next began with read as the
 * one row they are: the engine folds consecutive runs within a page, and a
 * page boundary is not something that happened between them.
 */
export function mergePages(pages: readonly CompanyFeedAnswer[]): FeedEntry[] {
  const seen = new Set<string>();
  const out: FeedEntry[] = [];
  for (const page of pages) {
    for (const row of page.rows ?? []) {
      const id =
        row.work?.id ??
        row.page?.id ??
        (row.schedule
          ? `${row.schedule.scope_type}/${row.schedule.scope_id}/${row.schedule.name}@${row.at}/${row.schedule.target ?? ""}`
          : row.at);
      const key = `${row.kind}:${id}`;
      if (seen.has(key)) continue;
      seen.add(key);
      const last = out[out.length - 1];
      if (last?.schedule && row.schedule && sameSchedule(last.schedule, row.schedule)) {
        out[out.length - 1] = {
          ...last,
          schedule: {
            ...last.schedule,
            runs: runsOf(last.schedule) + runsOf(row.schedule),
            since: row.schedule.since ?? row.at,
          },
        };
        continue;
      }
      out.push(row);
    }
  }
  return out;
}

function sameSchedule(a: FeedScheduleRun, b: FeedScheduleRun): boolean {
  return (
    a.scope_type === b.scope_type &&
    a.scope_id === b.scope_id &&
    a.name === b.name &&
    (a.target ?? "") === (b.target ?? "") &&
    (a.outcome ?? "") === (b.outcome ?? "")
  );
}

/** How many runs a row stands for: at least the one it is. */
function runsOf(run: FeedScheduleRun): number {
  return Math.max(1, run.runs ?? 1);
}

/**
 * EVERY SENTENCE A FEED ROW SAYS, in one table keyed on the feed's own kinds.
 *
 * Not `contract/work.ts`'s `CHANGES`: that table is a task's HISTORY — one
 * phrase per change kind, written about "it" ("moved it", "reassigned it")
 * because the task is the page's subject. A feed row is a sentence about the
 * COMPANY with the task as its object ("SWE completed ENG-398 …"), and two of
 * its kinds are not change kinds at all: a completion is a status move INTO a
 * delivered status, a hand-off an assignee move between two holders. So the
 * feed's verbs are their own table, and this is the only one.
 */
export const FEED_PHRASES = {
  completed: "completed",
  created: "filed",
  handoff: "handed",
  page_created: "published",
  page_saved: "updated",
} as const;

/**
 * What a schedule row says the scheduler did, per ledger outcome — each said
 * as what happened. The engine's feed carries only RUNS (`fired`); the two
 * skips are here so that a row carrying one can never read "ran", which is the
 * sentence a tick nobody ran must not say.
 */
export const SCHEDULE_OUTCOMES = {
  fired: { one: "ran", many: (n: number) => `ran ${n.toLocaleString()} times` },
  skipped_catchup: {
    one: "skipped a missed tick",
    many: (n: number) => `skipped ${n.toLocaleString()} missed ticks`,
  },
  skipped_paused: {
    one: "skipped a tick while its runner was paused",
    many: (n: number) => `skipped ${n.toLocaleString()} ticks while its runner was paused`,
  },
} as const satisfies Record<string, { one: string; many: (n: number) => string }>;

/** The verb phrase of a schedule row: "ran", "ran 12 times", "skipped a
 *  missed tick" — and, for an outcome this build does not know, the engine's
 *  own word rather than a guess. */
export function scheduleVerb(run: FeedScheduleRun): string {
  const n = runsOf(run);
  const outcome = run.outcome || "fired";
  const phrase = (
    SCHEDULE_OUTCOMES as Record<string, { one: string; many: (n: number) => string }>
  )[outcome];
  if (!phrase) {
    const word = outcome.replace(/_/g, " ");
    return n > 1 ? `recorded “${word}” ${n.toLocaleString()} times` : `recorded “${word}”`;
  }
  return n > 1 ? phrase.many(n) : phrase.one;
}

/** A chat surface's name as a reader knows it. */
export function surfaceName(surface: string): string {
  const known: Record<string, string> = { slack: "Slack", mattermost: "Mattermost" };
  return known[surface] ?? surface.charAt(0).toUpperCase() + surface.slice(1);
}
