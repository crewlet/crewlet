/**
 * What a seat's profile derives from what the engine sent — PURE, so each
 * reading is pinned by a test that names what it protects rather than by a
 * render that happens to exercise it.
 */

import { BUDGET_WINDOWS } from "~/contract/config.ts";
import { PERIOD_WORDS } from "~/lib/budget.ts";
import { fmtCount, tsKey } from "~/lib/format.ts";
import { callWords } from "~/lib/turnsteps.ts";
import { activityOf, roundOf, type OrgIndex, type Seat, type SeatState } from "~/lib/seats.ts";
import type {
  AgentRow,
  BudgetWindow,
  ConfigRole,
  SeatActivityDay,
  ScheduleRow,
  SeatActivityRow,
  TokenBudget,
  TurnRow,
} from "~/protocol/index.ts";

// ---------------------------------------------------------------------------
// The tabs
// ---------------------------------------------------------------------------

/**
 * THE KIND DECIDES THE SET, and the set decides what a `tab=` may resolve to.
 *
 * Turns, Memory and Schedules are properties of a RUNTIME, and a human seat
 * has none — it is addressable and never spawned — so a person's profile is
 * Overview, Work and Settings. A `tab=` naming a tab this kind does not have
 * (or one an earlier build had: `threads`, `cost`, `access`, `model`) lands on
 * Overview through `useTab`'s own fallback rather than on a blank strip.
 *
 * WORK IS ON BOTH: a person has tasks assigned to them and questions put to
 * them, which is the whole of what this product asks a person to do.
 */
export const AGENT_TABS = ["overview", "work", "turns", "memory", "schedules", "settings"] as const;
export const HUMAN_TABS = ["overview", "work", "settings"] as const;
export type SeatTab = (typeof AGENT_TABS)[number];

/** Each tab's word, in the order the strip draws them. */
export const TAB_LABELS: Readonly<Record<SeatTab, string>> = {
  overview: "Overview",
  work: "Work",
  turns: "Turns",
  memory: "Memory",
  schedules: "Schedules",
  settings: "Settings",
};

// ---------------------------------------------------------------------------
// Which seat
// ---------------------------------------------------------------------------

/**
 * The seat a handle names, resolved the ONE way.
 *
 * Three lookups rather than one, because a handle reaches this screen spelled
 * three ways: a link built from the roster carries the handle, a link built
 * from a config field carries the ROLE NAME (`seatPath` addresses a seat the
 * engine reported no handle for by name), and a pasted URL carries whatever
 * somebody typed.
 */
export function findSeat(index: OrgIndex, handle: string): Seat | null {
  return (
    index.byHandle.get(handle) ??
    index.byName.get(handle) ??
    [...index.byHandle.values()].find((s) => s.handle.toLowerCase() === handle.toLowerCase()) ??
    null
  );
}

/** The live row for a seat, matched every way the roster and the overlay agree. */
export function liveRow(
  agents: readonly AgentRow[],
  handle: string,
  seat: Seat | null,
): AgentRow | undefined {
  return agents.find((a) => a.handle === handle || a.id === handle || a.role === seat?.name);
}

// ---------------------------------------------------------------------------
// The header
// ---------------------------------------------------------------------------

/**
 * The state pill's word — short, because the callout under the header says
 * the rest (who paused it and why, which run is waiting): "Working", "Needs
 * you", "Paused", "Stopped · budget", "Not placed", "Idle".
 *
 * Every word is the engine's `activity` and `stopped_reason`; a reason this
 * build does not know reads "Stopped" rather than a guess.
 */
export function pillWord(row: AgentRow | null | undefined): string {
  const state: SeatState = activityOf(row);
  switch (state) {
    case "working":
      return "Working";
    case "needs":
      return "Needs you";
    case "idle":
      return "Idle";
    case "offline":
      return "No state yet";
    case "stopped":
      switch (row?.stopped_reason) {
        case "paused":
          return "Paused";
        case "budget":
          return "Stopped · budget";
        case "provider":
          return "Stopped · provider";
        case "unplaced":
          return "Not placed";
        default:
          return "Stopped";
      }
  }
}

// ---------------------------------------------------------------------------
// The figures
// ---------------------------------------------------------------------------

/**
 * The tokens tile: the seat's CAPPED window where it has one, so the figure
 * and its ceiling ("2.1M of 3M budget") are one window's — else the week the
 * other tiles count, with no ceiling to name.
 *
 * WHICH WINDOW, when several are capped: the one nearest its ceiling — the
 * highest share of its limit used — because that is the one that refuses the
 * seat first; a tie goes to the shorter period, which resets sooner and so is
 * the one a person watches. A meter per window is the peek's; a tile has room
 * for one number.
 */
