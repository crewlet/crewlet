/**
 * Changing a token ceiling, as values: which document a scope's ceilings live
 * in, what the write that changes one carries, what it is recorded as, and
 * what the engine's answer to it means.
 *
 * PURE, so every rule a screen's Save turns on is pinned by a test without a
 * network or a clock: `lib/useCeilingWrite.ts` is the half that sends it.
 *
 * # Two scopes, two writes
 *
 * The COMPANY's ceilings are a top-level key of the company document, so they
 * change by a merge patch naming only the windows that change
 * (`{token_budget: {day: 50000000}}`, and `null` to remove one): a colleague's
 * change to anything else in the document survives it, and so does a ceiling
 * on a window this write did not touch.
 *
 * A SEAT's live inside that seat, which may sit in a unit at any depth — and a
 * merge patch cannot address a list element (RFC 7396 replaces an array
 * whole), so a patch naming one seat would rewrite the roster. A seat's
 * ceilings therefore change by replacing THAT SEAT (`PUT
 * /config/roles/{handle}`): read it, change its `token_budget`, send it back.
 * The engine validates the whole company behind either, so neither can store
 * a ceiling that breaks it.
 */

import { BUDGET_WINDOWS } from "~/contract/config.ts";
import type { ConfigRefusal } from "~/protocol/configAnswer.ts";
import type { TokenBudget } from "~/protocol/types.ts";
import { PERIOD_ADJECTIVE, readCeiling } from "./budget.ts";
import { fmtCount } from "./format.ts";
import { configGuardedReason } from "./useWriteAccess.ts";

export type Period = (typeof BUDGET_WINDOWS)[number]["period"];

/** Whose ceilings a write changes. */
export type CeilingScope =
  | { readonly kind: "company" }
  | {
      readonly kind: "seat";
      /** The seat's handle — the id `PUT /config/roles/{handle}` addresses. */
      readonly handle: string;
      /** How the chart names it, for the words a reader and the history see. */
      readonly name: string;
    };

/** The windows a write changes: a number of tokens, or `null` for NO CEILING. */
export type CeilingChanges = Readonly<Partial<Record<Period, number | null>>>;

/** The periods in the engine's order, the order every write and sentence uses. */
const PERIODS: readonly Period[] = BUDGET_WINDOWS.map((w) => w.period);

/** "the company's", "Agent PM's" — how a sentence names the scope's ceiling. */
export function whoseCeiling(scope: CeilingScope): string {
  return scope.kind === "company" ? "the company's" : `${scope.name}'s`;
}

/**
 * The merge patch that changes the company's ceilings: only the windows named,
 * `null` removing one.
 */
export function companyPatch(changes: CeilingChanges): Record<string, unknown> {
  const budget: Record<string, number | null> = {};
  for (const period of PERIODS) {
    if (period in changes) budget[period] = changes[period] ?? null;
  }
  return { token_budget: budget };
}

/**
 * A seat as read, with its ceilings changed — the body its `PUT` takes back.
 *
 * EVERYTHING ELSE AS READ, masks included: the engine restores a mask it
 * served against the revision it served it from, so a redacted credential
 * sent back is the credential kept. And a seat left capping nothing carries
 * NO `token_budget` key at all rather than an empty one, which is how the
 * engine writes a seat nobody capped.
 */
export function seatWithCeilings(
  seat: Readonly<Record<string, unknown>>,
  changes: CeilingChanges,
): Record<string, unknown> {
  const held = seat.token_budget;
  const budget: Record<string, number> = {};
  if (held && typeof held === "object" && !Array.isArray(held)) {
    for (const [key, value] of Object.entries(held)) {
      if (typeof value === "number") budget[key] = value;
    }
  }
  for (const period of PERIODS) {
    if (!(period in changes)) continue;
    const next = changes[period];
    if (next === null || next === undefined) delete budget[period];
    else budget[period] = next;
  }
  const out: Record<string, unknown> = { ...seat };
  if (Object.keys(budget).length === 0) delete out.token_budget;
  else out.token_budget = budget;
  return out;
}

