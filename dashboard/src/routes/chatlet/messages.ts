/**
 * The arithmetic a transcript is: how pages join, and which rows are on
 * screen.
 *
 * SEPARATED FROM THE COMPONENT because neither half can be exercised through
 * one. jsdom computes no layout, so a windowing rule asserted by rendering is
 * a rule asserted against every height being zero; and the paging rule's whole
 * value is at a boundary a screen shows as one uninterrupted list, where the
 * failure — a message drawn twice, or a hole drawn as continuity — looks
 * exactly like the correct answer until somebody counts.
 *
 * The rules, once:
 *
 *   - A ROOM IS HELD NEWEST FIRST, contiguous, as one run. The engine's
 *     transcript pages backwards from the top of the room on its own cursor,
 *     so a tail is "the newest N messages" and an older page extends it
 *     downwards.
 *   - A MESSAGE IS ITS ID. A refetch brings the same rows back with edits,
 *     reactions and tombstones applied, so rows join by identity and the
 *     fresher copy wins — never by position, which would draw one message
 *     twice the first time a page boundary moved.
 *   - WITHIN THE RANGE A PAGE COVERS, THE PAGE IS THE TRUTH. A row this tab
 *     holds inside that range and the page does not carry has been erased, and
 *     keeping it would render a message the company has deleted.
 */

import type {
  ChatChange,
  ChatMessageView,
  ChatMessagesAnswer,
  ChatThreadAnswer,
  LogPosition,
} from "~/protocol/index.ts";
import { comparePositions } from "~/protocol/session.ts";
import type { PagesReached } from "~/lib/usePaged.ts";

/** One room's transcript, as a tab holds it. */
export interface Tail {
  /** Newest first, contiguous by construction. */
  messages: ChatMessageView[];
  /**
   * The cursor the next OLDER page is asked with, or "" when the start of the
   * room has been reached. It is the engine's own `next_cursor`, carried
   * unchanged — the room's per-message number rather than an offset or an
   * instant, which is what makes it stable while people are still talking.
   */
  older: string;
  /** Whether any page has been merged. Told apart from an empty room, which
   *  is a real answer a screen says something different about. */
  loaded: boolean;
  /**
   * How many rows the NEWEST page could not decode — a message a newer peer
   * wrote.
   *
   * From the newest page only, and deliberately not accumulated: it is a fact
   * about what this node's build can render right now, which a screen states
   * once. Summed across the pages a reader happened to open it would climb
   * with scrolling and describe nothing.
   */
  unreadable: number;
}

export const EMPTY_TAIL: Tail = { messages: [], older: "", loaded: false, unreadable: 0 };

/** The lowest per-room number a page covers, or 0 for an empty page. */
function floorOf(messages: readonly ChatMessageView[]): number {
  return messages[messages.length - 1]?.channel_seq ?? 0;
}

/** Newest first, by the room's own sequence — a total order, because the
 *  applier mints it from log order and it is unique within a room. */
function newestFirst(messages: ChatMessageView[]): ChatMessageView[] {
  return [...messages].sort((a, b) => b.channel_seq - a.channel_seq);
}

/**
 * Join a page read from the TOP of the room onto what a tab already holds.
 *
 * This is the read every live frame causes: a frame is evidence that something
 * landed and carries no body, so the repair for any change in the open room is
 * to re-read its newest page. Which means this runs constantly, and has to
 * leave a reader who has paged back through an afternoon exactly where they
 * were.
 */