export function tokensTile(
  windows: readonly BudgetWindow[] | undefined,
  weekTokens: number | undefined,
): { window: BudgetWindow | null; label: string; value: number | undefined; of: number | null } {
  const capped = (windows ?? []).filter((w) => typeof w.limit === "number" && w.limit > 0);
  const order = BUDGET_WINDOWS.map((w) => w.period as string);
  const nearest = capped.reduce<BudgetWindow | null>((best, w) => {
    if (!best) return w;
    const share = w.used / (w.limit ?? 1);
    const bestShare = best.used / (best.limit ?? 1);
    if (share !== bestShare) return share > bestShare ? w : best;
    return order.indexOf(w.period) < order.indexOf(best.period) ? w : best;
  }, null);
  if (nearest) {
    return {
      window: nearest,
      label: `Tokens · ${PERIOD_WORDS[nearest.period]}`,
      value: nearest.used,
      of: nearest.limit ?? null,
    };
  }
  return { window: null, label: "Tokens · 7 days", value: weekTokens, of: null };
}

/** A window as a rate after a ceiling: "60M/day" — a tile's line has room
 *  for two of them, where "60M a day and 900M a month" stood four lines
 *  tall in a phone's tile. */
const PER_PERIOD: Readonly<Record<BudgetWindow["period"], string>> = {
  day: "/day",
  week: "/week",
  month: "/month",
};

/**
 * The company's own ceilings — the ones that bound every seat, including one
 * that writes none of its own — as the sentence that NAMES them: "the
 * company's 60M/day applies", "the company's 60M/day and 900M/month
 * apply", or "" where the company caps nothing. `org.token_budget` is
 * the budget AS WRITTEN on the public chart; a window with no key has no
 * ceiling.
 *
 * NAMED, NOT POINTED AT: "the company's applies" left out the noun and the
 * number, while the Settings tab one click away said "the company's 60M
 * applies" of the same ceiling.
 */
export function companyCeilings(budget: TokenBudget | undefined): string {
  const caps = BUDGET_WINDOWS.flatMap(({ period }) => {
    const limit = budget?.[period];
    return typeof limit === "number" ? [`${fmtCount(limit)}${PER_PERIOD[period]}`] : [];
  });
  if (caps.length === 0) return "";
  const list = caps.length === 1 ? caps[0]! : `${caps.slice(0, -1).join(", ")} and ${caps.at(-1)!}`;
  return `the company's ${list} ${caps.length === 1 ? "applies" : "apply"}`;
}

/**
 * The company's ceiling in a tile's one line: its FIRST window — the day's
 * where one is set, the tightest a founder writes — as "company cap
 * 60M/day", with a "+n" for the others. The whole sentence ([companyCeilings],
 * and that the seat has no cap of its own) is the tile's title: at 1440 "no
 * seat cap · the company's 60M/day and 900M/month apply" wrapped to three
 * lines and stood the whole KPI row 150px tall against the artboard's
 * one-line "of 3M budget", and even "no seat cap · company 60M/day +1" took
 * two in a quarter of the Overview's main column.
 */
export function companyCeilingShort(budget: TokenBudget | undefined): string {
  const caps = BUDGET_WINDOWS.flatMap(({ period }) => {
    const limit = budget?.[period];
    return typeof limit === "number" ? [`${fmtCount(limit)}${PER_PERIOD[period]}`] : [];
  });
  if (caps.length === 0) return "";
  return `company cap ${caps[0]!}${caps.length > 1 ? ` +${caps.length - 1}` : ""}`;
}

/** A signed change for a delta: "+9", "−3", "±0". */
export function signed(n: number): string {
  if (n > 0) return `+${n.toLocaleString()}`;
  if (n < 0) return `−${Math.abs(n).toLocaleString()}`;
  return "±0";
}

/**
 * The fortnight the Turns-per-day chart draws: the previous window's days and
 * then this window's, oldest first — both off the ONE answer the KPIs read, so
 * the chart and the "+n vs last week" beside it are the same days.
 */
export function fortnight(row: SeatActivityRow | undefined): SeatActivityDay[] {
  if (!row) return [];
  return [...(row.previous?.per_day ?? []), ...row.per_day];
}

/** How many of those days' turns failed. */
export function failedIn(days: readonly SeatActivityDay[]): number {
  return days.reduce((n, d) => n + d.failed, 0);
}

// ---------------------------------------------------------------------------
// The current turn's calls
// ---------------------------------------------------------------------------

/** One row of the current turn's call feed. */
export interface FeedRow {
  key: string;
  /** When the call was handed to the tool; "" where nothing timed it. */
  at: string;
  name: string;
  /** Its arguments as a person reads them. */
  words: string;
  /** How long it took, finished; absent while it runs or where nothing timed it. */
  tookMs?: number;
  failed: boolean;
  /** The call running now. */
  running: boolean;
}

/**
 * How many rows the card's call feed holds, the running call's included — a
 * screenful of the phase's newest work, which is what "what is it doing" asks;
 * the whole ledger is the turn's own page.
 */
export const FEED_ROWS = 5;

/**
 * The calls the turn's phase in flight has made, newest last, with the one
 * running now at the foot — every time and duration the engine's own
 * (`started_at`, `duration_ms`, `running_call.started_at`), never a gap
 * between two pushes measured here. At most `limit` rows, the running one
 * counted: it is the row the card exists for, so it is never the one cut.
 */
