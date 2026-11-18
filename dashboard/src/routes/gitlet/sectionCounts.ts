/**
 * Gitlet's section figures: the company's live changes, repositories and queued entries, asked once
 * a page view and published on the header's tabs. A page that counts the company too reads this
 * answer rather than asking again.
 */

import { useEffect } from "react";
import { useSectionCounts } from "~/app/Shell.tsx";
import { fmtCount } from "~/lib/format.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type { CodeCounts, CodeCountsAnswer } from "~/protocol/index.ts";
import { kindCount } from "./repoListing.ts";

/** The company's counts every Gitlet section page asks, once a page view. */
export const SECTION_COUNTS_PARAMS = { parts: "changes,repos,queue" } as const;

/** The last figures published, drawn by the next section page while its own question is out. */
let held: Record<string, string> = {};

/** The section tabs' figures from an answer: a key for each part answered, none for the rest. */
export function sectionFigures(counts: CodeCounts | undefined): Record<string, string> {
  const figures: Record<string, string> = {};
  const changes = counts?.changes?.live;
  if (typeof changes === "number") figures.changes = fmtCount(changes);
  const repos = kindCount(counts?.repos, "", "");
  if (repos !== undefined) figures.repos = fmtCount(repos);
  const queue = counts?.queue;
  if (typeof queue?.live === "number") {
    figures.queue = queue.more ? `${fmtCount(queue.live)}+` : fmtCount(queue.live);
  }
  return figures;
}

/** Ask Gitlet's section counts and publish them on the tabs; the answer, for a page that draws them too. */
export function useGitletSectionCounts(
  options: { pollMs?: number } = {},
): QueryResult<CodeCountsAnswer> {
  const answer = useCodeQuery("code_counts", SECTION_COUNTS_PARAMS, { pollMs: options.pollMs });
  const figures = answer.data
    ? sectionFigures(answer.data.counts)
    : answer.error !== null
      ? {}
      : held;
  useEffect(() => {
    held = figures;
  });
  useSectionCounts(figures);
  return answer;
}
