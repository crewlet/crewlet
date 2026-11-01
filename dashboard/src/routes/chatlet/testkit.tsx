/**
 * What the chat screen's own suites mount that is not a screen. Imported by
 * tests only.
 *
 * HERE RATHER THAN IN `test/chat.tsx`, which holds what every chat suite
 * needs of the FRAME (the act route, the socket, the viewer): a kit outside
 * `routes/` that reached into a screen's module would pull the screen into
 * the entry chunk the moment anything else imported it, which is what
 * `app/lazy.test.tsx` refuses.
 */

import { useMemo, useState } from "react";

import { useQuery } from "~/lib/useQuery.ts";

import type { ReadCursors } from "./cursor.ts";
import { ChatDrafts, DraftsContext } from "./drafts.ts";
import { useChatFrames } from "./live.ts";
import {
  ChatFrameProvider,
  ChatNavProvider,
  OpenRoomProvider,
  useChatNavigation,
  type ChatFrame,
  type ChatNav,
  type OpenRoom,
} from "./nav.tsx";
import { useOutbox } from "./outbox.ts";
import { Room } from "./Room.tsx";
import { roomTitle, roomWhere } from "./rooms.ts";

/** What a suite reads back of the screen around the room. */
export interface LoneRoomProbe {
  nav: ChatNav | null;
  marked: { room: string; through: unknown }[];
  /** Each star pressed in the room, where a suite asks for them. */
  stars?: { room: string; on: boolean }[];
}

/**
 * A room mounted on its own, inside the frame the chat screen holds above
 * every room (`Chat.tsx`): its navigation, the outbox, the cursor writer, the
 * drafts and the open room's detail, read once here as the frame reads it.
 * What the frame does is the frame's suite's; this is what a suite about one
 * room needs, and no second idea of what any of it is.
 */
export function LoneRoom({
  channelID,
  viewer,
  nameOf,
  cursors,
  message,
  probe,
}: {
  channelID: string;
  viewer: string;
  nameOf: (handle: string) => string;
  cursors: ReadCursors;
  /** A message the address lands on. */
  message?: string;
  probe?: LoneRoomProbe;
}) {
  const outbox = useOutbox();
  const [drafts] = useState(() => new ChatDrafts());
  const [landed, setLanded] = useState(false);
  // A WIDE COLUMN, as a suite's jsdom measures no width at all.
  const nav = useChatNavigation(
    { kind: "room", id: channelID, message: landed ? undefined : message },
    null,
    false,
  );
  const shown: ChatNav = useMemo(() => ({ ...nav, landed: () => setLanded(true) }), [nav]);
  if (probe) probe.nav = shown;
  const detail = useQuery("chat_channel", { channel_id: channelID });
  useChatFrames(channelID, () => detail.refetch());
  const rail = useQuery("chat_channels", {});
  const channels = useMemo(() => rail.data?.channels ?? [], [rail.data]);
  const summary = channels.find((row) => row.channel.id === channelID);
  const open = useMemo<OpenRoom>(() => {
    const channel = detail.data?.channel ?? summary?.channel;
    const title = channel
      ? roomTitle(channel, { participants: summary?.participants, viewer, nameOf })
      : channelID;
    return {
      id: channelID,
      detail,
      summary,
      channel,
      title,
      where: channel ? roomWhere(channel.kind, title) : title,
    };
  }, [channelID, detail, summary, viewer, nameOf]);
  const frame = useMemo<ChatFrame>(
    () => ({
      viewer,
      nameOf,
      rail,
      channels,
      measured: true,
      outbox,
      cursors,
      markRead: (room, through) => probe?.marked.push({ room, through }),
      // THE READER'S MARKS AND COUNTS are the top bar's and the Rooms panel's,
      // which a room alone does not draw; a star pressed in the room itself
      // (a team's room offers one) is recorded for the suite that asks.
      starred: new Set<string>(),
      star: (room, on) => probe?.stars?.push({ room, on }),
      counts: null,
    }),
    [viewer, nameOf, rail, channels, outbox, cursors, probe],
  );
  return (
    <ChatNavProvider value={shown}>
      <ChatFrameProvider value={frame}>
        <DraftsContext.Provider value={drafts}>
          <OpenRoomProvider room={open}>
            <Room key={channelID} channelID={channelID} />
            {shown.dialog && <output data-dialog={shown.dialog.kind} />}
          </OpenRoomProvider>
        </DraftsContext.Provider>
      </ChatFrameProvider>
    </ChatNavProvider>
  );
}
