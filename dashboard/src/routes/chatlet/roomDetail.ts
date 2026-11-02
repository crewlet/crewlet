/**
 * One room's detail (`chat_channel`) for a pane or a dialog about it.
 *
 * THE OPEN ROOM'S IS THE FRAME'S, read once for the top bar, the room and
 * every pane about it (`nav.tsx`'s `useOpenRoom`); a room the conversation
 * column is not on (a thread opened from Activity, a link naming another
 * room) is asked here, and only then. Two reads of one answer are two answers
 * that can disagree, and the socket runs four questions at a time.
 */

import { useQuery, type QueryResult } from "~/lib/useQuery.ts";
import type { ChatChannelDetail } from "~/protocol/index.ts";

import { useOpenRoom } from "./nav.tsx";

export function useRoomDetail(room: string): QueryResult<ChatChannelDetail> {
  const open = useOpenRoom();
  const own = open?.id === room ? open.detail : null;
  const asked = useQuery("chat_channel", { channel_id: room }, { enabled: own === null });
  return own ?? asked;
}