export function mergeNewest(tail: Tail, page: ChatMessagesAnswer): Tail {
  const fresh = page.messages ?? [];
  if (fresh.length === 0) {
    // AN EMPTY NEWEST PAGE IS AN EMPTY ROOM, and on a loaded tail that is not
    // nothing happening — it is every message this tab holds having been
    // erased or pruned out from under it. Keeping them would draw a
    // conversation the company has deleted.
    return {
      messages: [],
      older: page.next_cursor ?? "",
      loaded: true,
      unreadable: page.unreadable ?? 0,
    };
  }
  const floor = floorOf(fresh);
  const top = tail.messages[0]?.channel_seq ?? 0;
  if (!tail.loaded || top === 0 || floor > top + 1 || !page.next_cursor) {
    // DISJOINT: more messages arrived than one page holds while this tab was
    // away, so the page and what is held do not touch. Rendering both would
    // draw a hole as though it were continuity, which is the one thing a
    // transcript may not do — so the page becomes the tail and the reader
    // pages back into what they had.
    //
    // OR THE WHOLE ROOM: a newest page with nothing below it covers every
    // message the room has, so a row this tab holds below it has been erased
    // or pruned, and the page is the truth over all of it.
    return {
      messages: newestFirst(fresh),
      older: page.next_cursor ?? "",
      loaded: true,
      unreadable: page.unreadable ?? 0,
    };
  }
  const below = tail.messages.filter((view) => view.channel_seq < floor);
  return {
    messages: newestFirst([...fresh, ...below]),
    // NOT THE PAGE'S CURSOR. It points just below the newest page, and this
    // tab already holds rows further down — taking it would make the next
    // "older" read re-fetch the pages already on screen.
    older: tail.older,
    loaded: true,
    unreadable: page.unreadable ?? 0,
  };
}

/**
 * Extend a tail downwards with a page asked for on its own `older` cursor.
 *
 * The join is by id rather than by appending, which is the boundary case the
 * cursor is built to avoid and the one that still has to be right: the engine
 * pages STRICTLY BELOW the cursor, so a duplicate here means the cursor and
 * the rows disagree — and a list that appends blindly would draw that message
 * twice rather than once.
 */
export function mergeOlder(tail: Tail, page: ChatMessagesAnswer): Tail {
  const older = page.messages ?? [];
  const seen = new Set(older.map((view) => view.message.id));
  const held = tail.messages.filter((view) => !seen.has(view.message.id));
  return {
    messages: newestFirst([...held, ...older]),
    older: page.next_cursor ?? "",
    loaded: true,
    // An older page says nothing about the newest one, which is what this
    // field is about.
    unreadable: tail.unreadable,
  };
}

/**
 * How far one live frame reaches into the messages a screen already holds.
 *
 *   - `none`: it changed no message that was already there. A post adds one
 *     at the end of its room or its thread, and a room's own records — its
 *     creation, its settings, its members — change no message at all.
 *   - `message`: it changed the one message it names — an edit, a delete, a
 *     reaction.
 *   - `room`: it may have changed any of them. An erase or a prune names no
 *     message, and an op this build does not know is one whose reach it
 *     cannot say, so it is read as the widest.
 */
export type Reach = "none" | "message" | "room";

export function reachOf(frame: ChatChange): Reach {
  switch (frame.op) {
    case "post":
    case "create":
    case "patch":
    case "members":
      return "none";
    case "edit":
    case "delete":
    case "react":
      return frame.message_id ? "message" : "room";
    default:
      return "room";
  }
}

/**
 * What a frame asks a room's held rows to be read again for: the message it
 * changed, or every row (`message: null`), and where on the log it sits.
 */
export interface Touch {
  /** A read derived through this position carries what the frame did. Null
   *  where nothing says — frames this tab let go of. */
  position: LogPosition | null;
  message: string | null;
}

/** The touches in a run of frames, or one over every row for `null` — frames
 *  were let go of, and what they changed is unknown. */
export function touchesOf(frames: readonly ChatChange[] | null): Touch[] {
  if (frames === null) return [{ position: null, message: null }];
  const out: Touch[] = [];
  for (const frame of frames) {
    const reach = reachOf(frame);
    if (reach === "none") continue;
    out.push({
      position: frame.position ?? null,
      message: reach === "message" ? (frame.message_id ?? null) : null,
    });
  }
  return out;
}

/**
 * Whether a page derived through `served` answers for the frame at `touch`.
 *
 * BY THE LOG, because a page asked after a frame arrived can still be one a
 * node that had not applied it answered — or one that was already on its way
 * when the frame came. A page that says nothing of where it was derived
 * through is taken at its word, there being nothing better to compare.
 */
export function answersFor(served: LogPosition | null | undefined, touch: Touch): boolean {
  return !served || !touch.position || comparePositions(served, touch.position) >= 0;
}

