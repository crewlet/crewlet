/**
 * Chatlet: the company's own conversation, for a person.
 *
 * # Who is asking is the server's answer, always
 *
 * Every question this screen asks is answered for the principal the SESSION
 * resolves to (ADR-0024), as the seat it holds: there is no handle parameter
 * anywhere in the chat family and no arm (an admin's included) that reads
 * somebody else's rooms. A credential acting as no seat gets NOTHING here,
 * reads included, which is stricter than every other personal surface in this
 * dashboard and deliberately so: a transcript is the most sensitive thing a
 * deployment holds, and a pipeline's credential is not a person.
 *
 * So a reader arrives in one of three shapes before there are any rooms: one
 * without the Chatlet app, a credential acting as no seat, and a person. The
 * FIRST IS THE FRAME'S: the chat row names its app (`app/nav.ts`), so a reader
 * the engine says holds none is drawn the one refusal every app's workspace
 * draws (`GrantRequired`) in place of every chat page, never a second, weaker
 * one here. The SECOND is this screen's, because no other surface has it: a
 * credential the engine resolves and the identity directory binds to no seat
 * (a deployment's own token, a service account). It is an ordinary state, not
 * a fault, and the remedy is one binding (`crewlet iam bind`), so the screen
 * says that from the one fact it holds independently: the viewer answer with
 * a login and no seat.
 *
 * A VIEWER READ THAT FAILED IS NO ANSWER and holds nothing: the screen asks,
 * and the engine's own answer or refusal says what is true. Waiting on it
 * would hold the screen until the viewer's next poll, minutes away, while
 * every other screen worked, which is the failure `lib/viewer.ts` names.
 *
 * # The frame
 *
 * The conversation runs edge to edge under the page bar (the shell's flush
 * fill), with the top bar across it. The Rooms panel slides in from the
 * start edge INSIDE the chat area (the chat area hosts its own layers, so the
 * app sidebar is never covered), or is pinned as a column beside the
 * conversation. A pane (a thread, a room's details, a profile, a catch up)
 * stands beside the conversation on a wide screen and over it on a narrow
 * one. Where the reader is, which pane is open and how to move are
 * `nav.tsx`'s, all of it addresses. The frame's rules are
 * `styles/chatlet-frame.css`, imported here rather than by `main.tsx`, so
 * they ship beside Chatlet's chunk.
 *
 * # Nothing here decides who may see what
 *
 * Every room in the rail, every message in a transcript and every live frame
 * on the socket has already been through the engine's own visibility rule. A
 * client-side filter would be a second rule that could disagree with it, and
 * the disagreement would be silent in whichever direction it went.
 */

import "~/styles/chatlet-frame.css";
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type ReactNode,
  type RefObject,
} from "react";
import { EmptyState, InlineCode, LayerHost, Skeleton } from "@crewlethq/ui";
import { UserGlyph } from "@crewlethq/icons/glyphs";

import { samePath, useNavigator, useRoute } from "~/app/router.tsx";
import { useFillScreen } from "~/app/fill.tsx";
import type { ChatView } from "~/app/routes.ts";
import { usePageCoverEnd, usePageLabels } from "~/app/Shell.tsx";
import { CHAT_NARROW } from "~/app/layout.ts";
import { UnboundRemedy } from "~/components/people.tsx";
import { useChatActivity, useChatRead, useCoalesced } from "~/lib/chatLive.ts";
import { badgesAreMeasured, useChatRooms } from "~/lib/chatRooms.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { LogPosition } from "~/protocol/index.ts";

