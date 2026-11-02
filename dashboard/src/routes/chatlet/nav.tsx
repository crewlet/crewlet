/**
 * Where a reader is in Chatlet, and the one way every part of the screen moves
 * them.
 *
 * # Everything is an address
 *
 * A PLACE is a path under `#/chatlet`: a room (`#/chatlet/{id}`), or one of the
 * views beside the rooms (`activity`, `threads`, `drafts`, `saved`, `search`,
 * `browse`, `new`). The bare `#/chatlet` is no place of its own: it is
 * Activity, Chatlet's landing, said differently ([placeOf]). A PANE is ONE query parameter, `pane`, on whatever place
 * the reader is at: a thread, a room's details, a profile, or a catch up. One
 * parameter rather than one per kind, so two panes can never be open at once
 * by construction rather than by a precedence rule; and an address rather than
 * component state, so Back closes a pane, a reload keeps it, and a thread is a
 * link somebody can hand to a colleague from any place, not only from inside
 * its room.
 *
 * Every link and every move is built by [placeHref] and [ChatNav], so a place
 * is spelled one way in the whole chunk: two spellings of one address are two
 * addresses that can come to disagree about where a reader is.
 *
 * # What the screen holds above every place
 *
 * The frame (`Chat.tsx`) holds what outlives a place: the reader's rooms (the
 * rail), the outbox, the read cursor writer, and the open room's own detail.
 * They are handed down through [useChatFrame] and [useOpenRoom] rather than
 * read again below, because a second read of one answer is a second answer
 * that can disagree with the first, and the socket runs four questions at a
 * time.
 */

import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useState,
  useSyncExternalStore,
  type ReactNode,
} from "react";
import { href, useNavigator } from "~/app/router.tsx";
import { CHAT_VIEWS, type ChatView } from "~/app/routes.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type {
  ChatChannel,
  ChatChannelDetail,
  ChatChannelSummary,
  ChatChannelsAnswer,
  ChatMessageView,
  LogPosition,
} from "~/protocol/index.ts";

import type { ReadCursors } from "./cursor.ts";
import type { OutboxHandle } from "./outbox.ts";
import type { ReaderCounts } from "./panelViews.ts";

// ---------------------------------------------------------------------------
// Places, panes and dialogs
// ---------------------------------------------------------------------------

/** Where the conversation column is. */
export type ChatPlace =
  /** A room. `message` is the one a link landed on, drawn flashed. */
  | { kind: "room"; id: string; message?: string | undefined }
  | { kind: "activity" }
  | { kind: "threads" }
  | { kind: "drafts" }
  | { kind: "saved" }
  /** `room` narrows the search to one room ("Search this room"). */
  | { kind: "search"; q?: string | undefined; room?: string | undefined }
  | { kind: "browse" }
  /** A new message, with the people or agents already chosen. */
  | { kind: "new"; to?: readonly string[] | undefined };

/** The four tabs of a room's details pane. */
export type DetailsTab = "about" | "members" | "shared" | "notifications";

export const DETAILS_TABS: readonly DetailsTab[] = ["about", "members", "shared", "notifications"];

/** Whether Catch up reads what is unread, or summarizes a room read through. */
export type CatchUpMode = "catchup" | "summarize";

/** What the pane beside the conversation (or over it, on a narrow screen) holds. */
export type ChatPane =
  | { kind: "thread"; room: string; root: string }
  | { kind: "details"; room: string; tab: DetailsTab }
  | { kind: "profile"; handle: string }
  | { kind: "catchup"; room: string; mode: CatchUpMode };

/**
 * A gesture in progress: finished or abandoned, never a place, so never an
 * address (`#/chatlet/new-room` would be a page nobody can be on).
 */
export type ChatDialog =
  | { kind: "new-room" }
  | { kind: "shortcuts" }
  | { kind: "delete"; channelID: string; message: ChatMessageView }
  | { kind: "people" | "topic" | "description" | "template"; room: string };

// ---------------------------------------------------------------------------
// Addresses
// ---------------------------------------------------------------------------

/** The query parameter a pane is kept in. */
export const PANE_PARAM = "pane";

/**
 * The separator inside a pane's value. A room is a uuid and a message id is
 * one, and a seat handle is a slug, so none of them can hold it.
 */
const SEP = ":";

/** A pane, as its parameter spells it. */
export function paneParam(pane: ChatPane): string {
  switch (pane.kind) {
    case "thread":
      return ["thread", pane.room, pane.root].join(SEP);
    case "details":
      return ["details", pane.room, pane.tab].join(SEP);
    case "profile":
      return ["profile", pane.handle].join(SEP);
    case "catchup":
      return [pane.mode, pane.room].join(SEP);
  }
}

