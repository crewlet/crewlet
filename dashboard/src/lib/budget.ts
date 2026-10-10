/**
 * Reading the engine's budget windows.
 *
 * NO THRESHOLD LIVES HERE. Every window arrives with the engine's own `state`
 * (`ok`, `near`, `refusing`), computed once beside the counter it describes,
 * and the screens used to hold three thresholds of their own — 75% on the
 * Budgets table, 90% in the attention queue and the kit meter's own — so one
 * window read as healthy, nearly spent and full at once depending on where it
 * was drawn. What is left for the client is WHICH window to name and how to
 * colour a state, both of which are presentation.
 */

import type { BudgetState } from "~/contract/spend.ts";
import { companyDateLabel } from "~/lib/format.ts";
import { dayLabelIn } from "~/lib/range.ts";
import type { BudgetWindow } from "~/protocol/types.ts";

/** How a period reads in front of "token budget": "the company's daily token
 *  budget". */
export const PERIOD_ADJECTIVE: Readonly<Record<BudgetWindow["period"], string>> = {
  day: "daily",
  week: "weekly",
  month: "monthly",
};

/** How a window reads after a figure's label — "Budget this week", "Tokens ·
 *  today": its span on the company clock. */
export const PERIOD_WORDS: Readonly<Record<BudgetWindow["period"], string>> = {
  day: "today",
  week: "this week",
  month: "this month",
};

/**
 * The window in `state` a scope waits on longest: the one that turns over
 * LAST, the longer period where two turn over together.
 *
 * THE ENGINE'S TIE-BREAK (`coord.Outlasts`), which is how the budget gate
 * names the window it refused in and how a parked seat decides when to wake:
 * naming an earlier window would tell a reader the scope has room again at a
 * moment the gate will still refuse it. The windows arrive in day, week, month
 * order, so the later of two equal instants is the longer period.
 */
export function waitedOn(
  windows: readonly BudgetWindow[] | undefined,
  state: BudgetState,
): BudgetWindow | undefined {
  let out: BudgetWindow | undefined;
  for (const w of windows ?? []) {
    if (w.state !== state) continue;
    if (!out || Date.parse(w.resets_at) >= Date.parse(out.resets_at)) out = w;
  }
  return out;
}

/** The window of `period` in a scope's list, if the list states one. */
export function windowOf(
  windows: readonly BudgetWindow[] | undefined,
  period: BudgetWindow["period"],
): BudgetWindow | undefined {
  return windows?.find((w) => w.period === period);
}

// ---------------------------------------------------------------------------
// When a window turns over
// ---------------------------------------------------------------------------

/*
 * ON THE COMPANY'S CALENDAR, the clock every window is cut on (`timezone` on
 * the budgets answer and the live meter, ADR-0018). A day always turns over at
 * the company's midnight, so its reading is that midnight with the zone NAMED —
 * a reader's own midnight may be another. A week or a month turns over on the
 * company date its first instant falls on, which on a reader's own clock may
 * be the evening before.
 *
 * ONE READING, for the caption and the sentence alike. It lived in a React
 * component, so the attention queue — a pure module — could not reach it and
 * printed the engine's raw `resets_at` ("2026-02-01T00:00:00Z") where every
 * other screen said "Feb 1".
 */

/** The company date a window turns over on ("Feb 1"), or null where the
 *  engine's instant does not parse. */
function turnoverDate(w: BudgetWindow, zone: string | undefined): string | null {
  const at = Date.parse(w.resets_at);
  return Number.isFinite(at) ? companyDateLabel(dayLabelIn(at, zone)) : null;
}

/** "midnight (Asia/Tokyo)": the company's midnight, the zone named. */
function companyMidnightWords(zone: string | undefined): string {
  return `midnight (${zone || "UTC"})`;
}

/**
 * When a window's allowance comes back, as a caption says it: "resets at
 * midnight (Asia/Tokyo)" for a day, "resets Oct 1" for a week or a month.
 * Empty where the engine's instant does not parse.
 */
export function resetsWords(w: BudgetWindow, zone: string | undefined): string {
  const date = turnoverDate(w, zone);
  if (date === null) return "";
  return w.period === "day" ? `resets at ${companyMidnightWords(zone)}` : `resets ${date}`;
}

