/**
 * The chat screen's frame, for a suite about one thing inside it: a pane, a
 * view, a dialog, the top bar. Imported by tests only.
 *
 * THE FRAME THE SCREEN BUILDS (`Chat.tsx`'s `ChatFrameRoot`), never a second
 * one written for the suite: the navigation, the reader's rooms, the outbox,
 * the read cursor writer, the reader's marks and counts, the drafts and the
 * open room's detail are each the screen's own, so a suite that passes here
 * passes against what the reader gets. Where the frame is, is an ADDRESS, as
 * it is on the screen: the suite names a hash, and a press that moves the
 * reader moves it.
 *
 * HERE RATHER THAN IN `test/chat.tsx`, which holds what every chat suite needs
 * of the APP's frame: a kit outside `routes/` that reached into a screen's
 * module would pull the screen into the entry chunk the moment anything else
 * imported it, which is what `app/lazy.test.tsx` refuses.
 */

import { useRef, type ReactNode } from "react";

import { useRoute } from "~/app/router.tsx";
import { chatViewOf, resolve } from "~/app/routes.ts";
import { ChatRoomsProvider } from "~/lib/chatRooms.ts";
import { useViewer } from "~/lib/viewer.ts";
import { mountChat } from "~/test/chat.tsx";

import { Chat, ChatFrameRoot, type ChatAddress } from "./Chat.tsx";

/** What `mountInFrame` takes: `mountChat`'s options, and where the frame is. */
export type FrameOptions = NonNullable<Parameters<typeof mountChat>[1]> & {
  /** The Chatlet address the frame is at: `#/chatlet`, a room, a view, a pane. */
  hash?: string;
};

/** Mount `children` inside the chat screen's frame, at `hash`. */
export function mountInFrame(
  children: ReactNode,
  { hash = "#/chatlet", ...opts }: FrameOptions = {},
) {
  location.hash = hash;
  // THE APP FRAME'S ONE READING OF THE READER'S ROOMS, which the chat frame
  // takes rather than asking for itself (`lib/chatRooms.ts`).
  return mountChat(
    <ChatRoomsProvider>
      <InFrame>{children}</InFrame>
    </ChatRoomsProvider>,
    opts,
  );
}

/**
 * Mount the whole chat screen at `hash`, as the app mounts it for that
 * address: a move the reader makes changes the hash, and the screen follows.
 */
export function mountScreen({ hash = "#/chatlet", ...opts }: FrameOptions = {}) {
  location.hash = hash;
  return mountChat(
    <ChatRoomsProvider>
      <ScreenAtAddress />
    </ChatRoomsProvider>,
    opts,
  );
}

/** The Chatlet address the router holds, as `app/routes.ts` resolves it. */
function useAddress(): ChatAddress {
  const route = useRoute();
  const where = resolve(route.path);
  if (!where.resolved) return {};
  if (where.screen === "chatlet") return { id: where.id };
  return { view: chatViewOf(where.screen) ?? undefined };
}

/** The screen at the router's address. */
function ScreenAtAddress() {
  return <Chat {...useAddress()} />;
}

/** The frame at the address the router holds, for the seat the tab reads as. */
function InFrame({ children }: { children: ReactNode }) {
  const viewer = useViewer();
  const seat = useRef(viewer.owner);
  seat.current = viewer.owner;
  return (
    <ChatFrameRoot address={useAddress()} seat={seat}>
      {children}
    </ChatFrameRoot>
  );
}
