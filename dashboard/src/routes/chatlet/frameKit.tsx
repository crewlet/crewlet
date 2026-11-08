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

import { AssistantControlContext, type AssistantControl } from "~/app/assistant/control.ts";
import { crumbsFor } from "~/app/crumbs.ts";
import { Trail } from "~/app/header/PageHeader.tsx";
import { useRoute } from "~/app/router.tsx";
import { chatViewOf, resolve } from "~/app/routes.ts";
import { ChatRoomsProvider } from "~/lib/chatRooms.ts";
import { useViewer } from "~/lib/viewer.ts";
import { mountChat } from "~/test/chat.tsx";

import { Chat, ChatColumn, ChatFrameRoot, type ChatAddress } from "./Chat.tsx";

/** What `mountInFrame` takes: `mountChat`'s options, and where the frame is. */
export type FrameOptions = NonNullable<Parameters<typeof mountChat>[1]> & {
  /** The Chatlet address the frame is at: `#/chatlet`, a room, a view, a pane. */
  hash?: string;
  /**
   * The app frame's hold on Ask Crewlet (`app/Shell.tsx`), for a suite about
   * a control that reads it; unset, it is the frame's own default, still
   * checking whether the reader may ask.
   */
  assistant?: AssistantControl;
};

/** `node` under the app frame's hold on Ask Crewlet, where a suite gives one. */
function withAssistant(node: ReactNode, assistant: AssistantControl | undefined): ReactNode {
  if (!assistant) return node;
  return (
    <AssistantControlContext.Provider value={assistant}>{node}</AssistantControlContext.Provider>
  );
}

/** Mount `children` inside the chat screen's frame, at `hash`. */
export function mountInFrame(
  children: ReactNode,
  { hash = "#/chatlet", assistant, ...opts }: FrameOptions = {},
) {
  location.hash = hash;
  // THE APP FRAME'S ONE READING OF THE READER'S ROOMS, which the chat frame
  // takes rather than asking for itself (`lib/chatRooms.ts`).
  return mountChat(
    withAssistant(
      <ChatRoomsProvider>
        <InFrame>{children}</InFrame>
      </ChatRoomsProvider>,
      assistant,
    ),
    opts,
  );
}

/**
 * The page trail the app frame draws over the screen (`app/Shell.tsx`), from
 * the router's address: its last crumb is the page's one `h1`.
 */
function PageTrail() {
  const route = useRoute();
  return <Trail crumbs={crumbsFor(route.path, {}, route.query)} />;
}

/**
 * Mount a reader view in the screen's conversation column, under the page
 * trail that names it and the top bar its tools stand in (`ViewHead.tsx`).
 */
export function mountView(view: ReactNode, opts: FrameOptions = {}) {
  return mountInFrame(
    <>
      <PageTrail />
      <ChatColumn>{view}</ChatColumn>
    </>,
    opts,
  );
}

/**
 * Mount the whole chat screen at `hash`, as the app mounts it for that
 * address: a move the reader makes changes the hash, and the screen follows.
 */
export function mountScreen({ hash = "#/chatlet", assistant, ...opts }: FrameOptions = {}) {
  location.hash = hash;
  return mountChat(
    withAssistant(
      <ChatRoomsProvider>
        <PageTrail />
        <ScreenAtAddress />
      </ChatRoomsProvider>,
      assistant,
    ),
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
