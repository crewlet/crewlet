/**
 * Who files into a knowledge space — the units whose `space:` names it, and
 * the tracker project each of them works in.
 *
 * # Read from the org chart, in FOUR states
 *
 * A unit's `space:` is guarded (`internal/api/orgprojection_test.go`: "a
 * knowledge container key: where this unit's pages are written"), so the
 * org projection carries none of it, and the answer comes from the
 * org chart (`GET /chart`), which states it on every unit row — the runtime
 * half is not needed. It came from the company document, which holds no units
 * any more: the chart left it for a log of its own, so the list was empty for
 * every space. Four different facts come out of trying to read it, and each
 * has its own sentence:
 *
 *  - `read`    — the chart answered; an EMPTY list is then a real fact about
 *                the company ("no unit names this space").
 *  - `unread`  — the read is in flight. Not a refusal: that sentence was drawn
 *                at a reader who holds the grant for as long as the read took.
 *  - `refused` — the engine refused it on authority, naming the grants that
 *                would have admitted the reader (`needsSentence`).
 *  - `failed`  — the engine did not answer, or could not; the shared REST read
 *                asks again on its own (`~/lib/chartReads.ts`).
 *
 * RE-READ ON EVERY ORG PUSH, which follows a chart write that landed — so a
 * unit given a space shows up here without a reload.
 *
 * ONE HOOK for the tree's space rows and the container rail, because both
 * state the same fact about the same space and two derivations of it had
 * already disagreed about which states exist.
 */

import { useMemo } from "react";
import { useChartRead, WHOLE_CHART } from "~/lib/chartReads.ts";
import { needsSentence } from "~/lib/refusal.ts";
import { useOrgPushes } from "~/lib/store-hooks.ts";
import type { ChartRead } from "~/protocol/index.ts";

/** One unit filing into a space: its name, its KEY — what a link addresses — and its project. */
export interface SpaceUnit {
  name: string;
  key: string;
  project?: string;
}

export type SpaceOwners =
  | { state: "read"; units: SpaceUnit[] }
  | { state: "unread" }
  | { state: "refused"; grants: readonly string[] }
  | { state: "failed" };

/** A lookup from a space key to who files into it. */
export function useSpaceOwners(options: { enabled?: boolean } = {}): (key: string) => SpaceOwners {
  const pushes = useOrgPushes();
  const chart = useChartRead<ChartRead>(
    (options.enabled ?? true) ? WHOLE_CHART : null,
    undefined,
    pushes,
  );
  return useMemo(() => {
    switch (chart.state) {
      case "refused":
        return () => ({ state: "refused" as const, grants: chart.grants });
      case "failed":
      case "absent":
        return () => ({ state: "failed" as const });
      case "unread":
        return () => ({ state: "unread" as const });
    }
    const units = chart.value.units ?? [];
    return (key: string) => ({
      state: "read" as const,
      // CASE-INSENSITIVE: the engine upper-cases a container key on the way
      // in, and whoever wrote the unit wrote whatever they typed.
      units: units
        .filter((u) => (u.space ?? "").toUpperCase() === key.toUpperCase())
        .map((u) => ({ name: u.name || u.key, key: u.key, project: u.project })),
    });
  }, [chart]);
}

/** The one sentence that states `owners`, for a tooltip or a screen reader. */
export function ownerSentence(owners: SpaceOwners): string {
  switch (owners.state) {
    case "unread":
      return "Who files here is still being read";
    case "refused":
      return needsSentence("Reading who files here", owners.grants);
    case "failed":
      return "Who files here could not be read just now";
    case "read": {
      if (owners.units.length === 0) return "No unit names this space in its space:";
      return owners.units
        .map((u) => `Filed by ${u.name}${u.project ? `, whose work is in ${u.project}` : ""}`)
        .join("; ");
    }
  }
}

/**
 * The tracker projects of the units filing here that the space's own key does
 * not already name — the ones worth DRAWING beside it. The Nimbus shape keys a
 * space the same as its unit's project, and a second `ENG` beside `ENG` says
 * nothing; a space filed by a unit working in another project is exactly the
 * case a reader cannot guess.
 */
export function otherProjects(owners: SpaceOwners, key: string): string[] {
  if (owners.state !== "read") return [];
  const out: string[] = [];
  for (const u of owners.units) {
    const p = u.project?.trim();
    if (p && p.toUpperCase() !== key.toUpperCase() && !out.includes(p)) out.push(p);
  }
  return out;
}
