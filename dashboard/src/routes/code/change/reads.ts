/**
 * The parts of one change the page reads, each its own `code_change`
 * question, at the tab's floor on the code log.
 *
 * A PART IS READ A PAGE AT A TIME, its page the engine's largest (`code.
 * ReadQuery.Limit`: two hundred comments, reviews or findings): a change's
 * conversation past that is rare, and where a page carries a cursor the tab
 * that draws it SAYS that more exist rather than drawing a page as the whole.
 */

import { useMemo } from "react";
import { findingStands } from "~/lib/code.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { CodeChangeDetail, CodeFindingRow } from "~/protocol/index.ts";

/**
 * How often an open change is read again: a MINUTE, the workspace's cadence on
 * the code log, which has no push to the dashboard — a seat's review reaches
 * the open page by this read or by the reader's own press, which asks again
 * at once.
 */
export const CHANGE_POLL_MS = 60_000;
/** The most rows a part's page reads: the engine's own bound for the conversation's parts. */
export const PART_PAGE = 200;

/** One part of a change, as its tab reads it. */
export function useChangePart(
  change: CodeChangeDetail,
  part: "reviewers" | "comments" | "reviews" | "findings" | "files" | "diff",
  extra: Readonly<Record<string, string | number | undefined>> = {},
  enabled = true,
) {
  const listed = part !== "files" && part !== "diff";
  return useCodeQuery(
    "code_change",
    {
      name: change.repo,
      change: change.number,
      part,
      ...(listed ? { limit: PART_PAGE } : {}),
      ...extra,
    },
    { enabled, pollMs: CHANGE_POLL_MS },
  );
}

/**
 * The reader's own STANDING findings on the change — open, or answered by an
 * author — which a review of theirs may carry (`code.Review.Carries`): the
 * findings it would supersede by leaving them out. None for a reader bound
 * to no seat, who raised none.
 */
export function useStandingFindings(change: CodeChangeDetail, handle: string): CodeFindingRow[] {
  const read = useChangePart(change, "findings", {}, handle !== "");
  const findings = read.data?.findings;
  return useMemo(
    () => (findings ?? []).filter((f) => f.raiser === handle && findingStands(f.state)),
    [findings, handle],
  );
}

/** Whether one position on the code log is before another: by generation, then sequence. */
export function positionBefore(
  a: { generation: number; seq: number },
  b: { generation: number; seq: number },
): boolean {
  return a.generation === b.generation ? a.seq < b.seq : a.generation < b.generation;
}
