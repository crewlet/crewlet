/**
 * Who files into a knowledge space — the units whose `space:` names it, and
 * the tracker project each of them works in.
 *
 * # Read from the guarded company document, in FOUR states
 *
 * A unit's `space:` is guarded (`internal/api/orgprojection_test.go`: "a
 * knowledge container key: where this unit's pages are written"), so the
 * `state:read` org projection carries none of it and the answer comes from the
 * company document, which takes `config:read`. Four different facts come out
 * of trying to read it, and each has its own sentence:
 *
 *  - `read`     — the document answered; an EMPTY list is then a real fact
 *                 about the company ("no unit names this space").
 *  - `no_grant` — the engine has said this reader does not hold
 *                 `config:read`, so nothing was asked: the refusal is known
 *                 before the question.
 *  - `loading`  — the read is in flight, or the viewer has not answered yet.
 *                 Not "needs a grant": that sentence was drawn at a reader who
 *                 HOLDS it for as long as the read took.
 *  - `refused`  — the read answered with an error.
 *
 * ONE HOOK for the tree's space rows and the container rail, because both
 * state the same fact about the same space and two derivations of it had
 * already disagreed about which states exist.
 */

import { useMemo } from "react";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { documentUnits, RUNTIME_GRANT } from "~/lib/seats.ts";

export type SpaceOwners =
  | { state: "read"; units: { id: string; name: string; project?: string }[] }
  | { state: "no_grant" }
  | { state: "loading" }
  | { state: "refused" };

/** A lookup from a space key to who files into it. */
export function useSpaceOwners(options: { enabled?: boolean } = {}): (key: string) => SpaceOwners {
  const viewer = useViewer();
  const holds = viewer.grants.includes(RUNTIME_GRANT);
  const refused = !viewer.loading && !holds;
  const doc = useQuery("config", undefined, { enabled: holds && (options.enabled ?? true) });
  return useMemo(() => {
    if (refused) return () => ({ state: "no_grant" as const });
    if (doc.error) return () => ({ state: "refused" as const });
    if (!doc.data) return () => ({ state: "loading" as const });
    const units = documentUnits(doc.data);
    return (key: string) => ({
      state: "read" as const,
      // CASE-INSENSITIVE: the engine upper-cases a container key on the way
      // in and a config file says whatever its author typed.
      units: units
        .filter((u) => (u.space ?? "").toUpperCase() === key.toUpperCase())
        .map((u) => ({ id: u.id ?? "", name: u.name, project: u.project })),
    });
  }, [refused, doc.data, doc.error]);
}

/** The one sentence that states `owners`, for a tooltip or a screen reader. */
export function ownerSentence(owners: SpaceOwners): string {
  switch (owners.state) {
    case "no_grant":
      return "Who files here needs config:read to read";
    case "loading":
      return "Who files here is still being read";
    case "refused":
      return "Who files here could not be read";
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
