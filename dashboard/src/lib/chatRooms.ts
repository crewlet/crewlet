/**
 * The reader's own rooms, read ONCE for every surface in the frame: the
 * Chatlet screen's list of rooms, Home's chat widgets and the sidebar's
 * Chatlet badge are one answer, `chat_channels`.
 *
 * # Why the frame owns it
 *
 * The badge is on every page, so its reading has to outlive every screen, and
 * a screen that read the same question for itself would put the reader's
 * whole room list on the socket twice on the one page that shows both, and
 * let the badge and the list beside it disagree for a re-read's length. So the
 * frame reads it ([ChatRoomsProvider], in `FrameReadings`) and every surface
 * takes the frame's answer ([useChatRooms]).
 *
 * # When it re-reads
 *
 * The frames are evidence, not content (`routes/chatlet/live.ts`): a message
 * in any room moves the list's order, its previews and its counts, none of
 * which a frame carries, so the reading asks again when the tab's chat
 * activity moves, and when this person's own read state moves past what the
 * answer drew. Every frame the engine admits for this reader reaches the
 * socket whatever page is open, so the badge is live everywhere. A re-read is
 * coalesced: at the room's own rate ([LIVE_REFETCH_MS]) while a surface that
 * draws the rooms is mounted, and at [BADGE_REFETCH_MS] while only the badge
 * reads it.
 *
 * # Who it is asked for
 *
 * Every chat question is answered as the seat the session holds, so nothing is
 * asked while the viewer is unanswered or for a credential acting as no seat.
 * For the badge alone, only where the company runs Chatlet and the reader
 * holds it (`lib/services.ts`, `lib/viewer.ts`), as every chat read in the
 * frame is gated (`lib/roomNames.ts`). A surface that draws the rooms CLAIMS
 * the reading ([useChatRooms]) and is asked for on the screen's own terms: the
 * screen is mounted only where the frame admitted the reader, and a viewer
 * read that failed is no answer about their grants, so the screen's question
 * is put and the engine's answer or refusal says what is true.
 */

