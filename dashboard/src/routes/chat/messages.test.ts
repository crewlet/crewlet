/**
 * How a transcript joins its pages, and what it draws.
 *
 * The failures these protect against all look like a working screen: a message
 * rendered twice at a page boundary, a hole between two pages drawn as though
 * the conversation were continuous, an erased message still on screen, and a
 * scrollbar that claims a length the list does not have.
 */

import { describe, expect, test } from "vitest";

import {
  EMPTY_TAIL,
  EMPTY_THREAD,
  mergeNewest,
  mergeOlder,
  mergeReplies,
  windowFor,
  type Tail,
} from "./messages.ts";
import type { ChatMessagesAnswer, ChatMessageView, ChatThreadAnswer } from "~/protocol/index.ts";

function view(seq: number, over: Partial<ChatMessageView["message"]> = {}): ChatMessageView {
  return {
    message: {
      v: 1,
      id: `m-${seq}`,
      channel_id: "room-1",
      author: "ada",
      author_kind: "human",
      body: `message ${seq}`,
      created_at: "2026-01-01T00:00:00Z",
      ...over,
    },
    channel_seq: seq,
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
  };
}

/** A page as the engine answers it: newest first, with the cursor for the
 *  page below it. */
function page(views: ChatMessageView[], next = ""): ChatMessagesAnswer {
  return {
    read_level: "session",
    complete: true,
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 999 },
    channel_id: "room-1",
    messages: views,
    next_cursor: next,
  };
}

const seqs = (tail: Tail) => tail.messages.map((m) => m.channel_seq);

describe("paging a room", () => {
  test("an older page extends the tail and repeats nothing across the boundary", () => {
    // The cursor is the room's own per-message number and the engine pages
    // STRICTLY below it, so a row arriving on both sides of a boundary means
    // the two disagree — and a list that appended blindly would draw that
    // message twice.
    let tail = mergeNewest(EMPTY_TAIL, page([view(10), view(9), view(8)], "8"));
    tail = mergeOlder(tail, page([view(8), view(7), view(6)], "6"));
    expect(seqs(tail)).toEqual([10, 9, 8, 7, 6]);
    expect(tail.messages.map((m) => m.message.id)).toHaveLength(
      new Set(tail.messages.map((m) => m.message.id)).size,
    );
    expect(tail.older).toBe("6");
  });

  test("the start of a room is a cursor that came back empty", () => {
    // Told apart from "nothing has answered yet", which is the state a screen
    // draws a skeleton for rather than "this is the beginning".
    expect(EMPTY_TAIL.loaded).toBe(false);
    const tail = mergeOlder(mergeNewest(EMPTY_TAIL, page([view(2), view(1)], "1")), page([], ""));
    expect(tail.loaded).toBe(true);
    expect(tail.older).toBe("");
  });

  test("re-reading the newest page keeps the pages the reader scrolled back through", () => {
    // Every live frame causes this read. A reader who has paged back through
    // an afternoon has to stay exactly where they were, and the cursor for
    // what is below them must not be replaced by the newest page's.
    let tail = mergeNewest(EMPTY_TAIL, page([view(10), view(9)], "9"));
    tail = mergeOlder(tail, page([view(8), view(7)], "7"));
    tail = mergeNewest(tail, page([view(11), view(10), view(9)], "9"));
    expect(seqs(tail)).toEqual([11, 10, 9, 8, 7]);
    expect(tail.older).toBe("7");
  });

  test("an edit arrives in place rather than as a second message", () => {
    // A message is its id. Joined by position, an edited row would appear
    // beside the copy it replaced.
    let tail = mergeNewest(EMPTY_TAIL, page([view(2), view(1)], "1"));
    tail = mergeNewest(tail, page([view(2, { body: "fixed", edited_at: "2026-01-01T01:00:00Z" })]));
    expect(tail.messages).toHaveLength(2);
    expect(tail.messages[0]?.message.body).toBe("fixed");
  });

  test("a message erased out from under the tab stops being drawn", () => {
    // Inside the range a page covers, the page is the truth: a row this tab
    // holds that the page does not carry has been erased, and keeping it
    // renders a message the company deleted.
    let tail = mergeNewest(EMPTY_TAIL, page([view(3), view(2), view(1)], "1"));
    tail = mergeNewest(tail, page([view(3), view(1)], "1"));
    expect(seqs(tail)).toEqual([3, 1]);
  });

  test("a gap wider than a page resets rather than being drawn as continuity", () => {
    // More arrived than one page holds while the tab was away. Both halves
    // rendered together would show a hole as though the conversation ran
    // straight through it — which a reader has no way to see.
    let tail = mergeNewest(EMPTY_TAIL, page([view(3), view(2), view(1)], "1"));
    tail = mergeNewest(tail, page([view(90), view(89), view(88)], "88"));
    expect(seqs(tail)).toEqual([90, 89, 88]);
    expect(tail.older).toBe("88");
  });

  test("a page that touches the tail exactly is joined rather than reset", () => {
    // The ordinary live case: one message arrived, and the re-read overlaps
    // everything else. 41 and 42 are contiguous, so nothing is thrown away.
    let tail = mergeNewest(EMPTY_TAIL, page([view(41), view(40)], "40"));
    tail = mergeNewest(tail, page([view(42), view(41)], "41"));
    expect(seqs(tail)).toEqual([42, 41, 40]);
  });

  test("an emptied room empties the screen", () => {
    // A prune or an erase can take the whole visible room. The newest page
    // coming back empty is that fact, not a failed read — a failed read never
    // reaches here.
    const tail = mergeNewest(mergeNewest(EMPTY_TAIL, page([view(2), view(1)])), page([]));
    expect(tail.messages).toEqual([]);
    expect(tail.loaded).toBe(true);
  });
});