/** One range of a room to read again, and the messages it answers for. */
export interface Reread {
  /** The page asked from here: the newest `limit` messages strictly below it,
   *  which is the transcript's own cursor (`next_cursor`). */
  cursor: string;
  messages: string[];
}

/**
 * The ranges BELOW the newest page to read again for these touches, once the
 * newest page has been read and merged into `tail`.
 *
 * THE NEWEST PAGE ANSWERS FOR EVERYTHING IT CARRIES, so what is left is a
 * touched row it does not reach: one the reader paged back to, or one a burst
 * of new messages pushed off its foot — which is why this runs after the
 * newest page lands rather than when the frame does. A touch over every row
 * (an erase, a prune, frames let go of) reaches every held row below it.
 *
 * EACH RANGE IS THE PAGE THAT ENDS AT A TOUCHED MESSAGE, asked from just
 * above it, and it answers for every other touched message within `limit`
 * of it: a room's per-message numbers are distinct integers, so at most
 * `limit` messages lie in those `limit` numbers and the page carries all of
 * them. A touched row further down starts the next range.
 */
export function rangesToReread(
  tail: Tail,
  newest: ChatMessagesAnswer,
  touches: readonly Touch[],
  limit: number,
): Reread[] {
  const floor = floorOf(newest.messages ?? []);
  const every = touches.some((touch) => touch.message === null);
  const named = new Set(touches.map((touch) => touch.message));
  const out: Reread[] = [];
  let reach = Infinity;
  // NEWEST FIRST, as the tail is held.
  for (const view of tail.messages) {
    if (view.channel_seq >= floor) continue;
    if (!every && !named.has(view.message.id)) continue;
    if (view.channel_seq < reach) {
      out.push({ cursor: String(view.channel_seq + 1), messages: [] });
      reach = view.channel_seq + 1 - limit;
    }
    out[out.length - 1]!.messages.push(view.message.id);
  }
  return out;
}

/**
 * Join a page read from `cursor` back into what a tab holds.
 *
 * WITHIN ITS RANGE THE PAGE IS THE TRUTH: from just below the cursor down to
 * its own last row — or to the start of the room, when nothing is below it —
 * a held row it does not carry has been erased or pruned and leaves, and
 * every row it carries is the fresher copy. Outside the range nothing moves.
 *
 * ONLY WHILE IT STILL MEETS WHAT IS HELD. A newest page that did not touch
 * the tail replaced it while this range was out (`mergeNewest`), and joining
 * an older range under that would draw a hole as continuity — so the range
 * is dropped, and the reader pages back into it as they would anything else.
 * And a range that reaches the foot of what is held moves the pager: the
 * next older page now begins where this one ended.
 */
export function mergeRange(tail: Tail, cursor: string, page: ChatMessagesAnswer): Tail {
  const top = Number(cursor);
  const fresh = page.messages ?? [];
  const lowest = tail.messages[tail.messages.length - 1]?.channel_seq;
  if (lowest === undefined || lowest > top) return tail;
  // A cursor with no row beside it cannot be: the engine names one from the
  // row past the page. Read as reaching nothing rather than as the bottom.
  if (fresh.length === 0 && page.next_cursor) return tail;
  const bottom = page.next_cursor ? floorOf(fresh) : -Infinity;
  const kept = tail.messages.filter((view) => view.channel_seq >= top || view.channel_seq < bottom);
  return {
    ...tail,
    messages: newestFirst([...kept, ...fresh]),
    older: bottom <= lowest ? (page.next_cursor ?? "") : tail.older,
  };
}

/**
 * Which of a thread's pages a run of frames reached, asked of each page as
 * `usePaged` holds it: every page, for frames let go of (`null`) and for a
 * frame whose reach is the whole room.
 *
 * ONLY WHAT A FRAME NAMES. A thread is bounded in who takes part and not in
 * how much they say, so re-asking every page it holds on every frame in the
 * room paid a query per page for messages in other threads. What reaches it:
 *
 *   - a change to the ROOT, which comes back with the first page;
 *   - a change to a reply it holds, on whichever page carries it;
 *   - a NEW REPLY, at the end of the thread — the last page, which is also
 *     the freshest word on who has spoken here. A post whose frame carries
 *     no routing snapshot (`channel_kind` absent: the engine narrating the
 *     room) does not say which thread it is in, so it is read as maybe this
 *     one.
 */