/**
 * The pane a parameter names, or null for one that names nothing this build
 * draws: a hand-edited address lands on the place without a pane rather than
 * on a pane that cannot be filled.
 */
export function paneOf(raw: string | null): ChatPane | null {
  if (!raw) return null;
  const [kind, first = "", second = ""] = raw.split(SEP);
  switch (kind) {
    case "thread":
      return first && second ? { kind: "thread", room: first, root: second } : null;
    case "details":
      return first && (DETAILS_TABS as readonly string[]).includes(second)
        ? { kind: "details", room: first, tab: second as DetailsTab }
        : null;
    case "profile":
      return first ? { kind: "profile", handle: first } : null;
    case "catchup":
    case "summarize":
      return first ? { kind: "catchup", room: first, mode: kind } : null;
    default:
      return null;
  }
}

/** The room a pane belongs to, or "" for one that belongs to none (a profile). */
export function paneRoom(pane: ChatPane): string {
  return pane.kind === "profile" ? "" : pane.room;
}

/**
 * The place an address names, from what the router parsed (`app/routes.ts`)
 * and the query beside it.
 *
 * THE BARE `#/chatlet` NAMES ACTIVITY, Chatlet's landing, or with `dm=` (a
 * seat's page and peek ask for a direct message that way) New message with
 * that seat chosen. Neither is a place of its own: the frame replaces the
 * address with the place's own spelling ([placePath]), so Back never lands on
 * a spelling the reader did not choose.
 */
export function placeOf(
  where: { id?: string | undefined; view?: ChatView | undefined },
  query: URLSearchParams,
): ChatPlace {
  if (where.id) return { kind: "room", id: where.id, message: query.get("message") || undefined };
  switch (where.view) {
    case undefined: {
      const dm = query.get("dm");
      return dm ? { kind: "new", to: [dm] } : { kind: "activity" };
    }
    case "search":
      return {
        kind: "search",
        q: query.get("q") || undefined,
        room: query.get("in") || undefined,
      };
    case "new": {
      const to = (query.get("to") ?? "").split(",").filter(Boolean);
      return to.length > 0 ? { kind: "new", to } : { kind: "new" };
    }
    default:
      return { kind: where.view };
  }
}

/** The path a place lives at. */
export function placePath(place: ChatPlace): string[] {
  return place.kind === "room" ? ["chatlet", place.id] : ["chatlet", place.kind];
}

/** The query a place and a pane carry, with no empty parameter in it. */
export function placeQuery(place: ChatPlace, pane: ChatPane | null = null): Record<string, string> {
  const query: Record<string, string> = {};
  if (place.kind === "room" && place.message) query.message = place.message;
  if (place.kind === "search") {
    if (place.q) query.q = place.q;
    if (place.room) query.in = place.room;
  }
  if (place.kind === "new" && place.to && place.to.length > 0) query.to = place.to.join(",");
  if (pane) query[PANE_PARAM] = paneParam(pane);
  return query;
}

/** An anchor's href for a place and, optionally, a pane over it. */
export function placeHref(place: ChatPlace, pane: ChatPane | null = null): string {
  return href(placePath(place), placeQuery(place, pane));
}

/** Whether two places are the same screen, ignoring a flashed message. */
export function samePlace(a: ChatPlace, b: ChatPlace): boolean {
  if (a.kind !== b.kind) return false;
  if (a.kind === "room" && b.kind === "room") return a.id === b.id;
  return placeHref(a) === placeHref(b);
}

/**
 * Which pane survives a move to `place`.
 *
 * A PROFILE IS NOBODY'S ROOM, so it stays wherever the reader goes: they were
 * reading about a person, and the person did not change. A pane that belongs
 * to a room stays only while the reader is in THAT room; arriving anywhere
 * else would leave a thread open beside a conversation it was not said in.
 */
export function paneAfter(pane: ChatPane | null, place: ChatPlace): ChatPane | null {
  if (!pane) return null;
  const room = paneRoom(pane);
  if (room === "") return pane;
  return place.kind === "room" && place.id === room ? pane : null;
}

// ---------------------------------------------------------------------------
// The navigation every part of the screen moves through
// ---------------------------------------------------------------------------

