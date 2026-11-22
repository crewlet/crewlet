/**
 * The figure the Advisor's sidebar row carries: the open findings that block
 * or cost the company, which an administrator is to look at.
 *
 * THE ENGINE COUNTS (`advisor_findings`' `counts`): the owner's open findings,
 * neither snoozed nor applied and waiting on their effect, by severity, over
 * the whole list whatever the page. So ONE ROW IS READ, as the Queue's count
 * is (`useQueueCount.ts`), and Worth knowing is left out: a figure that grows
 * with every note would be one a reader learns to ignore.
 *
 * ASKED ONLY OF AN ADMINISTRATOR, whose page it is: for anybody else the row
 * is locked, and a figure beside a lock is a refusal drawn as a number.
 */

import { useQuery } from "./useQuery.ts";

/**
 * How often it is read. The Advisor's duty asks what is due once a minute, so
 * a finding it raises or closes reaches the row within two.
 */
export const ADVISOR_FIGURE_POLL_MS = 60_000;

/** Open findings that block or cost the company, or null until the engine answers (or where not asked). */
export function useAdvisorFigure(enabled: boolean): number | null {
  const read = useQuery("advisor_findings", enabled ? { state: "open", limit: 1 } : undefined, {
    enabled,
    pollMs: ADVISOR_FIGURE_POLL_MS,
  });
  const counts = read.data?.counts;
  return counts ? counts.blocks + counts.costs : null;
}
