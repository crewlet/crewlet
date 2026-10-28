/**
 * The pager every paged history shares, and the one rule a list paged by a
 * cursor may not break: no row falls between two pages.
 *
 * An older page continues from the cursor the page before it was answered
 * with, minted from its last row. The newest page read again under newer rows
 * pushes its last ones out, and the older page starts after them — so the
 * cases below drive the newest page's real loader (`useQuery`, its poll and
 * its reconnect) and a scripted engine, and assert what a reader would see.
 */

import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { useClient, useConnection } from "./store-hooks.ts";
import { usePageHold, usePaged } from "./usePaged.ts";
import { useQuery, type QueryResult } from "./useQuery.ts";
import { session } from "~/protocol/session.ts";
import { afterMicrotasks } from "~/test/settle.ts";

vi.mock("./store-hooks.ts", () => ({
  useClient: vi.fn(),
  useConnection: vi.fn(),
}));

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

/** How often the newest page is polled here. */
const POLL_MS = 30_000;

/** A page as the engine answers one: its rows, newest first, and the cursor after them. */
interface Page {
  comments: { id: string }[];
  next_cursor?: string;
}

const rowsOf = (a: Page) => a.comments;
const idOf = (row: { id: string }) => row.id;
const cursorOf = (a: Page) => a.next_cursor ?? "";

/**
 * A history of `count` rows, newest first (`r<count>` … `r1`), answered
 * `size` at a time: the newest page with no cursor, an older one from the
 * cursor naming the last row of the page before — as a keyset cursor is
 * minted, so a page asked after newer rows land continues from that row.
 */
function history(size: number) {
  let count = 0;
  const answer = (after: number | null): Page => {
    const top = after === null ? count : after - 1;
    const ids = [];
    for (let n = top; n > 0 && ids.length < size; n--) ids.push(n);
    const last = ids[ids.length - 1];
    return {
      comments: ids.map((n) => ({ id: `r${n}` })),
      ...(last !== undefined && last > 1 ? { next_cursor: `c${last}` } : {}),
    };
  };
  return {
    /** The engine holds `n` rows. */
    holds(n: number) {
      count = n;
    },
    newest: () => answer(null),
    after: (cursor: string) => answer(Number(cursor.slice(1))),
  };
}

/** The socket every newest-page ask goes through, and the connection it rides. */
function engine(newest: () => Page) {
  const query = vi.fn((_what: string, _params?: Record<string, unknown>) =>
    Promise.resolve(newest()),
  );
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  return query;
}

/** The socket goes away and comes back. */
function reconnect(rerender: () => void) {
  vi.mocked(useConnection).mockReturnValue({ connected: false, accessRefused: null } as never);
  rerender();
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  rerender();
}

/**
 * A pager over `work_comments`, its newest page asked as every caller asks it:
 * with the hold `usePageHold` says.
 */
function usePager(params: Record<string, unknown>, page: (cursor: string) => Promise<Page>) {
  const hold = usePageHold<Page>(params);
  const first = useQuery("work_comments", params, {
    pollMs: POLL_MS,
    hold: hold.holding,
  }) as unknown as QueryResult<Page>;
  return usePaged(hold, first, page, rowsOf, idOf, cursorOf);
}

const ids = (items: readonly { id: string }[]) => items.map((row) => row.id);

// HELD FROM THE PRESS: a poll tick or a reconnect while the older page is on
// its way, or after it landed, asks nothing of the newest page — read again,
// it would push its last row out from under the page that continues it.
// Mutation: ask the newest page without the hold, and the tick asks it.
test("from the press of older, no poll tick and no reconnect asks the newest page", async () => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  const h = history(2);
  h.holds(4);
  const query = engine(h.newest);
  let land: (page: Page) => void = () => {};
  const page = vi.fn(
    (_cursor: string) =>
      new Promise<Page>((resolve) => {
        land = resolve;
      }),
  );
  const { result, rerender } = renderHook(() => usePager({ item: "t-1" }, page));
  await afterMicrotasks();
  expect(ids(result.current.items)).toEqual(["r4", "r3"]);
  expect(query).toHaveBeenCalledTimes(1);

  act(() => result.current.older());
  expect(page).toHaveBeenCalledWith("c3");
  // ON ITS WAY: a tick and a reconnect pass the newest page by.
  h.holds(5);
  await act(async () => void vi.advanceTimersByTime(POLL_MS));
  reconnect(rerender);
  await afterMicrotasks();
  expect(query).toHaveBeenCalledTimes(1);

  await act(async () => land(h.after("c3")));
  await afterMicrotasks();
  expect(ids(result.current.items)).toEqual(["r4", "r3", "r2", "r1"]);
  expect(result.current.held).toBe(true);
  // AND HELD AFTER IT LANDED.
  await act(async () => void vi.advanceTimersByTime(5 * POLL_MS));
  reconnect(rerender);
  await afterMicrotasks();
  expect(query).toHaveBeenCalledTimes(1);
  expect(ids(result.current.items)).toEqual(["r4", "r3", "r2", "r1"]);
});

