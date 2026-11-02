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

import type { ChatChannelSummary, ChatChannelsAnswer, ChatKind } from "~/protocol/index.ts";
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
 * What waits on the reader in ONE room, as every badge that counts it says:
 * in a named room, the unread messages that name them or the whole room; in a
 * direct conversation, every unread message, since each of them is addressed
 * to the reader; in a muted room, nothing, which is what muting it asked for.
 *
 * ONE RULE for the sidebar's Chatlet badge ([chatAttention]) and every row
 * and switcher in Chatlet that badges a room, so no two can count a room
 * differently. Whether the counts were measured at all is the caller's to ask
 * ([badgesAreMeasured]), of the answer the row came from.
 */
export function roomWaiting(row: ChatChannelSummary): number {
  if (row.muted) return 0;
  return isDirect(row.channel.kind) ? row.unread : row.mentions;
}

/** What waits on the reader in Chatlet, as the sidebar's badge says it. */
export interface ChatAttention {
  /** Unread messages naming the reader, plus unread direct messages. */
  value: number;
  /** A count stopped at the engine's bound, or rooms were left out: a floor. */
  capped: boolean;
}

/**
 * What waits on the reader across their rooms, each counted by [roomWaiting].
 *
 * NULL WHERE THERE IS NO FIGURE TO DRAW: before the answer, on an answer whose
 * counts were not measured ([badgesAreMeasured]), and when nothing waits. A
 * floor where the engine said one: a room counted to its bound
 * (`unread_capped`), or a list cut short (`truncated`) that may hold more.
 */
export function chatAttention(answer: ChatChannelsAnswer | null | undefined): ChatAttention | null {
  if (!answer || !badgesAreMeasured(answer)) return null;
  let value = 0;
  let capped = answer.truncated === true;
  for (const row of answer.channels) {
    const waiting = roomWaiting(row);
    if (waiting <= 0) continue;
    value += waiting;
    if (row.unread_capped) capped = true;
  }
  return value > 0 ? { value, capped } : null;
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
 * What waits on the reader in Chatlet, off the frame's reading, for the
 * sidebar's badge; null where there is no figure ([chatAttention]).
 */
export function useChatAttention(): ChatAttention | null {
  const reading = useContext(ReadingContext);
  if (reading === null) {
    throw new Error(
      "useChatAttention() outside a ChatRoomsProvider: the frame mounts one (FrameReadings, app/Shell.tsx)",
    );
  }
  const data = reading.data;
  return useMemo(() => chatAttention(data), [data]);
}