/** The Rooms panel: slid in over the conversation, or docked beside it. */
export interface RoomsPanel {
  /** The sheet is slid in. Never true while docked. */
  open: boolean;
  /** The panel is a column beside the conversation. Never true when narrow. */
  docked: boolean;
  /** Menu: docked, shows and hides the column; otherwise slides the sheet. */
  toggle: () => void;
  close: () => void;
  /** Pin (true) or unpin (false), remembered for this browser. */
  dock: (docked: boolean) => void;
}

export interface ChatNav {
  place: ChatPlace;
  pane: ChatPane | null;
  /**
   * A new place, keeping only the pane [paneAfter] keeps. Pushes; with
   * `replace`, replaces the entry instead, for a place said differently
   * rather than a place gone to (a search refined as it is typed, whose
   * every prefix would otherwise be a step of Back).
   */
  go: (place: ChatPlace, opts?: { replace?: boolean | undefined }) => void;
  /** A room, optionally landing on one of its messages. */
  openRoom: (id: string, opts?: { message?: string | undefined }) => void;
  /**
   * The flashed message has been landed on: drop it from the address (by
   * replace), so the same link flashes again when it is followed again.
   */
  landed: () => void;
  /**
   * A thread. From a room it opens in that room; from a view (Threads,
   * Activity, Saved) it opens beside the view, which stays where it was.
   */
  openThread: (room: string, root: string) => void;
  openPane: (pane: ChatPane) => void;
  closePane: () => void;
  /** An anchor's href, so a link is a real link (middle click, copy). */
  hrefOf: (place: ChatPlace, pane?: ChatPane | null) => string;
  dialog: ChatDialog | null;
  openDialog: (dialog: ChatDialog) => void;
  closeDialog: () => void;
  /**
   * The chat area is narrower than `app/layout.ts`'s `CHAT_NARROW`: the Rooms
   * panel is never pinned, and a pane covers the conversation.
   */
  narrow: boolean;
  rooms: RoomsPanel;
}

const ChatNavContext = createContext<ChatNav | null>(null);

/** The screen's navigation. Throws outside it: that is a wiring bug. */
export function useChatNav(): ChatNav {
  const nav = useContext(ChatNavContext);
  if (!nav) throw new Error("useChatNav outside the Chatlet screen");
  return nav;
}

export const ChatNavProvider = ChatNavContext.Provider;

/**
 * Whether the Rooms panel is docked, as this browser remembers it.
 *
 * A PER-BROWSER CONVENIENCE, as a dragged panel width is (`lib/storage.ts`):
 * how somebody likes their screen laid out, which no other reader and no
 * other device needs to agree on. Read and written through one subscription,
 * so a second tab's pin is not this tab's business until it reloads, and a
 * refused accessor (a private window) reads as unpinned.
 */
const dockListeners = new Set<() => void>();

function readDocked(): boolean {
  try {
    return localStorage.getItem(STORAGE_KEYS.chatletRoomsDocked) === "1";
  } catch {
    return false;
  }
}

function writeDocked(docked: boolean): void {
  try {
    localStorage.setItem(STORAGE_KEYS.chatletRoomsDocked, docked ? "1" : "0");
  } catch {
    // Refused storage: the choice lasts until the tab reloads.
    memoryDocked = docked;
  }
  for (const listener of dockListeners) listener();
}

/** What a tab whose storage refuses every write remembers instead. */
let memoryDocked: boolean | null = null;

function docked(): boolean {
  return memoryDocked ?? readDocked();
}

function subscribeDocked(listener: () => void): () => void {
  dockListeners.add(listener);
  return () => dockListeners.delete(listener);
}

/** Forget a pin a suite made. Imported by tests only. */
export function resetDockForTest(): void {
  memoryDocked = null;
}

/**
 * Build the screen's navigation for the place the address names.
 *
 * Held by the frame (`Chat.tsx`) and handed down through [ChatNavProvider]:
 * one value per render, so every control that reads it agrees on where the
 * reader is. `narrow` is the frame's measurement of the chat area's own
 * width, never the window's: the app sidebar, a peek and the assistant's
 * column take shares of the window no media query can see.
 */