describe("a thread", () => {
  function thread(replies: ChatMessageView[], next = ""): ChatThreadAnswer {
    return {
      read_level: "session",
      complete: true,
      position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 999 },
      channel_id: "room-1",
      root: view(1),
      replies,
      next_cursor: next,
    };
  }

  test("replies read forwards and never repeat across a page", () => {
    // A conversation is read in the order it was said, which is the opposite
    // direction from the room it is in.
    let tail = mergeReplies(EMPTY_THREAD, thread([view(2), view(3)], "3"));
    tail = mergeReplies(tail, thread([view(3), view(4)], ""));
    expect(tail.replies.map((r) => r.channel_seq)).toEqual([2, 3, 4]);
    expect(tail.next).toBe("");
  });
});

describe("the window", () => {
  const rows = (n: number, height = 40) => ({
    count: n,
    heightOf: () => height,
  });

  test("the padding is exactly the height of what is not drawn", () => {
    // The scrollbar is the only thing telling a reader how much conversation
    // is above them, and it is made of these two numbers. Padding that does
    // not sum to the unrendered height is a scroller that jumps under the
    // thumb on every window change.
    const w = windowFor({ ...rows(500), scrollTop: 4_000, viewport: 800 });
    expect(w.padTop).toBe(w.start * 40);
    expect(w.padBottom).toBe((500 - w.end) * 40);
    expect(w.padTop + (w.end - w.start) * 40 + w.padBottom).toBe(500 * 40);
  });

  test("the window covers what the reader can see, plus the overscan", () => {
    const w = windowFor({ ...rows(500), scrollTop: 4_000, viewport: 800, overscan: 400 });
    // Everything from 3,600px to 5,200px: rows 90 through 130.
    expect(w.start).toBeLessThanOrEqual(90);
    expect(w.end).toBeGreaterThanOrEqual(130);
    // And not the whole list — the point of the exercise.
    expect(w.end - w.start).toBeLessThan(200);
  });

  test("an unmeasured viewport draws the whole list rather than nothing", () => {
    // The first render, and every render in a suite: jsdom computes no layout
    // at all. A window of zero rows there is a transcript that renders empty
    // and a screen nothing can assert against.
    const w = windowFor({ ...rows(12), scrollTop: 0, viewport: 0 });
    expect(w).toEqual({ start: 0, end: 12, padTop: 0, padBottom: 0 });
  });

  test("a scroll past the end of the list still draws the last row", () => {
    // One frame of this happens whenever tall rows are replaced by short
    // ones. An empty window would blank the conversation mid-scroll.
    const w = windowFor({ ...rows(10), scrollTop: 100_000, viewport: 800 });
    expect(w.end).toBe(10);
    expect(w.start).toBeLessThan(10);
    expect(w.padTop + (w.end - w.start) * 40 + w.padBottom).toBe(400);
  });

  test("an empty list is an empty window", () => {
    expect(windowFor({ ...rows(0), scrollTop: 0, viewport: 800 })).toEqual({
      start: 0,
      end: 0,
      padTop: 0,
      padBottom: 0,
    });
  });
});
