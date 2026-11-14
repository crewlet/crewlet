/**
 * Changing one of the engine's caps, as values: what a person types for a
 * cap, the merge patch that writes it, and what the revision is recorded as.
 *
 * PURE, so every rule the Limits page's Save turns on is pinned without a
 * network: `lib/useSettingWrite.ts` is the half that sends it.
 *
 * # The engine decides what a cap may be
 *
 * A typed value is read here only as far as its SHAPE: a whole number for a
 * count or a rate, a number of seconds, a percentage for a share. Whether
 * that number is one the company can run (a ceiling above its own rounds, a
 * task's time limit under its call's) is the engine's rule, checked whole
 * against the document by the dry run every save makes first, and said in
 * the engine's own words. A second copy of those bounds here would drift.
 *
 * # The default is the key's absence
 *
 * A cap the document does not write is the engine's default, so going back to
 * the default REMOVES the key (a merge patch's `null`) rather than writing
 * the default's number: a document that writes today's default keeps it when
 * a later build changes the default, which is not what "use the default"
 * asks for.
 */

import type { LimitsAnswer } from "~/contract/limits.ts";
import type { CompanyDocument } from "~/protocol/types.ts";
import type { SettingChange, SettingPlan } from "./useSettingWrite.ts";

export type LimitRow = LimitsAnswer["limits"][number];

/** A cap as typed: a value to write, the default (the key removed), or why it is neither. */
export type TypedLimit =
  | { readonly ok: true; readonly value: number | null }
  | { readonly ok: false; readonly error: string };

/** What the field of a cap of this unit takes, as the line under it. */
export function limitTyping(row: LimitRow): string {
  switch (row.unit) {
    case "seconds":
      return "A number of seconds.";
    case "fraction":
      return "A percentage, from 1 to 100.";
    case "per_second":
      return "A whole number per second.";
    case "count":
      return "A whole number.";
  }
}

/** A cap's value as its field shows it: the number a person would type. */
export function limitText(row: LimitRow, value: number): string {
  // A SHARE IS TYPED AS A PERCENTAGE, rounded to what a float keeps of it:
  // 0.35 times 100 is 35.00000000000001.
  if (row.unit === "fraction") return String(Math.round(value * 10_000) / 100);
  return String(value);
}

/** Reads what was typed for a cap. Empty is the default. */
export function readLimit(row: LimitRow, text: string): TypedLimit {
  const typed = text.trim().replace(/%$/, "").trim();
  if (typed === "") return { ok: true, value: null };
  if (!/^\d+(\.\d+)?$/.test(typed)) {
    return { ok: false, error: `${limitTyping(row)} Leave it empty for the default.` };
  }
  const n = Number(typed);
  if (!Number.isFinite(n)) return { ok: false, error: limitTyping(row) };
  switch (row.unit) {
    case "fraction":
      return { ok: true, value: Math.round(n * 100) / 10_000 };
    case "seconds":
      return { ok: true, value: n };
    case "count":
    case "per_second":
      if (!Number.isInteger(n)) return { ok: false, error: limitTyping(row) };
      return { ok: true, value: n };
  }
}

/** A cap's key as the document's path: its dotted segments. */
function segmentsOf(key: string): string[] {
  return key.split(".");
}

/** The value the document itself writes at `key`, `undefined` where it writes none. */
export function documentLimit(document: CompanyDocument, key: string): unknown {
  let at: unknown = document;
  for (const segment of segmentsOf(key)) {
    if (typeof at !== "object" || at === null || Array.isArray(at)) return undefined;
    at = (at as Record<string, unknown>)[segment];
  }
  return at ?? undefined;
}

/** The merge patch that writes `value` at `key`, or removes the key for `null`. */
export function limitPatch(key: string, value: number | null): Record<string, unknown> {
  const segments = segmentsOf(key);
  let patch: unknown = value;
  for (let i = segments.length - 1; i >= 0; i--) patch = { [segments[i]!]: patch };
  return patch as Record<string, unknown>;
}

/**
 * One cap's change against the document as it is now: nothing where the
 * document already holds what was typed, else the patch and the summary the
 * revision history shows for it.
 */
export function limitPlan(row: LimitRow, label: string, value: number | null): SettingPlan {
  return (document): SettingChange | null => {
    const held = documentLimit(document, row.key);
    if (value === null ? held === undefined : held === value) return null;
    return {
      patch: limitPatch(row.key, value),
      summary:
        value === null
          ? `Limits: ${label} back to the default (${row.key})`
          : `Limits: ${label} set to ${limitText(row, value)}${row.unit === "fraction" ? "%" : ""} (${row.key})`,
    };
  };
}
