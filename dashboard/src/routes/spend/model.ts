/**
 * The Spend workspace's arithmetic: which window it asks for, and every figure
 * it derives from the engine's answers.
 *
 * PURE FUNCTIONS OVER VALUES, for the reason `lib/spend.ts` gives: a rule about
 * a number a reader acts on — a share, a change against last month, what a
 * task's line says drove it — is re-measured only if it can be exercised
 * without rendering a screen.
 *
 * NOTHING HERE AGGREGATES. The engine folds every window (`internal/tokens`)
 * and computes every judgement (a budget window's `state`, a task's median);
 * this file divides what it was given and chooses words.
 *
 * AND NOTHING HERE IS MONEY. Every figure is a token count — rule 19 of
 * `docs/reference/dashboard-design.md` — and no function takes or returns a
 * price.
 */

import { RANGE_MS, daysLabel, isRange, companyDays, type Offer, type Window } from "~/lib/range.ts";
import { plural } from "~/lib/format.ts";
import { toCsv } from "~/lib/csv.ts";
import type {
  AgentSpendRow,
  Bucket,
  BudgetWindow,
  Rollup,
  WorkRowSpend,
} from "~/protocol/types.ts";

/**
 * WHICH WINDOWS SPEND HAS: whole company days, a week, a month and a quarter
 * or two dates of the reader's own — and nothing finer. A named spend window
 * is read from the replicated usage domain, whose smallest unit is one company
 * day, so there is no hour to draw and no "last 24 hours": the engine would
 * answer it as today and yesterday, which is not what the label said. Ninety
 * is `tokens.MaxSpendRangeDays`; a custom window past it is refused by the
 * engine, in its own words.
 *
 * Declared ONCE and read by both the hook and the control, so the set the URL
 * is checked against cannot differ from the set a reader can see.
 *
 * IT OPENS ON THIRTY DAYS, the Spend artboard's window and the monthly
 * budget's scale beside it: a week of company days is too short a run for a
 * daily chart to say anything about a trend, and a company days old leaves
 * most of a seven-day chart as empty columns.
 */
export function spendOffer(zone: string | undefined): Offer {
  return {
    ranges: ["7d", "30d", "90d"],
    custom: true,
    customDays: true,
    fallback: "30d",
    buckets: ["day"],
    zone,
  };
}

/**
 * The window as the engine's spend questions take it: `days` for a named
 * range — the company days ending today, cut on the COMPANY's clock by the
 * engine rather than subtracted on this browser's — or two company dates,
 * both inclusive, for a window the reader named.
 */
export function spendWindowParams(
  w: Window,
  zone: string | undefined,
): { days: number } | { since: string; until: string } {
  if (isRange(w)) return { days: Math.round(RANGE_MS[w] / 86_400_000) };
  return companyDays(w, zone);
}

/**
 * What the window IS, in the words a heading uses — read from the ANSWER, never
 * from the control: the figures under a label are the engine's window, and a
 * label built from the control would name the window asked for while an older
 * answer was still on screen. A named range is "last 30 days"; two dates are
 * the dates.
 */
export function windowWords(answer: Pick<Rollup, "from" | "to" | "days">, named: boolean): string {
  if (!answer.from || !answer.to) return "this window";
  if (named && answer.days) return `last ${plural(answer.days, "day")}`;
  return daysLabel({ since: answer.from, until: answer.to });
}

/**
 * The change against the window before, as a signed whole percentage — or
 * null when there is nothing to compare with: a previous window that spent
 * nothing makes every change infinite, and "+∞%" is not a figure.
 */
export function changeVsPrevious(current: number, previous: number): number | null {
  if (!(previous > 0)) return null;
  return Math.round(((current - previous) / previous) * 100);
}

/** A change as a hero line writes it: "+18%", "−4%", "no change". */
export function changeWords(change: number): string {
  if (change === 0) return "no change";
  // A TRUE MINUS SIGN, which is the width of the plus beside it; a hyphen
  // reads as a dash in a line of figures.
  return change > 0 ? `+${change}%` : `−${Math.abs(change)}%`;
}

