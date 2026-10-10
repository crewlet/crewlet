/**
 * Who files into a knowledge space — the units whose `space:` names it, and
 * the tracker project each of them works in.
 *
 * # Read from the guarded company document, in FIVE states
 *
 * A unit's `space:` is guarded (`internal/api/orgprojection_test.go`: "a
 * knowledge container key: where this unit's pages are written"), so the
 * public org projection carries none of it and the answer comes from the
 * company document, which is an admin's (ADR-0031). Five different facts come
 * out of trying to read it, and each has its own sentence:
 *
 *  - `read`      — the document answered; an EMPTY list is then a real fact
 *                  about the company ("no unit names this space").
 *  - `no_token`  — this browser presents no key, so nothing was asked: the
 *                  refusal is known before the question.
 *  - `forbidden` — the key is a member's, which the viewer says or the engine
 *                  answered: the read is an admin's, and another paste of the
 *                  same key changes nothing, so the sentence is not "could
 *                  not be read with this token".
 *  - `loading`   — a key is held and the read is in flight. Not "needs a
 *                  key": that sentence was drawn at an admin who HAS one for
 *                  as long as the read took.
 *  - `refused`   — the read answered with any other error: the key did not
 *                  open it.
 *
 * NOT ASKED WHERE THE ANSWER IS KNOWN ([knownRefusal]): a member's key asking
 * for the document puts a refusal on the wire on every knowledge page.
 *
 * ONE HOOK for the tree's space rows and the container rail, because both
 * state the same fact about the same space and two derivations of it had
 * already disagreed about which states exist.
 */

import { useMemo } from "react";
import { useQuery } from "~/lib/useQuery.ts";
import { knownRefusal, useViewer } from "~/lib/viewer.ts";
import { apiToken } from "~/protocol/authToken.ts";
import { documentUnits } from "~/lib/seats.ts";

export type SpaceOwners =
  | { state: "read"; units: { name: string; project?: string }[] }
  | { state: "no_token" }
  | { state: "forbidden" }
  | { state: "loading" }
  | { state: "refused" };

/** A lookup from a space key to who files into it. */
export function useSpaceOwners(options: { enabled?: boolean } = {}): (key: string) => SpaceOwners {
  const known = knownRefusal(useViewer(), apiToken() !== "");
  const doc = useQuery("config", undefined, {
    enabled: known === null && (options.enabled ?? true),
  });
  return useMemo(() => {
    if (known === "unauthorized") return () => ({ state: "no_token" as const });
    // A MEMBER'S KEY, known or answered: the one refusal a key cannot fix.
    if (known === "forbidden" || doc.error === "forbidden") {
      return () => ({ state: "forbidden" as const });
    }
    if (doc.error) return () => ({ state: "refused" as const });
    if (!doc.data) return () => ({ state: "loading" as const });
    const units = documentUnits(doc.data);
    return (key: string) => ({
      state: "read" as const,
      // CASE-INSENSITIVE: the engine upper-cases a container key on the way
      // in and a config file says whatever its author typed.
      units: units
        .filter((u) => (u.space ?? "").toUpperCase() === key.toUpperCase())
        .map((u) => ({ name: u.name, project: u.project })),
    });
  }, [known, doc.data, doc.error]);
}

/** The one sentence that states `owners`, for a tooltip or a screen reader. */
export function ownerSentence(owners: SpaceOwners): string {
  switch (owners.state) {
    case "no_token":
      return "Who files here needs an admin key to read";
    case "forbidden":
      return "Who files here is for admins to read";
    case "loading":
      return "Who files here is still being read";
    case "refused":
      return "Who files here could not be read with this token";
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