export function feedRows(row: AgentRow, limit = FEED_ROWS): FeedRow[] {
  const call = row.live_call;
  if (!call) return [];
  const running = call.running_call;
  const room = Math.max(0, limit - (running ? 1 : 0));
  const done: FeedRow[] = (call.tool_executions ?? []).map((ex, i) => ({
    key: `done-${ex.round ?? 0}-${i}`,
    at: ex.started_at ?? "",
    name: ex.name ?? ex.tool ?? "tool",
    words: callWords(ex.arguments ?? ex.args),
    ...(typeof ex.duration_ms === "number" ? { tookMs: ex.duration_ms } : {}),
    failed: ex.failed === true || Boolean(ex.error),
    running: false,
  }));
  const shown = room > 0 ? done.slice(-room) : [];
  if (running) {
    shown.push({
      key: `running-${running.round}-${running.name}`,
      at: running.started_at,
      name: running.name,
      words: callWords(running.arguments),
      failed: false,
      running: true,
    });
  }
  return shown;
}

/** "round 7 of 25", or "" before the first round. */
export function roundWords(row: AgentRow): string {
  const call = row.live_call;
  const round = roundOf(call);
  if (round <= 0) return "";
  return call?.max_rounds ? `round ${round} of ${call.max_rounds}` : `round ${round}`;
}

/**
 * The tokens the turn in flight has spent so far: every phase the store has
 * recorded for it — the seat's newest turn row, when that row IS this turn —
 * and the phase still running, which no row holds until it completes.
 *
 * NULL UNTIL THE ROW IS ANSWERED. A newest row naming ANOTHER turn is an
 * answer — this turn has recorded no phase yet — but no answer at all is not
 * a zero, and a figure that jumped from the running phase's share to the
 * turn's when the read landed would be a number that changed its meaning.
 */
export function turnTokens(
  turnId: string,
  newest: TurnRow | undefined,
  answered: boolean,
  row: AgentRow,
): number | null {
  if (!answered || !turnId) return null;
  const recorded = newest && newest.turn_id === turnId ? newest.total_tokens : 0;
  const call = row.live_call;
  const running = call && call.in_progress && call.turn_id === turnId ? call.total_tokens : 0;
  return recorded + running;
}

// ---------------------------------------------------------------------------
// The setup card
// ---------------------------------------------------------------------------

/**
 * Where a seat's code work runs, in a reader's words: "not offered", or the
 * cell and the coding agent — "e2b · claude-code". An empty `run_in` or
 * `coding_agent` inherits the provider's default, and says so rather than
 * naming a default this client would have to guess.
 */
export function sandboxWords(role: ConfigRole): string {
  const box = role.sandbox as
    { enabled?: boolean; run_in?: string; coding_agent?: string } | undefined;
  if (!box?.enabled) return "not offered";
  return [box.run_in || "the provider's default cell", box.coding_agent]
    .filter(Boolean)
    .join(" · ");
}

/**
 * Which nodes may hold a seat, as its placement says: pinned to one node,
 * limited to nodes carrying labels, or any node that runs seats.
 */
export function placementWords(role: ConfigRole): string {
  const place = role.placement as { node?: string; labels?: Record<string, string> } | undefined;
  if (place?.node) return `only ${place.node}`;
  const labels = Object.entries(place?.labels ?? {});
  if (labels.length) return `nodes labelled ${labels.map(([k, v]) => `${k}=${v}`).join(", ")}`;
  return "any node";
}

/**
 * The delegate templates a seat's executor may hand work to. An empty list is
 * the document's "every one" — `workers:` NARROWS the company's templates,
 * it never grants — so it is said as that rather than as none.
 */
export function workersWords(role: ConfigRole): string {
  const workers = (role.workers ?? []).filter(Boolean);
  return workers.length ? workers.join(", ") : "every template the company defines";
}

// ---------------------------------------------------------------------------
// The recurring work
// ---------------------------------------------------------------------------

/**
 * The schedules that wake a seat, from the RESOLVED rows every reader is
 * pushed: its own (`scope_type` role) and every unit schedule whose fire
 * reaches it (`runners`, the engine's answer to whose day a unit schedule
 * lands in). Soonest first, and a schedule that cannot fire — no `next_run` —
 * last, so the card's head is what happens next.
 *
 * NOT the `schedules:` a seat AUTHORED in the company document: that is an
 * operator read, carries no effective zone, next run or problem, and never
 * sees a unit schedule at all — so the rows that actually wake a seat were
 * the ones an authored read could not find.
 */
export function seatSchedules(rows: readonly ScheduleRow[], handle: string): ScheduleRow[] {
  return rows
    .filter(
      (row) =>
        (row.scope_type === "role" && row.scope_id === handle) || row.runners.includes(handle),
    )
    .sort((a, b) => {
      const at = tsKey(a.next_run) || Infinity;
      const bt = tsKey(b.next_run) || Infinity;
      return at === bt ? a.name.localeCompare(b.name) : at - bt;
    });
}

/**
 * A person's line as a sentence: their own full stop kept, one added where
 * they wrote none — so a reason ending "lands." is not followed by a second.
 */
export function sentence(text: string): string {
  const t = text.trim();
  return /[.!?…]$/.test(t) ? t : `${t}.`;
}