export function threadPagesTouched(
  rootID: string,
  frames: readonly ChatChange[] | null,
): PagesReached<ChatThreadAnswer> {
  return (page, place) => {
    if (frames === null) return true;
    return frames.some((frame) => {
      const reach = reachOf(frame);
      if (reach === "room") return true;
      if (frame.op === "post") {
        return place.last && (frame.thread_root === rootID || !frame.channel_kind);
      }
      if (reach !== "message") return false;
      if (frame.message_id === rootID) return place.first;
      return (page.replies ?? []).some((view) => view.message.id === frame.message_id);
    });
  };
}

/**
 * How much of a room one JUMP reads per page: a jump to a date, to the very
 * beginning, to the New line or to a linked message further back than what
 * is held.
 *
 * FIVE HUNDRED, the engine's own ceiling (`chatlet.MaxLimit`, what it clamps a
 * larger ask to). Opening a room reads the screen's page (`page.ts`'s fifty),
 * because nobody asked to read further; a jump is the reader asking to go
 * somewhere further back, and reading there fifty rows at a time would be ten
 * round trips for every five hundred messages crossed.
 */
export const JUMP_PAGE = 500;

/** Which rows a scroller is actually over, and what stands in for the rest. */
export interface RowWindow {
  /** First index to render. */
  start: number;
  /** One past the last index to render. */
  end: number;
  /** The height of everything before `start`, as a spacer. */
  padTop: number;
  /** The height of everything from `end` on. */
  padBottom: number;
}

/**
 * How much of the list is drawn above and below what the reader can see.
 *
 * A SCREEN AND A HALF EITHER WAY, in pixels rather than rows because rows here
 * are not one height: a pasted stack trace and an "ack" differ by two orders
 * of magnitude, and a row count that covers the first covers thousands of the
 * second. It buys the two things overscan is for — a wheel flick that outruns
 * a render frame, and a find-in-page that reaches a little past the fold —
 * without rendering an afternoon of conversation to show a minute of it.
 */
export const OVERSCAN_PX = 600;

/**
 * The window of rows to render.
 *
 * PURE OVER NUMBERS. The heights come from whatever the screen measured, with
 * an estimate standing in for what has not been drawn yet, so this answers the
 * same way in a browser and in a suite — and the padding it reports is the
 * exact height of what it left out, which is what keeps the scrollbar honest
 * about a conversation only partly in the DOM.
 *
 * A VIEWPORT OF ZERO IS NOT AN EMPTY WINDOW. It means nothing has been
 * measured yet — the first render, and every render under jsdom — and the
 * honest answer there is the whole list: the rows a tab holds are bounded by
 * the pages it asked for, and the first measurement narrows them.
 */
export function windowFor(opts: {
  count: number;
  /** The height of one row, measured or estimated. */
  heightOf: (index: number) => number;
  scrollTop: number;
  viewport: number;
  overscan?: number;
}): RowWindow {
  const { count, heightOf, scrollTop, viewport } = opts;
  const overscan = opts.overscan ?? OVERSCAN_PX;
  if (count <= 0) return { start: 0, end: 0, padTop: 0, padBottom: 0 };
  if (viewport <= 0) return { start: 0, end: count, padTop: 0, padBottom: 0 };

  const from = scrollTop - overscan;
  const to = scrollTop + viewport + overscan;
  let offset = 0;
  let start = -1;
  let end = count;
  let padTop = 0;
  for (let i = 0; i < count; i++) {
    const height = heightOf(i);
    const bottom = offset + height;
    if (start < 0 && bottom > from) {
      start = i;
      padTop = offset;
    }
    if (start >= 0 && offset >= to) {
      end = i;
      break;
    }
    offset = bottom;
  }
  if (start < 0) {
    // Scrolled past the end of everything — which happens for one frame while
    // rows are being replaced by shorter ones. The last row is the honest
    // answer, not an empty list.
    start = count - 1;
    padTop = offset - heightOf(count - 1);
    end = count;
  }
  let padBottom = 0;
  for (let i = end; i < count; i++) padBottom += heightOf(i);
  return { start, end, padTop, padBottom };
}
