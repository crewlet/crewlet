/**
 * The Advisor's questions and presses, as its page reads and moves them. The
 * reads are the engine's questions (`advisor_findings`, `advisor_finding`,
 * `advisor_run`); every press is one request whose answer the page reads
 * again after.
 */

import { pathSegment, rest, RestError } from "~/protocol/rest.ts";
import type { AdvisorDecision, AdvisorKeptFinding } from "~/protocol/advisorFindings.ts";
import { useQuery } from "~/lib/useQuery.ts";

/**
 * How often the open findings are read at rest. The duty re-derives each
 * class on its own cadence, the fastest every 15 minutes, so a minute shows a
 * finding a run raised or closed while the page was open without asking for
 * what has not moved.
 */
export const FINDINGS_POLL_MS = 60_000;

/** How often a run somebody started is read while it runs: a run takes seconds. */
export const RUN_POLL_MS = 5_000;

/** The largest page of findings the engine answers. */
export const FINDINGS_PAGE = 100;

/** The findings in `state`, a page at `cursor`, with the open ones counted and the latest run. */
export function useFindings(state: "open" | "closed", cursor: string | null = null) {
  return useQuery(
    "advisor_findings",
    { state, limit: FINDINGS_PAGE, ...(cursor ? { cursor } : {}) },
    state === "open" ? { pollMs: FINDINGS_POLL_MS, refetchOnFocus: true } : {},
  );
}

/** One finding, its history and its earlier episodes. */
export function useFinding(id: string) {
  return useQuery("advisor_finding", { id }, { pollMs: FINDINGS_POLL_MS });
}

/** Weeks a page of the Reports tab asks for. */
export const REPORTS_PAGE = 26;

/** A page of the weekly reports, the newest first (`advisor_reports`). */
export function useReports(cursor: string | null = null) {
  return useQuery("advisor_reports", { limit: REPORTS_PAGE, ...(cursor ? { cursor } : {}) });
}

/** One week's report, by its ISO week (`advisor_report`). */
export function useReport(week: string) {
  return useQuery("advisor_report", { week });
}

/** The latest run, read every few seconds while `watching` (a run this page started). */
export function useLatestRun(watching: boolean) {
  return useQuery("advisor_run", undefined, watching ? { pollMs: RUN_POLL_MS } : {});
}

/** Run every check and the model pass now; a run already going is the one this asked for. */
export async function runNow(): Promise<void> {
  try {
    await rest.request("POST", "/advisor/runs", { body: {} });
  } catch (err) {
    if (err instanceof RestError && err.status === 409) return;
    throw err;
  }
}

/** A person's answer to a finding: snooze it, dismiss it with a reason, reopen it, or mark it useful. */
export async function decide(id: string, decision: AdvisorDecision): Promise<AdvisorKeptFinding> {
  const answer = await rest.request("POST", `/advisor/findings/${pathSegment(id)}/decision`, {
    body: decision,
  });
  return answer.body as AdvisorKeptFinding;
}
