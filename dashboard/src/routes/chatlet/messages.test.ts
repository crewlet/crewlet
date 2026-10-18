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
  answersFor,
  EMPTY_TAIL,
  mergeNewest,
  mergeOlder,
  mergeRange,
  rangesToReread,
  reachOf,
  threadPagesTouched,
  touchesOf,
  windowFor,
  type Tail,
} from "./messages.ts";
import type {
  ChatChange,
  ChatMessagesAnswer,
  ChatMessageView,
  ChatThreadAnswer,
} from "~/protocol/index.ts";

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
    tail = mergeNewest(
      tail,
      page([view(2, { body: "fixed", edited_at: "2026-01-01T01:00:00Z" })], "2"),
    );
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

  test("a newest page with nothing below it is the whole room", () => {
    // NO CURSOR BELOW IT, so it covers every message the room has: a row this
    // tab paged back to that it does not carry was erased or pruned, and
    // kept, it drew a message the company deleted.
    let tail = mergeNewest(EMPTY_TAIL, page([view(10), view(9)], "9"));
    tail = mergeOlder(tail, page([view(8), view(7)], "7"));
    tail = mergeNewest(tail, page([view(10), view(9)]));
    expect(seqs(tail)).toEqual([10, 9]);
    expect(tail.older).toBe("");
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

/** A frame as the socket delivers it: no body, and where it sits on the log. */
function change(op: string, over: Partial<ChatChange> = {}): ChatChange {
  return {
    channel_id: "room-1",
    op,
    op_id: `op-${op}`,
    at: "2026-01-01T00:00:00Z",
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 30 },
    channel_kind: "public",
    ...over,
  };
}

describe("what a frame reaches", () => {
  test("a post or a room's own record changes no message already held", () => {
    for (const op of ["post", "create", "patch", "members"]) {
      expect(reachOf(change(op, { message_id: "m-1" }))).toBe("none");
    }
  });

  test("an edit, a delete or a reaction changes the one message it names", () => {
    for (const op of ["edit", "delete", "react"]) {
      expect(reachOf(change(op, { message_id: "m-1" }))).toBe("message");
    }
  });

  test("an erase, a prune, and an op this build does not know may change any of them", () => {
    // AN UNKNOWN OP IS READ AS THE WIDEST: a newer build's record has a reach
    // this one cannot say, and reading it as none would leave a row drawn
    // as it was before something changed it.
    for (const op of ["erase", "prune", "redact"]) expect(reachOf(change(op))).toBe("room");
    // A message op that names no message cannot be placed either.
    expect(reachOf(change("edit"))).toBe("room");
  });

  test("frames let go of touch every row, because what they changed is unknown", () => {
    expect(touchesOf(null)).toEqual([{ position: null, message: null }]);
    expect(touchesOf([change("post", { message_id: "m-9" })])).toEqual([]);
    expect(touchesOf([change("delete", { message_id: "m-3" })])).toEqual([
      { position: change("x").position, message: "m-3" },
    ]);
  });

  test("a page answers for a frame it was derived through, and no later one", () => {
    // A PAGE ASKED AFTER A FRAME CAN STILL BE ONE A NODE BEHIND IT ANSWERED,
    // so the log decides — never the order things arrived in.
    const at = (seq: number) => ({ stream: "CREWLET_CHAT_LOG", generation: 1, seq });
    expect(answersFor(at(30), { position: at(30), message: "m-1" })).toBe(true);
    expect(answersFor(at(29), { position: at(30), message: "m-1" })).toBe(false);
    expect(answersFor(at(1), { position: null, message: null })).toBe(true);
  });
});

describe("reading a room's held rows again", () => {
  const LIMIT = 10;
  /** A tail whose newest page is 100..91 and which has paged back to 1. */
  function pagedBack(): { tail: Tail; newest: ChatMessagesAnswer } {
    const newest = page(
      Array.from({ length: 10 }, (_, i) => view(100 - i)),
      "91",
    );
    let tail = mergeNewest(EMPTY_TAIL, newest);
    tail = mergeOlder(
      tail,
      page(
        Array.from({ length: 90 }, (_, i) => view(90 - i)),
        "",
      ),
    );
    return { tail, newest };
  }
  const touch = (message: string | null) => ({ position: null, message });

  test("a touched row the newest page carries is read with it, not again", () => {
    const { tail, newest } = pagedBack();
    expect(rangesToReread(tail, newest, [touch("m-95")], LIMIT)).toEqual([]);
  });

  test("a touched row below it is read in the page that ends at it", () => {
    const { tail, newest } = pagedBack();
    expect(rangesToReread(tail, newest, [touch("m-85")], LIMIT)).toEqual([
      { cursor: "86", messages: ["m-85"] },
    ]);
  });

  test("touched rows one page reaches share a read, and the rest start their own", () => {
    // AT MOST `LIMIT` MESSAGES LIE IN `LIMIT` NUMBERS, so the page that ends
    // at 85 carries 80 — and 70 is past it.
    const { tail, newest } = pagedBack();
    expect(
      rangesToReread(tail, newest, [touch("m-85"), touch("m-80"), touch("m-70")], LIMIT),
    ).toEqual([
      { cursor: "86", messages: ["m-85", "m-80"] },
      { cursor: "71", messages: ["m-70"] },
    ]);
  });

  test("an erase or a prune reads every held row below the newest page", () => {
    const { tail, newest } = pagedBack();
    const ranges = rangesToReread(tail, newest, [touch(null)], LIMIT);
    expect(ranges.map((range) => range.cursor)).toEqual([
      "91",
      "81",
      "71",
      "61",
      "51",
      "41",
      "31",
      "21",
      "11",
    ]);
    expect(ranges.flatMap((range) => range.messages)).toHaveLength(90);
  });

  test("within its range the page is the truth, and outside it nothing moves", () => {
    const { tail } = pagedBack();
    // 85 was taken down and 83 erased; the page reaches one further down.
    const range = page(
      [
        view(85, { body: "", deleted_at: "2026-01-01T00:02:00Z" }),
        ...[84, 82, 81, 80, 79, 78, 77, 76, 75].map((seq) => view(seq)),
      ],
      "75",
    );
    const merged = mergeRange(tail, "86", range);
    expect(seqs(merged)).not.toContain(83);
    expect(merged.messages.find((v) => v.channel_seq === 85)?.message.deleted_at).toBeTruthy();
    expect(seqs(merged)).toEqual(seqs(tail).filter((seq) => seq !== 83));
    // NOT THE FOOT OF WHAT IS HELD, so the pager stays where it was.
    expect(merged.older).toBe(tail.older);
  });

  test("a range reaching the start of the room is the truth over all of it", () => {
    const { tail } = pagedBack();
    const merged = mergeRange(tail, "11", page([view(10), view(5)], ""));
    expect(seqs(merged).slice(-2)).toEqual([10, 5]);
    expect(merged.older).toBe("");
  });

  test("a range no longer meeting what is held is dropped rather than drawn as continuity", () => {
    // A NEWEST PAGE THAT DID NOT TOUCH THE TAIL REPLACED IT while the range
    // was out; joined under it, the range would draw a hole as continuity.
    const { tail } = pagedBack();
    const replaced = mergeNewest(tail, page([view(300), view(299)], "299"));
    expect(mergeRange(replaced, "86", page([view(85)], "85"))).toBe(replaced);
  });
});

describe("which of a thread's pages a frame reads again", () => {
  function threadPage(replies: number[], next = ""): ChatThreadAnswer {
    return {
      read_level: "session",
      complete: true,
      position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 999 },
      channel_id: "room-1",
      root: view(1),
      replies: replies.map((seq) => view(seq, { thread_root: "m-1" })),
      next_cursor: next,
    };
  }
  const pages = [threadPage([2, 3], "3"), threadPage([4, 5])];
  /** The pages a run of frames reaches, by index, as `usePaged` asks. */
  const reached = (held: ChatThreadAnswer[], frames: ChatChange[] | null) => {
    const touched = threadPagesTouched("m-1", frames);
    return held
      .map((page, i) => (touched(page, { first: i === 0, last: i === held.length - 1 }) ? i : -1))
      .filter((i) => i >= 0);
  };
  const which = (frames: ChatChange[] | null) => reached(pages, frames);

  test("a frame in another thread reads nothing", () => {
    expect(which([change("post", { message_id: "m-9", thread_root: "m-7" })])).toEqual([]);
    expect(which([change("delete", { message_id: "m-8" })])).toEqual([]);
    expect(which([change("react", { message_id: "m-8" })])).toEqual([]);
  });

  test("a change to a reply reads only the page that holds it", () => {
    expect(which([change("delete", { message_id: "m-4" })])).toEqual([1]);
    expect(which([change("edit", { message_id: "m-2", thread_root: "m-1" })])).toEqual([0]);
  });

  test("a change to the root reads the first page, which carries it", () => {
    expect(which([change("react", { message_id: "m-1" })])).toEqual([0]);
  });

  test("a new reply reads the last page, where it lands", () => {
    expect(which([change("post", { message_id: "m-6", thread_root: "m-1" })])).toEqual([1]);
    expect(reached([threadPage([2])], [change("post", { thread_root: "m-1" })])).toEqual([0]);
  });

  test("a post that does not say its thread may be this one's", () => {
    // NO ROUTING SNAPSHOT — the engine narrating the room — so nothing says
    // which thread it is in.
    expect(which([change("post", { message_id: "m-6", channel_kind: undefined })])).toEqual([1]);
  });

  test("an erase, a prune or frames let go of read every page", () => {
    expect(which([change("erase")])).toEqual([0, 1]);
    expect(which([change("prune")])).toEqual([0, 1]);
    expect(which(null)).toEqual([0, 1]);
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