import { BrowseView } from "./Browse.tsx";
import { markRead, packPosition, useReadCursors } from "./cursor.ts";
import { ChatDialogHost } from "./Dialogs.tsx";
import { ChatDrafts, DraftsContext } from "./drafts.ts";
import { useChatFrames } from "./live.ts";
import {
  ChatFrameProvider,
  ChatNavProvider,
  OpenRoomProvider,
  PANE_PARAM,
  paneOf,
  placeOf,
  placePath,
  placeQuery,
  useChatNav,
  useChatNavigation,
  type ChatFrame,
  type OpenRoom,
} from "./nav.tsx";
import { useOutbox } from "./outbox.ts";
import { ChatPaneHost } from "./Pane.tsx";
import { useChatPeople } from "./people.ts";
import { Room } from "./Room.tsx";
import { RoomsDock, RoomsSheet } from "./RoomsPanel.tsx";
import { RoomScopeProvider } from "./scope.tsx";
import { roomTitle, roomWhere } from "./rooms.ts";
import { SearchView } from "./Search.tsx";
import { useChatShortcuts } from "./shortcuts.ts";
import { ChatTopBar } from "./TopBar.tsx";
import { ActivityView, DraftsView, NewMessageView, SavedView, ThreadsView } from "./Views.tsx";
import type { ReaderCounts } from "./panelViews.ts";

/** Where the router says the screen is: a room's id, or one of the views. */
export interface ChatAddress {
  id?: string | undefined;
  view?: ChatView | undefined;
}

/**
 * The chat screen, for the seat this tab reads as now.
 *
 * EVERYTHING BELOW IT IS ONE SEAT'S, and it is MOUNTED UNDER THAT SEAT
 * (`viewer.owner`) so a seat that moves remounts it rather than handing it on:
 * the read cursor writer, what a room has paged in, the messages still being
 * sent from every room with their Retry (the outbox, held here so it outlives
 * the room it was said in), a half-typed message, an open thread, a dialog
 * half filled in, a refusal drawn beside a control. Kept, each is the last
 * seat's under the next one's name, and the ones that write would write as the
 * next one: the engine resolves the seat when a press arrives, so a Retry
 * would post the last seat's words as the next.
 *
 * TWO THINGS MOVE THE SEAT, and only one is in this tab. The identity
 * directory binding the person elsewhere changes `owner` without a reload
 * (the engine closes the socket and the frame asks the viewer again), and that
 * is what this key follows. A different PERSON signing in never reaches here:
 * it hands the whole tab over by a reload with its storage emptied
 * (`lib/session.ts`), which is a remount of everything. Nothing else remounts
 * this: the viewer read keeps its last answer on screen while it asks again,
 * so the screen is never between readers.
 *
 * `seat` IS THE SEAT NOW, read by whatever outlives a render (the writer's
 * timer, the flush a remount's cleanup makes) and written here, in the render
 * that moves it, so the screen being taken down already knows it is no longer
 * the tab's.
 */
export function Chat(address: ChatAddress) {
  const viewer = useViewer();
  const seat = useRef(viewer.owner);
  seat.current = viewer.owner;
  return <ChatAs key={viewer.owner} {...address} seat={seat} />;
}

function ChatAs({ id, view, seat }: ChatAddress & { seat: RefObject<string> }) {
  const viewer = useViewer();
  // THE SCREEN TAKES THE WINDOW'S HEIGHT AND RUNS EDGE TO EDGE rather than
  // growing the page inside its padding. A transcript inside a page that also
  // scrolls is two scrollers under one wheel, and which one moves is whichever
  // the pointer happens to be over, so the reader loses their place in a
  // conversation by looking at it.
  useFillScreen(true, { flush: true });

  if (viewer.asking) {
    return <Skeleton variant="text" rows={6} label="Working out who you are" />;
  }
  if (viewer.unbound) {
    return (
      <EmptyState
        icon={<UserGlyph size="xl" />}
        title="Chatlet is read as the seat you hold"
        description={
          <>
            You&rsquo;re signed in as <InlineCode>{viewer.login}</InlineCode>, which holds no seat.
            Chatlet needs a seat, so no room is open to you.{" "}
            <UnboundRemedy to="read and write here" />
          </>
        }
      />
    );
  }

  return (
    <ChatFrameRoot address={{ id, view }} seat={seat}>
      <ChatLayout>
        <PlaceView />
      </ChatLayout>
      <DialogHost />
    </ChatFrameRoot>
  );
}

