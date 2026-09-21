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

import type { MarkName } from "~/ui/glyph.tsx";
import type { ChatChannel, ChatKind } from "~/protocol/index.ts";

/** Whether this room is addressed by who is in it rather than by a name. */
export function isDirect(kind: ChatKind): boolean {
  return kind === "dm" || kind === "group";
}

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
 * The glyph a room is drawn with, in the design system's own vocabulary.
 *
 * `account_tree` for a unit's room rather than `group`, because that is the
 * mark this dashboard already draws the org chart and a unit with — the room
 * belongs to a box on the chart, not to a set of people who happen to be in
 * it.
 */
export function roomMark(kind: ChatKind): MarkName {
  switch (kind) {
    case "public":
      return "tag";
    case "private":
      return "shield";
    case "unit":
      return "account_tree";
    case "dm":
      return "person";
    case "group":
      return "group";
    default:
      // A ROOM THIS BUILD CANNOT CLASSIFY SAYS SO. A fallback to the public
      // mark would draw somebody else's private room with an open tag.
      return "help";
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
      return "A room this build does not know";
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
export function roomTitle(channel: ChatChannel, opts: TitleOptions = {}): string {
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
 * What an unread badge says.
 *
 * THE CAP IS THE ENGINE'S, NOT A NUMBER CHOSEN HERE. It counts to
 * `chat.UnreadLimit` (100) and says on the answer when it stopped there, so
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
 * Whether the badge on a row means anything at all.
 *
 * A COUNT WITH NO READ STATE BEHIND IT IS NOT A ZERO. When the coordination
 * record could not be read the engine zeroes every count and says so with
 * `read_state: false` — and drawing that as "you are caught up everywhere"
 * is the one wrong answer, because it is indistinguishable from the truth
 * until somebody misses a message.
 */
export function badgesAreMeasured(answer: { read_state?: boolean } | null | undefined): boolean {
  return answer?.read_state === true;
}