/**
 * The changes a form's typed values make against what a scope holds now: only
 * the windows whose value differs. `undefined` where a box cannot be read —
 * the form refuses those before anything is sent.
 */
export function changesFrom(
  typed: Readonly<Partial<Record<Period, string>>>,
  held: TokenBudget,
): CeilingChanges | undefined {
  const out: Partial<Record<Period, number | null>> = {};
  for (const period of PERIODS) {
    const text = typed[period];
    if (text === undefined) continue;
    const read = readCeiling(period, text);
    if (!read.ok) return undefined;
    if (read.value !== (held[period] ?? null)) out[period] = read.value;
  }
  return out;
}

/**
 * A ceiling as a field shows it: the shortest spelling that reads back as
 * EXACTLY this number — `40M`, `2.5M`, `750k`, or the digits where no suffix
 * is exact — because a prefilled field that rounded would save a different
 * ceiling the moment somebody pressed Save without touching it.
 */
export function ceilingText(value: number | undefined, period: Period = "day"): string {
  if (value === undefined) return "";
  for (const [suffix, scale] of [
    ["B", 1e9],
    ["M", 1e6],
    ["k", 1e3],
  ] as const) {
    // Two places at most, and under a thousand of the unit: `45678.901k`
    // reads back exactly and is no easier to read than the digits.
    const scaled = value / scale;
    if (value < scale || (suffix !== "B" && scaled >= 1000)) continue;
    if (Math.abs(scaled * 100 - Math.round(scaled * 100)) > 1e-6) continue;
    const text = `${Number(scaled.toFixed(2))}${suffix}`;
    const back = readCeiling(period, text);
    if (back.ok && back.value === value) return text;
  }
  return String(value);
}

/**
 * The revision summary a ceiling change is recorded under — the line an
 * operator reads in the history to find the change that stopped a seat, so it
 * says whose, which window, from what and to what.
 */
export function ceilingSummary(
  scope: CeilingScope,
  changes: CeilingChanges,
  held: TokenBudget,
): string {
  const whose = whoseCeiling(scope);
  const parts: string[] = [];
  for (const period of PERIODS) {
    if (!(period in changes)) continue;
    const next = changes[period] ?? null;
    const before = held[period];
    const window = `${PERIOD_ADJECTIVE[period]} token ceiling`;
    if (next === null) {
      parts.push(`remove ${whose} ${window} (was ${fmtCount(before)})`);
    } else if (before === undefined) {
      parts.push(`set ${whose} ${window} to ${fmtCount(next)}`);
    } else {
      parts.push(
        `${next > before ? "raise" : "lower"} ${whose} ${window} from ${fmtCount(before)} to ${fmtCount(next)}`,
      );
    }
  }
  const text = parts.join("; ");
  return text.charAt(0).toUpperCase() + text.slice(1);
}

/**
 * A refused write in one sentence a person can act on, and whether a fresh
 * read is the remedy.
 */
export function refusalWords(
  refusal: ConfigRefusal | { readonly kind: "missing" } | { readonly kind: "none" },
  scope: CeilingScope,
): { message: string; reload: boolean } {
  switch (refusal.kind) {
    case "guarded":
      return { message: configGuardedReason(refusal.code), reload: false };
    case "conflict":
      return {
        message:
          "The company's configuration changed since this was read, so nothing was saved. Reload to see the ceilings as they are now.",
        reload: true,
      };
    case "draining":
      return {
        message: `This node is draining and stored nothing${refusal.detail ? `: ${refusal.detail}` : "."}`,
        reload: false,
      };
    case "unreachable":
      return {
        message:
          "The engine did not answer, so the change may or may not have landed. Reload before trying again.",
        reload: true,
      };
    case "missing":
      return {
        message:
          scope.kind === "seat"
            ? `${scope.name} is no longer in the company's configuration.`
            : "No company is configured yet.",
        reload: true,
      };
    case "none":
      return { message: "No company is configured yet.", reload: false };
    case "problems":
      return {
        message: refusal.problems.map((p) => p.message).join(" ") || "The engine refused it.",
        reload: false,
      };
  }
}