/** Where the frame's measured chat area is handed its element. */
const ChatAreaContext = createContext<(el: HTMLDivElement | null) => void>(() => {});

/** How much of the measured chat area's end the assistant stands over, in px. */
const ChatCoverContext = createContext(0);

/**
 * Everything the screen holds above every place, provided to what is under
 * it: the navigation (`nav.tsx`), the frame's own state (the reader's rooms,
 * the outbox, the read cursor writer, the reader's marks and counts), the
 * drafts, and the open room's detail, read once.
 *
 * EXPORTED FOR THE SUITES (`frameKit.tsx`'s `mountInFrame`), so a pane, a view
 * or a dialog is tested inside the frame the screen builds rather than a
 * second one written for the test.
 */
export function ChatFrameRoot({
  address,
  seat,
  children,
}: {
  address: ChatAddress;
  seat: RefObject<string>;
  children: ReactNode;
}) {
  const { id, view } = address;
  const viewer = useViewer();
  const route = useRoute();
  const router = useNavigator();
  const { store } = useClient();

  const place = useMemo(() => placeOf({ id, view }, route.query), [id, view, route.query]);
  const pane = useMemo(() => paneOf(route.query.get(PANE_PARAM)), [route.query]);
  // THE CHAT AREA'S OWN WIDTH, not the window's: the app sidebar, a peek and
  // the assistant's column each take a share of the window that no media
  // query can see (`app/layout.ts`). The layout hands its element up.
  //
  // LESS WHAT THE ASSISTANT STANDS OVER: as an overlay it covers the area's
  // end, which is where a pane is drawn, so the area a reader can see is what
  // decides whether a pane covers the conversation, and the pane stands
  // beside the panel (`--chat-cover-end`, [ChatLayout]) rather than wholly
  // under it.
  const [area, setArea] = useState<HTMLDivElement | null>(null);
  const { width, endGap } = useWidth(area);
  const covered = Math.max(0, usePageCoverEnd() - endGap);
  const nav = useChatNavigation(place, pane, width > 0 && width - covered < CHAT_NARROW);

  // ONE CURSOR WRITER FOR THE SEAT, above the room, so switching rooms is a
  // flush rather than a new writer that has forgotten what the last one had
  // already written ([useReadCursors]).
  const cursors = useReadCursors(seat);
  // WHAT THIS PERSON IS STILL TRYING TO SAY, in every room: held here rather
  // than in the room, which is remounted for each room opened, so an
  // `unknown` row's same-id Retry survives a look at another room (`outbox.ts`).
  const outbox = useOutbox();
  // AND WHAT THEY HAVE NOT SENT YET, in every box: one store for the screen,
  // which the composers write and the Rooms panel and Drafts and sent read.
  const [drafts] = useState(() => new ChatDrafts());
  const people = useChatPeople();

  // THE BARE `#/chatlet` IS A PLACE SAID DIFFERENTLY ([placeOf]): Activity,
  // or New message with the seat a `dm=` names (a seat's page and peek). It
  // is drawn as that place at once, and the address is replaced with the
  // place's own spelling, keeping a pane, so a reload, a copied link and Back
  // all name the view on screen, and a `dm=` is not asked again.
  const bare = samePath(route.path, ["chatlet"]);
  useEffect(() => {
    if (bare) router.replace(placePath(place), placeQuery(place, pane));
  }, [bare, place, pane, router]);

  // THE RAIL IS THE VIEWER'S OWN ROOMS, which is narrower than what they may
  // read: a public room they have not joined is searchable and openable and
  // does not sit in their list. THE FRAME READS IT (`lib/chatRooms.ts`), once
  // for this screen and the sidebar's badge, and this screen CLAIMS the
  // reading: asked on its terms (never while the first viewer read is out,
  // never for a credential the engine has said acts as no seat) and re-read
  // at the room's own rate on every live frame and every move of this
  // person's read state, which it also seeds into the store.
  //
  // A company whose chat is somebody else's, or nobody's, is served no chat
  // question, and never sees this screen once the engine has said so: the
  // frame draws where its chat is in its place (`app/ServiceNotice.tsx`).
  const asks = !viewer.asking && !viewer.unbound;
  const rail = useChatRooms();
  const activity = useChatActivity();
  const read = useChatRead();
  const readRevision = read?.revision ?? 0;

  // WHAT WAITS IN THE PANEL'S VIEWS: the reader's unread mentions and the new
  // replies in the threads they follow, one row asked for since the counts
  // are what is wanted. Re-read on what moves either: a frame, or the read
  // state.
  const feed = useQuery("chat_activity", { limit: 1 }, { enabled: asks });
  useCoalesced(activity + readRevision, feed.refetch);

  const channels = useMemo(() => rail.data?.channels ?? [], [rail.data]);
  const measured = badgesAreMeasured(rail.data);

  // THE OPEN ROOM'S OWN DETAIL, read ONCE for the top bar, the room and every
  // pane about it, and asked again on every live frame in the room. An answer
  // about the room the reader just left is no answer about this one.
  const room = place.kind === "room" ? place.id : "";
  const detail = useQuery("chat_channel", { channel_id: room }, { enabled: asks && room !== "" });
  useChatFrames(room, () => {
    if (room) detail.refetch();
  });
  const summary = room ? channels.find((row) => row.channel.id === room) : undefined;
  const nameOf = people.nameOf;
  const open = useMemo<OpenRoom | null>(() => {
    if (!room) return null;
    const own = detail.data?.channel.id === room ? detail.data : null;
    const channel = own?.channel ?? summary?.channel;
    const title = channel
      ? roomTitle(channel, { participants: summary?.participants, viewer: viewer.handle, nameOf })
      : room;
    return {
      id: room,
      detail: { ...detail, data: own },
      summary,
      channel,
      title,
      where: channel ? roomWhere(channel.kind, title) : title,
    };
  }, [room, detail, summary, viewer.handle, nameOf]);

  // THE TRAIL AND THE TAB TITLE READ THE ROOM'S NAME, not its uuid, and the
  // label is withdrawn when the room goes, so the next screen's identically
  // shaped segment is not titled with a room.
  usePageLabels(open?.channel ? { [open.id]: open.where } : {});

  const mark = useAct("mark_chat_read");
  const markReadThrough = useCallback(
    (target: string, through?: LogPosition | null) => {
      const position = through ?? channels.find((row) => row.channel.id === target)?.last?.position;
      cursors.see(target, packPosition(position));
      cursors.flushNow();
    },
    [channels, cursors],
  );
  const star = useCallback(
    (target: string, on: boolean) =>
      void markRead(mark, store, on ? { star: [target] } : { unstar: [target] }, {
        done: on ? "Added to Starred" : "Removed from Starred",
      }),
    [mark, store],
  );
  const starred = useMemo(() => new Set(read?.starred ?? []), [read?.starred]);
  const counts = useMemo<ReaderCounts | null>(
    () =>
      feed.data && feed.data.read_state
        ? {
            activity: feed.data.unread.mention,
            threads: feed.data.unread.reply,
            saved: read?.saved?.length ?? 0,
          }
        : null,
    [feed.data, read?.saved],
  );

  const frame = useMemo<ChatFrame>(
    () => ({
      viewer: viewer.handle,
      nameOf,
      rail,
      channels,
      measured,
      outbox,
      cursors,
      markRead: markReadThrough,
      starred,
      star,
      counts,
    }),
    [
      viewer.handle,
      nameOf,
      rail,
      channels,
      measured,
      outbox,
      cursors,
      markReadThrough,
      starred,
      star,
      counts,
    ],
  );

  useChatShortcuts(nav, frame);

  return (
    <ChatNavProvider value={nav}>
      <ChatFrameProvider value={frame}>
        <DraftsContext.Provider value={drafts}>
          <OpenRoomProvider room={open}>
            <ChatAreaContext.Provider value={setArea}>
              <ChatCoverContext.Provider value={covered}>{children}</ChatCoverContext.Provider>
            </ChatAreaContext.Provider>
          </OpenRoomProvider>
        </DraftsContext.Provider>
      </ChatFrameProvider>
    </ChatNavProvider>
  );
}

