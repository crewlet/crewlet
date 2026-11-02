/**
 * A room's star: whether the reader starred it, why a star would be refused,
 * and the press, for every control that offers one (the room's More menu, and
 * the bar a team's room shows somebody outside the team).
 *
 * A STAR KEEPS A ROOM IN THE READER'S LIST whether or not they are in it,
 * where they may read it (the rail lists starred rooms beside memberships).
 * That is how somebody outside a team keeps the team's room to hand: its
 * membership is the org chart's, so there is no joining it.
 *
 * SAID, NOT GREYED: past the engine's cap a star is refused rather than
 * dropping an older one, so the cap is a sentence on the control before the
 * press, and so is the browser's own reason for the write.
 */

import { MAX_READER_MARKS } from "~/contract/chat.ts";
import { useWriteAccess } from "~/lib/useWriteAccess.ts";

import { useChatFrame } from "./nav.tsx";

/** Why a room cannot be starred once the reader holds the engine's cap of stars. */
export const STARS_FULL = `You have starred ${MAX_READER_MARKS} rooms, the most one reader keeps. Remove a star to add this room.`;

export interface RoomStar {
  starred: boolean;
  /** Why the star cannot change now, or "". */
  held: string;
  set: (on: boolean) => void;
}

export function useRoomStar(id: string): RoomStar {
  const frame = useChatFrame();
  const marks = useWriteAccess("mark_chat_read");
  const starred = frame.starred.has(id);
  const full = !starred && frame.starred.size >= MAX_READER_MARKS;
  return {
    starred,
    held: marks.can ? (full ? STARS_FULL : "") : marks.reason,
    set: (on) => frame.star(id, on),
  };
}