/**
 * The share of input the provider's prompt cache served — `cache_read /
 * input`, the ENGINE's contract (`tokens.Bucket`: the input count already
 * includes the cached prefix, so the two are never added).
 *
 * NULL WHEN NOTHING REPORTED A CACHE. A backend that never states cache counts
 * leaves both at zero, and "0% served from cache" over a provider that simply
 * does not say is a claim about caching nobody made — so a window in which no
 * call read or wrote a cache has no figure, rather than a zero.
 */
export function cacheShare(bucket: Bucket | null | undefined): number | null {
  if (!bucket || !(bucket.input_tokens > 0)) return null;
  const read = bucket.cache_read_tokens ?? 0;
  const write = bucket.cache_write_tokens ?? 0;
  if (read === 0 && write === 0) return null;
  return read / bucket.input_tokens;
}

/** One seat's row in "By agent", with every derived figure computed once. */
export interface AgentLine {
  row: AgentSpendRow;
  /** The seat's share of the window's tokens, 0..1. */
  share: number;
  /** Tokens per ended turn, or null where no turn ended in the window. */
  perTurn: number | null;
  /** The seat's DAILY budget window, as the live meter reports it — absent
   *  where nothing caps the seat's day. */
  day?: BudgetWindow;
}

/**
 * "By agent": every seat that spent in the window, most tokens first, with
 * its share, its tokens per turn and its capped day.
 *
 * THE DAY IS TODAY'S, whatever the window. A budget is a calendar window of
 * its own (ADR-0019), and the live meter reports the CURRENT one; the column
 * says so in its heading rather than dividing a thirty-day spend into one
 * day's allowance.
 *
 * PAIRED BY AGENT ID, never by handle: a rename moves a handle, and a spend
 * row the usage domain wrote before it named the seat by its old one — so
 * paired by handle, a renamed seat's row met nobody's meter, or a newcomer
 * who took the freed handle met the renamed seat's.
 */
export function agentLines(
  rollup: Rollup | null | undefined,
  dayOf: (agentID: string) => BudgetWindow | undefined,
): AgentLine[] {
  const total = rollup?.totals.total_tokens ?? 0;
  return (rollup?.by_agent ?? [])
    .filter((row) => row.total_tokens > 0)
    .map((row) => ({
      row,
      share: total > 0 ? row.total_tokens / total : 0,
      perTurn: row.turns && row.turns > 0 ? Math.round(row.total_tokens / row.turns) : null,
      day: row.agent_id ? dayOf(row.agent_id) : undefined,
    }))
    .sort(
      (a, b) => b.row.total_tokens - a.row.total_tokens || a.row.role.localeCompare(b.row.role),
    );
}

/**
 * Who leaned on a provider entry, in its two parts: the names the engine sent
 * (the top seats) and how many more there were (`seats_total`). TWO PARTS
 * because a row draws them apart — the names may be cut to fit a column, and
 * the count is the one part that must never be, or three names read as the
 * whole list.
 */
export function usedByParts(
  seats: readonly string[],
  total: number,
  nameOf: (handle: string) => string,
): { names: string; more: number } {
  const names = seats.map(nameOf);
  const more = Math.max(0, total - names.length);
  if (names.length === 0) return { names: more > 0 ? plural(more, "seat") : "", more: 0 };
  return { names: names.join(", "), more };
}

/** The same, as one sentence: "SWE, CTO, PM and 2 more". */
export function usedByWords(
  seats: readonly string[],
  total: number,
  nameOf: (handle: string) => string,
): string {
  const { names, more } = usedByParts(seats, total, nameOf);
  return more > 0 ? `${names} and ${more} more` : names;
}