/**
 * When a window turns over, as the tail of a sentence: "at midnight
 * (Asia/Tokyo)" for a day, "on Oct 1" for a week or a month. Empty where the
 * engine's instant does not parse, so a sentence ends rather than naming a
 * moment nobody can read.
 */
export function turnsOverWords(w: BudgetWindow, zone: string | undefined): string {
  const date = turnoverDate(w, zone);
  if (date === null) return "";
  return w.period === "day" ? `at ${companyMidnightWords(zone)}` : `on ${date}`;
}

// ---------------------------------------------------------------------------
// Typing a ceiling
// ---------------------------------------------------------------------------

/**
 * The largest token ceiling a form writes.
 *
 * JAVASCRIPT'S CEILING, NOT THE ENGINE'S. The engine reads a ceiling as a Go
 * `int64` and would take far more, but the value travels as a JSON number and
 * anything above 2^53-1 is rounded on the way through, so what the engine
 * stored would not be what somebody typed. A ceiling that large is a slipped
 * key rather than a budget, so it is refused, naming the largest one that can
 * be written.
 */
export const MAX_TOKEN_CEILING = Number.MAX_SAFE_INTEGER;

/** What a suffix multiplies by: the same letters `fmtCount` draws a figure in. */
const SCALE: Readonly<Record<string, number>> = {
  k: 1_000,
  m: 1_000_000,
  b: 1_000_000_000,
};

/** A typed ceiling, read: a number of tokens, NO CEILING, or why neither. */
export type TypedCeiling =
  | { readonly ok: true; readonly value: number | null }
  | { readonly ok: false; readonly error: string };

/**
 * A ceiling as a person types it, read into tokens.
 *
 * THE UNIT A PERSON THINKS IN. Every figure on the budget screens is drawn
 * `40M`, `2.5M`, `750k`, so a field that took only `40000000` asked the reader
 * to count zeros under a number they had just read in a different spelling —
 * and one zero short is a ceiling a tenth of the one meant, refusing the
 * company's turns by morning. So the field takes what the screen draws:
 * digits, with `,` `_` or spaces grouping them, and an optional `k`, `M` or
 * `B`. The result must still be a WHOLE number of tokens (`1.2345k` is not),
 * because the engine holds an integer and a rounded one is a value nobody
 * typed.
 *
 * EMPTY IS NO CEILING, never 0. The engine refuses a 0 rather than reading it
 * as unlimited — "remove `token_budget.day` for no daily ceiling" — so an
 * empty field is how the key is left out, and a 0 is refused here in the same
 * terms before any request is sent.
 */
export function readCeiling(period: BudgetWindow["period"], typed: string): TypedCeiling {
  const none = `no ${PERIOD_ADJECTIVE[period]} ceiling`;
  const text = typed.trim().replace(/[\s,_]/g, "");
  if (text === "") return { ok: true, value: null };
  const match = /^(\d+(?:\.\d+)?)([kmb])?$/i.exec(text);
  if (!match) {
    return {
      ok: false,
      error: `Give a whole number of tokens (40000000, or 40M), or leave it empty for ${none}.`,
    };
  }
  const value = Number(match[1]) * (match[2] ? SCALE[match[2].toLowerCase()]! : 1);
  // A product like 2.3 × 10⁶ carries binary noise (2299999.9999999995), so a
  // whole result is judged at a precision far below a token and then taken
  // exactly: the value written is the integer the person meant.
  const whole = Math.round(value);
  if (Math.abs(value - whole) > 1e-6) {
    return {
      ok: false,
      error: `Give a whole number of tokens (40000000, or 40M), or leave it empty for ${none}.`,
    };
  }
  if (whole === 0)
    return { ok: false, error: `A ceiling of 0 is refused: leave it empty for ${none}.` };
  if (whole > MAX_TOKEN_CEILING) {
    return {
      ok: false,
      error: `Give a ceiling of at most ${MAX_TOKEN_CEILING}, or leave it empty for ${none}.`,
    };
  }
  return { ok: true, value: whole };
}

/**
 * Why one window's typed ceiling cannot be written, or `undefined` when it
 * can. A shape a form can see is caught here so a save never sends a value the
 * engine would refuse; how the ceilings relate to each other is the engine's
 * to judge, and it says so in its warnings.
 */
export function tokenBudgetError(
  period: BudgetWindow["period"],
  typed: string,
): string | undefined {
  const read = readCeiling(period, typed);
  return read.ok ? undefined : read.error;
}
