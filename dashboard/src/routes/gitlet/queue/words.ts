/** Gitlet › Queue in words: a gate's required checks, a train's checks and time, an ejection, the summary. */

import { checkStateWord, ejectReasonWord, latestAttempts, longestTimeout } from "~/lib/checks.ts";
import type { MeterPart } from "~/lib/checks.ts";
import { fmtCount, listed, plural } from "~/lib/format.ts";
import type { CodeCheckRow, CodeEjectedRow, CodeRuleGate } from "~/protocol/index.ts";

/**
 * The checks a branch's gate requires: every gate of the rules the engine matched to it, each
 * check once and sorted, as `gitlet.GateRulesOf` composes them. Null where either read is missing.
 */
export function gateChecks(
  patterns: readonly string[] | null,
  gates: readonly CodeRuleGate[] | null,
): string[] | null {
  if (patterns === null || gates === null) return null;
  const names = new Set<string>();
  for (const g of gates) {
    if (patterns.includes(g.pattern)) for (const name of g.gate.checks ?? []) names.add(name);
  }
  return [...names].sort();
}

/** Whether any gate of the rules the engine matched asks an approval. */
export function gateAsksApproval(
  patterns: readonly string[] | null,
  gates: readonly CodeRuleGate[] | null,
): boolean {
  if (patterns === null || gates === null) return true;
  return gates.some((g) => patterns.includes(g.pattern) && g.gate.approvals !== "none");
}

/** The required rows of a tip at their latest attempts, by name. */
function requiredRows(rows: readonly CodeCheckRow[]): Map<string, CodeCheckRow> {
  return new Map(latestAttempts(rows.filter((r) => r.required)).map((r) => [r.name, r]));
}

/** The names a train's checks are said by: the gate's, then a required row it does not name. */
function namesOf(gate: readonly string[] | null, rows: readonly CodeCheckRow[]): string[] {
  const required = [...requiredRows(rows).keys()];
  if (gate === null) return required;
  return [...gate, ...required.filter((name) => !gate.includes(name))];
}

/** A train's required checks as a meter: passed, failed and running of every check the gate names. */
export function trainMeter(
  gate: readonly string[] | null,
  rows: readonly CodeCheckRow[],
): { parts: MeterPart[]; total: number } {
  const by = requiredRows(rows);
  const count = (state: string) => [...by.values()].filter((r) => r.state === state).length;
  return {
    parts: [
      { id: "passed", label: "passed", value: count("passed"), tone: "success" },
      { id: "failed", label: "failed", value: count("failed"), tone: "danger" },
      { id: "running", label: "running", value: count("running"), tone: "info" },
    ],
    total: namesOf(gate, rows).length,
  };
}

/** "unit passed · e2e running"; every one unstarted is one clause, "unit and e2e not started". */
export function checksWords(gate: readonly string[] | null, rows: readonly CodeCheckRow[]): string {
  const by = requiredRows(rows);
  const names = namesOf(gate, rows);
  if (names.length === 0)
    return gate === null ? "Its checks have not started" : "No required checks";
  if (names.every((name) => !by.has(name))) return `${listed(names)} not started`;
  return names
    .map((name) => {
      const row = by.get(name);
      return `${name} ${row ? checkStateWord(row.state) : "not started"}`;
    })
    .join(" · ");
}

const MINUTE = 60_000;

/** A duration in words: "4 minutes", "1 hour 5 minutes", "2 hours". */
function durationWords(ms: number): string {
  const minutes = Math.floor(ms / MINUTE);
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  if (hours === 0) return plural(minutes, "minute");
  return rest === 0 ? plural(hours, "hour") : `${plural(hours, "hour")} ${plural(rest, "minute")}`;
}

/** A declared timeout in words: whole minutes rounded up, or seconds under a minute. */
function timeoutWords(ms: number): string {
  return ms < MINUTE
    ? plural(Math.ceil(ms / 1000), "second")
    : durationWords(Math.ceil(ms / MINUTE) * MINUTE);
}

/** How long a train's checks have run, against the slowest declared timeout. */
export interface Elapsed {
  words: string;
  /** The bar's reading in minutes, where a timeout is declared. */
  bar?: { value: number; max: number };
}

/**
 * When a train's required checks ended: the latest finish once every one the gate names has a row
 * and none is queued or running. Null while they run, so the time keeps counting.
 */