export function useChatNavigation(
  place: ChatPlace,
  pane: ChatPane | null,
  narrow: boolean,
): ChatNav {
  const router = useNavigator();
  const pinned = useSyncExternalStore(subscribeDocked, docked, () => false);
  const [sheet, setSheet] = useState(false);
  const [dialog, setDialog] = useState<ChatDialog | null>(null);

  const move = useCallback(
    (next: ChatPlace, nextPane: ChatPane | null, replace = false) =>
      (replace ? router.replace : router.to)(placePath(next), placeQuery(next, nextPane)),
    [router],
  );

  const go = useCallback(
    (next: ChatPlace, opts?: { replace?: boolean | undefined }) =>
      move(next, paneAfter(pane, next), opts?.replace === true),
    [move, pane],
  );

  const isDocked = pinned && !narrow;
  const rooms = useMemo<RoomsPanel>(
    () => ({
      open: sheet && !isDocked,
      docked: isDocked,
      toggle: () => (isDocked ? writeDocked(false) : setSheet((open) => !open)),
      close: () => setSheet(false),
      dock: (next) => {
        setSheet(false);
        writeDocked(next);
      },
    }),
    [sheet, isDocked],
  );

  return useMemo<ChatNav>(
    () => ({
      place,
      pane,
      go,
      openRoom: (id, opts) => go({ kind: "room", id, message: opts?.message }),
      landed: () => {
        if (place.kind !== "room" || !place.message) return;
        const here: ChatPlace = { kind: "room", id: place.id };
        router.replace(placePath(here), placeQuery(here, pane));
      },
      openThread: (room, root) => {
        const next: ChatPlace = place.kind === "room" ? { kind: "room", id: room } : place;
        move(next, { kind: "thread", room, root });
      },
      openPane: (next) => move(place, next),
      closePane: () => move(place, null),
      hrefOf: (target, targetPane = null) => placeHref(target, targetPane),
      dialog,
      openDialog: setDialog,
      closeDialog: () => setDialog(null),
      narrow,
      rooms,
    }),
    [place, pane, go, move, router, dialog, narrow, rooms],
  );
}

// ---------------------------------------------------------------------------
// What the frame holds for every place
// ---------------------------------------------------------------------------

/** What the screen holds above every place, for the places to read. */
export interface ChatFrame {
  /** The seat this tab reads as. */
  viewer: string;
  /** A handle to the chart's name for it, or the handle itself. */
  nameOf: (handle: string) => string;
  /** The reader's own rooms, newest activity first (`chat_channels`). */
  rail: QueryResult<ChatChannelsAnswer>;
  channels: readonly ChatChannelSummary[];
  /** The badges are a measurement (`rooms.ts`'s `badgesAreMeasured`). */
  measured: boolean;
  /** Every message still being sent, from every room (`outbox.ts`). */
  outbox: OutboxHandle;
  /** The seat's one read cursor writer (`cursor.ts`). */
  cursors: ReadCursors;
  /**
   * Mark a room read through `through`, or through its newest message as the
   * rail last saw it, and write it now rather than at the next flush: the
   * reader asked for it, and a badge that stays after the press reads as a
   * press that did nothing.
   */
  markRead: (room: string, through?: LogPosition | null) => void;
  /** The rooms the reader starred, from their own state. */
  starred: ReadonlySet<string>;
  /** Star a room, or take its star away. */
  star: (room: string, on: boolean) => void;
  /** What waits in the Rooms panel's views, or null before it is read. */
  counts: ReaderCounts | null;
}

const ChatFrameContext = createContext<ChatFrame | null>(null);

export function useChatFrame(): ChatFrame {
  const frame = useContext(ChatFrameContext);
  if (!frame) throw new Error("useChatFrame outside the Chatlet screen");
  return frame;
}

export const ChatFrameProvider = ChatFrameContext.Provider;

/**
 * The room the conversation column is on: its detail, read ONCE by the frame
 * for the top bar, the room and every pane about it.
 */
export interface OpenRoom {
  id: string;
  /** `chat_channel`, refetched by the frame on every live frame in the room. */
  detail: QueryResult<ChatChannelDetail>;
  /** The room's row in the rail, absent for a room the reader is not in. */
  summary?: ChatChannelSummary | undefined;
  /** The detail's channel, else the rail's, else nothing yet. */
  channel?: ChatChannel | undefined;
  /** What the room is called, without a `#` (`rooms.ts`'s `roomTitle`). */
  title: string;
  /** What a sentence calls it: `#general`, or who a conversation is with. */
  where: string;
}

const OpenRoomContext = createContext<OpenRoom | null>(null);

/** The open room, or null when the conversation column is not on one. */
export function useOpenRoom(): OpenRoom | null {
  return useContext(OpenRoomContext);
}

export function OpenRoomProvider({
  room,
  children,
}: {
  room: OpenRoom | null;
  children: ReactNode;
}) {
  return <OpenRoomContext.Provider value={room}>{children}</OpenRoomContext.Provider>;
}

export { CHAT_VIEWS, type ChatView };
