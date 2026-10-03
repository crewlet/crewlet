/**
 * Changing a token ceiling, as values: which surface a scope's ceilings live
 * on, what the write that changes one carries, what it is recorded as, and
 * what the engine's answer to it means.
 *
 * PURE, so every rule a screen's Save turns on is pinned by a test without a
 * network or a clock: `lib/useCeilingWrite.ts` is the half that sends it.
 *
 * # Two scopes, two surfaces
 *
 * The COMPANY's ceilings are the settings' `token_budget`, a top-level key of
 * the company document, so they change by a merge patch of `/config` naming
 * only the windows that change (`{token_budget: {day: 50000000}}`, and `null`
 * to remove one): a colleague's change to anything else in the document
 * survives it, and so does a ceiling on a window this write did not touch.
 *
 * A SEAT's are in its RUNTIME half on the org chart, which left the company
 * document for a log of its own — `/config` refuses a body naming a seat, and
 * the write that used to replace the seat there (`PUT /config/roles/{handle}`)
 * no longer exists: the engine serves no `roles` collection, so that path is
 * a `404 no_route`. A seat's ceilings change by its chart content write
 * (`PATCH /chart/seats/{handle}`): read the seat with its runtime half
 * (`?runtime=true`), change the runtime's `token_budget`, and send the seat
 * back. A content write is the object's whole post-state — its prose as read
 * — with the runtime half stated because it changed ([seatCeilingBody]).
 * Each window is `{day, week, month}`, the shape the company's takes too.
 */

import { BUDGET_WINDOWS } from "~/contract/config.ts";
import type { ConfigRefusal } from "~/protocol/configAnswer.ts";
import type { RestError } from "~/protocol/rest.ts";
import type { ChartSeat, TokenBudget } from "~/protocol/types.ts";
import { needsSentence } from "./refusal.ts";
import { PERIOD_ADJECTIVE, readCeiling } from "./budget.ts";
import { fmtCount } from "./format.ts";

export type Period = (typeof BUDGET_WINDOWS)[number]["period"];

/** Whose ceilings a write changes. */
export type CeilingScope =
  | { readonly kind: "company" }
  | {
      readonly kind: "seat";
      /** The seat's handle — the address `PATCH /chart/seats/{handle}` writes. */
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
 * A seat's runtime half as read, with its ceilings changed.
 *
 * EVERYTHING ELSE AS READ, masks included: the chart restores a masked
 * credential from the row it patches, so a redacted value sent back is the
 * credential kept. And a runtime capping nothing carries NO `token_budget`
 * key at all rather than an empty one, which is how the chart holds a seat
 * nobody capped.
 */
export function runtimeWithCeilings(
  runtime: Readonly<Record<string, unknown>>,
  changes: CeilingChanges,
): Record<string, unknown> {
  const held = runtime.token_budget;
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
  const out: Record<string, unknown> = { ...runtime };
  if (Object.keys(budget).length === 0) delete out.token_budget;
  else out.token_budget = budget;
  return out;
}

/**
 * The body of the chart content write that changes a seat's ceilings: the
 * seat's content as read, and its runtime half with the ceilings changed.
 *
 * THE WHOLE CONTENT, because a content write is the object's post-state: a
 * field left out is a field cleared. Every one is restated as read — the
 * address masked as the chart served it, which it restores — so the write
 * changes the ceilings and nothing else. NO `kind` AND NO `manages`: both are
 * structure, which the content route refuses by name.
 *
 * THE RUNTIME STATED, never left out, since it is what changes; and a runtime
 * this change empties is CLEARED (`clear_runtime`) rather than sent as `{}`,
 * the one spelling the chart reads as "this seat has no runtime half".
 */
export function seatCeilingBody(seat: ChartSeat, changes: CeilingChanges): Record<string, unknown> {
  const content: Record<string, unknown> = {
    unit: seat.unit ?? "",
    name: seat.name ?? "",
    email: seat.email ?? "",
    backstory: seat.backstory ?? "",
    goal: seat.goal ?? "",
    responsibilities: seat.responsibilities ?? [],
    behavioral_guidelines: seat.behavioral_guidelines ?? [],
    project: seat.project ?? "",
    space: seat.space ?? "",
  };
  const runtime = runtimeWithCeilings(
    (seat.runtime ?? {}) as Readonly<Record<string, unknown>>,
    changes,
  );
  return Object.keys(runtime).length > 0
    ? { ...content, runtime }
    : { ...content, clear_runtime: true };
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
      // ABOUT THE GRANT, NOT A TOKEN: a ceiling is a change to the company's
      // configuration, and the credential presented does not carry the right
      // to make one.
      return {
        message:
          "The credential you presented does not carry config:write, so the ceiling was not changed.",
        reload: false,
      };
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
            ? `${scope.name} is no longer in the org chart.`
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

/**
 * A refused chart content write in one sentence a person can act on, and
 * whether a fresh read is the remedy — the seat scope's half of
 * [refusalWords].
 *
 * WHAT A CHART WRITE CAN ANSWER is not what `/config` can: there is no
 * revision to conflict with, and the object itself is the unit of contention
 * — a `409 stale` is a colleague's write to THIS seat landing first, settled
 * by reading it again. A `422` is the chart's own rule refusing the runtime
 * half (a ceiling it will not hold), in the rule's own words; a `403` names
 * the grant, or the fields a lead may not change; a `404` is a seat a removal
 * took. An outcome nobody can establish is not here: it is
 * `useCeilingWrite`'s `unknown`, resent under the same operation.
 */
export function chartRefusalWords(
  err: RestError,
  scope: CeilingScope,
): { message: string; reload: boolean } {
  const whose = whoseCeiling(scope);
  switch (err.status) {
    case 401:
    case 403:
      return {
        message: needsSentence(`Changing ${whose} ceilings`, err.grants),
        reload: false,
      };
    case 404:
      return {
        message:
          scope.kind === "seat"
            ? `${scope.name} is no longer in the org chart.`
            : "No company is configured yet.",
        reload: true,
      };
    case 409:
      return {
        message:
          "Somebody changed this seat at the same moment, so nothing was saved. Reload to see it as it is now.",
        reload: true,
      };
    default:
      return {
        message: err.detail || err.sentence || `The engine refused the change (${err.status}).`,
        reload: false,
      };
  }
}
