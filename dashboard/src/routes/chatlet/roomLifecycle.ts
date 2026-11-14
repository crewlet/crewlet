/**
 * A room's lifecycle and its record: archiving it or opening it again, and an
 * administrator's three destructions of what it said (how long it keeps
 * messages, deleting everything before a day, erasing named messages).
 *
 * # Who may, read the engine's way
 *
 * ARCHIVING CLOSES THE ROOM FOR EVERYBODY IN IT, so it is not any member's:
 * the room's creator, the lead of the unit a unit's room belongs to or of a
 * unit above it, or an admin (authz's `chat.channel.archive`, a tidy). The
 * engine decides it on the stored room; [archiveHold] reads the same three
 * facts off the room's own answer and the org push, so the control is held
 * with the engine's sentence rather than offered and refused.
 *
 * THE THREE DESTRUCTIONS ARE AN ADMINISTRATOR'S (`chat.channel.retention`,
 * `chat.prune`, `chat.erase`), each irreversible and confirmed by repeating
 * what it destroys in `?confirm=`, which the builders here spell exactly as
 * the engine checks them, so the confirmation the person gave in the dialog is
 * the one the request carries.
 *
 * # Pure
 *
 * Every rule and every request is a function of values, so it is held by a
 * test without a browser; the dialogs (`RoomAdmin.tsx`) only draw them.
 */

import type { KeyedRequest } from "~/lib/keyedWrite.ts";
import { pathSegment } from "~/protocol/index.ts";
import type { ChatChannel } from "~/protocol/index.ts";
import { isDirect } from "~/lib/chatRooms.ts";

/**
 * Who archives a room and opens it again, in the engine's words
 * (internal/chatlet's `archivers`), so the hold and the refusal say one thing.
 */
export const ARCHIVERS =
  "whoever created it, the lead of its team or of a team above it, or an admin";

/** Why this reader may not archive the room, or open it again. */
export const ARCHIVE_HELD = `Only ${ARCHIVERS} can archive this room or reopen it.`;

/** What a reader is, for [archiveHold]. */
export interface Archiver {
  /** The seat the reader acts as, or "" for a credential bound to none. */
  handle: string;
  /** Whether the reader holds Admin. */
  admin: boolean;
  /** The units the reader leads, and every unit inside them (`lib/leadScope.ts`). */
  leads: ReadonlyMap<string, string>;
}

/**
 * Why the reader may not archive the room or reopen it, or "" where they may.
 *
 * A DIRECT CONVERSATION IS ITS PEOPLE and has no creator to close it for the
 * others, so it is never offered at all (the caller draws nothing); this
 * answers for a named room.
 */
export function archiveHold(channel: ChatChannel, reader: Archiver): string {
  if (reader.admin) return "";
  if (reader.handle !== "" && channel.created_by === reader.handle) return "";
  if (channel.kind === "unit" && channel.unit && reader.leads.has(channel.unit)) return "";
  return ARCHIVE_HELD;
}

/** Whether a room offers archiving at all: a named room, never a conversation. */
export function archivable(channel: ChatChannel): boolean {
  return !isDirect(channel.kind);
}

/** The arguments `update_channel` archives a room with, or opens it again with. */
export function archiveArgs(channelID: string, archived: boolean) {
  return { channel: channelID, archived };
}

/** A room's own write path, under `/chatlet/channels/{id}/`. */
function roomPath(channelID: string, verb: "retention" | "prune" | "erase"): string {
  return `/chatlet/channels/${pathSegment(channelID)}/${verb}`;
}

/**
 * How a room's horizon is confirmed: its number of days, `0` for ever, or
 * `company` for the company's own (internal/chatlet's `HorizonConfirmation`).
 */
export function horizonConfirmation(days: number | null): string {
  return days === null ? "company" : String(days);
}

/**
 * Setting how long a room keeps its messages: a number of days, `0` for ever,
 * or null for the company's horizon.
 *
 * THE BODY ALWAYS NAMES IT, a null included: an absent one would read as the
 * company's horizon, and a request that said nothing would then destroy the
 * past of a room somebody had set to keep for ever.
 */
export function retentionRequest(
  channelID: string,
  days: number | null,
): Omit<KeyedRequest, "key"> {
  return {
    method: "PUT",
    path: roomPath(channelID, "retention"),
    query: { confirm: horizonConfirmation(days) },
    body: { retention_days: days },
  };
}

/** Deleting everything a room said before `cutoff`, an RFC 3339 instant, repeated as its confirmation. */
export function pruneRequest(channelID: string, cutoff: string): Omit<KeyedRequest, "key"> {
  return {
    method: "POST",
    path: roomPath(channelID, "prune"),
    query: { cutoff, confirm: cutoff },
  };
}

/**
 * Erasing named messages for good, with the reason the erase record keeps
 * about them: the count, once duplicates are folded, is its confirmation.
 */
export function eraseRequest(
  channelID: string,
  messageIDs: readonly string[],
  reason: string,
): Omit<KeyedRequest, "key"> {
  const ids = [...new Set(messageIDs)];
  return {
    method: "POST",
    path: roomPath(channelID, "erase"),
    query: { confirm: String(ids.length) },
    body: { message_ids: ids, reason: reason.trim() },
  };
}

/** How long a room keeps what is said, in words. */
export function keptFor(days: number | null | undefined): string {
  // ABSENT IS THE COMPANY'S ANSWER and zero is for ever: the room's own field
  // is nullable precisely so the two are not one value.
  if (days === 0) return "For ever";
  if (typeof days === "number") return `${days.toLocaleString()} days`;
  return "As long as the company default";
}

/** The longest horizon a person types here: a century, which a reader who means more means for ever. */
export const MAX_KEEP_DAYS = 36500;

/**
 * A typed number of days as a horizon, or the sentence that says why it is
 * none: a whole number from 1 (0 is for ever, which is its own choice).
 */
export function keepDays(text: string): { days: number } | { problem: string } {
  const trimmed = text.trim();
  if (!/^\d+$/.test(trimmed)) return { problem: "Enter a whole number of days." };
  const days = Number(trimmed);
  if (days < 1) return { problem: "Keep messages at least 1 day, or choose For ever." };
  if (days > MAX_KEEP_DAYS) {
    return {
      problem: `At most ${MAX_KEEP_DAYS.toLocaleString()} days. For longer, choose For ever.`,
    };
  }
  return { days };
}
