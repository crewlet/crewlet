/**
 * A paged history whose newest page keeps moving.
 *
 * THE ONE THING IT MUST NOT DO IS DRAW A HOLE AS CONTINUITY. A held page
 * begins where the newest page ended when it was asked for, and every arrival
 * at the top pushes the oldest entry off the newest page's foot — onto no
 * page at all, unless the hook notices the two no longer meet. These cases
 * are full pages over a keyset cursor, the shape every caller's history has:
 * the cursor is the last entry of a page, and the page before it is whatever
 * is older than that entry.
 */

import { act, renderHook, waitFor } from "@testing-library/react";
import { useLayoutEffect, useState } from "react";
import { expect, test, vi } from "vitest";

import { usePaged } from "./usePaged.ts";
import type { QueryResult } from "./useQuery.ts";

/** How many entries one page holds. */
const PAGE = 5;
const PARAMS = { limit: PAGE };

interface Answer {
  entries: { id: string }[];
  next: string;
}

const pick = (answer: Answer) => answer.entries;
const idOf = (entry: { id: string }) => entry.id;
const cursorOf = (answer: Answer) => answer.next;

/** One page of `history` (newest first): the newest, or what is older than
 *  the entry `cursor` names — a keyset cursor, as the engine's are. */
function pageOf(history: readonly number[], cursor = ""): Answer {
  const older = cursor === "" ? history : history.filter((n) => n < Number(cursor));
  const slice = older.slice(0, PAGE);
  return {
    entries: slice.map((n) => ({ id: `e-${n}` })),
    next: older.length > PAGE ? String(slice[slice.length - 1]) : "",
  };
}

/** Numbers from `high` down to `low`. */
const down = (high: number, low: number) =>
  Array.from({ length: high - low + 1 }, (_, i) => high - i);

function answered(data: Answer): QueryResult<Answer> {
  return { data, loading: false, error: null, refusal: null, refetch: () => undefined };
}

/** The hook over a history the test moves, paged back once. */
async function pagedBack(start: readonly number[]) {
  const history = { now: start };
  const page = vi.fn(async (cursor: string) => pageOf(history.now, cursor));
  const view = renderHook(
    ({ first }: { first: QueryResult<Answer> }) =>
      usePaged(first, PARAMS, page, pick, idOf, cursorOf),
    { initialProps: { first: answered(pageOf(history.now)) } },
  );
  act(() => view.result.current.older());
  await waitFor(() => expect(view.result.current.items).toHaveLength(2 * PAGE));
  const arrive = (next: readonly number[]) => {
    history.now = next;
    view.rerender({ first: answered(pageOf(history.now)) });
  };
  return { ...view, page, arrive, history };
}

const ids = (items: readonly { id: string }[]) => items.map((entry) => entry.id);

test("an entry pushed off the newest page by an arrival stays on the list", async () => {
  // Twelve entries, two pages drawn: 12..8 and 7..3. Entry 13 arrives and the
  // newest page is 13..9 — so 8 is on neither page unless the gap is read.
  const { result, arrive } = await pagedBack(down(12, 1));
  arrive(down(13, 1));
  await waitFor(() => expect(ids(result.current.items)).toEqual(down(13, 3).map((n) => `e-${n}`)));
  // AND THE PAGER STILL ENDS WHERE IT DID: the next page is the one before 3.
  expect(result.current.more).toBe(true);
  act(() => result.current.older());
  await waitFor(() => expect(ids(result.current.items)).toEqual(down(13, 1).map((n) => `e-${n}`)));
});

test("the list is never drawn with the gap while the gap is being read", async () => {
  const { result, arrive, page, history } = await pagedBack(down(12, 1));
  // The read that fills the gap is held until the test lets it go.
  let release: () => void = () => undefined;
  page.mockImplementationOnce(
    (cursor: string) =>
      new Promise<Answer>((resolve) => {
        release = () => resolve(pageOf(history.now, cursor));
      }),
  );
  arrive(down(13, 1));
  // AS IT WAS LAST DRAWN — 12..3, whole — and not 13..9 beside 7..3.
  await waitFor(() => expect(page).toHaveBeenCalledTimes(2));
  expect(ids(result.current.items)).toEqual(down(12, 3).map((n) => `e-${n}`));
  act(() => release());
  await waitFor(() => expect(ids(result.current.items)).toEqual(down(13, 3).map((n) => `e-${n}`)));
});

