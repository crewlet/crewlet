/**
 * Who a page is told is viewing it, and who is in its room: display facts
 * only, from the chart this tab holds (`docs/reference/artifact-runtime.md`,
 * Identity and Presence).
 */

import type { ArtifactRole } from "~/protocol/index.ts";
import type { ArtifactPeople } from "~/lib/artifacts/people.ts";
import type { Roster } from "~/lib/artifacts/room.ts";
import type { ViewerState } from "~/lib/viewer.ts";
import type { BridgePerson, BridgeRoster, BridgeViewer } from "~/runtime/bridge.ts";

/**
 * The reader as a page is told them, whether or not they hold a seat. A
 * reader signed in without one (an operator's token) is still somebody
 * viewing: their seat is empty, which a person cell cannot name, and they are
 * named as the dashboard names them rather than as "Somebody".
 */
export function pageSelf(
  viewer: Pick<ViewerState, "handle" | "kind" | "name">,
  people: Pick<ArtifactPeople, "pagePerson" | "seatName">,
): BridgePerson {
  return {
    ...people.pagePerson(viewer.handle, viewer.kind === "agent" ? "agent" : "human"),
    displayName: viewer.name || (viewer.handle ? people.seatName(viewer.handle) : "You"),
  };
}

/** The reader as `identity.me()` answers them, once the dashboard knows who is signed in. */
export function pageViewer(
  viewer: Pick<ViewerState, "loading" | "anonymous">,
  self: BridgePerson,
  role: ArtifactRole,
): BridgeViewer | null {
  return viewer.loading || viewer.anonymous ? null : { ...self, role };
}

/** The room as a page is told it: the reader's own session named as the reader. */
export function pageRoster(
  roster: Roster,
  ownSession: string | null,
  self: BridgePerson,
  people: Pick<ArtifactPeople, "pagePerson">,
): BridgeRoster {
  return {
    members: [...roster.members.values()].map((m) => ({
      ...(m.session === ownSession
        ? self
        : people.pagePerson(m.seat, m.kind === "agent" ? "agent" : "human")),
      session: m.session,
      activity: m.activity,
      focus: m.focus,
      at: m.at,
    })),
    ...(roster.counts ? { counts: roster.counts } : {}),
    truncated: roster.truncated,
  };
}
