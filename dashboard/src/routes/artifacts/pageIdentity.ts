/**
 * Who a page is told is viewing it, and who is in its room: display facts
 * only, from the chart this tab holds (`docs/reference/artifact-runtime.md`,
 * Identity and Presence).
 */

import type { RoomMember, ArtifactRole } from "~/protocol/index.ts";
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

/**
 * The room as a page is told it: the reader's own session named as the
 * reader, and everybody else as the chart names their seat.
 *
 * A MEMBER WITH NO SEAT IS TOLD AS NOBODY IN PARTICULAR. The room names one
 * by their login (an operator's token, a person the directory binds to no
 * seat), which the dashboard shows its own reader, and a page is never told
 * a login. Nobody present can hold a seat that has left the company, so a
 * member the chart does not hold is one of those.
 */
export function pageRoster(
  roster: Roster,
  ownSession: string | null,
  self: BridgePerson,
  people: Pick<ArtifactPeople, "pagePerson" | "index">,
): BridgeRoster {
  const told = (m: RoomMember): BridgePerson => {
    const kind = m.kind === "agent" ? "agent" : "human";
    if (m.session === ownSession) return self;
    if (!people.index.byHandle.has(m.seat)) return { seat: "", kind, displayName: "Somebody" };
    return people.pagePerson(m.seat, kind);
  };
  return {
    members: [...roster.members.values()].map((m) => ({
      ...told(m),
      session: m.session,
      activity: m.activity,
      focus: m.focus,
      at: m.at,
    })),
    ...(roster.counts ? { counts: roster.counts } : {}),
    truncated: roster.truncated,
  };
}