test("a gap longer than what was paged back through drops the held pages", async () => {
  // One page held; eleven entries arrive, so the gap is more than a page.
  const { result, arrive } = await pagedBack(down(12, 1));
  arrive(down(23, 1));
  await waitFor(() => expect(ids(result.current.items)).toEqual(down(23, 19).map((n) => `e-${n}`)));
  // AND THE READER PAGES BACK AGAIN from a list that is whole.
  expect(result.current.more).toBe(true);
  expect(result.current.pageError).toBeNull();
});

test("a gap that could not be read drops the held pages and says why", async () => {
  const { result, arrive, page } = await pagedBack(down(12, 1));
  page.mockImplementationOnce(async () => {
    throw new Error("unavailable");
  });
  arrive(down(13, 1));
  await waitFor(() => expect(result.current.pageError).toBe("unavailable"));
  expect(ids(result.current.items)).toEqual(down(13, 9).map((n) => `e-${n}`));
});

test("a newest page that still meets the held page asks nothing more", async () => {
  // An edit to an entry on the newest page moves nothing: its cursor is the
  // one the held page was asked under.
  const { result, arrive, page } = await pagedBack(down(12, 1));
  arrive(down(12, 1));
  await new Promise((r) => setTimeout(r, 20));
  expect(page).toHaveBeenCalledTimes(1);
  expect(ids(result.current.items)).toEqual(down(12, 3).map((n) => `e-${n}`));
});

test("a history that ends before the held page keeps only what is still there", async () => {
  // Everything older than the newest page was erased: the held page holds
  // entries that no longer exist, and the newest page says nothing is older.
  const { result, arrive } = await pagedBack(down(12, 1));
  arrive(down(12, 8));
  await waitFor(() => expect(ids(result.current.items)).toEqual(down(12, 8).map((n) => `e-${n}`)));
  expect(result.current.more).toBe(false);
});

// THE JOIN NEVER OVERWRITES WHAT LANDED SINCE ITS RENDER. A newest answer is
// committed, and its join runs in the passive effect after — and in between,
// an older page the reader asked for can land and append itself. The join
// used to replace the whole held state with one built from that render, so
// the appended page was dropped: "Older" came back with nothing added, which
// is a press that silently did nothing.
test("an older page that lands between a newest answer and its join is kept", async () => {
  const history = { now: down(16, 1) };
  const page = vi.fn(async (cursor: string) => pageOf(history.now, cursor));
  let move: (first: QueryResult<Answer>) => void = () => undefined;
  // RUN IN THE COMMIT THAT TAKES THE NEW ANSWER — a layout effect, so after
  // the commit and before the join's passive effect.
  let inCommit: (() => void) | null = null;
  const view = renderHook(() => {
    const [first, setFirst] = useState(() => answered(pageOf(history.now)));
    move = setFirst;
    useLayoutEffect(() => {
      inCommit?.();
      inCommit = null;
    }, [first]);
    return usePaged(first, PARAMS, page, pick, idOf, cursorOf);
  });
  act(() => view.result.current.older());
  await waitFor(() => expect(view.result.current.items).toHaveLength(2 * PAGE));

  let release: () => void = () => undefined;
  page.mockImplementationOnce(
    (cursor: string) =>
      new Promise<Answer>((resolve) => {
        release = () => resolve(pageOf(history.now, cursor));
      }),
  );
  act(() => view.result.current.older());
  await waitFor(() => expect(page).toHaveBeenCalledTimes(2));

  // OUTSIDE `act`, so the render and its effects are scheduled as a browser
  // schedules them rather than flushed as one step.
  const env = globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean };
  const previous = env.IS_REACT_ACT_ENVIRONMENT;
  env.IS_REACT_ACT_ENVIRONMENT = false;
  try {
    inCommit = () => release();
    // The newest page read again, unchanged — a new answer that still joins.
    move(answered(pageOf(history.now)));
    await waitFor(() =>
      expect(ids(view.result.current.items)).toEqual(down(16, 2).map((n) => `e-${n}`)),
    );
  } finally {
    env.IS_REACT_ACT_ENVIRONMENT = previous;
  }
});

