/**
 * `GET /diagnostics/insights`: the `diagnostics_insights` question, Settings ›
 * Diagnostics (ADR-0119). The engine's own shape is `queries.InsightsAnswer`,
 * and its lists are always lists.
 */

/** The consent: nothing shared, each record reviewed first, or every one sent. */
export type InsightsShare = "" | "off" | "review" | "auto";

/** Where one class's week stands in the fleet's record. */
export type InsightStatus =
  "observed" | "queued" | "scheduled" | "sending" | "sent" | "failed" | "declined";

/** One class's week, with its record as sent or as it would be. */
export interface InsightRow {
  class: string;
  week: string;
  status: InsightStatus;
  send_at?: string;
  attempts?: number;
  /** The relay's last status code, absent where none came. */
  answer?: number;
  record?: unknown;
}

/** One fact a class's record carries, as buckets. */
export interface InsightFact {
  key: string;
  values: string[];
}

/** One class of the catalogue: what it says, and the facts its record carries. */
export interface InsightClass {
  class: string;
  title: string;
  summary: string;
  facts: InsightFact[];
}

/** The catalogue this build carries. */
export interface InsightCatalogue {
  schema: number;
  classes: InsightClass[];
}

export interface InsightsAnswer {
  share: InsightsShare;
  /** The catalogue schema an administrator accepted; 0 for none. */
  accepted: number;
  /** The catalogue schema this build carries. */
  schema: number;
  /** The classes sent without review in review mode. */
  always: string[];
  relay: string;
  catalogue: InsightCatalogue;
  records: InsightRow[];
}