// A NEWEST PAGE THAT MOVES ANYWAY — here the reader's own write, which asks it
// again at the floor it raised — reads the held pages again from it, so the
// write is drawn and the rows it pushed out are on the page that now
// continues it. Mutations: drop the read again, and the write is never drawn;
// draw the newest page over the pages held instead, and r5 and r4 are on
// neither.
test("a newest page that moves under held pages reads them again from it, leaving no row out", async () => {
  const h = history(2);
  h.holds(5);
  const query = engine(h.newest);
  const page = vi.fn((cursor: string) => Promise.resolve(h.after(cursor)));
  const { result } = renderHook(() => usePager({ item: "t-1" }, page));
  await afterMicrotasks();
  act(() => result.current.older());
  await afterMicrotasks();
  expect(ids(result.current.items)).toEqual(["r5", "r4", "r3", "r2"]);

  // TWO ROWS LAND, one of them the reader's own comment: the newest page
  // asked again is r7 and r6, its cursor after r6.
  h.holds(7);
  act(() => void session.written("tracker", "CREWLET_TRACKER_LOG@1:9", []));
  await afterMicrotasks();
  await afterMicrotasks();
  expect(query).toHaveBeenCalledTimes(2);
  expect(page).toHaveBeenLastCalledWith("c6");
  expect(ids(result.current.items)).toEqual(["r7", "r6", "r5", "r4"]);
  // STILL ONE READING, held: the next page continues the one now drawn.
  expect(result.current.held).toBe(true);
  act(() => result.current.older());
  await afterMicrotasks();
  expect(page).toHaveBeenLastCalledWith("c4");
  expect(ids(result.current.items)).toEqual(["r7", "r6", "r5", "r4", "r3", "r2"]);
});

// A NEW QUESTION DISCARDS WHAT THE LAST ONE HELD. Hidden under its key, the
// last one's older pages came back when the reader returned to it, under a
// newest page read since: rows the reader had just seen vanished from the
// middle of the list. Mutation: keep the state and hide it by question, and
// A's old older page comes back.
test("a question left and come back to starts again from its newest page", async () => {
  const a = history(2);
  const b = history(2);
  a.holds(4);
  // B FILLS ITS PAGE TOO: `useQuery` shows a question's last answer until the
  // next one lands, so for a moment back on A the newest page is B's, and a
  // B with no cursor would let A's held pages go by chance.
  b.holds(4);
  const query = engine(() => a.newest());
  query.mockImplementation((_what, params) =>
    Promise.resolve(params?.item === "b" ? b.newest() : a.newest()),
  );
  const page = vi.fn((cursor: string) => Promise.resolve(a.after(cursor)));
  const { result, rerender } = renderHook(({ item }) => usePager({ item }, page), {
    initialProps: { item: "a" },
  });
  await afterMicrotasks();
  act(() => result.current.older());
  await afterMicrotasks();
  expect(ids(result.current.items)).toEqual(["r4", "r3", "r2", "r1"]);

  rerender({ item: "b" });
  await afterMicrotasks();
  expect(result.current.held).toBe(false);
  // TWO NEW ROWS on A while the reader looked at B.
  a.holds(6);
  rerender({ item: "a" });
  await afterMicrotasks();
  expect(ids(result.current.items)).toEqual(["r6", "r5"]);
  expect(result.current.held).toBe(false);
  expect(result.current.more).toBe(true);
});

// AN OLDER PAGE THAT FAILS WITH NOTHING OLDER HELD has nothing to hold still
// for: the newest page is live again, polled at its cadence, and the failure
// is said. Mutation: keep the hold, and the tick asks nothing.
test("an older page that fails with nothing held lets the newest page go live", async () => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  const h = history(2);
  h.holds(4);
  const query = engine(h.newest);
  const page = vi.fn(() => Promise.reject(new Error("unavailable")));
  const { result } = renderHook(() => usePager({ item: "t-1" }, page));
  await afterMicrotasks();
  act(() => result.current.older());
  await afterMicrotasks();
  expect(result.current.pageError).toBe("unavailable");
  expect(result.current.held).toBe(false);
  await act(async () => void vi.advanceTimersByTime(POLL_MS));
  await afterMicrotasks();
  expect(query).toHaveBeenCalledTimes(2);
});
