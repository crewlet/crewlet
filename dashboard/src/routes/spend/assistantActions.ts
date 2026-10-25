/**
 * What the Spend screens take from the assistant: Spend's window and the
 * split its chart is drawn by, written to the address as its own controls
 * write them, and on Budgets a ceiling prepared for its editor to open with.
 *
 * PURE VIEW CHANGES, so neither needs approval (D11): each narrows what the
 * reader can already read, applies at once, and its receipt offers Undo. The
 * window is held to the screen's own offer (`spendOffer`), so the assistant
 * cannot ask for a window the page would quietly replace with its fallback.
 */

import type { PageActionParse } from "~/app/assistant/context.ts";
import { choiceKey, windowKey, type UrlActionKey } from "~/app/assistant/urlAction.ts";
import { BUDGET_WINDOWS } from "~/contract/config.ts";
import { GROUPS } from "~/contract/spend.ts";
import { readCeiling } from "~/lib/budget.ts";
import type { Period } from "~/lib/ceilings.ts";
import type { Offer } from "~/lib/range.ts";

/** The key the window action writes. */
export function spendRangeKeys(offer: Offer, now: () => number): Record<string, UrlActionKey> {
  return { window: windowKey(offer, now) };
}

/** The key the split action writes: a section, `phase` its fallback, as the screen reads it. */
export function spendGroupKeys(): Record<string, UrlActionKey> {
  return { group: choiceKey("Split by", GROUPS, "section", "phase") };
}

/** A ceiling the assistant prepared for the Budgets screen's editor to open with. */
export interface CeilingProposal {
  /** `company`, or a seat's handle. */
  readonly scope: string;
  readonly period: Period;
  /** As the field takes it. */
  readonly text: string;
  /** What it reads as: a number of tokens, or null for no ceiling. */
  readonly value: number | null;
}

/**
 * Parse a prepared ceiling (`budgets.ceiling.prefill`): the company's, or a
 * seat's the screen draws, one of the three windows, and a value the field
 * itself would take ([readCeiling]), refused in the field's own words.
 */
export function parseCeilingPrefill(
  raw: unknown,
  seats: ReadonlySet<string>,
): PageActionParse<CeilingProposal> {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ok: false, reason: "That is not a ceiling." };
  }
  const { seat, period, ceiling, ...rest } = raw as Record<string, unknown>;
  if (Object.keys(rest).length > 0) {
    return { ok: false, reason: "A ceiling is a seat or the company, a window and a value." };
  }
  if (seat !== undefined && (typeof seat !== "string" || !seats.has(seat))) {
    return { ok: false, reason: `This screen shows no seat "${String(seat)}".` };
  }
  const window = BUDGET_WINDOWS.find((w) => w.period === period);
  if (!window) return { ok: false, reason: "A ceiling is daily, weekly or monthly." };
  if (ceiling !== null && typeof ceiling !== "string" && typeof ceiling !== "number") {
    return { ok: false, reason: "A ceiling is a number of tokens, or none." };
  }
  const text = ceiling === null ? "" : String(ceiling);
  const read = readCeiling(window.period, text);
  if (!read.ok) return { ok: false, reason: read.error };
  return {
    ok: true,
    value: {
      scope: typeof seat === "string" ? seat : "company",
      period: window.period,
      text,
      value: read.value,
    },
  };
}