export function trainEnd(
  gate: readonly string[] | null,
  rows: readonly CodeCheckRow[],
): number | null {
  const by = requiredRows(rows);
  if (by.size === 0 || (gate ?? []).some((name) => !by.has(name))) return null;
  const settled = [...by.values()];
  if (settled.some((r) => r.state === "queued" || r.state === "running")) return null;
  const ends = settled.map((r) => Date.parse(r.finished_at ?? "")).filter((t) => !Number.isNaN(t));
  return ends.length > 0 ? Math.max(...ends) : null;
}

/**
 * The time from the first required check's start against the slowest timeout: "4 of 30 minutes",
 * "ran 10 of 30 minutes" once they ended, "over 30 minutes" past the timeout.
 */
export function elapsedWords(
  gate: readonly string[] | null,
  rows: readonly CodeCheckRow[],
  now: number,
): Elapsed {
  const starts = [...requiredRows(rows).values()]
    .map((r) => Date.parse(r.started_at ?? ""))
    .filter((t) => !Number.isNaN(t));
  if (starts.length === 0) return { words: "not started yet" };
  const end = trainEnd(gate, rows);
  const ran = Math.max(0, (end ?? now) - Math.min(...starts));
  const said = (words: string) => (end === null ? words : `ran ${words}`);
  const timeout = longestTimeout(rows) * 1000;
  if (timeout === 0) {
    if (ran < MINUTE)
      return { words: end === null ? "running under a minute" : "ran under a minute" };
    return { words: end === null ? `running ${durationWords(ran)}` : `ran ${durationWords(ran)}` };
  }
  const bar = { value: Math.min(ran, timeout) / MINUTE, max: timeout / MINUTE };
  const of = timeoutWords(timeout);
  if (ran > timeout) return { words: said(`over ${of}`), bar };
  if (ran < MINUTE) return { words: said(`under a minute of ${of}`), bar };
  if (timeout >= MINUTE && Math.ceil(timeout / MINUTE) < 60) {
    return { words: said(`${Math.floor(ran / MINUTE)} of ${of}`), bar };
  }
  return { words: said(`${durationWords(ran)} of ${of}`), bar };
}

/** Names past three counted: "e2e, lint, unit and 2 more". */
function fewNames(names: readonly string[], noun?: string): string {
  if (names.length <= 3) return listed(names);
  const more = names.length - 3;
  return `${names.slice(0, 3).join(", ")} and ${noun ? plural(more, `more ${noun}`) : `${more} more`}`;
}

/** Why a queue took a change out, after "taken out of the queue {ago}:"; `onto` is the train's base id. */
export function ejectionWords(e: CodeEjectedRow): { why: string; onto?: string } {
  const target = e.target_ref.replace(/^refs\/heads\//, "");
  if (e.reason === "check_failed" && (e.checks?.length ?? 0) > 0) {
    const failed = `${fewNames(e.checks!)} failed`;
    return e.tested_in
      ? { why: `${failed} on the train's merge onto ${target} at`, onto: e.tested_in.base_oid }
      : { why: `${failed} on the train it was tested in` };
  }
  if (e.reason === "conflict" && (e.paths?.length ?? 0) > 0) {
    const [first, ...rest] = e.paths!;
    const more = rest.length > 0 ? ` and ${plural(rest.length, "more path")}` : "";
    return { why: `its merge conflicted on ${first}${more}` };
  }
  return { why: ejectReasonWord(e.reason) };
}

/** One half of the summary: a strong figure, where there is one, then its words. */
export interface SummaryHalf {
  figure?: string;
  words: string;
}

/** "2 changes queued · 1 taken out in the last day"; a half not answered is not drawn. */
export function summaryWords(
  queued: { live: number; more?: boolean } | null,
  out: { total: number; more?: boolean } | null,
  repo: string,
): SummaryHalf[] {
  const halves: SummaryHalf[] = [];
  const where = repo ? ` in ${repo}` : "";
  if (queued) {
    if (queued.live === 0 && !queued.more) halves.push({ words: `Nothing queued${where}` });
    else {
      const noun = queued.live === 1 && !queued.more ? "change" : "changes";
      halves.push({
        figure: `${queued.more ? "At least " : ""}${fmtCount(queued.live)}`,
        words: ` ${noun} queued${where}`,
      });
    }
  }
  if (out && (out.total > 0 || out.more)) {
    halves.push({
      figure: `${out.more ? "at least " : ""}${fmtCount(out.total)}`,
      words: " taken out in the last day",
    });
  }
  return halves;
}
