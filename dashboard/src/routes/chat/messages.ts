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

import type { ChatMessageView, ChatMessagesAnswer, ChatThreadAnswer } from "~/protocol/index.ts";

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
  if (!tail.loaded || top === 0 || floor > top + 1) {
    // DISJOINT: more messages arrived than one page holds while this tab was
    // away, so the page and what is held do not touch. Rendering both would
    // draw a hole as though it were continuity, which is the one thing a
    // transcript may not do — so the page becomes the tail and the reader
    // pages back into what they had.
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

/** One thread, as a tab holds it. Oldest first: a conversation is read
 *  forwards, and this is the one list here that pages that way. */
export interface ThreadTail {
  replies: ChatMessageView[];
  /** The cursor the next page of replies is asked with, "" at the end. */
  next: string;
  loaded: boolean;
}

export const EMPTY_THREAD: ThreadTail = { replies: [], next: "", loaded: false };

/**
 * Join a page of replies onto a thread.
 *
 * ONE FUNCTION FOR BOTH DIRECTIONS a thread is read in — the re-read a live
 * frame causes, which comes back from the root, and the "older" page a reader
 * asks for, which comes back from the cursor — because a thread is one level
 * deep and therefore short enough that the fresh copy of any row simply wins.
 * The list cannot have a hole in it for the same reason: every page of it
 * starts from the root.
 */
export function mergeReplies(tail: ThreadTail, page: ChatThreadAnswer): ThreadTail {
  const fresh = page.replies ?? [];
  const ids = new Set(fresh.map((view) => view.message.id));
  const held = tail.replies.filter((view) => !ids.has(view.message.id));
  return {
    replies: [...held, ...fresh].sort((a, b) => a.channel_seq - b.channel_seq),
    next: page.next_cursor ?? "",
    loaded: true,
  };
}

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