import {
  createContext,
  createElement,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import type { ChatChannelsAnswer, ChatKind } from "~/protocol/index.ts";
import { LIVE_REFETCH_MS, useChatActivity, useChatRead, useCoalesced } from "./chatLive.ts";
import { useServicesAsk } from "./services.ts";
import { useClient } from "./store-hooks.ts";
import { useQuery, type QueryResult } from "./useQuery.ts";
import { useViewer } from "./viewer.ts";

/**
 * How long live frames pile up before the badge alone re-reads the rooms.
 *
 * TWO SECONDS, because the badge is a figure a reader glances at beside other
 * work rather than a list they are reading, and it is read behind every page:
 * at the room's own third of a second a busy company's chatter would hold one
 * of the socket's four query slots for most of every burst on whatever screen
 * the reader has open. Two seconds keeps the figure inside the time a person
 * takes to notice it moved, and the list itself, once a surface draws it, is
 * re-read at the room's rate again.
 */
export const BADGE_REFETCH_MS = 2_000;

/** Whether this room is addressed by who is in it rather than by a name. */
export function isDirect(kind: ChatKind): boolean {
  return kind === "dm" || kind === "group";
}

/**
 * Whether the badges on an answer mean anything at all.
 *
 * A COUNT WITH NO READ STATE BEHIND IT IS NOT A ZERO. When the coordination
 * record could not be read the engine zeroes every count and says so with
 * `read_state: false`, and drawing that as "you are caught up everywhere" is
 * the one wrong answer, because it is indistinguishable from the truth until
 * somebody misses a message.
 */
export function badgesAreMeasured(answer: { read_state?: boolean } | null | undefined): boolean {
  return answer?.read_state === true;
}

/**
 * What waits on the reader, place by place, as EVERY badge in the frame counts
 * it: the sidebar's Chatlet row, Menu, each room's row and each thread's line.
 *
 * THE ENGINE'S ONE COUNT (`chat_channels`' `waiting`), the same rows Activity
 * marks unread: a direct conversation's unread posts, a message naming the
 * reader or the room, and a reply in a thread they follow, each once, in rooms
 * they have not muted. Counted there so no two badges can differ; whether it
 * was measured at all is [badgesAreMeasured]'s.
 */
export interface ChatWaiting {
  /** Everything that waits, summed. */
  value: number;
  /** A count stopped at the engine's bound, or rooms were left out: a floor. */
  capped: boolean;
  /** What waits in one room: its own posts and its threads' replies. */
  inRoom(channelID: string): number;
  /** Whether a room's count stopped at its window, so it is a floor. */
  floorIn(channelID: string): boolean;
  /** What waits in one thread, by its room and root message. */
  inThread(channelID: string, rootID: string): number;
}

/** The index over an answer's places; null where the counts were not measured. */
export function chatWaiting(answer: ChatChannelsAnswer | null | undefined): ChatWaiting | null {
  if (!answer || !badgesAreMeasured(answer)) return null;
  const rooms = new Map<string, number>();
  const threads = new Map<string, number>();
  const floors = new Set<string>();
  let value = 0;
  for (const place of answer.waiting) {
    if (place.count <= 0) continue;
    value += place.count;
    if (place.capped) floors.add(place.channel_id);
    rooms.set(place.channel_id, (rooms.get(place.channel_id) ?? 0) + place.count);
    if (place.root_id) threads.set(`${place.channel_id} ${place.root_id}`, place.count);
  }
  return {
    value,
    capped: answer.waiting_capped || answer.truncated === true,
    inRoom: (channelID) => rooms.get(channelID) ?? 0,
    floorIn: (channelID) => floors.has(channelID),
    inThread: (channelID, rootID) => threads.get(`${channelID} ${rootID}`) ?? 0,
  };
}

/** What waits on the reader in Chatlet, as the sidebar's badge says it. */
export interface ChatAttention {
  /** Unread direct messages, mentions and replies in threads they follow. */
  value: number;
  /** A count stopped at the engine's bound, or rooms were left out: a floor. */
  capped: boolean;
}

/** The sidebar's figure off [chatWaiting]; null where there is none to draw. */
export function chatAttention(answer: ChatChannelsAnswer | null | undefined): ChatAttention | null {
  return attentionOf(chatWaiting(answer));
}

function attentionOf(waiting: ChatWaiting | null): ChatAttention | null {
  return waiting && waiting.value > 0 ? { value: waiting.value, capped: waiting.capped } : null;
}

/** The figure as a badge writes it: a floor carries a "+". */
export function attentionFigure(attention: ChatAttention): string {
  return attention.capped ? `${attention.value}+` : String(attention.value);
}

type Reading = QueryResult<ChatChannelsAnswer>;

const ReadingContext = createContext<Reading | null>(null);

/** Claim the reading for a surface that draws the rooms; returns the release. */
const ClaimContext = createContext<(() => () => void) | null>(null);

/**
 * Read the rooms once, for everything under it. Inside the viewer's provider,
 * whose answer it is asked as (see the file's doc).
 */
export function ChatRoomsProvider({ children }: { children: ReactNode }) {
  const viewer = useViewer();
  const ask = useServicesAsk();
  const [claims, setClaims] = useState(0);
  const claim = useCallback(() => {
    setClaims((n) => n + 1);
    return () => setClaims((n) => n - 1);
  }, []);
  const drawn = claims > 0;
  const asSeat = !viewer.asking && !viewer.unbound;
  const enabled = asSeat && (drawn || (ask.chat && viewer.chatlet));
  const reading = useQuery("chat_channels", {}, { enabled });

  // THE ANSWER SEEDS THIS TAB'S READ STATE, which nothing else would until a
  // flush landed somewhere: the live channel pushes it only when it moves.
  // Through the store's own ordering, so a push that arrived while this answer
  // was in flight, newer by its revision, is kept over it.
  const { store } = useClient();
  useEffect(() => store.applyChatRead(reading.data?.read), [store, reading.data]);

  // A LIVE FRAME IN ANY ROOM MOVES THE LIST, and so the reading re-reads, at
  // the rate of whoever is reading it (see the file's doc).
  const { refetch } = reading;
  const window = drawn ? LIVE_REFETCH_MS : BADGE_REFETCH_MS;
  useCoalesced(useChatActivity(), refetch, window);

  // AND SO DOES THIS PERSON'S OWN READ STATE: every count and every mute in
  // the answer is answered from it. KEYED ON THE RECORD'S REVISION, never on
  // the slice: the seed above re-applies the answer's own state, so a key on
  // the slice would re-read on every answer of its own. A revision the answer
  // already carries is one it has drawn.
  const readRevision = useChatRead()?.revision ?? 0;
  const drawnRevision = reading.data?.read?.revision ?? 0;
  useCoalesced(
    readRevision,
    () => {
      if (readRevision > drawnRevision) refetch();
    },
    window,
  );

  return createElement(
    ClaimContext.Provider,
    { value: claim },
    createElement(ReadingContext.Provider, { value: reading }, children),
  );
}

/**
 * The frame's reading, for a surface that DRAWS the rooms: the Chatlet
 * screen's list, Home's chat widgets. Mounting it claims the reading, so it is
 * asked on the surface's terms and re-read at the room's own rate while the
 * surface is on screen (see the file's doc).
 *
 * THROWS OUTSIDE THE FRAME rather than asking for itself, as
 * `useInboxCounts` does: a fallback read here is the per-surface read this
 * module exists to remove, back again the day a surface is mounted somewhere
 * the frame is not.
 */
export function useChatRooms(): Reading {
  const claim = useContext(ClaimContext);
  const reading = useContext(ReadingContext);
  if (claim === null || reading === null) {
    throw new Error(
      "useChatRooms() outside a ChatRoomsProvider: the frame mounts one (FrameReadings, app/Shell.tsx), and a suite that mounts a screen without the frame mounts one around it",
    );
  }
  useEffect(() => claim(), [claim]);
  return reading;
}

/**
 * What waits on the reader, off the frame's reading WITHOUT claiming it: a
 * badge is drawn wherever the reading is, at its own rate ([chatWaiting]).
 */
export function useChatWaiting(): ChatWaiting | null {
  const reading = useContext(ReadingContext);
  if (reading === null) {
    throw new Error(
      "useChatWaiting() outside a ChatRoomsProvider: the frame mounts one (FrameReadings, app/Shell.tsx)",
    );
  }
  const data = reading.data;
  return useMemo(() => chatWaiting(data), [data]);
}

/** The sidebar's figure off the frame's reading ([chatAttention]). */
export function useChatAttention(): ChatAttention | null {
  const waiting = useChatWaiting();
  return useMemo(() => attentionOf(waiting), [waiting]);
}
