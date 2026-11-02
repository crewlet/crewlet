/**
 * Where a reader stands in a room, and what that lets them do about it.
 *
 * THE ENGINE'S OWN REFUSALS ABOUT MEMBERSHIP, mirrored so the screen does not
 * offer a button whose only outcome is a refusal: a private room admits no
 * join, a unit's room neither a join nor a leave, a direct conversation
 * neither, and a room this build cannot classify nothing at all. Every room a
 * reader is shown has already been through the engine's own visibility rule;
 * this decides only which gesture is offered, never who may see what.
 */

import { isDirect } from "~/lib/chatRooms.ts";
import type { ChatKind } from "~/protocol/index.ts";

import { knownKind, NEWER_ROOM } from "./rooms.ts";

/** Where the viewer stands in one room, as a join or a leave needs to know. */
export interface RoomStanding {
  kind: ChatKind;
  member: boolean;
  /** The unit whose room it is, for `kind: unit` — named in the sentence
   *  that says why its membership is not the viewer's to move. */
  unit?: string;
}

/**
 * Whether this viewer may JOIN this room.
 *
 * THE ENGINE'S OWN REFUSALS, mirrored so the screen does not offer a button
 * that cannot work: a **private** room refuses a join, because its membership
 * is the only way into it and somebody already inside is who adds you; a
 * **unit's** room refuses one, because its membership is the org chart's
 * alone — readable by every seat, joined only by being in the unit — and a
 * member the chart did not put there would be taken out at the next apply; a
 * **direct** conversation refuses one, because its id IS its participant set,
 * so joining would not widen that room but name a different one; and a room
 * whose kind this build cannot classify refuses every write, because a kind
 * read two-valued falls out as "not private" and that is the one direction
 * that cannot be walked back.
 *
 * A private room the viewer is not in never reaches this: the read that would
 * describe it answers "no such room", exactly as a room that does not exist,
 * because a private room's EXISTENCE is information.
 */
export function canJoin(standing: RoomStanding): boolean {
  return (
    !standing.member &&
    knownKind(standing.kind) &&
    !isDirect(standing.kind) &&
    standing.kind !== "private" &&
    standing.kind !== "unit"
  );
}

/**
 * Whether this viewer may LEAVE it.
 *
 * A UNIT'S ROOM REFUSES A LEAVE, which looks like a restriction and is the
 * opposite: that membership is the org chart's own, so the next apply would
 * put it back — and a gesture that silently reverts is worse than one that is
 * refused, because nothing reports it. A direct conversation refuses one for
 * [canJoin]'s reason, and an unclassifiable room refuses everything.
 */
export function canLeave(standing: RoomStanding): boolean {
  return (
    standing.member &&
    knownKind(standing.kind) &&
    !isDirect(standing.kind) &&
    standing.kind !== "unit"
  );
}

/**
 * Why a room's membership is not this viewer's to move, or "" where it is.
 *
 * SAID RATHER THAN GREYED OUT. Each of these is a rule about the room rather
 * than about who is reading, and a disabled button with no sentence
 * beside it reads as a permission somebody lacks.
 */
export function membershipNote(standing: RoomStanding): string {
  if (!knownKind(standing.kind)) {
    return NEWER_ROOM;
  }
  if (isDirect(standing.kind)) {
    return "A conversation is the people in it, so there’s nobody to add. Different people make a different conversation.";
  }
  if (standing.kind === "private" && !standing.member) {
    return "You can’t join a private room yourself. Someone already in it has to add you.";
  }
  if (standing.kind === "unit" && standing.member) {
    return "You’re in this room because the org chart puts you in its team, so you can’t leave it here.";
  }
  if (standing.kind === "unit") {
    const whose = standing.unit ? `the ${standing.unit} team’s` : "a team’s";
    return `This is ${whose} own room. Anyone can read it, and its members are the team’s, set in the org chart.`;
  }
  return "";
}