/** A gesture in progress, over the screen (`Dialogs.tsx`). */
function DialogHost() {
  const nav = useChatNav();
  return <ChatDialogHost dialog={nav.dialog} onClose={nav.closeDialog} />;
}

/**
 * The chat area: the pinned Rooms column, the conversation under its top
 * bar, and the pane. It hosts its own layers, so the slid-in Rooms panel
 * opens from under Menu, over the conversation, and the app sidebar stays.
 */
function ChatLayout({ children }: { children: ReactNode }) {
  const nav = useChatNav();
  const area = useContext(ChatAreaContext);
  // WHAT THE ASSISTANT STANDS OVER at the area's end, which a pane keeps
  // clear of (`styles/chatlet-frame.css`).
  const covered = useContext(ChatCoverContext);
  const cover = covered > 0 ? ({ "--chat-cover-end": `${covered}px` } as CSSProperties) : undefined;
  // MENU BRINGS THE PANEL BACK, so it is where focus goes when the panel is
  // unpinned from inside the dock.
  const menu = useRef<HTMLButtonElement>(null);
  const className = [
    "chat-app",
    nav.rooms.docked && "is-docked",
    nav.pane && "has-pane",
    nav.narrow && "is-narrow",
  ]
    .filter(Boolean)
    .join(" ");
  // ONE SCOPE FOR THE OPEN ROOM, around everything that acts on it: the top
  // bar's actions, the room, and a pane about it share one reading of its
  // rules and one set of presses (`scope.tsx`). Provided on every place,
  // the scope of no room off one, so moving between a room and a view never
  // remounts the panel or the bar beneath it.
  const room = nav.place.kind === "room" ? nav.place.id : "";
  return (
    <LayerHost className="chat-layer">
      <RoomScopeProvider channelID={room}>
        <div ref={area} className={className} style={cover}>
          {!nav.narrow && <RoomsDock docked={nav.rooms.docked} back={() => menu.current} />}
          <section className="chat-main" aria-label="Conversation">
            <ChatTopBar menu={menu} />
            {children}
          </section>
          {nav.pane && <ChatPaneHost pane={nav.pane} onClose={nav.closePane} />}
        </div>
      </RoomScopeProvider>
      {nav.rooms.open && <RoomsSheet />}
    </LayerHost>
  );
}