/**
 * What drove a task's spend, as the line under its title: "22 turns ·
 * reopened 4 times · 3 workers · sent back 2 times". A fact at zero is left
 * out — "reopened 0 times" on every row is a column of noise that hides the
 * row where it is not zero — except the turns, which every charged task has.
 */
export function taskFacts(spend: WorkRowSpend): string {
  const facts = [plural(spend.turns, "turn")];
  if (spend.reopens > 0) facts.push(`reopened ${plural(spend.reopens, "time")}`);
  if (spend.workers > 0) facts.push(plural(spend.workers, "worker"));
  if (spend.sent_back > 0) facts.push(`sent back ${plural(spend.sent_back, "time")}`);
  return facts.join(" · ");
}

/**
 * The expensive tasks' question: the tasks last changed inside the window's
 * two instants, most tokens first, each carrying its spend facts.
 *
 * BOUNDED AT BOTH EDGES. `updated=range:since..until` is the tasks whose last
 * change fell inside the window, and `closed_since` keeps open work plus what
 * finished since it began — so a task finished in it is in (finishing it
 * changed it), one nobody touched for a quarter is out, and on a window that
 * ended in the past one that moved only AFTER it is out too. That last half is
 * what a bound only at the start got wrong: a custom range from last month
 * listed tasks that moved this week. The tracker keeps one last-changed
 * instant per task, not a history of them, so a task that moved in the window
 * AND after it is not listed — every row shown did change inside the window,
 * which is the claim the caption makes. A task's tokens are its WHOLE LIFE's:
 * the tracker charges a task, not a day, which the screen says beside the
 * list. Only a task that spent anything is a row: a task no agent worked on is
 * not an expensive one.
 */
export function expensiveTasksParams(
  since: string,
  until: string,
  limit: number,
): Record<string, unknown> {
  return {
    sort: "-spend_tokens",
    closed_since: since,
    updated: `range:${since}..${until}`,
    spend_tokens: "gt:0",
    fields: "spend",
    limit,
  };
}

/**
 * The median task's question: tasks FINISHED inside the window's two instants
 * (half-open, as the engine answers it), that spent anything, with the median
 * and the count of their tokens taken by the tracker over the whole set.
 */
export function medianTaskParams(since: string, until: string): Record<string, unknown> {
  return {
    finished: `range:${since}..${until}`,
    show_closed: "true",
    spend_tokens: "gt:0",
    totals: "spend_tokens:median,spend_tokens:count",
    limit: 1,
  };
}

/**
 * The spend as a CSV: the window's rollup by phase, model, provider and worker,
 * then the by-agent table — each row labelled with its section and the window,
 * so a file forwarded without the screen still says what it is of.
 */
export function spendCsv(rollup: Rollup, lines: readonly AgentLine[], window: string): string {
  const rows: (string | number)[][] = [];
  const bucket = (section: string, name: string, b: Bucket) => {
    rows.push([
      window,
      section,
      name,
      b.total_tokens,
      b.input_tokens,
      b.output_tokens,
      b.cache_read_tokens ?? 0,
      b.calls,
      "",
    ]);
  };
  bucket("total", "", rollup.totals);
  for (const p of rollup.by_phase) bucket("phase", p.phase, p);
  for (const m of rollup.by_model) bucket("model", m.model, m);
  for (const p of rollup.by_provider ?? []) bucket("provider", p.provider_key, p);
  for (const w of rollup.by_worker) bucket("worker", w.worker, w);
  for (const line of lines) {
    rows.push([
      window,
      "agent",
      line.row.handle || line.row.role,
      line.row.total_tokens,
      line.row.input_tokens,
      line.row.output_tokens,
      line.row.cache_read_tokens ?? 0,
      line.row.calls,
      line.row.turns ?? "",
    ]);
  }
  return toCsv(
    [
      "window",
      "section",
      "name",
      "tokens",
      "input_tokens",
      "output_tokens",
      "cache_read_tokens",
      "calls",
      "turns",
    ],
    rows,
  );
}
