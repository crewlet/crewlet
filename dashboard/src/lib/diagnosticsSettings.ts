/**
 * Settings › Diagnostics' two choices as changes to the company document:
 * whether the engine reports its own bugs (`diagnostics.issue_reports`) and
 * what it shares of its usage insights (`diagnostics.insights`).
 *
 * PLANS FOR THE ONE SETTING WRITE (`lib/useSettingWrite.ts`), worked out
 * against the document as it is at the moment of the save, so each switch is
 * checked by the engine first, stops on a warning it introduces, and is
 * watched until this node applies it, as every other setting is.
 */

import type { CompanyDocument } from "~/protocol/types.ts";
import type { SettingPlan } from "./useSettingWrite.ts";

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** One block under `diagnostics`, or an empty one where the document has none. */
function diagnosticsBlock(document: CompanyDocument, key: string): Record<string, unknown> {
  const diagnostics = (document as Record<string, unknown>).diagnostics;
  if (!isRecord(diagnostics)) return {};
  const block = diagnostics[key];
  return isRecord(block) ? block : {};
}

/** Bug reports to Crewlet turned on or off; nothing where the document already says so. */
export function issueReportsPlan(on: boolean): SettingPlan {
  return (document) => {
    if ((diagnosticsBlock(document, "issue_reports").enabled === true) === on) return null;
    return {
      patch: { diagnostics: { issue_reports: { enabled: on } } },
      summary: on ? "Turned on bug reports to Crewlet" : "Turned off bug reports to Crewlet",
    };
  };
}

/**
 * What a usage insights choice sets: how records are shared, the list
 * accepted, and the patterns always sent without review.
 */
export interface InsightsChange {
  readonly share?: "off" | "review" | "auto";
  readonly accepted?: number;
  readonly always?: readonly string[];
}

/** Two pattern lists as sets: the order a list was written in decides nothing. */
function samePatterns(a: unknown, b: readonly string[]): boolean {
  const held = Array.isArray(a) ? a.filter((v): v is string => typeof v === "string") : [];
  return held.length === b.length && b.every((v) => held.includes(v));
}

/**
 * A usage insights choice, as `diagnostics.insights` keys; nothing where the
 * document already holds every key it sets. An unset share is off, as the
 * engine reads it.
 */
export function insightsPlan(change: InsightsChange, summary: string): SettingPlan {
  return (document) => {
    const held = diagnosticsBlock(document, "insights");
    const share = typeof held.share === "string" && held.share !== "" ? held.share : "off";
    const same =
      (change.share === undefined || change.share === share) &&
      (change.accepted === undefined || change.accepted === held.accepted) &&
      (change.always === undefined || samePatterns(held.always, change.always));
    if (same) return null;
    return { patch: { diagnostics: { insights: { ...change } } }, summary };
  };
}