// AN OLDER PAGE FOLLOWS THE PAGE IT WAS ASKED AFTER, OR IT IS NOT KEPT. It is
// asked under the held tail's cursor and answered from the history as it was
// then, and a join can replace or drop the held pages while it is out — so
// what lands may begin after a page the list no longer holds.

/** Hold the next read until `release`, answering from the history as it
 *  was when it was ASKED — an answer the engine formed before the move. */
function holdNext(page: ReturnType<typeof vi.fn>, history: { now: readonly number[] }) {
  const gate = { release: () => undefined as void };
  page.mockImplementationOnce((cursor: string) => {
    const asked = history.now;
    return new Promise<Answer>((resolve) => {
      gate.release = () => resolve(pageOf(asked, cursor));
    });
  });
  return gate;
}

test("an older page that lands after a join cut the held pages short is not appended", async () => {
  const { result, arrive, page, history } = await pagedBack(down(12, 1));
  const gate = holdNext(page, history);
  act(() => result.current.older());
  await waitFor(() => expect(page).toHaveBeenCalledTimes(2));
  // Everything older than 8 is erased and 13 and 14 arrive: the walk from
  // the newest page reads 9..8 and the history ends there, so the held 7..3
  // goes and 9..8 is the tail.
  arrive(down(14, 8));
  await waitFor(() => expect(ids(result.current.items)).toEqual(down(14, 8).map((n) => `e-${n}`)));
  // The page before 3 lands, answered before the erase: 2 and 1 follow a
  // page the list no longer holds, so drawing them would draw a seam.
  act(() => gate.release());
  await waitFor(() => expect(result.current.paging).toBe(false));
  expect(ids(result.current.items)).toEqual(down(14, 8).map((n) => `e-${n}`));
  expect(result.current.more).toBe(false);
});

test("an older page that lands after a join dropped the held pages is not held", async () => {
  const history = { now: down(12, 1) };
  const page = vi.fn(async (cursor: string) => pageOf(history.now, cursor));
  const drawn: string[][] = [];
  const view = renderHook(
    ({ first }: { first: QueryResult<Answer> }) => {
      const paged = usePaged(first, PARAMS, page, pick, idOf, cursorOf);
      drawn.push(ids(paged.items));
      return paged;
    },
    { initialProps: { first: answered(pageOf(history.now)) } },
  );
  act(() => view.result.current.older());
  await waitFor(() => expect(view.result.current.items).toHaveLength(2 * PAGE));
  const gate = holdNext(page, history);
  act(() => view.result.current.older());
  await waitFor(() => expect(page).toHaveBeenCalledTimes(2));
  // Eleven arrive: more than the one page held, so the join drops it.
  history.now = down(23, 1);
  view.rerender({ first: answered(pageOf(history.now)) });
  const whole = down(23, 19).map((n) => `e-${n}`);
  await waitFor(() => expect(ids(view.result.current.items)).toEqual(whole));
  const calls = page.mock.calls.length;
  const from = drawn.length;
  // The page before 3 lands: it follows 7..3, which nothing holds now.
  act(() => gate.release());
  await waitFor(() => expect(view.result.current.paging).toBe(false));
  await new Promise((r) => setTimeout(r, 20));
  // NEVER DRAWN, NOT EVEN FOR A RENDER — and no walk asked to repair it.
  for (const items of drawn.slice(from)) expect(items).toEqual(whole);
  expect(page.mock.calls.length).toBe(calls);
});
