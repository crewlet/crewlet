/**
 * The Advisor's routes, as the page reads and moves them. Reads are REST
 * (`lib/useRest.ts`); every press is one request whose answer the page draws
 * at once, and the list is read again after it.
 */

import { pathSegment, rest } from "~/protocol/rest.ts";
import { useRest, type RestResult } from "~/lib/useRest.ts";
import type {
  AdvisorDecision,
  AdvisorDismissReason,
  AdvisorFinding,
  AdvisorFindingDetail,
  AdvisorFindingList,
  AdvisorReport,
  AdvisorReportSummary,
  AdvisorRunSummary,
} from "./types.ts";

/**
 * How often the open findings are read at rest. The duty re-derives each
 * class on its own cadence, the fastest every 15 minutes, so a minute shows a
 * finding the run raised or closed while the page was open without asking for
 * what has not moved.
 */
export const FINDINGS_POLL_MS = 60_000;

/** How often a run somebody started is read while it runs: it takes seconds. */
export const RUN_POLL_MS = 5_000;

/** The largest page of findings the engine answers. */
export const FINDINGS_PAGE = 100;

/** The findings in `state`, the engine's ranking and counts with them. */
export function useFindings(state: "open" | "closed", cursor: string | null = null) {
  const query = new URLSearchParams({ state, limit: String(FINDINGS_PAGE) });
  if (cursor) query.set("cursor", cursor);
  const path = `/advisor/findings?${query}`;
  return useRest<AdvisorFindingList>(
    path,
    async (signal) => (await rest.request("GET", path, { signal })).body as AdvisorFindingList,
    state === "open" ? { pollMs: FINDINGS_POLL_MS, refetchOnFocus: true } : {},
  );
}

/** One finding, its history and its earlier episodes. */
export function useFinding(id: string): RestResult<AdvisorFindingDetail> {
  const path = `/advisor/findings/${pathSegment(id)}`;
  return useRest<AdvisorFindingDetail>(
    path,
    async (signal) => (await rest.request("GET", path, { signal })).body as AdvisorFindingDetail,
    { pollMs: FINDINGS_POLL_MS },
  );
}

/** The weekly reports, newest first. */
export function useReports(): RestResult<{
  reports: AdvisorReportSummary[];
  next_cursor: string | null;
}> {
  return useRest(
    "/advisor/reports",
    async (signal) =>
      (await rest.request("GET", "/advisor/reports", { signal })).body as {
        reports: AdvisorReportSummary[];
        next_cursor: string | null;
      },
  );
}

/** One weekly report. */
export function useReport(id: string): RestResult<AdvisorReport> {
  const path = `/advisor/reports/${pathSegment(id)}`;
  return useRest<AdvisorReport>(
    path,
    async (signal) => (await rest.request("GET", path, { signal })).body as AdvisorReport,
  );
}

/** The last run, read every few seconds while `watching` (a run this page started). */
export function useLatestRun(watching: boolean): RestResult<AdvisorRunSummary | null> {
  return useRest<AdvisorRunSummary | null>(
    "/advisor/runs/latest",
    async (signal) =>
      ((await rest.request("GET", "/advisor/runs/latest", { signal })).body ??
        null) as AdvisorRunSummary | null,
    watching ? { pollMs: RUN_POLL_MS } : {},
  );
}

/** Run every check and the model pass now; a run already going is joined, not started twice. */
export async function runNow(): Promise<void> {
  try {
    await rest.request("POST", "/advisor/runs", { body: {} });
  } catch (err) {
    // A RUN ALREADY GOING is the one this press asked for.
    if (err instanceof Error && "status" in err && (err as { status: number }).status === 409)
      return;
    throw err;
  }
}

/** A person's answer to a finding: snooze it, dismiss it with a reason, reopen it, or mark it useful. */
export async function decide(
  id: string,
  decision: AdvisorDecision,
  reason?: AdvisorDismissReason,
): Promise<AdvisorFinding> {
  const answer = await rest.request("POST", `/advisor/findings/${pathSegment(id)}/decision`, {
    body: reason ? { decision, reason } : { decision },
  });
  return answer.body as AdvisorFinding;
}
