/**
 * The Advisor page's words for the engine's values: severities, where a
 * finding stands and why it closed, and a fact's number in its unit. One
 * table each, so a finding reads the same in the list, its page, the Apply
 * dialog and a report.
 *
 * A CLASS IS NEVER NAMED HERE. The engine writes every finding's title, why
 * and fix sentence, so a class it adds is drawn the day it ships, with no
 * list in the client to fall behind it.
 */

import type { AttentionTone } from "@crewlethq/ui";
import { fmtBytes, fmtCount, plural } from "~/lib/format.ts";
import { dateTimeFormat } from "~/lib/formatters.ts";
import type {
  AdvisorCloseReason,
  AdvisorFact,
  AdvisorFactUnit,
  AdvisorKeptFinding,
} from "~/protocol/advisorFindings.ts";
import type { AdvisorSeverity } from "~/protocol/advisor.ts";

/** Each severity, most costly first, as the list heads it and the tone it is drawn in. */
export const SEVERITIES: readonly {
  severity: AdvisorSeverity;
  label: string;
  tone: AttentionTone;
}[] = [
  { severity: "blocks", label: "Blocks", tone: "danger" },
  { severity: "costs", label: "Costs", tone: "warning" },
  { severity: "note", label: "Worth knowing", tone: "info" },
];

/** A severity's tone. */
export function toneOf(severity: AdvisorSeverity): AttentionTone {
  return SEVERITIES.find((s) => s.severity === severity)?.tone ?? "info";
}

/** A severity's word. */
export function severityWord(severity: AdvisorSeverity): string {
  return SEVERITIES.find((s) => s.severity === severity)?.label ?? severity;
}

/** Why a finding closed, as its row and its page say it. */
export const CLOSE_REASONS: Readonly<Record<AdvisorCloseReason, string>> = {
  condition_cleared: "The condition cleared",
  fixed_by_this: "Fixed by the change applied here",
  fixed_elsewhere: "Fixed by another change",
  subject_gone: "What it was about is gone",
  fix_unbuildable: "Its change can no longer be made",
  not_confirmed: "Expired: not seen again for 7 days",
  dismissed_wont_fix: "Dismissed: won't fix",
  dismissed_wrong: "Dismissed as wrong",
  dismissed_known: "Dismissed: already known",
};

/** Where a finding stands, in one word. */
export const STATE_WORDS: Readonly<Record<AdvisorKeptFinding["state"], string>> = {
  open: "Open",
  applied: "Applied",
  resolved: "Resolved",
  retired: "No longer applies",
  expired: "Expired",
  dismissed: "Dismissed",
};

/** A number in its unit, as a reader compares two of them. */
export function factValue(value: number | string | null, unit: AdvisorFactUnit): string {
  if (value === null) return "none";
  if (typeof value === "string") return value;
  switch (unit) {
    case "hours":
      return `${round(value)} h`;
    case "minutes":
      return `${round(value)} min`;
    case "percent":
      return `${round(value)}%`;
    case "tokens":
      return `${fmtCount(value)} tokens`;
    case "items":
      return plural(value, "item");
    case "turns":
      return plural(value, "turn");
    case "bytes":
      return fmtBytes(value);
    // A COUNT IS THE NUMBER ALONE: its label says what is counted.
    case "count":
      return fmtCount(value);
    default:
      // A UNIT THIS BUILD DOES NOT KNOW is the number with its word.
      return `${fmtCount(value)} ${unit}`;
  }
}

/** One decimal at most, and none that is zero. */
const round = (n: number) => String(Math.round(n * 10) / 10);

/** A fact's number. */
export function factWords(fact: AdvisorFact): string {
  return factValue(fact.value, fact.unit);
}

/** What a finding is measured to cost a week, where its class can say: the row's quiet figure. */
export function impactWords(finding: AdvisorKeptFinding): string | null {
  const parts: string[] = [];
  const hours = finding.impact?.hours_per_week;
  const tokens = finding.impact?.tokens_per_week;
  if (hours !== null && hours !== undefined) parts.push(`${round(hours)} h a week`);
  if (tokens !== null && tokens !== undefined) parts.push(`${fmtCount(tokens)} tokens a week`);
  return parts.length === 0 ? null : parts.join(" and ");
}

/** The open findings ranked: the most costly severity first, then the largest measured cost, then the newest. */
export function ranked(findings: readonly AdvisorKeptFinding[]): AdvisorKeptFinding[] {
  const order = (s: AdvisorSeverity) => SEVERITIES.findIndex((x) => x.severity === s);
  const weight = (f: AdvisorKeptFinding) =>
    (f.impact?.hours_per_week ?? 0) * 1_000_000 + (f.impact?.tokens_per_week ?? 0);
  return [...findings].sort(
    (a, b) =>
      order(a.severity) - order(b.severity) ||
      weight(b) - weight(a) ||
      Date.parse(b.opened_at) - Date.parse(a.opened_at),
  );
}

/**
 * Who a report's finding is about, as the report names it: a seat's or a
 * unit's name as it is, and a subject that is the only one of its kind (the
 * company, the atlas), which the engine names by its kind, in words.
 */
export function reportSubject(subject: string): string {
  return SINGLETON_SUBJECTS[subject] ?? subject;
}

const SINGLETON_SUBJECTS: Readonly<Record<string, string>> = {
  company: "The company",
  atlas: "The atlas",
};

/** A report's week in words: `2026-W47` is "Week 47 of 2026"; anything else as it is. */
export function weekWords(week: string): string {
  const match = /^(\d{4})-W(\d{2})$/.exec(week);
  return match ? `Week ${Number(match[2])} of ${match[1]}` : week;
}

/**
 * A report's week as the days it covers, "Monday, Nov 9 to Sunday, Nov 15,
 * 2026", or null for a week not in ISO form. An ISO week names its calendar
 * days outright, Monday to Sunday on the company's clock, so they are worked
 * out from the week and never from the instants that bound it, which a
 * reader in another zone would read as the Sunday before.
 */
export function weekSpan(week: string): string | null {
  const match = /^(\d{4})-W(\d{2})$/.exec(week);
  if (!match) return null;
  // WEEK 1 is the one holding January 4th, and every ISO week starts on a Monday.
  const jan4 = Date.UTC(Number(match[1]), 0, 4);
  const fromMonday = (new Date(jan4).getUTCDay() + 6) % 7;
  const monday = jan4 - fromMonday * DAY_MS + (Number(match[2]) - 1) * 7 * DAY_MS;
  const day = dateTimeFormat(undefined, "UTC", { weekday: "long", month: "short", day: "numeric" });
  const last = dateTimeFormat(undefined, "UTC", {
    weekday: "long",
    month: "short",
    day: "numeric",
    year: "numeric",
  });
  return `${day.format(monday)} to ${last.format(monday + 6 * DAY_MS)}`;
}

const DAY_MS = 86_400_000;

/** What a week moved, in one line: "3 opened · 1 resolved · 0 applied · 0 dismissed". */
export function weekMoved(r: {
  opened: number;
  resolved: number;
  applied: number;
  dismissed: number;
}): string {
  return [
    `${fmtCount(r.opened)} opened`,
    `${fmtCount(r.resolved)} resolved`,
    `${fmtCount(r.applied)} applied`,
    `${fmtCount(r.dismissed)} dismissed`,
  ].join(" · ");
}