/** What the conversation column shows for the place. */
function PlaceView() {
  const { place } = useChatNav();
  switch (place.kind) {
    case "room":
      // ONE ROOM PER MOUNT: what a room has paged in, its scroll and its
      // composer belong to it, and another room is another conversation.
      return <Room key={place.id} channelID={place.id} />;
    case "activity":
      return <ActivityView />;
    case "threads":
      return <ThreadsView />;
    case "drafts":
      return <DraftsView />;
    case "saved":
      return <SavedView />;
    // A VIEW THAT WRITES ITS OWN ADDRESS AS THE READER TYPES (a search, the
    // people a new message is to) is never keyed on it: a remount per
    // keystroke would take the field's focus and caret with it.
    case "search":
      return <SearchView />;
    case "browse":
      return <BrowseView />;
    case "new":
      return <NewMessageView />;
  }
}

/**
 * An element's own width, measured again as it changes, and how far its end
 * stands from the window's end edge; zero until measured, which reads as
 * wide: the wide layout is the one that works before anything has been
 * measured.
 */
function useWidth(el: HTMLElement | null): { width: number; endGap: number } {
  const [box, setBox] = useState({ width: 0, endGap: 0 });
  useLayoutEffect(() => {
    if (!el || typeof ResizeObserver === "undefined") return;
    const measure = () => {
      const rect = el.getBoundingClientRect();
      const endGap = Math.max(0, window.innerWidth - rect.right);
      setBox((was) =>
        was.width === rect.width && was.endGap === endGap ? was : { width: rect.width, endGap },
      );
    };
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, [el]);
  return box;
}
