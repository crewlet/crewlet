/**
 * What a room is called, and what a badge says.
 *
 * ONE ANSWER FOR FIVE PLACES. A room appears in the rail, in its own header,
 * in a search result, in the mention feed and in a link somebody pasted, and a
 * direct conversation is the case that breaks a second implementation: it has
 * NO NAME at all — its identity is derived from the sorted handles of the
 * people in it — so every surface has to build the same title from the same
 * participants, minus the reader themself, or one screen calls a conversation
 * "Ada, Bo" and the next calls it "Bo, you".
 */

import type { GlyphName } from "@crewlethq/icons/glyphs";
import { isDirect, roomWaiting } from "~/lib/chatRooms.ts";
import type { ChatChannel, ChatChannelSummary, ChatKind } from "~/protocol/index.ts";

/**
 * Whether this build can classify the room at all.
 *
 * A NEWER PEER MAY WRITE A KIND THIS BUILD DOES NOT KNOW, and the engine
 * refuses every write to such a room rather than guessing — because a kind
 * read two-valued falls out as "not private", which would make somebody
 * else's room writable by anyone. The screen follows: it draws the room,
 * because reading it was allowed, and offers nothing that writes.
 */
export function knownKind(kind: ChatKind): boolean {
  return (
    kind === "public" || kind === "private" || kind === "unit" || kind === "dm" || kind === "group"
  );
}

/**
 * Why a room of a kind this build cannot classify takes nothing from here:
 * the composer's reason and the membership note alike, so the one fact is
 * said one way wherever it holds a control.
 */
export const NEWER_ROOM =
  "This room was made by a newer version of the engine, so it can’t be changed from this node until it’s upgraded.";

/**
 * The glyph a room is drawn with, in the design system's own vocabulary.
 *
 * `network` for a unit's room rather than `users`, because that is the mark
 * this dashboard already draws the org chart and a unit with — the room
 * belongs to a box on the chart, not to a set of people who happen to be in
 * it.
 */
export function roomMark(kind: ChatKind): GlyphName {
  switch (kind) {
    case "public":
      return "hash";
    case "private":
      // THE LOCK, which is what a private room is in every list a reader
      // meets one in: closed to whoever was not added.
      return "lock";
    case "unit":
      return "network";
    case "dm":
      return "user";
    case "group":
      return "users";
    default:
      // A ROOM THIS BUILD CANNOT CLASSIFY SAYS SO. A fallback to the public
      // mark would draw somebody else's private room with an open hash.
      return "circle-question-mark";
  }
}

/** What kind of room this is, in a word a person reads. */
export function roomKindLabel(kind: ChatKind): string {
  switch (kind) {
    case "public":
      return "Public channel";
    case "private":
      return "Private channel";
    case "unit":
      return "Unit room";
    case "dm":
      return "Direct message";
    case "group":
      return "Group message";
    default:
      return "A newer kind of room";
  }
}

export interface TitleOptions {
  /** Who is in a direct conversation, from the room's own summary. */
  participants?: readonly string[];
  /** The reader's own handle, which is dropped from a direct conversation's
   *  title: nobody reads their own name as the name of a conversation. */
  viewer?: string;
  /** A handle to a display name, where the roster is in hand. */
  nameOf?: (handle: string) => string;
}

/**
 * The room's title.
 *
 * WITHOUT THE `#`. A named room is drawn with one beside its glyph, and a
 * title carrying it would put the sigil into a page title, a browser tab and
 * a search result where it reads as punctuation somebody typed.
 */
export function roomTitle(
  channel: Pick<ChatChannel, "id" | "kind" | "name">,
  opts: TitleOptions = {},
): string {
  const { participants = [], viewer = "", nameOf } = opts;
  if (!isDirect(channel.kind)) {
    // A named room is its name. The name is IMMUTABLE in the value layer —
    // there is no rename — so this is stable for the life of the room.
    return channel.name || channel.id;
  }
  const others = participants.filter((handle) => handle && handle !== viewer);
  if (others.length === 0) {
    // A conversation with nobody else in it is one somebody opened with
    // themself, which the engine allows and which has to be called something.
    return "Just you";
  }
  return others.map((handle) => nameOf?.(handle) || handle).join(", ");
}

/**
 * What a sentence calls a room: `#name` for a channel, public or private;
 * its team's name for a team's room, which is never a channel's `#`; and the
 * title as it stands for a conversation, or for a kind this build cannot
 * classify.
 *
 * WORDS ALONE, NEVER BESIDE THE GLYPH. Where the room's glyph ([roomMark])
 * is drawn, the glyph is the room's sigil and the words beside it are the
 * bare [roomTitle]: the two together drew a channel's `#` twice, once as the
 * glyph and once as text, in the room's intro and on Home's widgets.
 */
export function roomWhere(kind: ChatKind, title: string): string {
  return kind === "public" || kind === "private" ? `#${title}` : title;
}

/**
 * What an unread badge says.
 *
 * THE CAP IS THE ENGINE'S, NOT A NUMBER CHOSEN HERE. It counts to
 * `chatlet.UnreadLimit` (100) and says on the answer when it stopped there, so
 * "99+" is a rendering of a stated fact rather than a client deciding that a
 * hundred is too many to print. A client that inferred it from the number
 * would render "100" for a room with exactly a hundred unread and for one with
 * four thousand.
 */
export function unreadLabel(unread: number, capped: boolean | undefined): string {
  if (unread <= 0) return "";
  return capped ? "99+" : String(unread);
}

/**
 * The Rooms panel's sections, in the order it draws them: the rooms the
 * reader starred, the named channels, the team rooms the org chart keeps,
 * and the conversations named by who is in them.
 */
export type RoomSection = "starred" | "channels" | "teams" | "direct";

export const ROOM_SECTIONS: readonly RoomSection[] = ["starred", "channels", "teams", "direct"];

/**
 * Which section a room is listed under.
 *
 * A STARRED ROOM IS LISTED ONCE, under Starred, whatever its kind: a room in
 * two sections is two rows with one badge each, and the reader counts what
 * waits for them twice. A kind this build cannot classify is a channel, the
 * section that promises the least about who is in it.
 */
export function roomSection(kind: ChatKind, starred: boolean): RoomSection {
  if (starred) return "starred";
  if (isDirect(kind)) return "direct";
  if (kind === "unit") return "teams";
  return "channels";
}

/**
 * What waits on the reader in a room, as the accent badge counts it
 * (`lib/chatRooms.ts`'s `roomWaiting`, the sidebar's rule), or nothing where
 * nothing was measured: a rail with no read state behind it zeroes every
 * count, and a zero drawn from that is a claim that nothing waits.
 */
export function waitingIn(room: ChatChannelSummary, measured: boolean): number {
  return measured ? roomWaiting(room) : 0;
}

/**
 * Whether a room has unread messages the reader has not silenced, which is
 * what sets its row in the primary ink and the medium weight.
 */
export function unreadIn(room: ChatChannelSummary, measured: boolean): boolean {
  return measured && !room.muted && room.unread > 0;
}
